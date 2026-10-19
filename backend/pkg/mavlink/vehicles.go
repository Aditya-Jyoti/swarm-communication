package mavlink

import (
	"errors"
	"fmt"
	"strconv"
	"sync"

	"swarm-net/pkg/protocol"
)

// System id bounds for assignable vehicles.
//
// 0 is the MAVLink broadcast address and 255 is conventionally the ground
// station's own id, so both are unusable for a vehicle even though the field is
// a full byte. That leaves 254 vehicles, which is 254 more than this swarm will
// ever run.
const (
	MinSysID byte = 1
	MaxSysID byte = 254
)

// ErrNoSysID means all 254 assignable system ids are held. The caller cannot
// recover by retrying; the swarm is simply larger than MAVLink addressing.
var ErrNoSysID = errors.New("mavlink: no free system id")

// Vehicles maps swarm node ids to MAVLink system ids.
//
// Pure bookkeeping: no I/O, no clock, no goroutine. The bridge asks it for a
// sysid when it first sees a node and never tells it anything else.
//
// # Why the mapping must be stable
//
// A ground station identifies a vehicle by sysid alone. Every piece of state it
// holds -- the vehicle's position history, its mission, which window it is in --
// is keyed on that number. Hand the same number to a different node and the
// ground station does not notice a substitution; it shows one vehicle that
// teleported. So an assignment is permanent for the process's lifetime:
//
//  1. If the node id ends in digits forming a value in [MinSysID, MaxSysID] and
//     that id is free, use it, so swarm-net-node-3 is vehicle 3 and a packet
//     capture can be read without a lookup table.
//  2. Otherwise take the lowest free id. "seed" gets one this way.
//  3. An id is NEVER released on failure. A killed node keeps its vehicle, so
//     when it restarts the ground station sees the link come back rather than a
//     new aircraft appearing next to the dead one -- which is exactly what a
//     failover demo needs to look like.
//
// Release exists for the case the swarm is genuinely reconfigured, and is not
// called on a node going down.
//
// # Concurrency
//
// Safe for concurrent use; all three maps are guarded by mu. The bridge's
// telemetry tick and its inbound-packet handler both reach for a sysid, from
// different goroutines, and the invariant that matters -- no two nodes share an
// id -- cannot be upheld by anything weaker than holding the lock across the
// read-decide-write sequence in SysID.
type Vehicles struct {
	mu     sync.Mutex
	byNode map[protocol.NodeID]byte // guarded by mu
	bySys  map[byte]protocol.NodeID // guarded by mu; inverse of byNode
}

// NewVehicles returns an empty assignment table.
func NewVehicles() *Vehicles {
	return &Vehicles{
		byNode: make(map[protocol.NodeID]byte),
		bySys:  make(map[byte]protocol.NodeID),
	}
}

// SysID returns the system id for node, assigning one on first sight.
//
// It is idempotent: the same node always gets the same id back.
func (v *Vehicles) SysID(node protocol.NodeID) (byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if id, ok := v.byNode[node]; ok {
		return id, nil
	}
	if v.byNode == nil {
		v.byNode = make(map[protocol.NodeID]byte)
		v.bySys = make(map[byte]protocol.NodeID)
	}

	if id, ok := trailingSysID(string(node)); ok {
		if _, taken := v.bySys[id]; !taken {
			v.assignLocked(node, id)
			return id, nil
		}
		// Fall through. Two nodes whose ids end in the same number is a naming
		// collision in the compose file, not an error here: the second one gets
		// an arbitrary id and still works, just with a less readable capture.
	}

	// Allocate DOWNWARD from MaxSysID, not upward from MinSysID.
	//
	// Upward allocation makes every number depend on arrival order. "seed"
	// carries no digits and sorts before "swarm-net-node-1", so it took id 1,
	// and then every numbered node found its own number occupied and shifted:
	// node-3 became vehicle 4 and node-4 became vehicle 9. A ground station
	// then reports the wrong aircraft dying, which is worse than an ugly id.
	//
	// Going downward keeps the low ids clear for the nodes that have a claim on
	// them, so a numbered node gets its number no matter when it connects, and
	// the odd one out gets 254.
	for id := int(MaxSysID); id >= int(MinSysID); id-- {
		if _, taken := v.bySys[byte(id)]; !taken {
			v.assignLocked(node, byte(id))
			return byte(id), nil
		}
	}
	return 0, fmt.Errorf("mavlink: %d vehicles already assigned, cannot place %q: %w",
		len(v.bySys), node, ErrNoSysID)
}

// assignLocked records a mapping. Caller must hold v.mu.
func (v *Vehicles) assignLocked(node protocol.NodeID, id byte) {
	v.byNode[node] = id
	v.bySys[id] = node
}

// Lookup returns a node's system id without assigning one.
func (v *Vehicles) Lookup(node protocol.NodeID) (byte, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	id, ok := v.byNode[node]
	return id, ok
}

// Node returns the node a system id belongs to. This is the direction inbound
// traffic needs: a mission arrives addressed to sysid 9 and the bridge has to
// decide whose cluster flies it.
func (v *Vehicles) Node(sysID byte) (protocol.NodeID, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	node, ok := v.bySys[sysID]
	return node, ok
}

// Release forgets a node, freeing its system id for reuse.
//
// Deliberately NOT called when a node dies; see the type comment. It is here for
// a genuine reconfiguration, and for tests.
func (v *Vehicles) Release(node protocol.NodeID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if id, ok := v.byNode[node]; ok {
		delete(v.byNode, node)
		delete(v.bySys, id)
	}
}

// Len returns the number of assigned vehicles.
func (v *Vehicles) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.byNode)
}

// Snapshot copies the whole mapping. The copy is what makes it safe to hand to
// a telemetry encoder or a template while other goroutines keep assigning.
func (v *Vehicles) Snapshot() map[protocol.NodeID]byte {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[protocol.NodeID]byte, len(v.byNode))
	for node, id := range v.byNode {
		out[node] = id
	}
	return out
}

// trailingSysID extracts a usable system id from the digits at the end of a node
// id: "swarm-net-node-12" -> 12.
//
// It reports false rather than clamping when the number is out of range, so
// "node-300" falls through to the lowest-free path instead of silently becoming
// vehicle 44 or 255. ParseUint also reports false on a digit run too long for a
// uint64, which is the one input that could otherwise overflow.
func trailingSysID(node string) (byte, bool) {
	i := len(node)
	for i > 0 && node[i-1] >= '0' && node[i-1] <= '9' {
		i--
	}
	if i == len(node) {
		return 0, false
	}
	n, err := strconv.ParseUint(node[i:], 10, 64)
	if err != nil || n < uint64(MinSysID) || n > uint64(MaxSysID) {
		return 0, false
	}
	return byte(n), true
}
