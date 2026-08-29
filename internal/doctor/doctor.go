// Package doctor preflights the environment the daemon depends on.
//
// Every environmental failure in this project's history was discovered the same
// way: a task was accepted, an agent was spawned, and then something silently
// did not work until a timeout fired — herdr's CLI changing under us, Claude
// Code no longer accepting a typed kickoff, `gh` unauthenticated, a GITHUB_TOKEN
// PAT quietly breaking the checks gate. The daemon started happily against all
// of them, because it validated the config and nothing else.
//
// Each check answers one question, and a failure carries the fix rather than
// just the symptom. The expensive one (kickoff delivery) is what pays for the
// package: it is the only check that exercises the path whose regressions have
// actually cost tasks.
package doctor

import (
	"context"
	"sort"
)

// Status is a check's outcome.
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn" // works, but something is off enough to say so
	StatusFail Status = "fail"
	StatusSkip Status = "skip" // not applicable, or not run in this mode
)

// Result is one check's verdict.
type Result struct {
	Name   string
	Status Status
	Detail string // what was observed
	Fix    string // what to do about it; required for warn/fail
}

func pass(name, detail string) Result {
	return Result{Name: name, Status: StatusPass, Detail: detail}
}
func warn(name, detail, fix string) Result {
	return Result{Name: name, Status: StatusWarn, Detail: detail, Fix: fix}
}
func fail(name, detail, fix string) Result {
	return Result{Name: name, Status: StatusFail, Detail: detail, Fix: fix}
}
func skip(name, detail string) Result {
	return Result{Name: name, Status: StatusSkip, Detail: detail}
}

// Check is one preflight question.
type Check struct {
	Name string
	// Expensive marks a check that creates a workspace or launches an agent.
	// The daemon's startup preflight skips these; `doctor` runs them.
	Expensive bool
	Run       func(ctx context.Context, env Env) Result
}

// Checks returns every check in dependency order — the tools first, then what
// they are pointed at, then the end-to-end delivery proof. Order matters for
// readability: the first failure in the list is usually the cause of the rest.
func Checks() []Check {
	return []Check{
		{Name: "config", Run: checkConfig},
		{Name: "herdr-binary", Run: checkHerdrBinary},
		{Name: "herdr-server", Run: checkHerdrServer},
		{Name: "agent-binary", Run: checkAgentBinary},
		{Name: "gh-auth", Run: checkGHAuth},
		{Name: "gh-token-env", Run: checkGHTokenEnv},
		{Name: "gh-repo", Run: checkGHRepo},
		{Name: "gh-label", Run: checkGHLabel},
		{Name: "repo-checkout", Run: checkRepoCheckout},
		{Name: "repo-base-branch", Run: checkBaseBranch},
		{Name: "repo-base-current", Run: checkBaseCurrent},
		{Name: "worktrees-dir", Run: checkWorktreesDir},
		{Name: "store", Run: checkStore},
		{Name: "kickoff-delivery", Expensive: true, Run: checkKickoffDelivery},
	}
}

// Run executes every check and returns the results in check order. Checks are
// run sequentially, not concurrently: they shell out to the same three tools,
// and a readable, deterministic report is worth more here than a fast one.
//
// includeExpensive selects the mode — `doctor` passes true, the daemon's startup
// preflight passes false so starting the daemon never launches an agent.
func Run(ctx context.Context, env Env, includeExpensive bool) []Result {
	env = env.withDefaults()
	var out []Result
	for _, c := range Checks() {
		if c.Expensive && !includeExpensive {
			out = append(out, skip(c.Name, "launches an agent; run `orchestratord doctor` without --quick to include it"))
			continue
		}
		if ctx.Err() != nil {
			out = append(out, skip(c.Name, "cancelled"))
			continue
		}
		out = append(out, c.Run(ctx, env))
	}
	return out
}

// Failed reports whether any check failed. Warnings never fail a run: they are
// things worth saying, not reasons to refuse to start.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// Counts tallies results by status, for a one-line summary.
func Counts(results []Result) map[Status]int {
	m := map[Status]int{}
	for _, r := range results {
		m[r.Status]++
	}
	return m
}

// sortedStrings is a small helper for deterministic reporting of set-like data.
func sortedStrings(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}
