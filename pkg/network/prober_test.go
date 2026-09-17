package network

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/health"
	"swarm-net/pkg/protocol"
)

// --- doubles ---------------------------------------------------------------------

type sentFrame struct {
	to  protocol.NodeID
	env *protocol.Envelope
}

// fakeTransport records every Send and lets the test answer as the peer.
type fakeTransport struct {
	self    protocol.NodeID
	sent    chan sentFrame
	sendErr error
}

func newFakeTransport(self protocol.NodeID) *fakeTransport {
	return &fakeTransport{self: self, sent: make(chan sentFrame, 64)}
}

func (f *fakeTransport) Self() protocol.NodeID { return f.self }
func (f *fakeTransport) Send(ctx context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	select {
	case f.sent <- sentFrame{to, env}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fakeTransport) Broadcast(context.Context, *protocol.Envelope) int { return 0 }
func (f *fakeTransport) Peers() []PeerInfo                                 { return nil }
func (f *fakeTransport) Events() <-chan PeerEvent                          { return nil }

func (f *fakeTransport) next(t *testing.T) sentFrame {
	t.Helper()
	select {
	case s := <-f.sent:
		return s
	case <-time.After(failsafe):
		t.Fatal("nothing was sent")
		return sentFrame{}
	}
}

func (f *fakeTransport) nothingSent(t *testing.T) {
	t.Helper()
	select {
	case s := <-f.sent:
		t.Fatalf("unexpected %s to %q", s.env.Type, s.to)
	default:
	}
}

// fakeClock is a settable monotonic-looking clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var resolveAB = func(a protocol.NodeAddress) (protocol.NodeID, bool) {
	switch a {
	case idA.Advertise:
		return idA.ID, true
	case idB.Advertise:
		return idB.ID, true
	}
	return "", false
}

// pongFor builds the PONG a well-behaved peer would send for a captured PING.
func pongFor(t *testing.T, ping sentFrame, from protocol.NodeID, nonceDelta uint64) *protocol.Envelope {
	t.Helper()
	req, err := protocol.PayloadOf[protocol.PingPayload](ping.env)
	if err != nil {
		t.Fatal(err)
	}
	pong, err := protocol.NewReply(ping.env, protocol.TypePong, from, protocol.PongPayload{Nonce: req.Nonce + nonceDelta, Seq: req.Seq})
	if err != nil {
		t.Fatal(err)
	}
	return pong
}

type probeOutcome struct {
	rtt time.Duration
	err error
}

func startProbe(ctx context.Context, m *MeshProber, target protocol.NodeAddress) <-chan probeOutcome {
	ch := make(chan probeOutcome, 1)
	go func() {
		rtt, err := m.Probe(ctx, target)
		ch <- probeOutcome{rtt, err}
	}()
	return ch
}

func awaitProbe(t *testing.T, ch <-chan probeOutcome) probeOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(failsafe):
		t.Fatal("probe did not return")
		return probeOutcome{}
	}
}

func pendingCount(m *MeshProber) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// --- probing ----------------------------------------------------------------------------

func TestProbeMeasuresRTTFromPingToMatchingPong(t *testing.T) {
	tr := newFakeTransport(idA.ID)
	clock := &fakeClock{t: time.Unix(1000, 0)}
	m := NewMeshProber(tr, resolveAB, clock.now)
	defer m.Close()

	out := startProbe(context.Background(), m, idB.Advertise)
	ping := tr.next(t)
	if ping.to != idB.ID || ping.env.Type != protocol.TypePing || ping.env.From != idA.ID {
		t.Fatalf("sent %s from %q to %q", ping.env.Type, ping.env.From, ping.to)
	}
	req, err := protocol.PayloadOf[protocol.PingPayload](ping.env)
	if err != nil {
		t.Fatal(err)
	}
	if req.Seq != 1 {
		t.Errorf("first probe Seq = %d, want 1", req.Seq)
	}

	clock.advance(37 * time.Millisecond)
	if !m.HandleFrame(idB.ID, pongFor(t, ping, idB.ID, 0)) {
		t.Error("PONG was not consumed")
	}
	o := awaitProbe(t, out)
	if o.err != nil || o.rtt != 37*time.Millisecond {
		t.Errorf("Probe = %v, %v; want 37ms", o.rtt, o.err)
	}
	if n := pendingCount(m); n != 0 {
		t.Errorf("pending after resolution = %d", n)
	}
}

