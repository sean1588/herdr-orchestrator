package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// smokeFake scripts the herdr calls SmokeKickoff makes.
func smokeFake() *proc.Fake {
	return &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "workspace" {
			switch c.Args[1] {
			case "create":
				return []byte(`{"result":{"root_pane":{"pane_id":"wS:p1"}}}`), nil
			case "list":
				return []byte(`{"result":{"workspaces":[{"workspace_id":"wS","label":"orchestratord-doctor"}]}}`), nil
			}
		}
		return nil, nil
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
// path a real spawn uses, then close the scratch workspace.
func TestSmokeKickoff_LaunchesDeliversAndTearsDown(t *testing.T) {
	f := smokeFake()
	h := NewHerdr(f)
	h.SubmitDelay = 0
	h.KickoffAckTimeout = 0 // verification off: the fake models no pane status

	if err := h.SmokeKickoff(context.Background(), "/scratch", []string{"claude", "--flag"}, "say OK"); err != nil {
		t.Fatalf("SmokeKickoff: %v", err)
	}
	calls := f.Snapshot()

	if !hasCallWith(calls, "workspace create --cwd /scratch --label orchestratord-doctor") {
		t.Errorf("no scratch workspace created:\n%s", strings.Join(argvOf(calls), "\n"))
	}
	if !hasCallWith(calls, "pane run wS:p1 claude --flag") {
		t.Errorf("agent argv not launched verbatim:\n%s", strings.Join(argvOf(calls), "\n"))
	}
	if !hasCallWith(calls, "say OK") {
		t.Errorf("kickoff never delivered:\n%s", strings.Join(argvOf(calls), "\n"))
	}
	if !hasCallWith(calls, "workspace close wS") {
		t.Errorf("scratch workspace not closed:\n%s", strings.Join(argvOf(calls), "\n"))
	}
}

// A workspace left behind would collide with the next run's label and break
// resolve-by-label, so teardown must happen even when the launch fails.
func TestSmokeKickoff_TearsDownAfterAFailedLaunch(t *testing.T) {
	f := smokeFake()
	inner := f.Responder
	f.Responder = func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "run" {
			return nil, errors.New("launch failed")
		}
		return inner(c)
	}
	h := NewHerdr(f)
	h.SubmitDelay = 0
	h.KickoffAckTimeout = 0

	if err := h.SmokeKickoff(context.Background(), "/scratch", []string{"claude"}, "say OK"); err == nil {
		t.Fatal("expected a failed launch to be reported")
	}
	if !hasCallWith(f.Snapshot(), "workspace close wS") {
		t.Errorf("scratch workspace leaked after a failed launch:\n%s", strings.Join(argvOf(f.Snapshot()), "\n"))
	}
}

func TestSmokeKickoff_RejectsAnEmptyLaunch(t *testing.T) {
	h := NewHerdr(smokeFake())
	if err := h.SmokeKickoff(context.Background(), "/scratch", nil, "say OK"); err == nil {
		t.Fatal("expected an error for an empty launch argv")
	}
}
