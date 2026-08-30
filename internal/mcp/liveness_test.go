package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

var testNow = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

// livenessHandler builds a handler with a frozen clock and a fixed deadline
// reporter, so age fields are exact rather than racing the wall clock.
func livenessHandler(tasks map[string]store.Task) *handler {
	h := newTestHandler(fakeReader{tasks: tasks}, &fakeController{})
	h.now = func() time.Time { return testNow }
	h.deadlines = func(state string) (string, string) {
		if state == "implementing" {
			return "45m", "10m"
		}
		return "", ""
	}
	return h
}

// The core of #56: state says where a task is, not whether it is moving. A task
// parked on a permission prompt looks identical to a working one unless the view
// carries agent status and how long it has been that way.
func TestTaskView_CarriesLiveness(t *testing.T) {
	tasks := map[string]store.Task{
		"issue-8": {
			ID: "issue-8", Issue: 8, Repo: "owner/repo", Branch: "agent/issue-8",
			CurrentState:   "implementing",
			AgentStatus:    "blocked",
			AgentStatusAt:  testNow.Add(-40 * time.Minute),
			StateEnteredAt: testNow.Add(-50 * time.Minute),
			CreatedAt:      testNow.Add(-time.Hour),
			UpdatedAt:      testNow.Add(-time.Minute),
		},
	}
	res, _ := call(livenessHandler(tasks), "get_task", 8)
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	var v TaskView
	if err := json.Unmarshal([]byte(res.Content[0].Text), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if v.AgentStatus != "blocked" {
		t.Errorf("AgentStatus = %q, want blocked", v.AgentStatus)
	}
	if v.AgentStatusFor == nil || *v.AgentStatusFor != 2400 {
		t.Errorf("AgentStatusFor = %v, want 2400 (40m blocked)", v.AgentStatusFor)
	}
	if v.StateFor == nil || *v.StateFor != 3000 {
		t.Errorf("StateFor = %v, want 3000 (50m in state)", v.StateFor)
	}
	// Without the bounds an operator cannot tell 40m blocked from fine.
	if v.StateTimeout != "45m" || v.BlockedTimeout != "10m" {
		t.Errorf("deadlines = (%q, %q), want (45m, 10m)", v.StateTimeout, v.BlockedTimeout)
	}
}

// An unknown age must be absent, not 0 — a supervisor reading "0 seconds in
// state" would conclude the task just moved when in fact nothing is known.
func TestTaskView_UnknownAgesAreOmitted(t *testing.T) {
	tasks := map[string]store.Task{
		"issue-9": {
			ID: "issue-9", Issue: 9, CurrentState: "queued",
			CreatedAt: testNow, UpdatedAt: testNow,
		},
	}
	res, _ := call(livenessHandler(tasks), "get_task", 9)
	var raw map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"agent_status", "agent_status_for_seconds", "state_for_seconds"} {
		if _, present := raw[k]; present {
			t.Errorf("%s should be omitted when unknown, got %v", k, raw[k])
		}
	}
}

// A handler with no deadline reporter still serves a valid view.
func TestTaskView_WithoutDeadlinesStillWorks(t *testing.T) {
	tasks := map[string]store.Task{
		"issue-9": {ID: "issue-9", Issue: 9, CurrentState: "implementing", CreatedAt: testNow, UpdatedAt: testNow},
	}
	h := newTestHandler(fakeReader{tasks: tasks}, &fakeController{})
	res, _ := call(h, "get_task", 9)
	if res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	var v TaskView
	if err := json.Unmarshal([]byte(res.Content[0].Text), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.StateTimeout != "" || v.BlockedTimeout != "" {
		t.Errorf("deadlines should be empty without a reporter, got (%q, %q)", v.StateTimeout, v.BlockedTimeout)
	}
}

// A clock that has gone backwards must clamp rather than report a negative age.
func TestSecondsSince_ClampsNegative(t *testing.T) {
	got := secondsSince(testNow, testNow.Add(time.Minute))
	if got == nil || *got != 0 {
		t.Errorf("secondsSince(now, future) = %v, want 0", got)
	}
	if secondsSince(testNow, time.Time{}) != nil {
		t.Error("a zero timestamp must yield nil, not 0")
	}
}
