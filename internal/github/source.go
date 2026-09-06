package github

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sean1588/herdr-orchestrator/internal/source"
)

// IssueClient is the GitHub issue API, separate from pull-request operations.
type IssueClient interface {
	Issue(context.Context, string, int) (*Issue, error)
	ListIssues(context.Context, string, string) ([]int, error)
	RemoveLabel(context.Context, string, int, string) error
	CloseIssue(context.Context, string, int, string) error
}

// IssueSource adapts GitHub issues to work intake. RepoDir is bound here, rather
// than supplied by the engine on each call, so source and code repo can differ.
type IssueSource struct {
	Client  IssueClient
	RepoDir string
}

var _ source.Source = IssueSource{}

func issueNumber(key string) (int, error) {
	n, err := strconv.Atoi(key)
	if err != nil || n <= 0 || strconv.Itoa(n) != key {
		return 0, fmt.Errorf("invalid GitHub issue key %q: expected a canonical positive integer", key)
	}
	return n, nil
}

func sourceLabel(s source.Selector) (string, error) {
	if s["label"] == nil {
		return "", nil
	}
	label, ok := s["label"].(string)
	if !ok {
		return "", fmt.Errorf("GitHub source selector label must be a string")
	}
	return label, nil
}

func (s IssueSource) List(ctx context.Context, selector source.Selector) ([]string, error) {
	label, err := sourceLabel(selector)
	if err != nil {
		return nil, err
	}
	if label == "" {
		return nil, fmt.Errorf("GitHub discovery requires a nonempty label")
	}
	numbers, err := s.Client.ListIssues(ctx, s.RepoDir, label)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(numbers))
	for i, n := range numbers {
		keys[i] = strconv.Itoa(n)
	}
	return keys, nil
}

func (s IssueSource) Get(ctx context.Context, key string) (*source.Item, error) {
	n, err := issueNumber(key)
	if err != nil {
		return nil, err
	}
	item, err := s.Client.Issue(ctx, s.RepoDir, n)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("GitHub issue %s returned no item", key)
	}
	return &source.Item{Key: key, Title: item.Title, Body: item.Body}, nil
}

func (s IssueSource) Acknowledge(ctx context.Context, key string, selector source.Selector) error {
	n, err := issueNumber(key)
	if err != nil {
		return err
	}
	label, err := sourceLabel(selector)
	if err != nil {
		return err
	}
	if label == "" {
		return nil
	}
	return s.Client.RemoveLabel(ctx, s.RepoDir, n, label)
}

func (s IssueSource) Complete(ctx context.Context, key, comment string) error {
	n, err := issueNumber(key)
	if err != nil {
		return err
	}
	return s.Client.CloseIssue(ctx, s.RepoDir, n, comment)
}
