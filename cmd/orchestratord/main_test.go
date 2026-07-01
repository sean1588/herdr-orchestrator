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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
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
