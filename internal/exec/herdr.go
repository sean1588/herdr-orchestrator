package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// smokeLabel is the herdr workspace label SmokeKickoff claims. It is fixed (not
// per-run) so a crashed preflight leaves exactly one stale workspace, which the
// next run closes, rather than accumulating one per attempt.
const smokeLabel = "orchestratord-doctor"

// waitBudgetSlack is how far WaitState's per-call subprocess budget sits above
// the --timeout it hands herdr, so the budget is a backstop for a herdr that
// ignores its own bound rather than a second, competing deadline.
const waitBudgetSlack = 30 * time.Second

// The herdr versions this backend's command surface (the subcommands it runs and
// the JSON fields it parses) has been driven against end to end. doctor judges
// the installed herdr against this range. A herdr release bump touches only
// these two constants: run the full doctor (kickoff smoke test included) on the
// new herdr, drive one issue to merged, then raise HerdrTestedMax.
const (
	HerdrTestedMin = "0.8.2" // oldest herdr the backend's command surface was driven against
	HerdrTestedMax = "0.9.1" // newest; bump after a full doctor + one merged task on it
)

// Herdr is the herdr-backed ExecutionBackend. It wraps the same git + herdr CLI
// commands proven in Spike 0, run through a proc.Runner so command construction
// is unit-testable.
type Herdr struct {
	r proc.Runner

	GitBin       string // default "git"
	HerdrBin     string // default "herdr"
	WorktreesDir string // parent dir for worktrees; "" => <repo>/.orchestrator/worktrees
	RepoDir      string // main checkout; lets Cleanup resolve a task's worktree path without a live pane
	// ReadyTimeout bounds each half of readiness: herdr detecting an agent on the
	// launched pane, then that agent reaching a state that takes input; default 20s.
	ReadyTimeout time.Duration
	WaitTimeout  time.Duration // WaitState bound when ctx has no sooner deadline; default 45m
	PollInterval time.Duration // Events and agent-detection poll cadence; default 2s

	// KickoffAckTimeout is the --timeout handed to `herdr agent prompt`: how long a
	// delivered kickoff has to show the agent working (or blocked); default 25s.
	KickoffAckTimeout time.Duration

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
		ReadyTimeout: 20 * time.Second,
		WaitTimeout:  45 * time.Minute,
		PollInterval: defaultEventPollInterval,

		KickoffAckTimeout: 25 * time.Second,
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
	if err := h.launchAndKickoff(ctx, pane, s.Launch, s.Kickoff); err != nil {
		return hd, err
	}
	return hd, nil
}

// SmokeKickoff proves the kickoff-delivery path end to end without a task: it
// creates a scratch workspace at dir, launches launch verbatim, delivers kickoff
// through the SAME verified path Spawn uses, and tears the workspace down.
//
// It exists because kickoff delivery has broken twice from underneath us — once
// when herdr's CLI changed, once when a Claude Code build stopped accepting
// typed text — and both times the failure was discovered by a task escalating
// with zero work done. Exercising the real delivery code (rather than a
// reimplementation of it) is the whole point: a smoke test that takes its own
// path would keep passing through exactly the regressions this is meant to catch.
//
// The workspace is closed on every exit path, including a failed launch.
func (h *Herdr) SmokeKickoff(ctx context.Context, dir string, launch []string, kickoff string) error {
	if len(launch) == 0 {
		return errors.New("smoke kickoff: no launch argv")
	}
	out, err := h.r.Run(ctx, "", h.HerdrBin, "workspace", "create", "--cwd", dir, "--label", smokeLabel, "--no-focus")
	if err != nil {
		return fmt.Errorf("create scratch workspace: %w", err)
	}
	pane, err := parseRootPaneID(out)
	if err != nil {
		return err
	}
	defer func() {
		// Best-effort teardown on every path: a scratch workspace left behind
		// would collide with the next run's label and break Resolve-by-label.
		if wsID, werr := h.workspaceByLabel(context.WithoutCancel(ctx), smokeLabel); werr == nil && wsID != "" {
			_, _ = h.r.Run(context.WithoutCancel(ctx), "", h.HerdrBin, "workspace", "close", wsID)
		}
	}()

	return h.launchAndKickoff(ctx, pane, launch, kickoff)
}

// launchAndKickoff is the one launch -> readiness -> delivery path, shared by
// Spawn and SmokeKickoff so the smoke test exercises exactly what a task does.
//
// Readiness and delivery both go through herdr's own agent detection rather
// than anything this package knows about a particular agent's TUI: herdr
// recognizes the agent kind, reports when it is ready for input, and confirms a
// submitted prompt took. The launch argv is run verbatim; we never inject
// --dangerously-skip-permissions (Claude Code refuses it as root; honor
// run_as: non_root).
func (h *Herdr) launchAndKickoff(ctx context.Context, pane string, launch []string, kickoff string) error {
	argv := strings.Join(launch, " ")
	if _, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "run", pane, argv); err != nil {
		return fmt.Errorf("launch %q on %s: %w", argv, pane, err)
	}
	if err := h.awaitAgent(ctx, pane, argv); err != nil {
		return err
	}
	return h.deliverKickoff(ctx, pane, kickoff)
}

