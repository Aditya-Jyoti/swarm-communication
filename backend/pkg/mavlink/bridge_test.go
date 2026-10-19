package mavlink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

const testWait = 10 * time.Second

// quietWait bounds a read that is expected to find nothing. It is the only
// timing in these tests, and it is not a synchronisation point: the datagrams
// being asserted absent would already be in the socket buffer, because every
// send in a driven test completes before the assertion runs.
const quietWait = 100 * time.Millisecond

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// --- test clock ----------------------------------------------------------------

// testClock is an injected clock. The bridge's loop reads it from one
// goroutine and a test advances it from another, so it is mutex-guarded even
// though most tests drive the loop themselves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// --- fake swarm ----------------------------------------------------------------

// fakeSwarm is a SwarmSource with no Control Center behind it. Mutex-guarded:
// the bridge's syncer calls it, and tests read what it recorded.
type fakeSwarm struct {
	mu        sync.Mutex
	nodes     []NodeState
	pushed    map[protocol.NodeID]protocol.Position
	pushes    int
	nodesErr  error
	pushedSig chan struct{} // closed-on-first-push signal, may be nil
	tasks     []MissionTask
	taskErr   error
}

func newFakeSwarm(nodes ...NodeState) *fakeSwarm {
	return &fakeSwarm{nodes: nodes, pushed: map[protocol.NodeID]protocol.Position{}}
}

// SubmitTask records the tasks a reached waypoint submits.
func (f *fakeSwarm) SubmitTask(_ context.Context, t MissionTask) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taskErr != nil {
		return f.taskErr
	}
	f.tasks = append(f.tasks, t)
	return nil
}

// submittedTasks is a copy of what SubmitTask has seen.
func (f *fakeSwarm) submittedTasks() []MissionTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MissionTask(nil), f.tasks...)
}

func (f *fakeSwarm) Nodes(context.Context) ([]NodeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	return append([]NodeState(nil), f.nodes...), nil
}

func (f *fakeSwarm) SetPositions(_ context.Context, pos map[protocol.NodeID]protocol.Position) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, p := range pos {
		f.pushed[id] = p
	}
	f.pushes++
	if f.pushedSig != nil {
		close(f.pushedSig)
		f.pushedSig = nil
	}
	return nil
}

func (f *fakeSwarm) set(nodes ...NodeState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = nodes
}

func (f *fakeSwarm) onPush() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushedSig = make(chan struct{})
	return f.pushedSig
}

// --- fake ground station -------------------------------------------------------

// fakeGCS is a ground station on a loopback UDP socket.
type fakeGCS struct {
	t    *testing.T
	conn *net.UDPConn
	buf  []byte
}

func newFakeGCS(t *testing.T) *fakeGCS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &fakeGCS{t: t, conn: conn, buf: make([]byte, MaxFrameLen)}
}

func (g *fakeGCS) addr() string { return g.conn.LocalAddr().String() }

// recv reads one frame, failing the test if none arrives.
func (g *fakeGCS) recv() *Frame {
	g.t.Helper()
	_ = g.conn.SetReadDeadline(time.Now().Add(testWait))
	n, _, err := g.conn.ReadFromUDP(g.buf)
	if err != nil {
		g.t.Fatalf("no frame from the bridge: %v", err)
	}
	f, err := DecodeDatagram(g.buf[:n])
	if err != nil {
		g.t.Fatalf("decode frame from the bridge: %v", err)
	}
	return f
}

// recvN reads exactly n frames.
func (g *fakeGCS) recvN(n int) []*Frame {
	g.t.Helper()
	out := make([]*Frame, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, g.recv())
	}
	return out
}

// recvID reads frames until one carries id, and fails if the burst ends
// without it.
//
// Matching by id rather than by position in the stream is what keeps these
// tests from being rewritten every time a message joins a tick: one status tick
// is statusBurst messages per vehicle (see Bridge.tickStatus) and most
// assertions here care about exactly one of them.
func (g *fakeGCS) recvID(id MsgID) *Frame {
	g.t.Helper()
	// Bounded by the largest burst a driven test can produce: a status tick for
	// a handful of vehicles.
	for i := 0; i < 8*statusBurst; i++ {
		if f := g.recv(); f.ID == id {
			return f
		}
	}
	g.t.Fatalf("no %s in the burst", id)
	return nil
}

// flush discards everything already queued.
//
// A driven test that advances the clock for ten simulated minutes emits
// thousands of frames that nobody reads; flushing lets the next assertion be
// about the NEXT tick instead of about the backlog.
func (g *fakeGCS) flush() {
	g.t.Helper()
	for {
		_ = g.conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if _, _, err := g.conn.ReadFromUDP(g.buf); err != nil {
			return
		}
	}
}

// drain reads and discards n frames.
func (g *fakeGCS) drain(n int) {
	g.t.Helper()
	for i := 0; i < n; i++ {
		g.recv()
	}
}

