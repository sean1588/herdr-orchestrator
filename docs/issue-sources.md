# Issue-source boundary

Work intake and code hosting are separate dependencies. A work item can originate
outside GitHub while the resulting code still goes through GitHub pull requests,
reviews, CI, and the existing merge gate.

`internal/source.Source` owns four operations:

- `List(ctx, selector)` discovers external item keys.
- `Get(ctx, key)` loads title, body, and optional URL for agent context.
- `Acknowledge(ctx, key, selector)` removes settled work from discovery.
- `Complete(ctx, key, comment)` marks successfully implemented work done.

Keys are opaque strings. A Notion adapter can use page IDs directly; it must not
convert them to issue numbers. The adapter is bound to an external collection
independently of the local code checkout. Selectors are provider-specific data,
not credentials. Each operation must honor context cancellation and mutations
must tolerate retries.

Acknowledgement is deliberately separate from completion. An escalated or
cancelled task leaves the queue but does not count as successfully implemented.
A dry-run merge does not complete the external item. Source mutation failures
are logged without undoing the engine's durable transition or repeating a merge.
The existing poll-time acknowledgement retry is retained; there is no new durable
outbox guaranteeing completion updates after a source outage.

## Existing GitHub behavior

`github.IssueSource` adapts the existing `gh issue` methods to this contract.
`github.PullRequests` exposes only PR operations. The daemon explicitly wires the
two dependencies; its discovery and acknowledgement call the source adapter.
`engine.Config.GitHub` remains a compatibility shorthand for older callers.

Existing workflow YAML, numeric CLI/MCP arguments, `issue-N` task IDs, branches,
and stored tasks are unchanged. SQLite gains two additive, immutable identity
columns (`source_id`, `source_key`), defaulting to empty on existing rows.

## Adding a source such as Notion

Implement `source.Source` and inject it as `engine.Config.Source`, together with:

- `SourceID`: a stable identity for the collection, including the external
  database identity. Do not reuse it for a different database.
- `PullRequests`: the GitHub code-host client.
- `SourceSelector`: provider-specific discovery settings for programmatic use.

Call `Engine.RunSource(ctx, externalKey)`. It persists the original source/key
pair and derives a safe internal task/branch ID by hashing the pair. Keys cannot
inject path separators or terminal newlines. Different collections do not share
tasks, even when an item key is the same. Recovery rejects tasks belonging to a
different configured source. `Run(ctx, issueNumber)` remains the legacy GitHub
entry point and rejects engines configured with a custom SourceID.

Use `scheduler.SchedulerOf[string]` to poll and drive string keys. Its worker
limits, duplicate suppression, cancellation, and deadlines are the same code
used by the existing numeric `scheduler.Scheduler` alias. A scheduler serves
one collection. On restart, seed it with the stored SourceKeys for that
collection's non-settled tasks. Resolve task IDs with `engine.SourceTaskID` when
checking whether an item is already settled. Keep one driver per task.

If a task's workflow snapshot contains a source whose ID matches SourceID, its
selector takes precedence over SourceSelector during settlement. Otherwise the
programmatic SourceSelector is used; it is not independently snapshotted.
Adapters must retain collection identity across restarts. Never put credentials
in workflow snapshots or persisted source identity.

This refactor does not add a Notion API client, credentials, database schema,
or status mappings. The shipped CLI, doctor, YAML source-type validation, and
MCP enqueue/cancel arguments still select GitHub issues. A Notion integration
will wire the new string-key scheduler and engine entry point into those
surfaces, alongside database selection and property mapping. MCP task listings
and escalation notifications already include the external source identity.

For Notion, the remaining product choices are which database to read, which
property contains the title, how page content becomes agent task text, which
status means ready, and how acknowledgement differs from successful completion.
Those choices belong to the adapter and its configuration, not the engine.
