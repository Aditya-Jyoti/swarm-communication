package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// --- test doubles -----------------------------------------------------------
//
// There is no time.Sleep in this file. Backpressure is made observable by gating
// the socket's Write and waiting on a channel for the writer goroutine to park,
// which is a happens-before edge rather than a guess about scheduling.

// gateConn is a net.Conn whose Write blocks until released, and which signals each
// time a Write is entered. That signal is what lets a test know the writer goroutine
// is parked inside the socket and the send queues are therefore stable.
type gateConn struct {
	entered chan struct{}
	release chan struct{}
	dead    chan struct{}
	once    sync.Once

	// written carries a copy of every frame that reached the socket, in order, so
	// a test can assert what the writer actually chose to send and when.
	written chan []byte

	mu     sync.Mutex
	writes int
}

func newGateConn() *gateConn {
	return &gateConn{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
		dead:    make(chan struct{}),
		written: make(chan []byte, 64),
	}
}

func (g *gateConn) Read(p []byte) (int, error) {
	<-g.dead
	return 0, io.EOF
}

func (g *gateConn) Write(p []byte) (int, error) {
	g.mu.Lock()
	g.writes++
	g.mu.Unlock()

	select {
	case g.entered <- struct{}{}:
	default:
	}

	select {
	case <-g.release:
		frame := append([]byte(nil), p...)
		select {
		case g.written <- frame:
		default:
		}
		return len(p), nil
	case <-g.dead:
		return 0, errors.New("gateConn: closed")
	}
}

// nextType decodes the type of the next frame the writer put on the wire.
func (g *gateConn) nextType(t *testing.T) protocol.MessageType {
	t.Helper()
	select {
	case frame := <-g.written:
		env, err := protocol.NewDecoder(bytes.NewReader(frame)).ReadFrame()
		if err != nil {
			t.Fatalf("decoding a written frame: %v", err)
		}
		return env.Type
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not drain the queue")
		return ""
	}
}

func (g *gateConn) Close() error {
	g.once.Do(func() { close(g.dead) })
	return nil
}

func (g *gateConn) writeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writes
}

func (g *gateConn) LocalAddr() net.Addr                { return fakeAddr{} }
func (g *gateConn) RemoteAddr() net.Addr               { return fakeAddr{} }
func (g *gateConn) SetDeadline(t time.Time) error      { return nil }
func (g *gateConn) SetReadDeadline(t time.Time) error  { return nil }
func (g *gateConn) SetWriteDeadline(t time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// recordingConn wraps a real net.Conn and records every deadline set on it, so a
// test can assert the read-deadline discipline without depending on a deadline
// actually firing.
type recordingConn struct {
	net.Conn
	mu    sync.Mutex
	reads []time.Time
}

func (r *recordingConn) SetReadDeadline(t time.Time) error {
	r.mu.Lock()
	r.reads = append(r.reads, t)
	r.mu.Unlock()
	return r.Conn.SetReadDeadline(t)
}

func (r *recordingConn) readDeadlines() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.reads...)
}

