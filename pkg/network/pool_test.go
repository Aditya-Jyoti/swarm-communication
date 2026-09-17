package network

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// --- test doubles ------------------------------------------------------------------
//
// No time.Sleep. The pool is driven through a fake Dialer that hands the test the
// peer's end of every socket it opens, and every wait is a channel receive with a
// generous failsafe deadline that only fires when something is actually broken.

const failsafe = 10 * time.Second

// fakeDialer opens a real loopback socket pair per dial and delivers the peer end
// to the test. Real sockets rather than net.Pipe because the disposition taxonomy
// (clean close vs died mid-frame) depends on io.EOF semantics that net.Pipe does
// not reproduce: it reports io.ErrClosedPipe for everything.
type fakeDialer struct {
	peers chan net.Conn
	// fail, if set, is consulted per attempt (1-based) and can veto the dial.
	fail func(attempt int) error
	// blockUntilCtx makes DialContext hang until the context is cancelled, which
	// is how a test proves Close interrupts a dial in progress.
	blockUntilCtx bool

	mu       sync.Mutex
	attempts int
	dialed   chan int
}

func newFakeDialer() *fakeDialer {
	return &fakeDialer{peers: make(chan net.Conn, 16), dialed: make(chan int, 64)}
}

func (d *fakeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.attempts++
	n := d.attempts
	d.mu.Unlock()
	d.dialed <- n

	if d.blockUntilCtx {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if d.fail != nil {
		if err := d.fail(n); err != nil {
			return nil, err
		}
	}
	local, remote, err := loopbackPair()
	if err != nil {
		return nil, err
	}
	select {
	case d.peers <- remote:
	case <-ctx.Done():
		local.Close()
		remote.Close()
		return nil, ctx.Err()
	}
	return local, nil
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

func loopbackPair() (client, server net.Conn, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer ln.Close()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, nil, err
	}
	server, err = ln.Accept()
	if err != nil {
		client.Close()
		return nil, nil, err
	}
	return client, server, nil
}

// harness wires a pool to a fake dialer and a frame sink.
type harness struct {
	t      *testing.T
	pool   *Pool
	dialer *fakeDialer
	frames chan *protocol.Envelope
}

func newHarness(t *testing.T, self Identity, mutate func(*PoolConfig)) *harness {
	t.Helper()
	h := &harness{t: t, dialer: newFakeDialer(), frames: make(chan *protocol.Envelope, 64)}
	cfg := PoolConfig{
		Self:    self,
		Dialer:  h.dialer,
		Backoff: func(int) time.Duration { return 0 },
		Handler: func(_ protocol.NodeID, env *protocol.Envelope) { h.frames <- env },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h.pool = NewPool(cfg)
	t.Cleanup(func() { h.pool.Close() })
	return h
}

// nextDial returns the peer end of the next socket the pool dialled.
func (h *harness) nextDial() net.Conn {
	h.t.Helper()
	select {
	case c := <-h.dialer.peers:
		return c
	case <-time.After(failsafe):
		h.t.Fatal("pool never dialled")
		return nil
	}
}

// acceptAs plays the peer for the pool's next dial and returns the peer's end.
func (h *harness) acceptAs(peer Identity, known ...protocol.NodeAddress) net.Conn {
	h.t.Helper()
	c := h.nextDial()
	if _, err := acceptHandshake(c, peer, known, far(), acceptAll); err != nil {
		h.t.Fatalf("peer-side accept: %v", err)
	}
	return c
}

// rejectAs plays a peer that rejects the pool's next dial with reason.
func (h *harness) rejectAs(peer Identity, reason string) {
	h.t.Helper()
	c := h.nextDial()
	_, err := acceptHandshake(c, peer, nil, far(), func(PeerInfo) (bool, string) { return false, reason })
	if !errors.Is(err, ErrHandshakeRejected) {
		h.t.Fatalf("peer-side reject: %v", err)
	}
	c.Close()
}

// inboundFrom hands the pool an inbound socket and plays the dialling peer on the
// other end. It returns the peer's end and the dial result.
func (h *harness) inboundFrom(peer Identity, known ...protocol.NodeAddress) (net.Conn, PeerInfo, error) {
	h.t.Helper()
	remote, local, err := loopbackPair()
	if err != nil {
		h.t.Fatal(err)
	}
	h.pool.Admit(local)
	info, err := dialHandshake(remote, peer, known, far())
	return remote, info, err
}

func (h *harness) event(kind PeerEventKind) PeerEvent {
	h.t.Helper()
	select {
	case ev, ok := <-h.pool.Events():
		if !ok {
			h.t.Fatalf("Events closed while waiting for %s", kind)
		}
		if ev.Kind != kind {
			h.t.Fatalf("got %s for %q, want %s", ev.Kind, ev.Peer.ID, kind)
		}
		return ev
	case <-time.After(failsafe):
		h.t.Fatalf("no %s event", kind)
		return PeerEvent{}
	}
}

// noEvent asserts that the event channel is empty right now. It is only valid
// after some other happens-before edge has proved the pool is quiescent.
func (h *harness) noEvent() {
	h.t.Helper()
	select {
	case ev, ok := <-h.pool.Events():
		if ok {
			h.t.Fatalf("unexpected event %s for %q", ev.Kind, ev.Peer.ID)
		}
	default:
	}
}

// readFrame decodes one frame from a peer's end with a failsafe deadline.
func readFrame(t *testing.T, c net.Conn) *protocol.Envelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(failsafe))
	env, err := protocol.NewDecoder(c).ReadFrame()
	if err != nil {
		t.Fatalf("reading frame from peer end: %v", err)
	}
	return env
}

