package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

func sampleTasks() map[string]store.Task {
	pr := 42
	return map[string]store.Task{
		"issue-7": {
			ID: "issue-7", Issue: 7, Repo: "owner/repo", Branch: "agent/issue-7",
			CurrentState: "pr_open", PRNumber: &pr,
			RetryCounts: map[string]int{"changes_requested": 1},
			CreatedAt:   time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 7, 3, 11, 0, 0, 0, time.UTC),
		},
		"issue-8": {
			ID: "issue-8", Issue: 8, Repo: "owner/repo", Branch: "agent/issue-8",
			CurrentState: "implementing", // no PR, no retries
			CreatedAt:    time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			UpdatedAt:    time.Date(2026, 7, 3, 9, 30, 0, 0, time.UTC),
		},
	}
}

func TestListTasks(t *testing.T) {
	h := newTestHandler(fakeReader{tasks: sampleTasks()}, &fakeController{})
	res, _ := call(h, "list_tasks", 0)
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	var views []TaskView
	if err := json.Unmarshal([]byte(res.Content[0].Text), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	// issue-7 (with PR + retries) and issue-8 (nil PR, empty retries).
	if views[0].PRNumber == nil || *views[0].PRNumber != 42 {
		t.Errorf("issue-7 pr_number = %v, want 42", views[0].PRNumber)
	}
	if views[0].CreatedAt != "2026-07-03T10:00:00Z" {
		t.Errorf("created_at = %q, want RFC3339", views[0].CreatedAt)
	}
	// nil PR / empty retries must be omitted from the JSON, not rendered null/{}.
	if strings.Contains(res.Content[0].Text, `"pr_number":null`) {
		t.Errorf("nil PRNumber should be omitted: %s", res.Content[0].Text)
	}
	if strings.Contains(res.Content[0].Text, `"retry_counts":{}`) {
		t.Errorf("empty RetryCounts should be omitted: %s", res.Content[0].Text)
	}
}

func TestGetTask(t *testing.T) {
	h := newTestHandler(fakeReader{tasks: sampleTasks()}, &fakeController{})

	res, _ := call(h, "get_task", 7)
	if res.IsError {
		t.Fatalf("get_task 7 should succeed: %+v", res)
	}
	var v TaskView
	if err := json.Unmarshal([]byte(res.Content[0].Text), &v); err != nil {
		t.Fatal(err)
	}
	if v.Issue != 7 || v.State != "pr_open" {
		t.Errorf("got %+v", v)
	}

	miss, _ := call(h, "get_task", 999)
	if !miss.IsError || !strings.Contains(miss.Content[0].Text, "not found") {
		t.Errorf("get_task 999 should be a not-found tool error: %+v", miss)
	}
}

func TestGetAudit(t *testing.T) {
	audit := map[string][]store.AuditEntry{
		"issue-7": {
			{TaskID: "issue-7", TS: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC), FromState: "queued", ToState: "implementing", Trigger: "scheduled"},
			{TaskID: "issue-7", TS: time.Date(2026, 7, 3, 10, 5, 0, 0, time.UTC), FromState: "implementing", ToState: "pr_open", Trigger: "agent.done", Result: "pass"},
		},
	}
	h := newTestHandler(fakeReader{tasks: sampleTasks(), audit: audit}, &fakeController{})
	res, _ := call(h, "get_audit", 7)
	if res.IsError {
		t.Fatalf("get_audit 7 should succeed: %+v", res)
	}
	var views []AuditEntryView
	if err := json.Unmarshal([]byte(res.Content[0].Text), &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].ToState != "implementing" || views[1].Result != "pass" {
		t.Fatalf("audit views wrong: %+v", views)
	}
}

func TestCancelTool(t *testing.T) {
	fc := &fakeController{}
	h := newTestHandler(fakeReader{tasks: sampleTasks()}, fc)
	res, _ := call(h, "cancel_task", 7)
	if res.IsError || !strings.Contains(res.Content[0].Text, "cancel dispatched") {
		t.Fatalf("cancel should succeed: %+v", res)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "cancel:7" {
		t.Fatalf("controller calls = %v, want [cancel:7]", fc.calls)
	}

	fc2 := &fakeController{cancelErr: errors.New("issue 9 is not currently running")}
	h2 := newTestHandler(fakeReader{}, fc2)
	miss, _ := call(h2, "cancel_task", 9)
	if !miss.IsError || !strings.Contains(miss.Content[0].Text, "not currently running") {
		t.Fatalf("cancel of non-running issue should be a tool error carrying the message: %+v", miss)
	}
}

func TestEnqueueTool(t *testing.T) {
	fc := &fakeController{}
	h := newTestHandler(fakeReader{}, fc)
	res, _ := call(h, "enqueue_task", 5)
	if res.IsError || !strings.Contains(res.Content[0].Text, "enqueued issue 5") {
		t.Fatalf("enqueue should succeed: %+v", res)
	}
	if len(fc.calls) != 1 || fc.calls[0] != "enqueue:5" {
		t.Fatalf("controller calls = %v, want [enqueue:5]", fc.calls)
	}
}
