package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/notify"
)

// recordingNotifier captures every Event for assertions and can be made to fail.
type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
	err    error
}

func (r *recordingNotifier) Notify(_ context.Context, ev notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return r.err
}

func (r *recordingNotifier) ofKind(kind string) []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notify.Event
	for _, e := range r.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Driving a task to escalated (here via the implementing timeout) fires exactly
// one "escalated" event, populated from the task.
func TestRun_Escalated_NotifiesOnce(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"} // no events: the agent never settles -> timeout
	rec := &recordingNotifier{}
	e := newEngine(t, st, b, &fakeGH{}, 10*time.Millisecond)
	e.notifier = rec

	final, err := e.Run(context.Background(), 21)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated", final)
	}
	got := rec.ofKind("escalated")
	if len(got) != 1 {
		t.Fatalf("escalated notifications = %d, want exactly 1 (%+v)", len(got), got)
	}
	if got[0].Issue != 21 || got[0].State != "escalated" || got[0].TaskID != "issue-21" {
		t.Errorf("escalated event = %+v, want Issue=21 State=escalated TaskID=issue-21", got[0])
	}
}

// A notifier that errors must not fail or block the drive: the task still
// reaches escalated and Run returns nil.
func TestRun_Escalated_NotifierErrorDoesNotFailDrive(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"}
	rec := &recordingNotifier{err: errors.New("webhook unreachable")}
	e := newEngine(t, st, b, &fakeGH{}, 10*time.Millisecond)
	e.notifier = rec

	final, err := e.Run(context.Background(), 22)
	if err != nil {
		t.Fatalf("run must not fail when the notifier errors: %v", err)
	}
	if final != "escalated" {
		t.Errorf("final = %q, want escalated", final)
	}
}

// An agent.blocked alert fires an "alert" event carrying the alert message,
// without changing state.
func TestRun_Blocked_NotifiesAlert(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateBlocked},
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	rec := &recordingNotifier{}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 5}}, 5*time.Second)
	e.goal = "pr_open"
	e.notifier = rec

	if _, err := e.Run(context.Background(), 23); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := rec.ofKind("alert")
	if len(got) != 1 {
		t.Fatalf("alert notifications = %d, want exactly 1 (%+v)", len(got), got)
	}
	if got[0].Detail != "needs_input" || got[0].State != "implementing" {
		t.Errorf("alert event = %+v, want Detail=needs_input State=implementing", got[0])
	}
}
