// Package engine executes a validated workflow as a deterministic state graph.
//
// Phase 1 drives only the proven slice: queued -> implementing ->
// (agent.done -> gate pr_exists -> pr_open | escalated), plus the agent.blocked
// alert and the implementing timeout -> escalated edge. It interprets the full
// default-pipeline.yaml but halts at the goal state (pr_open); review, merge,
// and triage are out of scope and surface as explicit "not implemented" errors
// if ever reached.
//
// The engine is the single writer of task state: all store writes happen on the
// goroutine that runs Run/Recover. GitHub is authoritative for artifacts; an
// agent's "done" is only a trigger to go check GitHub.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/notify"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// autoFiredEvents are events with no real source in Phase 1 (the scheduler is
// stubbed); the engine fires them immediately on entering the state.
var autoFiredEvents = map[string]bool{"scheduled": true}

// Config wires the engine's dependencies and tunables.
type Config struct {
	Workflow *config.Workflow
	Backend  exec.ExecutionBackend
	GitHub   github.Client
	Store    *store.Store

	// WorkflowSource is the raw config bytes snapshotted onto each new task, so
	// recovery resumes against the graph the task started under. Empty => no
	// snapshot (recovery falls back to the current --config).
	WorkflowSource []byte

	RepoDir   string // local checkout (absolute) where git/gh run
	Base      string // base branch, e.g. "main"
	Repo      string // owner/name slug recorded on the task
	ConfigDir string // dir of the workflow config; decision rubric paths resolve against it

	// Optional; sensible defaults applied by New.
	TaskDir      string                              // where task files are written; default os.TempDir()
	Goal         string                              // halt-on-enter success state; default "pr_open"
	StartState   string                              // where Phase 1 enqueues issues; default "queued"
	DurationFunc func(string) (time.Duration, error) // default time.ParseDuration
	// StatusPollInterval is how often the merge gate is re-evaluated while waiting
	// for CI/reviews/mergeability; default 15s.
	StatusPollInterval time.Duration
	Logger             *slog.Logger
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
	statusPoll          time.Duration
	log                 *slog.Logger
	notifier            notify.Notifier
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
		statusPoll:     c.StatusPollInterval,
		log:            c.Logger,
		notifier:       c.Notifier,
	}
	if e.taskDir == "" {
		e.taskDir = os.TempDir()
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
	if e.statusPoll == 0 {
		e.statusPoll = 15 * time.Second
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
func (e *Engine) Run(ctx context.Context, issue int) (string, error) {
	task, created, err := e.ensureTask(ctx, issue)
	if err != nil {
		return "", err
	}
	if !created {
		if err := e.reconcile(ctx, task); err != nil {
			return task.CurrentState, err
		}
	}
	return e.drive(ctx, task)
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
		// Drive the task against the graph it started under (its snapshot), never a
		// possibly-edited current --config. Re-validating via config.Parse keeps
		// recovery fail-closed: a snapshot that no longer satisfies the invariants
		// is skipped, not silently run. An empty snapshot (legacy row) resumes
		// against the current --config, preserving pre-snapshot behavior.
		eng := e
		if task.WorkflowSnapshot != "" {
			wf, _, perr := config.Parse([]byte(task.WorkflowSnapshot))
			if perr != nil {
				e.log.Warn("recover: task snapshot invalid; skipping (fix or migrate)", "task", task.ID, "err", perr)
				continue
			}
			eng = e.cloneWithWorkflow(wf)
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
	return &c
}

// ensureTask loads an existing task or creates a fresh one at the start state.
// The bool return reports whether a new task was created.
func (e *Engine) ensureTask(ctx context.Context, issue int) (*store.Task, bool, error) {
	id := taskID(issue)
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

// drive runs the interpreter loop until a halt state (goal or terminal).
func (e *Engine) drive(ctx context.Context, task *store.Task) (string, error) {
	// transitioned guards the escalated notification: fire only when this drive
	// actually moved the task into the alert terminal state, not when it was
	// re-entered already there (a re-run of an escalated issue must stay quiet).
	transitioned := false
	for {
		if e.isHalt(task.CurrentState) {
			if transitioned {
				e.notifyTerminalAlert(ctx, task)
			}
			e.log.Info("halt", "task", task.ID, "state", task.CurrentState, "pr", prNum(task))
			return task.CurrentState, nil
		}
		next, trigger, result, err := e.runState(ctx, task)
		if err != nil {
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
	// timeout (blocked_on_gate) polls until the gate passes or it gives up; one
	// without (approved) checks once on entry and branches.
	if sct := findEventTransition(st, "status.changed"); sct != nil {
		if timeoutT := findTimeoutTransition(st); timeoutT != nil {
			return e.awaitStatusGate(ctx, task, sct, timeoutT)
		}
		verdict, err := e.evaluateGate(ctx, task, sct)
		if err != nil {
			return "", "", "", err
		}
		return sct.Branch[verdict], "status.changed", verdict, nil
	}

	return "", "", "", fmt.Errorf("state %q: no supported trigger (no agent.done or status.changed transition)", name)
}

// awaitStatusGate polls the merge gate on an interval until it passes (taking the
// transition's pass branch, e.g. merging) or the state's timeout elapses (the
// timeout target, e.g. escalated). status changes have no push source in Phase
// 2a, so the engine polls the authoritative GitHub status.
func (e *Engine) awaitStatusGate(ctx context.Context, task *store.Task, gateT, timeoutT *config.Transition) (next, trigger, result string, err error) {
	d, perr := e.parseDur(timeoutT.When.Timeout)
	if perr != nil {
		return "", "", "", fmt.Errorf("parse timeout %q: %w", timeoutT.When.Timeout, perr)
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()

	for {
		verdict, gerr := e.evaluateGate(ctx, task, gateT)
		if gerr != nil {
			return "", "", "", gerr
		}
		if verdict == "pass" {
			return gateT.Branch["pass"], "status.changed", "pass", nil
		}
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-deadline.C:
			e.log.Warn("merge gate timeout", "task", task.ID, "state", task.CurrentState)
			return timeoutT.To, "timeout", "", nil
		case <-time.After(e.statusPoll):
			// re-poll
		}
	}
}

// awaitAgentState implements the implementing-state wait: react to agent.done
// (evaluate the gate, branch on pass/fail), alert on agent.blocked (stay), and
// escalate on the state timeout.
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
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
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
		case ev, ok := <-events:
			if !ok {
				return "", "", "", fmt.Errorf("event stream closed before agent settled")
			}
			if ev.PaneID != task.PaneID {
				continue
			}
			switch ev.State {
			case exec.StateDone:
				verdict, derr := e.evaluateDone(ctx, task, doneT)
				if derr != nil {
					return "", "", "", derr
				}
				return doneT.Branch[verdict], "agent.done", verdict, nil
			case exec.StateBlocked:
				if blockedT != nil && blockedT.Action != nil {
					e.alert(ctx, task, blockedT.Action.Alert)
				}
				// stay in the state and keep waiting
			default:
				// working / idle / unknown: keep waiting
			}
		}
	}
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
		Launch:   r.Launch,
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
// state whose agent.done transition evaluates a decision gets a reviewer task (the
// rubric + a PR pointer, producing a verdict file); otherwise the agent is an
// implementer and gets the issue.
func (e *Engine) agentTask(ctx context.Context, task *store.Task, st config.State, r config.Role) (taskFile, kickoff string, err error) {
	if dec := decisionForState(st); dec != "" {
		return e.reviewerTask(task, dec)
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
	if err := e.store.UpdateTask(ctx, task); err != nil {
		return fmt.Errorf("persist transition %s->%s: %w", from, next, err)
	}
	e.log.Info("transition", "task", task.ID, "from", from, "to", next, "trigger", trigger, "result", result)
	return nil
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
	e.notify(ctx, notify.Event{
		TaskID: task.ID,
		Issue:  task.Issue,
		State:  task.CurrentState,
		Kind:   "escalated",
	})
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
	if state == e.goal {
		return true
	}
	st, ok := e.wf.States[state]
	return ok && st.Terminal != ""
}

// --- small helpers ---

func taskID(issue int) string     { return fmt.Sprintf("issue-%d", issue) }
func branchName(issue int) string { return fmt.Sprintf("agent/issue-%d", issue) }

func prNum(t *store.Task) int {
	if t.PRNumber == nil {
		return 0
	}
	return *t.PRNumber
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
