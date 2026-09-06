// Package source defines the work-intake boundary, independent of code hosting.
package source

import "context"

// Item is the text an agent needs to work on an external issue or page.
// Key is opaque: adapters must preserve IDs such as Notion page UUIDs verbatim.
type Item struct {
	Key   string
	Title string
	Body  string
	URL   string
}

// Selector is provider-specific discovery configuration (for example a GitHub
// label or a Notion database status). It must not contain credentials. Adapters
// must treat it as read-only; it may be shared between concurrent task drives.
type Selector map[string]any

// Source is bound to one external collection, independently of the code checkout.
// All methods honor cancellation. List may return duplicates; callers deduplicate
// by key. Acknowledge and Complete must be idempotent, since recovery can retry.
type Source interface {
	List(context.Context, Selector) ([]string, error)
	Get(context.Context, string) (*Item, error)
	// Acknowledge removes settled work from discovery without marking it done.
	// Escalations and cancellations also acknowledge; they must NOT complete it.
	Acknowledge(context.Context, string, Selector) error
	// Complete marks successfully implemented work done, with a result comment.
	Complete(context.Context, string, string) error
}
