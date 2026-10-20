package mavlink

import (
	"context"
	"math"
	"net"
	"testing"
	"time"

	"swarm-net/pkg/litewing"
	"swarm-net/pkg/protocol"
)

const liveNode protocol.NodeID = "swarm-net-node-9"

func liveBridge(t *testing.T, nodes ...NodeState) *testBridge {
	t.Helper()
	if len(nodes) == 0 {
		nodes = []NodeState{
			node(liveNode, false, true, at(20, 20, 10)),
			node("swarm-net-node-2", false, true, at(40, 40, 10)),
		}
	}
	b := newTestBridge(t, newFakeSwarm(nodes...), func(c *BridgeConfig) {
		c.Move.DriftDisabled = true
		c.Live = LiveConfig{Node: liveNode, Bind: "127.0.0.1:0"}
	})
	return b
}

func sample() litewing.Sample {
	return litewing.Sample{
		DroneMs: 1000, Roll: 10, Pitch: -5, Yaw: 90, Thrust: 65535.0 / 2,
		Alt: 1.25, AltValid: true, Vbat: 3.9, Pct: 80, Armed: true, RSSI: -40,
	}
}

// frames reads one status tick's or position tick's worth of frames for every
// vehicle and returns them grouped by sysid and message id.
func collect(t *testing.T, b *testBridge, n int) map[byte]map[MsgID]*Frame {
	t.Helper()
	out := map[byte]map[MsgID]*Frame{}
	for _, f := range b.gcs.recvN(n) {
		if out[f.SysID] == nil {
			out[f.SysID] = map[MsgID]*Frame{}
		}
		out[f.SysID][f.ID] = f
	}
	return out
}

func liveText(t *testing.T, f *Frame) (string, uint8) {
	t.Helper()
	st, ok := f.Msg.(*Statustext)
	if !ok {
		t.Fatalf("got %s, want STATUSTEXT", f.ID)
	}
	return Chars(st.Text[:]), st.Severity
}

func TestLiveOffByDefault(t *testing.T) {
	b := newTestBridge(t, newFakeSwarm(node(liveNode, false, true, at(20, 20, 10))), nil)
	if b.LiveAddr() != nil || b.liveConn != nil {
		t.Fatal("a live socket was opened without MAV_LIVE_NODE")
	}
	b.sync(t)
	b.onLive(sample()) // even a sample that somehow arrived changes nothing
	b.tickStatus()
	st := b.gcs.recvID(MsgIDSysStatus).Msg.(*SysStatus)
	if st.VoltageBattery != batteryCells*cellFullVolts*1000 {
		t.Fatalf("voltage %d mV: the override applied while off", st.VoltageBattery)
	}
}

func TestLiveDefaults(t *testing.T) {
	c := BridgeConfig{Live: LiveConfig{Node: liveNode}}.withDefaults()
	if c.Live.Bind != DefaultLiveBind || c.Live.StaleAfter != DefaultLiveStaleAfter {
		t.Fatalf("defaults %+v", c.Live)
	}
	c = BridgeConfig{}.withDefaults()
	if c.Live.Bind != "" {
		t.Fatalf("live bind defaulted with no live node: %+v", c.Live)
	}
}

func TestLiveBeforeFirstSampleIsSynthetic(t *testing.T) {
	b := liveBridge(t)
	if b.LiveAddr() == nil {
		t.Fatal("no live socket")
	}
	b.sync(t)
	b.tickStatus()
	got := collect(t, b, 2*statusBurst)
	st := got[9][MsgIDSysStatus].Msg.(*SysStatus)
	if st.VoltageBattery != batteryCells*cellFullVolts*1000 || st.OnboardControlSensorsHealth != sensorsPresent {
		t.Fatalf("before any sample: %+v", st)
	}
}

