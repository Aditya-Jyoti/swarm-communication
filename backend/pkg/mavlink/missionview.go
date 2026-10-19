package mavlink

import (
	"context"
	"fmt"

	"swarm-net/pkg/protocol"
)

// The bridge's mission state, published for the dashboard.
//
// # Why a view at all
//
// Everything in mission.go is owned by the bridge's loop goroutine, so the
// Control Center's hub cannot read it: the hub owns its own state on its own
// goroutine and the two never touch each other's. This file is the one way
// mission state leaves the loop -- a flat, deep-copied value, built ON the
// loop and handed over a channel, in the same direction as everything else the
// bridge publishes.
//
// # Why the published state is not the internal state
//
// The view is what a browser can draw with no lookup table and no knowledge of
// MAVLink: node ids beside sysids, lowercase state names instead of itemState
// numbers, and positions in CUBE UNITS, the same space as the snapshot's
// nodes[].pos. Projecting to WGS-84 here would force the dashboard to
// unproject a coordinate the Control Center already has in cube units.
//
// The shape is a contract with the dashboard; it is documented in section 5 of
// docs/architecture/mavlink-bridge.md.

// Published item states. Lowercase strings rather than the internal itemState
// numbers, which are an implementation detail that must not reach a browser.
const (
	MissionItemPending = "pending"
	MissionItemFlying  = "flying"
	MissionItemDone    = "done"
	MissionItemSkipped = "skipped"
)

// MaxPublishedItems caps the items published per mission.
//
// The whole snapshot goes to every browser once a second, so an uncapped list
// is a ground station's choice of how much bandwidth the dashboard uses: a
// 500-item mission (maxMissionItems) is about 50 KB of JSON per second per
// browser, on top of the node and task tables. 100 items is roughly 10 KB and
// more waypoints than a demo plan has. Beyond the cap the view says
// Truncated, so the dashboard can report a partial plan instead of quietly
// drawing one.
const MaxPublishedItems = 100

// MissionItemView is one mission item as the dashboard sees it.
type MissionItemView struct {
	// Seq is the item's sequence number, as uploaded.
	Seq int `json:"seq"`
	// Cmd is the raw MAV_CMD, so the dashboard can label a waypoint, a
	// takeoff or a landing without the bridge inventing a taxonomy for it.
	Cmd int `json:"cmd"`
	// State is one of the MissionItem* constants.
	State string `json:"state"`
	// Node is the vehicle flying, or that flew, the item. Empty for an item
	// nobody has been given. It is a node id rather than a sysid because that
	// is the key nodes[] in the snapshot is keyed by.
	Node protocol.NodeID `json:"node"`
	// Pos is where the item sends its vehicle, in cube units. Nil for an item
	// with no position to draw: a bare takeoff (altitude only) or a LAND or
	// RETURN_TO_LAUNCH that nobody has been assigned yet, whose target depends
	// on the vehicle that gets it.
	Pos *protocol.Position `json:"pos,omitempty"`
}

// MissionView is one stored mission and its execution state.
type MissionView struct {
	// Owner is the sysid the mission was uploaded to, and OwnerNode that
	// vehicle's node id. Owner never changes: it is the mission's identity to
	// the ground station.
	Owner     byte            `json:"owner"`
	OwnerNode protocol.NodeID `json:"owner_node"`
	// Exec is the sysid whose cluster is flying the mission, and ExecNode its
	// node id. It equals Owner until the owner dies and the mission migrates
	// to the swarm's new leader (see Bridge.migrate), which is exactly the
	// failover the dashboard is there to show.
	Exec     byte            `json:"exec"`
	ExecNode protocol.NodeID `json:"exec_node"`
	// Started is false for an uploaded plan nobody has run yet: an upload is a
	// plan, not a launch, and MAV_CMD_MISSION_START is what sets this.
	Started bool `json:"started"`
	// Current is the last MISSION_CURRENT sequence sent, or -1 when none has
	// been. -1 rather than an omitted key so the dashboard reads one field
	// instead of two.
	Current int `json:"current"`
	// Items is the plan, in sequence order, capped at MaxPublishedItems.
	Items []MissionItemView `json:"items"`
	// Truncated is true when Items is shorter than the stored mission.
	Truncated bool `json:"truncated,omitempty"`
}

// missionReq asks the loop for a mission view. reply is buffered, so the loop
// never blocks answering one.
type missionReq struct{ reply chan []MissionView }

// Missions returns the bridge's mission state, in owning-sysid order.
//
// Safe from any goroutine: it posts a request onto the loop, which is the sole
// owner of the state, and the loop answers with a deep copy. The returned
// value aliases nothing the bridge will touch again, so a caller may keep it,
// mutate it or marshal it at its leisure.
//
// It blocks until the loop answers or ctx ends, which is why the Control
// Center never calls it from its hub goroutine; see Bridge.syncMissions.
func (b *Bridge) Missions(ctx context.Context) ([]MissionView, error) {
	req := missionReq{reply: make(chan []MissionView, 1)}
	select {
	case b.missionReqs <- req:
	case <-ctx.Done():
		// Terminal for this call only. The loop is busy or not running; a
		// caller on a tick simply asks again next time.
		return nil, fmt.Errorf("mavlink: read missions: %w", ctx.Err())
	}
	select {
	case out := <-req.reply:
		return out, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("mavlink: read missions: %w", ctx.Err())
	}
}

