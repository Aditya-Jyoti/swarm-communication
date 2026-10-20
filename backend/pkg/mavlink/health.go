package mavlink

import (
	"math"
	"time"

	"swarm-net/pkg/protocol"
)

// Health and reference telemetry: the messages a ground station needs before it
// will call a vehicle ready to fly.
//
// # Read this first
//
// Almost nothing in this file is a measurement. The swarm has no airframe: no
// IMU, no magnetometer, no GPS receiver and no battery. What it has is a
// position, a role, a liveness flag and two counters. A ground station,
// however, decides whether to show a vehicle as flyable by looking for sensor
// health bits, a GPS fix and a power source, and shows a grey "not ready" bar
// and no HUD when they are absent -- so the bridge synthesises them.
//
// They are synthesised CONSISTENTLY rather than plausibly at random: the GPS
// position is the same projection GLOBAL_POSITION_INT carries, the battery
// drains from the same motion the Mover reports, and a node the swarm calls
// degraded withdraws the one health bit that corresponds. That is the most this
// can honestly be. See doc.go and section 9 of
// docs/architecture/mavlink-bridge.md for the field-by-field split between real
// swarm state and ground-station fiction.

// sensorsPresent is the sensor bitmap the bridge claims, and it is a claim
// about an airframe rather than a formality.
//
// A ground station reads the three bitmaps as present / enabled / healthy and
// reports a sensor that is present and unhealthy in red, a sensor that is
// absent as no opinion at all, and a vehicle with NOTHING present as "not
// ready" with a grey health bar -- which is what the bridge used to send, and
// why nobody could tell a healthy swarm from a broken one in QGroundControl.
//
// The set is what a multirotor is expected to have, and no more: a gyro,
// accelerometer, magnetometer and barometer; GPS; the attitude, yaw, altitude
// and horizontal position controllers; motor outputs; a battery. Not claimed,
// deliberately: differential pressure (no pitot on a multirotor), optical flow,
// RC receiver (there is no pilot), geofence, terrain, logging and the prearm
// check. Claiming a sensor the bridge cannot say anything about would mean
// reporting a health bit it has no evidence for.
const sensorsPresent = MavSysStatusSensor3dGyro |
	MavSysStatusSensor3dAccel |
	MavSysStatusSensor3dMag |
	MavSysStatusSensorAbsolutePressure |
	MavSysStatusSensorGPS |
	MavSysStatusSensorAngularRateControl |
	MavSysStatusSensorAttitudeStabilization |
	MavSysStatusSensorYawPosition |
	MavSysStatusSensorZAltitudeControl |
	MavSysStatusSensorXyPositionControl |
	MavSysStatusSensorMotorOutputs |
	MavSysStatusSensorBattery

// degradedSensors is what a degraded node withdraws from its health bitmap.
//
// Degraded means the swarm has chaos "delay" injected on that node
// (TelemetryPayload.Degraded), so its reports arrive late. The bit withdrawn is
// GPS, because the sensor whose job is position is the closest analogue to a
// node whose position reports cannot be trusted to be current -- and because
// gps_raw_int drops to a 2D fix in the same tick, so the two messages agree.
//
// ONE bit, not the whole bitmap. A vehicle with every sensor unhealthy is
// indistinguishable from a vehicle that has crashed, and the thing an operator
// needs to see is which node is struggling and in what way. Dropping the whole
// map would also make the kill demo unreadable: a killed node stops
// heartbeating altogether, and that has to look different from a slow one.
const degradedSensors = MavSysStatusSensorGPS

// Battery model. A 6S lithium-polymer pack, which is what a drone of this size
// flies, sized so that a demo watches it fall rather than sit at 100%.
const (
	batteryCells = 6
	// batteryCapacityMAh at batteryIdleAmps gives about 42 minutes from full to
	// empty: slow enough that nothing dies mid-demo, fast enough that the
	// percentage visibly moves.
	batteryCapacityMAh = 10000.0
	// batteryIdleAmps is hover draw, batteryFlightAmps the draw at full
	// commanded speed. Interpolated by speed, so a vehicle flying a mission
	// drains faster than one holding station -- which is the only part of this
	// model that carries real information.
	batteryIdleAmps   = 14.0
	batteryFlightAmps = 26.0
	// Cell voltages at full and empty. 3.3 V is a sensible floor; a real pack
	// would not be taken below it.
	cellFullVolts  = 4.2
	cellEmptyVolts = 3.3
	// cellResistanceOhms produces voltage sag under load, so the pack reads
	// lower while flying and recovers on the hover. Without it the voltage is a
	// pure function of the percentage and says nothing extra.
	cellResistanceOhms = 0.004
)

