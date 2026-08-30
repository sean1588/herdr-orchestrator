// Package engine executes a validated workflow as a deterministic state graph.
//
// It drives the full review->merge loop: queued -> implementing -> pr_open ->
// (review decision) -> approved -> (merge gate) -> merging -> merged, plus the
// intake triage decision (accept/reject/needs_human), the changes_requested
// resume loop, the agent.blocked alert, and the timeout / retry_exhausted
// escalation edges. The default goal is "merged"; the real merge is withheld
// under policies.dry_run (default-on), which halts at "merging". The MCP surface
// and cross-task memory remain out of scope: the engine validates them in the
// full pipeline but does not execute them.
//
// Run drives one issue to completion. The daemon (cmd/orchestratord) may run up
// to policies.max_concurrent_tasks such drives concurrently; each task's state is
// written only by the goroutine driving that issue (tasks are row-partitioned by
// issue id) and the store serializes all writes through a single connection, so
// concurrent drives never race. GitHub is authoritative for artifacts; an agent's
// "done" is only a trigger to go check GitHub.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/notify"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// autoFiredEvents are events with no real external source; the engine fires them
// immediately on entering the state.
var autoFiredEvents = map[string]bool{"scheduled": true}

// errSuspended is a control signal, not a failure: a drive parked in a merge-gate
// wait (blocked_on_gate) evaluates the gate once and, while it neither passes nor
// times out, yields — Run returns with the task persisted at that state so the
// worker slot is freed. The scheduler re-drives the task on a later poll. Handled
// in drive; never surfaced to callers.
var errSuspended = errors.New("suspended: awaiting merge gate")

// ErrOperatorCancel is the cancellation cause an operator cancel carries: the
// scheduler cancels a drive's per-issue context with this cause, and a drive that
// observes it settles the task to CancelState instead of aborting, so the cancel
// sticks. Any other cause (daemon shutdown via SIGINT) aborts the drive and
// leaves the state untouched for recovery. Exported so the scheduler can cancel
// with it without importing engine (the daemon injects it).
var ErrOperatorCancel = errors.New("operator cancel")

// ErrDriveDeadline is the cancellation cause the scheduler's reaper carries when
// a drive outlives its wall-clock ceiling (policies.drive_deadline). A drive that
// observes it settles to its state's escalation target instead of aborting, so a
// wedged drive stops being re-driven forever.
//
// It exists because the engine's only clock — the timer in awaitAgentState — is
// armed solely once a drive reaches the event-wait loop in a state that declares
// a timeout. Everything outside that window (creating a worktree, launching a
// pane, evaluating a gate, running a decision, merging a PR) has no timer at all,
// so the bound must come from a watchdog that does not share a goroutine with the
// thing it watches. Exported so the scheduler can cancel with it without
// importing engine (the daemon injects it).
var ErrDriveDeadline = errors.New("drive deadline exceeded")

// settleTimeout bounds the detached write that records a forced settle. The
// drive's own context is already cancelled by then, and the store's writes are
// ctx-aware, so the settle runs on a context detached from that cancellation —
// otherwise the write would itself fail with context.Canceled and the settle
// would not stick.
const settleTimeout = 15 * time.Second

// CancelState is the reserved terminal a task lands in when an operator cancels
// it. It is never a workflow state (never in the YAML or workflow.schema.json);
// the store accepts it as an opaque current_state, and isHalt plus the daemon's
// settledStates recognize it as terminal so the task is neither re-driven nor
// re-listed. A workflow that declares a non-terminal state named "cancelled"
// would see it force-halted; the default pipeline does not.
const CancelState = "cancelled"

// Config wires the engine's dependencies and tunables.
type Config struct {
	Workflow *config.Workflow
	Backend  exec.ExecutionBackend
	GitHub   github.Client
	Store    *store.Store

	// WorkflowSource is the raw config bytes snapshotted onto each new task, so
	// every later drive resumes against the graph the task started under (see
	// engineForTask). Empty => no snapshot, and drives fall back to the current
	// --config.
	WorkflowSource []byte

	RepoDir   string // local checkout (absolute) where git/gh run
	Base      string // base branch, e.g. "main"
	Repo      string // owner/name slug recorded on the task
	ConfigDir string // dir of the workflow config; decision rubric paths resolve against it

	// Optional; sensible defaults applied by New.
	TaskDir      string                              // where task files are written; default os.TempDir()
	Goal         string                              // halt-on-enter success state; default "pr_open"
	StartState   string                              // where the daemon enqueues discovered issues; default "queued"
	DurationFunc func(string) (time.Duration, error) // default time.ParseDuration
	// NoProgressTimeout overrides the global liveness bound resolved from
	// policies.no_progress_timeout. Nil => resolve from the workflow. Set to a
	// pointer-to-zero to disable the bound outright (what most unit tests want,
	// since they drive fake backends whose panes never produce bytes).
	NoProgressTimeout *time.Duration
	Logger            *slog.Logger
	// Notifier forwards escalation/alert events out-of-band; default notify.Nop.
	Notifier notify.Notifier
}

// Engine drives tasks through the workflow.
type Engine struct {
	wf             *config.Workflow
	backend        exec.ExecutionBackend
	gh             github.Client
	store          *store.Store
	workflowSource []byte

	repoDir, base, repo string
	configDir           string
	taskDir             string
	goal, startState    string
	parseDur            func(string) (time.Duration, error)
	// noProgress is the resolved global liveness bound (see Config.NoProgressTimeout).
	noProgress time.Duration
	now        func() time.Time // injectable clock; drives the blocked_on_gate wait timeout
	log        *slog.Logger
	notifier   notify.Notifier
}

// New builds an Engine, applying defaults.
func New(c Config) *Engine {
	e := &Engine{
		wf:             c.Workflow,
		backend:        c.Backend,
		gh:             c.GitHub,
		store:          c.Store,
		workflowSource: c.WorkflowSource,
		repoDir:        c.RepoDir,
		base:           c.Base,
		repo:           c.Repo,
		configDir:      c.ConfigDir,
		taskDir:        c.TaskDir,
		goal:           c.Goal,
		startState:     c.StartState,
		parseDur:       c.DurationFunc,
		log:            c.Logger,
		notifier:       c.Notifier,
	}
	if e.taskDir == "" {
		e.taskDir = os.TempDir()
	}
	// Task/verdict files are handed to agents by path in their kickoff, but the
	// agent runs in its own worktree (a different cwd), so a relative taskDir would
	// resolve against the wrong directory and the agent couldn't find its rubric or
	// write its verdict where the engine reads it. Anchor it absolutely once.
	if abs, err := filepath.Abs(e.taskDir); err == nil {
		e.taskDir = abs
	}
	if e.goal == "" {
		e.goal = "merged"
	}
	if e.startState == "" {
		e.startState = "queued"
	}
	if e.parseDur == nil {
		e.parseDur = time.ParseDuration
	}
	// Resolved from the workflow, not via DurationFunc: tests stub DurationFunc to
	// return one fixed value for every state timeout, and letting that also govern
	// the global bound would silently couple two unrelated knobs.
	e.noProgress = resolveNoProgress(c.NoProgressTimeout, c.Workflow, e.log)
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if e.notifier == nil {
		e.notifier = notify.Nop{}
	}
	return e
}

