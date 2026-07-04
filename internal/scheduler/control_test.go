package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errTestCause = errors.New("test operator cancel")

// Cancel of a running issue cancels exactly that drive's context with the
// injected cause.
func TestControlCancel(t *testing.T) {
	started := make(chan int, 1)
	gotCause := make(chan error, 1)
	s := &Scheduler{
		Workers:  1,
		Interval: time.Hour, // don't poll during the test
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil },
		RunTask: func(ctx context.Context, issue int) error {
			started <- issue
			<-ctx.Done()
			gotCause <- context.Cause(ctx)
			return ctx.Err()
		},
	}
	s.EnableControl(errTestCause)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	if err := s.Enqueue(ctx, 7); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTask never started")
	}

	if err := s.Cancel(ctx, 7); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case c := <-gotCause:
		if !errors.Is(c, errTestCause) {
			t.Fatalf("cancel cause = %v, want %v", c, errTestCause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunTask was not cancelled")
	}
}

// Cancel of an issue with no active drive errors (nothing to cancel).
func TestControlCancelNotRunning(t *testing.T) {
	s := &Scheduler{
		Workers:  1,
		Interval: time.Hour,
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil },
		RunTask:  func(ctx context.Context, issue int) error { return nil },
	}
	s.EnableControl(errTestCause)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	if err := s.Cancel(ctx, 999); err == nil {
		t.Fatal("cancel of a non-running issue should error")
	}
}

// Enqueue drives a fresh issue exactly once through the command seam.
func TestControlEnqueue(t *testing.T) {
	ran := make(chan int, 4)
	s := &Scheduler{
		Workers:  1,
		Interval: time.Hour,
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil },
		RunTask: func(ctx context.Context, issue int) error {
			ran <- issue
			return nil
		},
	}
	s.EnableControl(errTestCause)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	if err := s.Enqueue(ctx, 42); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	select {
	case got := <-ran:
		if got != 42 {
			t.Fatalf("ran issue %d, want 42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueued issue never ran")
	}
}

// Control methods error when EnableControl was never called.
func TestControlDisabled(t *testing.T) {
	s := &Scheduler{}
	if err := s.Cancel(context.Background(), 1); err == nil {
		t.Fatal("Cancel without EnableControl should error")
	}
	if err := s.Enqueue(context.Background(), 1); err == nil {
		t.Fatal("Enqueue without EnableControl should error")
	}
}
