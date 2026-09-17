package cluster

import (
	"cmp"
	"slices"
	"sync"

	"swarm-net/pkg/health"
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
	// Score is the member's SELF-REPORTED health cost, learned from gossip, and
	// the input election ranks on. NaN means no report yet. It is not this node's
	// measurement of the member: a measured round trip is observer-relative and
	// feeding it to Elect gives every node a different answer. See node.go.
	Score float64
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

// Scores returns each member's self-reported score keyed by ID, which is the map
// Elect consumes. Members with no report are present as NaN so that "unreported"
// and "absent" are the same thing to the ranking: ineligible.
func (v View) Scores() map[protocol.NodeID]float64 {
	out := make(map[protocol.NodeID]float64, len(v.Members))
	for _, m := range v.Members {
		out[m.ID] = m.Score
	}
	return out
}

// Leaders returns the IDs currently marked as leaders, in sorted order.
//
// Counted first, then filled. Leaders are a small fraction of the swarm, so
// sizing the slice to len(Members) would over-allocate badly, while growing it
// by append costs a fresh allocation and copy at every doubling. One extra
// no-allocation scan buys exactly one right-sized allocation. The nil return
// for "no leaders" is preserved deliberately: callers compare the result
// against nil.
func (v View) Leaders() []protocol.NodeID {
	n := 0
	for _, m := range v.Members {
		if m.Role == RoleLeader && m.State == StateAlive {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	out := make([]protocol.NodeID, 0, n)
	for _, m := range v.Members {
		if m.Role == RoleLeader && m.State == StateAlive {
			out = append(out, m.ID)
		}
	}
	return out
}

// Size is the number of members believed alive -- the N in the leader-count formula.
//
// Counted in place rather than as len(v.Alive()): the count is read on every
// election round and on every status snapshot, and materialising the whole
// alive slice just to measure it allocates the entire membership for a number.
func (v View) Size() int {
	n := 0
	for _, m := range v.Members {
		if m.State == StateAlive {
			n++
		}
	}
	return n
}

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
		// A record with no score is silent about the score, not a claim that
		// it is unknown: a relayed rumour from a peer that has not heard the
		// member's report must not erase the report we already hold.
		if !health.IsValidScore(m.Score) {
			m.Score = existing.Score
		}
		if m.State == existing.State && m.Role == existing.Role && m.Addr == existing.Addr && sameScore(m.Score, existing.Score) {
			return false
		}
	}

	t.members[m.ID] = m
	t.version++
	return true
}

// SetScore records a member's self-reported score without touching liveness,
// role or incarnation. Used for this node's own record after each probe round.
func (t *Table) SetScore(id protocol.NodeID, score float64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	m, ok := t.members[id]
	if !ok || sameScore(m.Score, score) {
		return false
	}
	m.Score = score
	t.members[id] = m
	t.version++
	return true
}

// sameScore is equality that treats two invalid scores as equal, so a NaN
// re-applied over a NaN is a no-op rather than a version bump every round.
func sameScore(a, b float64) bool {
	if !health.IsValidScore(a) && !health.IsValidScore(b) {
		return true
	}
	return a == b
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

// Revive records first-hand evidence that a member is alive: a completed
// handshake with it, at the given incarnation. It reports whether anything
// changed.
//
// Upsert alone cannot do this. Its equal-incarnation rule says liveness only
// worsens, which is right for rumours -- a peer that has not heard the news must
// not talk a dead node back to life -- but a handshake is not a rumour. A node
// that dropped (SIGKILL, partition) and came back at the same incarnation would
// otherwise stay dead in every table forever, and self-healing would be a
// one-way door. The guard is that the evidence must be at least as new as what
// the table holds: a stale PeerUp (an old socket registering late) cannot
// resurrect a node that has since died at a higher incarnation.
func (t *Table) Revive(id protocol.NodeID, incarnation int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	m, ok := t.members[id]
	if !ok || incarnation < m.Incarnation {
		return false
	}
	if m.State == StateAlive && m.Incarnation == incarnation {
		return false
	}
	m.State = StateAlive
	m.Incarnation = incarnation
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
	// slices.SortFunc, not sort.Slice. sort.Slice swaps through reflect.Swapper,
	// so every swap of a Member -- a struct wider than 56 bytes carrying two
	// string headers -- is a reflective memmove, and the less function is an
	// indirect closure call the compiler cannot inline. The generic version
	// swaps and compares directly. Same order, same allocations, far cheaper per
	// element, which matters because Elect takes a fresh Snapshot every round.
	slices.SortFunc(out, func(a, b Member) int { return cmp.Compare(a.ID, b.ID) })

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
