// Package notify forwards out-of-band escalation/alert signals to an operator.
//
// The engine depends only on the Notifier interface (a small seam at the
// boundary, like exec/github); the default is a no-op, so nothing leaves the
// process unless an operator explicitly wires a real implementation. A notifier
// failure must never fail or block the engine's drive loop.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// defaultClient bounds a nil-Client webhook with a request timeout. The daemon's
// context lives for the whole process and carries no deadline, so without a
// client timeout a hung endpoint would block the calling drive loop forever —
// violating this package's "must never block the drive loop" contract. It is
// package-level so calls share one connection pool. An operator that wires their
// own Client owns its timeout.
var defaultClient = &http.Client{Timeout: 10 * time.Second}

// Event is an out-of-band signal worth surfacing to an operator.
//
// Beyond identifying the task, an escalation carries the diagnosis the daemon
// already holds at the moment it escalates: what caused it, how the task got
// there, what its agent last printed, and what a human should do. Without that
// the recipient has to reconstruct the story from the audit trail and a pane
// read — work the daemon can do once, correctly, for free.
type Event struct {
	TaskID string
	Issue  int
	State  string // the task's current state
	Kind   string // "alert" | "escalated"
	Detail string // e.g. the alert message

	// Cause is the trigger that produced the terminal transition — "timeout",
	// "blocked_timeout", "retry_exhausted", "no_progress", "drive_deadline", or a
	// decision/gate result. Empty when it could not be determined.
	Cause string `json:",omitempty"`
	// Recent is the tail of the audit trail, most recent first, so the escalation
	// reads as a story rather than a single row.
	Recent []Transition `json:",omitempty"`
	// PaneTail is the last of the agent's pane output. It is what distinguishes a
	// genuinely slow agent from one parked on a permission prompt, which is the
	// single most common cause of a silent stall.
	PaneTail string `json:",omitempty"`
	// Recommended is the concrete next action for a human.
	Recommended string `json:",omitempty"`
}

// Transition is one audit row, flattened for transport.
type Transition struct {
	From    string
	To      string
	Trigger string
	Result  string `json:",omitempty"`
}

// Notifier forwards Events. Implementations must be safe to call from the
// engine's drive loop and must honor ctx.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Nop discards events (the default).
type Nop struct{}

func (Nop) Notify(context.Context, Event) error { return nil }

// Webhook POSTs each Event as JSON to URL.
type Webhook struct {
	URL    string
	Client *http.Client // nil => a bounded default client (see defaultClient)
}

// Notify marshals ev to JSON and POSTs it to w.URL with a JSON content type. A
// non-2xx response is reported as a wrapped error.
func (w Webhook) Notify(ctx context.Context, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("notify: marshal event: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := w.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: POST %s: %w", w.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: POST %s: unexpected status %d", w.URL, resp.StatusCode)
	}
	return nil
}
