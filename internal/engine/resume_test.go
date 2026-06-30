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

func seedAt(t *testing.T, st *store.Store, state string, pr int, retry map[string]int) *store.Task {
	t.Helper()
	n := pr
	task := &store.Task{
		ID: "issue-5", Issue: 5, Branch: "agent/issue-5",
		CurrentState: state, PRNumber: &n, RetryCounts: retry,
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestChangesRequested_ResumeThenPRExists_LoopsToPROpen(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}
	e := newEngine(t, st, b, gh, 5*time.Second)
	e.goal = "pr_open"
	task := seedAt(t, st, "changes_requested", 42, nil)
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"request_changes","feedback":"fix the off-by-one in the loop"}`)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "pr_open" {
		t.Fatalf("final = %q, want pr_open", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "changes_requested", "pr_open", "agent.done", "pass") {
		t.Error("missing audit changes_requested->pr_open agent.done pass")
	}
	if b.spawns != 1 {
		t.Errorf("resumed implementer should spawn once, got %d", b.spawns)
	}
	if task.RetryCounts["changes_requested"] != 1 {
		t.Errorf("retry count = %d, want 1", task.RetryCounts["changes_requested"])
	}
	// the resumed implementer's task carries the reviewer feedback.
	content, err := os.ReadFile(b.spawnLog[0].TaskFile)
	if err != nil {
		t.Fatalf("read feedback task file: %v", err)
	}
	if !strings.Contains(string(content), "fix the off-by-one in the loop") {
		t.Errorf("feedback task file missing the reviewer feedback:\n%s", content)
	}
}

func TestChangesRequested_RetryExhausted_Escalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"} // no events: we exhaust at entry, before any wait
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.goal = "merged"
	// retry_caps.changes_requested is 3; seed at the cap so this entry exhausts it.
	task := seedAt(t, st, "changes_requested", 42, map[string]int{"changes_requested": 3})

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "changes_requested", "escalated", "retry_exhausted", "") {
		t.Error("missing audit changes_requested->escalated retry_exhausted")
	}
	if b.spawns != 0 {
		t.Errorf("must not resume once the retry cap is exhausted, spawns=%d", b.spawns)
	}
}

func TestChangesRequested_UnderCap_Resumes(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}
	e := newEngine(t, st, b, gh, 5*time.Second)
	e.goal = "pr_open"
	// already retried twice; a third (count 3 <= cap 3) still resumes.
	task := seedAt(t, st, "changes_requested", 42, map[string]int{"changes_requested": 2})

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "pr_open" || b.spawns != 1 {
		t.Fatalf("want resume to pr_open (final=%q spawns=%d)", final, b.spawns)
	}
	if task.RetryCounts["changes_requested"] != 3 {
		t.Errorf("retry count = %d, want 3", task.RetryCounts["changes_requested"])
	}
}
