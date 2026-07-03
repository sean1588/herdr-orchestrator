package exec

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakePoll is a scriptable, observable pane-list source for eventHub tests. It
// records how many times it was called and the maximum number of concurrent
// in-flight calls, so a test can prove a single shared poller drives all
// subscribers (max concurrency 1) rather than one poller per subscriber.
type fakePoll struct {
	mu          sync.Mutex
	panes       []paneInfo
	calls       int
	inFlight    int
	maxInFlight int
	delay       time.Duration // artificial in-flight window, widens the overlap detector
}

func (p *fakePoll) set(panes []paneInfo) {
	p.mu.Lock()
	p.panes = panes
	p.mu.Unlock()
}

func (p *fakePoll) poll(ctx context.Context) ([]paneInfo, error) {
	p.mu.Lock()
	p.calls++
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	panes := append([]paneInfo(nil), p.panes...)
	delay := p.delay
	p.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return panes, nil
}

func (p *fakePoll) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakePoll) maxConcurrency() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight
}

func fastInterval() func() time.Duration {
	return func() time.Duration { return 2 * time.Millisecond }
}

func recvEvent(t *testing.T, ch <-chan Event, timeout time.Duration) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(timeout):
		t.Fatal("timed out waiting for event")
		return Event{}, false
	}
}

// waitFor polls cond until it is true, failing after timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// waitClosed drains ch until it is closed, failing if that does not happen
// within timeout.
func waitClosed(t *testing.T, ch <-chan Event, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after unsubscribe")
		}
	}
}

// A change observed by the single poller must reach every live subscriber.
func TestEventHub_FanOutToMultipleSubscribers(t *testing.T) {
	fp := &fakePoll{}
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "working"}})
	hub := newEventHub(fp.poll, fastInterval())

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch1 := hub.subscribe(ctx1)
	ch2 := hub.subscribe(ctx2)

	for _, ch := range []<-chan Event{ch1, ch2} {
		ev, ok := recvEvent(t, ch, 2*time.Second)
		if !ok || ev.PaneID != "p1" || ev.State != StateWorking {
			t.Fatalf("want {p1 working}, got %+v ok=%v", ev, ok)
		}
	}

	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "done"}})
	for _, ch := range []<-chan Event{ch1, ch2} {
		ev, ok := recvEvent(t, ch, 2*time.Second)
		if !ok || ev.State != StateDone {
			t.Fatalf("want done, got %+v ok=%v", ev, ok)
		}
	}
}

// The debt regression: N subscribers must share ONE poller, not spawn one each.
// A single poll loop is serial, so max in-flight poll concurrency is exactly 1;
// a per-subscriber poller would overlap and reach 2.
func TestEventHub_SinglePollerRegardlessOfSubscribers(t *testing.T) {
	fp := &fakePoll{delay: 3 * time.Millisecond}
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "working"}})
	hub := newEventHub(fp.poll, func() time.Duration { return 1 * time.Millisecond })

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch1 := hub.subscribe(ctx1)
	ch2 := hub.subscribe(ctx2)

	// Both active (poller is delivering to each).
	recvEvent(t, ch1, 2*time.Second)
	recvEvent(t, ch2, 2*time.Second)

	// Let several poll cycles run.
	time.Sleep(30 * time.Millisecond)
	if mc := fp.maxConcurrency(); mc != 1 {
		t.Fatalf("max concurrent poll = %d, want 1 (one shared poller across subscribers)", mc)
	}
}

// A subscriber that joins after a pane already changed must be primed with the
// current state, even though the poller will not re-broadcast an unchanged
// status. Without priming, a late subscriber would miss an already-"done" pane
// and hang until its timeout.
func TestEventHub_PrimesLateSubscriberWithCurrentState(t *testing.T) {
	fp := &fakePoll{}
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "done"}})
	hub := newEventHub(fp.poll, fastInterval())

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ch1 := hub.subscribe(ctx1)
	ev, ok := recvEvent(t, ch1, 2*time.Second)
	if !ok || ev.State != StateDone {
		t.Fatalf("sub1 want done, got %+v ok=%v", ev, ok)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch2 := hub.subscribe(ctx2)
	ev2, ok2 := recvEvent(t, ch2, 2*time.Second)
	if !ok2 || ev2.PaneID != "p1" || ev2.State != StateDone {
		t.Fatalf("late subscriber not primed: want {p1 done}, got %+v ok=%v", ev2, ok2)
	}
}

