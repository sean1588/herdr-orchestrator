package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

type fakeReader struct {
	tasks map[string]store.Task
	audit map[string][]store.AuditEntry
}

func (f fakeReader) List(context.Context) ([]store.Task, error) {
	out := make([]store.Task, 0, len(f.tasks))
	for _, t := range f.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f fakeReader) GetTask(_ context.Context, id string) (*store.Task, error) {
	if t, ok := f.tasks[id]; ok {
		return &t, nil
	}
	return nil, store.ErrNotFound
}

func (f fakeReader) Audit(_ context.Context, id string) ([]store.AuditEntry, error) {
	return f.audit[id], nil
}

type fakeController struct {
	cancelErr  error
	enqueueErr error
	calls      []string
}

func (f *fakeController) Enqueue(_ context.Context, issue int) error {
	f.calls = append(f.calls, fmt.Sprintf("enqueue:%d", issue))
	return f.enqueueErr
}

func (f *fakeController) Cancel(_ context.Context, issue int) error {
	f.calls = append(f.calls, fmt.Sprintf("cancel:%d", issue))
	return f.cancelErr
}

func newTestHandler(r Reader, c Controller) *handler {
	return &handler{
		reader: r,
		ctrl:   c,
		taskID: func(i int) string { return fmt.Sprintf("issue-%d", i) },
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// callResult is the MCP tools/call result shape (content blocks + isError).
type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// call invokes tools/call for a tool and returns the parsed result.
func call(h *handler, name string, issue int) (callResult, bool) {
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"issue":%d}}}`, name, issue)
	raw, isNote := h.handle(context.Background(), []byte(req))
	var out struct {
		Result callResult `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result, isNote
}
