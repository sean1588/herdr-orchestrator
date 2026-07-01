# R2 — Scheduler Daemon + Executable Triage: Design

**Status:** approved design (2026-07-01), ready for an implementation plan.

**Goal:** turn the orchestrator from per-issue invocation (`orchestratord run --issue N`) into a long-running daemon that discovers labeled issues on its own and drives up to `max_concurrent_tasks` of them through the full pipeline concurrently — closing the self-coordination loop (poll → triage → queue → implement → review → merge).

**Architecture (one sentence):** a single poller goroutine is the only task creator, an N-worker pool drives tasks by wrapping the existing `engine.Run`, the SQLite store (`MaxOpenConns=1`) serializes all writes, and the pipeline's front door — the `intake`/`triage` decision — is made executable by reusing the Phase 2a decision machinery.

**Tech stack:** Go 1.26, existing packages (`config`, `engine`, `store`, `exec`, `github`, `proc`), `gh`/`herdr` via `proc.Runner`. No new dependencies.

---

## Global constraints (from the project)

- No new dependencies beyond Phase 1's (`modernc.org/sqlite`, `gopkg.in/yaml.v3`, `santhosh-tekuri/jsonschema/v6`).
- Engine depends on interfaces (`exec.ExecutionBackend`, `github.Client`), never on herdr/`gh` concretely.
- `context.Context` first arg of anything that blocks; honor cancellation/deadlines. Wrap errors with `%w`. No panics in the daemon path. No global mutable state.
- Never launch agents with `--dangerously-skip-permissions` (Claude Code refuses it as root). Task handoff = context file + single-line kickoff; never send multi-line text through a pane.
- Merge stays gated on `policies.dry_run` (default-on). GitHub is authoritative. Deterministic branch names `agent/issue-<n>`; pane ids are volatile (re-resolve, never persist).
- Keep `go build ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l` green at every milestone. TDD (red → green), table-driven tests.

---

## Two components

