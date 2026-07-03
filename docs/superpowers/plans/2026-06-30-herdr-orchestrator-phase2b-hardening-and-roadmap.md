# Herdr Orchestrator — Phase 2b (Hardening & Operability) + Post‑2a Roadmap

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement Phase 2b task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. The **Roadmap** section is intentionally NOT broken into bite-sized tasks — each roadmap item gets its own plan when it is picked up.

**Goal:** Close the operability/correctness gaps surfaced by the cross-project comparison (vs `awslabs/cli-agent-orchestrator` and the alettieri orchestrator-daemon spec), then sequence the remaining deferred subsystems after Phase 2a.

**Architecture:** Phase 2b is a set of small, independent hardening changes to the *existing* Phase 1+2a engine — no new subsystem, no inversion of the mechanism/policy thesis. Each lands behind the same seams we already have (`*store.Store`, `exec.ExecutionBackend`, `github.Client`, `config.Workflow`). The Roadmap (Phase 3+) covers the larger deferred subsystems (triage, scheduler/daemon, MCP, memory), each of which is its own plan.

**Tech Stack:** Go 1.26, single module `github.com/sean1588/herdr-orchestrator`, pure-Go static binary. Deps already present: `modernc.org/sqlite`, `gopkg.in/yaml.v3`, `github.com/santhosh-tekuri/jsonschema/v6`. **No new dependencies** are required by Phase 2b.

## Global Constraints

Copied from `CLAUDE.md` + established session conventions — every task's requirements implicitly include these:

- **No new dependencies.** Phase 2b uses only the stdlib + existing deps. (The webhook notifier uses `net/http`.)
- **Keep green at every task:** `go build ./...`, `go test ./...` (and `go test -race ./...`), `go vet ./...`, `gofmt -l` must all pass. No skipped tests. TDD: write the failing test first, watch it fail, then implement.
- **Small interfaces at package boundaries.** The engine depends on interfaces, never on herdr/`gh`/a notifier concretely.
- `context.Context` is the first arg of anything that does I/O or can block; honor cancellation/deadlines.
- Wrap errors with `fmt.Errorf("...: %w", err)`. **No panics in the daemon path. No global mutable state.**
- The store is the **single writer** of task state. The engine owns every transition; **GitHub is authoritative** for artifacts. Merge stays **statically gate-only-reachable** and gated again by `dry_run`.
- Agents are **never** launched with `--dangerously-skip-permissions`; honor `run_as: non_root`.
- Task handoff = **context file + single-line kickoff**; never inline a multi-line body through a pane.
- Deterministic ids: task `issue-<n>`, branch `agent/issue-<n>`; pane ids are volatile (re-resolve, never persist as a durable key).
- Commit message trailers (every commit):
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Mkou7Ub33gLsthfNosLDQK
  ```

---

# Phase 2b — Hardening & Operability

Ordered by value. T1 is a latent **correctness** bug; T2 is the one structural thing CAO does better (per-agent tool scoping); T3/T4 close the maturity gaps (live CLI-shape coverage, escalation visibility); T5/T6 are auditability/cleanup.

| Task | What | Source of the idea | Risk |
|------|------|--------------------|------|
| T1 | Snapshot the validated workflow into each task; `recover` resumes against the snapshot, not fresh `--config` | alettieri spec | correctness |
| T2 | Per-role `allowed_tools` → native tool-scoping flag at spawn | AWS CAO | security (defense-in-depth) |
| T3 | Integration tests vs a fake `herdr`/`gh` binary on `PATH` | both | maturity |
| T4 | `Notifier` seam → forward escalation/alert out-of-band (webhook) | AWS CAO | operability |
| T5 | `plan` subcommand: render the resolved graph + invariants | alettieri spec | auditability |
| T6 | Doc/comment refresh (stale "halts at pr_open" package docs) + `max_concurrent_tasks` honesty | — | hygiene |

---

## Task T1a: Persist a `workflow_snapshot` column on tasks

**Why:** `recover` re-reads `--config` fresh (`cmd/orchestratord/main.go` `wire`), so editing the YAML between `run` and `recover` resumes an in-flight task against a *different* graph — silently violating the invariants we statically checked at launch. Step one is to give each task a column to carry the graph it started under. (T1b writes it; T1c reads it.)

**Files:**
- Modify: `internal/store/task.go` (add field)
- Modify: `internal/store/store.go` (schema, migration, INSERT, SELECT×2, scanTask)
- Test: `internal/store/store_test.go` (round-trip)

**Interfaces:**
- Produces: `store.Task.WorkflowSnapshot string` — the exact config bytes the task started under (empty for legacy rows). Immutable after create (never written by `UpdateTask`).

- [ ] **Step 1: Write the failing test** in `internal/store/store_test.go`:

```go
func TestTask_WorkflowSnapshot_RoundTrips(t *testing.T) {
	st := newTestStore(t) // existing helper that opens a temp-file store
	ctx := context.Background()
	want := "version: 0\nname: x\nstates: {}\n"
	in := &store.Task{ID: "issue-1", Issue: 1, Repo: "o/r", Branch: "agent/issue-1",
		CurrentState: "queued", WorkflowSnapshot: want}
	if err := st.CreateTask(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkflowSnapshot != want {
		t.Errorf("snapshot = %q, want %q", got.WorkflowSnapshot, want)
	}
	// Snapshot is immutable: a state change must not drop it.
	got.CurrentState = "implementing"
	if err := st.UpdateTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetTask(ctx, "issue-1")
	if again.WorkflowSnapshot != want {
		t.Errorf("snapshot lost after update: %q", again.WorkflowSnapshot)
	}
}
```
(If `newTestStore` does not exist, reuse the engine package's `newStore` pattern: `store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))`.)

- [ ] **Step 2: Run it, watch it fail.** Run: `go test ./internal/store/ -run WorkflowSnapshot -v` → FAIL (unknown field `WorkflowSnapshot`).

- [ ] **Step 3: Add the field** in `internal/store/task.go`, after `PaneSpawnState`:

```go
	// WorkflowSnapshot is the exact config the task started under (raw bytes).
	// Recovery resumes against this, never a possibly-edited --config. Set once
	// at create; never rewritten. Empty for tasks created before this column.
	WorkflowSnapshot string
