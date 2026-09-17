package cluster

import "testing"

func TestQuorum(t *testing.T) {
	tests := []struct {
		n, want int
	}{
		{0, 0},
		{1, 1},
		// Two nodes: a majority is both of them. There is no way to lose one node
		// and still hold a majority of two.
		{2, 2},
		{3, 2},
		{4, 3},
		{5, 3},
		{10, 6},
		{11, 6},
		// Negative sizes are nonsense input, not a reason to panic.
		{-1, 0},
		{-7, 0},
	}

	for _, tt := range tests {
		if got := Quorum(tt.n); got != tt.want {
			t.Errorf("Quorum(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

// A quorum is a strict majority: any two quorums of the same n must overlap in at
// least one node. That is the property the number exists to provide.
func TestQuorumIsStrictMajority(t *testing.T) {
	for n := 1; n <= 50; n++ {
		q := Quorum(n)
		if 2*q <= n {
			t.Errorf("Quorum(%d) = %d is not a strict majority", n, q)
		}
		if 2*(q-1) > n {
			t.Errorf("Quorum(%d) = %d is larger than the smallest majority", n, q)
		}
	}
}

func TestDegraded(t *testing.T) {
	tests := []struct {
		name          string
		alive         int
		lastKnownSize int
		want          bool
	}{
		{"single node alone", 1, 1, false},
		// Quorum(2) == 2, so a two-node swarm that loses one node is, by definition,
		// a minority. Both halves of a split pair report degraded, which is the
		// honest answer: neither can know whether the other is dead or merely
		// unreachable.
		{"two-node swarm lost one", 1, 2, true},
		{"two-node swarm intact", 2, 2, false},
		{"majority of three", 2, 3, false},
		{"minority of three", 1, 3, true},
		{"all of three", 3, 3, false},
		{"exact quorum of five", 3, 5, false},
		{"one under quorum of five", 2, 5, true},
		{"exact quorum of ten", 6, 10, false},
		{"one under quorum of ten", 5, 10, true},
		// alive <= 0 is never degraded per the contract: a node that counts nobody,
		// including itself, has not observed anything yet.
		{"zero alive", 0, 5, false},
		{"negative alive", -1, 5, false},
		// lastKnownSize <= 1 is never degraded: there was never a swarm to be a
		// minority of.
		{"last known size one", 1, 1, false},
		{"last known size zero", 1, 0, false},
		{"last known size negative", 1, -4, false},
		// More alive than the high-water mark means the caller has not raised it
		// yet, and growth is never a minority.
		{"alive exceeds last known", 5, 3, false},
		{"both negative", -2, -2, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Degraded(tt.alive, tt.lastKnownSize); got != tt.want {
				t.Errorf("Degraded(alive=%d, last=%d) = %v, want %v",
					tt.alive, tt.lastKnownSize, got, tt.want)
			}
		})
	}
}

// Degraded must agree with Quorum exactly, so that the dashboard's "degraded"
// badge and any textual "x of y, quorum q" line can never disagree.
func TestDegradedMatchesQuorum(t *testing.T) {
	for last := 2; last <= 30; last++ {
		for alive := 1; alive <= last; alive++ {
			want := alive < Quorum(last)
			if got := Degraded(alive, last); got != want {
				t.Errorf("Degraded(%d, %d) = %v, want %v", alive, last, got, want)
			}
		}
	}
}