// Run drives a single issue from the start state to the goal (pr_open) or a
// terminal state, returning the final state. If the task already exists (a
// re-run after a crash), it is reconciled against herdr/GitHub before driving.
//
// The task is driven against its own WorkflowSnapshot, not the engine's current
// --config: the daemon re-drives non-settled tasks on every poll and on restart,
// so without this an operator editing the config file would silently change the
// rules for work already in flight.
func (e *Engine) Run(ctx context.Context, issue int) (string, error) {
	task, created, err := e.ensureTask(ctx, issue)
	if err != nil {
		return "", err
	}
	// Only a pre-existing task can have drifted from the current --config. One
	// created on this very call carries e's own workflowSource as its snapshot by
	// construction, so re-parsing it would be redundant work and an extra failure
	// mode on the hot path.
	eng := e
	if !created {
		eng, err = e.engineForTask(task)
		if err != nil {
			return "", err
		}
		if err := eng.reconcile(ctx, task); err != nil {
			return eng.reclassifyCancel(ctx, task, task.CurrentState, err)
		}
	}
	return eng.drive(ctx, task)
}

// engineForTask returns the engine a task must be driven by: one bound to the
// workflow the task started under (its snapshot), never a possibly-edited
// current --config. Re-validating via config.Parse keeps this fail-closed — a
// snapshot that no longer satisfies the invariants yields an error rather than
// being silently run. An empty snapshot (a legacy row written before snapshots
// existed) falls back to the current config, preserving pre-snapshot behavior.
//
// Policies pin alongside the graph, deliberately. dry_run is the motivating
// case: a flag whose entire job is to withhold a side effect must not stop
// applying to in-flight work the moment someone edits a file. Changing policy
// for a running task is therefore an explicit act — cancel it, or let it settle.
func (e *Engine) engineForTask(task *store.Task) (*Engine, error) {
	if task.WorkflowSnapshot == "" {
		return e, nil
	}
	wf, _, err := config.Parse([]byte(task.WorkflowSnapshot))
	if err != nil {
		return nil, fmt.Errorf("task %s: workflow snapshot invalid (fix or migrate the stored config): %w", task.ID, err)
	}
	return e.cloneWithWorkflow(wf), nil
}

// Recover reconciles in-flight tasks against live herdr panes and GitHub PRs,
// then resumes driving each to completion. Reconcile keys on the deterministic
// branch / durable task id, never the volatile pane id.
func (e *Engine) Recover(ctx context.Context) error {
	tasks, err := e.store.List(ctx)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	for i := range tasks {
		task := &tasks[i]
		// Drive the task against the graph it started under; see engineForTask.
		// Recovery is a batch over every stored task, so an invalid snapshot skips
		// just that task with a warning rather than aborting the whole sweep.
		eng, perr := e.engineForTask(task)
		if perr != nil {
			e.log.Warn("recover: task snapshot invalid; skipping (fix or migrate)", "task", task.ID, "err", perr)
			continue
		}
		if eng.isHalt(task.CurrentState) {
			continue
		}
		e.log.Info("recovering task", "task", task.ID, "state", task.CurrentState)
		if err := eng.reconcile(ctx, task); err != nil {
			e.log.Warn("reconcile failed", "task", task.ID, "err", err)
			continue
		}
		if _, err := eng.drive(ctx, task); err != nil {
			e.log.Warn("resume failed", "task", task.ID, "err", err)
		}
	}
	return nil
}

// cloneWithWorkflow returns a shallow copy of e bound to a different workflow, so
// a recovered task can be driven against the graph it started under. All other
// dependencies (store, backend, gh, logger, notifier, goal, ...) are shared.
func (e *Engine) cloneWithWorkflow(wf *config.Workflow) *Engine {
	c := *e
	c.wf = wf
	// The liveness bound is a policy of the workflow, so it must be re-resolved
	// against the snapshot rather than inherited from the daemon's current config.
	c.noProgress = resolveNoProgress(nil, wf, e.log)
	return &c
}

// resolveNoProgress resolves the global liveness bound: an explicit override
// wins, otherwise the workflow's policy (or its default). A malformed policy
// value is logged and treated as disabled rather than failing engine
// construction — config.Parse already rejects it at load, so reaching this is a
// programming error, not an operator one.
func resolveNoProgress(override *time.Duration, wf *config.Workflow, log *slog.Logger) time.Duration {
	if override != nil {
		return *override
	}
	if wf == nil {
		return 0
	}
	d, err := wf.Policies.ResolveNoProgressTimeout()
	if err != nil {
		log.Warn("no_progress_timeout unparseable; liveness bound disabled", "err", err)
		return 0
	}
	return d
}

// ensureTask loads an existing task or creates a fresh one at the start state.
// The bool return reports whether a new task was created.
func (e *Engine) ensureTask(ctx context.Context, issue int) (*store.Task, bool, error) {
	id := TaskID(issue)
	existing, err := e.store.GetTask(ctx, id)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, false, fmt.Errorf("load task %s: %w", id, err)
	}
	task := &store.Task{
		ID:               id,
		Issue:            issue,
		Repo:             e.repo,
		Branch:           branchName(issue),
		CurrentState:     e.startState,
		WorkflowSnapshot: string(e.workflowSource),
	}
	if err := e.store.CreateTask(ctx, task); err != nil {
		return nil, false, fmt.Errorf("create task %s: %w", id, err)
	}
	e.log.Info("task created", "task", id, "state", e.startState, "branch", task.Branch)
	return task, true, nil
}

// drive runs the interpreter loop until a halt state, then reclassifies an
// operator cancellation observed anywhere in the loop into a settle (see
// reclassifyCancel) — so the cancel sticks whether it landed in the agent wait,
// a gate read, a spawn, or the advance write.
func (e *Engine) drive(ctx context.Context, task *store.Task) (string, error) {
	state, err := e.driveLoop(ctx, task)
	return e.reclassifyCancel(ctx, task, state, err)
}

// reclassifyCancel converts a drive error caused by an operator cancel into a
// settle to CancelState; any other error (including a daemon-shutdown cancel,
// whose cause is not ErrOperatorCancel) is returned unchanged for recovery. It
// runs at every drive/reconcile exit so the settle is not tied to one wait site.
//
// The guard keys off the CONTEXT's cancellation cause, not the error's shape: a
// cancel that lands mid-subprocess (gh/herdr/git killed by SIGKILL via
// exec.CommandContext) surfaces as an *exec.ExitError ("signal: killed"), NOT
// context.Canceled — so requiring context.Canceled would miss exactly the
// reconcile/spawn/gate-read windows this wrapper exists to cover. Requiring only
// err != nil keeps a drive that completed (err == nil) from being force-settled
// when a cancel arrived too late.
func (e *Engine) reclassifyCancel(ctx context.Context, task *store.Task, state string, err error) (string, error) {
	if err == nil {
		return state, err
	}
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, ErrOperatorCancel):
		return e.settleCancelled(ctx, task)
	case errors.Is(cause, ErrDriveDeadline):
		return e.settleDeadline(ctx, task)
	}
	return state, err
}

// settleDeadline records a reaped drive as a transition to its state's
// escalation target. Unlike an operator cancel — which is a human deciding to
// stop a task — this is the system reporting that a drive made no progress and
// could not be bounded from within, so it routes to the same place the state's
// own timeout would have gone rather than to a bespoke terminal.
//
// The worktree is deliberately NOT torn down: a wedged drive is exactly the case
// a human needs to inspect, and it may hold uncommitted work.
func (e *Engine) settleDeadline(ctx context.Context, task *store.Task) (string, error) {
	from := task.CurrentState
	target, why := e.escalationTarget(from)
	e.log.Warn("drive deadline exceeded", "task", task.ID, "state", from, "to", target, "via", why)

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if err := e.advance(sctx, task, target, "drive_deadline", why); err != nil {
		return from, fmt.Errorf("settle drive deadline: %w", err)
	}
	e.notifyTerminalAlert(sctx, task)
	return target, nil
}

