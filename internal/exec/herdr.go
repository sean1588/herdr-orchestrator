package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// Herdr is the herdr-backed ExecutionBackend. It wraps the same git + herdr CLI
// commands proven in Spike 0, run through a proc.Runner so command construction
// is unit-testable.
type Herdr struct {
	r proc.Runner

	GitBin       string        // default "git"
	HerdrBin     string        // default "herdr"
	WorktreesDir string        // parent dir for worktrees; "" => sibling of the repo
	ReadyMatch   string        // readiness marker awaited before the kickoff; default ">"
	ReadyTimeout time.Duration // bound on the readiness wait; default 20s
	WaitTimeout  time.Duration // WaitState bound when ctx has no sooner deadline; default 45m
	PollInterval time.Duration // Events poll cadence; default 2s
}

var _ ExecutionBackend = (*Herdr)(nil)

// NewHerdr returns a Herdr backend with Spike-0 defaults.
func NewHerdr(r proc.Runner) *Herdr {
	return &Herdr{
		r:            r,
		GitBin:       "git",
		HerdrBin:     "herdr",
		ReadyMatch:   ">",
		ReadyTimeout: 20 * time.Second,
		WaitTimeout:  45 * time.Minute,
		PollInterval: 2 * time.Second,
	}
}

// Spawn: git worktree (isolated) -> herdr workspace -> launch agent -> readiness
// -> single-line kickoff. The multi-line task body lives in Spawn.TaskFile; only
// the single-line kickoff is ever sent through the pane (Spike 0 finding).
func (h *Herdr) Spawn(ctx context.Context, s Spawn) (Handle, error) {
	wt := h.worktreePath(s)

	// Best-effort cleanup of any prior attempt for this branch (ignore errors).
	_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "worktree", "remove", wt, "--force")
	_, _ = h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "branch", "-D", s.Branch)

	if _, err := h.r.Run(ctx, "", h.GitBin, "-C", s.RepoDir, "worktree", "add", "-b", s.Branch, wt, s.Base); err != nil {
		return Handle{}, fmt.Errorf("create worktree %s: %w", wt, err)
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
	_, _ = h.r.Run(ctx, "", h.HerdrBin, "wait", "output", pane, "--match", h.ReadyMatch, "--timeout", msString(h.ReadyTimeout))

	if _, err := h.r.Run(ctx, "", h.HerdrBin, "pane", "run", pane, s.Kickoff); err != nil {
		return hd, fmt.Errorf("send kickoff on %s: %w", pane, err)
	}
	return hd, nil
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
	_, err := h.r.Run(ctx, "", h.HerdrBin, "wait", "agent-status", hd.PaneID, "--status", string(target), "--timeout", msString(timeout))
	if err != nil {
		return h.currentStatus(ctx, hd.PaneID), fmt.Errorf("wait agent-status %s on %s: %w", target, hd.PaneID, err)
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

// Events polls `herdr pane list` and emits one Event per observed status change
// (including each pane's first observation). This is the liveness stream; GitHub
// remains authoritative for artifacts.
func (h *Herdr) Events(ctx context.Context) (<-chan Event, error) {
	ch := make(chan Event, 32)
	go h.pollEvents(ctx, ch)
	return ch, nil
}

func (h *Herdr) pollEvents(ctx context.Context, ch chan<- Event) {
	defer close(ch)
	last := map[string]AgentState{}
	ticker := time.NewTicker(h.PollInterval)
	defer ticker.Stop()
	for {
		if panes, err := h.listPanes(ctx); err == nil {
			for _, p := range panes {
				st := normalizeState(p.AgentStatus)
				if prev, ok := last[p.PaneID]; !ok || prev != st {
					last[p.PaneID] = st
					select {
					case ch <- Event{PaneID: p.PaneID, State: st}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
// worktree is intentionally left for review continuation in Phase 1.
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

// --- helpers ---

func (h *Herdr) worktreePath(s Spawn) string {
	base := h.WorktreesDir
	if base == "" {
		base = filepath.Dir(s.RepoDir)
	}
	return filepath.Join(base, "wt-"+s.TaskID)
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
