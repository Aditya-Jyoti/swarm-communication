package mavlink

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"time"

	"swarm-net/pkg/geo"
	"swarm-net/pkg/protocol"
)

// Movement defaults, from docs/architecture/mavlink-bridge.md section 6.
const (
	DefaultSpeedUnitsPerSec = 3.0
	DefaultDriftRadius      = 4.0
	DefaultDriftSpeed       = 0.4
)

// driftTurnRate is how fast the drift direction wanders, in radians-ish of
// direction change per second (it is a blend factor, not an angle).
//
// It is a constant rather than a knob because it is the one drift parameter
// nobody needs to tune: it trades "jitters like a dropped GPS fix" (high) for
// "flies in a straight line until it hits the radius" (low). 1.5 per second
// gives a hover that looks like a real multirotor holding position badly, and
// at the default 0.4 units/s the resulting path is smooth on a map.
const driftTurnRate = 1.5

// MoveConfig is how a vehicle moves. Zero values take the contract defaults,
// so the zero MoveConfig is a drifting vehicle, not a stationary one.
type MoveConfig struct {
	// SpeedUnitsPerSec is flight speed toward a target, in cube units per
	// second. MAV_SPEED_UNITS_S.
	SpeedUnitsPerSec float64
	// DriftDisabled switches idle drift off. Inverted so the zero value is the
	// contract default (MAV_DRIFT=true); set it when you are debugging
	// clustering and want nodes to hold still.
	DriftDisabled bool
	// DriftRadius bounds how far a vehicle may drift from home, in units.
	// MAV_DRIFT_RADIUS.
	DriftRadius float64
	// DriftSpeed is drift speed in units per second. MAV_DRIFT_SPEED.
	DriftSpeed float64
}

func (c MoveConfig) withDefaults() MoveConfig {
	if c.SpeedUnitsPerSec <= 0 {
		c.SpeedUnitsPerSec = DefaultSpeedUnitsPerSec
	}
	if c.DriftRadius <= 0 {
		c.DriftRadius = DefaultDriftRadius
	}
	if c.DriftSpeed <= 0 {
		c.DriftSpeed = DefaultDriftSpeed
	}
	return c
}

// Mover is one vehicle's simulated motion through the swarm's cube.
//
// # One mover, two behaviours
//
// A mover always has the same job -- advance a position by dt -- and the target
// decides how:
//
//   - With a target (SetTarget), it flies straight at SpeedUnitsPerSec and
//     stops dead on arrival. That is the seam the mission code uses: assign a
//     waypoint, step, watch Reached.
//   - With no target, it drifts: a seeded random walk pulled back toward home
//     and hard-bounded by DriftRadius.
//
// Drift is the no-target behaviour rather than a separate mode because every
// vehicle is idle most of the time and "idle" must still produce plausible
// telemetry -- a ground station showing ten vehicles frozen on exactly their
// spawn pixels looks broken. It is also why drift is slow: position feeds the
// latency model, which feeds elections, so fast drift makes elections flap and
// you lose the ability to say what caused a re-election.
//
// # Determinism
//
// The random walk is seeded from the node id alone, so a given node drifts the
// same way on every run of the process and a restarted Control Center does not
// scatter the swarm. Nothing here reads a clock: dt is passed in, which is what
// lets the bridge's tests advance time without sleeping.
//
// # Concurrency
//
// Not safe for concurrent use and carries no mutex. Each Mover belongs to one
// vehicle, and every vehicle belongs to the bridge's loop goroutine; see
// Bridge. A mutex here would only hide a caller that had reached across
// goroutines, which is precisely what the race detector should be allowed to
// report.
type Mover struct {
	cfg MoveConfig
	rng *rand.Rand

	home protocol.Position
	pos  protocol.Position
	// vel is the velocity actually achieved over the last Step, in units per
	// second: the measured displacement, not the intended one. Clamping at the
	// drift radius or stopping on a waypoint both reduce it, and a ground
	// station that is told 0.4 m/s while the dot on the map is stationary looks
	// like a broken estimator.
	vel protocol.Position
	// dir is the current drift heading, a unit vector. Kept across steps so the
	// walk is correlated: an uncorrelated direction every tick is Brownian
	// noise, which at 5 Hz renders as a vibrating dot.
	dir protocol.Position

	target  *protocol.Position
	reached bool
	// heading is the last known course in degrees clockwise from north. Held
	// through a stop, because a stationary vehicle still points somewhere.
	heading float64
}

