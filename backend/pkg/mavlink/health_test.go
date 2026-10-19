package mavlink

import (
	"math"
	"strconv"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// TestBridgeStatusBurstShape pins the 1 Hz stream: which messages, in which
// order, once each.
//
// It is worth a test of its own because every one of them is load-bearing in a
// ground station -- drop EXTENDED_SYS_STATE and the Takeoff button misbehaves,
// drop HOME_POSITION and Return greys out -- and because the order is the one
// an operator's screen fills in.
func TestBridgeStatusBurstShape(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	b.tickStatus()

	want := []MsgID{
		MsgIDHeartbeat, MsgIDSysStatus, MsgIDBatteryStatus,
		MsgIDGPSRawInt, MsgIDExtendedSysState,
		MsgIDHomePosition, MsgIDGPSGlobalOrigin,
	}
	if len(want) != statusBurst {
		t.Fatalf("this test expects %d messages, statusBurst says %d", len(want), statusBurst)
	}
	got := idsOf(b.gcs.recvN(statusBurst))
	b.gcs.expectQuiet()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status tick = %v, want %v", got, want)
		}
	}
}

func TestBridgeSensorHealth(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	tests := []struct {
		name     string
		degraded bool
		// want is the health bitmap, the whole claimed set less whatever this
		// node has withdrawn.
		want uint32
	}{
		{"healthy", false, sensorsPresent},
		{"degraded", true, sensorsPresent &^ degradedSensors},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swarm.set(NodeState{ID: "swarm-net-node-2", Connected: true, Pos: at(10, 10, 10), Degraded: tc.degraded})
			b.sync(t)
			b.tickStatus()
			st := b.gcs.recvID(MsgIDSysStatus).Msg.(*SysStatus)
			b.gcs.drain(statusBurst - 2)

			// Present and enabled never change: the sensors a vehicle has
			// fitted do not come and go, and a ground station that sees one
			// disappear reports a configuration change rather than a fault.
			if st.OnboardControlSensorsPresent != sensorsPresent ||
				st.OnboardControlSensorsEnabled != sensorsPresent {
				t.Errorf("present/enabled = %#x/%#x, want %#x twice",
					st.OnboardControlSensorsPresent, st.OnboardControlSensorsEnabled, uint32(sensorsPresent))
			}
			if st.OnboardControlSensorsHealth != tc.want {
				t.Errorf("health = %#x, want %#x", st.OnboardControlSensorsHealth, tc.want)
			}
		})
	}

	// The mapping is deliberately narrow: a degraded node must lose ONE bit,
	// not its whole bitmap, or it becomes indistinguishable from a crash.
	lost := uint32(sensorsPresent) &^ sensorHealth(&vehicle{degraded: true})
	if n := popcount(lost); n != 1 {
		t.Errorf("a degraded node withdrew %d health bits (%#x), want exactly 1", n, lost)
	}
	// And the claimed set is pinned. Adding a bit here is a claim about an
	// airframe that does not exist, so it should have to be made deliberately:
	// the twelve are a gyro, accelerometer, magnetometer and barometer, GPS,
	// four controllers, motor outputs and a battery. See sensorsPresent for
	// what is left out and why.
	if n := popcount(sensorsPresent); n != 12 {
		t.Errorf("sensorsPresent claims %d sensors (%#x), want the 12 a multirotor has",
			n, uint32(sensorsPresent))
	}
}

func popcount(v uint32) int {
	n := 0
	for ; v != 0; v >>= 1 {
		n += int(v & 1)
	}
	return n
}

