---
name: operate-orchestrator
description: >-
  Autonomously supervise a running herdr-orchestrator daemon — keep the issue→PR
  pipeline flowing (restart the daemon if it dies so it re-seeds in-flight work,
  stop a runaway task, nudge an idle one) and surface to a human ONLY when the
  pipeline escalates or the environment breaks. Use when asked to operate,
  supervise, watch, babysit, or run the orchestrator, and as the body of a
  `/loop` that tends it.
---

# Operate the orchestrator (autonomous supervisor)

You are the **autonomous operator** of a running `orchestratord` daemon — the
control plane that turns labeled GitHub issues into merged PRs by driving herdr
agents through a state graph. Your job is to keep that pipeline **flowing** and
to get out of the way. You have full authority over operational actions; you
call a human only when the pipeline says a human is needed or the environment is
broken.

New to the orchestrator? Skim `TUTORIAL.md` at the repo root once — this skill
assumes you know what the daemon, states, gates, and the merge loop are.

## Cardinal rule: don't fight the daemon

The daemon already self-heals. It times work out to `escalated` — a stuck agent
at 45m in `implementing`, and a merge gate that never clears at 30m in
`blocked_on_gate` — re-drives every non-settled task on each poll, runs the retry
cap, and removes the source label when a task settles. On restart it re-seeds and
resumes every non-settled task on its own. **Do not babysit what it already
handles.** Your value is the meta-layer it structurally cannot do:

- It **cannot restart itself** if the process dies — you can (and its own restart
  then resumes all in-flight work).
- It **terminates** an escalation; it doesn't **explain** it — you read the audit
  trail, diagnose the cause, and surface a recommended action.
- It runs its cap blindly; it can't judge a **pathological pattern** (retry
  churn, an externally-closed PR, a genuinely dead pane) — you can, and **stop**
  it (cancel is one-way — see below).

If a task is legitimately in progress or in a gate wait the daemon re-checks,
**leave it alone.**

## Your three surfaces

1. **MCP tools** (native, if the daemon is registered — see Setup):
   - `list_tasks` — all tasks + current states. Your primary observe call.
   - `get_task {issue}` — one task by issue number.
   - `get_audit {issue}` — a task's full transition history. Your primary
     diagnosis call.
   - `cancel_task {issue}` — stop an **actively-running** drive; it settles to the
     terminal `cancelled`. **One-way:** a cancelled task is settled and **cannot
     be re-driven** through these tools. Only works while the drive is in flight —
     a task suspended in `blocked_on_gate` between polls returns "not currently
     running."
   - `enqueue_task {issue}` — drive a **non-settled** issue that isn't already in
     flight (a freshly-labeled issue, or a nudge for an idle one). It **refuses
     any settled task** ("already settled; not re-driven"), so it is NOT a
     restart-after-cancel or restart-after-escalate mechanism.

   Control tools are **dispatch-acknowledged, not completion-acknowledged**: a
   success means the command was accepted, not that the drive finished. Always
   confirm the effect with a follow-up `get_task` / `get_audit`.

2. **CLI** (`orchestratord`) — lifecycle the MCP surface can't do. Run from the
   repo checkout: `orchestratord recover|daemon|validate|plan`.

3. **herdr** — manage the daemon's own pane (read its log, restart the process).
   See the `herdr` skill. Requires `HERDR_ENV=1`.

## Setup (once, before supervising)

The daemon must be running with its MCP control server on, in a pane you can see:

```bash
# In a dedicated herdr pane, from the repo checkout:
orchestratord daemon --config <config.yaml> --repo <repo-dir> \
  --db <db-path> --task-dir <task-dir> --worktrees-dir <wt-dir> \
  --mcp-listen 127.0.0.1:7777
```

Register that endpoint as an MCP server **once** so its tools are native:

```bash
claude mcp add --transport http orchestrator http://127.0.0.1:7777/mcp
claude mcp list   # confirm: "orchestrator: ... ✔ Connected"
```

Then read the daemon's `--config` so you know its **state timeouts** and
**source label** — you need them to diagnose. Prerequisites the daemon needs:
`HERDR_ENV=1`, an authenticated `gh`, a local repo checkout, and (for unattended
agent runs) the pre-armed permission setup described in `TUTORIAL.md`.

If MCP tools aren't available in this session (e.g. the daemon was registered
after the session started), fall back to `curl`:
`curl -s 127.0.0.1:7777/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}'`.

## The supervision tick

Run this each pass. Keep it cheap — most ticks do nothing but observe.