// NewMover returns a mover for node, homed at and starting from home.
func NewMover(node protocol.NodeID, home protocol.Position, cfg MoveConfig) *Mover {
	cfg = cfg.withDefaults()
	m := &Mover{
		cfg:  cfg,
		rng:  rand.New(rand.NewPCG(seedOf(node), seedOf(node)^0x9E3779B97F4A7C15)),
		home: home,
		pos:  home,
	}
	// A starting direction, so the first step does not have to pick one from a
	// zero vector.
	m.dir = normP(m.randomUnit())
	return m
}

// seedOf hashes a node id into a PRNG seed. FNV-1a is enough here: the only
// requirement is that two node ids differing in one byte drift differently,
// and the PCG seeding mixes the state itself.
func seedOf(node protocol.NodeID) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(node))
	return h.Sum64()
}

// Pos is the current position.
func (m *Mover) Pos() protocol.Position { return m.pos }

// Home is the position drift is pulled back toward.
func (m *Mover) Home() protocol.Position { return m.home }

// Vel is the velocity achieved over the last Step, in cube units per second.
func (m *Mover) Vel() protocol.Position { return m.vel }

// Heading is the course in degrees clockwise from north, [0, 360).
func (m *Mover) Heading() float64 { return m.heading }

// Target is the waypoint being flown to, or nil when the mover is drifting.
func (m *Mover) Target() (protocol.Position, bool) {
	if m.target == nil {
		return protocol.Position{}, false
	}
	return *m.target, true
}

// Flying reports whether the mover has a target. The bridge arms the vehicle
// exactly while this is true.
func (m *Mover) Flying() bool { return m.target != nil }

// Reached reports whether the current target has been arrived at. It stays true
// until the target changes, so a 1 Hz consumer cannot miss an arrival that
// happened on a 5 Hz step.
func (m *Mover) Reached() bool { return m.target != nil && m.reached }

// SetTarget sends the vehicle to p. This is the mission seam: pkg/mavlink's
// mission handling will call it per assigned waypoint and watch Reached.
func (m *Mover) SetTarget(p protocol.Position) {
	p = geo.ClampPosition(p)
	m.target = &p
	m.reached = false
}

// ClearTarget returns the vehicle to drifting around its current home.
func (m *Mover) ClearTarget() {
	m.target = nil
	m.reached = false
}

// Adopt moves the vehicle to p and makes p its new home, abandoning any
// target.
//
// This is what the bridge calls when something outside it -- a dashboard
// slider, POST /api/sim -- has moved the node. The bridge must not fight an
// operator: it adopts the new position as the truth, rather than dragging the
// node back to where it thought it was, which would make the slider look
// broken.
func (m *Mover) Adopt(p protocol.Position) {
	p = geo.ClampPosition(p)
	m.home = p
	m.pos = p
	m.vel = protocol.Position{}
	m.target = nil
	m.reached = false
}

// Step advances the position by dt.
//
// Non-positive dt is ignored (a clock that went backwards must not teleport a
// vehicle), and so is an absurdly large dt: see maxStep.
func (m *Mover) Step(dt time.Duration) {
	if dt <= 0 {
		return
	}
	secs := dt.Seconds()
	prev := m.pos
	switch {
	case m.target != nil:
		m.flyStep(secs)
	case m.cfg.DriftDisabled:
		// Deliberately still: MAV_DRIFT=false means the only thing that moves a
		// node is an operator or a mission.
	default:
		m.driftStep(secs)
	}
	m.pos = geo.ClampPosition(m.pos)
	m.vel = scaleP(subP(m.pos, prev), 1/secs)
	if h, ok := headingOf(m.vel); ok {
		m.heading = h
	}
}

// flyStep advances straight toward the target, stopping exactly on it.
func (m *Mover) flyStep(secs float64) {
	d := subP(*m.target, m.pos)
	dist := lenP(d)
	step := m.cfg.SpeedUnitsPerSec * secs
	if dist <= step || dist == 0 {
		m.pos = *m.target
		m.reached = true
		return
	}
	m.pos = addP(m.pos, scaleP(d, step/dist))
}

