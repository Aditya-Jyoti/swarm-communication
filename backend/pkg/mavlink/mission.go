package mavlink

import (
	"fmt"
	"math"
	"sort"
	"time"

	"swarm-net/pkg/protocol"
)

// The mission protocol and swarm-level mission execution, per section 5 of
// docs/architecture/mavlink-bridge.md.
//
// # Why silence is the one forbidden answer
//
// This file exists because of one dialog. A ground station asks every vehicle
// for its mission the moment the link comes up, retries, and then pops
// "Mission transfer failed. Error: Mission request list failed, maximum retries
// exceeded." -- once per vehicle. With eleven vehicles that is eleven modal
// dialogs before the operator has done anything at all. Nothing answered
// MISSION_REQUEST_LIST.
//
// So every inbound mission message here ends in a reply: a MISSION_COUNT, an
// item, or a MISSION_ACK carrying the MAV_MISSION_RESULT that says what was
// wrong. "Ignore it" is not a branch. The same rule covers the mission types
// nobody here implements: a geofence request is answered with count 0, because
// an unanswered fence request produces exactly the same dialog as an unanswered
// mission request.
//
// # Ownership
//
// Every type in this file is owned by the bridge's loop goroutine, like the
// vehicle table it indexes (see Bridge). No mutexes, and one answer to "who
// assigned this waypoint". The only thing that leaves the loop is a waypoint
// task, handed to the task goroutine over a buffered channel.

// MAV_MISSION_TYPE values. dialect.go does not generate the enum -- the
// generator emits only the enums it was asked for -- and these three are
// exactly the three a ground station requests on connect.
const (
	MissionTypeMission = 0
	MissionTypeFence   = 1
	MissionTypeRally   = 2
)

// MISSION_STATE values, for MISSION_CURRENT's mission_state extension field. A
// ground station uses it to label the mission as running or finished; it is
// extension data, so an older peer simply never sees it.
const (
	MissionStateNoMission  = 1
	MissionStateNotStarted = 2
	MissionStateActive     = 3
	MissionStateComplete   = 5
)

// Mission bounds.
const (
	// maxMissionItems bounds one stored mission. A refusal (MAV_MISSION_NO_SPACE)
	// is a number a ground station shows the operator; an unbounded accept is a
	// memory footprint a ground station can choose for us.
	maxMissionItems = 500

	// DefaultMissionUploadTimeout is how long a half-finished upload may stall
	// before the bridge abandons it. A ground station retries a lost
	// MISSION_REQUEST_INT every second or so, so this is generous; the point is
	// not to be fast but to make the wedged state impossible. Without it, a GCS
	// that is killed mid-transfer leaves the vehicle waiting for item 4 for
	// ever, and every later upload is answered with INVALID_SEQUENCE.
	DefaultMissionUploadTimeout = 10 * time.Second

	// missionTaskKind is the task kind a reached waypoint submits. "echo" is a
	// built-in the swarm's executor really runs (see cluster.NewDefaultExecutor),
	// so the waypoint shows up in the dashboard's task table as a completed
	// task rather than as "unknown task kind".
	missionTaskKind = "echo"
)

// MissionTask is the work a reached waypoint stands for.
//
// Flight is simulated; the task is not. The point of submitting it is that a
// waypoint lights up the real TASK / TASK_RESULT path: the leader replicates
// it, a worker runs it, and it appears in the dashboard's task table next to
// tasks a human submitted.
type MissionTask struct {
	// Kind is the task kind for the swarm's executor.
	Kind string
	// Node is the vehicle that arrived.
	Node protocol.NodeID
	// Seq is the mission item index that was reached.
	Seq int
	// Pos is where it arrived, in cube units.
	Pos protocol.Position
}

// itemKind is how the bridge flies one mission item.
//
// The mapping is deliberately small, and everything outside it is itemSkip
// rather than a rejection: a ground station plans a mission with camera
// triggers, speed changes and jumps in it, and refusing the whole upload
// because of one item the simulation has no meaning for would make the mission
// planner useless. A skipped item says so once, on the air.
type itemKind uint8

const (
	// itemSkip is accepted, never flown.
	itemSkip itemKind = iota
	// itemFly goes to the item's coordinate (NAV_WAYPOINT, NAV_TAKEOFF).
	itemFly
	// itemLand descends to the ground plane at the vehicle's own x/y.
	itemLand
	// itemHome flies back to the vehicle's home position (NAV_RETURN_TO_LAUNCH).
	itemHome
)

// itemState is where one item is in its life.
type itemState uint8

const (
	itemPending itemState = iota
	itemFlying
	itemDone
)

