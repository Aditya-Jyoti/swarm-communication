package main

import (
	"context"
	"io"
	"testing"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/protocol"
)

// applySim forwards the effective election pair only for a newer version, and
// only when it differs from the last pair forwarded. "Keep own" values resolve
// to the node's start-up settings, so clearing an override reverts them.
func TestApplySimForwardsChangedOverridesOnly(t *testing.T) {
	a, err := newApp(loopbackConfig("sim"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = a.run(ctx)
	})
	own := electionPair{threshold: cluster.DefaultThreshold, hysteresis: cluster.DefaultHysteresis}
	if a.ownElection != own {
		t.Fatalf("ownElection = %+v, want %+v", a.ownElection, own)
	}
	var got []electionPair
	a.setElection = func(th, hy float64) { got = append(got, electionPair{th, hy}) }

	sim := func(v uint64, th, hy float64) protocol.SimConfigPayload {
		return protocol.SimConfigPayload{Version: v, Threshold: th, Hysteresis: hy}
	}
	a.applySim(sim(1, 0, -1))                        // resolves to own: nothing to forward
	a.applySim(sim(2, 0.5, -1))                      // (0.5, own hysteresis)
	a.applySim(sim(3, 0.5, -3))                      // same effective pair: not forwarded
	a.applySim(sim(3, 0.9, 0))                       // stale version: ignored entirely
	a.applySim(sim(2, 0.9, 0))                       // older version: ignored
	a.applySim(sim(4, 0.5, 0))                       // hysteresis 0 is real: forwarded
	a.applySim(sim(5, 0, -1))                        // cleared: back to own values
	a.applySim(sim(6, cluster.DefaultThreshold, -1)) // equals own: not forwarded

	want := []electionPair{
		{0.5, cluster.DefaultHysteresis},
		{0.5, 0},
		own,
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", got, want)
		}
	}
	if v := a.emu.Version(); v != 6 {
		t.Fatalf("emulation version = %d, want 6", v)
	}
}

// Set then clear, against the real node: the override takes effect, and a
// SIM_CONFIG with threshold 0 / hysteresis -1 restores the node's own values
// (configured threshold, start-up default hysteresis).
func TestApplySimSetThenClearRevertsNode(t *testing.T) {
	cfg := loopbackConfig("revert")
	cfg.Threshold = 0.6
	a, err := newApp(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()
	defer func() {
		cancel()
		if err := recvT(t, done); err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	settled := func(th, hy float64) func() bool {
		return func() bool {
			st := a.node.Status()
			return st.Threshold == th && st.Hysteresis == hy
		}
	}
	waitFor(t, "own settings", settled(0.6, cluster.DefaultHysteresis))

	a.applySim(protocol.SimConfigPayload{Version: 1, Threshold: 0.9, Hysteresis: 0})
	waitFor(t, "override (0.9, 0)", settled(0.9, 0))

	a.applySim(protocol.SimConfigPayload{Version: 2, Threshold: 0, Hysteresis: -1})
	waitFor(t, "revert to (0.6, default)", settled(0.6, cluster.DefaultHysteresis))

	// Clearing one field at a time reverts only that field.
	a.applySim(protocol.SimConfigPayload{Version: 3, Threshold: 0.2, Hysteresis: 1.5})
	waitFor(t, "override (0.2, 1.5)", settled(0.2, 1.5))
	a.applySim(protocol.SimConfigPayload{Version: 4, Threshold: 0.2, Hysteresis: -1})
	waitFor(t, "hysteresis reverted", settled(0.2, cluster.DefaultHysteresis))
}

// End to end over real sockets: a SIM_CONFIG on one node delays the PONGs it
// sends, which its peer measures as RTT, and that node's telemetry carries the
// mesh frames it sent.
func TestSimConfigDelaysPongsAndTelemetryCarriesFlows(t *testing.T) {
	peer, err := newApp(loopbackConfig("peer"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if peer.flows != nil {
		t.Fatal("flow recorder built without a control center")
	}
	cc := newCCStub(t)
	cfg := loopbackConfig("drone", protocol.NodeAddress(peer.ln.Addr().String()))
	cfg.ControlCenter = cc.addr()
	cfg.TelemetryInterval = 20 * time.Millisecond
	a, err := newApp(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a.flows == nil {
		t.Fatal("no flow recorder with a control center")
	}
	a.exit = func(int) { t.Error("unexpected exit") }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- peer.run(ctx) }()
	go func() { done <- a.run(ctx) }()
	defer func() {
		cancel()
		for range 2 {
			if err := recvT(t, done); err != nil {
				t.Errorf("run: %v", err)
			}
		}
	}()
	cc.accept()

	// Telemetry shows PINGs to the peer: the prober sends through the
	// recorder.
	cc.next(protocol.TypeTelemetry, func(env *protocol.Envelope) bool {
		tp, err := protocol.PayloadOf[protocol.TelemetryPayload](env)
		if err != nil {
			return false
		}
		for _, f := range tp.Flows {
			if f.To == "peer" && f.Type == protocol.TypePing && f.Count > 0 {
				return true
			}
		}
		return false
	})

	const base = 150.0
	cc.send(protocol.TypeSimConfig, "drone", protocol.SimConfigPayload{
		Version: 1, Enabled: true, BaseMS: base,
		Positions: map[protocol.NodeID]protocol.Position{"drone": {}, "peer": {}},
		Threshold: 0, Hysteresis: -1,
	})

	// The peer's score for the drone climbs towards the emulated delay, and
	// the drone's telemetry now also counts the PONGs it sends.
	waitFor(t, "peer to measure the emulated delay", func() bool {
		return peer.node.Status().Scores["drone"] >= base*0.8
	})
	cc.next(protocol.TypeTelemetry, func(env *protocol.Envelope) bool {
		tp, err := protocol.PayloadOf[protocol.TelemetryPayload](env)
		if err != nil || tp.SimVersion != 1 {
			return false
		}
		for _, f := range tp.Flows {
			if f.To == "peer" && f.Type == protocol.TypePong && f.Count > 0 {
				return true
			}
		}
		return false
	})

	// Disabling the emulation removes the delay again.
	cc.send(protocol.TypeSimConfig, "drone", protocol.SimConfigPayload{Version: 2, Threshold: 0, Hysteresis: -1})
	waitFor(t, "delay to clear", func() bool {
		return a.emu.Version() == 2 && a.emu.PeerDelay("peer") == 0
	})
}
