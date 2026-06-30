// Package store persists orchestrator task state and an append-only audit log
// in a single-writer SQLite database. The engine is the single writer of task
// state; the store stays workflow-agnostic (it never interprets state names).
package store

import (
	"errors"
	"time"
)

// Task is the persisted state of one unit of work flowing through the workflow.
type Task struct {
	ID           string // deterministic, e.g. "issue-5"
	Issue        int    // source issue number
	Repo         string // e.g. "sean1588/minicode"
	Branch       string // deterministic, e.g. "agent/issue-5"
	CurrentState string // workflow state name, e.g. "implementing"
	PaneID       string // VOLATILE herdr pane id; best-effort, may be re-resolved on restart
	// PaneSpawnState is the state PaneID's agent was spawned for. The engine
	// reuses a live pane only when re-entering that same state (crash recovery);
	// entering a new state spawns a fresh agent for that state's role.
	PaneSpawnState string
	PRNumber       *int           // nil until a PR is detected
	RetryCounts    map[string]int // keyed by retry-cap key, may be nil/empty
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AuditEntry is one immutable row in a task's transition history.
type AuditEntry struct {
	TaskID    string
	TS        time.Time
	FromState string
	ToState   string
	Trigger   string // the trigger that fired, e.g. "agent.done", "timeout"
	Result    string // gate/verdict outcome, e.g. "pass", "fail", ""
}

// ErrNotFound is returned when a task id has no row.
var ErrNotFound = errors.New("store: task not found")