// escalationTarget resolves where a state gives up to when a bound fires that is
// not itself a declared transition — the drive deadline, the blocked bound, and
// the no-progress bound all land here. It prefers the state's own timeout target,
// so such an escalation is indistinguishable from the timeout it stands in for. A
// state with no timeout transition falls back to the workflow's alerting terminal
// (the escalated state), and a workflow with neither falls back to CancelState —
// which settles the task rather than leaving it to be re-driven and re-bounded
// forever.
//
// This fallback is what makes the bounds apply by default rather than by
// remembering: before it, a bound whose only escalation path was the state's
// timeout edge was silently inert in every state that declared none.
//
// The second return names the derivation, and is recorded on the audit row so a
// fallback is never silent.
func (e *Engine) escalationTarget(state string) (target, why string) {
	if t := findTimeoutTransition(e.wf.States[state]); t != nil && t.To != "" {
		return t.To, "state_timeout_target"
	}
	if n := e.alertTerminal(); n != "" {
		return n, "alert_terminal"
	}
	return CancelState, "no_escalation_target"
}

// alertTerminal returns the workflow's escalation terminal — a terminal state
// flagged alert — or "" if it declares none. Map iteration is randomized, so the
// names are sorted to keep the choice deterministic across restarts.
func (e *Engine) alertTerminal() string {
	names := make([]string, 0, len(e.wf.States))
	for name := range e.wf.States {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if st := e.wf.States[name]; st.Terminal != "" && st.Alert {
			return name
		}
	}
	return ""
}

// driveLoop runs the interpreter loop until a halt state (goal or terminal).
func (e *Engine) driveLoop(ctx context.Context, task *store.Task) (string, error) {
	// transitioned guards the escalated notification: fire only when this drive
	// actually moved the task into the alert terminal state, not when it was
	// re-entered already there (a re-run of an escalated issue must stay quiet).
	transitioned := false
	for {
		if e.isHalt(task.CurrentState) {
			if transitioned {
				e.notifyTerminalAlert(ctx, task)
				e.maybeCleanup(ctx, task)
			}
			e.log.Info("halt", "task", task.ID, "state", task.CurrentState, "pr", prNum(task))
			return task.CurrentState, nil
		}
		next, trigger, result, err := e.runState(ctx, task)
		if errors.Is(err, errSuspended) {
			// A merge-gate wait yielded: leave the task at its current state (no
			// transition, no audit) and return so the worker frees. The scheduler
			// re-drives it, re-evaluating the gate, until it passes or times out.
			e.log.Info("suspend: awaiting merge gate; yielding worker", "task", task.ID, "state", task.CurrentState)
			return task.CurrentState, nil
		}
		if err != nil {
			// Cancellation is reclassified by the drive wrapper; other errors surface.
			return task.CurrentState, err
		}
		if next == "" {
			// A state action chose to halt without a transition (a dry-run merge:
			// the side effect is withheld, so pr.merged never fires). Record it and
			// stop. Every real transition returns a non-empty next.
			if aerr := e.store.AppendAudit(ctx, store.AuditEntry{
				TaskID: task.ID, FromState: task.CurrentState, ToState: task.CurrentState,
				Trigger: trigger, Result: result,
			}); aerr != nil {
				return task.CurrentState, fmt.Errorf("audit halt: %w", aerr)
			}
			e.log.Info("halt (action)", "task", task.ID, "state", task.CurrentState, "trigger", trigger, "result", result)
			return task.CurrentState, nil
		}
		if err := e.advance(ctx, task, next, trigger, result); err != nil {
			return task.CurrentState, err
		}
		transitioned = true
	}
}

// runState runs the current state's entry action, then waits for a trigger and
// resolves the next state.
func (e *Engine) runState(ctx context.Context, task *store.Task) (next, trigger, result string, err error) {
	name := task.CurrentState
	st := e.wf.States[name]

	if st.Entry != nil {
		switch {
		case st.Entry.Spawn != "":
			if err := e.spawn(ctx, task, st.Entry.Spawn, st); err != nil {
				return "", "", "", err
			}
		case st.Entry.Resume != "":
			// Count a retry only for a genuinely new round. A crash + Recover
			// re-enters the same state with its agent already spawned for it
			// (PaneSpawnState == state); re-counting there would burn a retry the
			// reviewer never asked for. PaneSpawnState != state means this is the
			// first entry since the last transition in, i.e. a fresh round.
			if task.PaneSpawnState != task.CurrentState {
				target, exhausted, err := e.checkRetryCap(task, st)
				if err != nil {
					return "", "", "", err
				}
				if exhausted {
					e.log.Info("retry cap exhausted", "task", task.ID, "state", name)
					return target, "retry_exhausted", "", nil
				}
			}
			if err := e.spawn(ctx, task, st.Entry.Resume, st); err != nil {
				return "", "", "", err
			}
		case st.Entry.Action != "":
			return e.runMergeAction(ctx, task, st)
		}
	}

	// Auto-fired events (scheduler stubbed): fire immediately.
	for i := range st.Transitions {
		t := &st.Transitions[i]
		if t.When.Event != "" && autoFiredEvents[t.When.Event] {
			return t.To, t.When.Event, "", nil
		}
	}

	// Agent-driven wait (the implementing slice).
	if findEventTransition(st, "agent.done") != nil {
		return e.awaitAgentState(ctx, task, st)
	}

	// Merge-gate wait: status.changed re-evaluates the merge gate. A state with a
	// timeout (blocked_on_gate) evaluates once and, while the gate neither passes
	// nor has timed out, suspends (yields its worker); one without (approved) checks
	// once on entry and branches.
	if sct := findEventTransition(st, "status.changed"); sct != nil {
		if timeoutT := findTimeoutTransition(st); timeoutT != nil {
			return e.evaluateGateOrSuspend(ctx, task, sct, timeoutT)
		}
		verdict, err := e.evaluateGate(ctx, task, sct)
		if err != nil {
			return "", "", "", err
		}
		return sct.Branch[verdict], "status.changed", verdict, nil
	}

	return "", "", "", fmt.Errorf("state %q: no supported trigger (no agent.done or status.changed transition)", name)
}

// evaluateGateOrSuspend evaluates the merge gate once. On pass it takes the
// transition's pass branch (e.g. merging). On fail it compares how long the task
// has been in this state — measured from the audit-recorded entry time, so the
// bound survives suspend/resume and daemon restarts — against the state's timeout:
// past the timeout it escalates; otherwise it returns errSuspended so the drive
// yields its worker and the scheduler re-drives it on a later poll. Status changes
// have no push source, so re-evaluation is scheduler-paced rather than an
// in-process poll loop that would pin the worker slot for the whole wait.
func (e *Engine) evaluateGateOrSuspend(ctx context.Context, task *store.Task, gateT, timeoutT *config.Transition) (next, trigger, result string, err error) {
	verdict, gerr := e.evaluateGate(ctx, task, gateT)
	if gerr != nil {
		return "", "", "", gerr
	}
	if verdict == "pass" {
		return gateT.Branch["pass"], "status.changed", "pass", nil
	}
	d, perr := e.parseDur(timeoutT.When.Timeout)
	if perr != nil {
		return "", "", "", fmt.Errorf("parse timeout %q: %w", timeoutT.When.Timeout, perr)
	}
	entry, ok, eerr := e.store.StateEntryTime(ctx, task.ID, task.CurrentState)
	if eerr != nil {
		return "", "", "", fmt.Errorf("state entry time: %w", eerr)
	}
	// ok is always true in practice: advance() records the approved->blocked_on_gate
	// entry before the state change, and suspend appends no row, so a task parked
	// here always has a genuine entry. If it were somehow missing, keep re-checking
	// the gate (suspend) rather than escalate on an unknown elapsed time.
	if ok && e.now().Sub(entry) >= d {
		e.log.Warn("merge gate timeout", "task", task.ID, "state", task.CurrentState)
		return timeoutT.To, "timeout", "", nil
	}
	return "", "", "", errSuspended
}