func TestBridgeGPSRawInt(t *testing.T) {
	tests := []struct {
		name     string
		degraded bool
		wantFix  uint8
		minSats  uint8
		maxSats  uint8
	}{
		// A ground station shows no GPS lock, and refuses some commands,
		// without a 3D fix.
		{"healthy", false, GPSFixType3dFix, gpsSatsMin, gpsSatsMax},
		// A degraded node keeps a usable fix but a visibly worse one, matching
		// the GPS health bit SYS_STATUS withdraws in the same tick.
		{"degraded", true, GPSFixType2dFix, gpsSatsMin - 4, gpsSatsMax - 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos := at(25, 50, 1)
			swarm := newFakeSwarm(NodeState{ID: "swarm-net-node-9", Connected: true, Pos: pos, Degraded: tc.degraded})
			b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.Move.Idle = IdleStill })
			b.sync(t)
			b.tickStatus()
			g := b.gcs.recvID(MsgIDGPSRawInt).Msg.(*GPSRawInt)

			if g.FixType != tc.wantFix {
				t.Errorf("fix_type = %d, want %d", g.FixType, tc.wantFix)
			}
			if g.SatellitesVisible < tc.minSats || g.SatellitesVisible > tc.maxSats {
				t.Errorf("satellites = %d, want [%d, %d]", g.SatellitesVisible, tc.minSats, tc.maxSats)
			}
			// The receiver reports the SAME position the fused estimate does.
			// A ground station draws both and reports a position-estimate
			// failure when they disagree.
			want := DefaultProjection().Project(pos)
			if g.Lat != want.LatE7 || g.Lon != want.LonE7 || g.Alt != want.AltMM {
				t.Errorf("gps position = %d/%d/%d, want %+v", g.Lat, g.Lon, g.Alt, want)
			}
			// Dilution of position is in hundredths, so this is HDOP 0.5 to 5:
			// anything outside that is either implausibly perfect or no fix.
			if g.Eph < 50 || g.Eph > 500 || g.Epv <= g.Eph {
				t.Errorf("eph/epv = %d/%d, want a plausible pair with epv worse", g.Eph, g.Epv)
			}
		})
	}
}

func TestBridgeGPSSatellitesVaryOverTime(t *testing.T) {
	// A count pinned to one value for an hour is one of the tells that the
	// telemetry is synthetic.
	swarm := newFakeSwarm(node("swarm-net-node-3", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	v := b.vs["swarm-net-node-3"]

	seen := map[uint8]bool{}
	for i := 0; i < gpsSatsMax-gpsSatsMin+1; i++ {
		seen[b.gpsSats(v, b.clock.Now())] = true
		b.clock.advance(gpsSatCyclePeriod)
	}
	if len(seen) < 2 {
		t.Errorf("the satellite count never moved: %v", seen)
	}
	for sats := range seen {
		if sats < gpsSatsMin || sats > gpsSatsMax {
			t.Errorf("satellite count %d is outside [%d, %d]", sats, gpsSatsMin, gpsSatsMax)
		}
	}
}

func TestBridgeBatteryDrains(t *testing.T) {
	tests := []struct {
		name string
		// fly sends the vehicle on a long mission leg, so it drains at the
		// flight draw rather than the hover draw.
		fly bool
	}{
		{"hovering", false},
		{"flying", true},
	}
	drain := func(fly bool) (pct float64, mv uint16) {
		swarm := newFakeSwarm(node("swarm-net-node-1", false, true, at(50, 50, 10)))
		b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.Move.Idle = IdleStill })
		b.sync(t)
		v := b.vs["swarm-net-node-1"]
		if v.batt.remaining() != 100 {
			t.Fatalf("a fresh vehicle starts at %.1f%%, want a full pack", v.batt.remaining())
		}
		if fly {
			// Far enough that it is still flying at the end of the run: the
			// point is the draw, not the arrival. Horizontally, so the leg is
			// long -- the cube is 100 units across and only 100 tall.
			v.mover.SetTarget(at(99, 99, 10))
		}
		// Ten minutes at the position rate, driven off the injected clock.
		for i := 0; i < 10*60*5; i++ {
			b.clock.advance(b.cfg.PositionInterval)
			b.tickPositions()
		}
		return v.batt.remaining(), v.batt.voltageMV()
	}

	var hoverPct, flyPct float64
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pct, mv := drain(tc.fly)
			if tc.fly {
				flyPct = pct
			} else {
				hoverPct = pct
			}
			// Visibly moved, nowhere near flat: a demo must see the gauge fall
			// without a vehicle dying in the middle of it.
			if pct >= 100 || pct < 50 {
				t.Errorf("after ten minutes the pack is at %.1f%%, want a slow drain", pct)
			}
			// Voltage follows, and stays inside the pack's own range.
			full := uint16(batteryCells * cellFullVolts * 1000)
			empty := uint16(batteryCells * cellEmptyVolts * 1000)
			if mv >= full || mv <= empty-1000 {
				t.Errorf("pack voltage = %d mV, want between %d and %d", mv, empty, full)
			}
		})
	}
	// Flight costs more than a hover. This is the one part of the battery model
	// that carries real information: it is driven by the mover.
	if !(flyPct < hoverPct) {
		t.Errorf("flying drained to %.2f%% and hovering to %.2f%%; flight must cost more", flyPct, hoverPct)
	}
}

