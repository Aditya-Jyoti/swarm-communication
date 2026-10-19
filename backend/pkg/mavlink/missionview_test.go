package mavlink

import (
	"context"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// The mission view the dashboard draws (missionview.go). Every test here drives
// the bridge by hand, so the loop's state is the test goroutine's and nothing
// is timed: the view is asserted after the step that should have changed it.

// MAV_CMD_DO_DIGICAM_CONTROL. The bridge has no camera, so it is accepted and
// skipped -- the published state the dashboard has to be able to show.
const cmdDigicamControl = 206

// missionViewSwarm is clusterSwarm's leader (sysid 9) and its two workers.
// Three members, so a four-waypoint mission leaves one item with nobody to fly
// it, which is what makes "pending" observable.
func missionViewPlan(t *testing.T, b *testBridge) []*MissionItemInt {
	t.Helper()
	p := b.cfg.Projection
	near1 := p.Project(at(12, 12, 5)) // next to node-1 at (10,10)
	near2 := p.Project(at(88, 88, 5)) // next to node-2 at (90,90)
	near9 := p.Project(at(52, 52, 5)) // next to node-9 at (50,50)
	spare := p.Project(at(20, 80, 5)) // nobody left to fly it
	skip := &MissionItemInt{Seq: 3, Command: cmdDigicamControl, TargetSystem: 9, TargetComponent: 1}
	return []*MissionItemInt{
		waypoint(0, 9, near1.LatE7, near1.LonE7, 25),
		waypoint(1, 9, near2.LatE7, near2.LonE7, 25),
		waypoint(2, 9, near9.LatE7, near9.LonE7, 25),
		skip,
		waypoint(4, 9, spare.LatE7, spare.LonE7, 25),
	}
}

// states is the published state of every item, in sequence order.
func states(v MissionView) []string {
	out := make([]string, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, it.State)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// only is the single mission in a view, failed if there is not exactly one.
func only(t *testing.T, vs []MissionView) MissionView {
	t.Helper()
	if len(vs) != 1 {
		t.Fatalf("%d missions published, want 1", len(vs))
	}
	return vs[0]
}

// startMission uploads the plan and starts it, leaving the first assignment
// done.
func startMission(t *testing.T, b *testBridge, target byte, items []*MissionItemInt) {
	t.Helper()
	if ack := uploadMission(t, b, target, items); ack.Type != MavMissionAccepted {
		t.Fatalf("upload ack type = %d, want ACCEPTED", ack.Type)
	}
	node, ok := b.vehicles.Node(target)
	if !ok {
		t.Fatalf("no vehicle for sysid %d", target)
	}
	b.vs[node].armed = true
	b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: target, TargetComponent: 1})
	b.pumpOne(t)
	b.runMissions()
	b.gcs.flush()
}

// flyUntilArrival advances the clock in position steps until node's item is no
// longer being flown, so an arrival is reached by movement rather than by a
// test reaching into the mover.
func flyUntilArrival(t *testing.T, b *testBridge, node protocol.NodeID) {
	t.Helper()
	// Bounded: the cube's diagonal at the default speed is about a minute of
	// simulated flight, and one step is PositionInterval.
	for i := 0; i < 2000; i++ {
		if !b.vs[node].mover.Flying() {
			return
		}
		b.clock.advance(b.cfg.PositionInterval)
		b.tickPositions()
		b.runMissions()
		if i%50 == 0 {
			b.gcs.flush()
		}
	}
	t.Fatalf("%s never reached its waypoint", node)
}

