package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/github"
)

func greenStatus() *github.PRStatus {
	return &github.PRStatus{
		State: "OPEN", ChecksTotal: 1, ApprovedReviews: 1,
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
	}
}

func TestApproved_GateGreen_ReachesMerging(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{status: greenStatus()}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	e.goal = "merging"
	task := seedAt(t, st, "approved", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merging" {
		t.Fatalf("final = %q, want merging", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "approved", "merging", "status.changed", "pass") {
		t.Error("missing audit approved->merging status.changed pass")
	}
}

func TestApproved_GateFails_GoesToBlockedOnGate(t *testing.T) {
	cases := map[string]*github.PRStatus{
		"failing check":     {State: "OPEN", ChecksTotal: 1, ChecksFailed: 1, ApprovedReviews: 1, Mergeable: "MERGEABLE"},
		"pending check":     {State: "OPEN", ChecksTotal: 1, ChecksPending: 1, ApprovedReviews: 1, Mergeable: "MERGEABLE"},
		"no approval":       {State: "OPEN", ApprovedReviews: 0, Mergeable: "MERGEABLE"},
		"merge conflict":    {State: "OPEN", ApprovedReviews: 1, Mergeable: "CONFLICTING"},
		"mergeable UNKNOWN": {State: "OPEN", ApprovedReviews: 1, Mergeable: "UNKNOWN"},
	}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			st := newStore(t)
			e := newEngine(t, st, &fakeBackend{}, &fakeGH{status: status}, 5*time.Second)
			e.goal = "blocked_on_gate"
			task := seedAt(t, st, "approved", 42, nil)

			final, err := e.drive(context.Background(), task)
			if err != nil {
				t.Fatalf("drive: %v", err)
			}
			if final != "blocked_on_gate" {
				t.Fatalf("final = %q, want blocked_on_gate", final)
			}
		})
	}
}

func TestBlockedOnGate_PollsUntilGreen_ReachesMerging(t *testing.T) {
	st := newStore(t)
	red := &github.PRStatus{State: "OPEN", ChecksTotal: 1, ChecksPending: 1, ApprovedReviews: 1, Mergeable: "MERGEABLE"}
	gh := &fakeGH{statusSeq: []*github.PRStatus{red, red, greenStatus()}}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second) // large timeout
	e.statusPoll = time.Millisecond
	e.goal = "merging"
	task := seedAt(t, st, "blocked_on_gate", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merging" {
		t.Fatalf("final = %q, want merging", final)
	}
	if gh.statusIdx < 3 {
		t.Errorf("expected at least 3 polls, got %d", gh.statusIdx)
	}
	if !hasAudit(auditFor(t, st, task.ID), "blocked_on_gate", "merging", "status.changed", "pass") {
		t.Error("missing audit blocked_on_gate->merging status.changed pass")
	}
}

func TestBlockedOnGate_NeverGreen_TimesOutToEscalated(t *testing.T) {
	st := newStore(t)
	red := &github.PRStatus{State: "OPEN", ChecksTotal: 1, ChecksFailed: 1, ApprovedReviews: 1, Mergeable: "MERGEABLE"}
	gh := &fakeGH{status: red}
	e := newEngine(t, st, &fakeBackend{}, gh, 20*time.Millisecond) // tiny timeout
	e.statusPoll = time.Millisecond
	e.goal = "merged"
	task := seedAt(t, st, "blocked_on_gate", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "blocked_on_gate", "escalated", "timeout", "") {
		t.Error("missing audit blocked_on_gate->escalated timeout")
	}
}

func TestMergeGate_NoPR_Fails(t *testing.T) {
	st := newStore(t)
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{}, 5*time.Second)
	e.goal = "blocked_on_gate"
	// approved with no detected PR: the merge gate cannot pass.
	n := 0
	task := seedAt(t, st, "approved", n, nil)
	task.PRNumber = nil
	if err := st.UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "blocked_on_gate" {
		t.Fatalf("final = %q, want blocked_on_gate (no PR => gate fail)", final)
	}
}