func TestPingIsAnsweredWithPongEchoingNonceSeqAndID(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	defer m.Close()

	ping, err := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{Nonce: 0xdead, Seq: 9})
	if err != nil {
		t.Fatal(err)
	}
	if !m.HandleFrame(idA.ID, ping) {
		t.Fatal("PING was not consumed")
	}
	pong := tr.next(t)
	if pong.to != idA.ID || pong.env.Type != protocol.TypePong || pong.env.From != idB.ID {
		t.Errorf("reply = %s from %q to %q", pong.env.Type, pong.env.From, pong.to)
	}
	if pong.env.ID != ping.ID {
		t.Errorf("PONG ID = %q, want the PING's %q", pong.env.ID, ping.ID)
	}
	p, err := protocol.PayloadOf[protocol.PongPayload](pong.env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Nonce != 0xdead || p.Seq != 9 {
		t.Errorf("PONG payload = %+v", p)
	}

	// Frames that are neither PING nor PONG are not ours.
	if m.HandleFrame(idA.ID, mustEnv(t, protocol.TypeHeartbeat)) {
		t.Error("HEARTBEAT was consumed by the prober")
	}
	// A PING with an undecodable payload gets no answer and no panic.
	bad := mustEnv(t, protocol.TypePing)
	bad.Payload = []byte(`"nope"`)
	m.HandleFrame(idA.ID, bad)
	m.Close() // joins any reply goroutine so nothingSent is not racing one
	tr.nothingSent(t)
}

func TestNonceMismatchIsIgnoredAndProbeStaysPending(t *testing.T) {
	tr := newFakeTransport(idA.ID)
	clock := &fakeClock{t: time.Unix(1000, 0)}
	m := NewMeshProber(tr, resolveAB, clock.now)
	defer m.Close()

	out := startProbe(context.Background(), m, idB.Advertise)
	ping := tr.next(t)

	// Wrong nonce: consumed (it is a PONG) but not credited.
	if !m.HandleFrame(idB.ID, pongFor(t, ping, idB.ID, 1)) {
		t.Error("stale PONG was not consumed")
	}
	if n := pendingCount(m); n != 1 {
		t.Fatalf("pending after a mismatched PONG = %d, want 1", n)
	}
	select {
	case o := <-out:
		t.Fatalf("probe resolved on a mismatched nonce: %+v", o)
	default:
	}

	// A PONG for an ID nobody is waiting on, and an undecodable one, are also
	// consumed without effect.
	stray := mustEnv(t, protocol.TypePong)
	m.HandleFrame(idB.ID, stray)
	bad := pongFor(t, ping, idB.ID, 0)
	bad.Payload = []byte(`[]`)
	m.HandleFrame(idB.ID, bad)
	if n := pendingCount(m); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}

	clock.advance(5 * time.Millisecond)
	m.HandleFrame(idB.ID, pongFor(t, ping, idB.ID, 0))
	if o := awaitProbe(t, out); o.err != nil || o.rtt != 5*time.Millisecond {
		t.Errorf("Probe = %v, %v", o.rtt, o.err)
	}
}

