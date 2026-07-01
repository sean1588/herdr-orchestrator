package engine

import (
	"context"
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

// reviewerDoneBackend emits a single agent.done for the reviewer pane.
func reviewerDoneBackend() *fakeBackend {
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
			b := reviewerDoneBackend()
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			if tt.halt != "" {
				e.goal = tt.halt
			}
			task := seedPROpen(t, st, 42)
			writeVerdict(t, e.taskDir, task.ID, `{"verdict":"`+tt.verdict+`","feedback":"do X"}`)

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
	b := reviewerDoneBackend()
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"approve","feedback":""}`)

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
	b := reviewerDoneBackend()
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"lgtm","feedback":""}`) // not a declared verdict

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error on a verdict outside the decision's declared set")
	}
}

func TestPROpen_MissingVerdictFile_IsError(t *testing.T) {
	st := newStore(t)
	b := reviewerDoneBackend()
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "approved"
	task := seedPROpen(t, st, 42)
	// no verdict file written

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error when the reviewer wrote no verdict file")
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
