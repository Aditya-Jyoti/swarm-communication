package mavlink

import (
	"math"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// TestBridgePositionBurstShape pins the fast stream. All three come from one
// movement step, so a ground station cannot read a groundspeed from one tick
// beside a position from the next.
func TestBridgePositionBurstShape(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	b.clock.advance(b.cfg.PositionInterval)
	b.tickPositions()

	want := []MsgID{MsgIDGlobalPositionInt, MsgIDAttitude, MsgIDVfrHud}
	if len(want) != positionBurst {
		t.Fatalf("this test expects %d messages, positionBurst says %d", len(want), positionBurst)
	}
	got := idsOf(b.gcs.recvN(positionBurst))
	b.gcs.expectQuiet()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position tick = %v, want %v", got, want)
		}
	}
}

// flyFor sends a vehicle at target and runs the position tick until it has been
// moving steadily, returning the bridge and the vehicle. "Steadily" matters for
// the attitude: roll and pitch ease toward their target over attSmoothing, so
// one step is a transient.
func flyFor(t *testing.T, start, target protocol.Position, ticks int, mutate func(*BridgeConfig)) (*testBridge, *vehicle) {
	t.Helper()
	swarm := newFakeSwarm(node("swarm-net-node-1", false, true, start))
	b := newTestBridge(t, swarm, func(c *BridgeConfig) {
		// Idle motion off, so every number below comes from the commanded
		// flight and not from a random walk.
		c.Move.Idle = IdleStill
		c.Move.SpeedUnitsPerSec = 3
		if mutate != nil {
			mutate(c)
		}
	})
	b.sync(t)
	v := b.vs["swarm-net-node-1"]
	if target != start {
		v.mover.SetTarget(target)
	}
	for i := 0; i < ticks; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
	}
	b.gcs.flush()
	return b, v
}

func TestBridgeVfrHud(t *testing.T) {
	// 3 units/s at the default 20 m per unit is 60 m/s.
	const cruise = 3 * DefaultMetresPerUnit

	tests := []struct {
		name            string
		start, target   protocol.Position
		wantSpeed       float64
		wantClimb       float64
		wantHeading     int16
		wantThrottlePct uint16
	}{
		// A parked vehicle still hovers: a throttle bar at 0% reads as motors
		// stopped, which is not what a holding vehicle looks like.
		{"holding", at(50, 50, 10), at(50, 50, 10), 0, 0, 0, hoverThrottlePct},
		{"cruising north", at(50, 10, 10), at(50, 90, 10), cruise, 0, 0, maxThrottlePct},
		{"cruising east", at(10, 50, 10), at(90, 50, 10), cruise, 0, 90, maxThrottlePct},
		// Positive climb is UP in VFR_HUD, unlike the NED velocities in
		// GLOBAL_POSITION_INT. Heading is held through a vertical-only move,
		// because a climbing vehicle still points somewhere.
		{"climbing", at(50, 50, 10), at(50, 50, 90), 0, cruise, 0, hoverThrottlePct},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, v := flyFor(t, tc.start, tc.target, 10, nil)
			b.clock.advance(b.cfg.PositionInterval)
			b.tickPositions()
			hud := b.gcs.recvID(MsgIDVfrHud).Msg.(*VfrHud)

			if math.Abs(float64(hud.Groundspeed)-tc.wantSpeed) > 1e-3 {
				t.Errorf("groundspeed = %v m/s, want %v", hud.Groundspeed, tc.wantSpeed)
			}
			// No wind model exists, so the two cannot differ. Inventing a
			// difference would be inventing a wind nobody can measure.
			if hud.Airspeed != hud.Groundspeed {
				t.Errorf("airspeed %v != groundspeed %v with no wind model", hud.Airspeed, hud.Groundspeed)
			}
			if math.Abs(float64(hud.Climb)-tc.wantClimb) > 1e-3 {
				t.Errorf("climb = %v m/s, want %v", hud.Climb, tc.wantClimb)
			}
			if hud.Heading != tc.wantHeading {
				t.Errorf("heading = %d deg, want %d", hud.Heading, tc.wantHeading)
			}
			if hud.Throttle != tc.wantThrottlePct {
				t.Errorf("throttle = %d%%, want %d%%", hud.Throttle, tc.wantThrottlePct)
			}
			// alt is altitude above mean sea level, per the message
			// definition, not the height above the ground plane that
			// GLOBAL_POSITION_INT.relative_alt carries.
			_, _, wantAlt := b.cfg.Projection.ProjectFloat(v.mover.Pos())
			if math.Abs(float64(hud.Alt)-wantAlt) > 0.5 {
				t.Errorf("alt = %v m AMSL, want %v", hud.Alt, wantAlt)
			}
		})
	}
}

