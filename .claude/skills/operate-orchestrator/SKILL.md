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
at whatever `implementing`'s `timeout:` is in the run's `pipeline.yaml`, and a
merge gate that never clears at whatever `blocked_on_gate`'s `timeout:` is
(shipped defaults 45m and 30m) — re-drives every non-settled task on each poll,
runs the retry cap, and removes the source label when a task settles. On restart
it re-seeds and resumes every non-settled task on its own. **Do not babysit what
it already handles.** Your value is the meta-layer it structurally cannot do:

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

1. **MCP tools** (native, if the daemon is registered — see Find the run):
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
   - `message_task {issue, text}` — submit one line at a running task's agent
     prompt, delivered and verified the same way as its kickoff. Refused for an
     unknown issue, a settled task, or a task with no pane. See "Nudging an idle
     agent" below for when it is safe.

   Control tools are **dispatch-acknowledged, not completion-acknowledged**: a
   success means the command was accepted, not that the drive finished. Always
   confirm the effect with a follow-up `get_task` / `get_audit`.

2. **CLI** (`orchestratord`) — lifecycle the MCP surface can't do. Run from the
   repo checkout: `orchestratord recover|daemon|validate|plan`.

3. **herdr** — manage the daemon's own pane (read its log, restart the process).
   See the `herdr` skill. Requires a herdr server reachable from this session
   (`herdr status`).

## Find the run

`setup-orchestrator` starts the daemon and records where the run lives in
`~/orchestrator-runs/<name>/run.env`. Read it (ask for `<name>` only if more than one
`~/orchestrator-*/run.env` exists):

- `PORT` — the MCP endpoint, `http://127.0.0.1:<PORT>/mcp`.
- `PANE` — the daemon's herdr pane: its log, and where you restart it.
- `REPO_DIR` — the local checkout; `REPO` — the `<owner>/<name>` slug.

No `run.env` means the daemon is not set up: say so and point at
`setup-orchestrator`. Do not start a daemon from this skill.

Then read the run's `~/orchestrator-runs/<name>/pipeline.yaml` so you know its
**state timeouts** and **source label** — you need them to diagnose. The daemon
also needs a herdr server reachable from this session (`herdr status`), an
authenticated `gh`, and (for unattended agent runs) the permission setup
`setup-orchestrator` step 8 describes.

If the MCP tools aren't available in this session (setup registers them for the
*next* session), fall back to `curl`:
`curl -s 127.0.0.1:<PORT>/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}'`.

## The supervision tick

Run this each pass. Keep it cheap — most ticks do nothing but observe.

### 1. Observe
- `list_tasks`. For any **non-terminal** task that looks off (see Diagnosis),
  `get_audit {issue}` to read its last transition and *when* it happened.
- Confirm the daemon is alive: if `list_tasks` errors or its pane shows the
  process exited, the daemon is down.

> **Read liveness first.** Each task view carries `agent_status`
> (`working`/`idle`/`blocked`/`done`), `agent_status_for_seconds`,
> `state_for_seconds`, and the `state_timeout` / `blocked_timeout` it is racing.
> Comparing elapsed against bound answers "is anything wedged" directly — a task
> `blocked` for a duration approaching `blocked_timeout` is about to escalate, and
> is worth surfacing *before* it does. An absent age means "not observed", not
> "just now".
>
> **`list_tasks` alone cannot see a blocked agent.** Blocking does not change
> state, so a task parked on an unanswerable prompt looks byte-identical to one
> making progress — a state-change watcher stays silent through the whole thing.
> This has cost a real run 47 idle minutes. Also check, each tick:
> `get_audit {issue}` for an `agent.blocked` row, or grep the daemon pane for
> `agent blocked`. Configure `policies.blocked_timeout` so the engine bounds it
> without you, and `--notify-webhook` so it reaches you when nobody is watching.

### 2. Classify & act (autonomous — then log every action)

| Situation | Action |
|---|---|
| **Daemon down / unreachable** | Restart it in `PANE` with the same `daemon …` command `setup-orchestrator` step 9 ran. On startup it re-seeds and resumes every non-settled task itself — do **not** also run `orchestratord recover` against a live daemon (two engines on one DB/repo). Use `recover` only for a one-shot when no daemon is running. |
| **A runaway you must stop** — pathological retry churn, its PR closed/merged externally, an agent looping and burning resources | `cancel_task {issue}` to stop the running drive. This is **one-way** — it settles to `cancelled` and cannot be re-driven — so then **surface**: a human decides whether to re-open the issue or fix the root cause. |
| **A non-settled task the daemon isn't driving** (idle, e.g. freshly labeled and not yet picked up) | `enqueue_task {issue}` to nudge it. (Refused for any settled task.) |
| **A driven task whose agent sits idle at its prompt** with the state's work unfinished | Nudge the agent — see below. |
| **Task legitimately working, or in a gate wait the daemon re-checks** | Leave it. Do nothing. |