func TestLiveOverridesInstruments(t *testing.T) {
	b := liveBridge(t)
	b.sync(t)
	b.onLive(sample())
	text, sev := liveText(t, b.gcs.recv())
	if text != "LiteWing link up" || sev != SeverityInfo {
		t.Fatalf("got %q severity %d", text, sev)
	}

	b.tickStatus()
	got := collect(t, b, 2*statusBurst)
	hb := got[9][MsgIDHeartbeat].Msg.(*Heartbeat)
	if hb.BaseMode&MavModeFlagSafetyArmed == 0 || hb.SystemStatus != MavStateActive {
		t.Errorf("heartbeat %+v: the drone is armed", hb)
	}
	st := got[9][MsgIDSysStatus].Msg.(*SysStatus)
	if st.VoltageBattery != 3900 || st.BatteryRemaining != 80 || st.CurrentBattery != -1 {
		t.Errorf("sys_status battery %d mV %d%% %d cA", st.VoltageBattery, st.BatteryRemaining, st.CurrentBattery)
	}
	if st.OnboardControlSensorsHealth != sensorsPresent {
		t.Errorf("health %#x, want everything healthy", st.OnboardControlSensorsHealth)
	}
	bs := got[9][MsgIDBatteryStatus].Msg.(*BatteryStatus)
	if bs.Voltages[0] != 3900 || bs.Voltages[1] != math.MaxUint16 || bs.BatteryRemaining != 80 ||
		bs.CurrentBattery != -1 || bs.CurrentConsumed != -1 || bs.ChargeState != MavBatteryChargeStateOk {
		t.Errorf("battery_status %+v: want one 3.9 V cell", bs)
	}
	// The other vehicle is untouched.
	other := got[2][MsgIDSysStatus].Msg.(*SysStatus)
	if other.VoltageBattery != batteryCells*cellFullVolts*1000 {
		t.Errorf("node 2 voltage %d: the override leaked", other.VoltageBattery)
	}
	if hb2 := got[2][MsgIDHeartbeat].Msg.(*Heartbeat); hb2.BaseMode&MavModeFlagSafetyArmed != 0 {
		t.Error("node 2 reports armed")
	}

	b.clock.advance(200 * time.Millisecond)
	b.tickPositions()
	pos := collect(t, b, 2*positionBurst)
	att := pos[9][MsgIDAttitude].Msg.(*Attitude)
	const k = math.Pi / 180
	if math.Abs(float64(att.Roll)-10*k) > 1e-6 || math.Abs(float64(att.Pitch)+5*k) > 1e-6 || math.Abs(float64(att.Yaw)-90*k) > 1e-6 {
		t.Errorf("attitude %+v", att)
	}
	gp := pos[9][MsgIDGlobalPositionInt].Msg.(*GlobalPositionInt)
	if gp.RelativeAlt != 1250 {
		t.Errorf("relative_alt %d mm, want the range sensor's 1250", gp.RelativeAlt)
	}
	ground := b.cfg.Projection.Project(protocol.Position{}).AltMM
	if gp.Alt != ground+1250 {
		t.Errorf("alt %d, want ground %d + 1250", gp.Alt, ground)
	}
	// Latitude and longitude stay the swarm's: there is no GPS on the drone.
	want := b.cfg.Projection.Project(at(20, 20, 10))
	if gp.Lat != want.LatE7 || gp.Lon != want.LonE7 {
		t.Errorf("lat/lon %d/%d, want the swarm position %d/%d", gp.Lat, gp.Lon, want.LatE7, want.LonE7)
	}
	hud := pos[9][MsgIDVfrHud].Msg.(*VfrHud)
	_, _, groundM := b.cfg.Projection.ProjectFloat(protocol.Position{})
	if hud.Throttle != 50 || math.Abs(float64(hud.Alt)-(groundM+1.25)) > 1e-3 {
		t.Errorf("vfr_hud throttle %d alt %v", hud.Throttle, hud.Alt)
	}
	// The swarm itself is not moved by the drone's altitude.
	if p := b.sync(t)[liveNode]; p != at(20, 20, 10) {
		t.Errorf("swarm position became %+v", p)
	}
}