// battery is one vehicle's synthetic pack. Loop-owned, like the Mover it is
// driven from; it carries no mutex for the same reason.
//
// There is exactly one piece of state, the charge drawn so far, and it starts
// at zero. A restarted node is a new vehicle (see Bridge.applyNodes), so its
// pack is full again -- which is honest about what the model is: it tracks the
// bridge's uptime for that node, not an airframe's history.
type battery struct {
	consumedMAh float64
	// amps is the draw over the last step, kept so that SYS_STATUS and
	// BATTERY_STATUS report the same current in the same tick.
	amps float64
}

// step drains the pack for dt at a draw interpolated by speed.
//
// speedFrac is horizontal speed as a fraction of the configured flight speed,
// clamped to [0, 1] by the caller's own maths rather than trusted: a mover
// stepped with a long dt can report a speed above the commanded one for a
// single tick, and a current that spikes with it would look like a fault.
func (b *battery) step(dt time.Duration, speedFrac float64) {
	if dt <= 0 {
		return
	}
	speedFrac = math.Max(0, math.Min(1, speedFrac))
	b.amps = batteryIdleAmps + (batteryFlightAmps-batteryIdleAmps)*speedFrac
	b.consumedMAh += b.amps * 1000 * dt.Hours()
	if b.consumedMAh > batteryCapacityMAh {
		b.consumedMAh = batteryCapacityMAh // flat, not negative remaining
	}
}

// remaining is the state of charge in percent, [0, 100].
func (b *battery) remaining() float64 {
	return math.Max(0, 100*(1-b.consumedMAh/batteryCapacityMAh))
}

// voltageMV is pack voltage in millivolts: a linear curve in state of charge,
// less the sag from the current draw.
//
// Linear, not a real discharge curve. A lithium pack's plateau-then-knee shape
// would make the voltage barely move for most of the flight and then collapse,
// which is MORE realistic and LESS informative: the straight line is readable
// as a fuel gauge, and the honest statement is in the doc rather than in a
// curve nobody can check.
func (b *battery) voltageMV() uint16 {
	soc := b.remaining() / 100
	perCell := cellEmptyVolts + (cellFullVolts-cellEmptyVolts)*soc
	volts := float64(batteryCells) * (perCell - b.amps*cellResistanceOhms)
	return saturateU16(uint64(math.Max(0, math.Round(volts*1000))))
}

// currentCA is the draw in centiamps, BATTERY_STATUS's and SYS_STATUS's unit.
func (b *battery) currentCA() int16 {
	return saturateI16(math.Round(b.amps * 100))
}

// chargeState maps the percentage onto MAV_BATTERY_CHARGE_STATE; see
// chargeStateOf, which the live vehicle shares.
func (b *battery) chargeState() uint8 { return chargeStateOf(b.remaining()) }

// batteryStatus is the full per-pack report.
//
// The cell voltages array carries the pack voltage divided by the cell count in
// voltages[0..5] and UINT16_MAX ("not reported") in the rest, which is the
// convention for a pack with no per-cell monitoring. Putting the whole pack
// voltage in voltages[0] -- also legal -- makes a ground station draw a 25 V
// cell.
func (b *Bridge) batteryStatus(v *vehicle) *BatteryStatus {
	if l := b.liveFor(v); l != nil {
		return liveBatteryStatus(l)
	}
	m := &BatteryStatus{
		ID:               0,
		BatteryFunction:  MavBatteryFunctionAll,
		Type:             MavBatteryTypeLipo,
		Temperature:      math.MaxInt16, // no temperature sensor to invent
		CurrentBattery:   v.batt.currentCA(),
		CurrentConsumed:  int32(math.Round(v.batt.consumedMAh)),
		EnergyConsumed:   -1, // "not provided"
		BatteryRemaining: int8(math.Round(v.batt.remaining())),
		TimeRemaining:    v.batt.timeRemainingS(),
		ChargeState:      v.batt.chargeState(),
	}
	cell := v.batt.voltageMV() / batteryCells
	for i := range m.Voltages {
		if i < batteryCells {
			m.Voltages[i] = cell
			continue
		}
		m.Voltages[i] = math.MaxUint16
	}
	for i := range m.VoltagesExt {
		// The extension array is "0 if not used", not UINT16_MAX: a non-zero
		// entry here would add 4 more cells to the pack.
		m.VoltagesExt[i] = 0
	}
	return m
}

