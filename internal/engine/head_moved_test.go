package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// The defect: in changes_requested the PR already exists — it was opened in the
// round that put the task there — so pr_exists could only ever pass. A resumed
// implementer that changed nothing advanced anyway, the reviewer re-reviewed
// unchanged code, and the loop burned retries until retry_exhausted.
//
// With the head_moved gate, an agent.done with an unmoved head fails the gate
// and escalates instead of laundering "nothing happened" into "pass".
func TestChangesRequested_UnmovedHeadDoesNotAdvance(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
	// The same head all the way through: the implementer committed nothing.
	gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}, headSHA: "sha-as-reviewed"}
	e := newEngine(t, st, b, gh, 5*time.Second)

	task := seedAt(t, st, "changes_requested", 42, nil)
	task.StateEntryHead = "sha-as-reviewed"
	if err := st.UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	writeVerdict(t, e.taskDir, task.ID, `{"verdict":"request_changes","feedback":"fix the off-by-one"}`)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated: an unmoved head is not evidence of work", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "changes_requested", "escalated", "agent.done", "fail") {
		t.Errorf("missing changes_requested->escalated fail row: %+v", auditFor(t, st, task.ID))
	}
}

// The baseline has to be captured when the task ENTERS the state, or the gate
// compares against the wrong commit. Driving pr_open -> changes_requested with a
// request_changes verdict must record the head as the reviewer saw it. Halting
// on entry is what makes this observable: by the time the task settles, later
// transitions have already cleared the baseline they no longer need.
func TestHeadBaselineIsRecordedOnEntry(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{
		pane:           "w1:p1",
		events:         []exec.Event{{PaneID: "w1:p1", State: exec.StateDone}},
		verdictOnSpawn: map[string]string{"reviewer": `{"verdict":"request_changes","feedback":"needs work"}`},
	}
	gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}, headSHA: "sha-as-reviewed"}
	e := newEngine(t, st, b, gh, 5*time.Second)
	e.goal = "changes_requested" // halt the moment we land there, baseline just written

	task := seedAt(t, st, "pr_open", 42, nil)
	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "changes_requested" {
		t.Fatalf("final = %q, want changes_requested", final)
	}
	got, _ := st.GetTask(context.Background(), task.ID)
	if got.StateEntryHead != "sha-as-reviewed" {
		t.Errorf("StateEntryHead = %q, want the head as the reviewer saw it", got.StateEntryHead)
	}
}

// A baseline is only worth a GitHub round trip for states that actually gate on
// commits. Every other transition — including the detached settle writes — must
// not pay for a read it will never use.
func TestHeadBaselineOnlyReadForStatesThatGateOnCommits(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, time.Second)

	if !e.stateGatesOnCommits("changes_requested") {
		t.Error("changes_requested evaluates head_moved; it needs a baseline")
	}
	for _, s := range []string{"implementing", "pr_open", "approved", "merging", "queued"} {
		if e.stateGatesOnCommits(s) {
			t.Errorf("state %q does not gate on commits; it must not trigger a baseline read", s)
		}
	}
}

// Losing the baseline (a status read that failed, or a task predating the
// column) must degrade to the old behavior rather than escalate a task on a
// question whose input was never captured.
func TestUnknownHeadBaselinePassesRatherThanEscalates(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{pane: "w1:p1"}, &fakeGH{headSHA: "sha-whatever"}, time.Second)

	pr := 42
	task := &store.Task{ID: "issue-9", CurrentState: "changes_requested", PRNumber: &pr} // no StateEntryHead
	g := e.wf.Gates["head_moved"]
	pass, err := e.gatePass(context.Background(), task, "head_moved", g, &github.PRStatus{HeadSHA: "sha-whatever"})
	if err != nil {
		t.Fatalf("gatePass: %v", err)
	}
	if !pass {
		t.Error("an unknown baseline must pass (degrade), not escalate the task")
	}
}
