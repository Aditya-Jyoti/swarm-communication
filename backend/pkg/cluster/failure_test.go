package cluster

import (
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/health"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// hbEvery is the failure-detector tick Phase 4 tests run at: the production
// default, so the arithmetic in the tests matches the documented timings.
const hbEvery = DefaultHeartbeatInterval

// phase4 opts a harness into the production failure detector: a live tick and
// the default miss thresholds (zero means default).
func phase4(c *NodeConfig) {
	c.HeartbeatInterval = hbEvery
	c.SuspectAfter = 0
	c.DeadAfter = 0
}

// step advances the clock one tick at a time, settling after each, so a probe
// round started by one tick is applied before the next tick fires.
func (h *harness) step(d time.Duration) {
	h.t.Helper()
	for elapsed := time.Duration(0); elapsed < d; elapsed += hbEvery {
		h.clock.Advance(hbEvery)
		h.settle()
	}
}

func (h *harness) member(id protocol.NodeID) Member {
	h.t.Helper()
	m, ok := h.status().View.Get(id)
	if !ok {
		h.t.Fatalf("no record of %s", id)
	}
	return m
}

// A lost link makes the peer suspect at once, at its own incarnation, and it
// becomes dead only once SuspicionTimeout has passed without a reconnect.
func TestLinkLossIsConfirmedAfterSuspicionTimeout(t *testing.T) {
	h := newHarness(t, "node-a", phase4)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)

	h.hs.fail(addrOf("node-b"), health.ErrUnreachable) // the link is really gone
	h.peerDown("node-b", network.DispositionPeerDied)
	if m := h.member("node-b"); m.State != StateSuspect || m.Incarnation != 1 {
		t.Fatalf("node-b = %+v, want suspect at 1 (no bump)", m)
	}

	h.step(DefaultSuspicionTimeout - hbEvery)
	if m := h.member("node-b"); m.State != StateSuspect {
		t.Fatalf("node-b = %+v before the timeout, want still suspect", m)
	}
	h.step(hbEvery)
	if m := h.member("node-b"); m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("node-b = %+v at the timeout, want dead at 2", m)
	}
	if st := h.status(); st.View.Size() != 1 {
		t.Fatalf("alive = %d, want 1", st.View.Size())
	}
}

// A reconnect inside the window clears the suspicion: no death, no bump, and
// the old timer does not fire later.
func TestReconnectInsideSuspicionWindowClearsIt(t *testing.T) {
	h := newHarness(t, "node-a", phase4)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	h.peerDown("node-b", network.DispositionTimeout)
	h.step(hbEvery)
	h.peerUp("node-b", 1)
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 1 {
		t.Fatalf("node-b = %+v after reconnect, want alive at 1", m)
	}
	h.step(2 * DefaultSuspicionTimeout)
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 1 {
		t.Fatalf("node-b = %+v long after reconnect, want alive at 1", m)
	}
}

// Consecutive failed probes: SuspectAfter makes the peer suspect, DeadAfter
// makes it dead even when the suspicion timer has not fired.
func TestProbeMissThresholds(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		phase4(c)
		c.SuspicionTimeout = time.Hour // isolate the DeadAfter path
	})
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	h.probeRound()
	h.hs.fail(addrOf("node-b"), health.ErrUnreachable)

	for i := 1; i <= DefaultDeadAfter; i++ {
		h.probeRound()
		m := h.member("node-b")
		want := StateAlive
		switch {
		case i >= DefaultDeadAfter:
			want = StateDead
		case i >= DefaultSuspectAfter:
			want = StateSuspect
		}
		if m.State != want {
			t.Fatalf("after %d misses node-b = %+v, want %s", i, m, want)
		}
		if want != StateDead && m.Incarnation != 1 {
			t.Fatalf("after %d misses incarnation = %d, want 1: suspicion must not bump", i, m.Incarnation)
		}
	}
	if _, ok := h.status().Missed["node-b"]; ok {
		t.Fatal("a dead peer's miss count was kept")
	}
}

// A suspicion raised by missed probes is cleared by the next successful probe,
// which is first-hand evidence of life just like a handshake.
func TestProbeSuccessClearsSuspicion(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		phase4(c)
		c.SuspectAfter = 1
	})
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	h.hs.fail(addrOf("node-b"), health.ErrUnreachable)
	h.probeRound()
	if m := h.member("node-b"); m.State != StateSuspect {
		t.Fatalf("node-b = %+v, want suspect", m)
	}
	h.hs.set(addrOf("node-b"), 1.0)
	h.probeRound()
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 1 {
		t.Fatalf("node-b = %+v, want alive at 1", m)
	}
	h.step(2 * DefaultSuspicionTimeout)
	if m := h.member("node-b"); m.State != StateAlive {
		t.Fatalf("a cleared suspicion was confirmed later: %+v", m)
	}
}

