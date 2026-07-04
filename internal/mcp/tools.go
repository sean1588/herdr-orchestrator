package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var (
	issueSchema  = json.RawMessage(`{"type":"object","properties":{"issue":{"type":"integer","description":"GitHub issue number"}},"required":["issue"]}`)
	noArgsSchema = json.RawMessage(`{"type":"object","properties":{}}`)
)

func toolDefs() []toolDef {
	return []toolDef{
		{"list_tasks", "List all orchestrator tasks and their current states.", noArgsSchema},
		{"get_task", "Get one task by its GitHub issue number.", issueSchema},
		{"get_audit", "Get a task's audit trail (state transitions) by issue number.", issueSchema},
		{"cancel_task", "Cancel the running drive for an issue; it settles to 'cancelled'.", issueSchema},
		{"enqueue_task", "Re-drive an issue by number (idempotent if already running).", issueSchema},
	}
}

// TaskView is the stable serialized shape of a task. Volatile/internal fields
// (pane id, pane spawn state, workflow snapshot) are omitted; a nil PR and empty
// retry map are omitted rather than rendered null/{}.
type TaskView struct {
	ID          string         `json:"id"`
	Issue       int            `json:"issue"`
	Repo        string         `json:"repo"`
	Branch      string         `json:"branch"`
	State       string         `json:"state"`
	PRNumber    *int           `json:"pr_number,omitempty"`
	RetryCounts map[string]int `json:"retry_counts,omitempty"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
}

type AuditEntryView struct {
	TS        string `json:"ts"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Trigger   string `json:"trigger"`
	Result    string `json:"result,omitempty"`
}

func toTaskView(t store.Task) TaskView {
	rc := t.RetryCounts
	if len(rc) == 0 {
		rc = nil
	}
	return TaskView{
		ID: t.ID, Issue: t.Issue, Repo: t.Repo, Branch: t.Branch,
		State: t.CurrentState, PRNumber: t.PRNumber, RetryCounts: rc,
		CreatedAt: t.CreatedAt.Format(time.RFC3339), UpdatedAt: t.UpdatedAt.Format(time.RFC3339),
	}
}

func toAuditView(a store.AuditEntry) AuditEntryView {
	return AuditEntryView{
		TS: a.TS.Format(time.RFC3339), FromState: a.FromState,
		ToState: a.ToState, Trigger: a.Trigger, Result: a.Result,
	}
}

type callParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Issue int `json:"issue"`
	} `json:"arguments"`
}

// callTool dispatches a tools/call. Tool-execution problems (not found, not
// running) return a successful result with isError:true carrying a message; only
// malformed params are a JSON-RPC protocol error.
func (h *handler) callTool(ctx context.Context, req request) response {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, codeInvalidPar, "invalid params: "+err.Error())
	}
	switch p.Name {
	case "list_tasks":
		tasks, err := h.reader.List(ctx)
		if err != nil {
			return okResp(req.ID, h.toolErr("list tasks: "+err.Error()))
		}
		views := make([]TaskView, 0, len(tasks))
		for _, t := range tasks {
			views = append(views, toTaskView(t))
		}
		return okResp(req.ID, h.toolJSON(views))
	case "get_task":
		t, err := h.reader.GetTask(ctx, h.taskID(p.Arguments.Issue))
		if err != nil {
			return okResp(req.ID, h.toolErr(fmt.Sprintf("issue %d not found", p.Arguments.Issue)))
		}
		return okResp(req.ID, h.toolJSON(toTaskView(*t)))
	case "get_audit":
		aud, err := h.reader.Audit(ctx, h.taskID(p.Arguments.Issue))
		if err != nil {
			return okResp(req.ID, h.toolErr(fmt.Sprintf("issue %d audit: %s", p.Arguments.Issue, err.Error())))
		}
		views := make([]AuditEntryView, 0, len(aud))
		for _, a := range aud {
			views = append(views, toAuditView(a))
		}
		return okResp(req.ID, h.toolJSON(views))
	case "cancel_task":
		if err := h.ctrl.Cancel(ctx, p.Arguments.Issue); err != nil {
			return okResp(req.ID, h.toolErr(err.Error()))
		}
		return okResp(req.ID, h.toolText(fmt.Sprintf("cancel dispatched for issue %d", p.Arguments.Issue)))
	case "enqueue_task":
		if err := h.ctrl.Enqueue(ctx, p.Arguments.Issue); err != nil {
			return okResp(req.ID, h.toolErr(err.Error()))
		}
		return okResp(req.ID, h.toolText(fmt.Sprintf("enqueued issue %d", p.Arguments.Issue)))
	default:
		return okResp(req.ID, h.toolErr("unknown tool: "+p.Name))
	}
}

// An MCP tool result is a list of typed content blocks; isError flags a
// tool-level failure (distinct from a JSON-RPC protocol error).
func (h *handler) toolText(s string) map[string]interface{} {
	return map[string]interface{}{"content": []map[string]string{{"type": "text", "text": s}}}
}

func (h *handler) toolErr(s string) map[string]interface{} {
	r := h.toolText(s)
	r["isError"] = true
	return r
}

func (h *handler) toolJSON(v interface{}) map[string]interface{} {
	b, err := json.Marshal(v)
	if err != nil {
		return h.toolErr("marshal: " + err.Error())
	}
	return h.toolText(string(b))
}