// awaitAgentState implements the implementing-state wait: react to agent.done
// (evaluate the gate, branch on pass/fail), alert on agent.blocked, bound how
// long it may stay blocked (policies.blocked_timeout), and escalate on the state
// timeout.
//
// Why blocked needs its own clock: a blocked agent is parked on an interactive
// prompt nobody will answer, so it will never report done — but blocking does not
// change state, and a state timeout is anchored to state *entry*. Without a
// separate bound the only lever is shortening the whole state timeout, which
// would also kill legitimately long implementations. Observed cost: an agent
// blocked 47 minutes before its 60m implementing timeout fired.
func (e *Engine) awaitAgentState(ctx context.Context, task *store.Task, st config.State) (next, trigger, result string, err error) {
	doneT := findEventTransition(st, "agent.done")
	blockedT := findEventTransition(st, "agent.blocked")
	timeoutT := findTimeoutTransition(st)

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel() // reap the Events goroutine on return

	var timer <-chan time.Time
	if timeoutT != nil {
		d, perr := e.parseDur(timeoutT.When.Timeout)
		if perr != nil {
			return "", "", "", fmt.Errorf("parse timeout %q: %w", timeoutT.When.Timeout, perr)
		}
		// Anchor the deadline to the audit-recorded state entry, not to now, so it
		// survives a daemon restart / re-drive (mirrors evaluateGateOrSuspend). A
		// fresh full-duration timer on every entry would let a stuck agent evade
		// the timeout by outliving restarts; a task already past its deadline
		// escalates immediately (remaining clamps to 0).
		remaining := d
		entry, ok, eerr := e.store.StateEntryTime(ctx, task.ID, task.CurrentState)
		if eerr != nil {
			return "", "", "", fmt.Errorf("state entry time: %w", eerr)
		}
		if ok {
			if remaining = entry.Add(d).Sub(e.now()); remaining < 0 {
				remaining = 0
			}
		}
		t := time.NewTimer(remaining)
		defer t.Stop()
		timer = t.C
	}

	// The blocked bound needs somewhere to go — by definition "this state gave
	// up". It used to reuse the state's timeout target, which made it silently
	// INERT in any state that declared no timeout (pr_open, changes_requested):
	// the policy was set, the clock never armed, and nothing said so.
	// escalationTarget always resolves a destination, so the bound now applies
	// wherever it is configured.
	var blockedFor time.Duration
	if e.wf.Policies.BlockedTimeout != "" {
		if blockedFor, err = e.parseDur(e.wf.Policies.BlockedTimeout); err != nil {
			return "", "", "", fmt.Errorf("parse blocked_timeout %q: %w", e.wf.Policies.BlockedTimeout, err)
		}
	}
	// Deliberately a live timer rather than an audit-anchored deadline (unlike the
	// state timeout above): it must measure *continuous* blocking, so any non-blocked
	// event clears it and a transient prompt the agent resolves itself never counts.
	// A daemon restart therefore restarts this clock — acceptable because the
	// audit-anchored state timeout is still the hard backstop, so the worst case is
	// exactly today's behavior.
	var blockedTimer *time.Timer
	var blockedC <-chan time.Time
	clearBlocked := func() {
		if blockedTimer != nil {
			blockedTimer.Stop()
			blockedTimer, blockedC = nil, nil
		}
	}
	defer clearBlocked()

	// The global liveness bound: how long this task may produce NO observable
	// signal at all. Where the state timeout above measures POSITION (time since
	// entering the state), this measures PROGRESS, so it bounds every agent state
	// — including the ones that declare no timeout of their own — without capping
	// how long honest work may take.
	//
	// Deliberately a live per-drive timer rather than an audit-anchored deadline:
	// the question is "has anything happened lately", which has no meaning across
	// a restart. The audit-anchored state timeout and the scheduler's drive
	// deadline remain the hard backstops, so the worst case is exactly the
	// behavior before this bound existed.
	var progressTimer *time.Timer
	var progressC <-chan time.Time
	var lastDigest string
	if e.noProgress > 0 {
		lastDigest = e.paneDigest(ctx, task) // baseline; skipped when the bound is off
		progressTimer = time.NewTimer(e.noProgress)
		progressC = progressTimer.C
		defer progressTimer.Stop()
	}
	resetProgress := func() {
		if progressTimer == nil {
			return
		}
		progressTimer.Stop()
		progressTimer.Reset(e.noProgress)
	}

	events, err := e.backend.Events(waitCtx)
	if err != nil {
		return "", "", "", fmt.Errorf("subscribe to events: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-timer:
			e.log.Warn("state timeout", "task", task.ID, "state", task.CurrentState)
			return timeoutT.To, "timeout", "", nil
		case <-blockedC:
			target, why := e.escalationTarget(task.CurrentState)
			e.log.Warn("blocked timeout", "task", task.ID, "state", task.CurrentState,
				"blocked_for", e.wf.Policies.BlockedTimeout, "to", target, "via", why)
			return target, "blocked_timeout", why, nil

		case <-progressC:
			// The timer expiring is a suspicion, not a verdict. Pane STATUS can sit
			// on "working" for an hour while an agent works steadily, and the event
			// hub broadcasts only diffs — so silence on the event stream is not
			// evidence of a dead agent. Confirm against the pane's own bytes before
			// giving up on it, which is what keeps a legitimately slow agent from
			// being mistaken for a wedged one.
			moved, digest := e.paneMoved(ctx, task, lastDigest)
			if moved {
				lastDigest = digest
				resetProgress()
				continue
			}
			target, why := e.escalationTarget(task.CurrentState)
			e.log.Warn("no progress", "task", task.ID, "state", task.CurrentState,
				"window", e.noProgress, "to", target, "via", why)
			return target, "no_progress", why, nil
		case ev, ok := <-events:
			if !ok {
				return "", "", "", fmt.Errorf("event stream closed before agent settled")
			}
			if ev.PaneID != task.PaneID {
				continue
			}
			// The hub broadcasts only diffs, so an event for our pane IS a change:
			// the agent did something observable. Reset before dispatching, so
			// every arm below counts, not just the ones that return.
			resetProgress()
			e.recordAgentStatus(ctx, task, string(ev.State))
			switch ev.State {
			case exec.StateDone:
				verdict, derr := e.evaluateDone(ctx, task, doneT)
				if derr != nil {
					return "", "", "", derr
				}
				return doneT.Branch[verdict], "agent.done", verdict, nil
			case exec.StateIdle:
				// Claude Code commonly lands at an idle prompt when it finishes
				// instead of reporting "done": herdr's live_prompt_box rule reads
				// any text left in the prompt box as idle. Without handling idle,
				// a finished agent is indistinguishable from a slow one and the
				// state rides to its timeout — observed on a real run, where an
				// implementer had already opened its PR and would still have
				// escalated at 45m.
				//
				// In both arms the authoritative *artifact* decides, never the pane
				// status. An idle that has produced nothing keeps waiting, so the
				// "dead pane wrote nothing" case still surfaces on the timeout
				// rather than being masked here.
				if dec := decisionRefOf(doneT); dec != "" {
					// Decision state (intake, pr_open): the verdict file is the artifact.
					v, found, derr := e.tryDecisionVerdict(task, dec)
					if derr != nil {
						return "", "", "", derr
					}
					if found {
						return doneT.Branch[v], "agent.done", v, nil
					}
				} else if len(doneT.GateRefs()) > 0 && blockedTimer == nil {
					// Gate state (implementing, changes_requested): GitHub is the
					// artifact. Only a pass advances. A fail means the agent has not
					// opened its PR yet — not a reason to escalate while the state
					// timeout is still running — so we keep waiting exactly as before.
					// A transient gh error is logged and waited through rather than
					// failing the drive: idle fires once per status change (the event
					// hub broadcasts only diffs), and the timeout still bounds us.
					//
					// The blockedTimer guard keeps this from reopening the hole the
					// blocked bound exists to close: an agent parked on an
					// unanswerable prompt can report idle rather than blocked, so
					// inside an open blocked window idle stays non-recovery. Only
					// *working* clears that window, so an agent that was blocked,
					// recovered, then finished still takes the shortcut.
					v, gerr := e.evaluateGate(ctx, task, doneT)
					if gerr != nil {
						e.log.Warn("idle gate check failed; still waiting",
							"task", task.ID, "state", task.CurrentState, "err", gerr)
					} else if v == "pass" {
						return doneT.Branch[v], "agent.done", v, nil
					}
				}
			case exec.StateBlocked:
				if blockedT != nil && blockedT.Action != nil {
					e.alert(ctx, task, blockedT.Action.Alert)
				}
				// Stay in the state and keep waiting, but start the clock on the
				// first blocked event so a permanently-parked agent can't sit here
				// until the state timeout. Repeat blocked events must not restart
				// it, or a pane that re-reports blocked would extend forever.
				if blockedFor > 0 && blockedTimer == nil {
					blockedTimer = time.NewTimer(blockedFor)
					blockedC = blockedTimer.C
				}
			default:
				// Only *working* counts as recovery, so the bound measures continuous
				// blocking. Idle is deliberately not recovery: an agent parked at an
				// unanswerable prompt can read as idle, which is precisely the case
				// this bound exists to catch — clearing on it would reopen the hole.
				if ev.State == exec.StateWorking {
					clearBlocked()
				}
			}
		}
	}
}

