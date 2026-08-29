package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// smokeKickoffText is what the scratch agent is asked to do. It must be a
// single line (the pane shell receives it verbatim), harmless, and something an
// agent answers immediately — the check is about DELIVERY, not about the answer.
const smokeKickoffText = "Reply with the single word OK. Do not run any tools or make any changes."

func checkConfig(ctx context.Context, env Env) Result {
	const name = "config"
	if env.ConfigPath == "" && env.Workflow == nil {
		return skip(name, "no config given")
	}
	src := env.ConfigSource
	if len(src) == 0 {
		b, err := os.ReadFile(env.ConfigPath)
		if err != nil {
			return fail(name, fmt.Sprintf("cannot read %s: %v", env.ConfigPath, err),
				"point --config at a readable workflow YAML")
		}
		src = b
	}
	_, warnings, err := config.Parse(src)
	if err != nil {
		return fail(name, err.Error(), "fix the config, then re-run `orchestratord validate <config>`")
	}
	if len(warnings) > 0 {
		return warn(name, strings.Join(warnings, "; "), "review the warnings; none of them block startup")
	}
	return pass(name, "valid, no warnings")
}

func checkHerdrBinary(ctx context.Context, env Env) Result {
	const name = "herdr-binary"
	path, err := env.LookPath(env.HerdrBin)
	if err != nil {
		return fail(name, fmt.Sprintf("%q not on PATH", env.HerdrBin),
			"install herdr and ensure it is on the daemon's PATH")
	}
	out, err := env.Runner.Run(ctx, "", env.HerdrBin, "--version")
	if err != nil {
		// Present but unusable is worse than absent, because everything downstream
		// will fail in a way that does not name herdr.
		return fail(name, fmt.Sprintf("%s: --version failed: %v", path, err),
			"check the herdr install; the orchestrator drives it entirely through this binary")
	}
	return pass(name, fmt.Sprintf("%s (%s)", path, firstLine(out)))
}

func checkHerdrServer(ctx context.Context, env Env) Result {
	const name = "herdr-server"
	if _, err := env.Runner.Run(ctx, "", env.HerdrBin, "pane", "list"); err != nil {
		return fail(name, fmt.Sprintf("cannot reach the herdr server: %v", err),
			"start it with `herdr server`; the daemon cannot spawn or observe any agent without it")
	}
	return pass(name, "reachable")
}

// checkAgentBinary resolves every distinct agent command the workflow's roles
// launch. A missing agent binary is the failure that looks most like "the agent
// did nothing": the pane opens, the launch fails, and the task rides to its
// timeout with an empty prompt.
func checkAgentBinary(ctx context.Context, env Env) Result {
	const name = "agent-binary"
	if env.Workflow == nil {
		return skip(name, "no workflow loaded")
	}
	seen := map[string]bool{}
	var cmds []string
	for _, r := range env.Workflow.Roles {
		if len(r.Launch) > 0 && !seen[r.Launch[0]] {
			seen[r.Launch[0]] = true
			cmds = append(cmds, r.Launch[0])
		}
	}
	if len(cmds) == 0 {
		return skip(name, "no roles declare a launch command")
	}
	var missing, found []string
	for _, c := range sortedStrings(cmds) {
		if p, err := env.LookPath(c); err != nil {
			missing = append(missing, c)
		} else {
			found = append(found, p)
		}
	}
	if len(missing) > 0 {
		return fail(name, fmt.Sprintf("not on PATH: %s", strings.Join(missing, ", ")),
			"install the agent CLI, or point roles.*.launch at its absolute path")
	}
	return pass(name, strings.Join(found, ", "))
}

func checkGHAuth(ctx context.Context, env Env) Result {
	const name = "gh-auth"
	if _, err := env.LookPath(env.GHBin); err != nil {
		return fail(name, fmt.Sprintf("%q not on PATH", env.GHBin), "install the GitHub CLI")
	}
	if _, err := env.Runner.Run(ctx, "", env.GHBin, "auth", "status"); err != nil {
		return fail(name, "gh is not authenticated", "run `gh auth login`")
	}
	return pass(name, "authenticated")
}