func TestProbeCancellationRemovesPendingAndReturnsCtxErr(t *testing.T) {
	tr := newFakeTransport(idA.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	out := startProbe(ctx, m, idB.Advertise)
	tr.next(t)
	cancel()
	o := awaitProbe(t, out)
	if !errors.Is(o.err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", o.err)
	}
	if n := pendingCount(m); n != 0 {
		t.Errorf("pending after cancellation = %d", n)
	}

	// A deadline behaves the same way, and is what health.classify turns into
	// its own-timeout-vs-cancelled decision.
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer dcancel()
	_, err := m.Probe(dctx, idB.Advertise)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestProbeUnknownTargetAndSendFailure(t *testing.T) {
	tr := newFakeTransport(idA.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	defer m.Close()

	if _, err := m.Probe(context.Background(), "nowhere:1"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("unknown target err = %v, want ErrUnknownPeer", err)
	}

	tr.sendErr = ErrConnClosed
	_, err := m.Probe(context.Background(), idB.Advertise)
	if !errors.Is(err, ErrConnClosed) {
		t.Errorf("send failure err = %v, want ErrConnClosed", err)
	}
	if n := pendingCount(m); n != 0 {
		t.Errorf("pending after a failed send = %d", n)
	}
}

func TestCloseFailsPendingProbesAndRejectsNewOnes(t *testing.T) {
	tr := newFakeTransport(idA.ID)
	m := NewMeshProber(tr, resolveAB, nil)

	out1 := startProbe(context.Background(), m, idB.Advertise)
	out2 := startProbe(context.Background(), m, idB.Advertise)
	tr.next(t)
	tr.next(t)
	m.Close()
	for _, out := range []<-chan probeOutcome{out1, out2} {
		if o := awaitProbe(t, out); !errors.Is(o.err, ErrProberClosed) {
			t.Errorf("err after Close = %v, want ErrProberClosed", o.err)
		}
	}
	if _, err := m.Probe(context.Background(), idB.Advertise); !errors.Is(err, ErrProberClosed) {
		t.Errorf("Probe after Close = %v, want ErrProberClosed", err)
	}
	m.Close() // idempotent

	// A PING arriving after Close is not answered: no goroutine is started.
	ping, _ := protocol.NewEnvelope(protocol.TypePing, idB.ID, idA.ID, protocol.PingPayload{})
	m.HandleFrame(idB.ID, ping)
	tr.nothingSent(t)
}

func TestConcurrentProbesToOneTargetResolveIndependently(t *testing.T) {
	tr := newFakeTransport(idA.ID)
	clock := &fakeClock{t: time.Unix(1000, 0)}
	m := NewMeshProber(tr, resolveAB, clock.now)
	defer m.Close()

	const n = 5
	outs := make([]<-chan probeOutcome, n)
	for i := range outs {
		outs[i] = startProbe(context.Background(), m, idB.Advertise)
	}
	pings := make([]sentFrame, n)
	ids := map[string]bool{}
	for i := range pings {
		pings[i] = tr.next(t)
		if ids[pings[i].env.ID] {
			t.Fatalf("duplicate probe ID %q", pings[i].env.ID)
		}
		ids[pings[i].env.ID] = true
	}
	// Answer them in reverse, advancing the clock between answers: each probe
	// must be credited with its own RTT, not the first or last one.
	rtts := map[string]time.Duration{}
	for i := n - 1; i >= 0; i-- {
		clock.advance(time.Millisecond)
		rtts[pings[i].env.ID] = time.Duration(n-i) * time.Millisecond
		m.HandleFrame(idB.ID, pongFor(t, pings[i], idB.ID, 0))
	}
	got := map[time.Duration]int{}
	for _, out := range outs {
		o := awaitProbe(t, out)
		if o.err != nil {
			t.Fatal(o.err)
		}
		got[o.rtt]++
	}
	for _, want := range rtts {
		if got[want] != 1 {
			t.Errorf("RTT %v seen %d times, want once (all: %v)", want, got[want], got)
		}
	}
	if pendingCount(m) != 0 {
		t.Error("pending not empty")
	}
}

// --- delay hook ------------------------------------------------------------------------------

// fakeTimer lets the test decide when a delay elapses and records the stops.
type fakeTimer struct {
	mu      sync.Mutex
	fires   []chan time.Time
	armed   chan time.Duration
	stopped int
}

func newFakeTimer() *fakeTimer { return &fakeTimer{armed: make(chan time.Duration, 8)} }

func (f *fakeTimer) timer(d time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	f.mu.Lock()
	f.fires = append(f.fires, ch)
	f.mu.Unlock()
	f.armed <- d
	return ch, func() bool {
		f.mu.Lock()
		f.stopped++
		f.mu.Unlock()
		return true
	}
}

func (f *fakeTimer) fire(i int) {
	f.mu.Lock()
	ch := f.fires[i]
	f.mu.Unlock()
	ch <- time.Time{}
}

func TestSetDelayDefersTheReply(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	defer m.Close()
	ft := newFakeTimer()
	m.timer = ft.timer
	m.SetDelay(250 * time.Millisecond)

	ping, _ := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{Nonce: 7})
	m.HandleFrame(idA.ID, ping)
	if d := <-ft.armed; d != 250*time.Millisecond {
		t.Errorf("timer armed for %v, want 250ms", d)
	}
	tr.nothingSent(t) // the timer has been armed and not fired: no PONG yet
	ft.fire(0)
	if pong := tr.next(t); pong.env.ID != ping.ID {
		t.Errorf("PONG ID = %q", pong.env.ID)
	}

	// Clearing the delay answers immediately, without a timer.
	m.SetDelay(0)
	m.HandleFrame(idA.ID, ping)
	tr.next(t)
	select {
	case d := <-ft.armed:
		t.Errorf("a timer was armed for %v with no delay set", d)
	default:
	}
}

func TestCloseCancelsADelayedReply(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	ft := newFakeTimer()
	m.timer = ft.timer
	m.SetDelay(time.Hour)

	ping, _ := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{})
	m.HandleFrame(idA.ID, ping)
	<-ft.armed
	m.Close() // joins the reply goroutine; would hang for an hour on a real Sleep
	tr.nothingSent(t)
	ft.mu.Lock()
	stopped := ft.stopped
	ft.mu.Unlock()
	if stopped != 1 {
		t.Errorf("timer stopped %d times, want 1", stopped)
	}
}

// A reply blocked in Send (full queue) must not hold Close up either.
func TestCloseUnblocksAReplyStuckInSend(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	tr.sent = make(chan sentFrame) // unbuffered and never read: Send blocks
	m := NewMeshProber(tr, resolveAB, nil)
	ping, _ := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{})
	m.HandleFrame(idA.ID, ping)
	m.Close()
}

