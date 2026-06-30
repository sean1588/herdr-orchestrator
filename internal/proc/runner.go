// Package proc runs external commands (git, herdr, gh) behind a small,
// mockable interface so the execution and GitHub backends can be unit-tested
// by asserting the exact argv they construct.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs an external command and returns its standard output. dir is the
// working directory ("" inherits the parent's). Implementations must honor ctx
// cancellation and deadlines.
type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type osRunner struct{}

// New returns a Runner backed by os/exec.
func New() Runner { return osRunner{} }

func (osRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
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
