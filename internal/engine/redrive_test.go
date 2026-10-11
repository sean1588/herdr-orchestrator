package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// Run on a stopped task (operator-cancelled, or escalated to an alerting
// terminal) starts it over: back to the snapshot workflow's entry state (intake
// in the fixture) through an operator.redrive audit row, with every tie to the
// previous attempt cleared, so the triager spawns fresh and the branch is
// recreated from base.
//
// The engine's own startState is the default (queued) and differs from the
// fixture's entry_state, so landing in intake proves the reset reads the bound
// workflow, not the daemon's start state.
//
// The spawn is made to fail so the drive stops right after it and the persisted
// row still shows what the reset wrote. The prior pane is live (resolve=true) and
// was spawned for intake, so a spawn happening at all proves PaneID and
// PaneSpawnState were cleared — otherwise the engine would reuse that agent.
func TestRun_RedrivesStoppedTask(t *testing.T) {
	for _, from := range []string{CancelState, "escalated"} {
		t.Run(from, func(t *testing.T) {
			st := newStore(t)
			b := &fakeBackend{pane: "w0:p9", resolve: true, spawnErr: errors.New("spawn halted by test")}
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
			ctx := context.Background()

			pr := 42
			if err := st.CreateTask(ctx, &store.Task{
				ID: "issue-9", Issue: 9, Repo: "owner/repo", Branch: "agent/issue-9",
				CurrentState: from, PaneID: "w0:p9", PaneSpawnState: "intake",
				PRNumber: &pr, RetryCounts: map[string]int{"changes_requested": 2},
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := e.Run(ctx, 9); err == nil {
				t.Fatal("run: want the test's spawn error, got nil")
			}

			if !hasAudit(auditFor(t, st, "issue-9"), from, "intake", "operator.redrive", "") {
				t.Fatalf("missing %s->intake operator.redrive audit: %+v", from, auditFor(t, st, "issue-9"))
			}
			if len(b.spawnLog) != 1 {
				t.Fatalf("want 1 fresh spawn, got %d", len(b.spawnLog))
			}
			if sp := b.spawnLog[0]; sp.Role != "triager" || sp.PreserveBranch {
				t.Errorf("spawn role=%q PreserveBranch=%v, want triager with PreserveBranch false", sp.Role, sp.PreserveBranch)
			}
			got, _ := st.GetTask(ctx, "issue-9")
			if got.PaneID != "" || got.PaneSpawnState != "" || got.PRNumber != nil || len(got.RetryCounts) != 0 {
				t.Errorf("reset left PaneID=%q PaneSpawnState=%q PRNumber=%v RetryCounts=%v, want all cleared",
					got.PaneID, got.PaneSpawnState, got.PRNumber, got.RetryCounts)
			}
		})
	}
}

// A finished task (a non-alerting terminal) is not reset: Run halts on it exactly
// as before, with no redrive audit and no spawn.
func TestRun_FinishedTaskIsNotRedriven(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w0:p9"}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	ctx := context.Background()

	pr := 42
	if err := st.CreateTask(ctx, &store.Task{
		ID: "issue-9", Issue: 9, Repo: "owner/repo", Branch: "agent/issue-9",
		CurrentState: "merged", PRNumber: &pr,
	}); err != nil {
		t.Fatal(err)
	}

	final, err := e.Run(ctx, 9)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "merged" {
		t.Errorf("final = %q, want merged", final)
	}
	if rows := auditFor(t, st, "issue-9"); len(rows) != 0 {
		t.Errorf("a finished task must not be touched, got audit rows %+v", rows)
	}
	if b.spawns != 0 {
		t.Errorf("a finished task must not spawn, got %d", b.spawns)
	}
	if got, _ := st.GetTask(ctx, "issue-9"); got.PRNumber == nil || *got.PRNumber != 42 {
		t.Errorf("PRNumber = %v, want 42 kept", got.PRNumber)
	}
}

// A stopped task whose branch still has an open PR is refused: that PR would read
// as the new attempt's artifact (pr_exists, reconcile) before the new attempt
// pushed anything. Run names the PR and changes nothing — no reset, no audit, no
// spawn — so the operator closes it, deletes the branch, and enqueues again.
func TestRun_RedriveRefusedWhileBranchHasOpenPR(t *testing.T) {
	for _, from := range []string{CancelState, "escalated"} {
		t.Run(from, func(t *testing.T) {
			st := newStore(t)
			b := &fakeBackend{pane: "w0:p9", resolve: true}
			e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}, 5*time.Second)
			ctx := context.Background()

			pr := 42
			if err := st.CreateTask(ctx, &store.Task{
				ID: "issue-9", Issue: 9, Repo: "owner/repo", Branch: "agent/issue-9",
				CurrentState: from, PaneID: "w0:p9", PaneSpawnState: "implementing",
				PRNumber: &pr, RetryCounts: map[string]int{"changes_requested": 2},
			}); err != nil {
				t.Fatal(err)
			}

			_, err := e.Run(ctx, 9)
			if err == nil || !strings.Contains(err.Error(), "#42") {
				t.Fatalf("run: want an error naming PR #42, got %v", err)
			}

			got, _ := st.GetTask(ctx, "issue-9")
			if got.CurrentState != from || got.PaneID != "w0:p9" || got.PaneSpawnState != "implementing" ||
				got.PRNumber == nil || *got.PRNumber != 42 || got.RetryCounts["changes_requested"] != 2 {
				t.Errorf("row changed: state=%q PaneID=%q PaneSpawnState=%q PRNumber=%v RetryCounts=%v",
					got.CurrentState, got.PaneID, got.PaneSpawnState, got.PRNumber, got.RetryCounts)
			}
			if rows := auditFor(t, st, "issue-9"); len(rows) != 0 {
				t.Errorf("a refused redrive must not audit, got %+v", rows)
			}
			if b.spawns != 0 {
				t.Errorf("a refused redrive must not spawn, got %d", b.spawns)
			}
		})
	}
}