// expectQuiet fails if anything else arrives.
func (g *fakeGCS) expectQuiet() {
	g.t.Helper()
	_ = g.conn.SetReadDeadline(time.Now().Add(quietWait))
	n, _, err := g.conn.ReadFromUDP(g.buf)
	if err == nil {
		f, derr := DecodeDatagram(g.buf[:n])
		if derr == nil {
			g.t.Fatalf("unexpected %s from sysid %d", f.ID, f.SysID)
		}
		g.t.Fatalf("unexpected %d bytes from the bridge", n)
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		g.t.Fatalf("read: %v", err)
	}
}

// send puts a datagram on the bridge's socket, as a ground station would.
func (g *fakeGCS) send(to net.Addr, b []byte) {
	g.t.Helper()
	_ = g.conn.SetWriteDeadline(time.Now().Add(testWait))
	if _, err := g.conn.WriteTo(b, to); err != nil {
		g.t.Fatal(err)
	}
}

// sendMsg encodes m as the ground station (sysid 255) and sends it.
func (g *fakeGCS) sendMsg(to net.Addr, m Message) {
	g.t.Helper()
	g.send(to, Encode(nil, 0, 255, 190, m))
}

// --- bridge harness ------------------------------------------------------------

type testBridge struct {
	*Bridge
	clock *testClock
	gcs   *fakeGCS
	swarm *fakeSwarm
}

