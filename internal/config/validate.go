package config

import (
	"fmt"
	"time"
)

// authoritativeGateTypes are the only gate types allowed to feed a gate
// evaluation (invariant 4). Gates must read objective sources, never agent
// self-report or untrusted text.
var authoritativeGateTypes = map[string]bool{
	"github_pr":        true,
	"github_checks":    true,
	"github_reviews":   true,
	"github_mergeable": true,
	"github_commits":   true,
}

// commitsGateSince are the supported `since` anchors for a github_commits gate.
// The anchor is required rather than implied, so the YAML states what the gate
// actually asks ("commits since we entered this state") instead of hiding it.
var commitsGateSince = map[string]bool{"state_entry": true}

// sideEffectingActions are entry actions with an irreversible repo effect
// (invariant 5): entering such a state must be gate-evaluated.
var sideEffectingActions = map[string]bool{
	"merge_pr": true,
}

// semanticChecks enforces the safety invariants and collects non-fatal
// warnings. It is a faithful port of validate_workflow.py's semantic_checks and
// assumes the workflow has already passed JSON-Schema validation.
func semanticChecks(wf *Workflow) (warnings, errs []string) {
	states := wf.States
	stateNames := sortedKeys(states)

	// Invariant 1 (refs resolve) + 2 (decision totality) + 3 (gate branches).
	for _, sname := range stateNames {
		s := states[sname]
		checkEntryRoles(wf, sname, s, &errs)
		for i, t := range s.Transitions {
			checkTransition(wf, sname, i, t, &errs)
		}
	}

	// Invariant 4: gates read only authoritative sources.
	for _, gname := range sortedKeys(wf.Gates) {
		if !authoritativeGateTypes[wf.Gates[gname].Type] {
			errs = append(errs, fmt.Sprintf(
				"gate %q: type %q is not an authoritative source (allowed: [github_checks github_commits github_mergeable github_pr github_reviews])",
				gname, wf.Gates[gname].Type))
		}
	}

	// A github_commits gate must name a supported anchor.
	for _, gname := range sortedKeys(wf.Gates) {
		g := wf.Gates[gname]
		if g.Type != "github_commits" {
			continue
		}
		if !commitsGateSince[g.Since] {
			errs = append(errs, fmt.Sprintf(
				"gate %q: github_commits requires since: state_entry (got %q)", gname, g.Since))
		}
	}

	// Invariant 5: merge is gate-only.
	checkMergeGated(wf, stateNames, &errs)

	// Invariant 6: loops terminate.
	checkLoopsTerminate(wf, &errs)

	// Invariant 7: every non-terminal state has an exit (+ terminal-with-transitions warning).
	for _, sname := range stateNames {
		s := states[sname]
		if s.Terminal != "" {
			if len(s.Transitions) > 0 {
				warnings = append(warnings, fmt.Sprintf("terminal state %q also declares transitions (ignored)", sname))
			}
			continue
		}
		if len(s.Transitions) == 0 && s.WaitFor == "" {
			errs = append(errs, fmt.Sprintf("non-terminal state %q has no exit (no transitions, no wait_for)", sname))
		}
	}

	// Reachability (warnings, except an undeclared entry_state which is an error).
	checkReachability(wf, stateNames, &warnings, &errs)

	// Invariant 8: no agent state may be unbounded.
	checkAgentStatesBounded(wf, stateNames, &warnings, &errs)

	// An explicit drive_deadline below a declared state timeout would preempt it.
	checkDriveDeadline(wf, &errs)

	// role allowed_tools tokens must be shell-safe (delivered space-joined into
	// the pane shell; arg-scoped specs would break at spawn — fail closed here).
	checkAllowedTools(wf, &errs)

	return warnings, errs
}

