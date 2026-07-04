# MCP Server Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose orchestrator state (list/get/audit) and per-task control
(cancel/enqueue) over a loopback MCP server mounted in `orchestratord daemon`.

**Architecture:** Read tools call the store's existing single-writer-safe reads.
Control tools route through a new scheduler command seam: `cancel` cancels a
per-issue context and the owning drive settles to a daemon-owned `cancelled`
terminal on a *detached* context (single-writer invariant preserved); `enqueue`
re-drives via the poller. Transport is hand-rolled JSON-RPC 2.0 over stdlib
`net/http` (request/response subset of MCP Streamable HTTP). Zero new deps.

**Tech Stack:** Go 1.26, stdlib only (`net/http`, `encoding/json`, `context`),
`modernc.org/sqlite` (existing). Reference spec:
`docs/superpowers/specs/2026-07-03-mcp-server-design.md`.

## Global Constraints

- **Zero new dependencies.** stdlib only. Pure-Go / cgo-free / single binary intact.
- **`context.Context` is the first arg** of anything doing I/O or that can block.
- **Wrap errors** with `fmt.Errorf("...: %w", err)`. **No panics in the daemon path.**
- **The engine's `advance` is the single mutation point** for task state; the
  scheduler never writes task state, it only cancels contexts.
- **Small interfaces at package boundaries** — `internal/mcp` depends on its own
  `Reader`/`Controller` interfaces, never on `*store.Store`/`*scheduler.Scheduler`
  concretely.
- **Tests table-driven.** Keep `go build ./...`, `go test ./...`,
  `go test -race ./...`, `go vet ./...`, `gofmt -l .` green at every task.
- **`engine.CancelState` (`"cancelled"`) is reserved** — referenced from the
  single exported const, never duplicated as a magic string.

## File Structure

- `internal/engine/engine.go` (modify) — `ErrOperatorCancel`, `CancelState`,
  `isHalt` extension, `settleCancelled`, drive error-branch hook.
- `internal/engine/cancel_test.go` (create) — the cancel-settle correctness test.
- `internal/scheduler/scheduler.go` (modify) — `command`, `EnableControl`,
  `commands` field, `cancelCause`, worker per-issue cancel ctx, command handling,
  `Enqueue`/`Cancel` methods.
- `internal/scheduler/cancelreg.go` (create) — `cancelRegistry` (Serve-local).
- `internal/scheduler/control_test.go` (create) — command-seam tests.
- `internal/mcp/jsonrpc.go` (create) — JSON-RPC 2.0 envelope + codes.
- `internal/mcp/protocol.go` (create) — MCP dispatch + `Reader`/`Controller`.
- `internal/mcp/tools.go` (create) — 5 tool handlers + DTOs.
- `internal/mcp/server.go` (create) — `net/http` server + lifecycle + `New`.
- `internal/mcp/*_test.go` (create) — table-driven tests per file.
- `cmd/orchestratord/main.go` (modify) — `--mcp-listen`, mount, cancel-terminal.
- `README.md`, `ROADMAP.md` (modify) — document the surface; move item to Built.

---

### Task 1: Engine cancel-settle

**Files:**
- Modify: `internal/engine/engine.go` (add sentinel/const near line 45; extend
  `isHalt` at :780; hook `drive` error branch at :248; add `settleCancelled`).
- Test: `internal/engine/cancel_test.go` (create).

**Interfaces:**
- Produces: `engine.ErrOperatorCancel error`, `engine.CancelState = "cancelled"`
  (exported const), the settle behavior (drive cancelled with `ErrOperatorCancel`
  cause → task ends at `CancelState`).
- Consumes: existing `advance(ctx, task, next, trigger, result) error`,
  `maybeCleanup`, `isHalt`, `store.Task`.