// The shared poller stops once the last subscriber leaves (no busy-poll with no
// listeners), and the subscriber's channel is closed on unsubscribe.
func TestEventHub_StopsPollerWhenLastSubscriberLeaves(t *testing.T) {
	fp := &fakePoll{}
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "working"}})
	hub := newEventHub(fp.poll, fastInterval())

	ctx, cancel := context.WithCancel(context.Background())
	ch := hub.subscribe(ctx)
	recvEvent(t, ch, 2*time.Second) // poller is running

	time.Sleep(20 * time.Millisecond)
	cancel() // last subscriber leaves

	waitClosed(t, ch, 2*time.Second)

	// Give any in-flight poll time to finish, then confirm polling has stopped.
	time.Sleep(15 * time.Millisecond)
	before := fp.callCount()
	time.Sleep(30 * time.Millisecond)
	if after := fp.callCount(); after != before {
		t.Fatalf("poller kept polling after last unsubscribe: %d -> %d", before, after)
	}
}

// One of several concurrent subscribers leaving must NOT stop the shared poller:
// the survivors keep receiving status changes. This is the core multiplexing
// invariant (the poller runs while >=1 subscriber remains). A regression to
// "stop on any unsubscribe" would pass every other test yet silently starve the
// remaining in-flight drives until their state timeout.
func TestEventHub_SurvivesPartialUnsubscribe(t *testing.T) {
	fp := &fakePoll{}
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "working"}})
	hub := newEventHub(fp.poll, fastInterval())

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	ch1 := hub.subscribe(ctx1)
	ch2 := hub.subscribe(ctx2)
	recvEvent(t, ch1, 2*time.Second)
	recvEvent(t, ch2, 2*time.Second)

	// One subscriber leaves; only its channel closes.
	cancel1()
	waitClosed(t, ch1, 2*time.Second)

	// The survivor still sees a subsequent change — proof the poller kept running.
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "done"}})
	ev, ok := recvEvent(t, ch2, 2*time.Second)
	if !ok || ev.State != StateDone {
		t.Fatalf("survivor should still receive changes after a peer left, got %+v ok=%v", ev, ok)
	}

	// Now the last subscriber leaves; the poller stops.
	cancel2()
	waitClosed(t, ch2, 2*time.Second)
	time.Sleep(15 * time.Millisecond)
	before := fp.callCount()
	time.Sleep(30 * time.Millisecond)
	if after := fp.callCount(); after != before {
		t.Fatalf("poller kept polling after the last subscriber left: %d -> %d", before, after)
	}
}

// A new subscriber after the poller stopped restarts it, priming from the
// retained last-seen state.
func TestEventHub_RestartsPollerForNewSubscriberAfterStop(t *testing.T) {
	fp := &fakePoll{}
	fp.set([]paneInfo{{PaneID: "p1", AgentStatus: "working"}})
	hub := newEventHub(fp.poll, fastInterval())

	ctx1, cancel1 := context.WithCancel(context.Background())
	ch1 := hub.subscribe(ctx1)
	recvEvent(t, ch1, 2*time.Second)
	cancel1()
	waitClosed(t, ch1, 2*time.Second)

	time.Sleep(15 * time.Millisecond)
	stopped := fp.callCount()
	time.Sleep(20 * time.Millisecond)
	if fp.callCount() != stopped {
		t.Fatalf("poller did not stop: %d -> %d", stopped, fp.callCount())
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch2 := hub.subscribe(ctx2)
	ev, ok := recvEvent(t, ch2, 2*time.Second)
	if !ok || ev.State != StateWorking {
		t.Fatalf("restarted poller should deliver current state, got %+v ok=%v", ev, ok)
	}
	// ch2's event above came from the prime; confirm the poller itself restarted.
	waitFor(t, 2*time.Second, func() bool { return fp.callCount() > stopped })
}