// newTestBridge builds a bridge whose loop the TEST drives: nothing calls Run,
// so the test goroutine is the single owner of the bridge's state and every
// assertion is deterministic without a clock that ticks. Run is exercised
// separately, in TestBridgeRunShutdown.
func newTestBridge(t *testing.T, swarm *fakeSwarm, mutate func(*BridgeConfig)) *testBridge {
	t.Helper()
	gcs := newFakeGCS(t)
	clock := newClock()
	cfg := BridgeConfig{
		Bind:   "127.0.0.1:0",
		GCS:    gcs.addr(),
		Source: swarm,
		Now:    clock.Now,
		Logger: quietLog,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := NewBridge(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return &testBridge{Bridge: b, clock: clock, gcs: gcs, swarm: swarm}
}

// sync runs one swarm exchange without the syncer goroutine.
func (b *testBridge) sync(t *testing.T) map[protocol.NodeID]protocol.Position {
	t.Helper()
	nodes, err := b.swarm.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pos := b.applyNodes(nodes)
	if err := b.swarm.SetPositions(context.Background(), pos); err != nil {
		t.Fatal(err)
	}
	return pos
}

func node(id protocol.NodeID, leader, connected bool, p protocol.Position) NodeState {
	return NodeState{ID: id, Leader: leader, Connected: connected, Pos: p}
}

func at(x, y, z float64) protocol.Position { return protocol.Position{X: x, Y: y, Z: z} }

// bySysID groups frames by the vehicle that sent them.
func bySysID(frames []*Frame) map[byte][]*Frame {
	out := map[byte][]*Frame{}
	for _, f := range frames {
		out[f.SysID] = append(out[f.SysID], f)
	}
	return out
}

// --- tests ---------------------------------------------------------------------

func TestBridgeVehiclesPerNode(t *testing.T) {
	swarm := newFakeSwarm(
		node("seed", true, true, at(10, 10, 10)),
		node("swarm-net-node-2", false, true, at(20, 20, 20)),
		node("swarm-net-node-3", false, true, at(30, 30, 30)),
	)
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	// Sysids follow the contract: trailing digits where there are any, highest
	// free id otherwise, so "seed" takes 254 and leaves the numbers alone.
	for node, want := range map[protocol.NodeID]byte{"seed": 254, "swarm-net-node-2": 2, "swarm-net-node-3": 3} {
		if got, ok := b.vehicles.Lookup(node); !ok || got != want {
			t.Errorf("%s got sysid (%d, %v), want %d", node, got, ok, want)
		}
	}

	// One whole status burst per vehicle per status tick, HEARTBEAT first: a
	// ground station shows nothing at all until it has one.
	b.tickStatus()
	got := bySysID(b.gcs.recvN(3 * statusBurst))
	b.gcs.expectQuiet()
	if len(got) != 3 {
		t.Fatalf("heard from %d vehicles, want 3", len(got))
	}
	for sysID, frames := range got {
		if len(frames) != statusBurst || frames[0].ID != MsgIDHeartbeat || frames[1].ID != MsgIDSysStatus {
			t.Errorf("sysid %d sent %v, want %d messages starting HEARTBEAT, SYS_STATUS",
				sysID, idsOf(frames), statusBurst)
		}
		if frames[0].CompID != MavCompIDAutopilot1 {
			t.Errorf("sysid %d used compid %d, want %d", sysID, frames[0].CompID, MavCompIDAutopilot1)
		}
	}

	// Sequence numbers are per vehicle, not per process: a shared counter would
	// make a ground station report 90% packet loss on every vehicle at once.
	b.tickStatus()
	for sysID, frames := range bySysID(b.gcs.recvN(3 * statusBurst)) {
		if want := byte(statusBurst); frames[0].Seq != want {
			t.Errorf("sysid %d second heartbeat seq = %d, want %d", sysID, frames[0].Seq, want)
		}
	}
}

func idsOf(frames []*Frame) []MsgID {
	out := make([]MsgID, len(frames))
	for i, f := range frames {
		out[i] = f.ID
	}
	return out
}

func TestBridgeKilledNodeStopsHeartbeating(t *testing.T) {
	swarm := newFakeSwarm(
		node("swarm-net-node-1", true, true, at(10, 10, 10)),
		node("swarm-net-node-2", false, true, at(20, 20, 20)),
	)
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	b.tickStatus()
	if n := len(bySysID(b.gcs.recvN(2 * statusBurst))); n != 2 {
		t.Fatalf("heard from %d vehicles, want 2", n)
	}

	// Node 1 is killed. Its link is gone, so it is no longer a vehicle on the
	// air -- that is exactly what a failover is supposed to look like in a
	// ground station -- while node 2 carries on.
	swarm.set(
		node("swarm-net-node-1", false, false, at(10, 10, 10)),
		node("swarm-net-node-2", true, true, at(20, 20, 20)),
	)
	b.sync(t)
	b.tickStatus()
	got := bySysID(b.gcs.recvN(statusBurst))
	b.gcs.expectQuiet()
	if len(got) != 1 || got[2] == nil {
		t.Fatalf("after the kill, heard from %v, want only sysid 2", keysOf(got))
	}
	// The survivor was promoted, and custom_mode says so.
	hb, ok := got[2][0].Msg.(*Heartbeat)
	if !ok || hb.CustomMode != 1 {
		t.Fatalf("promoted node 2 heartbeat = %+v, want custom_mode 1", got[2][0].Msg)
	}
	// A killed node must not be pushed back into the swarm either: nothing is
	// flying it.
	if pos := b.sync(t); len(pos) != 1 {
		t.Fatalf("pushed %d positions, want 1 (the live node)", len(pos))
	}
}

func keysOf(m map[byte][]*Frame) []byte {
	out := make([]byte, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBridgeHeartbeatFields(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-7", false, true, at(50, 50, 50)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	tests := []struct {
		name       string
		leader     bool
		flying     bool
		customMode uint32
		baseMode   uint8
		state      uint8
	}{
		{"idle worker", false, false, 0, MavModeFlagCustomModeEnabled, MavStateStandby},
		{"idle leader", true, false, 1, MavModeFlagCustomModeEnabled, MavStateStandby},
		// Armed is what the ground station's Arm button sets. Flying implies it
		// in practice, because takeoff and mission start both refuse while
		// disarmed, but the heartbeat reports the flag itself rather than
		// inferring it from motion.
		{"armed leader", true, true, 1, MavModeFlagCustomModeEnabled | MavModeFlagSafetyArmed, MavStateActive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := b.vs["swarm-net-node-7"]
			v.leader = tc.leader
			v.armed = tc.flying
			if tc.flying {
				v.mover.SetTarget(at(60, 60, 60))
			} else {
				v.mover.ClearTarget()
			}
			b.tickStatus()
			f := b.gcs.recvID(MsgIDHeartbeat)
			b.gcs.drain(statusBurst - 1)
			hb, ok := f.Msg.(*Heartbeat)
			if !ok {
				t.Fatalf("got %s, want HEARTBEAT", f.ID)
			}
			if hb.CustomMode != tc.customMode || hb.BaseMode != tc.baseMode || hb.SystemStatus != tc.state {
				t.Errorf("heartbeat = %+v, want custom_mode %d base_mode %d state %d",
					hb, tc.customMode, tc.baseMode, tc.state)
			}
			// MAV_AUTOPILOT_GENERIC, not ArduPilot: claiming ArduPilot makes a
			// ground station demand a full parameter set and real flight modes.
			if hb.Autopilot != MavAutopilotGeneric || hb.Type != MavTypeQuadrotor {
				t.Errorf("autopilot %d type %d, want %d/%d", hb.Autopilot, hb.Type, MavAutopilotGeneric, MavTypeQuadrotor)
			}
			if hb.MavlinkVersion != ProtocolVersion {
				t.Errorf("mavlink_version = %d, want %d", hb.MavlinkVersion, ProtocolVersion)
			}
		})
	}
}

func TestBridgeSysStatusMapping(t *testing.T) {
	swarm := newFakeSwarm(NodeState{ID: "swarm-net-node-5", Connected: true, Pos: at(5, 5, 5), Dropped: 7, LedgerSize: 12})
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	b.tickStatus()
	st, ok := b.gcs.recvID(MsgIDSysStatus).Msg.(*SysStatus)
	if !ok {
		t.Fatal("want SYS_STATUS")
	}
	// load is the ledger size in d%; errors_comm is total dropped frames; the
	// first sample's drop rate is the whole counter.
	if st.Load != 120 || st.ErrorsComm != 7 || st.ErrorsCount1 != 12 || st.DropRateComm != 700 {
		t.Errorf("sys_status = %+v, want load 120 errors_comm 7 count1 12 drop_rate 700", st)
	}
	// The battery fields are synthesised rather than sent as "not available"
	// sentinels, because a ground station with no power source shows a grey
	// health bar. A fresh vehicle's pack is full; the model is in health.go and
	// the honesty note is in doc.go.
	if st.BatteryRemaining != 100 || st.VoltageBattery != batteryCells*cellFullVolts*1000 {
		t.Errorf("battery = %d%% at %d mV, want a full 6S pack",
			st.BatteryRemaining, st.VoltageBattery)
	}

	// The rate is a delta, so a quiet second reads as zero rather than
	// repeating the total.
	b.tickStatus()
	st = b.gcs.recvID(MsgIDSysStatus).Msg.(*SysStatus)
	if st.DropRateComm != 0 || st.ErrorsComm != 7 {
		t.Errorf("second sample = %+v, want drop_rate 0 errors_comm 7", st)
	}

	// A node that restarted and reset its counter must not produce a negative
	// delta wrapped into a huge rate.
	swarm.set(NodeState{ID: "swarm-net-node-5", Connected: true, Pos: at(5, 5, 5), Dropped: 0})
	b.sync(t)
	b.tickStatus()
	if st = b.gcs.recvID(MsgIDSysStatus).Msg.(*SysStatus); st.DropRateComm != 0 {
		t.Errorf("after a counter reset, drop_rate = %d, want 0", st.DropRateComm)
	}
}

func TestBridgePositionProjection(t *testing.T) {
	// Drift off, so the position on the wire is exactly the one in the cube.
	swarm := newFakeSwarm(node("swarm-net-node-9", false, true, at(25, 50, 1)))
	b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.Move.DriftDisabled = true })
	b.sync(t)

	b.clock.advance(200 * time.Millisecond)
	b.tickPositions()
	f := b.gcs.recvID(MsgIDGlobalPositionInt)
	gp, ok := f.Msg.(*GlobalPositionInt)
	if !ok {
		t.Fatalf("got %s, want GLOBAL_POSITION_INT", f.ID)
	}
	want := DefaultProjection().Project(at(25, 50, 1))
	if gp.Lat != want.LatE7 || gp.Lon != want.LonE7 || gp.Alt != want.AltMM {
		t.Errorf("position = %d/%d/%d, want %+v", gp.Lat, gp.Lon, gp.Alt, want)
	}
	// Relative altitude is height above the ground plane: z = 1 unit at the
	// default 20 m per unit is 20 m, or 20000 mm. Nothing is moving.
	if gp.RelativeAlt != 20000 || gp.Vx != 0 || gp.Vy != 0 || gp.Vz != 0 {
		t.Errorf("a stationary vehicle reported %+v", gp)
	}
	if gp.TimeBootMs != 200 {
		t.Errorf("time_boot_ms = %d, want 200", gp.TimeBootMs)
	}
}

