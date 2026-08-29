// Package config loads, schema-validates, and invariant-checks Herdr Orchestrator
// workflow configs. The workflow types defined here are the single source of
// truth consumed by the engine; the engine never redefines them.
package config

import (
	"fmt"
	"time"
)

// Workflow is a complete, decoded workflow config.
type Workflow struct {
	Version int    `yaml:"version"`
	Name    string `yaml:"name"`
	// EntryState is a pointer so an absent key (nil -> reachability warning) is
	// distinguishable from an explicit empty/undeclared value (-> hard error),
	// matching the reference validator's None-vs-"" semantics.
	EntryState *string             `yaml:"entry_state"`
	Policies   Policies            `yaml:"policies"`
	Sources    []Source            `yaml:"sources"`
	Roles      map[string]Role     `yaml:"roles"`
	Gates      map[string]Gate     `yaml:"gates"`
	Decisions  map[string]Decision `yaml:"decisions"`
	States     map[string]State    `yaml:"states"`
}

// Policies holds workflow-wide policy knobs. retry_caps, dry_run, and
// max_concurrent_tasks are enforced (retry_caps bounds the changes_requested
// loop; dry_run gates the real merge — see DryRunEnabled; max_concurrent_tasks
// caps the daemon scheduler's concurrent workers). circuit_breaker and execution
// are recorded but not yet acted on.
type Policies struct {
	MaxConcurrentTasks int            `yaml:"max_concurrent_tasks"` // caps the daemon scheduler's concurrent workers
	DryRun             *bool          `yaml:"dry_run"`              // gates the real merge; nil => default-on
	CircuitBreaker     bool           `yaml:"circuit_breaker"`
	RetryCaps          map[string]int `yaml:"retry_caps"` // keyed by state name
	// BlockedTimeout bounds how long an agent may sit *continuously* blocked
	// (waiting on an interactive prompt nobody will answer) before the engine
	// gives up on it. Empty => no bound, i.e. only the state timeout applies.
	// See Engine.awaitAgentState for why this can't be expressed as a transition.
	BlockedTimeout string `yaml:"blocked_timeout"`
	// DriveDeadline is the hard wall-clock ceiling on a single drive, enforced by
	// the scheduler from OUTSIDE the drive goroutine. It is the backstop for the
	// windows no in-drive timer covers — a spawn, a gate read, a decision, a merge
	// — where the engine's only clock is not armed. Empty => derived (see
	// ResolveDriveDeadline).
	DriveDeadline string `yaml:"drive_deadline"`
	// NoProgressTimeout bounds how long a task may go without ANY observable
	// signal — the global liveness bound. Where a state timeout measures POSITION
	// ("how long has this task been in state X"), this measures PROGRESS ("how
	// long since it last did anything"), which is the question that actually
	// distinguishes a legitimately slow agent from a dead one.
	//
	// It inverts the default. A per-state `timeout:` protects only the states
	// someone remembered to annotate; this applies everywhere an agent runs, so
	// absence of config means the global bound applies rather than no bound at
	// all, and a per-state timeout becomes an override for the thing that
	// genuinely varies — "this one legitimately takes 90 minutes".
	//
	// Empty => defaultNoProgressTimeout. "0s" disables it (and warns).
	NoProgressTimeout string    `yaml:"no_progress_timeout"`
	Execution         Execution `yaml:"execution"`
}

// defaultNoProgressTimeout is the global liveness bound applied when a workflow
// declares none. It is deliberately generous: the bound is confirmed against the
// agent pane's own bytes before anything escalates, so it fires only when
// genuinely nothing has happened.
const defaultNoProgressTimeout = 30 * time.Minute