func TestBridgeBatteryAgreesWithSysStatus(t *testing.T) {
	// A ground station may read either message. Two numbers from two ticks
	// beside each other look like a failing pack.
	swarm := newFakeSwarm(node("swarm-net-node-1", false, true, at(50, 50, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	for i := 0; i < 600; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
	}
	b.gcs.flush()
	b.tickStatus()
	st := b.gcs.recvID(MsgIDSysStatus).Msg.(*SysStatus)
	bs := b.gcs.recvID(MsgIDBatteryStatus).Msg.(*BatteryStatus)

	if st.BatteryRemaining != bs.BatteryRemaining || st.CurrentBattery != bs.CurrentBattery {
		t.Errorf("SYS_STATUS says %d%% at %d cA, BATTERY_STATUS says %d%% at %d cA",
			st.BatteryRemaining, st.CurrentBattery, bs.BatteryRemaining, bs.CurrentBattery)
	}
	// The cell array must describe a 6S pack, not one 25 V cell: a ground
	// station draws per-cell voltages when they are present.
	var sum uint32
	for i := 0; i < batteryCells; i++ {
		if bs.Voltages[i] < 3000 || bs.Voltages[i] > 4300 {
			t.Errorf("cell %d = %d mV, which is not a lithium cell", i, bs.Voltages[i])
		}
		sum += uint32(bs.Voltages[i])
	}
	for i := batteryCells; i < len(bs.Voltages); i++ {
		if bs.Voltages[i] != math.MaxUint16 {
			t.Errorf("cell %d = %d, want UINT16_MAX for a cell that is not there", i, bs.Voltages[i])
		}
	}
	if diff := int(sum) - int(st.VoltageBattery); diff > batteryCells || diff < -batteryCells {
		t.Errorf("cells sum to %d mV, pack reads %d mV", sum, st.VoltageBattery)
	}
	if bs.ChargeState != MavBatteryChargeStateOk {
		t.Errorf("charge_state = %d after ten minutes, want OK", bs.ChargeState)
	}
}

func TestBridgeBatteryResetsOnRestart(t *testing.T) {
	// A restarted node is a new vehicle, so its pack is full again. That is
	// honest about the model: it tracks the bridge's uptime for that node, not
	// an airframe's history.
	swarm := newFakeSwarm(node("swarm-net-node-1", false, true, at(50, 50, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	for i := 0; i < 3000; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
	}
	if pct := b.vs["swarm-net-node-1"].batt.remaining(); pct >= 100 {
		t.Fatalf("the pack is still at %.1f%%; the rest of this test proves nothing", pct)
	}

	// The swarm expires the node, then it comes back.
	swarm.set()
	b.sync(t)
	swarm.set(node("swarm-net-node-1", false, true, at(50, 50, 10)))
	b.sync(t)
	if pct := b.vs["swarm-net-node-1"].batt.remaining(); pct != 100 {
		t.Errorf("a restarted node came back at %.1f%%, want a full pack", pct)
	}
}

func TestBridgeBatteryChargeState(t *testing.T) {
	tests := []struct {
		pct  float64
		want uint8
	}{
		{100, MavBatteryChargeStateOk},
		{30, MavBatteryChargeStateOk},
		{25, MavBatteryChargeStateLow},
		{15, MavBatteryChargeStateCritical},
		{5, MavBatteryChargeStateEmergency},
		{0, MavBatteryChargeStateFailed},
	}
	for _, tc := range tests {
		t.Run(strconv.FormatFloat(tc.pct, 'f', -1, 64)+"pct", func(t *testing.T) {
			b := battery{consumedMAh: batteryCapacityMAh * (1 - tc.pct/100)}
			if got := b.chargeState(); got != tc.want {
				t.Errorf("at %.0f%%: charge_state = %d, want %d", tc.pct, got, tc.want)
			}
		})
	}
}

func TestBridgeBatteryNeverGoesNegative(t *testing.T) {
	// A long demo, a stalled process or a clock jump must leave a flat pack
	// rather than a negative percentage, which a ground station renders as a
	// nonsense gauge.
	b := battery{}
	for i := 0; i < 100; i++ {
		b.step(time.Hour, 1)
	}
	if b.remaining() != 0 || b.chargeState() != MavBatteryChargeStateFailed {
		t.Errorf("after 100 hours: %.2f%% state %d, want 0%% and FAILED", b.remaining(), b.chargeState())
	}
	if b.voltageMV() == 0 {
		t.Error("a flat pack still reports a voltage; 0 mV reads as a dead bus")
	}
	if b.timeRemainingS() != 0 {
		t.Errorf("time_remaining = %d on a flat pack, want 0", b.timeRemainingS())
	}
}

func TestBridgeExtendedSysState(t *testing.T) {
	tests := []struct {
		name   string
		pos    protocol.Position
		flying bool
		want   uint8
	}{
		// ON_GROUND is the ground plane and nothing else: a ground station
		// chooses between offering Takeoff and offering Land on this field.
		{"on the ground plane", at(10, 10, 0), false, MavLandedStateOnGround},
		{"holding at altitude", at(10, 10, 5), false, MavLandedStateInAir},
		{"taking off from the ground", at(10, 10, 0), true, MavLandedStateInAir},
		{"flying", at(10, 10, 20), true, MavLandedStateInAir},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swarm := newFakeSwarm(node("swarm-net-node-1", false, true, tc.pos))
			b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.Move.Idle = IdleStill })
			b.sync(t)
			if tc.flying {
				b.vs["swarm-net-node-1"].mover.SetTarget(at(10, 10, 40))
			}
			b.tickStatus()
			es := b.gcs.recvID(MsgIDExtendedSysState).Msg.(*ExtendedSysState)
			if es.LandedState != tc.want {
				t.Errorf("landed_state = %d, want %d", es.LandedState, tc.want)
			}
			// A multirotor has no transition to report; MAV_VTOL_STATE_MC
			// would claim a quadplane.
			if es.VtolState != MavVtolStateUndefined {
				t.Errorf("vtol_state = %d, want UNDEFINED", es.VtolState)
			}
		})
	}
}

