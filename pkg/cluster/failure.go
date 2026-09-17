package cluster

import (
	"context"
	"time"

	"swarm-net/pkg/protocol"
)

// Failure detection: suspicion, confirmation, and the tick that drives them.
//
// # Two steps, not one
//
// Phase 3 turned every lost link straight into a death. Phase 4 puts a doubt in
// between (SWIM's "suspect" state):
//
//	alive --(link lost | SuspectAfter missed probes | missed beats)--> suspect
//	suspect --(SuspicionTimeout | DeadAfter missed probes)--> dead
//	suspect --(reconnect | probe success | member refutes)--> alive
//
// Evidence that needs no confirmation skips the doubt: a LEAVE or clean close
// (markLeft) and a peer speaking garbage (markDead).
//
// # Why suspect does not bump the incarnation
//
// A death bumps it so a queued "alive" at the old number cannot outrank it
// (SetState, HIGH-2). A suspicion must NOT do that. It has to stay refutable by
// the member at the incarnation it already holds, and it has to lose to a
// Revive at that incarnation when the link comes back. A bump would turn every
// doubt into a verdict.
//
// # First-hand versus relayed suspicion
//
// Only suspicions this node raised are timed (n.suspects). A suspect record
// relayed by gossip is still merged, so the election sees it, but it never
// becomes a death here. The originator confirms its own doubt and pushes the
// death, or the member hears the rumour and refutes it. If every node timed every
// rumour, a single node with a bad link could kill a healthy peer swarm-wide.

// tombstone is one death record being aged.
type tombstone struct {
	since       time.Time
	incarnation int64
}

// suspicion is one first-hand doubt being timed.
type suspicion struct {
	// since is when the doubt was raised, on the node's Clock.
	since time.Time
	// incarnation is the member's incarnation when the doubt was raised. A
	// record at a different incarnation means the member refuted or restarted,
	// and the doubt is resolved.
	incarnation int64
	// round is the probe round sequence current when the doubt was raised.
	// Only a probe round started after it may clear the doubt.
	round uint64
}

// markSuspect records a first-hand doubt about id and reports whether the table
// changed. It starts the suspicion timer even when the record was already
// suspect (a relayed rumour we now have our own evidence for), because a
// relayed suspicion is otherwise never confirmed here. A dead member is left
// alone: SetState would otherwise "improve" it to suspect.
func (n *Node) markSuspect(ctx context.Context, id protocol.NodeID, reason string, err error) bool {
	if id == n.cfg.Self {
		return false
	}
	m, ok := n.table.Snapshot().Get(id)
	if !ok || m.State == StateDead {
		return false
	}
	if _, timing := n.suspects[id]; !timing {
		n.suspects[id] = suspicion{since: n.cfg.Clock.Now(), incarnation: m.Incarnation, round: n.probeSeq}
	}
	changed := n.table.SetState(id, StateSuspect)
	if changed {
		n.log.Warn("peer suspected", "peer", id, "reason", reason, "err", err, "timeout", n.cfg.SuspicionTimeout)
		// Pushed like a death: the suspect itself is among the receivers, and
		// hearing the rumour is the only way it can refute it.
		n.pushRecord(ctx, id)
	}
	return changed
}

// probeMissed applies the miss thresholds after n.missed[id] was incremented,
// and reports whether the table changed.
func (n *Node) probeMissed(ctx context.Context, id protocol.NodeID, err error) bool {
	switch missed := n.missed[id]; {
	case missed >= n.cfg.DeadAfter:
		return n.markDead(ctx, id, "missed probes", err)
	case missed >= n.cfg.SuspectAfter:
		return n.markSuspect(ctx, id, "missed probes", err)
	}
	return false
}

// probeClears handles a successful probe of a suspect member: first-hand
// evidence of life, like a handshake, so the suspect record is revived at the
// member's own incarnation. It reports whether the table changed.
//
// The round check is the guard against a stale success. A round that started
// before the link dropped can finish after the PeerDown that raised the
// suspicion, and its success says nothing about the link now.
func (n *Node) probeClears(id protocol.NodeID, round uint64, incarnation int64) bool {
	if s, timing := n.suspects[id]; timing && round <= s.round {
		return false
	}
	delete(n.suspects, id)
	if !n.table.Revive(id, incarnation) {
		return false
	}
	n.log.Info("suspicion cleared by probe", "peer", id)
	return true
}

// usableLeaders filters leaders down to the ones this node may attach to: not
// one it suspects first-hand. A relayed suspicion does not count, for the same
// reason it is not timed: one node's bad link must not empty every cluster.
func (n *Node) usableLeaders(leaders []protocol.NodeID) []protocol.NodeID {
	if len(n.suspects) == 0 {
		return leaders
	}
	out := make([]protocol.NodeID, 0, len(leaders))
	for _, id := range leaders {
		if _, doubted := n.suspects[id]; !doubted {
			out = append(out, id)
		}
	}
	return out
}

