package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// --- fakes ---

type fakeBackend struct {
	pane       string
	events     []exec.Event
	resolve    bool
	resolveErr error
	spawns     int
	resumes    int
	spawnLog   []exec.Spawn
	resumeLog  []string // kickoffs sent to Resume
}

func (f *fakeBackend) Spawn(ctx context.Context, s exec.Spawn) (exec.Handle, error) {
	f.spawns++
	f.spawnLog = append(f.spawnLog, s)
	return exec.Handle{PaneID: f.pane, Workdir: "/wt"}, nil
}
func (f *fakeBackend) Resume(ctx context.Context, h exec.Handle, kickoff string) error {
	f.resumes++
	f.resumeLog = append(f.resumeLog, kickoff)
	return nil
}
func (f *fakeBackend) WaitState(ctx context.Context, h exec.Handle, target exec.AgentState) (exec.AgentState, error) {
	return target, nil
}
func (f *fakeBackend) Read(ctx context.Context, h exec.Handle, lines int) (string, error) {
	return "", nil
}
func (f *fakeBackend) Events(ctx context.Context) (<-chan exec.Event, error) {
	ch := make(chan exec.Event)
	go func() {
		defer close(ch)
		for _, ev := range f.events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done() // keep the stream open until the engine is done with it
	}()
	return ch, nil
}
func (f *fakeBackend) Resolve(ctx context.Context, label string) (exec.Handle, bool, error) {
	if f.resolveErr != nil {
		return exec.Handle{}, false, f.resolveErr
	}
	return exec.Handle{PaneID: f.pane, Workdir: "/wt"}, f.resolve, nil
}
func (f *fakeBackend) Close(ctx context.Context, h exec.Handle) error { return nil }

type fakeGH struct {
	pr    *github.PR
	issue *github.Issue

	// merge-gate inputs (M9/M10): a single status, or a sequence consumed one per
	// PRStatus call to simulate a PR whose state changes across polls.
	status    *github.PRStatus
	statusSeq []*github.PRStatus
	statusIdx int

	merged   bool
	mergeErr error
	merges   int
}

func (g *fakeGH) FindPR(ctx context.Context, repoDir, branch string) (*github.PR, error) {
	return g.pr, nil
}
func (g *fakeGH) Issue(ctx context.Context, repoDir string, n int) (*github.Issue, error) {
	if g.issue != nil {
		return g.issue, nil
	}
	return &github.Issue{Number: n, Title: "Test", Body: "Body"}, nil
}
func (g *fakeGH) PRStatus(ctx context.Context, repoDir string, pr int) (*github.PRStatus, error) {
	if len(g.statusSeq) > 0 {
		i := min(g.statusIdx, len(g.statusSeq)-1)
		g.statusIdx++
		return g.statusSeq[i], nil
	}
	if g.status != nil {
		return g.status, nil
	}
	return &github.PRStatus{State: "OPEN"}, nil
}
func (g *fakeGH) Merge(ctx context.Context, repoDir string, pr int) error {
	g.merges++
	if g.mergeErr != nil {
		return g.mergeErr
	}
	g.merged = true
	return nil
}

func newEngine(t *testing.T, st *store.Store, b exec.ExecutionBackend, gh github.Client, timeout time.Duration) *Engine {
	t.Helper()
	wf, _, err := config.Load("../config/testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return New(Config{
		Workflow:     wf,
		Backend:      b,
		GitHub:       gh,
		Store:        st,
		RepoDir:      "/repo",
		Base:         "main",
		Repo:         "owner/repo",
		ConfigDir:    "../config/testdata",
		TaskDir:      t.TempDir(),
		DurationFunc: func(string) (time.Duration, error) { return timeout, nil },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func auditFor(t *testing.T, st *store.Store, id string) []store.AuditEntry {
	t.Helper()
	rows, err := st.Audit(context.Background(), id)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	return rows
}

func hasAudit(rows []store.AuditEntry, from, to, trigger, result string) bool {
	for _, r := range rows {
		if r.FromState == from && r.ToState == to && r.Trigger == trigger && r.Result == result {
			return true
		}
	}
	return false
}

// --- the transition table for the slice ---

func TestRun_DoneWithPR_ReachesPROpen(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}, 5*time.Second)

	final, err := e.Run(context.Background(), 7)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "pr_open" {
		t.Errorf("final state = %q, want pr_open", final)
	}
	if b.spawns != 1 {
		t.Errorf("implementer should be spawned exactly once, got %d", b.spawns)
	}

	task, _ := st.GetTask(context.Background(), "issue-7")
	if task.PRNumber == nil || *task.PRNumber != 42 {
		t.Errorf("PRNumber = %v, want 42", task.PRNumber)
	}
	if task.Branch != "agent/issue-7" {
		t.Errorf("branch = %q, want agent/issue-7 (deterministic)", task.Branch)
	}

	rows := auditFor(t, st, "issue-7")
	if !hasAudit(rows, "queued", "implementing", "scheduled", "") {
		t.Errorf("missing queued->implementing audit: %+v", rows)
	}
	if !hasAudit(rows, "implementing", "pr_open", "agent.done", "pass") {
		t.Errorf("missing implementing->pr_open(pass) audit: %+v", rows)
	}
}

