package cluster

import (
	"testing"
	"time"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// ---- leader side ----

func (h *harness) beats() []protocol.HeartbeatPayload {
	h.t.Helper()
	var out []protocol.HeartbeatPayload
	for _, s := range h.tr.sentOf(protocol.TypeHeartbeat) {
		p, err := protocol.PayloadOf[protocol.HeartbeatPayload](s.env)
		if err != nil {
			h.t.Fatal(err)
		}
		if s.to != s.env.To {
			h.t.Fatalf("beat addressed to %s sent to %s", s.env.To, s.to)
		}
		out = append(out, p)
	}
	return out
}

func beatTargets(h *harness) []protocol.NodeID {
	var out []protocol.NodeID
	for _, s := range h.tr.sentOf(protocol.TypeHeartbeat) {
		out = append(out, s.to)
	}
	return out
}

// leaderWith builds node-a, leading alone, with the given workers attached.
func leaderWith(t *testing.T, workers ...protocol.NodeID) *harness {
	t.Helper()
	h := newHarness(t, "node-a", phase4)
	for _, w := range workers {
		h.peerUp(w, 1)
		h.frame(w, protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: w})
	}
	requireIDs(t, "Attached", h.status().Attached, workers)
	return h
}

// A leader beats every attached worker once per interval, with a per-worker
// sequence, its term, its ID and its cluster size.
func TestLeaderBeatsEachAttachedWorker(t *testing.T) {
	h := leaderWith(t, "node-b", "node-c")
	term := h.status().Term
	h.step(hbEvery)
	h.step(hbEvery)
	requireIDs(t, "targets", beatTargets(h), ids("node-b", "node-c", "node-b", "node-c"))
	for i, p := range h.beats() {
		wantSeq := uint64(i/2 + 1)
		if p.Seq != wantSeq || p.Term != term || p.LeaderID != "node-a" || p.ClusterSize != 2 {
			t.Fatalf("beat %d = %+v, want seq %d term %d from node-a size 2", i, p, wantSeq, term)
		}
	}
}

