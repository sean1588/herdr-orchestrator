# Herdr Orchestrator — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans (inline) — steps use checkbox (`- [ ]`) tracking.

**Goal:** A Go control-plane daemon that drives one GitHub issue through `queued → implementing → pr_open` by spawning an implementer agent in an isolated git worktree via herdr and detecting the resulting PR via `gh`, all driven by a declarative YAML workflow.

**Architecture:** A state-graph engine interprets a validated `default-pipeline.yaml`. Workflow types are defined once in `internal/config` (loaded + JSON-Schema-validated + invariant-checked). The engine depends only on small interfaces: `exec.ExecutionBackend` (herdr impl), `github.Client` (gh impl), and a `*store.Store` (SQLite, single-writer). External commands run through a mockable `internal/proc.Runner`. The engine is event-driven over `backend.Events()` and treats GitHub as authoritative for artifacts.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go), `gopkg.in/yaml.v3`, `github.com/santhosh-tekuri/jsonschema/v6`. herdr + `gh` via `os/exec`.

## Global Constraints (verbatim from spec)

- Module: single Go module at repo root. `go build ./...`, `go test ./...`, `go vet ./...`, `gofmt -l -w .` all green at every milestone. No skipped tests.
- Trigger key in configs is **`when`**, never `on`.
- Branch names deterministic: **`agent/issue-<n>`**. Reconcile by these on restart.
- Pane ids are **volatile** — re-resolve from create/list/events; never persist as a durable key.
- **Never** launch agents as root with `--dangerously-skip-permissions`. Honor `run_as: non_root` + `sandbox`. Phase 1 launches `role.launch` verbatim and never injects skip-permissions.
- Task handoff is **file + single-line kickoff**, never an inline multi-line prompt.
- Engine is the **single writer** of task state. GitHub is **authoritative** for artifacts; agent `done` is only a trigger-to-check.
- **Phase 1 only.** Stub/omit: review agent + `review` decision, merge gate + `approved`/`merging`, triage/`intake` decision, scheduler/concurrency>1, MCP server, context memory. The engine must still *parse + validate* the full pipeline.
- No dependencies beyond the four listed. Wrap errors with `%w`. `context.Context` first arg of anything I/O. No panics in daemon path. No global mutable state.

## File Structure

```
go.mod / go.sum
cmd/orchestratord/main.go        # CLI: validate | run | recover
internal/proc/
  runner.go                      # Runner interface + os/exec impl
  fake.go                        # Fake runner for tests (scripted argv->output)
internal/config/
  types.go                       # Workflow + all nested types (the single source of workflow types)
  unmarshal.go                   # GateRef + Trigger/Evaluate YAML decoding (string-or-array gate)
  schema.go                      # //go:embed workflow.schema.json ; compiled validator
  workflow.schema.json
  load.go                        # Load(path) (*Workflow, []string warnings, error)
  validate.go                    # semantic invariants (port of validate_workflow.py)
  graph.go                       # transition graph + Tarjan SCC (cycles) + reachability
  testdata/default-pipeline.yaml
  testdata/broken-pipeline.yaml
internal/exec/
  backend.go                     # ExecutionBackend, Spawn, Handle, Event, AgentState
  herdr.go                       # herdr backend (proc.Runner + socket for events)
internal/github/
  client.go                      # Client, PR, Issue
  gh.go                          # gh CLI impl (proc.Runner)
internal/store/
  store.go                       # *Store over modernc sqlite; single-writer; reconcile queries
  schema.sql                     # embedded DDL
  task.go                        # Task, AuditEntry, state constants
internal/engine/
  engine.go                      # interpreter: Run(issue) + Recover(); entry actions; trigger wait
docs/superpowers/plans/...md     # this file
```

