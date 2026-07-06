package engine

import (
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

// The shipped pipeline is fully drivable — every non-terminal state has an entry
// action or an agent.done/status.changed/auto-fired trigger.
func TestCheckExecutable_DefaultPipelineIsExecutable(t *testing.T) {
	wf, _, err := config.Load("../config/testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if errs := CheckExecutable(wf); len(errs) != 0 {
		t.Fatalf("default-pipeline must be executable, got: %v", errs)
	}
}

// A state whose only progress path is a decision trigger (plus a timeout) is a
// runtime dead-end: the engine has no agent.done/status.changed/auto-fired event
// to act on, so it errors "no supported trigger" every poll. This is the exact
// config a live dogfood hit — valid per the schema/graph checks, un-runnable.
func TestCheckExecutable_RejectsDecisionTriggerOnlyState(t *testing.T) {
	entry := "intake"
	wf := &config.Workflow{
		EntryState: &entry,
		States: map[string]config.State{
			"intake": {Transitions: []config.Transition{
				{When: config.Trigger{Decision: "triage"}, Branch: map[string]string{"accept": "done", "reject": "done", "needs_human": "esc"}},
				{When: config.Trigger{Timeout: "5m"}, To: "esc"},
			}},
			"done": {Terminal: "success"},
			"esc":  {Terminal: "needs_human"},
		},
	}
	errs := CheckExecutable(wf)
	if !strings.Contains(strings.Join(errs, "\n"), `state "intake" is not executable`) {
		t.Fatalf("want an intake non-executable error, got: %v", errs)
	}
}

// The four drivable forms all pass: an agent.done state (with a spawn), an
// auto-fired (scheduled) event, a status.changed gate, and an entry action.
func TestCheckExecutable_AcceptsAllDrivableForms(t *testing.T) {
	entry := "a"
	wf := &config.Workflow{
		EntryState: &entry,
		States: map[string]config.State{
			"a":    {Entry: &config.Entry{Spawn: "impl"}, Transitions: []config.Transition{{When: config.Trigger{Event: "agent.done"}, Branch: map[string]string{"pass": "b", "fail": "b"}}}},
			"b":    {Transitions: []config.Transition{{When: config.Trigger{Event: "scheduled"}, To: "c"}}},
			"c":    {Transitions: []config.Transition{{When: config.Trigger{Event: "status.changed"}, Branch: map[string]string{"pass": "d", "fail": "d"}}}},
			"d":    {Entry: &config.Entry{Action: "merge_pr"}, Transitions: []config.Transition{{When: config.Trigger{Event: "pr.merged"}, To: "done"}}},
			"done": {Terminal: "success"},
		},
	}
	if errs := CheckExecutable(wf); len(errs) != 0 {
		t.Fatalf("all drivable forms must pass, got: %v", errs)
	}
}

// wait_for is NOT special-cased: the engine never implements it, so a
// wait_for-only state (no drivable trigger) is a runtime dead-end and must be
// flagged — this is the CLAUDE.md appendix's `blocked_on_gate: {wait_for: ...}`
// form, which the graph validator blesses but the engine cannot drive. A
// wait_for state that also carries a status.changed transition passes (the
// transition drives it; wait_for is ignored). Terminals are exempt.
func TestCheckExecutable_WaitForOnlyIsFlagged(t *testing.T) {
	entry := "w"
	wf := &config.Workflow{
		EntryState: &entry,
		States: map[string]config.State{
			"w":    {WaitFor: "status.changed"}, // no transitions => engine dead-ends
			"done": {Terminal: "success"},
		},
	}
	if !strings.Contains(strings.Join(CheckExecutable(wf), "\n"), `state "w" is not executable`) {
		t.Fatalf("wait_for-only state must be flagged (engine never drives wait_for)")
	}

	// wait_for + a real drivable trigger is fine.
	wf.States["w"] = config.State{
		WaitFor:     "status.changed",
		Transitions: []config.Transition{{When: config.Trigger{Event: "status.changed"}, Branch: map[string]string{"pass": "done", "fail": "done"}}},
	}
	if errs := CheckExecutable(wf); len(errs) != 0 {
		t.Fatalf("wait_for with a status.changed transition must pass, got: %v", errs)
	}
}

// entry.action drives a state only when nothing shadows it: runState checks
// spawn/resume first and falls through, reaching the action case only if both are
// empty. A spawn+action state with no drivable trigger is a dead-end.
func TestCheckExecutable_SpawnPlusActionNeedsDrivableTrigger(t *testing.T) {
	entry := "m"
	wf := &config.Workflow{
		EntryState: &entry,
		States: map[string]config.State{
			"m":    {Entry: &config.Entry{Spawn: "impl", Action: "merge_pr"}, Transitions: []config.Transition{{When: config.Trigger{Event: "pr.merged"}, To: "done"}}},
			"done": {Terminal: "success"},
		},
	}
	if !strings.Contains(strings.Join(CheckExecutable(wf), "\n"), `state "m" is not executable`) {
		t.Fatalf("spawn+action with no drivable trigger must be flagged (spawn shadows the action)")
	}
}

// An agent.done transition drives only if this state launches the agent whose
// completion it awaits; without a spawn/resume no agent.done ever arrives.
func TestCheckExecutable_AgentDoneWithoutSpawnIsFlagged(t *testing.T) {
	entry := "a"
	wf := &config.Workflow{
		EntryState: &entry,
		States: map[string]config.State{
			"a":    {Transitions: []config.Transition{{When: config.Trigger{Event: "agent.done"}, Branch: map[string]string{"pass": "done", "fail": "done"}}}},
			"done": {Terminal: "success"},
		},
	}
	if !strings.Contains(strings.Join(CheckExecutable(wf), "\n"), `state "a" is not executable`) {
		t.Fatalf("agent.done without a spawn/resume must be flagged")
	}
}
