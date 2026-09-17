package network

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// node is a pool plus a listener on loopback, for end-to-end tests over real
// sockets with the real dialer.
type node struct {
	id     Identity
	pool   *Pool
	ln     *Listener
	frames chan *protocol.Envelope
}

func startNode(t *testing.T, id protocol.NodeID, mutate func(*PoolConfig)) *node {
	t.Helper()
	n := &node{frames: make(chan *protocol.Envelope, 64)}
	cfg := PoolConfig{
		Self:    Identity{ID: id, Incarnation: 1},
		Backoff: func(int) time.Duration { return 0 },
		Handler: func(_ protocol.NodeID, env *protocol.Envelope) { n.frames <- env },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	// Bind first so the advertised address is known before the pool speaks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Self.Advertise = protocol.NodeAddress(ln.Addr().String())
	n.id = cfg.Self
	n.pool = NewPool(cfg)
	n.ln = serve(ln, n.pool)
	t.Cleanup(func() {
		n.ln.Close()
		n.pool.Close()
	})
	return n
}

func (n *node) event(t *testing.T, kind PeerEventKind) PeerEvent {
	t.Helper()
	select {
	case ev, ok := <-n.pool.Events():
		if !ok {
			t.Fatalf("%s: Events closed", n.id.ID)
		}
		if ev.Kind != kind {
			t.Fatalf("%s: got %s for %q, want %s", n.id.ID, ev.Kind, ev.Peer.ID, kind)
		}
		return ev
	case <-time.After(failsafe):
		t.Fatalf("%s: no %s event", n.id.ID, kind)
		return PeerEvent{}
	}
}

func TestListenBindsAndReportsAddr(t *testing.T) {
	pool := NewPool(PoolConfig{Self: idA})
	defer pool.Close()
	l, err := Listen(ListenerConfig{Addr: "127.0.0.1:0"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr().(*net.TCPAddr).Port == 0 {
		t.Error("Addr() reports port 0 after bind")
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Binding an address that cannot be listened on is reported with context.
	if _, err := Listen(ListenerConfig{Addr: "127.0.0.1:1"}, pool); err == nil {
		t.Error("Listen on a privileged port succeeded")
	} else if !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EADDRINUSE) {
		t.Logf("Listen error (accepted as long as it is non-nil): %v", err)
	}
}

func TestTwoNodesExchangeFramesEndToEnd(t *testing.T) {
	a := startNode(t, "node-a", nil)
	b := startNode(t, "node-b", nil)

	a.pool.Connect(b.id.Advertise)
	upA := a.event(t, PeerUp)
	upB := b.event(t, PeerUp)
	if upA.Peer.ID != "node-b" || upB.Peer.ID != "node-a" {
		t.Fatalf("PeerUp pair = %q/%q", upA.Peer.ID, upB.Peer.ID)
	}
	if upB.Peer.Advertise != a.id.Advertise {
		t.Errorf("b learned advertise %q, want %q", upB.Peer.Advertise, a.id.Advertise)
	}

	ping := mustEnv(t, protocol.TypePing)
	if err := a.pool.Send(context.Background(), "node-b", ping); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-b.frames:
		if got.ID != ping.ID {
			t.Errorf("b received %s, want %s", got.ID, ping.ID)
		}
	case <-time.After(failsafe):
		t.Fatal("b never received the frame")
	}

	pong := mustEnv(t, protocol.TypePong)
	if n := b.pool.Broadcast(context.Background(), pong); n != 1 {
		t.Errorf("Broadcast = %d, want 1", n)
	}
	select {
	case got := <-a.frames:
		if got.ID != pong.ID {
			t.Errorf("a received %s, want %s", got.ID, pong.ID)
		}
	case <-time.After(failsafe):
		t.Fatal("a never received the frame")
	}

	// Closing b's pool is a graceful departure: a sees a clean close.
	b.ln.Close()
	b.pool.Close()
	down := a.event(t, PeerDown)
	if down.Disposition != DispositionCleanClose {
		t.Errorf("a saw %v, want clean-close", down.Disposition)
	}
}

// gatedDialer holds every dial until the test releases it, so the order in which
// two nodes' dials complete is chosen by the test rather than by the scheduler.
type gatedDialer struct {
	release chan struct{}
	inner   net.Dialer
}

func (g *gatedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.inner.DialContext(ctx, network, addr)
}

// Both nodes dial each other. The connection initiated by the lower ID (a) must
// be the one that survives on both sides, whichever dial lands first, and each
// side must see exactly one PeerUp.
func TestSimultaneousDialLowerInitiatorWinsWhenItLandsFirst(t *testing.T) {
	gate := &gatedDialer{release: make(chan struct{})}
	a := startNode(t, "node-a", nil)
	b := startNode(t, "node-b", func(c *PoolConfig) { c.Dialer = gate })

	a.pool.Connect(b.id.Advertise)
	b.pool.Connect(a.id.Advertise)
	a.event(t, PeerUp)
	b.event(t, PeerUp)

	// a's connection is registered on both sides. Now let b's dial go: a must
	// reject it at the handshake, and b's loop must park on the inbound.
	close(gate.release)
	waitQuiescent(t, a.pool)
	waitQuiescent(t, b.pool)
	assertSettledOnOneConnection(t, a, b)
}

func TestSimultaneousDialLowerInitiatorWinsWhenItLandsSecond(t *testing.T) {
	gate := &gatedDialer{release: make(chan struct{})}
	a := startNode(t, "node-a", func(c *PoolConfig) { c.Dialer = gate })
	b := startNode(t, "node-b", nil)

	a.pool.Connect(b.id.Advertise)
	b.pool.Connect(a.id.Advertise)
	a.event(t, PeerUp)
	b.event(t, PeerUp)

	// b's connection is registered on both sides. Capture both entries, release
	// a's dial, and wait until both pools have replaced them and have nothing in
	// flight: that is the proof the swap completed on both sides.
	oldAtA := entryFor(t, a.pool, "node-b")
	oldAtB := entryFor(t, b.pool, "node-a")
	close(gate.release)
	waitQuiescent(t, a.pool)
	waitQuiescent(t, b.pool)
	if entryFor(t, a.pool, "node-b") == oldAtA || entryFor(t, b.pool, "node-a") == oldAtB {
		t.Fatal("a superseded connection is still registered")
	}
	assertSettledOnOneConnection(t, a, b)
}

// assertSettledOnOneConnection proves two pools share exactly one connection:
// traffic flows both ways, each lists one peer, and neither has a stray event.
func assertSettledOnOneConnection(t *testing.T, a, b *node) {
	t.Helper()
	ping := mustEnv(t, protocol.TypePing)
	if err := a.pool.Send(context.Background(), "node-b", ping); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.frames:
	case <-time.After(failsafe):
		t.Fatal("b never received a's frame")
	}
	if err := b.pool.Send(context.Background(), "node-a", ping); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.frames:
	case <-time.After(failsafe):
		t.Fatal("a never received b's frame")
	}
	for _, n := range []*node{a, b} {
		select {
		case ev := <-n.pool.Events():
			t.Errorf("%s: extra event %s for %q", n.id.ID, ev.Kind, ev.Peer.ID)
		default:
		}
		if peers := n.pool.Peers(); len(peers) != 1 {
			t.Errorf("%s: Peers() = %+v, want exactly one", n.id.ID, peers)
		}
	}
}