// A stopped task whose branch has no open PR but still exists on the remote (a
// closed PR's head, or a push before any PR) is refused too: the fresh branch's
// push would be rejected as non-fast-forward after the row was already reset.
// Run names the branch and changes nothing. A failed lookup refuses the same way.
func TestRun_RedriveRefusedWhileRemoteBranchExists(t *testing.T) {
	for _, tc := range []struct {
		name string
		gh   *fakeGH
		want string
	}{
		{name: "branch exists", gh: &fakeGH{remoteBranch: true}, want: "agent/issue-9"},
		{name: "lookup fails", gh: &fakeGH{remoteBranchErr: errors.New("gh api down")}, want: "gh api down"},
	} {
		for _, from := range []string{CancelState, "escalated"} {
			t.Run(tc.name+"/"+from, func(t *testing.T) {
				st := newStore(t)
				b := &fakeBackend{pane: "w0:p9", resolve: true}
				e := newEngine(t, st, b, tc.gh, 5*time.Second)
				ctx := context.Background()

				pr := 42
				if err := st.CreateTask(ctx, &store.Task{
					ID: "issue-9", Issue: 9, Repo: "owner/repo", Branch: "agent/issue-9",
					CurrentState: from, PaneID: "w0:p9", PaneSpawnState: "implementing",
					PRNumber: &pr, RetryCounts: map[string]int{"changes_requested": 2},
				}); err != nil {
					t.Fatal(err)
				}

				_, err := e.Run(ctx, 9)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("run: want an error containing %q, got %v", tc.want, err)
				}

				got, _ := st.GetTask(ctx, "issue-9")
				if got.CurrentState != from || got.PaneID != "w0:p9" || got.PaneSpawnState != "implementing" ||
					got.PRNumber == nil || *got.PRNumber != 42 || got.RetryCounts["changes_requested"] != 2 {
					t.Errorf("row changed: state=%q PaneID=%q PaneSpawnState=%q PRNumber=%v RetryCounts=%v",
						got.CurrentState, got.PaneID, got.PaneSpawnState, got.PRNumber, got.RetryCounts)
				}
				if rows := auditFor(t, st, "issue-9"); len(rows) != 0 {
					t.Errorf("a refused redrive must not audit, got %+v", rows)
				}
				if b.spawns != 0 {
					t.Errorf("a refused redrive must not spawn, got %d", b.spawns)
				}
			})
		}
	}
}