// checkAgentStatesBounded enforces that every state running an agent is bounded
// by something. It replaces the old blanket "spawns an agent but has no timeout
// transition" warning, which fired on configs that were in fact perfectly safe —
// including the shipped default pipeline, which warned about itself on every
// startup and so trained operators to ignore the one warning that mattered.
//
// The global no-progress bound covers omission, so a missing per-state timeout is
// no longer noteworthy on its own. What IS noteworthy is a config that opts out
// of the global bound: disabling it is allowed, but only if every agent state
// then declares its own timeout. Otherwise a config could opt out of every bound
// by omission, which is exactly the shape this whole change exists to remove.
func checkAgentStatesBounded(wf *Workflow, stateNames []string, warnings, errs *[]string) {
	noProgress, err := wf.Policies.ResolveNoProgressTimeout()
	if err != nil {
		*errs = append(*errs, err.Error())
		return
	}
	if noProgress > 0 {
		return // the global bound covers every agent state
	}
	*warnings = append(*warnings, "policies.no_progress_timeout is 0s: the global liveness "+
		"bound is disabled, so every agent state must declare its own timeout")
	for _, sname := range stateNames {
		s := wf.States[sname]
		if s.Entry == nil || (s.Entry.Spawn == "" && s.Entry.Resume == "") {
			continue
		}
		if !s.HasTimeoutTransition() {
			*errs = append(*errs, fmt.Sprintf(
				"state %q runs an agent with no timeout transition, and policies.no_progress_timeout "+
					"is disabled: nothing would bound it (declare a timeout, or re-enable the global bound)",
				sname))
		}
	}
}

// checkDriveDeadline rejects a per-drive ceiling that sits at or below the
// longest declared state timeout. The ceiling is a backstop for a drive wedged
// where no in-drive timer is armed; set below a state's own timeout it stops
// being a backstop and becomes a competing deadline that reaps every task in
// that state before the state's transition can ever fire.
func checkDriveDeadline(wf *Workflow, errs *[]string) {
	if wf.Policies.DriveDeadline == "" {
		return // derived; ResolveDriveDeadline keeps the margin by construction
	}
	deadline, err := time.ParseDuration(wf.Policies.DriveDeadline)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("policies.drive_deadline %q: %v", wf.Policies.DriveDeadline, err))
		return
	}
	longest, err := wf.LongestStateTimeout()
	if err != nil {
		*errs = append(*errs, err.Error())
		return
	}
	if longest > 0 && deadline <= longest {
		*errs = append(*errs, fmt.Sprintf(
			"policies.drive_deadline %s is not above the longest state timeout %s: it would reap drives "+
				"before their own timeout transition could fire (raise it, or lower the state timeout)",
			deadline, longest))
	}
}

// checkAllowedTools rejects role allowed_tools entries that are not shell-safe
// tokens. The launch argv is delivered space-joined into the pane shell (see
// engine.launchArgs / exec.Herdr.Spawn), so a token with spaces/globs/parens
// would produce a shell error at spawn; validation fails closed instead.
func checkAllowedTools(wf *Workflow, errs *[]string) {
	for _, rname := range sortedKeys(wf.Roles) {
		for _, tok := range wf.Roles[rname].AllowedTools {
			if !shellSafeToolToken(tok) {
				*errs = append(*errs, fmt.Sprintf(
					"role %q: allowed_tools entry %q is not a shell-safe token "+
						"(only letters, digits, underscore); arg-scoped specs like "+
						"\"Bash(gh pr view:*)\" are not deliverable yet",
					rname, tok))
			}
		}
	}
}

// shellSafeToolToken reports whether s is a bare tool token safe to pass through
// the unquoted pane-shell launch: letters, digits, and underscore only (covers
// coarse tool names and mcp__server__tool).
func shellSafeToolToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

func checkEntryRoles(wf *Workflow, sname string, s State, errs *[]string) {
	if s.Entry == nil {
		return
	}
	if s.Entry.Spawn != "" {
		if _, ok := wf.Roles[s.Entry.Spawn]; !ok {
			*errs = append(*errs, fmt.Sprintf("state %q: entry.spawn references unknown role %q", sname, s.Entry.Spawn))
		}
	}
	if s.Entry.Resume != "" {
		if _, ok := wf.Roles[s.Entry.Resume]; !ok {
			*errs = append(*errs, fmt.Sprintf("state %q: entry.resume references unknown role %q", sname, s.Entry.Resume))
		}
	}
}

