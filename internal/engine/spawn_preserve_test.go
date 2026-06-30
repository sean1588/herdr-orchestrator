package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// The engine sets exec.Spawn.PreserveBranch from whether a PR already exists: a
// fresh implementer spawn recreates the branch; a reviewer/resume spawn (PR
// detected) must keep it so the PR's commits survive.
func TestSpawn_PreserveBranch_DerivedFromPR(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)
	ctx := context.Background()

	// Fresh implementer spawn, no PR yet -> PreserveBranch false.
	t1 := &store.Task{ID: "issue-1", Issue: 1, Branch: "agent/issue-1", CurrentState: "implementing"}
	if err := st.CreateTask(ctx, t1); err != nil {
		t.Fatal(err)
	}
	if err := e.spawn(ctx, t1, "implementer", e.wf.States["implementing"]); err != nil {
		t.Fatalf("spawn implementer: %v", err)
	}
	if b.spawnLog[0].PreserveBranch {
		t.Error("fresh implementer spawn must not preserve the branch")
	}

	// Reviewer spawn after a PR exists -> PreserveBranch true.
	n := 42
	t2 := &store.Task{ID: "issue-2", Issue: 2, Branch: "agent/issue-2", CurrentState: "pr_open", PRNumber: &n}
	if err := st.CreateTask(ctx, t2); err != nil {
		t.Fatal(err)
	}
	if err := e.spawn(ctx, t2, "reviewer", e.wf.States["pr_open"]); err != nil {
		t.Fatalf("spawn reviewer: %v", err)
	}
	if !b.spawnLog[1].PreserveBranch {
		t.Error("re-spawn with a detected PR must preserve the branch")
	}
}
