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

func TestParseIdleMode(t *testing.T) {
	tests := []struct {
		in   string
		want IdleMode
		ok   bool
	}{
		// Unset is the default, so a deployment that never heard of MAV_MOTION
		// behaves exactly as it did before the knob existed.
		{"", IdleDrift, true},
		{"drift", IdleDrift, true},
		{"patrol", IdlePatrol, true},
		{"still", IdleStill, true},
		// Named, not indexed: an operator typed this, and the error has to be
		// readable in a compose log.
		{"patroll", "", false},
		{"Drift", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseIdleMode(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("ParseIdleMode(%q) error = %v, want ok %v", tc.in, err, tc.ok)
			}
			if got != tc.want {
				t.Errorf("ParseIdleMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMoveConfigIdleNormalisation(t *testing.T) {
	tests := []struct {
		name string
		in   MoveConfig
		want IdleMode
	}{
		{"zero value drifts", MoveConfig{}, IdleDrift},
		{"patrol kept", MoveConfig{Idle: IdlePatrol}, IdlePatrol},
		// An unvalidated mode must not freeze a vehicle a ground station is
		// watching; the default is the safe reading.
		{"garbage drifts", MoveConfig{Idle: "sideways"}, IdleDrift},
		// MAV_DRIFT=false predates MAV_MOTION and wins: an operator who set it
		// means "nothing moves on its own".
		{"MAV_DRIFT=false overrides", MoveConfig{Idle: IdlePatrol, DriftDisabled: true}, IdleStill},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.withDefaults().Idle; got != tc.want {
				t.Errorf("idle = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMoverPatrolFliesACircuit(t *testing.T) {
	home := protocol.Position{X: 50, Y: 50, Z: 10}
	cfg := MoveConfig{Idle: IdlePatrol, DriftRadius: 4, DriftSpeed: 0.4}
	m := NewMover("swarm-net-node-1", home, cfg)

	// One lap is 2*pi*R/v = 62.8 s at these numbers. Fly two of them at the
	// bridge's own 5 Hz position rate.
	const dt = 200 * time.Millisecond
	const steps = 2 * 63 * 5
	var minR, maxR = math.Inf(1), 0.0
	var maxSpeed float64
	for i := 0; i < steps; i++ {
		m.Step(dt)
		off := subP(m.Pos(), home)
		if off.Z != 0 {
			t.Fatalf("step %d: the circuit left the home altitude: %+v", i, m.Pos())
		}
		// The radius is a promise the contract makes about drift and patrol
		// alike, so it is asserted every step, not just at the end.
		if r := lenP(off); r > cfg.DriftRadius+1e-9 {
			t.Fatalf("step %d: %.4f units from home, radius is %v", i, r, cfg.DriftRadius)
		}
		if s := lenP(m.Vel()); s > maxSpeed {
			maxSpeed = s
		}
		// Ignore the first lap: it is the spiral outward onto the circuit.
		if i > 63*5 {
			r := lenP(off)
			minR, maxR = math.Min(minR, r), math.Max(maxR, r)
		}
	}
	// Settled on the circuit rather than hovering somewhere inside it.
	if minR < 0.99*cfg.DriftRadius || maxR > cfg.DriftRadius {
		t.Errorf("settled radius spans [%.3f, %.3f], want both near %v", minR, maxR, cfg.DriftRadius)
	}
	// Speed never exceeds the knob that sets it. This is what the pursuit
	// formulation buys: placing the vehicle on the circle directly would have
	// reported a 20-unit-per-second jump on the first step.
	if maxSpeed > cfg.DriftSpeed+1e-9 {
		t.Errorf("peak speed %.3f units/s, want at most %v", maxSpeed, cfg.DriftSpeed)
	}

	// It is still moving at the end: a demo watches this for minutes.
	before := m.Pos()
	for i := 0; i < 5; i++ {
		m.Step(dt)
	}
	if lenP(subP(m.Pos(), before)) < 0.1 {
		t.Errorf("patrol stalled at %+v", m.Pos())
	}
}

func TestMoverPatrolPhaseDiffersPerNode(t *testing.T) {
	// Otherwise the whole swarm flies the same circuit in formation, which
	// renders as one vehicle with N labels on it.
	home := protocol.Position{X: 50, Y: 50, Z: 10}
	cfg := MoveConfig{Idle: IdlePatrol, DriftRadius: 6, DriftSpeed: 1}
	first := NewMover("swarm-net-node-1", home, cfg)
	second := NewMover("swarm-net-node-2", home, cfg)
	for i := 0; i < 100; i++ {
		first.Step(200 * time.Millisecond)
		second.Step(200 * time.Millisecond)
	}
	if d := lenP(subP(first.Pos(), second.Pos())); d < 1 {
		t.Errorf("two nodes are %.3f units apart on the same circuit", d)
	}
}

func TestMoverPatrolYieldsToAMission(t *testing.T) {
	// Patrol is the IDLE behaviour: a waypoint must win, and the vehicle must
	// go back to patrolling after it.
	home := protocol.Position{X: 50, Y: 50, Z: 10}
	m := NewMover("seed", home, MoveConfig{Idle: IdlePatrol, SpeedUnitsPerSec: 3, DriftRadius: 4, DriftSpeed: 0.4})
	m.SetTarget(protocol.Position{X: 80, Y: 50, Z: 10})
	for i := 0; i < 20 && !m.Reached(); i++ {
		m.Step(time.Second)
	}
	if !m.Reached() {
		t.Fatal("patrol kept the vehicle from flying its waypoint")
	}
	m.ClearTarget()
	before := m.Pos()
	m.Step(time.Second)
	if m.Pos() == before {
		t.Error("the vehicle did not resume patrolling after the waypoint")
	}
}