- [ ] **Step 1: Write the failing test** — `internal/engine/cancel_test.go`.
  Drive a task parked in an agent-wait state, cancel its context with
  `ErrOperatorCancel`, assert it settles to `CancelState` and wrote the audit row;
  a second sub-test cancels with a plain cause and asserts the state is NOT
  `CancelState` (aborted, left for recovery). Reuse the package's existing test
  harness/fakes (see `engine` test files for `fakeBackend`/`fakeGH`/store setup).

```go
package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

func TestDriveOperatorCancelSettles(t *testing.T) {
	tests := []struct {
		name      string
		cause     error
		wantState string // CancelState, or "" meaning "unchanged from start"
	}{
		{"operator cancel settles", ErrOperatorCancel, CancelState},
		{"plain cancel aborts", context.Canceled, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, st := newTestEngineParkedAgent(t) // helper: engine whose implementing
			// state blocks in awaitAgentState on a never-firing event stream.
			task := seedTask(t, st, "implementing")

			ctx, cancel := context.WithCancelCause(context.Background())
			done := make(chan string, 1)
			go func() {
				final, _ := e.drive(ctx, task)
				done <- final
			}()
			time.Sleep(20 * time.Millisecond) // let drive reach the wait
			cancel(tc.cause)

			select {
			case final := <-done:
				if tc.wantState == CancelState && final != CancelState {
					t.Fatalf("final = %q, want %q", final, CancelState)
				}
				if tc.wantState == "" && final == CancelState {
					t.Fatalf("plain cancel should not settle to %q", CancelState)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("drive did not return after cancel")
			}

			if tc.wantState == CancelState {
				got, err := st.GetTask(context.Background(), task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.CurrentState != CancelState {
					t.Fatalf("persisted state = %q, want %q", got.CurrentState, CancelState)
				}
				aud, _ := st.Audit(context.Background(), task.ID)
				if len(aud) == 0 || aud[len(aud)-1].ToState != CancelState ||
					aud[len(aud)-1].Trigger != "operator.cancel" {
					t.Fatalf("missing operator.cancel audit row: %+v", aud)
				}
			}
			_ = errors.Is // keep import if unused after edits
		})
	}
}
```

  (Write the `newTestEngineParkedAgent`/`seedTask` helpers against the existing
  fakes; a "parked agent" is an engine whose `backend.Events` returns a channel
  that never sends and never closes, so the drive blocks in `awaitAgentState`
  until ctx is cancelled.)

- [ ] **Step 2: Run to verify it fails**
  Run: `go test ./internal/engine/ -run TestDriveOperatorCancelSettles -v`
  Expected: FAIL — `ErrOperatorCancel`/`CancelState`/`settleCancelled` undefined.

- [ ] **Step 3: Implement** in `internal/engine/engine.go`.

  Add near `errSuspended` (~line 45):
```go
// ErrOperatorCancel is the cancellation cause an operator cancel carries (set by
// the scheduler via context.CancelCauseFunc). A drive that observes this cause
// settles the task to CancelState instead of aborting, so the cancel sticks. Any
// other cancellation cause (daemon shutdown) aborts and leaves state for recovery.
var ErrOperatorCancel = errors.New("operator cancel")

// CancelState is the reserved terminal a task lands in when cancelled by an
// operator. It is never a workflow state (never in the YAML/schema); the store
// accepts it as an opaque current_state and isHalt / the daemon's settledStates
// recognize it as terminal.
const CancelState = "cancelled"
```

  Extend `isHalt` (~line 780):
```go
func (e *Engine) isHalt(state string) bool {
	return state == e.goal || state == CancelState || e.isTerminal(state)
}
```

  Hook `drive`'s error branch (the block at ~line 248, `if err != nil { return ... }`):
```go
		if err != nil {
			if errors.Is(context.Cause(ctx), ErrOperatorCancel) {
				return e.settleCancelled(ctx, task)
			}
			return task.CurrentState, err
		}
```

  Add `settleCancelled` (near `advance`):