// checkGHTokenEnv warns about a token in the environment. The daemon already
// scrubs GITHUB_TOKEN/GH_TOKEN for its own gh calls (a PAT lacking checks:read
// 403s the check-runs API and silently breaks the ci_green gate), but agents
// inherit the full environment, so it is still worth naming.
//
// It reports only that a variable is SET, never any part of its value.
func checkGHTokenEnv(ctx context.Context, env Env) Result {
	const name = "gh-token-env"
	var set []string
	for _, v := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if env.Getenv(v) != "" {
			set = append(set, v)
		}
	}
	if len(set) == 0 {
		return pass(name, "no token in the environment; gh will use its stored OAuth credentials")
	}
	return warn(name, fmt.Sprintf("%s set in the environment", strings.Join(set, ", ")),
		"the daemon scrubs these for its own gh calls, but spawned agents inherit them; "+
			"unset them unless an agent needs one")
}

func checkGHRepo(ctx context.Context, env Env) Result {
	const name = "gh-repo"
	if env.RepoSlug == "" {
		return skip(name, "no source repo declared in the workflow")
	}
	if _, err := env.Runner.Run(ctx, env.RepoDir, env.GHBin, "repo", "view", env.RepoSlug, "--json", "nameWithOwner"); err != nil {
		return fail(name, fmt.Sprintf("cannot read %s: %v", env.RepoSlug, err),
			"check the repo slug in the workflow's sources, and that your gh account can see it")
	}
	return pass(name, env.RepoSlug)
}

// checkGHLabel confirms the source label exists. A missing label is not fatal —
// the daemon simply finds no work — but it is the single most likely reason for
// "the orchestrator is running and nothing happens".
func checkGHLabel(ctx context.Context, env Env) Result {
	const name = "gh-label"
	if env.Label == "" || env.RepoSlug == "" {
		return skip(name, "no source label declared")
	}
	out, err := env.Runner.Run(ctx, env.RepoDir, env.GHBin, "label", "list", "--repo", env.RepoSlug, "--json", "name")
	if err != nil {
		return warn(name, fmt.Sprintf("could not list labels: %v", err),
			"verify by hand that the source label exists on the repo")
	}
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &labels); err != nil {
		return warn(name, fmt.Sprintf("could not parse the label list: %v", err), "verify the label by hand")
	}
	for _, l := range labels {
		if l.Name == env.Label {
			return pass(name, fmt.Sprintf("%q exists on %s", env.Label, env.RepoSlug))
		}
	}
	return warn(name, fmt.Sprintf("%q does not exist on %s", env.Label, env.RepoSlug),
		fmt.Sprintf("create it (`gh label create %s --repo %s`) or the daemon will poll forever and find nothing",
			env.Label, env.RepoSlug))
}

func checkRepoCheckout(ctx context.Context, env Env) Result {
	const name = "repo-checkout"
	if env.RepoDir == "" {
		return skip(name, "no --repo given")
	}
	if _, err := env.Runner.Run(ctx, env.RepoDir, env.GitBin, "rev-parse", "--is-inside-work-tree"); err != nil {
		return fail(name, fmt.Sprintf("%s is not a git checkout: %v", env.RepoDir, err),
			"point --repo at a local clone of the source repo")
	}
	return pass(name, env.RepoDir)
}

func checkBaseBranch(ctx context.Context, env Env) Result {
	const name = "repo-base-branch"
	if env.RepoDir == "" {
		return skip(name, "no --repo given")
	}
	if _, err := env.Runner.Run(ctx, env.RepoDir, env.GitBin, "rev-parse", "--verify", "refs/heads/"+env.Base); err != nil {
		return fail(name, fmt.Sprintf("base branch %q does not exist locally", env.Base),
			fmt.Sprintf("check out %s, or pass --base with the branch task worktrees should fork from", env.Base))
	}
	return pass(name, env.Base)
}

// checkBaseCurrent warns when the local base branch is behind its remote. Task
// worktrees fork from the LOCAL base, so a stale base silently produces PRs
// built on old code — work that looks fine until it conflicts at merge.
func checkBaseCurrent(ctx context.Context, env Env) Result {
	const name = "repo-base-current"
	if env.RepoDir == "" {
		return skip(name, "no --repo given")
	}
	// Fetch first, or "behind by 0" only means "behind by 0 as of whenever
	// someone last fetched", which is exactly the stale reading being checked for.
	if _, err := env.Runner.Run(ctx, env.RepoDir, env.GitBin, "fetch", "--quiet", "origin", env.Base); err != nil {
		return warn(name, fmt.Sprintf("could not fetch origin/%s: %v", env.Base, err),
			"check network/remote access; task worktrees fork from the local base branch")
	}
	out, err := env.Runner.Run(ctx, env.RepoDir, env.GitBin, "rev-list", "--count",
		fmt.Sprintf("%s..origin/%s", env.Base, env.Base))
	if err != nil {
		return warn(name, fmt.Sprintf("could not compare %s to origin/%s: %v", env.Base, env.Base, err),
			"verify by hand that the base branch is current")
	}
	behind, err := strconv.Atoi(firstLine(out))
	if err != nil {
		return warn(name, fmt.Sprintf("unexpected rev-list output %q", firstLine(out)),
			"verify by hand that the base branch is current")
	}
	if behind > 0 {
		return warn(name, fmt.Sprintf("%s is %d commit(s) behind origin/%s", env.Base, behind, env.Base),
			fmt.Sprintf("`git -C %s pull` — task worktrees fork from the local base, so agents would build on stale code",
				env.RepoDir))
	}
	return pass(name, fmt.Sprintf("%s is up to date with origin", env.Base))
}