// missionItem is one accepted item.
type missionItem struct {
	seq  uint16
	cmd  uint16
	kind itemKind
	// pos is the item's absolute target in cube units, for itemFly.
	pos protocol.Position
	// hasXY is false for an item with no coordinate -- a ground station's
	// NAV_TAKEOFF carries an altitude and lat/lon 0. Flying to lat/lon 0 would
	// put the vehicle in the corner of the cube instead of straight up.
	hasXY bool
	// raw is the item exactly as it arrived, echoed on download. Keeping the
	// bytes rather than re-deriving them from pos is what makes a retransmitted
	// MISSION_REQUEST_INT idempotent: the ground station gets back the item it
	// uploaded, not a rounded reconstruction of it.
	raw MissionItemInt

	state itemState
	// node is the vehicle flying (or that flew) the item.
	node protocol.NodeID
	// goal is the target handed to that vehicle's mover, after clamping. An
	// arrival only counts while the mover still holds it: if an operator drags
	// the node or a LAND command arrives, the item is no longer being flown and
	// must not be credited to it.
	goal protocol.Position
}

// mission is one vehicle's stored mission and its execution state.
type mission struct {
	// owner is the sysid the mission was uploaded to. It is the mission's
	// identity to the ground station and never changes: that is where a
	// download comes from.
	owner byte
	// exec is the sysid whose cluster is flying it. It equals owner until the
	// owner dies mid-mission and the swarm elects somebody else.
	exec byte
	// leaderMission records whether the upload went to a leader. Fixed at
	// upload: a mission sent to a worker is that node's alone (contract section
	// 5), and a worker that is later promoted does not inherit a cluster-wide
	// mission it was never given.
	leaderMission bool

	items []missionItem

	// started gates execution. MAV_CMD_MISSION_START sets it, and arming is
	// checked there, so the armed flag means something rather than decorating a
	// heartbeat.
	started bool

	// current is the last MISSION_CURRENT sent; sentCurrent distinguishes
	// "never sent" from "sent 0".
	current     uint16
	sentCurrent bool

	// members is the cluster that was flying it at the previous pass, used to
	// find the swarm's new leader after the old one dies. The bridge does not
	// vote; it observes who the survivors now follow.
	members []protocol.NodeID
}

// upload is one transfer in flight from the ground station.
//
// Items are staged here rather than written into the mission as they arrive, so
// a transfer that dies halfway leaves the previous mission intact. A vehicle
// with half a mission on board is worse than one with the old mission on board:
// the operator cannot tell which.
type upload struct {
	typ   uint8
	count uint16
	// next is the sequence number the bridge has asked for and not yet received.
	next  uint16
	items []missionItem
	// gcsSys and gcsComp are who to address the ACK at.
	gcsSys  byte
	gcsComp byte
	// last is when the transfer last made progress, for the timeout.
	last time.Time
}

// --- addressing -----------------------------------------------------------------

// missionTarget resolves a mission message's addressing fields to one of our
// vehicles.
//
// Two rules that are easy to get wrong and both produce visible bugs:
//
//   - target_component 0 (broadcast within the system) and 1
//     (MAV_COMP_ID_AUTOPILOT1) are both us. Some ground stations address the
//     autopilot by name and some broadcast; rejecting either means the mission
//     protocol works in one and silently fails in the other.
//   - target_system 0 is a broadcast to every vehicle. It is IGNORED here, with
//     one log line, because answering it means eleven vehicles replying to one
//     request with eleven MISSION_COUNTs, and a ground station matching them to
//     whichever vehicle it was asking about. No ground station addresses a
//     mission transfer to 0; anything that does is confused already.
func (b *Bridge) missionTarget(target, comp byte) (*vehicle, bool) {
	if comp != 0 && comp != MavCompIDAutopilot1 {
		return nil, false
	}
	if target == 0 {
		if !b.warnedMissionBroadcast {
			b.warnedMissionBroadcast = true
			b.log.Warn("mavlink: ignoring broadcast mission request (target_system 0)")
		}
		return nil, false
	}
	v := b.vehicleBySysID(target)
	if v == nil {
		// Not one of ours. Silence is right here, and only here: some other
		// system on the link owns that sysid.
		return nil, false
	}
	return v, true
}

// missionAck sends a MISSION_ACK back to the sender of f.
func (b *Bridge) missionAck(sysID byte, f *Frame, typ uint8, result uint8) {
	b.send(sysID, &MissionAck{
		TargetSystem:    f.SysID,
		TargetComponent: f.CompID,
		Type:            result,
		MissionType:     typ,
	})
}

// statusf sends one STATUSTEXT from sysID. Truncated at maxStatusText.
func (b *Bridge) statusf(sysID byte, severity uint8, format string, args ...any) {
	m := &Statustext{Severity: severity}
	SetChars(m.Text[:], fmt.Sprintf(format, args...))
	b.send(sysID, m)
}

// --- inbound: download ----------------------------------------------------------

// onMissionRequestList answers with MISSION_COUNT, for every mission type.
func (b *Bridge) onMissionRequestList(f *Frame, m *MissionRequestList) {
	v, ok := b.missionTarget(m.TargetSystem, m.TargetComponent)
	if !ok {
		return
	}
	count := 0
	// FENCE and RALLY are answered with zero, not ignored: see the file header.
	if m.MissionType == MissionTypeMission {
		if ms := b.missions[v.sysID]; ms != nil {
			count = len(ms.items)
		}
	}
	b.send(v.sysID, &MissionCount{
		Count:           uint16(count),
		TargetSystem:    f.SysID,
		TargetComponent: f.CompID,
		MissionType:     m.MissionType,
	})
}

