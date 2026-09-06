// Package source defines the work-intake boundary, independent of code hosting.
// Sources are bound to an external collection and must support concurrent calls
// with context cancellation. Item keys are opaque to the interface; the current
// daemon maps its numeric issue IDs at the boundary. Completion means successful
// implementation, while acknowledgement only removes settled work from discovery.
// Adapters must make both mutations idempotent. Selectors are read-only workflow
// data and may come from a task's pinned configuration during recovery.
package source

import "context"

// Item is the text an agent needs to work on an external issue or page.
// Key is opaque: adapters must preserve IDs such as Notion page UUIDs verbatim.
type Item struct {
	Key   string
	Title string
	Body  string
}

// Selector is provider-specific discovery configuration (for example a GitHub
// label or a Notion database status). It must not contain credentials. Adapters
// must treat it as read-only; it may be shared between concurrent task drives.
type Selector map[string]any

// Source is bound to one external collection, independently of the code checkout.
// All methods honor cancellation. List may return duplicates; callers deduplicate
// by key. Acknowledge and Complete must be idempotent, since recovery can retry.
type Source interface {
	List(ctx context.Context, selector Selector) ([]string, error)
	Get(ctx context.Context, key string) (*Item, error)
	// Acknowledge removes settled work from discovery without marking it done.
	// Escalations and cancellations also acknowledge; they must NOT complete it.
	Acknowledge(ctx context.Context, key string, selector Selector) error
	// Complete marks successfully implemented work done, with a result comment.
	Complete(ctx context.Context, key, comment string) error
}
