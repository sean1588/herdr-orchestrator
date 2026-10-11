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

func formatArgs(argvs [][]string) string {
	var b strings.Builder
	for _, a := range argvs {
		fmt.Fprintf(&b, "  herdr %s\n", strings.Join(a, " "))
	}
	return b.String()
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
	// herdr detects the agent on the second pane list, so readiness must poll.
	f := herdrScript{undetected: 1}.fake()
	h := testHerdr(f)
	s := testSpawn()

	hd, err := h.Spawn(context.Background(), s)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if hd.PaneID != "w7:p1" {
		t.Errorf("pane id = %q, want w7:p1 (must be parsed from output, never hardcoded)", hd.PaneID)
	}
	if hd.Workdir != "/home/u/repo/.orchestrator/worktrees/wt-issue-5" {
		t.Errorf("workdir = %q, want /home/u/repo/.orchestrator/worktrees/wt-issue-5", hd.Workdir)
	}

	calls := f.Snapshot()
	// Fresh task: clean slate — delete any stale branch, fetch the base, and branch
	// from origin/<base> so it starts at the just-merged tip, not a stale local base.
	hasExactCall(t, calls, "git", "-C", "/home/u/repo", "branch", "-D", "agent/issue-5")
	hasExactCall(t, calls, "git", "-C", "/home/u/repo", "fetch", "origin", "main")
	// --no-track: the branch must NOT inherit origin/<base> as its upstream, or a
	// bare `git push` under push.default=upstream pushes the agent's work straight
	// to the base branch (this happened — a commit landed on main).
	hasExactCall(t, calls, "git", "-C", "/home/u/repo", "worktree", "add", "-b", "agent/issue-5", "--no-track", "/home/u/repo/.orchestrator/worktrees/wt-issue-5", "origin/main")

	// From the workspace onward the herdr sequence is exactly: create, launch the
	// agent verbatim, poll until herdr detects it, wait for it to take input, and
	// submit the kickoff through herdr's agent prompt. Nothing types into the pane
	// directly.
	herdr := herdrCalls(calls)
	start := slices.IndexFunc(herdr, func(c proc.Call) bool { return slices.Equal(c.Args[:2], []string{"workspace", "create"}) })
	if start < 0 {
		t.Fatalf("no workspace create:\n%s", formatCalls(calls))
	}
	want := [][]string{
		{"workspace", "create", "--cwd", "/home/u/repo/.orchestrator/worktrees/wt-issue-5", "--label", "issue-5", "--no-focus"},
		{"pane", "run", "w7:p1", "claude"},
		{"pane", "list"},
		{"pane", "list"},
		{"agent", "wait", "w7:p1", "--until", "idle", "--until", "blocked", "--timeout", "20000"},
		{"agent", "prompt", "w7:p1", s.Kickoff, "--wait", "--until", "working", "--until", "blocked", "--timeout", "25000"},
	}
	got := herdr[start:]
	if !slices.EqualFunc(got, want, func(c proc.Call, w []string) bool { return slices.Equal(c.Args, w) }) {
		t.Errorf("herdr command sequence mismatch\ngot:\n%swant:\n%s", formatCalls(got), formatArgs(want))
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
	base := herdrScript{pane: "wNew:p1"}.responder()
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "list" {
			return []byte(`{"result":{"workspaces":[{"workspace_id":"wOld","label":"issue-5"}]}}`), nil
		}
		return base(c)
	}}

	hd, err := testHerdr(f).Spawn(context.Background(), testSpawn()) // TaskID == "issue-5"
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if hd.PaneID != "wNew:p1" {
		t.Errorf("pane = %q, want wNew:p1", hd.PaneID)
	}
	hasExactCall(t, f.Snapshot(), "herdr", "workspace", "close", "wOld")
}