// onMissionRequest answers MISSION_REQUEST_INT (51) and the legacy
// MISSION_REQUEST (40) with a MISSION_ITEM_INT.
//
// Both are answered with the _INT form deliberately. MISSION_ITEM's float
// degrees lose about a centimetre of precision and the integer message is what
// every current ground station prefers; a legacy requester that cannot read
// MISSION_ITEM_INT is a peer this swarm has never had.
func (b *Bridge) onMissionRequest(f *Frame, target, comp byte, seq uint16, typ uint8) {
	v, ok := b.missionTarget(target, comp)
	if !ok {
		return
	}
	ms := b.missions[v.sysID]
	if typ != MissionTypeMission || ms == nil || int(seq) >= len(ms.items) {
		// There is no item to send, and the protocol has no "no such item"
		// message, so the refusal travels as an ACK. INVALID_SEQUENCE is what a
		// ground station reads as "stop asking for that one" instead of
		// retrying until it gives up.
		b.missionAck(v.sysID, f, typ, MavMissionInvalidSequence)
		return
	}
	// Echoed from the upload, so a retransmitted request gets a byte-identical
	// answer. Idempotence is the whole download story: a lossy link then
	// converges instead of failing.
	out := ms.items[seq].raw
	out.TargetSystem = f.SysID
	out.TargetComponent = f.CompID
	out.MissionType = MissionTypeMission
	b.send(v.sysID, &out)
}

// --- inbound: upload ------------------------------------------------------------

// onMissionCount starts an upload: the ground station announces how many items
// it has, and the vehicle asks for them one at a time.
func (b *Bridge) onMissionCount(f *Frame, m *MissionCount) {
	v, ok := b.missionTarget(m.TargetSystem, m.TargetComponent)
	if !ok {
		return
	}
	switch {
	case m.MissionType != MissionTypeMission:
		// A zero-length fence or rally upload is a clear, which succeeds
		// trivially. Anything with items in it is honestly refused: this bridge
		// has no geofence to enforce and no rally points to divert to, and
		// accepting them would claim a protection the swarm does not have.
		result := uint8(MavMissionAccepted)
		if m.Count > 0 {
			result = MavMissionUnsupported
		}
		b.missionAck(v.sysID, f, m.MissionType, result)
		return
	case m.Count > maxMissionItems:
		b.missionAck(v.sysID, f, m.MissionType, MavMissionNoSpace)
		return
	case m.Count == 0:
		// MISSION_COUNT 0 is how a ground station clears a mission by upload.
		b.clearMission(v.sysID)
		b.missionAck(v.sysID, f, m.MissionType, MavMissionAccepted)
		return
	}
	// A second MISSION_COUNT replaces any transfer in flight rather than
	// erroring: that is what a ground station that gave up and restarted sends,
	// and the alternative is a vehicle that cannot be re-uploaded to until the
	// stall timeout fires.
	b.uploads[v.sysID] = &upload{
		typ:     m.MissionType,
		count:   m.Count,
		items:   make([]missionItem, 0, m.Count),
		gcsSys:  f.SysID,
		gcsComp: f.CompID,
		last:    b.cfg.Now(),
	}
	b.requestItem(v.sysID, f, 0)
}

// requestItem asks for one item by sequence number.
func (b *Bridge) requestItem(sysID byte, f *Frame, seq uint16) {
	b.send(sysID, &MissionRequestInt{
		Seq:             seq,
		TargetSystem:    f.SysID,
		TargetComponent: f.CompID,
		MissionType:     MissionTypeMission,
	})
}

// onMissionItemInt takes one uploaded item.
func (b *Bridge) onMissionItemInt(f *Frame, m *MissionItemInt) {
	v, ok := b.missionTarget(m.TargetSystem, m.TargetComponent)
	if !ok {
		return
	}
	up := b.uploads[v.sysID]
	if up == nil {
		// No transfer in flight. Either this is the tail of one that timed out
		// or a ground station is confused; either way an ACK ends the exchange
		// instead of leaving it retrying.
		b.missionAck(v.sysID, f, m.MissionType, MavMissionError)
		return
	}
	switch {
	case m.MissionType != up.typ:
		b.missionAck(v.sysID, f, m.MissionType, MavMissionInvalid)
		delete(b.uploads, v.sysID)
		return
	case m.Seq < up.next:
		// A duplicate: the ground station did not see our request for the next
		// one and resent the last. Re-request where we actually are. Position in
		// the sequence is NOT lost, which is the difference between a lossy link
		// that converges and one that restarts the transfer from item 0.
		up.last = b.cfg.Now()
		b.requestItem(v.sysID, f, up.next)
		return
	case m.Seq > up.next:
		// A gap. Accepting it would store a mission with a hole in it, and the
		// ground station would download something it never uploaded.
		b.missionAck(v.sysID, f, m.MissionType, MavMissionInvalidSequence)
		delete(b.uploads, v.sysID)
		return
	}
	item, result := b.acceptItem(m)
	if result != MavMissionAccepted {
		b.missionAck(v.sysID, f, m.MissionType, result)
		delete(b.uploads, v.sysID)
		return
	}
	up.items = append(up.items, item)
	up.next++
	up.last = b.cfg.Now()
	if up.next < up.count {
		b.requestItem(v.sysID, f, up.next)
		return
	}
	delete(b.uploads, v.sysID)
	b.installMission(v, up.items)
	b.missionAck(v.sysID, f, m.MissionType, MavMissionAccepted)
	b.log.Info("mavlink: mission accepted", "peer", v.id, "sysid", v.sysID, "items", len(up.items))
}