// ResolveNoProgressTimeout returns the global no-progress bound in effect: the
// explicit policies.no_progress_timeout if set, else defaultNoProgressTimeout.
// Zero means the bound is disabled.
func (p Policies) ResolveNoProgressTimeout() (time.Duration, error) {
	if p.NoProgressTimeout == "" {
		return defaultNoProgressTimeout, nil
	}
	d, err := time.ParseDuration(p.NoProgressTimeout)
	if err != nil {
		return 0, fmt.Errorf("parse no_progress_timeout %q: %w", p.NoProgressTimeout, err)
	}
	return d, nil
}

// minDriveDeadline floors the derived per-drive ceiling, so a workflow whose
// states declare only short timeouts (or none) still gets a sane bound rather
// than one that preempts ordinary work.
const minDriveDeadline = time.Hour

// ResolveDriveDeadline returns the wall-clock ceiling on a single drive: the
// explicit policies.drive_deadline if set, else twice the longest declared state
// timeout, floored at one hour.
//
// The default is derived rather than fixed because the ceiling must sit safely
// ABOVE every per-state timeout — it is a backstop for a wedged drive, not a
// competing deadline, and a ceiling below a state's own timeout would preempt it
// and turn every long-running state into a reaped one. Doubling the longest
// declared timeout keeps that margin automatic as configs change.
func (wf *Workflow) ResolveDriveDeadline() (time.Duration, error) {
	if s := wf.Policies.DriveDeadline; s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("parse drive_deadline %q: %w", s, err)
		}
		return d, nil
	}
	longest, err := wf.LongestStateTimeout()
	if err != nil {
		return 0, err
	}
	if d := 2 * longest; d > minDriveDeadline {
		return d, nil
	}
	return minDriveDeadline, nil
}

// LongestStateTimeout returns the longest timeout any state declares (zero if
// none do).
func (wf *Workflow) LongestStateTimeout() (time.Duration, error) {
	var longest time.Duration
	for _, sname := range sortedKeys(wf.States) {
		for _, t := range wf.States[sname].Transitions {
			if !t.When.IsTimeout() {
				continue
			}
			d, err := time.ParseDuration(t.When.Timeout)
			if err != nil {
				return 0, fmt.Errorf("state %q: parse timeout %q: %w", sname, t.When.Timeout, err)
			}
			if d > longest {
				longest = d
			}
		}
	}
	return longest, nil
}

// DryRunEnabled reports whether auto-merge should be withheld. dry_run is
// default-on: a nil (absent) or true value gates the merge; only an explicit
// false performs it.
func (p Policies) DryRunEnabled() bool { return p.DryRun == nil || *p.DryRun }

// Execution describes how agents are run.
type Execution struct {
	Backend string `yaml:"backend"` // herdr | local | container
	RunAs   string `yaml:"run_as"`  // root | non_root
	Sandbox bool   `yaml:"sandbox"`
}

// Source is a place work originates; currently only github_issues, polled by the daemon.
type Source struct {
	ID      string         `yaml:"id"`
	Type    string         `yaml:"type"`
	Repo    string         `yaml:"repo"`
	Select  map[string]any `yaml:"select"`
	EmitsTo string         `yaml:"emits_to"`
}

// Role is an agent profile.
type Role struct {
	Launch       []string `yaml:"launch"`
	TaskDelivery string   `yaml:"task_delivery"` // context_file | inline
	Workspace    string   `yaml:"workspace"`     // per_task | shared
	Kickoff      string   `yaml:"kickoff"`
	// AllowedTools optionally scopes the agent's tools (defense-in-depth). When
	// set, the backend passes the launcher's native allowlist flag. Empty => the
	// agent's own default permission config governs. Entries must be shell-safe
	// tokens (coarse tool names like "Read"/"Edit"/"Bash") — the launch argv is
	// delivered space-joined into the pane shell, so arg-scoped specs with
	// spaces/globs/parens are not safely deliverable yet (see engine.launchArgs).
	AllowedTools []string `yaml:"allowed_tools"`
}

