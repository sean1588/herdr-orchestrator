# CLI reference

Reference moved out of the README; the short version is in [`README.md`](../README.md).

## Build, test

Requires Go 1.26+. The binary is pure Go (no cgo) — a single static binary.

```sh
go build ./...     # compile everything
go test ./...      # run the test suite
go vet ./...
gofmt -l .

# build the CLI into a runnable binary
go build -o orchestratord ./cmd/orchestratord
```

The commands below assume `./orchestratord` is on your `PATH`; otherwise run them
through the toolchain, e.g. `go run ./cmd/orchestratord validate <config>`.

Releases are cut by pushing a tag; [`release.yml`](../.github/workflows/release.yml)
builds the four binaries, stamps them with the tag, and publishes the release:

```sh
git tag v0.1.0 && git push origin v0.1.0
```


## Usage

Scaffold a working config for your repo — writes `pipeline.yaml` + `prompts/`
from the shipped default (readable copy in [`examples/`](../examples/)):

```sh
orchestratord init --repo <owner>/<name> [--label agent-ready] [--dir .]
```

Validate a workflow config (JSON Schema + the safety invariants) — no external
dependencies, safe to run anywhere:

```sh
orchestratord validate examples/default-pipeline.yaml
```

Preflight the whole environment — herdr, the agent CLI, `gh`, the checkout, the
store — and get a fix for anything that is not ready:

```sh
orchestratord doctor --config pipeline.yaml --repo /path/to/checkout
```

`doctor` exits non-zero on any failure, so it composes:
`orchestratord doctor --config c.yaml --repo r && orchestratord daemon ...`.
Its last check launches the agent in a scratch herdr workspace and proves a
kickoff is actually accepted; `--quick` skips it. That check is the reason the
command exists — kickoff delivery has broken twice from underneath this project,
and both times the failure was discovered by tasks escalating with no work done.
The daemon runs the cheap subset at startup and refuses to start on a failure
(`--skip-preflight` overrides); it never launches an agent to do so.

### Prerequisites for `run` / `recover`

`run` and `recover` drive a real agent and touch GitHub, so they need:

- **herdr running**, and the process able to reach it — run from inside a herdr
  pane, or with `HERDR_SOCKET_PATH` pointing at the server socket
  (`echo $HERDR_ENV` should be `1` inside a pane; `echo $HERDR_SOCKET_PATH`).
- **`gh` authenticated** for the target repo — verify with `gh auth status`.
  (Confirm it from inside a herdr pane too; PR creation fails silently otherwise.)
- A **local checkout** of the repo the agent will work in, passed as `--repo`
  (absolute path). The engine creates per-task worktrees inside it, under
  `.orchestrator/worktrees` (kept out of `git status` via `.git/info/exclude`).
- The agent CLI named in the workflow's `roles.*.launch` on `PATH` (default
  `claude`). Agents run **non-root** with no `--dangerously-skip-permissions`; on
  a first run the agent TUI may prompt to trust the folder — run `claude` once in
  the checkout and accept, and that trust covers every worktree inside it.
- An issue to work — in the repo your config's `sources` block names (set by
  `init --repo`). `run` drives the `--issue` number you pass directly; the
  `daemon` instead polls the source `select:` label (`agent-ready`).

Drive one issue through the pipeline (to `merged`, or `merging` under the shipped
`dry_run: true`):

```sh
orchestratord run \
  --config pipeline.yaml \
  --repo /abs/path/to/checkout \
  --base main \
  --issue 123 \
  --db ./orchestrator.db          # optional; defaults to ./orchestrator.db
```

Exit code is `0` when the task reaches `merged` (a real merge) or halts at
`merging` (a dry run withheld the merge), non-zero otherwise (e.g. `escalated`).
Task state and a per-transition audit log persist in the `--db` SQLite file. Two
more optional flags are accepted: `--worktrees-dir` (parent dir for the per-task
git worktrees; defaults to `<repo>/.orchestrator/worktrees`) and `--task-dir` (where task
context files are written; defaults to the system temp dir).

Reconcile and resume in-flight tasks after a restart (crash recovery) — keys on
the deterministic `agent/issue-<n>` branch and the durable task id, never the
volatile herdr pane id:

```sh
orchestratord recover --config <c> --repo /abs/path/to/checkout
```


## MCP control surface

The `daemon` can expose an optional **MCP server** so an operator or a
supervising agent can observe its tasks and intervene per-task. It is
**off by default**; enable it with `--mcp-listen`:

```sh
orchestratord daemon --config <c> --repo /abs/path/to/checkout \
  --mcp-listen 127.0.0.1:7777
```

The server runs in-process (sharing the daemon's single store handle and its
scheduler), speaks hand-rolled **JSON-RPC 2.0 over HTTP** (the request/response
subset of MCP's Streamable HTTP transport) on a single `/mcp` endpoint, and adds
**zero dependencies**.

**Posture:** bind **loopback only**. There is **no auth** — the bind address is
the trust boundary, so any local process can drive it (including the control
tools). Run it only where you trust every local process; do not bind a
non-loopback address.

**Tools:**

| Tool | Args | Does |
| --- | --- | --- |
| `list_tasks` | — | list every task with its state, branch, PR, retries, and liveness |
| `get_task` | `issue` | one task's current view |
| `get_audit` | `issue` | a task's state-transition history |
| `cancel_task` | `issue` | cancel the running drive; it settles to `cancelled` |
| `enqueue_task` | `issue` | (re-)drive an issue by number (idempotent) |
| `message_task` | `issue`, `text` | submit one line at a running agent's idle prompt (verified like the kickoff; never into a dialog) |

**Liveness.** State says *where* a task is; it cannot say whether it is *moving* —
blocking doesn't change state, so an agent parked on a permission prompt looks
exactly like one working. Task views therefore carry `agent_status`
(`working`/`idle`/`blocked`/`done`), `agent_status_for_seconds`,
`state_for_seconds`, and the `state_timeout` / `blocked_timeout` the task is
racing. Elapsed-vs-bound is the "is anything wedged" question, answerable without
holding the config in your head. Unknown ages are omitted, never reported as `0`.

**Escalations explain themselves.** An escalation delivered via
`--notify-webhook` carries the diagnosis the daemon already had when it
escalated: the `Cause` (`timeout`, `blocked_timeout`, `retry_exhausted`,
`no_progress`, `blocked_on_prompt`, `agent_crashed`, `drive_deadline`, or a
gate/decision result), the last few transitions, the tail of the agent's pane,
and a concrete recommended action.
Since a settled task can never be re-driven, the recommendation is always "fix
the cause and open a fresh issue", never "retry it".

**Event log.** `--event-log <path>` appends every event as JSON Lines —
transitions, spawns, gate evaluations, decisions, agent status changes,
escalations — so a supervisor can `tail -f | jq` instead of scraping the daemon's
terminal pane. It appends across restarts.

**Control semantics.** `cancel_task` / `enqueue_task` are
**dispatch-acknowledged, not completion-acknowledged**: the tool confirms the
command reached the scheduler and was actionable, then returns. Observe the
result — a `cancelled` state, a new PR — via `get_task` / `get_audit`. A
cancelled task **settles to the reserved `cancelled` terminal** (its worktree is
left in place for inspection, not torn down) and is neither re-driven nor
re-listed. `cancel_task` on an issue with no active drive is a tool error.

Smoke-test the endpoint with `curl`:

```sh
curl -s 127.0.0.1:7777/mcp -d \
  '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}'
```
