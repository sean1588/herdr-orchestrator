// Package eventlog gives the daemon a machine-readable sink for the narrative it
// already produces.
//
// The engine emits every transition, spawn, gate evaluation, decision, agent
// status change and escalation through slog with structured attributes — but the
// daemon's only sink was a text handler writing to a terminal pane, so a
// supervisor had to either poll the task store on a timer or scrape a TUI. This
// package adds a second handler writing JSON Lines to a file, so that same
// stream can be tailed by a program.
//
// It is a slog.Handler rather than a bespoke event bus deliberately: the events
// already exist and are already structured, so a new sink needs no new call
// sites, and no future log line can be forgotten by the event log.
package eventlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// Open creates (or appends to) a JSON Lines event log at path.
//
// Append rather than truncate: a daemon restart is routine (that is how config
// changes are applied), and a restart that erased the run's history would defeat
// the purpose of having a log at all.
//
// The returned Handler records at LevelDebug and above, so the event log keeps
// the full narrative even when the console handler is running at Info.
func Open(path string) (slog.Handler, io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open event log %s: %w", path, err)
	}
	h := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
	return h, f, nil
}

// Tee returns a Handler that forwards every record to all of hs.
//
// Used to keep the operator's console output exactly as it was while adding the
// JSON sink alongside it, rather than making one a replacement for the other.
func Tee(hs ...slog.Handler) slog.Handler {
	switch len(hs) {
	case 0:
		return discard{}
	case 1:
		return hs[0]
	}
	return &tee{hs: hs}
}

type tee struct {
	hs []slog.Handler
}

// Enabled is true if any sink wants the level: the sinks may be configured at
// different levels (console at Info, event log at Debug), and taking the union
// is what lets each apply its own threshold in Handle.
func (t *tee) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t.hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

// Handle fans out, re-checking Enabled per sink so a handler that does not want
// this level does not receive it. Every sink is attempted even if an earlier one
// fails; the first error is returned.
//
// Deliberately holds no lock. Each sink already serializes its own writes, and
// this type has no mutable state to protect — so a lock would buy nothing and
// would turn any re-entrant Handle into a deadlock. That is not hypothetical:
// teeing slog's built-in default handler creates exactly such a cycle, because
// slog.SetDefault points the log package's output back at the new default
// handler. The daemon avoids the cycle at the wiring site by teeing an explicit
// console handler; not locking here means a future mistake fails loudly rather
// than hanging a live daemon with no output at all.
func (t *tee) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range t.hs {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *tee) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		out[i] = h.WithAttrs(as)
	}
	return &tee{hs: out}
}

func (t *tee) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		out[i] = h.WithGroup(name)
	}
	return &tee{hs: out}
}

type discard struct{}

func (discard) Enabled(context.Context, slog.Level) bool  { return false }
func (discard) Handle(context.Context, slog.Record) error { return nil }
func (discard) WithAttrs([]slog.Attr) slog.Handler        { return discard{} }
func (discard) WithGroup(string) slog.Handler             { return discard{} }
