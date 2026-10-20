package mavlink

import (
	"math"
	"time"

	"swarm-net/pkg/protocol"
)

// The flight instruments: VFR_HUD and ATTITUDE.
//
// Both are derived from the Mover, and only from the Mover. Every number here
// is a function of the position the swarm already has and the velocity the
// mover measured over the last step, so the HUD cannot disagree with the dot on
// the map -- which is the whole reason for not generating them independently.
//
// Like everything in health.go, these are synthesised for the ground station.
// See the honesty section in doc.go.

// Attitude model. Kinematic, NOT dynamic: there is no mass, no thrust and no
// inertia anywhere in this package. The attitude is read BACKWARDS off the
// motion the mover has already performed, using the relations a multirotor
// would have had to obey to perform it:
//
//   - A multirotor accelerates by tilting, so pitch is nose-down in proportion
//     to how fast it is going.
//   - A turn at speed v and yaw rate omega needs a bank of atan(v*omega/g) to
//     be coordinated, so that is the bank reported.
//
// A real airframe's attitude LEADS its velocity (it tilts, then accelerates).
// This one lags it by a smoothing time constant, because the velocity is the
// input. Nothing a ground station shows depends on the difference, and inverting
// it would mean a controller, a model and a tuning problem for a vehicle that
// does not exist.
const (
	// attMaxPitchRad is the nose-down pitch at full commanded speed. 12 degrees
	// is a brisk but unremarkable cruise attitude for a multirotor.
	attMaxPitchRad = 12 * math.Pi / 180
	// attMaxRollRad caps the bank, so a mover that reverses direction in one
	// step (a clock jump, an adopted position) cannot roll the vehicle past
	// knife-edge on the HUD.
	attMaxRollRad = 30 * math.Pi / 180
	// attSmoothing is the blend rate, per second, that roll and pitch ease
	// toward their target. 4 per second settles in about a quarter of a second:
	// fast enough to look responsive, slow enough that a single noisy drift
	// step does not flick the horizon.
	attSmoothing = 4.0
	// gravity, for the coordinated-turn bank. m/s^2.
	gravity = 9.80665
)

// attitudeState is one vehicle's attitude, in radians. Loop-owned, like the
// Mover it is derived from.
//
// It is state rather than a pure function of the current velocity because the
// rates (rollspeed, pitchspeed, yawspeed) are differences across a step, and
// because the smoothing needs somewhere to remember what it was smoothing from.
type attitudeState struct {
	roll, pitch, yaw                float64
	rollspeed, pitchspeed, yawspeed float64
}

// step updates the attitude from the motion the mover just performed.
//
// maxSpeed is the configured flight speed in cube units per second, used only
// to scale pitch: it is what "full speed" means for this swarm, so a vehicle on
// a mission leg sits at attMaxPitchRad and a drifting one is nearly level.
func (a *attitudeState) step(dt time.Duration, m *Mover, maxSpeed, metresPerUnit float64) {
	if dt <= 0 {
		return
	}
	secs := dt.Seconds()
	vel := m.Vel()
	speedUnits := math.Hypot(vel.X, vel.Y)

	// Yaw follows the course exactly. A multirotor can fly sideways, so course
	// and heading need not agree -- but the mover has no notion of a nose
	// direction, and a yaw that disagreed with the direction of travel would
	// make every vehicle on the map point the wrong way.
	yawWant := wrapPi(m.Heading() * math.Pi / 180)
	// The yaw rate is the SHORTEST turn between the two angles, not their
	// difference: a course that crosses north goes from +179 to -179 degrees,
	// and the naive difference reports a 358-degree-per-step snap roll.
	dYaw := wrapPi(yawWant - a.yaw)
	a.yawspeed = dYaw / secs
	a.yaw = yawWant

	// Coordinated-turn bank: tan(roll) = v * omega / g, with v in m/s.
	bank := math.Atan2(speedUnits*metresPerUnit*a.yawspeed, gravity)
	bank = math.Max(-attMaxRollRad, math.Min(attMaxRollRad, bank))

	// Nose down in proportion to speed. Negative: MAVLink's pitch is positive
	// nose-up, and a multirotor that is moving forward is tilted forward.
	frac := 0.0
	if maxSpeed > 0 {
		frac = math.Min(1, speedUnits/maxSpeed)
	}
	pitchWant := -attMaxPitchRad * frac

	// Ease, capped at 1 so a long dt settles instead of overshooting into an
	// inverted attitude.
	blend := math.Min(1, attSmoothing*secs)
	prevRoll, prevPitch := a.roll, a.pitch
	a.roll += (bank - a.roll) * blend
	a.pitch += (pitchWant - a.pitch) * blend
	// Rates are the achieved change, not the commanded one, for the same reason
	// Mover.Vel is the measured displacement: a rate that disagrees with the
	// angle beside it looks like a broken estimator.
	a.rollspeed = (a.roll - prevRoll) / secs
	a.pitchspeed = (a.pitch - prevPitch) / secs
}

