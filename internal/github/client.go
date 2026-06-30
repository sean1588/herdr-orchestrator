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

// Client reads GitHub state. repoDir is the local checkout to run gh in.
type Client interface {
	// FindPR returns the open PR whose head branch is `branch`, or (nil, nil) if
	// none exists. This is the authoritative artifact-detection signal.
	FindPR(ctx context.Context, repoDir, branch string) (*PR, error)
	// Issue fetches an issue's title and body by number.
	Issue(ctx context.Context, repoDir string, number int) (*Issue, error)
}
