## Build, test, format

```
go build ./...
go test ./...
go vet ./...
gofmt -l -w .
```

Every PR keeps all four green. Tests are table-driven and written alongside the
code. Fixtures are hand-written and synthetic; never paste real terminal output,
issue text, or conversation content into `testdata/`.

## Dependencies (keep it minimal)

Three direct dependencies: `modernc.org/sqlite` (pure Go, no cgo, keeps the
single-binary build), `gopkg.in/yaml.v3`, `github.com/santhosh-tekuri/jsonschema/v6`.
`herdr` and `gh` are shelled out through `internal/proc`, never linked. Do not add
a dependency without a named reason in the PR.

## Conventions

- Small interfaces at package boundaries (`exec.ExecutionBackend`,
  `github.Client`, `source.IssueSource`, `classify.PaneClassifier`). The engine
  depends on interfaces, never on herdr or `gh` concretely.
- `context.Context` is the first argument of anything that does I/O or can
  block; honor cancellation. Timeouts are config-driven, never constants.
- Wrap errors with context (`fmt.Errorf("...: %w", err)`). No panics in the
  daemon path.
- No global mutable state. The task store is the single writer of task state.
- Parse every herdr and GitHub id from command output; never hardcode formats.
- Optional behavior is switched at the flag boundary in `cmd/orchestratord`; a
  nil dependency means "off, today's behavior" (see `--pane-classifier`).
- A task is driven against the workflow snapshot it started under, not the live
  config.

## Scope discipline

- The whole graph is implemented: triage, implement, review, merge gate,
  scheduler, MCP control surface, `doctor`. Work the issue you were given;
  do not widen it.
- Merges are gate-evaluated (`ci_green`, `no_conflicts`), never reached from a
  decision or a bare event. Do not add a path into `merging` that skips a gate.
- The seven safety invariants are enforced by `internal/config` and listed in
  `README.md`. A config change that fails validation is a bug in the change,
  not in the validator.
- The authoritative artifact decides (a PR on GitHub, a merge, CI), never a
  pane's status text.

## Where things live

- Schema: `internal/config/workflow.schema.json`. Example config and prompts:
  `examples/`. Reference validator for the invariants: `validate_workflow.py`.
  The original herdr + `gh` command spike: `spike0.sh`.
- Operator docs: `README.md` (start here), `RUNBOOK.md` (operate),
  `TUTORIAL.md` (human-paced walkthrough), `ROADMAP.md`.
