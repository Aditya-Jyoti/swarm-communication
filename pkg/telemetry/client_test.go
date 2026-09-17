package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

const testWait = 10 * time.Second

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeCC is a hand-rolled Control Center: a real TCP listener that answers the
// HELLO itself and then speaks raw frames, so the tests see exactly what the
// client puts on the wire.
type fakeCC struct {
	t  *testing.T
	ln net.Listener
	id protocol.NodeID
}

func newFakeCC(t *testing.T, id protocol.NodeID) *fakeCC {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &fakeCC{t: t, ln: ln, id: id}
}

func (f *fakeCC) addr() protocol.NodeAddress { return protocol.NodeAddress(f.ln.Addr().String()) }

// ccLink is one accepted, handshaken connection.
type ccLink struct {
	t     *testing.T
	raw   net.Conn
	enc   *protocol.Encoder
	dec   *protocol.Decoder
	hello protocol.HelloPayload
	from  protocol.NodeID
}

// accept waits for the client, reads its HELLO and accepts it.
func (f *fakeCC) accept() *ccLink {
	f.t.Helper()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := f.ln.Accept()
		ch <- res{c, err}
	}()
	var raw net.Conn
	select {
	case r := <-ch:
		if r.err != nil {
			f.t.Fatalf("accept: %v", r.err)
		}
		raw = r.c
	case <-time.After(testWait):
		f.t.Fatal("client never dialled")
	}
	f.t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(testWait))
	l := &ccLink{t: f.t, raw: raw, enc: protocol.NewEncoder(raw), dec: protocol.NewDecoder(raw)}
	env, err := l.dec.ReadFrame()
	if err != nil || env.Type != protocol.TypeHello {
		f.t.Fatalf("want HELLO, got %v / %v", env, err)
	}
	l.from = env.From
	if l.hello, err = protocol.PayloadOf[protocol.HelloPayload](env); err != nil {
		f.t.Fatal(err)
	}
	ack, _ := protocol.NewReply(env, protocol.TypeHelloAck, f.id, protocol.HelloAckPayload{Accepted: true})
	if err := l.enc.WriteEnvelope(ack); err != nil {
		f.t.Fatal(err)
	}
	return l
}

// next reads frames until one of type want arrives, skipping telemetry noise.
func (l *ccLink) next(want protocol.MessageType) *protocol.Envelope {
	l.t.Helper()
	for {
		env, err := l.dec.ReadFrame()
		if err != nil {
			l.t.Fatalf("waiting for %s: %v", want, err)
		}
		if env.Type == want {
			return env
		}
	}
}

func (l *ccLink) send(typ protocol.MessageType, payload any) *protocol.Envelope {
	l.t.Helper()
	env, err := protocol.NewEnvelope(typ, ControlCenterID, l.from, payload)
	if err != nil {
		l.t.Fatal(err)
	}
	if err := l.enc.WriteEnvelope(env); err != nil {
		l.t.Fatal(err)
	}
	return env
}

type harness struct {
	client *Client
	clock  *cluster.FakeClock
	tasks  chan protocol.TaskPayload
	chaos  chan Chaos
	cancel context.CancelFunc
	done   chan error
}

func startClient(t *testing.T, addr protocol.NodeAddress, mutate func(*Config)) *harness {
	t.Helper()
	h := &harness{
		clock: cluster.NewFakeClock(time.Unix(1_000, 0)),
		tasks: make(chan protocol.TaskPayload, 8),
		chaos: make(chan Chaos, 8),
		done:  make(chan error, 1),
	}
	seq := uint64(0)
	cfg := Config{
		Self: network.Identity{ID: "node-1", Advertise: "node-1:7000", Incarnation: 99},
		Addr: addr,
		Snapshot: func() protocol.TelemetryPayload {
			seq++ // Run goroutine only
			return protocol.TelemetryPayload{Node: "node-1", Role: "leader", Term: seq}
		},
		SubmitTask: func(_ context.Context, p protocol.TaskPayload) error {
			h.tasks <- p
			return nil
		},
		OnChaos: func(c Chaos) { h.chaos <- c },
		Clock:   h.clock,
		Backoff: func(int) time.Duration { return time.Millisecond },
		Logger:  quietLog,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.client = c
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- c.Run(ctx) }()
	t.Cleanup(func() { h.stop(t) })
	return h
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Run did not return after cancel")
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
	}
	panic("unreachable")
}

