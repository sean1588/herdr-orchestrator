package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// --event-log is installed on the process default before wire builds the engine,
// so a record the wired engine logs must land in the file. It used to reach only
// the console: the engine fell back to a private stderr handler.
func TestEventLog_CapturesWiredEngineRecords(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	closer, err := installEventLog(path)
	if err != nil {
		t.Fatalf("installEventLog: %v", err)
	}
	defer closer.Close()

	cf := commonFlags{
		config:  "../../examples/default-pipeline.yaml",
		repo:    dir,
		db:      filepath.Join(dir, "orchestrator.db"),
		taskDir: filepath.Join(dir, "tasks"),
	}
	ctx := context.Background()
	w, err := cf.wire(ctx)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	defer w.store.Close()

	// Recover over a task whose snapshot is unparseable logs a warning through
	// the engine's logger and skips the task, touching neither herdr nor gh.
	if err := w.store.CreateTask(ctx, &store.Task{
		ID: "issue-9", Issue: 9, Branch: "agent/issue-9",
		CurrentState: "implementing", WorkflowSnapshot: "not a workflow",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := w.eng.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var rec struct{ Msg, Task string }
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("event log line is not JSON: %q: %v", sc.Text(), err)
		}
		if strings.HasPrefix(rec.Msg, "recover: task snapshot invalid") && rec.Task == "issue-9" {
			return
		}
	}
	t.Errorf("engine record missing from event log:\n%s", b)
}