func checkWorktreesDir(ctx context.Context, env Env) Result {
	const name = "worktrees-dir"
	dir := env.WorktreesDir
	if dir == "" {
		// The backend defaults to a sibling of the repo; check that instead of
		// reporting a pass for a directory nobody will use.
		if env.RepoDir == "" {
			return skip(name, "no --repo or --worktrees-dir given")
		}
		dir = filepath.Dir(env.RepoDir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(name, fmt.Sprintf("cannot create %s: %v", dir, err),
			"point --worktrees-dir at a writable directory")
	}
	f, err := os.CreateTemp(dir, ".orchestratord-doctor-*")
	if err != nil {
		return fail(name, fmt.Sprintf("%s is not writable: %v", dir, err),
			"point --worktrees-dir at a writable directory; every task gets its own worktree there")
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return pass(name, dir)
}

// checkStore opens the task database, which also applies migrations — so a
// schema that cannot be brought up to date surfaces here rather than on the
// first task write.
func checkStore(ctx context.Context, env Env) Result {
	const name = "store"
	if env.DBPath == "" {
		return skip(name, "no --db given")
	}
	st, err := store.Open(ctx, env.DBPath)
	if err != nil {
		return fail(name, fmt.Sprintf("cannot open %s: %v", env.DBPath, err),
			"point --db at a writable path on a local filesystem")
	}
	defer st.Close()
	if _, err := st.List(ctx); err != nil {
		return fail(name, fmt.Sprintf("cannot read tasks from %s: %v", env.DBPath, err),
			"the database may be corrupt; move it aside to start fresh (in-flight tasks would be lost)")
	}
	return pass(name, fmt.Sprintf("%s open, schema current", env.DBPath))
}

// checkKickoffDelivery is the check that pays for this whole command. Kickoff
// delivery has broken twice from underneath us — once when herdr's CLI changed,
// once when Claude Code stopped accepting typed text — and both times the
// failure was discovered by tasks escalating with no work done, ten minutes at a
// time. It launches the real agent in a scratch workspace and proves a kickoff
// is actually accepted, in seconds, before any task is at stake.
func checkKickoffDelivery(ctx context.Context, env Env) Result {
	const name = "kickoff-delivery"
	if env.Smoker == nil {
		return skip(name, "no execution backend wired")
	}
	launch := firstLaunch(env.Workflow)
	if len(launch) == 0 {
		return skip(name, "no role declares a launch command")
	}

	dir := env.TempDir
	if dir == "" {
		d, err := os.MkdirTemp("", "orchestratord-doctor-")
		if err != nil {
			return fail(name, fmt.Sprintf("cannot create a scratch dir: %v", err), "check TMPDIR")
		}
		defer os.RemoveAll(d)
		dir = d
	}

	if err := env.Smoker.SmokeKickoff(ctx, dir, launch, smokeKickoffText); err != nil {
		return fail(name, fmt.Sprintf("%s did not accept a kickoff: %v", strings.Join(launch, " "), err),
			"this is the failure that escalates tasks having done nothing. Check the agent CLI version "+
				"and that it starts cleanly by hand in a herdr pane; if it opens but ignores the kickoff, "+
				"its input handling has changed and exec.deliverKickoff needs a new delivery method")
	}
	return pass(name, fmt.Sprintf("%s accepted a kickoff", strings.Join(launch, " ")))
}

// firstLaunch returns the launch argv of the alphabetically-first role, so the
// smoke test is deterministic across runs (map iteration is not).
func firstLaunch(wf *config.Workflow) []string {
	if wf == nil {
		return nil
	}
	names := make([]string, 0, len(wf.Roles))
	for n := range wf.Roles {
		names = append(names, n)
	}
	for _, n := range sortedStrings(names) {
		if len(wf.Roles[n].Launch) > 0 {
			return wf.Roles[n].Launch
		}
	}
	return nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