// onMissionItem takes a legacy float MISSION_ITEM (39) by widening it to the
// integer form. The two messages carry the same fields with lat/lon in degrees
// rather than degrees * 1e7, so there is one conversion and no second code path
// through the state machine.
func (b *Bridge) onMissionItem(f *Frame, m *MissionItem) {
	b.onMissionItemInt(f, &MissionItemInt{
		Param1:          m.Param1,
		Param2:          m.Param2,
		Param3:          m.Param3,
		Param4:          m.Param4,
		X:               toInt32(math.Round(float64(m.X) * 1e7)),
		Y:               toInt32(math.Round(float64(m.Y) * 1e7)),
		Z:               m.Z,
		Seq:             m.Seq,
		Command:         m.Command,
		TargetSystem:    m.TargetSystem,
		TargetComponent: m.TargetComponent,
		Frame:           m.Frame,
		Current:         m.Current,
		Autocontinue:    m.Autocontinue,
		MissionType:     m.MissionType,
	})
}

// acceptItem validates one item and turns it into a missionItem.
//
// The returned MAV_MISSION_RESULT is the whole contract with the ground
// station: MAV_MISSION_UNSUPPORTED_FRAME for a frame the projection cannot
// read, MAV_MISSION_ACCEPTED for everything else, including commands this
// bridge will skip.
func (b *Bridge) acceptItem(m *MissionItemInt) (missionItem, uint8) {
	it := missionItem{seq: m.Seq, cmd: m.Command, raw: *m}
	switch m.Command {
	case MavCmdNavWaypoint, MavCmdNavTakeoff:
		it.kind = itemFly
	case MavCmdNavLand:
		it.kind = itemLand
	case MavCmdNavReturnToLaunch:
		it.kind = itemHome
	default:
		// Accepted and skipped; see itemKind. No frame check: a DO_ or
		// CONDITION_ item comes in MAV_FRAME_MISSION, which is not a coordinate
		// frame at all, and rejecting it would fail an upload a ground station
		// considers perfectly ordinary.
		it.kind = itemSkip
		return it, MavMissionAccepted
	}

	// Only the items that actually navigate need a frame the projection can
	// read. RETURN_TO_LAUNCH carries no coordinate, so its frame is whatever
	// the planner felt like and must not be judged.
	if it.kind == itemHome {
		return it, MavMissionAccepted
	}

	// The altitude reference is the only thing the frame decides here:
	// MISSION_ITEM_INT always carries x/y as degrees * 1e7, whatever the frame
	// says.
	g := Global{LatE7: m.X, LonE7: m.Y}
	switch m.Frame {
	case MavFrameGlobalRelativeAltInt, MavFrameGlobalRelativeAlt:
		// Relative altitude is height above the GROUND PLANE (cube z = 0),
		// which projects to exactly the origin altitude. This is the frame
		// QGroundControl plans in by default.
		g.AltMM = toInt32(math.Round(b.cfg.Projection.OriginAltM*1000 + float64(m.Z)*1000))
	case MavFrameGlobalInt, MavFrameGlobal:
		// AMSL. Unproject subtracts the origin altitude itself.
		g.AltMM = toInt32(math.Round(float64(m.Z) * 1000))
	default:
		// LOCAL_NED, TERRAIN_ALT and the rest. Terrain-relative altitude would
		// need a terrain model, and this cube has none; claiming support would
		// fly the vehicle to the wrong height silently.
		return it, MavMissionUnsupportedFrame
	}
	it.pos = b.cfg.Projection.Unproject(g)
	// lat/lon exactly 0 is how a ground station says "no coordinate" on a
	// takeoff item. Treating it as a position would fly to the cube's corner.
	it.hasXY = m.X != 0 || m.Y != 0
	return it, MavMissionAccepted
}

// installMission replaces the vehicle's mission with a freshly uploaded one.
func (b *Bridge) installMission(v *vehicle, items []missionItem) {
	b.clearAssignments(b.missions[v.sysID])
	ms := &mission{
		owner:         v.sysID,
		exec:          v.sysID,
		leaderMission: v.leader,
		items:         items,
	}
	b.missions[v.sysID] = ms
	b.sendCurrent(ms)
}

