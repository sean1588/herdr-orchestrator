package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMerging_DryRun_HaltsWithoutMerging(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{} // default-pipeline ships dry_run: true
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merging" {
		t.Fatalf("final = %q, want merging (dry-run halts before merge)", final)
	}
	if gh.merges != 0 {
		t.Errorf("dry-run must not call Merge, got %d calls", gh.merges)
	}
	if !hasAudit(auditFor(t, st, task.ID), "merging", "merging", "dry_run", "would_merge") {
		t.Error("missing dry-run audit row")
	}
}

func TestMerging_RealMerge_ReachesMerged(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff // opt into a real merge
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if gh.merges != 1 || !gh.merged {
		t.Errorf("expected exactly one confirmed Merge (merges=%d merged=%v)", gh.merges, gh.merged)
	}
	if !hasAudit(auditFor(t, st, task.ID), "merging", "merged", "pr.merged", "") {
		t.Error("missing audit merging->merged pr.merged")
	}
}

func TestMerging_MergeError_FailsDrive(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{mergeErr: errors.New("not mergeable")}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 42, nil)

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error when gh pr merge fails")
	}
}

func TestMerging_NoPR_FailsDrive(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 0, nil)
	task.PRNumber = nil
	_ = st.UpdateTask(context.Background(), task)

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error on merge_pr with no detected PR")
	}
}

// Verify the default goal drives past pr_open by default (Phase 2a behavior):
// a default engine over the shipped pipeline halts at merging under dry-run, not
// at pr_open.
func TestDefaultGoal_IsMerged(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, 5*time.Second)
	if e.goal != "merged" {
		t.Fatalf("default goal = %q, want merged", e.goal)
	}
}