// progressReadLines is how much pane tail the confirmation read hashes. Wide
// enough that any real agent activity — a tool call, a diff, a status line —
// lands inside it, cheap enough to run once per no-progress window.
const progressReadLines = 80

// paneMoved reports whether the agent pane's tail has changed since prev,
// returning the new digest. A read failure counts as movement: a herdr blip must
// never be the thing that escalates a task. That deliberately means a persistently
// unreadable pane keeps this bound from firing — the state timeout and the
// scheduler's drive deadline are the backstops for that case.
func (e *Engine) paneMoved(ctx context.Context, task *store.Task, prev string) (bool, string) {
	if task.PaneID == "" {
		return true, prev
	}
	out, err := e.backend.Read(ctx, exec.Handle{PaneID: task.PaneID}, progressReadLines)
	if err != nil {
		e.log.Warn("progress check: pane read failed; assuming progress",
			"task", task.ID, "pane", task.PaneID, "err", err)
		return true, prev
	}
	d := digestOf(out)
	return d != prev, d
}

// paneDigest takes the baseline the first confirmation read compares against.
// An unreadable pane yields "", which simply costs one extra window before the
// bound can fire.
func (e *Engine) paneDigest(ctx context.Context, task *store.Task) string {
	if task.PaneID == "" {
		return ""
	}
	out, err := e.backend.Read(ctx, exec.Handle{PaneID: task.PaneID}, progressReadLines)
	if err != nil {
		return ""
	}
	return digestOf(out)
}

// digestOf hashes pane output so a whole window's worth of tail is held as 32
// bytes rather than kilobytes of retained terminal text.
func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// decisionRefOf returns a transition's decision name, or "" if the transition is
// nil or gate-based. Lets the wait loop ask "is this a decision state?" without a
// nil-deref when an agent reports done/idle in a state that has no such transition.
func decisionRefOf(t *config.Transition) string {
	if t == nil {
		return ""
	}
	return t.DecisionRef()
}

// evaluateDone resolves the outcome of an agent.done transition: a decision
// verdict (judgment, read from the reviewer) or a gate result (authoritative).
func (e *Engine) evaluateDone(ctx context.Context, task *store.Task, t *config.Transition) (string, error) {
	if dec := t.DecisionRef(); dec != "" {
		return e.evaluateDecision(task, dec)
	}
	if len(t.GateRefs()) > 0 {
		return e.evaluateGate(ctx, task, t)
	}
	return "", fmt.Errorf("state %q: agent.done transition has neither a decision nor a gate to evaluate", task.CurrentState)
}

// evaluateGate evaluates all gates referenced by a transition over authoritative
// sources, returning "pass" iff every gate passes, else "fail". The PR-status
// gates (checks/reviews/mergeable) share one PRStatus read so a single evaluation
// sees a consistent snapshot.
func (e *Engine) evaluateGate(ctx context.Context, task *store.Task, t *config.Transition) (string, error) {
	var status *github.PRStatus
	for _, gname := range t.GateRefs() {
		g, ok := e.wf.Gates[gname]
		if !ok {
			return "", fmt.Errorf("gate %q not declared", gname)
		}
		// The merge gates read PR status; fetch it once, lazily.
		if g.Type != "github_pr" && status == nil {
			if task.PRNumber == nil {
				return "fail", nil // no PR yet => merge gates cannot pass
			}
			s, err := e.gh.PRStatus(ctx, e.repoDir, *task.PRNumber)
			if err != nil {
				return "", fmt.Errorf("gate %q: read PR status: %w", gname, err)
			}
			status = s
		}
		pass, err := e.gatePass(ctx, task, gname, g, status)
		if err != nil {
			return "", err
		}
		if !pass {
			return "fail", nil
		}
	}
	return "pass", nil
}

// gatePass evaluates one gate against the authoritative source: github_pr via a
// fresh PR lookup, the merge gates against the shared PRStatus snapshot.
func (e *Engine) gatePass(ctx context.Context, task *store.Task, name string, g config.Gate, status *github.PRStatus) (bool, error) {
	switch g.Type {
	case "github_pr":
		pr, err := e.gh.FindPR(ctx, e.repoDir, task.Branch)
		if err != nil {
			return false, fmt.Errorf("gate %q: %w", name, err)
		}
		if pr == nil {
			return false, nil
		}
		n := pr.Number
		task.PRNumber = &n
		e.log.Info("gate pass: PR detected", "task", task.ID, "gate", name, "pr", n)
		return true, nil
	case "github_checks":
		return status.ChecksGreen(), nil
	case "github_reviews":
		return status.ApprovedReviews >= g.MinApproved, nil
	case "github_commits":
		// The artifact-movement question: has anything been committed since the
		// task entered this state? In changes_requested the previous gate
		// (pr_exists) was tautological — the PR was opened in the round that put
		// the task here, so it could only pass, whether or not the implementer
		// addressed a single line of the review.
		if task.StateEntryHead == "" {
			// No baseline: no PR when the state was entered, a status read that
			// failed, or a task predating the column. We cannot answer, so we
			// degrade to the previous behavior (pass) rather than escalating a task
			// on a question we never captured the input for. Logged, never silent.
			e.log.Warn("gate: no head baseline recorded for this state; cannot verify movement, passing",
				"task", task.ID, "gate", name, "state", task.CurrentState)
			return true, nil
		}
		if status.HeadSHA == "" {
			return false, fmt.Errorf("gate %q: PR status carries no head SHA", name)
		}
		moved := status.HeadSHA != task.StateEntryHead
		if !moved {
			e.log.Info("gate fail: head has not moved since state entry",
				"task", task.ID, "gate", name, "state", task.CurrentState, "head", status.HeadSHA)
		}
		return moved, nil
	case "github_mergeable":
		// `require: clean` demands GitHub's CLEAN mergeStateStatus (no conflicts
		// AND up to date AND not blocked), which is stricter than mere
		// conflict-freeness. Without it, fall back to the conflict check.
		if g.Require == "clean" {
			return status.MergeStateStatus == "CLEAN", nil
		}
		return status.Mergeable == "MERGEABLE", nil
	default:
		return false, fmt.Errorf("gate %q: type %q not supported", name, g.Type)
	}
}

