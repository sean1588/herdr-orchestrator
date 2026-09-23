---
name: setup-orchestrator
description: >-
  Install and configure herdr-orchestrator for a target GitHub repo and start
  its daemon: prerequisites (herdr, gh, the agent CLI), the orchestratord
  binary, the run directory, the source label, the one auto-merge question,
  doctor until green, daemon in its own herdr pane, MCP registered. Idempotent —
  checks before it changes anything. Use when asked to set up, install, or
  configure the orchestrator, and as step 1 of /start.
---

# Set up the orchestrator

**Outcome:** an `orchestratord` daemon running against `<owner>/<name>` in its
own herdr pane, `orchestratord doctor` fully green, the control surface
registered, and a short summary of where everything lives.

## Rules

- **Check, then act.** Every step starts with a check; if it passes, say so in
  one line and move on. Running this twice must change nothing the second time.
- **Ask nothing you can find out.** Versions, paths, whether a repo exists,
  whether a label exists: look.
- **Interactive logins are the user's.** `gh auth login` and `gh auth refresh`
  open a browser. Tell the user the exact command and that they can run it in
  this session as `! <command>`; wait; re-check.
- **Say what you install before you install it**, in one line, then do it.
  Nothing here needs `sudo`; everything goes under `~/.local/bin` or the Go
  bin dir.
- **The one question with a real trade-off is auto-merge** (step 7). Ask it in
  outcomes, never in flag names.

Paths used below. `<clone>` is this repo's checkout (you are in it).
`<name>` is the target repo's name. The run directory is `~/orchestrator-<name>`.

## 1. herdr

herdr is the execution substrate: it owns the panes the agents run in.

```bash
herdr status          # server: status: running  → done
```

- `herdr` not found → install it: `curl -fsSL https://herdr.dev/install.sh | sh`,
  then re-check.
- Installed but no server running → the user has to open herdr themselves and
  start Claude from a pane inside it, because the panes the daemon creates live
  in that server. Tell them: "Start `herdr` in a terminal, open a pane, run
  `claude` there, and run `/start` again." Stop here; there is no way around it.

## 2. gh

```bash
gh auth status        # need: logged in, and Token scopes including 'repo' and 'workflow'
```

- Not installed → install from https://cli.github.com (macOS with Homebrew:
  `brew install gh`; otherwise the release tarball into `~/.local/bin`).
- Not logged in → user runs `! gh auth login -h github.com -s repo,workflow`.
- Logged in without `workflow` → user runs
  `! gh auth refresh -h github.com -s workflow`. Without it, any issue that
  touches `.github/workflows/` stalls at push.

Once the scopes are confirmed, always run:

```bash
gh auth setup-git     # idempotent
```

It makes git push with gh's token; otherwise git keeps whatever token its own
credential helper (e.g. the macOS keychain) cached, and a scope added above
never reaches `git push`.

## 3. The agent CLI

```bash
which claude && claude --version
```

The shipped workflow launches `claude`. If it is missing, install Claude Code
(https://claude.com/claude-code) and re-check. (A different agent CLI works too;
change `roles.*.launch` in the config in step 6 and use its name here.)

## 4. orchestratord

```bash
orchestratord version   # any output → installed; skip to 5
```

Prefer a release binary; build only when there is none.

```bash
gh release view -R sean1588/herdr-orchestrator --json tagName --jq .tagName
```

- **A release exists**: download the asset for this platform (`uname -s`,
  `uname -m` → `orchestratord_<tag>_darwin_arm64.tar.gz` etc.) into
  `~/.local/bin/`, `chmod +x`, and make sure `~/.local/bin` is on `PATH`.
- **No release**: needs Go 1.26+ (`go version`). If Go is missing, say so and
  install the official tarball from https://go.dev/dl into `~/.local/go`
  (add `~/.local/go/bin` to `PATH`). Then, from `<clone>`:

  ```bash
  go install ./cmd/orchestratord
  ```

  That lands in `$(go env GOPATH)/bin`. If that dir is not on `PATH`, copy the
  binary to `~/.local/bin/` as a **fresh file** (`rm -f` any old one first) and,
  on macOS, re-sign it: `codesign --force -s - ~/.local/bin/orchestratord`.
  Overwriting a signed binary in place gets it killed on launch.

Re-check `orchestratord version`.

## 5. The target repo

Resolve what the user gave (an `owner/name`, a URL, or nothing) to `<owner>/<name>`.

```bash
gh repo view <owner>/<name> --json name,defaultBranchRef --jq '.defaultBranchRef.name'
```

