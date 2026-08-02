package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/github"
)

func TestMerging_DryRun_HaltsWithoutMerging(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{} // default-pipeline ships dry_run: true
	b := &fakeBackend{}
	e := newEngine(t, st, b, gh, 5*time.Second)
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
	if len(b.cleanups) != 0 {
		t.Errorf("dry-run merging halt is not terminal and must not trigger cleanup, got %v", b.cleanups)
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
	// The orchestrator owns the merge, so it settles the issue too: the default
	// kickoff's `gh pr create --fill` writes no "Closes #N" trailer, so nothing
	// else ever closes the issue (it stayed open after a real run).
	if !slices.Equal(gh.closedIssues, []int{5}) {
		t.Errorf("closed issues = %v, want [5] (the task's source issue)", gh.closedIssues)
	}
	if len(gh.closeComments) != 1 || !strings.Contains(gh.closeComments[0], "#42") {
		t.Errorf("close comment %q should reference the merged PR #42", gh.closeComments)
	}
	if !slices.Equal(gh.deletedBranches, []string{"agent/issue-5"}) {
		t.Errorf("deleted branches = %v, want [agent/issue-5]", gh.deletedBranches)
	}
}

// The merge is irreversible by the time the bookkeeping runs, so a failure to
// close the issue or delete the branch must be a log line — never an error that
// fails the action and re-drives merging.
func TestMerging_BookkeepingFailuresDoNotFailTheMerge(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{
		closeErr:  errors.New("issue close forbidden"),
		deleteErr: errors.New("branch already gone"),
	}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged despite bookkeeping errors", final)
	}
	if len(gh.closedIssues) != 1 || len(gh.deletedBranches) != 1 {
		t.Errorf("both bookkeeping calls should still be attempted, got closes=%v deletes=%v",
			gh.closedIssues, gh.deletedBranches)
	}
}

// A dry run withholds every side effect, including the post-merge bookkeeping —
// closing the issue would be as unrecoverable as the merge itself.
func TestMerging_DryRun_DoesNotCloseIssueOrDeleteBranch(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{} // default-pipeline ships dry_run: true
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	task := seedAt(t, st, "merging", 42, nil)

	if _, err := e.drive(context.Background(), task); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if len(gh.closedIssues) != 0 || len(gh.deletedBranches) != 0 {
		t.Errorf("dry run must not settle anything, got closes=%v deletes=%v",
			gh.closedIssues, gh.deletedBranches)
	}
}

// Crash recovery: a prior run already merged the PR but died before persisting
// `merged`. Re-driving merging must NOT call Merge again (idempotent); it just
// fires pr.merged.
func TestMerging_AlreadyMerged_IsIdempotent(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{status: &github.PRStatus{State: "MERGED"}}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if gh.merges != 0 {
		t.Errorf("already-merged PR must not be re-merged, got %d Merge calls", gh.merges)
	}
}

// `gh pr merge` can exit 0 while the PR is still OPEN (queued behind a merge
// queue). The post-merge confirmation must reject that rather than declare
// success.
func TestMerging_MergeNotConfirmed_FailsDrive(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{mergeResultState: "OPEN"}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff
	task := seedAt(t, st, "merging", 42, nil)

	if _, err := e.drive(context.Background(), task); err == nil {
		t.Fatal("drive should error when the PR is not MERGED after merge")
	}
	if gh.merges != 1 {
		t.Errorf("Merge should have been attempted once, got %d", gh.merges)
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
