# Herdr Orchestrator

[![CI](https://github.com/sean1588/herdr-orchestrator/actions/workflows/ci.yml/badge.svg)](https://github.com/sean1588/herdr-orchestrator/actions/workflows/ci.yml)

Turns GitHub issues into merged pull requests, unattended.

Label an issue, and a daemon spawns a coding agent in its own git worktree,
waits for the PR, spawns a second agent to review it against a rubric, sends it
back with the findings until it passes, and squash-merges once CI is green and
the branch is mergeable. Every state, gate, and timeout is in one YAML file. Every
merge is decided by GitHub's own signals, never by an agent's say-so.

Agents run in [herdr](https://herdr.dev) panes, so you can watch any of them
work, and the daemon exposes a small control surface you can drive from Claude
Code or `curl`.

## Get started

You need three things installed: [herdr](https://herdr.dev)
(`curl -fsSL https://herdr.dev/install.sh | sh`), the [GitHub CLI](https://cli.github.com)
logged in, and [Claude Code](https://claude.com/claude-code). Everything else is
set up for you.

Open herdr, and from a pane inside it:

```sh
git clone https://github.com/sean1588/herdr-orchestrator
cd herdr-orchestrator
claude
```

Then:

```
/start owner/repo
```

`/start` installs the daemon, points it at your repo (creating the repo if you
don't have one), asks whether it should merge on its own or stop just short,
checks the whole environment with `orchestratord doctor`, and starts. It then
asks one question: **plan something new**, and it works out the issues with
you, records which depend on which, and feeds them in; or **run issues you
already have**, and it labels the ones you name. After that it supervises
until the queue drains and only speaks up when something needs you.

Prefer to drive it yourself, or use a different agent? Every step is plain
markdown and plain commands: [TUTORIAL.md](TUTORIAL.md) walks through it by
hand, and [RUNBOOK.md](RUNBOOK.md) is the reference the agent operates from
(written for it, readable by you).

## Install

`/start` does this for you. By hand: download a prebuilt binary for macOS or
Linux (amd64 / arm64) from the
[releases page](https://github.com/sean1588/herdr-orchestrator/releases) into
any directory on your `PATH`, for example:

```sh
VERSION=v0.1.0   # pick the latest from the releases page
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/sean1588/herdr-orchestrator/releases/download/${VERSION}/orchestratord_${VERSION}_darwin_arm64.tar.gz" \
  | tar -xz -C ~/.local/bin orchestratord
orchestratord version
```

The binaries are unsigned; a tarball fetched in a browser needs
`xattr -d com.apple.quarantine ~/.local/bin/orchestratord` on macOS. Each release
carries a `checksums.txt`. Or, with Go 1.26+:
`go install github.com/sean1588/herdr-orchestrator/cmd/orchestratord@latest`
(`orchestratord version` then prints `dev`; only release binaries carry a tag).

## What it does with an issue

```
intake ──triage──▶ queued ──▶ implementing ──PR opened──▶ pr_open ──review──▶ approved
                                    ▲                                    │
                                    └──── changes_requested ◀── request_changes
approved ──CI green + mergeable──▶ merging ──▶ merged
anything that stalls, exhausts its retries, or needs a human ──▶ escalated
```

Triage and review are agent decisions against rubrics you can edit. The step
into `merging` is a gate on GitHub's checks and mergeability, and the config
validator refuses any workflow where a merge could be reached another way. By
default the pipeline stops just short of the real merge (`dry_run: true`)
until you say otherwise.

## Documentation

| | |
|---|---|
| [TUTORIAL.md](TUTORIAL.md) | Human-paced walkthrough: build, validate, drive one issue, go live |
| [RUNBOOK.md](RUNBOOK.md) | The agent's operating reference: bring-up, supervision, escalations, teardown. Written for the agent, readable by a human |
| [docs/WORKFLOW.md](docs/WORKFLOW.md) | The YAML config: states, gates, decisions, liveness bounds, the seven safety invariants |
| [docs/CLI.md](docs/CLI.md) | Every `orchestratord` command and the MCP control surface |
| [docs/DESIGN.md](docs/DESIGN.md) | Architecture and the review → merge loop in detail |
| [ROADMAP.md](ROADMAP.md) | What is deferred and why |
| [`.claude/`](.claude/) | The `/start` command and the three skills it runs: `setup-orchestrator`, `plan-issues`, `operate-orchestrator` |

## Contributing

```sh
go build ./... && go test ./... && go vet ./... && gofmt -l .
```

Pure Go, no cgo, three dependencies. [CLAUDE.md](CLAUDE.md) has the
conventions. This repo runs its own issues through the orchestrator; most
recent PRs were written, reviewed, and merged by it.

## License

[Apache 2.0](LICENSE).
