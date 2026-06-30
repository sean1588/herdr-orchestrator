# Phase 2a — Review → Auto-merge Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:test-driven-development for every task. Steps use checkbox (`- [ ]`) syntax. Keep `go build ./...`, `go test -race ./...`, `go vet ./...`, and `gofmt -l .` green at every milestone boundary (this is what CI enforces).

**Goal:** Drive a task past `pr_open` through the rest of `default-pipeline.yaml` — reviewer agent → `review` decision → (changes loop | merge gate) → `merge_pr` → `merged` — so an issue can reach a merged PR unattended, with the merge gated on `policies.dry_run`.

**Architecture:** The Phase 1 engine already interprets the full graph but halts at `pr_open` and errors on decisions / `resume` / `action`. Phase 2a fills those in: a deterministic **decision evaluator** (reads a verdict the reviewer agent writes — the engine never judges), a **GitHub status poller** that turns `status.changed` into periodic merge-gate re-evaluation, the three merge-gate reads on `github.Client`, agent **resume**, retry-cap enforcement, and the `merge_pr` action. The engine stays the single writer; GitHub stays authoritative; the safety invariants (merge reachable only through a gate) are already enforced by the validator and unchanged.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, `gopkg.in/yaml.v3`, `santhosh-tekuri/jsonschema/v6`; herdr + `gh` shelled via `internal/proc.Runner`.

## Global Constraints

- No new third-party dependencies.
- `context.Context` first arg on every blocking call; honor cancellation/deadlines (poll intervals + state timeouts are config-driven).
- Wrap errors with `%w`; no panics on the daemon path; no global mutable state; engine is the single writer of task state.
- Parse all ids/states from command output; never hardcode formats. herdr pane ids stay volatile (re-resolve, never persist as a key).
- Agents launched non-root, never `--dangerously-skip-permissions`; task handoff is a context file + single-line kickoff, never inline multi-line.
- **`merge_pr` honors `policies.dry_run`** (nil ⇒ default-on): dry-run logs intent and halts without merging; only `dry_run: false` performs `gh pr merge`.
- The reviewer/`review` decision is **deterministic at the engine layer**: the engine reads a verdict ∈ the decision's declared `verdicts`; it does not itself call an LLM.

---

## Design decisions (resolved)

1. **Decision evaluation = verdict file.** On `pr_open` entry the engine spawns the reviewer with a task file = the decision's rubric (resolved relative to the config dir) + PR context, and a kickoff instructing it to write `verdict-<task>.json` = `{"verdict": "...", "feedback": "..."}`. On `agent.done` the engine reads + validates `verdict ∈ decision.Verdicts` and branches. `impl.type: exec` (triage, later) will instead run a command and read stdout — the evaluator is an interface with an `llm` impl now.
2. **`status.changed` = GitHub poll.** `approved`/`blocked_on_gate` poll checks/reviews/mergeable on `StatusPollInterval` until the merge gate passes (→`merging`) or `StatusTimeout` elapses (→`escalated`). A failed gate stays in the wait (the `blocked_on_gate` condition); we do not require the YAML to name an explicit re-eval edge — `wait_for: status.changed` is interpreted as "re-evaluate on the next poll".
3. **Merge-gate reads are authoritative.** `github_checks`→`gh pr checks`, `github_reviews`→`gh pr view --json reviewDecision`, `github_mergeable`→`gh pr view --json mergeable,mergeStateStatus`. Added to `github.Client`.
4. **Resume reuses the live pane.** `changes_requested` resolves the implementer's pane via `Resolve`; if live, send a feedback kickoff; else re-spawn on the branch. Feedback travels as a file the engine writes from the verdict.
5. **Retry cap** increments `Task.RetryCounts["changes_requested"]` on entry to that state; over `policies.retry_caps.changes_requested` fires `retry_exhausted` → `escalated`.
6. **`merge_pr`** runs `gh pr merge --squash --delete-branch` (only when `dry_run:false`), then verifies `state == MERGED` (authoritative) before `→ merged`.
7. **Goal** becomes `merged`; the engine halts on any terminal state. `reconcile`'s implementing short-circuit targets `pr_open` explicitly (not the goal).

---

## File structure

- `internal/github/client.go` — extend `Client`: `Checks`, `Reviews`, `Mergeable`, `PRState` (merged?), `Merge`. New value types `Checks`, `ReviewState`, `Mergeability`.
- `internal/github/status.go` (new) — gh impls of the three reads + merged-state.
- `internal/github/merge.go` (new) — `Merge` impl.
- `internal/exec/backend.go` — add `Resume(ctx, h Handle, kickoff string) error` (send a single-line kickoff to an existing pane).
- `internal/exec/herdr.go` — implement `Resume` via `herdr pane run`.
- `internal/engine/decision.go` (new) — decision evaluator (verdict-file read + validate), rubric resolution, reviewer task/kickoff.
- `internal/engine/merge.go` (new) — `approved` status-poll gate loop, `merge_pr` action, `pr.merged` verify.
- `internal/engine/engine.go` — wire decisions into `runState`; `resume`; retry-cap on entry; goal→merged; `evalGate` for the three new gate types; reconcile target fix.
- `internal/config/types.go` — none expected (decisions/gates/actions already modeled); add `Policies.DryRunEnabled()` helper.
- `cmd/orchestratord/main.go` — pass `ConfigDir`; update `run` exit-code success state to `merged` (and accept `pr_open`/dry-run halt as non-error where appropriate).
- `internal/config/testdata/prompts/review.md` (new) — default review rubric.
- `internal/config/testdata/default-pipeline.yaml` — possibly add an explicit `blocked_on_gate` re-eval transition (decide during M9; keep validator green either way).
- `README.md` — document the full loop, the verdict-file contract, dry-run, the rubric path resolution.

