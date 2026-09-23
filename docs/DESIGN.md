# Design

Reference moved out of the README; the short version is in [`README.md`](../README.md).

## Design in one paragraph

A fixed engine (mechanism) interprets a per-team workflow (policy) supplied as
YAML. A **task** is a token moving through a directed state graph; the engine —
never a model — owns every transition. Judgment enters only at constrained
`decision` points; irreversible side effects (merge) are reachable only through
`gate` evaluations over **authoritative** sources. **GitHub is the source of
truth for artifacts**; an agent's `done` status is only a trigger to go check
GitHub. The engine is the **single writer** of durable task state (SQLite).


## Architecture

```
cmd/orchestratord/      CLI: validate | plan | run | recover | daemon | version
internal/config/        workflow types, JSON-Schema validation, the 7 safety invariants
internal/engine/        the state-graph executor
internal/scheduler/     the daemon loop: one poller, N workers, single-writer store
internal/store/         SQLite task store + per-transition audit log (single writer)
internal/exec/          ExecutionBackend interface + herdr implementation
internal/github/        Client interface + gh CLI implementation (PR detection)
internal/mcp/           loopback MCP control server (read state + cancel/enqueue)
internal/notify/        out-of-band escalation/alert notifier (webhook)
internal/proc/          mockable os/exec runner (the seam under herdr + gh)
```

The engine depends only on small interfaces (`exec.ExecutionBackend`,
`github.Client`, `*store.Store`), never on herdr or `gh` concretely — so the
backend can later be swapped for a headless/container implementation.


## The review → merge loop

Past `pr_open` the engine runs the rest of the pipeline:

- **Review (a `decision`).** Entering `pr_open` spawns the `reviewer` role with a
  task file built from the decision's **rubric** (e.g. `prompts/review.md`,
  resolved relative to the config file) plus a pointer to the PR. The reviewer
  writes a **verdict file** — `{"verdict": "...", "feedback": "..."}` — and on
  `agent.done` the engine reads it, validates the verdict against the decision's
  declared `verdicts`, and branches. The engine reads a verdict; it never judges.
- **Changes requested.** `changes_requested` resumes the implementer carrying the
  reviewer's `feedback`, and loops back to `pr_open` only once the PR head has
  actually moved (`pr_exists` + `github_commits`) — an agent that reports done
  having committed nothing escalates rather than sending the reviewer back to
  unchanged code. It gives up to `escalated` once
  `policies.retry_caps.changes_requested` is exceeded.
- **Merge gate.** `approved` evaluates the merge gate
  (`github_checks` + `github_reviews` + `github_mergeable`) over one authoritative
  `PRStatus` read. If not yet green it parks in `blocked_on_gate`, which evaluates
  the gate once and, while it neither passes nor has timed out, **suspends** —
  the drive returns and frees its worker slot instead of pinning it for the whole
  wait, and the scheduler re-drives the task each poll to re-check the gate
  (`status.changed` has no push source). The state timeout is measured from the
  audit-recorded entry time, so it survives suspend/resume and daemon restarts;
  past it, the task escalates.
- **Merge.** `merging` runs the `merge_pr` action — **gated on `policies.dry_run`
  (default-on)**. A dry run logs the intended merge and halts at `merging`; with
  `dry_run: false` it `gh pr merge --squash`, verifies the PR is `MERGED`
  (authoritative), and reaches `merged`. Merge is reachable only through a gate
  (a safety invariant) and the side effect itself is gated again by `dry_run`.
  Once the merge is confirmed the engine settles the task's bookkeeping: it
  closes the source issue (the default kickoff's `gh pr create --fill` writes no
  `Closes #N` trailer, so nothing else would) and deletes the remote branch.
  Both are best-effort — the merge is already irreversible, so a bookkeeping
  failure is logged rather than re-driven as a failed merge.
