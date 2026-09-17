package cluster

import (
	"sort"
	"sync"
	"time"
)

// Clock is the time seam for the node's event loop.
//
// Production uses RealClock. Tests use FakeClock, so that "advance thirty seconds
// and check that the floor election ran" is a synchronous call rather than a
// time.Sleep and a hope. Every timer the node creates goes through this interface;
// a bare time.NewTicker anywhere in the loop would be invisible to FakeClock and
// would make the test that depends on it flaky by construction.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
	// After returns a channel that receives once, after d.
	After(d time.Duration) <-chan time.Time
}

// Ticker is the subset of *time.Ticker the loop needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// RealClock is the production Clock: a thin wrapper over package time.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// FakeClock is a Clock that only moves when a test tells it to.
//
// # Delivery semantics, and why they differ from package time
//
// A real ticker drops ticks a slow receiver did not collect. FakeClock does the
// opposite: Advance delivers every tick that fell due, in deadline order, and blocks
// on each one until the receiver has taken it (or has stopped the ticker). Ticker
// channels are therefore unbuffered. The point is determinism: when Advance returns,
// the receiving goroutine has *observed* every tick, so a test can advance the clock
// and then inspect state without racing the loop it is driving. A buffered channel
// would let Advance return with the tick still sitting in the buffer, and the
// assertion that follows would be a coin toss.
//
// After is the one exception. Its channel is buffered with capacity one, matching
// time.After, because a one-shot timer has no Stop and so no way to tell Advance
// that its receiver has gone; an unbuffered send to an abandoned After would hang
// the test forever.
//
// # Synchronisation
//
// mu guards now and waiters. advMu serialises Advance calls so that two tests
// goroutines advancing concurrently cannot interleave their deliveries. mu is
// released around each blocking send so a receiver may call Now, NewTicker or Stop
// while a tick is in flight -- the loop under test does exactly that.
type FakeClock struct {
	advMu sync.Mutex

	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
	seq     uint64
	// registered is signalled (under mu) whenever a timer is added, so a test
	// can wait for a goroutine to reach its timer without polling.
	registered *sync.Cond
}

// fakeWaiter is one pending timer. period is zero for a one-shot After.
type fakeWaiter struct {
	deadline time.Time
	period   time.Duration
	seq      uint64 // creation order: the tie-break for equal deadlines
	ch       chan time.Time
	stopped  chan struct{}
	stopOnce sync.Once
}

// NewFakeClock returns a FakeClock reading start.
func NewFakeClock(start time.Time) *FakeClock {
	f := &FakeClock{now: start}
	f.registered = sync.NewCond(&f.mu)
	return f
}

// Now returns the current fake time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Set moves the clock to t without firing anything. Timers whose deadline is now in
// the past fire on the next Advance, which is what lets a test position the clock
// and then trigger deliveries explicitly.
func (f *FakeClock) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

// NewTicker registers a periodic timer whose first tick is due at now+d.
func (f *FakeClock) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("cluster: FakeClock.NewTicker: non-positive interval")
	}
	w := f.register(d, d, make(chan time.Time))
	return fakeTicker{f: f, w: w}
}

// After registers a one-shot timer due at now+d.
func (f *FakeClock) After(d time.Duration) <-chan time.Time {
	w := f.register(d, 0, make(chan time.Time, 1))
	return w.ch
}

func (f *FakeClock) register(d, period time.Duration, ch chan time.Time) *fakeWaiter {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	w := &fakeWaiter{
		deadline: f.now.Add(d),
		period:   period,
		seq:      f.seq,
		ch:       ch,
		stopped:  make(chan struct{}),
	}
	f.waiters = append(f.waiters, w)
	f.registered.Broadcast()
	return w
}

// awaitTimers blocks until at least n timers are registered. Test seam: a
// goroutine that parks on After (a sleeping task) has no other way to say it
// has got there, and advancing before it does would fire nothing.
func (f *FakeClock) awaitTimers(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.registered.Wait()
	}
}

