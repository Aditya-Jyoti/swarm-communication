package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/protocol"
)

// Bounds for the loopback tests. Generous because -race and -count=3 share the
// machine; a test that finishes in 50ms normally must not fail at 2s under load.
const (
	testWait          = 15 * time.Second
	testPoll          = 20 * time.Millisecond
	testProbeInterval = 50 * time.Millisecond
	testIdleTimeout   = 2 * time.Second
)

func loopbackConfig(id string, seeds ...protocol.NodeAddress) Config {
	return Config{
		NodeID: protocol.NodeID(id),
		Listen: "127.0.0.1:0",
		// Port 0 is not dialable, but Advertise only has to be unique and
		// stable here: peers find each other through Seeds, and the prober
		// resolves this string back to a NodeID through the membership table.
		Advertise:     protocol.NodeAddress("127.0.0.1:0#" + id),
		Seeds:         seeds,
		Threshold:     cluster.DefaultThreshold,
		ProbeInterval: testProbeInterval,
		ElectionFloor: cluster.DefaultElectionFloor,
		// Short enough that the real-socket tests also exercise anti-entropy.
		GossipInterval: testProbeInterval,
		IdleTimeout:    testIdleTimeout,
		LogLevel:       slog.LevelDebug,
	}
}

// waitFor polls cond on a ticker until it holds or the deadline passes. It is
// the test's synchronisation primitive in place of a sleep: it returns the
// moment the condition is true and fails loudly if it never is.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(testWait)
	defer deadline.Stop()
	tick := time.NewTicker(testPoll)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestRunStartsAndStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs bytes.Buffer
	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, loopbackConfig("solo"), &logs, func(a net.Addr) { listening <- a })
	}()

	var addr net.Addr
	select {
	case addr = <-listening:
	case err := <-done:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(testWait):
		t.Fatal("never started listening")
	}
	if _, port, err := net.SplitHostPort(addr.String()); err != nil || port == "0" {
		t.Fatalf("bound address %q is not a real port", addr)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("run did not return after cancel")
	}
	for _, want := range []string{"listening", "node starting", "leaving", "closing listener", "closing prober", "closing pool", "shutdown complete"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

func TestTwoNodesConnectAndSeeEachOther(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := newApp(loopbackConfig("alpha"), io.Discard)
	if err != nil {
		t.Fatalf("newApp alpha: %v", err)
	}
	seed := protocol.NodeAddress(a.ln.Addr().String())
	b, err := newApp(loopbackConfig("beta", seed), io.Discard)
	if err != nil {
		t.Fatalf("newApp beta: %v", err)
	}

	done := make(chan error, 2)
	go func() { done <- a.run(ctx) }()
	go func() { done <- b.run(ctx) }()

	sees := func(n *cluster.Node, id protocol.NodeID) bool {
		m, ok := n.Status().View.Get(id)
		return ok && m.State == cluster.StateAlive
	}
	waitFor(t, "alpha to see beta", func() bool { return sees(a.node, "beta") })
	waitFor(t, "beta to see alpha", func() bool { return sees(b.node, "alpha") })

	// With two members and the default threshold one leader is elected and
	// the other attaches to it. Both sides must agree on who.
	waitFor(t, "a single agreed leader", func() bool {
		sa, sb := a.node.Status(), b.node.Status()
		return len(sa.Leaders) == 1 && len(sb.Leaders) == 1 &&
			sa.Leaders[0] == sb.Leaders[0] &&
			sa.Leader == sa.Leaders[0] && sb.Leader == sb.Leaders[0]
	})

	cancel()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(testWait):
			t.Fatal("a node did not stop after cancel")
		}
	}
}

func TestRunListenFailureIsRuntimeError(t *testing.T) {
	// Occupy a port so Listen fails deterministically.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	cfg := loopbackConfig("clash")
	cfg.Listen = taken.Addr().String()
	err = run(context.Background(), cfg, io.Discard, func(net.Addr) { t.Error("onListening must not fire") })
	if err == nil {
		t.Fatal("expected listen failure")
	}
	if exitCode(err) != exitRuntime {
		t.Fatalf("exit code %d, want %d for %v", exitCode(err), exitRuntime, err)
	}
	if !strings.Contains(err.Error(), "clash") {
		t.Fatalf("error should name the node: %v", err)
	}
}