```go
// settleCancelled records an operator cancel as a terminal transition to
// CancelState. The drive's context is already cancelled, and the store's writes
// are ctx-aware, so it runs on a context detached from that cancellation (with a
// bound) — otherwise the settle write would fail with context.Canceled and the
// cancel would not stick. Best-effort worktree cleanup follows, like any no-PR
// terminal.
func (e *Engine) settleCancelled(ctx context.Context, task *store.Task) (string, error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := e.advance(sctx, task, CancelState, "operator.cancel", ""); err != nil {
		return task.CurrentState, fmt.Errorf("settle cancelled: %w", err)
	}
	e.maybeCleanup(sctx, task)
	e.log.Info("task cancelled by operator", "task", task.ID)
	return CancelState, nil
}
```

- [ ] **Step 4: Run to verify it passes**
  Run: `go test ./internal/engine/ -run TestDriveOperatorCancelSettles -race -v`
  Expected: PASS (both sub-tests).

- [ ] **Step 5: Full green + commit**
  Run: `go build ./... && go vet ./... && gofmt -l internal/engine && go test ./internal/engine/ -race`
  Expected: builds, no vet/gofmt output, tests pass.
```bash
git add internal/engine/
git commit -m "Engine: settle an operator-cancelled drive to a terminal cancelled state"
```

---

### Task 2: Scheduler command seam

**Files:**
- Create: `internal/scheduler/cancelreg.go` — the per-issue cancel registry.
- Modify: `internal/scheduler/scheduler.go` — `command`, `commands` field,
  `cancelCause`, `EnableControl`, worker per-issue ctx + register, Serve command
  case, `Enqueue`/`Cancel` methods, `submit`.
- Test: `internal/scheduler/control_test.go` (create).

**Interfaces:**
- Consumes: nothing from Task 1 directly (the cause is injected as `error`, wired
  in Task 6 to `engine.ErrOperatorCancel`).
- Produces: `(*Scheduler).EnableControl(cancelCause error)`,
  `(*Scheduler).Enqueue(ctx, issue) error`, `(*Scheduler).Cancel(ctx, issue) error`
  — satisfying the `mcp.Controller` interface built in Task 3.

- [ ] **Step 1: Write the failing test** — `internal/scheduler/control_test.go`.
  Table/cases: (a) `Cancel` of a running issue cancels its per-issue context with
  the injected cause; (b) `Cancel` of a non-running issue returns a
  "not currently running" error; (c) `Enqueue` of a new issue drives it once;
  (d) `Enqueue`/`Cancel` before `EnableControl` return a "control not enabled" error.

```go
package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var errTestCause = errors.New("test operator cancel")

func TestControlCancel(t *testing.T) {
	started := make(chan int, 1)
	gotCause := make(chan error, 1)
	s := &Scheduler{
		Workers:  1,
		Interval: time.Hour, // don't poll during the test
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil },
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		RunTask: func(ctx context.Context, issue int) error {
			started <- issue
			<-ctx.Done()
			gotCause <- context.Cause(ctx)
			return ctx.Err()
		},
	}
	s.EnableControl(errTestCause)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx)

	// Enqueue via a command, then cancel it.
	if err := s.Enqueue(ctx, 7); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTask never started")
	}
	if err := s.Cancel(ctx, 7); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case c := <-gotCause:
		if !errors.Is(c, errTestCause) {
			t.Fatalf("cause = %v, want %v", c, errTestCause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunTask not cancelled")
	}
}

func TestControlCancelNotRunning(t *testing.T) {
	s := &Scheduler{Workers: 1, Interval: time.Hour,
		RunTask:  func(ctx context.Context, i int) error { return nil },
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil }}
	s.EnableControl(errTestCause)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx)
	if err := s.Cancel(ctx, 999); err == nil {
		t.Fatal("cancel of non-running issue should error")
	}
}

func TestControlDisabled(t *testing.T) {
	s := &Scheduler{}
	if err := s.Cancel(context.Background(), 1); err == nil {
		t.Fatal("Cancel without EnableControl should error")
	}
	if err := s.Enqueue(context.Background(), 1); err == nil {
		t.Fatal("Enqueue without EnableControl should error")
	}
}

var _ = sync.Mutex{}
```