func TestBridgeFlightVelocityAndHeading(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-4", false, true, at(50, 50, 50)))
	b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.Move.SpeedUnitsPerSec = 3 })
	b.sync(t)
	// Due north, at 3 units/s, with 20 m per unit: 60 m/s, 6000 cm/s.
	b.vs["swarm-net-node-4"].mover.SetTarget(at(50, 90, 50))

	b.clock.advance(time.Second)
	b.tickPositions()
	gp := b.gcs.recvID(MsgIDGlobalPositionInt).Msg.(*GlobalPositionInt)
	if gp.Vx != 6000 || gp.Vy != 0 || gp.Vz != 0 {
		t.Errorf("velocity = %d/%d/%d cm/s, want 6000 north", gp.Vx, gp.Vy, gp.Vz)
	}
	if gp.Hdg != 0 {
		t.Errorf("heading = %d cdeg, want 0 (north)", gp.Hdg)
	}

	// Straight down: NED's z is positive DOWN while the cube's Z is up, which
	// is the sign everyone gets wrong once.
	b.vs["swarm-net-node-4"].mover.Adopt(at(50, 50, 50))
	b.vs["swarm-net-node-4"].mover.SetTarget(at(50, 50, 10))
	b.clock.advance(time.Second)
	b.tickPositions()
	gp = b.gcs.recvID(MsgIDGlobalPositionInt).Msg.(*GlobalPositionInt)
	if gp.Vz != 6000 {
		t.Errorf("descending: vz = %d cm/s, want +6000 (down is positive)", gp.Vz)
	}
}