// timeRemainingS is the endurance left at the current draw, in seconds, or 0
// for "not provided" when nothing is being drawn yet.
func (b *battery) timeRemainingS() int32 {
	if b.amps <= 0 {
		return 0
	}
	hours := (batteryCapacityMAh - b.consumedMAh) / 1000 / b.amps
	return int32(math.Round(hours * 3600))
}

// GPS model. A receiver with a good sky view: a 3D fix and a dozen or more
// satellites, degrading to a 2D fix on a node the swarm reports as degraded.
const (
	gpsSatsMin = 12
	gpsSatsMax = 16
	// gpsSatCyclePeriod is how often the satellite count steps. A count pinned
	// to one value for an hour is one of the tells that telemetry is
	// synthetic; stepping it slowly looks like a receiver and costs nothing.
	gpsSatCyclePeriod = 17 * time.Second
	// gpsBaseEphCm is the horizontal dilution of position in hundredths, for a
	// receiver at gpsSatsMax. 0.6 HDOP with 16 satellites is about what a
	// modern module reports in the open.
	gpsBaseEphCm = 60
	// gpsEphPerLostSat worsens the dilution as the count falls.
	gpsEphPerLostSat = 10
	// gpsDegradedEphFactor is how much worse the dilution gets on a degraded
	// node, which also drops to a 2D fix.
	gpsDegradedEphFactor = 3
)

// gpsRawInt is the receiver's view of the same position GLOBAL_POSITION_INT
// carries.
//
// The SAME projection, not an independently fuzzed one. Adding noise would be
// more lifelike and would break the thing the message is for: a ground station
// draws the GPS position and the fused position in the same place and reports a
// "position estimate" failure when they disagree.
func (b *Bridge) gpsRawInt(v *vehicle, now time.Time) *GPSRawInt {
	g := b.cfg.Projection.Project(v.mover.Pos())
	sats := b.gpsSats(v, now)
	eph := gpsBaseEphCm + gpsEphPerLostSat*(gpsSatsMax-int(sats))
	fix := uint8(GPSFixType3dFix)
	if v.degraded {
		fix = GPSFixType2dFix
		eph *= gpsDegradedEphFactor
	}
	vel := v.mover.Vel()
	k := b.cfg.Projection.MetresPerUnit
	hdg := uint16(math.Round(v.mover.Heading() * 100))
	if hdg >= 36000 {
		hdg = 0
	}
	return &GPSRawInt{
		// Unix microseconds. A ground station infers the epoch from the
		// magnitude, and a time since boot would make it think the receiver has
		// no time solution.
		TimeUsec:          uint64(now.UnixMicro()),
		Lat:               g.LatE7,
		Lon:               g.LonE7,
		Alt:               g.AltMM,
		Eph:               saturateU16(uint64(eph)),
		Epv:               saturateU16(uint64(eph) + 40), // vertical is always worse
		Vel:               saturateU16(uint64(math.Round(math.Hypot(vel.X, vel.Y) * k * 100))),
		COG:               hdg,
		FixType:           fix,
		SatellitesVisible: sats,
	}
}

// gpsSats is the satellite count: a per-node base so two vehicles do not report
// the same number, stepped slowly so it is not frozen.
//
// A degraded node loses four, which is the difference between a 3D and a 2D fix
// on a real receiver and keeps this message consistent with the GPS health bit
// SYS_STATUS withdraws in the same tick.
func (b *Bridge) gpsSats(v *vehicle, now time.Time) uint8 {
	span := uint64(gpsSatsMax - gpsSatsMin + 1)
	cycle := uint64(now.Sub(b.boot) / gpsSatCyclePeriod)
	sats := gpsSatsMin + int(((seedOf(v.id)>>8)+cycle)%span)
	if v.degraded {
		sats -= 4
	}
	return uint8(sats)
}

// extendedSysState reports airborne-or-not.
//
// ON_GROUND is the ground plane, cube z = 0, and nothing else. A ground station
// uses the landed state to decide whether to offer Takeoff or Land, so a
// vehicle that is at altitude and claims to be on the ground offers the wrong
// button -- and a swarm whose nodes all sit at z = 0 genuinely is on the
// ground, however much it drifts sideways.
//
// vtol_state is UNDEFINED: a multirotor has no transition to report, and
// MAV_VTOL_STATE_MC would claim a quadplane in copter mode.
func (b *Bridge) extendedSysState(v *vehicle) *ExtendedSysState {
	landed := uint8(MavLandedStateOnGround)
	if v.mover.Flying() || v.mover.Pos().Z > groundEpsilon {
		landed = MavLandedStateInAir
	}
	return &ExtendedSysState{
		VtolState:   MavVtolStateUndefined,
		LandedState: landed,
	}
}

