# The workflow config

Reference moved out of the README; the short version is in [`README.md`](../README.md).

## The workflow config

A workflow is a versioned YAML document — the *policy* the fixed engine
interprets. It is validated in **two stages** before anything runs: a JSON Schema
for shape, then seven semantic **safety invariants**. `validate` reports both;
`run` and `recover` refuse to start on any error.

### Schema & reference files

| File | Role |
| --- | --- |
| `internal/config/workflow.schema.json` | JSON Schema (Draft 2020-12) for the config **shape**; embedded in the binary via `go:embed` and applied first. The authoritative shape contract. |
| `internal/config/validate.go` | The runtime validator: applies the schema, then the seven invariants, returning errors + warnings. |
| `validate_workflow.py` (repo root) | Reference spec for the invariants, kept behaviorally equivalent to `validate.go`. Runs standalone: `python3 validate_workflow.py <config> [--schema workflow.schema.json]`. |
| `examples/default-pipeline.yaml` | The shipped default pipeline (with `prompts/` beside it) — what `orchestratord init` scaffolds, and the starting point when authoring your own. |
| `internal/config/testdata/broken-pipeline.yaml` | A structurally-valid config that **violates** the invariants (merge reachable without a gate; an unbounded loop) — used to prove the validator bites. |
| `spike0.sh` (repo root) | The proven herdr + `gh` command sequence the herdr backend wraps. |

### Structure

Top-level keys (`version`, `name`, and `states` are required; unknown keys are
rejected):

| Key | Meaning |
| --- | --- |
| `version` | Schema version (integer ≥ 0). |
| `name` | Workflow name (non-empty). |
| `entry_state` | The state a new task starts in (used for reachability checks). |
| `policies` | Workflow-wide knobs (below). |
| `sources` | Where work originates — `github_issues`, polled by the `daemon` on the `select:` label. |
| `roles` | Agent profiles a state can `spawn`/`resume`. |
| `gates` | Deterministic predicates over **authoritative** sources (GitHub). |
| `decisions` | Constrained judgment hooks with a closed set of `verdicts`. |
| `states` | The nodes of the state graph (below). |

**`policies`** — `max_concurrent_tasks`, `dry_run`, `circuit_breaker`,
`retry_caps` (a per-state cap map, `state_name: N`), the liveness bounds
`no_progress_timeout` / `blocked_timeout` / `drive_deadline`, and `execution`
(`backend: herdr|local|container`, `run_as: root|non_root`, `sandbox: bool`).
The engine reads these: `retry_caps` bounds per-state retries and is validated,
`dry_run` gates the real merge, the three bounds below keep work from wedging,
and `max_concurrent_tasks` bounds the daemon's concurrency (R2).
`circuit_breaker` and the finer `execution` knobs (`sandbox`) are parsed but not
yet enforced.

#### Liveness bounds

A per-state `timeout:` transition is an **override** for the one thing that
genuinely varies per state ("this one legitimately takes 90 minutes"). Three
policies cover everything else, so nothing is unbounded merely because nobody
remembered to annotate it.

**`no_progress_timeout`** (a duration; absent ⇒ `30m`, `0s` disables) is the
global bound. A state timeout measures **position** — how long a task has been
in state X — which cannot distinguish a legitimately slow agent from a dead one,
and only protects the states someone annotated. This measures **progress**: how
long since the task last did anything observable. Any pane status event resets
it. When it expires the engine treats that as a suspicion, not a verdict, and
confirms against the agent pane's own bytes before escalating (trigger
`no_progress`) — the event hub broadcasts only status *diffs*, so an agent
working steadily for an hour emits no events at all. A pane that cannot be read
counts as progress: a herdr blip must never be what escalates a task. Disabling
it is allowed only if every agent state declares its own timeout; the validator
rejects the combination that would leave a state unbounded.