func TestBridgePositionRateIsFasterThanStatus(t *testing.T) {
	// The two clocks: positions at 5 Hz out of the bridge's own state, swarm
	// pushes at 1 Hz. Driving both tickers by hand shows the ratio without
	// waiting for a second of real time.
	swarm := newFakeSwarm(node("swarm-net-node-1", false, true, at(40, 40, 40)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	for i := 0; i < 5; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
	}
	b.tickStatus()
	frames := b.gcs.recvN(5*positionBurst + statusBurst)
	b.gcs.expectQuiet()
	var pos, hb int
	for _, f := range frames {
		switch f.ID {
		case MsgIDGlobalPositionInt:
			pos++
		case MsgIDHeartbeat:
			hb++
		}
	}
	if pos != 5 || hb != 1 {
		t.Fatalf("got %d positions and %d heartbeats, want 5 and 1", pos, hb)
	}
	// One sync pushes one position per node, whatever happened in between: the
	// swarm is not spammed at the position rate.
	if n := len(b.sync(t)); n != 1 {
		t.Fatalf("pushed %d positions per sync, want 1", n)
	}
	if swarm.pushes != 2 {
		t.Fatalf("%d pushes after two syncs", swarm.pushes)
	}
}

func TestBridgeAdoptsExternalMove(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-6", false, true, at(50, 50, 50)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	v := b.vs["swarm-net-node-6"]

	// Drift for a while. The swarm echoes back what the bridge pushed, so
	// nothing is adopted and home stays put.
	for i := 0; i < 10; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
		pushed := b.sync(t)
		swarm.set(node("swarm-net-node-6", false, true, pushed["swarm-net-node-6"]))
	}
	if v.mover.Home() != at(50, 50, 50) {
		t.Fatalf("home moved to %+v while only the bridge was writing", v.mover.Home())
	}
	drifted := v.mover.Pos()
	if drifted == at(50, 50, 50) {
		t.Fatal("the vehicle never drifted; the rest of this test proves nothing")
	}

	// Now a human drags the node across the dashboard.
	moved := at(90, 10, 5)
	swarm.set(node("swarm-net-node-6", false, true, moved))
	b.sync(t)
	if v.mover.Pos() != moved || v.mover.Home() != moved {
		t.Fatalf("after an external move: pos %+v home %+v, want both %+v", v.mover.Pos(), v.mover.Home(), moved)
	}
	// And the bridge pushes the adopted position back rather than dragging the
	// node to where it thought it was.
	if got := swarm.pushed["swarm-net-node-6"]; got != moved {
		t.Fatalf("pushed %+v after adoption, want %+v", got, moved)
	}
	// Drift continues around the new home.
	for i := 0; i < 50; i++ {
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
	}
	if d := lenP(subP(v.mover.Pos(), moved)); d > b.cfg.Move.DriftRadius+1e-9 {
		t.Fatalf("drifted %.3f units from the adopted home", d)
	}
}

func TestBridgeStatustextOnSwarmEvents(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-8", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	tests := []struct {
		kind string
		want uint8
		skip bool
	}{
		{kind: EventLeaderChange, want: SeverityInfo},
		{kind: EventNodeUp, want: SeverityInfo},
		// CRITICAL: losing a node is the event the whole demo turns on, and a
		// ground station only raises a toast for CRITICAL and worse.
		{kind: EventNodeDown, want: SeverityCritical},
		// Everything else stays on the dashboard where it belongs.
		{kind: "task_done", skip: true},
		{kind: "sim", skip: true},
		{kind: "chaos", skip: true},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			b.OnEvent(tc.kind, "swarm-net-node-8", "worker -> leader")
			select {
			case e := <-b.events:
				if tc.skip {
					t.Fatalf("%s was queued for STATUSTEXT", tc.kind)
				}
				b.onEvent(e)
			default:
				if !tc.skip {
					t.Fatalf("%s was not queued", tc.kind)
				}
				return
			}
			f := b.gcs.recv()
			st, ok := f.Msg.(*Statustext)
			if !ok {
				t.Fatalf("got %s, want STATUSTEXT", f.ID)
			}
			if f.SysID != 8 {
				t.Errorf("STATUSTEXT came from sysid %d, want the node's own 8", f.SysID)
			}
			if st.Severity != tc.want {
				t.Errorf("severity = %d, want %d", st.Severity, tc.want)
			}
			if text := Chars(st.Text[:]); text == "" || len(text) > maxStatusText {
				t.Errorf("text = %q", text)
			}
		})
	}

	// An event about a node with no vehicle yet cannot be attributed to a
	// sysid, and is dropped rather than sent from somebody else's.
	b.OnEvent(EventNodeDown, "never-seen", "gone")
	b.onEvent(<-b.events)
	b.gcs.expectQuiet()

	// OnEvent never blocks, whatever the loop is doing: it is called from the
	// Control Center's hub goroutine, which owns all of the CC's state.
	for i := 0; i < eventDepth*3; i++ {
		b.OnEvent(EventNodeDown, "swarm-net-node-8", "flood")
	}
}

// --- inbound -------------------------------------------------------------------