// A worker that acks with another leader is dropped and no longer beaten.
func TestLeaderDropsWorkerAttachedElsewhere(t *testing.T) {
	h := leaderWith(t, "node-b", "node-c")
	h.frame("node-c", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{Seq: 1, ObservedLeader: "node-q"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))
	h.step(hbEvery)
	requireIDs(t, "targets", beatTargets(h), ids("node-b"))
	if err := h.node.barrier(h.ctx, func() {
		if _, ok := h.node.beatSeq["node-c"]; ok {
			t.Error("beat sequence kept for a dropped worker")
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// A worker that still names us, but that we lost, is taken back; a dead one,
// or one that now claims leadership, is not.
func TestLeaderReattachesWorkerThatStillNamesIt(t *testing.T) {
	h := leaderWith(t)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.peerUp("node-d", 1)
	h.frame("node-b", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{ObservedLeader: "node-a"})
	if st := h.status(); !equalIDs(st.Attached, ids("node-b")) {
		t.Fatalf("Attached = %v in %s", st.Attached, st)
	}

	h.peerDead("node-c")
	h.frame("node-c", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{ObservedLeader: "node-a"})
	// Unmeasured, so the claim does not win node-d the election here.
	h.deltaAbout("node-d", "node-d", RoleLeader, protocol.UnmeasuredScore, 5)
	requireIDs(t, "Leaders", h.status().Leaders, ids("node-a"))
	h.frame("node-d", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{ObservedLeader: "node-a"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))
}

// A leader adopts a higher term from an ack: that is how a leader that missed
// a leader change catches up.
func TestLeaderAdoptsHigherTermFromAck(t *testing.T) {
	h := leaderWith(t, "node-b")
	h.frame("node-b", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{Term: 40, ObservedLeader: "node-a"})
	if got := h.status().Term; got != 40 {
		t.Fatalf("Term = %d, want 40", got)
	}
	h.step(hbEvery)
	if b := h.beats(); len(b) != 1 || b[0].Term != 40 {
		t.Fatalf("beats = %+v, want term 40", b)
	}
	// A lower term changes nothing.
	h.frame("node-b", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{Term: 3, ObservedLeader: "node-a"})
	if got := h.status().Term; got != 40 {
		t.Fatalf("Term = %d after a lower ack, want 40", got)
	}
}

// Send failures, immediate or delayed, are logged and survived: the silence
// detector on the other end is the recovery path.
func TestBeatAndReplySendFailuresAreSurvived(t *testing.T) {
	h := leaderWith(t, "node-b", "node-c")
	h.tr.failSend("node-b", network.ErrUnknownPeer)
	h.tr.failSend("node-c", network.ErrSendQueueFull)
	h.step(hbEvery)
	requireIDs(t, "Attached", h.status().Attached, ids("node-b", "node-c"))

	w := attachedWorker(t)
	w.tr.failSend("node-b", network.ErrSendQueueFull)
	w.beatFrom("node-b", 1, w.status().Term)
	w.node.SetChaosDelay(time.Millisecond)
	w.beatFrom("node-b", 2, w.status().Term)
	w.clock.Advance(time.Millisecond)
	w.stop() // joins the delayed sender, whose send failed
	if n := len(w.acks()); n != 0 {
		t.Fatalf("acks recorded despite failures: %d", n)
	}
}

func TestWorkerDoesNotBeat(t *testing.T) {
	h := attachedWorker(t)
	h.step(4 * hbEvery)
	if got := h.tr.sentOf(protocol.TypeHeartbeat); len(got) != 0 {
		t.Fatalf("a worker sent beats: %+v", got)
	}
}

// ---- worker side ----

// attachedWorker builds node-z attached to node-b. Four nodes at threshold 0.5
// elect node-b and node-c; node-b is closer.
func attachedWorker(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "node-z", func(c *NodeConfig) {
		phase4(c)
		c.Election.Threshold = 0.5
	})
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 1.5)
	h.hs.set(addrOf("node-d"), 10.0)
	for _, id := range ids("node-b", "node-c", "node-d") {
		h.peerUp(id, 1)
	}
	h.deltaAbout("node-b", "node-b", RoleLeader, 1.0, 5)
	h.deltaAbout("node-c", "node-c", RoleLeader, 1.0, 5)
	h.deltaAbout("node-d", "node-d", RoleWorker, 10, 5)
	h.probeRound()
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	st := h.status()
	requireIDs(t, "Leaders", st.Leaders, ids("node-b", "node-c"))
	if st.Leader != "node-b" {
		t.Fatalf("setup: %s, want attached to node-b", st)
	}
	return h
}

func (h *harness) beatFrom(from protocol.NodeID, seq, term uint64) {
	h.t.Helper()
	h.frame(from, protocol.TypeHeartbeat, protocol.HeartbeatPayload{Seq: seq, Term: term, LeaderID: from, ClusterSize: 1})
}

func (h *harness) acks() []sentAck {
	h.t.Helper()
	var out []sentAck
	for _, s := range h.tr.sentOf(protocol.TypeHeartbeatAck) {
		p, err := protocol.PayloadOf[protocol.HeartbeatAckPayload](s.env)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, sentAck{to: s.to, id: s.env.ID, p: p})
	}
	return out
}

type sentAck struct {
	to protocol.NodeID
	id string
	p  protocol.HeartbeatAckPayload
}

// A worker acks every beat, echoing Seq and the correlation ID, with its term
// and the leader it considers itself attached to.
func TestWorkerAcksBeats(t *testing.T) {
	h := attachedWorker(t)
	term := h.status().Term
	env := h.frame("node-b", protocol.TypeHeartbeat, protocol.HeartbeatPayload{Seq: 7, Term: term, LeaderID: "node-b"})
	acks := h.acks()
	if len(acks) != 1 {
		t.Fatalf("acks = %+v", acks)
	}
	a := acks[0]
	if a.to != "node-b" || a.id != env.ID || a.p.Seq != 7 || a.p.Term != term || a.p.ObservedLeader != "node-b" {
		t.Fatalf("ack = %+v", a)
	}
	// A beat from a leader we are not attached to is answered with the one we are.
	h.beatFrom("node-c", 1, term)
	if a := h.acks()[1]; a.to != "node-c" || a.p.ObservedLeader != "node-b" {
		t.Fatalf("ack to a stranger leader = %+v", a)
	}
}

// Beats every interval keep the worker attached indefinitely.
func TestWorkerStaysWhileBeatsArrive(t *testing.T) {
	h := attachedWorker(t)
	for i := uint64(1); i <= 20; i++ {
		h.step(hbEvery)
		h.beatFrom("node-b", i, h.status().Term)
	}
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("failed over despite beats: %s", st)
	}
	if m := h.member("node-b"); m.State != StateAlive {
		t.Fatalf("node-b = %+v", m)
	}
}

// HeartbeatMisses ticks without a beat: the worker suspects its leader, leaves
// it, and joins the best remaining one. Beats from other leaders do not count.
func TestWorkerFailsOverAfterMissedBeats(t *testing.T) {
	h := attachedWorker(t)
	joins := len(h.tr.sentOf(protocol.TypeJoinCluster))
	for i := 1; i < DefaultHeartbeatMisses; i++ {
		h.step(hbEvery)
		h.beatFrom("node-c", uint64(i), h.status().Term)
		if st := h.status(); st.Leader != "node-b" {
			t.Fatalf("failed over after %d silent ticks: %s", i, st)
		}
	}
	h.step(hbEvery)
	st := h.status()
	if m := h.member("node-b"); m.State != StateSuspect || m.Incarnation != 1 {
		t.Fatalf("node-b = %+v, want suspect at 1", m)
	}
	requireIDs(t, "Leaders", st.Leaders, ids("node-b", "node-c")) // the seat is kept
	got := h.tr.sentOf(protocol.TypeJoinCluster)
	if st.Leader != "node-c" || len(got) != joins+1 || got[joins].to != "node-c" {
		t.Fatalf("after silence: %s, JOINs %+v", st, got[joins:])
	}
}

