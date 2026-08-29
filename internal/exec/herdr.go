package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// waitBudgetSlack is how far WaitState's per-call subprocess budget sits above
// the --timeout it hands herdr, so the budget is a backstop for a herdr that
// ignores its own bound rather than a second, competing deadline.
const waitBudgetSlack = 30 * time.Second

// Herdr is the herdr-backed ExecutionBackend. It wraps the same git + herdr CLI
// commands proven in Spike 0, run through a proc.Runner so command construction
// is unit-testable.
type Herdr struct {
	r proc.Runner

	GitBin       string        // default "git"
	HerdrBin     string        // default "herdr"
	WorktreesDir string        // parent dir for worktrees; "" => sibling of the repo
	RepoDir      string        // main checkout; lets Cleanup resolve a task's worktree path without a live pane
	ReadyMatch   string        // readiness marker awaited before the kickoff; default ">"
	ReadyTimeout time.Duration // bound on the readiness wait; default 20s
	WaitTimeout  time.Duration // WaitState bound when ctx has no sooner deadline; default 45m
	PollInterval time.Duration // Events poll cadence; default 2s
	// SubmitDelay is how long to wait after typing the kickoff before sending the
	// submitting Enter. Claude Code's Ink TUI swallows a too-early Enter, so the
	// text + Enter must be separated; default 1s (0 in tests).
	SubmitDelay time.Duration

	// KickoffAckTimeout bounds how long Spawn waits for evidence that a delivered
	// kickoff was actually accepted before trying the other delivery method;
	// default 25s. KickoffAckPoll is that wait's polling cadence; default 1s.
	KickoffAckTimeout time.Duration
	KickoffAckPoll    time.Duration

	// hub multiplexes one pane-list poller across all Events subscribers, reading
	// PollInterval at poller start so a test-set interval still applies.
	hub *eventHub
}

var _ ExecutionBackend = (*Herdr)(nil)

// NewHerdr returns a Herdr backend with Spike-0 defaults.
func NewHerdr(r proc.Runner) *Herdr {
	h := &Herdr{
		r:            r,
		GitBin:       "git",
		HerdrBin:     "herdr",
		ReadyMatch:   ">",
		ReadyTimeout: 20 * time.Second,
		WaitTimeout:  45 * time.Minute,
		PollInterval: defaultEventPollInterval,
		SubmitDelay:  1 * time.Second,

		KickoffAckTimeout: 25 * time.Second,
		KickoffAckPoll:    1 * time.Second,
	}
	h.hub = newEventHub(h.listPanes, func() time.Duration { return h.PollInterval })
	return h
}

// Spawn: git worktree (isolated) -> herdr workspace -> launch agent -> readiness
// -> single-line kickoff. The multi-line task body lives in Spawn.TaskFile; only
// the single-line kickoff is ever sent through the pane (Spike 0 finding).
func (h *Herdr) Spawn(ctx context.Context, s Spawn) (Handle, error) {
	wt := h.worktreePath(s)

	// Best-effort cleanup of any prior attempt's worktree (ignore errors).
	_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "worktree", "remove", wt, "--force")
	// A prior attempt (e.g. an escalation/re-drive) can leave the worktree path as a
	// stray directory git no longer tracks, so the `worktree remove` above is a no-op
	// on it. Clear the dir and prune the registry so the `worktree add` below doesn't
	// fail with "already exists" and retry forever.
	_ = os.RemoveAll(wt)
	_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "worktree", "prune")
	if !s.PreserveBranch {
		// Fresh task: discard any stale branch so we recreate a clean slate. A
		// re-spawn (PreserveBranch) must keep the branch — it carries the PR.
		_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "branch", "-D", s.Branch)
	}
	// Symmetric herdr-side cleanup: close any prior workspace with this label so a
	// re-spawn doesn't create a duplicate label (which would break Resolve, since
	// it matches workspaces by label).
	if wsID, err := h.workspaceByLabel(ctx, s.TaskID); err == nil && wsID != "" {
		_, _ = h.r.Run(ctx, "", h.HerdrBin, "workspace", "close", wsID)
	}

	if err := h.addWorktree(ctx, s, wt); err != nil {
		return Handle{}, err
	}

	out, err := h.r.Run(ctx, "", h.HerdrBin, "workspace", "create", "--cwd", wt, "--label", s.TaskID, "--no-focus")
	if err != nil {
		return Handle{}, fmt.Errorf("create herdr workspace: %w", err)
	}
	pane, err := parseRootPaneID(out)
	if err != nil {
		return Handle{}, err
	}
	hd := Handle{PaneID: pane, Workdir: wt}

	// Launch the agent process verbatim. We never inject --dangerously-skip-permissions
	// (Claude Code refuses it as root; honor run_as: non_root).
	if _, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "run", pane, strings.Join(s.Launch, " ")); err != nil {
		return hd, fmt.Errorf("launch agent on %s: %w", pane, err)
	}

	// Readiness: wait for the prompt before sending the kickoff, instead of a
	// fixed sleep (Spike 0). A timeout here is non-fatal — proceed to the kickoff.
	_, _ = h.r.Run(ctx, "", h.HerdrBin, "pane", "wait-output", pane, "--match", h.ReadyMatch, "--timeout", msString(h.ReadyTimeout))

	if err := h.deliverKickoff(ctx, pane, s.Kickoff); err != nil {
		return hd, err
	}
	return hd, nil
}

