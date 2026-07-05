# Design: `operate-orchestrator` — an autonomous supervisor skill

**Status:** approved 2026-07-05. Spike de-risked (see §6). Ready to implement.

## 1. Goal

A Claude Code **skill** that turns a session into the autonomous operator of a
running `orchestratord` daemon: keep the issue→PR pipeline flowing, and surface
to a human *only* when the pipeline itself escalates or the environment breaks.

## 2. Settled decisions (from brainstorming)

- **Autonomy:** an autonomous supervisor, run as a loop — not a reactive manual.
- **Authority:** full authority over *operational* actions (enqueue, cancel,
  recover, restart the daemon). It surfaces to a human only on (a) a pipeline
  escalation — a task at `escalated`, or a `needs_human` triage verdict — or
  (b) an environmental dead-end it cannot fix (herdr down, `gh` auth broken,
  invalid config, disk/db failure).
- **MCP access:** the daemon's loopback MCP control server, registered as a
  **Claude Code HTTP MCP server**, so `list_tasks`/`get_task`/`get_audit`/
  `cancel_task`/`enqueue_task` are native tool-calls. **Confirmed working** (§6).

## 3. Division of labour — do not fight the daemon

The daemon already self-heals a lot; the supervisor must not duplicate it.

| The daemon already does | So the supervisor instead does |
|---|---|
| Times work out → `escalated` (45m `implementing` agent / 30m `blocked_on_gate` gate wait) | Reads the audit trail to **explain** an escalation and recommend a human action |
| Re-drives every non-settled task each poll (`SeedFrom`); re-seeds on restart | Uses `enqueue_task` only to nudge a **non-settled** idle issue the loop hasn't picked up |
| Removes the source label on settle; runs the retry cap | Judges **patterns** the daemon can't — retry churn, a pathological loop, an externally-closed PR — and `cancel_task`s the genuine runaway |
| Alerts via `--notify-webhook` (best-effort) | **Cannot restart itself** → the supervisor restarts the daemon process (which re-seeds in-flight work itself on startup) |

Net: the supervisor is the meta-layer above the daemon's built-in resilience —
process lifecycle, diagnosis, escalation triage, and reactions to state outside
the daemon's model.

**Tool-semantics constraints (verified against source; they bound what the
supervisor can autonomously do):**
- `enqueue_task` **refuses any settled issue** (`scheduler.go`: "already settled;
  not re-driven"). It only drives a non-settled, not-in-flight issue.
- `cancel_task` settles a running drive to the terminal `cancelled` — **one-way**;
  a cancelled task cannot be re-driven through the tools. It also only acts on an
  *actively-running* drive (a suspended `blocked_on_gate` task returns "not
  currently running").
- Consequence: there is **no restart-after-cancel / restart-after-settle** via the
  operator surface. A runaway is a *stop-and-escalate*, not a stop-and-restart —
  re-opening settled work is a human decision. (A future "reopen a settled task"
  affordance is a possible orchestrator enhancement, out of scope here.)

## 4. Components

1. **`.claude/skills/operate-orchestrator/SKILL.md`** — the deliverable: operator
   knowledge + the per-tick supervision procedure + the decision table +
   escalation format + the action log. A **project skill** (versioned with the
   tool, ships to anyone who clones the repo).
2. **Setup the skill documents (not ships):** the daemon runs in a dedicated
   herdr pane with `--mcp-listen 127.0.0.1:PORT`; the operator registers it once
   with `claude mcp add --transport http orchestrator http://127.0.0.1:PORT/mcp`.
   Registration is per-deployment (port varies), so the skill documents the
   command rather than shipping a fixed `.mcp.json`.
3. **The cadence:** `/loop <interval> operate the orchestrator` (or self-paced)
   runs the SKILL's tick.

The three surfaces the supervisor uses:
- **MCP tools** (native) — observe (`list_tasks`/`get_task`/`get_audit`) and
  control (`cancel_task`/`enqueue_task`). Control tools are
  **dispatch-acknowledged, not completion-acknowledged** — confirm the effect
  with a follow-up `get_task`/`get_audit`.
- **CLI** (`orchestratord daemon|recover|validate|plan`) — lifecycle the MCP
  surface can't do: start/restart/recover the daemon, validate a config.
- **herdr** — manage the daemon's pane (read its log, restart the process).

## 5. The supervision tick

Each tick: **observe → classify → act → escalate.**

1. **Observe** — `list_tasks`; for any non-terminal task that looks off,
   `get_audit` to see its last transition + timing.
2. **Classify & act** (autonomous):
   - Daemon unreachable / pane dead → **restart** the daemon in its pane; its
     startup re-seed resumes in-flight tasks (don't run `recover` against a live
     daemon). Log it.
   - Runaway the daemon won't stop (pathological loop, external PR close, dead
     pane) → `cancel_task` to **stop** it (one-way), then **surface** — it can't
     be auto-restarted. Log it.
   - A non-settled idle issue the loop hasn't picked up → `enqueue_task`. Log it.
   - Task legitimately in progress or in a gate wait the daemon re-checks →
     **leave it.**
3. **Escalate** (surface to human) — a task at `escalated` or a `needs_human`
   verdict, or an environmental dead-end. Emit a `⚠️ ESCALATION` block: issue #,
   state, a one-line audit summary, the likely cause, and a recommended human
   action. (The daemon's own webhook is the complementary always-on channel.)
4. **Log** — every autonomous action appends to a running action log (what, why,
   result) so a human can audit what the supervisor did unattended.

## 6. De-risking spike (done 2026-07-05)

The one real risk was whether CC's HTTP MCP client accepts our **response-only**
server (we implement the request/response subset of Streamable HTTP; the GET/SSE
stream returns 405). Verified against a live daemon (`--mcp-listen 127.0.0.1:7799`):

- Full handshake over plain HTTP works: `initialize` → 200 (`serverInfo`),
  `notifications/initialized` → 202, `tools/list` → 5 tools, `tools/call` → MCP
  result.
- `claude mcp add --transport http orch-spike http://127.0.0.1:7799/mcp` then
  `claude mcp list` → **`✔ Connected`.**

**Conclusion: no SSE fallback needed.** Registration is a one-liner; the skill
documents it.

## 7. Scope & validation

- **In scope:** the skill, the documented setup, the loop pattern, the decision
  table, the escalation format, the action log. **No engine changes.** The
  supervisor keeps the pipeline *flowing*; it does not decide *what* work to do
  (the daemon's source/label owns intake).
- **Validate by dogfood:** point it at a live daemon on the self-repo backlog and
  confirm it (a) restarts+recovers a killed daemon, (b) surfaces a seeded
  escalation with a correct diagnosis, (c) leaves legitimately-working tasks
  alone. Capture the run in the skill's examples.
