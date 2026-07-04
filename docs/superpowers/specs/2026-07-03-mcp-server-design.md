# MCP Server Surface — Design Spec

**Date:** 2026-07-03
**Status:** Approved (brainstorm), proceeding to plan + implementation.
**Roadmap item:** "Deferred subsystems → MCP server surface."

## Goal

Expose orchestrator state and a small control surface over MCP, so an operator
or a supervising agent can observe the daemon's tasks and intervene per-task —
without SIGINT-ing the whole daemon. Read tools land on the store's existing
single-writer-safe reads; control tools route through the scheduler, never
touching a task row directly.

## Scope (locked)

**Read tools (3):** `list_tasks`, `get_task`, `get_audit`.
**Control tools (2):** `cancel_task`, `enqueue_task`.
**Deferred:** `pause`/`resume`, OAuth, streamable-HTTP SSE/server-push,
pagination/filtering, any second transport. Each is a later cut if a concrete
need names itself.

**Transport:** loopback HTTP, hand-rolled JSON-RPC 2.0 on stdlib
(`net/http` + `encoding/json`) — the request/response subset of MCP's
Streamable HTTP transport. **Zero new dependencies**; pure-Go / cgo-free intact.
**Auth:** none — the loopback bind is the only boundary (see Named debts).
**Mount:** in-process goroutine inside `orchestratord daemon`, opt-in via a
`--mcp-listen host:port` flag (default `""` = off), mirroring the
`--notify-webhook` opt-in precedent.

## Why these choices (the forks that were decided)

- **Full control, not read-only.** The daemon runs unattended driving agents
  that demonstrably hang (529s, dead panes); today the only remedy is SIGINT,
  which kills all N workers. A per-task `cancel` is a real operational escape
  hatch — a named reason, not gold-plating.
- **In-process, listener transport (stdio ruled out).** Control must reach the
  running scheduler's command seam and the shared store handle, so the server
  lives in the daemon goroutine. A persistent daemon can't also be a stdio
  subprocess an MCP client spawns, so the transport must be a localhost
  listener. Separate-process was rejected: no WAL is set, so cross-process
  reads degrade to 5s lock-waits, and control is structurally impossible.
- **Hand-rolled JSON-RPC over an SDK.** Verified compatible with the repo's
  minimal-deps / pure-Go discipline; an SDK (and any OAuth stack) is an
  unvetted dependency subtree. `encoding/json` is already the pervasive JSON
  idiom in the tree.
- **No auth, loopback only.** Single-operator box; the bind address is the
  boundary. Named debt below.

## Architecture

New package **`internal/mcp`**, depending only on small interfaces (per the
`CLAUDE.md` boundary rule) — never on `*store.Store` / `*scheduler.Scheduler`
concretely:

```go
// internal/mcp

// Reader is the read surface. *store.Store already satisfies it.
type Reader interface {
	List(ctx context.Context) ([]store.Task, error)
	GetTask(ctx context.Context, id string) (*store.Task, error)
	Audit(ctx context.Context, taskID string) ([]store.AuditEntry, error)
}

// Controller is the control surface. *scheduler.Scheduler satisfies it.
// Methods return descriptive errors the tool layer renders verbatim as
// isError text — no sentinel coupling. Enqueue is idempotent (nil on
// already-inflight); Cancel errors ("issue N is not currently running") when
// no drive is active for the issue.
type Controller interface {
	Enqueue(ctx context.Context, issue int) error
	Cancel(ctx context.Context, issue int) error
}
```

Files, one responsibility each:

- `jsonrpc.go` — JSON-RPC 2.0 request / response / error envelope + standard
  error codes. Handles the notification case (no `id` → no response).
- `protocol.go` — MCP method dispatch: `initialize`, `notifications/initialized`,
  `tools/list`, `tools/call` (+ optional `ping`).
- `tools.go` — the 5 tool handlers; maps `store.Task` → `TaskView` and
  `store.AuditEntry` → `AuditEntryView` (stable output shapes).
- `server.go` — the `net/http` server: single `/mcp` endpoint, loopback bind,
  per-request panic recovery, lifecycle.

**Injection.** The daemon constructs the server with
`mcp.New(reader Reader, ctrl Controller, taskID func(int) string, logger)`,
passing `engine.TaskID` as the canonical `issue → "issue-N"` formatter so the id
format is not duplicated (the daemon imports both packages; `mcp` imports
neither `engine` nor `scheduler`).

