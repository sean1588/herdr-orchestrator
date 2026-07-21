// Command orchestratord is the Herdr Orchestrator control-plane daemon and CLI.
//
// Phase 1 subcommands:
//
//	orchestratord validate <config.yaml>
//	    Validate a workflow config (JSON Schema + safety invariants).
//
//	orchestratord plan <config.yaml>
//	    Render the validated workflow's resolved graph, terminal and
//	    side-effecting states, and cycles (with their cap/timeout status).
//
//	orchestratord run --config <c> --repo <dir> --issue <n> [--base main] [--db path]
//	    Drive one issue through the pipeline to merged (or a terminal state).
//
//	orchestratord recover --config <c> --repo <dir> [--base main] [--db path]
//	    Reconcile and resume in-flight tasks against herdr panes and GitHub PRs.
//
//	orchestratord daemon --config <c> --repo <dir> [--poll-interval 30s]
//	    Poll a labeled GitHub source and drive up to max_concurrent_tasks issues
//	    through the pipeline concurrently. Runs until SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/engine"
	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/github"
	"github.com/sean1588/herdr-orchestrator/internal/mcp"
	"github.com/sean1588/herdr-orchestrator/internal/notify"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
	"github.com/sean1588/herdr-orchestrator/internal/scheduler"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// version is the binary's reported version. Overridable at build time via