// Gate is a deterministic predicate over an authoritative source. The
// type-specific fields select the threshold the engine checks the PR status
// against (the JSON schema permits these via additionalProperties).
type Gate struct {
	Type        string `yaml:"type"`
	Head        string `yaml:"head"`         // github_pr
	AllPassing  bool   `yaml:"all_passing"`  // github_checks
	MinApproved int    `yaml:"min_approved"` // github_reviews
	Require     string `yaml:"require"`      // github_mergeable, e.g. "clean"
}

// Decision is a constrained LLM/exec judgment hook with declared verdicts.
type Decision struct {
	Impl     DecisionImpl `yaml:"impl"`
	Verdicts []string     `yaml:"verdicts"`
}

// DecisionImpl is how a decision is computed.
type DecisionImpl struct {
	Type    string   `yaml:"type"` // llm | exec
	Rubric  string   `yaml:"rubric"`
	Command []string `yaml:"command"`
}

// State is a node in the workflow graph.
type State struct {
	Entry       *Entry       `yaml:"entry"`
	Transitions []Transition `yaml:"transitions"`
	Terminal    string       `yaml:"terminal"` // success | rejected | needs_human
	WaitFor     string       `yaml:"wait_for"`
	Alert       bool         `yaml:"alert"`
}

// Entry is the action run on entering a state.
type Entry struct {
	Spawn  string `yaml:"spawn"`  // role name
	Resume string `yaml:"resume"` // role name
	With   string `yaml:"with"`
	Action string `yaml:"action"` // merge_pr (side-effecting)
}

// Transition is one outgoing edge: a trigger, an optional secondary evaluation,
// and exactly one of {To, Branch, Action}.
type Transition struct {
	When     Trigger           `yaml:"when"`
	Evaluate *Evaluate         `yaml:"evaluate"`
	To       string            `yaml:"to"`
	Branch   map[string]string `yaml:"branch"` // verdict/{pass,fail} -> state
	Action   *Action           `yaml:"action"`
}

// Trigger fires a transition. Exactly one field is set (enforced by the schema).
type Trigger struct {
	Event    string  `yaml:"event"`
	Timeout  string  `yaml:"timeout"` // duration, e.g. "45m"
	Decision string  `yaml:"decision"`
	Gate     GateRef `yaml:"gate"`
}

// IsTimeout reports whether this trigger is a timeout trigger.
func (t Trigger) IsTimeout() bool { return t.Timeout != "" }

// Evaluate is an optional secondary check applied after an event trigger.
type Evaluate struct {
	Decision string  `yaml:"decision"`
	Gate     GateRef `yaml:"gate"`
}

// Action is a side action that does not change state (Phase 1: alert).
type Action struct {
	Alert string `yaml:"alert"`
}

// GateRef is one or more gate names; YAML accepts a scalar or a sequence.
type GateRef []string

// DecisionRef returns the decision referenced by this transition's when or
// evaluate (when takes precedence), mirroring validate_workflow.py's trigger_ref.
func (t Transition) DecisionRef() string {
	if t.When.Decision != "" {
		return t.When.Decision
	}
	if t.Evaluate != nil {
		return t.Evaluate.Decision
	}
	return ""
}

// GateRefs returns the gates referenced by this transition's when or evaluate
// (when takes precedence).
func (t Transition) GateRefs() []string {
	if len(t.When.Gate) > 0 {
		return t.When.Gate
	}
	if t.Evaluate != nil {
		return t.Evaluate.Gate
	}
	return nil
}

// Targets returns the destination states of this transition: To if set,
// otherwise the branch values. Action-only transitions have no targets.
func (t Transition) Targets() []string {
	if t.To != "" {
		return []string{t.To}
	}
	out := make([]string, 0, len(t.Branch))
	for _, v := range t.Branch {
		out = append(out, v)
	}
	return out
}

// HasTimeoutTransition reports whether the state has any timeout-triggered transition.
func (s State) HasTimeoutTransition() bool {
	for _, t := range s.Transitions {
		if t.When.IsTimeout() {
			return true
		}
	}
	return false
}
