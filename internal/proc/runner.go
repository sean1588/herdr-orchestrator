// Package proc runs external commands (git, herdr, gh) behind a small,
// mockable interface so the execution and GitHub backends can be unit-tested
// by asserting the exact argv they construct.
//
// Every command carries a per-call budget. Nothing the daemon shells out to can
// hang forever: a `gh pr checks` that stalls on a network hiccup or a `git
// worktree add` waiting on a lock fails in seconds with a precise error instead
// of wedging its worker permanently. The budget is per call site, not global —
// `gh pr merge` should take seconds, while `herdr agent wait` may legitimately
// block for the better part of an hour (see WithBudget).
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds any single command that does not declare its own budget.
// Every command the daemon runs — git, gh, herdr — is a short, local, or
// single-API-call operation, so a two-minute ceiling is far above the honest
// worst case and far below "wedged". The one deliberate exception is
// herdr's blocking agent wait, which opts out via WithBudget.
const DefaultTimeout = 2 * time.Minute

// killGrace bounds how long Run waits after killing an over-budget command.
// Killing the command does not, on its own, unblock the wait: os/exec keeps
// reading the output pipes, and a grandchild that inherited them (a shell's
// still-running child) holds them open for as long as it lives — so a bounded
// command could still block its caller indefinitely. WaitDelay closes the pipes
// and gives up instead.
const killGrace = 5 * time.Second

// ErrBudgetExceeded is the cause returned when a command outlives its per-call
// budget. It is distinct from a caller-side cancellation (daemon shutdown, an
// operator cancel) so the two are never confused in a log or an escalation.
var ErrBudgetExceeded = errors.New("per-call budget exceeded")

// Runner runs an external command and returns its standard output. dir is the
// working directory ("" inherits the parent's). Implementations must honor ctx
// cancellation and deadlines.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type budgetKey struct{}

// WithBudget returns a context whose commands use budget d instead of the
// runner's default. A non-positive d removes the budget entirely, leaving only
// the caller's own deadline — reserved for commands that block by design, like
// `herdr agent wait`, which carries its own `--timeout`.
//
// The budget composes with, and never overrides, an existing deadline: whichever
// expires first wins.
func WithBudget(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, budgetKey{}, d)
}

// budgetFrom returns the budget in effect for ctx, falling back to def.
func budgetFrom(ctx context.Context, def time.Duration) time.Duration {
	if d, ok := ctx.Value(budgetKey{}).(time.Duration); ok {
		return d
	}
	return def
}

type osRunner struct {
	// dropEnv names environment variables to strip from the child's environment.
	// Empty means "inherit the parent env unchanged" (the common case).
	dropEnv []string
	// timeout is the default per-call budget; WithBudget overrides it per call.
	timeout time.Duration
}

// New returns a Runner backed by os/exec that inherits the parent environment
// and applies DefaultTimeout to every command.
func New() Runner { return osRunner{timeout: DefaultTimeout} }

// NewScrubbed returns a Runner that strips the named variables from the child's
// environment. Used for `gh`: a GITHUB_TOKEN PAT lacking checks:read 403s the
// check-runs API and breaks the ci_green gate, so gh must run WITHOUT it and fall
// back to its stored OAuth token. Scoped to the GitHub client so agent launches
// still see the full environment.
func NewScrubbed(drop ...string) Runner { return osRunner{dropEnv: drop, timeout: DefaultTimeout} }

// WithTimeout returns a copy of r whose default per-call budget is d. A
// non-positive d disables the default budget. Runners that are not os-backed
// (the test Fake) are returned unchanged.
func WithTimeout(r Runner, d time.Duration) Runner {
	o, ok := r.(osRunner)
	if !ok {
		return r
	}
	o.timeout = d
	return o
}

func (o osRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	// Track the caller's context separately, so a budget expiry is reported as a
	// timeout while a caller-side cancel (shutdown, operator cancel) still
	// surfaces as itself — the engine's cancel reclassification depends on the
	// distinction.
	callerCtx := ctx
	budget := budgetFrom(ctx, o.timeout)
	if budget > 0 {
		bctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		ctx = bctx
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = killGrace
	if len(o.dropEnv) > 0 {
		cmd.Env = scrubEnv(os.Environ(), o.dropEnv)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A command killed by its budget surfaces from os/exec as "signal: killed",
		// which says nothing about why. Report the budget explicitly and wrap
		// ErrBudgetExceeded so callers can tell a wedge from a genuine failure.
		if ctx.Err() != nil && callerCtx.Err() == nil {
			return stdout.Bytes(), fmt.Errorf("%s %s: %w after %s",
				name, strings.Join(args, " "), ErrBudgetExceeded, budget)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// scrubEnv returns env with every entry whose KEY is in drop removed.
func scrubEnv(env, drop []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !contains(drop, key) {
			out = append(out, kv)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
