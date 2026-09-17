package cluster

import (
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)

// collect receives from c on its own goroutine and returns a function that stops
// receiving and reports what was received. This is the receiver shape the node's
// loop has: always ready, so Advance never blocks on it.
func collect(c <-chan time.Time) (stop func() []time.Time) {
	var (
		mu   sync.Mutex
		got  []time.Time
		done = make(chan struct{})
		quit = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			select {
			case t := <-c:
				mu.Lock()
				got = append(got, t)
				mu.Unlock()
			case <-quit:
				return
			}
		}
	}()
	return func() []time.Time {
		close(quit)
		<-done
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestFakeClockNowAndSet(t *testing.T) {
	f := NewFakeClock(epoch)
	if !f.Now().Equal(epoch) {
		t.Fatalf("Now = %v, want %v", f.Now(), epoch)
	}
	later := epoch.Add(time.Hour)
	f.Set(later)
	if !f.Now().Equal(later) {
		t.Fatalf("after Set, Now = %v, want %v", f.Now(), later)
	}
	// Set does not fire; Advance(0) delivers what is now overdue.
	f.Set(epoch)
	tk := f.NewTicker(time.Second)
	f.Set(epoch.Add(time.Second))
	stop := collect(tk.C())
	f.Advance(0)
	if got := stop(); len(got) != 1 {
		t.Fatalf("Advance(0) after Set past the deadline fired %d ticks, want 1", len(got))
	}
}

func TestFakeClockAdvanceWithNoWaitersMovesTime(t *testing.T) {
	f := NewFakeClock(epoch)
	f.Advance(3 * time.Second)
	if !f.Now().Equal(epoch.Add(3 * time.Second)) {
		t.Fatalf("Now = %v, want %v", f.Now(), epoch.Add(3*time.Second))
	}
}

func TestFakeClockTickerFiresNTimesForNIntervals(t *testing.T) {
	f := NewFakeClock(epoch)
	tk := f.NewTicker(time.Second)
	stop := collect(tk.C())

	f.Advance(5 * time.Second)
	got := stop()
	if len(got) != 5 {
		t.Fatalf("got %d ticks, want 5", len(got))
	}
	for i, ts := range got {
		want := epoch.Add(time.Duration(i+1) * time.Second)
		if !ts.Equal(want) {
			t.Errorf("tick %d at %v, want %v", i, ts, want)
		}
	}
	if !f.Now().Equal(epoch.Add(5 * time.Second)) {
		t.Errorf("Now = %v, want %v", f.Now(), epoch.Add(5*time.Second))
	}
}

func TestFakeClockTickerDoesNotFireBeforeInterval(t *testing.T) {
	f := NewFakeClock(epoch)
	tk := f.NewTicker(time.Second)
	stop := collect(tk.C())
	f.Advance(999 * time.Millisecond)
	if got := stop(); len(got) != 0 {
		t.Fatalf("fired %d ticks before the interval elapsed", len(got))
	}
}

func TestFakeClockNowReadsTickDeadlineDuringDelivery(t *testing.T) {
	f := NewFakeClock(epoch)
	tk := f.NewTicker(time.Second)
	seen := make(chan time.Time, 1)
	go func() {
		<-tk.C()
		seen <- f.Now()
	}()
	f.Advance(time.Second)
	if got := <-seen; !got.Equal(epoch.Add(time.Second)) {
		t.Fatalf("Now during tick = %v, want %v", got, epoch.Add(time.Second))
	}
}

func TestFakeClockStopPreventsFurtherTicksAndReleasesAdvance(t *testing.T) {
	f := NewFakeClock(epoch)
	tk := f.NewTicker(time.Second)
	stop := collect(tk.C())
	f.Advance(2 * time.Second)
	if got := stop(); len(got) != 2 {
		t.Fatalf("got %d ticks, want 2", len(got))
	}

	// Nobody is receiving now. Without Stop, Advance would block forever on the
	// unbuffered send; with it, Advance must return promptly.
	tk.Stop()
	tk.Stop() // idempotent
	if f.pending() != 0 {
		t.Fatalf("pending = %d after Stop, want 0", f.pending())
	}
	f.Advance(10 * time.Second)

	select {
	case <-tk.C():
		t.Fatal("stopped ticker delivered a tick")
	default:
	}
}

func TestFakeClockStopDuringDeliveryUnblocksAdvance(t *testing.T) {
	f := NewFakeClock(epoch)
	tk := f.NewTicker(time.Second)

	// Receiver takes one tick and then stops the ticker instead of taking the
	// second, which is exactly what a loop does on shutdown.
	go func() {
		<-tk.C()
		tk.Stop()
	}()
	f.Advance(5 * time.Second) // must not hang
}

func TestFakeClockAfterFiresOnce(t *testing.T) {
	f := NewFakeClock(epoch)
	c := f.After(2 * time.Second)

	f.Advance(time.Second)
	select {
	case <-c:
		t.Fatal("After fired early")
	default:
	}

	f.Advance(time.Second)
	select {
	case ts := <-c:
		if !ts.Equal(epoch.Add(2 * time.Second)) {
			t.Fatalf("After fired at %v, want %v", ts, epoch.Add(2*time.Second))
		}
	default:
		t.Fatal("After did not fire at its deadline")
	}
	if f.pending() != 0 {
		t.Fatalf("pending = %d after one-shot fired, want 0", f.pending())
	}

	f.Advance(time.Hour)
	select {
	case <-c:
		t.Fatal("After fired twice")
	default:
	}
}

func TestFakeClockOrdersTwoTickersByDeadline(t *testing.T) {
	f := NewFakeClock(epoch)
	slow := f.NewTicker(3 * time.Second)
	fast := f.NewTicker(2 * time.Second)

	var (
		mu    sync.Mutex
		order []string
		done  = make(chan struct{})
		quit  = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			select {
			case <-fast.C():
				mu.Lock()
				order = append(order, "fast")
				mu.Unlock()
			case <-slow.C():
				mu.Lock()
				order = append(order, "slow")
				mu.Unlock()
			case <-quit:
				return
			}
		}
	}()

	// Deadlines: fast at 2, 4, 6; slow at 3, 6. At t=6 both are due and the
	// tie breaks on creation order: slow was created first.
	f.Advance(6 * time.Second)
	close(quit)
	<-done

	want := []string{"fast", "slow", "fast", "slow", "fast"}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestFakeClockTickerCreatedInsideHandlerFiresOnNextAdvance(t *testing.T) {
	f := NewFakeClock(epoch)
	first := f.NewTicker(time.Second)

	// The handler registers a second ticker while the first tick is being
	// delivered. Whether the same Advance sees it is a race between the handler and
	// Advance re-acquiring the clock, so nothing is asserted about that call; the
	// next Advance must honour it.
	registered := make(chan Ticker, 1)
	go func() {
		<-first.C()
		first.Stop()
		registered <- f.NewTicker(time.Second)
	}()
	f.Advance(time.Second)
	second := <-registered
	stop := collect(second.C())
	f.Advance(2 * time.Second)
	if got := stop(); len(got) != 2 {
		t.Fatalf("ticker created inside a handler fired %d times on the next Advance, want 2", len(got))
	}
}

func TestFakeClockPanicsOnBadArguments(t *testing.T) {
	f := NewFakeClock(epoch)
	assertPanics(t, "NewTicker(0)", func() { f.NewTicker(0) })
	assertPanics(t, "Advance(-1)", func() { f.Advance(-1) })
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", name)
		}
	}()
	fn()
}

func TestRealClock(t *testing.T) {
	var c Clock = RealClock{}
	if c.Now().IsZero() {
		t.Fatal("RealClock.Now returned zero")
	}
	tk := c.NewTicker(time.Millisecond)
	select {
	case <-tk.C():
	case <-time.After(5 * time.Second):
		t.Fatal("RealClock ticker never fired")
	}
	tk.Stop()
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("RealClock.After never fired")
	}
}
