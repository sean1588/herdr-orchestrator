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
	// HeadSHA is the PR head commit (headRefOid). It is the artifact-movement
	// signal: comparing it against the SHA recorded when a task entered its
	// current state answers "did anything actually get committed here?".
	HeadSHA string
}

// ChecksGreen reports whether no check is failing or pending. It is vacuously
// true when the PR has no checks at all (a repo with no CI does not block merge).
func (s PRStatus) ChecksGreen() bool { return s.ChecksFailed == 0 && s.ChecksPending == 0 }

// Client reads (and, for Merge, mutates) GitHub state. repoDir is the local
// checkout to run gh in.
type Client interface {
	IssueClient
	PullRequests
}

// PullRequests is the code-host boundary. It has no work-intake operations.
type PullRequests interface {
	// FindPR returns the open PR whose head branch is `branch`, or (nil, nil) if
	// none exists. This is the authoritative artifact-detection signal.
	FindPR(ctx context.Context, repoDir, branch string) (*PR, error)
	// PRStatus reads the merge-gate inputs (state, checks, reviews, mergeability)
	// for a PR in one call.
	PRStatus(ctx context.Context, repoDir string, pr int) (*PRStatus, error)
	// Merge squash-merges a PR. It is side-effecting; the engine calls it only
	// from a gate-guarded, non-dry-run merge_pr action. It does not touch the
	// branch — see DeleteRemoteBranch.
	Merge(ctx context.Context, repoDir string, pr int) error
	// DeleteRemoteBranch removes a merged PR's head branch from the remote. The
	// engine calls it best-effort after a confirmed merge, so a branch that
	// cannot be deleted never fails the merge.
	DeleteRemoteBranch(ctx context.Context, repoDir, branch string) error
}