// A beat from an older term is a split-brain symptom: it is not liveness, so
// the worker still fails over, but it is acked with the newer term.
func TestStaleTermBeatIsNotLiveness(t *testing.T) {
	h := attachedWorker(t)
	term := h.status().Term
	if term == 0 {
		t.Fatal("setup: term 0")
	}
	for i := 1; i <= DefaultHeartbeatMisses; i++ {
		h.step(hbEvery)
		h.beatFrom("node-b", uint64(i), term-1)
	}
	if st := h.status(); st.Leader == "node-b" {
		t.Fatalf("stale-term beats kept the worker attached: %s", st)
	}
	for _, a := range h.acks() {
		if a.p.Term < term {
			t.Fatalf("ack carried term %d, want >= %d", a.p.Term, term)
		}
	}
	if n := len(h.acks()); n != DefaultHeartbeatMisses {
		t.Fatalf("acks = %d, want one per stale beat", n)
	}
}

// A newer term in a beat is adopted.
func TestWorkerAdoptsHigherTermFromBeat(t *testing.T) {
	h := attachedWorker(t)
	h.beatFrom("node-b", 1, 99)
	if got := h.status().Term; got != 99 {
		t.Fatalf("Term = %d, want 99", got)
	}
}

// A silence suspicion is not cleared by a successful probe (a wedged loop
// still answers PINGs), but it is cleared by a beat, and the worker goes back.
func TestSilenceSuspicionNeedsABeatToClear(t *testing.T) {
	h := attachedWorker(t)
	h.step(time.Duration(DefaultHeartbeatMisses) * hbEvery)
	if m := h.member("node-b"); m.State != StateSuspect {
		t.Fatalf("setup: node-b = %+v", m)
	}
	h.probeRound() // node-b's probe succeeds
	h.probeRound()
	if m := h.member("node-b"); m.State != StateSuspect {
		t.Fatalf("a probe cleared a silence suspicion: %+v", m)
	}
	h.beatFrom("node-b", 1, h.status().Term)
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 1 {
		t.Fatalf("a resumed beat did not clear it: %+v", m)
	}
}

// ---- chaos delay ----

func TestSetChaosDelayClampsAndReports(t *testing.T) {
	h := attachedWorker(t)
	for _, tt := range []struct{ in, want time.Duration }{
		{300 * time.Millisecond, 300 * time.Millisecond},
		{-time.Second, 0},
		{time.Hour, MaxChaosDelay},
		{0, 0},
	} {
		h.node.SetChaosDelay(tt.in)
		if got := h.status().ChaosDelay; got != tt.want {
			t.Fatalf("SetChaosDelay(%v): Status.ChaosDelay = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// A delayed ack leaves after exactly the delay on the Clock, and the loop is
// not blocked meanwhile.
func TestChaosDelayDefersTheAck(t *testing.T) {
	h := attachedWorker(t)
	const d = 300 * time.Millisecond
	h.node.SetChaosDelay(d)
	sends := h.tr.watch()
	h.beatFrom("node-b", 1, h.status().Term)
	if n := len(h.acks()); n != 0 {
		t.Fatalf("ack sent before the delay: %d", n)
	}
	// The loop is free: another frame is handled while the ack waits.
	h.beatFrom("node-b", 2, h.status().Term)
	h.clock.Advance(d - time.Millisecond)
	h.settle()
	if n := len(h.acks()); n != 0 {
		t.Fatalf("ack sent before the delay: %d", n)
	}
	h.clock.Advance(time.Millisecond)
	for i := 0; i < 2; i++ {
		select {
		case s := <-sends:
			if s.env.Type != protocol.TypeHeartbeatAck {
				i--
			}
		case <-time.After(5 * time.Second):
			t.Fatal("delayed ack never sent")
		}
	}
	if n := len(h.acks()); n != 2 {
		t.Fatalf("acks = %d, want 2", n)
	}
}

// Shutdown does not wait out a pending delay, and the reply is not sent.
func TestShutdownAbandonsDelayedReplies(t *testing.T) {
	h := attachedWorker(t)
	h.node.SetChaosDelay(MaxChaosDelay)
	h.beatFrom("node-b", 1, h.status().Term)
	h.stop()
	if n := len(h.acks()); n != 0 {
		t.Fatalf("a delayed ack was sent during shutdown: %d", n)
	}
}
