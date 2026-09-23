package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var (
	issueSchema   = json.RawMessage(`{"type":"object","properties":{"issue":{"type":"integer","description":"GitHub issue number"}},"required":["issue"]}`)
	noArgsSchema  = json.RawMessage(`{"type":"object","properties":{}}`)
	messageSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"issue":{"type":"integer","description":"GitHub issue number"},` +
		`"text":{"type":"string","description":"One line to submit at the agent's prompt. No newlines: put multi-line content in a file and reference its path."}` +
		`},"required":["issue","text"]}`)
)

func toolDefs() []toolDef {
	return []toolDef{
		{"list_tasks", "List all orchestrator tasks and their current states.", noArgsSchema},
		{"get_task", "Get one task by its GitHub issue number.", issueSchema},
		{"get_audit", "Get a task's audit trail (state transitions) by issue number.", issueSchema},
		{"cancel_task", "Cancel the running drive for an issue; it settles to 'cancelled'.", issueSchema},
		{"enqueue_task", "Re-drive an issue by number (idempotent if already running).", issueSchema},
		{"message_task", "Send one line to a running task's agent, delivered and verified like its kickoff. Use only when the agent's pane shows an idle prompt, never a dialog.", messageSchema},
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

	// Liveness. State answers "where is this task"; these answer "is it moving,
	// and what is it racing" — which state alone cannot, because blocking does
	// not change state. AgentStatus is "" when no pane event has been observed
	// yet (a queued task, or one whose agent has not started).
	AgentStatus string `json:"agent_status,omitempty"`
	// Seconds the agent has held AgentStatus, and seconds since the task entered
	// State. Omitted when unknown (an old row predating these columns) rather
	// than reported as 0, which would read as "just now".
	AgentStatusFor *int `json:"agent_status_for_seconds,omitempty"`
	StateFor       *int `json:"state_for_seconds,omitempty"`
	// The bounds this task is racing in its current state, as configured.
	// Comparing them against StateFor is the whole "is anything wedged" question.
	StateTimeout   string `json:"state_timeout,omitempty"`
	BlockedTimeout string `json:"blocked_timeout,omitempty"`
}

// Deadlines reports the bounds a task sitting in `state` is racing: the state's
// own timeout transition and the global blocked bound. Injected rather than read
// from config here so this package stays free of the workflow types. Either may
// be "" when not configured.
type Deadlines func(state string) (stateTimeout, blockedTimeout string)

type AuditEntryView struct {
	TS        string `json:"ts"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Trigger   string `json:"trigger"`
	Result    string `json:"result,omitempty"`
}

func (h *handler) toTaskView(t store.Task) TaskView {
	rc := t.RetryCounts
	if len(rc) == 0 {
		rc = nil
	}
	v := TaskView{
		ID: t.ID, Issue: t.Issue, Repo: t.Repo, Branch: t.Branch,
		State: t.CurrentState, PRNumber: t.PRNumber, RetryCounts: rc,
		CreatedAt: t.CreatedAt.Format(time.RFC3339), UpdatedAt: t.UpdatedAt.Format(time.RFC3339),
		AgentStatus:    t.AgentStatus,
		AgentStatusFor: secondsSince(h.clock(), t.AgentStatusAt),
		StateFor:       secondsSince(h.clock(), t.StateEnteredAt),
	}
	if h.deadlines != nil {
		v.StateTimeout, v.BlockedTimeout = h.deadlines(t.CurrentState)
	}
	return v
}

// secondsSince returns whole seconds between then and now, or nil when `then` is
// unset — an unknown age must stay absent rather than render as 0, which an
// operator would read as "just now". A clock that has gone backwards clamps to 0.
func secondsSince(now, then time.Time) *int {
	if then.IsZero() {
		return nil
	}
	n := int(now.Sub(then).Seconds())
	if n < 0 {
		n = 0
	}
	return &n
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
		Issue *int   `json:"issue"` // pointer so a missing arg is distinguishable from 0
		Text  string `json:"text"`
	} `json:"arguments"`
}

