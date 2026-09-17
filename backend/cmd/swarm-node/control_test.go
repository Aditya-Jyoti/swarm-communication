package main

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/controlcenter"
	"swarm-net/pkg/protocol"
	"swarm-net/pkg/telemetry"
)

// ccStub is a Control Center reduced to a TCP listener that answers one HELLO
// and then speaks raw frames, so the test sees exactly what the node sends.
type ccStub struct {
	t   *testing.T
	ln  net.Listener
	raw net.Conn
	enc *protocol.Encoder
	dec *protocol.Decoder
}

func newCCStub(t *testing.T) *ccStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return &ccStub{t: t, ln: ln}
}

func (c *ccStub) addr() protocol.NodeAddress { return protocol.NodeAddress(c.ln.Addr().String()) }

func (c *ccStub) accept() *protocol.Envelope {
	c.t.Helper()
	got := make(chan net.Conn, 1)
	go func() {
		raw, err := c.ln.Accept()
		if err != nil {
			close(got)
			return
		}
		got <- raw
	}()
	select {
	case raw, ok := <-got:
		if !ok {
			c.t.Fatal("accept failed")
		}
		c.raw = raw
	case <-time.After(testWait):
		c.t.Fatal("node never dialled the control center")
	}
	c.t.Cleanup(func() { _ = c.raw.Close() })
	_ = c.raw.SetDeadline(time.Now().Add(testWait))
	c.enc, c.dec = protocol.NewEncoder(c.raw), protocol.NewDecoder(c.raw)
	hello, err := c.dec.ReadFrame()
	if err != nil || hello.Type != protocol.TypeHello {
		c.t.Fatalf("want HELLO: %v %v", hello, err)
	}
	ack, _ := protocol.NewReply(hello, protocol.TypeHelloAck, telemetry.ControlCenterID, protocol.HelloAckPayload{Accepted: true})
	if err := c.enc.WriteEnvelope(ack); err != nil {
		c.t.Fatal(err)
	}
	return hello
}

func (c *ccStub) next(want protocol.MessageType, match func(*protocol.Envelope) bool) *protocol.Envelope {
	c.t.Helper()
	for {
		env, err := c.dec.ReadFrame()
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", want, err)
		}
		if env.Type == want && (match == nil || match(env)) {
			return env
		}
	}
}

func (c *ccStub) send(typ protocol.MessageType, to protocol.NodeID, payload any) {
	c.t.Helper()
	env, _ := protocol.NewEnvelope(typ, telemetry.ControlCenterID, to, payload)
	if err := c.enc.WriteEnvelope(env); err != nil {
		c.t.Fatal(err)
	}
}

// recorder captures the process-level side effects the app would otherwise
// perform on the real process.
type recorder struct {
	exits chan int
	delay chan time.Duration
}

func newRecorder() *recorder {
	return &recorder{exits: make(chan int, 1), delay: make(chan time.Duration, 4)}
}

func (r *recorder) install(a *app) {
	a.exit = func(code int) { r.exits <- code }
	a.setDelays = func(d time.Duration) { r.delay <- d }
}

