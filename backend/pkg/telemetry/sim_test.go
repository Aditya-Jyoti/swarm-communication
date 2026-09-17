package telemetry

import (
	"math"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/geo"
	"swarm-net/pkg/protocol"
)

func TestSanitizeSimConfig(t *testing.T) {
	in := protocol.SimConfigPayload{
		Version: 3, Enabled: true,
		Positions: map[protocol.NodeID]protocol.Position{
			"a": {X: -5, Y: 50, Z: 500},
			"b": {X: math.NaN(), Y: 10, Z: 20},
		},
		BaseMS: 9999, PerUnitMS: -1, JitterMS: math.NaN(),
		Threshold: 1.5, Hysteresis: 0,
	}
	got := SanitizeSimConfig(in)
	if got.Positions["a"] != (protocol.Position{X: 0, Y: 50, Z: geo.Size}) {
		t.Errorf("a = %+v", got.Positions["a"])
	}
	if got.Positions["b"] != (protocol.Position{X: 0, Y: 10, Z: 20}) {
		t.Errorf("b = %+v", got.Positions["b"])
	}
	if got.BaseMS != geo.MaxBaseMS || got.PerUnitMS != 0 || got.JitterMS != 0 {
		t.Errorf("params = %v/%v/%v", got.BaseMS, got.PerUnitMS, got.JitterMS)
	}
	if got.Threshold != 0 {
		t.Errorf("threshold 1.5 -> %v, want 0 (unchanged)", got.Threshold)
	}
	if got.Hysteresis != 0 {
		t.Errorf("hysteresis 0 -> %v, want 0 kept", got.Hysteresis)
	}
	if got.Version != 3 || !got.Enabled {
		t.Errorf("version/enabled not preserved: %+v", got)
	}
	// The input map is untouched.
	if in.Positions["a"].X != -5 {
		t.Error("SanitizeSimConfig modified its input")
	}

	for _, tc := range []struct{ th, wantTh, hy, wantHy float64 }{
		{0.4, 0.4, 0.5, 0.5},
		{1, 1, -1, -1},
		{0, 0, -3, -3},
		{-0.1, 0, math.NaN(), -1},
		{math.NaN(), 0, math.Inf(1), -1},
		{math.Inf(1), 0, math.Inf(-1), -1},
	} {
		got := SanitizeSimConfig(protocol.SimConfigPayload{Threshold: tc.th, Hysteresis: tc.hy})
		if got.Threshold != tc.wantTh || got.Hysteresis != tc.wantHy {
			t.Errorf("(%v, %v) -> (%v, %v), want (%v, %v)", tc.th, tc.hy, got.Threshold, got.Hysteresis, tc.wantTh, tc.wantHy)
		}
		if got.Positions == nil {
			t.Error("Positions must be non-nil")
		}
	}
}

func simAt(version uint64, enabled bool, pos map[protocol.NodeID]protocol.Position) protocol.SimConfigPayload {
	return protocol.SimConfigPayload{
		Version: version, Enabled: enabled, Positions: pos,
		BaseMS: 1, PerUnitMS: 2, JitterMS: 10,
	}
}

func TestEmulationPeerDelay(t *testing.T) {
	u := 0.5
	e := NewEmulation("a", func() float64 { return u })
	if d := e.PeerDelay("b"); d != 0 || e.Version() != 0 {
		t.Fatalf("fresh emulation: delay %v version %d", d, e.Version())
	}

	pos := map[protocol.NodeID]protocol.Position{
		"a": {X: 0, Y: 0, Z: 0},
		"b": {X: 3, Y: 4, Z: 0}, // distance 5
	}
	if !e.Apply(simAt(2, true, pos)) {
		t.Fatal("first Apply refused")
	}
	// Mutating the caller's map after Apply must not reach the snapshot.
	pos["b"] = protocol.Position{X: 90, Y: 90, Z: 90}
	want := time.Duration((1 + 5*2 + 10*0.5) * float64(time.Millisecond)) // 16ms
	if d := e.PeerDelay("b"); d != want {
		t.Fatalf("delay = %v, want %v", d, want)
	}
	u = 0
	if d := e.PeerDelay("b"); d != 11*time.Millisecond {
		t.Fatalf("delay with u=0 = %v, want 11ms (fresh draw per call)", d)
	}
	if d := e.PeerDelay("ghost"); d != 0 {
		t.Fatalf("unknown peer delay = %v, want 0", d)
	}
	if e.Version() != 2 {
		t.Fatalf("Version = %d", e.Version())
	}

	// Older and equal versions are ignored.
	if e.Apply(simAt(2, false, nil)) || e.Apply(simAt(1, false, nil)) {
		t.Fatal("stale Apply accepted")
	}
	if d := e.PeerDelay("b"); d == 0 {
		t.Fatal("stale Apply changed the state")
	}

	// Disabled: no delay, even with positions.
	if !e.Apply(simAt(3, false, map[protocol.NodeID]protocol.Position{"a": {}, "b": {X: 10}})) {
		t.Fatal("Apply v3 refused")
	}
	if d := e.PeerDelay("b"); d != 0 {
		t.Fatalf("disabled delay = %v", d)
	}

	// Self position unknown: no delay.
	e.Apply(simAt(4, true, map[protocol.NodeID]protocol.Position{"b": {X: 10}}))
	if d := e.PeerDelay("b"); d != 0 {
		t.Fatalf("delay without a self position = %v", d)
	}
}