func TestBridgeAnswersParamRequestList(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", true, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	b.gcs.sendMsg(b.Addr(), &ParamRequestList{TargetSystem: 2, TargetComponent: 1})
	b.pumpOne(t)

	frames := b.gcs.recvN(len(bridgeParams))
	b.gcs.expectQuiet()
	seen := map[string]*ParamValue{}
	for i, f := range frames {
		pv, ok := f.Msg.(*ParamValue)
		if !ok {
			t.Fatalf("frame %d is %s, want PARAM_VALUE", i, f.ID)
		}
		if int(pv.ParamCount) != len(bridgeParams) {
			t.Errorf("param_count = %d, want %d", pv.ParamCount, len(bridgeParams))
		}
		if int(pv.ParamIndex) != i {
			t.Errorf("param %d has index %d; a ground station caches by index", i, pv.ParamIndex)
		}
		seen[Chars(pv.ParamID[:])] = pv
	}
	// SYSID_THISMAV is the name a ground station actually looks for; without it
	// some versions wait on parameters for ever.
	if pv := seen["SYSID_THISMAV"]; pv == nil || pv.ParamValue != 2 {
		t.Fatalf("SYSID_THISMAV = %+v, want 2", pv)
	}
	if pv := seen["SWARM_LEADER"]; pv == nil || pv.ParamValue != 1 {
		t.Fatalf("SWARM_LEADER = %+v, want 1 for a leader", pv)
	}
}

func TestBridgeAnswersParamRequestRead(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	// By index.
	b.gcs.sendMsg(b.Addr(), &ParamRequestRead{TargetSystem: 2, TargetComponent: 1, ParamIndex: 0})
	b.pumpOne(t)
	pv, ok := b.gcs.recv().Msg.(*ParamValue)
	if !ok || Chars(pv.ParamID[:]) != "SYSID_THISMAV" {
		t.Fatalf("index 0 answered with %+v", pv)
	}

	// By name: param_index -1 means "use param_id".
	req := &ParamRequestRead{TargetSystem: 2, TargetComponent: 1, ParamIndex: -1}
	SetChars(req.ParamID[:], "SWARM_M_PER_UNIT")
	b.gcs.sendMsg(b.Addr(), req)
	b.pumpOne(t)
	pv, ok = b.gcs.recv().Msg.(*ParamValue)
	if !ok || pv.ParamValue != float32(DefaultMetresPerUnit) {
		t.Fatalf("SWARM_M_PER_UNIT answered with %+v", pv)
	}

	// Unknown name and out-of-range index get no PARAM_VALUE: inventing one
	// would have the ground station cache a parameter this vehicle never had.
	bad := &ParamRequestRead{TargetSystem: 2, TargetComponent: 1, ParamIndex: -1}
	SetChars(bad.ParamID[:], "NOT_A_PARAM")
	b.gcs.sendMsg(b.Addr(), bad)
	b.pumpOne(t)
	b.gcs.sendMsg(b.Addr(), &ParamRequestRead{TargetSystem: 2, TargetComponent: 1, ParamIndex: 99})
	b.pumpOne(t)
	// An unknown vehicle likewise.
	b.gcs.sendMsg(b.Addr(), &ParamRequestRead{TargetSystem: 77, TargetComponent: 1, ParamIndex: 0})
	b.pumpOne(t)
	b.gcs.expectQuiet()
}

func TestBridgeAcksUnsupportedCommand(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	tests := []struct {
		name   string
		target byte
		cmd    uint16
		acked  bool
	}{
		// Commands this simulation has no meaning for are refused explicitly.
		// Arm, takeoff, land, return and mission start are handled, and are
		// covered by TestBridgeArming and the mission tests.
		{"set servo", 2, 183, true},
		{"video start", 2, 2500, true},
		// Broadcast is acked once, by the lowest vehicle, not N times.
		{"broadcast", 0, 183, true},
		// A command for a vehicle that does not exist is not ours to answer.
		{"unknown vehicle", 99, 183, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b.gcs.sendMsg(b.Addr(), &CommandLong{TargetSystem: tc.target, TargetComponent: 1, Command: tc.cmd})
			b.pumpOne(t)
			if !tc.acked {
				b.gcs.expectQuiet()
				return
			}
			f := b.gcs.recv()
			ack, ok := f.Msg.(*CommandAck)
			if !ok {
				t.Fatalf("got %s, want COMMAND_ACK", f.ID)
			}
			// Silence would make a ground station retry for ever; UNSUPPORTED
			// is the honest answer for a simulated swarm.
			if ack.Command != tc.cmd || ack.Result != MavResultUnsupported {
				t.Errorf("ack = %+v, want command %d result UNSUPPORTED", ack, tc.cmd)
			}
			// Addressed back at the sender, which announced itself as 255/190.
			if ack.TargetSystem != 255 || ack.TargetComponent != 190 {
				t.Errorf("ack addressed to %d/%d, want 255/190", ack.TargetSystem, ack.TargetComponent)
			}
			b.gcs.expectQuiet()
		})
	}
}

