package doctor

import (
	"context"
	"os"
	osexec "os/exec"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// KickoffSmoker delivers a kickoff into a scratch workspace and tears it down.
// Narrow on purpose: doctor depends on the behavior it wants to prove, not on
// the herdr backend (exec.Herdr satisfies it).
type KickoffSmoker interface {
	SmokeKickoff(ctx context.Context, dir string, launch []string, kickoff string) error
}

// Env is everything the checks need. Every external dependency is injected, so
// the whole suite runs against a proc.Fake in tests.
type Env struct {
	Runner proc.Runner
	// Smoker runs the end-to-end kickoff proof. Nil skips that check.
	Smoker KickoffSmoker

	ConfigPath   string
	ConfigSource []byte // the raw config bytes, if the caller already read them
	Workflow     *config.Workflow

	RepoDir      string
	RepoSlug     string // owner/name, from the workflow's source
	Base         string
	Label        string
	WorktreesDir string
	DBPath       string

	GitBin, GHBin, HerdrBin string

	// Getenv reads the environment; injectable so the token check is testable
	// without mutating the process environment.
	Getenv func(string) string
	// LookPath resolves a command on PATH; injectable so the suite is hermetic
	// (CI has git and gh but neither herdr nor an agent CLI).
	LookPath func(string) (string, error)
	// TempDir is where the kickoff smoke test runs. Empty => os.MkdirTemp.
	TempDir string
}

func (e Env) withDefaults() Env {
	if e.GitBin == "" {
		e.GitBin = "git"
	}
	if e.GHBin == "" {
		e.GHBin = "gh"
	}
	if e.HerdrBin == "" {
		e.HerdrBin = "herdr"
	}
	if e.Base == "" {
		e.Base = "main"
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.LookPath == nil {
		e.LookPath = osexec.LookPath
	}
	return e
}