func TestBridgeHomePositionAndOrigin(t *testing.T) {
	home := at(20, 60, 2)
	swarm := newFakeSwarm(node("swarm-net-node-4", false, true, home))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	// Drift away from home, so a home position that accidentally reported the
	// CURRENT position would fail here.
	for i := 0; i < 50; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
	}
	b.gcs.flush()
	b.tickStatus()
	hp := b.gcs.recvID(MsgIDHomePosition).Msg.(*HomePosition)
	og := b.gcs.recvID(MsgIDGPSGlobalOrigin).Msg.(*GPSGlobalOrigin)

	p := DefaultProjection()
	want := p.Project(home)
	if hp.Latitude != want.LatE7 || hp.Longitude != want.LonE7 || hp.Altitude != want.AltMM {
		t.Errorf("home = %d/%d/%d, want %+v", hp.Latitude, hp.Longitude, hp.Altitude, want)
	}
	// Local frame is NED metres from the cube origin: x north, y east, z down.
	k := p.MetresPerUnit
	if hp.X != float32(home.Y*k) || hp.Y != float32(home.X*k) || hp.Z != float32(-home.Z*k) {
		t.Errorf("home local NED = %v/%v/%v, want %v/%v/%v",
			hp.X, hp.Y, hp.Z, home.Y*k, home.X*k, -home.Z*k)
	}
	if hp.Q != [4]float32{1, 0, 0, 0} {
		t.Errorf("home attitude = %v, want the identity quaternion", hp.Q)
	}

	// The origin is cube (0, 0, 0), which is what makes the local coordinates
	// above convertible back to a map position.
	zero := p.Project(protocol.Position{})
	if og.Latitude != zero.LatE7 || og.Longitude != zero.LonE7 || og.Altitude != zero.AltMM {
		t.Errorf("origin = %d/%d/%d, want %+v", og.Latitude, og.Longitude, og.Altitude, zero)
	}

	// Home follows an external move, because the mover adopts it: a node
	// dragged across the dashboard must not keep a home marker on the map
	// where it used to be.
	moved := at(80, 15, 0)
	swarm.set(node("swarm-net-node-4", false, true, moved))
	b.sync(t)
	b.tickStatus()
	hp = b.gcs.recvID(MsgIDHomePosition).Msg.(*HomePosition)
	if got := p.Project(moved); hp.Latitude != got.LatE7 || hp.Longitude != got.LonE7 {
		t.Errorf("after an external move home = %d/%d, want %+v", hp.Latitude, hp.Longitude, got)
	}
}