## Interfaces (consumed across tasks)

```go
// github.Client additions
type Checks struct { Total, Passed, Failed, Pending int }
func (c Checks) AllPassing() bool { return c.Failed == 0 && c.Pending == 0 && c.Total > 0 }
type Mergeability struct { Mergeable bool; State string } // State e.g. "CLEAN","BLOCKED","DIRTY"
Checks(ctx, repoDir string, pr int) (*Checks, error)
Reviews(ctx, repoDir string, pr int) (approved int, decision string, err error) // decision: APPROVED/REVIEW_REQUIRED/CHANGES_REQUESTED
Mergeable(ctx, repoDir string, pr int) (*Mergeability, error)
PRState(ctx, repoDir string, pr int) (string, error) // OPEN/MERGED/CLOSED
Merge(ctx, repoDir string, pr int) error             // gh pr merge --squash --delete-branch

// exec.ExecutionBackend addition
Resume(ctx context.Context, h Handle, kickoff string) error

// engine internals
func (e *Engine) evaluateDecision(ctx, task, *config.Transition) (verdict string, err error)
func (e *Engine) awaitGate(ctx, task, st) (next, trigger, result string, err error) // approved/blocked_on_gate poll
```

---

## Milestones

### M6 — github merge-gate reads (`Checks`, `Reviews`, `Mergeable`, `PRState`, `Merge`)
Independent leaf package; table-driven tests with `proc.Fake` asserting argv + parsing. Each read parses `gh ... --json` output; `Merge` asserts the `gh pr merge --squash --delete-branch <n>` argv. Done when the new `Client` methods exist, are covered, and `go test ./internal/github/...` is green.

### M7 — decision evaluator + reviewer (`pr_open` executes)
- Verdict-file read+validate (`evaluateDecision`); reviewer rubric resolution (relative to `ConfigDir`); reviewer task file (rubric + PR context) + verdict-writing kickoff; spawn differentiates implementer vs reviewer.
- `runState`: a transition with a `decision` ref (in `when` or `evaluate`) after `agent.done` evaluates the decision and branches on the verdict.
- Tests: fake backend fires `agent.done`; a seeded verdict file drives `pr_open`→{approved|changes_requested|escalated}; invalid/missing verdict → error.

### M8 — `changes_requested` (resume + retry cap)
- `resume` (Resolve live pane → feedback kickoff, else re-spawn); engine writes feedback file from the verdict.
- Retry-cap increment on entry; over cap → `retry_exhausted` → `escalated`.
- `agent.done` → gate `pr_exists` → `pr_open` (loop). Tests: loop once then succeed; loop past cap → escalated.

### M9 — `approved` + merge gate via status poll
- `evalGate` for `github_checks`/`github_reviews`/`github_mergeable`.
- `awaitGate`: poll until pass→`merging` or timeout→`escalated`; `blocked_on_gate` shares the loop. Config: `StatusPollInterval` (default 15s), `StatusTimeout` (default 30m), injectable for tests.
- Tests (fake github): fail→fail→pass reaches merging; never-pass → timeout → escalated.

### M10 — `merging` + `merge_pr` (dry-run honored)
- `merge_pr` action: if `dry_run` (default-on) log "would merge PR #N" and halt as dry-run-done; else `Merge` then verify `PRState==MERGED`, fire `pr.merged` → `merged`.
- Goal→`merged`; `reconcile` implementing target→`pr_open`; CLI exit-code success states updated.
- Tests: dry_run true → halts pre-merge, no `Merge` call; dry_run false → `Merge` called, verified, `merged`.

### M11 — full-loop wiring + end-to-end test + docs
- One table-driven engine test drives a fake issue from `queued` all the way to `merged` (dry_run:false) and to the dry-run halt (dry_run:true), asserting the audit trail.
- Default review rubric committed; README updated; `go test -race ./...`, `vet`, `gofmt` all green; commit per milestone.

## Self-review notes
- Spec coverage: every non-terminal state in `default-pipeline.yaml` past `pr_open` now has an engine path (pr_open, changes_requested, approved, blocked_on_gate, merging) + terminals.
- Type consistency: `Checks`/`Mergeability`/verdict JSON names are fixed above and reused by M7/M9/M10.
- Safety: `merge_pr` is reached only via the gate (validator-enforced) and further gated on `dry_run`; the engine never merges on a decision/event alone.
