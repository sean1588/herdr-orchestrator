package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/notify"
)

// recorder captures the events the engine emits.
type recorder struct{ events []notify.Event }

func (r *recorder) Notify(_ context.Context, ev notify.Event) error {
	r.events = append(r.events, ev)
	return nil
}

// An escalation must arrive with its diagnosis attached. Previously the payload
// carried only task/issue/state, so the recipient had to reconstruct the story
// from get_audit plus a pane read — the exact manual work #56 is about.
func TestEscalation_CarriesCauseHistoryAndRecommendation(t *testing.T) {
	st := newStore(t)
	rec := &recorder{}
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) {
		return "Do you want to allow git push? (y/n)", nil
	}}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.notifier = rec

	task := seedAt(t, st, "implementing", 0, nil)
	task.PaneID = "w1:p1"
	if err := st.UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	// Drive the task into the alerting terminal the way a real timeout would.
	if err := e.advance(context.Background(), task, "escalated", "blocked_timeout", ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	e.notifyTerminalAlert(context.Background(), task)

	if len(rec.events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Kind != "escalated" || ev.Issue != 5 {
		t.Errorf("wrong identity: %+v", ev)
	}
	if ev.Cause != "blocked_timeout" {
		t.Errorf("Cause = %q, want blocked_timeout", ev.Cause)
	}
	if len(ev.Recent) == 0 {
		t.Fatal("Recent is empty; the escalation should carry how the task got here")
	}
	if ev.Recent[0].To != "escalated" || ev.Recent[0].Trigger != "blocked_timeout" {
		t.Errorf("Recent[0] should be the escalating transition, got %+v", ev.Recent[0])
	}
	if !strings.Contains(ev.PaneTail, "allow git push") {
		t.Errorf("PaneTail = %q; it must carry what the agent last printed", ev.PaneTail)
	}
	if !strings.Contains(ev.Recommended, "permissions.allow") {
		t.Errorf("Recommended = %q; a blocked_timeout should point at the allow-list", ev.Recommended)
	}
}

// Non-alerting terminals stay silent — a merged task is not an escalation.
func TestNonAlertTerminal_EmitsNothing(t *testing.T) {
	st := newStore(t)
	rec := &recorder{}
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, 5*time.Second)
	e.notifier = rec

	task := seedAt(t, st, "merged", 42, nil)
	e.notifyTerminalAlert(context.Background(), task)

	if len(rec.events) != 0 {
		t.Errorf("got %d events, want 0 for a non-alerting terminal", len(rec.events))
	}
}

// Every part of the diagnosis is optional: an escalation missing its pane tail
// is still useful, one that never arrives because a diagnostic failed is not.
func TestEscalation_SurvivesDiagnosticFailures(t *testing.T) {
	st := newStore(t)
	rec := &recorder{}
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) {
		return "", context.DeadlineExceeded
	}}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	e.notifier = rec

	task := seedAt(t, st, "implementing", 0, nil)
	task.PaneID = "w1:p1"
	_ = st.UpdateTask(context.Background(), task)
	if err := e.advance(context.Background(), task, "escalated", "timeout", ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	e.notifyTerminalAlert(context.Background(), task)

	if len(rec.events) != 1 {
		t.Fatalf("a failed pane read must not suppress the escalation (got %d events)", len(rec.events))
	}
	if rec.events[0].PaneTail != "" {
		t.Errorf("PaneTail = %q, want empty after a read failure", rec.events[0].PaneTail)
	}
	if rec.events[0].Cause != "timeout" {
		t.Errorf("the rest of the diagnosis should survive: Cause = %q", rec.events[0].Cause)
	}
}

func TestRecommendFor_CoversEveryEscalationCause(t *testing.T) {
	for _, tc := range []struct{ cause, want string }{
		{"blocked_timeout", "permissions.allow"},
		{"no_progress", "no observable output"},
		{"drive_deadline", "wall-clock ceiling"},
		{"timeout", "state timeout"},
		{"retry_exhausted", "retry cap"},
		{"agent.done:fail", "no artifact"},
		{"", "audit trail"},
	} {
		got := recommendFor(tc.cause)
		if got == "" {
			t.Errorf("cause %q has no recommendation", tc.cause)
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("cause %q -> %q, want it to mention %q", tc.cause, got, tc.want)
		}
	}
}

// The engine records what the agent was last seen doing, so the supervision
// surface can distinguish a task that is moving from one parked on a prompt.
func TestRecordAgentStatus_TracksChangesOnly(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, 5*time.Second)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return clock }

	task := seedAt(t, st, "implementing", 0, nil)
	e.recordAgentStatus(context.Background(), task, "working")
	firstSeen := task.AgentStatusAt

	// Same status again: the timestamp must NOT move, or "blocked for 40m" would
	// reset to zero on every repeat observation.
	clock = clock.Add(10 * time.Minute)
	e.recordAgentStatus(context.Background(), task, "working")
	if !task.AgentStatusAt.Equal(firstSeen) {
		t.Errorf("AgentStatusAt moved on an unchanged status: %v -> %v", firstSeen, task.AgentStatusAt)
	}

	// A real change re-stamps it.
	e.recordAgentStatus(context.Background(), task, "blocked")
	if task.AgentStatus != "blocked" {
		t.Errorf("AgentStatus = %q, want blocked", task.AgentStatus)
	}
	if !task.AgentStatusAt.Equal(clock) {
		t.Errorf("AgentStatusAt = %v, want %v", task.AgentStatusAt, clock)
	}

	got, err := st.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentStatus != "blocked" {
		t.Errorf("status did not persist: %q", got.AgentStatus)
	}
}

// Time-in-state must come from the state transition, not from updated_at, which
// an agent-status write also bumps.
func TestStateEnteredAt_IsNotDisturbedByStatusWrites(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, 5*time.Second)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return clock }

	task := seedAt(t, st, "queued", 0, nil)
	if err := e.advance(context.Background(), task, "implementing", "scheduled", ""); err != nil {
		t.Fatal(err)
	}
	entered := task.StateEnteredAt
	if entered.IsZero() {
		t.Fatal("StateEnteredAt not stamped on advance")
	}

	clock = clock.Add(30 * time.Minute)
	e.recordAgentStatus(context.Background(), task, "working")

	got, _ := st.GetTask(context.Background(), task.ID)
	if !got.StateEnteredAt.Equal(entered) {
		t.Errorf("StateEnteredAt moved on a status write: %v -> %v", entered, got.StateEnteredAt)
	}
	if !got.UpdatedAt.After(entered) {
		t.Error("precondition: the status write should have bumped UpdatedAt")
	}
}
