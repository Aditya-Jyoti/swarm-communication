package controlcenter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"swarm-net/pkg/geo"
	"swarm-net/pkg/protocol"
)

// The drone simulation, CC side (docs/architecture/drone-simulation.md).
//
// The CC is the single source of truth for the emulated airspace: every
// node's position, the latency model and the election overrides. It pushes
// all of it to every node as one SIM_CONFIG snapshot. A snapshot rather than
// deltas because the CC->node sends are asynchronous (sendAsync, one
// goroutine each) and may land out of order or not at all; with a version on
// a full snapshot, a node simply keeps the newest one it has seen, and a
// stale frame arriving late is harmless.

const (
	// flowTTL is how long a node's flows sample stays in the snapshot. A node
	// that stops reporting must not leave dots animating on a link for ever.
	flowTTL = 3 * time.Second
	// maxSimPositions caps the position map. Entries are ~70 bytes on the
	// wire, so the cap keeps one SIM_CONFIG far below protocol.MaxFrameSize,
	// and it bounds what a stream of made-up node IDs can make the CC hold.
	maxSimPositions = 4096
	// maxSimNodeIDLen caps an operator-supplied position key.
	maxSimNodeIDLen = 128
	// maxHysteresis caps the hysteresis override. The margin is in score
	// units (ms under the latency strategy); anything above the largest
	// emulated delay could never be beaten and would freeze the election.
	maxHysteresis = float64(geo.MaxDelay / time.Millisecond)
	// noHysteresis is the "no operator override" value on the wire.
	noHysteresis = -1
)

// simState is the CC's emulation settings. Hub-owned, like everything else in
// hub.
type simState struct {
	version uint64
	enabled bool
	params  geo.Params
	// threshold is the operator override; 0 means none.
	threshold float64
	// hysteresis is the operator override; noHysteresis means none.
	hysteresis float64
	// positions holds every node ever seen, plus operator placements for IDs
	// that have not connected yet.
	positions map[protocol.NodeID]protocol.Position
	// dirty means positions changed because nodes joined, and the other
	// nodes have not been told yet. Flushed on the snapshot tick, so a burst
	// of joins (a whole swarm starting) costs one broadcast, not N.
	dirty bool
}

func newSimState(cfg Config) simState {
	params := geo.DefaultParams()
	if cfg.SimParams != nil {
		params = cfg.SimParams.Clamp()
	}
	return simState{
		enabled:    !cfg.SimDisabled,
		params:     params,
		hysteresis: noHysteresis,
		positions:  make(map[protocol.NodeID]protocol.Position),
	}
}

// bumpSim advances the version. It never goes below the wall clock in
// milliseconds: nodes keep only a NEWER version, so a restarted CC counting
// from 1 would be ignored by every node that outlived it. Seeding from the
// clock makes a restart look newer, and +1 keeps versions strictly
// increasing even if the clock steps back.
func (h *hub) bumpSim() {
	v := h.sim.version + 1
	if ms := h.now().UnixMilli(); ms > 0 && uint64(ms) > v {
		v = uint64(ms)
	}
	h.sim.version = v
}

func (h *hub) simView() SimView {
	return SimView{
		Version:    h.sim.version,
		Enabled:    h.sim.enabled,
		BaseMS:     h.sim.params.BaseMS,
		PerUnitMS:  h.sim.params.PerUnitMS,
		JitterMS:   h.sim.params.JitterMS,
		Threshold:  h.sim.threshold,
		Hysteresis: h.sim.hysteresis,
		Size:       geo.Size,
		MaxDelayMS: float64(geo.MaxDelay / time.Millisecond),
	}
}

// simPayload encodes the current SIM_CONFIG body once, so a broadcast to N
// nodes does not re-encode a map of up to maxSimPositions entries N times.
// The map is encoded here, on the hub, so no other goroutine ever reads it.
func (h *hub) simPayload() (json.RawMessage, error) {
	return json.Marshal(protocol.SimConfigPayload{
		Version:    h.sim.version,
		Enabled:    h.sim.enabled,
		Positions:  h.sim.positions,
		BaseMS:     h.sim.params.BaseMS,
		PerUnitMS:  h.sim.params.PerUnitMS,
		JitterMS:   h.sim.params.JitterMS,
		Threshold:  h.sim.threshold,
		Hysteresis: h.sim.hysteresis,
	})
}

// sendSim sends body to one connected node. SIM_CONFIG is control plane, so
// the send may wait for queue space; sendAsync keeps that wait off the hub,
// exactly as for CHAOS. A send that fails is logged and not retried: the next
// change carries the full state anyway.
func (h *hub) sendSim(n *nodeEntry, body json.RawMessage) {
	if n.link == nil {
		return
	}
	env, err := protocol.NewEnvelope(protocol.TypeSimConfig, NodeID, n.id, body)
	if err != nil {
		h.s.log.Error("encode SIM_CONFIG", "peer", n.id, "err", err)
		return
	}
	h.sendAsync(n.link, env)
}

// broadcastSim pushes the current config to every connected node.
func (h *hub) broadcastSim() {
	h.sim.dirty = false
	body, err := h.simPayload()
	if err != nil {
		h.s.log.Error("encode SIM_CONFIG", "err", err)
		return
	}
	for _, n := range h.nodes {
		h.sendSim(n, body)
	}
}

// simJoin runs once a node's link is registered. A node seen for the first
// time gets its default position, which changes the config: bump now, so the
// joiner's copy already has a version of its own, and let the next tick tell
// everyone else.
func (h *hub) simJoin(n *nodeEntry) {
	if _, ok := h.sim.positions[n.id]; !ok {
		h.makeRoom(1)
		h.sim.positions[n.id] = geo.DefaultPosition(n.id)
		h.bumpSim()
		h.sim.dirty = true
	}
	body, err := h.simPayload()
	if err != nil {
		h.s.log.Error("encode SIM_CONFIG", "peer", n.id, "err", err)
		return
	}
	h.sendSim(n, body)
}

