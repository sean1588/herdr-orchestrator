package eventlog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpen_WritesJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	h, c, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	log := slog.New(h)
	log.Info("transition", "task", "issue-7", "from", "queued", "to", "implementing")
	log.Info("halt", "task", "issue-7", "state", "merged")
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), b)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if first["msg"] != "transition" || first["task"] != "issue-7" || first["to"] != "implementing" {
		t.Errorf("attributes not preserved: %v", first)
	}
}

// A daemon restart is routine — it is how a config change is applied — so the
// event log must not erase the run's history every time.
func TestOpen_AppendsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	for _, msg := range []string{"first-run", "second-run"} {
		h, c, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		slog.New(h).Info(msg)
		c.Close()
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"first-run", "second-run"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("event log lost %q across restart: %s", want, b)
		}
	}
}

func TestTee_FansOutToEverySink(t *testing.T) {
	var text, jsonBuf bytes.Buffer
	h := Tee(
		slog.NewTextHandler(&text, nil),
		slog.NewJSONHandler(&jsonBuf, nil),
	)
	slog.New(h).Info("escalated", "task", "issue-9")

	if !strings.Contains(text.String(), "escalated") {
		t.Errorf("text sink missing the record: %q", text.String())
	}
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(jsonBuf.Bytes()), &got); err != nil {
		t.Fatalf("json sink is not valid JSON: %v (%q)", err, jsonBuf.String())
	}
	if got["task"] != "issue-9" {
		t.Errorf("json sink lost attributes: %v", got)
	}
}

// Each sink applies its own level: the console runs at Info while the event log
// keeps Debug, so a debug record must reach the file and not the terminal.
func TestTee_RespectsPerSinkLevels(t *testing.T) {
	var info, debug bytes.Buffer
	h := Tee(
		slog.NewTextHandler(&info, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewJSONHandler(&debug, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)
	slog.New(h).Debug("gate evaluated", "gate", "head_moved")

	if info.Len() != 0 {
		t.Errorf("info sink should not receive a debug record: %q", info.String())
	}
	if !strings.Contains(debug.String(), "head_moved") {
		t.Errorf("debug sink missing the record: %q", debug.String())
	}
}

func TestTee_WithAttrsReachesEverySink(t *testing.T) {
	var a, b bytes.Buffer
	h := Tee(slog.NewJSONHandler(&a, nil), slog.NewJSONHandler(&b, nil))
	slog.New(h).With("run", "r1").Info("spawn")

	for name, buf := range map[string]*bytes.Buffer{"a": &a, "b": &b} {
		if !strings.Contains(buf.String(), `"run":"r1"`) {
			t.Errorf("sink %s lost the With attr: %q", name, buf.String())
		}
	}
}

func TestTee_ZeroHandlersIsSafe(t *testing.T) {
	slog.New(Tee()).Info("nobody is listening") // must not panic
}

func TestOpen_UnwritablePathErrors(t *testing.T) {
	_, _, err := Open(filepath.Join(t.TempDir(), "no-such-dir", "events.jsonl"))
	if err == nil {
		t.Fatal("expected an error for an unwritable path")
	}
	if !strings.Contains(err.Error(), "event log") {
		t.Errorf("error %q should name what failed", err)
	}
}

// reentrant calls back into the tee the first time it handles a record, which is
// the shape slog's built-in default handler creates once SetDefault points the
// log package back at the new default. A tee that locked would deadlock here —
// and a deadlocked daemon starts up, opens its store, and then logs nothing at
// all, which is far harder to diagnose than a crash.
type reentrant struct {
	inner slog.Handler
	depth int
}

func (h *reentrant) Enabled(context.Context, slog.Level) bool { return true }
func (h *reentrant) Handle(ctx context.Context, r slog.Record) error {
	if h.depth == 0 {
		h.depth++
		return h.inner.Handle(ctx, r.Clone()) // back through the tee
	}
	return nil
}
func (h *reentrant) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *reentrant) WithGroup(string) slog.Handler      { return h }

func TestTee_ReentrantHandleDoesNotDeadlock(t *testing.T) {
	var buf bytes.Buffer
	re := &reentrant{}
	h := Tee(re, slog.NewJSONHandler(&buf, nil))
	re.inner = h

	done := make(chan struct{})
	go func() {
		defer close(done)
		slog.New(h).Info("reentrant")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tee deadlocked on a re-entrant Handle")
	}
	if !strings.Contains(buf.String(), "reentrant") {
		t.Errorf("record never reached the sink: %q", buf.String())
	}
}

func TestTee_ConcurrentHandleIsSafe(t *testing.T) {
	var a, b bytes.Buffer
	h := Tee(
		slog.NewJSONHandler(&lockedWriter{w: &a}, nil),
		slog.NewJSONHandler(&lockedWriter{w: &b}, nil),
	)
	log := slog.New(h)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			log.Info("concurrent", "n", n)
		}(i)
	}
	wg.Wait()

	for name, buf := range map[string]*bytes.Buffer{"a": &a, "b": &b} {
		if got := strings.Count(buf.String(), "concurrent"); got != 50 {
			t.Errorf("sink %s got %d records, want 50", name, got)
		}
	}
}

// lockedWriter models a sink whose own writes are serialized, which is what the
// real handlers do — the tee relies on that rather than adding its own lock.
type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
