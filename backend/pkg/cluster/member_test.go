package cluster

import (
	"math"
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

// HIGH-2: a death is recorded one incarnation past what the member last said
// about itself, so it outranks a refutation that was already in flight.
func TestSetStateDeadBumpsIncarnation(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 6, State: StateAlive})

	if !tbl.SetState("node-1", StateDead) {
		t.Fatal("SetState(dead) reported no change")
	}
	m, _ := tbl.Snapshot().Get("node-1")
	if m.State != StateDead || m.Incarnation != 7 {
		t.Fatalf("member = %+v, want dead at 7", m)
	}
	// A second death is a no-op: the incarnation must not creep on repeats, or
	// two observers of one crash would disagree about its number.
	if tbl.SetState("node-1", StateDead) {
		t.Fatal("a repeated death reported a change")
	}
	if m, _ := tbl.Snapshot().Get("node-1"); m.Incarnation != 7 {
		t.Fatalf("Incarnation = %d after a repeated death, want 7", m.Incarnation)
	}
}

func TestSetStateSuspectKeepsIncarnation(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: 6, State: StateAlive})
	tbl.SetState("node-1", StateSuspect)
	if m, _ := tbl.Snapshot().Get("node-1"); m.Incarnation != 6 {
		t.Fatalf("Incarnation = %d after suspect, want 6", m.Incarnation)
	}
}

// The STATE.md interleaving at table level: B refuted a rumour with "alive at 7",
// then died; the death is recorded first, the queued refutation arrives second.
func TestRefutationAtOldIncarnationLosesToDeath(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-b", Incarnation: 6, State: StateAlive})
	tbl.SetState("node-b", StateDead) // dead at 7

	if tbl.Upsert(Member{ID: "node-b", Incarnation: 7, State: StateAlive}) {
		t.Fatal("a refutation at the death's incarnation was applied")
	}
	if m, _ := tbl.Snapshot().Get("node-b"); m.State != StateDead || m.Incarnation != 7 {
		t.Fatalf("member = %+v, want dead at 7", m)
	}
}

func TestRefutationAtHigherIncarnationBeatsDeath(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-b", Incarnation: 6, State: StateAlive})
	tbl.SetState("node-b", StateDead) // dead at 7

	if !tbl.Upsert(Member{ID: "node-b", Incarnation: 8, State: StateAlive}) {
		t.Fatal("a refutation past the death was rejected")
	}
	if m, _ := tbl.Snapshot().Get("node-b"); m.State != StateAlive || m.Incarnation != 8 {
		t.Fatalf("member = %+v, want alive at 8", m)
	}
}

func TestSetStateDeadSaturatesIncarnation(t *testing.T) {
	tbl := NewTable()
	tbl.Upsert(Member{ID: "node-1", Incarnation: math.MaxInt64, State: StateAlive})
	tbl.SetState("node-1", StateDead)
	m, _ := tbl.Snapshot().Get("node-1")
	if m.State != StateDead || m.Incarnation != math.MaxInt64 {
		t.Fatalf("member = %+v, want dead at MaxInt64 (clamped, not wrapped)", m)
	}
}

