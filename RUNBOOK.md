# Operator Runbook

You are operating **Herdr Orchestrator**: a daemon that turns labeled GitHub
issues into merged pull requests, unattended. This runbook is your end-to-end
map — bring the system up from cold, keep it flowing, recover it, and tear it
down. It is written for an autonomous agent operator, but reads fine for a human.

Your moment-to-moment job — what to check each pass and how to react — lives in
the **`operate-orchestrator` skill** (`.claude/skills/operate-orchestrator/SKILL.md`).
This runbook is the whole lifecycle *around* that loop; the skill is the loop.
Deeper design and the full config contract are in [README.md](README.md); a
human-paced walkthrough is in [TUTORIAL.md](TUTORIAL.md).

---

## 1. What the orchestrator is

A fixed **state-graph engine** (the mechanism) interprets a declarative YAML
**workflow** (the policy). A *task* is one GitHub issue moving as a token through
a directed graph of states; the **engine — never a model — owns every
transition**. Judgment enters only at constrained `decision` points (an LLM
returns one of a closed set of verdicts), and irreversible side effects (the
merge) are reachable only through `gate` evaluations over **authoritative**
sources. **GitHub is the source of truth for artifacts** — an agent reporting
"done" is only a trigger to go *check* GitHub for the real PR/CI/review state.
The engine is the **single writer** of durable task state (SQLite), so
concurrent drives never race.

The pieces you operate:

- **The daemon** (`orchestratord daemon`) — polls a label, and drives up to
  `max_concurrent_tasks` issues concurrently (one poller, N workers, one store).
- **herdr** — the execution substrate. The daemon shells out to it to create an
  isolated git worktree + pane per task and launch the coding agent there.
- **The MCP control surface** — an optional loopback server the daemon exposes so
  you can observe and steer tasks without killing the daemon.
- **The `operate-orchestrator` skill** — you, on a `/loop`, supervising all of
  the above.

### The pipeline at a glance

The shipped `default-pipeline.yaml` drives this graph (states are config, not
code — read the actual config for the workflow you run):

| State | What it means | Leaves via |
| --- | --- | --- |
| `intake` | triager agent triages the issue | `triage` decision → `queued` / `closed` / `escalated`; 15m timeout |
| `queued` | accepted, awaiting a worker | auto `scheduled` → `implementing` |
| `implementing` | implementer agent writing code in a worktree | `agent.done` + `pr_exists` gate → `pr_open` / `escalated`; 45m timeout |
| `pr_open` | reviewer agent reviewing the PR | `review` decision → `approved` / `changes_requested` / `escalated` |
| `changes_requested` | implementer resumes with review feedback | `agent.done` + `pr_exists` gate → `pr_open` / `escalated`; `retry_exhausted` → `escalated` |
| `approved` | merge gate being evaluated | gate pass → `merging`, fail → `blocked_on_gate` |
| `blocked_on_gate` | merge gate not green; **suspended**, re-checked each poll | gate pass → `merging`; 30m timeout → `escalated` |
| `merging` | runs the `merge_pr` action (**withheld under `dry_run`**) | `pr.merged` → `merged` |
| `merged` | ✅ terminal (success) | — |
| `closed` | terminal (triage rejected) | — |
| `escalated` | 🚨 terminal — **needs a human** | — |
| `cancelled` | terminal — an operator cancelled the drive | — |

`merged` / `closed` / `escalated` / `cancelled` are **settled**: the daemon stops
re-driving them and removes the source label. Under `dry_run: true` (the shipped
default) the real merge is withheld and the task halts at `merging` — that is a
*success* halt, not a failure.

---

## 2. How work flows in: GitHub issues & labels

The daemon does not invent work — it **polls a labeled source**. Every poll it
lists the repo's open issues carrying the source label; each labeled issue
becomes a task and is driven from the workflow's `entry_state` (triage first).

Both the repo and the label live in the config's **`sources`** block:

```yaml
sources:
  - id: gh_issues
    type: github_issues
    repo: sean1588/minicode      # which repo's issues to poll
    select:
      label: agent-ready         # only issues with THIS label are picked up
    emits_to: intake
```

So, to know what a running daemon watches: **read its `--config`'s `sources`
block** (or ask the daemon — it logs `label=…` at startup). The daemon resolves
the label via the first `github_issues` source's `select.label`.