- [ ] **Step 2: Run to verify it fails**
  Run: `go test ./internal/scheduler/ -run TestControl -v`
  Expected: FAIL — `EnableControl`/`Enqueue`/`Cancel` undefined.

- [ ] **Step 3a: Implement the registry** — `internal/scheduler/cancelreg.go`.

```go
package scheduler

import (
	"context"
	"sync"
)

// cancelRegistry maps an in-flight issue to its drive's cancel func so an
// operator cancel can interrupt exactly one drive. Per-issue registration is
// serialized by the inflightSet (one worker per issue at a time), so a plain
// map keyed by issue is race-free with a mutex; no generation token is needed.
type cancelRegistry struct {
	mu sync.Mutex
	m  map[int]context.CancelCauseFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{m: map[int]context.CancelCauseFunc{}}
}

func (r *cancelRegistry) register(issue int, c context.CancelCauseFunc) {
	r.mu.Lock()
	r.m[issue] = c
	r.mu.Unlock()
}

func (r *cancelRegistry) deregister(issue int) {
	r.mu.Lock()
	delete(r.m, issue)
	r.mu.Unlock()
}

// cancel invokes the issue's cancel func with cause, returning false if no drive
// is registered for the issue. The func is captured under the lock and called
// after releasing it (context cancel never re-enters the registry, but keep the
// critical section tight).
func (r *cancelRegistry) cancel(issue int, cause error) bool {
	r.mu.Lock()
	c, ok := r.m[issue]
	r.mu.Unlock()
	if ok {
		c(cause)
	}
	return ok
}
```

- [ ] **Step 3b: Implement the command seam** — `internal/scheduler/scheduler.go`.

  Add to imports: `"errors"`, `"fmt"`. Add fields to `Scheduler`:
```go
	// Control seam (nil unless EnableControl): the MCP server submits commands
	// here and they are processed in the poller goroutine.
	commands    chan command
	cancelCause error
```

  Add types + methods:
```go
type cmdKind int

const (
	cmdEnqueue cmdKind = iota
	cmdCancel
)

type command struct {
	kind  cmdKind
	issue int
	reply chan error // buffered(1); poller never blocks replying
}

// EnableControl turns on the external control surface: Enqueue/Cancel become
// live and the Serve loop processes their commands. cancelCause is the cause an
// operator cancel carries (the daemon wires engine.ErrOperatorCancel).
func (s *Scheduler) EnableControl(cancelCause error) {
	s.commands = make(chan command)
	s.cancelCause = cancelCause
}

// Enqueue re-drives an issue by number (idempotent: a no-op if already in
// flight). Satisfies mcp.Controller.
func (s *Scheduler) Enqueue(ctx context.Context, issue int) error {
	return s.submit(ctx, command{kind: cmdEnqueue, issue: issue})
}

// Cancel cancels the drive of a running issue, erroring if none is running.
// Satisfies mcp.Controller.
func (s *Scheduler) Cancel(ctx context.Context, issue int) error {
	return s.submit(ctx, command{kind: cmdCancel, issue: issue})
}

func (s *Scheduler) submit(ctx context.Context, c command) error {
	if s.commands == nil {
		return errors.New("scheduler control not enabled")
	}
	c.reply = make(chan error, 1)
	select {
	case s.commands <- c:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

  In `Serve`, create the registry before the worker pool:
```go
	cancels := newCancelRegistry()
