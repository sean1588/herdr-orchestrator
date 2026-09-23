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

// A clean reject (triage -> closed) tears down its worktree; a needs_human
// escalation is PRESERVED, since it may hold uncommitted work a human wants to
// inspect (force-removing it is the data loss dogfood #34 flagged).
func TestDrive_NoPRTerminal_Cleanup(t *testing.T) {
	tests := []struct {
		name, verdict, wantTo string
		wantClean             bool
	}{
		{"reject -> closed tears down", "reject", "closed", true},
		{"needs_human -> escalated preserves worktree", "needs_human", "escalated", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			b := agentDoneBackend()
			b.verdictOnSpawn = map[string]string{"triager": `{"verdict":"` + tt.verdict + `","feedback":""}`}
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			task := newIntakeTask(t, st)

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
			wantN := 0
			if tt.wantClean {
				wantN = 1
			}
			if len(b.cleanups) != wantN {
				t.Fatalf("cleanups = %v, want %d", b.cleanups, wantN)
			}
			if tt.wantClean && b.cleanups[0] != task.ID {
				t.Errorf("want Cleanup with %q, got %v", task.ID, b.cleanups)
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
	b.verdictOnSpawn = map[string]string{"triager": `{"verdict":"reject","feedback":""}`}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	task := newIntakeTask(t, st)

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

// An alerting terminal is preserved whether or not it has a PR: an escalate
// verdict at pr_open reaches `escalated` with a PR, and its worktree may hold
// uncommitted work a human needs, so no cleanup runs.
func TestDrive_AlertTerminalWithPR_DoesNotClean(t *testing.T) {
	st := newStore(t)
	b := agentDoneBackend()
	b.verdictOnSpawn = map[string]string{"reviewer": `{"verdict":"escalate","feedback":""}`}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	task := seedPROpen(t, st, 42)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated", final)
	}
	if len(b.cleanups) != 0 {
		t.Errorf("an alerting terminal must not be cleaned, got cleanups %v", b.cleanups)
	}
}

// A merge tears down the worktree and workspace even though the task has a PR:
// after a squash merge every commit is on main and the PR is on GitHub, so nothing
// local is left to want.
func TestDrive_MergedWithPR_Cleans(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if len(b.cleanups) != 1 || b.cleanups[0] != task.ID {
		t.Errorf("want exactly one Cleanup with %q, got %v", task.ID, b.cleanups)
	}
}

// A re-run of an already-merged task did not transition into the terminal, so it
// does not repeat the teardown.
func TestDrive_AlreadyMerged_DoesNotCleanAgain(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	task := seedAt(t, st, "merged", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if len(b.cleanups) != 0 {
		t.Errorf("re-run of a settled merge must not clean again, got %v", b.cleanups)
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
