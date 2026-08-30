# Herdr Orchestrator

[![CI](https://github.com/sean1588/herdr-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/sean1588/herdr-orchestrator/actions/workflows/ci.yml)

A control-plane daemon that turns GitHub issues into pull requests by driving
[herdr](https://herdr.dev) (the execution substrate) through a deterministic
**state-graph engine** that reads a declarative YAML workflow.

> **Phase 1 + 2a — issue to merged.** The engine drives the loop end to end:
> one issue → spawn an implementer in an isolated git worktree via herdr → the
> agent opens a PR → spawn a reviewer → the `review` decision branches
> (approve / request_changes / escalate) → on approve, the merge gate
> (CI + mergeable, plus approvals if configured) is polled over GitHub → `merge_pr` squash-merges
> → `merged`. The real merge is gated on `policies.dry_run` (default-on), so the
> shipped config halts at `merging` and logs the intended merge until you set
> `dry_run: false`. Triage/intake and the concurrent scheduler daemon now ship
> (R2), and the daemon exposes an optional loopback **MCP control surface**
> ([below](#mcp-control-surface)). **Cross-task memory** remains deferred —
> tracked in [ROADMAP.md](ROADMAP.md).

## Quick start: let Claude operate it against your repo

You don't drive the orchestrator by hand — you hand it to a coding agent. The
repo ships everything the agent needs: [`RUNBOOK.md`](RUNBOOK.md) is the
end-to-end operator guide, and the
[`operate-orchestrator` skill](.claude/skills/operate-orchestrator/SKILL.md) is
the supervision loop.

**Prerequisites:** Go 1.26+ · [herdr](https://herdr.dev) running ·
[`gh`](https://cli.github.com) authenticated for your repo ·
[Claude Code](https://claude.com/claude-code) (`claude`) installed.

1. Clone this repo and, **from inside a herdr pane**, start a Claude session in
   the checkout:

   ```sh
   git clone https://github.com/sean1588/herdr-orchestrator
   cd herdr-orchestrator
   claude
   ```

2. Tell it what to operate:

   > Read RUNBOOK.md and operate the orchestrator against `<owner>/<repo>`.

   The agent takes it from there: builds the binary, scaffolds a config wired to
   your repo (`orchestratord init`), creates the source label, starts the
   daemon, and supervises the pipeline — restarting it if it dies, diagnosing
   stuck tasks, and surfacing anything that needs you.

3. Feed it work: label a small, well-specified issue in your repo `agent-ready`.
   The next poll picks it up and runs it through triage → implement → review →
   merge-gate.

The shipped default is `dry_run: true` — the pipeline stops just short of the
real merge and halts each task at `merging` as a success. Watch one dry pass,
then have the agent set `dry_run: false` and restart the daemon.

Not using Claude? Any capable coding agent works — `RUNBOOK.md` §3.0 and the
skill file are plain markdown, and the control surface is plain JSON-RPC over
HTTP. Prefer to drive it yourself? The [Usage](#usage) section below has every
command, and [TUTORIAL.md](TUTORIAL.md) is the human-paced walkthrough.

## Design in one paragraph

A fixed engine (mechanism) interprets a per-team workflow (policy) supplied as
YAML. A **task** is a token moving through a directed state graph; the engine —
never a model — owns every transition. Judgment enters only at constrained
`decision` points; irreversible side effects (merge) are reachable only through
`gate` evaluations over **authoritative** sources. **GitHub is the source of
truth for artifacts**; an agent's `done` status is only a trigger to go check
GitHub. The engine is the **single writer** of durable task state (SQLite).

## Architecture

```
cmd/orchestratord/      CLI: validate | plan | run | recover | daemon | version
internal/config/        workflow types, JSON-Schema validation, the 7 safety invariants
internal/engine/        the state-graph executor
internal/scheduler/     the daemon loop: one poller, N workers, single-writer store
internal/store/         SQLite task store + per-transition audit log (single writer)
internal/exec/          ExecutionBackend interface + herdr implementation
internal/github/        Client interface + gh CLI implementation (PR detection)
internal/mcp/           loopback MCP control server (read state + cancel/enqueue)
internal/notify/        out-of-band escalation/alert notifier (webhook)
internal/proc/          mockable os/exec runner (the seam under herdr + gh)
```

The engine depends only on small interfaces (`exec.ExecutionBackend`,
`github.Client`, `*store.Store`), never on herdr or `gh` concretely — so the
backend can later be swapped for a headless/container implementation.

## The review → merge loop

Past `pr_open` the engine runs the rest of the pipeline:

- **Review (a `decision`).** Entering `pr_open` spawns the `reviewer` role with a
  task file built from the decision's **rubric** (e.g. `prompts/review.md`,
  resolved relative to the config file) plus a pointer to the PR. The reviewer
  writes a **verdict file** — `{"verdict": "...", "feedback": "..."}` — and on
  `agent.done` the engine reads it, validates the verdict against the decision's
  declared `verdicts`, and branches. The engine reads a verdict; it never judges.
- **Changes requested.** `changes_requested` resumes the implementer carrying the
  reviewer's `feedback`, and loops back to `pr_open` only once the PR head has
  actually moved (`pr_exists` + `github_commits`) — an agent that reports done
  having committed nothing escalates rather than sending the reviewer back to
  unchanged code. It gives up to `escalated` once
  `policies.retry_caps.changes_requested` is exceeded.
- **Merge gate.** `approved` evaluates the merge gate
  (`github_checks` + `github_reviews` + `github_mergeable`) over one authoritative
  `PRStatus` read. If not yet green it parks in `blocked_on_gate`, which evaluates
  the gate once and, while it neither passes nor has timed out, **suspends** —
  the drive returns and frees its worker slot instead of pinning it for the whole
  wait, and the scheduler re-drives the task each poll to re-check the gate
  (`status.changed` has no push source). The state timeout is measured from the
  audit-recorded entry time, so it survives suspend/resume and daemon restarts;
  past it, the task escalates.
- **Merge.** `merging` runs the `merge_pr` action — **gated on `policies.dry_run`
  (default-on)**. A dry run logs the intended merge and halts at `merging`; with
  `dry_run: false` it `gh pr merge --squash`, verifies the PR is `MERGED`
  (authoritative), and reaches `merged`. Merge is reachable only through a gate
  (a safety invariant) and the side effect itself is gated again by `dry_run`.
  Once the merge is confirmed the engine settles the task's bookkeeping: it
  closes the source issue (the default kickoff's `gh pr create --fill` writes no
  `Closes #N` trailer, so nothing else would) and deletes the remote branch.
  Both are best-effort — the merge is already irreversible, so a bookkeeping
  failure is logged rather than re-driven as a failed merge.

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

## Usage

Scaffold a working config for your repo — writes `pipeline.yaml` + `prompts/`
from the shipped default (readable copy in [`examples/`](examples/)):

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
  (absolute path). The engine creates per-task worktrees beside it.
- The agent CLI named in the workflow's `roles.*.launch` on `PATH` (default
  `claude`). Agents run **non-root** with no `--dangerously-skip-permissions`; on
  a brand-new worktree the agent TUI may prompt to trust the folder.
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
git worktrees; defaults to the repo's sibling) and `--task-dir` (where task
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
`no_progress`, `drive_deadline`, or a gate/decision result), the last few
transitions, the tail of the agent's pane, and a concrete recommended action.
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

## The workflow config

A workflow is a versioned YAML document — the *policy* the fixed engine
interprets. It is validated in **two stages** before anything runs: a JSON Schema
for shape, then seven semantic **safety invariants**. `validate` reports both;
`run` and `recover` refuse to start on any error.

### Schema & reference files

| File | Role |
| --- | --- |
| `internal/config/workflow.schema.json` | JSON Schema (Draft 2020-12) for the config **shape**; embedded in the binary via `go:embed` and applied first. The authoritative shape contract. |
| `internal/config/validate.go` | The runtime validator: applies the schema, then the seven invariants, returning errors + warnings. |
| `validate_workflow.py` (repo root) | Reference spec for the invariants, kept behaviorally equivalent to `validate.go`. Runs standalone: `python3 validate_workflow.py <config> [--schema workflow.schema.json]`. |
| `examples/default-pipeline.yaml` | The shipped default pipeline (with `prompts/` beside it) — what `orchestratord init` scaffolds, and the starting point when authoring your own. |
| `internal/config/testdata/broken-pipeline.yaml` | A structurally-valid config that **violates** the invariants (merge reachable without a gate; an unbounded loop) — used to prove the validator bites. |
| `spike0.sh` (repo root) | The proven herdr + `gh` command sequence the herdr backend wraps. |

### Structure

Top-level keys (`version`, `name`, and `states` are required; unknown keys are
rejected):

| Key | Meaning |
| --- | --- |
| `version` | Schema version (integer ≥ 0). |
| `name` | Workflow name (non-empty). |
| `entry_state` | The state a new task starts in (used for reachability checks). |
| `policies` | Workflow-wide knobs (below). |
| `sources` | Where work originates — `github_issues`, polled by the `daemon` on the `select:` label. |
| `roles` | Agent profiles a state can `spawn`/`resume`. |
| `gates` | Deterministic predicates over **authoritative** sources (GitHub). |
| `decisions` | Constrained judgment hooks with a closed set of `verdicts`. |
| `states` | The nodes of the state graph (below). |

**`policies`** — `max_concurrent_tasks`, `dry_run`, `circuit_breaker`,
`retry_caps` (a per-state cap map, `state_name: N`), the liveness bounds
`no_progress_timeout` / `blocked_timeout` / `drive_deadline`, and `execution`
(`backend: herdr|local|container`, `run_as: root|non_root`, `sandbox: bool`).
The engine reads these: `retry_caps` bounds per-state retries and is validated,
`dry_run` gates the real merge, the three bounds below keep work from wedging,
and `max_concurrent_tasks` bounds the daemon's concurrency (R2).
`circuit_breaker` and the finer `execution` knobs (`sandbox`) are parsed but not
yet enforced.

#### Liveness bounds

A per-state `timeout:` transition is an **override** for the one thing that
genuinely varies per state ("this one legitimately takes 90 minutes"). Three
policies cover everything else, so nothing is unbounded merely because nobody
remembered to annotate it.

**`no_progress_timeout`** (a duration; absent ⇒ `30m`, `0s` disables) is the
global bound. A state timeout measures **position** — how long a task has been
in state X — which cannot distinguish a legitimately slow agent from a dead one,
and only protects the states someone annotated. This measures **progress**: how
long since the task last did anything observable. Any pane status event resets
it. When it expires the engine treats that as a suspicion, not a verdict, and
confirms against the agent pane's own bytes before escalating (trigger
`no_progress`) — the event hub broadcasts only status *diffs*, so an agent
working steadily for an hour emits no events at all. A pane that cannot be read
counts as progress: a herdr blip must never be what escalates a task. Disabling
it is allowed only if every agent state declares its own timeout; the validator
rejects the combination that would leave a state unbounded.

**`drive_deadline`** (a duration; absent ⇒ twice the longest state timeout,
floored at `1h`) is the hard ceiling on a single drive, enforced by a reaper in
the scheduler — **outside** the drive it watches. The engine's own timer is armed
only once a drive reaches the agent-wait loop in a state that declares a timeout;
a drive wedged in a spawn, a gate read, a decision, or a merge has no timer at
all, and a watchdog sharing a goroutine with a wedged drive is no watchdog. A
reaped drive settles (trigger `drive_deadline`) rather than aborting, so it is
not re-driven and re-reaped forever. The bound is per *drive*, not per task: a
task that suspends in a merge-gate wait and resumes next poll starts a fresh
clock. An explicit value at or below the longest state timeout is a validation
error — it would preempt the transitions it exists to back up.

**`blocked_timeout`** (a duration, e.g. `10m`; absent ⇒ only the bounds above
apply) caps how long an agent may sit *continuously* blocked before the engine
gives up on it, with trigger `blocked_timeout` in the audit. It exists
because a blocked agent is parked on an interactive prompt nobody will answer —
it will never report done — yet **blocking does not change state**, and a state
timeout is anchored to state *entry*. Without this the only lever is shortening
the whole state timeout, which would also kill legitimately long runs. Any
`working` event clears the clock, so a prompt the agent resolves itself never
counts; `idle` deliberately does **not** clear it, because a pane parked at an
unanswerable prompt can report idle. The state timeout remains the hard backstop.

All three bounds escalate to the same place: the state's own `timeout:` target if
it declares one, else the workflow's alerting terminal, else `cancelled`. That
fallback is what makes them apply by default — a bound whose only escalation path
was the state's timeout edge was silently **inert** in every state that declared
none. The derivation is recorded in the audit `result` column, so a fallback is
never silent.

**`roles`** — each has `launch` (argv, required, e.g. `["claude"]`),
`task_delivery` (`context_file` | `inline`), `workspace` (`per_task` | `shared`),
and an optional `kickoff` string.

**`gates`** — `type` is one of `github_pr`, `github_checks`, `github_reviews`,
`github_mergeable`, `github_commits` (the only authoritative sources accepted).
Type-specific fields (`head`, `all_passing`, `min_approved`, `require`, `since`)
are allowed alongside.

**`github_commits`** (`since: state_entry`, required) passes only if the PR head
commit differs from the one recorded when the task entered its current state. It
exists because a gate must ask the question its state actually asks. In
`implementing`, `pr_exists` means "did you produce the artifact?" — real
evidence. Reused in `changes_requested` it verifies nothing: the PR was opened in
the round that put the task there, so it could only ever pass, whether or not the
implementer addressed a single line of the review. A resumed agent that did
nothing advanced anyway, the reviewer re-reviewed unchanged code, and the loop
burned retries. `github_commits` asks whether anything was actually committed.

The baseline is captured when the task enters the state, and only for states that
evaluate such a gate — every other transition, including the detached writes that
settle a cancel or a reaped drive, skips the read. If the baseline is unknown (no
PR yet, a failed read, or a task predating the column) the gate passes and logs:
degrading to the previous behavior beats escalating a task on a question whose
input was never captured.

**`decisions`** — `impl.type` is `llm` (with a `rubric` path) or `exec` (with a
`command` argv); `verdicts` is the closed, unique set of outcomes it may return.

**`states`** — each state may declare:

- `entry` — an action on arrival: `spawn` / `resume` a role (optionally `with` a
  named input), or `action: merge_pr` (the only side-effecting entry action).
- `transitions` — outgoing edges (below).
- `terminal` — `success` | `rejected` | `needs_human` (a leaf; takes no transitions).
- `wait_for` — an event the state parks on (e.g. `status.changed`).
- `alert` — surface the state to a human.

A **transition** carries a `when` **trigger** (exactly one of `event`, `timeout`
— matching `^[0-9]+(s|m|h)$`, `decision`, or `gate`), an optional secondary
`evaluate` (`decision` or `gate`, run after an event), and exactly one outcome:

- `to: <state>` — unconditional move;
- `branch: { <key>: <state>, … }` — keys are the decision's **verdicts**, or
  exactly `{pass, fail}` for a gate;
- `action: { alert: <name> }` — a side action that does not change state.

A `gate` reference is a single name or a list (every gate must pass).

### The seven safety invariants

1. **Refs resolve** — every `spawn`/`resume` role, `decision`/`gate`, and
   `to`/`branch` target names a declared entity.
2. **Decisions are total** — a transition's branch keys exactly equal the
   referenced decision's declared verdicts.
3. **Gate branches are `{pass, fail}`**.
4. **Gates read authoritative sources only** — `github_pr`, `github_checks`,
   `github_reviews`, `github_mergeable`, `github_commits`.
5. **Merge is gate-only** — entering a side-effecting (`merge_pr`) state must be
   gate-evaluated, never decided by a model or raw event.
6. **Loops terminate** — every cycle has a retry cap or a timeout.
7. **Every non-terminal state has an exit**.

### Authoring & validating

Copy `default-pipeline.yaml`, edit it, and check it — no external services
needed, so it is safe in CI or a pre-commit hook:

```sh
orchestratord validate path/to/your-workflow.yaml
#   OK: "your-workflow" valid (N warning(s))    -> exit 0
#   FAIL: K error(s), N warning(s)              -> exit 1   (warnings alone pass)
```

> The trigger key is **`when`**, never `on` — a bare `on:` is coerced to the YAML
> boolean `true` and would silently drop the trigger. The schema rejects it.

## Conventions / guardrails

- Branch names are deterministic: `agent/issue-<n>` (the durable reconcile key).
- herdr pane ids are **volatile** — parsed from output, re-resolved on restart,
  never persisted as a durable key.
- Agents are never launched with `--dangerously-skip-permissions`; honor
  `run_as: non_root` + `sandbox`.
- Task handoff is a **context file + single-line kickoff**, never an inline
  multi-line prompt typed through the pane.
