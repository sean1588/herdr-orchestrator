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
	// The fake also asserts the --json field set: it emits the fixture only when
	// the request carries the fields PRStatus parses. So if PRStatus drops
	// statusCheckRollup or reviews from its query, the fake exits non-zero and the
	// test fails — that is the contract this integration test is meant to pin.
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = view ]; then\n" +
		"  case \"$*\" in\n" +
		"    *statusCheckRollup*reviews*) cat " + abs + "; exit 0 ;;\n" +
		"    *) echo \"fake gh: missing required --json fields in: $*\" >&2; exit 3 ;;\n" +
		"  esac\n" +
		"fi\n" +
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

func TestGH_ListIssues_AgainstFakeBinary(t *testing.T) {
	abs, err := filepath.Abs("testdata/issue_list.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Fake gh emits the fixture only for `gh issue list --label ... --json number`.
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = issue ] && [ \"$2\" = list ]; then\n" +
		"  case \"$*\" in *--label*--json*number*) cat " + abs + "; exit 0 ;; esac\n" +
		"  echo \"fake gh: unexpected issue list args: $*\" >&2; exit 3\n" +
		"fi\n" +
		"exit 9\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(body), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := github.New(proc.New()).ListIssues(context.Background(), t.TempDir(), "agent-ready")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []int{5, 8, 13}
	if len(got) != len(want) {
		t.Fatalf("ListIssues = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListIssues = %v, want %v", got, want)
		}
	}
}