// expectEOF proves the pool closed a peer's connection.
func expectEOF(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(failsafe))
	_, err := protocol.NewDecoder(c).ReadFrame()
	if err == nil {
		t.Fatal("expected the pool to close this connection, but a frame arrived")
	}
}

var (
	addrA = protocol.NodeAddress("node-a:7000")
	addrB = protocol.NodeAddress("node-b:7000")
)

// --- connect and events -----------------------------------------------------------------

func TestConnectIsIdempotentAndEmitsOnePeerUp(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrB)
	h.pool.Connect(addrB)
	h.pool.Connect(addrB)

	peer := h.acceptAs(idB, "node-c:7000")
	defer peer.Close()

	ev := h.event(PeerUp)
	if ev.Peer.ID != idB.ID || ev.Peer.Advertise != idB.Advertise || ev.Peer.Incarnation != idB.Incarnation {
		t.Errorf("PeerUp info = %+v, want %+v", ev.Peer, idB)
	}
	if len(ev.Peer.KnownPeers) != 1 || ev.Peer.KnownPeers[0] != "node-c:7000" {
		t.Errorf("PeerUp KnownPeers = %v", ev.Peer.KnownPeers)
	}
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d, want 1 (Connect must dedupe)", n)
	}
	h.noEvent()

	h.pool.mu.Lock()
	loops := len(h.pool.dials)
	h.pool.mu.Unlock()
	if loops != 1 {
		t.Errorf("dial loops = %d, want 1", loops)
	}
}

func TestPeerCloseEmitsPeerDownWithDisposition(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.MaxRedials = 1 })
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	h.event(PeerUp)

	// A length prefix and then a hang-up: the SIGKILL signature.
	if _, err := peer.Write([]byte{0, 0, 0, 50}); err != nil {
		t.Fatal(err)
	}
	peer.Close()

	ev := h.event(PeerDown)
	if ev.Peer.ID != idB.ID {
		t.Errorf("PeerDown for %q, want %q", ev.Peer.ID, idB.ID)
	}
	if ev.Disposition != DispositionPeerDied {
		t.Errorf("Disposition = %v, want peer-died", ev.Disposition)
	}
	if ev.Err == nil {
		t.Error("PeerDown.Err is nil")
	}
	if len(h.pool.Peers()) != 0 {
		t.Errorf("Peers() = %v after PeerDown", h.pool.Peers())
	}
}

func TestCleanCloseIsNotAFailure(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.MaxRedials = 1 })
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	h.event(PeerUp)
	peer.Close()

	ev := h.event(PeerDown)
	if ev.Disposition != DispositionCleanClose || ev.Disposition.IsFailure() {
		t.Errorf("Disposition = %v, want clean-close", ev.Disposition)
	}
}

// --- redial ---------------------------------------------------------------------------------

func TestRedialsAfterDropAndResetsAttempt(t *testing.T) {
	backoffs := make(chan int, 16)
	h := newHarness(t, idA, func(c *PoolConfig) {
		c.Backoff = func(attempt int) time.Duration { backoffs <- attempt; return 0 }
	})
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	h.event(PeerUp)
	peer.Close()
	h.event(PeerDown)

	// The loop must wait Backoff(0): the previous session succeeded.
	if got := <-backoffs; got != 0 {
		t.Errorf("first redial used Backoff(%d), want 0", got)
	}
	peer2 := h.acceptAs(idB)
	defer peer2.Close()
	h.event(PeerUp)
	if n := h.dialer.count(); n != 2 {
		t.Errorf("dial attempts = %d, want 2", n)
	}
}

func TestBackoffAttemptCountsConsecutiveFailures(t *testing.T) {
	backoffs := make(chan int, 16)
	h := newHarness(t, idA, func(c *PoolConfig) {
		c.Backoff = func(attempt int) time.Duration { backoffs <- attempt; return 0 }
		c.Dialer.(*fakeDialer).fail = func(n int) error {
			if n <= 2 {
				return errors.New("ECONNREFUSED")
			}
			return nil
		}
	})
	h.pool.Connect(addrB)
	if got := <-backoffs; got != 1 {
		t.Errorf("after first failure Backoff(%d), want 1", got)
	}
	if got := <-backoffs; got != 2 {
		t.Errorf("after second failure Backoff(%d), want 2", got)
	}
	peer := h.acceptAs(idB)
	defer peer.Close()
	h.event(PeerUp)
}

func TestMaxRedialsGivesUp(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) {
		c.MaxRedials = 2
		c.Dialer.(*fakeDialer).fail = func(int) error { return errors.New("ECONNREFUSED") }
	})
	h.pool.Connect(addrB)
	for i := 1; i <= 3; i++ {
		select {
		case n := <-h.dialer.dialed:
			if n != i {
				t.Fatalf("dial #%d, want #%d", n, i)
			}
		case <-time.After(failsafe):
			t.Fatalf("dial #%d never happened", i)
		}
	}
	// Close joins the loop; if it were still running it would keep dialling and
	// the count would exceed 1 + MaxRedials.
	h.pool.Close()
	if n := h.dialer.count(); n != 3 {
		t.Errorf("dial attempts = %d, want 3 (1 initial + 2 redials)", n)
	}
}

