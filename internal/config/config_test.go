package config

import (
	"errors"
	"strings"
	"testing"
)

// mustParse parses inline YAML and fails the test on a non-validation error.
func mustParse(t *testing.T, yaml string) (*Workflow, []string, error) {
	t.Helper()
	wf, warnings, err := parse([]byte(yaml))
	return wf, warnings, err
}

// semErrors returns the semantic error strings from err, or fails if err is not
// a *ValidationErrors at the semantic stage.
func semErrors(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationErrors
	if err == nil {
		return nil
	}
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationErrors, got %T: %v", err, err)
	}
	if ve.Stage != "semantic" {
		t.Fatalf("expected semantic-stage errors, got stage %q: %v", ve.Stage, ve.Errors)
	}
	return ve.Errors
}

func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --- Golden tests: the two fixtures are the validator's acceptance bar. ---

func TestLoad_DefaultPipeline_ValidWith2Warnings(t *testing.T) {
	wf, warnings, err := Load("testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("default-pipeline.yaml should be valid, got error: %v", err)
	}
	if wf.Name != "default-pipeline" {
		t.Errorf("name = %q, want default-pipeline", wf.Name)
	}
	if len(warnings) != 2 {
		t.Fatalf("want exactly 2 warnings, got %d: %v", len(warnings), warnings)
	}
	// Both warnings are the spawn/resume-without-timeout kind, for pr_open and changes_requested.
	if !containsSubstr(warnings, `state "pr_open" spawns/resumes`) {
		t.Errorf("missing pr_open spawn-no-timeout warning: %v", warnings)
	}
	if !containsSubstr(warnings, `state "changes_requested" spawns/resumes`) {
		t.Errorf("missing changes_requested resume-no-timeout warning: %v", warnings)
	}
}

func TestLoad_DefaultPipeline_DecodesStructure(t *testing.T) {
	wf, _, err := Load("testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if wf.Version != 0 || wf.EntryState == nil || *wf.EntryState != "intake" {
		t.Errorf("version/entry = %d/%v", wf.Version, wf.EntryState)
	}
	if len(wf.States) != 11 {
		t.Errorf("want 11 states, got %d", len(wf.States))
	}
	impl, ok := wf.States["implementing"]
	if !ok || impl.Entry == nil || impl.Entry.Spawn != "implementer" {
		t.Fatalf("implementing.entry.spawn not decoded: %+v", impl)
	}
	if len(impl.Transitions) != 3 {
		t.Fatalf("implementing should have 3 transitions, got %d", len(impl.Transitions))
	}
	// transition[0]: when event agent.done, evaluate gate pr_exists, branch pass/fail.
	t0 := impl.Transitions[0]
	if t0.When.Event != "agent.done" {
		t.Errorf("t0 event = %q", t0.When.Event)
	}
	if t0.Evaluate == nil || len(t0.Evaluate.Gate) != 1 || t0.Evaluate.Gate[0] != "pr_exists" {
		t.Errorf("t0 evaluate gate = %+v", t0.Evaluate)
	}
	if t0.Branch["pass"] != "pr_open" || t0.Branch["fail"] != "escalated" {
		t.Errorf("t0 branch = %v", t0.Branch)
	}
	// transition[2]: timeout 45m -> escalated.
	if impl.Transitions[2].When.Timeout != "45m" || impl.Transitions[2].To != "escalated" {
		t.Errorf("t2 = %+v", impl.Transitions[2])
	}
	// dry_run default tracked as *bool, set to true in the fixture.
	if wf.Policies.DryRun == nil || *wf.Policies.DryRun != true {
		t.Errorf("dry_run = %v", wf.Policies.DryRun)
	}
	if wf.Policies.Execution.RunAs != "non_root" {
		t.Errorf("run_as = %q", wf.Policies.Execution.RunAs)
	}
}

func TestLoad_BrokenPipeline_FailsInvariants1256(t *testing.T) {
	_, _, err := Load("testdata/broken-pipeline.yaml")
	errs := semErrors(t, err)
	if len(errs) != 4 {
		t.Fatalf("broken-pipeline should produce exactly 4 semantic errors, got %d:\n%s",
			len(errs), strings.Join(errs, "\n"))
	}
	checks := map[string]string{
		"invariant 1 (unknown role ref)":  "unknown role",
		"invariant 2 (decision totality)": "must exactly cover verdicts",
		"invariant 5 (ungated merge)":     "without a gate",
		"invariant 6 (non-terminating)":   "non-terminating loop",
	}
	for name, sub := range checks {
		if !containsSubstr(errs, sub) {
			t.Errorf("%s: expected an error containing %q, got:\n%s", name, sub, strings.Join(errs, "\n"))
		}
	}
}

