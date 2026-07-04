package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// Reader is the read surface an MCP server needs. *store.Store satisfies it.
type Reader interface {
	List(ctx context.Context) ([]store.Task, error)
	GetTask(ctx context.Context, id string) (*store.Task, error)
	Audit(ctx context.Context, taskID string) ([]store.AuditEntry, error)
}

// Controller is the control surface. *scheduler.Scheduler satisfies it. Methods
// return descriptive errors the tool layer renders verbatim as isError text —
// no sentinel coupling. Enqueue is idempotent; Cancel errors when no drive is
// active for the issue.
type Controller interface {
	Enqueue(ctx context.Context, issue int) error
	Cancel(ctx context.Context, issue int) error
}

// handler holds the wired dependencies and dispatches MCP methods.
type handler struct {
	reader Reader
	ctrl   Controller
	taskID func(int) string // engine.TaskID, injected so mcp needn't import engine
	log    *slog.Logger
}

// handle dispatches one JSON-RPC message. It returns the marshalled response and
// whether the input was a notification (no id => no response is written).
func (h *handler) handle(ctx context.Context, raw []byte) ([]byte, bool) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return mustMarshal(errResp(nil, codeParse, "parse error")), false
	}
	isNote := len(req.ID) == 0

	var resp response
	switch req.Method {
	case "initialize":
		resp = okResp(req.ID, initializeResult())
	case "notifications/initialized", "ping":
		resp = okResp(req.ID, map[string]interface{}{})
	case "tools/list":
		resp = okResp(req.ID, map[string]interface{}{"tools": toolDefs()})
	case "tools/call":
		resp = h.callTool(ctx, req)
	default:
		resp = errResp(req.ID, codeMethodNotFn, "method not found: "+req.Method)
	}

	if isNote {
		return nil, true
	}
	return mustMarshal(resp), false
}

func initializeResult() map[string]interface{} {
	return map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		"serverInfo":      map[string]interface{}{"name": "herdr-orchestrator", "version": "1"},
	}
}
