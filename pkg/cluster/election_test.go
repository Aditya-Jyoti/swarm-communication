package cluster

import (
	"math"
	"reflect"
	"sync"
	"testing"

	"swarm-net/pkg/health"
	"swarm-net/pkg/protocol"
)

func member(id protocol.NodeID, role Role) Member {
	return Member{ID: id, Addr: protocol.NodeAddress(string(id) + ":7946"), Role: role, State: StateAlive}
}

func viewOf(ms ...Member) View { return View{Members: ms} }

func TestLeaderCount(t *testing.T) {
	tests := []struct {
		n         int
		threshold float64
		want      int
	}{
		{0, 0.3, 0},
		// max(1, ...) is what stops a small swarm electing nobody and stalling.
		{1, 0.3, 1},
		{2, 0.3, 1},
		{3, 0.3, 1},
		{4, 0.3, 2},
		{10, 0.3, 3},
		{100, 0.3, 30},
		// Ceiling, not rounding: 11 * 0.3 = 3.3 -> 4.
		{11, 0.3, 4},
		{1, 1.0, 1},
		{5, 1.0, 5},
		// Out-of-range thresholds fall back to the default rather than panicking.
		{10, 0, 3},
		{10, -1, 3},
		{10, 1.5, 3},
	}

	for _, tt := range tests {
		if got := LeaderCount(tt.n, tt.threshold); got != tt.want {
			t.Errorf("LeaderCount(%d, %v) = %d, want %d", tt.n, tt.threshold, got, tt.want)
		}
	}
}

// Leaders must never exceed the member count, or a 2-node swarm with a high
// threshold would try to elect three.
func TestLeaderCountNeverExceedsN(t *testing.T) {
	for n := 1; n <= 20; n++ {
		if got := LeaderCount(n, 1.0); got > n {
			t.Errorf("LeaderCount(%d, 1.0) = %d, which exceeds N", n, got)
		}
	}
}

