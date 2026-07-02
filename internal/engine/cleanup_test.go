package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

func newIntakeTask(t *testing.T, st *store.Store) *store.Task {
	t.Helper()
	task := &store.Task{ID: "issue-9", Issue: 9, Branch: "agent/issue-9", CurrentState: "intake"}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

// A task that settles at a no-PR terminal state (a triage reject -> closed, or an
// intake needs_human -> escalated) has its worktree + workspace cleaned up.
func TestDrive_NoPRTerminal_Cleans(t *testing.T) {
	tests := []struct{ name, verdict, wantTo string }{
		{"reject -> closed", "reject", "closed"},
		{"needs_human -> escalated", "needs_human", "escalated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			b := agentDoneBackend()
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			task := newIntakeTask(t, st)
			writeVerdict(t, e.taskDir, task.ID, `{"verdict":"`+tt.verdict+`","feedback":""}`)

			final, err := e.drive(context.Background(), task)
			if err != nil {
				t.Fatalf("drive: %v", err)
			}
			if final != tt.wantTo {
				t.Fatalf("final = %q, want %q", final, tt.wantTo)
			}
			if task.PRNumber != nil {
				t.Fatalf("precondition: task should have no PR, got %v", task.PRNumber)
			}
			if len(b.cleanups) != 1 || b.cleanups[0] != task.ID {
				t.Errorf("want Cleanup called once with %q, got %v", task.ID, b.cleanups)
			}
		})
	}
}

// A Cleanup failure must never fail the drive: the terminal halt still returns
// normally with the settled state.
func TestDrive_CleanupError_DoesNotFailDrive(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend()
	b.cleanupErr = errors.New("herdr momentarily unavailable")
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	task := newIntakeTask(t, st)
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"reject","feedback":""}`)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive must not fail on a Cleanup error, got %v", err)
	}
	if final != "closed" {
		t.Fatalf("final = %q, want closed", final)
	}
	if len(b.cleanups) != 1 || b.cleanups[0] != task.ID {
		t.Errorf("Cleanup should still have been attempted once with %q, got %v", task.ID, b.cleanups)
	}
}

// A terminal state that produced a PR keeps its worktree (a human may want it):
// an escalate verdict at pr_open reaches the terminal `escalated` with a PR, so
// no cleanup runs.
func TestDrive_TerminalWithPR_DoesNotClean(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend()
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	task := seedPROpen(t, st, 42)
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"escalate","feedback":""}`)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated", final)
	}
	if len(b.cleanups) != 0 {
		t.Errorf("a PR-bearing terminal must not be cleaned, got cleanups %v", b.cleanups)
	}
}

// The goal halt (pr_open) is not terminal and carries a PR: no cleanup runs.
func TestDrive_GoalHalt_DoesNotClean(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}, 5*time.Second)
	e.goal = "pr_open"

	if _, err := e.Run(context.Background(), 7); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(b.cleanups) != 0 {
		t.Errorf("goal halt (pr_open) must not be cleaned, got %v", b.cleanups)
	}
}