// clearMission forgets a vehicle's mission and stops anything flying it.
func (b *Bridge) clearMission(sysID byte) {
	b.clearAssignments(b.missions[sysID])
	delete(b.missions, sysID)
	delete(b.uploads, sysID)
}

// clearAssignments releases every vehicle currently flying an item of ms, so a
// cleared or replaced mission does not leave a node flying to a waypoint that
// no longer exists.
func (b *Bridge) clearAssignments(ms *mission) {
	if ms == nil {
		return
	}
	for i := range ms.items {
		it := &ms.items[i]
		if it.state != itemFlying {
			continue
		}
		if v := b.vehicleByNode(it.node); v != nil && b.holdsGoal(v, *it) {
			v.mover.ClearTarget()
		}
		it.state = itemPending
		it.node = ""
	}
}

// onMissionClearAll clears and acks.
func (b *Bridge) onMissionClearAll(f *Frame, m *MissionClearAll) {
	v, ok := b.missionTarget(m.TargetSystem, m.TargetComponent)
	if !ok {
		return
	}
	// Clearing a fence or a rally list that was never stored succeeds: the
	// post-condition the ground station asked for already holds.
	if m.MissionType == MissionTypeMission {
		b.clearMission(v.sysID)
	}
	b.missionAck(v.sysID, f, m.MissionType, MavMissionAccepted)
}

// onMissionSetCurrent jumps to an item and reports MISSION_CURRENT.
func (b *Bridge) onMissionSetCurrent(f *Frame, m *MissionSetCurrent) {
	v, ok := b.missionTarget(m.TargetSystem, m.TargetComponent)
	if !ok {
		return
	}
	ms := b.missions[v.sysID]
	if ms == nil || int(m.Seq) >= len(ms.items) {
		b.missionAck(v.sysID, f, MissionTypeMission, MavMissionInvalidSequence)
		return
	}
	// A jump forward abandons the items behind it -- that is what jumping means
	// -- so they are marked done and the planner does not fly them later. Items
	// already in the air keep going: the operator moved the mission on, they
	// did not ask for a vehicle to stop mid-leg.
	for i := range ms.items {
		it := &ms.items[i]
		if it.seq < m.Seq && it.state == itemPending {
			it.state = itemDone
		}
	}
	ms.current = m.Seq
	ms.sentCurrent = true
	b.send(ms.exec, b.missionCurrent(ms, m.Seq))
}

// --- inbound: commands ----------------------------------------------------------

// runCommand executes one COMMAND_LONG and returns the MAV_RESULT to ack with.
//
// The rule inherited from the original bridge still holds: anything genuinely
// unsupported is acked MAV_RESULT_UNSUPPORTED rather than ignored, because a
// ground station retries an unacknowledged command for ever.
func (b *Bridge) runCommand(v *vehicle, m *CommandLong) uint8 {
	switch m.Command {
	case MavCmdComponentArmDisarm:
		return b.cmdArm(v, m)
	case MavCmdNavTakeoff:
		return b.cmdTakeoff(v, m)
	case MavCmdNavLand:
		return b.cmdLand(v)
	case MavCmdNavReturnToLaunch:
		return b.cmdReturnToLaunch(v)
	case MavCmdMissionStart:
		return b.cmdMissionStart(v)
	case MavCmdDoSetMode:
		// UNSUPPORTED, and the reason is structural rather than laziness:
		// HEARTBEAT.custom_mode carries the swarm role (1 = leader), which is
		// the one piece of swarm state a ground station can display with no
		// plugin. A flight mode written there would overwrite it, so the
		// operator would gain a mode field that means nothing and lose the only
		// indication of which vehicle is leading. There is also nothing to put
		// a mode into: motion here is a mover and a target, not a controller.
		return MavResultUnsupported
	default:
		return MavResultUnsupported
	}
}

// cmdArm arms or disarms. param1 is 1 to arm, 0 to disarm.
//
// Arming is advisory -- there is no airframe to energise -- but it is not
// cosmetic: a disarmed vehicle refuses to start a mission (see cmdMissionStart),
// so the flag the ground station's Arm button sets is the same flag execution
// checks.
func (b *Bridge) cmdArm(v *vehicle, m *CommandLong) uint8 {
	switch {
	case m.Param1 == 1:
		v.armed = true
		b.log.Info("mavlink: armed", "peer", v.id, "sysid", v.sysID)
		return MavResultAccepted
	case m.Param1 == 0:
		v.armed = false
		// Disarming stops the vehicle and hands back whatever mission item it
		// was flying, so the planner can give it to somebody else. A vehicle
		// that reported disarmed while still travelling to a waypoint would be
		// lying on the one field the operator uses to decide it is safe.
		v.mover.ClearTarget()
		b.log.Info("mavlink: disarmed", "peer", v.id, "sysid", v.sysID)
		return MavResultAccepted
	default:
		// MAV_RESULT_DENIED, not FAILED: the command is supported, the
		// parameter is not.
		return MavResultDenied
	}
}