Decisions locked:
- One copy of each YAML fixture, in `internal/config/testdata/`. The `run` CLI takes `--config <path>`; integration points it at that file. Schema embedded from `internal/config/workflow.schema.json` (go:embed can't reach `..`, so schema lives in-package).
- `pr_open` is the Phase-1 goal/halt state. The engine halts on entering a terminal state OR the goal state, and does NOT execute `pr_open`'s `spawn: reviewer` entry (out of scope). **Named debt:** a real engine continues into review.

---

## Shared interfaces (the form — get these right first)

### internal/proc
```go
package proc
type Runner interface {
    // Run executes name+args with working dir `dir` ("" = inherit). Returns stdout.
    // On non-zero exit, returns stdout and an error wrapping stderr.
    Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}
func New() Runner // os/exec impl
// Fake: records calls, returns scripted results keyed by a matcher; in fake.go (test helper, but exported for cross-package tests).
```

### internal/exec  (from brief §3.1, verbatim shapes)
```go
type AgentState string // "working" | "blocked" | "done" | "idle" | "unknown"
type Spawn struct { TaskID, Role, Branch, RepoDir, Base, TaskFile string; Launch []string; Kickoff string }
type Handle struct { PaneID, Workdir string }
type Event struct { PaneID string; State AgentState }
type ExecutionBackend interface {
    Spawn(ctx, Spawn) (Handle, error)
    WaitState(ctx, Handle, target AgentState) (AgentState, error)
    Read(ctx, Handle, lines int) (string, error)
    Events(ctx) (<-chan Event, error)
    Close(ctx, Handle) error
}
```

### internal/github
```go
type PR struct { Number int; URL, State string }
type Issue struct { Number int; Title, Body string }
type Client interface {
    FindPR(ctx, repoDir, branch string) (*PR, error) // (nil,nil) = none
    Issue(ctx, repoDir string, number int) (*Issue, error)
}
```

### internal/store
```go
type Task struct {
    ID, Repo, Branch, CurrentState, PaneID string
    Issue int
    PRNumber *int
    RetryCounts map[string]int
    CreatedAt, UpdatedAt time.Time
}
type AuditEntry struct { TaskID, FromState, ToState, Trigger, Result string; TS time.Time }
// *Store methods: Open(path), CreateTask, GetTask, ListActive, UpdateTask, AppendAudit, Audit(taskID), Close.
```

### internal/engine
```go
type Engine struct { wf *config.Workflow; backend exec.ExecutionBackend; gh github.Client; store *store.Store;
                     repoDir, base, worktreesDir string; clock func() time.Time; log *slog.Logger }
func New(...) *Engine
func (e *Engine) Run(ctx, issue int) (finalState string, err error)   // starts at "queued", halts at goal/terminal
func (e *Engine) Recover(ctx) error                                    // reconcile active tasks, resume
```

---

## Milestones / Tasks

### M0 — Scaffold + config load/validate
**Files:** go.mod, internal/proc/*, internal/config/* (types, unmarshal, schema, load, validate, graph), testdata fixtures, cmd/orchestratord/main.go (`validate` subcommand only).
**Tests:** golden — `default-pipeline.yaml` passes with exactly 2 warnings; `broken-pipeline.yaml` fails invariants **1, 2, 5, 6**. Table tests for each invariant in isolation (small inline configs). YAML decode tests (gate string-vs-array, trigger one-key).
**Invariants to port (validate.go):** 1 refs-resolve, 2 decision branch keys == verdicts, 3 gate branch keys == {pass,fail}, 4 gate types ∈ authoritative set, 5 merge-only-by-gate, 6 every cycle has retry_cap or timeout, 7 every non-terminal state has exit. Warnings: unreachable-from-entry_state, spawn/resume state with no timeout transition.
**Acceptance:** `orchestratord validate internal/config/testdata/default-pipeline.yaml` → OK (2 warnings, exit 0); `...broken-pipeline.yaml` → FAIL listing invariants 1/2/5/6, exit 1.

### M1 — herdr backend
**Files:** internal/exec/backend.go, internal/exec/herdr.go.
**Approach:** Construct the proven spike0 commands through `proc.Runner`:
- Spawn: `git -C <repo> worktree add -b agent/issue-<n> <wt> <base>` (clean prior attempt first: `worktree remove --force` + `branch -D`, ignore errors); `herdr workspace create --cwd <abs-wt> --label issue-<n>` → parse `result.root_pane.pane_id` from JSON; `herdr pane run <pane> "<launch>"`; readiness wait; `herdr pane run <pane> "<kickoff>"`.
- WaitState: `herdr wait agent-status <pane> --status <target> --timeout <ms>`.
- Read: `herdr pane read <pane> --source recent --lines N`.
- Events: subscribe to herdr `pane.agent_status_changed` (socket, newline-delimited JSON) → `<-chan Event`. **Verify herdr's exact event/CLI surface via the herdr skill before coding.**
**Tests:** inject `proc.Fake`; assert exact argv for Spawn/WaitState/Read; assert pane id parsed from sample `workspace create` JSON; assert branch name format. Never hardcode pane id format (parse from output).
**Acceptance (unit):** command-construction tests pass. (Live spike-replication deferred to integration in M5.)

### M2 — GitHub adapter
**Files:** internal/github/client.go, internal/github/gh.go.
**Approach:** `FindPR`: `gh pr list --head <branch> --json number,url,state` run with dir=repoDir → parse JSON array → first elem or nil. `Issue`: `gh issue view <n> --json number,title,body`.
**Tests:** `proc.Fake` returns sample gh JSON; assert PR parsed, empty array → (nil,nil), assert argv + cwd.

### M3 — Store
**Files:** internal/store/{store.go, task.go, schema.sql}.
**Approach:** modernc sqlite, `_pragma=busy_timeout`, MaxOpenConns(1) to serialize. Tables: `tasks` (id PK, issue, repo, branch, current_state, pr_number NULL, pane_id, retry_counts JSON, created_at, updated_at), `audit` (id PK, task_id FK, ts, from_state, to_state, trigger, result). RetryCounts as JSON column.
**Tests:** round-trip Create→Get; UpdateTask mutates + bumps updated_at; AppendAudit + Audit ordering; ListActive excludes terminal states; PRNumber nil/non-nil.

### M4 — Engine (the slice)
**Files:** internal/engine/engine.go.
**Approach:** generic interpreter restricted to slice trigger kinds (event, timeout, gate; decision → explicit "Phase 1 not implemented" error if ever reached). Loop from `queued`:
- `queued`: auto-fire `scheduled` (no scheduler in Phase 1) → `implementing`.
- `implementing`: build Spawn (write issue task file via gh.Issue; Kickoff = single-line spike0-style referencing TaskFile+branch+base; Launch from role). `backend.Spawn`. Then select over `backend.Events()` filtered to pane, with deadline = parsed `timeout` (45m): `done`→evaluate gate `pr_exists` (gh.FindPR(branch)) → pass `pr_open` / fail `escalated`; `blocked`→record alert audit, keep waiting; deadline→`escalated`.
- Halt on entering goal (`pr_open`) or any terminal. On halt at pr_open, persist PRNumber, return "pr_open".
- Every transition writes an audit row (from,to,trigger,result). All writes on the single Run goroutine.
**Tests:** transition-table — inject fake backend (scripted Events) + fake gh + temp sqlite; assert (state,event,gate-result)→next state for: done+PR→pr_open, done+noPR→escalated, blocked→stays+alert audit, timeout→escalated. Assert audit log contents.

### M5 — Run CLI + crash recovery
**Files:** cmd/orchestratord/main.go (`run`, `recover` subcommands), engine.Recover.
**Approach:** `run --config <p> --repo <dir> --base main --issue <n> --db <path>` wires real proc/exec/github/store + engine, runs to pr_open, prints final state + PR. `recover` / `run` startup: `store.ListActive` → for each, re-resolve pane (herdr workspace list by label `issue-<n>`), `gh.FindPR(branch)`; if PR exists and state=implementing, advance to pr_open; else resume waiting. Reconcile by deterministic branch name.
**Tests (unit):** Recover with fake backend+gh+seeded store: active task in `implementing` with PR present → reconciled to `pr_open`; no PR → stays/resumes. 
**Acceptance (integration, needs live herdr+repo+gh):** `orchestratord run ... --issue <n>` drives a trivial issue to pr_open detecting the real PR; restart mid-run reconciles.

---

## Self-review notes
- Spec coverage: invariants 1–7 + 2 warnings (M0); ExecutionBackend full interface (M1); FindPR+Issue (M2); task+audit+reconcile (M3); slice + blocked/timeout (M4); run+recover CLI (M5). ✓
- Out-of-scope states (intake/triage, approved/merging, changes_requested/review, blocked_on_gate) are parsed+validated but never executed; decision trigger → explicit not-implemented error. ✓
- Type consistency: workflow types defined once in config; engine/store/exec/github interfaces fixed above and referenced verbatim by later tasks. ✓
