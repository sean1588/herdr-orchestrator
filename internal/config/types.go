// Package config loads, schema-validates, and invariant-checks Herdr Orchestrator
// workflow configs. The workflow types defined here are the single source of
// truth consumed by the engine; the engine never redefines them.
package config

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

// Policies holds workflow-wide policy knobs. retry_caps and dry_run are enforced
// (retry_caps bounds the changes_requested loop; dry_run gates the real merge —
// see DryRunEnabled). max_concurrent_tasks is parsed but not yet enforced: the
// scheduler that would honor it is deferred (roadmap R2), so today one issue is
// driven per run. circuit_breaker and execution are likewise recorded but not
// yet acted on.
type Policies struct {
	MaxConcurrentTasks int            `yaml:"max_concurrent_tasks"` // parsed; scheduler that enforces it is deferred (R2)
	DryRun             *bool          `yaml:"dry_run"`              // gates the real merge; nil => default-on
	CircuitBreaker     bool           `yaml:"circuit_breaker"`
	RetryCaps          map[string]int `yaml:"retry_caps"` // keyed by state name
	Execution          Execution      `yaml:"execution"`
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

// Source is a place work originates (Phase 1: github_issues, not yet polled).
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
	// agent's own default permission config governs.
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
