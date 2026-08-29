package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunner_CapturesStdout(t *testing.T) {
	out, err := New().Run(context.Background(), "", "echo", "hello world")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello world" {
		t.Errorf("stdout = %q", out)
	}
}

func TestRunner_HonorsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := New().Run(context.Background(), dir, "ls")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(out), "marker.txt") {
		t.Errorf("ls in %s did not list marker.txt: %q", dir, out)
	}
}

func TestRunner_ErrorWrapsStderr(t *testing.T) {
	out, err := New().Run(context.Background(), "", "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected error for exit 3")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include stderr, got: %v", err)
	}
	_ = out
}

func TestNewScrubbed_RemovesNamedEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "pat-secret")
	// A plain runner inherits the parent environment.
	out, err := New().Run(context.Background(), "", "sh", "-c", `printf %s "$GITHUB_TOKEN"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "pat-secret" {
		t.Fatalf("plain runner should inherit GITHUB_TOKEN, got %q", out)
	}
	// A scrubbed runner drops it, so `gh` falls back to its stored OAuth token.
	out, err = NewScrubbed("GITHUB_TOKEN").Run(context.Background(), "", "sh", "-c", `printf %s "$GITHUB_TOKEN"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("scrubbed runner should drop GITHUB_TOKEN, got %q", out)
	}
}

func TestFake_RecordsCallsAndScriptsResponses(t *testing.T) {
	f := &Fake{Responder: func(c Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) > 0 && c.Args[0] == "workspace" {
			return []byte(`{"ok":true}`), nil
		}
		return nil, nil
	}}
	out, err := f.Run(context.Background(), "/repo", "herdr", "workspace", "create")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Errorf("scripted output = %q", out)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 recorded call, got %d", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Dir != "/repo" || c.Name != "herdr" || strings.Join(c.Args, " ") != "workspace create" {
		t.Errorf("recorded call = %+v", c)
	}
}

func TestRunner_BudgetKillsAHungCommand(t *testing.T) {
	r := WithTimeout(New(), 100*time.Millisecond)
	start := time.Now()
	_, err := r.Run(context.Background(), "", "sleep", "5")
	if err == nil {
		t.Fatal("expected the per-call budget to kill `sleep 5`")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("error should wrap ErrBudgetExceeded, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("budget did not fire promptly: took %s", elapsed)
	}
}

func TestRunner_BudgetLeavesFastCommandsAlone(t *testing.T) {
	out, err := WithTimeout(New(), 30*time.Second).Run(context.Background(), "", "echo", "fine")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "fine" {
		t.Errorf("stdout = %q", out)
	}
}

func TestWithBudget_OverridesTheRunnerDefault(t *testing.T) {
	// A runner whose default budget would kill this command, overridden per call.
	r := WithTimeout(New(), 10*time.Millisecond)
	ctx := WithBudget(context.Background(), 30*time.Second)
	if _, err := r.Run(ctx, "", "sleep", "0.2"); err != nil {
		t.Fatalf("per-call budget should have overridden the 10ms default: %v", err)
	}
}

func TestWithBudget_ZeroDisablesTheBound(t *testing.T) {
	r := WithTimeout(New(), 10*time.Millisecond)
	ctx := WithBudget(context.Background(), 0)
	if _, err := r.Run(ctx, "", "sleep", "0.2"); err != nil {
		t.Fatalf("a zero budget should leave the command unbounded: %v", err)
	}
}

// A caller-side cancel must NOT be reported as a budget expiry: the engine
// reclassifies an operator cancel by cause, and mislabeling it would settle the
// task as a timeout escalation instead.
func TestRunner_CallerCancelIsNotReportedAsBudgetExceeded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := WithTimeout(New(), time.Hour).Run(ctx, "", "sleep", "5")
	if err == nil {
		t.Fatal("expected an error from the cancelled command")
	}
	if errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("caller cancel misreported as a budget expiry: %v", err)
	}
}

// The Fake is not os-backed, so WithTimeout must leave it untouched rather than
// silently returning a runner that drops its scripted responses.
func TestWithTimeout_LeavesNonOSRunnersUnchanged(t *testing.T) {
	f := &Fake{}
	if got := WithTimeout(f, time.Second); got != Runner(f) {
		t.Errorf("WithTimeout should return the Fake unchanged, got %T", got)
	}
}

// Killing an over-budget command is not enough on its own: os/exec keeps reading
// the output pipes, and a grandchild that inherited them holds them open. Without
// WaitDelay this returns only when the orphaned `sleep` exits — the exact
// indefinite block the budget exists to prevent.
func TestRunner_BudgetReturnsEvenWhenAGrandchildHoldsThePipes(t *testing.T) {
	r := WithTimeout(New(), 100*time.Millisecond)
	start := time.Now()
	_, err := r.Run(context.Background(), "", "sh", "-c", "sleep 30")
	if err == nil {
		t.Fatal("expected the budget to fire")
	}
	if elapsed := time.Since(start); elapsed > killGrace+5*time.Second {
		t.Errorf("Run blocked on a pipe held by an orphaned grandchild: took %s", elapsed)
	}
}