```

- [ ] **Step 4: Wire the column** in `internal/store/store.go`:
  - `schema` CREATE TABLE: add `workflow_snapshot TEXT NOT NULL DEFAULT ''` (after `pane_spawn_state`).
  - `applyMigrations`: add a second idempotent ALTER, mirroring `pane_spawn_state`:
    ```go
    const addSnapshot = `ALTER TABLE tasks ADD COLUMN workflow_snapshot TEXT NOT NULL DEFAULT ''`
    if _, err := db.ExecContext(ctx, addSnapshot); err != nil && !strings.Contains(err.Error(), "duplicate column") {
        return fmt.Errorf("store: migrate workflow_snapshot: %w", err)
    }
    ```
  - `CreateTask` INSERT: add `workflow_snapshot` to the column list, a `?`, and `t.WorkflowSnapshot` to the args.
  - `GetTask` + `List` SELECT: add `workflow_snapshot` to the column list (keep ordering consistent with `scanTask`).
  - `scanTask`: scan it into `&t.WorkflowSnapshot` (append to the Scan dest list).
  - **Do not** add it to `UpdateTask` (immutable after create).

- [ ] **Step 5: Run tests.** Run: `go test ./internal/store/ -v` → PASS (new + existing). Then `go test ./...` stays green (engine creates `Task{}` without the field → empty snapshot, fine).

- [ ] **Step 6: Commit** (`feat(store): add immutable workflow_snapshot column`).

---

## Task T1b: Write the snapshot at task creation

**Files:**
- Modify: `internal/config/load.go` (export `Parse`)
- Modify: `cmd/orchestratord/main.go` (`wire`: read bytes, pass to engine)
- Modify: `internal/engine/engine.go` (`Config.WorkflowSource`, store on create)
- Test: `internal/engine/engine_test.go` (created task carries snapshot)

**Interfaces:**
- Produces: `config.Parse(data []byte) (*config.Workflow, []string, error)` — exported wrapper over the existing unexported `parse`; same validation, no file I/O.
- Produces: `engine.Config.WorkflowSource []byte` — the raw config bytes the engine snapshots onto each new task (optional; empty ⇒ no snapshot, preserving today's behavior).

- [ ] **Step 1: Failing test** in `internal/engine/engine_test.go`:

```go
func TestRun_NewTask_RecordsWorkflowSnapshot(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{{PaneID: "w1:p1", State: exec.StateDone}}}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}, 5*time.Second)
	e.goal = "pr_open"
	e.workflowSource = []byte("SNAPSHOT-BYTES") // unexported test hook set directly

	if _, err := e.Run(context.Background(), 7); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := st.GetTask(context.Background(), "issue-7")
	if got.WorkflowSnapshot != "SNAPSHOT-BYTES" {
		t.Errorf("snapshot = %q, want SNAPSHOT-BYTES", got.WorkflowSnapshot)
	}
}
```

- [ ] **Step 2: Run, watch fail.** `go test ./internal/engine/ -run RecordsWorkflowSnapshot` → FAIL (no `workflowSource` field).

- [ ] **Step 3: Engine plumbing** in `internal/engine/engine.go`:
  - `Config`: add `WorkflowSource []byte // raw config bytes snapshotted onto new tasks; empty => no snapshot`.
  - `Engine`: add field `workflowSource []byte`.
  - `New`: set `e.workflowSource = c.WorkflowSource`.
  - `ensureTask`: when constructing the new `&store.Task{...}`, set `WorkflowSnapshot: string(e.workflowSource)`.