// launchArgs returns the agent launch argv, scoping tools when the role declares
// allowed_tools. Tools are appended as separate args to match claude's variadic
// --allowedTools <tools...> flag (translation is claude-targeted today; our only
// launcher; matched by basename so an absolute path still counts). Config
// validation rejects shell-unsafe tool tokens, because the backend delivers this
// argv space-joined into the pane's shell (see exec.Herdr.Spawn); arg-scoped
// specs with spaces/globs/parens would need shell quoting at delivery, which is
// future work.
func launchArgs(r config.Role) []string {
	args := append([]string(nil), r.Launch...)
	if len(r.AllowedTools) > 0 && len(r.Launch) > 0 && filepath.Base(r.Launch[0]) == "claude" {
		args = append(args, "--allowedTools")
		args = append(args, r.AllowedTools...)
	}
	return args
}

// spawn launches the agent for a state's entry: build the role-specific task file
// + single-line kickoff and start the agent. It reuses an existing pane ONLY when
// re-entering the same state its agent was spawned for (crash recovery) — entering
// a new state always spawns a fresh agent for that state's role, so the reviewer
// at pr_open is not mistaken for the still-labelled implementer workspace.
func (e *Engine) spawn(ctx context.Context, task *store.Task, role string, st config.State) error {
	if task.PaneID != "" && task.PaneSpawnState == task.CurrentState {
		h, ok, err := e.backend.Resolve(ctx, task.ID)
		if err != nil {
			// A transient Resolve failure must NOT fall through to a fresh spawn:
			// backend.Spawn force-removes the worktree, which would destroy a
			// still-live agent's uncommitted work. Abort and let the caller retry.
			return fmt.Errorf("resolve existing agent for %s (refusing to re-spawn over a possibly-live worktree): %w", task.ID, err)
		}
		if ok {
			task.PaneID = h.PaneID
			e.log.Info("reusing live agent", "task", task.ID, "pane", h.PaneID, "state", task.CurrentState)
			return nil
		}
		// err == nil && !ok: the prior agent is genuinely gone — safe to spawn fresh.
	}

	r, ok := e.wf.Roles[role]
	if !ok {
		return fmt.Errorf("role %q not declared", role)
	}

	taskFile, kickoff, err := e.agentTask(ctx, task, st, r)
	if err != nil {
		return err
	}

	sp := exec.Spawn{
		TaskID:   task.ID,
		Role:     role,
		Branch:   task.Branch,
		RepoDir:  e.repoDir,
		Base:     e.base,
		TaskFile: taskFile,
		Launch:   launchArgs(r),
		Kickoff:  kickoff,
		// A task with a detected PR is being re-spawned (reviewer/resume): keep
		// its branch so the PR's commits survive (see exec.Spawn.PreserveBranch).
		PreserveBranch: task.PRNumber != nil,
	}
	h, err := e.backend.Spawn(ctx, sp)
	if err != nil {
		return fmt.Errorf("spawn %s: %w", role, err)
	}
	task.PaneID = h.PaneID
	task.PaneSpawnState = task.CurrentState
	if err := e.store.UpdateTask(ctx, task); err != nil {
		return fmt.Errorf("persist pane id: %w", err)
	}
	e.log.Info("agent spawned", "task", task.ID, "role", role, "pane", h.PaneID, "state", task.CurrentState)
	return nil
}

// agentTask builds the context file + single-line kickoff for a spawned agent. A
// state whose agent.done transition evaluates a decision gets a triage task
// (rubric + issue, no PR) when no PR exists yet, or a reviewer task (rubric + PR
// pointer) once a PR is present — each produces a verdict file. Otherwise the
// agent is an implementer and gets the issue.
func (e *Engine) agentTask(ctx context.Context, task *store.Task, st config.State, r config.Role) (taskFile, kickoff string, err error) {
	if dec := decisionForState(st); dec != "" {
		if task.PRNumber == nil {
			return e.triageTask(ctx, task, dec) // pipeline-entry decision: rubric + issue
		}
		return e.reviewerTask(task, dec) // review decision: rubric + PR pointer
	}
	if st.Entry != nil && st.Entry.Resume != "" {
		return e.feedbackTask(task, st.Entry.With)
	}
	taskFile, err = e.writeTaskFile(ctx, task)
	if err != nil {
		return "", "", err
	}
	return taskFile, e.kickoff(r, task, taskFile), nil
}

// writeTaskFile fetches the issue and writes its title+body to a context file.
// The multi-line body is NEVER sent through the pane — only the kickoff is.
func (e *Engine) writeTaskFile(ctx context.Context, task *store.Task) (string, error) {
	issue, err := e.gh.Issue(ctx, e.repoDir, task.Issue)
	if err != nil {
		return "", fmt.Errorf("fetch issue %d: %w", task.Issue, err)
	}
	path := filepath.Join(e.taskDir, "task-"+task.ID+".md")
	body := fmt.Sprintf("# %s\n\n%s\n", issue.Title, issue.Body)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write task file: %w", err)
	}
	return path, nil
}

// kickoff builds the single-line kickoff that points the agent at the task file.
func (e *Engine) kickoff(r config.Role, task *store.Task, taskFile string) string {
	if r.Kickoff != "" {
		return r.Kickoff
	}
	return fmt.Sprintf(
		"Read the task in %s and implement it on this branch (%s). Then commit, run 'git push -u origin %s', and open a PR with 'gh pr create --fill --base %s'. Stop when the PR is open.",
		taskFile, task.Branch, task.Branch, e.base)
}

// reconcile re-resolves a task's volatile pane and short-circuits to the goal if
// GitHub already shows the artifact for an implementing task.
func (e *Engine) reconcile(ctx context.Context, task *store.Task) error {
	// Only a state that runs an agent has a volatile pane worth re-resolving. A
	// gate-wait state (blocked_on_gate) has no live agent, and the daemon re-drives
	// such a task every poll to re-check the merge gate — so that resume, and the
	// escalation timeout, must not depend on herdr (the gate read and escalation
	// touch only GitHub). Skipping the Resolve keeps a herdr outage from stalling
	// the gate and its safety timeout, and avoids per-poll shell-outs for no reason.
	if !stateHasAgent(e.wf.States[task.CurrentState]) {
		return nil
	}
	h, ok, err := e.backend.Resolve(ctx, task.ID)
	if err != nil {
		// Don't clear the volatile pane on a transient failure: a cleared pane
		// would let a later spawn re-launch over a possibly-live worktree. Let
		// the caller log and skip/retry this task.
		return fmt.Errorf("reconcile resolve %s: %w", task.ID, err)
	}
	if ok {
		task.PaneID = h.PaneID
	} else {
		task.PaneID = "" // the prior agent is genuinely gone
	}
	if task.CurrentState == "implementing" {
		pr, err := e.gh.FindPR(ctx, e.repoDir, task.Branch)
		if err != nil {
			return fmt.Errorf("reconcile FindPR: %w", err)
		}
		if pr != nil {
			n := pr.Number
			task.PRNumber = &n
			// The implementing agent.done gate already passed; advance as if it
			// fired (to pr_open), derived from the config rather than the goal.
			target := e.doneBranchTarget("implementing", "pass")
			if target == "" {
				return fmt.Errorf("reconcile: implementing has no agent.done pass branch")
			}
			return e.advance(ctx, task, target, "reconcile", "pass")
		}
	}
	return e.store.UpdateTask(ctx, task)
}