// -ldflags "-X main.version=...".
var version = "dev"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "validate":
		return cmdValidate(args[1:])
	case "plan":
		return cmdPlan(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "recover":
		return cmdRecover(args[1:])
	case "daemon":
		return cmdDaemon(args[1:])
	case "version":
		return cmdVersion(os.Stdout)
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
  plan <config.yaml>                             render the resolved graph + invariants
  run --config <c> --repo <dir> --issue <n>      drive one issue to merged
  recover --config <c> --repo <dir>              reconcile/resume in-flight tasks
  daemon --config <c> --repo <dir>               poll a labeled source and drive concurrently
  version                                        print the orchestratord version

run/recover/daemon flags:
  --config PATH          workflow config (required)
  --repo PATH            local repo checkout (required)
  --issue N              issue number (run only, required)
  --base BRANCH          base branch (default "main")
  --db PATH              sqlite store path (default "orchestrator.db")
  --worktrees-dir PATH   parent dir for worktrees (default: sibling of repo)
  --task-dir PATH        dir for task context files (default: temp dir)
  --notify-webhook URL   POST escalation/alert events as JSON (default: none)
  --poll-interval DUR    daemon source poll cadence (default 30s)
  --mcp-listen ADDR      daemon MCP control server address, e.g. 127.0.0.1:7777 (default: off)
`)
}

// cmdVersion prints the binary's version on a single line and returns 0.
func cmdVersion(w io.Writer) int {
	fmt.Fprintf(w, "orchestratord %s\n", version)
	return 0
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
	// Beyond schema+semantic validity, verify every state is drivable by the
	// interpreter — a state with only a decision/gate/timeout trigger dead-ends
	// at runtime, which the graph-shape checks above do not catch.
	if xerrs := engine.CheckExecutable(wf); len(xerrs) > 0 {
		for _, e := range xerrs {
			fmt.Printf("  ERROR %s\n", e)
		}
		fmt.Printf("\nFAIL: %d error(s), %d warning(s)\n", len(xerrs), len(warnings))
		return 1
	}
	fmt.Printf("\nOK: %q valid (%d warning(s))\n", wf.Name, len(warnings))
	return 0
}

// cmdPlan renders a validated workflow's resolved graph + safety classification.
// Fail-closed: it refuses to render an invalid config (exit 1), same posture as
// run. Exit 2 on usage/IO error.
func cmdPlan(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: orchestratord plan <config.yaml>")
		return 2
	}
	wf, warnings, err := config.Load(args[0])
	var ve *config.ValidationErrors
	if errors.As(err, &ve) {
		for _, e := range ve.Errors {
			fmt.Fprintf(os.Stderr, "  ERROR %s\n", e)
		}
		fmt.Fprintf(os.Stderr, "refusing to render: config invalid (%d error(s))\n", len(ve.Errors))
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  WARN  %s\n", w)
	}
	writePlan(os.Stdout, wf)
	return 0
}

// writePlan renders the resolved transition graph with per-state markers
// ([terminal], [side-effecting], [wait_for]) and a cycles section noting whether
// each cycle is retry-capped/timeout-bounded. Read-only; reuses config.Analyze.
func writePlan(w io.Writer, wf *config.Workflow) {
	a := config.Analyze(wf)
	terminal := map[string]string{}
	for _, s := range a.Terminal {
		terminal[s] = wf.States[s].Terminal
	}
	side := map[string]bool{}
	for _, s := range a.SideEffecting {
		side[s] = true
	}

	entry := "(none)"
	if wf.EntryState != nil {
		entry = *wf.EntryState
	}
	fmt.Fprintf(w, "workflow: %s  (entry_state: %s)\n\n", wf.Name, entry)

	names := make([]string, 0, len(wf.States))
	for n := range wf.States {
		names = append(names, n)
	}
	slices.Sort(names)

	fmt.Fprintf(w, "states (%d):\n", len(names))
	for _, name := range names {
		tail := ""
		if v, ok := terminal[name]; ok {
			tail += "  [terminal:" + v + "]"
		}
		if side[name] {
			tail += "  [side-effecting]"
		}
		if wf.States[name].WaitFor != "" {
			tail += "  [wait_for:" + wf.States[name].WaitFor + "]"
		}
		if targets := a.Edges[name]; len(targets) > 0 {
			tail += "  ->  " + strings.Join(targets, ", ")
		}
		fmt.Fprintf(w, "  %s%s\n", name, tail)
	}

	fmt.Fprintf(w, "\ncycles (non-trivial SCCs):\n")
	found := false
	for _, comp := range a.SCCs {
		cyclic := len(comp) > 1 || (len(comp) == 1 && slices.Contains(a.Edges[comp[0]], comp[0]))
		if !cyclic {
			continue
		}
		found = true
		note := "retry-capped or timeout-bounded"
		if !cycleBounded(comp, wf) {
			note = "UNCAPPED (validation would have rejected this)"
		}
		c := append([]string(nil), comp...)
		slices.Sort(c)
		fmt.Fprintf(w, "  {%s}  %s\n", strings.Join(c, ", "), note)
	}
	if !found {
		fmt.Fprintln(w, "  (none)")
	}
}

// cycleBounded mirrors the validator (checkLoopsTerminate): a cycle terminates
// if any member state carries a retry cap or a timeout transition.
func cycleBounded(comp []string, wf *config.Workflow) bool {
	for _, n := range comp {
		if _, ok := wf.Policies.RetryCaps[n]; ok {
			return true
		}
		if wf.States[n].HasTimeoutTransition() {
			return true
		}
	}
	return false
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

// wired bundles everything a subcommand needs after loading + validating config.
type wired struct {
	eng     *engine.Engine
	store   *store.Store
	gh      github.Client
	wf      *config.Workflow
	repoDir string
}

// wire loads+validates the config and builds the engine with real backends.
func (cf commonFlags) wire(ctx context.Context) (*wired, error) {
	raw, err := os.ReadFile(cf.config)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", cf.config, err)
	}
	wf, warnings, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  WARN  %s\n", w)
	}
	// Fail fast: a config that passes schema+semantic validation can still have a
	// state the engine cannot drive (a runtime dead-end). Refuse to start rather
	// than silently re-drive-and-fail it every poll.
	if xerrs := engine.CheckExecutable(wf); len(xerrs) > 0 {
		return nil, fmt.Errorf("workflow not executable:\n  %s", strings.Join(xerrs, "\n  "))
	}

	absRepo, err := filepath.Abs(cf.repo)
	if err != nil {
		return nil, fmt.Errorf("resolve repo dir: %w", err)
	}

	st, err := store.Open(ctx, cf.db)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	runner := proc.New()
	backend := exec.NewHerdr(runner)
	backend.RepoDir = absRepo // lets Cleanup resolve a task's worktree path deterministically
	if cf.worktreesDir != "" {
		backend.WorktreesDir = cf.worktreesDir
	}

	// nil notifier => engine.New defaults to notify.Nop (no out-of-band delivery).
	var notifier notify.Notifier
	if cf.notifyWebhook != "" {
		notifier = notify.Webhook{URL: cf.notifyWebhook}
	}

	// StartState is the workflow's entry_state so `run`/`daemon` create tasks at
	// the pipeline front door (intake/triage). Empty entry_state falls back to the
	// engine default ("queued"), preserving pre-triage behavior.
	start := ""
	if wf.EntryState != nil {
		start = *wf.EntryState
	}

	// gh runs with GITHUB_TOKEN/GH_TOKEN scrubbed so it uses its stored OAuth token:
	// a PAT lacking checks:read 403s the check-runs API and breaks the ci_green gate.
	// The exec backend keeps the full env (agent launches may need it).
	gh := github.New(proc.NewScrubbed("GITHUB_TOKEN", "GH_TOKEN"))
	eng := engine.New(engine.Config{
		Workflow:       wf,
		WorkflowSource: raw,
		Backend:        backend,
		GitHub:         gh,
		Store:          st,
		RepoDir:        absRepo,
		Base:           cf.base,
		Repo:           repoSlug(wf),
		ConfigDir:      filepath.Dir(cf.config),
		TaskDir:        cf.taskDir,
		Notifier:       notifier,
		StartState:     start,
	})
	return &wired{eng: eng, store: st, gh: gh, wf: wf, repoDir: absRepo}, nil
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

	w, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer w.store.Close()

	final, err := w.eng.Run(ctx, *issue)
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

	w, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer w.store.Close()

	if err := w.eng.Recover(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "recover: %v\n", err)
		return 1
	}
	fmt.Println("reconcile complete")
	return 0
}

// cmdDaemon runs the orchestrator as a long-running daemon: poll a labeled source
// and drive up to max_concurrent_tasks issues through the pipeline concurrently.
func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	var cf commonFlags
	registerCommon(fs, &cf)
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "source poll cadence")
	mcpListen := fs.String("mcp-listen", "", "MCP control server listen address, e.g. 127.0.0.1:7777 (default: off)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cf.config == "" || cf.repo == "" {
		fmt.Fprintln(os.Stderr, "daemon requires --config and --repo")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	w, err := cf.wire(ctx)
	if err != nil {
		return reportConfigErr(err)
	}
	defer w.store.Close()

	label, err := sourceLabel(w.wf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return 2
	}
	settled := settledStates(w.wf)
	workers := w.wf.Policies.MaxConcurrentTasks
	if workers < 1 {
		workers = 1
	}

	dc := doneChecker{gh: w.gh, store: w.store, settled: settled, repoDir: w.repoDir, label: label, log: slog.Default()}

	sched := &scheduler.Scheduler{
		List: func(ctx context.Context) ([]int, error) {
			return w.gh.ListIssues(ctx, w.repoDir, label)
		},
		Done: dc.done,
		RunTask: func(ctx context.Context, issue int) error {
			_, err := w.eng.Run(ctx, issue)
			return err
		},
		SeedFrom: func(ctx context.Context) ([]int, error) {
			tasks, err := w.store.List(ctx)
			if err != nil {
				return nil, err
			}
			var out []int
			for _, tk := range tasks {
				if !settled[tk.CurrentState] {
					out = append(out, tk.Issue)
				}
			}
			return out, nil
		},
		Interval: *pollInterval,
		Workers:  workers,
		Log:      slog.Default(),
	}

	// Optional loopback MCP control server: shares the daemon's store handle
	// (reads) and the scheduler command seam (control), on the same signal ctx so
	// SIGINT tears both down. Binding is done synchronously so a misconfigured
	// address fails startup rather than silently running without the surface.
	if *mcpListen != "" {
		sched.EnableControl(engine.ErrOperatorCancel)
		ln, err := net.Listen("tcp", *mcpListen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: mcp listen %q: %v\n", *mcpListen, err)
			return 1
		}
		srv := mcp.New(w.store, sched, engine.TaskID, slog.Default())
		go func() {
			if err := srv.Serve(ctx, ln); err != nil {
				slog.Error("mcp server stopped", "err", err)
			}
		}()
		slog.Info("mcp control server listening", "addr", *mcpListen)
	}

	slog.Info("daemon starting", "label", label, "workers", workers, "poll", pollInterval.String())
	if err := sched.Serve(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return 1
	}
	slog.Info("daemon stopped")
	return 0
}

// sourceLabel returns the label of the first github_issues source, or an error
// if the workflow declares no such source with select.label.
func sourceLabel(wf *config.Workflow) (string, error) {
	for _, s := range wf.Sources {
		if s.Type == "github_issues" {
			if l, ok := s.Select["label"].(string); ok && l != "" {
				return l, nil
			}
		}
	}
	return "", fmt.Errorf("no github_issues source with select.label declared")
}

// terminalStates returns the set of state names with a terminal verdict.
func terminalStates(wf *config.Workflow) map[string]bool {
	out := map[string]bool{}
	for name, s := range wf.States {
		if s.Terminal != "" {
			out[name] = true
		}
	}
	return out
}

// settledStates returns the state names at which the daemon must stop re-driving
// an issue: every terminal state, plus — when dry_run is enabled — any merge_pr
// side-effecting state. dry_run (default-on) deliberately halts the engine at the
// merge gate ("merging"), a NON-terminal state; without treating that as settled,
// a completed issue would be re-driven on every poll (unbounded audit growth).
func settledStates(wf *config.Workflow) map[string]bool {
	out := terminalStates(wf)      // fresh map; safe to extend
	out[engine.CancelState] = true // an operator-cancelled task is terminal (daemon-owned)
	if wf.Policies.DryRunEnabled() {
		for name, s := range wf.States {
			if s.Entry != nil && s.Entry.Action == "merge_pr" {
				out[name] = true
			}
		}
	}
	return out
}

// doneChecker implements the scheduler's Done seam: it reports whether a polled
// issue's task has settled and, on the settled path, drains the source label so
// the poller stops re-listing it. It groups the poll-time dependencies so the
// scheduler callback stays a plain (ctx, issue) func.
type doneChecker struct {
	gh      github.Client
	store   *store.Store
	settled map[string]bool
	repoDir string
	label   string
	log     *slog.Logger
}

// done reports whether the issue's task is settled. A missing task is not
// settled (it has never been driven). When settled, the issue still carries the
// source label (it was just returned by ListIssues), so removing it drains the
// backlog: subsequent polls no longer list it. Removal is best-effort — a
// failure is logged and the issue reports done anyway, so the worker pool is
// never wedged; the still-labelled issue is simply retried next poll, and
// RemoveLabel is idempotent.
func (d doneChecker) done(ctx context.Context, issue int) (bool, error) {
	tk, err := d.store.GetTask(ctx, engine.TaskID(issue))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !d.settled[tk.CurrentState] {
		return false, nil
	}
	if err := d.gh.RemoveLabel(ctx, d.repoDir, issue, d.label); err != nil {
		d.log.Warn("remove source label failed", "issue", issue, "label", d.label, "err", err)
	}
	return true, nil
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