func mustEnv(t *testing.T, typ protocol.MessageType) *protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(typ, "node-1", "node-2", nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

// --- error classification ---------------------------------------------------

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Disposition
	}{
		{"clean close at a frame boundary", io.EOF, DispositionCleanClose},
		{"died mid-frame", io.ErrUnexpectedEOF, DispositionPeerDied},
		{"wrapped mid-frame death", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), DispositionPeerDied},
		{"our own read deadline", os.ErrDeadlineExceeded, DispositionTimeout},
		{"oversize frame", protocol.ErrFrameTooLarge, DispositionProtocolViolation},
		{"malformed frame", protocol.ErrMalformedFrame, DispositionProtocolViolation},
		{"zero-length frame", protocol.ErrZeroLengthFrame, DispositionProtocolViolation},
		{"unsupported version", protocol.ErrUnsupportedVersion, DispositionProtocolViolation},
		{"unrelated error", errors.New("boom"), DispositionOther},
		{"nil", nil, DispositionOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// Only silence and mid-frame death are evidence about the peer's health. A clean
// close means it left on purpose, and a protocol violation means it is speaking
// nonsense -- neither should feed a missed-beat counter.
func TestOnlySilenceAndDeathCountAsFailure(t *testing.T) {
	failures := map[Disposition]bool{
		DispositionCleanClose:        false,
		DispositionPeerDied:          true,
		DispositionProtocolViolation: false,
		DispositionTimeout:           true,
		DispositionOther:             false,
	}
	for d, want := range failures {
		if got := d.IsFailure(); got != want {
			t.Errorf("%v.IsFailure() = %v, want %v", d, got, want)
		}
	}
}

// --- deadline discipline ----------------------------------------------------

// The read deadline must be refreshed before EVERY ReadFrame, computed from the
// injected clock. A peer that was SIGKILLed leaves a socket that never errors, so
// this deadline is the only thing that makes failure detection possible.
func TestReadDeadlineIsSetBeforeEveryFrame(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	// The clock is fixed so the expected deadline is exact, but it must be anchored
	// to real time: net.Pipe honours deadlines, so a hardcoded past date would expire
	// the read before the first frame ever arrived.
	fixed := time.Now()
	rec := &recordingConn{Conn: server}

	received := make(chan struct{}, 4)
	c := NewConn("node-2", rec, func(protocol.NodeID, *protocol.Envelope) {
		received <- struct{}{}
	}, ConnConfig{
		IdleTimeout: 30 * time.Second,
		Now:         func() time.Time { return fixed },
	})
	defer c.Close()

	// Owner: this test. The goroutine writes two frames and returns; net.Pipe is
	// synchronous so each Write completes only once the reader has consumed it.
	enc := protocol.NewEncoder(client)
	go func() {
		for i := 0; i < 2; i++ {
			if err := enc.WriteEnvelope(mustEnvelopeNoT()); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-received:
		case <-c.Done():
			d, err := c.Disposition()
			t.Fatalf("connection closed before frame %d: %v (%v)", i, d, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d never arrived", i)
		}
	}

	want := fixed.Add(30 * time.Second)
	got := rec.readDeadlines()
	if len(got) < 2 {
		t.Fatalf("expected a deadline per frame, got %d", len(got))
	}
	for i, d := range got[:2] {
		if !d.Equal(want) {
			t.Errorf("read deadline %d = %v, want %v", i, d, want)
		}
	}
}

func mustEnvelopeNoT() *protocol.Envelope {
	env, err := protocol.NewEnvelope(protocol.TypePing, "node-1", "node-2", nil)
	if err != nil {
		panic(err)
	}
	return env
}

// --- round trip -------------------------------------------------------------

func TestConnRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	got := make(chan *protocol.Envelope, 1)
	c := NewConn("node-2", server, func(_ protocol.NodeID, env *protocol.Envelope) {
		got <- env
	}, ConnConfig{})
	defer c.Close()

	sent := mustEnv(t, protocol.TypeHeartbeat)
	enc := protocol.NewEncoder(client)
	go func() { _ = enc.WriteEnvelope(sent) }()

	select {
	case env := <-got:
		if env.Type != protocol.TypeHeartbeat {
			t.Errorf("Type = %v, want HEARTBEAT", env.Type)
		}
		if env.ID != sent.ID {
			t.Errorf("ID = %q, want %q", env.ID, sent.ID)
		}
	case <-c.Done():
		d, err := c.Disposition()
		t.Fatalf("connection closed before delivering: %v (%v)", d, err)
	}
}

// --- backpressure -----------------------------------------------------------

// A data-plane frame is shed when its queue is full rather than blocking the
// caller. Losing a telemetry sample is a gap in a graph; blocking the goroutine
// that produced it could stall the node.
func TestDataPlaneFramesAreDroppedWhenQueueIsFull(t *testing.T) {
	g := newGateConn()
	c := NewConn("node-2", g, nil, ConnConfig{DataDepth: 1, CtrlDepth: 1})
	defer c.Close()

	ctx := context.Background()

	// Frame 1 is picked up by the writer, which then parks inside Write. Waiting on
	// `entered` is what makes the rest of this test deterministic: from here the
	// writer consumes nothing more.
	if err := c.Send(ctx, mustEnv(t, protocol.TypeTelemetry)); err != nil {
		t.Fatalf("first send: %v", err)
	}
	<-g.entered

	// Frame 2 fills the depth-1 queue.
	if err := c.Send(ctx, mustEnv(t, protocol.TypeTelemetry)); err != nil {
		t.Fatalf("second send should fill the queue, not fail: %v", err)
	}

	// Frame 3 has nowhere to go and must be dropped immediately.
	err := c.Send(ctx, mustEnv(t, protocol.TypeTelemetry))
	if !errors.Is(err, ErrDataDropped) {
		t.Fatalf("want ErrDataDropped, got %v", err)
	}
	if n := c.Dropped(); n != 1 {
		t.Errorf("Dropped() = %d, want 1", n)
	}
}

// A control-plane frame is never silently dropped. It waits for space and reports
// ErrSendQueueFull, which tells the caller the frame never reached the socket --
// so the stream is intact and the peer is merely slow.
func TestControlPlaneSendReportsQueueFullRatherThanDropping(t *testing.T) {
	g := newGateConn()
	c := NewConn("node-2", g, nil, ConnConfig{
		CtrlDepth:       1,
		CtrlSendTimeout: 20 * time.Millisecond,
	})
	defer c.Close()

	ctx := context.Background()

	if err := c.Send(ctx, mustEnv(t, protocol.TypeHeartbeat)); err != nil {
		t.Fatalf("first send: %v", err)
	}
	<-g.entered

	if err := c.Send(ctx, mustEnv(t, protocol.TypeHeartbeat)); err != nil {
		t.Fatalf("second send should fill the queue: %v", err)
	}

	err := c.Send(ctx, mustEnv(t, protocol.TypeHeartbeat))
	if !errors.Is(err, ErrSendQueueFull) {
		t.Fatalf("want ErrSendQueueFull, got %v", err)
	}
	if n := c.Dropped(); n != 0 {
		t.Errorf("a control frame was counted as dropped: %d", n)
	}
}

// A caller's cancelled context must abort a blocked control send, so a shutting-down
// node is not held up by a peer that stopped reading.
func TestControlSendHonoursCallerContext(t *testing.T) {
	g := newGateConn()
	c := NewConn("node-2", g, nil, ConnConfig{
		CtrlDepth:       1,
		CtrlSendTimeout: time.Hour, // would hang without ctx
	})
	defer c.Close()

	if err := c.Send(context.Background(), mustEnv(t, protocol.TypeHeartbeat)); err != nil {
		t.Fatalf("first send: %v", err)
	}
	<-g.entered
	if err := c.Send(context.Background(), mustEnv(t, protocol.TypeHeartbeat)); err != nil {
		t.Fatalf("second send: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Send(ctx, mustEnv(t, protocol.TypeHeartbeat)); !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

// Control frames must not queue behind data frames. This is the head-of-line
// blocking hazard: a heartbeat delayed behind a large task payload can exceed the
// K-missed-beat threshold and get a healthy leader declared dead.
func TestControlFramesArePreferredOverData(t *testing.T) {
	g := newGateConn()
	c := NewConn("node-2", g, nil, ConnConfig{DataDepth: 8, CtrlDepth: 8})
	defer c.Close()

	ctx := context.Background()

	// Park the writer with a throwaway frame so the queues below stay stable.
	if err := c.Send(ctx, mustEnv(t, protocol.TypeTask)); err != nil {
		t.Fatalf("priming send: %v", err)
	}
	<-g.entered

	// Queue data first, then control. Priority, not arrival order, must decide.
	for i := 0; i < 4; i++ {
		if err := c.Send(ctx, mustEnv(t, protocol.TypeTelemetry)); err != nil {
			t.Fatalf("data send %d: %v", i, err)
		}
	}
	if err := c.Send(ctx, mustEnv(t, protocol.TypeElectionResult)); err != nil {
		t.Fatalf("control send: %v", err)
	}

	// Release the writer and read back the order it actually chose.
	close(g.release)

	// The priming TASK was already in flight before the queues were loaded.
	if got := g.nextType(t); got != protocol.TypeTask {
		t.Fatalf("first frame = %v, want the priming TASK", got)
	}

	// The control frame was queued LAST but must be written FIRST, ahead of the
	// four telemetry frames already waiting.
	if got := g.nextType(t); got != protocol.TypeElectionResult {
		t.Errorf("second frame = %v, want ELECTION_RESULT to overtake queued data", got)
	}
	for i := 0; i < 4; i++ {
		if got := g.nextType(t); got != protocol.TypeTelemetry {
			t.Errorf("frame %d = %v, want TELEMETRY", i+3, got)
		}
	}
}

// --- lifecycle --------------------------------------------------------------

func TestCloseIsIdempotentAndUnblocksEverything(t *testing.T) {
	_, server := net.Pipe()
	c := NewConn("node-2", server, nil, ConnConfig{})

	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Errorf("Close %d returned %v", i, err)
		}
	}

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed")
	}

	if err := c.Send(context.Background(), mustEnv(t, protocol.TypeHeartbeat)); !errors.Is(err, ErrConnClosed) {
		t.Errorf("Send after Close = %v, want ErrConnClosed", err)
	}
}

// A peer closing cleanly at a frame boundary must be recorded as a clean close, not
// as a death. Mistaking one for the other makes a graceful scale-down look like a
// failure and triggers a needless re-election.
func TestPeerCloseIsRecordedAsCleanClose(t *testing.T) {
	client, server := net.Pipe()

	got := make(chan struct{}, 1)
	c := NewConn("node-2", server, func(protocol.NodeID, *protocol.Envelope) {
		got <- struct{}{}
	}, ConnConfig{})
	defer c.Close()

	// Send one frame first so the reader is provably inside ReadFrame before the
	// peer goes away. Without this the test races the reader's first
	// SetReadDeadline call, and net.Pipe reports io.ErrClosedPipe from SetDeadline
	// when EITHER end is closed -- an artifact of the double, not of real sockets,
	// where a peer close surfaces as io.EOF on the next read.
	enc := protocol.NewEncoder(client)
	go func() { _ = enc.WriteEnvelope(mustEnvelopeNoT()) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("priming frame never arrived")
	}

	client.Close()

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not notice the peer closing")
	}

	d, _ := c.Disposition()
	if d != DispositionCleanClose {
		t.Errorf("Disposition = %v, want clean-close", d)
	}
	if d.IsFailure() {
		t.Error("a clean close must not count as a failure")
	}
}

