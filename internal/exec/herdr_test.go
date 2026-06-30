package exec

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// hasExactCall asserts that some recorded call matches name + args exactly.
func hasExactCall(t *testing.T, calls []proc.Call, name string, args ...string) {
	t.Helper()
	for _, c := range calls {
		if c.Name == name && slices.Equal(c.Args, args) {
			return
		}
	}
	t.Errorf("no call matched: %s %s\nrecorded:\n%s", name, strings.Join(args, " "), formatCalls(calls))
}

func formatCalls(calls []proc.Call) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "  [dir=%s] %s %s\n", c.Dir, c.Name, strings.Join(c.Args, " "))
	}
	return b.String()
}

func paneRunCalls(calls []proc.Call) []proc.Call {
	var out []proc.Call
	for _, c := range calls {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "run" {
			out = append(out, c)
		}
	}
	return out
}

func testSpawn() Spawn {
	return Spawn{
		TaskID:   "issue-5",
		Role:     "implementer",
		Branch:   "agent/issue-5",
		RepoDir:  "/home/u/repo",
		Base:     "main",
		TaskFile: "/tmp/task-5.md",
		Launch:   []string{"claude"},
		Kickoff:  "Read the task in /tmp/task-5.md and implement it on agent/issue-5, then open a PR.",
	}
}