// TestMissionViewTracksItemStates walks one mission through every published
// state. The dashboard draws a waypoint from this and nothing else, so a state
// that lags the swarm is a waypoint drawn in the wrong colour.
func TestMissionViewTracksItemStates(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)
	items := missionViewPlan(t, b)

	if vs := b.missionViews(); vs != nil {
		t.Fatalf("a bridge with no mission published %v, want nothing at all", vs)
	}

	// Uploaded, not started: an upload is a plan, not a launch.
	if ack := uploadMission(t, b, 9, items); ack.Type != MavMissionAccepted {
		t.Fatalf("upload ack type = %d, want ACCEPTED", ack.Type)
	}
	v := only(t, b.missionViews())
	want := []string{
		MissionItemPending, MissionItemPending, MissionItemPending,
		MissionItemSkipped, MissionItemPending,
	}
	if got := states(v); !sameStrings(got, want) {
		t.Errorf("states before MISSION_START = %v, want %v", got, want)
	}
	if v.Started {
		t.Error("started is true before MISSION_START")
	}
	for _, it := range v.Items {
		if it.Node != "" {
			t.Errorf("item %d is already assigned to %s", it.Seq, it.Node)
		}
	}

	// Started: three idle members take the first three waypoints, the camera
	// item is skipped, and the fourth waypoint waits for somebody.
	b.vs["swarm-net-node-9"].armed = true
	b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	b.runMissions()
	b.gcs.flush()

	v = only(t, b.missionViews())
	want = []string{
		MissionItemFlying, MissionItemFlying, MissionItemFlying,
		MissionItemSkipped, MissionItemPending,
	}
	if got := states(v); !sameStrings(got, want) {
		t.Fatalf("states after MISSION_START = %v, want %v", got, want)
	}
	if !v.Started {
		t.Error("started is false after MISSION_START")
	}
	assigned := map[int]protocol.NodeID{
		0: "swarm-net-node-1",
		1: "swarm-net-node-2",
		2: "swarm-net-node-9",
	}
	for _, it := range v.Items {
		if want, ok := assigned[it.Seq]; ok && it.Node != want {
			t.Errorf("item %d went to %q, want the nearest member %q", it.Seq, it.Node, want)
		}
		if _, ok := assigned[it.Seq]; !ok && it.Node != "" {
			t.Errorf("item %d reports node %q, want nobody", it.Seq, it.Node)
		}
	}

	// Flown: node-1 arrives, so its item is done and the waiting waypoint is
	// handed to it.
	flyUntilArrival(t, b, "swarm-net-node-1")
	v = only(t, b.missionViews())
	if got := states(v)[0]; got != MissionItemDone {
		t.Errorf("item 0 after arrival = %q, want %q", got, MissionItemDone)
	}
	if got := states(v)[4]; got != MissionItemFlying {
		t.Errorf("item 4 after a member freed up = %q, want %q", got, MissionItemFlying)
	}
	// Whose it is depends on which leg finished first, which is a property of
	// the plan, not of the view. That it has an owner at all is the point.
	if v.Items[4].Node == "" {
		t.Error("item 4 is flying with nobody assigned to it")
	}
}

