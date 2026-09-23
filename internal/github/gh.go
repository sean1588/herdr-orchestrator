package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// GH is a Client backed by the `gh` CLI, run through a proc.Runner.
type GH struct {
	run proc.Runner
}

// New returns a Client that shells out to gh via r.
func New(r proc.Runner) Client { return &GH{run: r} }

// FindPR runs `gh pr list --head <branch> --json number,url,state` in repoDir and
// returns the first PR, or (nil, nil) when no open PR matches the branch.
func (g *GH) FindPR(ctx context.Context, repoDir, branch string) (*PR, error) {
	out, err := g.run.Run(ctx, repoDir, "gh", "pr", "list", "--head", branch, "--json", "number,url,state")
	if err != nil {
		return nil, fmt.Errorf("gh pr list --head %s: %w", branch, err)
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list output: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	pr := prs[0]
	return &pr, nil
}

// Issue runs `gh issue view <number> --json number,title,body` in repoDir.
func (g *GH) Issue(ctx context.Context, repoDir string, number int) (*Issue, error) {
	out, err := g.run.Run(ctx, repoDir, "gh", "issue", "view", strconv.Itoa(number), "--json", "number,title,body")
	if err != nil {
		return nil, fmt.Errorf("gh issue view %d: %w", number, err)
	}
	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parse gh issue view output: %w", err)
	}
	return &issue, nil
}

// ListIssues runs `gh issue list --label <label> --json number,blockedBy` in
// repoDir and returns the matching issues, each with the numbers of its blockers
// whose state is not CLOSED. blockedBy carries each blocker's state, so one call
// answers both questions.
func (g *GH) ListIssues(ctx context.Context, repoDir, label string) ([]ListedIssue, error) {
	out, err := g.run.Run(ctx, repoDir, "gh", "issue", "list", "--label", label, "--json", "number,blockedBy")
	if err != nil {
		return nil, fmt.Errorf("gh issue list --label %s: %w", label, err)
	}
	var items []struct {
		Number    int `json:"number"`
		BlockedBy struct {
			Nodes []struct {
				Number int    `json:"number"`
				State  string `json:"state"`
			} `json:"nodes"`
		} `json:"blockedBy"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, fmt.Errorf("parse gh issue list output: %w", err)
	}
	issues := make([]ListedIssue, len(items))
	for i, it := range items {
		issues[i].Number = it.Number
		for _, b := range it.BlockedBy.Nodes {
			if b.State != "CLOSED" {
				issues[i].OpenBlockers = append(issues[i].OpenBlockers, b.Number)
			}
		}
	}
	return issues, nil
}

// RemoveLabel runs `gh issue edit <number> --remove-label <label>` in repoDir.
// gh treats removing a label the issue does not carry as a no-op (exit 0), so
// this is idempotent — the daemon can call it whenever an issue is settled
// without tracking whether the label is still present.
func (g *GH) RemoveLabel(ctx context.Context, repoDir string, number int, label string) error {
	if _, err := g.run.Run(ctx, repoDir, "gh", "issue", "edit", strconv.Itoa(number), "--remove-label", label); err != nil {
		return fmt.Errorf("gh issue edit %d --remove-label %s: %w", number, label, err)
	}
	return nil
}
