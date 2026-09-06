package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/sean1588/herdr-orchestrator/internal/source"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// SourceTaskID maps a collection and opaque external key to a safe task/branch
// name. Hash the tuple rather than concatenate delimiters or sanitize the key:
// distinct page IDs and collections must not alias, or inject path components.
func SourceTaskID(collection, key string) string {
	data, _ := json.Marshal([2]string{collection, key})
	return fmt.Sprintf("source-%x", sha256.Sum256(data))
}

// RunSource drives an opaque external item using the injected source. Existing
// Run(int) keeps GitHub's issue-N IDs, so old databases and branches keep working.
func (e *Engine) RunSource(ctx context.Context, key string) (string, error) {
	if strings.TrimSpace(e.sourceID) == "" || strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("source collection ID and item key must be nonempty")
	}
	if e.source == nil {
		return "", fmt.Errorf("source %q is not configured", e.sourceID)
	}
	id := SourceTaskID(e.sourceID, key)
	task, err := e.store.GetTask(ctx, id)
	created := false
	if errors.Is(err, store.ErrNotFound) {
		task = &store.Task{ID: id, SourceID: e.sourceID, SourceKey: key,
			Repo: e.repo, Branch: "agent/" + id, CurrentState: e.startState,
			WorkflowSnapshot: string(e.workflowSource)}
		if err = e.store.CreateTask(ctx, task); err != nil {
			return "", err
		}
		created = true
	} else if err != nil {
		return "", err
	}
	return e.runTask(ctx, task, created)
}

func sourceKey(task *store.Task) string {
	if task.SourceKey != "" {
		return task.SourceKey
	}
	return strconv.Itoa(task.Issue)
}

func (e *Engine) checkSource(task *store.Task) error {
	if e.source == nil {
		return fmt.Errorf("task %s: no issue source configured", task.ID)
	}
	if task.SourceID != e.sourceID {
		return fmt.Errorf("task %s belongs to source %q, configured source is %q", task.ID, task.SourceID, e.sourceID)
	}
	return nil
}

func (e *Engine) sourceItem(ctx context.Context, task *store.Task) (*source.Item, error) {
	if err := e.checkSource(task); err != nil {
		return nil, err
	}
	item, err := e.source.Get(ctx, sourceKey(task))
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("source returned no item for %q", sourceKey(task))
	}
	if item.Key != sourceKey(task) {
		return nil, fmt.Errorf("source returned key %q for %q", item.Key, sourceKey(task))
	}
	return item, nil
}

// Resolve selectors from the task's pinned workflow whenever it declares the
// collection. Programmatic sources can instead supply SourceSelector in Config.
func (e *Engine) sourceSelector() source.Selector {
	if e.sourceID == "" {
		return source.Selector{"label": e.wf.SourceLabel()}
	}
	for _, s := range e.wf.Sources {
		if s.ID == e.sourceID {
			return source.Selector(s.Select)
		}
	}
	return e.selector
}

func (e *Engine) acknowledge(ctx context.Context, task *store.Task) error {
	if err := e.checkSource(task); err != nil {
		return err
	}
	return e.source.Acknowledge(ctx, sourceKey(task), e.sourceSelector())
}

func (e *Engine) completeSource(ctx context.Context, task *store.Task, comment string) error {
	if err := e.checkSource(task); err != nil {
		return err
	}
	return e.source.Complete(ctx, sourceKey(task), comment)
}

// Use the safe internal ID in terminal kickoffs; external IDs may contain newlines.
func sourceDisplay(task *store.Task) string {
	if task.SourceID == "" {
		return "#" + strconv.Itoa(task.Issue)
	}
	return task.ID
}
