package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

var errTestDeadline = errors.New("test drive deadline")

// The reaper cancels a drive that outlives the ceiling, carrying the configured
// cause so the engine settles it rather than aborting. The watchdog runs in its
// own goroutine precisely so a drive wedged like this one — never returning on
// its own — can still be stopped.
func TestReaperCancelsAWedgedDrive(t *testing.T) {
	causes := make(chan error, 1)
	s := &Scheduler{
		List: func(context.Context) ([]int, error) { return []int{1}, nil },
		Done: func(context.Context, int) (bool, error) { return false, nil },
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		RunTask: func(ctx context.Context, issue int) error {
			<-ctx.Done() // a drive that will never end on its own
			causes <- context.Cause(ctx)
			return ctx.Err()
		},
		Interval:      10 * time.Millisecond,
		Workers:       1,
		DriveDeadline: 50 * time.Millisecond,
		DeadlineCause: errTestDeadline,
		ReapInterval:  10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx)

	select {
	case got := <-causes:
		if !errors.Is(got, errTestDeadline) {
			t.Errorf("cancellation cause = %v, want %v", got, errTestDeadline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reaper never cancelled the wedged drive")
	}
}

// A drive that finishes inside the ceiling must not be touched: the reaper is a
// backstop for wedged work, not a cap on how long honest work may take.
func TestReaperLeavesAHealthyDriveAlone(t *testing.T) {
	var mu sync.Mutex
	var cancelled bool
	done := make(chan struct{}, 1)

	s := &Scheduler{
		List: func(context.Context) ([]int, error) { return []int{1}, nil },
		Done: func(context.Context, int) (bool, error) { return false, nil },
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		RunTask: func(ctx context.Context, issue int) error {
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			cancelled = ctx.Err() != nil
			mu.Unlock()
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
		Interval:      time.Hour, // one seed pass only; no re-polling
		Workers:       1,
		DriveDeadline: 10 * time.Second,
		DeadlineCause: errTestDeadline,
		ReapInterval:  5 * time.Millisecond,
		SeedFrom:      func(context.Context) ([]int, error) { return []int{1}, nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drive never ran")
	}
	mu.Lock()
	defer mu.Unlock()
	if cancelled {
		t.Error("reaper cancelled a drive that was well inside its deadline")
	}
}

// A claim with no armed cancel func is an issue queued but not yet picked up by a
// worker. There is no drive to bound, and its zero start time would otherwise
// read as "infinitely old" and be reaped instantly.
func TestReapSkipsClaimedButUnarmedIssues(t *testing.T) {
	set := &inflightSet{m: map[int]*driveHandle{}}
	set.add(7)

	if got := set.reap(time.Nanosecond, time.Now(), errTestDeadline); len(got) != 0 {
		t.Fatalf("reaped an unarmed claim: %v", got)
	}

	armed := false
	set.arm(7, func(error) { armed = true }, time.Now().Add(-time.Hour))
	if got := set.reap(time.Minute, time.Now(), errTestDeadline); len(got) != 1 || got[0] != 7 {
		t.Fatalf("reap() = %v, want [7] once armed", got)
	}
	if !armed {
		t.Error("reap did not invoke the drive's cancel func")
	}
}

// A zero ceiling disables the reaper entirely; Serve must still shut down
// cleanly rather than block forever waiting on a goroutine that never started.
func TestReaperDisabledStillShutsDownCleanly(t *testing.T) {
	s := &Scheduler{
		List:     func(context.Context) ([]int, error) { return nil, nil },
		Done:     func(context.Context, int) (bool, error) { return false, nil },
		RunTask:  func(context.Context, int) error { return nil },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval: 10 * time.Millisecond,
		Workers:  1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return with the reaper disabled")
	}
}