// deliverKickoff gets the single-line kickoff into the agent's prompt and proves
// it was accepted.
//
// Neither delivery method is reliable on its own, and both have failed in
// production against different Claude Code builds:
//
//   - `pane run` (text + Enter in one call) raced Claude Code's Ink renderer,
//     which swallowed the Enter and left the kickoff sitting unsent.
//   - `pane send-text` + a separate `pane send-keys Enter` fixed that, then
//     stopped landing entirely on Claude Code v2.1.236: the text never reached
//     the prompt box at all.
//
// A dropped kickoff used to be silent — the agent sat at an empty prompt, herdr
// reported it blocked, and the task burned its whole blocked_timeout before
// escalating with nothing to show. So delivery is now *verified*: send, then
// watch the agent's status leave its pre-kickoff state. An agent that received
// its instruction starts working; one that didn't stays put. If the first method
// doesn't take, try the other before giving up, and if neither does, fail the
// spawn loudly rather than handing the engine a mute agent.
func (h *Herdr) deliverKickoff(ctx context.Context, pane, kickoff string) error {
	before := h.currentStatus(ctx, pane)

	twoStep := func() error {
		if _, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "send-text", pane, kickoff); err != nil {
			return fmt.Errorf("send kickoff text on %s: %w", pane, err)
		}
		if h.SubmitDelay > 0 {
			time.Sleep(h.SubmitDelay)
		}
		if _, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "send-keys", pane, "Enter"); err != nil {
			return fmt.Errorf("submit kickoff on %s: %w", pane, err)
		}
		return nil
	}
	oneShot := func() error {
		if _, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "run", pane, kickoff); err != nil {
			return fmt.Errorf("run kickoff on %s: %w", pane, err)
		}
		return nil
	}

	// Verification off (KickoffAckTimeout <= 0): deliver once and trust it. Used
	// by tests, which drive a fake runner that models no pane status.
	if h.KickoffAckTimeout <= 0 {
		return twoStep()
	}

	var lastErr error
	for i, send := range []func() error{twoStep, oneShot} {
		if err := send(); err != nil {
			lastErr = err
			continue // the other method may still get through
		}
		if h.kickoffAccepted(ctx, pane, before) {
			return nil
		}
		lastErr = fmt.Errorf("kickoff not accepted on %s (agent still %s after delivery attempt %d)", pane, before, i+1)
	}
	return fmt.Errorf("deliver kickoff on %s: %w", pane, lastErr)
}