// controlTick is the failure-detector tick, every HeartbeatInterval.
func (n *Node) controlTick(ctx context.Context) {
	now := n.cfg.Clock.Now()
	if n.expireSuspicions(ctx, now) {
		n.membershipChanged(ctx)
	}
	if n.sweepTombstones(now) {
		n.publish()
	}
}

// expireSuspicions confirms every first-hand suspicion older than
// SuspicionTimeout and forgets every one that has been resolved. It reports
// whether the table changed.
func (n *Node) expireSuspicions(ctx context.Context, now time.Time) bool {
	if len(n.suspects) == 0 {
		return false
	}
	view := n.table.Snapshot()
	ids := make([]protocol.NodeID, 0, len(n.suspects))
	for id := range n.suspects {
		ids = append(ids, id)
	}
	sortIDs(ids)
	changed := false
	for _, id := range ids {
		s := n.suspects[id]
		m, ok := view.Get(id)
		if !ok || m.State != StateSuspect || m.Incarnation != s.incarnation {
			// Refuted, revived, restarted, or already dead: nothing to confirm.
			delete(n.suspects, id)
			continue
		}
		if now.Sub(s.since) < n.cfg.SuspicionTimeout {
			continue
		}
		if n.markDead(ctx, id, "suspicion timed out", nil) {
			changed = true
		}
	}
	return changed
}

// sweepTombstones removes death records older than TombstoneTTL and reports
// whether it removed any.
//
// # Why dead records are kept at all
//
// A death record at incarnation k+1 is what outranks every stale "alive at k"
// still circulating (markLeft, SetState). Delete it and the next such echo
// re-inserts the member as alive.
//
// # Why they cannot be kept forever
//
// Every full view carries them, so gossip grows with churn rather than with
// swarm size, and a long-lived swarm with container restarts accumulates a
// record per restart.
//
// # The trade-off, and the TTL
//
// After removal, a stale "alive" record for the member re-infects us. How long
// can such a record survive elsewhere? Every node that holds it hears the death
// through anti-entropy within (2N-1) * GossipInterval of the death reaching any
// peer that gossips to it (gossipRound). With N=10 and 2s that is 38s, so 60s
// covers swarms up to about N=15 at the default interval; a bigger swarm or a
// slower interval should raise the TTL. A re-infection past the TTL is not
// permanent: the ghost is probed, fails, is suspected and dies again within
// about DeadAfter probe intervals, at the cost of one extra tombstone cycle.
//
// Relayed tombstones for members we have already removed are not re-inserted
// (handleMembershipDelta), which is what stops two nodes that collected a death
// a few seconds apart from handing it back to each other forever.
//
// The clock starts when THIS node first sees the death, whether first-hand or
// relayed: there is no wall-clock death time on the wire, and a node that
// joined late has no reason to keep the record shorter than anyone else.
func (n *Node) sweepTombstones(now time.Time) bool {
	if v := n.table.Version(); v != n.tombVersion {
		n.tombVersion = v
		view := n.table.Snapshot()
		dead := make(map[protocol.NodeID]int64)
		for _, m := range view.Members {
			if m.State == StateDead && m.ID != n.cfg.Self {
				dead[m.ID] = m.Incarnation
			}
		}
		for id, inc := range dead {
			// A newer death (the member restarted and died again) restarts
			// the clock: it is a new record for gossip to carry.
			if ts, ok := n.tombstones[id]; !ok || ts.incarnation != inc {
				n.tombstones[id] = tombstone{since: now, incarnation: inc}
			}
		}
		for id := range n.tombstones {
			if _, ok := dead[id]; !ok {
				delete(n.tombstones, id) // refuted, restarted, or already gone
			}
		}
	}
	removed := false
	for id, ts := range n.tombstones {
		if now.Sub(ts.since) < n.cfg.TombstoneTTL {
			continue
		}
		delete(n.tombstones, id)
		if m, ok := n.table.Snapshot().Get(id); ok && m.State == StateDead && m.Incarnation == ts.incarnation {
			n.table.Remove(id)
			// The per-peer bookkeeping that outlives a death goes with it. The
			// address is forgotten too, so if the member really comes back
			// under a new container, learning its address dials it again.
			delete(n.claims, id)
			if m.Addr != n.cfg.Advertise {
				delete(n.dialed, m.Addr)
			}
			n.log.Info("tombstone collected", "peer", id, "incarnation", m.Incarnation)
			removed = true
		}
	}
	return removed
}
