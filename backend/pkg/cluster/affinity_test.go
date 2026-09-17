package cluster

import (
	"math"
	"testing"

	"swarm-net/pkg/protocol"
)

func TestChooseLeader(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)

	tests := []struct {
		name    string
		scores  map[protocol.NodeID]float64
		leaders []protocol.NodeID
		want    protocol.NodeID
		wantOK  bool
	}{
		{
			name:    "empty leaders",
			scores:  map[protocol.NodeID]float64{"node-1": 1},
			leaders: nil,
		},
		{
			name:    "nil scores map",
			scores:  nil,
			leaders: []protocol.NodeID{"node-1", "node-2"},
		},
		{
			name:    "all leaders unmeasured",
			scores:  map[protocol.NodeID]float64{"node-9": 1},
			leaders: []protocol.NodeID{"node-1", "node-2"},
		},
		{
			name:    "all NaN",
			scores:  map[protocol.NodeID]float64{"node-1": nan, "node-2": nan},
			leaders: []protocol.NodeID{"node-1", "node-2"},
		},
		{
			name:    "one valid among NaNs",
			scores:  map[protocol.NodeID]float64{"node-1": nan, "node-2": 40, "node-3": nan},
			leaders: []protocol.NodeID{"node-1", "node-2", "node-3"},
			want:    "node-2",
			wantOK:  true,
		},
		{
			name:    "+Inf is rejected even when it is the only score",
			scores:  map[protocol.NodeID]float64{"node-1": inf},
			leaders: []protocol.NodeID{"node-1"},
		},
		{
			name:    "+Inf loses to a finite score",
			scores:  map[protocol.NodeID]float64{"node-1": inf, "node-2": 500},
			leaders: []protocol.NodeID{"node-1", "node-2"},
			want:    "node-2",
			wantOK:  true,
		},
		{
			name:    "lowest score wins",
			scores:  map[protocol.NodeID]float64{"node-1": 3, "node-2": 1, "node-3": 2},
			leaders: []protocol.NodeID{"node-1", "node-2", "node-3"},
			want:    "node-2",
			wantOK:  true,
		},
		{
			name:    "exact tie breaks on lowest ID",
			scores:  map[protocol.NodeID]float64{"node-1": 2, "node-2": 2, "node-3": 2},
			leaders: []protocol.NodeID{"node-3", "node-2", "node-1"},
			want:    "node-1",
			wantOK:  true,
		},
		{
			name:    "scores for non-leaders are ignored",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 6, "worker-x": 0.1},
			leaders: []protocol.NodeID{"node-1", "node-2"},
			want:    "node-1",
			wantOK:  true,
		},
		{
			name:    "zero is a valid, excellent score",
			scores:  map[protocol.NodeID]float64{"node-1": 0, "node-2": 1},
			leaders: []protocol.NodeID{"node-2", "node-1"},
			want:    "node-1",
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ChooseLeader(tt.scores, tt.leaders)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tt.wantOK, got)
			}
			if got != tt.want {
				t.Errorf("leader = %q, want %q", got, tt.want)
			}
		})
	}
}

// The answer must not depend on the order leaders were handed to us: two workers
// with the same scores must land on the same leader regardless of how each of them
// happened to enumerate the leader set.
func TestChooseLeaderOrderIndependent(t *testing.T) {
	scores := map[protocol.NodeID]float64{
		"node-1": 4, "node-2": 2, "node-3": 2, "node-4": math.NaN(), "node-5": 9,
	}
	base := []protocol.NodeID{"node-1", "node-2", "node-3", "node-4", "node-5"}

	want, ok := ChooseLeader(scores, base)
	if !ok || want != "node-2" {
		t.Fatalf("baseline = %q, %v; want node-2, true", want, ok)
	}

	for _, perm := range permutations(base) {
		got, ok := ChooseLeader(scores, perm)
		if !ok || got != want {
			t.Errorf("ChooseLeader(%v) = %q, %v; want %q, true", perm, got, ok, want)
		}
	}
}

// ChooseLeader must not mutate its input. Callers pass View.Leaders() straight in,
// and a reordering would leak into the snapshot they hold.
func TestChooseLeaderDoesNotMutateInput(t *testing.T) {
	leaders := []protocol.NodeID{"node-3", "node-1", "node-2"}
	orig := append([]protocol.NodeID(nil), leaders...)
	scores := map[protocol.NodeID]float64{"node-1": 1, "node-2": 1, "node-3": 1}

	ChooseLeader(scores, leaders)

	for i := range orig {
		if leaders[i] != orig[i] {
			t.Fatalf("input mutated: %v, was %v", leaders, orig)
		}
	}
}

