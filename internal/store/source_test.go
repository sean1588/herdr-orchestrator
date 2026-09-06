package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceIdentityMigrationAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.ReplaceAll(schema, "    source_id TEXT NOT NULL DEFAULT '',\n", "")
	old = strings.ReplaceAll(old, "    source_key TEXT NOT NULL DEFAULT '',\n", "")
	if _, err := db.ExecContext(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks
        (id, issue, repo, branch, current_state, pane_id, retry_counts, created_at, updated_at)
        VALUES ('old-task', 99, 'owner/repo', 'agent/issue-99', 'queued', '', '{}',
        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"", ""}, {"notion-database", "opaque-page-uuid"}} {
		task := sampleTask("task-" + identity[0])
		task.SourceID = identity[0]
		task.SourceKey = identity[1]
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		// Source identity, like the workflow snapshot, is immutable on UpdateTask.
		task.SourceID = "changed"
		task.SourceKey = "changed"
		task.CurrentState = "implementing"
		if err := st.UpdateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tasks, err := st.List(ctx)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("tasks=%v err=%v", tasks, err)
	}
	for _, task := range tasks {
		got, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SourceID != task.SourceID || got.SourceKey != task.SourceKey {
			t.Fatal("list/get disagree")
		}
		switch task.ID {
		case "task-", "old-task":
			if task.SourceID != "" || task.SourceKey != "" {
				t.Fatal("legacy identity changed")
			}
		default:
			if task.SourceID != "notion-database" || task.SourceKey != "opaque-page-uuid" {
				t.Fatalf("identity lost: %+v", task)
			}
		}
	}
}
