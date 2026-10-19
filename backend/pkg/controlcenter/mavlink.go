package controlcenter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"swarm-net/pkg/geo"
	"swarm-net/pkg/mavlink"
	"swarm-net/pkg/protocol"
)

// The MAVLink bridge, Control Center side (docs/architecture/mavlink-bridge.md).
//
// The bridge itself lives in pkg/mavlink and knows nothing about the CC. This
// file is the whole adapter: it implements mavlink.SwarmSource by posting
// closures to the hub, exactly as the HTTP handlers do.
//
// # Why an adapter and not a second reader of hub state
//
// The hub goroutine is the single owner of h.nodes and h.sim (see Server). The
// bridge runs its own loop at its own rates, so every exchange between them has
// to cross goroutines, and the CC already has one way to do that: do and post.
// Reaching into h.sim.positions from the bridge's loop -- even under a new
// mutex -- would make "who last moved this node" unanswerable, which is the
// question the latency simulation and the election both depend on.

// mavSource gives the bridge its view of the swarm.
type mavSource struct{ s *Server }

// Nodes implements mavlink.SwarmSource.
func (m mavSource) Nodes(ctx context.Context) ([]mavlink.NodeState, error) {
	var out []mavlink.NodeState
	if !m.s.do(ctx, func(h *hub) { out = h.mavNodes() }) {
		// The hub has stopped, or ctx ended. Terminal for this sync, not for
		// the bridge: the next tick either works or ends the same way, and the
		// bridge keeps heartbeating from its own state meanwhile.
		return nil, ErrUnavailable
	}
	return out, nil
}

// SetPositions implements mavlink.SwarmSource.
func (m mavSource) SetPositions(ctx context.Context, positions map[protocol.NodeID]protocol.Position) error {
	if !m.s.do(ctx, func(h *hub) { h.mavSetPositions(positions) }) {
		return ErrUnavailable
	}
	return nil
}

// mavNodes snapshots the node table for the bridge. Hub-owned.
//
// Sorted by id so the bridge assigns system ids in a stable order on the first
// sync, which is what makes a vehicle's number reproducible across restarts for
// nodes whose ids carry no digits.
func (h *hub) mavNodes() []mavlink.NodeState {
	out := make([]mavlink.NodeState, 0, len(h.nodes))
	for id, n := range h.nodes {
		connected := n.link != nil
		out = append(out, mavlink.NodeState{
			ID: id,
			// A disconnected node's last report said "leader"; it is not one
			// now, and a ground station must not show a dead leader's mode.
			Leader: connected && n.tel.Role == "leader",
			// Who this node follows, for cluster membership. Empty while it is
			// disconnected: a dead node's last known leader is not a cluster a
			// mission should be handed to.
			LeaderID:   leaderOf(connected, n.tel.Leader),
			Connected:  connected,
			Pos:        h.positionOf(id),
			Dropped:    n.tel.Dropped,
			LedgerSize: n.tel.LedgerSize,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// leaderOf is the node's current leader, or empty while it is disconnected.
func leaderOf(connected bool, leader protocol.NodeID) protocol.NodeID {
	if !connected {
		return ""
	}
	return leader
}

// SubmitTask implements mavlink.SwarmSource: a reached waypoint becomes a real
// task on the swarm's own TASK / TASK_RESULT path.
//
// The body carries the waypoint, so the dashboard's task table shows which
// mission item a row came from. Count is 1: one arrival is one task.
func (m mavSource) SubmitTask(ctx context.Context, t mavlink.MissionTask) error {
	body, err := json.Marshal(map[string]any{
		"msg":  fmt.Sprintf("waypoint %d reached by %s", t.Seq, t.Node),
		"node": string(t.Node),
		"seq":  t.Seq,
	})
	if err != nil {
		return err
	}
	var ids []string
	if !m.s.do(ctx, func(h *hub) {
		ids, err = h.submitTasks(ClientMessage{Kind: t.Kind, Body: body, Count: 1})
	}) {
		return ErrUnavailable
	}
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		// submitTasks returns no ids when the swarm has no leader to take the
		// task, which happens mid-election. Not an error worth retrying: the
		// waypoint is already flown.
		m.s.log.Debug("mavlink: waypoint task accepted by nobody", "peer", t.Node, "seq", t.Seq)
	}
	return nil
}

// mavSetPositions applies the bridge's positions. Hub-owned.
//
// Three deliberate differences from updateSim:
//
//   - No event. This runs once a second for every flying or drifting node; an
//     event per sync would drown the dashboard's event log in exactly the
//     noise an operator is trying to read elections out of.
//   - dirty rather than broadcastSim. The snapshot tick already flushes dirty
//     once per SnapshotInterval, coalescing with node joins, so the swarm sees
//     one SIM_CONFIG per tick however many vehicles moved.
//   - Unknown ids are ignored instead of creating a placement. The bridge only
//     ever pushes ids it was given by mavNodes, so an unknown id means the node
//     expired mid-sync; honouring it would let the position map grow behind
//     maxSimPositions' back.
func (h *hub) mavSetPositions(positions map[protocol.NodeID]protocol.Position) {
	changed := false
	for id, p := range positions {
		if _, listed := h.nodes[id]; !listed {
			continue
		}
		p = geo.ClampPosition(p)
		if cur, ok := h.sim.positions[id]; ok && cur == p {
			continue
		}
		h.sim.positions[id] = p
		changed = true
	}
	if !changed {
		return
	}
	h.bumpSim()
	h.sim.dirty = true
}

// chainSinks composes two optional event sinks, returning nil when both are.
// Keeping the composition here means hub.event has one branch, not two.
func chainSinks(a, b func(kind string, node protocol.NodeID, detail string)) func(string, protocol.NodeID, string) {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return func(kind string, node protocol.NodeID, detail string) {
		a(kind, node, detail)
		b(kind, node, detail)
	}
}
