// Package scheduler runs the orchestrator as a daemon: a single poller goroutine
// discovers candidate issues (the only task creator, so there is no create/create
// race) and an N-worker pool drives each issue to completion by wrapping the
// engine's per-issue entry point. Tasks are row-partitioned by issue and the
// store serializes writes, so the workers need no additional locking.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// queueDepth bounds the in-memory work channel. Overflow is not lost: the poller
// skips a full channel and the still-labelled issue is re-discovered next tick.
const queueDepth = 128

// defaultInterval is used when Serve is given a non-positive Interval, so the
// daemon poll loop never panics on a zero value.
const defaultInterval = 30 * time.Second

// Scheduler drives discovered issues concurrently. All external dependencies are
// injected as funcs so it is unit-testable without the engine, store, or gh.
type Scheduler struct {
	List     func(ctx context.Context) ([]int, error)           // discover candidate issues
	Done     func(ctx context.Context, issue int) (bool, error) // true iff a TERMINAL task exists
	RunTask  func(ctx context.Context, issue int) error         // drive one issue to completion
	SeedFrom func(ctx context.Context) ([]int, error)           // non-terminal issues to resume at startup
	Interval time.Duration
	Workers  int
	Log      *slog.Logger
}

// Serve seeds in-flight work, starts the worker pool, then polls until ctx is
// done. On cancellation it stops the poller, lets the workers drain (each drive
// returns when ctx is done), and returns. Tasks persist their state, so the next
// start resumes them via SeedFrom.
func (s *Scheduler) Serve(ctx context.Context) error {
	workers := s.Workers
	if workers < 1 {
		workers = 1
	}
	work := make(chan int, queueDepth)
	inflight := &inflightSet{m: map[int]bool{}}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for issue := range work {
				if err := s.RunTask(ctx, issue); err != nil {
					s.Log.Warn("run task failed", "issue", issue, "err", err)
				}
				inflight.remove(issue) // allow re-discovery (retry on a later poll)
			}
		}()
	}

	if s.SeedFrom != nil {
		if seed, err := s.SeedFrom(ctx); err != nil {
			s.Log.Warn("seed failed", "err", err)
		} else {
			s.enqueue(ctx, work, inflight, seed)
		}
	}

	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(work) // Serve is the only sender; safe to close
			wg.Wait()
			return nil
		case <-ticker.C:
			issues, err := s.List(ctx)
			if err != nil {
				s.Log.Warn("poll failed", "err", err)
				continue
			}
			s.enqueue(ctx, work, inflight, issues)
		}
	}
}

// enqueue adds issues that are neither in-flight nor already done. It never
// blocks: a full channel means the issue is skipped and re-discovered next poll.
func (s *Scheduler) enqueue(ctx context.Context, work chan int, inflight *inflightSet, issues []int) {
	for _, issue := range issues {
		if inflight.has(issue) {
			continue
		}
		if s.Done != nil {
			done, err := s.Done(ctx, issue)
			if err != nil {
				s.Log.Warn("done check failed", "issue", issue, "err", err)
				continue
			}
			if done {
				continue
			}
		}
		if !inflight.add(issue) {
			continue
		}
		select {
		case work <- issue:
		default:
			inflight.remove(issue) // channel full; retry next poll
		}
	}
}

// inflightSet tracks issues currently enqueued or being driven, so a poll never
// hands the same non-terminal issue to a second worker.
type inflightSet struct {
	mu sync.Mutex
	m  map[int]bool
}

func (s *inflightSet) add(issue int) bool { // true if newly added
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[issue] {
		return false
	}
	s.m[issue] = true
	return true
}

func (s *inflightSet) remove(issue int) {
	s.mu.Lock()
	delete(s.m, issue)
	s.mu.Unlock()
}

func (s *inflightSet) has(issue int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[issue]
}
