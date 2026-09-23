package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

const testConfig = `
version: 0
name: test-pipeline
entry_state: intake
policies:
  max_concurrent_tasks: 1
sources:
  - id: gh_issues
    type: github_issues
    repo: owner/name
    select: { label: agent-ready }
    emits_to: intake
roles:
  implementer:
    launch: ["claude"]
  reviewer:
    launch: ["claude"]
gates:
  pr_exists: { type: github_pr, head: "{branch}" }
states:
  intake:
    transitions:
      - when: { event: scheduled }
        to: implementing
  implementing:
    entry: { spawn: implementer }
    transitions:
      - when: { event: agent.done }
        evaluate: { gate: pr_exists }
        branch: { pass: merged, fail: escalated }
      - when: { timeout: 45m }
        to: escalated
  merged: { terminal: success }
  escalated: { terminal: needs_human, alert: true }
`

type fakeSmoker struct {
	err       error
	calls     int
	launch    []string
	dir       string
	dirExists bool // whether dir existed when the smoke test ran in it
}

func (f *fakeSmoker) SmokeKickoff(ctx context.Context, dir string, launch []string, kickoff string) error {
	f.calls++
	f.launch = launch
	f.dir = dir
	_, err := os.Stat(dir)
	f.dirExists = err == nil
	return f.err
}

// healthyEnv is an environment where every check passes: every command the
// checks run succeeds, every binary resolves, the base is current.
func healthyEnv(t *testing.T) (Env, *proc.Fake) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "pipeline.yaml")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, _, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatalf("fixture config must be valid: %v", err)
	}

	f := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		switch {
		case c.Name == "herdr" && c.Args[0] == "--version":
			return []byte("herdr 0.8.2\n"), nil
		case c.Name == "gh" && c.Args[0] == "label":
			return []byte(`[{"name":"agent-ready"},{"name":"bug"}]`), nil
		case c.Name == "git" && c.Args[0] == "rev-list":
			return []byte("0\n"), nil
		case c.Name == "gh" && c.Args[0] == "api":
			return ghAPI(repoSquashAllowed, rulesNone, protectionNone)(c)
		}
		return nil, nil
	}}

	return Env{
		Runner:       f,
		Smoker:       &fakeSmoker{},
		ConfigPath:   cfgPath,
		ConfigSource: []byte(testConfig),
		Workflow:     wf,
		RepoDir:      dir,
		RepoSlug:     "owner/name",
		Base:         "main",
		Label:        "agent-ready",
		WorktreesDir: filepath.Join(dir, "wt"),
		TaskDir:      filepath.Join(dir, "tasks"),
		DBPath:       filepath.Join(dir, "test.db"),
		TempDir:      dir,
		Getenv:       func(string) string { return "" },
		LookPath:     func(c string) (string, error) { return "/usr/local/bin/" + c, nil },
	}, f
}

func byName(results []Result, name string) Result {
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	return Result{Name: name, Status: "missing"}
}

func TestRun_HealthyEnvironmentPassesEverything(t *testing.T) {
	env, _ := healthyEnv(t)
	results := Run(context.Background(), env, true)

	if len(results) != len(Checks()) {
		t.Fatalf("got %d results for %d checks", len(results), len(Checks()))
	}
	for _, r := range results {
		if r.Status != StatusPass {
			t.Errorf("check %q = %s (%s)", r.Name, r.Status, r.Detail)
		}
	}
	if Failed(results) {
		t.Error("a healthy environment must not report failure")
	}
}

// Every non-passing result has to carry a fix. A preflight that reports a
// symptom without a remedy is just a slower way to be confused.
func TestEveryNonPassingResultCarriesAFix(t *testing.T) {
	env, f := healthyEnv(t)
	f.Responder = func(c proc.Call) ([]byte, error) { return nil, errors.New("boom") }
	env.LookPath = func(c string) (string, error) { return "", errors.New("not found") }
	env.Getenv = func(string) string { return "tok" }
	env.DBPath = filepath.Join(t.TempDir(), "nonexistent-dir", "x.db")

	for _, r := range Run(context.Background(), env, true) {
		if (r.Status == StatusWarn || r.Status == StatusFail) && r.Fix == "" {
			t.Errorf("check %q is %s with no fix: %s", r.Name, r.Status, r.Detail)
		}
	}
}

