package main

import (
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

func TestDeadlinesFor(t *testing.T) {
	wf, _, err := config.Load("../../internal/config/testdata/default-pipeline.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	d := deadlinesFor(wf)

	// implementing spawns an agent and declares a timeout: both bounds apply.
	stateTimeout, blocked := d("implementing")
	if stateTimeout == "" {
		t.Error("implementing should report its state timeout")
	}
	if blocked != wf.Policies.BlockedTimeout {
		t.Errorf("blocked = %q, want %q", blocked, wf.Policies.BlockedTimeout)
	}

	// A terminal spawns nothing, so neither bound is meaningful — reporting the
	// blocked bound there would invite waiting on a clock that never starts.
	if st, bl := d("merged"); st != "" || bl != "" {
		t.Errorf("merged reported bounds (%q, %q), want none", st, bl)
	}

	// queued has no agent and no timeout transition.
	if _, bl := d("queued"); bl != "" {
		t.Errorf("queued reported a blocked bound %q; it spawns no agent", bl)
	}

	// An unknown state must not panic or invent bounds.
	if st, bl := d("no-such-state"); st != "" || bl != "" {
		t.Errorf("unknown state reported (%q, %q), want empty", st, bl)
	}
}