func TestNewValidates(t *testing.T) {
	snap := func() protocol.TelemetryPayload { return protocol.TelemetryPayload{} }
	bad := []Config{
		{Addr: "cc:7000", Snapshot: snap},
		{Self: network.Identity{ID: ControlCenterID}, Addr: "cc:7000", Snapshot: snap},
		{Self: network.Identity{ID: "n"}, Snapshot: snap},
		{Self: network.Identity{ID: "n"}, Addr: "cc:7000"},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: New accepted %+v", i, cfg)
		}
	}
	c, err := New(Config{Self: network.Identity{ID: "n", Incarnation: 5}, Addr: "cc:7000", Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.Interval != DefaultInterval || c.cfg.SubmitTimeout != DefaultSubmitTimeout ||
		c.cfg.ResultBuffer != DefaultResultBuffer || c.cfg.Self.Incarnation != 0 || c.cfg.Logger == nil {
		t.Fatalf("defaults not applied: %+v", c.cfg)
	}
}

func TestClientSendsTelemetryOnConnectAndEveryTick(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	h := startClient(t, cc.addr(), nil)
	link := cc.accept()

	if link.from != "node-1" || link.hello.Advertise != "node-1:7000" || link.hello.Incarnation != 0 {
		t.Fatalf("HELLO from %q = %+v", link.from, link.hello)
	}
	first := link.next(protocol.TypeTelemetry)
	if first.To != ControlCenterID {
		t.Fatalf("telemetry addressed to %q", first.To)
	}
	p, _ := protocol.PayloadOf[protocol.TelemetryPayload](first)
	if p.Term != 1 {
		t.Fatalf("first sample term = %d, want 1 (sent on connect)", p.Term)
	}
	h.clock.Advance(DefaultInterval)
	p, _ = protocol.PayloadOf[protocol.TelemetryPayload](link.next(protocol.TypeTelemetry))
	if p.Term != 2 {
		t.Fatalf("second sample term = %d, want 2 (sent on tick)", p.Term)
	}
	if !h.client.Connected() {
		t.Fatal("Connected() = false with a live link")
	}
}

func TestClientDispatchesTaskChaosAndPing(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	h := startClient(t, cc.addr(), nil)
	link := cc.accept()
	link.next(protocol.TypeTelemetry)

	link.send(protocol.TypeTask, protocol.TaskPayload{TaskID: "t-1", Kind: "echo", Body: json.RawMessage(`{"a":1}`)})
	task := recv(t, h.tasks, "task")
	if task.TaskID != "t-1" || task.Kind != "echo" || string(task.Body) != `{"a":1}` {
		t.Fatalf("task = %+v", task)
	}

	link.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "delay", DelayMS: 300})
	if c := recv(t, h.chaos, "chaos delay"); c != (Chaos{Action: ChaosDelay, Delay: 300 * time.Millisecond}) {
		t.Fatalf("chaos = %+v", c)
	}
	// Out of range and malformed instructions never reach the handler; the
	// clear that follows must be the next thing it sees.
	link.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "delay", DelayMS: 6000})
	link.send(protocol.TypeChaos, "not an object")
	link.send(protocol.TypeTask, 42)
	link.send(protocol.TypeHeartbeat, protocol.HeartbeatPayload{}) // ignored type
	link.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "clear"})
	if c := recv(t, h.chaos, "chaos clear"); c != (Chaos{Action: ChaosClear}) {
		t.Fatalf("chaos = %+v, want clear", c)
	}

	ping := link.send(protocol.TypePing, protocol.PingPayload{Nonce: 77, Seq: 3})
	pong := link.next(protocol.TypePong)
	pp, _ := protocol.PayloadOf[protocol.PongPayload](pong)
	if pong.ID != ping.ID || pp.Nonce != 77 || pp.Seq != 3 {
		t.Fatalf("pong %+v / %+v does not echo ping", pong, pp)
	}
	link.send(protocol.TypePing, "garbage") // no reply, no crash
	select {
	case <-h.tasks:
		t.Fatal("malformed TASK was submitted")
	default:
	}
}

func TestClientReportsSubmitFailure(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	startClient(t, cc.addr(), func(c *Config) {
		c.SubmitTask = func(context.Context, protocol.TaskPayload) error { return cluster.ErrNoLeader }
	})
	link := cc.accept()
	link.send(protocol.TypeTask, protocol.TaskPayload{TaskID: "t-9", Kind: "echo"})
	r, _ := protocol.PayloadOf[protocol.TaskResultPayload](link.next(protocol.TypeTaskResult))
	if r.TaskID != "t-9" || r.OK || r.Worker != "node-1" || r.Output == "" {
		t.Fatalf("failure result = %+v", r)
	}
}

