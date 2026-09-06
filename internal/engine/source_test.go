package engine

import (
	"context"
	"errors"
	"os"
	"reflect"
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
		Store: st, TaskDir: t.TempDir(), ConfigDir: baseline.configDir, Logger: baseline.log,
		Repo: "owner/code", RepoDir: "/code", StartState: "intake"})
}

func TestSourceTriageAndResume(t *testing.T) {
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

			final, err := e.Run(ctx, 5)
			if err != nil || final != tc.want {
				t.Fatalf("run = %s, %v", final, err)
			}
			task, err := st.GetTask(ctx, TaskID(5))
			if err != nil {
				t.Fatal(err)
			}
			if task.Issue != 5 || task.ID != TaskID(5) {
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
			if _, err := resumed.Run(ctx, 5); err != nil {
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

func TestSourceSettlement(t *testing.T) {
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
			task := &store.Task{ID: TaskID(5), Issue: 5, CurrentState: tc.state, PRNumber: &pr}
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
			if tc.ack > 0 && !reflect.DeepEqual(pages.selector, source.Selector{"label": "agent-ready"}) {
				t.Fatalf("ack selector = %v", pages.selector)
			}
			if len(gh.closedIssues) != 0 || len(gh.removedLabels) != 0 {
				t.Fatal("touched GitHub issues for an external page")
			}
		})
	}
}

type brokenSource struct {
	source.Source
	item *source.Item
}

func (s brokenSource) Get(context.Context, string) (*source.Item, error) { return s.item, nil }

func TestSourceItemContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		item    *source.Item
		wantErr bool
	}{
		{"nil", nil, true}, {"wrong key", &source.Item{Key: "6"}, true}, {"matching key", &source.Item{Key: "5"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{source: brokenSource{item: tc.item}}
			_, err := e.sourceItem(context.Background(), &store.Task{Issue: 5})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAcknowledgeUsesPinnedWorkflowLabel(t *testing.T) {
	st := newStore(t)
	pages := &pageSource{}
	e := sourceEngine(t, st, &fakeBackend{}, &fakeGH{}, pages)
	task := &store.Task{ID: TaskID(5), Issue: 5, WorkflowSnapshot: strings.ReplaceAll(shippedConfig(t), "agent-ready", "original-ready")}
	pinned, err := e.engineForTask(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.acknowledge(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pages.selector, source.Selector{"label": "original-ready"}) {
		t.Fatalf("selector=%v", pages.selector)
	}
}