func TestNewAppRejectsInvalidNode(t *testing.T) {
	// Load never produces this, but newApp must still fail closed and release
	// the pool it already built rather than leak it.
	cfg := loopbackConfig("")
	if _, err := newApp(cfg, io.Discard); err == nil {
		t.Fatal("expected NewNode failure for empty NodeID")
	}
}

// fakeListener stands in for a *network.Listener whose accept loop has died.
type fakeListener struct {
	addr net.Addr
	done chan struct{}
	err  error
}

func (f *fakeListener) Addr() net.Addr        { return f.addr }
func (f *fakeListener) Done() <-chan struct{} { return f.done }
func (f *fakeListener) Err() error            { return f.err }
func (f *fakeListener) Close() error          { return nil }
func (f *fakeListener) die(err error)         { f.err = err; close(f.done) }
func newFakeListener(addr net.Addr) *fakeListener {
	return &fakeListener{addr: addr, done: make(chan struct{})}
}

func TestRunExitsWhenListenerDies(t *testing.T) {
	a, err := newApp(loopbackConfig("fragile"), io.Discard)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	real := a.ln
	fake := newFakeListener(real.Addr())
	a.ln = fake
	_ = real.Close()

	done := make(chan error, 1)
	go func() { done <- a.run(context.Background()) }()

	// The node is alone; it has elected itself by the time Run enters its
	// loop. Wait for that so the failure is injected into a running node.
	waitFor(t, "self-election", func() bool { return a.node.Status().Leader == "fragile" })

	boom := errors.New("accept: interface vanished")
	fake.die(boom)

	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("run = %v, want to wrap %v", err, boom)
		}
		if exitCode(err) != exitRuntime {
			t.Fatalf("exit code %d, want %d", exitCode(err), exitRuntime)
		}
	case <-time.After(testWait):
		t.Fatal("run did not return after listener died")
	}
}

func TestRunListenerCleanCloseIsNotAnError(t *testing.T) {
	a, err := newApp(loopbackConfig("quiet"), io.Discard)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	real := a.ln
	fake := newFakeListener(real.Addr())
	a.ln = fake
	_ = real.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()

	// Done with a nil Err is what Close produces; it must not end the run.
	fake.die(nil)
	waitFor(t, "self-election", func() bool { return a.node.Status().Leader == "quiet" })
	select {
	case err := <-done:
		t.Fatalf("run returned early: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("run did not return after cancel")
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, exitOK},
		{errConfig, exitConfig},
		{errors.Join(errors.New("x"), errConfig), exitConfig},
		{errors.New("runtime"), exitRuntime},
	}
	for _, tc := range cases {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
	// The real config path: Load's errors must map to exitConfig.
	_, err := Load([]string{"-threshold=2"}, noEnv, host("box"))
	if exitCode(err) != exitConfig {
		t.Fatalf("Load error maps to %d, want %d", exitCode(err), exitConfig)
	}
}

func TestPrintVersion(t *testing.T) {
	old := version
	version = "v1.2.3-test"
	defer func() { version = old }()
	var out bytes.Buffer
	printVersion(&out)
	if out.String() != "swarm-node v1.2.3-test\n" {
		t.Fatalf("printVersion = %q", out.String())
	}
}

func TestAppRunsOnce(t *testing.T) {
	a, err := newApp(loopbackConfig("once"), io.Discard)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A second run finds the node loop already spent. Every Close it repeats is
	// idempotent, so the only outcome is the node's own refusal, surfaced.
	err = a.run(ctx)
	if !errors.Is(err, cluster.ErrAlreadyRunning) {
		t.Fatalf("second run = %v, want ErrAlreadyRunning", err)
	}
	if exitCode(err) != exitRuntime {
		t.Fatalf("exit code %d, want %d", exitCode(err), exitRuntime)
	}
}
