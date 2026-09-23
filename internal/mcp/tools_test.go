package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestMissingIssueArg(t *testing.T) {
	fc := &fakeController{}
	h := newTestHandler(fakeReader{tasks: sampleTasks()}, fc)
	// arguments with no "issue" must NOT silently act on issue 0.
	raw, _ := h.handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cancel_task","arguments":{}}}`))
	var out struct {
		Result callResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Result.IsError || !strings.Contains(out.Result.Content[0].Text, "issue") {
		t.Fatalf("missing issue should be a tool error, got: %+v", out.Result)
	}
	if len(fc.calls) != 0 {
		t.Fatalf("controller must not be called for a missing issue, calls=%v", fc.calls)
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

// messageTasks is one task per message_task case: a running agent with a pane,
// a settled task that still records its last pane, and a running task that has
// not spawned yet.
func messageTasks() map[string]store.Task {
	return map[string]store.Task{
		"issue-8":  {ID: "issue-8", Issue: 8, CurrentState: "implementing", PaneID: "w3:p1"},
		"issue-9":  {ID: "issue-9", Issue: 9, CurrentState: "merged", PaneID: "w4:p1"},
		"issue-10": {ID: "issue-10", Issue: 10, CurrentState: "implementing"},
	}
}

func messageHandler(m Messenger, a Auditor) *handler {
	h := newTestHandler(fakeReader{tasks: messageTasks()}, &fakeController{})
	h.messenger, h.auditor = m, a
	h.settled = map[string]bool{"merged": true, "escalated": true}
	return h
}

func TestMessageTool_DeliversToTheTaskPane(t *testing.T) {
	fm, fa := &fakeMessenger{}, &fakeAuditor{}
	const text = "auth is refreshed; push the branch"
	res := callMessage(messageHandler(fm, fa), 8, text)
	if res.IsError || !strings.Contains(res.Content[0].Text, "message delivered to issue 8") {
		t.Fatalf("message_task should succeed: %+v", res)
	}
	if len(fm.calls) != 1 || fm.calls[0].h.PaneID != "w3:p1" || fm.calls[0].text != text {
		t.Fatalf("backend calls = %+v, want one Message on w3:p1 with the text", fm.calls)
	}
	want := store.AuditEntry{
		TaskID: "issue-8", FromState: "implementing", ToState: "implementing",
		Trigger: "message_task", Result: fmt.Sprintf("text_len=%d", len(text)),
	}
	if len(fa.entries) != 1 || fa.entries[0] != want {
		t.Fatalf("audit = %+v, want [%+v]", fa.entries, want)
	}
}

func TestMessageTool_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		issue   int
		text    string
		unwired bool
		sendErr error
		want    string
	}{
		{name: "settled task", issue: 9, text: "hi", want: "settled"},
		{name: "unknown issue", issue: 999, text: "hi", want: "not found"},
		{name: "no pane", issue: 10, text: "hi", want: "no agent pane"},
		{name: "newline", issue: 8, text: "line one\nline two", want: "single line"},
		{name: "carriage return", issue: 8, text: "line one\rline two", want: "single line"},
		{name: "empty text", issue: 8, text: "  ", want: "text"},
		{name: "no backend wired", issue: 8, text: "hi", unwired: true, want: "not available"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fm, fa := &fakeMessenger{}, &fakeAuditor{}
			h := messageHandler(fm, fa)
			if tc.unwired {
				h.messenger = nil
			}
			res := callMessage(h, tc.issue, tc.text)
			if !res.IsError || !strings.Contains(res.Content[0].Text, tc.want) {
				t.Fatalf("want a tool error containing %q, got %+v", tc.want, res)
			}
			if len(fm.calls) != 0 {
				t.Errorf("backend must not be touched, got %+v", fm.calls)
			}
			if len(fa.entries) != 0 {
				t.Errorf("a refused message must not be audited, got %+v", fa.entries)
			}
		})
	}
}

func TestMessageTool_DeliveryFailureIsAToolErrorAndNotAudited(t *testing.T) {
	fm := &fakeMessenger{err: errors.New("kickoff not accepted on w3:p1")}
	fa := &fakeAuditor{}
	res := callMessage(messageHandler(fm, fa), 8, "hi")
	if !res.IsError || !strings.Contains(res.Content[0].Text, "kickoff not accepted") {
		t.Fatalf("a failed delivery should be a tool error carrying the cause: %+v", res)
	}
	if len(fa.entries) != 0 {
		t.Errorf("an undelivered message must not be audited, got %+v", fa.entries)
	}
}