func TestExhaustedAddressCanBeConnectedAgain(t *testing.T) {
	released := make(chan struct{})
	h := newHarness(t, idA, func(c *PoolConfig) {
		c.MaxRedials = 1
		c.Dialer.(*fakeDialer).fail = func(n int) error {
			if n <= 2 {
				return errors.New("ECONNREFUSED")
			}
			return nil
		}
	})
	h.pool.Connect(addrB)
	<-h.dialer.dialed
	<-h.dialer.dialed
	// Wait for the loop to release its entry, then Connect again.
	go func() {
		for {
			h.pool.mu.Lock()
			_, live := h.pool.dials[addrB]
			h.pool.mu.Unlock()
			if !live {
				close(released)
				return
			}
			select {
			case <-h.pool.ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	<-released
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	defer peer.Close()
	h.event(PeerUp)
}

func TestForgetStopsRedialingAndClosesConnection(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Forget(addrB) // unknown address: no-op
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	defer peer.Close()
	h.event(PeerUp)

	h.pool.Forget(addrB)
	ev := h.event(PeerDown)
	if ev.Peer.ID != idB.ID {
		t.Errorf("PeerDown for %q", ev.Peer.ID)
	}
	expectEOF(t, peer)

	// Prove the loop is gone rather than redialling: Close joins it, and the
	// attempt count is unchanged.
	h.pool.Close()
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d after Forget, want 1", n)
	}
}

func TestForgetDuringBackoffStopsTheLoop(t *testing.T) {
	entered := make(chan struct{}, 1)
	h := newHarness(t, idA, func(c *PoolConfig) {
		c.Backoff = func(int) time.Duration { entered <- struct{}{}; return time.Hour }
		c.Dialer.(*fakeDialer).fail = func(int) error { return errors.New("ECONNREFUSED") }
	})
	h.pool.Connect(addrB)
	<-entered
	h.pool.Forget(addrB)
	h.pool.Close() // would hang for an hour if the timer were not interruptible
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d, want 1", n)
	}
}

func TestSelfConnectIsTerminal(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrA)
	c := h.nextDial()
	_, err := acceptHandshake(c, idA, nil, far(), acceptAll)
	if !errors.Is(err, ErrSelfConnect) {
		t.Fatalf("peer-side error = %v", err)
	}
	c.Close()
	h.pool.Close()
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d, want 1 (self-connect must not redial)", n)
	}
	h.noEvent()
}

func TestCloseInterruptsDialInProgress(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.Dialer.(*fakeDialer).blockUntilCtx = true })
	h.pool.Connect(addrB)
	<-h.dialer.dialed
	h.pool.Close() // returns only if the dial's context was cancelled
}

func TestCloseInterruptsOutboundHandshake(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.HandshakeTimeout = time.Hour })
	h.pool.Connect(addrB)
	peer := h.nextDial()
	defer peer.Close()
	// Read the HELLO so the pool is parked waiting for an ACK that never comes.
	readFrame(t, peer)
	h.pool.Close() // must not wait out the hour
}

// --- inbound ------------------------------------------------------------------------------------

func TestAdmitRegistersInboundPeer(t *testing.T) {
	h := newHarness(t, idA, nil)
	remote, info, err := h.inboundFrom(idB, "node-c:7000")
	if err != nil {
		t.Fatalf("inbound dial: %v", err)
	}
	defer remote.Close()
	if info.ID != idA.ID {
		t.Errorf("peer learned %q, want %q", info.ID, idA.ID)
	}
	ev := h.event(PeerUp)
	if ev.Peer.ID != idB.ID {
		t.Errorf("PeerUp for %q, want %q", ev.Peer.ID, idB.ID)
	}
	known := h.pool.Known()
	if len(known) != 2 || known[0] != addrB || known[1] != "node-c:7000" {
		t.Errorf("Known() = %v, want [%s node-c:7000] sorted", known, addrB)
	}

	remote.Close()
	h.event(PeerDown)
}

func TestAdmitRejectsGarbageWithoutEvents(t *testing.T) {
	h := newHarness(t, idA, nil)
	remote, local, err := loopbackPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	h.pool.Admit(local)
	if _, err := remote.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	expectEOF(t, remote)
	h.pool.Close()
	h.noEvent()
}

func TestCloseInterruptsInboundHandshake(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.HandshakeTimeout = time.Hour })
	remote, local, err := loopbackPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	h.pool.Admit(local) // the peer never sends HELLO
	h.pool.Close()      // must not wait out the hour
}

func TestAdmitAfterCloseClosesTheSocket(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Close()
	remote, local, err := loopbackPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	h.pool.Admit(local)
	expectEOF(t, remote)
	h.pool.Connect(addrB) // no-op after Close
	if n := h.dialer.count(); n != 0 {
		t.Errorf("Connect after Close dialled %d times", n)
	}
}

// --- Transport methods -------------------------------------------------------------------------

func TestSendAndBroadcast(t *testing.T) {
	h := newHarness(t, idA, nil)
	if h.pool.Self() != idA.ID {
		t.Errorf("Self() = %q", h.pool.Self())
	}
	env := mustEnv(t, protocol.TypePing)
	if err := h.pool.Send(context.Background(), idB.ID, env); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("Send to unknown peer: %v, want ErrUnknownPeer", err)
	}
	if err := h.pool.Send(context.Background(), idB.ID, nil); err == nil || errors.Is(err, ErrUnknownPeer) {
		t.Errorf("Send(nil) = %v, want a nil-envelope error", err)
	}
	if n := h.pool.Broadcast(context.Background(), env); n != 0 {
		t.Errorf("Broadcast to nobody = %d", n)
	}
	if n := h.pool.Broadcast(context.Background(), nil); n != 0 {
		t.Errorf("Broadcast(nil) = %d", n)
	}

	idZ := Identity{ID: "node-z", Advertise: "node-z:7000"}
	h.pool.Connect(addrB)
	peerB := h.acceptAs(idB)
	defer peerB.Close()
	h.event(PeerUp)
	peerZ, _, err := h.inboundFrom(idZ)
	if err != nil {
		t.Fatal(err)
	}
	defer peerZ.Close()
	h.event(PeerUp)

	peers := h.pool.Peers()
	if len(peers) != 2 || peers[0].ID != idB.ID || peers[1].ID != idZ.ID {
		t.Errorf("Peers() = %+v, want [node-b node-z]", peers)
	}

	if err := h.pool.Send(context.Background(), idZ.ID, env); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := readFrame(t, peerZ); got.ID != env.ID {
		t.Errorf("peer z received %s/%s, want %s", got.Type, got.ID, env.ID)
	}

	bc := mustEnv(t, protocol.TypeHeartbeat)
	if n := h.pool.Broadcast(context.Background(), bc); n != 2 {
		t.Errorf("Broadcast = %d, want 2", n)
	}
	for _, c := range []net.Conn{peerB, peerZ} {
		if got := readFrame(t, c); got.ID != bc.ID {
			t.Errorf("broadcast frame id = %s, want %s", got.ID, bc.ID)
		}
	}

	// Inbound frames reach the Handler with the peer already resolved.
	in := mustEnv(t, protocol.TypePong)
	if err := protocol.NewEncoder(peerB).WriteEnvelope(in); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-h.frames:
		if got.ID != in.ID {
			t.Errorf("handler got %s, want %s", got.ID, in.ID)
		}
	case <-time.After(failsafe):
		t.Fatal("handler never received the frame")
	}
}