// A re-spawn for a task that already has a PR (PreserveBranch) must NOT delete
// the branch — it carries the PR's commits. Instead it fetches and checks out the
// existing branch into a fresh worktree, so a reviewer/resume agent works on the
// PR's state, not an empty branch off base.
func TestSpawn_PreserveBranch_KeepsExistingBranch(t *testing.T) {
	f := herdrScript{pane: "w9:p1"}.fake()
	s := testSpawn()
	s.PreserveBranch = true

	if _, err := testHerdr(f).Spawn(context.Background(), s); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	calls := f.Snapshot()
	// Must NOT delete the branch.
	for _, c := range calls {
		if c.Name == "git" && slices.Contains(c.Args, "branch") && slices.Contains(c.Args, "-D") {
			t.Errorf("PreserveBranch must not run git branch -D: %v", c.Args)
		}
	}
	// Fetch the branch, then check it out (no -b, no base) into the worktree.
	hasExactCall(t, calls, "git", "-C", "/home/u/repo", "fetch", "origin", "agent/issue-5")
	hasExactCall(t, calls, "git", "-C", "/home/u/repo", "worktree", "add", "/home/u/repo/.orchestrator/worktrees/wt-issue-5", "agent/issue-5")
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
	hasExactCall(t, f.Snapshot(), "herdr", "agent", "wait", "w7:p1", "--until", "done", "--timeout", "2700000")
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
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "agent" && c.Args[1] == "wait" {
			ms := c.Args[len(c.Args)-1]
			if ms == "2700000" {
				t.Errorf("timeout not clamped to ctx deadline: %s", ms)
			}
		}
	}
}

func TestWaitState_TimeoutReturnsCurrentStatus(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if len(c.Args) >= 2 && c.Args[0] == "agent" && c.Args[1] == "wait" {
			return nil, fmt.Errorf("timeout") // herdr agent wait exits non-zero on timeout
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
			return []byte(`{"result":{"panes":[{"pane_id":"w7:p1","agent_status":"working","workspace_id":"w7","cwd":"/home/u/repo/.orchestrator/worktrees/wt-issue-5"}]}}`), nil
		}
		return nil, nil
	}}
	h := NewHerdr(f)

	hd, ok, err := h.Resolve(context.Background(), "issue-5")
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if hd.PaneID != "w7:p1" || hd.Workdir != "/home/u/repo/.orchestrator/worktrees/wt-issue-5" {
		t.Errorf("resolved handle = %+v", hd)
	}

	if _, ok, _ := h.Resolve(context.Background(), "issue-404"); ok {
		t.Errorf("unknown label should not resolve")
	}
}

// hasNoCallContaining asserts that no recorded call matches name + the given arg
// subsequence (all args present, order-agnostic membership).
func hasNoCallContaining(t *testing.T, calls []proc.Call, name string, wantArgs ...string) {
	t.Helper()
	for _, c := range calls {
		if c.Name != name {
			continue
		}
		all := true
		for _, a := range wantArgs {
			if !slices.Contains(c.Args, a) {
				all = false
				break
			}
		}
		if all {
			t.Errorf("expected no %s call containing %v, but found: %v", name, wantArgs, c.Args)
		}
	}
}

// wsListResponder answers `herdr workspace list` with a single workspace under
// label (or none when wsID==""); other calls succeed by default.
func wsListResponder(label, wsID string) func(proc.Call) ([]byte, error) {
	return func(c proc.Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "list" {
			if wsID == "" {
				return []byte(`{"result":{"workspaces":[]}}`), nil
			}
			return []byte(fmt.Sprintf(`{"result":{"workspaces":[{"workspace_id":%q,"label":%q}]}}`, wsID, label)), nil
		}
		return nil, nil
	}
}

// cleanupBackend wires a Herdr with deterministic worktree resolution, so a task's
// worktree path is WorktreesDir + "wt-<taskID>" (here /wt/wt-<taskID>) with no live
// pane required.
func cleanupBackend(r proc.Runner) *Herdr {
	h := NewHerdr(r)
	h.WorktreesDir = "/wt"
	h.RepoDir = "/repo"
	return h
}

