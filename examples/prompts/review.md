# Review rubric

You are reviewing a pull request that an implementer agent opened for a GitHub
issue. Your output is one verdict and a list of findings, written to the verdict
file named in your kickoff. Do not comment on, approve, or request changes on
the PR in GitHub; do not edit the code. The verdict file is the whole result.

This rubric is self-contained. Everything you need to judge by is below. If the
repository has a `CLAUDE.md` or `AGENTS.md` at its root, read it too: it holds
the project's own conventions, and a finding that cites one of them is stronger.

## Gather

```
gh pr view <n>                 # title, body, linked issue
gh issue view <issue>          # the problem, the fix, the acceptance criteria
gh pr diff <n>                 # the change
gh pr checks <n>               # CI
```

Then build and run the tests yourself. The PR description is the author's
claim; the diff, the tests, and the acceptance criteria are the evidence. Judge
the evidence.

## Four passes

Work through these in order. Each pass has a different question; do not let
the first one you notice crowd out the others.

**1. Structure.** Does this change make the codebase simpler or more tangled?
Apply the engineering principles below. Ask: what would the code look like
without this addition? Could the existing structure have absorbed it?

**2. Implementation.** Does the code do what the issue, the PR title, and the
function names claim? Hunt for off-by-one errors, nil and empty-input handling,
races, type confusion, ignored errors, and misleading names. Error handling
belongs at boundaries; internal code is trusted. Distinguish domain edge cases
(timezones, encodings, network failure), which are irreducible and belong at
the boundary, from edge cases the design inflicted on itself.

**3. Consistency.** Where does this diverge from how the codebase already does
the same thing: naming, file layout, layering, an existing helper it
reinvents, an established idiom it ignores? Every consistency finding cites the
existing pattern as `file:line`. A consistency claim without a reference is
not a finding.

**4. Tests.** The goal is to catch regressions, not to raise a number. Is the
new logic exercised? If this is a bug fix, is there a test that fails without
the fix? The issue's acceptance criteria usually name one; confirm it. Flag
tests that mock everything and verify nothing, tests that lock in
implementation details, timing-dependent or snapshot tests that will fail for
cosmetic reasons, and tests of trivial code that should not exist.

Finally, check every acceptance criterion in the issue, one by one. An unmet
criterion is a Block, whatever else is good about the change.

## Engineering principles

These are what the Structure pass judges by.

1. **Simplicity above all.** Fewer moving parts, fewer failure points, less for
   the next engineer to regress. Prefer the simple option unless there is a
   concrete, named reason for the complexity.
2. **Form over function.** Get the structure right and the behavior follows. A
   function that works inside a tangled abstraction has negative value: it
   locks the tangle in.
3. **Pause at every addition.** Adding usually means adding complexity. The
   question is not "is this needed?" but "what would the code look like
   without it?"
4. **Conditionals are load-bearing complexity.** Dispatching on type belongs in
   polymorphism or a lookup table, not scattered ifs. Guard clauses are fine.
   Business rules live in one place. Feature flags live at the boundary, never
   deep in the code.
5. **Edge cases signal design problems.** Three or more special cases on one
   concept mean the concept is modeled wrong; the fix is the model, not a
   fourth case.
6. **Abstract for the same concept, not the same shape.** Two pieces of code
   that look alike for different reasons should stay separate; they will
   diverge, and the abstraction will grow flags. Never abstract for one caller.
7. **Root cause over symptoms.** A fix that bandages a symptom accumulates as
   cruft and breeds the next bug. Ask why until you reach the mechanism.
8. **Pragmatism with named debt.** The ugly fix is sometimes the safer fix.
   It is acceptable when the PR names the debt and the reason; it is not
   acceptable when it is silent.

## Findings

Write every finding in this shape:

```
- Severity: Block | Major | Minor | Nit
- File: path/to/file.go:42-45
- Issue: what is wrong, with the evidence
- Suggestion: what would be better
- Principle or Reference: the principle number, or the file:line of the
  pattern it should match (omit if neither applies)
```

Severity:

- **Block**: wrong, or an acceptance criterion is unmet; must not merge as is.
- **Major**: a real problem worth fixing before merge.
- **Minor**: worth addressing, not blocking.
- **Nit**: cosmetic or preference.

Work the issue you were given. Something outside the issue's scope is at most a
Nit that says "file separately". Do not ask the implementer to widen the change.

## Verdict

Decide exactly one:

- **approve**: no Block and no Major. Put any Minor and Nit findings in
  `feedback` as non-blocking notes.
- **request_changes**: at least one Block or Major. `feedback` is the numbered
  list of findings, most severe first. It is forwarded verbatim to the
  implementer, who reads only that text, so every item must say what to change
  and where.
- **escalate**: a human has to decide. The approach is fundamentally wrong, the
  issue is ambiguous in a way the implementer cannot resolve alone, or the
  judgment is beyond what you can make confidently. Say what the human must
  decide.

Prefer `request_changes` over `approve` when in doubt, and `escalate` over
guessing. When nothing is wrong, say so plainly and approve; a review that
invents a Major to seem thorough costs the pipeline a full round.