// driftStep advances one tick of the bounded random walk.
//
// The shape of it: pick a direction that is mostly random near home and mostly
// homeward near the radius, ease the current direction toward it, then move.
// The homeward weight is k^2 rather than k so that the inner half of the ball
// is almost free -- a linear pull makes every vehicle hover on a tight leash
// around the exact centre, which looks less like a drone and more like a spring.
func (m *Mover) driftStep(secs float64) {
	if m.cfg.DriftSpeed <= 0 || m.cfg.DriftRadius <= 0 {
		return
	}
	off := subP(m.pos, m.home)
	k := lenP(off) / m.cfg.DriftRadius
	if k > 1 {
		k = 1
	}
	want := scaleP(m.randomUnit(), 1-k*k)
	if k > 0 {
		want = subP(want, scaleP(normP(off), k*k))
	}
	// Ease, so the path has no corners. The blend is capped at 1 so a long dt
	// (a stalled process, a stepped clock) turns instantly instead of
	// overshooting into a direction reversal.
	blend := math.Min(1, driftTurnRate*secs)
	m.dir = normP(addP(m.dir, scaleP(subP(want, m.dir), blend)))
	if m.dir == (protocol.Position{}) {
		return // degenerate: both dir and want were zero
	}
	m.pos = addP(m.pos, scaleP(m.dir, m.cfg.DriftSpeed*secs))

	// Hard bound. The pull above makes exceeding the radius unlikely, not
	// impossible -- one long dt is enough -- and the radius is a promise the
	// contract makes, so it is enforced geometrically rather than hoped for.
	// Projecting onto the sphere also keeps the vehicle moving tangentially
	// instead of sticking to the boundary.
	off = subP(m.pos, m.home)
	if d := lenP(off); d > m.cfg.DriftRadius {
		m.pos = addP(m.home, scaleP(off, m.cfg.DriftRadius/d))
	}
}

// randomUnit draws a direction. Rejection sampling inside the unit ball rather
// than three independent uniforms, because a cube of samples biases directions
// toward its corners; at a 0.52 acceptance rate the loop is cheap and bounded
// in practice.
func (m *Mover) randomUnit() protocol.Position {
	for i := 0; i < 16; i++ {
		p := protocol.Position{
			X: m.rng.Float64()*2 - 1,
			Y: m.rng.Float64()*2 - 1,
			Z: m.rng.Float64()*2 - 1,
		}
		if l := lenP(p); l > 1e-6 && l <= 1 {
			return scaleP(p, 1/l)
		}
	}
	return protocol.Position{X: 1}
}

// headingOf is the course of a horizontal velocity, in degrees clockwise from
// north. It reports false for a vehicle that is not moving horizontally, whose
// heading is undefined rather than zero (zero is due north, a lie).
func headingOf(vel protocol.Position) (float64, bool) {
	if math.Hypot(vel.X, vel.Y) < 1e-9 {
		return 0, false
	}
	// X is east and Y is north, so the arguments are the other way round from
	// the usual atan2(y, x): a heading is measured from north, clockwise.
	deg := math.Atan2(vel.X, vel.Y) * 180 / math.Pi
	if deg < 0 {
		deg += 360
	}
	return deg, true
}

// --- vector helpers ------------------------------------------------------------
//
// protocol.Position is a plain struct of three float64s shared with the wire
// format, so it has no methods; these keep the movement maths readable without
// adding geometry to the protocol package.

func addP(a, b protocol.Position) protocol.Position {
	return protocol.Position{X: a.X + b.X, Y: a.Y + b.Y, Z: a.Z + b.Z}
}

func subP(a, b protocol.Position) protocol.Position {
	return protocol.Position{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}

func scaleP(a protocol.Position, k float64) protocol.Position {
	return protocol.Position{X: a.X * k, Y: a.Y * k, Z: a.Z * k}
}

func lenP(a protocol.Position) float64 {
	return math.Sqrt(a.X*a.X + a.Y*a.Y + a.Z*a.Z)
}

// normP returns a unit vector, or the zero vector for input too short to have a
// direction. Returning zero rather than NaN matters: a NaN position propagates
// into the projection and the vehicle vanishes from the map with no clue why.
func normP(a protocol.Position) protocol.Position {
	l := lenP(a)
	if l < 1e-12 || math.IsNaN(l) || math.IsInf(l, 0) {
		return protocol.Position{}
	}
	return scaleP(a, 1/l)
}
