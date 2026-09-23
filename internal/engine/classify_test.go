package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/classify"
	"github.com/sean1588/herdr-orchestrator/internal/github"
)

// fakeClassifier answers every call the same way and records what it was shown.
type fakeClassifier struct {
	res classify.Result
	err error

	mu    sync.Mutex
	calls int
	tails []string
}

func (f *fakeClassifier) Classify(ctx context.Context, tail string) (classify.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.tails = append(f.tails, tail)
	return f.res, f.err
}

func (f *fakeClassifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// With the pane's bytes never changing, the classifier's answer decides what the
// no-progress bound does. Every row runs in implementing, whose state timeout is
// the backstop: a row that "keeps waiting" ends on that timeout, never on
// no_progress.
func TestNoProgress_ClassifierExplainsAStaticPane(t *testing.T) {
	const window = 30 * time.Millisecond
	withPR := &github.PR{Number: 62, State: "OPEN"}

	tests := []struct {
		name         string
		res          classify.Result
		err          error
		pr           *github.PR
		stateTimeout time.Duration
		wantFinal    string
		wantAudit    [4]string // from, to, trigger, result
		// neverTrigger must not appear anywhere in the audit.
		neverTrigger string
		// minCalls pins how many consecutive static windows were ridden through.
		minCalls int
	}{
		{
			name:         "permission prompt escalates at once, before the state timeout",
			res:          classify.Result{Activity: classify.AwaitingPermission, Confidence: 0.95},
			stateTimeout: time.Hour,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "blocked_on_prompt", "state_timeout_target"},
			minCalls:     1,
		},
		{
			name:         "question to the human escalates as a prompt too",
			res:          classify.Result{Activity: classify.AwaitingAnswer, Confidence: 0.95},
			stateTimeout: time.Hour,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "blocked_on_prompt", "state_timeout_target"},
			minCalls:     1,
		},
		{
			name:         "crashed agent escalates at once with its own cause",
			res:          classify.Result{Activity: classify.Crashed, Confidence: 0.95},
			stateTimeout: time.Hour,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "agent_crashed", "state_timeout_target"},
			minCalls:     1,
		},
		{
			// The false-escalation case this feature exists for: a long quiet build.
			// On main the first window escalates; here it rides through window
			// after window until the state timeout, which is the real bound.
			name:         "working rides through consecutive static windows",
			res:          classify.Result{Activity: classify.Working, Confidence: 0.95},
			stateTimeout: 400 * time.Millisecond,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "timeout", ""},
			neverTrigger: "no_progress",
			minCalls:     3,
		},
		{
			name:         "finished with the PR open advances via the gate",
			res:          classify.Result{Activity: classify.Finished, Confidence: 0.95},
			pr:           withPR,
			stateTimeout: time.Hour,
			wantFinal:    "pr_open",
			wantAudit:    [4]string{"implementing", "pr_open", "agent.done", "pass"},
			minCalls:     1,
		},
		{
			// The artifact decides, never the classifier: no PR means keep waiting.
			name:         "finished with no PR keeps waiting",
			res:          classify.Result{Activity: classify.Finished, Confidence: 0.95},
			stateTimeout: 400 * time.Millisecond,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "timeout", ""},
			neverTrigger: "no_progress",
			minCalls:     3,
		},
		{
			name:         "classifier error is exactly main's behavior",
			err:          errors.New("openrouter: 502"),
			stateTimeout: time.Hour,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "no_progress", "state_timeout_target"},
			minCalls:     1,
		},
		{
			name:         "an unsure answer is exactly main's behavior",
			res:          classify.Result{Activity: classify.AwaitingPermission, Confidence: 0.6},
			stateTimeout: time.Hour,
			wantFinal:    "escalated",
			wantAudit:    [4]string{"implementing", "escalated", "no_progress", "state_timeout_target"},
			minCalls:     1,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) { return "the same bytes", nil }}
			e := newEngine(t, st, b, &fakeGH{pr: tt.pr}, tt.stateTimeout)
			e.goal = "pr_open"
			e.noProgress = window
			fc := &fakeClassifier{res: tt.res, err: tt.err}
			e.classifier = fc

			final, rows := driveFrom(t, e, st, 100+i, "implementing", nil)
			if final != tt.wantFinal {
				t.Fatalf("final = %q, want %q; audit triggers: %v", final, tt.wantFinal, auditTriggers(rows))
			}
			w := tt.wantAudit
			if !hasAudit(rows, w[0], w[1], w[2], w[3]) {
				t.Errorf("missing audit %v; got %+v", w, rows)
			}
			if tt.neverTrigger != "" && hasTrigger(rows, tt.neverTrigger) {
				t.Errorf("%s fired, but the classifier explained the static pane: %+v", tt.neverTrigger, rows)
			}
			if n := fc.callCount(); n < tt.minCalls {
				t.Errorf("classifier called %d times, want >= %d", n, tt.minCalls)
			}
			if len(fc.tails) > 0 && fc.tails[0] != "the same bytes" {
				t.Errorf("classifier saw %q, want the tail the progress check read", fc.tails[0])
			}
		})
	}
}

// The classifier runs only on a static pane: while bytes move, it is never asked.
func TestNoProgress_ClassifierNotConsultedWhilePaneMoves(t *testing.T) {
	st := newStore(t)
	n := 0
	b := &fakeBackend{pane: "w1:p1", readFunc: func(int) (string, error) {
		n++
		return string(rune('a' + n%26)), nil
	}}
	e := newEngine(t, st, b, &fakeGH{}, 300*time.Millisecond)
	e.noProgress = 30 * time.Millisecond
	fc := &fakeClassifier{res: classify.Result{Activity: classify.AwaitingPermission, Confidence: 0.99}}
	e.classifier = fc

	final, rows := driveFrom(t, e, st, 120, "implementing", nil)
	if final != "escalated" || !hasTrigger(rows, "timeout") {
		t.Fatalf("final = %q, triggers %v; want escalated via the state timeout", final, auditTriggers(rows))
	}
	if c := fc.callCount(); c != 0 {
		t.Errorf("classifier called %d times on a moving pane, want 0", c)
	}
}
