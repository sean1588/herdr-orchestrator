package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
)

// fullLoopBackend replays working+done on every agent wait, so both the
// implementer (implementing) and the reviewer (pr_open) settle to done.
func fullLoopBackend() *fakeBackend {
	return &fakeBackend{pane: "w1:p1", events: []exec.Event{
		{PaneID: "w1:p1", State: exec.StateWorking},
		{PaneID: "w1:p1", State: exec.StateDone},
	}}
}

// TestE2E_IssueToMerged drives one issue the whole way: queued -> implementing ->
// pr_open -> (review approve) -> approved -> (merge gate) -> merging -> merged.
func TestE2E_IssueToMerged(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}, status: greenStatus()}
	e := newEngine(t, st, fullLoopBackend(), gh, 5*time.Second)
	dryOff := false
	e.wf.Policies.DryRun = &dryOff // opt into a real merge
	writeVerdict(t, e.taskDir, "issue-7", `{"verdict":"approve","feedback":""}`)

	final, err := e.Run(context.Background(), 7)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if !gh.merged || gh.merges != 1 {
		t.Errorf("expected one confirmed merge (merged=%v merges=%d)", gh.merged, gh.merges)
	}
	rows := auditFor(t, st, "issue-7")
	for _, step := range [][4]string{
		{"queued", "implementing", "scheduled", ""},
		{"implementing", "pr_open", "agent.done", "pass"},
		{"pr_open", "approved", "agent.done", "approve"},
		{"approved", "merging", "status.changed", "pass"},
		{"merging", "merged", "pr.merged", ""},
	} {
		if !hasAudit(rows, step[0], step[1], step[2], step[3]) {
			t.Errorf("missing audit step %v->%v (%v/%v)", step[0], step[1], step[2], step[3])
		}
	}
}

// TestE2E_DryRun_HaltsAtMerging drives the full loop under the shipped
// dry_run: true policy and stops at merging without merging the PR.
func TestE2E_DryRun_HaltsAtMerging(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}, status: greenStatus()}
	e := newEngine(t, st, fullLoopBackend(), gh, 5*time.Second)
	writeVerdict(t, e.taskDir, "issue-7", `{"verdict":"approve","feedback":""}`)

	final, err := e.Run(context.Background(), 7)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "merging" {
		t.Fatalf("final = %q, want merging (dry-run halt)", final)
	}
	if gh.merges != 0 {
		t.Errorf("dry-run must not merge, got %d Merge calls", gh.merges)
	}
	if !hasAudit(auditFor(t, st, "issue-7"), "approved", "merging", "status.changed", "pass") {
		t.Error("should have reached merging through the merge gate")
	}
}
