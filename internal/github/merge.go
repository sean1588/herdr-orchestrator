package github

import (
	"context"
	"fmt"
	"strconv"
)

// Merge runs `gh pr merge <pr> --squash --delete-branch` in repoDir. It is the
// only mutating call on the client; the engine guards it behind a passing merge
// gate and a non-dry-run policy.
func (g *GH) Merge(ctx context.Context, repoDir string, pr int) error {
	if _, err := g.run.Run(ctx, repoDir, "gh", "pr", "merge", strconv.Itoa(pr), "--squash", "--delete-branch"); err != nil {
		return fmt.Errorf("gh pr merge %d: %w", pr, err)
	}
	return nil
}
