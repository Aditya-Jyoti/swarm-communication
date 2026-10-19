package mavlink

import (
	"math"
	"testing"
	"time"

	"swarm-net/pkg/geo"
	"swarm-net/pkg/protocol"
)

func TestMoverDriftStaysInRadius(t *testing.T) {
	home := protocol.Position{X: 50, Y: 50, Z: 50}
	cfg := MoveConfig{DriftRadius: 4, DriftSpeed: 0.4}
	tests := []struct {
		name string
		dt   time.Duration
		// steps covers enough simulated time for the walk to reach the
		// boundary many times over: at 0.4 units/s a vehicle crosses a 4-unit
		// radius in ten seconds.
		steps int
	}{
		{"5Hz", 200 * time.Millisecond, 5000},
		{"1Hz", time.Second, 1000},
		// A long dt is the case the hard bound exists for: the pull alone
		// cannot stop a vehicle that moves half the radius in one step.
		{"stalled process", 20 * time.Second, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMover("swarm-net-node-1", home, cfg)
			var maxSeen float64
			for i := 0; i < tc.steps; i++ {
				m.Step(tc.dt)
				d := lenP(subP(m.Pos(), home))
				if d > maxSeen {
					maxSeen = d
				}
				if d > cfg.DriftRadius+1e-9 {
					t.Fatalf("step %d: %.4f units from home, radius is %v", i, d, cfg.DriftRadius)
				}
				if p := m.Pos(); p != geo.ClampPosition(p) {
					t.Fatalf("step %d: %+v is outside the cube", i, p)
				}
			}
			// The walk must actually wander, or the bound above is vacuous.
			if maxSeen < cfg.DriftRadius/2 {
				t.Errorf("drift never got past %.3f units; the walk is not wandering", maxSeen)
			}
		})
	}
}

func TestMoverDriftIsSeededByNodeID(t *testing.T) {
	home := protocol.Position{X: 10, Y: 20, Z: 30}
	cfg := MoveConfig{DriftRadius: 4, DriftSpeed: 0.4}
	path := func(id protocol.NodeID) []protocol.Position {
		m := NewMover(id, home, cfg)
		var out []protocol.Position
		for i := 0; i < 50; i++ {
			m.Step(200 * time.Millisecond)
			out = append(out, m.Pos())
		}
		return out
	}
	a, again := path("swarm-net-node-3"), path("swarm-net-node-3")
	for i := range a {
		if a[i] != again[i] {
			t.Fatalf("step %d: %+v != %+v; a restart must drift identically", i, a[i], again[i])
		}
	}
	// And a different node must not trace the same path, or the whole swarm
	// would drift in formation.
	other := path("swarm-net-node-4")
	if other[len(other)-1] == a[len(a)-1] {
		t.Error("two node ids produced the same drift path")
	}
}

func TestMoverDriftDisabledHoldsStill(t *testing.T) {
	home := protocol.Position{X: 1, Y: 2, Z: 3}
	m := NewMover("seed", home, MoveConfig{DriftDisabled: true})
	for i := 0; i < 10; i++ {
		m.Step(time.Second)
	}
	if m.Pos() != home || m.Vel() != (protocol.Position{}) {
		t.Fatalf("drift disabled: pos %+v vel %+v, want still at %+v", m.Pos(), m.Vel(), home)
	}
}

