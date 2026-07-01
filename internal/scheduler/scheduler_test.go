package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Discovered issues are each driven exactly once; a completed issue (Done=true)
// is not re-run on the next poll.
func TestServe_DispatchesEachIssueOnce(t *testing.T) {
	var mu sync.Mutex
	runs := map[int]int{}
	done := map[int]bool{}
	ranAll := make(chan struct{})

	s := &Scheduler{
		List: func(ctx context.Context) ([]int, error) { return []int{1, 2, 3}, nil },
		Done: func(ctx context.Context, issue int) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return done[issue], nil
		},
		RunTask: func(ctx context.Context, issue int) error {
			mu.Lock()
			runs[issue]++
			done[issue] = true
			n := len(done)
			mu.Unlock()
			if n == 3 {
				select {
				case <-ranAll:
				default:
					close(ranAll)
				}
			}
			return nil
		},
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	select {
	case <-ranAll:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for all issues to run")
	}
	time.Sleep(30 * time.Millisecond) // let extra polls happen; dedup must hold
	cancel()

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i <= 3; i++ {
		if runs[i] != 1 {
			t.Errorf("issue %d ran %d times, want 1", i, runs[i])
		}
	}
}

// An issue whose task is already terminal is never run.
func TestServe_SkipsTerminalIssues(t *testing.T) {
	var ran int32
	s := &Scheduler{
		List:     func(ctx context.Context) ([]int, error) { return []int{7}, nil },
		Done:     func(ctx context.Context, issue int) (bool, error) { return true, nil },
		RunTask:  func(ctx context.Context, issue int) error { atomic.AddInt32(&ran, 1); return nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return nil, nil },
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = s.Serve(ctx)
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Errorf("terminal issue ran %d times, want 0", n)
	}
}

// Never more than Workers RunTask calls execute concurrently.
func TestServe_RespectsWorkerCap(t *testing.T) {
	release := make(chan struct{})
	var cur, max int32
	s := &Scheduler{
		List: func(ctx context.Context) ([]int, error) { return []int{1, 2, 3, 4, 5, 6}, nil },
		Done: func(ctx context.Context, issue int) (bool, error) { return false, nil },
		RunTask: func(ctx context.Context, issue int) error {
			n := atomic.AddInt32(&cur, 1)
			for {
				m := atomic.LoadInt32(&max)
				if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
					break
				}
			}
			<-release
			atomic.AddInt32(&cur, -1)
			return nil
		},
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Serve(ctx) }()
	time.Sleep(80 * time.Millisecond) // let workers saturate
	if m := atomic.LoadInt32(&max); m > 2 {
		t.Errorf("max concurrent RunTask = %d, want <= 2", m)
	}
	close(release)
	cancel()
}

// Startup seeds non-terminal issues even when the poll source is empty.
func TestServe_SeedsInFlightOnStartup(t *testing.T) {
	ran := make(chan int, 1)
	s := &Scheduler{
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		Done:     func(ctx context.Context, issue int) (bool, error) { return false, nil },
		RunTask:  func(ctx context.Context, issue int) error { ran <- issue; return nil },
		SeedFrom: func(ctx context.Context) ([]int, error) { return []int{9}, nil },
		Interval: time.Hour, // no polling; only the seed drives
		Workers:  1,
		Log:      testLog(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	select {
	case got := <-ran:
		if got != 9 {
			t.Errorf("seeded issue = %d, want 9", got)
		}
	case <-time.After(time.Second):
		t.Fatal("seed issue was not driven")
	}
}

// Serve returns promptly when the context is cancelled.
func TestServe_ReturnsOnCancel(t *testing.T) {
	s := &Scheduler{
		List:     func(ctx context.Context) ([]int, error) { return nil, nil },
		Done:     func(ctx context.Context, issue int) (bool, error) { return false, nil },
		RunTask:  func(ctx context.Context, issue int) error { return nil },
		Interval: 5 * time.Millisecond,
		Workers:  2,
		Log:      testLog(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
