package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// checkGHTokenScopes says up front what the token may push. An implementer
// whose issue touches `.github/workflows/` commits fine and is then refused at
// push without the `workflow` scope — and the fix is an interactive
// `gh auth refresh` only a human can run. A missing `workflow` scope warns
// rather than fails: most issues never touch CI.
func checkGHTokenScopes(ctx context.Context, env Env) Result {
	const name = "gh-token-scopes"
	out, err := env.Runner.Run(ctx, "", env.GHBin, "auth", "status")
	if err != nil {
		return skip(name, "gh is not authenticated; see gh-auth")
	}
	scopes, ok := activeTokenScopes(string(out))
	switch {
	case !ok:
		return warn(name, "token scopes unreadable (a fine-grained or environment token prints none)",
			"if pushes fail, check the token's permissions")
	case !scopes["repo"]:
		return fail(name, "token lacks the `repo` scope; the pipeline cannot push branches at all",
			"run `gh auth refresh -h github.com -s repo`")
	case !scopes["workflow"]:
		return warn(name, "token cannot push files under .github/workflows/; an issue that touches CI will stall at push",
			"run `gh auth refresh -h github.com -s workflow`")
	}
	return pass(name, "token can push code and workflow files")
}

// activeTokenScopes parses the `Token scopes:` line of the active account out of
// `gh auth status`, e.g. `  - Token scopes: 'gist', 'read:org', 'repo'`. gh
// lists every logged-in account, each opened by a "Logged in to" line; the
// first scopes line outside an `Active account: false` block is the active one.
// ok is false when no scopes line is printed at all.
func activeTokenScopes(status string) (scopes map[string]bool, ok bool) {
	inactive := false
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimLeft(strings.TrimSpace(line), "-✓ ")
		switch {
		case strings.HasPrefix(line, "Logged in to"):
			inactive = false
		case strings.HasPrefix(line, "Active account:"):
			inactive = strings.TrimSpace(strings.TrimPrefix(line, "Active account:")) == "false"
		case strings.HasPrefix(line, "Token scopes:") && !inactive:
			scopes = map[string]bool{}
			for _, s := range strings.Split(strings.TrimPrefix(line, "Token scopes:"), ",") {
				if s = strings.Trim(strings.TrimSpace(s), `'"`); s != "" {
					scopes[s] = true
				}
			}
			return scopes, true
		}
	}
	return nil, false
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

// mergeFix names the two real remedies for a base branch this account cannot
// squash-merge into, rather than restating the symptom.
const mergeFix = "either remove the requirement (drop required approvals — GitHub forbids approving " +
	"your own PR — drop the squash or update restriction, and enable squash merging), " +
	"or keep it and run with `dry_run: true` so tasks stop at the merge gate for a human to merge"

// checkGHMergeAllowed asks whether the pipeline's last step can succeed: is this
// account allowed to squash-merge into the base branch? Without it, a protected
// base lets a task pass every signal from triage to the merge gate and then
// fail at `gh pr merge --squash` — the worst possible moment on a first run.
//
// A blocker fails only when the daemon would actually merge (dry_run: false);
// under dry_run nothing merges yet, so it is said, not refused. Classic
// protection that cannot be read is a warning: like a pane that cannot be read,
// a missing admin scope must never be what stops the daemon.
func checkGHMergeAllowed(ctx context.Context, env Env) Result {
	const name = "gh-merge-allowed"
	if env.RepoSlug == "" || env.Workflow == nil {
		return skip(name, "no source repo declared in the workflow")
	}
	target := env.RepoSlug + ":" + env.Base
	blockers, unreadable, err := mergeBlockers(ctx, env)
	if err != nil {
		return warn(name, err.Error(), fmt.Sprintf("verify by hand that this account can squash-merge into %s", target))
	}
	if len(blockers) > 0 {
		detail := fmt.Sprintf("a squash merge into %s would be refused: %s", target, strings.Join(blockers, "; "))
		if env.Workflow.Policies.DryRunEnabled() {
			return warn(name, detail+" (dry_run is on, so nothing merges yet)", mergeFix)
		}
		return fail(name, detail, mergeFix)
	}
	if unreadable != nil {
		return warn(name, fmt.Sprintf("no ruleset blocks a squash merge into %s, but classic branch protection is unreadable: %v",
			target, unreadable),
			fmt.Sprintf("confirm by hand (or as a repo admin) that %s requires no approvals; if it does, %s", env.Base, mergeFix))
	}
	return pass(name, fmt.Sprintf("nothing blocks a squash merge into %s", target))
}