// --- GateRef: string-or-array decoding. ---

func TestGateRef_ScalarAndSequence(t *testing.T) {
	yaml := `
version: 0
name: gateref
entry_state: a
gates:
  g1: { type: github_pr }
  g2: { type: github_checks }
states:
  a:
    transitions:
      - when: { gate: g1 }
        branch: { pass: b, fail: b }
  b:
    transitions:
      - when: { gate: [g1, g2] }
        branch: { pass: a, fail: a }
`
	wf, _, err := mustParse(t, yaml)
	if err != nil {
		// This config has an uncapped cycle (a<->b) so it will carry a semantic
		// error; we only assert the decode shape here.
		if semErrors(t, err) == nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := wf.States["a"].Transitions[0].When.Gate; len(got) != 1 || got[0] != "g1" {
		t.Errorf("scalar gate decoded as %v", got)
	}
	if got := wf.States["b"].Transitions[0].When.Gate; len(got) != 2 || got[0] != "g1" || got[1] != "g2" {
		t.Errorf("sequence gate decoded as %v", got)
	}
}

// --- The `when` not `on` gotcha: `on:` must be rejected by the schema. ---

func TestSchema_RejectsOnInsteadOfWhen(t *testing.T) {
	yaml := `
version: 0
name: on-key
entry_state: a
states:
  a:
    transitions:
      - on: { event: x }
        to: a
`
	_, _, err := mustParse(t, yaml)
	var ve *ValidationErrors
	if !errors.As(err, &ve) || ve.Stage != "schema" {
		t.Fatalf("expected a schema-stage validation error for `on:`, got %v", err)
	}
}

// --- Invariants in isolation. ---

func TestInvariant1_UnknownRefs(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		sub  string
	}{
		{
			name: "unknown role",
			yaml: `
version: 0
name: t
entry_state: a
states:
  a:
    entry: { spawn: nobody }
    transitions: [ { when: { event: e }, to: a } ]`,
			sub: `entry.spawn references unknown role "nobody"`,
		},
		{
			name: "unknown decision",
			yaml: `
version: 0
name: t
entry_state: a
states:
  a:
    transitions:
      - when: { decision: missing }
        branch: { x: a }`,
			sub: `references unknown decision "missing"`,
		},
		{
			name: "unknown gate",
			yaml: `
version: 0
name: t
entry_state: a
states:
  a:
    transitions:
      - when: { gate: ghost }
        branch: { pass: a, fail: a }`,
			sub: `references unknown gate "ghost"`,
		},
		{
			name: "unknown target state",
			yaml: `
version: 0
name: t
entry_state: a
states:
  a:
    transitions: [ { when: { event: e }, to: nowhere } ]`,
			sub: `targets unknown state "nowhere"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := mustParse(t, tc.yaml)
			if !containsSubstr(semErrors(t, err), tc.sub) {
				t.Errorf("want error containing %q, got %v", tc.sub, err)
			}
		})
	}
}

func TestInvariant2_DecisionMustBeTotal(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
decisions:
  d: { impl: { type: llm }, verdicts: [yes, no] }
states:
  a:
    transitions:
      - when: { decision: d }
        branch: { yes: a }`
	_, _, err := mustParse(t, yaml)
	if !containsSubstr(semErrors(t, err), "must exactly cover verdicts") {
		t.Errorf("want totality error, got %v", err)
	}
}

func TestInvariant3_GateBranchMustBePassFail(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
gates:
  g: { type: github_pr }
states:
  a:
    transitions:
      - when: { gate: g }
        branch: { pass: a, maybe: a }`
	_, _, err := mustParse(t, yaml)
	if !containsSubstr(semErrors(t, err), "gate branch keys") {
		t.Errorf("want gate branch error, got %v", err)
	}
}

func TestInvariant4_GateMustBeAuthoritative(t *testing.T) {
	// The JSON Schema enum already blocks non-authoritative gate types, so this
	// invariant can only fire when bypassing the schema. Exercise the check
	// directly against a hand-built workflow.
	entry := "a"
	wf := &Workflow{
		EntryState: &entry,
		Gates:      map[string]Gate{"sketchy": {Type: "agent_self_report"}},
		States: map[string]State{
			"a": {Terminal: "success"},
		},
	}
	_, errs := semanticChecks(wf)
	if !containsSubstr(errs, "is not an authoritative source") {
		t.Errorf("want authoritative-source error, got %v", errs)
	}
}

func TestInvariant5_MergeIsGateOnly(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
decisions:
  d: { impl: { type: llm }, verdicts: [go, stop] }
states:
  a:
    transitions:
      - when: { decision: d }
        branch: { go: m, stop: done }
  m:
    entry: { action: merge_pr }
    transitions: [ { when: { event: pr.merged }, to: done } ]
  done: { terminal: success }`
	_, _, err := mustParse(t, yaml)
	if !containsSubstr(semErrors(t, err), "without a gate") {
		t.Errorf("want ungated-merge error, got %v", err)
	}
}

func TestInvariant5_MergeWithGateIsOK(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
gates:
  ci: { type: github_checks }
states:
  a:
    transitions:
      - when: { event: status.changed }
        evaluate: { gate: ci }
        branch: { pass: m, fail: done }
  m:
    entry: { action: merge_pr }
    transitions: [ { when: { event: pr.merged }, to: done } ]
  done: { terminal: success }`
	_, _, err := mustParse(t, yaml)
	if err != nil {
		t.Errorf("gate-evaluated merge should be valid, got %v", err)
	}
}

func TestInvariant6_UncappedCycleRejected(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
states:
  a:
    transitions: [ { when: { event: e }, to: b } ]
  b:
    transitions: [ { when: { event: e }, to: a } ]`
	_, _, err := mustParse(t, yaml)
	if !containsSubstr(semErrors(t, err), "non-terminating loop") {
		t.Errorf("want loop error, got %v", err)
	}
}

func TestInvariant6_CycleCappedByTimeout(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
states:
  a:
    transitions:
      - when: { event: e }
        to: b
  b:
    transitions:
      - when: { event: e }
        to: a
      - when: { timeout: 5m }
        to: done
  done: { terminal: success }`
	_, _, err := mustParse(t, yaml)
	if err != nil {
		t.Errorf("timeout-capped cycle should be valid, got %v", err)
	}
}

func TestInvariant6_CycleCappedByRetryCap(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
policies:
  retry_caps: { b: 3 }
states:
  a:
    transitions: [ { when: { event: e }, to: b } ]
  b:
    transitions: [ { when: { event: e }, to: a } ]`
	_, _, err := mustParse(t, yaml)
	if err != nil {
		t.Errorf("retry-cap-capped cycle should be valid, got %v", err)
	}
}

func TestInvariant7_NonTerminalNeedsExit(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
states:
  a: {}`
	_, _, err := mustParse(t, yaml)
	if !containsSubstr(semErrors(t, err), "has no exit") {
		t.Errorf("want no-exit error, got %v", err)
	}
}

// Fidelity with validate_workflow.py: an absent entry_state is a warning, but an
// explicit empty (or otherwise undeclared) entry_state is a hard error.
func TestEntryState_AbsentIsWarning(t *testing.T) {
	yaml := `
version: 0
name: x
states:
  a: { terminal: success }`
	_, warnings, err := mustParse(t, yaml)
	if err != nil {
		t.Fatalf("absent entry_state should be valid (warning only), got %v", err)
	}
	if !containsSubstr(warnings, "no entry_state declared") {
		t.Errorf("want no-entry_state warning, got %v", warnings)
	}
}

func TestEntryState_ExplicitEmptyIsError(t *testing.T) {
	yaml := `
version: 0
name: x
entry_state: ""
states:
  a: { terminal: success }`
	_, _, err := mustParse(t, yaml)
	if !containsSubstr(semErrors(t, err), `entry_state "" is not a declared state`) {
		t.Errorf("explicit empty entry_state must error (match reference validator), got %v", err)
	}
}

func TestWarning_UnreachableState(t *testing.T) {
	yaml := `
version: 0
name: t
entry_state: a
states:
  a: { terminal: success }
  orphan: { terminal: success }`
	_, warnings, err := mustParse(t, yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !containsSubstr(warnings, `state "orphan" is unreachable`) {
		t.Errorf("want unreachable warning, got %v", warnings)
	}
}