func TestBridgeSurvivesMalformedDatagrams(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	good := Encode(nil, 0, 255, 190, &ParamRequestList{TargetSystem: 2, TargetComponent: 1})
	corrupt := append([]byte(nil), good...)
	corrupt[len(corrupt)-1] ^= 0xFF // break the CRC

	tests := []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"not mavlink", []byte("GET / HTTP/1.1\r\n")},
		{"bad crc", corrupt},
		{"truncated", good[:len(good)-3]},
		// Well-formed but carrying a message this build does not implement.
		{"unknown msgid", mangleMsgID(good, 0x7FFFFF)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := b.Stats().Malformed
			if len(tc.b) > 0 {
				b.gcs.send(b.Addr(), tc.b)
				b.pumpOne(t)
			} else {
				// An empty datagram never reaches the loop (a zero-length read
				// is not forwarded), so the policy is exercised directly.
				b.onDatagram(datagram{})
			}
			if got := b.Stats().Malformed; got != before+1 {
				t.Errorf("malformed count %d -> %d, want +1", before, got)
			}
			b.gcs.expectQuiet()
		})
	}

	// Still alive and still answering, which is the whole point.
	b.gcs.send(b.Addr(), good)
	b.pumpOne(t)
	if _, ok := b.gcs.recv().Msg.(*ParamValue); !ok {
		t.Fatal("the bridge stopped answering after malformed input")
	}
}

// mangleMsgID rewrites a v2 frame's message id and fixes nothing else, so the
// CRC still matches the (now unknown) id only by luck -- which is fine, since
// both outcomes are "malformed".
func mangleMsgID(frame []byte, id uint32) []byte {
	out := append([]byte(nil), frame...)
	out[7], out[8], out[9] = byte(id), byte(id>>8), byte(id>>16)
	return out
}

func TestBridgeLearnsGroundStation(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	// No configured GCS at all: section 7 of the contract says an inbound
	// connection must work unconfigured.
	b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.GCS = "" })
	b.sync(t)

	// Nowhere to send yet.
	b.tickStatus()
	b.gcs.expectQuiet()

	b.gcs.sendMsg(b.Addr(), &Heartbeat{Type: MavTypeGcs, Autopilot: MavAutopilotInvalid})
	b.pumpOne(t)
	b.tickStatus()
	if f := b.gcs.recv(); f.ID != MsgIDHeartbeat || f.SysID != 2 {
		t.Fatalf("got %s from sysid %d, want a vehicle heartbeat", f.ID, f.SysID)
	}
}

func TestBridgeConfiguredAndLearnedAreDeduplicated(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, nil) // GCS is the fake's own address
	b.sync(t)

	// The learned address is the same socket as the configured one, so the
	// frame must be sent once, not twice.
	b.gcs.sendMsg(b.Addr(), &Heartbeat{})
	b.pumpOne(t)
	b.tickStatus()
	b.gcs.drain(statusBurst)
	b.gcs.expectQuiet()
}

// pumpOne handles exactly one queued datagram, standing in for the loop.
//
// The reader goroutine is not running in a driven test, so the datagram is read
// off the socket here with a deadline rather than waited for.
func (b *testBridge) pumpOne(t *testing.T) {
	t.Helper()
	buf := make([]byte, MaxFrameLen)
	_ = b.conn.SetReadDeadline(time.Now().Add(testWait))
	n, from, err := b.conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("bridge received nothing: %v", err)
	}
	b.onDatagram(datagram{b: buf[:n], from: from})
}

// --- Run ------------------------------------------------------------------------