// A peer that vanishes mid-frame is the SIGKILL signature and IS a failure signal.
func TestTruncatedFrameIsRecordedAsPeerDeath(t *testing.T) {
	client, server := net.Pipe()

	c := NewConn("node-2", server, nil, ConnConfig{})
	defer c.Close()

	// Owner: this test. Declares a 64-byte frame, supplies 4 bytes, then vanishes.
	go func() {
		_, _ = client.Write([]byte{0, 0, 0, 64})
		_, _ = client.Write([]byte(`{"v"`))
		client.Close()
	}()

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not notice the truncated frame")
	}

	d, err := c.Disposition()
	if d != DispositionPeerDied {
		t.Errorf("Disposition = %v (%v), want peer-died", d, err)
	}
	if !d.IsFailure() {
		t.Error("a mid-frame death must count as a failure")
	}
}

// Garbage on the wire ends the connection but is NOT a health signal: the peer is
// speaking nonsense, not dying, and redialing it in a tight loop would be a bug.
func TestProtocolViolationIsNotAHealthSignal(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	c := NewConn("node-2", server, nil, ConnConfig{})
	defer c.Close()

	go func() {
		body := []byte("this is not json at all")
		hdr := []byte{0, 0, 0, byte(len(body))}
		_, _ = client.Write(hdr)
		_, _ = client.Write(body)
	}()

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not reject the malformed frame")
	}

	d, _ := c.Disposition()
	if d != DispositionProtocolViolation {
		t.Errorf("Disposition = %v, want protocol-violation", d)
	}
	if d.IsFailure() {
		t.Error("a protocol violation must not increment a missed-beat counter")
	}
}