// TestMissionViewPositionsAreCubeUnits: the dashboard draws waypoints in the
// same view as nodes[].pos, so an item's position has to be in cube units and
// has to be the goal the bridge really handed to the mover.
func TestMissionViewPositionsAreCubeUnits(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)
	p := b.cfg.Projection
	g := p.Project(at(30, 70, 5))

	// A waypoint, a bare takeoff (altitude only, no coordinate), a landing and
	// the camera item. Each has a different answer to "what should be drawn".
	items := []*MissionItemInt{
		waypoint(0, 9, g.LatE7, g.LonE7, 25),
		{Seq: 1, Command: MavCmdNavTakeoff, Z: 40, TargetSystem: 9, TargetComponent: 1,
			Frame: MavFrameGlobalRelativeAltInt},
		{Seq: 2, Command: MavCmdNavLand, TargetSystem: 9, TargetComponent: 1,
			Frame: MavFrameGlobalRelativeAltInt},
		{Seq: 3, Command: cmdDigicamControl, TargetSystem: 9, TargetComponent: 1},
	}
	if ack := uploadMission(t, b, 9, items); ack.Type != MavMissionAccepted {
		t.Fatalf("upload ack type = %d, want ACCEPTED", ack.Type)
	}

	v := only(t, b.missionViews())
	// Unassigned: only the item that carries a coordinate has one to draw.
	if v.Items[0].Pos == nil {
		t.Fatal("a plain waypoint has no position")
	}
	// x and y come from the projection, z from the item's 25 m relative
	// altitude. The tolerance is the round trip through degrees * 1e7, which
	// is millimetres.
	wantPos := at(30, 70, 25/p.MetresPerUnit)
	if got := *v.Items[0].Pos; lenP(subP(got, wantPos)) > 1e-3 {
		t.Errorf("waypoint pos = %+v, want cube units near %+v", got, wantPos)
	}
	for _, seq := range []int{1, 2, 3} {
		if v.Items[seq].Pos != nil {
			t.Errorf("item %d published pos %+v before anybody was assigned to it: "+
				"its target depends on the vehicle that gets it", seq, *v.Items[seq].Pos)
		}
	}

	// Assigned: the takeoff and the landing now have the goal the bridge gave
	// the mover, which is what the vehicle is flying to.
	b.vs["swarm-net-node-9"].armed = true
	b.gcs.sendMsg(b.Addr(), &CommandLong{Command: MavCmdMissionStart, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	b.runMissions()
	b.gcs.flush()

	v = only(t, b.missionViews())
	for _, seq := range []int{0, 1, 2} {
		it := v.Items[seq]
		if it.Node == "" {
			t.Fatalf("item %d was not assigned; the rest of this test proves nothing", seq)
		}
		if it.Pos == nil {
			t.Fatalf("item %d is being flown by %s with no position to draw", seq, it.Node)
		}
		goal, _ := b.vs[it.Node].mover.Target()
		if *it.Pos != goal {
			t.Errorf("item %d pos = %+v, want the mover's goal %+v", seq, *it.Pos, goal)
		}
	}
	if v.Items[3].Pos != nil {
		t.Error("the skipped camera item published a position; nothing ever flies it")
	}
}

// TestMissionViewCurrent: current is the last MISSION_CURRENT sequence sent,
// and -1 before any has been.
func TestMissionViewCurrent(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)

	p := b.cfg.Projection
	g := p.Project(at(60, 60, 5))
	items := []*MissionItemInt{
		waypoint(0, 9, g.LatE7, g.LonE7, 25),
		waypoint(1, 9, g.LatE7, g.LonE7, 25),
	}

	// installMission sends the first MISSION_CURRENT, so the only way to see
	// -1 is a mission that has not been installed yet: an upload still in
	// flight. One item announced, none delivered.
	b.gcs.sendMsg(b.Addr(), &MissionCount{Count: 1, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	b.gcs.flush()
	if vs := b.missionViews(); vs != nil {
		t.Fatalf("a transfer in flight published %v; it is not a mission yet", vs)
	}

	uploadMission(t, b, 9, items)
	b.gcs.flush()
	if got := only(t, b.missionViews()).Current; got != 0 {
		t.Errorf("current after upload = %d, want 0", got)
	}

	// Jumping the mission on moves it.
	b.gcs.sendMsg(b.Addr(), &MissionSetCurrent{Seq: 1, TargetSystem: 9, TargetComponent: 1})
	b.pumpOne(t)
	b.gcs.flush()
	v := only(t, b.missionViews())
	if v.Current != 1 {
		t.Errorf("current after MISSION_SET_CURRENT = %d, want 1", v.Current)
	}
	if got := states(v)[0]; got != MissionItemDone {
		t.Errorf("item 0 after a jump past it = %q, want %q", got, MissionItemDone)
	}
}

// TestMissionViewMigration is the failover the dashboard exists to show: the
// vehicle a mission was uploaded to dies, the swarm elects somebody else, and
// the mission moves. owner must stay put -- it is where a download comes from
// -- while exec follows the election.
func TestMissionViewMigration(t *testing.T) {
	swarm := clusterSwarm()
	b := newTestBridge(t, swarm, nil)
	b.sync(t)
	startMission(t, b, 9, missionViewPlan(t, b))

	before := only(t, b.missionViews())
	if before.Owner != 9 || before.Exec != 9 {
		t.Fatalf("before the failover: owner %d exec %d, want both 9", before.Owner, before.Exec)
	}
	if before.OwnerNode != "swarm-net-node-9" || before.ExecNode != "swarm-net-node-9" {
		t.Fatalf("node ids = %q / %q, want swarm-net-node-9 for both",
			before.OwnerNode, before.ExecNode)
	}

	// The leader is killed and node-1 is elected. The bridge does not vote; it
	// reads the swarm's own answer.
	swarm.set(
		NodeState{ID: "swarm-net-node-9", Connected: false, Pos: at(50, 50, 10)},
		NodeState{ID: "swarm-net-node-1", Leader: true, LeaderID: "swarm-net-node-1",
			Connected: true, Pos: at(10, 10, 10)},
		NodeState{ID: "swarm-net-node-2", LeaderID: "swarm-net-node-1",
			Connected: true, Pos: at(90, 90, 10)},
		NodeState{ID: "swarm-net-node-3", LeaderID: "swarm-net-node-8",
			Connected: true, Pos: at(20, 80, 10)},
	)
	b.sync(t)
	b.runMissions()
	b.gcs.flush()

	after := only(t, b.missionViews())
	if after.Owner != 9 || after.OwnerNode != "swarm-net-node-9" {
		t.Errorf("owner moved to %d (%s); a download still comes from the uploaded sysid",
			after.Owner, after.OwnerNode)
	}
	if after.Exec != 1 || after.ExecNode != "swarm-net-node-1" {
		t.Errorf("exec = %d (%s), want 1 (swarm-net-node-1): the mission follows the election",
			after.Exec, after.ExecNode)
	}
	// The dead node's leg is back in the pool rather than credited to it.
	for _, it := range after.Items {
		if it.Node == "swarm-net-node-9" && it.State != MissionItemDone {
			t.Errorf("item %d is still %q on the dead leader", it.Seq, it.State)
		}
	}
}

// TestMissionViewIsADeepCopy. The view crosses two goroutines -- it is built on
// the bridge's loop and marshalled on the Control Center's hub -- so an alias
// would be a data race under go test -race and, worse, a waypoint that changes
// while a browser's snapshot is being encoded.
func TestMissionViewIsADeepCopy(t *testing.T) {
	b := newTestBridge(t, clusterSwarm(), nil)
	b.sync(t)
	startMission(t, b, 9, missionViewPlan(t, b))

	held := only(t, b.missionViews())
	heldStates := states(held)
	heldNode := held.Items[0].Node
	heldPos := *held.Items[0].Pos

	// Mutating the view must not reach the bridge.
	held.Items[0].State = "nonsense"
	held.Items[0].Node = "mutated"
	held.Items[0].Pos.X += 1000
	held.Exec = 200

	fresh := only(t, b.missionViews())
	if got := states(fresh)[0]; got != heldStates[0] {
		t.Errorf("mutating the view changed the bridge: item 0 is now %q", got)
	}
	if fresh.Items[0].Node != heldNode {
		t.Errorf("item 0 node is now %q, want %q", fresh.Items[0].Node, heldNode)
	}
	if *fresh.Items[0].Pos != heldPos {
		t.Errorf("item 0 pos is now %+v, want %+v", *fresh.Items[0].Pos, heldPos)
	}
	if fresh.Exec != 9 {
		t.Errorf("exec is now %d, want 9", fresh.Exec)
	}

	// And the bridge moving on must not reach a view already handed out.
	snapshot := only(t, b.missionViews())
	snapStates := states(snapshot)
	snapPos := *snapshot.Items[0].Pos
	flyUntilArrival(t, b, snapshot.Items[0].Node)
	if got := states(only(t, b.missionViews()))[0]; got == snapStates[0] {
		t.Fatalf("item 0 is still %q after its arrival; the rest of this test proves nothing", got)
	}
	if got := states(snapshot)[0]; got != snapStates[0] {
		t.Errorf("the held view changed to %q when the bridge progressed", got)
	}
	if *snapshot.Items[0].Pos != snapPos {
		t.Errorf("the held view's pos changed to %+v", *snapshot.Items[0].Pos)
	}
}

// TestMissionViewTruncation: the snapshot goes to every browser once a second,
// so a 500-item plan must not decide how much bandwidth that costs.
func TestMissionViewTruncation(t *testing.T) {
	tests := []struct {
		name      string
		items     int
		wantItems int
		wantTrunc bool
	}{
		{name: "under the cap", items: 3, wantItems: 3},
		{name: "at the cap", items: MaxPublishedItems, wantItems: MaxPublishedItems},
		{name: "over the cap", items: MaxPublishedItems + 1, wantItems: MaxPublishedItems, wantTrunc: true},
		{name: "the stored maximum", items: maxMissionItems, wantItems: MaxPublishedItems, wantTrunc: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBridge(t, clusterSwarm(), nil)
			b.sync(t)

			// Installed directly rather than uploaded: this is about the size
			// of the published list, and 500 protocol round trips would say
			// nothing more about it.
			items := make([]missionItem, 0, tc.items)
			for i := 0; i < tc.items; i++ {
				items = append(items, missionItem{
					seq: uint16(i), cmd: MavCmdNavWaypoint, kind: itemFly,
					pos: at(float64(i%100), 50, 5), hasXY: true,
				})
			}
			b.installMission(b.vs["swarm-net-node-9"], items)
			b.gcs.flush()

			v := only(t, b.missionViews())
			if len(v.Items) != tc.wantItems {
				t.Errorf("published %d of %d items, want %d", len(v.Items), tc.items, tc.wantItems)
			}
			if v.Truncated != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", v.Truncated, tc.wantTrunc)
			}
			// Truncation keeps the FIRST items: a mission is flown in
			// sequence order, so the head is the part an operator is watching.
			for i, it := range v.Items {
				if it.Seq != i {
					t.Fatalf("item %d has seq %d; the published list is not the head of the plan", i, it.Seq)
				}
			}
		})
	}
}