// missionViews builds the view. Loop-owned: called only from the loop, and the
// only state it reads is the loop's.
func (b *Bridge) missionViews() []MissionView {
	order := b.missionOrder()
	if len(order) == 0 {
		// nil, not an empty slice: the Control Center omits the snapshot key
		// entirely when there are no missions, and "no missions" should not
		// cost every browser a JSON array per second.
		return nil
	}
	out := make([]MissionView, 0, len(order))
	for _, sysID := range order {
		out = append(out, b.missionView(b.missions[sysID]))
	}
	return out
}

func (b *Bridge) missionView(ms *mission) MissionView {
	n := len(ms.items)
	truncated := false
	if n > MaxPublishedItems {
		n, truncated = MaxPublishedItems, true
	}
	v := MissionView{
		Owner:     ms.owner,
		OwnerNode: b.nodeOfSysID(ms.owner),
		Exec:      ms.exec,
		ExecNode:  b.nodeOfSysID(ms.exec),
		Started:   ms.started,
		Current:   -1,
		// Freshly allocated, like every slice that leaves the loop. Handing
		// out a window onto ms.items would let a browser-side encoder read a
		// waypoint the loop is reassigning in the same instant.
		Items:     make([]MissionItemView, 0, n),
		Truncated: truncated,
	}
	if ms.sentCurrent {
		v.Current = int(ms.current)
	}
	for i := 0; i < n; i++ {
		v.Items = append(v.Items, itemView(ms.items[i]))
	}
	return v
}

// itemView flattens one item. By value: the copy is the point.
func itemView(it missionItem) MissionItemView {
	out := MissionItemView{
		Seq:   int(it.seq),
		Cmd:   int(it.cmd),
		State: itemStateName(it),
		Node:  it.node,
	}
	switch {
	case it.node != "":
		// The goal the bridge actually handed to the mover, clamped to the
		// cube. It is the only right answer for LAND and RETURN_TO_LAUNCH,
		// whose target depends on the vehicle, and for a bare takeoff, which
		// carries an altitude and no coordinate.
		p := it.goal
		out.Pos = &p
	case it.kind == itemFly && it.hasXY:
		// Planned but unassigned: the uploaded coordinate is where it will go,
		// whoever gets it.
		p := it.pos
		out.Pos = &p
	}
	return out
}

// itemStateName maps one item onto a published state.
//
// Skipped is a property of the item's KIND, not of its itemState: the planner
// marks an unflyable item (a camera trigger, a speed change) itemDone the
// moment it reaches it, because nothing will ever fly it. Publishing that as
// "done" would show the operator a leg that was completed; publishing it as
// "skipped" at every point in its life says what really happened, and matches
// the STATUSTEXT the bridge sends for it.
func itemStateName(it missionItem) string {
	if it.kind == itemSkip {
		return MissionItemSkipped
	}
	switch it.state {
	case itemFlying:
		return MissionItemFlying
	case itemDone:
		return MissionItemDone
	default:
		return MissionItemPending
	}
}

// nodeOfSysID is the node id a sysid belongs to, empty if none ever did.
//
// Vehicles, not b.vs: an assignment is held for the process's lifetime, so a
// mission whose owner has died still reports the node it was uploaded to --
// which is the whole point of keeping Owner separate from Exec.
func (b *Bridge) nodeOfSysID(sysID byte) protocol.NodeID {
	id, _ := b.vehicles.Node(sysID)
	return id
}

// syncMissions publishes the mission view to the swarm's consumer. Called from
// the syncer goroutine, once per sync interval.
//
// Pushed by the bridge rather than pulled by the Control Center for one
// reason: the CC builds its snapshot on its hub goroutine, and asking the
// bridge there would make a tick of the dashboard wait on a loop that is busy
// answering a ground station. The CC therefore serves whatever it last
// received, which is stale by at most one sync interval -- a second -- and a
// second-old waypoint list is correct for a 1 Hz broadcast in a way that a
// stalled hub is not.
//
// It is published on every tick, including when there are no missions: a
// cleared mission has to reach the dashboard too, and this call is the only
// thing that would clear it.
func (b *Bridge) syncMissions(ctx context.Context) {
	ms, err := b.Missions(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("mavlink: read missions", "err", err)
		}
		return
	}
	if err := b.cfg.Source.SetMissions(ctx, ms); err != nil && ctx.Err() == nil {
		b.log.Warn("mavlink: publish missions", "err", err)
	}
}
