package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// A drive reaped for exceeding its wall-clock ceiling settles to the state's own
// timeout target, so a deadline escalation lands exactly where the timeout it is
// standing in for would have — and, crucially, settles at all: an aborted drive
// would be re-driven and re-reaped forever.
func TestDriveDeadlineSettlesToTheStateTimeoutTarget(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"} // no events: the drive parks in awaitAgentState
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)

	bg := context.Background()
	if err := st.CreateTask(bg, &store.Task{
		ID: "issue-9", Issue: 9, Repo: "owner/repo",
		Branch: "agent/issue-9", CurrentState: "implementing",
	}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(bg, "issue-9")

	ctx, cancel := context.WithCancelCause(bg)
	done := make(chan string, 1)
	go func() {
		final, _ := e.drive(ctx, task)
		done <- final
	}()
	time.Sleep(50 * time.Millisecond) // let the drive reach the agent wait
	cancel(ErrDriveDeadline)

	select {
	case final := <-done:
		timeoutT := findTimeoutTransition(e.wf.States["implementing"])
		if timeoutT == nil {
			t.Fatal("fixture no longer has an implementing timeout transition")
		}
		want := timeoutT.To
		got, _ := st.GetTask(bg, "issue-9")
		if final != want || got.CurrentState != want {
			t.Fatalf("final=%q persisted=%q, want %q", final, got.CurrentState, want)
		}
		if !hasAudit(auditFor(t, st, "issue-9"), "implementing", want, "drive_deadline", "state_timeout_target") {
			t.Fatalf("missing drive_deadline audit row: %+v", auditFor(t, st, "issue-9"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not return after the deadline cancel")
	}
}

// deadlineTarget's fallbacks, in order: the state's timeout edge, then the
// workflow's alerting terminal, then CancelState. The last one matters most —
// without it a workflow that declares no escalation terminal would leave a
// reaped task non-settled and the reaper would chase it forever.
func TestDeadlineTargetFallbacks(t *testing.T) {
	alerting := config.State{Terminal: "needs_human", Alert: true}
	timedOut := config.State{Transitions: []config.Transition{
		{When: config.Trigger{Timeout: "5m"}, To: "escalated"},
	}}

	tests := []struct {
		name       string
		states     map[string]config.State
		from       string
		wantTarget string
		wantWhy    string
	}{
		{
			name:       "state timeout target wins",
			states:     map[string]config.State{"implementing": timedOut, "escalated": alerting},
			from:       "implementing",
			wantTarget: "escalated",
			wantWhy:    "state_timeout_target",
		},
		{
			name:       "no timeout edge falls back to the alerting terminal",
			states:     map[string]config.State{"pr_open": {}, "escalated": alerting},
			from:       "pr_open",
			wantTarget: "escalated",
			wantWhy:    "alert_terminal",
		},
		{
			name:       "no escalation target at all settles as cancelled",
			states:     map[string]config.State{"pr_open": {}, "merged": {Terminal: "success"}},
			from:       "pr_open",
			wantTarget: CancelState,
			wantWhy:    "no_escalation_target",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{wf: &config.Workflow{States: tc.states}}
			target, why := e.deadlineTarget(tc.from)
			if target != tc.wantTarget || why != tc.wantWhy {
				t.Errorf("deadlineTarget(%q) = (%q, %q), want (%q, %q)", tc.from, target, why, tc.wantTarget, tc.wantWhy)
			}
		})
	}
}

// Map iteration is randomized, so the alerting-terminal fallback must not pick a
// different state on each restart.
func TestAlertTerminalIsDeterministic(t *testing.T) {
	e := &Engine{wf: &config.Workflow{States: map[string]config.State{
		"zulu":      {Terminal: "needs_human", Alert: true},
		"alpha":     {Terminal: "needs_human", Alert: true},
		"middle":    {Terminal: "needs_human", Alert: true},
		"not_alert": {Terminal: "rejected"},
	}}}
	for i := 0; i < 20; i++ {
		if got := e.alertTerminal(); got != "alpha" {
			t.Fatalf("alertTerminal() = %q on iteration %d, want the lowest-sorted alerting terminal %q", got, i, "alpha")
		}
	}
}
