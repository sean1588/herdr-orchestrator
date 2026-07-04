# Roadmap

A living tracker of what's built and what's deferred. Dated design/plan docs under
`docs/superpowers/` hold the detail; this file is the index so deferred work doesn't
get lost. Update it as phases land.

## Built

- **Phase 1** — the validated slice: config load + schema/invariant validation, the
  state-graph engine (`queued → implementing → pr_open`), SQLite task store + audit
  log, herdr execution backend, `gh` client, `orchestratord validate|run|recover`.
- **Phase 2a** — the review → auto-merge loop: `pr_open → (review) → approved →
  (merge gate) → merging → merged`; default goal `merged`; the real merge gated on
  `policies.dry_run` (default-on, halts at `merging`).
- **Phase 2b** — hardening: per-task `workflow_snapshot` recovery, per-role
  `allowed_tools` tool-scoping at spawn, fixture-based CLI-contract integration tests,
  the `Notifier` seam, the `plan` subcommand (graph + invariant render), doc honesty.
- **R2** — scheduler daemon + executable triage: `orchestratord daemon` polls a
  labeled source and drives up to `max_concurrent_tasks` concurrently (1 poller / N
  workers / single-writer store, tasks row-partitioned by `issue-<n>`); `intake`
  runs a real `triage` decision (`accept/reject/needs_human`). Dogfooded live.