// Advance moves the clock forward by d, firing every timer that falls due on the
// way in deadline order. It returns only when every fired tick has been received or
// its ticker stopped. Advancing with nothing registered simply moves the clock.
func (f *FakeClock) Advance(d time.Duration) {
	f.advanceStepwise(d, nil)
}

// advanceStepwise is Advance with a hook: after each delivered tick it calls
// between (if non-nil) before looking for the next due timer.
//
// # Why Advance alone is not enough for a deterministic simulation
//
// Advance waits until each tick is *received*, not until the receiver has
// finished what the tick started. When several timers share a deadline (the
// node's 1s probe, 2s gossip and 500ms failure-detector tickers all meet at
// every even second), the probe tick starts a round on other goroutines, and
// by the time Advance offers the gossip tick the round's result may or may not
// be waiting too. The loop's select then picks between them at random, so
// whether a new self-score is announced before or after that instant's gossip
// and beats depends on the scheduler. A test that settles the node inside
// between gets one fixed order: each tick's consequences, probe round included,
// are complete before the next tick at the same instant is delivered.
func (f *FakeClock) advanceStepwise(d time.Duration, between func()) {
	if d < 0 {
		panic("cluster: FakeClock.Advance: negative duration")
	}
	f.advMu.Lock()
	defer f.advMu.Unlock()

	f.mu.Lock()
	target := f.now.Add(d)
	for {
		w := f.nextDue(target)
		if w == nil {
			f.now = target
			f.mu.Unlock()
			return
		}
		// The clock reads the tick's own deadline while the tick is delivered, so a
		// receiver that calls Now sees a time consistent with the timer that woke it.
		f.now = w.deadline
		fireAt := w.deadline
		if w.period > 0 {
			w.deadline = w.deadline.Add(w.period)
		} else {
			f.remove(w)
		}
		f.mu.Unlock()

		// Unlocked during the send: the receiver may need mu (Now, NewTicker, Stop)
		// before it can take the tick. Selecting on stopped is what keeps a ticker
		// whose owner has gone from hanging the test.
		select {
		case w.ch <- fireAt:
		case <-w.stopped:
		}
		// Also unlocked: between typically waits on the receiving loop, which
		// may itself need mu.
		if between != nil {
			between()
		}

		f.mu.Lock()
	}
}

// nextDue returns the earliest unstopped waiter with deadline <= target, breaking
// ties on creation order. Caller holds mu.
func (f *FakeClock) nextDue(target time.Time) *fakeWaiter {
	sort.SliceStable(f.waiters, func(i, j int) bool {
		a, b := f.waiters[i], f.waiters[j]
		if !a.deadline.Equal(b.deadline) {
			return a.deadline.Before(b.deadline)
		}
		return a.seq < b.seq
	})
	for _, w := range f.waiters {
		if w.deadline.After(target) {
			return nil
		}
		select {
		case <-w.stopped:
			continue
		default:
			return w
		}
	}
	return nil
}

// remove unregisters w. Caller holds mu.
func (f *FakeClock) remove(w *fakeWaiter) {
	for i, x := range f.waiters {
		if x == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			return
		}
	}
}

// pending reports how many timers are registered. Used by tests to prove Stop
// releases the waiter.
func (f *FakeClock) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

type fakeTicker struct {
	f *FakeClock
	w *fakeWaiter
}

func (t fakeTicker) C() <-chan time.Time { return t.w.ch }

// Stop unregisters the ticker and releases any Advance blocked on delivering to it.
// Idempotent, like time.Ticker.Stop.
func (t fakeTicker) Stop() {
	t.w.stopOnce.Do(func() {
		close(t.w.stopped)
		t.f.mu.Lock()
		t.f.remove(t.w)
		t.f.mu.Unlock()
	})
}
