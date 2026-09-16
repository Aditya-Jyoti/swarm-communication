package cluster

import (
	"reflect"
	"sync"
	"testing"

	"swarm-net/pkg/protocol"
)

func TestRoleAndStateWireRoundTrip(t *testing.T) {
	for _, r := range []Role{RoleWorker, RoleLeader} {
		if got := ParseRole(r.String()); got != r {
			t.Errorf("ParseRole(%q) = %v, want %v", r.String(), got, r)
		}
	}
	for _, s := range []State{StateAlive, StateSuspect, StateDead} {
		if got := ParseState(s.String()); got != s {
			t.Errorf("ParseState(%q) = %v, want %v", s.String(), got, s)
		}
	}
}

// An unrecognised role must not be read as leadership. A future node inventing a
// role we have never heard of should not be able to seize the role by naming it.
func TestUnknownRoleIsNotLeader(t *testing.T) {
	if got := ParseRole("supreme-coordinator"); got != RoleWorker {
		t.Errorf("ParseRole(unknown) = %v, want worker", got)
	}
	if got := ParseState("vibing"); got != StateAlive {
		t.Errorf("ParseState(unknown) = %v, want alive", got)
	}
}

func TestUpsertAddsNewMember(t *testing.T) {
	tbl := NewTable()
	if !tbl.Upsert(Member{ID: "node-1", Addr: "node-1:7946", Incarnation: 1}) {
		t.Fatal("first Upsert reported no change")
	}
	if got := tbl.Snapshot().Size(); got != 1 {
		t.Errorf("Size = %d, want 1", got)
	}
}

// A higher incarnation is the node speaking about itself after a restart. It must
// be able to overrule any rumour, including a death rumour -- that is the only way
// a node that was wrongly declared dead ever rejoins.
func TestHigherIncarnationRefutesADeathRumour(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 1, State: StateDead})

	if !tbl.Upsert(Member{ID: "node-1", Incarnation: 2, State: StateAlive}) {
		t.Fatal("a higher incarnation was rejected")
	}

	m, _ := tbl.Snapshot().Get("node-1")
	if m.State != StateAlive {
		t.Errorf("State = %v, want alive after a restart", m.State)
	}
	if m.Incarnation != 2 {
		t.Errorf("Incarnation = %d, want 2", m.Incarnation)
	}
}

// A lower incarnation is a stale rumour arriving late. Applying it would resurrect
// information the swarm has already moved past.
func TestLowerIncarnationIsIgnored(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 5, State: StateAlive})

	if tbl.Upsert(Member{ID: "node-1", Incarnation: 2, State: StateDead}) {
		t.Fatal("a stale rumour was applied")
	}

	m, _ := tbl.Snapshot().Get("node-1")
	if m.State != StateAlive || m.Incarnation != 5 {
		t.Errorf("stale rumour changed the record: %+v", m)
	}
}

// At equal incarnation failure information propagates, but recovery information does
// not. Otherwise two nodes with different beliefs flip each other back and forth
// forever -- the classic membership oscillation.
func TestAtEqualIncarnationStateOnlyWorsens(t *testing.T) {
	t.Run("alive to suspect is accepted", func(t *testing.T) {
		tbl := NewTable()
		tbl.Upsert(Member{ID: "node-1", Incarnation: 3, State: StateAlive})
		if !tbl.Upsert(Member{ID: "node-1", Incarnation: 3, State: StateSuspect}) {
			t.Fatal("a worsening state was rejected")
		}
		m, _ := tbl.Snapshot().Get("node-1")
		if m.State != StateSuspect {
			t.Errorf("State = %v, want suspect", m.State)
		}
	})

	t.Run("dead to alive is refused", func(t *testing.T) {
		tbl := NewTable()
		tbl.Upsert(Member{ID: "node-1", Incarnation: 3, State: StateDead})
		if tbl.Upsert(Member{ID: "node-1", Incarnation: 3, State: StateAlive}) {
			t.Fatal("a node was resurrected without a new incarnation")
		}
		m, _ := tbl.Snapshot().Get("node-1")
		if m.State != StateDead {
			t.Errorf("State = %v, want dead", m.State)
		}
	})
}

// A record carrying a role change must not smuggle a stale "alive" in with it. This
// is the case that slipped through an earlier version of the merge: the role
// differed, so the record was applied wholesale, resurrecting a buried node.
func TestRoleChangeCannotResurrectADeadNode(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 4, State: StateDead, Role: RoleWorker})

	tbl.Upsert(Member{ID: "node-1", Incarnation: 4, State: StateAlive, Role: RoleLeader})

	m, _ := tbl.Snapshot().Get("node-1")
	if m.State != StateDead {
		t.Errorf("State = %v, want dead -- a role change is not a liveness event", m.State)
	}
}

// A promotion at the same incarnation is legitimate and must be recorded: leadership
// changes far more often than liveness does.
func TestPromotionAtEqualIncarnationIsRecorded(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 2, State: StateAlive, Role: RoleWorker})

	if !tbl.Upsert(Member{ID: "node-1", Incarnation: 2, State: StateAlive, Role: RoleLeader}) {
		t.Fatal("a promotion was rejected")
	}
	m, _ := tbl.Snapshot().Get("node-1")
	if m.Role != RoleLeader {
		t.Errorf("Role = %v, want leader", m.Role)
	}
}

