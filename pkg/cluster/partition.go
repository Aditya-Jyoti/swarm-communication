package cluster

// Partition detection.
//
// This is an AP design. When the network splits, every partition keeps electing
// leaders and keeps serving tasks; a drone group that halts because it lost sight
// of the rest of the swarm is a group that has stopped coordinating, which is worse
// than a group coordinating on partial information. Degraded therefore does not
// gate anything. It exists so that split-brain is observable -- a node that knows
// it is in the minority says so in telemetry and the dashboard shows it -- not so
// that it is prevented. Preventing it would require a consensus algorithm (Raft,
// Paxos) with the write-availability cost that implies, and this system does not
// replicate a log that would need one.

// Quorum is the smallest strict majority of n: floor(n/2)+1.
//
// Strict rather than "half": with n = 4, two nodes are not a majority, because the
// other two could be alive on the far side of a partition and believe exactly the
// same thing. Any two sets of size Quorum(n) overlap in at least one node, which is
// the property that makes "I can see a quorum" mean "no other partition can".
// Quorum(n <= 0) is 0.
func Quorum(n int) int {
	if n <= 0 {
		return 0
	}
	return n/2 + 1
}

// Degraded reports whether this node is in a minority partition: it can see fewer
// alive nodes than a majority of the swarm it last knew about.
//
// lastKnownSize is the caller's high-water mark of alive count, not the current
// membership size. Measuring against the current view would be circular -- a
// partition of two would see two, compute a quorum of two, and declare itself
// healthy. The cost of a high-water mark is that a swarm which legitimately scales
// down looks degraded until the caller resets it, and that is the right trade:
// "we shrank" and "we were cut off" are indistinguishable from inside, so both
// should be visible until an operator says otherwise.
//
// alive <= 0 or lastKnownSize <= 1 is never degraded. A node counting nobody has
// not observed the swarm yet, and a swarm that was never larger than one has no
// majority to be a minority of.
func Degraded(alive, lastKnownSize int) bool {
	if alive <= 0 || lastKnownSize <= 1 {
		return false
	}
	return alive < Quorum(lastKnownSize)
}