func TestClientWithoutSubmitTaskDropsTasks(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	h := startClient(t, cc.addr(), func(c *Config) { c.SubmitTask = nil })
	link := cc.accept()
	link.send(protocol.TypeTask, protocol.TaskPayload{TaskID: "t-1", Kind: "echo"})
	// A chaos frame sent after the task proves the task was processed (and
	// dropped) without wedging the loop.
	link.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "kill"})
	if c := recv(t, h.chaos, "kill"); c.Action != ChaosKill {
		t.Fatalf("chaos = %+v", c)
	}
}

func TestClientBuffersResultsUntilConnected(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	// Queue before Run even starts: the node may finish a task before the CC
	// link is up.
	c, err := New(Config{
		Self:     network.Identity{ID: "node-1"},
		Addr:     cc.addr(),
		Snapshot: func() protocol.TelemetryPayload { return protocol.TelemetryPayload{Node: "node-1"} },
		Logger:   quietLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SendResult(protocol.TaskResultPayload{TaskID: "t-1", OK: true, Output: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	link := cc.accept()
	r, _ := protocol.PayloadOf[protocol.TaskResultPayload](link.next(protocol.TypeTaskResult))
	if r.TaskID != "t-1" || !r.OK {
		t.Fatalf("result = %+v", r)
	}
	// And a result produced while connected goes straight out.
	c.SendResult(protocol.TaskResultPayload{TaskID: "t-2"})
	r, _ = protocol.PayloadOf[protocol.TaskResultPayload](link.next(protocol.TypeTaskResult))
	if r.TaskID != "t-2" {
		t.Fatalf("result = %+v", r)
	}
	cancel()
	if err := recv(t, done, "Run exit"); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run = %v", err)
	}
}

func TestClientPendingBufferIsBounded(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	// Never accept: the link stays down, so everything goes to pending.
	c, err := New(Config{
		Self:         network.Identity{ID: "node-1"},
		Addr:         cc.addr(),
		Snapshot:     func() protocol.TelemetryPayload { return protocol.TelemetryPayload{} },
		ResultBuffer: 2,
		Logger:       quietLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Channel capacity is 2 as well: the third is shed at SendResult.
	for i := 0; i < 3; i++ {
		c.SendResult(protocol.TaskResultPayload{TaskID: "t"})
	}
	if got := c.DroppedResults(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	// Refill while Run drains into pending; pending (cap 2) then sheds oldest.
	deadline := time.Now().Add(testWait)
	for c.DroppedResults() < 3 && time.Now().Before(deadline) {
		c.SendResult(protocol.TaskResultPayload{TaskID: "more"})
	}
	cancel()
	if err := recv(t, done, "Run exit"); err != nil {
		t.Fatal(err)
	}
	if c.DroppedResults() < 3 {
		t.Fatalf("dropped = %d, want >= 3", c.DroppedResults())
	}
}

func TestClientRedialsAfterDrop(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	h := startClient(t, cc.addr(), nil)
	first := cc.accept()
	first.next(protocol.TypeTelemetry)
	_ = first.raw.Close()

	second := cc.accept()
	second.next(protocol.TypeTelemetry)
	second.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "clear"})
	recv(t, h.chaos, "chaos over the redialled link")
}

func TestClientStopsOnWrongPeer(t *testing.T) {
	cc := newFakeCC(t, "node-7")
	c, err := New(Config{
		Self:     network.Identity{ID: "node-1"},
		Addr:     cc.addr(),
		Snapshot: func() protocol.TelemetryPayload { return protocol.TelemetryPayload{} },
		Logger:   quietLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	cc.accept()
	if err := recv(t, done, "Run exit"); !errors.Is(err, ErrNotControlCenter) {
		t.Fatalf("Run = %v, want ErrNotControlCenter", err)
	}
}

func TestClientIgnoresFramesFromOtherPeers(t *testing.T) {
	c, err := New(Config{
		Self:     network.Identity{ID: "node-1"},
		Addr:     "cc:1",
		Snapshot: func() protocol.TelemetryPayload { return protocol.TelemetryPayload{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := protocol.NewEnvelope(protocol.TypeChaos, "node-2", "node-1", protocol.ChaosPayload{Action: "kill"})
	c.handle("node-2", env) // must not block: nothing is reading inbound
	if len(c.inbound) != 0 {
		t.Fatal("a frame from a non-CC peer was queued")
	}
}