func TestMoverFliesToTarget(t *testing.T) {
	m := NewMover("seed", protocol.Position{X: 0, Y: 0, Z: 0}, MoveConfig{SpeedUnitsPerSec: 3})
	target := protocol.Position{X: 30, Y: 0, Z: 0}
	m.SetTarget(target)
	if !m.Flying() || m.Reached() {
		t.Fatal("a fresh target must not be reached")
	}
	// One second at 3 units/s.
	m.Step(time.Second)
	if got := m.Pos().X; math.Abs(got-3) > 1e-9 {
		t.Fatalf("after 1s at 3 units/s, x = %v, want 3", got)
	}
	// Velocity is reported in units/s, east-positive.
	if v := m.Vel(); math.Abs(v.X-3) > 1e-9 || v.Y != 0 {
		t.Fatalf("vel = %+v, want 3 units/s east", v)
	}
	if h := m.Heading(); math.Abs(h-90) > 1e-9 {
		t.Fatalf("heading = %v, want 90 (due east)", h)
	}
	// It must stop ON the waypoint, not oscillate around it: the last step is
	// shorter than a full one.
	for i := 0; i < 20 && !m.Reached(); i++ {
		m.Step(2 * time.Second)
	}
	if !m.Reached() || m.Pos() != target {
		t.Fatalf("pos %+v reached %v, want to stop exactly on %+v", m.Pos(), m.Reached(), target)
	}
	// Stopped: no residual velocity to report to a ground station.
	m.Step(time.Second)
	if m.Vel() != (protocol.Position{}) {
		t.Fatalf("vel after arrival = %+v, want zero", m.Vel())
	}
	// Heading survives the stop; a parked vehicle still points somewhere.
	if math.Abs(m.Heading()-90) > 1e-9 {
		t.Fatalf("heading after arrival = %v, want 90", m.Heading())
	}
}

func TestMoverAdopt(t *testing.T) {
	m := NewMover("seed", protocol.Position{X: 50, Y: 50, Z: 50}, MoveConfig{})
	m.SetTarget(protocol.Position{X: 60, Y: 50, Z: 50})
	m.Step(time.Second)

	moved := protocol.Position{X: 10, Y: 90, Z: 20}
	m.Adopt(moved)
	if m.Pos() != moved || m.Home() != moved {
		t.Fatalf("after Adopt: pos %+v home %+v, want both %+v", m.Pos(), m.Home(), moved)
	}
	if m.Flying() {
		t.Error("Adopt must abandon the target; an operator outranks a stale waypoint")
	}
	// Drift now happens around the new home.
	for i := 0; i < 100; i++ {
		m.Step(200 * time.Millisecond)
	}
	if d := lenP(subP(m.Pos(), moved)); d > DefaultDriftRadius+1e-9 {
		t.Fatalf("drifted %.3f from the adopted home, radius is %v", d, DefaultDriftRadius)
	}
	// Out-of-cube adoption is clamped rather than believed.
	m.Adopt(protocol.Position{X: -5, Y: 1e9, Z: 0})
	if p := m.Pos(); p != geo.ClampPosition(p) {
		t.Fatalf("adopted an out-of-cube position: %+v", p)
	}
}

func TestMoverIgnoresNonPositiveDT(t *testing.T) {
	m := NewMover("seed", protocol.Position{X: 5, Y: 5, Z: 5}, MoveConfig{})
	before := m.Pos()
	m.Step(0)
	m.Step(-time.Second) // a clock that stepped backwards
	if m.Pos() != before {
		t.Fatalf("pos moved on non-positive dt: %+v -> %+v", before, m.Pos())
	}
}

func TestHeadingOf(t *testing.T) {
	tests := []struct {
		name string
		vel  protocol.Position
		want float64
		ok   bool
	}{
		{"north", protocol.Position{Y: 1}, 0, true},
		{"east", protocol.Position{X: 1}, 90, true},
		{"south", protocol.Position{Y: -1}, 180, true},
		{"west", protocol.Position{X: -1}, 270, true},
		{"north-east", protocol.Position{X: 1, Y: 1}, 45, true},
		// Climbing straight up has no course, and reporting 0 would claim north.
		{"vertical only", protocol.Position{Z: 1}, 0, false},
		{"still", protocol.Position{}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := headingOf(tc.vel)
			if ok != tc.ok || (ok && math.Abs(got-tc.want) > 1e-9) {
				t.Errorf("headingOf(%+v) = (%v, %v), want (%v, %v)", tc.vel, got, ok, tc.want, tc.ok)
			}
		})
	}
}