func TestBroadcastCountsOnlyQueuedPeers(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.Conn.DataDepth = 1 })
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	defer peer.Close()
	h.event(PeerUp)

	// Close the pool's side of the connection out from under it: the Conn is
	// done, so Send fails, so the broadcast counts zero even though Peers() may
	// still briefly list it.
	h.pool.mu.Lock()
	c := h.pool.peers[idB.ID].conn
	h.pool.mu.Unlock()
	c.Close()
	<-c.Done()
	if n := h.pool.Broadcast(context.Background(), mustEnv(t, protocol.TypeHeartbeat)); n != 0 {
		t.Errorf("Broadcast to a closed conn = %d, want 0", n)
	}
	h.event(PeerDown)
}

// --- tie-break ------------------------------------------------------------------------------------

// We are the higher ID. The peer (lower) dials us while our own dial to it is
// up: the inbound connection, initiated by the lower ID, must win silently.
func TestTieBreakInboundFromLowerIDReplacesOurOutbound(t *testing.T) {
	h := newHarness(t, idB, nil)
	h.pool.Connect(addrA)
	outbound := h.acceptAs(idA)
	defer outbound.Close()
	h.event(PeerUp)

	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatalf("lower-ID peer's dial was rejected: %v", err)
	}
	defer inbound.Close()

	// Our old outbound is closed; the inbound now carries traffic.
	expectEOF(t, outbound)
	env := mustEnv(t, protocol.TypePing)
	if err := h.pool.Send(context.Background(), idA.ID, env); err != nil {
		t.Fatalf("Send after swap: %v", err)
	}
	if got := readFrame(t, inbound); got.ID != env.ID {
		t.Errorf("frame arrived on the wrong connection")
	}
	h.noEvent() // no PeerDown, no second PeerUp

	// The dial loop is parked on the inbound connection, not redialling. When
	// the inbound drops, exactly one PeerDown, then a redial.
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d, want 1", n)
	}
	inbound.Close()
	h.event(PeerDown)
	peer2 := h.acceptAs(idA)
	defer peer2.Close()
	h.event(PeerUp)
}

// We are the lower ID with an outbound up. An inbound from the higher ID must be
// rejected at the handshake with a reason, and nothing changes.
func TestTieBreakInboundFromHigherIDIsRejected(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrB)
	outbound := h.acceptAs(idB)
	defer outbound.Close()
	h.event(PeerUp)

	inbound, info, err := h.inboundFrom(idB)
	defer inbound.Close()
	if !errors.Is(err, ErrHandshakeRejected) {
		t.Fatalf("higher-ID peer's dial = %v, want ErrHandshakeRejected", err)
	}
	if !strings.Contains(err.Error(), "lower node id") {
		t.Errorf("rejection reason %q does not explain the tie-break", err)
	}
	if info.ID != idA.ID {
		t.Errorf("rejected peer learned %q, want %q", info.ID, idA.ID)
	}
	h.noEvent()
	if p := h.pool.Peers(); len(p) != 1 || p[0].ID != idB.ID {
		t.Errorf("Peers() = %+v", p)
	}
	// The outbound still works.
	env := mustEnv(t, protocol.TypePing)
	if err := h.pool.Send(context.Background(), idB.ID, env); err != nil {
		t.Fatal(err)
	}
	readFrame(t, outbound)
}

// We are the lower ID and the inbound from the higher ID landed first. When our
// own dial completes it must supersede the inbound silently.
func TestTieBreakOurOutboundReplacesInboundFromHigherID(t *testing.T) {
	h := newHarness(t, idA, nil)
	inbound, _, err := h.inboundFrom(idB)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)

	h.pool.Connect(addrB)
	outbound := h.acceptAs(idB)
	defer outbound.Close()

	expectEOF(t, inbound)
	env := mustEnv(t, protocol.TypePing)
	if err := h.pool.Send(context.Background(), idB.ID, env); err != nil {
		t.Fatal(err)
	}
	if got := readFrame(t, outbound); got.ID != env.ID {
		t.Error("frame arrived on the wrong connection")
	}
	h.noEvent()

	outbound.Close()
	h.event(PeerDown)
}