func TestEmulationDefaultRandIsBounded(t *testing.T) {
	e := NewEmulation("a", nil)
	e.Apply(protocol.SimConfigPayload{
		Version: 1, Enabled: true,
		Positions: map[protocol.NodeID]protocol.Position{"a": {}, "b": {}},
		JitterMS:  100,
	})
	for range 100 {
		if d := e.PeerDelay("b"); d < 0 || d >= 100*time.Millisecond {
			t.Fatalf("delay %v outside [0, 100ms)", d)
		}
	}
}

// Readers on many goroutines while versions are applied: run under -race.
func TestEmulationConcurrentApplyAndDelay(t *testing.T) {
	e := NewEmulation("a", nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.PeerDelay("b")
					_ = e.Version()
				}
			}
		}()
	}
	var appliers sync.WaitGroup
	for g := range 2 {
		appliers.Add(1)
		go func() {
			defer appliers.Done()
			for v := uint64(1); v <= 200; v++ {
				e.Apply(simAt(v*2+uint64(g), true, map[protocol.NodeID]protocol.Position{"a": {}, "b": {X: float64(v % 100)}}))
			}
		}()
	}
	appliers.Wait()
	close(stop)
	wg.Wait()
	if v := e.Version(); v != 401 {
		t.Fatalf("final version = %d, want the highest applied (401)", v)
	}
}

func TestFromStatusElectionFields(t *testing.T) {
	p := FromStatus(cluster.Status{Self: "n", Threshold: 0.4, Hysteresis: 0})
	if p.Threshold != 0.4 || p.Hysteresis != 0 || p.SimVersion != 0 || p.Flows != nil {
		t.Fatalf("payload = %+v", p)
	}
}

func TestSnapshotFunc(t *testing.T) {
	status := func() cluster.Status { return cluster.Status{Self: "n", Threshold: 0.3, Hysteresis: 0.5} }
	flows := []protocol.FlowRecord{{To: "m", Type: protocol.TypePing, Count: 2}}
	snap := SnapshotFunc(status, func() uint64 { return 7 }, func() []protocol.FlowRecord { return flows })
	p := snap()
	if p.Node != "n" || p.Threshold != 0.3 || p.Hysteresis != 0.5 || p.SimVersion != 7 {
		t.Fatalf("payload = %+v", p)
	}
	if len(p.Flows) != 1 || p.Flows[0] != flows[0] {
		t.Fatalf("flows = %+v", p.Flows)
	}

	// nil sources are allowed.
	p = SnapshotFunc(status, nil, nil)()
	if p.SimVersion != 0 || p.Flows != nil {
		t.Fatalf("payload with nil sources = %+v", p)
	}
}

func TestClientDispatchesSimConfig(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	sims := make(chan protocol.SimConfigPayload, 4)
	h := startClient(t, cc.addr(), func(c *Config) {
		c.OnSim = func(p protocol.SimConfigPayload) { sims <- p }
	})
	link := cc.accept()
	link.next(protocol.TypeTelemetry)

	link.send(protocol.TypeSimConfig, protocol.SimConfigPayload{
		Version: 5, Enabled: true,
		Positions: map[protocol.NodeID]protocol.Position{"node-1": {X: 200, Y: 1, Z: 2}},
		BaseMS:    1, PerUnitMS: 50, JitterMS: 0.5,
		Threshold: 2, Hysteresis: 0.25,
	})
	got := recv(t, sims, "sim config")
	if got.Version != 5 || !got.Enabled {
		t.Fatalf("sim = %+v", got)
	}
	if got.Positions["node-1"] != (protocol.Position{X: geo.Size, Y: 1, Z: 2}) {
		t.Fatalf("position not clamped: %+v", got.Positions)
	}
	if got.PerUnitMS != geo.MaxPerUnitMS || got.Threshold != 0 || got.Hysteresis != 0.25 {
		t.Fatalf("values not sanitized: %+v", got)
	}

	// A malformed SIM_CONFIG is dropped; the next good frame still arrives.
	link.send(protocol.TypeSimConfig, "nope")
	link.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "clear"})
	recv(t, h.chaos, "chaos after bad sim")
	select {
	case p := <-sims:
		t.Fatalf("malformed SIM_CONFIG delivered: %+v", p)
	default:
	}
}

func TestClientWithoutOnSimDropsSimConfig(t *testing.T) {
	cc := newFakeCC(t, ControlCenterID)
	h := startClient(t, cc.addr(), nil)
	link := cc.accept()
	link.send(protocol.TypeSimConfig, protocol.SimConfigPayload{Version: 1})
	link.send(protocol.TypeChaos, protocol.ChaosPayload{Action: "clear"})
	recv(t, h.chaos, "chaos after an unhandled sim config")
}