func TestGarbageClientIsRejectedAndListenerKeepsAccepting(t *testing.T) {
	a := startNode(t, "node-a", nil)

	raw, err := net.Dial("tcp", string(a.id.Advertise))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Write([]byte("not a frame at all\n")); err != nil {
		t.Fatal(err)
	}
	expectEOF(t, raw)

	b := startNode(t, "node-b", nil)
	b.pool.Connect(a.id.Advertise)
	a.event(t, PeerUp)
	b.event(t, PeerUp)
}

func TestListenerCloseStopsAcceptLoop(t *testing.T) {
	a := startNode(t, "node-a", nil)
	addr := string(a.id.Advertise)
	if err := a.ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Error("dial succeeded after the listener closed")
	}
}

// --- accept-loop error handling -----------------------------------------------

// scriptedListener returns a scripted sequence of Accept results, then blocks
// until closed.
type scriptedListener struct {
	script []acceptStep
	i      int
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	// accepts counts calls so a test can prove the loop retried.
	accepts chan int
}

type acceptStep struct {
	conn net.Conn
	err  error
}

func newScriptedListener(steps ...acceptStep) *scriptedListener {
	return &scriptedListener{script: steps, closed: make(chan struct{}), accepts: make(chan int, 64)}
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	s.mu.Lock()
	s.i++
	n := s.i
	var step *acceptStep
	if n <= len(s.script) {
		step = &s.script[n-1]
	}
	s.mu.Unlock()
	s.accepts <- n
	if step != nil {
		return step.conn, step.err
	}
	<-s.closed
	return nil, net.ErrClosed
}