func TestBridgeAnswersRequestMessage(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	tests := []struct {
		name   string
		id     MsgID
		want   MsgID
		result uint8
	}{
		// The one that matters: a ground station needs the capability bitmap
		// before it knows how to upload a mission.
		{"autopilot version", MsgIDAutopilotVersion, MsgIDAutopilotVersion, MavResultAccepted},
		{"home position", MsgIDHomePosition, MsgIDHomePosition, MavResultAccepted},
		{"gps origin", MsgIDGPSGlobalOrigin, MsgIDGPSGlobalOrigin, MavResultAccepted},
		{"extended sys state", MsgIDExtendedSysState, MsgIDExtendedSysState, MavResultAccepted},
		// Everything else is refused out loud. The streams arrive by
		// themselves, and silence would make a station retry for ever.
		{"unsupported", MsgIDAttitude, 0, MavResultUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b.gcs.sendMsg(b.Addr(), &CommandLong{
				TargetSystem:    2,
				TargetComponent: 1,
				Command:         MavCmdRequestMessage,
				Param1:          float32(tc.id),
			})
			b.pumpOne(t)

			// The ACK comes FIRST. A ground station waits for it before it
			// looks for the message it asked for.
			f := b.gcs.recv()
			ack, ok := f.Msg.(*CommandAck)
			if !ok {
				t.Fatalf("got %s, want COMMAND_ACK first", f.ID)
			}
			if ack.Result != tc.result {
				t.Errorf("result = %d, want %d", ack.Result, tc.result)
			}
			if tc.result != MavResultAccepted {
				b.gcs.expectQuiet()
				return
			}
			if f = b.gcs.recv(); f.ID != tc.want {
				t.Fatalf("got %s after the ack, want %s", f.ID, tc.want)
			}
			b.gcs.expectQuiet()
			if av, ok := f.Msg.(*AutopilotVersion); ok {
				// MISSION_INT or a ground station uploads waypoints as
				// float32 degrees, which is good to about 2 metres.
				if av.Capabilities&MavProtocolCapabilityMissionInt == 0 {
					t.Error("capabilities do not advertise MISSION_INT")
				}
				if av.Capabilities&MavProtocolCapabilityMissionFloat == 0 {
					t.Error("capabilities do not advertise MISSION_FLOAT")
				}
				// COMMAND_INT is not handled, so it must not be advertised:
				// a station told otherwise would send one and get silence.
				if av.Capabilities&MavProtocolCapabilityCommandInt != 0 {
					t.Error("capabilities advertise COMMAND_INT, which the bridge does not handle")
				}
				// Stable per node: a ground station logs by uid.
				if av.UID != seedOf("swarm-net-node-2") {
					t.Errorf("uid = %d, want the node's own %d", av.UID, seedOf("swarm-net-node-2"))
				}
			}
		})
	}
}
