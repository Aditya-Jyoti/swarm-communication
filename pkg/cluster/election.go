package cluster

import (
	"math"
	"sort"

	"swarm-net/pkg/health"
	"swarm-net/pkg/protocol"
)

// DefaultThreshold is the fraction of the swarm that becomes leaders.
const DefaultThreshold = 0.3

// DefaultHysteresis is how much better a challenger must score before it may
// displace a sitting leader.
//
// The unit is whatever the health strategy's unit is, which is why this is
// configuration rather than a constant buried in the algorithm: 0.5 is half a
// millisecond under LatencyHealthStrategy and half of everything under a strategy
// reporting a 0-1 load fraction. A deployment that swaps the strategy must revisit
// this number, and the docs say so.
const DefaultHysteresis = 0.5

// Config parameterises an election.
type Config struct {
	// Threshold is the leader fraction. Out-of-range values fall back to the default
	// rather than panicking, matching the convention in pkg/health.
	Threshold float64
	// Hysteresis is the margin a challenger must beat an incumbent by. Zero is legal
	// and means "no damping", which is a reasonable choice in tests and a bad one in
	// production.
	Hysteresis float64
}

func (c Config) withDefaults() Config {
	if c.Threshold <= 0 || c.Threshold > 1 {
		c.Threshold = DefaultThreshold
	}
	if c.Hysteresis < 0 || math.IsNaN(c.Hysteresis) {
		c.Hysteresis = DefaultHysteresis
	}
	return c
}

// Result is the outcome of one election.
type Result struct {
	// Leaders is sorted by ID, so two nodes computing the same election produce
	// byte-identical results and disagreement is detectable by comparison.
	Leaders []protocol.NodeID
	// Size is the N the leader count was computed from: members believed alive.
	Size int
	// Want is the number of leaders the formula asked for. It can exceed len(Leaders)
	// when too few candidates have a usable score, and that gap is the interesting
	// signal -- it means the swarm could not staff its own leadership.
	Want int
}

// LeaderCount implements LeaderCount = max(1, ceil(N * threshold)).
//
// The ceiling is what guarantees a swarm of any size has someone in charge: for any
// n >= 1 and threshold > 0, ceil(n * threshold) is already at least 1 (a 2-node swarm
// at 0.3 gives ceil(0.6) = 1, not 0). The max(1, ...) floor is therefore belt and
// braces: it only bites when the product is exactly zero, and the guards above
// (n <= 0 returns early, an out-of-range threshold falls back to the default) make
// that unreachable. It stays because the formula is stated in the brief with the
// floor, and because a future change to the guards must not be able to produce a
// leaderless swarm. The result is also capped at n, because electing more leaders
// than there are nodes is not a thing.
func LeaderCount(n int, threshold float64) int {
	if n <= 0 {
		return 0
	}
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultThreshold
	}
	k := int(math.Ceil(float64(n) * threshold))
	if k < 1 {
		k = 1
	}
	if k > n {
		k = n
	}
	return k
}

