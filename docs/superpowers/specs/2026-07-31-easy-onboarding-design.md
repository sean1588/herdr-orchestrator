# Easy onboarding: generalize the orchestrator for other users

**Date:** 2026-07-31
**Status:** Approved

## Goal

A stranger (or their coding agent) clones this repo, is told a target GitHub
repo, and gets from zero to a supervised running daemon using nothing but
`RUNBOOK.md`. Today that is impossible: the only shipped config is a test
fixture (`internal/config/testdata/default-pipeline.yaml`) hardcoded to
`sean1588/minicode`, and the rubric prompts exist only under testdata.

## Design

### 1. `examples/` — the single source of truth for the shipped default

New repo-root directory:

- `examples/default-pipeline.yaml` — canonical default config, based on the
  current live testdata pipeline (triager role, intake 15m timeout,
  `blocked_on_gate` re-check loop). Two deltas from testdata:
  - `repo: your-github-user/your-repo` placeholder, with inline comments on
    the two lines users edit (repo, label).
  - `approvals` **dropped from both merge-gate lists** (`approved` and
    `blocked_on_gate`), so the default works first-try on single-account
    repos (closes #36 — GitHub forbids self-approval). The `approvals` gate
    stays *defined* in the `gates:` map with a comment: team repos add it
    back to the two gate lists.
- `examples/prompts/triage.md`, `examples/prompts/review.md` — copied
  verbatim from testdata (already fully generic).
- `examples/embed.go` — package `examples` exposing the directory as an
  `embed.FS` (`//go:embed` cannot reach parent dirs, so the FS lives beside
  the files). `internal/config/testdata/` remains an independent test
  fixture set — different purpose (exercising engine features), no sync
  contract. **Named debt:** two similar pipelines; the example is covered by
  its own validation test (below), testdata by the existing suites.

### 2. `orchestratord init` — scaffold a working config

```
orchestratord init --repo owner/name [--label agent-ready] [--dir .]
```

- Writes `<dir>/pipeline.yaml` + `<dir>/prompts/{triage,review}.md` from the
  embedded FS.
- Substitutes the placeholder repo token (`your-github-user/your-repo`) and,
  when `--label` differs from the default, the `agent-ready` label token.
  Exact-token string replacement — we control the embedded file, so no YAML
  rewriting (which would destroy comments).
- `--repo` is required and must look like `owner/name` (exactly one `/`,
  both halves non-empty) — guard clause, exit 2 on violation.
- **Refuses to overwrite:** pre-checks all three target paths before writing
  anything; any existing file aborts with exit 1 and no writes.
- Creates `<dir>` and `<dir>/prompts` as needed.
- Prints next steps on success: `orchestratord validate <dir>/pipeline.yaml`,
  create the label in the target repo, then RUNBOOK §3.
- Filesystem-only — no network. Creating the `agent-ready` label in the
  target repo stays a documented `gh label create` step.
- `usage()` and the package doc comment gain the new subcommand.

### 3. `RUNBOOK.md` — the true entry point

New section before bring-up: **"Zero to running (fresh clone)"** — written
for an agent told "operate the orchestrator for repo X" with nothing but
this file: build (`go build ./cmd/orchestratord` or `go install`),
`orchestratord init --repo X`, `gh label create agent-ready` in X, validate,
then flow into existing §3.1 prereqs / §3.2 daemon start. Includes a note
for non-Claude agents (e.g. Codex): `SKILL.md` is plain markdown — read it
directly and run the tick loop yourself; nothing depends on Claude's skill
system.

### 4. De-minicode + warning inversion sweep

- `RUNBOOK.md:88` sources example, `README.md:120`, `TUTORIAL.md:106/216` →
  placeholder repo.
- The single-account warnings in `RUNBOOK.md` §1 and
  `.claude/skills/operate-orchestrator/SKILL.md` invert: approvals is now
  opt-in, so the note becomes "team repos: add `approvals` back to the gate
  lists."

## Testing

- Embedded-example validation test: write the embedded FS to a temp dir,
  `config.Load` must succeed with no errors, and `engine.CheckExecutable`
  must return none.
- Table-driven `init` tests: happy path (files written, repo + label
  substituted, no placeholder remains, output validates via `config.Load`),
  refuse-overwrite (no partial writes), bad `--repo` shapes, `--dir`
  creation.
- All existing suites stay green; `go vet` + `gofmt` clean.

## Out of scope

- #37 (permission-wedge surfacing) — already documented in RUNBOOK §3.1.
- Release binaries / goreleaser — users build from the clone; the operator
  is an agent with a Go toolchain per RUNBOOK prereqs.
- `init` touching GitHub (label creation, repo checks).
