package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/source"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

type pageSource struct {
	fetched, acknowledged, completed []string
	ackErr                           error
	selector                         source.Selector
}

func (s *pageSource) List(context.Context, source.Selector) ([]string, error) {
	return []string{"page-uuid"}, nil
}
func (s *pageSource) Get(ctx context.Context, key string) (*source.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.fetched = append(s.fetched, key)
	return &source.Item{Key: key, Title: "Page title", Body: "Page content"}, nil
}
func (s *pageSource) Acknowledge(_ context.Context, key string, selector source.Selector) error {
	s.acknowledged = append(s.acknowledged, key)
	s.selector = selector
	return s.ackErr
}
func (s *pageSource) Complete(_ context.Context, key, comment string) error {
	s.completed = append(s.completed, key)
	return nil
}

func sourceEngine(t *testing.T, st *store.Store, b exec.ExecutionBackend, gh github.PullRequests, pages source.Source) *Engine {
	t.Helper()
	baseline := newEngine(t, st, b, nil, time.Second)
	return New(Config{Workflow: baseline.wf, Backend: b, PullRequests: gh, Source: pages,
		SourceID: "notion-database", SourceSelector: source.Selector{"status": "Ready"},
		Store: st, TaskDir: t.TempDir(), ConfigDir: baseline.configDir, Logger: baseline.log,
		Repo: "owner/code", RepoDir: "/code", StartState: "intake"})
}

func TestOpaqueSourceTriageAndResume(t *testing.T) {
	for _, tc := range []struct {
		verdict, goal, want string
		ack                 int
	}{
		{"accept", "queued", "queued", 0}, {"reject", "merged", "closed", 1}, {"needs_human", "merged", "escalated", 1},
	} {
		t.Run(tc.verdict, func(t *testing.T) {
			ctx := context.Background()
			st := newStore(t)
			pages := &pageSource{}
			b := agentDoneBackend()
			b.verdictOnSpawn = map[string]string{"triager": `{"verdict":"` + tc.verdict + `"}`}
			// This dependency exposes only PR methods. No GitHub issue API is available.
			prs := struct{ github.PullRequests }{&fakeGH{}}
			e := sourceEngine(t, st, b, prs, pages)
			e.goal = tc.goal
			key := "b3d9a1c4-opaque-page-uuid"
			final, err := e.RunSource(ctx, key)
			if err != nil || final != tc.want {
				t.Fatalf("run = %s, %v", final, err)
			}
			task, err := st.GetTask(ctx, SourceTaskID(e.sourceID, key))
			if err != nil {
				t.Fatal(err)
			}
			if task.SourceKey != key || task.SourceID != e.sourceID || task.Issue != 0 {
				t.Fatalf("identity = %+v", task)
			}
			body, err := os.ReadFile(b.spawnLog[0].TaskFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "Page content") {
				t.Fatalf("task = %s", body)
			}
			if len(pages.acknowledged) != tc.ack || len(pages.completed) != 0 {
				t.Fatalf("wrong settlement: %+v", pages)
			}
			// A newly wired engine resumes the same persisted task, rather than creating
			// another issue-N task or invoking GitHub's issue API.
			resumed := sourceEngine(t, st, b, prs, pages)
			resumed.goal = tc.goal
			if _, err := resumed.RunSource(ctx, key); err != nil {
				t.Fatal(err)
			}
			tasks, err := st.List(ctx)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%v err=%v", tasks, err)
			}
			if b.spawns != 1 {
				t.Fatalf("resume spawned %d agents", b.spawns)
			}
		})
	}
}

func TestOpaqueSourceSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, state   string
		dry, failAck  bool
		complete, ack int
	}{
		{name: "merged", state: "merging", complete: 1, ack: 1},
		{name: "dry run", state: "merging", dry: true},
		{name: "cancelled", state: CancelState, ack: 1},
		{name: "ack failure after merge", state: "merging", failAck: true, complete: 1, ack: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newStore(t)
			pages := &pageSource{}
			if tc.failAck {
				pages.ackErr = errors.New("source unavailable")
			}
			gh := &fakeGH{}
			e := sourceEngine(t, st, &fakeBackend{}, struct{ github.PullRequests }{gh}, pages)
			e.wf.Policies.DryRun = &tc.dry
			pr := 42
			task := &store.Task{ID: SourceTaskID(e.sourceID, "uuid"), SourceID: e.sourceID, SourceKey: "uuid", CurrentState: tc.state, PRNumber: &pr}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if tc.state == CancelState {
				if err := e.advance(ctx, task, CancelState, "test", ""); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := e.drive(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			if len(pages.completed) != tc.complete || len(pages.acknowledged) != tc.ack {
				t.Fatalf("wrong settlement: %+v", pages)
			}
			if len(gh.closedIssues) != 0 || len(gh.removedLabels) != 0 {
				t.Fatal("touched GitHub issues for an external page")
			}
		})
	}
}

func TestSourceIdentityIsolation(t *testing.T) {
	seen := map[string]bool{}
	for _, pair := range [][2]string{{"a", "b:c"}, {"a:b", "c"}, {"one", "same"}, {"two", "same"}, {"one", "../../x\ncommand"}} {
		id := SourceTaskID(pair[0], pair[1])
		if seen[id] || strings.ContainsAny(id, "/\n:") {
			t.Fatalf("unsafe or aliased ID %q", id)
		}
		seen[id] = true
	}
	st := newStore(t)
	e := sourceEngine(t, st, &fakeBackend{}, &fakeGH{}, &pageSource{})
	if _, err := e.engineForTask(&store.Task{ID: "other", SourceID: "different"}); err == nil {
		t.Fatal("accepted a task from another source")
	}
	if _, err := e.Run(context.Background(), 1); err == nil {
		t.Fatal("numeric entry point accepted custom source")
	}
	if _, err := e.RunSource(context.Background(), ""); err == nil {
		t.Fatal("accepted empty key")
	}
}

func TestSourceContextCancellation(t *testing.T) {
	st := newStore(t)
	e := sourceEngine(t, st, &fakeBackend{}, &fakeGH{}, &pageSource{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.sourceItem(ctx, &store.Task{SourceID: e.sourceID, SourceKey: "uuid"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
