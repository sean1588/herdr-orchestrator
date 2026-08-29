package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// blockedEngine builds an engine whose blocked bound is far shorter than the
// state timeout, so a test can tell the two apart: the shipped durations differ
// only in the string, and newEngine's DurationFunc collapses every string to one
// value. "10m" is the blocked bound; the states use 15m/45m/30m.
func blockedEngine(t *testing.T, st *store.Store, b exec.ExecutionBackend, bound string) *Engine {
	t.Helper()
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}, 10*time.Second)
	e.goal = "pr_open"
	e.wf.Policies.BlockedTimeout = bound
	e.parseDur = func(s string) (time.Duration, error) {
		if s == "10m" { // the blocked bound
			return 5 * time.Millisecond, nil
		}
		return 10 * time.Second, nil // every state timeout: effectively never
	}
	return e
}

// The bug this bounds: an agent parked on an interactive prompt never reports
// done, and blocking doesn't change state, so before this the task sat until the
// state timeout (observed: 47 idle minutes inside a 60m implementing timeout).
func TestImplementing_BlockedPastBound_Escalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateBlocked},
	}} // ...and then nothing, forever: nobody answers the prompt
	e := blockedEngine(t, st, b, "10m")
	task := seedAt(t, st, "implementing", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated (blocked past the bound)", final)
	}
	// The trigger must name the blocked bound, not the state timeout — an operator
	// reading the audit has to be able to tell "parked on a prompt" apart from
	// "legitimately slow and overran".
	if !hasAudit(auditFor(t, st, task.ID), "implementing", "escalated", "blocked_timeout", "state_timeout_target") {
		t.Error("missing audit implementing->escalated blocked_timeout")
	}
}

// Without the policy the old behavior must be exact: blocking alerts and the task
// keeps waiting for the state timeout.
func TestImplementing_BlockedWithNoBound_KeepsWaiting(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateBlocked},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := blockedEngine(t, st, b, "") // no bound configured
	task := seedAt(t, st, "implementing", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "pr_open" {
		t.Fatalf("final = %q, want pr_open (no bound: blocked must not escalate)", final)
	}
}

// A prompt the agent resolves itself must not count against the bound, or the
// short fuse would misfire on transient blocking. Working is the recovery signal.
func TestImplementing_BlockedThenWorking_DoesNotEscalate(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateBlocked},
		{PaneID: "w1:p1", State: exec.StateWorking}, // recovered: clock clears
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := blockedEngine(t, st, b, "10m")
	task := seedAt(t, st, "implementing", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "pr_open" {
		t.Fatalf("final = %q, want pr_open (recovered before the bound)", final)
	}
}

// Idle is deliberately NOT recovery. An agent parked at an unanswerable prompt
// can report idle rather than blocked, which is exactly the case this bound
// exists to catch — treating idle as recovery would reopen the hole. Note
// `implementing` is a gate state, so an idle here carries no verdict shortcut.
func TestImplementing_BlockedThenIdle_StillEscalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateBlocked},
		{PaneID: "w1:p1", State: exec.StateIdle}, // still parked, just reported differently
	}}
	e := blockedEngine(t, st, b, "10m")
	task := seedAt(t, st, "implementing", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated (idle must not clear the blocked clock)", final)
	}
	// Assert the trigger, not just the terminal: the state timeout escalates too,
	// so only the trigger distinguishes "the bound fired" from "we waited it out".
	if !hasAudit(auditFor(t, st, task.ID), "implementing", "escalated", "blocked_timeout", "state_timeout_target") {
		t.Error("escalated, but not via the blocked bound")
	}
}

// A pane that keeps re-reporting blocked must not push the deadline out forever.
func TestImplementing_RepeatBlockedEvents_DoNotResetTheClock(t *testing.T) {
	st := newStore(t)
	evs := make([]exec.Event, 0, 40)
	for i := 0; i < 40; i++ {
		evs = append(evs, exec.Event{PaneID: "w1:p1", State: exec.StateBlocked})
	}
	b := &fakeBackend{pane: "w1:p1", events: evs}
	e := blockedEngine(t, st, b, "10m")
	task := seedAt(t, st, "implementing", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated (repeat blocked events must not restart the bound)", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "implementing", "escalated", "blocked_timeout", "state_timeout_target") {
		t.Error("escalated, but not via the blocked bound")
	}
}

// Events for another pane must not start the clock — one daemon drives many tasks
// and every task sees the whole event stream.
func TestImplementing_BlockedOnAnotherPane_IsIgnored(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w9:p9", State: exec.StateBlocked}, // a different task's agent
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	e := blockedEngine(t, st, b, "10m")
	task := seedAt(t, st, "implementing", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "pr_open" {
		t.Fatalf("final = %q, want pr_open (another pane's block is not ours)", final)
	}
}