// doneBranchTarget returns the state a named state's agent.done transition
// branches to for a given verdict (empty if absent).
func (e *Engine) doneBranchTarget(stateName, verdict string) string {
	st, ok := e.wf.States[stateName]
	if !ok {
		return ""
	}
	t := findEventTransition(st, "agent.done")
	if t == nil {
		return ""
	}
	return t.Branch[verdict]
}

// advance records the transition (audit + state change) and persists the task.
// This is the single mutation point for task state.
func (e *Engine) advance(ctx context.Context, task *store.Task, next, trigger, result string) error {
	from := task.CurrentState
	if err := e.store.AppendAudit(ctx, store.AuditEntry{
		TaskID:    task.ID,
		FromState: from,
		ToState:   next,
		Trigger:   trigger,
		Result:    result,
	}); err != nil {
		return fmt.Errorf("audit %s->%s: %w", from, next, err)
	}
	task.CurrentState = next
	task.StateEnteredAt = e.now()
	task.StateEntryHead = e.headBaseline(ctx, task)
	if err := e.store.UpdateTask(ctx, task); err != nil {
		return fmt.Errorf("persist transition %s->%s: %w", from, next, err)
	}
	e.log.Info("transition", "task", task.ID, "from", from, "to", next, "trigger", trigger, "result", result)
	e.maybeDrainLabel(ctx, task)
	return nil
}

// maybeDrainLabel removes the source label once a task settles, so the label
// stops meaning "the backlog plus everything ever completed".
//
// The daemon also drains it from doneChecker.done, but that path is only reached
// for an issue ListIssues just returned — and `gh issue list` defaults to open
// issues, while the success path closes the issue as part of the merge. So for
// the one outcome the pipeline exists to produce, the poll-time drain can never
// run. Draining here instead keys on the settle itself, which every terminal
// reaches through advance: merged, closed, escalated, and the detached writes
// that settle an operator cancel or a reaped drive.
//
// Best-effort and deliberately after the state write: the transition is the
// durable fact, and a label left behind must never fail a drive or re-run a
// terminal transition. The poll-time drain remains as the idempotent backstop
// for tasks that settle while their issue is still open.
func (e *Engine) maybeDrainLabel(ctx context.Context, task *store.Task) {
	if !e.isSettled(task.CurrentState) {
		return
	}
	label := e.wf.SourceLabel()
	if label == "" {
		return // no labeled source: nothing to drain
	}
	if err := e.gh.RemoveLabel(ctx, e.repoDir, task.Issue, label); err != nil {
		e.log.Warn("remove source label after settle failed", "task", task.ID,
			"issue", task.Issue, "label", label, "err", err)
	}
}

// recordAgentStatus persists the agent's last observed pane status so the
// supervision surface can answer "is this task actually moving?".
//
// It exists because state is not liveness: blocking does not change state, so a
// task parked on an unanswerable permission prompt is byte-identical over the
// wire to one making progress. Real runs have lost 45+ minutes to exactly that,
// with the only signal being a state that had not changed for suspiciously long.
//
// Writes only on a *change* of status, so an agent flipping between the same two
// states does not rewrite the row on every event; AgentStatusAt therefore marks
// when the current status began, which is the number an operator wants ("idle
// for 40 minutes"), not when it was last re-observed.
//
// Best-effort: a diagnostic must never fail a drive.
func (e *Engine) recordAgentStatus(ctx context.Context, task *store.Task, status string) {
	if status == "" || task.AgentStatus == status {
		return
	}
	task.AgentStatus = status
	task.AgentStatusAt = e.now()
	if err := e.store.UpdateTask(ctx, task); err != nil {
		e.log.Warn("record agent status failed", "task", task.ID, "status", status, "err", err)
	}
}

// isSettled reports whether a state means the task is finished for good: any
// terminal, plus the daemon-owned cancel terminal. Deliberately narrower than
// isHalt, which also stops on the goal state (a mid-pipeline halt used by tests
// and the one-shot run) and on the dry-run merging halt — neither of which has
// settled anything.
func (e *Engine) isSettled(state string) bool {
	return state == CancelState || e.isTerminal(state)
}

// headBaseline captures the PR head SHA the task is entering its new state with,
// so a github_commits gate can later ask whether anything was committed since.
//
// It is only read for states that actually evaluate such a gate: every other
// transition — including the detached, time-bounded writes that settle a cancel
// or a reaped drive — must not pay for a GitHub round trip it will never use.
//
// Best-effort by design. A read failure yields "" (unknown), which the gate
// treats as "cannot answer" and passes, degrading to the pre-gate behavior rather
// than escalating a task because GitHub was briefly unreachable.
func (e *Engine) headBaseline(ctx context.Context, task *store.Task) string {
	if task.PRNumber == nil || !e.stateGatesOnCommits(task.CurrentState) {
		return ""
	}
	status, err := e.gh.PRStatus(ctx, e.repoDir, *task.PRNumber)
	if err != nil {
		e.log.Warn("could not record head baseline for this state; commit-movement gate will pass",
			"task", task.ID, "state", task.CurrentState, "err", err)
		return ""
	}
	return status.HeadSHA
}

// stateGatesOnCommits reports whether any transition out of state evaluates a
// github_commits gate — i.e. whether entering it needs a head baseline.
func (e *Engine) stateGatesOnCommits(state string) bool {
	for _, t := range e.wf.States[state].Transitions {
		for _, gname := range t.GateRefs() {
			if e.wf.Gates[gname].Type == "github_commits" {
				return true
			}
		}
	}
	return false
}

// alert records an agent.blocked alert as an audit row without changing state.
func (e *Engine) alert(ctx context.Context, task *store.Task, msg string) {
	e.log.Warn("agent blocked", "task", task.ID, "state", task.CurrentState, "alert", msg)
	if err := e.store.AppendAudit(ctx, store.AuditEntry{
		TaskID:    task.ID,
		FromState: task.CurrentState,
		ToState:   task.CurrentState,
		Trigger:   "agent.blocked",
		Result:    "alert:" + msg,
	}); err != nil {
		e.log.Warn("failed to record alert", "task", task.ID, "err", err)
	}
	e.notify(ctx, notify.Event{
		TaskID: task.ID,
		Issue:  task.Issue,
		State:  task.CurrentState,
		Kind:   "alert",
		Detail: msg,
	})
}

// notify forwards an out-of-band event, swallowing any delivery error: a
// notifier must never fail or block the drive loop.
func (e *Engine) notify(ctx context.Context, ev notify.Event) {
	if err := e.notifier.Notify(ctx, ev); err != nil {
		e.log.Warn("notify failed", "task", ev.TaskID, "kind", ev.Kind, "err", err)
	}
}

// notifyTerminalAlert fires an "escalated" event when a task halts at a terminal
// state flagged alert (the escalated state); other halts (goal, plain terminals)
// are silent.
func (e *Engine) notifyTerminalAlert(ctx context.Context, task *store.Task) {
	if !e.wf.States[task.CurrentState].Alert {
		return
	}
	ev := notify.Event{
		TaskID: task.ID,
		Issue:  task.Issue,
		State:  task.CurrentState,
		Kind:   "escalated",
	}
	e.explain(ctx, task, &ev)
	e.notify(ctx, ev)
}

// alertHistory is how many audit rows an escalation carries. Enough to show how
// the task arrived (the transition that escalated it, plus the couple before),
// few enough that a webhook payload stays readable.
const alertHistory = 3

// alertPaneLines is how much agent output an escalation carries. Sized to show a
// permission prompt and the command that triggered it without shipping a screen.
const alertPaneLines = 25

