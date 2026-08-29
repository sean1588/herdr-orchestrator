package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/doctor"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// cmdDoctor preflights the environment and reports one line per check, with a
// fix for anything that is not passing. Exits non-zero if any check failed, so
// it composes: `orchestratord doctor && orchestratord daemon ...`.
func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var cf commonFlags
	registerCommon(fs, &cf)
	quick := fs.Bool("quick", false, "skip the expensive checks (does not launch an agent)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cf.config == "" {
		fmt.Fprintln(os.Stderr, "doctor requires --config (and --repo, to check the checkout)")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	env, err := cf.doctorEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 2
	}
	results := doctor.Run(ctx, env, !*quick)
	reportDoctor(os.Stdout, results)
	if doctor.Failed(results) {
		return 1
	}
	return 0
}

// doctorEnv builds the check environment from the shared flags. The config is
// read (not validated) here: an unparseable config is itself a check result, so
// it must not abort the run before the report is printed.
func (cf commonFlags) doctorEnv() (doctor.Env, error) {
	src, err := os.ReadFile(cf.config)
	if err != nil {
		return doctor.Env{}, fmt.Errorf("read config %q: %w", cf.config, err)
	}
	env := doctor.Env{
		Runner:       proc.WithTimeout(proc.New(), cf.commandTimeout),
		ConfigPath:   cf.config,
		ConfigSource: src,
		Base:         cf.base,
		DBPath:       cf.db,
		WorktreesDir: cf.worktreesDir,
	}
	if cf.repo != "" {
		abs, err := filepath.Abs(cf.repo)
		if err != nil {
			return doctor.Env{}, fmt.Errorf("resolve repo dir: %w", err)
		}
		env.RepoDir = abs
	}
	// A config that does not parse still yields a report; the checks that need a
	// workflow report "skip" rather than the command refusing to run.
	if wf, _, perr := config.Parse(src); perr == nil {
		env.Workflow = wf
		env.RepoSlug = repoSlug(wf)
		if label, lerr := sourceLabel(wf); lerr == nil {
			env.Label = label
		}
		backend := exec.NewHerdr(env.Runner)
		backend.RepoDir = env.RepoDir
		if cf.worktreesDir != "" {
			backend.WorktreesDir = cf.worktreesDir
		}
		env.Smoker = backend
	}
	return env, nil
}

// reportDoctor prints one aligned line per check, with an indented fix under
// anything that is not passing — so the output is scannable when everything is
// fine and actionable when it is not.
func reportDoctor(w io.Writer, results []doctor.Result) {
	width := 0
	for _, r := range results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	for _, r := range results {
		fmt.Fprintf(w, "  %-6s %-*s  %s\n", marker(r.Status), width, r.Name, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(w, "         %-*s  → %s\n", width, "", r.Fix)
		}
	}
	c := doctor.Counts(results)
	fmt.Fprintf(w, "\n%d passed, %d warning(s), %d failed, %d skipped\n",
		c[doctor.StatusPass], c[doctor.StatusWarn], c[doctor.StatusFail], c[doctor.StatusSkip])
	if c[doctor.StatusFail] > 0 {
		fmt.Fprintln(w, "FAIL: fix the failures above before starting the daemon")
	}
}

func marker(s doctor.Status) string {
	switch s {
	case doctor.StatusPass:
		return "ok"
	case doctor.StatusWarn:
		return "WARN"
	case doctor.StatusFail:
		return "FAIL"
	default:
		return "skip"
	}
}