// callTool dispatches a tools/call. Tool-execution problems (not found, not
// running, a missing required arg) return a successful result with isError:true
// carrying a message; only malformed params are a JSON-RPC protocol error.
func (h *handler) callTool(ctx context.Context, req request) response {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errResp(req.ID, codeInvalidPar, "invalid params: "+err.Error())
	}

	// list_tasks is the only tool without an issue argument.
	if p.Name == "list_tasks" {
		tasks, err := h.reader.List(ctx)
		if err != nil {
			return okResp(req.ID, h.toolErr("list tasks: "+err.Error()))
		}
		views := make([]TaskView, 0, len(tasks))
		for _, t := range tasks {
			views = append(views, h.toTaskView(t))
		}
		return okResp(req.ID, h.toolJSON(views))
	}

	// Every other tool requires a positive issue number; a missing int must not
	// silently become issue 0 and drive/cancel a bogus task.
	if p.Arguments.Issue == nil || *p.Arguments.Issue <= 0 {
		return okResp(req.ID, h.toolErr("missing or invalid required argument: issue (positive integer)"))
	}
	issue := *p.Arguments.Issue

	switch p.Name {
	case "get_task":
		t, err := h.reader.GetTask(ctx, h.taskID(issue))
		if err != nil {
			return okResp(req.ID, h.toolErr(fmt.Sprintf("issue %d not found", issue)))
		}
		return okResp(req.ID, h.toolJSON(h.toTaskView(*t)))
	case "get_audit":
		aud, err := h.reader.Audit(ctx, h.taskID(issue))
		if err != nil {
			return okResp(req.ID, h.toolErr(fmt.Sprintf("issue %d audit: %s", issue, err.Error())))
		}
		views := make([]AuditEntryView, 0, len(aud))
		for _, a := range aud {
			views = append(views, toAuditView(a))
		}
		return okResp(req.ID, h.toolJSON(views))
	case "cancel_task":
		if err := h.ctrl.Cancel(ctx, issue); err != nil {
			return okResp(req.ID, h.toolErr(err.Error()))
		}
		return okResp(req.ID, h.toolText(fmt.Sprintf("cancel dispatched for issue %d", issue)))
	case "enqueue_task":
		if err := h.ctrl.Enqueue(ctx, issue); err != nil {
			return okResp(req.ID, h.toolErr(err.Error()))
		}
		return okResp(req.ID, h.toolText(fmt.Sprintf("enqueued issue %d", issue)))
	case "message_task":
		return okResp(req.ID, h.messageTask(ctx, issue, p.Arguments.Text))
	default:
		return okResp(req.ID, h.toolErr("unknown tool: "+p.Name))
	}
}

// messageTask types one line into a running task's agent prompt.
//
// The engine needs no part in this. A delivered message makes the pane report
// working, then idle or done, and the drive's wait loop decides from the
// authoritative artifact exactly as it does for any other agent activity — a
// message is just more agent activity, not a transition.
func (h *handler) messageTask(ctx context.Context, issue int, text string) map[string]any {
	// The kickoff's rule: a prompt takes one line, anything longer goes in a file.
	if strings.TrimSpace(text) == "" {
		return h.toolErr("missing required argument: text")
	}
	if strings.ContainsAny(text, "\r\n") {
		return h.toolErr("text must be a single line: put multi-line content in a file and reference its path")
	}
	if h.messenger == nil {
		return h.toolErr("message_task is not available: no execution backend wired")
	}
	t, err := h.reader.GetTask(ctx, h.taskID(issue))
	if err != nil {
		return h.toolErr(fmt.Sprintf("issue %d not found", issue))
	}
	if h.settled[t.CurrentState] {
		return h.toolErr(fmt.Sprintf("issue %d is settled (%s): no running agent to message", issue, t.CurrentState))
	}
	if t.PaneID == "" {
		return h.toolErr(fmt.Sprintf("issue %d has no agent pane (never spawned, or its pane is gone)", issue))
	}
	if err := h.messenger.Message(ctx, exec.Handle{PaneID: t.PaneID}, text); err != nil {
		return h.toolErr(fmt.Sprintf("message issue %d: %s", issue, err.Error()))
	}
	// The length, not the text: the audit trail is not a transcript. A failed
	// write is logged, not returned — the agent already has the message, and an
	// error here would invite a retry that delivers it twice.
	entry := store.AuditEntry{
		TaskID: t.ID, FromState: t.CurrentState, ToState: t.CurrentState,
		Trigger: "message_task", Result: fmt.Sprintf("text_len=%d", len(text)),
	}
	if err := h.auditor.AppendAudit(ctx, entry); err != nil {
		h.log.Warn("message_task: could not record audit entry", "task", t.ID, "err", err)
	}
	return h.toolText(fmt.Sprintf("message delivered to issue %d", issue))
}

// An MCP tool result is a list of typed content blocks; isError flags a
// tool-level failure (distinct from a JSON-RPC protocol error).
func (h *handler) toolText(s string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": s}}}
}

func (h *handler) toolErr(s string) map[string]any {
	r := h.toolText(s)
	r["isError"] = true
	return r
}

func (h *handler) toolJSON(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return h.toolErr("marshal: " + err.Error())
	}
	return h.toolText(string(b))
}