**Mount point** — in `cmdDaemon`, after `cf.wire(ctx)` and before the blocking
`sched.Serve(ctx)`:

```
ln, err := net.Listen("tcp", mcpListen)   // synchronous: bind failure fails startup
if err != nil { return fmt.Errorf("mcp listen: %w", err) }
srv := mcp.New(w.store, sched, engine.TaskID, logger)
go srv.Serve(ctx, ln)                      // accept loop; srv.Shutdown on ctx.Done
```

The MCP surface is auxiliary: a *bind* failure fails startup (an explicit
`--mcp-listen` that can't bind is a misconfig the operator must see), but a
post-bind accept-loop failure is logged loudly and does **not** tear down the
task-driving daemon.

## The control seam (the only existing code that changes)

Two additions to `internal/scheduler`, plus one principled reach into
`internal/engine`.

### (a) Command channel

A `commands chan command` on `Scheduler`, selected in `Serve`'s existing poller
`for`/`select` loop alongside `ticker.C` / `ctx.Done()`. Commands are handled
**in the poller goroutine** — already the sole sender on the `work` channel — so
`enqueue` preserves the single-sender invariant for free. Each command carries a
`reply chan error` so the tool returns a prompt *dispatch* result, not
completion:

```go
type cmdKind int
const ( cmdEnqueue cmdKind = iota; cmdCancel )

type command struct {
	kind  cmdKind
	issue int
	reply chan error   // nil, ErrNotRunning, or ErrAlreadyInflight
}
```

`Scheduler.Enqueue(issue)` / `Scheduler.Cancel(issue)` (satisfying
`mcp.Controller`) build a `command`, send it on `commands`, and block on `reply`
(with the caller's ctx as an escape). This makes the scheduler the single owner
of both mutations.

### (b) Per-issue cancel registry

Today workers pass the shared ctx to `RunTask` unmodified. Change: when a worker
picks up issue N,

```go
taskCtx, cancel := context.WithCancelCause(ctx)
s.registerCancel(issue, cancel)      // mutex-guarded map[int]context.CancelCauseFunc
defer s.deregisterCancel(issue, cancel)
err := s.RunTask(taskCtx, issue)
```

A `cmdCancel` handled in the poller looks up the issue's func; if present it
calls `cancel(ErrOperatorCancel)` and replies `nil`; if absent (no task
inflight) it replies `ErrNotRunning`.

### (c) The one engine reach — and why it is unavoidable

A cancel that merely stops the drive leaves the task in a **non-terminal**
state, so `SeedFrom` re-drives it next poll and un-cancels it. For cancel to
*stick*, the task must reach a terminal state — and the single-writer invariant
says **only the owning drive goroutine may write that row**. So the drive's
error path (which already surfaces `ctx.Err()` from `awaitAgentState`'s
`ctx.Done()` select) learns one new thing: if the context was cancelled with the
operator cause, settle instead of abort.

```go
// engine (exported so the scheduler can cancel with it; the engine interprets it):
var ErrOperatorCancel = errors.New("operator cancel")
const CancelState = "cancelled"     // reserved runtime state; not a workflow state

// in drive's error branch (was: return task.CurrentState, err):
if err != nil {
	if errors.Is(context.Cause(ctx), ErrOperatorCancel) {
		return e.settleCancelled(ctx, task)   // settle to CancelState + cleanup
	}
	return task.CurrentState, err             // plain shutdown: leave state for recovery
}
```

**Correctness detail found while reading source:** the store's writes are
ctx-aware, so calling `advance` on the *cancelled* context would fail with
`context.Canceled` and the settle would never persist. `settleCancelled` must run
on a **detached context**:

```go
func (e *Engine) settleCancelled(ctx context.Context, task *store.Task) (string, error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := e.advance(sctx, task, CancelState, "operator.cancel", ""); err != nil {
		return task.CurrentState, err
	}
	e.maybeCleanup(sctx, task)   // tear down the worktree (no-PR case), best-effort
	return CancelState, nil
}
```

`advance` does **not** gate on FSM-legality (verified: engine.go:693 just appends
audit + writes state), so `implementing → cancelled` is a valid write. This keeps
**all** task-state writes inside the engine — the scheduler only cancels
contexts, it never writes state. The rejected alternative (scheduler writes
`cancelled` after the drive exits, zero engine touch) forks write authority and
needs cross-goroutine "wait for return then write" coordination; preserving
"`advance` is THE single mutation point" is the stronger structural property.

### The `cancelled` terminal is daemon-owned, not a workflow state

`engine.CancelState` (`"cancelled"`) never appears in the YAML or
`workflow.schema.json` (no config/schema change). It is a runtime `current_state`
string the store accepts (the store is workflow-agnostic), recognized as terminal
in exactly two places — the same (already-forked) halt knowledge the rest of the
system uses:

- **Engine** — `isHalt` gains `|| state == CancelState` (engine.go:780), so
  every engine invocation (`daemon`, `recover`, `run`) treats a cancelled task
  as halted. Without this, a later `orchestratord recover` would try to drive
  `cancelled` → `runState` errors "no supported trigger." This also makes a stray
  `RunTask` on an already-cancelled task a clean no-op.
- **Daemon** — `settledStates` gains `out[engine.CancelState] = true`
  (main.go:502), so `SeedFrom` won't re-drive a cancelled task and `doneChecker`
  drains its source label on the settle path like any other terminal
  (`RemoveLabel` is already idempotent — main.go:533).

The name is referenced from the single exported const, not duplicated as a magic
string. This lightly touches the ROADMAP's "settled/resume fork" (settled-set
recognition) by extending both forks consistently; the broader `Recover`/`isHalt`
*unification* stays its existing backlog item. Caveat: `"cancelled"` is now a
reserved runtime state name — a workflow that declares a non-terminal state named
`cancelled` would see it force-halted (the default pipeline does not; noted in
the README).

### Wiring the cancel cause (no engine↔scheduler import)

`ErrOperatorCancel` lives in `engine` (the package that interprets it). The
scheduler cancels with it *without importing engine*: the daemon injects it via
`sched.EnableControl(engine.ErrOperatorCancel)`, which also creates the command
channel. Both control mutations flow through one uniform mechanism — the
scheduler's command channel, processed in the poller goroutine (already the sole
sender on `work`, so `enqueue` preserves the single-sender invariant). The
per-issue `context.CancelCauseFunc` registry is `Serve`-local (never escapes),
touched only by the workers (register/deregister) and the poller's command
handler (cancel). Control is inert when `--mcp-listen` is off: the `commands`
channel is nil (its `select` case is never ready) and no MCP server is started.

