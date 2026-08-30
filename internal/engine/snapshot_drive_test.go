package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// seedWithSnapshot creates an in-flight task carrying an explicit workflow
// snapshot — the shape the daemon's re-drive path actually encounters.
func seedWithSnapshot(t *testing.T, st *store.Store, state string, pr int, snapshot string) *store.Task {
	t.Helper()
	n := pr
	task := &store.Task{
		ID: "issue-5", Issue: 5, Branch: "agent/issue-5",
		CurrentState: state, PRNumber: &n, WorkflowSnapshot: snapshot,
	}
	if err := st.CreateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func shippedConfig(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../config/testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("read shipped config: %v", err)
	}
	return string(b)
}

// The bug this file exists for: the daemon re-drives in-flight tasks against
// --config, so flipping dry_run in the file and restarting used to make tasks
// already parked in `merging` perform a real merge. dry_run's whole job is to
// withhold a side effect, so it must keep applying to work already in flight.
func TestRun_HonorsSnapshotDryRun_OverEditedConfig(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)

	// The task started under the shipped config, which ships dry_run: true.
	seedWithSnapshot(t, st, "merging", 42, shippedConfig(t))
	// The operator then edits the live config to dry_run: false and restarts.
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff

	final, err := e.Run(context.Background(), 5)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gh.merges != 0 || gh.merged {
		t.Errorf("snapshot says dry_run: true, so no merge may happen (merges=%d merged=%v)", gh.merges, gh.merged)
	}
	if final == "merged" {
		t.Errorf("final = %q; a dry run must halt at merging, not reach merged", final)
	}
	if len(gh.closedIssues) != 0 {
		t.Errorf("dry run must not close issues, closed %v", gh.closedIssues)
	}
}

// The mirror of the above: with no snapshot (a legacy row written before
// snapshots existed) the task falls back to the current config, so the same
// edit does take effect. This is what keeps the fix from silently freezing
// pre-existing rows.
func TestRun_EmptySnapshot_FallsBackToCurrentConfig(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)

	seedWithSnapshot(t, st, "merging", 42, "")
	dryRunOff := false
	e.wf.Policies.DryRun = &dryRunOff

	final, err := e.Run(context.Background(), 5)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if final != "merged" {
		t.Fatalf("final = %q, want merged", final)
	}
	if gh.merges != 1 {
		t.Errorf("merges = %d, want 1 (legacy row follows the current config)", gh.merges)
	}
}

// Fail closed: a snapshot that no longer parses must stop the drive with a
// diagnosable error, never fall back to the current config — falling back is
// exactly the behavior this issue is about.
func TestRun_InvalidSnapshot_FailsClosed(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)

	seedWithSnapshot(t, st, "merging", 42, "not: a: valid: workflow")

	_, err := e.Run(context.Background(), 5)
	if err == nil {
		t.Fatal("expected an error for an unparseable snapshot, got nil")
	}
	if !strings.Contains(err.Error(), "snapshot invalid") {
		t.Errorf("error %q should name the snapshot as the cause", err)
	}
	if gh.merges != 0 {
		t.Errorf("a fail-closed drive must not merge (merges=%d)", gh.merges)
	}
}
