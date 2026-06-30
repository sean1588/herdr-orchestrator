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
)

// Event is an out-of-band signal worth surfacing to an operator.
type Event struct {
	TaskID string
	Issue  int
	State  string // the task's current state
	Kind   string // "alert" | "escalated"
	Detail string // e.g. the alert message
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
	Client *http.Client // nil => http.DefaultClient
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
		client = http.DefaultClient
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