// explain fills in the diagnosis an escalation should arrive with, so the
// recipient does not have to reconstruct it from get_audit plus a pane read.
//
// Every part is best-effort and independently optional: an escalation that
// reaches a human missing its pane tail is still useful, one that never arrives
// because a diagnostic failed is not. Nothing here can fail the drive.
func (e *Engine) explain(ctx context.Context, task *store.Task, ev *notify.Event) {
	if aud, err := e.store.Audit(ctx, task.ID); err == nil {
		// Most recent first: the escalating transition is the headline.
		for i := len(aud) - 1; i >= 0 && len(ev.Recent) < alertHistory; i-- {
			ev.Recent = append(ev.Recent, notify.Transition{
				From: aud[i].FromState, To: aud[i].ToState,
				Trigger: aud[i].Trigger, Result: aud[i].Result,
			})
		}
		if len(ev.Recent) > 0 {
			ev.Cause = ev.Recent[0].Trigger
			if ev.Recent[0].Result != "" {
				ev.Cause = ev.Recent[0].Trigger + ":" + ev.Recent[0].Result
			}
		}
	} else {
		e.log.Warn("escalation: could not read audit trail", "task", task.ID, "err", err)
	}

	if task.PaneID != "" {
		if tail, err := e.backend.Read(ctx, exec.Handle{PaneID: task.PaneID}, alertPaneLines); err == nil {
			ev.PaneTail = tail
		} else {
			e.log.Warn("escalation: could not read agent pane", "task", task.ID, "err", err)
		}
	}
	ev.Recommended = recommendFor(ev.Cause)
}

// recommendFor maps an escalation cause to the action a human should take. The
// wording matches the diagnosis table in the operate-orchestrator skill, so the
// alert and the runbook cannot drift into saying different things.
//
// A settled task can never be re-driven, so no recommendation here is ever
// "retry it" — the honest advice is always to fix the cause and open a fresh
// issue.
func recommendFor(cause string) string {
	switch {
	case strings.HasPrefix(cause, "blocked_timeout"):
		return "The agent sat blocked on an interactive prompt. Read its pane (read-only) to see which one, add the tool to permissions.allow, then open a fresh issue — a settled task cannot be re-driven."
	case strings.HasPrefix(cause, "no_progress"):
		return "The agent produced no observable output for the no_progress window. Read its pane (read-only): a prompt means fix the allow-list; a dead pane means check herdr. Then open a fresh issue."
	case strings.HasPrefix(cause, "drive_deadline"):
		return "The drive outlived its wall-clock ceiling, so it was reaped from outside. Check for a wedged subprocess or an over-scoped task, then open a fresh issue."
	case strings.HasPrefix(cause, "timeout"):
		return "The agent ran past its state timeout. Read its pane (read-only) to tell a slow task from a permission wedge, then either raise the state's timeout or narrow the issue, and open a fresh one."
	case strings.HasPrefix(cause, "retry_exhausted"):
		return "The reviewer and implementer disagreed until the retry cap ran out. Read the PR's review history; this usually needs a human to settle the disagreement or clarify the issue."
	case strings.HasPrefix(cause, "agent.done:fail"), strings.HasPrefix(cause, "agent.done"):
		return "The agent reported done but the gate found no artifact — typically no PR, or no new commits since the review. Check the agent's worktree and the issue's clarity."
	case cause == "":
		return "Read the task's audit trail (get_audit) and the agent's pane to determine why it escalated."
	default:
		return "Read the task's audit trail (get_audit) and the agent's pane, then act on the cause above."
	}
}

// checkRetryCap enforces a state's retry cap on entry: it increments the
// per-state counter and, once the cap is exceeded, returns the state's
// retry_exhausted target. A capped state with no retry_exhausted transition is a
// config error. The incremented count is persisted by the spawn/advance that
// follows.
func (e *Engine) checkRetryCap(task *store.Task, st config.State) (target string, exhausted bool, err error) {
	limit, ok := e.wf.Policies.RetryCaps[task.CurrentState]
	if !ok {
		return "", false, nil
	}
	if task.RetryCounts == nil {
		task.RetryCounts = map[string]int{}
	}
	task.RetryCounts[task.CurrentState]++
	if task.RetryCounts[task.CurrentState] <= limit {
		return "", false, nil
	}
	rt := findEventTransition(st, "retry_exhausted")
	if rt == nil {
		return "", true, fmt.Errorf("state %q exceeded retry cap %d but declares no retry_exhausted transition", task.CurrentState, limit)
	}
	return rt.To, true, nil
}

func (e *Engine) isHalt(state string) bool {
	return state == e.goal || state == CancelState || e.isTerminal(state)
}

// settleCancelled records an operator cancel as a terminal transition to
// CancelState. The drive's context is already cancelled and the store's writes
// are ctx-aware, so it runs on a context detached from that cancellation (with a
// bound); otherwise the settle write would itself fail with context.Canceled and
// the cancel would not stick. The worktree is deliberately NOT torn down: unlike
// an automated no-PR terminal, an operator cancel is a human intervening on a
// possibly-runaway agent who may want to inspect its uncommitted work.
func (e *Engine) settleCancelled(ctx context.Context, task *store.Task) (string, error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if err := e.advance(sctx, task, CancelState, "operator.cancel", ""); err != nil {
		return task.CurrentState, fmt.Errorf("settle cancelled: %w", err)
	}
	e.log.Info("task cancelled by operator", "task", task.ID)
	return CancelState, nil
}

func (e *Engine) isTerminal(state string) bool {
	st, ok := e.wf.States[state]
	return ok && st.Terminal != ""
}

// maybeCleanup tears down a settled task's isolated worktree + herdr workspace when
// it halts at a terminal state with no PR. This covers every no-PR terminal: a
// triage reject (-> closed), an intake needs_human (-> escalated), and a failed
// implementation that escalates before opening a PR (-> escalated) — each of which
// would otherwise leave a wt-issue-<n> worktree and workspace registered. Terminal
// states that produced a PR keep their branch/PR on GitHub (a human may still want
// the local worktree), and the dry-run `merging` halt is not terminal; both are
// left alone. Cleanup is best-effort: a failure is logged and never fails the drive.
//
// Only called on the drive that actually transitions into the terminal (see the
// `transitioned` guard at the halt site), so a re-run of an already-settled task
// does not repeat the teardown.
func (e *Engine) maybeCleanup(ctx context.Context, task *store.Task) {
	if task.PRNumber != nil || !e.isTerminal(task.CurrentState) {
		return
	}
	// A needs_human escalation (an alerting terminal) can hold uncommitted work a
	// human wants to inspect — force-removing the worktree here is the data loss
	// that destroyed a completed-but-uncommitted task. Preserve it (mirrors
	// settleCancelled); only a clean reject (closed) is torn down.
	if e.wf.States[task.CurrentState].Alert {
		return
	}
	if err := e.backend.Cleanup(ctx, task.ID); err != nil {
		e.log.Warn("cleanup failed", "task", task.ID, "state", task.CurrentState, "err", err)
	}
}

// --- small helpers ---

func TaskID(issue int) string     { return fmt.Sprintf("issue-%d", issue) }
func branchName(issue int) string { return fmt.Sprintf("agent/issue-%d", issue) }

func prNum(t *store.Task) int {
	if t.PRNumber == nil {
		return 0
	}
	return *t.PRNumber
}

// stateHasAgent reports whether a state runs an agent (a spawn or resume entry),
// i.e. whether it has a volatile herdr pane worth re-resolving on reconcile.
func stateHasAgent(st config.State) bool {
	return st.Entry != nil && (st.Entry.Spawn != "" || st.Entry.Resume != "")
}

func findEventTransition(st config.State, event string) *config.Transition {
	for i := range st.Transitions {
		if st.Transitions[i].When.Event == event {
			return &st.Transitions[i]
		}
	}
	return nil
}

func findTimeoutTransition(st config.State) *config.Transition {
	for i := range st.Transitions {
		if st.Transitions[i].When.IsTimeout() {
			return &st.Transitions[i]
		}
	}
	return nil
}
