package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// smokeFake scripts the herdr calls SmokeKickoff makes: the scratch workspace
// exists under the doctor label, and the rest behaves as script describes.
func smokeFake(script herdrScript) *proc.Fake {
	script.pane = "wS:p1"
	base := script.responder()
	return &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "list" {
			return []byte(`{"result":{"workspaces":[{"workspace_id":"wS","label":"orchestratord-doctor"}]}}`), nil
		}
		return base(c)
	}}
}

func argvOf(calls []proc.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name+" "+strings.Join(c.Args, " "))
	}
	return out
}

func hasCallWith(calls []proc.Call, substr string) bool {
	for _, s := range argvOf(calls) {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// The smoke test must launch the agent and deliver a kickoff through the same
// readiness and delivery path a real spawn uses, then close the scratch workspace.
func TestSmokeKickoff_LaunchesDeliversAndTearsDown(t *testing.T) {
	f := smokeFake(herdrScript{})

	if err := testHerdr(f).SmokeKickoff(context.Background(), "/scratch", []string{"claude", "--flag"}, "say OK"); err != nil {
		t.Fatalf("SmokeKickoff: %v", err)
	}
	calls := f.Snapshot()

	for _, want := range []string{
		"workspace create --cwd /scratch --label orchestratord-doctor",
		"pane run wS:p1 claude --flag",
		"agent wait wS:p1 --until idle --until blocked",
		"agent prompt wS:p1 say OK --wait --until working --until blocked",
		"workspace close wS",
	} {
		if !hasCallWith(calls, want) {
			t.Errorf("no call with %q:\n%s", want, strings.Join(argvOf(calls), "\n"))
		}
	}
}

// A workspace left behind would collide with the next run's label and break
// resolve-by-label, so teardown must happen on every failure, not just success.
func TestSmokeKickoff_TearsDownOnEveryFailure(t *testing.T) {
	cases := []struct {
		name    string
		script  herdrScript
		failRun bool
	}{
		{name: "launch fails", failRun: true},
		{name: "agent never detected", script: herdrScript{undetected: -1}},
		{name: "agent blocked at kickoff", script: herdrScript{prompts: []string{"agent_blocked"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := smokeFake(tc.script)
			if tc.failRun {
				inner := f.Responder
				f.Responder = func(c proc.Call) ([]byte, error) {
					if len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "run" {
						return nil, errors.New("launch failed")
					}
					return inner(c)
				}
			}
			h := testHerdr(f)
			h.ReadyTimeout = 20 * time.Millisecond

			if err := h.SmokeKickoff(context.Background(), "/scratch", []string{"claude"}, "say OK"); err == nil {
				t.Fatal("expected the failure to be reported")
			}
			if !hasCallWith(f.Snapshot(), "workspace close wS") {
				t.Errorf("scratch workspace leaked:\n%s", strings.Join(argvOf(f.Snapshot()), "\n"))
			}
		})
	}
}

func TestSmokeKickoff_RejectsAnEmptyLaunch(t *testing.T) {
	h := NewHerdr(smokeFake(herdrScript{}))
	if err := h.SmokeKickoff(context.Background(), "/scratch", nil, "say OK"); err == nil {
		t.Fatal("expected an error for an empty launch argv")
	}
}