// A probe round that started before the link dropped may still succeed after
// the drop. That success is stale and must not clear the suspicion.
func TestStaleProbeSuccessDoesNotClearSuspicion(t *testing.T) {
	h := newHarness(t, "node-a", phase4)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	release := h.hs.hold(addrOf("node-b"))

	h.clock.Advance(probeEvery)
	h.barrier() // round in flight, held at the gate
	if err := h.node.barrier(h.ctx, func() {
		// Delivered on the loop, deterministically ahead of the round result.
		h.node.handlePeerEvent(h.ctx, network.PeerEvent{
			Kind: network.PeerDown, Peer: network.PeerInfo{ID: "node-b"}, Disposition: network.DispositionPeerDied,
		})
	}); err != nil {
		t.Fatal(err)
	}
	release()
	h.settle()
	if m := h.member("node-b"); m.State != StateSuspect {
		t.Fatalf("node-b = %+v, want still suspect after a stale success", m)
	}
}

// A suspect member that refutes at a higher incarnation is alive, and the
// timer that was running for the old incarnation is dropped, not confirmed.
func TestRefutationResolvesSuspicion(t *testing.T) {
	h := newHarness(t, "node-a", phase4)
	h.peerUp("node-b", 1)
	h.peerDown("node-b", network.DispositionPeerDied)
	h.report("node-b", 2, 1.0)
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 2 {
		t.Fatalf("node-b = %+v, want alive at 2", m)
	}
	h.hs.set(addrOf("node-b"), 1.0)
	h.step(2 * DefaultSuspicionTimeout)
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 2 {
		t.Fatalf("node-b = %+v, want still alive at 2", m)
	}
	if n := len(h.node.suspectsSnapshot(t, h)); n != 0 {
		t.Fatalf("%d suspicions still timed after the refutation", n)
	}
}

// A suspicion relayed by gossip is merged but never timed here: only the
// originator confirms it, so one node's bad link cannot kill a healthy peer
// everywhere.
func TestRelayedSuspicionIsNotConfirmedHere(t *testing.T) {
	h := newHarness(t, "node-a", phase4)
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 1.0)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	// No clock movement below, so no probe round: no first-hand evidence either way.
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-c", Advertise: addrOf("node-c"), Incarnation: 1, Role: "worker", State: "suspect",
	}}})
	if m := h.member("node-c"); m.State != StateSuspect {
		t.Fatalf("relayed suspicion not merged: %+v", m)
	}
	if err := confirmSuspicions(h.ctx, h.node); err != nil {
		t.Fatal(err)
	}
	if m := h.member("node-c"); m.State != StateSuspect || m.Incarnation != 1 {
		t.Fatalf("a relayed suspicion was confirmed: %+v", m)
	}
}

// A first-hand suspicion of our own leader detaches us; a relayed one does not.
func TestWorkerDetachesOnlyFromLeaderItSuspects(t *testing.T) {
	t.Run("relayed", func(t *testing.T) {
		h := threeNodes(t, "node-a")
		h.reportRole("node-b", 1, 1.0, RoleLeader)
		h.frame("node-c", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
			ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 1, Role: "leader", State: "suspect",
		}}})
		st := h.status()
		if st.Leader != "node-b" {
			t.Fatalf("detached on a rumour: %s", st)
		}
		requireIDs(t, "Leaders", st.Leaders, ids("node-b"))
	})
	t.Run("first-hand", func(t *testing.T) {
		h := threeNodes(t, "node-a")
		h.reportRole("node-b", 1, 1.0, RoleLeader)
		joins := len(h.tr.sentOf(protocol.TypeJoinCluster))
		h.peerDown("node-b", network.DispositionPeerDied)
		st := h.status()
		if st.Leader != "" {
			t.Fatalf("still attached to a leader we suspect: %s", st)
		}
		if got := len(h.tr.sentOf(protocol.TypeJoinCluster)); got != joins {
			t.Fatalf("sent %d JOINs with no usable leader", got-joins)
		}
		// Reconnect: the suspicion clears and the worker goes back.
		h.peerUp("node-b", 1)
		if st := h.status(); st.Leader != "node-b" {
			t.Fatalf("did not re-attach after the reconnect: %s", st)
		}
	})
}

