package mavlink

import (
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// The mission protocol, driven the way a ground station drives it.
//
// The bug these tests exist for: QGroundControl asked every vehicle for its
// mission list on connect, nothing answered, and QGC retried until it gave up
// with "Mission request list failed, maximum retries exceeded". The protocol's
// rule is that a request is always answered -- with a refusal if need be, never
// with silence -- and most of what follows is checking exactly that.

// clusterSwarm is a leader with two workers that follow it, plus an unrelated
// node, which is the smallest swarm that makes cluster assignment meaningful.
func clusterSwarm() *fakeSwarm {
	leader := NodeState{ID: "swarm-net-node-9", Leader: true, Connected: true, Pos: at(50, 50, 10)}
	w1 := NodeState{ID: "swarm-net-node-1", LeaderID: "swarm-net-node-9", Connected: true, Pos: at(10, 10, 10)}
	w2 := NodeState{ID: "swarm-net-node-2", LeaderID: "swarm-net-node-9", Connected: true, Pos: at(90, 90, 10)}
	other := NodeState{ID: "swarm-net-node-3", LeaderID: "swarm-net-node-8", Connected: true, Pos: at(20, 80, 10)}
	return newFakeSwarm(leader, w1, w2, other)
}

// waypoint builds a MISSION_ITEM_INT the way a ground station sends one: global
// frame, relative altitude, NAV_WAYPOINT.
func waypoint(seq uint16, target byte, lat, lon int32, altM float32) *MissionItemInt {
	return &MissionItemInt{
		Param1:          0,
		X:               lat,
		Y:               lon,
		Z:               altM,
		Seq:             seq,
		Command:         MavCmdNavWaypoint,
		TargetSystem:    target,
		TargetComponent: 1,
		Frame:           MavFrameGlobalRelativeAltInt,
		Autocontinue:    1,
	}
}

// upload runs a whole transfer and returns the final ACK.
func uploadMission(t *testing.T, b *testBridge, target byte, items []*MissionItemInt) *MissionAck {
	t.Helper()
	b.gcs.sendMsg(b.Addr(), &MissionCount{Count: uint16(len(items)), TargetSystem: target, TargetComponent: 1})
	b.pumpOne(t)
	for range items {
		req, ok := recvUntil[*MissionRequestInt](t, b)
		if !ok {
			t.Fatalf("no MISSION_REQUEST_INT")
		}
		if int(req.Seq) >= len(items) {
			t.Fatalf("asked for item %d of %d", req.Seq, len(items))
		}
		b.gcs.sendMsg(b.Addr(), items[req.Seq])
		b.pumpOne(t)
	}
	ack, ok := recvUntil[*MissionAck](t, b)
	if !ok {
		t.Fatalf("no MISSION_ACK after the last item")
	}
	return ack
}

// recvUntil reads frames until one is of type T, skipping the unsolicited
// traffic a vehicle also emits (MISSION_CURRENT, STATUSTEXT). A ground station
// does the same: it matches a reply by type, not by arrival order.
func recvUntil[T Message](t *testing.T, b *testBridge) (T, bool) {
	t.Helper()
	var zero T
	for i := 0; i < 8; i++ {
		f := b.gcs.recv()
		if m, ok := f.Msg.(T); ok {
			return m, true
		}
	}
	return zero, false
}

// TestMissionRequestListIsAlwaysAnswered is the regression test for the QGC
// dialog. All three list types must be answered, including the two that are
// always empty: an unanswered fence or rally request produces the same retry
// storm as an unanswered mission request.
func TestMissionRequestListIsAlwaysAnswered(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	for _, typ := range []uint8{MissionTypeMission, MissionTypeFence, MissionTypeRally} {
		b.gcs.sendMsg(b.Addr(), &MissionRequestList{TargetSystem: 9, TargetComponent: 1, MissionType: typ})
		b.pumpOne(t)
		f := b.gcs.recv()
		count, ok := f.Msg.(*MissionCount)
		if !ok {
			t.Fatalf("mission_type %d got %s, want MISSION_COUNT", typ, f.ID)
		}
		if count.Count != 0 {
			t.Errorf("mission_type %d count = %d, want 0 on a fresh vehicle", typ, count.Count)
		}
		if count.MissionType != typ {
			t.Errorf("answered mission_type %d with %d: a ground station matches on this field",
				typ, count.MissionType)
		}
	}
}

// TestMissionRequestListTargetComponent: a station may address the autopilot
// (1) or broadcast within the system (0). Both are us, and rejecting either
// makes the protocol work in one station and silently fail in another.
func TestMissionRequestListTargetComponent(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	for _, comp := range []byte{0, 1} {
		b.gcs.sendMsg(b.Addr(), &MissionRequestList{TargetSystem: 9, TargetComponent: comp})
		b.pumpOne(t)
		if _, ok := b.gcs.recv().Msg.(*MissionCount); !ok {
			t.Errorf("target_component %d was not answered", comp)
		}
	}

	// target_system 0 is a broadcast. Answering it means every vehicle replies
	// to one request and the station cannot tell which mission is whose.
	b.gcs.sendMsg(b.Addr(), &MissionRequestList{TargetSystem: 0, TargetComponent: 1})
	b.pumpOne(t)
	b.gcs.expectQuiet()
}

func TestMissionUploadAndDownload(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	p := b.cfg.Projection
	a := p.Project(at(20, 20, 5))
	c := p.Project(at(80, 20, 5))
	items := []*MissionItemInt{
		waypoint(0, 9, a.LatE7, a.LonE7, 25),
		waypoint(1, 9, c.LatE7, c.LonE7, 25),
	}
	if ack := uploadMission(t, b, 9, items); ack.Type != MavMissionAccepted {
		t.Fatalf("upload ack type = %d, want ACCEPTED", ack.Type)
	}

	// Download: the count comes back, then each item, and a repeated request
	// for the same item must return the same item rather than an error.
	b.gcs.sendMsg(b.Addr(), &MissionRequestList{TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	count, ok := b.gcs.recv().Msg.(*MissionCount)
	if !ok || count.Count != 2 {
		t.Fatalf("count = %+v, want 2 items", count)
	}
	for _, seq := range []uint16{0, 1, 1} {
		b.gcs.sendMsg(b.Addr(), &MissionRequestInt{Seq: seq, TargetSystem: 9, TargetComponent: 1})
		b.pumpOne(t)
		f := b.gcs.recv()
		it, ok := f.Msg.(*MissionItemInt)
		if !ok {
			t.Fatalf("seq %d got %s, want MISSION_ITEM_INT", seq, f.ID)
		}
		if it.Seq != seq {
			t.Errorf("asked for seq %d, got %d", seq, it.Seq)
		}
		if it.X != items[seq].X || it.Y != items[seq].Y {
			t.Errorf("seq %d came back as %d/%d, want %d/%d", seq, it.X, it.Y, items[seq].X, items[seq].Y)
		}
	}

	// Out of range is refused, not ignored.
	b.gcs.sendMsg(b.Addr(), &MissionRequestInt{Seq: 7, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	f := b.gcs.recv()
	ack, ok := f.Msg.(*MissionAck)
	if !ok {
		t.Fatalf("got %s, want MISSION_ACK refusing the request", f.ID)
	}
	if ack.Type != MavMissionInvalidSequence {
		t.Errorf("ack type = %d, want INVALID_SEQUENCE", ack.Type)
	}
}

func TestMissionClearAll(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	p := b.cfg.Projection
	g := p.Project(at(30, 30, 5))
	uploadMission(t, b, 9, []*MissionItemInt{waypoint(0, 9, g.LatE7, g.LonE7, 25)})

	b.gcs.sendMsg(b.Addr(), &MissionClearAll{TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	if ack, ok := b.gcs.recv().Msg.(*MissionAck); !ok || ack.Type != MavMissionAccepted {
		t.Fatalf("clear ack = %+v, want ACCEPTED", ack)
	}

	b.gcs.sendMsg(b.Addr(), &MissionRequestList{TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	if count, ok := b.gcs.recv().Msg.(*MissionCount); !ok || count.Count != 0 {
		t.Errorf("count after clear = %+v, want 0", count)
	}
}

// TestMissionUploadTimeout: a station that disappears mid-transfer must not
// leave the vehicle waiting for an item for ever, with every later upload
// refused as out of sequence.
func TestMissionUploadTimeout(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	b.gcs.sendMsg(b.Addr(), &MissionCount{Count: 3, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	if _, ok := b.gcs.recv().Msg.(*MissionRequestInt); !ok {
		t.Fatal("no item request after MISSION_COUNT")
	}

	b.clock.advance(b.cfg.MissionUploadTimeout + time.Second)
	b.expireUploads()

	// The abandoned transfer is gone, so a fresh upload works rather than being
	// refused for arriving at sequence 0 again.
	p := b.cfg.Projection
	g := p.Project(at(40, 40, 5))
	if ack := uploadMission(t, b, 9, []*MissionItemInt{waypoint(0, 9, g.LatE7, g.LonE7, 25)}); ack.Type != MavMissionAccepted {
		t.Errorf("upload after a timed-out transfer was refused: %+v", ack)
	}
}

// --- arming ---------------------------------------------------------------------

func TestBridgeArming(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	arm := func(on float32) *CommandAck {
		t.Helper()
		b.gcs.sendMsg(b.Addr(), &CommandLong{
			Param1: on, Command: MavCmdComponentArmDisarm, TargetSystem: 9, TargetComponent: 1,
		})
		b.pumpOne(t)
		f := b.gcs.recv()
		ack, ok := f.Msg.(*CommandAck)
		if !ok {
			t.Fatalf("got %s, want COMMAND_ACK", f.ID)
		}
		return ack
	}

	if ack := arm(1); ack.Result != MavResultAccepted {
		t.Fatalf("arm result = %d, want ACCEPTED: the ground station's Arm button does nothing otherwise", ack.Result)
	}
	if !b.vs["swarm-net-node-9"].armed {
		t.Error("arm was acked but the vehicle is not armed")
	}
	if ack := arm(0); ack.Result != MavResultAccepted || b.vs["swarm-net-node-9"].armed {
		t.Errorf("disarm result = %d, armed = %v", ack.Result, b.vs["swarm-net-node-9"].armed)
	}
}

// TestMissionStartNeedsArming keeps the armed flag honest: it gates execution,
// so it means something rather than decorating a heartbeat.
func TestMissionStartNeedsArming(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	p := b.cfg.Projection
	g := p.Project(at(30, 30, 5))
	uploadMission(t, b, 9, []*MissionItemInt{waypoint(0, 9, g.LatE7, g.LonE7, 25)})

	start := func() uint8 {
		t.Helper()
		b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: 9, TargetComponent: 1})
		b.pumpOne(t)
		for {
			f := b.gcs.recv()
			if ack, ok := f.Msg.(*CommandAck); ok {
				return ack.Result
			}
			// A STATUSTEXT may precede the ack; the ack is what is asserted.
		}
	}

	if got := start(); got == MavResultAccepted {
		t.Error("a disarmed vehicle started a mission")
	}
	b.vs["swarm-net-node-9"].armed = true
	if got := start(); got != MavResultAccepted {
		t.Errorf("an armed vehicle refused MISSION_START: result %d", got)
	}
}

// --- assignment and execution -----------------------------------------------------

// TestMissionAssignsToNearestClusterMember is the swarm-level behaviour: a
// mission uploaded to a leader is flown by its whole cluster, each item by the
// member closest to it, so several vehicles fly at once.
func TestMissionAssignsToNearestClusterMember(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	p := b.cfg.Projection
	// One waypoint next to node-1 (at 10,10) and one next to node-2 (at 90,90).
	near1 := p.Project(at(12, 12, 5))
	near2 := p.Project(at(88, 88, 5))
	uploadMission(t, b, 9, []*MissionItemInt{
		waypoint(0, 9, near1.LatE7, near1.LonE7, 25),
		waypoint(1, 9, near2.LatE7, near2.LonE7, 25),
	})
	b.vs["swarm-net-node-9"].armed = true
	b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	b.runMissions()

	flying := map[protocol.NodeID]bool{}
	for id, v := range b.vs {
		if v.mover.Flying() {
			flying[id] = true
		}
	}
	if !flying["swarm-net-node-1"] || !flying["swarm-net-node-2"] {
		t.Errorf("items went to %v, want the two nearest cluster members flying at once", flying)
	}
	// The node in another leader's cluster is not conscripted.
	if flying["swarm-net-node-3"] {
		t.Error("a node outside the cluster was given an item")
	}
}

// TestWorkerMissionFliesAlone: contract section 5's other half.
func TestWorkerMissionFliesAlone(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	p := b.cfg.Projection
	g := p.Project(at(50, 10, 5))
	uploadMission(t, b, 1, []*MissionItemInt{waypoint(0, 1, g.LatE7, g.LonE7, 25)})
	b.vs["swarm-net-node-1"].armed = true
	b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: 1, TargetComponent: 1})
	b.pumpOne(t)
	b.runMissions()

	for id, v := range b.vs {
		if v.mover.Flying() && id != "swarm-net-node-1" {
			t.Errorf("%s is flying a mission that was uploaded to a worker", id)
		}
	}
	if !b.vs["swarm-net-node-1"].mover.Flying() {
		t.Error("the addressed worker is not flying its own mission")
	}
}

// TestMissionSurvivesPositionSync is a regression test for a bug found against
// a live swarm: the 1 Hz swarm sync adopted the swarm's position for a flying
// vehicle, Adopt clears the flight target, and so every mission leg was
// cancelled and reassigned once a second. Vehicles appeared to crawl and never
// reached a waypoint.
//
// The swarm's position for a flying node is ALWAYS slightly stale -- the bridge
// moves it at 5 Hz and pushes once a second -- so this is the normal case, not
// an edge case.
func TestMissionSurvivesPositionSync(t *testing.T) {
	swarm := clusterSwarm()
	b := newTestBridge(t, swarm, nil)
	b.sync(t)

	p := b.cfg.Projection
	g := p.Project(at(80, 10, 5))
	uploadMission(t, b, 1, []*MissionItemInt{waypoint(0, 1, g.LatE7, g.LonE7, 25)})
	b.vs["swarm-net-node-1"].armed = true
	b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: 1, TargetComponent: 1})
	b.pumpOne(t)
	b.runMissions()

	v := b.vs["swarm-net-node-1"]
	if !v.mover.Flying() {
		t.Fatal("the mission did not start")
	}
	target, _ := v.mover.Target()

	// Fly for a moment, then let the swarm report the position it knew a tick
	// ago, which is what a real Control Center does.
	b.clock.advance(time.Second)
	b.tickPositions()
	stale := at(10, 10, 10)
	swarm.set(NodeState{ID: "swarm-net-node-1", LeaderID: "swarm-net-node-9", Connected: true, Pos: stale})
	b.sync(t)

	if !v.mover.Flying() {
		t.Fatal("a stale position from the swarm cancelled the mission leg")
	}
	if got, _ := v.mover.Target(); got != target {
		t.Errorf("target changed to %+v, want %+v", got, target)
	}
	if v.mover.Pos() == stale {
		t.Error("the vehicle was teleported back to the swarm's stale position")
	}
}
