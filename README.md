# Herdr Orchestrator

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

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .
```

## Usage

Validate a workflow config (JSON Schema + the safety invariants):

```sh
orchestratord validate internal/config/testdata/default-pipeline.yaml
```

Drive one issue through the slice (needs a running herdr, a local checkout, and
`gh` authenticated):

```sh
orchestratord run \
  --config internal/config/testdata/default-pipeline.yaml \
  --repo /abs/path/to/checkout \
  --base main \
  --issue 123
```

Reconcile and resume in-flight tasks after a restart (crash recovery):

```sh
orchestratord recover --config <c> --repo /abs/path/to/checkout
```

## The workflow config

A workflow is a versioned YAML document validated in two stages: a JSON Schema
(`internal/config/workflow.schema.json`, embedded in the binary) for shape, then
seven semantic **safety invariants**:

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

The reference implementation of these checks is `validate_workflow.py` at the
repo root; the Go validator in `internal/config/validate.go` is kept
behaviorally equivalent. `spike0.sh` is the proven herdr + `gh` command sequence
the herdr backend wraps.

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