// wrapPi folds an angle into [-pi, pi], which is the range ATTITUDE's fields
// are documented to use. A ground station that is handed 6.2 radians of yaw
// draws the compass at nearly a full turn past north.
func wrapPi(rad float64) float64 {
	rad = math.Mod(rad+math.Pi, 2*math.Pi)
	if rad < 0 {
		rad += 2 * math.Pi
	}
	return rad - math.Pi
}

// attitude is the ATTITUDE message for v's current attitude state.
func (b *Bridge) attitude(v *vehicle, now time.Time) *Attitude {
	if l := b.liveFor(v); l != nil {
		roll, pitch, yaw := l.s.AttitudeRad()
		return &Attitude{
			TimeBootMs: uint32(now.Sub(b.boot).Milliseconds()),
			Roll:       float32(roll),
			Pitch:      float32(pitch),
			Yaw:        float32(yaw),
			Rollspeed:  float32(l.rollRate),
			Pitchspeed: float32(l.pitchRate),
			Yawspeed:   float32(l.yawRate),
		}
	}
	return &Attitude{
		TimeBootMs: uint32(now.Sub(b.boot).Milliseconds()),
		Roll:       float32(v.att.roll),
		Pitch:      float32(v.att.pitch),
		Yaw:        float32(v.att.yaw),
		Rollspeed:  float32(v.att.rollspeed),
		Pitchspeed: float32(v.att.pitchspeed),
		Yawspeed:   float32(v.att.yawspeed),
	}
}

// Throttle model, for the HUD's throttle bar: a hover percentage plus whatever
// the commanded speed adds. It is the battery's draw expressed as a bar, and
// the two are consistent on purpose.
const (
	hoverThrottlePct = 35
	maxThrottlePct   = 85
)

// vfrHud is the Fly view's instrument panel: speed, heading, altitude, climb.
//
// airspeed EQUALS groundspeed here, and that is a statement rather than
// laziness: there is no wind model in this simulation, so the two cannot
// differ. A ground station shows both, and inventing a difference would be
// inventing a wind nobody could measure.
func (b *Bridge) vfrHud(v *vehicle) *VfrHud {
	vel := v.mover.Vel()
	k := b.cfg.Projection.MetresPerUnit
	ground := math.Hypot(vel.X, vel.Y) * k
	_, _, altM := b.cfg.Projection.ProjectFloat(v.mover.Pos())

	frac := 0.0
	if b.cfg.Move.SpeedUnitsPerSec > 0 {
		frac = math.Min(1, math.Hypot(vel.X, vel.Y)/b.cfg.Move.SpeedUnitsPerSec)
	}
	// A vehicle that is not being flown at all still hovers, so the throttle
	// bar never reads zero while the vehicle is in the air; a reader who sees
	// 0% concludes the motors have stopped.
	throttle := hoverThrottlePct + (maxThrottlePct-hoverThrottlePct)*frac

	// heading is whole degrees in [0, 360) in an int16 field, which is the one
	// MAVLink heading unit that is NOT centidegrees -- GLOBAL_POSITION_INT.hdg
	// and GPS_RAW_INT.cog both are.
	hdg := int16(math.Round(v.mover.Heading())) % 360
	if hdg < 0 {
		hdg += 360
	}
	climb := vel.Z * k
	if l := b.liveFor(v); l != nil {
		// The drone's own throttle and range-sensor height. Ground and air
		// speed stay the swarm's: there is nothing on the drone to measure them.
		_, _, groundM := b.cfg.Projection.ProjectFloat(protocol.Position{})
		altM = groundM + l.altM
		throttle = l.s.ThrottlePercent()
		climb = l.climb
	}
	return &VfrHud{
		Airspeed:    float32(ground),
		Groundspeed: float32(ground),
		Heading:     hdg,
		Throttle:    uint16(math.Round(throttle)),
		// alt is altitude above mean sea level, per the message definition, so
		// it is the projected altitude rather than the height above the ground
		// plane that GLOBAL_POSITION_INT.relative_alt carries.
		Alt: float32(altM),
		// Positive is UP in VFR_HUD, unlike the NED velocities in
		// GLOBAL_POSITION_INT, so the cube's Z needs no sign flip here.
		Climb: float32(climb),
	}
}