// We are the higher ID and the inbound from the lower ID landed first. Our own
// dial completes, loses, is closed, and the loop parks on the inbound.
func TestTieBreakOurOutboundLosesToExistingInbound(t *testing.T) {
	h := newHarness(t, idB, nil)
	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)

	h.pool.Connect(addrA)
	outbound := h.acceptAs(idA) // the peer accepts; we discard on our side
	defer outbound.Close()
	expectEOF(t, outbound)
	h.noEvent()

	inbound.Close()
	h.event(PeerDown)
	// The loop wakes and redials.
	peer2 := h.acceptAs(idA)
	defer peer2.Close()
	h.event(PeerUp)
}

// We are the higher ID, the inbound is up, and the peer rejects our dial with
// the tie-break reason. That rejection is not a failure: the loop parks on the
// inbound connection instead of entering the backoff loop.
func TestDialRejectedAsDuplicateParksOnInbound(t *testing.T) {
	h := newHarness(t, idB, nil)
	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)

	h.pool.Connect(addrA)
	h.rejectAs(idA, "already connected")

	// Forget is the observable: a loop parked on the inbound closes it on
	// cancellation, while a loop in the failure path never touches it. This is
	// deterministic whichever side of the check Forget lands on, because the
	// inbound is still registered when the check runs.
	h.pool.Forget(addrA)
	ev := h.event(PeerDown)
	if ev.Peer.ID != idA.ID {
		t.Errorf("PeerDown for %q", ev.Peer.ID)
	}
	expectEOF(t, inbound)
	h.pool.Close()
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d, want 1 (a tie-break rejection must not redial)", n)
	}
}

func TestNewerIncarnationReplacesRegardlessOfTieBreak(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrB)
	old := h.acceptAs(idB)
	defer old.Close()
	h.event(PeerUp)

	restarted := Identity{ID: idB.ID, Advertise: idB.Advertise, Incarnation: idB.Incarnation + 1}
	inbound, _, err := h.inboundFrom(restarted)
	if err != nil {
		t.Fatalf("restarted peer rejected: %v", err)
	}
	defer inbound.Close()
	expectEOF(t, old)
	h.noEvent()
	if p := h.pool.Peers(); len(p) != 1 || p[0].Incarnation != restarted.Incarnation {
		t.Errorf("Peers() = %+v, want the new incarnation", p)
	}
}

func TestOlderIncarnationIsRejected(t *testing.T) {
	h := newHarness(t, idB, nil)
	h.pool.Connect(addrA)
	cur := h.acceptAs(idA)
	defer cur.Close()
	h.event(PeerUp)

	stale := Identity{ID: idA.ID, Advertise: idA.Advertise, Incarnation: idA.Incarnation - 1}
	inbound, _, err := h.inboundFrom(stale)
	defer inbound.Close()
	if !errors.Is(err, ErrHandshakeRejected) {
		t.Fatalf("stale incarnation = %v, want rejection", err)
	}
	h.noEvent()
}

func TestForgetClosesInboundConnectionTheLoopIsParkedOn(t *testing.T) {
	h := newHarness(t, idB, nil)
	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)
	h.pool.Connect(addrA)
	outbound := h.acceptAs(idA)
	defer outbound.Close()
	expectEOF(t, outbound)

	h.pool.Forget(addrA)
	h.event(PeerDown)
	expectEOF(t, inbound)
}

// --- Close --------------------------------------------------------------------------------------------

func TestCloseEmitsPeerDownForEveryPeerThenClosesEvents(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrB)
	peerB := h.acceptAs(idB)
	defer peerB.Close()
	h.event(PeerUp)
	peerZ, _, err := h.inboundFrom(Identity{ID: "node-z"})
	if err != nil {
		t.Fatal(err)
	}
	defer peerZ.Close()
	h.event(PeerUp)

	if err := h.pool.Close(); err != nil {
		t.Fatal(err)
	}
	downs := map[protocol.NodeID]bool{}
	for ev := range h.pool.Events() {
		if ev.Kind != PeerDown {
			t.Errorf("unexpected %s after Close", ev.Kind)
		}
		downs[ev.Peer.ID] = true
	}
	if !downs[idB.ID] || !downs["node-z"] {
		t.Errorf("PeerDown set = %v, want both peers", downs)
	}
	if err := h.pool.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	expectEOF(t, peerB)
	expectEOF(t, peerZ)
}

// A consumer that has stopped reading must not deadlock Close: once closing,
// a full buffer is skipped rather than waited on.
func TestCloseDoesNotDeadlockOnAFullEventBuffer(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) { c.EventBuffer = 1 })
	h.pool.Connect(addrB)
	peerB := h.acceptAs(idB)
	defer peerB.Close()
	h.event(PeerUp)
	peerZ, _, err := h.inboundFrom(Identity{ID: "node-z"})
	if err != nil {
		t.Fatal(err)
	}
	defer peerZ.Close()
	h.event(PeerUp)
	// Buffer is now empty with capacity 1; two PeerDowns will be produced and
	// nobody reads. Close must still return.
	h.pool.Close()
	n := 0
	for range h.pool.Events() {
		n++
	}
	if n != 1 {
		t.Errorf("drained %d events, want exactly the buffered 1", n)
	}
}

// --- defaults -----------------------------------------------------------------------------------------

func TestDefaultBackoff(t *testing.T) {
	mid := defaultBackoff(func() float64 { return 0.5 })
	lo := defaultBackoff(func() float64 { return 0 })
	hi := defaultBackoff(func() float64 { return 1 })

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{-1, 200 * time.Millisecond},
		{0, 200 * time.Millisecond},
		{1, 400 * time.Millisecond},
		{3, 1600 * time.Millisecond},
		{5, 6400 * time.Millisecond},
		{6, 10 * time.Second},
		{40, 10 * time.Second},
		{1 << 30, 10 * time.Second},
	}
	for _, c := range cases {
		if got := mid(c.attempt); got != c.want {
			t.Errorf("Backoff(%d) = %v, want %v", c.attempt, got, c.want)
		}
		if got := lo(c.attempt); got != c.want*3/4 {
			t.Errorf("Backoff(%d) at -25%% = %v, want %v", c.attempt, got, c.want*3/4)
		}
		if got := hi(c.attempt); got != c.want*5/4 {
			t.Errorf("Backoff(%d) at +25%% = %v, want %v", c.attempt, got, c.want*5/4)
		}
	}
}