// groundEpsilon is how close to z = 0 counts as on the ground, in cube units.
// A mission that lands a vehicle stops exactly on z = 0, so this only absorbs
// float64 arithmetic in the projection and the mover.
const groundEpsilon = 1e-9

// homePosition is the vehicle's home: where drift is centred and where Return
// flies to.
//
// Sent on the status tick rather than only on request. A ground station that
// has no home position shows no home marker and greys out Return, and it asks
// for one exactly once per connection -- so a station that reconnects
// mid-flight, or one that missed the reply, never asks again. Two small
// messages a second per vehicle is nothing beside the 5 Hz position stream, and
// it makes the marker appear by itself.
//
// The quaternion is identity and the approach vector is zero: there is no
// surveyed landing direction to report, and MAVLink's own convention for an
// unknown approach is a zero vector.
func (b *Bridge) homePosition(v *vehicle, now time.Time) *HomePosition {
	home := v.mover.Home()
	g := b.cfg.Projection.Project(home)
	k := b.cfg.Projection.MetresPerUnit
	return &HomePosition{
		Latitude:  g.LatE7,
		Longitude: g.LonE7,
		Altitude:  g.AltMM,
		// Local frame is NED in metres from the cube origin: x north, y east,
		// z DOWN. The cube's Z is up, hence the sign.
		X:        float32(home.Y * k),
		Y:        float32(home.X * k),
		Z:        float32(-home.Z * k),
		Q:        [4]float32{1, 0, 0, 0},
		TimeUsec: uint64(now.UnixMicro()),
	}
}

// gpsGlobalOrigin is the local frame's origin, which is cube (0, 0, 0).
//
// It is the same for every vehicle, because every vehicle's position comes from
// the same projection. Sending it is what lets a ground station convert the
// local NED coordinates in HOME_POSITION back to a map position and agree with
// the lat/lon in the same message; a station with no origin treats local
// coordinates as unusable.
func (b *Bridge) gpsGlobalOrigin(now time.Time) *GPSGlobalOrigin {
	g := b.cfg.Projection.Project(protocol.Position{})
	return &GPSGlobalOrigin{
		Latitude:  g.LatE7,
		Longitude: g.LonE7,
		Altitude:  g.AltMM,
		TimeUsec:  uint64(now.UnixMicro()),
	}
}

// bridgeCapabilities is what AUTOPILOT_VERSION advertises.
//
// MISSION_INT is the one that matters: a ground station that does not see it
// uploads missions with the float MISSION_ITEM message, whose lat/lon are
// float32 and therefore good to about 2 m at these latitudes. MISSION_FLOAT is
// advertised too because the bridge does accept the legacy messages
// (see Bridge.onMissionItem), and MAVLINK2 because every frame it sends is v2.
//
// COMMAND_INT is NOT advertised. The bridge handles COMMAND_LONG only, and a
// station told otherwise would send COMMAND_INT and get silence -- the one
// failure mode the command path exists to avoid. Advertising a capability is a
// promise about the inbound path, not a wish.
const bridgeCapabilities = MavProtocolCapabilityMissionInt |
	MavProtocolCapabilityMissionFloat |
	MavProtocolCapabilityMavlink2

// autopilotVersion identifies the "firmware".
//
// The version numbers are the bridge's, and the vendor, product and board ids
// are zero, which is MAVLink's "unknown hardware". A real vendor id here would
// make a ground station load that vendor's parameter metadata and flight-mode
// list and then report every one of them missing.
//
// The uid is derived from the node id, so a vehicle keeps the same serial
// number across restarts of the node and of the Control Center -- a ground
// station logs by uid, and a swarm whose uids reshuffled would produce a log
// nobody can join back to a node.
func (b *Bridge) autopilotVersion(v *vehicle) *AutopilotVersion {
	return &AutopilotVersion{
		Capabilities: bridgeCapabilities,
		UID:          seedOf(v.id),
		// major.minor.patch.type, most significant byte first.
		FlightSwVersion:     0x00_01_00_00,
		MiddlewareSwVersion: 0x00_01_00_00,
	}
}

// sensorHealth is the health bitmap for v: everything claimed present, less
// whatever a degraded node withdraws.
func sensorHealth(v *vehicle) uint32 {
	health := uint32(sensorsPresent)
	if v.degraded {
		health &^= degradedSensors
	}
	return health
}
