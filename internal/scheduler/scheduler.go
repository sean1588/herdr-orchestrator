// Package scheduler runs the orchestrator as a daemon: a single poller goroutine
// discovers candidate issues (the only task creator, so there is no create/create
// race) and an N-worker pool drives each issue to completion by wrapping the
// engine's per-issue entry point. Tasks are row-partitioned by issue and the
// store serializes writes, so the workers need no additional locking.
package scheduler

import (
	"context"
	"errors"
	"fmt"
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

	// Control seam (nil unless EnableControl): the MCP server submits commands
	// here and Serve processes them in the poller goroutine. cancelCause is the
	// cause an operator cancel carries (the daemon wires engine.ErrOperatorCancel).
	commands    chan command
	cancelCause error
}

type cmdKind int

const (
	cmdEnqueue cmdKind = iota
	cmdCancel
)

type command struct {
	kind  cmdKind
	issue int
	reply chan error // buffered(1); the poller never blocks replying
}

// EnableControl turns on the external control surface: Enqueue/Cancel become
// live and Serve processes their commands. cancelCause is the cause an operator
// cancel carries (the daemon passes engine.ErrOperatorCancel).
func (s *Scheduler) EnableControl(cancelCause error) {
	s.commands = make(chan command)
	s.cancelCause = cancelCause
}

// Enqueue re-drives an issue by number. Idempotent: a no-op if the issue is
// already in flight. Satisfies the MCP control surface.
func (s *Scheduler) Enqueue(ctx context.Context, issue int) error {
	return s.submit(ctx, command{kind: cmdEnqueue, issue: issue})
}

// Cancel cancels the running drive for an issue, erroring if none is running.
// Satisfies the MCP control surface.
func (s *Scheduler) Cancel(ctx context.Context, issue int) error {
	return s.submit(ctx, command{kind: cmdCancel, issue: issue})
}

// submit sends a command to the poller and waits for its reply, honoring ctx so
// a caller (an MCP request) is never wedged if the daemon is shutting down.
func (s *Scheduler) submit(ctx context.Context, c command) error {
	if s.commands == nil {
		return errors.New("scheduler control not enabled")
	}
	c.reply = make(chan error, 1)
	select {
	case s.commands <- c:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Serve seeds in-flight work, starts the worker pool, then polls until ctx is
// done. Each poll discovers new candidates (List) and re-enqueues non-settled
// in-flight tasks (SeedFrom), so a task that yielded its worker mid-drive — a
// blocked_on_gate merge-gate wait that suspended rather than pinning its slot for
// the whole wait — is resumed to re-check its gate. On cancellation it stops the
// poller, lets the workers drain (each drive returns when ctx is done), and returns.
func (s *Scheduler) Serve(ctx context.Context) error {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	workers := s.Workers
	if workers < 1 {
		workers = 1
	}
	work := make(chan int, queueDepth)
	inflight := &inflightSet{m: map[int]context.CancelCauseFunc{}}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for issue := range work {
				// Derive a per-issue context and arm the inflight entry with its
				// cancel func, so a control cancel can interrupt this one drive.
				// Cancelling with ErrOperatorCancel makes the drive settle; the
				// parent ctx cancels it too on daemon shutdown.
				runCtx, cancel := context.WithCancelCause(ctx)
				inflight.arm(issue, cancel)
				if err := s.RunTask(runCtx, issue); err != nil {
					s.Log.Warn("run task failed", "issue", issue, "err", err)
				}
				cancel(nil)            // release the context
				inflight.remove(issue) // clears the cancel func; allows re-discovery next poll
			}
		}()
	}

	s.seed(ctx, work, inflight) // resume in-flight/suspended tasks immediately

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
		case c := <-s.commands: // nil channel when control is disabled: never ready
			s.handleCommand(ctx, c, work, inflight)
		case <-ticker.C:
			if issues, err := s.List(ctx); err != nil {
				s.Log.Warn("poll failed", "err", err)
			} else {
				s.enqueue(ctx, work, inflight, issues)
			}
			// Re-enqueue non-settled in-store tasks too, so a suspended drive (a
			// blocked_on_gate wait that yielded its worker) resumes. Runs even when
			// List errors — a source outage must not strand in-flight work.
			s.seed(ctx, work, inflight)
		}
	}
}