// makeRoom frees space for extra new positions, only when the cap is hit, by
// forgetting every ID that is not listed: expired nodes and operator
// placements still waiting for their node. A listed node always keeps its
// entry. It reports whether there is room now.
func (h *hub) makeRoom(extra int) bool {
	if len(h.sim.positions)+extra <= maxSimPositions {
		return true
	}
	for id := range h.sim.positions {
		if _, listed := h.nodes[id]; !listed {
			delete(h.sim.positions, id)
		}
	}
	return len(h.sim.positions)+extra <= maxSimPositions
}

// updateSim applies one partial update from the API or a browser.
//
// Everything that can be rejected is checked before anything is changed, so a
// 400 leaves the state exactly as it was. Within one request, reset and
// randomize run before explicit positions, so "randomize, but pin node-3
// here" does what it says.
func (h *hub) updateSim(m ClientMessage) (SimView, error) {
	if m.Threshold != nil && !(*m.Threshold > 0 && *m.Threshold <= 1) {
		return SimView{}, fmt.Errorf("%w: threshold %v must be in (0,1]", ErrBadRequest, *m.Threshold)
	}
	newIDs := 0
	for id := range m.Positions {
		switch {
		case id == "":
			return SimView{}, fmt.Errorf("%w: position for an empty node id", ErrBadRequest)
		case id == NodeID:
			return SimView{}, fmt.Errorf("%w: %q is not a drone", ErrBadRequest, NodeID)
		case len(id) > maxSimNodeIDLen:
			return SimView{}, fmt.Errorf("%w: node id longer than %d bytes", ErrBadRequest, maxSimNodeIDLen)
		}
		if _, ok := h.sim.positions[id]; !ok {
			newIDs++
		}
	}
	if newIDs > 0 && !h.makeRoom(newIDs) {
		return SimView{}, fmt.Errorf("%w: more than %d positions", ErrBadRequest, maxSimPositions)
	}

	var changes []string
	setF := func(name string, cur *float64, v float64) {
		if v != *cur {
			changes = append(changes, name+" "+fmtNum(*cur)+" -> "+fmtNum(v))
			*cur = v
		}
	}

	if m.Enabled != nil && *m.Enabled != h.sim.enabled {
		changes = append(changes, "enabled "+strconv.FormatBool(h.sim.enabled)+" -> "+strconv.FormatBool(*m.Enabled))
		h.sim.enabled = *m.Enabled
	}
	want := h.sim.params
	if m.BaseMS != nil {
		want.BaseMS = *m.BaseMS
	}
	if m.PerUnitMS != nil {
		want.PerUnitMS = *m.PerUnitMS
	}
	if m.JitterMS != nil {
		want.JitterMS = *m.JitterMS
	}
	want = want.Clamp()
	setF("base_ms", &h.sim.params.BaseMS, want.BaseMS)
	setF("per_unit_ms", &h.sim.params.PerUnitMS, want.PerUnitMS)
	setF("jitter_ms", &h.sim.params.JitterMS, want.JitterMS)
	if m.Threshold != nil {
		setF("threshold", &h.sim.threshold, *m.Threshold)
	}
	if m.Hysteresis != nil {
		// Negative is the wire's "no override", so a negative request
		// clears the override rather than being an error.
		v := min(*m.Hysteresis, maxHysteresis)
		if v < 0 {
			v = noHysteresis
		}
		setF("hysteresis", &h.sim.hysteresis, v)
	}

	ids := h.simIDs()
	if m.ResetPositions {
		moved := 0
		for _, id := range ids {
			if p := geo.DefaultPosition(id); p != h.sim.positions[id] {
				h.sim.positions[id] = p
				moved++
			}
		}
		if moved > 0 {
			changes = append(changes, "reset "+plural(moved, "position"))
		}
	}
	if m.Randomize && len(ids) > 0 {
		// Uniform in the cube. IDs are visited in sorted order so an
		// injected Rand gives the same layout every run.
		for _, id := range ids {
			h.sim.positions[id] = geo.ClampPosition(protocol.Position{
				X: h.s.cfg.Rand() * geo.Size,
				Y: h.s.cfg.Rand() * geo.Size,
				Z: h.s.cfg.Rand() * geo.Size,
			})
		}
		changes = append(changes, "randomized "+plural(len(ids), "position"))
	}
	var moved []protocol.NodeID
	for id, p := range m.Positions {
		p = geo.ClampPosition(p)
		if cur, ok := h.sim.positions[id]; ok && cur == p {
			continue
		}
		h.sim.positions[id] = p
		moved = append(moved, id)
	}
	var subject protocol.NodeID
	switch len(moved) {
	case 0:
	case 1:
		subject = moved[0]
		changes = append(changes, "moved "+string(moved[0]))
	default:
		changes = append(changes, "moved "+plural(len(moved), "drone"))
	}

	if len(changes) == 0 {
		return h.simView(), nil // nothing changed: no new version to push
	}
	h.bumpSim()
	h.broadcastSim()
	h.event(EventSim, subject, strings.Join(changes, ", "))
	return h.simView(), nil
}

// simIDs returns every positioned ID, sorted.
func (h *hub) simIDs() []protocol.NodeID {
	ids := make([]protocol.NodeID, 0, len(h.sim.positions))
	for id := range h.sim.positions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// fmtNum prints a setting the way a person typed it: 2, 0.5, not 2.000000.
func fmtNum(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