func TestSendNilIsRejected(t *testing.T) {
	_, server := net.Pipe()
	c := NewConn("node-2", server, nil, ConnConfig{})
	defer c.Close()

	if err := c.Send(context.Background(), nil); err == nil {
		t.Error("Send(nil) returned nil error")
	}
}

// Many goroutines share one connection by design -- the heartbeat ticker and
// whatever dispatches tasks do not coordinate. Sends must be safe under -race.
func TestConcurrentSends(t *testing.T) {
	g := newGateConn()
	close(g.release) // writes complete immediately
	c := NewConn("node-2", g, nil, ConnConfig{})
	defer c.Close()

	const senders, each = 8, 25
	var wg sync.WaitGroup
	wg.Add(senders)

	// Owner: this test. Each goroutine runs a bounded loop and returns; wg.Wait
	// below joins every one, so none can outlive the test.
	for i := 0; i < senders; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				typ := protocol.TypeHeartbeat
				if j%2 == 0 {
					typ = protocol.TypeTelemetry
				}
				env, err := protocol.NewEnvelope(typ, "node-1", "node-2", nil)
				if err != nil {
					t.Errorf("NewEnvelope: %v", err)
					return
				}
				// Drops are legitimate here; only a panic or a race is a failure.
				_ = c.Send(context.Background(), env)
			}
		}(i)
	}
	wg.Wait()
}
