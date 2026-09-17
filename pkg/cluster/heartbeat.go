package cluster

import (
	"context"
	"errors"
	"log/slog"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// Heartbeats: the leader-to-worker liveness channel.
//
// # Why heartbeats when there are already probes
//
// A probe measures a link and is answered on the peer's reader goroutine. A
// beat is sent by the leader's event loop, so a worker that hears beats knows
// the loop that assigns its work and replicates its state is running. It is
// also the only traffic that tells a worker whether the leader still counts it
// as attached: a leader beats exactly its attached set, so a worker the leader
// has forgotten goes quiet and fails over, instead of waiting forever.
//
// # Terms
//
// Each node's Term counts the leader-set changes it has observed, so two nodes'
// terms are not directly comparable. Over the leader-worker link they are made
// comparable, Lamport-style: whoever sees a higher term (in a beat, an ack, or a
// STATE_SYNC) adopts it. ELECTION_RESULT claims still never move a term; they
// are observability only. With that, a beat carrying a LOWER term than the
// worker's means the leader has not caught up with a leader change the worker
// has seen. That is the split-brain symptom the field exists for: the beat is
// logged and does not count as liveness. The worker still acks it, and the ack
// carries the worker's term, so a healthy leader that was merely behind catches
// up on the next beat instead of losing all its workers.

// beat sends one HEARTBEAT to every attached worker. Loop goroutine only.
func (n *Node) beat(ctx context.Context) {
	workers := n.attachedIDs()
	for id := range n.beatSeq {
		if _, ok := n.attached[id]; !ok {
			delete(n.beatSeq, id)
		}
	}
	for _, w := range workers {
		n.beatSeq[w]++
		env, err := protocol.NewEnvelope(protocol.TypeHeartbeat, n.cfg.Self, w, protocol.HeartbeatPayload{
			Seq:         n.beatSeq[w],
			Term:        n.term,
			LeaderID:    n.cfg.Self,
			ClusterSize: len(workers),
		})
		if err != nil {
			continue
		}
		if err := n.cfg.Transport.Send(ctx, w, env); err != nil {
			// The worker's own silence detector is the recovery path; a failed
			// beat is exactly the event it exists to notice.
			n.log.Log(ctx, sendFailureLevel(err), "HEARTBEAT send failed", "worker", w, "err", err)
		}
	}
}

// checkLeaderSilence is the worker's side of the tick: count a tick without a
// beat, and fail over after HeartbeatMisses of them.
func (n *Node) checkLeaderSilence(ctx context.Context) {
	n.sinceBeat++
	if n.sinceBeat < n.cfg.HeartbeatMisses {
		return
	}
	leader := n.leader
	n.log.Warn("leader silent; failing over", "leader", leader, "ticks", n.sinceBeat)
	n.sinceBeat = 0
	n.markSilent(ctx, leader)
	// Detach whether or not the table changed: the leader may already be a
	// relayed suspect, and we still have first-hand reason to leave it.
	// evaluate skips it (usableLeaders) and joins the best remaining leader.
	n.leader = ""
	n.membershipChanged(ctx)
}

// handleHeartbeat answers a beat and, if it is from our leader and current,
// counts it as liveness.
func (n *Node) handleHeartbeat(ctx context.Context, f inboundFrame, p protocol.HeartbeatPayload) {
	from := f.peer
	current := true
	switch {
	case p.Term < n.term:
		n.log.Warn("heartbeat from an older term; ignoring as liveness (split-brain symptom)",
			"from", from, "their_term", p.Term, "our_term", n.term)
		current = false
	case p.Term > n.term:
		n.term = p.Term
		n.publish()
	}
	if current {
		if from == n.leader && !n.isLeader() {
			n.sinceBeat = 0
		}
		n.beatResumed(ctx, from)
	}
	// ObservedLeader is who we consider ourselves attached to. If that is not
	// the sender, the sender drops us.
	ack, err := protocol.NewReply(f.env, protocol.TypeHeartbeatAck, n.cfg.Self, protocol.HeartbeatAckPayload{
		Seq:            p.Seq,
		Term:           n.term,
		ObservedLeader: n.leader,
	})
	if err != nil {
		return
	}
	n.sendDelayed(ctx, from, ack)
}

// beatResumed clears a silence suspicion of from: a beat is evidence from the
// sender's own loop, which is exactly what the suspicion doubted. Other
// suspicions (a lost link) are left to their own resolution, because a beat
// processed after a PeerDown may have been queued before the link died.
func (n *Node) beatResumed(ctx context.Context, from protocol.NodeID) {
	s, ok := n.suspects[from]
	if !ok || !s.silent {
		return
	}
	delete(n.suspects, from)
	if n.table.Revive(from, s.incarnation) {
		n.log.Info("heartbeats resumed; suspicion cleared", "peer", from)
		n.membershipChanged(ctx)
	}
}

// handleHeartbeatAck keeps the attached set honest.
func (n *Node) handleHeartbeatAck(from protocol.NodeID, p protocol.HeartbeatAckPayload) {
	changed := false
	if p.Term > n.term {
		n.term = p.Term
		changed = true
	}
	_, attached := n.attached[from]
	switch {
	case p.ObservedLeader != n.cfg.Self && attached:
		// The worker has re-homed (or never completed its JOIN). Beating it
		// further would only produce more of these acks.
		delete(n.attached, from)
		n.log.Info("worker attached elsewhere; dropped", "worker", from, "observed_leader", p.ObservedLeader)
		changed = true
	case p.ObservedLeader == n.cfg.Self && !attached && n.isLeader():
		// The worker still counts us as its leader but we lost it (we stepped
		// down and back up between its beats). Taking it back is the cheap
		// repair; the alternative is its silence detector suspecting a healthy
		// leader. A worker that has moved on says so in its next ack.
		if m, ok := n.table.Snapshot().Get(from); ok && m.State != StateDead && m.Role != RoleLeader {
			n.attached[from] = struct{}{}
			n.log.Info("worker re-attached from its ack", "worker", from)
			changed = true
		}
	}
	if changed {
		n.publish()
	}
}

// sendDelayed sends env to peer after the injected chaos delay, without
// blocking the loop.
//
// The timer is created here, on the loop goroutine, rather than inside the
// goroutine: a FakeClock only fires timers that exist when Advance runs, so a
// test that has let the loop finish handling the beat can advance the clock
// and know the reply is due. The goroutine is bounded by the delay (at most
// 5s, see SetChaosDelay) or by Run's context, and bgWG joins it on shutdown.
// With a 500ms beat that is at most ten parked goroutines per leader.
func (n *Node) sendDelayed(ctx context.Context, peer protocol.NodeID, env *protocol.Envelope) {
	d := n.ChaosDelay()
	if d <= 0 {
		if err := n.cfg.Transport.Send(ctx, peer, env); err != nil {
			n.log.Log(ctx, sendFailureLevel(err), "reply send failed", "peer", peer, "type", env.Type, "err", err)
		}
		return
	}
	timer := n.cfg.Clock.After(d)
	n.bgWG.Add(1)
	go func() {
		defer n.bgWG.Done()
		select {
		case <-timer:
		case <-ctx.Done():
			return
		}
		if err := n.cfg.Transport.Send(ctx, peer, env); err != nil {
			n.log.Log(ctx, sendFailureLevel(err), "delayed reply send failed", "peer", peer, "type", env.Type, "err", err)
		}
	}()
}

// sendFailureLevel logs a missing connection quietly: membership and the
// connection table legitimately disagree for a while.
func sendFailureLevel(err error) slog.Level {
	if errors.Is(err, network.ErrUnknownPeer) {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

// attachedIDs returns the attached set, sorted.
func (n *Node) attachedIDs() []protocol.NodeID {
	out := make([]protocol.NodeID, 0, len(n.attached))
	for id := range n.attached {
		out = append(out, id)
	}
	sortIDs(out)
	return out
}