**Nudging an idle agent.** Use `message_task {issue, text}` — never type into the
agent's pane yourself. Only when the pane shows an **idle prompt, never a
dialog**: read it first (`herdr pane read <pane>`; find the pane by its
`issue-<N>` workspace label in `herdr workspace list`). A permission dialog is
fixed in the allow-list, not answered. `text` is one line; put anything longer in
a file and reference its path. Send one complete instruction the agent can finish
by doing the state's work; in `changes_requested`, a turn that pushes no commit
fails `head_moved` and escalates terminally. A success means the agent took the
message (its status moved); the audit records a `message_task` row with the
text's length. The engine then decides from the artifact as for any other agent
activity — there is nothing else to trigger.

Cancel is destructive **and one-way** — it kills in-flight agent work and the task
cannot be restarted through these tools (settled means settled; the engine is the
single writer). Use it only for a runaway you have diagnosed, never as a first
resort, and always **surface afterward** so a human can decide the follow-up.

### 3. Escalate (surface to a human)

Surface **only** for a pipeline escalation or an environmental dead-end:
- any task at `escalated`, or a task closed via a `needs_human` triage verdict;
- an environment you can't fix: herdr down, `gh` not authenticated, an invalid
  config, a disk/DB failure.

When the daemon runs with `--notify-webhook`, the escalation payload already
carries `Cause`, the last few transitions, the agent's pane tail, and a
recommended action — prefer relaying that over reconstructing it by hand.

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
    too-large task or a slow agent — but also a **permission wedge**: a tool not in
    `permissions.allow` pops an interactive prompt the agent can't answer, and the
    daemon keeps reading the pane as "working" until the timeout fires. Read the
    pane read-only (`herdr pane read <pane>`) to tell them apart; a prompt means fix
    the allow-list, then open a fresh issue to retry — never type into the pane, and
    a settled task can't be re-driven).
  - from `implementing` on `blocked_timeout` → the agent sat continuously blocked
    past `policies.blocked_timeout`. Distinct from `timeout` on purpose: this one
    means "parked on a prompt", not "legitimately slow". Read the pane read-only to
    see *which* prompt, fix the environment that caused it, and open a fresh issue.
  - from *any* state on `no_progress` → the task produced no observable signal for a
    whole `policies.no_progress_timeout` window, confirmed against the pane's own
    bytes. "Nothing happened at all", rather than "this took too long". Read the
    pane read-only to see where it died.
  - from *any* state on `blocked_on_prompt` → (daemon run with `--pane-classifier`)
    the static pane was classified as parked on an interactive prompt. Read the pane
    read-only to see which one, add the tool to the allow-list, then open a fresh
    issue — never send keystrokes into the pane.
  - from *any* state on `agent_crashed` → (daemon run with a Jev `--pane-classifier`) the
    static pane shows an error or a bare shell with no agent running. Read the pane
    read-only for the error and check herdr.
  - from *any* state on `drive_deadline` → the scheduler's reaper stopped a drive
    that outlived `policies.drive_deadline`. This is the backstop for a drive wedged
    where the engine has no timer armed at all — a spawn, a gate read, a decision, a
    merge — so suspect a hung `git`/`gh`/`herdr` first. The audit `result` names how
    the escalation target was chosen.
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

Read the run's `pipeline.yaml` for the actual timeout values and the source label; use
them, don't guess.

## Running the loop

Drive this skill on a cadence:

```
/loop 5m operate the orchestrator
```

Or omit the interval to self-pace. Between ticks there is usually nothing to do —
that is the healthy state. Do not invent work; a quiet pipeline is a working one.

## Worked example (from a real run)

```
# tick — task working, within timeout → leave it alone
list_tasks → [{issue:29, state:"implementing"}]

# a later tick — the daemon process has died
list_tasks → connection error                → daemon DOWN
  action: restart it in its pane; on startup it re-seeds issue-29
          (still implementing) and resumes. No `recover`.

# a later tick — the implementer overran its deadline
get_audit 29 → … implementing → escalated (trigger=timeout)
  ⚠️ ESCALATION — issue #29 (escalated)
  Cause: implementer ran past its deadline without opening a PR
  Audit: implementing→escalated (timeout); queued→implementing (scheduled)
  Recommended: inspect the agent's worktree / issue scope, or raise the
  state's timeout, then open a fresh issue to retry — a settled task
  cannot be re-driven (enqueue_task 29 → "already settled; not re-driven").
```

## Safety

- **Idempotency:** before acting, re-check state — never double-cancel or
  double-enqueue the same issue in one tick.
- **Single-writer respect:** the engine owns task-state transitions. Your control
  tools (`cancel`/`enqueue`/`message`) are operator signals, not state writes — a cancel
  settles the drive to `cancelled` through the engine, never behind its back.
  Don't edit the DB directly.
- **Merges stay gated:** never try to force a merge. The merge gate
  (CI + no-conflicts, plus approvals if configured) is the only path to `merged`; a blocked merge
  is a human decision, so surface it.