// TestMissionsAccessorAndPush exercises the two halves of the hand-off with the
// bridge's own goroutines running: Missions posts onto the loop, and the syncer
// publishes the result to the swarm once a tick.
func TestMissionsAccessorAndPush(t *testing.T) {
	swarm := clusterSwarm()
	gcs := newFakeGCS(t)
	clock := newClock()
	b, err := NewBridge(BridgeConfig{
		Bind: "127.0.0.1:0",
		GCS:  gcs.addr(),
		// Fast enough that the test is driven by the bridge's own tick rather
		// than by a sleep; the clock is still injected, so nothing else here
		// depends on real time.
		SyncInterval: time.Millisecond,
		Source:       swarm,
		Now:          clock.Now,
		Logger:       quietLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	// The first push is the empty view: a dashboard has to be told there is no
	// mission, or it would keep drawing the last one.
	pushed := swarm.onMissions()
	select {
	case <-pushed:
	case <-ctx.Done():
		t.Fatal("the bridge never published a mission view")
	}
	if got := swarm.publishedMissions(); len(got) != 0 {
		t.Errorf("published %v on a swarm with no mission, want nothing", got)
	}

	// The accessor answers from the loop, from this goroutine, with no race.
	ms, err := b.Missions(ctx)
	if err != nil {
		t.Fatalf("Missions: %v", err)
	}
	if len(ms) != 0 {
		t.Errorf("Missions = %v, want nothing", ms)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A bridge that is not running cannot answer, and the caller gets its
	// context's error rather than blocking for ever.
	stopped, scancel := context.WithCancel(context.Background())
	scancel()
	if _, err := b.Missions(stopped); err == nil {
		t.Error("Missions succeeded against a stopped bridge")
	}
}