// seed re-enqueues the non-settled in-flight tasks reported by SeedFrom. It runs
// at startup and on every poll so a task that yielded its worker mid-drive (a
// suspended blocked_on_gate wait) is resumed; enqueue's inflight and Done checks
// keep it from racing a task already being driven or one that has since settled.
func (s *Scheduler) seed(ctx context.Context, work chan int, inflight *inflightSet) {
	if s.SeedFrom == nil {
		return
	}
	issues, err := s.SeedFrom(ctx)
	if err != nil {
		s.Log.Warn("seed failed", "err", err)
		return
	}
	s.enqueue(ctx, work, inflight, issues)
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

// handleCommand processes an external control command in the poller goroutine —
// so an enqueue keeps Serve the single sender on the work channel, and a cancel
// reads the inflight set without a second owner. The reply channel is buffered,
// so this never blocks on a caller that has already given up (a cancelled request).
func (s *Scheduler) handleCommand(ctx context.Context, c command, work chan int, inflight *inflightSet) {
	switch c.kind {
	case cmdEnqueue:
		c.reply <- s.enqueueOne(ctx, work, inflight, c.issue)
	case cmdCancel:
		if inflight.cancel(c.issue, s.cancelCause) {
			c.reply <- nil
		} else {
			c.reply <- fmt.Errorf("issue %d is not currently running", c.issue)
		}
	default:
		c.reply <- fmt.Errorf("unknown command kind %d", c.kind)
	}
}

// enqueueOne places a single operator-requested issue on the work channel,
// returning a descriptive error when it was NOT actually queued — so the MCP
// caller is never told an issue was enqueued when it was skipped. Unlike the
// poller's batch enqueue (fire-and-forget: a labelled issue is re-discovered next
// poll), a manual enqueue has no such backstop, so its disposition must be honest.
// An already-in-flight issue is a benign success (it is being driven).
func (s *Scheduler) enqueueOne(ctx context.Context, work chan int, inflight *inflightSet, issue int) error {
	if inflight.has(issue) {
		return nil // already being driven
	}
	if s.Done != nil {
		done, err := s.Done(ctx, issue)
		if err != nil {
			return fmt.Errorf("issue %d: readiness check failed: %w", issue, err)
		}
		if done {
			return fmt.Errorf("issue %d has already settled; not re-driven", issue)
		}
	}
	if !inflight.add(issue) {
		return nil // raced another claim; being driven
	}
	select {
	case work <- issue:
		return nil
	default:
		inflight.remove(issue)
		return fmt.Errorf("issue %d not enqueued: work queue full, retry", issue)
	}
}

// inflightSet tracks issues currently enqueued or being driven, so a poll never
// hands the same non-terminal issue to a second worker. It also holds each
// driving issue's cancel func (nil while claimed-but-not-yet-running), so a
// control cancel can interrupt exactly one drive — the cancel handle lives and
// dies with the same per-issue claim, needing no second structure. Per-issue
// registration is serialized (one worker per issue at a time), so a mutex-guarded
// map is race-free.
type inflightSet struct {
	mu sync.Mutex
	m  map[int]context.CancelCauseFunc
}

func (s *inflightSet) add(issue int) bool { // true if newly claimed
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[issue]; ok {
		return false
	}
	s.m[issue] = nil
	return true
}

// arm records the running drive's cancel func for an already-claimed issue.
func (s *inflightSet) arm(issue int, cancel context.CancelCauseFunc) {
	s.mu.Lock()
	s.m[issue] = cancel
	s.mu.Unlock()
}

// cancel invokes the issue's cancel func with cause, returning false if the issue
// is not currently being driven (unclaimed, or claimed but not yet armed).
func (s *inflightSet) cancel(issue int, cause error) bool {
	s.mu.Lock()
	cancel := s.m[issue]
	s.mu.Unlock()
	if cancel != nil {
		cancel(cause)
		return true
	}
	return false
}

func (s *inflightSet) remove(issue int) {
	s.mu.Lock()
	delete(s.m, issue)
	s.mu.Unlock()
}

func (s *inflightSet) has(issue int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[issue]
	return ok
}
