// Package proc runs external commands (git, herdr, gh) behind a small,
// mockable interface so the execution and GitHub backends can be unit-tested
// by asserting the exact argv they construct.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner runs an external command and returns its standard output. dir is the
// working directory ("" inherits the parent's). Implementations must honor ctx
// cancellation and deadlines.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type osRunner struct {
	// dropEnv names environment variables to strip from the child's environment.
	// Empty means "inherit the parent env unchanged" (the common case).
	dropEnv []string
}

// New returns a Runner backed by os/exec that inherits the parent environment.
func New() Runner { return osRunner{} }

// NewScrubbed returns a Runner that strips the named variables from the child's
// environment. Used for `gh`: a GITHUB_TOKEN PAT lacking checks:read 403s the
// check-runs API and breaks the ci_green gate, so gh must run WITHOUT it and fall
// back to its stored OAuth token. Scoped to the GitHub client so agent launches
// still see the full environment.
func NewScrubbed(drop ...string) Runner { return osRunner{dropEnv: drop} }

func (o osRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(o.dropEnv) > 0 {
		cmd.Env = scrubEnv(os.Environ(), o.dropEnv)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
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