```
  Change the worker loop to derive+register a per-issue cancel context:
```go
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for issue := range work {
				runCtx, cancel := context.WithCancelCause(ctx)
				cancels.register(issue, cancel)
				if err := s.RunTask(runCtx, issue); err != nil {
					s.Log.Warn("run task failed", "issue", issue, "err", err)
				}
				cancel(nil)             // release the context
				cancels.deregister(issue)
				inflight.remove(issue)
			}
		}()
	}
```
  Add a command case to the `select` in the poll loop (alongside `ctx.Done()`
  and `ticker.C`):
```go
		case c := <-s.commands: // nil channel when control disabled: never ready
			s.handleCommand(ctx, c, work, inflight, cancels)
```
  Add the handler:
```go
// handleCommand processes an external control command in the poller goroutine,
// so an enqueue keeps Serve the single sender on the work channel and a cancel
// reads the registry without a second owner. The reply channel is buffered, so
// this never blocks on a caller that has already given up.
func (s *Scheduler) handleCommand(ctx context.Context, c command, work chan int, inflight *inflightSet, cancels *cancelRegistry) {
	switch c.kind {
	case cmdEnqueue:
		s.enqueue(ctx, work, inflight, []int{c.issue})
		c.reply <- nil
	case cmdCancel:
		if cancels.cancel(c.issue, s.cancelCause) {
			c.reply <- nil
		} else {
			c.reply <- fmt.Errorf("issue %d is not currently running", c.issue)
		}
	default:
		c.reply <- fmt.Errorf("unknown command kind %d", c.kind)
	}
}
```

- [ ] **Step 4: Run to verify it passes**
  Run: `go test ./internal/scheduler/ -run TestControl -race -v`
  Expected: PASS (all cases).

- [ ] **Step 5: Full green + commit**
  Run: `go build ./... && go vet ./... && gofmt -l internal/scheduler && go test ./internal/scheduler/ -race`
```bash
git add internal/scheduler/
git commit -m "Scheduler: add a command seam for external enqueue/cancel control"
```

---

### Task 3: mcp JSON-RPC core + protocol dispatch + interfaces

**Files:**
- Create: `internal/mcp/jsonrpc.go`, `internal/mcp/protocol.go`.
- Test: `internal/mcp/jsonrpc_test.go`, `internal/mcp/protocol_test.go`.

**Interfaces:**
- Produces: `mcp.Reader`, `mcp.Controller` (consumed by Task 4/6);
  `handle(ctx, raw []byte) (response, isNotification bool)` dispatch used by Task 5.
- Consumes: `store.Task`, `store.AuditEntry`; `Enqueue`/`Cancel` from Task 2.

- [ ] **Step 1: Write failing tests.**
  `jsonrpc_test.go`: decode a valid request; a request with no `id` is a
  notification (dispatch returns `isNotification=true`, no response written);
  encode an error response with code `-32601`.
  `protocol_test.go`: `initialize` returns a result with `serverInfo` +
  `capabilities.tools`; `tools/list` returns 5 tools each with a non-empty
  `name` and an `inputSchema`; an unknown method returns error `-32601`.

```go
// protocol_test.go (essentials)
func TestToolsList(t *testing.T) {
	h := newTestHandler(t) // wraps fakeReader+fakeController
	resp, isNote := h.handle(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if isNote {
		t.Fatal("tools/list is a request, not a notification")
	}
	var out struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Result.Tools) != 5 {
		t.Fatalf("got %d tools, want 5", len(out.Result.Tools))
	}
	for _, tl := range out.Result.Tools {
		if tl.Name == "" || len(tl.InputSchema) == 0 {
			t.Fatalf("tool missing name/inputSchema: %+v", tl)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**
  Run: `go test ./internal/mcp/ -v`
  Expected: FAIL — package/types undefined.

- [ ] **Step 3: Implement `jsonrpc.go`.**
```go
// Package mcp serves orchestrator state and per-task control over a loopback
// MCP endpoint: hand-rolled JSON-RPC 2.0 (the request/response subset of MCP's
// Streamable HTTP transport) on stdlib net/http. Reads hit the store's
// single-writer-safe reads; control routes through the injected Controller.
package mcp

import "encoding/json"

const (
	codeParse       = -32700
	codeInvalidReq  = -32600
	codeMethodNotFn = -32601
	codeInvalidPar  = -32602
	codeInternal    = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func okResp(id json.RawMessage, result interface{}) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}
func errResp(id json.RawMessage, code int, msg string) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}
```

- [ ] **Step 4: Implement `protocol.go`** — the interfaces + dispatch.
```go
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// Reader is the read surface. *store.Store satisfies it.
type Reader interface {
	List(ctx context.Context) ([]store.Task, error)
	GetTask(ctx context.Context, id string) (*store.Task, error)
	Audit(ctx context.Context, taskID string) ([]store.AuditEntry, error)
}

// Controller is the control surface. *scheduler.Scheduler satisfies it.
type Controller interface {
	Enqueue(ctx context.Context, issue int) error
	Cancel(ctx context.Context, issue int) error
}

// handler holds the wired dependencies and dispatches MCP methods.
type handler struct {
	reader Reader
	ctrl   Controller
	taskID func(int) string // engine.TaskID, injected (no engine import)
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

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(errResp(nil, codeInternal, "marshal error"))
	}
	return b
}
```

- [ ] **Step 5: Run to verify it passes + commit**
  Run: `go test ./internal/mcp/ -race -v` (Task 3 tests; `callTool`/`toolDefs`
  stubs may be needed to compile — introduce them minimally in Task 4; for this
  task, stub `toolDefs()` returning the 5 names and `callTool` returning a
  not-implemented error so the package compiles and Task 3 tests pass).
```bash
git add internal/mcp/jsonrpc.go internal/mcp/protocol.go internal/mcp/*_test.go
git commit -m "mcp: JSON-RPC 2.0 core + MCP method dispatch + Reader/Controller"
```

---

### Task 4: mcp tool handlers + DTOs

**Files:**
- Create: `internal/mcp/tools.go` (replaces the Task 3 stubs).
- Test: `internal/mcp/tools_test.go`.

**Interfaces:**
- Consumes: `Reader`, `Controller`, `handler` from Task 3.
- Produces: `toolDefs() []toolDef`, `(*handler).callTool`, `TaskView`,
  `AuditEntryView`.

- [ ] **Step 1: Write failing tests** — `tools_test.go`, with `fakeReader` +
  `fakeController`. Assert: `list_tasks` returns a `TaskView` array with nil
  `PRNumber` / empty `RetryCounts` omitted and RFC3339 times; `get_task` by issue
  found and not-found (`isError:true`); `get_audit` chronological; `cancel_task`
  success + not-running (`isError:true` carrying the controller message);
  `enqueue_task` success. Verify the exact JSON via `tools/call` through `handle`.

```go
type fakeReader struct {
	tasks map[string]store.Task
	audit map[string][]store.AuditEntry
}
func (f fakeReader) List(context.Context) ([]store.Task, error) { /* sorted */ }
func (f fakeReader) GetTask(_ context.Context, id string) (*store.Task, error) {
	if t, ok := f.tasks[id]; ok { return &t, nil }
	return nil, store.ErrNotFound
}
func (f fakeReader) Audit(_ context.Context, id string) ([]store.AuditEntry, error) {
	return f.audit[id], nil
}

type fakeController struct{ cancelErr, enqueueErr error; calls []string }
func (f *fakeController) Enqueue(_ context.Context, i int) error {
	f.calls = append(f.calls, fmt.Sprintf("enqueue:%d", i)); return f.enqueueErr
}
func (f *fakeController) Cancel(_ context.Context, i int) error {
	f.calls = append(f.calls, fmt.Sprintf("cancel:%d", i)); return f.cancelErr
}
```

- [ ] **Step 2: Run to verify it fails**
  Run: `go test ./internal/mcp/ -run TestTools -v` → FAIL.

- [ ] **Step 3: Implement `tools.go`.**
```go
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

var issueSchema = json.RawMessage(`{"type":"object","properties":{"issue":{"type":"integer"}},"required":["issue"]}`)
var noArgsSchema = json.RawMessage(`{"type":"object","properties":{}}`)

func toolDefs() []toolDef {
	return []toolDef{
		{"list_tasks", "List all orchestrator tasks and their states.", noArgsSchema},
		{"get_task", "Get one task by issue number.", issueSchema},
		{"get_audit", "Get a task's audit trail by issue number.", issueSchema},
		{"cancel_task", "Cancel the running drive for an issue.", issueSchema},
		{"enqueue_task", "Re-drive an issue by number.", issueSchema},
	}
}

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

// callTool dispatches tools/call. Tool-execution problems (not found, not
// running) return a successful result with isError:true; only malformed params
// are a protocol error.
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

// MCP tool result: content is a list of typed blocks; isError flags a tool-level
// failure (distinct from a protocol error).
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
```
  Remove the Task 3 stubs of `toolDefs`/`callTool`.

- [ ] **Step 4: Run to verify it passes**
  Run: `go test ./internal/mcp/ -race -v` → PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/mcp/tools.go internal/mcp/tools_test.go internal/mcp/protocol.go
git commit -m "mcp: 5 tool handlers (list/get/audit/cancel/enqueue) + stable DTOs"
```

---

### Task 5: mcp HTTP server + lifecycle

**Files:**
- Create: `internal/mcp/server.go`.
- Test: `internal/mcp/server_test.go`.

**Interfaces:**
- Produces: `mcp.New(reader Reader, ctrl Controller, taskID func(int) string, log *slog.Logger) *Server`
  and `(*Server).Serve(ctx context.Context, ln net.Listener) error`.
- Consumes: `handler.handle` from Task 3.

- [ ] **Step 1: Write failing test** — `server_test.go` (httptest, mirrors
  `internal/notify/notify_test.go`). POST a `tools/call` for `list_tasks`, assert
  HTTP 200 + a JSON-RPC result; POST a malformed body, assert a `-32700` error.

```go
func TestServeToolsCall(t *testing.T) {
	srv := New(fakeReader{tasks: map[string]store.Task{}}, &fakeController{},
		func(i int) string { return fmt.Sprintf("issue-%d", i) }, slog.Default())
	ts := httptest.NewServer(http.HandlerFunc(srv.handleHTTP))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"result"`) {
		t.Fatalf("no result in %s", body)
	}
}
```

- [ ] **Step 2: Run to verify it fails**
  Run: `go test ./internal/mcp/ -run TestServe -v` → FAIL.

- [ ] **Step 3: Implement `server.go`.**
```go
package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server is the loopback MCP HTTP server. It never blocks the daemon: Serve runs
// the accept loop and returns when ctx is done (graceful Shutdown).
type Server struct {
	h *handler
}

func New(reader Reader, ctrl Controller, taskID func(int) string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{h: &handler{reader: reader, ctrl: ctrl, taskID: taskID, log: log}}
}

// Serve runs the HTTP server on ln until ctx is cancelled, then shuts it down.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleHTTP)
	mux.HandleFunc("/", s.handleHTTP)
	srv := &http.Server{Handler: mux}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleHTTP reads one JSON-RPC message and writes its response. Every request
// is recover-wrapped so a handler panic never crosses back into the daemon.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			s.h.log.Error("mcp handler panic", "recover", rec)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustMarshal(errResp(nil, codeInternal, "internal error")))
		}
	}()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(errResp(nil, codeParse, "read error")))
		return
	}
	resp, isNote := s.h.handle(r.Context(), body)
	w.Header().Set("Content-Type", "application/json")
	if isNote {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	_, _ = w.Write(resp)
}
```

- [ ] **Step 4: Run to verify it passes**
  Run: `go test ./internal/mcp/ -race -v` → PASS (all mcp tests).

- [ ] **Step 5: Commit**
```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "mcp: loopback HTTP server + graceful lifecycle"
```

---

### Task 6: Daemon wiring + docs

**Files:**
- Modify: `cmd/orchestratord/main.go` — `--mcp-listen` flag, mount the server,
  `EnableControl`, add the cancel terminal to `settledStates`, usage text.
- Modify: `README.md`, `ROADMAP.md`.

**Interfaces:**
- Consumes: `engine.ErrOperatorCancel`, `engine.CancelState`, `engine.TaskID`
  (Task 1); `(*Scheduler).EnableControl` (Task 2); `mcp.New`, `(*Server).Serve`
  (Task 5).

- [ ] **Step 1: Wire the flag + mount** in `cmdDaemon`. Add imports `"net"` and
  `"github.com/sean1588/herdr-orchestrator/internal/mcp"`. After `pollInterval`:
```go
	mcpListen := fs.String("mcp-listen", "", "MCP control server listen address, e.g. 127.0.0.1:7777 (default: off)")
```
  Add `engine.CancelState` to the settled set (right after `settled := settledStates(w.wf)`):
```go
	settled[engine.CancelState] = true // operator-cancelled tasks are terminal
```
  After `sched := &scheduler.Scheduler{...}` and before `sched.Serve(ctx)`:
```go
	if *mcpListen != "" {
		sched.EnableControl(engine.ErrOperatorCancel)
		ln, err := net.Listen("tcp", *mcpListen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: mcp listen %q: %v\n", *mcpListen, err)
			return 1
		}
		srv := mcp.New(w.store, sched, engine.TaskID, slog.Default())
		go func() {
			if err := srv.Serve(ctx, ln); err != nil {
				slog.Error("mcp server stopped", "err", err)
			}
		}()
		slog.Info("mcp control server listening", "addr", *mcpListen)
	}
```
  Add a `--mcp-listen` line to `usage()` under the daemon flags.

- [ ] **Step 2: Verify build + full suite + race**
  Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -race`
  Expected: green across the module.

- [ ] **Step 3: Manual smoke (optional, not CI)** — start a daemon with
  `--mcp-listen 127.0.0.1:7777` against a test config/db and `curl` a
  `tools/list` and a `list_tasks` to confirm the endpoint answers. Document the
  exact curl in the README.

- [ ] **Step 4: Documentation.**
  - `README.md`: add a "MCP control surface" section — enable via `--mcp-listen`,
    the loopback/no-auth posture, the 5 tools + their contracts, the
    dispatch-vs-completion semantics of cancel/enqueue, and that `cancelled` is a
    reserved terminal state.
  - `ROADMAP.md`: move "MCP server surface" from *Deferred subsystems* to *Built*,
    with a one-line note of what's deferred within it (pause/resume, auth, SSE).

- [ ] **Step 5: Commit**
```bash
git add cmd/orchestratord/main.go README.md ROADMAP.md
git commit -m "daemon: mount an optional loopback MCP control server (--mcp-listen); docs"
```

---

## Notes for the implementer

- **`context.WithoutCancel` + `context.WithCancelCause`** are Go 1.21/1.20+; the
  module is on go 1.26 — available.
- **The engine test "parked agent"** relies on `backend.Events` returning a
  channel that never sends/closes. Check the existing engine test fakes for the
  backend shape and reuse them; do not add a test-only method to production code.
- **`store.ErrNotFound`** is the sentinel `GetTask` wraps; `fakeReader` must
  return it so `get_task` renders the not-found tool error.
- **Do not** route cancel around the scheduler by writing task state directly —
  the whole point is that `advance` stays the single mutation point.
