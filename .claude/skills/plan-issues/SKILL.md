---
name: plan-issues
description: >-
  Turn what the user wants to build into GitHub issues the orchestrator can
  run unattended: a short plan agreed with the user, one issue per
  agent-sized piece with decisions made and non-vacuous acceptance criteria,
  dependency edges recorded with GitHub's blocked-by, and a deliberate stop
  before labeling. Use when asked to plan, break down, or scope work for the
  orchestrator, and as path (a) of /start.
---

# Plan work into issues

**Outcome:** a set of issues in `<owner>/<name>` that an agent can each
complete in one session without asking anyone anything, with `blocked-by`
edges so the daemon runs them in a working order, and the user's explicit go
before any of them is labeled.

The daemon's implementer reads only the issue. Whatever you leave ambiguous,
it decides alone, or asks a question nobody answers. So the work here is
making decisions, not listing options.

## Rules

- **Hand the agent a decision, not a choice.** "Use X because Y" beats "X or
  Z". If you cannot decide, ask the user now, not the agent later.
- **One issue, one session.** Roughly: one package or component, a diff a
  reviewer reads in ten minutes, under an hour of agent time. Split anything
  bigger; the edges keep the order.
- **At least one acceptance criterion must fail today.** A test the current
  code passes proves nothing. Say which criterion is the one that fails on
  `main` before the change.
- **Never quote the user's private material** (documents, messages, data)
  into an issue; describe it.
- **You do not label.** Labeling starts work; the user says when.

## 1. Understand

Read enough of the repo to speak its language: `README`, the top-level
layout, the test setup, anything named in the request. For an empty repo, say
so and plan the scaffold as the first issue.

Ask the user at most three questions, only ones whose answer changes the plan
(what done looks like, a hard constraint, a choice between two real
directions). Then write the plan in ten lines or fewer: goal, non-goals, the
pieces in order, and what you decided on their behalf. Get a yes before
continuing; iterate in place, don't restart.

## 2. Decompose

One issue per piece, each with this shape (headings verbatim):

```
## Problem
What is wrong or missing, and why it matters — two to five sentences, with the
concrete evidence (a command and its output, a file:line, a number).

## Fix
The decided change. Name files, functions, and the mechanism. State what is
deliberately out of scope so the agent does not widen it.

## Acceptance criteria
1. <the criterion that fails on current main, marked: **This test must fail on
   current `main`.**>
2. <behavior that must not change, pinned by a test>
3. ...
N. `go build ./... && go test ./... && go vet ./...` green (or the repo's own
   check command); formatting clean.
```

Write each body to a file and create it with `gh issue create -R <owner>/<name>
--title "<title>" --body-file <file> --label <type>`, where `<type>` is
`enhancement` or `bug` (create the label if the repo lacks it). Titles state
the change and the reason in one line, not the component name.

## 3. Sequence

Draw an edge from every issue to each issue it needs merged first. Two
reasons an edge exists:

- **It builds on the other's code** (a package, a type, a flag the other one
  adds).
- **They touch the same files.** Two agents editing one package concurrently
  produce a conflict the daemon cannot resolve; the second one to finish stalls
  and a human rebases. Serialize them.

Record each edge on GitHub, never in the body text:

```bash
gh issue edit <n> -R <owner>/<name> --add-blocked-by <m>
```

The daemon reads these: a labeled issue waits until everything it is blocked
by is closed, and its log says what it is waiting on.

## 4. Show the user

List the issues in the order they will run, each as `#n title — blocked by
#m, #k` (or `ready`). Say how many can start immediately and what
`max_concurrent_tasks` in the run config is (default 4). Then ask:

> Label all of these `agent-ready` now? The ones with no blockers start on the
> daemon's next poll (within 30 seconds); the rest follow as their blockers
> merge.

Take edits: reorder, merge, split, rewrite. Do not label until the answer is yes.

## 5. Label and hand off

On yes, label every issue in the set in one pass:

```bash
gh issue edit <n> -R <owner>/<name> --add-label agent-ready
```

Then follow the `operate-orchestrator` skill. If the daemon is not running,
that is a setup problem: go back to `setup-orchestrator` step 9.