Operating implications:

- **To feed work:** apply the source label (`agent-ready`) to an issue in that
  repo. The next poll picks it up and runs it through triage → implement →
  review → merge-gate.
- **The label is auto-removed on settle.** When a task reaches a settled state,
  the daemon drains the label so the poller stops re-listing it. A settled issue
  is done from the daemon's point of view.
- **To hold work back:** don't apply the label (or let triage `reject` /
  `needs_human` it). Removing the label from an *in-flight* issue does **not**
  stop the drive — use `cancel_task` for that (§5).
- Triage is the front door: a labeled issue still has to pass the `triage`
  decision (`accept` / `reject` / `needs_human`) before it reaches `implementing`.

---

## 3. Bring-up (cold start)

### 3.1 Prerequisites — verify all before starting

| Requirement | Check | Why |
| --- | --- | --- |
| Running **inside a herdr pane** | `echo $HERDR_ENV` → `1` | the backend shells out to `herdr`; outside a pane it can't reach the session |
| **`gh` authenticated** for the target repo | `gh auth status` | issue reads, PR detection, reviews, merge |
| A **local checkout** of the target repo | `ls <repo-dir>` | the engine makes per-task worktrees beside it (`--repo`, absolute path) |
| The **agent CLI** on `PATH` | `which claude` (or whatever `roles.*.launch` names) | the daemon launches it per task |
| A **valid config** + its `prompts/` rubrics | `orchestratord validate <config>` → exit 0 | run/daemon refuse to start on any error; rubric paths resolve beside the config |
| **Unattended permissions armed** (for hands-off runs) | see below | a coding agent otherwise stalls on per-command permission prompts |

**Arming unattended permissions:** agents run **non-root, without**
`--dangerously-skip-permissions`, so for hands-off operation pre-authorize them:
a `permissions.allow` list (`Bash`, `Edit`, `Write`, `Read`, `Glob`, `Grep`) in
`~/.claude/settings.json`, plus a pre-accepted trust entry for the worktree path.
Treat this as temporary global state and **revert it after the run**. Full recipe
in [TUTORIAL.md](TUTORIAL.md) §11.

### 3.2 Start the daemon (in its own pane, with the MCP surface on)

Run the daemon in a **dedicated herdr pane** you can see and restart:

```bash
orchestratord daemon \
  --config   <config.yaml> \
  --repo     <abs/path/to/checkout> \
  --db       <path/to/orchestrator.db> \
  --task-dir <path/to/task-files> \
  --worktrees-dir <parent/for/worktrees> \
  --poll-interval 30s \
  --mcp-listen 127.0.0.1:7777
```

Only `--config` and `--repo` are required; `--base` defaults to `main`, `--db` to
`./orchestrator.db`, `--poll-interval` to `30s`, and `--mcp-listen` is **off**
unless set. Add `--notify-webhook <url>` to POST escalations/alerts out-of-band
(the always-on channel for when no one is watching the loop).

**Verify it came up:** the daemon logs `daemon starting label=<…> workers=<N>`
and `mcp control server listening addr=127.0.0.1:7777`. If the address is
already in use or the config is invalid, it exits at startup rather than running
half-configured.

---

## 4. Attach the supervisor and run the loop

Register the daemon's MCP endpoint **once** so its tools are native to your
session, then start the supervision loop:

```bash
claude mcp add --transport http orchestrator http://127.0.0.1:7777/mcp
claude mcp list   # confirm: "orchestrator: … ✔ Connected"
```

```
/loop 5m operate the orchestrator
```

That `/loop` runs the `operate-orchestrator` skill each tick — its per-tick
procedure (observe → classify → act → escalate → log) is the authority on what to
do each pass; this runbook does not repeat it. Omit the interval to self-pace.

> If the MCP tools aren't available in a session where the daemon was registered
> *after* the session started, fall back to `curl` against `/mcp` — see §6.

---

## 5. Steady-state management

Most passes do nothing but observe — a quiet pipeline is a healthy one. The MCP
tools are your surface:

| Tool | Args | Use |
| --- | --- | --- |
| `list_tasks` | — | primary observe: every task + state, branch, PR, retries |
| `get_task` | `issue` | one task's current view |
| `get_audit` | `issue` | primary diagnosis: a task's full transition history |
| `enqueue_task` | `issue` | nudge a **non-settled** idle issue (refused if settled) |
| `cancel_task` | `issue` | stop an actively-running drive; settles it to `cancelled` |

**Two semantics that bound what you can do autonomously:**

- Control tools are **dispatch-acknowledged, not completion-acknowledged** — a
  success means the command reached the scheduler, not that the drive finished.
  Confirm the effect with a follow-up `get_task` / `get_audit`.
- `cancel_task` is **one-way**: a cancelled task is settled and **cannot be
  re-driven** through the tools, and it only acts on an *actively-running* drive
  (a suspended `blocked_on_gate` task returns "not currently running"). So a
  runaway is **stop-and-escalate**, not stop-and-restart. `enqueue_task` likewise
  **refuses any settled issue**. Re-opening settled work is a human decision.

**Cardinal rule: don't fight the daemon.** It already times work out to
`escalated`, re-drives every non-settled task each poll, runs the retry cap,
removes the label on settle, and **re-seeds all in-flight work on restart**. Only
do the meta-layer it structurally cannot: restart the dead process, diagnose and
explain escalations, and judge pathological patterns.

---

## 6. Handling escalations & a dead daemon

**An escalation** (`escalated`, or a `needs_human` triage verdict) is the
pipeline asking for a human. `get_audit {issue}` and read the last transition —
the trigger tells you why:

- `implementing → escalated` on `agent.done` (`fail`) → the agent finished but
  opened **no PR**.
- `implementing → escalated` on `timeout` → the agent ran past its deadline.
- `pr_open → escalated` → the reviewer returned `escalate`.
- `changes_requested → escalated` on `retry_exhausted` → the change cap was hit.
- `blocked_on_gate → escalated` on `timeout` → the merge gate never cleared
  (CI red, missing approval, or conflicts).

Surface a clearly-marked block (issue #, state, one-line cause, last transitions,
recommended human action) and stop touching that item — see the skill's
escalation format.

**A dead daemon** is the one recovery lever the daemon can't pull for itself.
If `list_tasks` errors or its pane shows the process exited, **restart it in its
pane** with the same `daemon …` command. On startup it re-seeds and resumes every
non-settled task on its own — do **not** also run `orchestratord recover` against
a live daemon (two engines on one DB/repo). `recover` is only the one-shot for
when no daemon is running:

```bash
orchestratord recover --config <config.yaml> --repo <abs/path/to/checkout> --db <db>
```

**`curl` fallback** if MCP tools aren't loaded in your session:

```bash
curl -s 127.0.0.1:7777/mcp -d \
  '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}'
```

---

## 7. Teardown

When you're done operating:

1. **Stop the loop** — end the `/loop`.
2. **Stop the daemon** — SIGINT its pane (Ctrl-C). Task state is durable in the
   `--db`, so nothing is lost; a later daemon start resumes in-flight work.
3. **Deregister the MCP server** — `claude mcp remove orchestrator`.
4. **Clean up herdr** — close the workspaces/panes the daemon opened, and prune
   per-task worktrees (`git worktree prune`) if you want the checkout tidy.
   Cancelled tasks intentionally leave their worktree for inspection.
5. **Revert temporary global settings** — undo the `~/.claude/settings.json`
   permission/trust entries you armed in §3.1.

---

## 8. Quick reference

**Daemon flags:** `--config` · `--repo` · `--base` (main) · `--db`
(orchestrator.db) · `--task-dir` · `--worktrees-dir` · `--poll-interval` (30s) ·
`--notify-webhook` · `--mcp-listen` (off).

**MCP posture:** loopback only, **no auth** — the bind address is the trust
boundary. Never bind a non-loopback address.

**Where to look:**

- **What to do each tick** → `.claude/skills/operate-orchestrator/SKILL.md`
- **Design, the config contract, the 7 safety invariants** → [README.md](README.md)
- **Human getting-started walkthrough** → [TUTORIAL.md](TUTORIAL.md)
- **What's built vs deferred, tracked debt** → [ROADMAP.md](ROADMAP.md)
- **The workflow you're running** → its `--config` YAML (states, timeouts,
  `sources` label, `dry_run`)