func TestPoolConfigDefaults(t *testing.T) {
	cfg := PoolConfig{}.withDefaults()
	if cfg.Dialer == nil || cfg.Backoff == nil || cfg.Rand == nil || cfg.Now == nil || cfg.Conn.Now == nil {
		t.Errorf("defaults missing: %+v", cfg)
	}
	if cfg.HandshakeTimeout != 3*time.Second || cfg.EventBuffer != 64 {
		t.Errorf("defaults wrong: %+v", cfg)
	}
	if d := cfg.Backoff(0); d < 150*time.Millisecond || d > 250*time.Millisecond {
		t.Errorf("default Backoff(0) = %v, outside 200ms +-25%%", d)
	}
	if _, ok := cfg.Dialer.(*net.Dialer); !ok {
		t.Errorf("default Dialer is %T", cfg.Dialer)
	}
	for _, k := range []PeerEventKind{PeerUp, PeerDown, 0} {
		if k.String() == "" {
			t.Errorf("empty String for %d", k)
		}
	}
}

func TestKnownExcludesSelfAndEmpty(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB, addrA, "", "node-c:7000")
	defer peer.Close()
	h.event(PeerUp)
	known := h.pool.Known()
	want := []protocol.NodeAddress{addrB, "node-c:7000"}
	if len(known) != len(want) || known[0] != want[0] || known[1] != want[1] {
		t.Errorf("Known() = %v, want %v", known, want)
	}
	// A returned PeerInfo must not alias pool state.
	p := h.pool.Peers()
	p[0].KnownPeers[0] = "mutated"
	if h.pool.Peers()[0].KnownPeers[0] == "mutated" {
		t.Error("Peers() aliases the pool's KnownPeers slice")
	}
}

// Close can land between a handshake completing and its registration. The
// registration must then discard the socket rather than resurrect a closed pool.
func TestRegisterAfterCloseDiscardsTheSocket(t *testing.T) {
	h := newHarness(t, idA, nil)
	h.pool.Close()
	remote, local, err := loopbackPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if e := h.pool.register(PeerInfo{ID: idB.ID}, local, true); e != nil {
		t.Error("register after Close returned an entry")
	}
	expectEOF(t, remote)
	if len(h.pool.Peers()) != 0 {
		t.Error("a peer was registered on a closed pool")
	}
}

// --- claims: deferring PeerDown while a replacement handshake is in flight ----------

// The connection dies while our own dial to the same peer is mid-handshake. The
// PeerDown must wait for the dial: if it succeeds, the peer never went away.
func TestDeathDuringOwnDialIsSilentWhenTheDialSucceeds(t *testing.T) {
	h := newHarness(t, idB, nil)
	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)

	old := entryFor(t, h.pool, idA.ID)
	h.pool.Connect(addrA)
	peer := h.nextDial() // the dial claim on addrA is held from before this point
	defer peer.Close()

	inbound.Close()
	// Wait for the pool to process the death under the dial claim, so the
	// handshake below completes against a parked entry rather than racing it
	// (the other order is the ordinary tie-break path, covered elsewhere).
	waitState(t, h.pool, "PeerDown parked", func() bool { return h.pool.deferred[idA.ID] == old })
	if _, err := acceptHandshake(peer, idA, nil, far(), acceptAll); err != nil {
		t.Fatal(err)
	}
	waitState(t, h.pool, "replacement registered", func() bool {
		cur := h.pool.peers[idA.ID]
		return cur != nil && cur != old
	})
	env := mustEnv(t, protocol.TypePing)
	if err := h.pool.Send(context.Background(), idA.ID, env); err != nil {
		t.Fatalf("Send after silent replacement: %v", err)
	}
	if got := readFrame(t, peer); got.ID != env.ID {
		t.Error("frame did not arrive on the replacement connection")
	}
	h.noEvent()
}

func TestDeathDuringOwnDialEmitsPeerDownWhenTheDialFails(t *testing.T) {
	h := newHarness(t, idB, func(c *PoolConfig) { c.MaxRedials = 1 })
	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)

	old := entryFor(t, h.pool, idA.ID)
	h.pool.Connect(addrA)
	peer := h.nextDial()
	inbound.Close()
	waitState(t, h.pool, "PeerDown parked", func() bool { return h.pool.deferred[idA.ID] == old })
	// The peer hangs up mid-handshake: the claim resolves as a failure and the
	// deferred PeerDown is emitted -- exactly once.
	peer.Close()
	ev := h.event(PeerDown)
	if ev.Peer.ID != idA.ID {
		t.Errorf("PeerDown for %q", ev.Peer.ID)
	}
	if len(h.pool.Peers()) != 0 {
		t.Errorf("dead entry still listed: %+v", h.pool.Peers())
	}
	// The loop redials once more (MaxRedials=1) and then gives up; drain it.
	h.nextDial().Close()
	h.pool.Close()
	h.noEvent()
}

