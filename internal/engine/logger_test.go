package engine

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
)

// capture records the message of every record it handles.
type capture struct {
	mu   sync.Mutex
	msgs []string
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, r.Message)
	return nil
}
func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

func (c *capture) saw(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// The daemon's --event-log tee is installed on the process default. An engine
// built without a Logger must log there, or the event log sees none of the
// transitions it exists to record; an explicit Logger still wins.
func TestNew_LoggerDefaultsToProcessDefault(t *testing.T) {
	cases := []struct {
		name         string
		explicit     bool
		wantDefault  bool
		wantExplicit bool
	}{
		{name: "nil Logger uses slog.Default", explicit: false, wantDefault: true},
		{name: "explicit Logger is used", explicit: true, wantExplicit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			def := &capture{}
			slog.SetDefault(slog.New(def))

			own := &capture{}
			var logger *slog.Logger
			if tc.explicit {
				logger = slog.New(own)
			}

			wf, _, err := config.Load("../config/testdata/default-pipeline.yaml")
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			gh := &fakeGH{pr: &github.PR{Number: 42, State: "OPEN"}}
			e := New(Config{
				Workflow:     wf,
				Backend:      &fakeBackend{pane: "w1:p1", events: []exec.Event{{PaneID: "w1:p1", State: exec.StateDone}}},
				PullRequests: gh,
				Source:       github.IssueSource{Client: gh, RepoDir: "/repo"},
				Store:        newStore(t),
				RepoDir:      "/repo",
				Base:         "main",
				Repo:         "owner/repo",
				ConfigDir:    "../config/testdata",
				TaskDir:      t.TempDir(),
				DurationFunc: func(string) (time.Duration, error) { return 5 * time.Second, nil },
				Logger:       logger,
			})
			e.goal = "pr_open"
			if _, err := e.Run(context.Background(), 7); err != nil {
				t.Fatalf("run: %v", err)
			}

			if got := def.saw("transition"); got != tc.wantDefault {
				t.Errorf("process default saw transition = %v, want %v (got %q)", got, tc.wantDefault, def.msgs)
			}
			if got := own.saw("transition"); got != tc.wantExplicit {
				t.Errorf("explicit Logger saw transition = %v, want %v (got %q)", got, tc.wantExplicit, own.msgs)
			}
		})
	}
}
