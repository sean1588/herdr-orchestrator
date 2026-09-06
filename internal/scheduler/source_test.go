package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOpaqueKeysSeedDeduplicateAndCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 2)
	causes := make(chan error, 2)
	s := &SchedulerOf[string]{Workers: 2, Interval: time.Hour,
		List:     func(context.Context) ([]string, error) { return nil, nil },
		SeedFrom: func(context.Context) ([]string, error) { return []string{"page-a", "page-a", "page-b"}, nil },
		RunTask: func(ctx context.Context, key string) error {
			started <- key
			<-ctx.Done()
			causes <- context.Cause(ctx)
			return ctx.Err()
		},
	}
	s.EnableControl(errTestCause)
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(ctx) }()
	seen := map[string]bool{}
	for range 2 {
		select {
		case key := <-started:
			if seen[key] {
				t.Fatal("duplicate drive")
			}
			seen[key] = true
		case <-ctx.Done():
			t.Fatal("no drive")
		}
	}
	if err := s.Enqueue(ctx, "page-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, "page-a"); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-causes:
		if !errors.Is(cause, errTestCause) {
			t.Fatalf("cause=%v", cause)
		}
	case <-ctx.Done():
		t.Fatal("cancel did not reach drive")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}