func TestElectPicksLowestScores(t *testing.T) {
	v := viewOf(
		member("node-1", RoleWorker),
		member("node-2", RoleWorker),
		member("node-3", RoleWorker),
		member("node-4", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{
		"node-1": 10, "node-2": 2, "node-3": 8, "node-4": 1,
	}

	got := Elect(v, scores, Config{Threshold: 0.5, Hysteresis: 0})

	want := []protocol.NodeID{"node-2", "node-4"}
	if !reflect.DeepEqual(got.Leaders, want) {
		t.Errorf("Leaders = %v, want %v", got.Leaders, want)
	}
	if got.Size != 4 {
		t.Errorf("Size = %d, want 4", got.Size)
	}
	if got.Want != 2 {
		t.Errorf("Want = %d, want 2", got.Want)
	}
}

// The worklog committed to testing idempotence as a property, because two triggers
// (membership change and the periodic floor) enter the same routine. If the result
// depended on the path or on call count, the periodic sweep would cause churn.
func TestElectIsIdempotent(t *testing.T) {
	v := viewOf(
		member("node-1", RoleLeader),
		member("node-2", RoleWorker),
		member("node-3", RoleWorker),
		member("node-4", RoleWorker),
		member("node-5", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{
		"node-1": 5, "node-2": 3, "node-3": 9, "node-4": 1, "node-5": 7,
	}
	cfg := Config{Threshold: 0.4, Hysteresis: 0.5}

	first := Elect(v, scores, cfg)
	for i := 0; i < 20; i++ {
		got := Elect(v, scores, cfg)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("call %d differed:\n got %+v\nwant %+v", i, got, first)
		}
	}
}

// Two nodes holding the same membership must reach the same answer without talking,
// regardless of the order their views happen to be assembled in.
func TestElectIsIndependentOfMemberOrder(t *testing.T) {
	scores := map[protocol.NodeID]float64{
		"node-1": 4, "node-2": 4, "node-3": 4, "node-4": 1,
	}
	cfg := Config{Threshold: 0.5, Hysteresis: 0}

	forward := Elect(viewOf(
		member("node-1", RoleWorker), member("node-2", RoleWorker),
		member("node-3", RoleWorker), member("node-4", RoleWorker),
	), scores, cfg)

	reverse := Elect(viewOf(
		member("node-4", RoleWorker), member("node-3", RoleWorker),
		member("node-2", RoleWorker), member("node-1", RoleWorker),
	), scores, cfg)

	if !reflect.DeepEqual(forward.Leaders, reverse.Leaders) {
		t.Errorf("order changed the outcome: %v vs %v", forward.Leaders, reverse.Leaders)
	}
	// Equal scores must break on ID, so node-1 joins node-4 rather than an arbitrary peer.
	want := []protocol.NodeID{"node-1", "node-4"}
	if !reflect.DeepEqual(forward.Leaders, want) {
		t.Errorf("Leaders = %v, want %v", forward.Leaders, want)
	}
}

// A node we could not probe has no score, not a perfect one. Promoting it would be
// acting on an absence of information -- and with a bare `<` comparison instead of
// health.Better, NaN would sort first and win every election.
func TestUnreachableNodeIsNeverElected(t *testing.T) {
	v := viewOf(
		member("node-1", RoleWorker),
		member("node-2", RoleWorker),
		member("node-3", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{
		"node-1": health.ScoreUnavailable(),
		"node-2": 50,
		"node-3": 80,
	}

	got := Elect(v, scores, Config{Threshold: 0.3, Hysteresis: 0})

	for _, id := range got.Leaders {
		if id == "node-1" {
			t.Fatalf("an unreachable node was elected: %v", got.Leaders)
		}
	}
	if len(got.Leaders) != 1 || got.Leaders[0] != "node-2" {
		t.Errorf("Leaders = %v, want [node-2]", got.Leaders)
	}
}

// A candidate absent from the score map is unmeasured, which is the same as
// unreachable -- not a free pass into leadership.
func TestMissingScoreIsTreatedAsUnmeasured(t *testing.T) {
	v := viewOf(member("node-1", RoleWorker), member("node-2", RoleWorker))
	scores := map[protocol.NodeID]float64{"node-2": 12}

	got := Elect(v, scores, Config{Threshold: 0.3, Hysteresis: 0})

	if len(got.Leaders) != 1 || got.Leaders[0] != "node-2" {
		t.Errorf("Leaders = %v, want [node-2]", got.Leaders)
	}
}

// If nothing is measurable the swarm still needs someone in charge, or it stalls
// forever waiting for a probe that may never succeed. The fallback must be
// deterministic so every node picks the same one.
func TestAllUnreachableFallsBackToDeterministicChoice(t *testing.T) {
	v := viewOf(
		member("node-7", RoleWorker),
		member("node-2", RoleWorker),
		member("node-5", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{
		"node-7": math.NaN(), "node-2": math.NaN(), "node-5": math.NaN(),
	}

	first := Elect(v, scores, Config{})
	if len(first.Leaders) != 1 {
		t.Fatalf("want exactly one fallback leader, got %v", first.Leaders)
	}
	if first.Leaders[0] != "node-2" {
		t.Errorf("fallback = %v, want the lowest ID node-2", first.Leaders[0])
	}
	if got := Elect(v, scores, Config{}); !reflect.DeepEqual(got.Leaders, first.Leaders) {
		t.Errorf("fallback is not deterministic: %v then %v", first.Leaders, got.Leaders)
	}
}

// Want records what the formula asked for even when too few candidates are
// measurable. The gap is the signal that the swarm could not staff its leadership.
func TestWantRecordsTheShortfall(t *testing.T) {
	v := viewOf(
		member("node-1", RoleWorker), member("node-2", RoleWorker),
		member("node-3", RoleWorker), member("node-4", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{"node-1": 5} // only one measurable

	got := Elect(v, scores, Config{Threshold: 0.75, Hysteresis: 0})

	if got.Want != 3 {
		t.Errorf("Want = %d, want 3", got.Want)
	}
	if len(got.Leaders) != 1 {
		t.Errorf("Leaders = %v, want a single staffed seat", got.Leaders)
	}
}

// Dead and suspect members are not candidates and do not count toward N.
func TestOnlyAliveMembersParticipate(t *testing.T) {
	v := viewOf(
		Member{ID: "node-1", Role: RoleWorker, State: StateAlive},
		Member{ID: "node-2", Role: RoleWorker, State: StateDead},
		Member{ID: "node-3", Role: RoleWorker, State: StateSuspect},
		Member{ID: "node-4", Role: RoleWorker, State: StateAlive},
	)
	scores := map[protocol.NodeID]float64{"node-1": 9, "node-2": 1, "node-3": 1, "node-4": 8}

	got := Elect(v, scores, Config{Threshold: 0.5, Hysteresis: 0})

	if got.Size != 2 {
		t.Errorf("Size = %d, want 2 (only alive members)", got.Size)
	}
	for _, id := range got.Leaders {
		if id == "node-2" || id == "node-3" {
			t.Errorf("a non-alive member was elected: %v", got.Leaders)
		}
	}
}

func TestElectOnEmptyView(t *testing.T) {
	got := Elect(View{}, nil, Config{})
	if len(got.Leaders) != 0 || got.Size != 0 || got.Want != 0 {
		t.Errorf("empty view produced %+v", got)
	}
}

// --- hysteresis -------------------------------------------------------------

// Without damping, leadership follows noise: a challenger 0.2 better would take the
// seat and hand it back on the next evaluation, and every swap costs a cluster-wide
// re-home.
func TestIncumbentKeepsSeatWithinHysteresisMargin(t *testing.T) {
	v := viewOf(
		member("node-1", RoleLeader), // incumbent
		member("node-2", RoleWorker), // marginally better challenger
	)
	scores := map[protocol.NodeID]float64{"node-1": 10.0, "node-2": 9.8}

	got := Elect(v, scores, Config{Threshold: 0.3, Hysteresis: 0.5})

	if len(got.Leaders) != 1 || got.Leaders[0] != "node-1" {
		t.Errorf("Leaders = %v, want the incumbent node-1 to hold its seat", got.Leaders)
	}
}

// Damping must not become paralysis: a genuinely better node still wins.
func TestChallengerBeatingMarginTakesTheSeat(t *testing.T) {
	v := viewOf(
		member("node-1", RoleLeader),
		member("node-2", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{"node-1": 10.0, "node-2": 4.0}

	got := Elect(v, scores, Config{Threshold: 0.3, Hysteresis: 0.5})

	if len(got.Leaders) != 1 || got.Leaders[0] != "node-2" {
		t.Errorf("Leaders = %v, want node-2 to displace the incumbent", got.Leaders)
	}
}

// Hysteresis resists noise; it must not protect a corpse. An incumbent we can no
// longer measure has to lose its seat or a dead leader is never replaced.
func TestUnreachableIncumbentLosesItsSeat(t *testing.T) {
	v := viewOf(
		member("node-1", RoleLeader),
		member("node-2", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{
		"node-1": health.ScoreUnavailable(),
		"node-2": 100.0,
	}

	got := Elect(v, scores, Config{Threshold: 0.3, Hysteresis: 0.5})

	if len(got.Leaders) != 1 || got.Leaders[0] != "node-2" {
		t.Errorf("Leaders = %v, want the unreachable incumbent replaced by node-2", got.Leaders)
	}
}

// A sub-margin score change must not flip leadership on the periodic sweep. This is
// the concrete churn scenario the worklog named: a 0.3 ms move must not cost a
// re-home.
func TestSmallScoreDriftDoesNotFlipLeadership(t *testing.T) {
	v := viewOf(
		member("node-1", RoleLeader),
		member("node-2", RoleWorker),
		member("node-3", RoleWorker),
	)
	cfg := Config{Threshold: 0.3, Hysteresis: 0.5}

	base := map[protocol.NodeID]float64{"node-1": 5.0, "node-2": 5.2, "node-3": 9.0}
	drift := map[protocol.NodeID]float64{"node-1": 5.3, "node-2": 5.0, "node-3": 9.0}

	before := Elect(v, base, cfg)
	after := Elect(v, drift, cfg)

	if !reflect.DeepEqual(before.Leaders, after.Leaders) {
		t.Errorf("a 0.3 drift flipped leadership: %v -> %v", before.Leaders, after.Leaders)
	}
}

// Zero hysteresis is legal and means no damping at all.
func TestZeroHysteresisAllowsAnyImprovement(t *testing.T) {
	v := viewOf(member("node-1", RoleLeader), member("node-2", RoleWorker))
	scores := map[protocol.NodeID]float64{"node-1": 10.0, "node-2": 9.99}

	got := Elect(v, scores, Config{Threshold: 0.3, Hysteresis: 0})

	if len(got.Leaders) != 1 || got.Leaders[0] != "node-2" {
		t.Errorf("Leaders = %v, want node-2 with damping disabled", got.Leaders)
	}
}

// Elect must not mutate the view it was handed: callers hold snapshots and reuse them.
func TestElectDoesNotMutateItsInput(t *testing.T) {
	members := []Member{
		member("node-1", RoleLeader),
		member("node-2", RoleWorker),
		member("node-3", RoleWorker),
	}
	original := append([]Member(nil), members...)
	scores := map[protocol.NodeID]float64{"node-1": 3, "node-2": 1, "node-3": 2}

	Elect(View{Members: members}, scores, Config{Threshold: 0.6, Hysteresis: 0.5})

	if !reflect.DeepEqual(members, original) {
		t.Errorf("Elect mutated the view:\n got %+v\nwant %+v", members, original)
	}
}

// The election runs from several goroutines in a live node -- the membership path
// and the periodic sweep. It shares nothing, so this must be race-free.
func TestConcurrentElections(t *testing.T) {
	v := viewOf(
		member("node-1", RoleLeader), member("node-2", RoleWorker),
		member("node-3", RoleWorker), member("node-4", RoleWorker),
	)
	scores := map[protocol.NodeID]float64{"node-1": 5, "node-2": 3, "node-3": 9, "node-4": 1}
	cfg := Config{Threshold: 0.5, Hysteresis: 0.5}
	want := Elect(v, scores, cfg)

	var wg sync.WaitGroup
	wg.Add(16)
	// Owner: this test. Each goroutine runs a bounded loop and returns; wg.Wait
	// joins them all, so none can outlive the test.
	for i := 0; i < 16; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if got := Elect(v, scores, cfg); !reflect.DeepEqual(got, want) {
					t.Errorf("concurrent election differed: %+v", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
