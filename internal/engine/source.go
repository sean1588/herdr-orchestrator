package engine

import (
	"context"
	"fmt"
	"strconv"

	"github.com/sean1588/herdr-orchestrator/internal/source"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// Numeric task identity stays at this boundary until a second source supplies
// the concrete requirements for persistent opaque identities.
func sourceKey(task *store.Task) string { return strconv.Itoa(task.Issue) }

func (e *Engine) sourceItem(ctx context.Context, task *store.Task) (*source.Item, error) {
	key := sourceKey(task)
	item, err := e.source.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("source returned no item for %q", key)
	}
	if item.Key != key {
		return nil, fmt.Errorf("source returned key %q for %q", item.Key, key)
	}
	return item, nil
}

func (e *Engine) acknowledge(ctx context.Context, task *store.Task) error {
	return e.source.Acknowledge(ctx, sourceKey(task), source.Selector{"label": e.wf.SourceLabel()})
}

func (e *Engine) completeSource(ctx context.Context, task *store.Task, comment string) error {
	return e.source.Complete(ctx, sourceKey(task), comment)
}
