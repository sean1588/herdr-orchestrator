package proc

import (
	"context"
	"sync"
)

// Call records one Runner invocation, for assertions in tests.
type Call struct {
	Dir  string
	Name string
	Args []string
}

// Fake is a scriptable Runner for tests. It records every call and delegates to
// Responder (if set) to produce stdout/err. It is safe for concurrent use.
type Fake struct {
	mu        sync.Mutex
	Calls     []Call
	Responder func(Call) ([]byte, error)
}

// Run records the call and returns the scripted response.
func (f *Fake) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	c := Call{Dir: dir, Name: name, Args: append([]string(nil), args...)}
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	resp := f.Responder
	f.mu.Unlock()
	if resp != nil {
		return resp(c)
	}
	return nil, nil
}

// Snapshot returns a copy of the recorded calls.
func (f *Fake) Snapshot() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.Calls...)
}
