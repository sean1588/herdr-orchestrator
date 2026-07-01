package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// timeLayout is the on-disk representation of every stored time. RFC3339Nano
// preserves nanosecond precision, so times round-trip faithfully (compare with
// time.Time.Equal, which ignores the monotonic clock reading stripped here).
const timeLayout = time.RFC3339Nano

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    id            TEXT PRIMARY KEY,
    issue         INTEGER NOT NULL,
    repo          TEXT NOT NULL,
    branch        TEXT NOT NULL,
    current_state TEXT NOT NULL,
    pane_id       TEXT NOT NULL,
    pane_spawn_state TEXT NOT NULL DEFAULT '',
    workflow_snapshot TEXT NOT NULL DEFAULT '',
    pr_number     INTEGER,
    retry_counts  TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL,
    ts         TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    trigger    TEXT NOT NULL,
    result     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_task ON audit(task_id, id);
`

// Store persists task state and an append-only audit log in SQLite. It is the
// single writer of task state: writes are serialized through one connection.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema idempotently. MaxOpenConns is pinned to 1 so writes are serialized and
// cannot corrupt under the engine's single-writer contract.
func Open(ctx context.Context, path string) (*Store, error) {
	// busy_timeout makes a momentarily-locked DB wait rather than fail fast.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// applyMigrations idempotently adds columns introduced after the initial schema.
// SQLite lacks ADD COLUMN IF NOT EXISTS, so the duplicate-column error (the column
// already exists on a freshly-created table) is tolerated.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	const addPaneSpawnState = `ALTER TABLE tasks ADD COLUMN pane_spawn_state TEXT NOT NULL DEFAULT ''`
	if _, err := db.ExecContext(ctx, addPaneSpawnState); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("store: migrate pane_spawn_state: %w", err)
	}
	const addSnapshot = `ALTER TABLE tasks ADD COLUMN workflow_snapshot TEXT NOT NULL DEFAULT ''`
	if _, err := db.ExecContext(ctx, addSnapshot); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("store: migrate workflow_snapshot: %w", err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// CreateTask inserts a new task, populating CreatedAt/UpdatedAt if unset.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	now := s.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}

	rc, err := marshalRetryCounts(t.RetryCounts)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks
			(id, issue, repo, branch, current_state, pane_id, pane_spawn_state, workflow_snapshot, pr_number, retry_counts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Issue, t.Repo, t.Branch, t.CurrentState, t.PaneID, t.PaneSpawnState, t.WorkflowSnapshot,
		prNumberArg(t.PRNumber), rc,
		t.CreatedAt.Format(timeLayout), t.UpdatedAt.Format(timeLayout),
	)
	if err != nil {
		return fmt.Errorf("store: insert task %q: %w", t.ID, err)
	}
	return nil
}

// GetTask returns the task with the given id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, issue, repo, branch, current_state, pane_id, pane_spawn_state, workflow_snapshot, pr_number, retry_counts, created_at, updated_at
		FROM tasks WHERE id = ?`, id)

	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: get task %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task %q: %w", id, err)
	}
	return t, nil
}

// UpdateTask updates the mutable columns of an existing task and bumps
// UpdatedAt. It returns ErrNotFound if the row is missing.
func (s *Store) UpdateTask(ctx context.Context, t *Task) error {
	t.UpdatedAt = s.now()

	rc, err := marshalRetryCounts(t.RetryCounts)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET
			repo = ?, branch = ?, current_state = ?, pane_id = ?, pane_spawn_state = ?,
			pr_number = ?, retry_counts = ?, updated_at = ?
		WHERE id = ?`,
		t.Repo, t.Branch, t.CurrentState, t.PaneID, t.PaneSpawnState,
		prNumberArg(t.PRNumber), rc, t.UpdatedAt.Format(timeLayout), t.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update task %q: %w", t.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update task %q: %w", t.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("store: update task %q: %w", t.ID, ErrNotFound)
	}
	return nil
}

// List returns all tasks ordered by id. The store is workflow-agnostic; callers
// filter terminal states themselves.
func (s *Store) List(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, issue, repo, branch, current_state, pane_id, pane_spawn_state, workflow_snapshot, pr_number, retry_counts, created_at, updated_at
		FROM tasks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list tasks: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	return out, nil
}

// AppendAudit appends one row to the task's transition history.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	ts := e.TS
	if ts.IsZero() {
		ts = s.now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit (task_id, ts, from_state, to_state, trigger, result)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.TaskID, ts.Format(timeLayout), e.FromState, e.ToState, e.Trigger, e.Result,
	)
	if err != nil {
		return fmt.Errorf("store: append audit for %q: %w", e.TaskID, err)
	}
	return nil
}

// Audit returns the task's audit rows in chronological (insertion) order.
func (s *Store) Audit(ctx context.Context, taskID string) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, ts, from_state, to_state, trigger, result
		FROM audit WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: audit for %q: %w", taskID, err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e     AuditEntry
			tsStr string
		)
		if err := rows.Scan(&e.TaskID, &tsStr, &e.FromState, &e.ToState, &e.Trigger, &e.Result); err != nil {
			return nil, fmt.Errorf("store: audit for %q: %w", taskID, err)
		}
		if e.TS, err = time.Parse(timeLayout, tsStr); err != nil {
			return nil, fmt.Errorf("store: audit for %q: parse ts %q: %w", taskID, tsStr, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: audit for %q: %w", taskID, err)
	}
	return out, nil
}

// scanner is the common surface of *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanTask(sc scanner) (*Task, error) {
	var (
		t       Task
		pr      sql.NullInt64
		rc      string
		created string
		updated string
	)
	if err := sc.Scan(&t.ID, &t.Issue, &t.Repo, &t.Branch, &t.CurrentState, &t.PaneID, &t.PaneSpawnState,
		&t.WorkflowSnapshot, &pr, &rc, &created, &updated); err != nil {
		return nil, err
	}

	if pr.Valid {
		n := int(pr.Int64)
		t.PRNumber = &n
	}
	if err := json.Unmarshal([]byte(rc), &t.RetryCounts); err != nil {
		return nil, fmt.Errorf("decode retry_counts: %w", err)
	}

	var err error
	if t.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return nil, fmt.Errorf("parse created_at %q: %w", created, err)
	}
	if t.UpdatedAt, err = time.Parse(timeLayout, updated); err != nil {
		return nil, fmt.Errorf("parse updated_at %q: %w", updated, err)
	}
	return &t, nil
}

// marshalRetryCounts serializes the map as JSON. nil marshals to "null" and a
// non-nil empty map to "{}", so the nil/empty distinction round-trips.
func marshalRetryCounts(m map[string]int) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("store: encode retry_counts: %w", err)
	}
	return string(b), nil
}

func prNumberArg(pr *int) any {
	if pr == nil {
		return nil
	}
	return *pr
}