func TestBridgeRunShutdown(t *testing.T) {
	gcs := newFakeGCS(t)
	swarm := newFakeSwarm(
		node("swarm-net-node-1", true, true, at(10, 10, 10)),
		node("swarm-net-node-2", false, true, at(20, 20, 20)),
	)
	pushed := swarm.onPush()
	b, err := NewBridge(BridgeConfig{
		Bind:   "127.0.0.1:0",
		GCS:    gcs.addr(),
		Source: swarm,
		// Short, real intervals: this test is about the goroutines, so it uses
		// the real clock and waits on events rather than on time.
		StatusInterval:   5 * time.Millisecond,
		PositionInterval: 5 * time.Millisecond,
		SyncInterval:     5 * time.Millisecond,
		Logger:           quietLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	// Vehicles appear and telemetry flows.
	if f := gcs.recv(); f.SysID != 1 && f.SysID != 2 {
		t.Fatalf("first frame came from sysid %d", f.SysID)
	}
	select {
	case <-pushed:
	case <-time.After(testWait):
		t.Fatal("the bridge never pushed positions into the swarm")
	}
	// Inbound works through the reader goroutine too.
	gcs.sendMsg(b.Addr(), &CommandLong{TargetSystem: 1, TargetComponent: 1, Command: MavCmdNavTakeoff})

	// A second Run is refused rather than starting a second set of goroutines.
	if err := b.Run(ctx); !errors.Is(err, ErrBridgeRunning) {
		t.Errorf("second Run = %v, want ErrBridgeRunning", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("Run did not return after cancel")
	}
	// Run returning means wg.Wait returned: both the reader and the syncer
	// goroutine are joined, and the socket is closed. -race covers the rest.
	if err := b.Close(); err != nil {
		t.Errorf("Close after Run: %v", err)
	}
}

func TestNewBridgeValidates(t *testing.T) {
	swarm := newFakeSwarm()
	tests := []struct {
		name string
		cfg  BridgeConfig
	}{
		{"no source", BridgeConfig{Bind: "127.0.0.1:0"}},
		{"bad bind", BridgeConfig{Bind: "nope", Source: swarm}},
		{"bad gcs", BridgeConfig{Bind: "127.0.0.1:0", GCS: "nope", Source: swarm}},
		// Port 1 needs root, and binding is attempted in NewBridge precisely so
		// that this is a startup error rather than silence.
		{"privileged port", BridgeConfig{Bind: "127.0.0.1:1", Source: swarm}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBridge(tc.cfg)
			if err == nil {
				_ = b.Close()
				t.Fatalf("NewBridge(%+v) succeeded", tc.cfg)
			}
		})
	}
}

func TestBridgeDefaults(t *testing.T) {
	b, err := NewBridge(BridgeConfig{Bind: "127.0.0.1:0", Source: newFakeSwarm(), Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if b.cfg.Projection != DefaultProjection() {
		t.Errorf("projection = %+v", b.cfg.Projection)
	}
	// The zero MoveConfig is a drifting vehicle at the contract's speeds, not a
	// stationary one at zero.
	want := MoveConfig{
		SpeedUnitsPerSec: DefaultSpeedUnitsPerSec,
		Idle:             IdleDrift,
		DriftRadius:      DefaultDriftRadius,
		DriftSpeed:       DefaultDriftSpeed,
	}
	if b.cfg.Move != want {
		t.Errorf("move = %+v, want %+v", b.cfg.Move, want)
	}
	if b.cfg.StatusInterval != time.Second || b.cfg.PositionInterval != 200*time.Millisecond || b.cfg.SyncInterval != time.Second {
		t.Errorf("intervals = %v/%v/%v", b.cfg.StatusInterval, b.cfg.PositionInterval, b.cfg.SyncInterval)
	}
}

// TestBridgeServesTwoGroundStations is a regression test for a bug found with
// QGroundControl and a diagnostic script connected at once -- which is what a
// demo looks like. The bridge kept only the newest inbound sender, so the two
// stations thrashed the single slot: whoever spoke last got the telemetry and
// the other went silent a second at a time.
func TestBridgeServesTwoGroundStations(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	// No configured GCS: both stations are learned, which is the real case.
	b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.GCS = "" })
	b.sync(t)

	second := newFakeGCS(t)
	b.gcs.sendMsg(b.Addr(), &Heartbeat{Type: MavTypeGcs})
	b.pumpOne(t)
	second.sendMsg(b.Addr(), &Heartbeat{Type: MavTypeGcs})
	b.pumpOne(t)

	if got := len(b.stations); got != 2 {
		t.Fatalf("learned %d ground stations, want 2", got)
	}

	// One status tick must reach BOTH, not whichever spoke last.
	b.tickStatus()
	for name, g := range map[string]*fakeGCS{"first": b.gcs, "second": second} {
		var sawHeartbeat bool
		for _, f := range g.recvN(statusBurst) {
			if _, ok := f.Msg.(*Heartbeat); ok {
				sawHeartbeat = true
			}
		}
		if !sawHeartbeat {
			t.Errorf("%s station got no HEARTBEAT", name)
		}
	}
}

// TestBridgeForgetsQuietGroundStation: a station that reconnects arrives from a
// new source port, so stale entries must expire rather than doubling the
// outbound traffic for ever.
func TestBridgeForgetsQuietGroundStation(t *testing.T) {
	swarm := newFakeSwarm(node("swarm-net-node-2", false, true, at(10, 10, 10)))
	b := newTestBridge(t, swarm, func(c *BridgeConfig) { c.GCS = "" })
	b.sync(t)

	b.gcs.sendMsg(b.Addr(), &Heartbeat{Type: MavTypeGcs})
	b.pumpOne(t)
	if len(b.destinations()) != 1 {
		t.Fatalf("destinations = %d, want 1", len(b.destinations()))
	}

	b.clock.advance(stationTTL + time.Second)
	if got := len(b.destinations()); got != 0 {
		t.Errorf("a station silent for longer than the TTL is still a destination (%d)", got)
	}
	if got := len(b.stations); got != 0 {
		t.Errorf("expired station not forgotten: %d", got)
	}
}
