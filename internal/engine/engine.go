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

	RepoDir string // local checkout (absolute) where git/gh run
	Base    string // base branch, e.g. "main"
	Repo    string // owner/name slug recorded on the task

	// Optional; sensible defaults applied by New.
	TaskDir      string                              // where task files are written; default os.TempDir()
	Goal         string                              // halt-on-enter success state; default "pr_open"
	StartState   string                              // where Phase 1 enqueues issues; default "queued"
	DurationFunc func(string) (time.Duration, error) // default time.ParseDuration
	Logger       *slog.Logger
}

// Engine drives tasks through the workflow.
type Engine struct {
	wf      *config.Workflow
	backend exec.ExecutionBackend
	gh      github.Client
	store   *store.Store

	repoDir, base, repo string
	taskDir             string
	goal, startState    string
	parseDur            func(string) (time.Duration, error)
	log                 *slog.Logger
}

// New builds an Engine, applying defaults.
func New(c Config) *Engine {
	e := &Engine{
		wf:         c.Workflow,
		backend:    c.Backend,
		gh:         c.GitHub,
		store:      c.Store,
		repoDir:    c.RepoDir,
		base:       c.Base,
		repo:       c.Repo,
		taskDir:    c.TaskDir,
		goal:       c.Goal,
		startState: c.StartState,
		parseDur:   c.DurationFunc,
		log:        c.Logger,
	}
	if e.taskDir == "" {
		e.taskDir = os.TempDir()
	}
	if e.goal == "" {
		e.goal = "pr_open"
	}
	if e.startState == "" {
		e.startState = "queued"
	}
	if e.parseDur == nil {
		e.parseDur = time.ParseDuration
	}
	if e.log == nil {
		e.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return e
}

// Run drives a single issue from the start state to the goal (pr_open) or a
// terminal state, returning the final state.
func (e *Engine) Run(ctx context.Context, issue int) (string, error) {
	task, err := e.ensureTask(ctx, issue)
	if err != nil {
		return "", err
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
		if e.isHalt(task.CurrentState) {
			continue
		}
		e.log.Info("recovering task", "task", task.ID, "state", task.CurrentState)
		if err := e.reconcile(ctx, task); err != nil {
			e.log.Warn("reconcile failed", "task", task.ID, "err", err)
			continue
		}
		if _, err := e.drive(ctx, task); err != nil {
			e.log.Warn("resume failed", "task", task.ID, "err", err)
		}
	}
	return nil
}

// ensureTask loads an existing task or creates a fresh one at the start state.
func (e *Engine) ensureTask(ctx context.Context, issue int) (*store.Task, error) {
	id := taskID(issue)
	existing, err := e.store.GetTask(ctx, id)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("load task %s: %w", id, err)
	}
	task := &store.Task{
		ID:           id,
		Issue:        issue,
		Repo:         e.repo,
		Branch:       branchName(issue),
		CurrentState: e.startState,
	}
	if err := e.store.CreateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("create task %s: %w", id, err)
	}
	e.log.Info("task created", "task", id, "state", e.startState, "branch", task.Branch)
	return task, nil
}

// drive runs the interpreter loop until a halt state (goal or terminal).
func (e *Engine) drive(ctx context.Context, task *store.Task) (string, error) {
	for {
		if e.isHalt(task.CurrentState) {
			e.log.Info("halt", "task", task.ID, "state", task.CurrentState, "pr", prNum(task))
			return task.CurrentState, nil
		}
		next, trigger, result, err := e.runState(ctx, task)
		if err != nil {
			return task.CurrentState, err
		}
		if err := e.advance(ctx, task, next, trigger, result); err != nil {
			return task.CurrentState, err
		}
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
			if err := e.spawn(ctx, task, st.Entry.Spawn); err != nil {
				return "", "", "", err
			}
		case st.Entry.Resume != "":
			return "", "", "", fmt.Errorf("state %q: entry.resume is out of Phase 1 scope", name)
		case st.Entry.Action != "":
			return "", "", "", fmt.Errorf("state %q: entry.action %q is out of Phase 1 scope", name, st.Entry.Action)
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

	return "", "", "", fmt.Errorf("state %q: no Phase 1-supported trigger (decisions and other events are out of scope)", name)
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
				verdict, gerr := e.evaluateGate(ctx, task, doneT)
				if gerr != nil {
					return "", "", "", gerr
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

// evaluateGate evaluates all gates referenced by a transition over authoritative
// sources, returning "pass" iff every gate passes, else "fail".
func (e *Engine) evaluateGate(ctx context.Context, task *store.Task, t *config.Transition) (string, error) {
	for _, gname := range t.GateRefs() {
		g, ok := e.wf.Gates[gname]
		if !ok {
			return "", fmt.Errorf("gate %q not declared", gname)
		}
		pass, err := e.evalGate(ctx, task, gname, g)
		if err != nil {
			return "", err
		}
		if !pass {
			return "fail", nil
		}
	}
	return "pass", nil
}

func (e *Engine) evalGate(ctx context.Context, task *store.Task, name string, g config.Gate) (bool, error) {
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
	default:
		// ci_green / approvals / no_conflicts are part of the merge gate (out of Phase 1).
		return false, fmt.Errorf("gate %q: type %q not implemented in Phase 1", name, g.Type)
	}
}

// spawn runs the implementer entry action: write the task file, build a
// single-line kickoff, and launch the agent. It is idempotent — if the task
// already has a live agent (resume), it re-resolves the pane instead of
// launching a second one.
func (e *Engine) spawn(ctx context.Context, task *store.Task, role string) error {
	if task.PaneID != "" {
		if h, ok, err := e.backend.Resolve(ctx, task.ID); err == nil && ok {
			task.PaneID = h.PaneID
			e.log.Info("reusing live agent", "task", task.ID, "pane", h.PaneID)
			return nil
		}
	}

	r, ok := e.wf.Roles[role]
	if !ok {
		return fmt.Errorf("role %q not declared", role)
	}

	taskFile, err := e.writeTaskFile(ctx, task)
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
		Kickoff:  e.kickoff(r, task, taskFile),
	}
	h, err := e.backend.Spawn(ctx, sp)
	if err != nil {
		return fmt.Errorf("spawn %s: %w", role, err)
	}
	task.PaneID = h.PaneID
	if err := e.store.UpdateTask(ctx, task); err != nil {
		return fmt.Errorf("persist pane id: %w", err)
	}
	e.log.Info("agent spawned", "task", task.ID, "role", role, "pane", h.PaneID)
	return nil
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
	if h, ok, err := e.backend.Resolve(ctx, task.ID); err == nil && ok {
		task.PaneID = h.PaneID
	} else {
		task.PaneID = ""
	}
	if task.CurrentState == "implementing" {
		pr, err := e.gh.FindPR(ctx, e.repoDir, task.Branch)
		if err != nil {
			return fmt.Errorf("reconcile FindPR: %w", err)
		}
		if pr != nil {
			n := pr.Number
			task.PRNumber = &n
			return e.advance(ctx, task, e.goal, "reconcile", "pass")
		}
	}
	return e.store.UpdateTask(ctx, task)
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
