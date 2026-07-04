package scheduler

import (
	"context"
	"sync"
)

// cancelRegistry maps an in-flight issue to its drive's cancel func, so an
// operator cancel can interrupt exactly one drive. Per-issue registration is
// serialized by the inflightSet (at most one worker drives an issue at a time,
// and a worker deregisters before it frees the issue for re-enqueue), so a plain
// issue-keyed map guarded by a mutex is race-free — no generation token needed.
type cancelRegistry struct {
	mu sync.Mutex
	m  map[int]context.CancelCauseFunc
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{m: map[int]context.CancelCauseFunc{}}
}

func (r *cancelRegistry) register(issue int, c context.CancelCauseFunc) {
	r.mu.Lock()
	r.m[issue] = c
	r.mu.Unlock()
}

func (r *cancelRegistry) deregister(issue int) {
	r.mu.Lock()
	delete(r.m, issue)
	r.mu.Unlock()
}

// cancel invokes the issue's cancel func with cause, returning false if no drive
// is registered for it. The func is captured under the lock and invoked after
// releasing it.
func (r *cancelRegistry) cancel(issue int, cause error) bool {
	r.mu.Lock()
	c, ok := r.m[issue]
	r.mu.Unlock()
	if ok {
		c(cause)
	}
	return ok
}