- **R2 efficiency debts (paid down):**
  - *Shared Events poller* (#22) — one `herdr pane list` poller multiplexed across
    all `Events` subscribers (fan-out + prime-on-join), replacing one poller per drive.
  - *Suspend a merge-gate wait* (#23) — `blocked_on_gate` evaluates the gate once and
    suspends (frees its worker) instead of pinning a slot for a 30m in-process poll;
    the scheduler re-drives it each poll; the timeout is audit-anchored (survives
    restarts).

## Deferred subsystems

Each is its own brainstorm → spec → plan (they carry real design forks — brainstorm
first). Listed in rough priority.

### MCP server surface
- **Scope:** expose orchestrator state/control (list tasks, inspect audit, nudge/cancel)
  over MCP, so an operator or a supervising agent can drive it.
- **Lands on:** `*store.Store` reads (tasks + audit) + a small command surface; the
  engine stays the single writer of task state.
- **Hard questions:** read-only vs control; which mutations are safe to expose;
  auth/loopback posture (localhost-only with optional OAuth is the model to adopt).
- **Reference:** CAO's `mcp_server/` (FastMCP) + REST.

### Cross-task memory / context
- **Scope:** durable context shared across tasks (repo conventions, prior triage
  rationale) that agents can consult, instead of re-deriving each run.
- **Hard questions:** what is worth remembering vs re-deriving; secret-leak prevention
  on agent-authored writes (adopt a `secret_gate`-style deny-list if we persist agent
  content); scope (per-repo vs federated). This is the likely inflection point where a
  Python/LangGraph-style memory sidecar could earn its keep — evaluate as a sidecar,
  not a rewrite.
- **Reference:** CAO's memory feature (markdown wiki + `memory_metadata` + audit log +
  `secret_gate.py`).

## Cross-cutting / opportunistic

- **Push-based herdr events** *(open question):* if herdr exposes `events.subscribe`
  over its unix socket, replace the 2s pane-list poll ticker (and the merge-gate poll)
  with a push stream — lower latency, less work. #22's `eventHub` already built the
  fan-out layer a push source would reuse (swap the ticker for a socket reader,
  subscribers untouched). **Verify the socket API exists first**; if push doesn't
  exist, polling is the correct call — don't build it.
- **Per-provider tool-flag table:** generalize Phase 2b's claude-only `--allowedTools`
  translation into a provider → flag map when a second launcher is added. YAGNI until then.
- **Full live e2e harness:** a live herdr+`gh` smoke test behind a build tag (opt-in,
  not default CI) to catch real end-to-end drift. The fixture tests pin the CLI JSON
  contract; a live run has exercised the real path a handful of times (issue 208/PR #227,
  the R2 dogfood).

## Known small debts (tracked, not urgent)

Efficiency-not-correctness; each barely bites at `max_concurrent_tasks` 3–4. Named here
so they aren't rediscovered from scratch.

- **`eventHub.last` retention** (#22): the shared last-seen map keeps an entry per pane
  observed for the daemon's lifetime (never pruned). Negligible at scale; a push-based
  source would replace the poller wholesale.
- **Per-poll `SeedFrom` full-table scan** (#23): the daemon re-lists all non-settled
  in-store tasks every poll by reading all tasks and filtering in Go. Bounded fix: a
  state-filtered store query (`... WHERE current_state NOT IN (settled)`), if it ever
  matters.
- **Arg-scoped tool specs** (Phase 2b): `allowed_tools` entries with spaces/globs/parens
  (e.g. `Bash(gh pr view:*)`) aren't deliverable — the launch argv is space-joined into
  the pane shell unquoted, so validation rejects shell-unsafe tokens. Needs quoting at
  delivery to support arg-scoping.
- **README "out of scope" drift:** addressed by PR #24 for the README prose. The
  residual drift is in *code* comments a maintainer reads — tracked under
  "Phase-stamped comment drift" in the assessment backlog below.

## Code-quality backlog — from the 2026-07-03 assessment

An 8-lens engineering-philosophy assessment (each finding adversarially verified by a
facts skeptic + a philosophy-fit skeptic, then hand-adjudicated against the source)
surfaced these. Nothing is on fire — the daemon works and was dogfooded; this is
pay-down-before-the-next-subsystem work, ordered by priority within tiers.

### Correctness gaps (close before more lands on top)

- **Webhook notifier can block a drive worker forever** — `internal/notify/notify.go:57`.
  A nil `Client` falls back to `http.DefaultClient` (no `Timeout`), and the daemon ctx
  (`signal.NotifyContext`) carries no deadline, so a hung webhook endpoint pins that
  worker's drive loop until SIGINT — at escalation time, in an unattended daemon,
  contradicting the package's own "must never block the drive loop" contract (line 6).
  Since #23 made workers the concurrency budget, that's a stuck slot. **Fix:** default
  the fallback to `&http.Client{Timeout: 10 * time.Second}` (2 lines). Only bites when a
  real webhook is wired (default notifier is `Nop`).
- **The decision seam over-trusts the temp-dir verdict file** — `internal/engine/decision.go`.
  Two facets of one gap: (a) `verdictPath` is keyed only on `task.ID` and never cleared
  before a new round (line 24), so a round-2 reviewer that reaches herdr's heuristic
  `done` *without writing* (a dead-pane failure this repo has seen) lets `evaluateDecision`
  silently re-consume the round-1 verdict → wrong branch with legitimate-looking audit
  rows; (b) `feedbackTask:130` swallows a missing-file read → implementer resumes with
  empty feedback, burning a capped retry, and it's the one un-logged swallow in an engine
  that logs every other. **Fix:** `os.Remove(vp)` before spawning a decision agent (turns
  done-without-writing into a loud read error) + log the read failure. This is the
  silent-wrong-judgment class — worth prioritizing on principle.

### Structural cleanup (natural first step of the MCP / second-backend work)

- **`ExecutionBackend` carries 3 dead methods** — `internal/exec/backend.go:57/59/68`
  (`WaitState`, `Read`, `Close`). Zero production callers (engine uses only
  `Spawn`/`Events`/`Resolve`/`Cleanup`); pre-Events and pre-#20 leftovers. Every test
  fake stubs them and the roadmapped container backend would have to implement them, on
  the one interface `CLAUDE.md` explicitly calls "small." Dead code, no failure mode.
  **Fix:** delete from the interface + `Herdr` (keep `Read` as a concrete `*Herdr` method
  if wanted for debugging).
- **"Settled/resume" semantics are forked between the engine and `main`/daemon, and the
  forks already disagree** — `cmd/orchestratord/main.go:502` + `internal/engine/engine.go:138`.
  (a) `main.settledStates` re-derives engine halt knowledge (dry-run halts at `merging`
  without transitioning), while `engine.Recover`'s `isHalt` omits that halt — so
  `orchestratord recover` re-drives every dry-run-completed task and appends a redundant
  `merging→merging` audit row each run. (b) `WorkflowSnapshot` (persisted so recovery
  "resumes against this, never a possibly-edited --config", `store/task.go:23`) is
  consumed *only* in `Recover`; the daemon's own restart path `Run`→`ensureTask` drives
  against live `e.wf` and never reads it, so the documented safety invariant is false in
  the primary operating mode (and a renamed state makes the daemon error every poll
  forever, never escalating). **Fix:** one engine-owned `Settled(state)` predicate + a
  single snapshot-honoring resume path, consumed by `Recover`, `doneChecker`, `SeedFrom`,
  and `cmdRun`'s exit switch. Small blast radius today, but it's the core seam and
  already drifting.

### Tests

- **The concurrency-isolation seam is untested** — `internal/engine/engine.go:411`. Since
  #22's shared hub broadcasts every pane's events to every subscriber,
  `if ev.PaneID != task.PaneID { continue }` is the only thing isolating concurrent
  drives — yet deleting that line passes the entire suite (the fakes replay a single-pane
  stream, more forgiving than the real hub). **Fix:** one interleaving-event test (a
  foreign pane's `done` before the task's own, with a mutable `fakeGH`) asserting the
  drive still lands correctly and evaluates the gate exactly once.

### Minor / docs (fold into the next docs touch)

- **Phase-stamped comment drift:** PR #24 corrected the README prose but left stale phase
  stamps in code comments — `internal/config/types.go:24` ("R2 deferred"),
  `internal/scheduler/scheduler.go:27` (`Done` documented as "terminal", actually
  *settled*), `internal/exec/backend.go:24` and `internal/engine/engine.go:67` ("Phase 1").
- **Gate params `head` / `all_passing` are typed and documented as thresholds but
  `gatePass` ignores them** — `internal/config/types.go:77`. A silent no-op on the
  merge-safety gates (`all_passing: false` fails closed by accident, not design).
  **Fix:** delete the fields + correct the comment, or consume them.
- **Small pure-deletion duplication:** `cycleBounded` re-implements the validator's cycle
  classification (`cmd/orchestratord/main.go:234`); two hand-rolled `contains()` vs
  `slices.Contains` (`internal/config/helpers.go:23`, `internal/engine/decision.go:163`);
  the verdict-protocol string duplicated verbatim (`internal/engine/decision.go:90`).

### Considered and declined (recorded so they aren't re-opened)

- **A store single-writer concurrency test** — a naive N-goroutine test would be
  tautological: `busy_timeout(5000)` absorbs distinct-row (row-partitioned) write
  contention *regardless* of `SetMaxOpenConns(1)`, so it would pass with or without the
  contract it claims to pin — false confidence. The path already runs under `go test -race`.
- **Converging the two state-timeout mechanisms** (`internal/engine/engine.go:390`) —
  same-shape/different-reasons: `blocked_on_gate` is audit-anchored *because* it suspends
  (no in-process continuity); `implementing` uses an in-process `time.NewTimer` *because*
  it blocks in-process. Converging adds a fallible store read to the hot path to defend a
  rare compound edge (repeated restarts while an agent hangs).
- **`reconcile` hardcoding `"implementing"`** (`internal/engine/engine.go:657`) — a form
  nit, not a bug. Under the shipped config it is correct: the only other agent-state,
  `changes_requested`, already has `PRNumber != nil` and so needs no PR short-circuit.
  Only renamed/alternate configs (out of scope per `CLAUDE.md`) would lose it; `line 667`
  is a free `task.CurrentState` swap if the block is ever touched.

## Pointers

- `docs/superpowers/plans/` — dated implementation plans (Phase 1, 2a, 2b, R2).
- `docs/superpowers/specs/` — dated design specs (R2 scheduler).
- `CLAUDE.md` — build/test/convention rules and the Phase-1 scope discipline.