### 1. Observe
- `list_tasks`. For any **non-terminal** task that looks off (see Diagnosis),
  `get_audit {issue}` to read its last transition and *when* it happened.
- Confirm the daemon is alive: if `list_tasks` errors or its pane shows the
  process exited, the daemon is down.

### 2. Classify & act (autonomous — then log every action)

| Situation | Action |
|---|---|
| **Daemon down / unreachable** | Restart it in its pane (same `daemon …` command). On startup it re-seeds and resumes every non-settled task itself — do **not** also run `orchestratord recover` against a live daemon (two engines on one DB/repo). Use `recover` only for a one-shot when no daemon is running. |
| **A runaway you must stop** — pathological retry churn, its PR closed/merged externally, an agent looping and burning resources | `cancel_task {issue}` to stop the running drive. This is **one-way** — it settles to `cancelled` and cannot be re-driven — so then **surface**: a human decides whether to re-open the issue or fix the root cause. |
| **A non-settled task the daemon isn't driving** (idle, e.g. freshly labeled and not yet picked up) | `enqueue_task {issue}` to nudge it. (Refused for any settled task.) |
| **Task legitimately working, or in a gate wait the daemon re-checks** | Leave it. Do nothing. |

Cancel is destructive **and one-way** — it kills in-flight agent work and the task
cannot be restarted through these tools (settled means settled; the engine is the
single writer). Use it only for a runaway you have diagnosed, never as a first
resort, and always **surface afterward** so a human can decide the follow-up.

### 3. Escalate (surface to a human)

Surface **only** for a pipeline escalation or an environmental dead-end:
- any task at `escalated`, or a task closed via a `needs_human` triage verdict;
- an environment you can't fix: herdr down, `gh` not authenticated, an invalid
  config, a disk/DB failure.

Emit a clearly-marked block and stop touching that item:

```
⚠️ ESCALATION — issue #<N> (<state>)
Cause: <one line — e.g. "implementer reached done but opened no PR" / "reviewer verdict: escalate" / "gh auth expired">
Audit: <last 2-3 transitions, most recent first>
Recommended: <the specific human action>
```

The daemon's own `--notify-webhook` is the complementary always-on alert channel
when no one is watching the loop.

### 4. Log
Append every autonomous action to a running action log (a markdown file you name
at loop start, e.g. `orchestrator-supervisor.log` beside the DB). One line each:
`<timestamp> <issue> <action> — <why> → <result>`. An autonomous agent that can
cancel work owes the human an auditable record of what it did.

## Diagnosis reference

Terminal states: `merged` (success), `closed` (triage reject), `escalated`
(needs a human), `cancelled` (operator-cancelled). Everything else is in-flight.

To find *why* a task is where it is, `get_audit {issue}` and read the last
transition's `from → to (trigger/result)`:

- **`… → escalated`** — the reason is in the trigger:
  - from `implementing` on `agent.done` (`fail`) → the agent finished but opened
    **no PR**. Human should check the agent's work / the issue's clarity.
  - from `implementing` on `timeout` → the agent ran past its deadline (often a
    too-large task, or a slow/blocked agent).
  - from `pr_open` on a `review` verdict of `escalate` → the reviewer punted.
  - from `changes_requested` on `retry_exhausted` → the change cap was hit.
  - from `blocked_on_gate` on `timeout` → the merge gate never cleared (CI red,
    no approval, or conflicts) within the window.
- **stuck in `blocked_on_gate`** — the merge gate is failing: read the PR on
  GitHub. CI still running → leave it (the daemon re-checks). Needs an approval
  or a conflict fix → that's a **human** action → surface.
- **bouncing `changes_requested → pr_open` repeatedly** — retry churn; the
  reviewer and implementer disagree. If it's burning retries with no progress,
  `cancel_task` and surface with the pattern.

Read the daemon's config for the actual timeout values and the source label; use
them, don't guess.

## Running the loop

Drive this skill on a cadence:

```
/loop 5m operate the orchestrator
```

Or omit the interval to self-pace. Between ticks there is usually nothing to do —
that is the healthy state. Do not invent work; a quiet pipeline is a working one.

## Safety

- **Idempotency:** before acting, re-check state — never double-cancel or
  double-enqueue the same issue in one tick.
- **Single-writer respect:** the engine owns task-state transitions. Your control
  tools (`cancel`/`enqueue`) are operator signals, not state writes — a cancel
  settles the drive to `cancelled` through the engine, never behind its back.
  Don't edit the DB directly.
- **Merges stay gated:** never try to force a merge. The merge gate
  (CI + approvals + no-conflicts) is the only path to `merged`; a blocked merge
  is a human decision, so surface it.