// The default worktrees dir lives inside the checkout, so the folder trust the
// user granted the repo covers every spawn; an explicit --worktrees-dir wins.
func TestWorktreeDir(t *testing.T) {
	tests := []struct {
		name         string
		worktreesDir string
		want         string
	}{
		{name: "default is inside the repo", want: "/home/u/repo/.orchestrator/worktrees/wt-issue-5"},
		{name: "explicit dir wins", worktreesDir: "/elsewhere", want: "/elsewhere/wt-issue-5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHerdr(&proc.Fake{})
			h.WorktreesDir = tc.worktreesDir
			if got := h.worktreeDir("/home/u/repo", "issue-5"); got != tc.want {
				t.Errorf("worktreeDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// Cleanup removes the task's worktree at its DETERMINISTIC path and closes its
// herdr workspace, keyed by the durable taskID label.
func TestCleanup_RemovesWorktreeAndClosesWorkspace(t *testing.T) {
	f := &proc.Fake{Responder: wsListResponder("issue-5", "wZ")}
	if err := cleanupBackend(f).Cleanup(context.Background(), "issue-5"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	calls := f.Snapshot()
	hasExactCall(t, calls, "git", "-C", "/repo", "worktree", "remove", "/wt/wt-issue-5", "--force")
	hasExactCall(t, calls, "herdr", "workspace", "close", "wZ")
}

// A failed-drive escalation closes the agent pane before cleanup runs: the workspace
// is still registered but no pane carries it. Cleanup must STILL remove the worktree
// via its deterministic path — the case the old live-pane lookup silently missed.
func TestCleanup_NoLivePane_StillRemovesWorktree(t *testing.T) {
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		switch {
		case c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "list":
			return []byte(`{"result":{"workspaces":[{"workspace_id":"wZ","label":"issue-5"}]}}`), nil
		case c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "pane" && c.Args[1] == "list":
			return []byte(`{"result":{"panes":[]}}`), nil // pane already gone
		}
		return nil, nil
	}}
	if err := cleanupBackend(f).Cleanup(context.Background(), "issue-5"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	calls := f.Snapshot()
	hasExactCall(t, calls, "git", "-C", "/repo", "worktree", "remove", "/wt/wt-issue-5", "--force")
	hasExactCall(t, calls, "herdr", "workspace", "close", "wZ")
}

// An unknown label => nothing registered; Cleanup is a no-op (no worktree removal,
// no workspace close) and errors nothing (idempotent-safe re-run).
func TestCleanup_UnknownLabel_NoOp(t *testing.T) {
	f := &proc.Fake{Responder: wsListResponder("issue-5", "")}
	if err := cleanupBackend(f).Cleanup(context.Background(), "issue-404"); err != nil {
		t.Fatalf("cleanup unknown label should be a no-op, got %v", err)
	}
	calls := f.Snapshot()
	hasNoCallContaining(t, calls, "git", "worktree", "remove")
	hasNoCallContaining(t, calls, "herdr", "close")
}

// A failing worktree removal (e.g. already gone) must not fail cleanup: the
// worktree remove is best-effort; the workspace is still closed.
func TestCleanup_WorktreeRemoveFailure_StillClosesWorkspace(t *testing.T) {
	base := wsListResponder("issue-5", "wZ")
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if c.Name == "git" && slices.Contains(c.Args, "worktree") && slices.Contains(c.Args, "remove") {
			return nil, fmt.Errorf("fatal: cannot remove worktree")
		}
		return base(c)
	}}
	if err := cleanupBackend(f).Cleanup(context.Background(), "issue-5"); err != nil {
		t.Fatalf("worktree-remove failure must be swallowed, got %v", err)
	}
	hasExactCall(t, f.Snapshot(), "herdr", "workspace", "close", "wZ")
}

// A failing workspace close is surfaced (the engine logs it and continues), so
// the caller can observe cleanup that only partially succeeded.
func TestCleanup_WorkspaceCloseFailure_IsReturned(t *testing.T) {
	base := wsListResponder("issue-5", "wZ")
	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) >= 2 && c.Args[0] == "workspace" && c.Args[1] == "close" {
			return nil, fmt.Errorf("herdr unavailable")
		}
		return base(c)
	}}
	if err := cleanupBackend(f).Cleanup(context.Background(), "issue-5"); err == nil {
		t.Fatal("workspace-close failure should be returned")
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

/* -------------------------------------------------------------------------- */
/* Readiness and kickoff delivery                                              */
/*                                                                             */
/* Both go through herdr's agent detection: the pane must be detected as an   */
/* agent, then `agent prompt` submits and reports whether the text took. These */
/* pin how herdr's JSON error codes map onto a spawn or message outcome.       */
/* -------------------------------------------------------------------------- */

// herdrScript models the herdr surface launch -> readiness -> delivery drives.
type herdrScript struct {
	pane string // root pane `workspace create` answers with; "" => "w7:p1"
	// undetected is how many `pane list` calls show the launched pane before herdr
	// detects its agent; negative means never.
	undetected int
	// prompts are the successive `agent prompt` answers: "" exits 0, anything else
	// is the error code in herdr's stderr envelope. Calls past the end succeed.
	prompts []string
}

// herdrErr mimics a failed herdr call as proc.Runner reports it: the full argv
// (prompt text included), then herdr's JSON error envelope from stderr.
func herdrErr(args []string, code string) error {
	return fmt.Errorf("herdr %s: exit status 1: {\"error\":{\"code\":%q,\"message\":\"synthetic\"},\"id\":\"cli:%s:%s\"}",
		strings.Join(args, " "), code, args[0], args[1])
}

// explainBlocked is a hand-written `agent explain --json` body for an agent held
// at a folder-trust dialog; only matched_rule.id matters to the backend.
const explainBlocked = `{"agent":"some-agent","state":"blocked","matched_rule":{"id":"trust_directory","priority":900,"region":"screen","state":"blocked"},"evaluated_rules":[]}`

func (s herdrScript) responder() func(proc.Call) ([]byte, error) {
	pane := s.pane
	if pane == "" {
		pane = "w7:p1"
	}
	var lists, prompts atomic.Int32
	return func(c proc.Call) ([]byte, error) {
		if c.Name != "herdr" || len(c.Args) < 2 {
			return nil, nil
		}
		switch c.Args[0] + " " + c.Args[1] {
		case "workspace create":
			return []byte(`{"result":{"root_pane":{"pane_id":"` + pane + `"}}}`), nil
		case "pane list":
			agent := ""
			if n := int(lists.Add(1)); s.undetected >= 0 && n > s.undetected {
				agent = `"agent":"some-agent",`
			}
			return []byte(`{"result":{"panes":[{"pane_id":"` + pane + `",` + agent + `"agent_status":"idle","workspace_id":"w7"}]}}`), nil
		case "agent prompt":
			if i := int(prompts.Add(1)) - 1; i < len(s.prompts) && s.prompts[i] != "" {
				return nil, herdrErr(c.Args, s.prompts[i])
			}
		case "agent explain":
			return []byte(explainBlocked), nil
		}
		return nil, nil
	}
}

func (s herdrScript) fake() *proc.Fake { return &proc.Fake{Responder: s.responder()} }

// testHerdr is a backend with sub-second readiness pacing so detection polling
// does not sleep through tests.
func testHerdr(r proc.Runner) *Herdr {
	h := NewHerdr(r)
	h.PollInterval = time.Millisecond
	return h
}

// herdrCalls returns the herdr calls whose first two args are sub, e.g.
// ("agent", "prompt").
func herdrCalls(calls []proc.Call, sub ...string) []proc.Call {
	var out []proc.Call
	for _, c := range calls {
		if c.Name == "herdr" && len(c.Args) >= len(sub) && slices.Equal(c.Args[:len(sub)], sub) {
			out = append(out, c)
		}
	}
	return out
}

func TestSpawn_AgentBlocked_NamesTheRule(t *testing.T) {
	f := herdrScript{prompts: []string{"agent_blocked"}}.fake()
	_, err := testHerdr(f).Spawn(context.Background(), testSpawn())
	if err == nil || !strings.Contains(err.Error(), "trust_directory") {
		t.Fatalf("Spawn error = %v, want one naming the trust_directory rule", err)
	}
	calls := f.Snapshot()
	// agent_blocked means herdr refused to type into a dialog; re-sending cannot help.
	if n := len(herdrCalls(calls, "agent", "prompt")); n != 1 {
		t.Errorf("agent prompt calls = %d, want exactly 1", n)
	}
	hasExactCall(t, calls, "herdr", "agent", "explain", "w7:p1", "--json")
}

func TestSpawn_PromptStalledOnce_RetriesAndSucceeds(t *testing.T) {
	f := herdrScript{prompts: []string{"agent_prompt_stalled", ""}}.fake()
	if _, err := testHerdr(f).Spawn(context.Background(), testSpawn()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if n := len(herdrCalls(f.Snapshot(), "agent", "prompt")); n != 2 {
		t.Errorf("agent prompt calls = %d, want exactly 2 (one stall, one retry)", n)
	}
}

func TestSpawn_PromptStalledTwice_FailsWithRule(t *testing.T) {
	f := herdrScript{prompts: []string{"agent_prompt_stalled", "agent_prompt_stalled"}}.fake()
	_, err := testHerdr(f).Spawn(context.Background(), testSpawn())
	if err == nil || !strings.Contains(err.Error(), "stalled") || !strings.Contains(err.Error(), "trust_directory") {
		t.Fatalf("Spawn error = %v, want a stall naming the detection rule", err)
	}
	if n := len(herdrCalls(f.Snapshot(), "agent", "prompt")); n != 2 {
		t.Errorf("agent prompt calls = %d, want exactly 2 (retried once, no more)", n)
	}
}

func TestSpawn_PromptOtherError_IsWrapped(t *testing.T) {
	// A code that is neither blocked nor stalled (here herdr's own timeout) is not
	// retried and is surfaced as-is, without an explain call.
	f := herdrScript{prompts: []string{"timeout"}}.fake()
	_, err := testHerdr(f).Spawn(context.Background(), testSpawn())
	if err == nil || !strings.Contains(err.Error(), "deliver kickoff on w7:p1") {
		t.Fatalf("Spawn error = %v, want a wrapped delivery error", err)
	}
	calls := f.Snapshot()
	if n := len(herdrCalls(calls, "agent", "prompt")); n != 1 {
		t.Errorf("agent prompt calls = %d, want exactly 1", n)
	}
	if n := len(herdrCalls(calls, "agent", "explain")); n != 0 {
		t.Errorf("agent explain calls = %d, want 0", n)
	}
}

func TestSpawn_AgentNeverDetected_FailsWithinReadyTimeout(t *testing.T) {
	f := herdrScript{undetected: -1}.fake()
	h := testHerdr(f)
	h.ReadyTimeout = 50 * time.Millisecond
	start := time.Now()
	_, err := h.Spawn(context.Background(), testSpawn())
	if err == nil || !strings.Contains(err.Error(), "no agent detected") || !strings.Contains(err.Error(), `"claude"`) {
		t.Fatalf("Spawn error = %v, want one naming the undetected launch argv", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Spawn took %s, want it bounded by ReadyTimeout (50ms)", elapsed)
	}
	calls := f.Snapshot()
	if n := len(herdrCalls(calls, "agent")); n != 0 {
		t.Errorf("no agent command may run on an undetected pane, got:\n%s", formatCalls(herdrCalls(calls, "agent")))
	}
}

func TestHerdrErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"envelope in error text", herdrErr([]string{"agent", "prompt", "w1:p1", "push the branch", "--wait"}, "agent_blocked"), "agent_blocked"},
		{"envelope-looking fragment in the prompt text",
			herdrErr([]string{"agent", "prompt", "w1:p1", `see {"error" here`, "--wait"}, "agent_blocked"), "agent_blocked"},
		{"whole envelope quoted in the prompt text",
			herdrErr([]string{"agent", "prompt", "w1:p1", `quoting {"error":{"code":"timeout"}}`, "--wait"}, "agent_blocked"), "agent_blocked"},
		{"no envelope", fmt.Errorf("herdr agent prompt: exit status 1: connection refused"), ""},
		{"malformed envelope", fmt.Errorf(`herdr: exit status 1: {"error":{"code":`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := herdrErrorCode(tc.err); got != tc.want {
				t.Errorf("herdrErrorCode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMessage_DeliversThroughAgentPrompt(t *testing.T) {
	const text = "the token scope is fixed; push the branch now"
	hd := Handle{PaneID: "w7:p1", Workdir: "/wt"}
	cases := []struct {
		name    string
		prompts []string
		wantErr string
	}{
		{"accepted", nil, ""},
		{"blocked at a dialog", []string{"agent_blocked"}, "trust_directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := herdrScript{prompts: tc.prompts}.fake()
			err := testHerdr(f).Message(context.Background(), hd, text)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Message: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Message error = %v, want one containing %q", err, tc.wantErr)
			}
			calls := f.Snapshot()
			if n := len(herdrCalls(calls, "agent", "prompt")); n != 1 {
				t.Errorf("agent prompt calls = %d, want exactly 1", n)
			}
			hasExactCall(t, calls, "herdr", "agent", "prompt", "w7:p1", text, "--wait", "--until", "working", "--until", "blocked", "--timeout", "25000")
		})
	}
}