func TestControlCenterWiring(t *testing.T) {
	cc := newCCStub(t)
	cfg := loopbackConfig("wired")
	cfg.ControlCenter = cc.addr()
	cfg.TelemetryInterval = 20 * time.Millisecond

	var logs syncWriter
	a, err := newApp(cfg, &logs)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if a.client == nil {
		t.Fatal("no client built despite SWARM_CONTROL_CENTER")
	}
	rec := newRecorder()
	rec.install(a)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()

	hello := cc.accept()
	if hello.From != "wired" {
		t.Fatalf("HELLO from %q", hello.From)
	}
	p, _ := protocol.PayloadOf[protocol.HelloPayload](hello)
	if p.Advertise != cfg.Advertise {
		t.Fatalf("HELLO advertise %q", p.Advertise)
	}

	// Telemetry reflects the node: alone, it elects itself.
	cc.next(protocol.TypeTelemetry, func(env *protocol.Envelope) bool {
		tp, err := protocol.PayloadOf[protocol.TelemetryPayload](env)
		return err == nil && tp.Node == "wired" && tp.Role == "leader" && tp.Leader == "wired"
	})

	// CHAOS delay and clear reach both delay setters (via the seam).
	cc.send(protocol.TypeChaos, "wired", protocol.ChaosPayload{Action: "delay", DelayMS: 300})
	if d := recvT(t, rec.delay); d != 300*time.Millisecond {
		t.Fatalf("delay = %v", d)
	}
	cc.send(protocol.TypeChaos, "wired", protocol.ChaosPayload{Action: "clear"})
	if d := recvT(t, rec.delay); d != 0 {
		t.Fatalf("clear = %v", d)
	}

	// TASK goes to node.SubmitTask. Whatever the node does with it -- run it
	// (Phase 4) or refuse it (the stub) -- a TASK_RESULT for that id comes back.
	cc.send(protocol.TypeTask, "wired", protocol.TaskPayload{TaskID: "t-1", Kind: "echo", Body: []byte(`"hi"`)})
	res := cc.next(protocol.TypeTaskResult, nil)
	rp, _ := protocol.PayloadOf[protocol.TaskResultPayload](res)
	if rp.TaskID != "t-1" {
		t.Fatalf("result = %+v", rp)
	}

	// CHAOS kill calls exit(1) and nothing else.
	cc.send(protocol.TypeChaos, "wired", protocol.ChaosPayload{Action: "kill"})
	if code := recvT(t, rec.exits); code != exitRuntime {
		t.Fatalf("exit code %d", code)
	}

	cancel()
	if err := recvT(t, done); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"chaos kill received", "chaos delay applied", "closing control center uplink", "shutdown complete"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

func TestNoControlCenterMeansNoClient(t *testing.T) {
	a, err := newApp(loopbackConfig("lonely"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a.client != nil {
		t.Fatal("client built without SWARM_CONTROL_CENTER")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMisconfiguredControlCenterDoesNotStopNode(t *testing.T) {
	// Point the uplink at a mesh node: the client gives up, the node runs on.
	peer, err := newApp(loopbackConfig("peer"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loopbackConfig("confused")
	cfg.ControlCenter = protocol.NodeAddress(peer.ln.Addr().String())
	cfg.TelemetryInterval = 20 * time.Millisecond
	var logs syncWriter
	a, err := newApp(cfg, &logs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- peer.run(ctx) }()
	go func() { done <- a.run(ctx) }()

	waitFor(t, "uplink to give up", func() bool { return strings.Contains(logs.String(), "control center uplink stopped") })
	waitFor(t, "node still electing", func() bool { return a.node.Status().Leader != "" })
	cancel()
	for i := 0; i < 2; i++ {
		if err := recvT(t, done); err != nil {
			t.Errorf("run: %v", err)
		}
	}
}

func TestDefaultSetDelaysIsWired(t *testing.T) {
	a, err := newApp(loopbackConfig("delays"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The production seam must reach the real prober and node without
	// panicking; what they do with it is their packages' tests' business.
	a.applyChaos(telemetry.Chaos{Action: telemetry.ChaosDelay, Delay: 10 * time.Millisecond})
	a.applyChaos(telemetry.Chaos{Action: telemetry.ChaosClear})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNewAppRejectsBadClientConfig(t *testing.T) {
	// Load forbids this ID implicitly (the CC owns it); newApp must still fail
	// closed and release what it built.
	cfg := loopbackConfig(string(telemetry.ControlCenterID))
	cfg.ControlCenter = "cc:7000"
	if _, err := newApp(cfg, io.Discard); err == nil {
		t.Fatal("newApp accepted the control center's own node id")
	}
}

func recvT[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatal("timed out")
	}
	panic("unreachable")
}

// syncWriter is a goroutine-safe log sink.
type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestUplinkIdleTimeoutNeverUndercutsKeepAlive(t *testing.T) {
	for _, tc := range []struct{ mesh, want time.Duration }{
		{0, uplinkIdleFloor},
		{5 * time.Second, uplinkIdleFloor},
		{time.Minute, time.Minute},
	} {
		if got := uplinkIdleTimeout(tc.mesh); got != tc.want {
			t.Errorf("uplinkIdleTimeout(%v) = %v, want %v", tc.mesh, got, tc.want)
		}
	}
	if uplinkIdleFloor < 3*controlcenter.DefaultKeepAlive {
		t.Errorf("floor %v is under three CC keep-alive periods (%v)", uplinkIdleFloor, controlcenter.DefaultKeepAlive)
	}
}