// cmdTakeoff climbs to param7 metres above the ground plane, over the vehicle's
// current x/y.
func (b *Bridge) cmdTakeoff(v *vehicle, m *CommandLong) uint8 {
	if !v.armed {
		b.statusf(v.sysID, SeverityWarning, "takeoff refused: not armed")
		// TEMPORARILY_REJECTED is the honest code: arming fixes it, and a
		// ground station says so to the operator instead of reporting a
		// permanent failure.
		return MavResultTemporarilyRejected
	}
	if !(m.Param7 > 0) {
		return MavResultDenied
	}
	p := v.mover.Pos()
	p.Z = float64(m.Param7) / b.cfg.Projection.MetresPerUnit
	v.mover.SetTarget(p)
	b.releaseItems(v.id)
	return MavResultAccepted
}

// cmdLand descends to the ground plane, keeping x/y.
func (b *Bridge) cmdLand(v *vehicle) uint8 {
	p := v.mover.Pos()
	p.Z = 0
	v.mover.SetTarget(p)
	b.releaseItems(v.id)
	return MavResultAccepted
}

// cmdReturnToLaunch flies home.
func (b *Bridge) cmdReturnToLaunch(v *vehicle) uint8 {
	v.mover.SetTarget(v.mover.Home())
	b.releaseItems(v.id)
	return MavResultAccepted
}

// cmdMissionStart begins executing the stored mission.
func (b *Bridge) cmdMissionStart(v *vehicle) uint8 {
	ms := b.missions[v.sysID]
	if ms == nil || len(ms.items) == 0 {
		b.statusf(v.sysID, SeverityWarning, "no mission to start")
		return MavResultDenied
	}
	if !v.armed {
		b.statusf(v.sysID, SeverityWarning, "mission refused: not armed")
		return MavResultTemporarilyRejected
	}
	ms.started = true
	b.runMissions()
	return MavResultAccepted
}

// releaseItems hands back any mission item node was flying, because an operator
// command has just taken the vehicle over. The item goes back in the pool
// instead of being credited: nobody flew it.
func (b *Bridge) releaseItems(node protocol.NodeID) {
	for _, sysID := range b.missionOrder() {
		ms := b.missions[sysID]
		for i := range ms.items {
			it := &ms.items[i]
			if it.state == itemFlying && it.node == node {
				it.state = itemPending
				it.node = ""
			}
		}
	}
}

// --- upload timeout -------------------------------------------------------------

// expireUploads abandons transfers that have stalled. Called from the status
// tick, so the resolution is StatusInterval; the timeout is in seconds and the
// tick is 1 Hz.
func (b *Bridge) expireUploads() {
	if len(b.uploads) == 0 {
		return
	}
	now := b.cfg.Now()
	// Sorted, so two uploads timing out in the same tick produce the same two
	// frames in the same order on every run.
	sysIDs := make([]byte, 0, len(b.uploads))
	for sysID := range b.uploads {
		sysIDs = append(sysIDs, sysID)
	}
	sort.Slice(sysIDs, func(i, j int) bool { return sysIDs[i] < sysIDs[j] })
	for _, sysID := range sysIDs {
		up := b.uploads[sysID]
		if now.Sub(up.last) < b.cfg.MissionUploadTimeout {
			continue
		}
		delete(b.uploads, sysID)
		// The previous mission is untouched; items were staged, not stored. The
		// ACK matters more than the cleanup: it tells the ground station the
		// transfer is over, so the next upload starts from MISSION_COUNT
		// instead of being refused as out of sequence for ever.
		b.send(sysID, &MissionAck{
			TargetSystem:    up.gcsSys,
			TargetComponent: up.gcsComp,
			Type:            MavMissionOperationCancelled,
			MissionType:     up.typ,
		})
		b.statusf(sysID, SeverityWarning, "mission upload stalled at %d/%d", up.next, up.count)
		b.log.Warn("mavlink: mission upload timed out", "sysid", sysID, "got", up.next, "want", up.count)
	}
}

// --- execution ------------------------------------------------------------------

// runMissions is the swarm-level half of the protocol: arrivals, reassignment
// after a death, and handing pending items to idle cluster members.
//
// Called from tickPositions, after every mover has advanced, so an arrival is
// noticed within one position tick; and from applyNodes, so a re-election is
// acted on as soon as the swarm reports it rather than up to 200 ms later.
// Loop-owned, like everything it touches.
func (b *Bridge) runMissions() {
	for _, sysID := range b.missionOrder() {
		ms := b.missions[sysID]
		b.collectArrivals(ms)
		b.migrate(ms)
		b.assignItems(ms)
		b.sendCurrent(ms)
	}
}