func checkTransition(wf *Workflow, sname string, i int, t Transition, errs *[]string) {
	where := fmt.Sprintf("state %q transition[%d]", sname, i)
	dec := t.DecisionRef()
	gts := t.GateRefs()

	if dec != "" {
		if _, ok := wf.Decisions[dec]; !ok {
			*errs = append(*errs, fmt.Sprintf("%s: references unknown decision %q", where, dec))
		}
	}
	for _, g := range gts {
		if _, ok := wf.Gates[g]; !ok {
			*errs = append(*errs, fmt.Sprintf("%s: references unknown gate %q", where, g))
		}
	}
	for _, tgt := range t.Targets() {
		if _, ok := wf.States[tgt]; !ok {
			*errs = append(*errs, fmt.Sprintf("%s: targets unknown state %q", where, tgt))
		}
	}

	switch {
	case len(t.Branch) > 0:
		keys := sortedKeys(t.Branch)
		switch {
		case dec != "":
			verdicts := wf.Decisions[dec].Verdicts
			if !sameSet(keys, verdicts) {
				*errs = append(*errs, fmt.Sprintf(
					"%s: decision %q branch keys %v must exactly cover verdicts %v",
					where, dec, keys, sortedCopy(verdicts)))
			}
		case len(gts) > 0:
			if !sameSet(keys, []string{"pass", "fail"}) {
				*errs = append(*errs, fmt.Sprintf("%s: gate branch keys %v must be [fail pass]", where, keys))
			}
		default:
			*errs = append(*errs, fmt.Sprintf("%s: 'branch' requires a decision or gate trigger/evaluate", where))
		}
	case (dec != "" || len(gts) > 0) && t.Action == nil:
		*errs = append(*errs, fmt.Sprintf("%s: decision/gate transition must have a 'branch'", where))
	}
}

func checkMergeGated(wf *Workflow, stateNames []string, errs *[]string) {
	side := map[string]bool{}
	for sname, s := range wf.States {
		if s.Entry != nil && sideEffectingActions[s.Entry.Action] {
			side[sname] = true
		}
	}
	for _, sname := range stateNames {
		for i, t := range wf.States[sname].Transitions {
			if len(t.GateRefs()) > 0 {
				continue
			}
			for _, tgt := range t.Targets() {
				if side[tgt] {
					*errs = append(*errs, fmt.Sprintf(
						"state %q transition[%d]: enters side-effecting state %q without a gate (merge must be gate-evaluated, never a decision/event)",
						sname, i, tgt))
				}
			}
		}
	}
}

func checkLoopsTerminate(wf *Workflow, errs *[]string) {
	graph := buildGraph(wf)
	retryCaps := wf.Policies.RetryCaps
	for _, comp := range tarjanSCC(graph) {
		cyclic := len(comp) > 1 || (len(comp) == 1 && contains(graph[comp[0]], comp[0]))
		if !cyclic {
			continue
		}
		capped := false
		for _, n := range comp {
			if _, ok := retryCaps[n]; ok {
				capped = true
				break
			}
			if wf.States[n].HasTimeoutTransition() {
				capped = true
				break
			}
		}
		if !capped {
			*errs = append(*errs, fmt.Sprintf("cycle %v has no retry cap or timeout -> non-terminating loop", sortedCopy(comp)))
		}
	}
}

func checkReachability(wf *Workflow, stateNames []string, warnings, errs *[]string) {
	if wf.EntryState == nil {
		*warnings = append(*warnings, "no entry_state declared; cannot check reachability")
		return
	}
	entry := *wf.EntryState
	if _, ok := wf.States[entry]; !ok {
		*errs = append(*errs, fmt.Sprintf("entry_state %q is not a declared state", entry))
		return
	}
	graph := buildGraph(wf)
	seen := map[string]bool{}
	stack := []string{entry}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			continue
		}
		seen[n] = true
		stack = append(stack, graph[n]...)
	}
	for _, n := range stateNames {
		if !seen[n] && wf.States[n].WaitFor == "" {
			*warnings = append(*warnings, fmt.Sprintf("state %q is unreachable from entry_state %q", n, entry))
		}
	}
}
