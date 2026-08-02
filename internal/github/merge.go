package github

import (
	"context"
	"fmt"
	"strconv"
)

// Merge runs `gh pr merge <pr> --squash` in repoDir. It is a mutating call; the
// engine guards it behind a passing merge gate and a non-dry-run policy.
//
// Deliberately NOT --delete-branch: gh deletes the remote branch and then the
// local one, and the local delete cannot succeed — the branch is checked out in
// the task's own worktree. gh then exits non-zero for an *already merged* PR,
// which the engine could only read as a merge failure. Branch teardown is a
// separate, best-effort step (DeleteRemoteBranch) so a cleanup problem can never
// masquerade as a merge problem.
func (g *GH) Merge(ctx context.Context, repoDir string, pr int) error {
	if _, err := g.run.Run(ctx, repoDir, "gh", "pr", "merge", strconv.Itoa(pr), "--squash"); err != nil {
		return fmt.Errorf("gh pr merge %d: %w", pr, err)
	}
	return nil
}

// DeleteRemoteBranch deletes a merged PR's head branch from the remote via the
// refs API. It touches only the remote: the local branch is still checked out in
// the task's worktree, and that worktree's teardown owns it.
func (g *GH) DeleteRemoteBranch(ctx context.Context, repoDir, branch string) error {
	ref := "repos/{owner}/{repo}/git/refs/heads/" + branch
	if _, err := g.run.Run(ctx, repoDir, "gh", "api", "--method", "DELETE", ref); err != nil {
		return fmt.Errorf("gh api DELETE %s: %w", ref, err)
	}
	return nil
}

// CloseIssue closes an issue with an explanatory comment. The orchestrator merges
// the PR itself, so it — not the PR body — is what settles the issue: an agent
// that opens its PR with `gh pr create --fill` produces no "Closes #N" trailer,
// and the issue would otherwise stay open forever after a successful merge.
func (g *GH) CloseIssue(ctx context.Context, repoDir string, number int, comment string) error {
	args := []string{"issue", "close", strconv.Itoa(number), "--reason", "completed"}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	if _, err := g.run.Run(ctx, repoDir, "gh", args...); err != nil {
		return fmt.Errorf("gh issue close %d: %w", number, err)
	}
	return nil
}