// White-box: an inbound claim that is released without a registration must
// surface the deferred PeerDown, and a claim that is released while the
// connection is still alive changes nothing.
func TestInboundClaimReleaseResolvesDeadEntry(t *testing.T) {
	h := newHarness(t, idB, nil)
	h.pool.Connect(addrA)
	peer := h.acceptAs(idA)
	defer peer.Close()
	h.event(PeerUp)

	// Take a claim as admitPolicy would for an approved HELLO.
	if ok, _ := h.pool.admitPolicy(PeerInfo{ID: idA.ID, Incarnation: idA.Incarnation}); !ok {
		t.Fatal("policy rejected a lower-ID inbound")
	}
	// Releasing it while the connection is healthy is a no-op.
	h.pool.releaseClaim(idA.ID)
	h.noEvent()
	if len(h.pool.Peers()) != 1 {
		t.Fatal("healthy entry was removed by a claim release")
	}

	// Claim again, kill the connection: the entry leaves the map but its
	// PeerDown is parked until the claim resolves.
	h.pool.admitPolicy(PeerInfo{ID: idA.ID, Incarnation: idA.Incarnation})
	e := entryFor(t, h.pool, idA.ID)
	peer.Close()
	waitState(t, h.pool, "PeerDown parked", func() bool { return h.pool.deferred[idA.ID] == e })
	select {
	case <-e.gone:
		t.Fatal("a parked entry was retired before its fate was decided")
	default:
	}
	h.noEvent()
	if len(h.pool.Peers()) != 0 {
		t.Fatal("a dead entry is still listed")
	}
	if err := h.pool.Send(context.Background(), idA.ID, mustEnv(t, protocol.TypePing)); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("Send to a parked peer = %v, want ErrUnknownPeer", err)
	}

	h.pool.releaseClaim(idA.ID)
	ev := h.event(PeerDown)
	if ev.Peer.ID != idA.ID || ev.Disposition != DispositionCleanClose {
		t.Errorf("deferred PeerDown = %+v", ev)
	}
	select {
	case <-e.gone:
	case <-time.After(failsafe):
		t.Fatal("resolved entry was never retired")
	}
	// The redial loop was parked on e.gone throughout; only now does it dial.
	// Count while this handshake is still held open, before anything can
	// trigger a further attempt.
	held := h.nextDial()
	if n := h.dialer.count(); n != 2 {
		t.Errorf("dial attempts = %d, want 2 (no redial while parked)", n)
	}
	held.Close()
}

// While a PeerDown is parked, a replacement that arrives from the higher ID is
// accepted (there is nothing to tie-break against) and the parked event is
// discarded without a PeerUp: the consumer never learned the peer was gone.
func TestParkedPeerDownIsDiscardedByReplacement(t *testing.T) {
	h := newHarness(t, idA, nil) // we are the lower ID
	h.pool.Connect(addrB)
	peer := h.acceptAs(idB)
	defer peer.Close()
	h.event(PeerUp)
	e := entryFor(t, h.pool, idB.ID)

	// Hold a claim so the death is parked, then kill the connection. Taken
	// directly: as the lower ID our own policy would not approve an inbound
	// from node-b while this connection is alive, which is the point.
	h.pool.mu.Lock()
	h.pool.claims[idB.ID]++
	h.pool.mu.Unlock()
	peer.Close()
	waitState(t, h.pool, "PeerDown parked", func() bool { return h.pool.deferred[idB.ID] == e })

	inbound, _, err := h.inboundFrom(idB)
	if err != nil {
		t.Fatalf("inbound while a PeerDown is parked was rejected: %v", err)
	}
	defer inbound.Close()
	// The ACK precedes registration; wait for the replacement to be installed.
	waitState(t, h.pool, "replacement registered", func() bool {
		cur := h.pool.peers[idB.ID]
		return cur != nil && cur != e
	})
	h.pool.releaseClaim(idB.ID)
	h.noEvent()
	// Traffic proves the replacement is live.
	env := mustEnv(t, protocol.TypePing)
	if err := h.pool.Send(context.Background(), idB.ID, env); err != nil {
		t.Fatal(err)
	}
	readFrame(t, inbound)
	// The redial loop was parked on the old entry throughout and is now parked
	// on the replacement: no redial happened.
	if n := h.dialer.count(); n != 1 {
		t.Errorf("dial attempts = %d, want 1", n)
	}
}

// entryFor returns the registered entry for id, failing the test if none exists.
func entryFor(t *testing.T, p *Pool, id protocol.NodeID) *peerEntry {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.peers[id]
	if e == nil {
		t.Fatalf("%s has no entry for %s", p.Self(), id)
	}
	return e
}

// waitState blocks until pred, evaluated with the pool's mutex held, is true. It
// rides the pool's Cond, so it wakes on the exact mutation that satisfies it
// rather than polling; the failsafe only fires when something is broken.
func waitState(t *testing.T, p *Pool, what string, pred func() bool) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		p.mu.Lock()
		for !pred() {
			p.changed.Wait()
		}
		p.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(failsafe):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitQuiescent blocks until no handshake is in flight and no PeerDown is parked.
func waitQuiescent(t *testing.T, p *Pool) {
	t.Helper()
	waitState(t, p, "quiescence", func() bool {
		return len(p.claims) == 0 && len(p.dialing) == 0 && len(p.deferred) == 0 && len(p.pending) == 0
	})
}

// --- ordering, re-queue and claim-by-ID fixes -----------------------------------------