// mergeBlockers makes the three reads that together answer the question. The
// ruleset and classic-protection endpoints are both needed: a repo can carry
// either or both, and neither reports the other's rules. unreadable carries a
// classic-protection read that failed for any reason other than "not
// protected" — it needs admin, and a non-admin gets 403 (or, on a repo it
// cannot push to, 404 "Not Found", which is why the message is matched rather
// than the status).
func mergeBlockers(ctx context.Context, env Env) (blockers []string, unreadable, err error) {
	api := func(path string) ([]byte, error) {
		return env.Runner.Run(ctx, env.RepoDir, env.GHBin, "api", path)
	}

	out, err := api("repos/" + env.RepoSlug)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %w", env.RepoSlug, err)
	}
	var repo struct {
		// Absent for an account without push access; only an explicit false blocks.
		AllowSquashMerge *bool `json:"allow_squash_merge"`
	}
	if err := json.Unmarshal(out, &repo); err != nil {
		return nil, nil, fmt.Errorf("cannot parse %s: %w", env.RepoSlug, err)
	}
	if repo.AllowSquashMerge != nil && !*repo.AllowSquashMerge {
		blockers = append(blockers, "squash merging is disabled on the repo (allow_squash_merge: false)")
	}

	out, err = api(fmt.Sprintf("repos/%s/rules/branches/%s", env.RepoSlug, env.Base))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read the rulesets on %s: %w", env.Base, err)
	}
	var rules []struct {
		Type       string `json:"type"`
		RulesetID  int64  `json:"ruleset_id"`
		Parameters struct {
			RequiredApprovingReviewCount int      `json:"required_approving_review_count"`
			AllowedMergeMethods          []string `json:"allowed_merge_methods"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(out, &rules); err != nil {
		return nil, nil, fmt.Errorf("cannot parse the rulesets on %s: %w", env.Base, err)
	}
	for _, r := range rules {
		switch r.Type {
		case "pull_request":
			if n := r.Parameters.RequiredApprovingReviewCount; n > 0 {
				blockers = append(blockers, fmt.Sprintf("ruleset %d requires %d approving review(s)", r.RulesetID, n))
			}
			if m := r.Parameters.AllowedMergeMethods; len(m) > 0 && !slices.Contains(m, "squash") {
				blockers = append(blockers, fmt.Sprintf("ruleset %d does not allow squash merges", r.RulesetID))
			}
		case "update":
			blockers = append(blockers, fmt.Sprintf("ruleset %d restricts updates to bypass actors", r.RulesetID))
		}
	}

	out, err = api(fmt.Sprintf("repos/%s/branches/%s/protection", env.RepoSlug, env.Base))
	switch {
	case err != nil && strings.Contains(err.Error(), "Branch not protected"):
		// No classic protection: nothing to add.
	case err != nil:
		unreadable = err
	default:
		var prot struct {
			RequiredPullRequestReviews *struct {
				RequiredApprovingReviewCount int `json:"required_approving_review_count"`
			} `json:"required_pull_request_reviews"`
		}
		if err := json.Unmarshal(out, &prot); err != nil {
			return nil, nil, fmt.Errorf("cannot parse the branch protection on %s: %w", env.Base, err)
		}
		if r := prot.RequiredPullRequestReviews; r != nil && r.RequiredApprovingReviewCount > 0 {
			blockers = append(blockers, fmt.Sprintf("branch protection requires %d approving review(s)", r.RequiredApprovingReviewCount))
		}
	}
	return blockers, unreadable, nil
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
	dir := spawnsDir(env)
	if dir == "" {
		return skip(name, "no --repo or --worktrees-dir given")
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
	// The default dir sits inside the checkout; keep it out of the user's
	// `git status` without touching their committed .gitignore.
	if env.WorktreesDir == "" {
		if err := excludeFromGit(ctx, env, orchestratorDirPattern); err != nil {
			return warn(name, fmt.Sprintf("%s is writable, but %s could not be added to the repo's git exclude: %v",
				dir, orchestratorDirPattern, err),
				fmt.Sprintf("add the line %s to .git/info/exclude in %s by hand, or `git status` there will list it",
					orchestratorDirPattern, env.RepoDir))
		}
	}
	return pass(name, dir)
}

// orchestratorDirPattern is the exclude line covering the default worktrees dir.
const orchestratorDirPattern = ".orchestrator/"

// spawnsDir is the directory task worktrees are created in: --worktrees-dir, or
// the backend's default, <repo>/.orchestrator/worktrees ("" when neither is
// known). It mirrors exec.Herdr.worktreeDir: inside the checkout, so the folder
// trust the user granted the repo covers every spawn.
func spawnsDir(env Env) string {
	if env.WorktreesDir != "" {
		return env.WorktreesDir
	}
	if env.RepoDir == "" {
		return ""
	}
	return filepath.Join(env.RepoDir, ".orchestrator", "worktrees")
}

// excludeFromGit ensures pattern is a line of the repo's info/exclude, which is
// local and never committed. The git dir is asked of git, not assumed to be
// <repo>/.git: in a linked worktree .git is a file, and exclude lives in the
// common dir.
func excludeFromGit(ctx context.Context, env Env, pattern string) error {
	out, err := env.Runner.Run(ctx, env.RepoDir, env.GitBin, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve the git dir: %w", err)
	}
	gitDir := firstLine(out)
	if gitDir == "" {
		return fmt.Errorf("git rev-parse --git-common-dir printed nothing")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(env.RepoDir, gitDir)
	}
	path := filepath.Join(gitDir, "info", "exclude")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	b = append(b, pattern+"\n"...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// checkTaskDir creates the task-file directory and proves it writable — the
// engine writes every rubric, context, and verdict file there, and a missing
// dir used to fail every drive with a per-poll WARN while the task wedged in
// intake until its state timeout.
func checkTaskDir(ctx context.Context, env Env) Result {
	const name = "task-dir"
	if env.TaskDir == "" {
		return skip(name, "no --task-dir given; the engine defaults to the OS temp dir")
	}
	if err := os.MkdirAll(env.TaskDir, 0o755); err != nil {
		return fail(name, fmt.Sprintf("cannot create %s: %v", env.TaskDir, err),
			"point --task-dir at a writable directory")
	}
	f, err := os.CreateTemp(env.TaskDir, ".orchestratord-doctor-*")
	if err != nil {
		return fail(name, fmt.Sprintf("%s is not writable: %v", env.TaskDir, err),
			"point --task-dir at a writable directory; every task's context and verdict files live there")
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return pass(name, env.TaskDir)
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

	// The scratch dir lives where real spawns run, not under $TMPDIR: the agent's
	// behavior depends on its directory (Claude Code's folder-trust dialog appears
	// only where no ancestor is trusted), so a smoke test run anywhere else can
	// pass while every spawn dies, or fail for a reason no task will ever hit.
	dir := env.TempDir
	if dir == "" {
		parent := spawnsDir(env)
		if parent == "" {
			return skip(name, "no --repo or --worktrees-dir given")
		}
		d, err := os.MkdirTemp(parent, ".doctor-")
		if err != nil {
			return fail(name, fmt.Sprintf("cannot create a scratch dir in %s: %v", parent, err),
				"see the worktrees-dir check")
		}
		defer os.RemoveAll(d)
		dir = d
	}

	if err := env.Smoker.SmokeKickoff(ctx, dir, launch, smokeKickoffText); err != nil {
		return fail(name, fmt.Sprintf("%s did not accept a kickoff in %s: %v", strings.Join(launch, " "), dir, err),
			"this is the failure that escalates tasks having done nothing. If the agent opened a first-launch "+
				"dialog in that directory (e.g. a folder-trust prompt), launch it once by hand in the worktrees "+
				"dir and accept. Otherwise check the agent CLI version and that it starts cleanly by hand in a "+
				"herdr pane; if it opens but ignores the kickoff, its input handling has changed and "+
				"exec.deliverKickoff needs a new delivery method")
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
