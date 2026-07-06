package engine

import (
	"fmt"
	"sort"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

// CheckExecutable reports states the interpreter cannot drive. A non-terminal
// state dead-ends at runtime ("no supported trigger", or a permanent wait) unless
// runState can make progress on it — the daemon then re-drives it every poll and
// the task never advances.
//
// config's semantic validation is deliberately ignorant of which events the
// engine drives (it validates graph shape — reachability, cycles, totality, and
// it counts wait_for as a valid "exit"), so this drivability check lives here,
// next to runState. Keep drivable() an exact mirror of runState.
func CheckExecutable(wf *config.Workflow) []string {
	names := make([]string, 0, len(wf.States))
	for n := range wf.States {
		names = append(names, n)
	}
	sort.Strings(names)

	var errs []string
	for _, name := range names {
		s := wf.States[name]
		if s.Terminal != "" {
			continue
		}
		if !drivable(s) {
			errs = append(errs, fmt.Sprintf(
				"state %q is not executable: needs an entry merge action, an agent.done "+
					"transition backed by an entry spawn/resume, a status.changed transition, "+
					"or an auto-fired event; a decision/gate/timeout/wait_for-only state dead-ends "+
					"at runtime", name))
		}
	}
	return errs
}

// drivable mirrors runState's progress conditions. runState (internal/engine/
// engine.go) checks the entry in the order spawn, resume, action — spawn/resume
// fall through to trigger matching, only action returns via runMergeAction — then
// fires an auto-fired event, else awaits agent.done, else evaluates status.changed,
// else errors "no supported trigger". wait_for is NOT handled by the engine, so a
// wait_for state must still carry a real drivable trigger.
func drivable(s config.State) bool {
	hasAgent := s.Entry != nil && (s.Entry.Spawn != "" || s.Entry.Resume != "")
	// A merge action drives the state, but only when nothing shadows it: runState
	// reaches the action case only if spawn and resume are both empty.
	if s.Entry != nil && s.Entry.Action != "" && !hasAgent {
		return true
	}
	for _, t := range s.Transitions {
		switch t.When.Event {
		case "status.changed":
			return true
		case "agent.done":
			// agent.done drives only if this state launches the agent whose
			// completion it awaits; without a spawn/resume the pane filter in
			// awaitAgentState never matches and it hangs (or only ever times out).
			if hasAgent {
				return true
			}
		default:
			if autoFiredEvents[t.When.Event] {
				return true
			}
		}
	}
	return false
}