// missionOrder is the mission keys, sorted. Mission execution moves vehicles
// and emits frames, so the order it happens in has to be reproducible.
func (b *Bridge) missionOrder() []byte {
	if len(b.missions) == 0 {
		return nil
	}
	out := make([]byte, 0, len(b.missions))
	for sysID := range b.missions {
		out = append(out, sysID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// holdsGoal reports whether v is still flying the target it was given for it.
func (b *Bridge) holdsGoal(v *vehicle, it missionItem) bool {
	tgt, ok := v.mover.Target()
	return ok && tgt == it.goal
}

// collectArrivals credits finished items and releases lost ones.
func (b *Bridge) collectArrivals(ms *mission) {
	for i := range ms.items {
		it := &ms.items[i]
		if it.state != itemFlying {
			continue
		}
		v := b.vehicleByNode(it.node)
		switch {
		case v == nil || !v.connected:
			// The node carrying it has died. The contract protects items in
			// flight on SURVIVING workers; this one is not surviving, so the
			// item goes back in the pool.
			it.state = itemPending
			it.node = ""
		case !b.holdsGoal(v, *it):
			// Something else took the vehicle: an operator dragged it on the
			// dashboard, or a LAND arrived. It is not flying this item any
			// more, and crediting its eventual arrival to this waypoint would
			// report a waypoint reached that nobody visited.
			it.state = itemPending
			it.node = ""
		case v.mover.Reached():
			it.state = itemDone
			v.mover.ClearTarget()
			b.send(ms.exec, &MissionItemReached{Seq: it.seq})
			b.queueTask(MissionTask{
				Kind: missionTaskKind,
				Node: v.id,
				Seq:  int(it.seq),
				Pos:  v.mover.Pos(),
			})
		}
	}
}

// migrate follows the swarm's election after the vehicle a mission was uploaded
// to has died.
//
// The bridge does not vote and does not promote anybody: it reads Leader out of
// the swarm's own state and moves the mission to whoever the survivors now
// follow. Items already in flight are left exactly as they are.
func (b *Bridge) migrate(ms *mission) {
	if !ms.leaderMission {
		// A worker's mission is that node's alone (contract section 5). There
		// is no cluster to move it to, and promoting it to a cluster mission
		// because its node died would be inventing intent.
		return
	}
	if v := b.vehicleBySysID(ms.exec); v != nil && v.connected && v.leader {
		return
	}
	next := b.electedLeaderFor(ms)
	if next == nil || next.sysID == ms.exec {
		// No leader yet. The swarm is mid-election; the mission waits, which is
		// strictly better than scattering its items over leaderless nodes.
		return
	}
	pending := 0
	for i := range ms.items {
		if ms.items[i].state == itemPending {
			pending++
		}
	}
	b.log.Info("mavlink: mission moved to new leader",
		"from_sysid", ms.exec, "to_sysid", next.sysID, "peer", next.id, "pending", pending)
	ms.exec = next.sysID
	// On the NEW leader's sysid, because the old one has stopped heartbeating
	// and a ground station has already marked it lost: a STATUSTEXT from a dead
	// vehicle is a message nobody reads.
	b.statusf(next.sysID, SeverityWarning, "mission moved here: %d items left", pending)
	// Force the next MISSION_CURRENT out even if the index has not changed, so
	// the new vehicle reports progress rather than looking idle.
	ms.sentCurrent = false
}

// electedLeaderFor finds the leader a stranded mission belongs to.
func (b *Bridge) electedLeaderFor(ms *mission) *vehicle {
	// First choice: the leader the surviving members have attached themselves
	// to. That is the swarm's own answer, observed rather than chosen, so the
	// mission follows the cluster instead of splitting it.
	for _, id := range ms.members {
		v := b.vs[id]
		if v == nil || !v.connected || v.leaderID == "" {
			continue
		}
		if lv := b.vs[v.leaderID]; lv != nil && lv.connected && lv.leader {
			return lv
		}
	}
	// Fallback: the lowest-numbered connected leader. Used when the whole
	// cluster went with its leader, which leaves nobody to ask.
	for _, id := range b.order {
		if v := b.vs[id]; v != nil && v.connected && v.leader {
			return v
		}
	}
	return nil
}

// execMembers is the cluster flying ms, leader first and then by node id.
func (b *Bridge) execMembers(ms *mission) []*vehicle {
	ex := b.vehicleBySysID(ms.exec)
	if ex == nil || !ex.connected {
		return nil
	}
	if !ms.leaderMission {
		return []*vehicle{ex}
	}
	out := make([]*vehicle, 0, len(b.order))
	out = append(out, ex)
	for _, id := range b.order {
		v := b.vs[id]
		if v == nil || v == ex || !v.connected {
			continue
		}
		if v.leaderID == ex.id {
			out = append(out, v)
		}
	}
	return out
}

// assignItems hands pending items to idle members, in sequence order.
func (b *Bridge) assignItems(ms *mission) {
	members := b.execMembers(ms)
	ms.members = ms.members[:0]
	for _, v := range members {
		ms.members = append(ms.members, v.id)
	}
	if !ms.started {
		// Nothing moves until MAV_CMD_MISSION_START, and that needs an armed
		// vehicle. An upload is a plan, not a launch.
		return
	}
	busy := b.busyNodes()
	for i := range ms.items {
		it := &ms.items[i]
		if it.state != itemPending {
			continue
		}
		if it.kind == itemSkip {
			// One line on the air and move on. Rejecting the upload over this
			// item would have thrown away the whole plan; silently dropping it
			// would leave the operator wondering why the vehicle skipped a leg.
			it.state = itemDone
			b.statusf(ms.exec, SeverityWarning, "item %d (cmd %d) skipped", it.seq, it.cmd)
			b.send(ms.exec, &MissionItemReached{Seq: it.seq})
			continue
		}
		v := b.nearestIdle(members, busy, *it)
		if v == nil {
			// Everybody is busy. Later items wait: handing item 5 out while
			// item 3 has nobody to fly it would break the sequence order the
			// contract promises.
			return
		}
		v.mover.SetTarget(b.goalFor(v, *it))
		// Read the target back: SetTarget clamps to the cube, and the clamped
		// value is what an arrival is compared against.
		it.goal, _ = v.mover.Target()
		// A member handed a waypoint is flying, so it reports armed. The
		// alternative -- demanding the operator arm each worker in a cluster
		// mission -- makes the swarm's one-mission-many-vehicles behaviour
		// unusable.
		v.armed = true
		it.state = itemFlying
		it.node = v.id
		busy[v.id] = struct{}{}
	}
}

// busyNodes is every node currently flying a mission item, across all missions.
// One node can only be in one place, and two leaders' missions may legitimately
// overlap in membership while the swarm is re-electing.
func (b *Bridge) busyNodes() map[protocol.NodeID]struct{} {
	out := make(map[protocol.NodeID]struct{})
	for _, sysID := range b.missionOrder() {
		for _, it := range b.missions[sysID].items {
			if it.state == itemFlying {
				out[it.node] = struct{}{}
			}
		}
	}
	return out
}

// nearestIdle picks the idle member closest to where the item would send it.
//
// Distance to the item's own target, not to the item's coordinate, so LAND and
// RETURN_TO_LAUNCH -- whose target depends on the vehicle -- sort sensibly
// instead of all scoring the same.
func (b *Bridge) nearestIdle(members []*vehicle, busy map[protocol.NodeID]struct{}, it missionItem) *vehicle {
	var best *vehicle
	bestDist := math.Inf(1)
	for _, v := range members {
		if _, taken := busy[v.id]; taken || !v.connected || v.mover.Flying() {
			continue
		}
		d := lenP(subP(b.goalFor(v, it), v.mover.Pos()))
		// Strictly closer, and members are in a deterministic order, so an
		// exact tie goes to the earlier one every time.
		if d < bestDist {
			best, bestDist = v, d
		}
	}
	return best
}

// goalFor is where item it sends vehicle v.
func (b *Bridge) goalFor(v *vehicle, it missionItem) protocol.Position {
	p := v.mover.Pos()
	switch it.kind {
	case itemFly:
		if !it.hasXY {
			// A takeoff with no coordinate: climb in place.
			p.Z = it.pos.Z
			return p
		}
		return it.pos
	case itemLand:
		p.Z = 0
		return p
	case itemHome:
		return v.mover.Home()
	default:
		return p
	}
}

// sendCurrent emits MISSION_CURRENT when the item being worked on changes.
//
// On change rather than at 1 Hz: the contract's message table says "on change",
// and a ground station redraws its mission progress on every one of these.
func (b *Bridge) sendCurrent(ms *mission) {
	if len(ms.items) == 0 {
		return
	}
	cur := ms.items[len(ms.items)-1].seq
	for i := range ms.items {
		if ms.items[i].state != itemDone {
			cur = ms.items[i].seq
			break
		}
	}
	if ms.sentCurrent && ms.current == cur {
		return
	}
	ms.current = cur
	ms.sentCurrent = true
	b.send(ms.exec, b.missionCurrent(ms, cur))
}

func (b *Bridge) missionCurrent(ms *mission, seq uint16) *MissionCurrent {
	state := uint8(MissionStateNotStarted)
	switch {
	case len(ms.items) == 0:
		state = MissionStateNoMission
	case b.missionComplete(ms):
		state = MissionStateComplete
	case ms.started:
		state = MissionStateActive
	}
	return &MissionCurrent{
		Seq:          seq,
		Total:        uint16(len(ms.items)),
		MissionState: state,
	}
}

func (b *Bridge) missionComplete(ms *mission) bool {
	for i := range ms.items {
		if ms.items[i].state != itemDone {
			return false
		}
	}
	return len(ms.items) > 0
}

// queueTask hands a waypoint task to the task goroutine. Never blocks: the
// bridge's loop is also answering a ground station, and a swarm with no leader
// (mid-election, exactly when a mission is interesting) makes the submission
// slow. A dropped task costs a row in the dashboard's table, not a waypoint.
func (b *Bridge) queueTask(t MissionTask) {
	select {
	case b.tasks <- t:
	default:
		b.log.Debug("mavlink: waypoint task dropped, queue full", "peer", t.Node, "seq", t.Seq)
	}
}

// vehicleByNode looks a vehicle up by swarm node id.
func (b *Bridge) vehicleByNode(id protocol.NodeID) *vehicle {
	if id == "" {
		return nil
	}
	return b.vs[id]
}