// --- wiring ---------------------------------------------------------------------------------------

// The prober over a real loopback pool pair, driven by the real strategy,
// produces a finite, non-negative latency score.
func TestMeshProberWiredIntoLatencyStrategy(t *testing.T) {
	var proberA, proberB *MeshProber
	a := startNode(t, "node-a", func(c *PoolConfig) {
		c.Handler = func(peer protocol.NodeID, env *protocol.Envelope) { proberA.HandleFrame(peer, env) }
	})
	b := startNode(t, "node-b", func(c *PoolConfig) {
		c.Handler = func(peer protocol.NodeID, env *protocol.Envelope) { proberB.HandleFrame(peer, env) }
	})
	resolve := func(addr protocol.NodeAddress) (protocol.NodeID, bool) {
		switch addr {
		case a.id.Advertise:
			return a.id.ID, true
		case b.id.Advertise:
			return b.id.ID, true
		}
		return "", false
	}
	proberA = NewMeshProber(a.pool, resolve, nil)
	proberB = NewMeshProber(b.pool, resolve, nil)
	defer proberA.Close()
	defer proberB.Close()

	a.pool.Connect(b.id.Advertise)
	a.event(t, PeerUp)
	b.event(t, PeerUp)

	strategy := health.NewLatencyHealthStrategy(health.Config{Probe: proberA.Probe, Alpha: 1})
	score, err := strategy.EvaluateScore(context.Background(), b.id.Advertise)
	if err != nil {
		t.Fatalf("EvaluateScore: %v", err)
	}
	if math.IsNaN(score) || score < 0 {
		t.Errorf("score = %v, want a finite non-negative latency in ms", score)
	}

	// An address the resolver does not know is unreachable, not a panic.
	if _, err := strategy.EvaluateScore(context.Background(), "ghost:1"); !errors.Is(err, health.ErrUnreachable) {
		t.Errorf("unknown target via strategy = %v, want ErrUnreachable", err)
	}
}
