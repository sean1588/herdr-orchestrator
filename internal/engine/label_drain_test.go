package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The bug: the daemon drains the source label only for issues `gh issue list`
// returns, but that defaults to OPEN issues and the merge path closes the issue
// as part of settling. So for the one outcome the pipeline exists to produce,
// the drain could never run and merged issues kept the label forever.
func TestMerge_DrainsSourceLabel(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff // a real merge closes the issue
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if !slices.Equal(gh.removedLabels, []string{"5:agent-ready"}) {
		t.Errorf("removed labels = %v, want [5:agent-ready] — the merged issue must not keep the queue label", gh.removedLabels)
	}
}

// Uniform across settle paths, not just merge: an escalation settles too, and
// leaving the label on an escalated issue would re-list it forever.
func TestEscalate_DrainsSourceLabel(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	task := seedAt(t, st, "escalated", 0, nil)

	if err := e.advance(context.Background(), task, "escalated", "test", ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !slices.Equal(gh.removedLabels, []string{"5:agent-ready"}) {
		t.Errorf("removed labels = %v, want [5:agent-ready]", gh.removedLabels)
	}
}

// An operator cancel settles to the reserved CancelState, which is terminal but
// is not a workflow state — isSettled must still recognize it.
func TestCancel_DrainsSourceLabel(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	task := seedAt(t, st, "implementing", 0, nil)

	if _, err := e.settleCancelled(context.Background(), task); err != nil {
		t.Fatalf("settleCancelled: %v", err)
	}
	if !slices.Equal(gh.removedLabels, []string{"5:agent-ready"}) {
		t.Errorf("removed labels = %v, want [5:agent-ready]", gh.removedLabels)
	}
}

// The other half of the contract: a task still moving must keep its label. A
// mid-pipeline transition is not a settle, and draining early would drop work
// out of the queue while it is still being driven.
func TestInFlightTransition_KeepsSourceLabel(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	task := seedAt(t, st, "queued", 0, nil)

	if err := e.advance(context.Background(), task, "implementing", "scheduled", ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(gh.removedLabels) != 0 {
		t.Errorf("removed labels = %v, want none — the task is still in flight", gh.removedLabels)
	}
}

// The dry-run halt at `merging` is not a settle: the side effect was withheld,
// the issue stays open, and the daemon's poll-time drain still covers it. The
// engine must not treat it as terminal.
func TestDryRunMergingHalt_KeepsSourceLabel(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{} // shipped config: dry_run: true
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	task := seedAt(t, st, "merging", 42, nil)

	if _, err := e.drive(context.Background(), task); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(gh.closedIssues) != 0 {
		t.Fatalf("precondition: a dry run must not close the issue, closed %v", gh.closedIssues)
	}
	if len(gh.removedLabels) != 0 {
		t.Errorf("removed labels = %v, want none — nothing settled, the issue is still open", gh.removedLabels)
	}
}

// The drain is bookkeeping after an irreversible transition. A GitHub failure
// must be a log line, never an error that fails the drive or re-runs the merge.
func TestLabelDrainFailure_DoesNotFailTheDrive(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{removeLabelErr: errors.New("gh: label service down")}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("a failed label drain must not fail the drive: %v", err)
	}
	if final != "merged" {
		t.Errorf("final = %q, want merged", final)
	}
	if gh.merges != 1 {
		t.Errorf("merges = %d, want exactly 1 (no re-run of the merge action)", gh.merges)
	}
}
