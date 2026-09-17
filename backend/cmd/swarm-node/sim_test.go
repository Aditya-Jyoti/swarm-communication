package main

import (
	"context"
	"io"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// applySim forwards an election override only for a newer version whose
// override differs from the last one forwarded.
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
	var got []electionOverride
	a.setElection = func(th, hy float64) { got = append(got, electionOverride{th, hy}) }

	sim := func(v uint64, th, hy float64) protocol.SimConfigPayload {
		return protocol.SimConfigPayload{Version: v, Threshold: th, Hysteresis: hy}
	}
	a.applySim(sim(1, 0, -1))   // no override yet: nothing to forward
	a.applySim(sim(2, 0.5, -1)) // forwarded
	a.applySim(sim(3, 0.5, -1)) // same override: not forwarded
	a.applySim(sim(3, 0.9, 0))  // stale version: ignored entirely
	a.applySim(sim(2, 0.9, 0))  // older version: ignored
	a.applySim(sim(4, 0.5, 0))  // hysteresis changed: forwarded
	a.applySim(sim(5, 0, -1))   // back to "keep own": forwarded (a no-op on the node)

	want := []electionOverride{{0.5, -1}, {0.5, 0}, {0, -1}}
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", got, want)
		}
	}
	if v := a.emu.Version(); v != 5 {
		t.Fatalf("emulation version = %d, want 5", v)
	}
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