// kickoffAccepted polls the agent's status for evidence the kickoff landed: an
// agent that read its instruction moves off whatever it was doing before (a
// freshly-launched one sits idle or blocked at an empty prompt and starts
// working). Bounded by KickoffAckTimeout; a false here means "no evidence", not
// "definitely dropped", which is why the caller tries the other method rather
// than escalating on the first miss.
func (h *Herdr) kickoffAccepted(ctx context.Context, pane string, before AgentState) bool {
	deadline := time.Now().Add(h.KickoffAckTimeout)
	for {
		switch st := h.currentStatus(ctx, pane); st {
		case StateWorking, StateDone:
			return true
		case before:
			// no movement yet
		default:
			// Any other change (e.g. idle -> blocked on a permission prompt) still
			// proves the agent read something.
			if st != StateUnknown {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(h.KickoffAckPoll):
		}
	}
}

// addWorktree creates the isolated worktree for a spawn. A fresh task branches
// from base; a re-spawn for a task with an existing PR (PreserveBranch) checks
// out the existing branch after fetching it, so the PR's commits are preserved
// rather than discarded by a recreate-from-base.
func (h *Herdr) addWorktree(ctx context.Context, s Spawn, wt string) error {
	if s.PreserveBranch {
		_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "fetch", "origin", s.Branch)
		if _, err := h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "worktree", "add", wt, s.Branch); err != nil {
			return fmt.Errorf("create worktree %s on existing branch %s: %w", wt, s.Branch, err)
		}
		return nil
	}
	// Fetch the base and branch from origin/<base>, not the local base: a fresh task
	// must start from the just-merged tip, or a worktree created after an earlier
	// task merged would miss those commits (a stale-base merge conflict later).
	_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "fetch", "origin", s.Base)
	// --no-track is a safety rail, not a preference. Branching from a remote-tracking
	// ref otherwise sets branch.<b>.merge = refs/heads/<base>, and a user whose git
	// has push.default = upstream (or tracking) turns a bare `git push` in the agent's
	// worktree into a push straight to <base> — an agent landed a commit on main this
	// way. Without an upstream, a bare push fails loudly with "use --set-upstream"
	// instead, and the kickoff's explicit `git push -u origin <branch>` still works.
	if _, err := h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "worktree", "add", "-b", s.Branch, "--no-track", wt, "origin/"+s.Base); err != nil {
		return fmt.Errorf("create worktree %s: %w", wt, err)
	}
	return nil
}

// WaitState blocks until the pane's agent status reaches target, bounded by the
// sooner of ctx's deadline and WaitTimeout. On timeout it returns the agent's
// best-known current status alongside the error.
func (h *Herdr) WaitState(ctx context.Context, hd Handle, target AgentState) (AgentState, error) {
	timeout := h.WaitTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < timeout {
			timeout = rem
		}
	}
	// This is the one command that blocks by design, so the runner's default
	// per-call budget would kill it long before herdr's own --timeout fired. Give
	// it a budget just above that timeout: herdr stays the primary bound, and a
	// herdr that ignores its own --timeout still cannot hang the caller forever.
	waitCtx := proc.WithBudget(ctx, timeout+waitBudgetSlack)
	_, err := h.r.Run(waitCtx, "", h.HerdrBin, "agent", "wait", hd.PaneID, "--until", string(target), "--timeout", msString(timeout))
	if err != nil {
		return h.currentStatus(ctx, hd.PaneID), fmt.Errorf("agent wait %s on %s: %w", target, hd.PaneID, err)
	}
	return target, nil
}

// Read returns the last `lines` of recent pane output.
func (h *Herdr) Read(ctx context.Context, hd Handle, lines int) (string, error) {
	out, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "read", hd.PaneID, "--source", "recent", "--lines", strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("pane read %s: %w", hd.PaneID, err)
	}
	return string(out), nil
}

// Events returns a stream of agent status changes across all panes, primed with
// the current pane states and then carrying live changes. The engine filters by
// its task's pane id. All subscribers share one `herdr pane list` poller (see
// eventHub); the subscription ends when ctx is done. This is the liveness stream;
// GitHub remains authoritative for artifacts.
func (h *Herdr) Events(ctx context.Context) (<-chan Event, error) {
	return h.hub.subscribe(ctx), nil
}

// Resolve maps a durable workspace label (= Spawn.TaskID) back to its current
// volatile pane, for crash recovery.
func (h *Herdr) Resolve(ctx context.Context, label string) (Handle, bool, error) {
	wsID, err := h.workspaceByLabel(ctx, label)
	if err != nil {
		return Handle{}, false, err
	}
	if wsID == "" {
		return Handle{}, false, nil
	}
	panes, err := h.listPanes(ctx)
	if err != nil {
		return Handle{}, false, err
	}
	for _, p := range panes {
		if p.WorkspaceID == wsID {
			return Handle{PaneID: p.PaneID, Workdir: p.Cwd}, true, nil
		}
	}
	return Handle{}, false, nil
}

// Close tears down the agent's herdr workspace (best-effort). The on-disk git
// worktree is left in place for inspection; Cleanup removes it on the settled path.
func (h *Herdr) Close(ctx context.Context, hd Handle) error {
	wsID, err := h.workspaceForPane(ctx, hd.PaneID)
	if err != nil {
		return err
	}
	if wsID == "" {
		return nil
	}
	if _, err := h.r.Run(ctx, "", h.HerdrBin, "workspace", "close", wsID); err != nil {
		return fmt.Errorf("close workspace %s: %w", wsID, err)
	}
	return nil
}