func TestNextIncarnation(t *testing.T) {
	for _, tt := range []struct{ in, want int64 }{
		{0, 1},
		{41, 42},
		{math.MaxInt64 - 1, math.MaxInt64},
		{math.MaxInt64, math.MaxInt64},
	} {
		if got := NextIncarnation(tt.in); got != tt.want {
			t.Errorf("NextIncarnation(%d) = %d, want %d", tt.in, got, tt.want)
		}
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

func TestRevive(t *testing.T) {
	tests := []struct {
		name        string
		seed        *Member
		incarnation int64
		want        bool
		wantState   State
		wantInc     int64
	}{
		{"unknown member", nil, 1, false, StateAlive, 0},
		{"dead at equal incarnation revives", &Member{ID: "x", State: StateDead, Incarnation: 3}, 3, true, StateAlive, 3},
		{"suspect at equal incarnation revives", &Member{ID: "x", State: StateSuspect, Incarnation: 3}, 3, true, StateAlive, 3},
		{"newer incarnation revives and updates", &Member{ID: "x", State: StateDead, Incarnation: 3}, 5, true, StateAlive, 5},
		{"stale evidence cannot resurrect", &Member{ID: "x", State: StateDead, Incarnation: 3}, 2, false, StateDead, 3},
		{"already alive is a no-op", &Member{ID: "x", State: StateAlive, Incarnation: 3}, 3, false, StateAlive, 3},
		{"alive at newer incarnation is a change", &Member{ID: "x", State: StateAlive, Incarnation: 3}, 4, true, StateAlive, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := NewTable()
			if tt.seed != nil {
				tb.Upsert(*tt.seed)
			}
			before := tb.Snapshot().Version
			if got := tb.Revive("x", tt.incarnation); got != tt.want {
				t.Fatalf("Revive = %v, want %v", got, tt.want)
			}
			if tt.seed == nil {
				return
			}
			m, _ := tb.Snapshot().Get("x")
			if m.State != tt.wantState || m.Incarnation != tt.wantInc {
				t.Fatalf("member = %+v, want state %s incarnation %d", m, tt.wantState, tt.wantInc)
			}
			if changed := tb.Snapshot().Version != before; changed != tt.want {
				t.Fatalf("version changed = %v, want %v", changed, tt.want)
			}
		})
	}
}

func TestUpsertScoreOnlyChangeBumpsVersionNotLiveness(t *testing.T) {
	tb := NewTable()
	tb.Upsert(Member{ID: "x", Incarnation: 2, State: StateDead, Role: RoleLeader, Score: math.NaN()})
	v0 := tb.Snapshot().Version

	// A score at equal incarnation is applied, the version moves, and the
	// member is not resurrected by the record's "alive".
	if !tb.Upsert(Member{ID: "x", Incarnation: 2, State: StateAlive, Role: RoleLeader, Score: 1.5}) {
		t.Fatal("score-only change reported as no change")
	}
	m, _ := tb.Snapshot().Get("x")
	if m.Score != 1.5 || m.State != StateDead || m.Role != RoleLeader {
		t.Fatalf("member = %+v, want score 1.5, still dead, still leader", m)
	}
	if tb.Snapshot().Version == v0 {
		t.Fatal("version did not move on a score change")
	}

	// Same score again: no change.
	if tb.Upsert(Member{ID: "x", Incarnation: 2, State: StateDead, Role: RoleLeader, Score: 1.5}) {
		t.Fatal("identical record reported as a change")
	}
	// A record with no score preserves the one we hold.
	if tb.Upsert(Member{ID: "x", Incarnation: 2, State: StateDead, Role: RoleLeader, Score: math.NaN()}) {
		t.Fatal("scoreless record reported as a change")
	}
	if m, _ := tb.Snapshot().Get("x"); m.Score != 1.5 {
		t.Fatalf("scoreless record erased the score: %v", m.Score)
	}
	// A newer incarnation overwrites everything, including a missing score.
	tb.Upsert(Member{ID: "x", Incarnation: 3, State: StateAlive, Score: math.NaN()})
	if m, _ := tb.Snapshot().Get("x"); !math.IsNaN(m.Score) || m.State != StateAlive {
		t.Fatalf("restart did not reset the record: %+v", m)
	}
}

// Claims (Role, Score) are ordered by Seq within an incarnation, whoever
// relays them. This is the convergence fix: arrival order no longer decides.
func TestUpsertOrdersClaimsBySeq(t *testing.T) {
	tb := NewTable()
	tb.Upsert(Member{ID: "x", Incarnation: 1, Role: RoleLeader, Score: 1, Seq: 5})

	// Older and equal Seq: the claims held stand, and nothing changes.
	for _, seq := range []uint64{0, 4, 5} {
		if tb.Upsert(Member{ID: "x", Incarnation: 1, Role: RoleWorker, Score: 9, Seq: seq}) {
			t.Fatalf("Seq %d over 5 reported a change", seq)
		}
		if m, _ := tb.Snapshot().Get("x"); m.Role != RoleLeader || m.Score != 1 || m.Seq != 5 {
			t.Fatalf("Seq %d over 5 changed the claims: %+v", seq, m)
		}
	}
	// ...but state still worsens through an old claim: liveness has its own order.
	if !tb.Upsert(Member{ID: "x", Incarnation: 1, State: StateSuspect, Role: RoleWorker, Score: 9, Seq: 4}) {
		t.Fatal("a suspicion carried by an old claim was dropped")
	}
	if m, _ := tb.Snapshot().Get("x"); m.State != StateSuspect || m.Role != RoleLeader || m.Score != 1 {
		t.Fatalf("member = %+v, want suspect with claims unchanged", m)
	}

	// Newer Seq: taken whole, an unmeasured score included (a member that went
	// blind must become ineligible everywhere).
	if !tb.Upsert(Member{ID: "x", Incarnation: 1, Role: RoleWorker, Score: math.NaN(), Seq: 6}) {
		t.Fatal("newer claim reported as no change")
	}
	if m, _ := tb.Snapshot().Get("x"); m.Role != RoleWorker || !math.IsNaN(m.Score) || m.Seq != 6 {
		t.Fatalf("member = %+v, want worker, NaN, Seq 6", m)
	}

	// A newer incarnation overwrites regardless of Seq: a restart counts afresh.
	if !tb.Upsert(Member{ID: "x", Incarnation: 2, Role: RoleLeader, Score: 3, Seq: 1}) {
		t.Fatal("restart ignored")
	}
	if m, _ := tb.Snapshot().Get("x"); m.Seq != 1 || m.Score != 3 || m.State != StateAlive {
		t.Fatalf("member = %+v after restart", m)
	}
}

// The setters are the author's writes, and each bumps Seq exactly once per
// real change.
func TestSetScoreAndSetRoleBumpSeq(t *testing.T) {
	tb := NewTable()
	tb.Upsert(Member{ID: "self", Seq: 1, Score: math.NaN()})
	tb.SetScore("self", 1)
	tb.SetScore("self", 1) // no change
	tb.SetRole("self", RoleLeader)
	tb.SetRole("self", RoleLeader) // no change
	if m, _ := tb.Snapshot().Get("self"); m.Seq != 3 {
		t.Fatalf("Seq = %d, want 3", m.Seq)
	}
	tb.SetState("self", StateSuspect)
	if m, _ := tb.Snapshot().Get("self"); m.Seq != 3 {
		t.Fatalf("SetState moved Seq to %d", m.Seq)
	}
}

func TestSetScoreAndViewScores(t *testing.T) {
	tb := NewTable()
	if tb.SetScore("nobody", 1) {
		t.Fatal("SetScore on an unknown member reported a change")
	}
	tb.Upsert(Member{ID: "a", Score: math.NaN()})
	tb.Upsert(Member{ID: "b", Score: 2})
	if tb.SetScore("a", math.NaN()) {
		t.Fatal("NaN over NaN reported as a change")
	}
	if !tb.SetScore("a", 0.5) || tb.SetScore("a", 0.5) {
		t.Fatal("SetScore change detection is wrong")
	}
	m, _ := tb.Snapshot().Get("a")
	if m.Score != 0.5 || m.State != StateAlive {
		t.Fatalf("member = %+v", m)
	}
	scores := tb.Snapshot().Scores()
	if scores["a"] != 0.5 || scores["b"] != 2 || len(scores) != 2 {
		t.Fatalf("Scores() = %v", scores)
	}
}