- **Exists** → note the default branch (`<base>`).
- **Does not exist** or the user has none → ask two things: the name, and
  public or private. Then `gh repo create <owner>/<name> --<visibility>
  --clone --add-readme` from the directory that should contain the checkout.
- **Local checkout**: ask where it is if you cannot find it (look for a sibling
  of `<clone>` named `<name>` first). If none, `gh repo clone <owner>/<name>
  <clone-parent>/<name>`. Call it `<repo-dir>`, absolute.

Trust: the agents run inside `<repo-dir>/.orchestrator/worktrees`, so the
trust the user already granted the checkout covers them. If the user has never
opened `claude` in `<repo-dir>`, `doctor` (step 8) will say so.

## 6. The run directory

```bash
ls ~/orchestrator-<name>/pipeline.yaml   # exists → skip init
orchestratord init --repo <owner>/<name> --dir ~/orchestrator-<name>
gh label list -R <owner>/<name> --json name --jq '.[].name' | grep -qx agent-ready \
  || gh label create agent-ready -R <owner>/<name> --description "Queued for the orchestrator"
orchestratord validate ~/orchestrator-<name>/pipeline.yaml
```

`init` writes `pipeline.yaml` and `prompts/`. Edit nothing else yet.

## 7. The auto-merge question

Ask, in these words:

> When a PR has passed review and CI, should the orchestrator **merge it
> itself**, or **stop just short** and leave every merge to you?
>
> Stopping short is safe for a first look but it is not "merge later": a task
> that stops short is finished as far as the orchestrator is concerned, and you
> merge that PR by hand.

- Merge itself → set `dry_run: false` in `~/orchestrator-<name>/pipeline.yaml`.
- Stop short → leave `dry_run: true`.

Write a one-line comment above the setting with the date and the user's
answer, so the next reader knows it was chosen, not defaulted. Re-run
`validate`.

## 8. doctor, until green

```bash
orchestratord doctor --config ~/orchestrator-<name>/pipeline.yaml \
  --repo <repo-dir> --db ~/orchestrator-<name>/orchestrator.db \
  --task-dir ~/orchestrator-<name>/tasks
```

Every failing line comes with its fix. Apply fixes you can (create a dir, a
label, re-sign a binary); hand the user the ones you cannot (a login, a
folder-trust dialog: "open `claude` once in `<repo-dir>` and accept, then tell
me"). Re-run until it reports `0 failed`. The last check launches a real agent
once to prove kickoff delivery; that is the point, let it.

**Unattended permissions.** Spawned agents run without
`--dangerously-skip-permissions`. If `~/.claude/settings.json` has no
`permissions.allow` list covering `Bash, Edit, Write, Read, Glob, Grep,
TodoWrite, BashOutput, KillShell, Task`, tell the user that without it agents
stall on the first permission prompt, that it is global to every Claude
session on this machine, and ask before adding it. Record that it was added in
the run directory's summary so it can be reverted.

## 9. Start the daemon

Pick a free loopback port (start at 7777; `lsof -nP -iTCP:<port> -sTCP:LISTEN`).
Start the daemon in its **own** herdr pane, not this one:

```bash
WS=$(herdr workspace create --cwd ~/orchestrator-<name> --label orchestratord-<name> --no-focus)
PANE=$(printf '%s' "$WS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])')
herdr pane run "$PANE" "orchestratord daemon --config ~/orchestrator-<name>/pipeline.yaml \
  --repo <repo-dir> --db ~/orchestrator-<name>/orchestrator.db \
  --task-dir ~/orchestrator-<name>/tasks --mcp-listen 127.0.0.1:<port> \
  --event-log ~/orchestrator-<name>/events.jsonl 2>&1 | tee -a ~/orchestrator-<name>/daemon.log"
```

Verify: `daemon starting` in the log, the port listening, and

```bash
curl -s 127.0.0.1:<port>/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}'
```

returns a result. Then register the control surface once:

```bash
claude mcp add --transport http orchestrator http://127.0.0.1:<port>/mcp
```

(Its tools appear in the next Claude session; this one uses the `curl` form.)

Write `~/orchestrator-<name>/run.env` with `REPO=<owner>/<name>`,
`REPO_DIR=<repo-dir>`, `BASE=<base>`, `PORT=<port>`, `PANE=$PANE`,
`DRY_RUN=<true|false>`, and `PERMISSIONS_ADDED=<yes|no>`, so the other skills
and a later session can find the run without asking.

## 10. Hand back

Report, in under ten lines: repo, run directory, daemon pane and port, whether
it merges on its own, and anything the user must undo later (the permissions
list). Then return to `/start` step 2.
