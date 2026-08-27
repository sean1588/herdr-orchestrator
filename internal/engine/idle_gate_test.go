package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
)

// An implementer commonly lands at an idle prompt after opening its PR instead of
// reporting "done": herdr's live_prompt_box rule reads any text left in the prompt
// box as idle. Before this was handled, such a task waited for a "done" that never
// came and rode its 45m timeout into escalated with a finished PR sitting on
// GitHub — observed on a real run (issue #60 / PR #62). The gate is authoritative,
// so an idle whose PR exists must be treated as done.
func TestImplementing_IdleWithPR_TreatedAsDone(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateIdle}, // never reports done
	}}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 62, State: "OPEN"}}, 5*time.Second)
	e.goal = "pr_open" // exercise only the implementing -> pr_open slice

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, err := e.Run(ctx, 7)
	if err != nil {
		t.Fatalf("run: %v (idle-with-PR should transition, not hang)", err)
	}
	if final != "pr_open" {
		t.Fatalf("final state = %q, want pr_open", final)
	}
	task, _ := st.GetTask(context.Background(), "issue-7")
	if task.PRNumber == nil || *task.PRNumber != 62 {
		t.Errorf("PRNumber = %v, want 62", task.PRNumber)
	}
	if !hasAudit(auditFor(t, st, "issue-7"), "implementing", "pr_open", "agent.done", "pass") {
		t.Errorf("missing implementing->pr_open(pass) audit from idle-with-PR")
	}
}

// The idle shortcut must not mask a genuinely unfinished agent. An idle with no PR
// is simply "not done yet" — it keeps waiting rather than branching to fail — and a
// later real done with still no PR produces the same escalation as before (mirrors
// TestRun_DoneWithoutPR_Escalates).
func TestImplementing_IdleWithoutPR_KeepsWaitingThenEscalates(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateIdle}, // idle, but no PR opened
		{PaneID: "w1:p1", State: exec.StateDone}, // then done, still no PR
	}}
	e := newEngine(t, st, b, &fakeGH{pr: nil}, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, err := e.Run(ctx, 8)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final state = %q, want escalated", final)
	}
	// The escalation must come from the real done, not from the earlier idle:
	// an idle that branched to fail would be the bug this guards against.
	if !hasAudit(auditFor(t, st, "issue-8"), "implementing", "escalated", "agent.done", "fail") {
		t.Errorf("missing implementing->escalated(fail) audit")
	}
}

// The blocked guard is scoped to an *open* blocked window, not to "was ever
// blocked". Only `working` clears that window, so an agent that blocked, recovered,
// then finished at an idle prompt still takes the gate shortcut — otherwise a
// single early prompt would cost the task its whole state timeout.
func TestImplementing_BlockedThenRecoveredThenIdleWithPR_Advances(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateBlocked},
		{PaneID: "w1:p1", State: exec.StateWorking}, // recovery clears the blocked clock
		{PaneID: "w1:p1", State: exec.StateIdle},    // finished, PR is open
	}}
	e := blockedEngine(t, st, b, "10m")
	task := seedAt(t, st, "implementing", 42, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	final, err := e.drive(ctx, task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "pr_open" {
		t.Fatalf("final = %q, want pr_open (recovered, then finished at idle)", final)
	}
}