func TestLiveDisarmedAndChargeStates(t *testing.T) {
	b := liveBridge(t)
	b.sync(t)
	s := sample()
	s.Armed = false
	s.Pct = 15
	b.onLive(s)
	b.gcs.recv() // link up
	b.tickStatus()
	got := collect(t, b, 2*statusBurst)
	if hb := got[9][MsgIDHeartbeat].Msg.(*Heartbeat); hb.BaseMode&MavModeFlagSafetyArmed != 0 || hb.SystemStatus != MavStateStandby {
		t.Errorf("heartbeat %+v: the drone is disarmed", hb)
	}
	if bs := got[9][MsgIDBatteryStatus].Msg.(*BatteryStatus); bs.ChargeState != MavBatteryChargeStateCritical {
		t.Errorf("charge state %d at 15%%", bs.ChargeState)
	}
	// The ground station's own Arm button no longer decides what the live
	// vehicle reports: the drone does.
	b.vs[liveNode].armed = true
	b.tickStatus()
	got = collect(t, b, 2*statusBurst)
	if hb := got[9][MsgIDHeartbeat].Msg.(*Heartbeat); hb.BaseMode&MavModeFlagSafetyArmed != 0 {
		t.Error("a GCS arm overrode the real drone's state")
	}
}

func TestLiveRatesAndClimb(t *testing.T) {
	b := liveBridge(t)
	b.sync(t)
	s := sample()
	s.Roll, s.Yaw, s.Alt = 0, 179, 1.0
	b.onLive(s)
	b.gcs.recv()
	b.clock.advance(100 * time.Millisecond)
	s.Roll, s.Yaw, s.Alt = 10, -179, 1.5
	b.onLive(s)
	b.tickPositions()
	pos := collect(t, b, 2*positionBurst)
	att := pos[9][MsgIDAttitude].Msg.(*Attitude)
	const k = math.Pi / 180
	if math.Abs(float64(att.Rollspeed)-100*k) > 1e-4 {
		t.Errorf("rollspeed %v, want 100 deg/s", att.Rollspeed)
	}
	// 179 -> -179 is a 2 degree turn, not a 358 degree one.
	if math.Abs(float64(att.Yawspeed)-20*k) > 1e-4 {
		t.Errorf("yawspeed %v, want 20 deg/s", att.Yawspeed)
	}
	if att.Pitchspeed != 0 {
		t.Errorf("pitchspeed %v", att.Pitchspeed)
	}
	hud := pos[9][MsgIDVfrHud].Msg.(*VfrHud)
	if math.Abs(float64(hud.Climb)-5) > 1e-4 {
		t.Errorf("climb %v m/s, want 5", hud.Climb)
	}
	gp := pos[9][MsgIDGlobalPositionInt].Msg.(*GlobalPositionInt)
	if gp.Vz != -500 {
		t.Errorf("vz %d cm/s, want -500 (NED, climbing)", gp.Vz)
	}
	// Two samples in the same instant: no division by zero, rates kept.
	b.onLive(s)
	b.tickPositions()
	att = collect(t, b, 2*positionBurst)[9][MsgIDAttitude].Msg.(*Attitude)
	if math.IsNaN(float64(att.Rollspeed)) || math.IsInf(float64(att.Rollspeed), 0) {
		t.Errorf("rollspeed %v after a zero-dt sample", att.Rollspeed)
	}
}

func TestLiveOutOfRangeAltitudeKeepsTheLast(t *testing.T) {
	b := liveBridge(t)
	b.sync(t)
	s := sample()
	s.Alt = 1.0
	b.onLive(s)
	b.gcs.recv()
	s.Alt, s.AltValid = 0, false // nothing in range: e.g. held above a table edge
	b.clock.advance(100 * time.Millisecond)
	b.onLive(s)
	b.tickPositions()
	pos := collect(t, b, 2*positionBurst)
	if gp := pos[9][MsgIDGlobalPositionInt].Msg.(*GlobalPositionInt); gp.RelativeAlt != 1000 {
		t.Errorf("relative_alt %d, want the last valid 1000", gp.RelativeAlt)
	}
	if hud := pos[9][MsgIDVfrHud].Msg.(*VfrHud); hud.Climb != 0 {
		t.Errorf("climb %v from an invalid reading", hud.Climb)
	}
}