func TestChecks_FailureModes(t *testing.T) {
	tests := []struct {
		name       string
		check      string
		mutate     func(*Env, *proc.Fake)
		wantStatus Status
		wantDetail string
	}{
		{
			name:  "gh not authenticated",
			check: "gh-auth",
			mutate: func(e *Env, f *proc.Fake) {
				f.Responder = func(c proc.Call) ([]byte, error) {
					if c.Name == "gh" && c.Args[0] == "auth" {
						return nil, errors.New("not logged in")
					}
					return nil, nil
				}
			},
			wantStatus: StatusFail,
			wantDetail: "not authenticated",
		},
		{
			name:       "a token in the environment is a warning, not a failure",
			check:      "gh-token-env",
			mutate:     func(e *Env, f *proc.Fake) { e.Getenv = func(v string) string { return "ghp_secret" } },
			wantStatus: StatusWarn,
			wantDetail: "GITHUB_TOKEN",
		},
		{
			name:  "base branch behind origin",
			check: "repo-base-current",
			mutate: func(e *Env, f *proc.Fake) {
				f.Responder = func(c proc.Call) ([]byte, error) {
					if c.Name == "git" && c.Args[0] == "rev-list" {
						return []byte("7\n"), nil
					}
					return nil, nil
				}
			},
			wantStatus: StatusWarn,
			wantDetail: "7 commit(s) behind",
		},
		{
			name:  "source label missing",
			check: "gh-label",
			mutate: func(e *Env, f *proc.Fake) {
				f.Responder = func(c proc.Call) ([]byte, error) {
					if c.Name == "gh" && c.Args[0] == "label" {
						return []byte(`[{"name":"bug"}]`), nil
					}
					return nil, nil
				}
			},
			wantStatus: StatusWarn,
			wantDetail: "does not exist",
		},
		{
			name:  "task dir not given is a skip, not a silent pass",
			check: "task-dir",
			mutate: func(e *Env, f *proc.Fake) {
				e.TaskDir = ""
			},
			wantStatus: StatusSkip,
			wantDetail: "temp dir",
		},
		{
			name:  "task dir cannot be created",
			check: "task-dir",
			mutate: func(e *Env, f *proc.Fake) {
				// A path below an existing FILE cannot be MkdirAll'd.
				e.TaskDir = filepath.Join(e.ConfigPath, "tasks")
			},
			wantStatus: StatusFail,
			wantDetail: "cannot create",
		},
		{
			name:  "herdr server unreachable",
			check: "herdr-server",
			mutate: func(e *Env, f *proc.Fake) {
				f.Responder = func(c proc.Call) ([]byte, error) {
					if c.Name == "herdr" && c.Args[0] == "pane" {
						return nil, errors.New("connection refused")
					}
					return []byte("herdr 0.8.2"), nil
				}
			},
			wantStatus: StatusFail,
			wantDetail: "cannot reach the herdr server",
		},
		{
			name:  "agent binary missing",
			check: "agent-binary",
			mutate: func(e *Env, f *proc.Fake) {
				e.LookPath = func(c string) (string, error) {
					if c == "claude" {
						return "", errors.New("not found")
					}
					return "/usr/local/bin/" + c, nil
				}
			},
			wantStatus: StatusFail,
			wantDetail: "not on PATH: claude",
		},
		{
			name:       "kickoff not accepted",
			check:      "kickoff-delivery",
			mutate:     func(e *Env, f *proc.Fake) { e.Smoker = &fakeSmoker{err: errors.New("agent still idle")} },
			wantStatus: StatusFail,
			wantDetail: "did not accept a kickoff",
		},
		{
			name:  "unparseable config",
			check: "config",
			mutate: func(e *Env, f *proc.Fake) {
				e.ConfigSource = []byte("version: 0\nname: broken\nstates: {}\n")
			},
			wantStatus: StatusFail,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, f := healthyEnv(t)
			tc.mutate(&env, f)
			got := byName(Run(context.Background(), env, true), tc.check)
			if got.Status != tc.wantStatus {
				t.Fatalf("check %q = %s (%s), want %s", tc.check, got.Status, got.Detail, tc.wantStatus)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// The token check must never echo the value it found — doctor output gets pasted
// into issues and chat logs.
func TestGHTokenEnvNeverPrintsTheValue(t *testing.T) {
	const secret = "ghp_thisisasecretvalue"
	env, _ := healthyEnv(t)
	env.Getenv = func(v string) string {
		if v == "GITHUB_TOKEN" {
			return secret
		}
		return ""
	}
	r := byName(Run(context.Background(), env, true), "gh-token-env")
	if strings.Contains(r.Detail+r.Fix, secret) {
		t.Fatalf("the token value leaked into the report: %+v", r)
	}
}

// Starting the daemon must never launch an agent, so the expensive check is
// skipped — and skipped visibly, not silently dropped from the report.
func TestQuickModeSkipsTheExpensiveCheckWithoutRunningIt(t *testing.T) {
	env, _ := healthyEnv(t)
	smoker := &fakeSmoker{}
	env.Smoker = smoker

	results := Run(context.Background(), env, false)
	r := byName(results, "kickoff-delivery")
	if r.Status != StatusSkip {
		t.Errorf("kickoff-delivery = %s, want skip", r.Status)
	}
	if smoker.calls != 0 {
		t.Errorf("quick mode launched an agent %d time(s)", smoker.calls)
	}
	if len(results) != len(Checks()) {
		t.Errorf("skipped checks must still appear in the report: %d of %d", len(results), len(Checks()))
	}
	if Failed(results) {
		t.Error("a skipped check must not fail the run")
	}
}

// Warnings are things worth saying, not reasons to refuse to start.
func TestFailedIgnoresWarnings(t *testing.T) {
	if Failed([]Result{{Status: StatusWarn}, {Status: StatusPass}, {Status: StatusSkip}}) {
		t.Error("warnings and skips must not count as failure")
	}
	if !Failed([]Result{{Status: StatusPass}, {Status: StatusFail}}) {
		t.Error("a failure must be reported")
	}
}

// The smoke test must exercise a role's real launch argv, picked deterministically
// (map iteration is not) so the report does not vary run to run.
func TestKickoffSmokeUsesADeterministicRoleLaunch(t *testing.T) {
	wf := &config.Workflow{Roles: map[string]config.Role{
		"zulu":        {Launch: []string{"zulu-agent"}},
		"implementer": {Launch: []string{"claude", "--flag"}},
		"reviewer":    {Launch: []string{"reviewer-agent"}},
	}}
	for i := 0; i < 20; i++ {
		got := firstLaunch(wf)
		if fmt.Sprint(got) != fmt.Sprint([]string{"claude", "--flag"}) {
			t.Fatalf("firstLaunch = %v on iteration %d, want the alphabetically-first role's argv", got, i)
		}
	}
}

// The smoke test must launch the agent where real spawns launch it: the agent's
// behavior depends on its directory (a folder-trust dialog appears only where no
// ancestor is trusted), so a scratch dir under $TMPDIR tests the wrong thing. The
// scratch dir must also be gone afterwards, pass or fail.
func TestKickoffSmokeRunsInsideTheWorktreesDir(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// worktreesDir is --worktrees-dir; "" exercises the backend's default,
		// a sibling of the repo.
		worktreesDir bool
		wantStatus   Status
	}{
		{name: "accepted", worktreesDir: true, wantStatus: StatusPass},
		{name: "not accepted", worktreesDir: true, err: errors.New("agent still unknown"), wantStatus: StatusFail},
		{name: "default worktrees dir", worktreesDir: false, wantStatus: StatusPass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := healthyEnv(t)
			root := t.TempDir()
			env.TempDir = ""
			env.RepoDir = filepath.Join(root, "repo")
			env.WorktreesDir = ""
			parent := root
			if tc.worktreesDir {
				env.WorktreesDir = filepath.Join(root, "wt")
				parent = env.WorktreesDir
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(parent, "existing"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			before := dirEntries(t, parent)
			smoker := &fakeSmoker{err: tc.err}
			env.Smoker = smoker

			got := checkKickoffDelivery(context.Background(), env)

			if got.Status != tc.wantStatus {
				t.Fatalf("kickoff-delivery = %s (%s), want %s", got.Status, got.Detail, tc.wantStatus)
			}
			rel, err := filepath.Rel(parent, smoker.dir)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				t.Errorf("smoke test ran in %s, want a scratch dir inside %s", smoker.dir, parent)
			}
			if !smoker.dirExists {
				t.Errorf("scratch dir %s did not exist when the agent launched", smoker.dir)
			}
			if _, err := os.Stat(smoker.dir); !os.IsNotExist(err) {
				t.Errorf("scratch dir %s still exists after the check (stat err %v)", smoker.dir, err)
			}
			if after := dirEntries(t, parent); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("%s left behind: before %v, after %v", parent, before, after)
			}
		})
	}
}

// apiReply is one scripted `gh api` response. stderr non-empty means the call
// failed the way gh fails: the body still on stdout, the message on stderr, and
// proc's runner folding stderr into the error.
type apiReply struct{ body, stderr string }

// Real response shapes. The unprotected trio is what this repo returns today
// (criterion 1 of the issue); the 403 is GitHub's reply to a non-admin reading
// classic protection; the "Not Found" 404 is what a repo the account cannot
// push to returns for the same read.
var (
	repoSquashAllowed  = apiReply{body: `{"full_name":"owner/name","allow_squash_merge":true,"allow_merge_commit":true,"allow_rebase_merge":true}`}
	repoSquashDisabled = apiReply{body: `{"full_name":"owner/name","allow_squash_merge":false,"allow_merge_commit":true,"allow_rebase_merge":true}`}

	rulesNone            = apiReply{body: `[]`}
	rulesRequireApproval = apiReply{body: `[{"type":"pull_request","parameters":{"required_approving_review_count":1,"dismiss_stale_reviews_on_push":false,"require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":false,"allowed_merge_methods":["merge","squash","rebase"]},"ruleset_source_type":"Repository","ruleset_source":"owner/name","ruleset_id":42}]`}
	rulesNoSquash        = apiReply{body: `[{"type":"pull_request","parameters":{"required_approving_review_count":0,"dismiss_stale_reviews_on_push":false,"require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":false,"allowed_merge_methods":["merge","rebase"]},"ruleset_source_type":"Repository","ruleset_source":"owner/name","ruleset_id":43}]`}
	rulesUnrelated       = apiReply{body: `[{"type":"deletion","ruleset_source_type":"Repository","ruleset_source":"owner/name","ruleset_id":44},{"type":"non_fast_forward","ruleset_source_type":"Repository","ruleset_source":"owner/name","ruleset_id":44}]`}

	protectionNone = apiReply{
		body:   `{"message":"Branch not protected","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"404"}`,
		stderr: "gh: Branch not protected (HTTP 404)",
	}
	protectionNoAdmin = apiReply{
		body:   `{"message":"Must have admin rights to Repository.","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"403"}`,
		stderr: "gh: Must have admin rights to Repository. (HTTP 403)",
	}
	protectionHidden = apiReply{
		body:   `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/branches/branch-protection#get-branch-protection","status":"404"}`,
		stderr: "gh: Not Found (HTTP 404)",
	}
	protectionRequireApproval = apiReply{body: `{"url":"https://api.github.com/repos/owner/name/branches/main/protection","required_pull_request_reviews":{"url":"https://api.github.com/repos/owner/name/branches/main/protection/required_pull_request_reviews","dismiss_stale_reviews":false,"require_code_owner_reviews":false,"require_last_push_approval":false,"required_approving_review_count":1},"enforce_admins":{"url":"https://api.github.com/repos/owner/name/branches/main/protection/enforce_admins","enabled":false}}`}
)

// ghAPI routes `gh api <path>` to the reply for the endpoint the path names.
func ghAPI(repo, rules, protection apiReply) func(proc.Call) ([]byte, error) {
	return func(c proc.Call) ([]byte, error) {
		path := c.Args[1]
		r := repo
		switch {
		case strings.HasSuffix(path, "/protection"):
			r = protection
		case strings.Contains(path, "/rules/branches/"):
			r = rules
		}
		if r.stderr != "" {
			return []byte(r.body), fmt.Errorf("gh api %s: exit status 1: %s", path, r.stderr)
		}
		return []byte(r.body), nil
	}
}

// mergeEnv is the healthy environment with the merge-permission reads scripted
// and dry_run set as given. Every other command keeps its healthy reply, so a
// Failed() verdict speaks for this check alone.
func mergeEnv(t *testing.T, dryRun bool, repo, rules, protection apiReply) Env {
	t.Helper()
	env, f := healthyEnv(t)
	healthy := f.Responder
	f.Responder = func(c proc.Call) ([]byte, error) {
		if c.Name == "gh" && c.Args[0] == "api" {
			return ghAPI(repo, rules, protection)(c)
		}
		return healthy(c)
	}
	env.Workflow.Policies.DryRun = &dryRun
	return env
}

// Each row of the issue's warn/fail table. A blocker fails only when the daemon
// would actually merge; everything else is said, not refused.
func TestGHMergeAllowed(t *testing.T) {
	tests := []struct {
		name                    string
		dryRun                  bool
		repo, rules, protection apiReply
		wantStatus              Status
		wantDetail              string
	}{
		{"unprotected base passes", false, repoSquashAllowed, rulesNone, protectionNone, StatusPass, "nothing blocks"},
		{"rules unrelated to merging pass", false, repoSquashAllowed, rulesUnrelated, protectionNone, StatusPass, "nothing blocks"},
		{"ruleset approvals, merging for real", false, repoSquashAllowed, rulesRequireApproval, protectionNone, StatusFail, "ruleset 42 requires 1 approving review"},
		{"ruleset approvals under dry_run", true, repoSquashAllowed, rulesRequireApproval, protectionNone, StatusWarn, "dry_run is on"},
		{"ruleset excludes squash, merging for real", false, repoSquashAllowed, rulesNoSquash, protectionNone, StatusFail, "does not allow squash"},
		{"classic approvals, merging for real", false, repoSquashAllowed, rulesNone, protectionRequireApproval, StatusFail, "branch protection requires 1 approving review"},
		{"classic approvals under dry_run", true, repoSquashAllowed, rulesNone, protectionRequireApproval, StatusWarn, "dry_run is on"},
		{"protection unreadable without admin", false, repoSquashAllowed, rulesNone, protectionNoAdmin, StatusWarn, "HTTP 403"},
		{"protection hidden behind a 404", false, repoSquashAllowed, rulesNone, protectionHidden, StatusWarn, "unreadable"},
		{"squash disabled, merging for real", false, repoSquashDisabled, rulesNone, protectionNone, StatusFail, "allow_squash_merge: false"},
		{"squash disabled under dry_run", true, repoSquashDisabled, rulesNone, protectionNone, StatusWarn, "allow_squash_merge: false"},
		{"a blocker outranks unreadable protection", false, repoSquashAllowed, rulesRequireApproval, protectionNoAdmin, StatusFail, "ruleset 42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := mergeEnv(t, tc.dryRun, tc.repo, tc.rules, tc.protection)
			got := byName(Run(context.Background(), env, false), "gh-merge-allowed")
			if got.Status != tc.wantStatus {
				t.Fatalf("gh-merge-allowed = %s (%s), want %s", got.Status, got.Detail, tc.wantStatus)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantDetail)
			}
			if got.Status != StatusPass && !strings.Contains(got.Fix, "dry_run: true") {
				t.Errorf("fix must name both remedies, got %q", got.Fix)
			}
			// A bypass actor clears neither this check (the rules endpoint lists
			// every active rule) nor the merge (plain --squash, no --admin).
			if strings.Contains(got.Fix, "bypass") {
				t.Errorf("fix must not offer a bypass actor as a remedy, got %q", got.Fix)
			}
		})
	}
}

// An operator who hits the folder-trust dialog needs to be told it is the likely
// cause, not sent to debug the agent CLI's input handling.
func TestKickoffFailureNamesFirstLaunchDialog(t *testing.T) {
	env, _ := healthyEnv(t)
	env.Smoker = &fakeSmoker{err: errors.New("agent still unknown")}
	got := checkKickoffDelivery(context.Background(), env)
	if got.Status != StatusFail || !strings.Contains(got.Fix, "first-launch dialog") {
		t.Errorf("fix = %q, want it to name a first-launch dialog", got.Fix)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// The warn/fail split, pinned at the level that matters: a blocked base under
// dry_run must not make the daemon refuse to start, and the same base with
// dry_run off must.
func TestFailed_BlockedBaseRefusesStartOnlyWhenMergingForReal(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		env := mergeEnv(t, dryRun, repoSquashAllowed, rulesRequireApproval, protectionNone)
		if got := Failed(Run(context.Background(), env, false)); got == dryRun {
			t.Errorf("dry_run=%v: Failed = %v, want %v", dryRun, got, !dryRun)
		}
	}
}
