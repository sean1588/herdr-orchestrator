package exec

import (
	"context"
	"sync"
	"time"
)

// defaultEventPollInterval is the pane-list poll cadence when none is set. It is
// the NewHerdr default and the eventHub's fallback, so a zero PollInterval never
// panics time.NewTicker in the daemon path.
const defaultEventPollInterval = 2 * time.Second

// eventBufferSize is the per-subscriber channel headroom beyond its join-time
// prime. Status changes are rare and consumers drain promptly, so this comfortably
// absorbs a whole wait's worth of events; a full buffer (only a wedged consumer)
// drops rather than stalling the shared poller and every other subscriber.
const eventBufferSize = 64

// eventHub multiplexes one `herdr pane list` poller across all Events subscribers.
// A single background poller runs while at least one subscriber is registered; on
// each tick it diffs pane statuses against a shared last-seen map and broadcasts
// every change to every subscriber. This replaces the previous one-poller-per-call
// model, where N concurrent drives each ran their own redundant `pane list` loop
// over the same global pane set.
//
// A subscriber joining mid-flight is primed with the current known state of every
// pane before it receives live changes, so it can never miss a status (e.g. a pane
// already "done") that changed before it subscribed. This preserves the old
// per-call poller's "first poll reports current state" semantics independent of a
// shared last-seen map. When the last subscriber leaves, the poller stops; a later
// subscriber restarts it, priming from the retained last-seen state.
type eventHub struct {
	poll     func(context.Context) ([]paneInfo, error) // snapshot source (herdr pane list)
	interval func() time.Duration                      // poll cadence, read at poller start

	mu   sync.Mutex
	subs map[*eventSub]struct{}
	// last holds the last-seen status per pane, the diff state shared across the
	// poller's lifetime. It retains an entry per pane observed since the daemon
	// started (never pruned); at max_concurrent_tasks scale that is negligible, and
	// a push-based event source (roadmap) would replace this poller wholesale.
	last   map[string]AgentState
	cancel context.CancelFunc // non-nil iff the poller is running
}

type eventSub struct {
	ch chan Event
}

func newEventHub(poll func(context.Context) ([]paneInfo, error), interval func() time.Duration) *eventHub {
	return &eventHub{
		poll:     poll,
		interval: interval,
		subs:     map[*eventSub]struct{}{},
		last:     map[string]AgentState{},
	}
}

// subscribe registers a new subscriber and returns its event channel, primed with
// the current known pane states and then carrying live status changes. The channel
// is closed and the subscriber unregistered when ctx is done; the shared poller
// stops once no subscribers remain.
func (hub *eventHub) subscribe(ctx context.Context) <-chan Event {
	hub.mu.Lock()
	// Buffer the prime plus headroom so priming under the lock never blocks.
	sub := &eventSub{ch: make(chan Event, len(hub.last)+eventBufferSize)}
	for pane, st := range hub.last {
		sub.ch <- Event{PaneID: pane, State: st}
	}
	hub.subs[sub] = struct{}{}
	hub.startLocked()
	hub.mu.Unlock()

	go func() {
		<-ctx.Done()
		hub.unsubscribe(sub)
	}()
	return sub.ch
}

// unsubscribe removes a subscriber, closes its channel, and stops the poller when
// no subscribers remain. Broadcast and unsubscribe both hold hub.mu, so the poller
// never sends on a closed channel.
func (hub *eventHub) unsubscribe(sub *eventSub) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if _, ok := hub.subs[sub]; !ok {
		return
	}
	delete(hub.subs, sub)
	close(sub.ch)
	if len(hub.subs) == 0 && hub.cancel != nil {
		hub.cancel()
		hub.cancel = nil
	}
}

// startLocked starts the poller if it is not already running. Caller holds hub.mu.
func (hub *eventHub) startLocked() {
	if hub.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	hub.cancel = cancel
	go hub.run(ctx)
}

// run is the single poll loop: poll, broadcast changes, wait a tick, repeat, until
// the last subscriber leaves (ctx cancelled).
func (hub *eventHub) run(ctx context.Context) {
	iv := hub.interval()
	if iv <= 0 {
		iv = defaultEventPollInterval
	}
	ticker := time.NewTicker(iv)
	defer ticker.Stop()
	for {
		if panes, err := hub.poll(ctx); err == nil {
			hub.broadcast(panes)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// broadcast diffs the polled panes against the shared last-seen map and delivers
// each change to every subscriber. Sends are non-blocking: a wedged consumer drops
// its event rather than stalling the poller and the other subscribers.
func (hub *eventHub) broadcast(panes []paneInfo) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, p := range panes {
		st := normalizeState(p.AgentStatus)
		if prev, ok := hub.last[p.PaneID]; ok && prev == st {
			continue
		}
		hub.last[p.PaneID] = st
		ev := Event{PaneID: p.PaneID, State: st}
		for sub := range hub.subs {
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}
