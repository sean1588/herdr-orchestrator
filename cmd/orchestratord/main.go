// Command orchestratord is the Herdr Orchestrator control-plane daemon and CLI.
//
// Phase 1 subcommands:
//
//	orchestratord validate <config.yaml>
//	    Validate a workflow config (JSON Schema + safety invariants).
//
//	orchestratord run --config <c> --repo <dir> --issue <n> [--base main] [--db path]
//	    Drive one issue through the pipeline to merged (or a terminal state).
//
//	orchestratord recover --config <c> --repo <dir> [--base main] [--db path]
//	    Reconcile and resume in-flight tasks against herdr panes and GitHub PRs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/engine"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/notify"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "validate":
		return cmdValidate(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "recover":
		return cmdRecover(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `usage: orchestratord <command> [args]

commands:
  validate <config.yaml>                         validate a workflow config
  run --config <c> --repo <dir> --issue <n>      drive one issue to merged
  recover --config <c> --repo <dir>              reconcile/resume in-flight tasks

run/recover flags:
  --config PATH          workflow config (required)
  --repo PATH            local repo checkout (required)
  --issue N              issue number (run only, required)
  --base BRANCH          base branch (default "main")
  --db PATH              sqlite store path (default "orchestrator.db")
  --worktrees-dir PATH   parent dir for worktrees (default: sibling of repo)
  --task-dir PATH        dir for task context files (default: temp dir)
  --notify-webhook URL   POST escalation/alert events as JSON (default: none)
`)
}

// cmdValidate validates a workflow config, exit 0 if valid (warnings allowed),
// 1 on validation failure, 2 on usage/IO error.
func cmdValidate(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: orchestratord validate <config.yaml>")
		return 2
	}
	wf, warnings, err := config.Load(args[0])
	for _, w := range warnings {
		fmt.Printf("  WARN  %s\n", w)
	}
	var ve *config.ValidationErrors
	if errors.As(err, &ve) {
		for _, e := range ve.Errors {
			fmt.Printf("  ERROR %s\n", e)
		}
		fmt.Printf("\nFAIL: %d error(s), %d warning(s)\n", len(ve.Errors), len(warnings))
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	fmt.Printf("\nOK: %q valid (%d warning(s))\n", wf.Name, len(warnings))
	return 0
}

// commonFlags are shared by run and recover.
type commonFlags struct {
	config, repo, base, db, worktreesDir, taskDir string
	notifyWebhook                                 string
}

func registerCommon(fs *flag.FlagSet, cf *commonFlags) {
	fs.StringVar(&cf.config, "config", "", "workflow config path (required)")
	fs.StringVar(&cf.repo, "repo", "", "local repo checkout dir (required)")
	fs.StringVar(&cf.base, "base", "main", "base branch")
	fs.StringVar(&cf.db, "db", "orchestrator.db", "sqlite store path")
	fs.StringVar(&cf.worktreesDir, "worktrees-dir", "", "parent dir for worktrees (default: sibling of repo)")
	fs.StringVar(&cf.taskDir, "task-dir", "", "dir for task context files (default: temp dir)")
	fs.StringVar(&cf.notifyWebhook, "notify-webhook", "", "POST escalation/alert events as JSON to this URL (default: none)")
}

// wire loads+validates the config and builds the engine with real backends.
func (cf commonFlags) wire(ctx context.Context) (*engine.Engine, *store.Store, error) {
	wf, warnings, err := config.Load(cf.config)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  WARN  %s\n", w)
	}

	absRepo, err := filepath.Abs(cf.repo)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve repo dir: %w", err)
	}

	st, err := store.Open(ctx, cf.db)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}

	runner := proc.New()
	backend := exec.NewHerdr(runner)
	if cf.worktreesDir != "" {
		backend.WorktreesDir = cf.worktreesDir
	}

	// nil notifier => engine.New defaults to notify.Nop (no out-of-band delivery).
	var notifier notify.Notifier
	if cf.notifyWebhook != "" {
		notifier = notify.Webhook{URL: cf.notifyWebhook}
	}

	eng := engine.New(engine.Config{
		Workflow:  wf,
		Backend:   backend,
		GitHub:    github.New(runner),
		Store:     st,
		RepoDir:   absRepo,
		Base:      cf.base,
		Repo:      repoSlug(wf),
		ConfigDir: filepath.Dir(cf.config),
		TaskDir:   cf.taskDir,
		Notifier:  notifier,
	})
	return eng, st, nil
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var cf commonFlags
	registerCommon(fs, &cf)
	issue := fs.Int("issue", 0, "issue number (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cf.config == "" || cf.repo == "" || *issue <= 0 {
		fmt.Fprintln(os.Stderr, "run requires --config, --repo, and --issue")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eng, st, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer st.Close()

	final, err := eng.Run(ctx, *issue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "run issue %d: %v\n", *issue, err)
		return 1
	}
	fmt.Printf("issue %d -> %s\n", *issue, final)
	// merged = real merge; merging = dry-run halt (policies.dry_run withheld it).
	switch final {
	case "merged", "merging":
		return 0
	default:
		return 1
	}
}

func cmdRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	var cf commonFlags
	registerCommon(fs, &cf)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cf.config == "" || cf.repo == "" {
		fmt.Fprintln(os.Stderr, "recover requires --config and --repo")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eng, st, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer st.Close()

	if err := eng.Recover(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "recover: %v\n", err)
		return 1
	}
	fmt.Println("reconcile complete")
	return 0
}

// reportConfigErr prints a config validation failure (refusing to run) or a
// generic wiring error.
func reportConfigErr(err error) int {
	var ve *config.ValidationErrors
	if errors.As(err, &ve) {
		for _, e := range ve.Errors {
			fmt.Fprintf(os.Stderr, "  ERROR %s\n", e)
		}
		fmt.Fprintf(os.Stderr, "refusing to run: config invalid (%d error(s))\n", len(ve.Errors))
		return 1
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

// repoSlug returns the owner/name of the first github_issues source, recorded on
// tasks for reference (Phase 1 PR detection uses the local repo dir + branch).
func repoSlug(wf *config.Workflow) string {
	for _, s := range wf.Sources {
		if s.Type == "github_issues" && s.Repo != "" {
			return s.Repo
		}
	}
	return ""
}