R2 ships as two independently-testable components, **built triage-first** (triage is the pipeline's front door the scheduler feeds):

- **Component A — Executable triage:** make the `intake` state actually run its accept/reject/needs_human decision.
- **Component B — Scheduler daemon:** discover labeled issues and drive up to `max_concurrent_tasks` through the pipeline.

End-to-end loop once both land:

```
[poll gh --label] → intake(triage) ─accept→ queued → implementing → pr_open(review)
                          │reject→ closed                              │approve→ approved
                          │needs_human→ escalated                      │        → merge-gate
                                                                       │        → merging (dry-run halt) / merged
```

---

## Component A — Executable triage

Reuses the **entire** Phase 2a decision path (state entry spawns an agent → `agent.done` → agent's verdict file → engine branches on the verdict). The only new engine logic is feeding the triager the *issue* instead of a *PR*.

### A.1 Config (`internal/config/testdata/default-pipeline.yaml` + the doc-appendix copy)

Restructure `intake` from decision-as-trigger (unsupported: no agent produces the verdict) to the proven review shape, and add the role. The `triage` decision already exists in the config; `emits_to` stays `intake` (the entry).

```yaml
roles:
  triager:
    launch: [claude]
    task_delivery: context_file
    workspace: per_task
    # allowed_tools optional (coarse names only; see T2). Triage only reads the
    # issue + writes a verdict file, so e.g. [Read, Write] is a reasonable scope.

states:
  intake:
    entry: { spawn: triager }
    transitions:
      - when: { event: agent.done }
        evaluate: { decision: triage }
        branch: { accept: queued, reject: closed, needs_human: escalated }
      - when: { timeout: 15m }        # decision/spawn states need a timeout (invariant + warning)
        to: escalated
```

This is schema-valid and passes every invariant: the decision branch keys `{accept, reject, needs_human}` exactly cover the `triage` verdicts; the state has an exit; the added timeout satisfies the spawn-without-timeout warning.

### A.2 Engine (`internal/engine/decision.go`, `engine.go`)

One contained addition — a `triageTask` sibling to `reviewerTask`, and a routing branch in `agentTask`:

```go
// agentTask (engine.go): route a decision state with no PR yet to triage.
if dec := decisionForState(st); dec != "" {
    if task.PRNumber == nil {
        return e.triageTask(task, dec)   // NEW: rubric + issue, no PR pointer
    }
    return e.reviewerTask(task, dec)     // existing: rubric + PR pointer
}

// triageTask (decision.go): rubric + issue title/body (fetched via gh), a
// single-line kickoff to write {"verdict": one of <verdicts>, "feedback": ...}
// to the task's verdict file. Mirrors reviewerTask but references the issue,
// not a PR. Verdict read/parse/branch is unchanged (evaluateDecision).
```

Everything else — `spawn` (creates the `agent/issue-N` worktree + herdr workspace), `WaitState`, `evaluateDecision` (reads/validates the verdict against the decision's declared verdicts), the branch — is reused as-is.

### A.3 Rubric (`prompts/triage.md`)

Accept / reject / needs_human criteria for an incoming issue (e.g., accept = clear, self-contained, agent-ready; needs_human = ambiguous/underspecified; reject = out of scope / duplicate / not actionable).

### A.4 Handoff and cleanup

- **Handoff:** the triager spawns in the `agent/issue-N` worktree. On `accept → queued → implementing`, the implementer spawn closes the triager's same-label herdr workspace and starts fresh (existing `Spawn` behavior). The triager wrote no code, only a verdict file (in `taskDir`), so the worktree recreate at `implementing` loses nothing.
- **Cleanup (named debt):** on `reject`/`needs_human` the task terminates but its `agent/issue-N` worktree/branch is left behind as cosmetic cruft. Cleanup is **deferred** — it is harmless (no PR, no remote branch) and out of MVP scope.

### A.5 Component A testing

- Table test (engine): a task at `intake` with a `fakeBackend` whose triager writes each verdict → assert `accept→queued`, `reject→closed`, `needs_human→escalated`.
- `triageTask` unit test: the context file carries the rubric + issue title/body and **no** `PR #` reference; the kickoff names the verdict file and the allowed verdicts.
- Regression: the existing `review` path (PR present) still routes to `reviewerTask`.

---

## Component B — Scheduler daemon

New `orchestratord daemon` subcommand. Resolves the original single-writer-vs-concurrency fork by **partitioning responsibilities**, not by adding locks.

### B.1 Concurrency model

- **1 poller goroutine — the only task creator.** A ticker (`--poll-interval`, default 30s) lists candidate issues and enqueues those not already handled. Because creation happens on exactly one goroutine, there is no create/create race.
- **N worker goroutines (`N = policies.max_concurrent_tasks`).** Each pulls an issue and calls the **existing `engine.Run(ctx, issue)`** (`ensureTask` → `reconcile` → `drive` to `merged`/halt). Workers never create tasks out of band; `Run`'s `ensureTask` creates the row for a brand-new issue and finds+reconciles the row for a resumed one. One worker owns one issue → tasks are **row-partitioned** (no shared rows).
- **The store serializes writes** (`MaxOpenConns=1`), so row-partitioned concurrent drivers need no additional locking. The engine value `e` is shared **read-only** (drives never mutate its fields; `cloneWithWorkflow` already copies when a per-task graph is needed).

Net: **creation is single-goroutine; driving is partitioned + serialized.** This is why N workers are safe with essentially no new synchronization.

### B.2 Discovery, dedup, and the work queue

- **Source:** `github.Client` gains `ListIssues(ctx, repoDir, label string) ([]int, error)` → `gh issue list --label <label> --json number` in `repoDir`. The label comes from the first `github_issues` source's `select.label`; the daemon errors at startup if the pipeline declares no such source/label.
- **In-flight set:** an in-memory `map[int]struct{}` of issues currently enqueued or being driven, guarded by a mutex. An issue is added when enqueued and removed when its `Run` returns.
- **Dedup rule (poller):** enqueue an issue only if it is **not** in the in-flight set **and** does not already have a terminal task row (a `merged`/`closed`/`escalated` task means "done, don't re-run"; a not-yet-existing task counts as not-terminal). A non-terminal task row that is not in-flight (e.g., after a crash) IS enqueued — that is how in-flight work resumes. The in-flight set is what prevents a currently-driving (non-terminal) task from being re-enqueued to a second worker.
- **Seed on startup:** before the first poll, enqueue every non-terminal task from `store.List` (crash-resume). The worker pool is the single execution path for both new and resumed tasks; the daemon does not call the synchronous `Recover` (that stays for the `recover` CLI subcommand).
- **Queue:** a buffered channel feeds idle workers. The poller enqueues with a non-blocking send; if the buffer is momentarily full it skips (the issue is still labeled and re-discovered next tick), so the poller never blocks.

### B.3 Capacity semantics

`max_concurrent_tasks` bounds concurrent **lifecycles**: at most N issues are being driven end-to-end at once. Excess labeled issues wait in the queue (or are re-discovered next poll) until a worker frees. A worker stays occupied for its task's whole lifecycle, including gate-polling waits (`blocked_on_gate`, up to its state timeout) — see named debt B.6.

### B.4 Lifecycle

- **Start:** wire engine (`startState = entry_state`, `goal = merged`), open store, seed from store, start N workers + the poller.
- **Shutdown (SIGINT/SIGTERM):** cancel the root ctx → poller stops, each `drive()` returns promptly (all blocking calls honor ctx) leaving its task at its **persisted** state; workers drain; daemon exits. **Cancel-and-resume, not drain** (approved): the next start re-enqueues non-terminal tasks and resumes. Canceling does not kill the herdr agents (they keep working; GitHub/herdr state is authoritative and reconciled on resume).

### B.5 Components / interfaces (small, testable)

```go
// internal/scheduler/scheduler.go
type Scheduler struct {
    List     func(ctx context.Context) ([]int, error)  // discover candidate issues (wraps gh.ListIssues)
    Done     func(ctx context.Context, issue int) (bool, error) // true iff a TERMINAL task row exists (dedup; not-found => false)
    RunTask  func(ctx context.Context, issue int) error // drive one issue (wraps eng.Run)
    SeedFrom func(ctx context.Context) ([]int, error)   // non-terminal issues at startup (wraps store.List)
    Interval time.Duration
    Workers  int
    Log      *slog.Logger
}
func (s *Scheduler) Serve(ctx context.Context) error    // seed → start workers + poller → block until ctx done
```

Depending on plain funcs (not the concrete engine/store) keeps the scheduler unit-testable with stubs and free of engine internals. The `daemon` subcommand wires the real `eng.Run`, `gh.ListIssues`, and `store` into those fields.

### B.6 Named debt / safety caveats

- **Per-drive Events pollers:** each `drive()` runs its own `Events()` poller, so N workers = N concurrent `herdr pane list` subprocesses. Correct but slightly wasteful; a shared poller is a future optimization. Fine at `max_concurrent_tasks=4`.
- **Worker-held-during-wait:** a worker is occupied even while its task merely polls a gate. So the cap bounds lifecycles, not just live agents. Releasing workers during waits is future work.
- **Error isolation:** a failed `Run` logs and drops that issue (removed from the in-flight set, so a later poll retries); one task's failure never kills the daemon or its siblings.

### B.7 Component B testing

- **Scheduler unit tests** (stubs, no engine): poll discovers 3 issues → 3 `RunTask` calls; dedup (an in-flight or terminal issue is not re-run); capacity (never more than `Workers` concurrent `RunTask`s — assert via a counting stub with a barrier); seed enqueues non-terminal issues; ctx cancel stops the poller and returns from `Serve`.
- **`ListIssues`** contract test: extend the T3 fake-`gh` integration test with a `gh issue list --label ... --json number` fixture.
- **End-to-end integration:** fake `gh` + fake `herdr` drive one labeled issue `intake → halt`, exercising triage + one worker.

---

## Explicitly out of scope (YAGNI)

LLM-quality triage tuning; dynamic config reload; backpressure beyond the cap; metrics/observability; drain-on-shutdown; releasing workers during gate waits; making `scheduled` an external event; multi-source or non-`github_issues` sources; the MCP surface and cross-task memory (later phases).

---

## Build sequencing

1. **Component A (triage)** — config restructure + `triageTask`/routing + rubric + tests. Deliverable: `orchestratord run --issue N` drives `intake → triage → queued → …` end-to-end.
2. **Component B (scheduler)** — `github.ListIssues`, `scheduler.Scheduler`, `orchestratord daemon` wiring + tests. Deliverable: `orchestratord daemon` polls a label and drives up to `max_concurrent_tasks` concurrently, resuming in-flight work on restart.