// awaitAgent waits for herdr to detect an agent on pane, then for that agent to
// be ready for input (idle, or blocked on a dialog the delivery step will name).
//
// A pane herdr never recognizes is a spawn error: whatever launch started is not
// an agent herdr can drive, and no kickoff would reach it. The readiness wait
// that follows is not fatal on timeout — the prompt is the authoritative check,
// and it fails with the reason when the agent cannot take input.
func (h *Herdr) awaitAgent(ctx context.Context, pane, argv string) error {
	deadline := time.Now().Add(h.ReadyTimeout)
	for !h.agentDetected(ctx, pane) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("no agent detected on %s within %s of launching %q", pane, h.ReadyTimeout, argv)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await agent on %s: %w", pane, ctx.Err())
		case <-time.After(h.PollInterval):
		}
	}
	_, _ = h.r.Run(ctx, "", h.HerdrBin, "agent", "wait", pane, "--until", "idle", "--until", "blocked", "--timeout", msString(h.ReadyTimeout))
	return nil
}

// deliverKickoff submits text to the agent on pane and returns nil once herdr
// has seen it take: the agent started working, or went blocked right after the
// submission (the engine's blocked handling takes it from there).
//
// herdr answers with a JSON error code when it did not take. agent_prompt_stalled
// (nothing moved within herdr's stall window) is retried once; agent_blocked (a
// dialog is up, so herdr refused to type into it) is not. Either way the error
// carries the detection rule herdr matched, so an escalation says what the agent
// is stuck on (e.g. trust_directory) instead of a bare "not accepted".
func (h *Herdr) deliverKickoff(ctx context.Context, pane, text string) error {
	prompt := func() (string, error) {
		_, err := h.r.Run(ctx, "", h.HerdrBin, "agent", "prompt", pane, text, "--wait", "--until", "working", "--until", "blocked", "--timeout", msString(h.KickoffAckTimeout))
		return herdrErrorCode(err), err
	}
	code, err := prompt()
	if code == "agent_prompt_stalled" {
		code, err = prompt()
	}
	switch {
	case err == nil:
		return nil
	case code == "agent_blocked":
		return fmt.Errorf("agent blocked before kickoff on %s: %s", pane, h.detectionRule(ctx, pane))
	case code == "agent_prompt_stalled":
		return fmt.Errorf("kickoff stalled twice on %s: %s", pane, h.detectionRule(ctx, pane))
	default:
		return fmt.Errorf("deliver kickoff on %s: %w", pane, err)
	}
}

// detectionRule names the rule herdr's agent detection matched on pane (e.g.
// trust_directory for a folder-trust dialog), for an error message. It never
// fails: an explain that errors or names no rule reads as "detection rule unknown".
func (h *Herdr) detectionRule(ctx context.Context, pane string) string {
	const unknown = "detection rule unknown"
	out, err := h.r.Run(ctx, "", h.HerdrBin, "agent", "explain", pane, "--json")
	if err != nil {
		return unknown
	}
	var resp struct {
		MatchedRule struct {
			ID string `json:"id"`
		} `json:"matched_rule"`
	}
	if json.Unmarshal(out, &resp) != nil || resp.MatchedRule.ID == "" {
		return unknown
	}
	return resp.MatchedRule.ID
}

// herdrErrorCode extracts error.code from a failed herdr command. herdr writes
// its {"error":{"code":...}} envelope to stderr and exits non-zero, and
// proc.Runner carries stderr in the error text, so the envelope is decoded from
// there. Returns "" for a nil error or one that carries no envelope.
func herdrErrorCode(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	i := strings.Index(msg, `{"error"`)
	if i < 0 {
		return ""
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.NewDecoder(strings.NewReader(msg[i:])).Decode(&resp) != nil {
		return ""
	}
	return resp.Error.Code
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

// Message delivers text to a live agent through the same path as the spawn
// kickoff: nil means herdr saw the agent take it, an error means it did not.
func (h *Herdr) Message(ctx context.Context, hd Handle, text string) error {
	return h.deliverKickoff(ctx, hd.PaneID, text)
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
// workspace, keyed by the durable taskID label. The engine calls it when a task
// halts at a non-alerting terminal state (a merge -> merged, or a triage reject ->
// closed), so a settled issue leaves nothing registered. Alerting terminals
// (escalated) are preserved for a human and never reach here.
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

// worktreeDir is a task's deterministic worktree path: WorktreesDir (or
// <repo>/.orchestrator/worktrees when unset) + "wt-<taskID>". The default sits
// inside the checkout so the folder trust the user granted it covers every spawn;
// doctor.spawnsDir mirrors it. Shared by Spawn (via the Spawn's RepoDir) and
// Cleanup (via the backend's RepoDir) so setup and teardown agree on the path.
func (h *Herdr) worktreeDir(repoDir, taskID string) string {
	base := h.WorktreesDir
	if base == "" {
		base = filepath.Join(repoDir, ".orchestrator", "worktrees")
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

// agentDetected reports whether herdr has recognized an agent on paneID: its
// pane list entry carries a non-empty agent kind. A failed list reads as "not
// yet", so the caller's deadline decides.
func (h *Herdr) agentDetected(ctx context.Context, paneID string) bool {
	panes, err := h.listPanes(ctx)
	if err != nil {
		return false
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return p.Agent != ""
		}
	}
	return false
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