Static bytes say only that nothing moved, not why. With `--pane-classifier URL`
(off by default; needs `OPENROUTER_API_KEY`) the daemon asks TypeSafe's Jev
(`typesafe/jev-1.13`, via OpenRouter's `/systemone`) what the unmoving tail
means, and acts only on an answer at p ≥ 0.9: a permission or question prompt
escalates at once as `blocked_on_prompt`; a crash as `agent_crashed`; `working`
(a long quiet build or test run) resets the window instead of escalating;
`finished` runs the same artifact check an idle agent gets, and advances only on
the verdict file or a passing gate. An unsure answer or a failed call escalates
`no_progress` exactly as without it. The classifier runs only here — never on a
moving pane, never on a gate or decision — and each call costs about $0.00004.

Without a key, `--pane-classifier heuristic` recognises the one wedge that has a
fixed shape: a Claude Code tool-permission or trust prompt, matched by its
rendered box and escalated at once as `blocked_on_prompt`. It answers nothing
else — a pattern cannot tell a silent test run from a dead pane — so every other
static pane escalates `no_progress` exactly as with the flag off. It makes no
network call and does not chain to Jev.

**`drive_deadline`** (a duration; absent ⇒ twice the longest state timeout,
floored at `1h`) is the hard ceiling on a single drive, enforced by a reaper in
the scheduler — **outside** the drive it watches. The engine's own timer is armed
only once a drive reaches the agent-wait loop in a state that declares a timeout;
a drive wedged in a spawn, a gate read, a decision, or a merge has no timer at
all, and a watchdog sharing a goroutine with a wedged drive is no watchdog. A
reaped drive settles (trigger `drive_deadline`) rather than aborting, so it is
not re-driven and re-reaped forever. The bound is per *drive*, not per task: a
task that suspends in a merge-gate wait and resumes next poll starts a fresh
clock. An explicit value at or below the longest state timeout is a validation
error — it would preempt the transitions it exists to back up.

**`blocked_timeout`** (a duration, e.g. `10m`; absent ⇒ only the bounds above
apply) caps how long an agent may sit *continuously* blocked before the engine
gives up on it, with trigger `blocked_timeout` in the audit. It exists
because a blocked agent is parked on an interactive prompt nobody will answer —
it will never report done — yet **blocking does not change state**, and a state
timeout is anchored to state *entry*. Without this the only lever is shortening
the whole state timeout, which would also kill legitimately long runs. Any
`working` event clears the clock, so a prompt the agent resolves itself never
counts; `idle` deliberately does **not** clear it, because a pane parked at an
unanswerable prompt can report idle. The state timeout remains the hard backstop.

All three bounds escalate to the same place: the state's own `timeout:` target if
it declares one, else the workflow's alerting terminal, else `cancelled`. That
fallback is what makes them apply by default — a bound whose only escalation path
was the state's timeout edge was silently **inert** in every state that declared
none. The derivation is recorded in the audit `result` column, so a fallback is
never silent.

**`roles`** — each has `launch` (argv, required, e.g. `["claude"]`),
`task_delivery` (`context_file` | `inline`), `workspace` (`per_task` | `shared`),
and an optional `kickoff` string.

**`gates`** — `type` is one of `github_pr`, `github_checks`, `github_reviews`,
`github_mergeable`, `github_commits` (the only authoritative sources accepted).
Type-specific fields (`head`, `all_passing`, `min_approved`, `require`, `since`)
are allowed alongside.

**`github_commits`** (`since: state_entry`, required) passes only if the PR head
commit differs from the one recorded when the task entered its current state. It
exists because a gate must ask the question its state actually asks. In
`implementing`, `pr_exists` means "did you produce the artifact?" — real
evidence. Reused in `changes_requested` it verifies nothing: the PR was opened in
the round that put the task there, so it could only ever pass, whether or not the
implementer addressed a single line of the review. A resumed agent that did
nothing advanced anyway, the reviewer re-reviewed unchanged code, and the loop
burned retries. `github_commits` asks whether anything was actually committed.

The baseline is captured when the task enters the state, and only for states that
evaluate such a gate — every other transition, including the detached writes that
settle a cancel or a reaped drive, skips the read. If the baseline is unknown (no
PR yet, a failed read, or a task predating the column) the gate passes and logs:
degrading to the previous behavior beats escalating a task on a question whose
input was never captured.

**`decisions`** — `impl.type` is `llm` (with a `rubric` path) or `exec` (with a
`command` argv); `verdicts` is the closed, unique set of outcomes it may return.

**`states`** — each state may declare:

- `entry` — an action on arrival: `spawn` / `resume` a role (optionally `with` a
  named input), or `action: merge_pr` (the only side-effecting entry action).
- `transitions` — outgoing edges (below).
- `terminal` — `success` | `rejected` | `needs_human` (a leaf; takes no transitions).
- `wait_for` — an event the state parks on (e.g. `status.changed`).
- `alert` — surface the state to a human.

A **transition** carries a `when` **trigger** (exactly one of `event`, `timeout`
— matching `^[0-9]+(s|m|h)$`, `decision`, or `gate`), an optional secondary
`evaluate` (`decision` or `gate`, run after an event), and exactly one outcome:

- `to: <state>` — unconditional move;
- `branch: { <key>: <state>, … }` — keys are the decision's **verdicts**, or
  exactly `{pass, fail}` for a gate;
- `action: { alert: <name> }` — a side action that does not change state.

A `gate` reference is a single name or a list (every gate must pass).

### The seven safety invariants

1. **Refs resolve** — every `spawn`/`resume` role, `decision`/`gate`, and
   `to`/`branch` target names a declared entity.
2. **Decisions are total** — a transition's branch keys exactly equal the
   referenced decision's declared verdicts.
3. **Gate branches are `{pass, fail}`**.
4. **Gates read authoritative sources only** — `github_pr`, `github_checks`,
   `github_reviews`, `github_mergeable`, `github_commits`.
5. **Merge is gate-only** — entering a side-effecting (`merge_pr`) state must be
   gate-evaluated, never decided by a model or raw event.
6. **Loops terminate** — every cycle has a retry cap or a timeout.
7. **Every non-terminal state has an exit**.

### Authoring & validating

Copy `default-pipeline.yaml`, edit it, and check it — no external services
needed, so it is safe in CI or a pre-commit hook:

```sh
orchestratord validate path/to/your-workflow.yaml
#   OK: "your-workflow" valid (N warning(s))    -> exit 0
#   FAIL: K error(s), N warning(s)              -> exit 1   (warnings alone pass)
```

> The trigger key is **`when`**, never `on` — a bare `on:` is coerced to the YAML
> boolean `true` and would silently drop the trigger. The schema rejects it.


## Conventions / guardrails

- Branch names are deterministic: `agent/issue-<n>` (the durable reconcile key).
- herdr pane ids are **volatile** — parsed from output, re-resolved on restart,
  never persisted as a durable key.
- Agents are never launched with `--dangerously-skip-permissions`; honor
  `run_as: non_root` + `sandbox`.
- Task handoff is a **context file + single-line kickoff**, never an inline
  multi-line prompt typed through the pane.

### Issue sources and code hosting

Work intake uses `source.Source`, independently of GitHub pull-request operations.
The source owns discovery, loading task text, acknowledging settled work, and
marking successful work complete. `github.IssueSource` implements this boundary;
the engine receives it separately from `github.PullRequests`.

Acknowledgement removes work from discovery without marking it done: cancellation
and escalation acknowledge, while only successful implementation completes the
source item. Source write failures are logged without undoing a merge. The
existing poll-time acknowledgement retry remains; completion updates have no
new durable retry mechanism.

Existing GitHub workflows retain their numeric task IDs, CLI/MCP arguments,
branches, database schema, and notification fields. The shared scheduler accepts
string keys, but the shipped daemon still uses numeric keys for MCP compatibility.
The source interface preserves opaque string keys so a future adapter can use
external IDs; persistent identity and CLI/configuration support for a second
source will be designed alongside that adapter. No Notion connection is included.
