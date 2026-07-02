package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

const (
	goodFixture   = "../../internal/config/testdata/default-pipeline.yaml"
	brokenFixture = "../../internal/config/testdata/broken-pipeline.yaml"
)

func TestRun_Dispatch(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 2},
		{"unknown command", []string{"bogus"}, 2},
		{"help", []string{"--help"}, 0},
		{"validate good", []string{"validate", goodFixture}, 0},
		{"validate broken", []string{"validate", brokenFixture}, 1},
		{"validate missing arg", []string{"validate"}, 2},
		{"run missing flags", []string{"run"}, 2},
		{"run missing issue", []string{"run", "--config", goodFixture, "--repo", "."}, 2},
		{"recover missing flags", []string{"recover"}, 2},
		{"daemon missing flags", []string{"daemon"}, 2},
		{"version", []string{"version"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestCmdVersion(t *testing.T) {
	var buf bytes.Buffer
	if got := cmdVersion(&buf); got != 0 {
		t.Errorf("cmdVersion = %d, want 0", got)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("cmdVersion wrote no output")
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Errorf("cmdVersion should write a single line, got %q", buf.String())
	}
	if !strings.Contains(out, version) {
		t.Errorf("cmdVersion output %q does not contain version %q", out, version)
	}
}

func TestRepoSlug(t *testing.T) {
	wf, _, err := config.Load(goodFixture)
	if err != nil {
		t.Fatal(err)
	}
	if got := repoSlug(wf); got != "sean1588/minicode" {
		t.Errorf("repoSlug = %q, want sean1588/minicode", got)
	}
}

func TestRun_Plan_Dispatches(t *testing.T) {
	if got := run([]string{"plan", goodFixture}); got != 0 {
		t.Errorf("run(plan good) = %d, want 0", got)
	}
	if got := run([]string{"plan"}); got != 2 {
		t.Errorf("run(plan) with no path = %d, want 2", got)
	}
	// Fail-closed: refuse to render an invalid config (same posture as run).
	if got := run([]string{"plan", brokenFixture}); got != 1 {
		t.Errorf("run(plan broken) = %d, want 1", got)
	}
}

func TestWritePlan_MarksSideEffectingTerminalAndCycle(t *testing.T) {
	wf, _, err := config.Load(goodFixture)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writePlan(&buf, wf)
	out := buf.String()
	for _, want := range []string{"merging  [side-effecting]", "merged  [terminal:success]"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q:\n%s", want, out)
		}
	}
	// The request_changes loop must render as a real SCC line (not just the
	// section header) with its bounded annotation.
	if !strings.Contains(out, "{changes_requested, pr_open}  retry-capped or timeout-bounded") {
		t.Errorf("plan output missing the request_changes cycle line:\n%s", out)
	}
}

func TestSourceLabel(t *testing.T) {
	wf, _, err := config.Load(goodFixture)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sourceLabel(wf)
	if err != nil {
		t.Fatalf("sourceLabel(goodFixture): unexpected error: %v", err)
	}
	if got != "agent-ready" {
		t.Errorf("sourceLabel = %q, want %q", got, "agent-ready")
	}

	_, err = sourceLabel(&config.Workflow{})
	if err == nil {
		t.Error("sourceLabel(empty workflow): expected error, got nil")
	}

	wfNoLabel := &config.Workflow{Sources: []config.Source{{Type: "github_issues"}}}
	if _, err := sourceLabel(wfNoLabel); err == nil {
		t.Error("sourceLabel(github_issues without select.label): expected error, got nil")
	}
}

func TestTerminalStates(t *testing.T) {
	wf, _, err := config.Load(goodFixture)
	if err != nil {
		t.Fatal(err)
	}
	ts := terminalStates(wf)
	for _, want := range []string{"merged", "closed", "escalated"} {
		if !ts[want] {
			t.Errorf("terminalStates: want %q to be true, got false", want)
		}
	}
	for _, notWant := range []string{"intake", "queued"} {
		if ts[notWant] {
			t.Errorf("terminalStates: want %q to be absent/false, got true", notWant)
		}
	}
}

func TestSettledStates(t *testing.T) {
	wf, _, err := config.Load(goodFixture)
	if err != nil {
		t.Fatal(err)
	}
	// default pipeline has dry_run: true, so the merge_pr state ("merging") is
	// settled alongside the terminal states.
	settled := settledStates(wf)
	for _, s := range []string{"merged", "closed", "escalated", "merging"} {
		if !settled[s] {
			t.Errorf("settledStates under dry_run: want %q settled", s)
		}
	}
	for _, s := range []string{"intake", "queued", "implementing", "pr_open"} {
		if settled[s] {
			t.Errorf("settledStates: %q must not be settled", s)
		}
	}
	// dry_run OFF: the engine drives merging -> merged, so "merging" is NOT settled.
	off := false
	wf.Policies.DryRun = &off
	settled = settledStates(wf)
	if settled["merging"] {
		t.Error(`settledStates with dry_run off: "merging" must not be settled`)
	}
	if !settled["merged"] {
		t.Error(`settledStates: "merged" must stay settled`)
	}
}

// The UNCAPPED branch is unreachable via cmdPlan (which refuses invalid configs),
// so exercise writePlan directly on an in-memory uncapped self-loop. This also
// pins cycleBounded's parity with the validator (a cycle with no cap/timeout).
func TestWritePlan_UncappedCycleAnnotated(t *testing.T) {
	wf := &config.Workflow{
		Name: "x",
		States: map[string]config.State{
			"loop": {Transitions: []config.Transition{{To: "loop"}}},
		},
	}
	var buf bytes.Buffer
	writePlan(&buf, wf)
	out := buf.String()
	if !strings.Contains(out, "{loop}") || !strings.Contains(out, "UNCAPPED") {
		t.Errorf("uncapped self-loop should render {loop} ... UNCAPPED:\n%s", out)
	}
}
