package engine

import (
	"context"
	"fmt"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// runMergeAction performs the merging state's merge_pr entry. It honors
// policies.dry_run (default-on): a dry run logs the intended merge and halts at
// merging without touching GitHub (the side effect is withheld, so pr.merged
// never fires). A real run squash-merges, verifies the PR is MERGED
// (authoritative), then fires pr.merged -> merged.
//
// This is the one side-effecting action; the validator guarantees merging is
// reachable only through a gate, and dry_run gates the side effect itself.
func (e *Engine) runMergeAction(ctx context.Context, task *store.Task, st config.State) (next, trigger, result string, err error) {
	if st.Entry.Action != "merge_pr" {
		return "", "", "", fmt.Errorf("state %q: unsupported entry.action %q", task.CurrentState, st.Entry.Action)
	}
	if task.PRNumber == nil {
		return "", "", "", fmt.Errorf("state %q: merge_pr with no detected PR", task.CurrentState)
	}
	pr := *task.PRNumber

	if e.wf.Policies.DryRunEnabled() {
		e.log.Info("dry-run: would merge PR (set policies.dry_run: false to merge)", "task", task.ID, "pr", pr)
		return "", "dry_run", "would_merge", nil // halt at merging
	}

	// Idempotent across crash recovery: if a prior run already merged this PR but
	// died before persisting `merged`, the PR is MERGED — skip the merge and just
	// fire the transition. Only merge a PR that is still open.
	status, err := e.gh.PRStatus(ctx, e.repoDir, pr)
	if err != nil {
		return "", "", "", fmt.Errorf("check PR %d before merge: %w", pr, err)
	}
	if status.State != "MERGED" {
		if err := e.gh.Merge(ctx, e.repoDir, pr); err != nil {
			return "", "", "", fmt.Errorf("merge PR %d: %w", pr, err)
		}
		// Confirm the merge against the authoritative source before declaring
		// success: `gh pr merge` can exit 0 while the PR is only queued.
		status, err = e.gh.PRStatus(ctx, e.repoDir, pr)
		if err != nil {
			return "", "", "", fmt.Errorf("verify merge of PR %d: %w", pr, err)
		}
		if status.State != "MERGED" {
			return "", "", "", fmt.Errorf("merge of PR %d not confirmed (PR state %q)", pr, status.State)
		}
	}
	mt := findEventTransition(st, "pr.merged")
	if mt == nil {
		return "", "", "", fmt.Errorf("state %q: no pr.merged transition", task.CurrentState)
	}
	e.log.Info("merged PR", "task", task.ID, "pr", pr)
	return mt.To, "pr.merged", "", nil
}
