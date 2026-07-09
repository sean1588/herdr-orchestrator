// Package exec defines the ExecutionBackend the engine uses to run agents, and a
// herdr-backed implementation. The engine depends only on the interface, so the
// backend can later be swapped for a headless/container implementation.
package exec

import "context"

// AgentState is herdr's detected agent status. It is a heuristic liveness
// signal, never proof of an artifact — the engine treats "done" only as a
// trigger to go check the authoritative source (GitHub).
type AgentState string

const (
	StateWorking AgentState = "working"
	StateBlocked AgentState = "blocked"
	StateDone    AgentState = "done"
	StateIdle    AgentState = "idle"
	StateUnknown AgentState = "unknown"
)

// Spawn is everything needed to launch one agent in an isolated worktree.
type Spawn struct {
	TaskID   string   // durable task id; also the herdr workspace label, e.g. "issue-5"
	Role     string   // e.g. "implementer" or "reviewer"
	Branch   string   // deterministic: agent/issue-<n>
	RepoDir  string   // main checkout (absolute)
	Base     string   // base branch, e.g. "main"
	TaskFile string   // absolute path to the task context file (NEVER inline the body)
	Launch   []string // e.g. ["claude"]
	Kickoff  string   // single-line kickoff referencing TaskFile
	// PreserveBranch keeps the existing branch on (re)spawn instead of recreating
	// it from base. Set when the task already has a PR (a reviewer/resume spawn):
	// recreating from base would discard the PR's commits.
	PreserveBranch bool
}

// Handle identifies a spawned agent. PaneID is VOLATILE — it is re-resolved from
// create/list/events and must never be persisted as a durable key.
type Handle struct {
	PaneID  string
	Workdir string
}

// Event is an agent status change observed on a pane.
type Event struct {
	PaneID string
	State  AgentState
}

// ExecutionBackend runs and observes agents. context.Context governs
// cancellation and deadlines for every blocking call.
type ExecutionBackend interface {
	// Spawn creates the isolated worktree + workspace, launches the agent, and
	// delivers the task via a single-line kickoff pointing at Spawn.TaskFile.
	Spawn(ctx context.Context, s Spawn) (Handle, error)
	// WaitState blocks until the agent reaches target (or ctx/timeout fires).
	WaitState(ctx context.Context, h Handle, target AgentState) (AgentState, error)
	// Read returns the last `lines` of the agent pane's recent output.
	Read(ctx context.Context, h Handle, lines int) (string, error)
	// Events returns a stream of agent status changes across all panes; the
	// engine filters by its task's pane id.
	Events(ctx context.Context) (<-chan Event, error)
	// Resolve re-resolves a spawned unit by its durable label (= Spawn.TaskID),
	// returning the current (volatile) pane. Used for crash recovery. The bool
	// is false when no live workspace carries that label.
	Resolve(ctx context.Context, label string) (Handle, bool, error)
	// Close tears down the agent's workspace (best-effort).
	Close(ctx context.Context, h Handle) error
	// Cleanup removes the task's isolated worktree and closes its herdr workspace,
	// keyed by the durable label (= Spawn.TaskID). Called when a task settles with
	// no artifact to preserve (a no-PR terminal halt). It is idempotent-safe: an
	// already-removed worktree or already-closed workspace is not an error.
	Cleanup(ctx context.Context, taskID string) error
}