func (s *scriptedListener) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *scriptedListener) Addr() net.Addr { return fakeAddr{} }

type tempErr struct{}

func (tempErr) Error() string   { return "temporary" }
func (tempErr) Temporary() bool { return true }

func TestAcceptLoopBacksOffOnTemporaryErrors(t *testing.T) {
	remote, local, err := loopbackPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()

	pool := NewPool(PoolConfig{Self: idA})
	defer pool.Close()
	ln := newScriptedListener(
		acceptStep{err: &net.OpError{Op: "accept", Err: syscall.EMFILE}},
		acceptStep{err: &net.OpError{Op: "accept", Err: syscall.ECONNABORTED}},
		acceptStep{err: tempErr{}},
		acceptStep{conn: local},
	)
	l := serve(ln, pool)
	defer l.Close()

	for i := 1; i <= 4; i++ {
		select {
		case n := <-ln.accepts:
			if n != i {
				t.Fatalf("accept #%d, want #%d", n, i)
			}
		case <-time.After(failsafe):
			t.Fatalf("accept #%d never happened: loop gave up on a temporary error", i)
		}
	}
	// The accepted socket reached the pool: it is in a handshake, so a HELLO
	// from the far end gets an answer.
	go func() {
		_, _ = dialHandshake(remote, idB, nil, far())
	}()
	select {
	case ev := <-pool.Events():
		if ev.Kind != PeerUp || ev.Peer.ID != idB.ID {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(failsafe):
		t.Fatal("the accepted socket never reached the pool")
	}
}

func TestAcceptLoopReturnsOnPermanentError(t *testing.T) {
	pool := NewPool(PoolConfig{Self: idA})
	defer pool.Close()
	ln := newScriptedListener(acceptStep{err: errors.New("EBADF")})
	l := serve(ln, pool)
	if err := l.Err(); err != nil {
		t.Errorf("Err() before the loop exited = %v", err)
	}
	<-ln.accepts
	// Close would also return here, because it unblocks a loop parked in a
	// backoff or on the next Accept. Done is the proof the loop exited on its own.
	select {
	case <-l.Done():
	case <-time.After(failsafe):
		t.Fatal("accept loop did not return on a permanent error")
	}
	err := l.Err()
	if err == nil || !strings.Contains(err.Error(), "EBADF") || !strings.Contains(err.Error(), string(idA.ID)) {
		t.Errorf("Err() = %v, want the accept error naming the node", err)
	}
	l.Close()
	if l.Err() != err {
		t.Error("Err() changed after Close")
	}
}

func TestListenerDoneAfterCloseHasNoError(t *testing.T) {
	pool := NewPool(PoolConfig{Self: idA})
	defer pool.Close()
	l, err := Listen(ListenerConfig{Addr: "127.0.0.1:0"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	select {
	case <-l.Done():
	case <-time.After(failsafe):
		t.Fatal("Done not closed after Close")
	}
	if err := l.Err(); err != nil {
		t.Errorf("Err() after a deliberate Close = %v", err)
	}
}

func TestAcceptLoopCloseDuringBackoff(t *testing.T) {
	pool := NewPool(PoolConfig{Self: idA})
	defer pool.Close()
	ln := newScriptedListener(
		acceptStep{err: &net.OpError{Op: "accept", Err: syscall.EMFILE}},
	)
	l := serve(ln, pool)
	<-ln.accepts
	// The loop is now in (or about to enter) a 5ms backoff, or already on the
	// blocking second Accept. Either way, Close must return promptly.
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestIsTemporaryAcceptError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("plain"), false},
		{net.ErrClosed, false},
		{os.ErrDeadlineExceeded, true}, // a timeout is a net.Error with Temporary()==true
		{syscall.EMFILE, true},
		{&net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.ENFILE)}, true},
		{&net.OpError{Op: "accept", Err: syscall.ECONNABORTED}, true},
		{&net.OpError{Op: "accept", Err: syscall.EAGAIN}, true},
		{&net.OpError{Op: "accept", Err: syscall.EBADF}, false},
		{tempErr{}, true},
	}
	for _, c := range cases {
		if got := isTemporaryAcceptError(c.err); got != c.want {
			t.Errorf("isTemporaryAcceptError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