## Tool contracts

All tools take `{"issue": <int>}` except `list_tasks` (no args).

| Tool | Args | Success | Tool error (`isError:true`) |
|------|------|---------|------------------------------|
| `list_tasks` | — | `[]TaskView` (all tasks, id order) | — |
| `get_task` | `issue` | one `TaskView` | `issue N not found` |
| `get_audit` | `issue` | `[]AuditEntryView` (chronological) | `issue N not found` |
| `cancel_task` | `issue` | `cancel dispatched for issue N` | `issue N is not currently running` |
| `enqueue_task` | `issue` | `enqueued issue N` | `issue N already running` |

Cancel/enqueue are **dispatch-acknowledged, not completion-acknowledged**: the
tool confirms the command reached the scheduler and was actionable; the operator
observes the outcome (`cancelled` state, a new PR) via `get_task` / `get_audit`.

### Output DTOs (stable shapes)

```go
type TaskView struct {
	ID          string         `json:"id"`
	Issue       int            `json:"issue"`
	Repo        string         `json:"repo"`
	Branch      string         `json:"branch"`
	State       string         `json:"state"`
	PRNumber    *int           `json:"pr_number,omitempty"`
	RetryCounts map[string]int `json:"retry_counts,omitempty"`
	CreatedAt   string         `json:"created_at"`   // RFC3339
	UpdatedAt   string         `json:"updated_at"`   // RFC3339
}

type AuditEntryView struct {
	TS        string `json:"ts"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Trigger   string `json:"trigger"`
	Result    string `json:"result,omitempty"`
}
```

Volatile/internal fields (`PaneID`, `PaneSpawnState`, `WorkflowSnapshot`) are
deliberately omitted; the audit `TaskID` is dropped (redundant with the query).

## Data flow (cancel, end to end)

```
client → POST /mcp {jsonrpc, method:"tools/call",
                    params:{name:"cancel_task", arguments:{issue:208}}}
  → http handler (recover-wrapped) → jsonrpc decode → tools/call dispatch
  → cancel_task handler → ctrl.Cancel(208)
  → scheduler.Cancel builds command{cmdCancel,208,reply}, sends on commands, waits
  → poller goroutine: cancelReg[208] present? call cancel(ErrOperatorCancel); reply nil
                       else reply ErrNotRunning
  → handler: nil → result "cancel dispatched for issue 208"
             ErrNotRunning → result isError "issue 208 is not currently running"
  → jsonrpc response → http 200