func TestShouldRehome(t *testing.T) {
	nan := math.NaN()
	leaders := []protocol.NodeID{"node-1", "node-2", "node-3"}

	tests := []struct {
		name    string
		current protocol.NodeID
		best    protocol.NodeID
		scores  map[protocol.NodeID]float64
		leaders []protocol.NodeID
		margin  float64
		want    bool
	}{
		{
			name:    "current no longer a leader",
			current: "node-9",
			best:    "node-1",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-9": 1},
			leaders: leaders,
			margin:  10,
			want:    true,
		},
		{
			name:    "current has NaN score",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": nan, "node-2": 50},
			leaders: leaders,
			margin:  10,
			want:    true,
		},
		{
			name:    "current has no score at all",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-2": 50},
			leaders: leaders,
			margin:  10,
			want:    true,
		},
		{
			name:    "current has +Inf score",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": math.Inf(1), "node-2": 50},
			leaders: leaders,
			margin:  10,
			want:    true,
		},
		{
			name:    "best is current",
			current: "node-1",
			best:    "node-1",
			scores:  map[protocol.NodeID]float64{"node-1": 5},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "best is current and current is invalid: nowhere to go",
			current: "node-1",
			best:    "node-1",
			scores:  map[protocol.NodeID]float64{"node-1": nan},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "best invalid",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": nan},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "best invalid and current invalid: still no destination",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": nan, "node-2": nan},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "best not in leaders is never a destination",
			current: "node-1",
			best:    "worker-x",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "worker-x": 0.1},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "better by less than margin",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 4.6},
			leaders: leaders,
			margin:  0.5,
			want:    false,
		},
		{
			name:    "better by exactly margin",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 4.5},
			leaders: leaders,
			margin:  0.5,
			want:    true,
		},
		{
			name:    "better by more than margin",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 1},
			leaders: leaders,
			margin:  0.5,
			want:    true,
		},
		{
			name:    "best is worse than current",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 1, "node-2": 5},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "equal scores with zero margin: not strictly better, stay put",
			current: "node-2",
			best:    "node-1",
			scores:  map[protocol.NodeID]float64{"node-1": 3, "node-2": 3},
			leaders: leaders,
			margin:  0,
			want:    false,
		},
		{
			name:    "NaN margin is treated as zero",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 4.99},
			leaders: leaders,
			margin:  nan,
			want:    true,
		},
		{
			name:    "negative margin is treated as zero",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 4.99},
			leaders: leaders,
			margin:  -3,
			want:    true,
		},
		{
			name:    "negative margin does not make a worse best attractive",
			current: "node-1",
			best:    "node-2",
			scores:  map[protocol.NodeID]float64{"node-1": 5, "node-2": 5.5},
			leaders: leaders,
			margin:  -3,
			want:    false,
		},
		{
			name:    "nil scores: current unmeasured but best unmeasured too",
			current: "node-1",
			best:    "node-2",
			scores:  nil,
			leaders: leaders,
			margin:  0,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldRehome(tt.current, tt.best, tt.scores, tt.leaders, tt.margin)
			if got != tt.want {
				t.Errorf("ShouldRehome(%q, %q, margin=%v) = %v, want %v",
					tt.current, tt.best, tt.margin, got, tt.want)
			}
		})
	}
}

// ChooseLeader followed by ShouldRehome must be a fixed point: once a worker is
// attached to the leader ChooseLeader picks, asking again never moves it.
func TestChooseLeaderThenRehomeIsStable(t *testing.T) {
	scores := map[protocol.NodeID]float64{"node-1": 3, "node-2": 1, "node-3": math.NaN()}
	leaders := []protocol.NodeID{"node-1", "node-2", "node-3"}

	best, ok := ChooseLeader(scores, leaders)
	if !ok {
		t.Fatal("expected an eligible leader")
	}
	for _, margin := range []float64{0, 0.5, 100} {
		if ShouldRehome(best, best, scores, leaders, margin) {
			t.Errorf("margin %v: worker attached to ChooseLeader's pick was told to move", margin)
		}
	}
}

// permutations returns every ordering of ids. Sizes in tests are tiny, so the
// factorial cost is irrelevant.
func permutations(ids []protocol.NodeID) [][]protocol.NodeID {
	if len(ids) <= 1 {
		return [][]protocol.NodeID{append([]protocol.NodeID(nil), ids...)}
	}
	var out [][]protocol.NodeID
	for i := range ids {
		rest := make([]protocol.NodeID, 0, len(ids)-1)
		rest = append(rest, ids[:i]...)
		rest = append(rest, ids[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]protocol.NodeID{ids[i]}, p...))
		}
	}
	return out
}