- [ ] **Step 4: Run engine test.** `go test ./internal/engine/ -run RecordsWorkflowSnapshot` → PASS.

- [ ] **Step 5: Export `config.Parse`** in `internal/config/load.go`:

```go
// Parse decodes + schema-validates + invariant-checks raw config bytes (no file
// I/O). Load is Parse plus os.ReadFile. Used for snapshot bytes on recovery.
func Parse(data []byte) (*Workflow, []string, error) { return parse(data) }
```

- [ ] **Step 6: Feed the bytes from the CLI** in `cmd/orchestratord/main.go` `wire`:
  - Replace `config.Load(cf.config)` with: read bytes once, then `config.Parse`:
    ```go
    raw, err := os.ReadFile(cf.config)
    if err != nil {
        return nil, nil, fmt.Errorf("read config %q: %w", cf.config, err)
    }
    wf, warnings, err := config.Parse(raw)
    if err != nil {
        return nil, nil, err
    }
    ```
  - Add `WorkflowSource: raw` to the `engine.Config{...}` literal.
  - (`cmdValidate` keeps using `config.Load(path)` — unchanged.)

- [ ] **Step 7: Run full suite.** `go test ./... && go vet ./... && gofmt -l .` → green.

- [ ] **Step 8: Commit** (`feat(engine): snapshot the workflow onto each new task`).

---

## Task T1c: `recover` resumes against the per-task snapshot

**Why:** the actual correctness fix — drive a recovered task with the graph it started under, not the current `--config`.

**Files:**
- Modify: `internal/engine/engine.go` (`Recover`, add `cloneWithWorkflow`)
- Test: `internal/engine/recover_test.go` (or wherever recover tests live)

**Interfaces:**
- Produces: `func (e *Engine) cloneWithWorkflow(wf *config.Workflow) *Engine` — a shallow copy of the engine with `wf` swapped; all other deps shared. Used only by `Recover`.

- [ ] **Step 1: Failing test** — a recovered task whose snapshot names a goal/graph different from the engine's current `wf` is driven against the snapshot. Minimal, deterministic version: seed a task in `implementing` with a snapshot equal to the real default pipeline, then have `Recover` resume it; assert it advances using the snapshot (e.g. reaches `pr_open`) even after we mutate `e.wf` to a sabotaged copy:

```go
func TestRecover_UsesPerTaskSnapshot_NotCurrentConfig(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	raw, _ := os.ReadFile("../config/testdata/default-pipeline.yaml")
	// In-flight task that started under the real pipeline.
	st.CreateTask(ctx, &store.Task{ID: "issue-9", Issue: 9, Repo: "o/r",
		Branch: "agent/issue-9", CurrentState: "implementing", WorkflowSnapshot: string(raw)})

	b := &fakeBackend{pane: "fresh:p1", resolve: true}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 99}}, 5*time.Second)
	e.goal = "pr_open"
	// Sabotage the engine's *current* wf so a recover that ignored the snapshot
	// would misbehave (no states => runState can't resolve anything).
	e.wf = &config.Workflow{Name: "sabotaged", States: map[string]config.State{}}

	if err := e.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := st.GetTask(ctx, "issue-9")
	if got.CurrentState != "pr_open" {
		t.Errorf("state = %q, want pr_open (recover must use the snapshot graph)", got.CurrentState)
	}
}
```

- [ ] **Step 2: Run, watch fail.** Recover currently drives with `e` (sabotaged `wf`) → reconcile/drive can't resolve `implementing` → task does not reach `pr_open`. FAIL.

