// Package github reads GitHub state via the `gh` CLI. It exposes a small Client
// interface so the engine can detect PRs (the authoritative artifact signal) and
// fetch issues without depending on gh concretely.
package github

import "context"

// PR is an open pull request as reported by gh.
type PR struct {
	Number int
	URL    string
	State  string
}

// Issue is a GitHub issue's title and body, used to build the agent task file.
type Issue struct {
	Number int
	Title  string
	Body   string
}

// PRStatus is an authoritative snapshot of a PR's merge-gate inputs, read in one
// `gh pr view` so a single poll evaluates every merge gate against a consistent
// view. The engine maps each gate type onto these fields.
type PRStatus struct {
	State            string // OPEN | MERGED | CLOSED
	ChecksTotal      int
	ChecksFailed     int
	ChecksPending    int
	ApprovedReviews  int    // distinct authors whose latest review is APPROVED
	ReviewDecision   string // APPROVED | REVIEW_REQUIRED | CHANGES_REQUESTED | ""
	Mergeable        string // MERGEABLE | CONFLICTING | UNKNOWN
	MergeStateStatus string // CLEAN | BLOCKED | DIRTY | UNSTABLE | BEHIND | ...
}

// ChecksGreen reports whether no check is failing or pending. It is vacuously
// true when the PR has no checks at all (a repo with no CI does not block merge).
func (s PRStatus) ChecksGreen() bool { return s.ChecksFailed == 0 && s.ChecksPending == 0 }

// Client reads (and, for Merge, mutates) GitHub state. repoDir is the local
// checkout to run gh in.
type Client interface {
	// FindPR returns the open PR whose head branch is `branch`, or (nil, nil) if
	// none exists. This is the authoritative artifact-detection signal.
	FindPR(ctx context.Context, repoDir, branch string) (*PR, error)
	// Issue fetches an issue's title and body by number.
	Issue(ctx context.Context, repoDir string, number int) (*Issue, error)
	// ListIssues returns the numbers of issues matching label, via
	// `gh issue list --label <label> --json number` in repoDir.
	ListIssues(ctx context.Context, repoDir, label string) ([]int, error)
	// PRStatus reads the merge-gate inputs (state, checks, reviews, mergeability)
	// for a PR in one call.
	PRStatus(ctx context.Context, repoDir string, pr int) (*PRStatus, error)
	// Merge squash-merges a PR and deletes its head branch. It is side-effecting;
	// the engine calls it only from a gate-guarded, non-dry-run merge_pr action.
	Merge(ctx context.Context, repoDir string, pr int) error
}