// A leader keeps a suspect worker attached: the doubt resolves within the
// window, and dropping it would force a re-JOIN for nothing.
func TestLeaderKeepsSuspectWorkerAttached(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		phase4(c)
		c.SuspectAfter = 1
		c.SuspicionTimeout = time.Hour
	})
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})
	h.probeRound() // node-b unscored: one miss, suspect
	if m := h.member("node-b"); m.State != StateSuspect {
		t.Fatalf("node-b = %+v, want suspect", m)
	}
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))
}

func TestMarkSuspectIgnoresSelfAndStrangers(t *testing.T) {
	h := newHarness(t, "node-a", phase4)
	if err := h.node.barrier(h.ctx, func() {
		if h.node.markSuspect(h.ctx, "node-a", "test", nil) {
			t.Error("suspected self")
		}
		if h.node.markSuspect(h.ctx, "node-q", "test", nil) {
			t.Error("suspected a stranger")
		}
		if len(h.node.suspects) != 0 {
			t.Errorf("suspects = %v", h.node.suspects)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if m := h.member("node-a"); m.State != StateAlive {
		t.Fatalf("self = %+v", m)
	}
}

// ---- tombstone GC ----

const testTombstoneTTL = 5 * time.Second

func withTombstones(c *NodeConfig) {
	phase4(c)
	c.TombstoneTTL = testTombstoneTTL
}

// A dead record is kept for TombstoneTTL from the first tick that sees it, and
// then removed together with the bookkeeping that outlives a death.
func TestTombstoneIsCollectedAfterTTL(t *testing.T) {
	var (
		mu     sync.Mutex
		dialed []protocol.NodeAddress
	)
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		withTombstones(c)
		c.Connect = func(a protocol.NodeAddress) {
			mu.Lock()
			defer mu.Unlock()
			dialed = append(dialed, a)
		}
	})
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeElectionResult, protocol.ElectionResultPayload{Term: 1, Leaders: ids("node-b")})
	h.peerDead("node-b")
	h.step(testTombstoneTTL)
	if m := h.member("node-b"); m.State != StateDead {
		t.Fatalf("node-b = %+v before the TTL, want dead", m)
	}
	h.step(hbEvery)
	st := h.status()
	if _, ok := st.View.Get("node-b"); ok {
		t.Fatal("tombstone not collected after the TTL")
	}
	if _, ok := st.Claims["node-b"]; ok {
		t.Fatal("claim of a collected member kept")
	}

	// A peer that has not collected it yet cannot hand it back.
	h.frame("node-c", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 2, Role: "worker", State: "dead",
	}}})
	if _, ok := h.status().View.Get("node-b"); ok {
		t.Fatal("a relayed tombstone was re-inserted after collection")
	}

	// The documented trade-off: a stale "alive" arriving after collection is
	// believed, and the member is dialled again, because its address was
	// forgotten with it.
	h.report("node-b", 1, 1.0)
	if m := h.member("node-b"); m.State != StateAlive {
		t.Fatalf("stale alive after collection = %+v", m)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != addrOf("node-b") {
		t.Fatalf("dialed = %v, want node-b's address once", dialed)
	}
}

// A member that refutes its death before the TTL is not collected.
func TestRefutedTombstoneIsNotCollected(t *testing.T) {
	h := newHarness(t, "node-a", withTombstones)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	h.peerDead("node-b")
	h.step(hbEvery) // the sweep sees the death
	h.report("node-b", 3, 1.0)
	h.step(2 * testTombstoneTTL)
	if m := h.member("node-b"); m.State != StateAlive || m.Incarnation != 3 {
		t.Fatalf("node-b = %+v, want alive at 3", m)
	}
}

// A second death (the member came back and died again) restarts the clock.
func TestNewerDeathRestartsTombstoneClock(t *testing.T) {
	h := newHarness(t, "node-a", withTombstones)
	h.peerUp("node-b", 1)
	h.peerDead("node-b") // dead at 2
	h.step(testTombstoneTTL - 2*hbEvery)
	h.peerUp("node-b", 5) // restarted
	h.peerDead("node-b")  // dead at 6
	h.step(testTombstoneTTL)
	if m := h.member("node-b"); m.State != StateDead || m.Incarnation != 6 {
		t.Fatalf("node-b = %+v, want still dead at 6", m)
	}
	h.step(2 * hbEvery)
	if _, ok := h.status().View.Get("node-b"); ok {
		t.Fatal("second tombstone not collected")
	}
}

// suspectsSnapshot copies the node's first-hand suspicion set on its loop.
func (n *Node) suspectsSnapshot(t *testing.T, h *harness) map[protocol.NodeID]suspicion {
	t.Helper()
	out := make(map[protocol.NodeID]suspicion)
	if err := n.barrier(h.ctx, func() {
		for k, v := range n.suspects {
			out[k] = v
		}
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