// Cleanup removes a settled task's isolated git worktree and closes its herdr
// workspace, keyed by the durable taskID label. It runs when a task halts at a
// no-PR terminal state (a triage reject -> closed, or a needs_human / failed-drive
// escalation -> escalated), so a settled issue leaves nothing registered.
//
// The worktree path is resolved deterministically (the same convention Spawn uses),
// so cleanup works even after the agent pane has exited — the exact state a
// failed-drive escalation produces. Both operations are best-effort and
// idempotent-safe: an already-removed worktree or already-closed workspace must not
// fail the drive. Only a failure to close a still-present workspace is surfaced (the
// engine logs it and continues).
func (h *Herdr) Cleanup(ctx context.Context, taskID string) error {
	wsID, err := h.workspaceByLabel(ctx, taskID)
	if err != nil {
		return fmt.Errorf("cleanup %s: %w", taskID, err)
	}
	if wsID == "" {
		return nil // nothing registered under this label — already clean
	}
	// Remove the worktree at its deterministic path (mirrors Spawn's prior-attempt
	// cleanup, run with -C RepoDir). Best-effort: an already-gone worktree is fine,
	// and this does not depend on a live agent pane.
	wt := h.worktreeDir(h.RepoDir, taskID)
	_, _ = h.r.Run(ctx, "", h.GitBin, "-C", h.RepoDir, "worktree", "remove", wt, "--force")
	if _, err := h.r.Run(ctx, "", h.HerdrBin, "workspace", "close", wsID); err != nil {
		return fmt.Errorf("cleanup %s: close workspace %s: %w", taskID, wsID, err)
	}
	return nil
}

// --- helpers ---

func (h *Herdr) worktreePath(s Spawn) string { return h.worktreeDir(s.RepoDir, s.TaskID) }

// worktreeDir is a task's deterministic worktree path: WorktreesDir (or the repo's
// sibling dir when unset) + "wt-<taskID>". Shared by Spawn (via the Spawn's RepoDir)
// and Cleanup (via the backend's RepoDir) so setup and teardown agree on the path.
func (h *Herdr) worktreeDir(repoDir, taskID string) string {
	base := h.WorktreesDir
	if base == "" {
		base = filepath.Dir(repoDir)
	}
	return filepath.Join(base, "wt-"+taskID)
}

type paneInfo struct {
	PaneID      string `json:"pane_id"`
	AgentStatus string `json:"agent_status"`
	WorkspaceID string `json:"workspace_id"`
	Cwd         string `json:"cwd"`
	Agent       string `json:"agent"`
}

func (h *Herdr) listPanes(ctx context.Context) ([]paneInfo, error) {
	out, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "list")
	if err != nil {
		return nil, fmt.Errorf("pane list: %w", err)
	}
	var resp struct {
		Result struct {
			Panes []paneInfo `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("decode pane list: %w", err)
	}
	return resp.Result.Panes, nil
}

func (h *Herdr) workspaceByLabel(ctx context.Context, label string) (string, error) {
	out, err := h.r.Run(ctx, "", h.HerdrBin, "workspace", "list")
	if err != nil {
		return "", fmt.Errorf("workspace list: %w", err)
	}
	var resp struct {
		Result struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Label       string `json:"label"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("decode workspace list: %w", err)
	}
	for _, w := range resp.Result.Workspaces {
		if w.Label == label {
			return w.WorkspaceID, nil
		}
	}
	return "", nil
}

func (h *Herdr) workspaceForPane(ctx context.Context, paneID string) (string, error) {
	panes, err := h.listPanes(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return p.WorkspaceID, nil
		}
	}
	return "", nil
}

func (h *Herdr) currentStatus(ctx context.Context, paneID string) AgentState {
	panes, err := h.listPanes(ctx)
	if err != nil {
		return StateUnknown
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return normalizeState(p.AgentStatus)
		}
	}
	return StateUnknown
}

func parseRootPaneID(out []byte) (string, error) {
	var resp struct {
		Result struct {
			RootPane struct {
				PaneID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("decode workspace create response: %w", err)
	}
	if resp.Result.RootPane.PaneID == "" {
		return "", fmt.Errorf("no result.root_pane.pane_id in workspace create response")
	}
	return resp.Result.RootPane.PaneID, nil
}

func normalizeState(s string) AgentState {
	switch AgentState(s) {
	case StateIdle, StateWorking, StateBlocked, StateDone:
		return AgentState(s)
	default:
		return StateUnknown
	}
}

func msString(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatInt(ms, 10)
}
