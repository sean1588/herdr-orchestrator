package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// A drive whose context is cancelled with ErrOperatorCancel settles the task to
// CancelState through the single mutation point (audit + persisted state). Any
// other cancellation cause (daemon shutdown) aborts, leaving the state for
// recovery — it must NOT settle to CancelState.
func TestDriveOperatorCancelSettles(t *testing.T) {
	tests := []struct {
		name       string
		cause      error
		wantCancel bool
	}{
		{"operator cancel settles", ErrOperatorCancel, true},
		{"plain cancel aborts", context.Canceled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			b := &fakeBackend{pane: "w1:p1"} // no events: the drive parks in awaitAgentState
			e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)

			bg := context.Background()
			if err := st.CreateTask(bg, &store.Task{
				ID: "issue-7", Issue: 7, Repo: "owner/repo",
				Branch: "agent/issue-7", CurrentState: "implementing",
			}); err != nil {
				t.Fatal(err)
			}
			task, _ := st.GetTask(bg, "issue-7")

			ctx, cancel := context.WithCancelCause(bg)
			done := make(chan string, 1)
			go func() {
				final, _ := e.drive(ctx, task)
				done <- final
			}()
			time.Sleep(50 * time.Millisecond) // let the drive reach the agent wait
			cancel(tc.cause)

			select {
			case final := <-done:
				got, _ := st.GetTask(bg, "issue-7")
				if tc.wantCancel {
					if final != CancelState || got.CurrentState != CancelState {
						t.Fatalf("final=%q persisted=%q, want %q", final, got.CurrentState, CancelState)
					}
					if !hasAudit(auditFor(t, st, "issue-7"), "implementing", CancelState, "operator.cancel", "") {
						t.Fatalf("missing operator.cancel audit row: %+v", auditFor(t, st, "issue-7"))
					}
				} else if got.CurrentState == CancelState {
					t.Fatalf("plain cancel must not settle to %q", CancelState)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("drive did not return after cancel")
			}
		})
	}
}

// An operator cancel that lands outside the agent-wait select — here the ctx is
// already cancelled when the drive tries its first advance (queued->implementing)
// — must still settle to CancelState, not surface a raw context.Canceled. This
// covers the reclassify-at-the-drive-boundary path, not just the await site.
func TestDriveOperatorCancelOutsideAgentWaitSettles(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1"}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)

	bg := context.Background()
	if err := st.CreateTask(bg, &store.Task{
		ID: "issue-3", Issue: 3, Repo: "owner/repo",
		Branch: "agent/issue-3", CurrentState: "queued",
	}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(bg, "issue-3")

	ctx, cancel := context.WithCancelCause(bg)
	cancel(ErrOperatorCancel) // cancelled before the drive begins

	done := make(chan string, 1)
	go func() {
		final, _ := e.drive(ctx, task)
		done <- final
	}()
	select {
	case final := <-done:
		if final != CancelState {
			t.Fatalf("final = %q, want %q", final, CancelState)
		}
		got, _ := st.GetTask(bg, "issue-3")
		if got.CurrentState != CancelState {
			t.Fatalf("persisted state = %q, want %q", got.CurrentState, CancelState)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not settle a pre-cancelled operator context")
	}
}

// A cancel that lands mid-subprocess surfaces as a non-context.Canceled error
// (a SIGKILL'd gh/herdr/git yields *exec.ExitError "signal: killed", not
// context.Canceled). The settle must key off the context's cause, not the error
// shape — otherwise the reconcile/spawn/gate windows silently drop the cancel and
// the task re-drives next poll. Here Spawn fails with such an error while the ctx
// is operator-cancelled; the task must still reach CancelState.
func TestDriveOperatorCancelWithNonCanceledErrorSettles(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{pane: "w1:p1", spawnErr: errors.New("spawn: signal: killed")}
	e := newEngine(t, st, b, &fakeGH{}, 5*time.Second)

	bg := context.Background()
	if err := st.CreateTask(bg, &store.Task{
		ID: "issue-4", Issue: 4, Repo: "owner/repo",
		Branch: "agent/issue-4", CurrentState: "implementing",
	}); err != nil {
		t.Fatal(err)
	}
	task, _ := st.GetTask(bg, "issue-4")

	ctx, cancel := context.WithCancelCause(bg)
	cancel(ErrOperatorCancel)

	done := make(chan string, 1)
	go func() {
		final, _ := e.drive(ctx, task)
		done <- final
	}()
	select {
	case final := <-done:
		if final != CancelState {
			t.Fatalf("final = %q, want %q (a non-context.Canceled error in a cancelled drive must still settle)", final, CancelState)
		}
		got, _ := st.GetTask(bg, "issue-4")
		if got.CurrentState != CancelState {
			t.Fatalf("persisted state = %q, want %q", got.CurrentState, CancelState)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drive did not settle after a non-context.Canceled error under operator cancel")
	}
}
