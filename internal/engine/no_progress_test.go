package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// driveFrom runs one drive from a freshly-created task at the given state and
// returns the final state plus the audit trail.
func driveFrom(t *testing.T, e *Engine, st *store.Store, issue int, state string, pr *int) (string, []store.AuditEntry) {
	t.Helper()
	ctx := context.Background()
	id := TaskID(issue)
	if err := st.CreateTask(ctx, &store.Task{
		ID: id, Issue: issue, Repo: "owner/repo",
		Branch: branchName(issue), CurrentState: state, PRNumber: pr,
	}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(ctx, id)

	done := make(chan string, 1)
	go func() {
		final, err := e.drive(ctx, task)
		if err != nil {
			t.Errorf("drive: %v", err)
		}
		done <- final
	}()
	select {
	case final := <-done:
		return final, auditFor(t, st, id)
	case <-time.After(10 * time.Second):
		t.Fatal("drive never returned")
		return "", nil
	}
}

func auditTriggers(rows []store.AuditEntry) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Trigger)
	}
	return out
}

func hasTrigger(rows []store.AuditEntry, trigger string) bool {
	for _, r := range rows {
		if r.Trigger == trigger {
			return true
		}
	}
	return false
}

// An agent whose pane has produced nothing for a whole window is escalated, even
// though the state timeout has not come close to firing. This is the bound that
// measures progress rather than position.
func TestNoProgress_StaticPaneEscalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) { return "nothing ever changes", nil }}
	e := newEngine(t, st, b, &fakeGH{}, time.Hour) // state timeout far away
	e.noProgress = 40 * time.Millisecond

	final, rows := driveFrom(t, e, st, 11, "implementing", nil)
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated (no progress); audit triggers: %v", final, auditTriggers(rows))
	}
	if !hasAudit(rows, "implementing", "escalated", "no_progress", "state_timeout_target") {
		t.Errorf("missing no_progress audit row: %+v", rows)
	}
}

// The timer expiring is a suspicion, not a verdict. An agent working steadily
// emits no pane STATUS changes — the hub broadcasts only diffs — so the pane's
// own bytes are what keep it from being mistaken for a dead one. Here the state
// timeout is what eventually fires, proving the no-progress bound never did.
func TestNoProgress_MovingPaneIsNotEscalated(t *testing.T) {
	st := newStore(t)
	n := 0
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) {
		n++
		return fmt.Sprintf("output line %d", n), nil
	}}
	e := newEngine(t, st, b, &fakeGH{}, 400*time.Millisecond)
	e.noProgress = 30 * time.Millisecond

	final, rows := driveFrom(t, e, st, 12, "implementing", nil)
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated via the state timeout", final)
	}
	if hasTrigger(rows, "no_progress") {
		t.Errorf("a pane producing new output must not be escalated for no progress: %+v", rows)
	}
	if !hasTrigger(rows, "timeout") {
		t.Errorf("expected the state timeout to be what fired: %+v", rows)
	}
	if n < 2 {
		t.Errorf("confirmation read ran %d times; the bound was never actually exercised", n)
	}
}

// The #51 fix. pr_open declares no timeout transition, so before this every
// bound that escalated "to the state's timeout target" was silently inert there:
// the policy was set, the clock never armed, and nothing said so. The bound now
// resolves the workflow's alerting terminal instead.
func TestNoProgress_BoundsAStateThatDeclaresNoTimeout(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) { return "static", nil }}
	e := newEngine(t, st, b, &fakeGH{}, time.Hour)
	e.noProgress = 40 * time.Millisecond

	if findTimeoutTransition(e.wf.States["pr_open"]) != nil {
		t.Skip("fixture changed: pr_open now declares a timeout, so this is no longer the unbounded case")
	}

	pr := 42
	final, rows := driveFrom(t, e, st, 13, "pr_open", &pr)
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated; audit triggers: %v", final, auditTriggers(rows))
	}
	if !hasAudit(rows, "pr_open", "escalated", "no_progress", "alert_terminal") {
		t.Errorf("expected escalation via the alerting-terminal fallback: %+v", rows)
	}
}

// Same inertness, same fix, for the blocked bound: a state with no timeout
// transition still honors policies.blocked_timeout.
func TestBlockedTimeout_AppliesWithoutAStateTimeoutTransition(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{{PaneID: "w1:p1", State: exec.StateBlocked}}}
	e := newEngine(t, st, b, &fakeGH{}, 40*time.Millisecond) // DurationFunc governs blocked_timeout too
	e.wf.Policies.BlockedTimeout = "40ms"
	e.noProgress = 0 // isolate the blocked bound

	pr := 42
	final, rows := driveFrom(t, e, st, 14, "pr_open", &pr)
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated; audit triggers: %v", final, auditTriggers(rows))
	}
	if !hasAudit(rows, "pr_open", "escalated", "blocked_timeout", "alert_terminal") {
		t.Errorf("expected the blocked bound to fire via the alerting-terminal fallback: %+v", rows)
	}
}

// A herdr blip must never be the thing that escalates a task: an unreadable pane
// counts as progress. The cost is that a persistently unreadable pane keeps this
// bound from firing — which is why the state timeout and the scheduler's drive
// deadline remain the backstops.
func TestNoProgress_UnreadablePaneAssumesProgress(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) {
		return "", errors.New("herdr: connection refused")
	}}
	e := newEngine(t, st, b, &fakeGH{}, 300*time.Millisecond)
	e.noProgress = 30 * time.Millisecond

	final, rows := driveFrom(t, e, st, 15, "implementing", nil)
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated via the state timeout", final)
	}
	if hasTrigger(rows, "no_progress") {
		t.Errorf("an unreadable pane must not escalate for no progress: %+v", rows)
	}
}

// A pane status change is itself progress, so a stream of events keeps the clock
// from ever expiring even when the pane text is static.
func TestNoProgress_PaneEventsResetTheClock(t *testing.T) {
	st := newStore(t)
	b := &tickingBackend{pane: "w1:p1", every: 20 * time.Millisecond}
	e := newEngine(t, st, b, &fakeGH{}, 500*time.Millisecond)
	e.noProgress = 150 * time.Millisecond

	final, rows := driveFrom(t, e, st, 16, "implementing", nil)
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated via the state timeout", final)
	}
	if hasTrigger(rows, "no_progress") {
		t.Errorf("pane events must reset the progress clock: %+v", rows)
	}
}

// tickingBackend emits a status event on a fixed cadence with a static pane, so
// the only thing that can hold the no-progress clock open is the event stream.
type tickingBackend struct {
	fakeBackend
	pane  string
	every time.Duration
}

func (b *tickingBackend) Spawn(ctx context.Context, s exec.Spawn) (exec.Handle, error) {
	return exec.Handle{PaneID: b.pane, Workdir: "/wt"}, nil
}
func (b *tickingBackend) Read(ctx context.Context, h exec.Handle, lines int) (string, error) {
	return "static", nil
}
func (b *tickingBackend) Events(ctx context.Context) (<-chan exec.Event, error) {
	ch := make(chan exec.Event)
	go func() {
		defer close(ch)
		t := time.NewTicker(b.every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case ch <- exec.Event{PaneID: b.pane, State: exec.StateWorking}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}