- [ ] **Step 3: Implement** in `internal/engine/engine.go`:
  - Add the clone helper:
    ```go
    // cloneWithWorkflow returns a shallow copy of e bound to a different workflow,
    // so a recovered task can be driven against the graph it started under.
    func (e *Engine) cloneWithWorkflow(wf *config.Workflow) *Engine {
        c := *e
        c.wf = wf
        return &c
    }
    ```
  - In `Recover`, before `reconcile`/`drive`, resolve the per-task engine:
    ```go
    eng := e
    if task.WorkflowSnapshot != "" {
        wf, _, perr := config.Parse([]byte(task.WorkflowSnapshot))
        if perr != nil {
            e.log.Warn("recover: task snapshot invalid; skipping (fix or migrate)", "task", task.ID, "err", perr)
            continue
        }
        eng = e.cloneWithWorkflow(wf)
    }
    // ...use eng.reconcile / eng.drive instead of e.reconcile / e.drive...
    ```
  - Keep the back-compat path: empty snapshot ⇒ `eng == e` (today's behavior, drives against `--config`).
  - Re-validating via `config.Parse` keeps recovery **fail-closed**: a snapshot that no longer satisfies the invariants is skipped, not silently run.

- [ ] **Step 4: Run.** `go test ./internal/engine/ -run Recover` → PASS; full `go test -race ./...` green.

- [ ] **Step 5: Commit** (`fix(engine): recover resumes against the task's workflow snapshot`).

---

## Task T2: Per-role `allowed_tools` → native tool-scoping at spawn

**Why:** the one capability CAO has that we lack — defense-in-depth that holds even if a permission prompt is bypassed. A reviewer agent that only needs read + PR-comment tools should not carry write/merge tools; scoping shrinks a misbehaving agent's blast radius **independently of the merge gate**. Roles already exist in the YAML; we add one optional field and translate it to the launcher's native flag. (Mechanism only; we do **not** scope the shipped roles by default — that is an operator choice, and changing live launch behavior without live tests is risky.)

**Files:**
- Modify: `internal/config/types.go` (`Role.AllowedTools`)
- Modify: `internal/config/workflow.schema.json` (role `allowed_tools`)
- Modify: `internal/engine/engine.go` (`spawn` builds Launch via `launchArgs`)
- Test: `internal/engine/engine_test.go` (or `decision_test.go`) — spawn appends the flag
- Test: `internal/config/config_test.go` — `allowed_tools` decodes

**Interfaces:**
- Produces: `config.Role.AllowedTools []string yaml:"allowed_tools"` — optional allowlist of agent tool names.
- Produces: `func launchArgs(r config.Role) []string` (engine, unexported) — `r.Launch` plus, when `len(r.AllowedTools) > 0` and the launcher is `claude`, `--allowedTools <csv>`. Documented as claude-targeted; a provider table is future (Roadmap).

- [ ] **Step 1: Failing test** in `internal/engine/engine_test.go`. Drive a spawn for a role with `allowed_tools` and assert the launched argv (captured by `fakeBackend.spawnLog`) carries the flag. Use a focused unit test on the helper to avoid threading config through a full drive:

```go
func TestLaunchArgs_AppendsAllowedToolsForClaude(t *testing.T) {
	got := launchArgs(config.Role{Launch: []string{"claude"}, AllowedTools: []string{"Read", "Bash(gh pr view:*)"}})
	want := []string{"claude", "--allowedTools", "Read,Bash(gh pr view:*)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launchArgs = %v, want %v", got, want)
	}
	// No allowlist => unchanged launch.
	if g := launchArgs(config.Role{Launch: []string{"claude"}}); !reflect.DeepEqual(g, []string{"claude"}) {
		t.Errorf("unscoped launch changed: %v", g)
	}
}
```

- [ ] **Step 2: Run, watch fail.** `go test ./internal/engine/ -run LaunchArgs` → FAIL (no `launchArgs`, no `AllowedTools`).

- [ ] **Step 3: Config field** in `internal/config/types.go` `Role`:
```go
	// AllowedTools optionally scopes the agent's tools (defense-in-depth). When
	// set, the backend passes the launcher's native allowlist flag. Empty => the
	// agent's own default permission config governs.
	AllowedTools []string `yaml:"allowed_tools"`
```
And in `internal/config/workflow.schema.json` under `$defs.role.properties`, add:
```json
"allowed_tools": { "type": "array", "items": { "type": "string" } }
```

- [ ] **Step 4: Helper + use** in `internal/engine/engine.go`:
```go
// launchArgs returns the agent launch argv, scoping tools when the role declares
// allowed_tools. Translation is claude-targeted today (our only launcher); a
// provider->flag table is future work.
func launchArgs(r config.Role) []string {
	args := append([]string(nil), r.Launch...)
	if len(r.AllowedTools) > 0 && len(r.Launch) > 0 && r.Launch[0] == "claude" {
		args = append(args, "--allowedTools", strings.Join(r.AllowedTools, ","))
	}
	return args
}
```
In `spawn`, change `Launch: r.Launch` to `Launch: launchArgs(r)` in the `exec.Spawn{...}` literal. Add `"strings"` to the engine imports if not present.

- [ ] **Step 5: Config decode test** in `internal/config/config_test.go` (table or focused): a role YAML with `allowed_tools: [Read, Write]` decodes into `Role.AllowedTools`. Run `go test ./internal/config/ ./internal/engine/` → PASS.

- [ ] **Step 6: Full suite + gofmt.** `go test -race ./... && gofmt -l .` → green.

- [ ] **Step 7: Commit** (`feat: optional per-role allowed_tools tool scoping at spawn`).

---

## Task T3: Integration tests against a fake `herdr`/`gh` binary on `PATH`

**Why:** our single biggest maturity gap — every test uses in-process fakes, so a drift in `gh pr view` or `herdr pane list` JSON shapes passes CI and breaks in production. The `proc.Runner` seam already lets us run the **real** `exec.Herdr`/`github.Client` against a tiny fake binary that emits captured fixture JSON, validating the CLI-shape contract our in-process fakes assume.

**Files:**
- Create: `internal/exec/herdr_integration_test.go`
- Create: `internal/github/gh_integration_test.go`
- Create: `internal/exec/testdata/*.json`, `internal/github/testdata/*.json` (captured shapes)

**Interfaces:**
- Consumes: the real `exec.NewHerdr(proc.New())` and `github.New(proc.New())` (no mocks). The fake binary is a shell script written to a temp dir that is prepended to `PATH`.

- [ ] **Step 1: Capture real fixtures.** Save real JSON shapes into `testdata/` (from the existing live run / `gh pr view --json ...` output): e.g. `internal/exec/testdata/pane_list.json` (a `{"result":{"panes":[...]}}` doc) and `internal/github/testdata/pr_view_clean.json` (a `gh pr view --json state,statusCheckRollup,...` doc with a CheckRun, a StatusContext, and an approved review).

- [ ] **Step 2: Failing test** in `internal/exec/herdr_integration_test.go`:

```go
//go:build integration || !windows

func TestHerdr_ListPanes_AgainstFakeBinary(t *testing.T) {
	dir := t.TempDir()
	// A fake `herdr` that echoes the captured pane-list fixture for `pane list`.
	fixture, _ := filepath.Abs("testdata/pane_list.json")
	script := "#!/bin/sh\nif [ \"$1\" = pane ] && [ \"$2\" = list ]; then cat " + fixture + "; exit 0; fi\nexit 9\n"
	writeExec(t, filepath.Join(dir, "herdr"), script) // 0755
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := exec.NewHerdr(proc.New()) // HerdrBin defaults to "herdr" (PATH lookup)
	hnd, ok, err := h.Resolve(context.Background(), "issue-208")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Assertions tied to the fixture's contents (workspace/label/pane ids).
	_ = hnd
	_ = ok
}
```
Add a `writeExec(t, path, body)` helper (os.WriteFile with 0o755). (`Resolve` calls both `workspace list` and `pane list`; the fake must answer both — extend the script's `case`.)

- [ ] **Step 3: Run, watch it fail** for the right reason (fixture mismatch / parse), then **iterate the fixtures until the real parser accepts them** — this is the point of the task: it pins the real CLI JSON contract. Mirror for `internal/github/gh_integration_test.go` driving `github.New(proc.New()).PRStatus` against `gh pr view` fixtures (assert `ChecksGreen`, `ApprovedReviews`, `MergeStateStatus`).

- [ ] **Step 4: Make CI run them.** These need a POSIX `sh` (present on the `ubuntu-latest` runner). Decide tagging: simplest is **no build tag** (they run everywhere with `sh`); if Windows CI is ever added, guard with `//go:build !windows`. Confirm `go test ./...` runs them locally.

- [ ] **Step 5: Run + commit** (`test: integration coverage of the real herdr/gh CLI JSON contract`). `go test ./... ` green.

---

## Task T4: `Notifier` seam for escalation/alert

**Why:** escalation is invisible today — `needs_human`/`alert` only emit an slog line + an audit row, so a stuck task waits unnoticed. CAO's observer pattern shows the cheap fix: a narrow seam the engine calls at alert/terminal-escalation points, default no-op, with a ~50-line webhook impl. Keeps the engine dependency-free (matches our boundary convention).

**Files:**
- Create: `internal/notify/notify.go` (interface + `Nop` + `Webhook`)
- Create: `internal/notify/notify_test.go`
- Modify: `internal/engine/engine.go` (`Config.Notifier`, call sites in `alert` + on terminal escalation in `advance`/`drive`)
- Modify: `cmd/orchestratord/main.go` (`--notify-webhook` flag wires `notify.Webhook`)

**Interfaces:**
- Produces:
  ```go
  package notify
  type Event struct { TaskID, Issue, State, Kind, Detail string } // Kind: "alert" | "escalated"
  type Notifier interface { Notify(ctx context.Context, ev Event) error }
  type Nop struct{}                  // Notify returns nil
  type Webhook struct { URL string; Client *http.Client } // POSTs ev as JSON
  ```
- Produces: `engine.Config.Notifier notify.Notifier` (default `notify.Nop{}` in `New`).

- [ ] **Step 1: Failing test** in `internal/notify/notify_test.go`: a `Webhook` pointed at an `httptest.Server` POSTs a JSON body containing the task id + kind; assert the server received it. Also assert `Nop{}.Notify` returns nil.

- [ ] **Step 2: Run, watch fail** (package doesn't exist).

- [ ] **Step 3: Implement `internal/notify/notify.go`** — `Nop`, and `Webhook.Notify` marshals `ev` and POSTs to `URL` with `Client` (default `http.DefaultClient`), honoring `ctx` (`http.NewRequestWithContext`), wrapping errors; a non-2xx is an error. Notifier failures must **never** block the engine (see Step 5).

- [ ] **Step 4: Engine wiring** in `internal/engine/engine.go`:
  - `Config`: add `Notifier notify.Notifier`. `New`: default `if e.notifier == nil { e.notifier = notify.Nop{} }`.
  - In `alert(...)`, after the audit write, fire-and-log (never fail the drive):
    ```go
    if err := e.notifier.Notify(ctx, notify.Event{TaskID: task.ID, Issue: ..., State: task.CurrentState, Kind: "alert", Detail: msg}); err != nil {
        e.log.Warn("notify failed", "task", task.ID, "err", err)
    }
    ```
  - On reaching a terminal state with `alert: true` (i.e. `escalated`): emit a `Kind: "escalated"` event. Cleanest single site: in `drive`, when `isHalt(next/current)` and the state's `Alert` is true. Add a guard so it fires once on entry. (Implementer: thread it where `advance` lands on a terminal `Alert` state, or check in `drive`'s halt branch.)

- [ ] **Step 5: Engine test** — inject a recording fake `Notifier` into `newEngine`, drive a task to `escalated` (existing timeout/retry-exhaust tests reach it), assert exactly one `escalated` event. Confirm a Notifier that returns an error does **not** fail the drive (the task still reaches `escalated`). Run `go test -race ./internal/...` green.

- [ ] **Step 6: CLI flag** in `cmd/orchestratord/main.go`: add `--notify-webhook URL` to `registerCommon`; in `wire`, if set, `Notifier: &notify.Webhook{URL: cf.notifyWebhook}`. Update `usage`.

- [ ] **Step 7: Commit** (`feat: Notifier seam + webhook for escalation/alert events`).

---

## Task T5: `plan` subcommand — render the resolved graph + invariants

**Why:** the alettieri spec gates write access behind "debate the graph and safety model first." We already compute the graph (`buildGraph` + `tarjanSCC`) and check invariants, but only emit pass/fail text. Surfacing the resolved graph (states, triggers, terminal/side-effecting nodes, cycles with their cap/timeout) makes the mechanism/policy split human-auditable. Pure read; no engine changes.

**Files:**
- Modify: `internal/config/graph.go` (export an analysis view)
- Modify: `cmd/orchestratord/main.go` (`plan` subcommand)
- Test: `internal/config/config_test.go` (analysis shape); `cmd` smoke via `run([]string{"plan", ...})`

**Interfaces:**
- Produces:
  ```go
  // GraphAnalysis is a human-auditable view of a validated workflow's structure.
  type GraphAnalysis struct {
      Edges         map[string][]string // state -> targets (sorted)
      SCCs          [][]string          // strongly-connected components
      Terminal      []string            // states with terminal set
      SideEffecting []string            // states whose entry.action is side-effecting (merge_pr)
  }
  func Analyze(wf *Workflow) GraphAnalysis
  ```
  (Reuses unexported `buildGraph`/`tarjanSCC`; no recomputation logic duplicated.)

- [ ] **Step 1: Failing test** in `internal/config/config_test.go`: load the default pipeline, `Analyze`, assert `merging ∈ SideEffecting`, `merged ∈ Terminal`, and that the `changes_requested↔pr_open` cycle appears as a multi-node SCC.

- [ ] **Step 2: Run, watch fail** (`Analyze` undefined).

- [ ] **Step 3: Implement `config.Analyze`** in `internal/config/graph.go` — build `Edges` from `buildGraph`, `SCCs` from `tarjanSCC`, scan states for `Terminal != ""` and `Entry.Action ∈ sideEffectingActions` (reuse the validator's side-effecting set; if it's a local in `validate.go`, lift it to a package var so both use one definition — do not duplicate the literal).

- [ ] **Step 4: `plan` subcommand** in `cmd/orchestratord/main.go`: add `case "plan": return cmdPlan(args[1:])`. `cmdPlan` loads+validates the config (reuse `config.Load`, refusing to render an invalid config — same fail-closed posture as `run`), then prints `Analyze(wf)` as a readable tree: each state with its transitions (trigger kind → targets), markers for `[terminal]`/`[side-effecting]`, and a "cycles" section listing each non-trivial SCC with whether every node has a retry cap or timeout. Update `usage`.

- [ ] **Step 5: Smoke test** the subcommand: `run([]string{"plan", "internal/config/testdata/default-pipeline.yaml"})` returns 0 and prints `merging` as side-effecting. Run `go test ./...` green.

- [ ] **Step 6: Commit** (`feat(cli): plan subcommand renders the resolved graph + invariants`).

---

## Task T6: Doc/comment refresh + `max_concurrent_tasks` honesty

**Why:** small hygiene that keeps the codebase honest after Phase 2a. The engine package doc (`internal/engine/engine.go:1-12`) still says "Phase 1 ... halts at the goal state (pr_open); review, merge, and triage are out of scope" — false since Phase 2a. And `Policies.MaxConcurrentTasks` is parsed-but-dead config; the comment should say so until the scheduler (Roadmap R2) enforces it.

**Files:**
- Modify: `internal/engine/engine.go` (package doc)
- Modify: `internal/config/types.go` (`Policies` comment)

**Interfaces:** none (comments only).

- [ ] **Step 1: Update the engine package doc** to describe the real current behavior: drives `queued → implementing → pr_open → (review) → approved → (merge gate) → merging → merged`; default goal `merged`; merge gated on `dry_run`; triage/scheduler/MCP/memory remain deferred.
- [ ] **Step 2: Update the `Policies` comment** in `types.go` to state `max_concurrent_tasks` is parsed-but-not-yet-enforced (the scheduler is Roadmap R2), and `dry_run` now **does** gate the (built) merge.
- [ ] **Step 3: Verify nothing else references the stale "Phase 1 halts at pr_open" wording** (`grep -rn "pr_open" --include=*.go` and the README — README was already updated in PR #1, just re-confirm).
- [ ] **Step 4: `go build ./... && go vet ./...`** (comments only; no test change). **Commit** (`docs: refresh engine package doc + policy comments for Phase 2a/2b`).

---

# Roadmap — Outstanding work after Phase 2a (each item = its own future plan)

These are the deferred subsystems. They are **not** decomposed into bite-sized tasks here — per scope discipline, each is its own plan (and several should be brainstormed first, since they have real design forks). Listed in recommended sequence with scope, the seams they land on, the hard questions, and the relevant external reference.

### R1 — Triage / intake front-end *(smallest; do first)*
- **Scope:** make `intake` + the `triage` decision real so the pipeline starts from "labeled issues" not "an issue number." A source poller for `github_issues` (`select: { label: ... }`) emits issues into `intake`; the `triage` LLM decision branches `accept → queued`, `reject → closed`, `needs_human → escalated`.
- **Lands on:** existing `decision` infra (`internal/engine/decision.go` already evaluates verdict files); a new `github.Client.ListIssues(repo, labels)`; the `Source` type already exists in config.
- **Hard questions:** dedupe/idempotency (don't re-triage the same issue → key on `issue-<n>` task existence); where the triage agent runs (a role? or a non-spawning `exec`-type decision?); rate/cost of LLM triage per poll.
- **Reference:** the alettieri spec's `herdr.issue` workflow type front-end.

### R2 — Scheduler / daemon *(biggest architectural step)*
- **Scope:** turn the one-shot `run` into a long-running daemon that polls sources, enqueues tasks, and drives up to `max_concurrent_tasks` concurrently; make the `scheduled` event real (it auto-fires today). Optionally a cron entry-point (unattended runs).
- **Lands on:** `Policies.MaxConcurrentTasks` (currently dead — see T6), the `autoFiredEvents{"scheduled"}` stub, `Recover`.
- **Hard questions (brainstorm first):** the **single-writer invariant** — today the engine is one goroutine; concurrent driving means N goroutines writing the store. The store already serializes at the SQL layer (`MaxOpenConns=1`), but the *engine's* "single writer" assumption (and any in-task pointer sharing) must be re-examined — likely a worker pool with one task per goroutine and the store as the synchronization point, or a single driving goroutine with cooperative scheduling. Decide which. Plus: fairness, backpressure, crash semantics for a fleet vs one task.
- **Reference:** CAO ships a cron scheduler (`Flows`: `flow_service.py`, health-check-gated) — a concrete reference for the cron/unattended side; CAO's unbounded `assign` fan-out is the **anti-pattern** to avoid (we want the cap enforced).

### R3 — MCP server surface
- **Scope:** expose orchestrator state/control (list tasks, inspect audit, nudge/cancel) over MCP, so an operator (or a supervising agent) can drive it.
- **Lands on:** `*store.Store` reads (tasks + audit) and a small command surface; the engine stays the single writer.
- **Hard questions:** read-only vs control; auth/loopback posture (CAO is localhost-only with optional OAuth — adopt that posture); which mutations are safe to expose.
- **Reference:** CAO's `mcp_server/` (FastMCP) + REST.

### R4 — Cross-task memory / context
- **Scope:** durable context shared across tasks (e.g. repo conventions, prior triage rationale) the agents can consult.
- **Hard questions:** what is actually worth remembering vs re-deriving; secret-leak prevention on writes (CAO has a `secret_gate.py` deny-list — adopt if we persist agent-authored content); scope (per-repo vs federated).
- **Reference:** CAO's memory feature (markdown wiki + `memory_metadata` + `audit_log.py` + `secret_gate.py`).

### Cross-cutting (fold into the above or do opportunistically)
- **Push-based herdr events** *(open question):* if herdr exposes `events.subscribe` over its unix socket, replace the 2s `eventHub` pane-list poll ticker + the merge-gate poll with a push stream — lower latency, less work. **Verify the socket API exists first** (the comparison flagged this as unconfirmed; CAO's `herdr_backend` reads `herdr pane get` JSON but that is not necessarily a subscription). If push doesn't exist, polling is the correct call — don't build it.
- **Per-provider tool-flag table:** generalize T2's claude-only `--allowedTools` translation into a provider→flag map when a second launcher is added. YAGNI until then.
- **Full live e2e harness:** T3 pins the CLI JSON contract with fixtures; a *live* herdr+gh smoke test (behind a build tag, opt-in, not in default CI) would catch real end-to-end drift. Lower priority; the live run on issue 208/PR #227 already exercised the real path once.

---

# Self-Review

- **Spec coverage:** every "adopt/consider" learning from the comparison maps to a task — snapshot-on-recover→T1, per-role tool scoping→T2, fixture integration tests→T3, notification hook→T4, `plan`/graph render→T5; the env-scrub "consider" was **dropped on purpose** (doesn't map cleanly to `herdr pane run`, which targets an existing shell — noted under Cross-cutting). All deferred subsystems (triage, scheduler, MCP, memory) are in the Roadmap with scope + references.
- **Placeholder scan:** no "TBD"/"handle errors appropriately"/"write tests for the above" — each task carries real test code, exact file paths, and the actual edits. The Roadmap intentionally omits bite-sized steps (each is its own plan) and says so explicitly.
- **Type consistency:** new symbols are defined once and reused — `store.Task.WorkflowSnapshot` (T1a) is consumed by T1b/T1c; `config.Parse` (T1b) is reused by T1c's recover path; `engine.Config.WorkflowSource` (T1b) feeds `ensureTask`; `config.Role.AllowedTools` + `launchArgs` (T2) match; `notify.Notifier`/`Event`/`Nop`/`Webhook` (T4) match the engine wiring; `config.Analyze`/`GraphAnalysis` (T5) match the `plan` subcommand. `cloneWithWorkflow` is used only by `Recover`.
- **Scope discipline:** Phase 2b is the coherent shippable unit (hardening the existing system) and carries full tasks; the larger subsystems are sequenced but deferred to their own plans.