func TestIdenticalUpsertReportsNoChange(t *testing.T) {
	tbl := NewTable()
	m := Member{ID: "node-1", Addr: "node-1:7946", Incarnation: 1, State: StateAlive, Role: RoleWorker}
	tbl.Upsert(m)

	if tbl.Upsert(m) {
		t.Error("an identical record reported a change, which would trigger a needless election")
	}
}

func TestSetRoleAndSetState(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 1})

	if !tbl.SetRole("node-1", RoleLeader) {
		t.Error("SetRole reported no change")
	}
	if tbl.SetRole("node-1", RoleLeader) {
		t.Error("a redundant SetRole reported a change")
	}
	if tbl.SetRole("missing", RoleLeader) {
		t.Error("SetRole on an unknown member reported a change")
	}

	if !tbl.SetState("node-1", StateSuspect) {
		t.Error("SetState reported no change")
	}
	if tbl.SetState("missing", StateDead) {
		t.Error("SetState on an unknown member reported a change")
	}

	m, _ := tbl.Snapshot().Get("node-1")
	if m.Role != RoleLeader || m.State != StateSuspect {
		t.Errorf("member = %+v", m)
	}
}

func TestRemove(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 1})

	if !tbl.Remove("node-1") {
		t.Error("Remove reported no change")
	}
	if tbl.Remove("node-1") {
		t.Error("removing twice reported a change")
	}
	if _, ok := tbl.Snapshot().Get("node-1"); ok {
		t.Error("member survived Remove")
	}
}

// Snapshot order must be deterministic. Ranging a map would make election results
// depend on Go's randomised iteration order.
func TestSnapshotIsSortedAndStable(t *testing.T) {
	tbl := NewTable()
	for _, id := range []protocol.NodeID{"node-9", "node-2", "node-5", "node-1"} {
		tbl.Upsert(Member{ID: id, Incarnation: 1})
	}

	want := []protocol.NodeID{"node-1", "node-2", "node-5", "node-9"}
	for i := 0; i < 20; i++ {
		var got []protocol.NodeID
		for _, m := range tbl.Snapshot().Members {
			got = append(got, m.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("snapshot %d = %v, want %v", i, got, want)
		}
	}
}

// A snapshot is a copy. A caller mutating what it got back must not corrupt the
// table that the election will read next.
func TestSnapshotIsIsolatedFromTheTable(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 1, Role: RoleWorker})

	snap := tbl.Snapshot()
	snap.Members[0].Role = RoleLeader

	m, _ := tbl.Snapshot().Get("node-1")
	if m.Role != RoleWorker {
		t.Error("mutating a snapshot changed the table")
	}
}

func TestVersionAdvancesOnlyOnRealChange(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 1})
	v1 := tbl.Snapshot().Version

	tbl.Upsert(Member{ID: "node-1", Incarnation: 1}) // no-op
	if got := tbl.Snapshot().Version; got != v1 {
		t.Errorf("version advanced on a no-op: %d -> %d", v1, got)
	}

	tbl.SetState("node-1", StateSuspect)
	if got := tbl.Snapshot().Version; got == v1 {
		t.Error("version did not advance on a real change")
	}
}

// Addresses feeds health.Retain, which bounds the EWMA map to live membership. A
// dead member must be excluded or its stale score survives -- and after Docker
// recycles its IP, that score belongs to a different container entirely.
func TestAddressesExcludesDeadMembers(t *testing.T) {
	v := viewOf(
		Member{ID: "node-1", Addr: "node-1:7946", State: StateAlive},
		Member{ID: "node-2", Addr: "node-2:7946", State: StateSuspect},
		Member{ID: "node-3", Addr: "node-3:7946", State: StateDead},
		Member{ID: "node-4", Addr: "", State: StateAlive},
	)

	got := v.Addresses()

	if _, ok := got["node-3:7946"]; ok {
		t.Error("a dead member's address was retained")
	}
	if _, ok := got["node-2:7946"]; !ok {
		t.Error("a suspect member's address was dropped, losing its history mid-diagnosis")
	}
	if _, ok := got[""]; ok {
		t.Error("an empty address was included")
	}
	if len(got) != 2 {
		t.Errorf("Addresses = %v, want 2 entries", got)
	}
}

func TestViewLeadersOnlyCountsAliveLeaders(t *testing.T) {
	v := viewOf(
		Member{ID: "node-1", Role: RoleLeader, State: StateAlive},
		Member{ID: "node-2", Role: RoleLeader, State: StateDead},
		Member{ID: "node-3", Role: RoleWorker, State: StateAlive},
	)

	got := v.Leaders()
	want := []protocol.NodeID{"node-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Leaders = %v, want %v", got, want)
	}
}

// The table is read by telemetry and the election while heartbeat handlers write it.
func TestConcurrentTableAccess(t *testing.T) {
	tbl := NewTable()
	var wg sync.WaitGroup

	// Owner: this test. Every goroutine runs a bounded loop; wg.Wait joins them all.
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer wg.Done()
			id := protocol.NodeID(string(rune('a' + i)))
			for j := 0; j < 100; j++ {
				tbl.Upsert(Member{ID: id, Incarnation: int64(j), State: StateAlive})
			}
		}(i)
	}

	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				snap := tbl.Snapshot()
				_ = snap.Alive()
				_ = snap.Addresses()
				_ = Elect(snap, nil, Config{})
			}
		}()
	}

	wg.Wait()

	if got := tbl.Snapshot().Size(); got != 8 {
		t.Errorf("Size = %d, want 8", got)
	}
}
