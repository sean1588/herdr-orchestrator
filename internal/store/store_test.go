package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func intptr(n int) *int { return &n }

// newStore opens a fresh file-backed store in a temp dir.
func newStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func sampleTask(id string) *Task {
	return &Task{
		ID:           id,
		Issue:        5,
		Repo:         "sean1588/minicode",
		Branch:       "agent/" + id,
		CurrentState: "implementing",
		PaneID:       "%3",
		PRNumber:     nil,
		RetryCounts:  map[string]int{"changes_requested": 2},
	}
}

func TestCreateGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	in := sampleTask("issue-5")
	if err := st.CreateTask(ctx, in); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if in.CreatedAt.IsZero() || in.UpdatedAt.IsZero() {
		t.Fatalf("CreateTask did not set timestamps: created=%v updated=%v", in.CreatedAt, in.UpdatedAt)
	}

	got, err := st.GetTask(ctx, "issue-5")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	if got.ID != in.ID || got.Issue != in.Issue || got.Repo != in.Repo ||
		got.Branch != in.Branch || got.CurrentState != in.CurrentState || got.PaneID != in.PaneID {
		t.Errorf("scalar mismatch:\n got=%+v\nwant=%+v", got, in)
	}
	if got.PRNumber != nil {
		t.Errorf("PRNumber = %v, want nil", *got.PRNumber)
	}
	if !reflect.DeepEqual(got.RetryCounts, in.RetryCounts) {
		t.Errorf("RetryCounts = %v, want %v", got.RetryCounts, in.RetryCounts)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, in.CreatedAt)
	}
	if !got.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, in.UpdatedAt)
	}
}

func TestTask_WorkflowSnapshot_RoundTrips(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	want := "version: 0\nname: x\nstates: {}\n"
	in := &Task{ID: "issue-1", Issue: 1, Repo: "o/r", Branch: "agent/issue-1",
		CurrentState: "queued", WorkflowSnapshot: want}
	if err := st.CreateTask(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkflowSnapshot != want {
		t.Errorf("snapshot = %q, want %q", got.WorkflowSnapshot, want)
	}
	// Snapshot is immutable: a state change must not drop it.
	got.CurrentState = "implementing"
	if err := st.UpdateTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetTask(ctx, "issue-1")
	if again.WorkflowSnapshot != want {
		t.Errorf("snapshot lost after update: %q", again.WorkflowSnapshot)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	st := newStore(t)
	_, err := st.GetTask(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask(missing) err = %v, want ErrNotFound", err)
	}
}

func TestUpdateTask(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// Pin time so UpdatedAt strictly advances on update.
	t0 := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return t0 }

	in := sampleTask("issue-7")
	if err := st.CreateTask(ctx, in); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	createdAt := in.CreatedAt

	st.now = func() time.Time { return t0.Add(time.Minute) }
	in.CurrentState = "pr_open"
	in.PaneID = "%9"
	in.PRNumber = intptr(42)
	in.RetryCounts = map[string]int{"changes_requested": 3}
	if err := st.UpdateTask(ctx, in); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	got, err := st.GetTask(ctx, "issue-7")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.CurrentState != "pr_open" {
		t.Errorf("CurrentState = %q, want pr_open", got.CurrentState)
	}
	if got.PaneID != "%9" {
		t.Errorf("PaneID = %q, want %%9", got.PaneID)
	}
	if got.PRNumber == nil || *got.PRNumber != 42 {
		t.Errorf("PRNumber = %v, want 42", got.PRNumber)
	}
	if !reflect.DeepEqual(got.RetryCounts, map[string]int{"changes_requested": 3}) {
		t.Errorf("RetryCounts = %v", got.RetryCounts)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt changed: %v != %v", got.CreatedAt, createdAt)
	}
	if !got.UpdatedAt.After(createdAt) {
		t.Errorf("UpdatedAt %v did not advance past CreatedAt %v", got.UpdatedAt, createdAt)
	}
}

func TestUpdateTaskNotFound(t *testing.T) {
	st := newStore(t)
	err := st.UpdateTask(context.Background(), sampleTask("ghost"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateTask(missing) err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	ids := []string{"issue-3", "issue-1", "issue-2"}
	for _, id := range ids {
		if err := st.CreateTask(ctx, sampleTask(id)); err != nil {
			t.Fatalf("CreateTask(%s): %v", id, err)
		}
	}

	got, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"issue-1", "issue-2", "issue-3"}
	if len(got) != len(want) {
		t.Fatalf("List len = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("List[%d].ID = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestAppendAuditOrder(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	if err := st.CreateTask(ctx, sampleTask("issue-9")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	entries := []AuditEntry{
		{TaskID: "issue-9", TS: time.Unix(1, 0).UTC(), FromState: "queued", ToState: "implementing", Trigger: "scheduled", Result: ""},
		{TaskID: "issue-9", TS: time.Unix(2, 0).UTC(), FromState: "implementing", ToState: "pr_open", Trigger: "agent.done", Result: "pass"},
		{TaskID: "issue-9", TS: time.Unix(3, 0).UTC(), FromState: "pr_open", ToState: "escalated", Trigger: "agent.done", Result: "fail"},
	}
	// Append for a different task too, to ensure Audit filters by task.
	if err := st.AppendAudit(ctx, AuditEntry{TaskID: "other", TS: time.Unix(5, 0).UTC(), FromState: "a", ToState: "b", Trigger: "x"}); err != nil {
		t.Fatalf("AppendAudit(other): %v", err)
	}
	for _, e := range entries {
		if err := st.AppendAudit(ctx, e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	got, err := st.Audit(ctx, "issue-9")
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("Audit len = %d, want %d", len(got), len(entries))
	}
	for i, want := range entries {
		g := got[i]
		if g.TaskID != want.TaskID || g.FromState != want.FromState || g.ToState != want.ToState ||
			g.Trigger != want.Trigger || g.Result != want.Result {
			t.Errorf("Audit[%d] = %+v, want %+v", i, g, want)
		}
		if !g.TS.Equal(want.TS) {
			t.Errorf("Audit[%d].TS = %v, want %v", i, g.TS, want.TS)
		}
	}
}

func TestRetryCountsNilAndEmpty(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	cases := []struct {
		id string
		rc map[string]int
	}{
		{"nil-rc", nil},
		{"empty-rc", map[string]int{}},
		{"full-rc", map[string]int{"changes_requested": 2, "review": 1}},
	}
	for _, c := range cases {
		in := sampleTask(c.id)
		in.RetryCounts = c.rc
		if err := st.CreateTask(ctx, in); err != nil {
			t.Fatalf("CreateTask(%s): %v", c.id, err)
		}
		got, err := st.GetTask(ctx, c.id)
		if err != nil {
			t.Fatalf("GetTask(%s): %v", c.id, err)
		}
		if !reflect.DeepEqual(got.RetryCounts, c.rc) {
			t.Errorf("%s: RetryCounts = %#v, want %#v", c.id, got.RetryCounts, c.rc)
		}
	}
}