func TestSpawn_ConstructsCommandsAndParsesPane(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"w7:p1"}}}`), nil
		}
		return nil, nil
	}}
	h := NewHerdr(f)
	s := testSpawn()

	hd, err := h.Spawn(context.Background(), s)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if hd.PaneID != "w7:p1" {
		t.Errorf("pane id = %q, want w7:p1 (must be parsed from output, never hardcoded)", hd.PaneID)
	}
	if hd.Workdir != "/home/u/wt-issue-5" {
		t.Errorf("workdir = %q, want /home/u/wt-issue-5", hd.Workdir)
	}

	calls := f.Snapshot()
	// Isolated worktree on the deterministic branch.
	hasExactCall(t, calls, "git", "-C", "/home/u/repo", "worktree", "add", "-b", "agent/issue-5", "/home/u/wt-issue-5", "main")
	// herdr workspace labeled with the durable task id.
	hasExactCall(t, calls, "herdr", "workspace", "create", "--cwd", "/home/u/wt-issue-5", "--label", "issue-5", "--no-focus")

	// Exactly two pane-run calls: launch (verbatim) then the single-line kickoff, in that order.
	runs := paneRunCalls(calls)
	if len(runs) != 2 {
		t.Fatalf("want 2 pane run calls (launch + kickoff), got %d:\n%s", len(runs), formatCalls(runs))
	}
	if last := runs[0].Args[len(runs[0].Args)-1]; last != "claude" {
		t.Errorf("first pane run should launch the agent verbatim, got %q", last)
	}
	if last := runs[1].Args[len(runs[1].Args)-1]; last != s.Kickoff {
		t.Errorf("second pane run should be the kickoff, got %q", last)
	}
	// Kickoff is a single line referencing the task file (never the inline body).
	if strings.Contains(s.Kickoff, "\n") || !strings.Contains(s.Kickoff, s.TaskFile) {
		t.Errorf("kickoff must be single-line and reference the task file")
	}

	// Guardrail: never inject --dangerously-skip-permissions.
	for _, c := range calls {
		if slices.Contains(c.Args, "--dangerously-skip-permissions") {
			t.Errorf("must never launch with --dangerously-skip-permissions: %v", c)
		}
	}
}

// A re-spawn must close any pre-existing workspace carrying the same label,
// mirroring the git worktree/branch cleanup; otherwise duplicate-label
// workspaces accumulate and break Resolve (which matches by label).
func TestSpawn_ClosesPreexistingSameLabelWorkspace(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "list" {
			return []byte(`{"result":{"workspaces":[{"workspace_id":"wOld","label":"issue-5"}]}}`), nil
		}
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "create" {
			return []byte(`{"result":{"root_pane":{"pane_id":"wNew:p1"}}}`), nil
		}
		return nil, nil
	}}
	h := NewHerdr(f)

	hd, err := h.Spawn(context.Background(), testSpawn()) // TaskID == "issue-5"
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if hd.PaneID != "wNew:p1" {
		t.Errorf("pane = %q, want wNew:p1", hd.PaneID)
	}
	hasExactCall(t, f.Snapshot(), "herdr", "workspace", "close", "wOld")
}

func TestSpawn_PropagatesWorktreeFailure(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if c.Name == "git" && len(c.Args) >= 4 && c.Args[3] == "add" {
			return nil, fmt.Errorf("fatal: branch exists")
		}
		return nil, nil
	}}
	_, err := NewHerdr(f).Spawn(context.Background(), testSpawn())
	if err == nil || !strings.Contains(err.Error(), "create worktree") {
		t.Errorf("want wrapped worktree error, got %v", err)
	}
}

func TestWaitState_Command(t *testing.T) {
	f := &proc.Fake{} // default responder: success for everything
	h := NewHerdr(f)
	got, err := h.WaitState(context.Background(), Handle{PaneID: "w7:p1"}, StateDone)
	if err != nil {
		t.Fatalf("waitstate: %v", err)
	}
	if got != StateDone {
		t.Errorf("state = %q, want done", got)
	}
	// Default WaitTimeout is 45m = 2_700_000 ms.
	hasExactCall(t, f.Snapshot(), "herdr", "wait", "agent-status", "w7:p1", "--status", "done", "--timeout", "2700000")
}

func TestWaitState_RespectsContextDeadline(t *testing.T) {
	f := &proc.Fake{}
	h := NewHerdr(f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := h.WaitState(ctx, Handle{PaneID: "w7:p1"}, StateDone); err != nil {
		t.Fatalf("waitstate: %v", err)
	}
	// The --timeout should be <= 30s (30000ms), i.e. clamped to the deadline, not 45m.
	for _, c := range f.Snapshot() {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "wait" {
			ms := c.Args[len(c.Args)-1]
			if ms == "2700000" {
				t.Errorf("timeout not clamped to ctx deadline: %s", ms)
			}
		}
	}
}

func TestWaitState_TimeoutReturnsCurrentStatus(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "wait" {
			return nil, fmt.Errorf("timeout") // herdr wait exits non-zero on timeout
		}
		if len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "list" {
			return []byte(`{"result":{"panes":[{"pane_id":"w7:p1","agent_status":"blocked","workspace_id":"w7"}]}}`), nil
		}
		return nil, nil
	}}
	got, err := NewHerdr(f).WaitState(context.Background(), Handle{PaneID: "w7:p1"}, StateDone)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got != StateBlocked {
		t.Errorf("on timeout want current status blocked, got %q", got)
	}
}

func TestRead_Command(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "read" {
			return []byte("recent pane output"), nil
		}
		return nil, nil
	}}
	out, err := NewHerdr(f).Read(context.Background(), Handle{PaneID: "w7:p1"}, 60)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "recent pane output" {
		t.Errorf("read = %q", out)
	}
	hasExactCall(t, f.Snapshot(), "herdr", "pane", "read", "w7:p1", "--source", "recent", "--lines", "60")
}

func TestEvents_EmitsStatusChanges(t *testing.T) {
	var n int32
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "list" {
			st := "working"
			if atomic.AddInt32(&n, 1) >= 2 {
				st = "done"
			}
			return []byte(fmt.Sprintf(`{"result":{"panes":[{"pane_id":"w7:p1","agent_status":%q,"workspace_id":"w7"}]}}`, st)), nil
		}
		return nil, nil
	}}
	h := NewHerdr(f)
	h.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := h.Events(ctx)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	want := []AgentState{StateWorking, StateDone}
	for i, w := range want {
		select {
		case ev := <-ch:
			if ev.PaneID != "w7:p1" || ev.State != w {
				t.Fatalf("event %d = %+v, want {w7:p1 %s}", i, ev, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d (%s)", i, w)
		}
	}
}

func TestResolve_ByLabel(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		switch {
		case len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "list":
			return []byte(`{"result":{"workspaces":[{"workspace_id":"w7","label":"issue-5"},{"workspace_id":"w5","label":"other"}]}}`), nil
		case len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "list":
			return []byte(`{"result":{"panes":[{"pane_id":"w7:p1","agent_status":"working","workspace_id":"w7","cwd":"/home/u/wt-issue-5"}]}}`), nil
		}
		return nil, nil
	}}
	h := NewHerdr(f)

	hd, ok, err := h.Resolve(context.Background(), "issue-5")
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if hd.PaneID != "w7:p1" || hd.Workdir != "/home/u/wt-issue-5" {
		t.Errorf("resolved handle = %+v", hd)
	}

	if _, ok, _ := h.Resolve(context.Background(), "issue-404"); ok {
		t.Errorf("unknown label should not resolve")
	}
}

func TestParseRootPaneID(t *testing.T) {
	id, err := parseRootPaneID([]byte(`{"result":{"root_pane":{"pane_id":"wA:p1"}}}`))
	if err != nil || id != "wA:p1" {
		t.Fatalf("parse = %q, %v", id, err)
	}
	if _, err := parseRootPaneID([]byte(`{"result":{}}`)); err == nil {
		t.Error("expected error when pane id missing")
	}
}
