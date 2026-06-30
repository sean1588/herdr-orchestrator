# Herdr Orchestrator

[![CI](https://github.com/sean1588/herdr-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/sean1588/herdr-orchestrator/actions/workflows/ci.yml)

A control-plane daemon that turns GitHub issues into pull requests by driving
[herdr](https://herdr.dev) (the execution substrate) through a deterministic
**state-graph engine** that reads a declarative YAML workflow.

> **Phase 1 — the validated slice.** This build drives one loop end to end:
> one issue → the engine spawns an implementer agent in an isolated git worktree
> via herdr → the agent opens a PR → the engine detects the PR via GitHub → the
> task reaches `pr_open`. Review, auto-merge, triage, the scheduler, the MCP
> surface, and cross-task memory are **out of scope** — the engine parses and
> validates the full pipeline but only executes this slice.

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
cmd/orchestratord/      CLI: validate | run | recover
internal/config/        workflow types, JSON-Schema validation, the 7 safety invariants
internal/engine/        the state-graph executor (Phase 1 slice)
internal/store/         SQLite task store + per-transition audit log (single writer)
internal/exec/          ExecutionBackend interface + herdr implementation
internal/github/        Client interface + gh CLI implementation (PR detection)
internal/proc/          mockable os/exec runner (the seam under herdr + gh)
```

The engine depends only on small interfaces (`exec.ExecutionBackend`,
`github.Client`, `*store.Store`), never on herdr or `gh` concretely — so the
backend can later be swapped for a headless/container implementation.

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

Validate a workflow config (JSON Schema + the safety invariants) — no external
dependencies, safe to run anywhere:

```sh
orchestratord validate internal/config/testdata/default-pipeline.yaml
```

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
- An issue to work — for the shipped `default-pipeline.yaml` that means an issue
  in `sean1588/minicode` (Phase 1 enqueues the `--issue` number directly; the
  source `select:` label is not yet polled).

Drive one issue through the slice to `pr_open`:

```sh
orchestratord run \
  --config internal/config/testdata/default-pipeline.yaml \
  --repo /abs/path/to/checkout \
  --base main \
  --issue 123 \
  --db ./orchestrator.db          # optional; defaults to ./orchestrator.db
```

Exit code is `0` when the task reaches `pr_open`, non-zero otherwise (e.g.
`escalated`). Task state and a per-transition audit log persist in the `--db`
SQLite file. Two more optional flags are accepted: `--worktrees-dir` (parent dir
for the per-task git worktrees; defaults to the repo's sibling) and `--task-dir`
(where task context files are written; defaults to the system temp dir).

Reconcile and resume in-flight tasks after a restart (crash recovery) — keys on
the deterministic `agent/issue-<n>` branch and the durable task id, never the
volatile herdr pane id:

```sh
orchestratord recover --config <c> --repo /abs/path/to/checkout
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
| `internal/config/testdata/default-pipeline.yaml` | The canonical **valid** example — copy this when authoring your own. |
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
| `sources` | Where work originates — Phase 1: `github_issues` (validated, not yet polled). |
| `roles` | Agent profiles a state can `spawn`/`resume`. |
| `gates` | Deterministic predicates over **authoritative** sources (GitHub). |
| `decisions` | Constrained judgment hooks with a closed set of `verdicts`. |
| `states` | The nodes of the state graph (below). |

**`policies`** — `max_concurrent_tasks`, `dry_run`, `circuit_breaker`,
`retry_caps` (a per-state cap map, `state_name: N`), and `execution`
(`backend: herdr|local|container`, `run_as: root|non_root`, `sandbox: bool`).
Phase 1 reads these but only `retry_caps` affects validation; the rest gate
merge/scheduler/concurrency machinery that is out of scope.

**`roles`** — each has `launch` (argv, required, e.g. `["claude"]`),
`task_delivery` (`context_file` | `inline`), `workspace` (`per_task` | `shared`),
and an optional `kickoff` string.

**`gates`** — `type` is one of `github_pr`, `github_checks`, `github_reviews`,
`github_mergeable` (the only authoritative sources accepted). Type-specific
fields (`head`, `all_passing`, `min_approved`, `require`) are allowed alongside.

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
   `github_reviews`, `github_mergeable`.
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