func TestLiveStaleWithdrawsHealthAnnouncesOnceAndRecovers(t *testing.T) {
	b := liveBridge(t)
	b.sync(t)
	b.onLive(sample())
	b.gcs.recv() // link up

	b.clock.advance(1400 * time.Millisecond)
	b.tickStatus()
	got := collect(t, b, 2*statusBurst) // still fresh: no STATUSTEXT in the tick
	if st := got[9][MsgIDSysStatus].Msg.(*SysStatus); st.OnboardControlSensorsHealth != sensorsPresent {
		t.Fatalf("unhealthy while fresh: %#x", st.OnboardControlSensorsHealth)
	}

	b.clock.advance(200 * time.Millisecond) // 1.6 s since the sample
	b.tickStatus()
	text, sev := liveText(t, b.gcs.recv())
	if text != "LiteWing link lost" || sev != SeverityCritical {
		t.Fatalf("got %q severity %d", text, sev)
	}
	got = collect(t, b, 2*statusBurst)
	st := got[9][MsgIDSysStatus].Msg.(*SysStatus)
	if st.OnboardControlSensorsHealth&liveSensors != 0 {
		t.Errorf("health %#x still claims the drone's sensors", st.OnboardControlSensorsHealth)
	}
	if st.OnboardControlSensorsHealth|liveSensors != sensorsPresent {
		t.Errorf("health %#x withdrew more than the drone's sensors", st.OnboardControlSensorsHealth)
	}
	// The last real values stay: no silent fallback to a healthy synthetic pack.
	if st.VoltageBattery != 3900 {
		t.Errorf("voltage %d after the link dropped", st.VoltageBattery)
	}
	// Node 2 is unaffected.
	if other := got[2][MsgIDSysStatus].Msg.(*SysStatus); other.OnboardControlSensorsHealth != sensorsPresent {
		t.Errorf("node 2 health %#x", other.OnboardControlSensorsHealth)
	}

	// Once, not every second.
	b.clock.advance(time.Second)
	b.tickStatus()
	collect(t, b, 2*statusBurst)
	b.gcs.expectQuiet()

	// Recovery.
	b.onLive(sample())
	if text, _ := liveText(t, b.gcs.recv()); text != "LiteWing link up" {
		t.Fatalf("got %q", text)
	}
	b.tickStatus()
	got = collect(t, b, 2*statusBurst)
	if st := got[9][MsgIDSysStatus].Msg.(*SysStatus); st.OnboardControlSensorsHealth != sensorsPresent {
		t.Errorf("health %#x after recovery", st.OnboardControlSensorsHealth)
	}
	// Steady samples announce nothing further.
	b.onLive(sample())
	b.gcs.expectQuiet()
}

func TestLiveSampleBeforeTheVehicleExists(t *testing.T) {
	b := liveBridge(t)
	b.onLive(sample()) // no sync yet: no vehicle, no sysid to speak for
	b.gcs.expectQuiet()
	b.sync(t)
	b.onLive(sample())
	if text, _ := liveText(t, b.gcs.recv()); text != "LiteWing link up" {
		t.Fatalf("got %q", text)
	}
}

func TestLiveNodeDisconnectedSendsNothing(t *testing.T) {
	b := liveBridge(t, node(liveNode, false, false, at(20, 20, 10)))
	b.sync(t)
	b.onLive(sample())
	b.tickStatus()
	b.clock.advance(3 * time.Second)
	b.tickStatus()
	b.gcs.expectQuiet() // a killed node stays silent, live or not
}

func TestNewBridgeLiveBindError(t *testing.T) {
	cfg := BridgeConfig{Bind: "127.0.0.1:0", Source: newFakeSwarm(), Logger: quietLog,
		Live: LiveConfig{Node: liveNode, Bind: "nope"}}
	if _, err := NewBridge(cfg); err == nil {
		t.Fatal("bad live bind accepted")
	}
	// Taken port: the main socket must be released when the live one fails.
	taken, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg.Live.Bind = taken.LocalAddr().String()
	cfg.Bind = "127.0.0.1:0"
	if _, err := NewBridge(cfg); err == nil {
		t.Fatal("live bind on a taken port accepted")
	}
}

