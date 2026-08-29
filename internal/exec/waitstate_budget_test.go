package exec_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// WaitState is the one command that blocks by design, so it must opt out of the
// runner's default per-call budget — otherwise a short default silently caps
// every agent wait. Driven end to end against a stub `herdr` that outlives the
// default budget but finishes well inside WaitState's own.
func TestWaitState_OptsOutOfTheDefaultCommandBudget(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 0.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := exec.NewHerdr(proc.WithTimeout(proc.New(), 10*time.Millisecond))
	h.HerdrBin = bin
	h.WaitTimeout = 5 * time.Second

	got, err := h.WaitState(context.Background(), exec.Handle{PaneID: "p1"}, exec.StateDone)
	if err != nil {
		t.Fatalf("WaitState was killed by the default command budget it should have overridden: %v", err)
	}
	if got != exec.StateDone {
		t.Errorf("state = %q, want %q", got, exec.StateDone)
	}
}

// The opt-out is a raised bound, not a removed one: a herdr that ignores its own
// --timeout must still be killed rather than block the caller forever.
func TestWaitState_StillBoundedWhenHerdrIgnoresItsOwnTimeout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr")
	stub := "#!/bin/sh\ncase \"$1\" in agent) sleep 30 ;; *) echo '{}' ;; esac\n"
	if err := os.WriteFile(bin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	h := exec.NewHerdr(proc.New())
	h.HerdrBin = bin
	// Budget = WaitTimeout + 30s slack, so a negative WaitTimeout keeps the whole
	// bound sub-second without reaching into unexported slack.
	h.WaitTimeout = -30*time.Second + 200*time.Millisecond

	start := time.Now()
	if _, err := h.WaitState(context.Background(), exec.Handle{PaneID: "p1"}, exec.StateDone); err == nil {
		t.Fatal("expected the budget to kill a herdr that ignores its own --timeout")
	}
	// Well under the stub's 30s sleep: the budget fired and the runner's kill
	// grace bounded the wait, rather than the command running to completion.
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("budget did not fire: took %s", elapsed)
	}
}
