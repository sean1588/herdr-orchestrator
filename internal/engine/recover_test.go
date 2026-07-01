package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// A recovered task must be driven against the graph it started under (its
// snapshot), not a possibly-edited current --config.
func TestRecover_UsesPerTaskSnapshot_NotCurrentConfig(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	raw, err := os.ReadFile("../config/testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// In-flight task that started under the real pipeline.
	if err := st.CreateTask(ctx, &store.Task{ID: "issue-9", Issue: 9, Repo: "o/r",
		Branch: "agent/issue-9", CurrentState: "implementing", WorkflowSnapshot: string(raw)}); err != nil {
		t.Fatal(err)
	}

	b := &fakeBackend{pane: "fresh:p1", resolve: true}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 99}}, 5*time.Second)
	e.goal = "pr_open"
	// Sabotage the engine's *current* wf so a recover that ignored the snapshot
	// would misbehave (no states => runState/reconcile can't resolve anything).
	e.wf = &config.Workflow{Name: "sabotaged", States: map[string]config.State{}}

	if err := e.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := st.GetTask(ctx, "issue-9")
	if got.CurrentState != "pr_open" {
		t.Errorf("state = %q, want pr_open (recover must use the snapshot graph)", got.CurrentState)
	}
}

// A snapshot that no longer parses/validates is skipped, not silently run
// against the current --config (fail-closed).
func TestRecover_InvalidSnapshot_IsSkipped(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	// Structurally valid YAML but schema-invalid (missing required name/states).
	if err := st.CreateTask(ctx, &store.Task{ID: "issue-4", Issue: 4, Repo: "o/r",
		Branch: "agent/issue-4", CurrentState: "implementing", WorkflowSnapshot: "version: 0\n"}); err != nil {
		t.Fatal(err)
	}

	b := &fakeBackend{pane: "fresh:p1", resolve: true}
	e := newEngine(t, st, b, &fakeGH{pr: &github.PR{Number: 7}}, 5*time.Second)

	if err := e.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, _ := st.GetTask(ctx, "issue-4")
	if got.CurrentState != "implementing" {
		t.Errorf("state = %q, want implementing (invalid snapshot must be skipped)", got.CurrentState)
	}
	if b.spawns != 0 {
		t.Errorf("spawns = %d, want 0 (must not drive a task with an unparseable snapshot)", b.spawns)
	}
}