func TestLiveRunReceivesOverUDP(t *testing.T) {
	gcs := newFakeGCS(t)
	swarm := newFakeSwarm(node(liveNode, false, true, at(20, 20, 10)))
	b, err := NewBridge(BridgeConfig{
		Bind: "127.0.0.1:0", GCS: gcs.addr(), Source: swarm, Logger: quietLog,
		StatusInterval: 5 * time.Millisecond, PositionInterval: 5 * time.Millisecond,
		SyncInterval: 5 * time.Millisecond,
		Live:         LiveConfig{Node: liveNode, Bind: "127.0.0.1:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	feed, err := net.DialUDP("udp", nil, b.LiveAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer feed.Close()
	good := []byte(`{"src":"litewing","t":1,"roll":12.5,"pitch":0,"yaw":0,"thrust":0,` +
		`"alt":0.5,"vbat":4.0,"pct":95,"armed":0,"rssi":-30,"m":[0,0,0,0]}`)
	deadline := time.Now().Add(testWait)
	var roll float32
	for time.Now().Before(deadline) {
		_, _ = feed.Write([]byte("not json"))
		_, _ = feed.Write(good)
		f := gcs.recv()
		if a, ok := f.Msg.(*Attitude); ok && f.SysID == 9 && a.Roll != 0 {
			roll = a.Roll
			break
		}
	}
	if math.Abs(float64(roll)-12.5*math.Pi/180) > 1e-6 {
		t.Fatalf("roll %v never arrived", roll)
	}
	s := b.Stats()
	if s.LiveAccepted == 0 || s.LiveRejected == 0 {
		t.Errorf("stats %+v", s)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testWait):
		t.Fatal("Run did not return: the live reader leaked")
	}
}

// TestLiveLoopBranches drives the reader directly: a full queue sheds, garbage
// is counted, the read deadline is survived, and both shutdown paths return.
func TestLiveLoopBranches(t *testing.T) {
	b := liveBridge(t)
	good := []byte(`{"src":"litewing","t":1,"roll":1,"pitch":0,"yaw":0,"thrust":0,` +
		`"alt":0.5,"vbat":4.0,"pct":95,"armed":0,"rssi":-30,"m":[0,0,0,0]}`)
	feed, err := net.DialUDP("udp", nil, b.LiveAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer feed.Close()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(testWait)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.liveLoop(ctx); close(done) }()

	// Outlive one read deadline before anything arrives.
	time.Sleep(readTimeout + 100*time.Millisecond)
	for i := 0; i < liveDepth; i++ {
		_, _ = feed.Write(good)
	}
	waitFor("a full queue", func() bool { return len(b.liveIn) == liveDepth })
	_, _ = feed.Write(good)
	waitFor("a shed sample", func() bool { return b.dropped.Load() == 1 })
	_, _ = feed.Write([]byte("garbage"))
	waitFor("a rejected datagram", func() bool { return b.liveRejected.Load() == 1 })
	if got := b.liveAccepted.Load(); got != liveDepth+1 {
		t.Fatalf("accepted %d", got)
	}

	// Cancelled while the queue is full: the reader must not block on it.
	cancel()
	_, _ = feed.Write(good)
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("reader blocked on a full queue after cancel")
	}

	// A closed socket ends a fresh reader at once.
	_ = b.liveConn.Close()
	done2 := make(chan struct{})
	go func() { b.liveLoop(context.Background()); close(done2) }()
	select {
	case <-done2:
	case <-time.After(testWait):
		t.Fatal("reader did not stop on a closed socket")
	}
}

func TestLiveLoopShutdownPaths(t *testing.T) {
	// Already cancelled: returns before touching the socket.
	b := liveBridge(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.liveLoop(ctx)

	// Closed while blocked in a read: the read's error ends it.
	b2 := liveBridge(t)
	done := make(chan struct{})
	go func() { b2.liveLoop(context.Background()); close(done) }()
	time.Sleep(50 * time.Millisecond) // let it block in ReadFromUDP
	_ = b2.liveConn.Close()
	select {
	case <-done:
	case <-time.After(testWait):
		t.Fatal("reader did not stop when its socket closed mid-read")
	}
}
