package engine

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// seedPROpen creates a task already at pr_open with a detected PR.
func seedPROpen(t *testing.T, st *store.Store, pr int) *store.Task {
	t.Helper()
	n := pr
	task := &store.Task{
		ID: "issue-5", Issue: 5, Branch: "agent/issue-5",
		CurrentState: "pr_open", PRNumber: &n,
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func writeVerdict(t *testing.T, dir, taskID, body string) {
	t.Helper()
	if err := os.WriteFile(verdictPath(dir, taskID), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// agentDoneBackend emits a single agent.done for the spawned pane (any role).
func agentDoneBackend() *fakeBackend {
	return &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
}

func TestPROpen_ReviewVerdict_Branches(t *testing.T) {
	tests := []struct {
		name    string
		verdict string
		halt    string // engine goal to halt at (the branch target)
		wantTo  string
	}{
		{"approve -> approved", "approve", "approved", "approved"},
		{"request_changes -> changes_requested", "request_changes", "changes_requested", "changes_requested"},
		// escalated is terminal, so any non-pr_open goal lets pr_open execute first.
		{"escalate -> escalated (terminal)", "escalate", "merged", "escalated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			b := agentDoneBackend()
			b.verdictOnSpawn = map[string]string{"reviewer": `{"verdict":"` + tt.verdict + `","feedback":"do X"}`}
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			if tt.halt != "" {
				e.goal = tt.halt
			}
			task := seedPROpen(t, st, 42)

			final, err := e.drive(context.Background(), task)
			if err != nil {
				t.Fatalf("drive: %v", err)
			}
			if final != tt.wantTo {
				t.Fatalf("final = %q, want %q", final, tt.wantTo)
			}
			if !hasAudit(auditFor(t, st, task.ID), "pr_open", tt.wantTo, "agent.done", tt.verdict) {
				t.Errorf("missing audit pr_open->%s agent.done %s", tt.wantTo, tt.verdict)
			}
			if b.spawns != 1 {
				t.Errorf("reviewer should spawn exactly once, got %d", b.spawns)
			}
		})
	}
}

func TestPROpen_ReviewerTask_CarriesRubricAndVerdictInstruction(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend()
	b.verdictOnSpawn = map[string]string{"reviewer": `{"verdict":"approve","feedback":""}`}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)

	if _, err := e.drive(context.Background(), task); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(b.spawnLog) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(b.spawnLog))
	}
	sp := b.spawnLog[0]
	// The reviewer's task file is the rubric + PR pointer.
	content, err := os.ReadFile(sp.TaskFile)
	if err != nil {
		t.Fatalf("read review task file: %v", err)
	}
	if !strings.Contains(string(content), "Review rubric") || !strings.Contains(string(content), "PR #42") {
		t.Errorf("review task file missing rubric or PR pointer:\n%s", content)
	}
	// The kickoff tells the reviewer to write the verdict file.
	if !strings.Contains(sp.Kickoff, "verdict") || !strings.Contains(sp.Kickoff, verdictPath(e.taskDir, task.ID)) {
		t.Errorf("kickoff does not instruct writing the verdict file: %q", sp.Kickoff)
	}
}

func TestPROpen_InvalidVerdict_IsError(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend()
	b.verdictOnSpawn = map[string]string{"reviewer": `{"verdict":"lgtm","feedback":""}`} // not a declared verdict
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error on a verdict outside the decision's declared set")
	}
}

func TestPROpen_MissingVerdictFile_IsError(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend()
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)
	// no verdict file written

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error when the reviewer wrote no verdict file")
	}
}

func TestClearVerdict(t *testing.T) {
	dir := t.TempDir()
	// A no-op (no error) when there is nothing to clear.
	if err := clearVerdict(dir, "issue-1"); err != nil {
		t.Fatalf("clearVerdict on an absent file = %v, want nil", err)
	}
	// Removes an existing verdict.
	writeVerdict(t, dir, "issue-1", `{"verdict":"approve","feedback":""}`)
	if err := clearVerdict(dir, "issue-1"); err != nil {
		t.Fatalf("clearVerdict = %v, want nil", err)
	}
	if _, err := os.Stat(verdictPath(dir, "issue-1")); !os.IsNotExist(err) {
		t.Fatalf("verdict file still present after clearVerdict (stat err = %v)", err)
	}
}

// A reviewer that reaches "done" WITHOUT writing a verdict (a dead pane) must not
// let the engine re-consume a stale verdict left from a prior round: spawning the
// reviewer clears the slate, so the missing write surfaces as a read error rather
// than a silent, legitimate-looking branch on the old verdict.
func TestPROpen_StaleVerdictNotReconsumed_IsError(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend() // reviewer reaches done but writes nothing (no verdictOnSpawn)
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)
	// A stale verdict from a prior round sits on disk.
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"approve","feedback":"STALE prior round"}`)

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive must error when the reviewer reached done without writing (a stale verdict must not be re-consumed)")
	}
}

// The same guarantee at the pipeline entry: a triager that reaches done without
// writing must not re-consume a stale triage verdict.
func TestIntake_StaleVerdictNotReconsumed_IsError(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend() // triager reaches done but writes nothing
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "queued"
	task := &store.Task{ID: "issue-9", Issue: 9, Branch: "agent/issue-9", CurrentState: "intake"}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"accept","feedback":"STALE prior round"}`)

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive must error when the triager reached done without writing (a stale verdict must not be re-consumed)")
	}
}

// A resumed implementer with an unreadable verdict file must still proceed (empty
// feedback), but the swallowed read must be logged — it is otherwise the one
// silent failure in an engine that logs every other.
func TestFeedbackTask_MissingVerdict_LogsWarning(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, agentDoneBackend(), &fakeGH{}, 5*time.Second)
	var buf bytes.Buffer
	e.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pr := 42
	task := &store.Task{ID: "issue-5", Issue: 5, Branch: "agent/issue-5", CurrentState: "changes_requested", PRNumber: &pr}
	// no verdict file present -> the read fails

	if _, _, err := e.feedbackTask(task, "review_feedback"); err != nil {
		t.Fatalf("feedbackTask must not fail on a missing verdict: %v", err)
	}
	if !strings.Contains(buf.String(), "verdict") {
		t.Errorf("expected a warning mentioning the unreadable verdict; logs: %q", buf.String())
	}
}

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
			b.verdictOnSpawn = map[string]string{"triager": `{"verdict":"` + tt.verdict + `","feedback":""}`}
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			e.goal = tt.goal
			task := &store.Task{ID: "issue-9", Issue: 9, Branch: "agent/issue-9", CurrentState: "intake"}
			if err := st.CreateTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}

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
			if len(b.spawnLog) > 0 && !strings.Contains(b.spawnLog[0].Kickoff, "Triage issue") {
				t.Errorf("intake spawn kickoff = %q, want a triage kickoff", b.spawnLog[0].Kickoff)
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
	b.verdictOnSpawn = map[string]string{"triager": `{"verdict":"accept","feedback":""}`}
	gh := &fakeGH{issue: &github.Issue{Number: 9, Title: "Add feature", Body: "the details"}}
	e := newEngine(t, st, b, gh, 5*time.Second)
	e.goal = "queued"
	task := &store.Task{ID: "issue-9", Issue: 9, Branch: "agent/issue-9", CurrentState: "intake"}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}

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
