package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

func greenStatus() *github.PRStatus {
	return &github.PRStatus{
		State: "OPEN", ChecksTotal: 1, ApprovedReviews: 1,
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN",
	}
}

// seedBlockedEntry records the approved->blocked_on_gate transition audit at ts,
// so the engine can derive how long the task has been waiting on the merge gate.
func seedBlockedEntry(t *testing.T, st *store.Store, id string, ts time.Time) {
	t.Helper()
	if err := st.AppendAudit(context.Background(), store.AuditEntry{
		TaskID: id, TS: ts, FromState: "approved", ToState: "blocked_on_gate",
		Trigger: "status.changed", Result: "fail",
	}); err != nil {
		t.Fatalf("seed blocked_on_gate entry: %v", err)
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

// A PR with no conflicts (MERGEABLE) but not up to date (BEHIND) must fail the
// no_conflicts gate, which is configured `require: clean` -> mergeStateStatus
// must be CLEAN, not merely conflict-free.
func TestApproved_MergeableButNotClean_GoesToBlockedOnGate(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{status: &github.PRStatus{
		State: "OPEN", ChecksTotal: 1, ApprovedReviews: 1,
		Mergeable: "MERGEABLE", MergeStateStatus: "BEHIND",
	}}
	e := newEngine(t, st, &fakeBackend{}, gh, 5*time.Second)
	e.goal = "blocked_on_gate"
	task := seedAt(t, st, "approved", 42, nil)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "blocked_on_gate" {
		t.Fatalf("final = %q, want blocked_on_gate (require: clean must fail on BEHIND)", final)
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

// A single gate evaluation that passes advances blocked_on_gate to merging — the
// worker no longer polls in-process; each drive evaluates the gate once.
func TestBlockedOnGate_GateGreen_ReachesMerging(t *testing.T) {
	st := newStore(t)
	gh := &fakeGH{status: greenStatus()}
	e := newEngine(t, st, &fakeBackend{}, gh, 30*time.Minute)
	e.goal = "merging"
	task := seedAt(t, st, "blocked_on_gate", 42, nil)
	seedBlockedEntry(t, st, task.ID, time.Unix(1000, 0).UTC())

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "merging" {
		t.Fatalf("final = %q, want merging (gate green on a single eval)", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "blocked_on_gate", "merging", "status.changed", "pass") {
		t.Error("missing audit blocked_on_gate->merging status.changed pass")
	}
}

// Gate still red but within the timeout: the drive SUSPENDS — it returns at
// blocked_on_gate (freeing the worker) without escalating or recording a
// transition. The scheduler re-drives it later.
func TestBlockedOnGate_GateRed_WithinTimeout_Suspends(t *testing.T) {
	st := newStore(t)
	red := &github.PRStatus{State: "OPEN", ChecksTotal: 1, ChecksPending: 1, ApprovedReviews: 1, Mergeable: "MERGEABLE"}
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{status: red}, 30*time.Minute)
	e.goal = "merged" // not a halt at blocked_on_gate, so the state actually runs
	entry := time.Unix(1000, 0).UTC()
	e.now = func() time.Time { return entry.Add(time.Minute) } // 1m elapsed < 30m
	task := seedAt(t, st, "blocked_on_gate", 42, nil)
	seedBlockedEntry(t, st, task.ID, entry)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "blocked_on_gate" {
		t.Fatalf("final = %q, want blocked_on_gate (red gate within timeout => suspend, worker yields)", final)
	}
	// Suspending records no transition — no escalation, no self-loop churn.
	rows := auditFor(t, st, task.ID)
	if hasAudit(rows, "blocked_on_gate", "escalated", "timeout", "") {
		t.Error("suspend within the timeout must not escalate")
	}
	if len(rows) != 1 { // only the seeded entry row
		t.Errorf("suspend must not append an audit row; got %d rows", len(rows))
	}
}

// Gate still red past the timeout: the drive escalates (the timeout is measured
// from the audit-recorded entry time, so it survives across suspend/resume and
// daemon restarts).
func TestBlockedOnGate_GateRed_PastTimeout_Escalates(t *testing.T) {
	st := newStore(t)
	red := &github.PRStatus{State: "OPEN", ChecksTotal: 1, ChecksFailed: 1, ApprovedReviews: 1, Mergeable: "MERGEABLE"}
	e := newEngine(t, st, &fakeBackend{}, &fakeGH{status: red}, 30*time.Minute)
	e.goal = "merged"
	entry := time.Unix(1000, 0).UTC()
	e.now = func() time.Time { return entry.Add(31 * time.Minute) } // 31m elapsed >= 30m
	task := seedAt(t, st, "blocked_on_gate", 42, nil)
	seedBlockedEntry(t, st, task.ID, entry)

	final, err := e.drive(context.Background(), task)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if final != "escalated" {
		t.Fatalf("final = %q, want escalated (red gate past the timeout)", final)
	}
	if !hasAudit(auditFor(t, st, task.ID), "blocked_on_gate", "escalated", "timeout", "") {
		t.Error("missing audit blocked_on_gate->escalated timeout")
	}
}

// A suspended gate-wait task resumes without touching herdr: reconcile skips the
// pane Resolve for a no-agent state, so a herdr outage cannot stall the merge gate
// or its escalation timeout. Here Resolve errors (herdr unreachable), yet a green
// gate still advances the task to merging via Run (the daemon's resume path).
func TestRun_BlockedOnGate_ResumesWithoutHerdr(t *testing.T) {
	st := newStore(t)
	b := &fakeBackend{resolveErr: errors.New("herdr unreachable")}
	e := newEngine(t, st, b, &fakeGH{status: greenStatus()}, 30*time.Minute)
	e.goal = "merging"
	task := seedAt(t, st, "blocked_on_gate", 42, nil)
	seedBlockedEntry(t, st, task.ID, time.Unix(1000, 0).UTC())

	final, err := e.Run(context.Background(), task.Issue)
	if err != nil {
		t.Fatalf("Run: %v (a gate-wait resume must not depend on herdr)", err)
	}
	if final != "merging" {
		t.Fatalf("final = %q, want merging (gate green; resume must not fail at reconcile Resolve)", final)
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