func TestBridgeAttitudeFromMotion(t *testing.T) {
	tests := []struct {
		name          string
		start, target protocol.Position
		// wantYawDeg is the course, which the yaw follows exactly: the mover
		// has no notion of a nose direction, and a yaw that disagreed with the
		// direction of travel would point every vehicle the wrong way.
		wantYawDeg float64
		// wantPitch is nose-down in proportion to horizontal speed, so a
		// vehicle at full commanded speed sits at the cap.
		wantPitch float64
	}{
		{"holding", at(50, 50, 10), at(50, 50, 10), 0, 0},
		{"cruising north", at(50, 10, 10), at(50, 90, 10), 0, -attMaxPitchRad},
		{"cruising east", at(10, 50, 10), at(90, 50, 10), 90, -attMaxPitchRad},
		{"cruising west", at(90, 50, 10), at(10, 50, 10), 270, -attMaxPitchRad},
		// A climb is not horizontal motion, so the vehicle stays level.
		{"climbing", at(50, 50, 10), at(50, 50, 90), 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Twenty ticks is four seconds, many times the smoothing constant.
			b, _ := flyFor(t, tc.start, tc.target, 20, nil)
			b.clock.advance(b.cfg.PositionInterval)
			b.tickPositions()
			att := b.gcs.recvID(MsgIDAttitude).Msg.(*Attitude)

			if got := float64(att.Yaw); math.Abs(got-wrapPi(tc.wantYawDeg*math.Pi/180)) > 1e-5 {
				t.Errorf("yaw = %v rad, want %v deg", got, tc.wantYawDeg)
			}
			if got := float64(att.Pitch); math.Abs(got-tc.wantPitch) > 1e-3 {
				t.Errorf("pitch = %v rad, want %v", got, tc.wantPitch)
			}
			// Straight and level: no bank, and no residual rates to make a
			// ground station's horizon twitch.
			if math.Abs(float64(att.Roll)) > 1e-3 || math.Abs(float64(att.Rollspeed)) > 1e-2 {
				t.Errorf("flying straight with roll %v rad at %v rad/s", att.Roll, att.Rollspeed)
			}
			// Yaw is reported in [-pi, pi]. A station handed 6.2 rad draws the
			// compass nearly a full turn past north.
			if math.Abs(float64(att.Yaw)) > math.Pi {
				t.Errorf("yaw %v is outside [-pi, pi]", att.Yaw)
			}
		})
	}
}

func TestBridgeAttitudeBanksIntoTurns(t *testing.T) {
	tests := []struct {
		name string
		// first and second are legs; the turn between them is what is being
		// measured.
		first, second protocol.Position
		// wantSign is the sign of the roll: positive is right wing down, and a
		// turn to the right banks right.
		wantSign float64
	}{
		{"right turn, north to east", at(50, 90, 10), at(90, 90, 10), +1},
		{"left turn, east to north", at(90, 50, 10), at(90, 90, 10), -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, v := flyFor(t, at(50, 50, 10), tc.first, 10, nil)
			// Steady on the first leg before the turn, or the bank measured
			// below would include the roll-in from the start of the flight.
			if math.Abs(v.att.roll) > 1e-3 {
				t.Fatalf("not settled before the turn: roll %v", v.att.roll)
			}
			v.mover.SetTarget(tc.second)
			b.clock.advance(b.cfg.PositionInterval)
			b.tickPositions()
			att := b.gcs.recvID(MsgIDAttitude).Msg.(*Attitude)

			roll := float64(att.Roll)
			if roll*tc.wantSign <= 0 {
				t.Errorf("roll = %v rad, want the sign of %v", roll, tc.wantSign)
			}
			// Capped, so a mover that reverses in one step -- a clock jump, an
			// adopted position -- cannot roll the vehicle past knife-edge.
			if math.Abs(roll) > attMaxRollRad+1e-9 {
				t.Errorf("roll = %v rad, over the %v cap", roll, attMaxRollRad)
			}
		})
	}
}

// TestAttitudeYawRateTakesTheShortWay is the bug this would otherwise have: a
// course crossing north goes from +179 to -179 degrees, and the naive
// difference reports a 358-degree-per-step snap roll, which banks the vehicle
// to its cap in the wrong direction.
func TestAttitudeYawRateTakesTheShortWay(t *testing.T) {
	m := NewMover("seed", protocol.Position{X: 50, Y: 50, Z: 10}, MoveConfig{Idle: IdleStill, SpeedUnitsPerSec: 3})
	a := attitudeState{}

	// North-north-west, then north-north-east: two degrees apart across the
	// discontinuity.
	for _, target := range []protocol.Position{{X: 49, Y: 90, Z: 10}, {X: 51, Y: 90, Z: 10}} {
		m.SetTarget(target)
		for i := 0; i < 10; i++ {
			m.Step(200 * time.Millisecond)
			a.step(200*time.Millisecond, m, 3, DefaultMetresPerUnit)
		}
		m.Adopt(protocol.Position{X: 50, Y: 50, Z: 10})
	}
	// The yaw rate over that step is a couple of degrees per second, not
	// hundreds.
	if math.Abs(a.yawspeed) > 1 {
		t.Errorf("yawspeed = %v rad/s across north, want a small rate", a.yawspeed)
	}
	if math.Abs(a.roll) > attMaxRollRad {
		t.Errorf("roll = %v rad, over the cap", a.roll)
	}
}

func TestWrapPi(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero", 0, 0},
		{"north-east", math.Pi / 4, math.Pi / 4},
		{"just past south", math.Pi + 0.1, -math.Pi + 0.1},
		{"full turn", 2 * math.Pi, 0},
		{"two turns and a bit", 4*math.Pi + 0.5, 0.5},
		{"negative", -3 * math.Pi / 2, math.Pi / 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapPi(tc.in)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("wrapPi(%v) = %v, want %v", tc.in, got, tc.want)
			}
			if got > math.Pi || got < -math.Pi {
				t.Errorf("wrapPi(%v) = %v, outside [-pi, pi]", tc.in, got)
			}
		})
	}
}

// TestAttitudeIgnoresNonPositiveDT: a clock that went backwards must not
// produce an infinite rate, which is what dividing by a negative dt would hand
// a ground station.
func TestAttitudeIgnoresNonPositiveDT(t *testing.T) {
	m := NewMover("seed", protocol.Position{X: 50, Y: 50, Z: 10}, MoveConfig{SpeedUnitsPerSec: 3})
	a := attitudeState{roll: 0.1, pitch: -0.2, yaw: 1.5}
	before := a
	a.step(0, m, 3, DefaultMetresPerUnit)
	a.step(-time.Second, m, 3, DefaultMetresPerUnit)
	if a != before {
		t.Errorf("attitude changed on non-positive dt: %+v -> %+v", before, a)
	}
}
