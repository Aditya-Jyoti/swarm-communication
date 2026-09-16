package cluster

import (
	"math"

	"swarm-net/pkg/health"
	"swarm-net/pkg/protocol"
)

// ChooseLeader picks, from leaders, the one with the best score.
//
// This is the whole of affinity clustering: a worker probes every elected leader
// and joins the one that scored best. There is no quota, no balancing and no
// assignment authority, so clusters come out uneven on purpose -- a leader on a fast
// path from seven workers gets seven workers. Load balancing, if it is ever wanted,
// is an admission policy on the leader, not a rule hidden in here.
//
// A leader with no score, or an invalid one, is ineligible: "I could not measure
// it" must never read as "it is the closest". Ties break on the lowest ID so that
// the answer is a function of the inputs alone, not of the order the caller
// enumerated the leader set in. Returns false when no leader is eligible; the
// caller decides whether to wait for a probe or fall back.
//
// Like Elect, this is pure: no clock, no socket, no mutation of leaders.
func ChooseLeader(scores map[protocol.NodeID]float64, leaders []protocol.NodeID) (protocol.NodeID, bool) {
	var (
		best      protocol.NodeID
		bestScore float64
		found     bool
	)
	for _, id := range leaders {
		s, ok := scores[id]
		if !ok || !health.IsValidScore(s) {
			continue
		}
		switch {
		case !found:
			best, bestScore, found = id, s, true
		case health.Better(s, bestScore):
			best, bestScore = id, s
		case !health.Better(bestScore, s) && id < best:
			// Neither is better: an exact tie. Lower ID wins, regardless of which
			// one we happened to see first.
			best, bestScore = id, s
		}
	}
	return best, found
}

// ShouldRehome reports whether a worker attached to current should move to best.
//
// It applies the same damping idea as election hysteresis. A worker that re-homes
// every time two leaders' scores cross by a hair pays a full detach-and-join for
// nothing, so a move needs a reason: best must be strictly better than current, and
// better by at least margin. Two cases bypass the margin because they are not
// noise -- current is no longer a leader, or current can no longer be measured.
// Hysteresis is for resisting jitter, not for staying loyal to a dead leader.
//
// If best is not itself a usable destination -- an invalid score, or not in
// leaders -- the answer is false whatever the state of current, because "move" with
// nowhere to move to is not an instruction the caller can act on.
//
// margin < 0 or NaN is treated as 0, matching Config.withDefaults' tolerance for
// bad configuration over panicking on it.
func ShouldRehome(current, best protocol.NodeID, scores map[protocol.NodeID]float64, leaders []protocol.NodeID, margin float64) bool {
	if margin < 0 || math.IsNaN(margin) {
		margin = 0
	}

	bestScore, ok := scores[best]
	if !ok || !health.IsValidScore(bestScore) || !contains(leaders, best) {
		return false
	}
	if best == current {
		return false
	}

	curScore, ok := scores[current]
	if !ok || !health.IsValidScore(curScore) || !contains(leaders, current) {
		return true
	}

	// Better rather than a bare `<`: the lower-is-better direction is stated once,
	// in pkg/health, and never re-derived here.
	return health.Better(bestScore, curScore) && curScore-bestScore >= margin
}

func contains(ids []protocol.NodeID, id protocol.NodeID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