// A PeerDown that has been decided but is still blocked in emission must be
// delivered before a PeerUp for the same peer from a fresh handshake. The
// interleaving is forced by filling a one-slot event buffer so the Down blocks,
// then completing a new inbound handshake for the same ID.
func TestPeerDownStrictlyPrecedesPeerUpForSamePeer(t *testing.T) {
	h := newHarness(t, idB, func(c *PoolConfig) { c.EventBuffer = 1 })
	first, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	h.event(PeerUp)
	old := entryFor(t, h.pool, idA.ID)

	// Occupy the only buffer slot with an unrelated event.
	z, _, err := h.inboundFrom(Identity{ID: "node-z"})
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()

	// Kill a's connection: its PeerDown is decided but cannot be delivered.
	first.Close()
	waitState(t, h.pool, "PeerDown in flight", func() bool { return h.pool.downInFlight[idA.ID] == old })

	// A new inbound from a passes the policy (no entry, nothing parked) and
	// registers. Its PeerUp must queue behind the blocked PeerDown.
	second, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waitState(t, h.pool, "replacement registered", func() bool {
		cur := h.pool.peers[idA.ID]
		return cur != nil && cur != old
	})

	want := []struct {
		kind PeerEventKind
		id   protocol.NodeID
	}{{PeerUp, "node-z"}, {PeerDown, idA.ID}, {PeerUp, idA.ID}}
	for _, w := range want {
		ev := h.event(w.kind)
		if ev.Peer.ID != w.id {
			t.Fatalf("got %s for %q, want %s for %q", ev.Kind, ev.Peer.ID, w.kind, w.id)
		}
	}
}

// Control frames queued on a superseded connection reach the winner; data
// frames do not, and their loss is not counted against the winner.
func TestSupersededControlFramesAreRequeuedOnWinner(t *testing.T) {
	h := newHarness(t, idA, nil) // lower ID: our own dial supersedes an inbound
	// The inbound runs over a net.Pipe so the writer parks in Write and every
	// later frame stays queued until the swap.
	remote, local := net.Pipe()
	defer remote.Close()
	h.pool.Admit(local)
	if _, err := dialHandshake(remote, idB, nil, far()); err != nil {
		t.Fatal(err)
	}
	h.event(PeerUp)

	blocker := mustEnv(t, protocol.TypeHeartbeat) // parks the writer
	data := mustEnv(t, protocol.TypeTelemetry)
	ctrl := mustEnv(t, protocol.TypePing)
	for _, env := range []*protocol.Envelope{blocker, data, ctrl} {
		if err := h.pool.Send(context.Background(), idB.ID, env); err != nil {
			t.Fatal(err)
		}
	}

	h.pool.Connect(addrB)
	winner := h.acceptAs(idB)
	defer winner.Close()
	h.noEvent()

	// Read until the control frame arrives on the winner. The blocker may or
	// may not precede it (it was mid-write, and may have been dequeued before
	// the swap); the data frame must never appear.
	for {
		got := readFrame(t, winner)
		if got.Type == protocol.TypeTelemetry {
			t.Fatal("a data frame was re-queued across the swap")
		}
		if got.ID == ctrl.ID {
			break
		}
	}
	cur := entryFor(t, h.pool, idB.ID)
	if n := cur.conn.Dropped(); n != 0 {
		t.Errorf("winner's Dropped = %d, want 0", n)
	}
}

// A peer dialled by one name and advertising another: the dial claim must still
// cover the peer's entry, by the ID the HELLO_ACK revealed on the previous dial.
func TestDialClaimCoversPeerDialledByADifferentName(t *testing.T) {
	const dialled = protocol.NodeAddress("seed-host:7000") // != idA.Advertise
	h := newHarness(t, idB, nil)
	h.pool.Connect(dialled)
	peer := h.acceptAs(idA)
	h.event(PeerUp)
	waitState(t, h.pool, "address learned against ID", func() bool {
		_, ok := h.pool.addrOf[idA.ID][dialled]
		return ok
	})
	peer.Close()
	h.event(PeerDown)

	// The loop redials; hold the handshake open (pre-ACK) so the address claim
	// is the only thing covering node-a.
	held := h.nextDial()
	defer held.Close()

	// Meanwhile node-a connects to us and that connection dies.
	inbound, _, err := h.inboundFrom(idA)
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	h.event(PeerUp)
	old := entryFor(t, h.pool, idA.ID)
	inbound.Close()
	waitState(t, h.pool, "PeerDown parked under the address claim", func() bool { return h.pool.deferred[idA.ID] == old })

	// Completing the held dial replaces the parked entry silently.
	if _, err := acceptHandshake(held, idA, nil, far(), acceptAll); err != nil {
		t.Fatal(err)
	}
	waitQuiescent(t, h.pool)
	h.noEvent()
	if p := h.pool.Peers(); len(p) != 1 || p[0].ID != idA.ID {
		t.Errorf("Peers() = %+v", p)
	}
}

func TestStaleIncarnationRejectionNamesTheIncarnations(t *testing.T) {
	h := newHarness(t, idB, nil)
	h.pool.Connect(addrA)
	cur := h.acceptAs(idA)
	defer cur.Close()
	h.event(PeerUp)
	stale := Identity{ID: idA.ID, Advertise: idA.Advertise, Incarnation: idA.Incarnation - 1}
	inbound, _, err := h.inboundFrom(stale)
	defer inbound.Close()
	if err == nil || !strings.Contains(err.Error(), "incarnation") {
		t.Errorf("stale rejection reason = %v, want one naming incarnations", err)
	}
}

// Close must leave no goroutine behind. The count is sampled once the runtime
// has had a chance to reap the exited goroutines; the loop is bounded by a
// deadline and yields via Gosched rather than sleeping.
func TestCloseLeaksNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	h := newHarness(t, idA, nil)
	h.pool.Connect(addrB)
	peerB := h.acceptAs(idB)
	defer peerB.Close()
	h.event(PeerUp)
	peerZ, _, err := h.inboundFrom(Identity{ID: "node-z"})
	if err != nil {
		t.Fatal(err)
	}
	defer peerZ.Close()
	h.event(PeerUp)
	h.pool.Close()
	for range h.pool.Events() {
	}

	deadline := time.Now().Add(failsafe)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines after Close = %d, before = %d", after, before)
	}
}
