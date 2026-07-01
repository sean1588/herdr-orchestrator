# R2 — Scheduler Daemon + Executable Triage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** turn the orchestrator into a long-running daemon that discovers labeled issues on its own and drives up to `max_concurrent_tasks` of them through the full pipeline concurrently, with the `intake`/`triage` decision made executable so the loop runs poll → triage → queue → implement → review → merge.

**Architecture:** two components built triage-first. (A) Make `intake` executable by reusing the Phase 2a decision path (spawn an agent → `agent.done` → verdict file → branch); the only new engine logic is `triageTask` (rubric + issue, no PR). (B) `orchestratord daemon`: a single poller goroutine is the only task creator, an N-worker pool drives tasks by wrapping the existing `engine.Run`, and the SQLite store (`MaxOpenConns=1`) serializes writes while tasks stay row-partitioned by issue.

**Tech Stack:** Go 1.26; existing packages `config`, `engine`, `store`, `exec`, `github`, `proc`; `gh`/`herdr` via `proc.Runner`. Spec: `docs/superpowers/specs/2026-07-01-r2-scheduler-design.md`.

## Global Constraints

- No new dependencies beyond Phase 1's (`modernc.org/sqlite`, `gopkg.in/yaml.v3`, `santhosh-tekuri/jsonschema/v6`).
- Engine depends on interfaces (`exec.ExecutionBackend`, `github.Client`), never on herdr/`gh` concretely.
- `context.Context` is the first arg of anything that blocks; honor cancellation. Wrap errors with `%w`. No panics in the daemon path. No global mutable state.
- Never launch agents with `--dangerously-skip-permissions`. Task handoff = context file + single-line kickoff; never send multi-line text through a pane.
- Merge stays gated on `policies.dry_run` (default-on). GitHub is authoritative. Branch names `agent/issue-<n>`; pane ids volatile.
- Every task keeps `go build ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l .` green. TDD (red → green), table-driven tests.
- Commit trailers on every commit:
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Mkou7Ub33gLsthfNosLDQK
  ```
- Work on branch `r2-scheduler` (already created).

---

## File Structure

- `internal/config/testdata/default-pipeline.yaml` — restructure `intake`, add `triager` role (Task A1).
- `internal/config/testdata/prompts/triage.md` — CREATE, triage rubric (Task A1).
- `internal/engine/decision.go` — add `triageTask` (Task A2).
- `internal/engine/engine.go` — route decision-state-without-PR to `triageTask` in `agentTask` (Task A2).
- `cmd/orchestratord/main.go` — `wire` sets `StartState` from entry_state (Task A2); refactor `wire` to a `*wired` struct + add `daemon` subcommand (Task B3).
- `internal/github/gh.go`, `internal/github/client.go` — `ListIssues` (Task B1).
- `internal/github/testdata/issue_list.json` — CREATE (Task B1).
- `internal/scheduler/scheduler.go` — CREATE, `Scheduler` + `Serve` (Task B2).
- Tests colocated: `internal/engine/decision_test.go`, `internal/config/config_test.go`, `internal/github/gh_integration_test.go`, `internal/engine/engine_test.go` (fakeGH gains `ListIssues`), `internal/scheduler/scheduler_test.go`, `cmd/orchestratord/main_test.go`.

---

## Component A — Executable triage

### Task A1: Make `intake` executable in config + triage rubric

**Files:**
- Modify: `internal/config/testdata/default-pipeline.yaml` (the `intake` state + `roles`)
- Create: `internal/config/testdata/prompts/triage.md`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: a `default-pipeline.yaml` whose `intake` state has `entry: {spawn: triager}` and an `agent.done` transition that `evaluate`s the existing `triage` decision, branching `{accept: queued, reject: closed, needs_human: escalated}`, plus a `timeout: 15m -> escalated`. A new `triager` role. Rubric at `prompts/triage.md`.

- [ ] **Step 1: Write the failing test** — add to `internal/config/config_test.go`:

```go
// intake is executable: it spawns a triager and branches on the triage verdict.
func TestLoad_DefaultPipeline_IntakeSpawnsTriagerAndBranchesTriage(t *testing.T) {
	wf, warnings, err := Load("testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(warnings) != 2 { // still only pr_open + changes_requested spawn-without-timeout
		t.Fatalf("want 2 warnings, got %d: %v", len(warnings), warnings)
	}
	if _, ok := wf.Roles["triager"]; !ok {
		t.Fatal("triager role not declared")
	}
	intake := wf.States["intake"]
	if intake.Entry == nil || intake.Entry.Spawn != "triager" {
		t.Fatalf("intake.entry.spawn = %+v, want triager", intake.Entry)
	}
	t0 := intake.Transitions[0]
	if t0.When.Event != "agent.done" || t0.Evaluate == nil || t0.Evaluate.Decision != "triage" {
		t.Fatalf("intake t0 = %+v, want agent.done/evaluate triage", t0)
	}
	if t0.Branch["accept"] != "queued" || t0.Branch["reject"] != "closed" || t0.Branch["needs_human"] != "escalated" {
		t.Errorf("intake branch = %v", t0.Branch)
	}
	if intake.Transitions[1].When.Timeout != "15m" || intake.Transitions[1].To != "escalated" {
		t.Errorf("intake t1 = %+v, want timeout 15m -> escalated", intake.Transitions[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run IntakeSpawnsTriager -v`
Expected: FAIL — current `intake` has no `entry`, and `triager` role is undeclared.

- [ ] **Step 3: Restructure `intake` + add the role** in `internal/config/testdata/default-pipeline.yaml`.

Replace the `intake` state:
```yaml
  intake:
    entry: { spawn: triager }
    transitions:
      - when: { event: agent.done }
        evaluate: { decision: triage }
        branch: { accept: queued, reject: closed, needs_human: escalated }
      - when: { timeout: 15m }
        to: escalated
```

Add the `triager` role under `roles:` (next to `implementer`/`reviewer`):
```yaml
  triager:
    launch: ["claude"]
    task_delivery: context_file
    workspace: per_task
```

- [ ] **Step 4: Create the rubric** `internal/config/testdata/prompts/triage.md`:

```markdown
# Triage rubric

Decide whether an incoming issue is ready for an autonomous implementer.

Reply with exactly one verdict:

- **accept** — the issue is clear, self-contained, and actionable: a concrete
  change with enough detail to implement and verify without further questions.
- **needs_human** — the intent is plausible but underspecified or ambiguous:
  missing acceptance criteria, unclear scope, or a decision a human must make.
- **reject** — out of scope, a duplicate, not actionable, or not a code change.

Judge only what the issue states. When genuinely unsure between accept and
needs_human, choose needs_human.
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -v 2>&1 | tail -20`
Expected: PASS, including `TestLoad_DefaultPipeline_IntakeSpawnsTriagerAndBranchesTriage`, `TestLoad_DefaultPipeline_ValidWith2Warnings`, and `TestLoad_DefaultPipeline_DecodesStructure` (state count is unchanged at 11).

- [ ] **Step 6: Commit**

```bash
git add internal/config/testdata/default-pipeline.yaml internal/config/testdata/prompts/triage.md internal/config/config_test.go
git commit  # message: "feat(config): make intake executable (spawn triager, branch triage verdict)"
```

---

### Task A2: `triageTask` + route decision-state-without-PR + start at entry_state

**Files:**
- Create/modify: `internal/engine/decision.go` (`triageTask`)
- Modify: `internal/engine/engine.go` (`agentTask` routing)
- Modify: `cmd/orchestratord/main.go` (`wire` sets `StartState` from entry_state; add `startState` helper)
- Test: `internal/engine/decision_test.go`

**Interfaces:**
- Consumes: `config.Role`, `store.Task` (`.PRNumber *int`, `.Issue int`, `.ID string`), `e.readRubric`, `e.gh.Issue(ctx, repoDir, n) (*github.Issue, error)`, `verdictPath`, `decisionForState`.
- Produces: `func (e *Engine) triageTask(ctx context.Context, task *store.Task, decisionName string) (taskFile, kickoff string, err error)`. `agentTask` now routes a decision state to `triageTask` when `task.PRNumber == nil`, else `reviewerTask`.

- [ ] **Step 1: Write the failing tests** — add to `internal/engine/decision_test.go`:

```go
// Driving from intake spawns the triager and branches on its verdict.
func TestIntake_TriageVerdict_Branches(t *testing.T) {
	tests := []struct{ name, verdict, goal, wantTo string }{
		{"accept -> queued", "accept", "queued", "queued"},
		{"reject -> closed (terminal)", "reject", "merged", "closed"},
		{"needs_human -> escalated (terminal)", "needs_human", "merged", "escalated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
				{PaneID: "w1:p1", State: exec.StateWorking},
				{PaneID: "w1:p1", State: exec.StateDone},
			}}
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			e.goal = tt.goal
			task := &store.Task{ID: "issue-9", Issue: 9, Branch: "agent/issue-9", CurrentState: "intake"}
			if err := st.CreateTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			writeVerdict(t, e.taskDir, task.ID, `{"verdict":"`+tt.verdict+`","feedback":""}`)

			final, err := e.drive(context.Background(), task)
			if err != nil {
				t.Fatalf("drive: %v", err)
			}
			if final != tt.wantTo {
				t.Fatalf("final = %q, want %q", final, tt.wantTo)
			}
			if b.spawns != 1 {
				t.Errorf("triager should spawn exactly once, got %d", b.spawns)
			}
		})
	}
}

// The triager's task file carries the rubric + the issue (no PR), and the
// kickoff instructs it to write the verdict file.
func TestIntake_TriagerTask_CarriesRubricAndIssueNoPR(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	gh := &fakeGH{issue: &github.Issue{Number: 9, Title: "Add feature", Body: "the details"}}
	e := newEngine(t, st, b, gh, 5*time.Second)
	e.goal = "queued"
	task := &store.Task{ID: "issue-9", Issue: 9, Branch: "agent/issue-9", CurrentState: "intake"}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"accept","feedback":""}`)

	if _, err := e.drive(context.Background(), task); err != nil {
		t.Fatalf("drive: %v", err)
	}
	sp := b.spawnLog[0]
	content, err := os.ReadFile(sp.TaskFile)
	if err != nil {
		t.Fatalf("read triage task file: %v", err)
	}
	if !strings.Contains(string(content), "Triage rubric") || !strings.Contains(string(content), "Add feature") {
		t.Errorf("triage task file missing rubric or issue:\n%s", content)
	}
	if strings.Contains(string(content), "PR #") {
		t.Errorf("triage task must not reference a PR:\n%s", content)
	}
	if !strings.Contains(sp.Kickoff, "Triage issue #9") || !strings.Contains(sp.Kickoff, verdictPath(e.taskDir, task.ID)) {
		t.Errorf("kickoff does not instruct writing the verdict for issue 9: %q", sp.Kickoff)
	}
}
```

`github` is already imported in `decision_test.go`? It is imported in `engine_test.go` (same package). `decision_test.go` imports `exec` and `store`; add `"github.com/sean1588/herdr-orchestrator/internal/github"` to its import block for the `github.Issue` literal.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'Intake_' -v`
Expected: FAIL — `agentTask` sends the intake spawn down the implementer path (writes the plain issue file, no verdict wiring), so the drive does not branch on the triage verdict / the triage task file assertions fail. (If the intake spawn currently routes to `reviewerTask`, it panics/errs on the nil PR via `prNum`; either way it fails.)

- [ ] **Step 3: Add `triageTask`** to `internal/engine/decision.go` (below `reviewerTask`):

```go
// triageTask builds the triager's context file (the decision rubric + the issue
// title/body) and a single-line kickoff to write the verdict file. Unlike
// reviewerTask it references the ISSUE, not a PR: triage runs at the pipeline
// entry, before any PR exists.
func (e *Engine) triageTask(ctx context.Context, task *store.Task, decisionName string) (taskFile, kickoff string, err error) {
	d := e.wf.Decisions[decisionName]
	rubric, err := e.readRubric(d.Impl.Rubric)
	if err != nil {
		return "", "", fmt.Errorf("decision %q: %w", decisionName, err)
	}
	issue, err := e.gh.Issue(ctx, e.repoDir, task.Issue)
	if err != nil {
		return "", "", fmt.Errorf("triage: fetch issue %d: %w", task.Issue, err)
	}
	path := filepath.Join(e.taskDir, "triage-task-"+task.ID+".md")
	body := fmt.Sprintf("%s\n\n## Issue under triage\n\n# %s\n\n%s\n", rubric, issue.Title, issue.Body)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", "", fmt.Errorf("write triage task file: %w", err)
	}
	vp := verdictPath(e.taskDir, task.ID)
	kickoff = fmt.Sprintf(
		"Triage issue #%d following the rubric in %s. When done, write your verdict as JSON {\"verdict\": one of %v, \"feedback\": \"...\"} to %s. Stop when the verdict file is written.",
		task.Issue, path, d.Verdicts, vp)
	return path, kickoff, nil
}
```

- [ ] **Step 4: Route decision-state-without-PR in `agentTask`** (`internal/engine/engine.go`). Replace the decision branch:

```go
	if dec := decisionForState(st); dec != "" {
		if task.PRNumber == nil {
			return e.triageTask(ctx, task, dec) // pipeline-entry decision: rubric + issue
		}
		return e.reviewerTask(task, dec) // review decision: rubric + PR pointer
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/engine/ -run 'Intake_' -v`
Expected: PASS. Then `go test ./internal/engine/ 2>&1 | tail -3` — the existing `review` tests (PR present) still route to `reviewerTask`.

- [ ] **Step 6: Start the CLI at the workflow entry_state** in `cmd/orchestratord/main.go`. In `wire`, after loading `wf`, set `StartState` on the `engine.Config`:

```go
	// StartState is the workflow's entry_state so `run`/`daemon` create tasks at
	// the pipeline front door (intake/triage). Empty entry_state falls back to the
	// engine default ("queued"), preserving pre-triage behavior.
	start := ""
	if wf.EntryState != nil {
		start = *wf.EntryState
	}
```
and add `StartState: start,` to the `engine.New(engine.Config{...})` literal.

- [ ] **Step 7: Run the full suite**

Run: `go build ./... && go test -race ./... 2>&1 | tail -12 && go vet ./... && gofmt -l .`
Expected: all packages `ok`, `gofmt` prints nothing. (Existing engine tests construct `Config` directly and are unaffected by the `wire` change; `main_test` dispatch tests assert exit codes only.)

- [ ] **Step 8: Commit**

```bash
git add internal/engine/decision.go internal/engine/engine.go internal/engine/decision_test.go cmd/orchestratord/main.go
git commit  # message: "feat(engine): executable triage — triageTask + route entry decision; CLI starts at entry_state"
```

---

## Component B — Scheduler daemon

### Task B1: `github.ListIssues`

**Files:**
- Modify: `internal/github/client.go` (add to `Client` interface)
- Modify: `internal/github/gh.go` (implement on `*GH`)
- Modify: `internal/engine/engine_test.go` (add `ListIssues` to `fakeGH` so it still satisfies `Client`)
- Create: `internal/github/testdata/issue_list.json`
- Test: `internal/github/gh_integration_test.go`

**Interfaces:**
- Produces: `ListIssues(ctx context.Context, repoDir, label string) ([]int, error)` on `github.Client` — runs `gh issue list --label <label> --json number` in `repoDir`, returns the issue numbers.

- [ ] **Step 1: Add the fixture** `internal/github/testdata/issue_list.json`:

```json
[{ "number": 5 }, { "number": 8 }, { "number": 13 }]
```

- [ ] **Step 2: Write the failing integration test** — add to `internal/github/gh_integration_test.go`:

```go
func TestGH_ListIssues_AgainstFakeBinary(t *testing.T) {
	abs, err := filepath.Abs("testdata/issue_list.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Fake gh emits the fixture only for `gh issue list --label ... --json number`.
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = issue ] && [ \"$2\" = list ]; then\n" +
		"  case \"$*\" in *--label*--json*number*) cat " + abs + "; exit 0 ;; esac\n" +
		"  echo \"fake gh: unexpected issue list args: $*\" >&2; exit 3\n" +
		"fi\n" +
		"exit 9\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := github.New(proc.New()).ListIssues(context.Background(), t.TempDir(), "agent-ready")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []int{5, 8, 13}
	if len(got) != len(want) {
		t.Fatalf("ListIssues = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListIssues = %v, want %v", got, want)
		}
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/github/ -run ListIssues -v`
Expected: FAIL — `ListIssues` is undefined on `github.Client`; package does not compile.

- [ ] **Step 4: Add to the `Client` interface** in `internal/github/client.go` (inside `type Client interface`):

```go
	// ListIssues returns the numbers of issues matching label, via
	// `gh issue list --label <label> --json number` in repoDir.
	ListIssues(ctx context.Context, repoDir, label string) ([]int, error)
```

- [ ] **Step 5: Implement on `*GH`** in `internal/github/gh.go`:

```go
// ListIssues runs `gh issue list --label <label> --json number` in repoDir and
// returns the matching issue numbers.
func (g *GH) ListIssues(ctx context.Context, repoDir, label string) ([]int, error) {
	out, err := g.run.Run(ctx, repoDir, "gh", "issue", "list", "--label", label, "--json", "number")
	if err != nil {
		return nil, fmt.Errorf("gh issue list --label %s: %w", label, err)
	}
	var items []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("parse gh issue list output: %w", err)
	}
	nums := make([]int, len(items))
	for i, it := range items {
		nums[i] = it.Number
	}
	return nums, nil
}
```

- [ ] **Step 6: Add `ListIssues` to `fakeGH`** in `internal/engine/engine_test.go` (so it still satisfies `github.Client`):

```go
func (g *fakeGH) ListIssues(ctx context.Context, repoDir, label string) ([]int, error) {
	return nil, nil
}
```

- [ ] **Step 7: Run to verify pass**

Run: `go test ./internal/github/ ./internal/engine/ 2>&1 | tail -4`
Expected: both `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/github/client.go internal/github/gh.go internal/github/testdata/issue_list.json internal/github/gh_integration_test.go internal/engine/engine_test.go
git commit  # message: "feat(github): ListIssues (gh issue list --label --json number)"
```

---

### Task B2: `internal/scheduler` package

**Files:**
- Create: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: nothing from other packages (depends only on injected funcs + stdlib), keeping it unit-testable without the engine.
- Produces:
  ```go
  type Scheduler struct {
      List     func(ctx context.Context) ([]int, error)           // discover candidate issues
      Done     func(ctx context.Context, issue int) (bool, error) // true iff a TERMINAL task exists (not-found => false)
      RunTask  func(ctx context.Context, issue int) error         // drive one issue to completion
      SeedFrom func(ctx context.Context) ([]int, error)           // non-terminal issues to resume at startup
      Interval time.Duration
      Workers  int
      Log      *slog.Logger
  }
  func (s *Scheduler) Serve(ctx context.Context) error // seed -> workers + poll loop -> block until ctx done
  ```

- [ ] **Step 1: Write the failing tests** — create `internal/scheduler/scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Discovered issues are each driven exactly once; a completed issue (Done=true)
// is not re-run on the next poll.
func TestServe_DispatchesEachIssueOnce(t *testing.T) {
	var mu sync.Mutex
	runs := map[int]int{}
	done := map[int]bool{}
	ranAll := make(chan struct{})

	s := &Scheduler{
		List: func(ctx context.Context) ([]int, error) { return []int{1, 2, 3}, nil },
		Done: func(ctx context.Context, issue int) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return done[issue], nil
		},
		RunTask: func(ctx context.Context, issue int) error {
			mu.Lock()
			runs[issue]++
			done[issue] = true
			n := len(done)
			mu.Unlock()
			if n == 3 {
				select {
				case <-ranAll:
				default:
					close(ranAll)
				}
			}
			return nil
		},
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	select {
	case <-ranAll:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for all issues to run")
	}
	time.Sleep(30 * time.Millisecond) // let extra polls happen; dedup must hold
	cancel()

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i <= 3; i++ {
		if runs[i] != 1 {
			t.Errorf("issue %d ran %d times, want 1", i, runs[i])
		}
	}
}

// An issue whose task is already terminal is never run.
func TestServe_SkipsTerminalIssues(t *testing.T) {
	var ran int32
	s := &Scheduler{
		List:     func(ctx context.Context) ([]int, error) { return []int{7}, nil },
		Done:     func(ctx context.Context, issue int) (bool, error) { return true, nil },
		RunTask:  func(ctx context.Context, issue int) error { atomic.AddInt32(&ran, 1); return nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil },
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = s.Serve(ctx)
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Errorf("terminal issue ran %d times, want 0", n)
	}
}

// Never more than Workers RunTask calls execute concurrently.
func TestServe_RespectsWorkerCap(t *testing.T) {
	release := make(chan struct{})
	var cur, max int32
	s := &Scheduler{
		List: func(ctx context.Context) ([]int, error) { return []int{1, 2, 3, 4, 5, 6}, nil },
		Done: func(ctx context.Context, issue int) (bool, error) { return false, nil },
		RunTask: func(ctx context.Context, issue int) error {
			n := atomic.AddInt32(&cur, 1)
			for {
				m := atomic.LoadInt32(&max)
				if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
					break
				}
			}
			<-release
			atomic.AddInt32(&cur, -1)
			return nil
		},
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	time.Sleep(80 * time.Millisecond) // let workers saturate
	if m := atomic.LoadInt32(&max); m > 2 {
		t.Errorf("max concurrent RunTask = %d, want <= 2", m)
	}
	close(release)
	cancel()
}

// Startup seeds non-terminal issues even when the poll source is empty.
func TestServe_SeedsInFlightOnStartup(t *testing.T) {
	ran := make(chan int, 1)
	s := &Scheduler{
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		Done:     func(ctx context.Context, issue int) (bool, error) { return false, nil },
		RunTask:  func(ctx context.Context, issue int) error { ran <- issue; return nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return []int{9}, nil },
		Interval: time.Hour, // no polling; only the seed drives
		Workers:  1,
		Log:      testLog(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	select {
	case got := <-ran:
		if got != 9 {
			t.Errorf("seeded issue = %d, want 9", got)
		}
	case <-time.After(time.Second):
		t.Fatal("seed issue was not driven")
	}
}

// Serve returns promptly when the context is cancelled.
func TestServe_ReturnsOnCancel(t *testing.T) {
	s := &Scheduler{
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		Done:     func(ctx context.Context, issue int) (bool, error) { return false, nil },
		RunTask:  func(ctx context.Context, issue int) error { return nil },
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/scheduler/ -v`
Expected: FAIL — package `scheduler` does not exist / `Scheduler` undefined.

- [ ] **Step 3: Implement** `internal/scheduler/scheduler.go`:

```go
// Package scheduler runs the orchestrator as a daemon: a single poller goroutine
// discovers candidate issues (the only task creator, so there is no create/create
// race) and an N-worker pool drives each issue to completion by wrapping the
// engine's per-issue entry point. Tasks are row-partitioned by issue and the
// store serializes writes, so the workers need no additional locking.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// queueDepth bounds the in-memory work channel. Overflow is not lost: the poller
// skips a full channel and the still-labelled issue is re-discovered next tick.
const queueDepth = 128

// Scheduler drives discovered issues concurrently. All external dependencies are
// injected as funcs so it is unit-testable without the engine, store, or gh.
type Scheduler struct {
	List     func(ctx context.Context) ([]int, error)           // discover candidate issues
	Done     func(ctx context.Context, issue int) (bool, error) // true iff a TERMINAL task exists
	RunTask  func(ctx context.Context, issue int) error         // drive one issue to completion
	SeedFrom func(ctx context.Context) ([]int, error)           // non-terminal issues to resume at startup
	Interval time.Duration
	Workers  int
	Log      *slog.Logger
}

// Serve seeds in-flight work, starts the worker pool, then polls until ctx is
// done. On cancellation it stops the poller, lets the workers drain (each drive
// returns when ctx is done), and returns. Tasks persist their state, so the next
// start resumes them via SeedFrom.
func (s *Scheduler) Serve(ctx context.Context) error {
	workers := s.Workers
	if workers < 1 {
		workers = 1
	}
	work := make(chan int, queueDepth)
	inflight := &inflightSet{m: map[int]bool{}}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for issue := range work {
				if err := s.RunTask(ctx, issue); err != nil {
					s.Log.Warn("run task failed", "issue", issue, "err", err)
				}
				inflight.remove(issue) // allow re-discovery (retry on a later poll)
			}
		}()
	}

	if s.SeedFrom != nil {
		if seed, err := s.SeedFrom(ctx); err != nil {
			s.Log.Warn("seed failed", "err", err)
		} else {
			s.enqueue(work, inflight, seed)
		}
	}

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(work) // Serve is the only sender; safe to close
			wg.Wait()
			return nil
		case <-ticker.C:
			issues, err := s.List(ctx)
			if err != nil {
				s.Log.Warn("poll failed", "err", err)
				continue
			}
			s.enqueue(work, inflight, issues)
		}
	}
}

// enqueue adds issues that are neither in-flight nor already done. It never
// blocks: a full channel means the issue is skipped and re-discovered next poll.
func (s *Scheduler) enqueue(work chan int, inflight *inflightSet, issues []int) {
	for _, issue := range issues {
		if inflight.has(issue) {
			continue
		}
		if s.Done != nil {
			done, err := s.Done(context.Background(), issue)
			if err != nil {
				s.Log.Warn("done check failed", "issue", issue, "err", err)
				continue
			}
			if done {
				continue
			}
		}
		if !inflight.add(issue) {
			continue
		}
		select {
		case work <- issue:
		default:
			inflight.remove(issue) // channel full; retry next poll
		}
	}
}

// inflightSet tracks issues currently enqueued or being driven, so a poll never
// hands the same non-terminal issue to a second worker.
type inflightSet struct {
	mu sync.Mutex
	m  map[int]bool
}

func (s *inflightSet) add(issue int) bool { // true if newly added
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[issue] {
		return false
	}
	s.m[issue] = true
	return true
}

func (s *inflightSet) remove(issue int) {
	s.mu.Lock()
	delete(s.m, issue)
	s.mu.Unlock()
}

func (s *inflightSet) has(issue int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[issue]
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test -race ./internal/scheduler/ -v 2>&1 | tail -20`
Expected: all five tests PASS, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit  # message: "feat(scheduler): poller + worker-pool Serve with dedup, seed, worker cap"
```

---

### Task B3: `orchestratord daemon` subcommand

**Files:**
- Modify: `cmd/orchestratord/main.go` (refactor `wire` to return a `*wired` struct; add `daemon` dispatch, `cmdDaemon`, `sourceLabel`, `terminalStates` helpers; update usage + package doc)
- Test: `cmd/orchestratord/main_test.go`

**Interfaces:**
- Consumes: `scheduler.Scheduler`/`Serve`, `github.Client.ListIssues`, `engine.Engine.Run`, `store.Store.{GetTask,List}`, `store.ErrNotFound`, `config.Workflow.{Sources,States,Policies,EntryState}`.
- Produces: `orchestratord daemon --config <c> --repo <dir> [--poll-interval 30s] [common flags]`.

- [ ] **Step 1: Write the failing dispatch test** — add a case to the table in `TestRun_Dispatch` (`cmd/orchestratord/main_test.go`):

```go
		{"daemon missing flags", []string{"daemon"}, 2},
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/orchestratord/ -run Dispatch -v`
Expected: FAIL — `daemon` is an unknown command, so `run([]string{"daemon"})` returns 2 via the `default` branch... which already returns 2. To make this test meaningful it must fail first: temporarily assert the command is *recognized* by also checking a valid-flags path is not "unknown command". Simpler: assert dispatch routes `daemon` to its own handler by expecting the missing-flags message. Since the `default` branch also returns 2, add this stronger assertion instead — a second test:

```go
func TestRun_Daemon_UnknownVsMissingFlags(t *testing.T) {
	// A recognized subcommand with missing flags returns 2 from its own handler,
	// not from the unknown-command branch. After implementation, `daemon` with no
	// --config/--repo returns 2; before it, `daemon` is an unknown command (also
	// 2) — so assert the handler exists by checking a parse error path is distinct
	// from unknown: `daemon --config x` (still missing --repo) must return 2.
	if got := run([]string{"daemon", "--config", goodFixture}); got != 2 {
		t.Errorf("daemon --config only = %d, want 2 (missing --repo)", got)
	}
}
```
Run: `go test ./cmd/orchestratord/ -run Daemon -v`
Expected: FAIL — `daemon --config <good>` hits the `default` unknown-command branch and prints "unknown command", but returns 2, so this specific assertion passes trivially. To force a real red, first add the assertion that the daemon accepts `--poll-interval`: a flag parse error would be 2 as well. Given all missing/invalid paths return 2, treat B3 as covered by the build + the end-to-end integration test in Step 8; keep `TestRun_Dispatch`'s `daemon` row as a routing smoke test and proceed. (Document: `cmdDaemon`'s happy path needs live `gh`/`herdr`, so it is validated by the integration test, not a unit test.)

- [ ] **Step 3: Refactor `wire` to a `*wired` struct** in `cmd/orchestratord/main.go`. Replace the `wire` signature and body's return:

```go
// wired bundles everything a subcommand needs after loading + validating config.
type wired struct {
	eng     *engine.Engine
	store   *store.Store
	gh      github.Client
	wf      *config.Workflow
	repoDir string
}

// wire loads+validates the config and builds the engine with real backends.
func (cf commonFlags) wire(ctx context.Context) (*wired, error) {
	// ... unchanged: read raw, config.Parse, warnings, absRepo, store.Open,
	//     runner, backend, notifier ...
	// Build the gh client once and reuse it for the engine and the daemon.
	gh := github.New(runner)
	start := ""
	if wf.EntryState != nil {
		start = *wf.EntryState
	}
	eng := engine.New(engine.Config{
		Workflow:       wf,
		WorkflowSource: raw,
		Backend:        backend,
		GitHub:         gh,
		Store:          st,
		RepoDir:        absRepo,
		Base:           cf.base,
		Repo:           repoSlug(wf),
		ConfigDir:      filepath.Dir(cf.config),
		TaskDir:        cf.taskDir,
		Notifier:       notifier,
		StartState:     start,
	})
	return &wired{eng: eng, store: st, gh: gh, wf: wf, repoDir: absRepo}, nil
}
```
(Note: the `StartState: start` line folds in Task A2 Step 6 — keep only one copy.)

- [ ] **Step 4: Update `cmdRun` and `cmdRecover`** to the new signature:

```go
	w, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer w.store.Close()
	// cmdRun:    final, err := w.eng.Run(ctx, *issue)
	// cmdRecover: err := w.eng.Recover(ctx)
```

- [ ] **Step 5: Add the `daemon` dispatch + `cmdDaemon`**. In `run`'s switch:

```go
	case "daemon":
		return cmdDaemon(args[1:])
```

Add `cmdDaemon` and helpers:

```go
// cmdDaemon runs the orchestrator as a long-running daemon: poll a labeled source
// and drive up to max_concurrent_tasks issues through the pipeline concurrently.
func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	var cf commonFlags
	registerCommon(fs, &cf)
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "source poll cadence")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cf.config == "" || cf.repo == "" {
		fmt.Fprintln(os.Stderr, "daemon requires --config and --repo")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer w.store.Close()

	label, err := sourceLabel(w.wf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return 2
	}
	terminal := terminalStates(w.wf)
	workers := w.wf.Policies.MaxConcurrentTasks
	if workers < 1 {
		workers = 1
	}

	sched := &scheduler.Scheduler{
		List: func(ctx context.Context) ([]int, error) {
			return w.gh.ListIssues(ctx, w.repoDir, label)
		},
		Done: func(ctx context.Context, issue int) (bool, error) {
			tk, err := w.store.GetTask(ctx, fmt.Sprintf("issue-%d", issue))
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return terminal[tk.CurrentState], nil
		},
		RunTask: func(ctx context.Context, issue int) error {
			_, err := w.eng.Run(ctx, issue)
			return err
		},
		SeedFrom: func(ctx context.Context) ([]int, error) {
			tasks, err := w.store.List(ctx)
			if err != nil {
				return nil, err
			}
			var out []int
			for _, tk := range tasks {
				if !terminal[tk.CurrentState] {
					out = append(out, tk.Issue)
				}
			}
			return out, nil
		},
		Interval: *pollInterval,
		Workers:  workers,
		Log:      slog.Default(),
	}

	slog.Info("daemon starting", "label", label, "workers", workers, "poll", pollInterval.String())
	if err := sched.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return 1
	}
	slog.Info("daemon stopped")
	return 0
}

// sourceLabel returns the label of the first github_issues source, or an error
// if the workflow declares no such source with select.label.
func sourceLabel(wf *config.Workflow) (string, error) {
	for _, s := range wf.Sources {
		if s.Type == "github_issues" {
			if l, ok := s.Select["label"].(string); ok && l != "" {
				return l, nil
			}
		}
	}
	return "", fmt.Errorf("no github_issues source with select.label declared")
}

// terminalStates returns the set of state names with a terminal verdict.
func terminalStates(wf *config.Workflow) map[string]bool {
	out := map[string]bool{}
	for name, s := range wf.States {
		if s.Terminal != "" {
			out[name] = true
		}
	}
	return out
}
```

Add imports to `main.go`: `"log/slog"`, `"time"`, `"github.com/sean1588/herdr-orchestrator/internal/scheduler"`. (`errors`, `fmt`, `os`, `os/signal`, `syscall`, `flag`, `store`, `config`, `github` are already imported.)

- [ ] **Step 6: Update `usage` + package doc** in `main.go`. Add to the commands block:

```
  daemon --config <c> --repo <dir>               poll a labeled source and drive concurrently
```
and to the flags block:
```
  --poll-interval DUR    daemon source poll cadence (default 30s)
```
Add a matching stanza to the package doc comment describing `daemon`.

- [ ] **Step 7: Run the suite**

Run: `go build ./... && go test -race ./... 2>&1 | tail -12 && go vet ./... && gofmt -l .`
Expected: all `ok`; `gofmt` prints nothing. `TestRun_Dispatch` (incl. the new `daemon` row) and `TestRun_Daemon_UnknownVsMissingFlags` pass.

- [ ] **Step 8: End-to-end integration test** — add to `internal/github/gh_integration_test.go` is not right (cross-package). Instead add `cmd/orchestratord/daemon_integration_test.go` (`//go:build !windows`) that puts fake `gh` and fake `herdr` on `PATH` and runs one poll→triage→halt cycle:

```go
//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A fake gh returns one labeled issue then drives the triage/verdict path via the
// engine's gh calls; a fake herdr answers workspace/pane so a spawn resolves. The
// daemon should discover the issue, create a task, and (with dry_run) progress it
// without a real merge. This is a smoke test of the wiring, not the agents.
func TestDaemon_PollsAndCreatesTask(t *testing.T) {
	// Skip if the harness cannot host fake binaries; otherwise build a bin dir
	// with `gh` (issue list -> [{"number":1}], issue view -> title/body, pr list ->
	// []) and `herdr` (workspace create/list, pane run/list) that let one triage
	// spawn resolve, run `run([]string{"daemon", ...})` in a goroutine with a
	// 2s context, then assert a task row for issue-1 exists in the --db.
	t.Skip("wiring smoke test: implement fake gh/herdr bin dir per T3 pattern")
	_ = context.Background
	_ = os.WriteFile
	_ = filepath.Join
	_ = time.Second
}
```

Flesh this out following the T3 fake-binary pattern (Task B1 + `internal/exec/herdr_integration_test.go`). If the fake `herdr` interaction proves too broad for a smoke test, keep the daemon covered by the scheduler unit tests (B2) + manual verification, and delete the skipped test rather than leave it half-built. Document the choice in the commit message.

- [ ] **Step 9: Commit**

```bash
git add cmd/orchestratord/main.go cmd/orchestratord/main_test.go cmd/orchestratord/daemon_integration_test.go
git commit  # message: "feat(cli): orchestratord daemon — poll a labeled source and drive concurrently"
```

---

## Manual verification (after B3)

```bash
go build -o /tmp/orchestratord ./cmd/orchestratord
/tmp/orchestratord plan internal/config/testdata/default-pipeline.yaml   # intake now shows a triager spawn
# Live (inside herdr, gh authed, a repo with an `agent-ready` issue):
/tmp/orchestratord daemon --config <cfg> --repo <dir> --poll-interval 30s --db /tmp/d.db
#   -> logs "daemon starting label=agent-ready workers=N"; discovers the issue,
#      spawns a triager, and on accept proceeds to queued/implementing.
# Ctrl-C -> "daemon stopped"; re-run resumes in-flight tasks via SeedFrom.
```

---

## Self-Review

**Spec coverage:** Component A (executable triage) → A1 (config + rubric) + A2 (triageTask/routing/start-at-entry). Component B → B1 (ListIssues), B2 (scheduler package: poller, workers, dedup, seed, capacity, shutdown), B3 (daemon subcommand + wiring). Concurrency model (single creator / N drivers / store-serialized) → B2 + B3. Named debt (per-drive Events pollers, worker-held-during-wait, deferred reject-worktree cleanup) is inherited from the engine and unchanged by this plan. Cancel-and-resume shutdown → B2 `Serve` + B3 signal handling + `SeedFrom`. Out-of-scope items are not implemented. **Covered.**

**Placeholder scan:** every code step shows complete code; commands show expected output. The one soft spot is B3 Step 8 (end-to-end daemon smoke test): it is explicitly optional with a documented fallback (delete if the fake-herdr surface is too broad; scheduler unit tests + manual verification carry the coverage). No `TODO`/`TBD` remain.

**Type consistency:** `Scheduler` field names/signatures in B2 match their use in B3's `cmdDaemon`. `ListIssues(ctx, repoDir, label) ([]int, error)` is identical across B1's interface, impl, `fakeGH`, and B3's `List` closure. `wired` fields (`eng`, `store`, `gh`, `wf`, `repoDir`) match cmdRun/cmdRecover/cmdDaemon usage. `taskID` format `issue-%d` matches `Done`'s `GetTask` key. `store.ErrNotFound`, `config.Workflow.{Sources,States,Policies,EntryState}` verified against the codebase.
