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
// terminal) starts it over: back to the start state through an operator.redrive
// audit row, with every tie to the previous attempt cleared, so the implementer
// spawns fresh and recreates the branch from base.
//
// The spawn is made to fail so the drive stops right after it and the persisted
// row still shows what the reset wrote. The prior pane is live (resolve=true) and
// was spawned for implementing, so a spawn happening at all proves PaneID and
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
				CurrentState: from, PaneID: "w0:p9", PaneSpawnState: "implementing",
				PRNumber: &pr, RetryCounts: map[string]int{"changes_requested": 2},
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := e.Run(ctx, 9); err == nil {
				t.Fatal("run: want the test's spawn error, got nil")
			}

			if !hasAudit(auditFor(t, st, "issue-9"), from, "queued", "operator.redrive", "") {
				t.Fatalf("missing %s->queued operator.redrive audit: %+v", from, auditFor(t, st, "issue-9"))
			}
			if len(b.spawnLog) != 1 {
				t.Fatalf("want 1 fresh spawn, got %d", len(b.spawnLog))
			}
			if sp := b.spawnLog[0]; sp.Role != "implementer" || sp.PreserveBranch {
				t.Errorf("spawn role=%q PreserveBranch=%v, want implementer with PreserveBranch false", sp.Role, sp.PreserveBranch)
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

// A redrive clears PRNumber, but the earlier attempt's PR can still be open on the
// branch. The implementer must then push onto that PR (a force push, since the
// branch was recreated from base) rather than run the fresh-task kickoff, whose
// plain push is rejected and whose `gh pr create` fails on the existing PR. With
// no open PR, the fresh-task kickoff is unchanged.
func TestRun_RedriveKickoffFollowsOpenPR(t *testing.T) {
	tests := []struct {
		name        string
		pr          *github.PR
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "open PR on the branch",
			pr:          &github.PR{Number: 42, State: "OPEN"},
			wantContain: []string{"PR #42", "git push --force -u origin agent/issue-9", "Do not open another PR"},
			wantAbsent:  []string{"gh pr create", "Stop when the PR is open"},
		},
		{
			name:        "no open PR",
			wantContain: []string{"git push -u origin agent/issue-9", "gh pr create", "Stop when the PR is open"},
			wantAbsent:  []string{"--force"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			b := &fakeBackend{pane: "w0:p9", spawnErr: errors.New("spawn halted by test")}
			e := newEngine(t, st, b, &fakeGH{pr: tt.pr}, 5*time.Second)
			ctx := context.Background()

			pr := 42
			if err := st.CreateTask(ctx, &store.Task{
				ID: "issue-9", Issue: 9, Repo: "owner/repo", Branch: "agent/issue-9",
				CurrentState: CancelState, PRNumber: &pr,
			}); err != nil {
				t.Fatal(err)
			}

			if _, err := e.Run(ctx, 9); err == nil {
				t.Fatal("run: want the test's spawn error, got nil")
			}
			if len(b.spawnLog) != 1 {
				t.Fatalf("want 1 spawn, got %d", len(b.spawnLog))
			}
			sp := b.spawnLog[0]
			if sp.PreserveBranch {
				t.Errorf("PreserveBranch = true, want the branch recreated from base")
			}
			for _, s := range tt.wantContain {
				if !strings.Contains(sp.Kickoff, s) {
					t.Errorf("kickoff missing %q: %s", s, sp.Kickoff)
				}
			}
			for _, s := range tt.wantAbsent {
				if strings.Contains(sp.Kickoff, s) {
					t.Errorf("kickoff must not contain %q: %s", s, sp.Kickoff)
				}
			}
		})
	}
}
