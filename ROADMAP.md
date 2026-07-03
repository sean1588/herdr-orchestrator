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
- **README "out of scope" drift:** the README overview still lists the scheduler /
  concurrency as out of scope, though R2 shipped them. Small living-doc cleanup.

## Pointers

- `docs/superpowers/plans/` — dated implementation plans (Phase 1, 2a, 2b, R2).
- `docs/superpowers/specs/` — dated design specs (R2 scheduler).
- `CLAUDE.md` — build/test/convention rules and the Phase-1 scope discipline.