(async) the drive unwinds, settles issue-208 to "cancelled" via advance.
```

## Error handling

- **Protocol errors** (malformed) → JSON-RPC `error`: parse `-32700`, invalid
  request `-32600`, method not found `-32601`, invalid params `-32602`, internal
  `-32603`.
- **Tool errors** (ran, reports a problem: not-found, not-running, already-inflight)
  → successful `result` with `isError:true` + a text `content` explaining. This
  is the MCP convention and keeps "your request was bad" distinct from "the tool
  says no."
- **Panic safety** — every request is `recover`-wrapped → log + `-32603`; never
  crashes the daemon ("no panics in the daemon path").
- **Store errors** — `ErrNotFound` → tool `isError`; other errors → tool
  `isError` with a generic message + the real error logged (no internal leak).

## Testing (table-driven, per `CLAUDE.md`)

- `internal/mcp/jsonrpc_test.go` — envelope encode/decode; error-code mapping;
  notification (no `id`) produces no response.
- `internal/mcp/protocol_test.go` — `initialize` shape; `tools/list` returns 5
  tools with valid `inputSchema`s; `tools/call` routes correctly; unknown method
  → `-32601`; unknown tool → tool `isError`.
- `internal/mcp/tools_test.go` — with `fakeReader` + `fakeController`:
  `list_tasks` serialization (nil `PRNumber` omitted, empty `RetryCounts`
  omitted, RFC3339 times); `get_task` found / not-found; `get_audit` ordering;
  `cancel_task` dispatched / not-running; `enqueue_task` enqueued / already-running.
  Assert exact JSON output.
- `internal/mcp/server_test.go` — `httptest` (precedent: `notify_test.go`): POST
  a `tools/call`, assert HTTP 200 + JSON-RPC result; malformed body → `-32700`.
- `internal/scheduler/*_test.go` — a `cmdCancel` cancels the registered per-issue
  context (fake `RunTask` blocks on ctx; assert it unblocks with cause
  `ErrOperatorCancel`); cancel of a non-inflight issue → `ErrNotRunning`;
  `cmdEnqueue` enqueues (work chan receives; `inflightSet` dedups a double-enqueue).
- `internal/engine/*_test.go` — **the key correctness test:** a drive whose ctx
  is cancelled with `ErrOperatorCancel` advances to `CancelState` (terminal) and
  writes the row through the single mutation point; a plain-shutdown cancel
  aborts, leaving state for recovery.

Keep `go build ./...`, `go test ./...`, `go test -race`, `go vet ./...`,
`gofmt -l` green at every task.

## Documentation

Update `README.md` when the surface lands: a short "MCP control surface"
section — how to enable (`--mcp-listen`), the 5 tools + their contracts, the
loopback/no-auth posture, and the dispatch-vs-completion semantics of the
control tools. Update `ROADMAP.md` to move "MCP server surface" from Deferred to
Built, noting what's deferred within it (pause/resume, auth, SSE).

## Named debts (accepted, recorded so they aren't rediscovered)

- **No auth.** The loopback bind is the only boundary; any local process can
  drive control (including cancel). Accepted for a single-operator box. Revisit
  if the daemon ever runs where not every local process is trusted — the
  bearer-token add is ~10 lines against the same flag.
- **MCP-enqueued issue without the source label.** It drives and settles fine
  (kept alive by `SeedFrom` over store non-settled states), but `doneChecker`'s
  settle-path label removal finds no label to drain. Verify `github.RemoveLabel`
  tolerates an absent label (no hard error); if it doesn't, guard it. In
  practice an enqueued issue usually still carries the label.
- **`cancelled` and the settled/resume fork.** v1 wires `cancelled` into the
  daemon settled-set + engine halt only; the broader `Recover`/`isHalt`
  unification remains the ROADMAP backlog item.
- **No pagination on `list_tasks`.** Returns all tasks; fine at current scale.
  Add a filter/limit if the settled backlog ever makes output noisy.
```