func TestRun_DoneWithoutPR_Escalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := newEngine(t, st, b, &fakeGH{pr: nil}, 5*time.Second) // "done but no artifact"

	final, err := e.Run(context.Background(), 8)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "escalated" {
		t.Errorf("final state = %q, want escalated", final)
	}
	if !hasAudit(auditFor(t, st, "issue-8"), "implementing", "escalated", "agent.done", "fail") {
		t.Errorf("missing implementing->escalated(fail) audit")
	}
}

func TestRun_BlockedThenDone_AlertsAndContinues(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateBlocked},
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 5}}, 5*time.Second)

	final, err := e.Run(context.Background(), 11)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "pr_open" {
		t.Errorf("final = %q, want pr_open", final)
	}
	rows := auditFor(t, st, "issue-11")
	// blocked raised an alert without changing state, then the run continued to pr_open.
	if !hasAudit(rows, "implementing", "implementing", "agent.blocked", "alert:needs_input") {
		t.Errorf("missing blocked alert audit: %+v", rows)
	}
	if !hasAudit(rows, "implementing", "pr_open", "agent.done", "pass") {
		t.Errorf("missing final pr_open audit: %+v", rows)
	}
}

func TestRun_Timeout_Escalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"} // no events: the agent never settles
	e := newEngine(t, st, b, &fakeGH{}, 10*time.Millisecond)

	final, err := e.Run(context.Background(), 12)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "escalated" {
		t.Errorf("final = %q, want escalated", final)
	}
	if !hasAudit(auditFor(t, st, "issue-12"), "implementing", "escalated", "timeout", "") {
		t.Errorf("missing timeout->escalated audit")
	}
}

// A transient Resolve error must NOT be treated as "no live agent": doing so
// would re-spawn over a possibly-live worktree, force-removing it and destroying
// uncommitted work.
func TestSpawn_ResolveErrorDoesNotDestructivelyRespawn(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.CreateTask(ctx, &store.Task{
		ID: "issue-5", Issue: 5, Branch: "agent/issue-5",
		CurrentState: "implementing", PaneID: "live:p1", PaneSpawnState: "implementing",
	}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, "issue-5")
	b := &fakeBackend{pane: "x:p1", resolveErr: errors.New("herdr momentarily unavailable")}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)

	err := e.spawn(ctx, task, "implementer", e.wf.States["implementing"])
	if err == nil {
		t.Fatal("spawn should error when Resolve fails for an existing agent, not re-spawn")
	}
	if b.spawns != 0 {
		t.Errorf("must not launch a fresh agent on a transient Resolve error (would destroy live worktree), spawns=%d", b.spawns)
	}
}

func TestReconcile_ResolveErrorPropagatesAndKeepsPane(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.CreateTask(ctx, &store.Task{
		ID: "issue-6", Issue: 6, Branch: "agent/issue-6",
		CurrentState: "implementing", PaneID: "live:p1",
	}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, "issue-6")
	b := &fakeBackend{resolveErr: errors.New("herdr unavailable")}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)

	if err := e.reconcile(ctx, task); err == nil {
		t.Fatal("reconcile should propagate a Resolve error, not swallow it")
	}
	if task.PaneID != "live:p1" {
		t.Errorf("pane id must not be cleared on a transient error, got %q", task.PaneID)
	}
}

func TestRecover_ImplementingWithPR_ReconcilesToPROpen(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	// Seed an in-flight task as if the daemon crashed mid-implementing.
	if err := st.CreateTask(ctx, &store.Task{
		ID: "issue-9", Issue: 9, Repo: "owner/repo",
		Branch: "agent/issue-9", CurrentState: "implementing", PaneID: "stale:p1",
	}); err != nil {
		t.Fatal(err)
	}
	// The agent finished while we were down: a PR now exists; pane re-resolves.
	b := &fakeBackend{pane: "fresh:p1", resolve: true}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 99}}, 5*time.Second)

	if err := e.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if b.spawns != 0 {
		t.Errorf("recover must not spawn a new agent when a PR already exists, spawns=%d", b.spawns)
	}
	got, _ := st.GetTask(ctx, "issue-9")
	if got.CurrentState != "pr_open" {
		t.Errorf("state = %q, want pr_open", got.CurrentState)
	}
	if got.PRNumber == nil || *got.PRNumber != 99 {
		t.Errorf("PRNumber = %v, want 99", got.PRNumber)
	}
	if got.PaneID != "fresh:p1" {
		t.Errorf("pane not re-resolved: %q", got.PaneID)
	}
	if !hasAudit(auditFor(t, st, "issue-9"), "implementing", "pr_open", "reconcile", "pass") {
		t.Errorf("missing reconcile audit")
	}
}
