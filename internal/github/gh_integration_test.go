//go:build !windows

package github_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// fakeGh installs a fake `gh` on PATH that emits fixture for `gh pr view ...`,
// so the REAL github.Client parses captured `gh pr view --json` output. This
// pins the CLI JSON contract: a drift in gh's field shapes fails here, where the
// unit tests (which mock proc) would silently pass.
func fakeGh(t *testing.T, fixture string) {
	t.Helper()
	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = view ]; then cat " + abs + "; exit 0; fi\n" +
		"exit 9\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGH_PRStatus_CleanFixture(t *testing.T) {
	fakeGh(t, "testdata/pr_view_clean.json")
	c := github.New(proc.New())

	s, err := c.PRStatus(context.Background(), t.TempDir(), 227)
	if err != nil {
		t.Fatalf("PRStatus: %v", err)
	}
	if s.State != "OPEN" {
		t.Errorf("State = %q, want OPEN", s.State)
	}
	if !s.ChecksGreen() {
		t.Errorf("ChecksGreen=false; total=%d failed=%d pending=%d", s.ChecksTotal, s.ChecksFailed, s.ChecksPending)
	}
	// octocat's COMMENTED review is advisory; only reviewer1's APPROVED counts.
	if s.ApprovedReviews != 1 {
		t.Errorf("ApprovedReviews = %d, want 1 (COMMENTED must not count)", s.ApprovedReviews)
	}
	if s.MergeStateStatus != "CLEAN" {
		t.Errorf("MergeStateStatus = %q, want CLEAN", s.MergeStateStatus)
	}
}

func TestGH_PRStatus_UnstableFixture(t *testing.T) {
	fakeGh(t, "testdata/pr_view_unstable.json")
	c := github.New(proc.New())

	s, err := c.PRStatus(context.Background(), t.TempDir(), 227)
	if err != nil {
		t.Fatalf("PRStatus: %v", err)
	}
	if s.ChecksGreen() {
		t.Error("ChecksGreen=true, want false (one pending + one failing check)")
	}
	if s.ChecksPending != 1 || s.ChecksFailed != 1 {
		t.Errorf("pending=%d failed=%d, want 1/1", s.ChecksPending, s.ChecksFailed)
	}
	if s.MergeStateStatus != "BLOCKED" {
		t.Errorf("MergeStateStatus = %q, want BLOCKED", s.MergeStateStatus)
	}
}