// Elect chooses leaders from view using scores, damped by which nodes are already
// leaders.
//
// # Why this is a pure function
//
// It takes a snapshot and a score map and returns a result. It opens no sockets,
// reads no clock, and mutates nothing. Two consequences follow, and both were
// committed to in the worklog:
//
//   - It is idempotent. The same view and scores produce the same leaders no matter
//     which trigger invoked it -- the membership-change path or the periodic floor.
//     Two entry points into one routine is only safe because of this, and it is
//     tested as a property rather than asserted in a comment.
//   - Every node running it against the same view reaches the same answer without
//     exchanging a message. Election results are gossiped to converge faster and to
//     make disagreement visible, not because agreement requires a round trip.
//
// # Scores
//
// Lower is better, and a score is only comparable to other scores from the same
// strategy instance. This function therefore compares with health.Better and never
// with a bare `<`, so a NaN from a failed probe can never win a seat: a node we
// could not reach must not be promoted on the strength of our inability to measure
// it.
//
// A candidate with no entry in scores is treated as unmeasured, not as excellent.
//
// # Suspect members
//
// A suspect LEADER stays in the election: it counts toward N and may keep its
// seat. Suspicion is a doubt that resolves within SuspicionTimeout, either by a
// refutation or by a death. Dropping the leader on the doubt would re-elect,
// and then re-elect back when the refutation lands, which turns every lost
// packet into two swarm-wide re-homes. Workers already stop using a leader
// they suspect first-hand (see Node.usableLeaders), so keeping the seat costs
// nobody a live leader; it only delays the promotion until the suspicion is
// confirmed. A DEAD leader gets no such grace and is replaced at once.
//
// A suspect WORKER is out: it is neither counted nor eligible. Promoting a node
// we already doubt is the wrong way to fill a seat.
func Elect(view View, scores map[protocol.NodeID]float64, cfg Config) Result {
	cfg = cfg.withDefaults()

	alive := electorate(view)
	result := Result{Size: len(alive)}
	if len(alive) == 0 {
		return result
	}
	result.Want = LeaderCount(len(alive), cfg.Threshold)

	scoreOf := func(id protocol.NodeID) float64 {
		s, ok := scores[id]
		if !ok {
			return health.ScoreUnavailable()
		}
		return s
	}

	// Rank by score, breaking ties on ID. The tie-break is not cosmetic: without it,
	// equal scores would leave ordering to sort stability over an arbitrary input
	// order, and two nodes could pick different leaders from identical data.
	ranked := make([]Member, len(alive))
	copy(ranked, alive)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := scoreOf(ranked[i].ID), scoreOf(ranked[j].ID)
		if health.Better(a, b) {
			return true
		}
		if health.Better(b, a) {
			return false
		}
		return ranked[i].ID < ranked[j].ID
	})

	// Only measurable candidates may lead. A node whose score is NaN is one we could
	// not probe, and promoting it would be acting on an absence of information.
	eligible := make([]Member, 0, len(ranked))
	for _, m := range ranked {
		if health.IsValidScore(scoreOf(m.ID)) {
			eligible = append(eligible, m)
		}
	}

	// If nothing is measurable the swarm still needs a leader, or it stalls forever
	// waiting for a probe that may never succeed. Fall back to the deterministic
	// lowest ID: an arbitrary but agreed-upon choice beats no choice.
	if len(eligible) == 0 {
		result.Leaders = []protocol.NodeID{ranked[0].ID}
		sortIDs(result.Leaders)
		return result
	}

	want := result.Want
	if want > len(eligible) {
		want = len(eligible)
	}

	chosen := make([]Member, want)
	copy(chosen, eligible[:want])

	chosen = applyHysteresis(chosen, eligible, scoreOf, cfg.Hysteresis)

	result.Leaders = make([]protocol.NodeID, 0, len(chosen))
	for _, m := range chosen {
		result.Leaders = append(result.Leaders, m.ID)
	}
	sortIDs(result.Leaders)
	return result
}

// electorate returns the members Elect considers, in view order: every alive
// member, plus suspect members that currently claim leadership.
func electorate(view View) []Member {
	out := make([]Member, 0, len(view.Members))
	for _, m := range view.Members {
		if m.State == StateAlive || (m.State == StateSuspect && m.Role == RoleLeader) {
			out = append(out, m)
		}
	}
	return out
}

// applyHysteresis lets a sitting leader keep its seat unless a challenger beats it by
// the configured margin.
//
// Without damping, leadership follows noise: two nodes 0.3 ms apart would trade the
// role on every evaluation, and every swap costs a cluster-wide re-home. The margin
// converts "better" into "better enough to be worth the disruption".
//
// An incumbent whose score has become invalid is never retained. It is unreachable,
// and hysteresis is meant to resist noise, not to protect a dead node.
func applyHysteresis(chosen, eligible []Member, scoreOf func(protocol.NodeID) float64, margin float64) []Member {
	if margin <= 0 {
		return chosen
	}

	inChosen := make(map[protocol.NodeID]bool, len(chosen))
	for _, m := range chosen {
		inChosen[m.ID] = true
	}

	// Incumbents that lost their seat, in deterministic order.
	var displaced []Member
	for _, m := range eligible {
		if m.Role == RoleLeader && !inChosen[m.ID] && health.IsValidScore(scoreOf(m.ID)) {
			displaced = append(displaced, m)
		}
	}
	if len(displaced) == 0 {
		return chosen
	}
	sort.SliceStable(displaced, func(i, j int) bool { return displaced[i].ID < displaced[j].ID })

	for _, inc := range displaced {
		// Find the weakest newcomer currently holding a seat: the one this incumbent
		// has the best claim to displace.
		weakest := -1
		for i, m := range chosen {
			if m.Role == RoleLeader {
				continue // not a newcomer; leave sitting leaders alone
			}
			if weakest == -1 || health.Better(scoreOf(chosen[weakest].ID), scoreOf(m.ID)) {
				weakest = i
			}
		}
		if weakest == -1 {
			break // every seat is held by an incumbent
		}

		challenger := scoreOf(chosen[weakest].ID)
		incumbent := scoreOf(inc.ID)

		// The challenger keeps the seat only if it is better by at least the margin.
		if incumbent-challenger >= margin {
			continue
		}
		chosen[weakest] = inc
		inChosen[inc.ID] = true
	}

	return chosen
}

func sortIDs(ids []protocol.NodeID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}
