package cluster

import (
	"sort"
	"sync"

	"swarm-net/pkg/protocol"
)

// Role is what a node is currently doing. It is a state, not a start-up flag: every
// node runs the same binary and may be promoted or demoted at any time.
type Role uint8

const (
	RoleWorker Role = iota
	RoleLeader
)

func (r Role) String() string {
	if r == RoleLeader {
		return "leader"
	}
	return "worker"
}

// ParseRole maps the wire representation back to a Role.
//
// An unrecognised role decodes to RoleWorker rather than failing. A node inventing a
// role we have never heard of is a forward-compatibility event, and the safe reading
// of "something I do not understand" is "not a leader" -- treating it as one would
// let an unknown string seize leadership.
func ParseRole(s string) Role {
	if s == "leader" {
		return RoleLeader
	}
	return RoleWorker
}

// State is what we believe about a peer's liveness.
type State uint8

const (
	StateAlive State = iota
	StateSuspect
	StateDead
)

func (s State) String() string {
	switch s {
	case StateSuspect:
		return "suspect"
	case StateDead:
		return "dead"
	default:
		return "alive"
	}
}

// ParseState maps the wire representation back to a State. An unrecognised state is
// read as StateAlive, matching the wire default for a node that simply omitted it.
func ParseState(s string) State {
	switch s {
	case "suspect":
		return StateSuspect
	case "dead":
		return StateDead
	default:
		return StateAlive
	}
}

// Member is this node's belief about one peer.
type Member struct {
	ID    protocol.NodeID
	Addr  protocol.NodeAddress
	Role  Role
	State State
	// Incarnation increases when a node restarts. It is the tie-breaker that lets a
	// node's own claim about itself defeat a stale rumour that it is dead, which is
	// what stops membership oscillating between "alive" and "dead" forever.
	Incarnation int64
}

// View is an immutable snapshot of membership.
//
// Election reasoning runs against a View, never against the live table. That is what
// makes the election a pure function of its inputs, and therefore idempotent and
// testable without a socket.
type View struct {
	// Members is sorted by ID so that iteration order is deterministic. Ranging over
	// a map would make election results depend on Go's randomised map order, which
	// would break the idempotence property outright.
	Members []Member
	Version uint64
}

// Alive returns the members believed to be alive, preserving sort order.
func (v View) Alive() []Member {
	out := make([]Member, 0, len(v.Members))
	for _, m := range v.Members {
		if m.State == StateAlive {
			out = append(out, m)
		}
	}
	return out
}

// Get returns the member with the given ID.
func (v View) Get(id protocol.NodeID) (Member, bool) {
	for _, m := range v.Members {
		if m.ID == id {
			return m, true
		}
	}
	return Member{}, false
}

// Leaders returns the IDs currently marked as leaders, in sorted order.
func (v View) Leaders() []protocol.NodeID {
	var out []protocol.NodeID
	for _, m := range v.Members {
		if m.Role == RoleLeader && m.State == StateAlive {
			out = append(out, m.ID)
		}
	}
	return out
}

// Size is the number of members believed alive -- the N in the leader-count formula.
func (v View) Size() int { return len(v.Alive()) }

// Table is the mutable membership store.
//
// It is the only mutable state in this package; everything downstream consumes
// Snapshot. Callers must never hold a reference into the table itself.
type Table struct {
	mu sync.RWMutex
	// members is guarded by mu.
	members map[protocol.NodeID]Member
	// version is guarded by mu and increments on every accepted mutation, so a
	// snapshot can be compared for staleness without deep equality.
	version uint64
}

func NewTable() *Table {
	return &Table{members: make(map[protocol.NodeID]Member)}
}

// Upsert merges one member record into the table and reports whether anything
// changed.
//
// The merge rule is incarnation-based, and the ordering of these cases is the whole
// algorithm:
//
//   - A higher incarnation always wins. It is the node speaking about itself after a
//     restart, and it must be able to overrule any rumour.
//   - At equal incarnation, a worse state wins (alive < suspect < dead). Failure
//     information propagates; a node cannot be talked back into being alive by a
//     peer that simply has not heard yet.
//   - A lower incarnation is ignored entirely. That is a stale rumour arriving late,
//     and applying it would resurrect information the swarm has already moved past.
//
// Without the second rule, two nodes with different beliefs would flip each other
// back and forth forever, which is the classic membership oscillation.
func (t *Table) Upsert(m Member) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	existing, ok := t.members[m.ID]
	if !ok {
		t.members[m.ID] = m
		t.version++
		return true
	}

	switch {
	case m.Incarnation > existing.Incarnation:
		// The node speaking about itself after a restart. Full overwrite, including
		// a return to alive: this is the only way a node refutes a death rumour.
	case m.Incarnation < existing.Incarnation:
		return false
	default:
		// Equal incarnation. Liveness may get worse but never better, and the clamp
		// is applied before anything else is compared. Without it, a record that
		// merely reports a role change would carry its stale "alive" along with it
		// and resurrect a node the swarm has already buried.
		if m.State < existing.State {
			m.State = existing.State
		}
		if m.State == existing.State && m.Role == existing.Role && m.Addr == existing.Addr {
			return false
		}
	}

	t.members[m.ID] = m
	t.version++
	return true
}

// SetRole records a role change without touching liveness or incarnation.
func (t *Table) SetRole(id protocol.NodeID, r Role) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	m, ok := t.members[id]
	if !ok || m.Role == r {
		return false
	}
	m.Role = r
	t.members[id] = m
	t.version++
	return true
}

// SetState records a liveness change at the member's current incarnation.
func (t *Table) SetState(id protocol.NodeID, s State) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	m, ok := t.members[id]
	if !ok || m.State == s {
		return false
	}
	m.State = s
	t.members[id] = m
	t.version++
	return true
}

// Remove drops a member outright. Used for a voluntary LEAVE, where there is no
// rumour to outlive.
func (t *Table) Remove(id protocol.NodeID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.members[id]; !ok {
		return false
	}
	delete(t.members, id)
	t.version++
	return true
}

// Snapshot returns an immutable, deterministically ordered copy.
func (t *Table) Snapshot() View {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]Member, 0, len(t.members))
	for _, m := range t.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return View{Members: out, Version: t.version}
}

// Addresses returns the live peer set in the form health.Retain expects, so the
// caller can bound the health history to current membership in one call.
func (v View) Addresses() map[protocol.NodeAddress]struct{} {
	out := make(map[protocol.NodeAddress]struct{}, len(v.Members))
	for _, m := range v.Members {
		if m.State != StateDead && m.Addr != "" {
			out[m.Addr] = struct{}{}
		}
	}
	return out
}
