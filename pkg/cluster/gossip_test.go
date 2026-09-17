package cluster

import (
	"sync"
	"testing"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// ---- anti-entropy peer selection (single node, fake transport) ----

// viewSends returns the MEMBERSHIP_DELTA sends after index from, as targets.
func viewSends(h *harness, from int) []protocol.NodeID {
	var out []protocol.NodeID
	for _, s := range h.tr.sentOf(protocol.TypeMembershipDelta)[from:] {
		out = append(out, s.to)
	}
	return out
}

// gossipTick fires one gossip round (and the probe ticks on the way) and waits
// for the loop to finish with it.
func (h *harness) gossipTick() {
	h.t.Helper()
	h.clock.Advance(gossipEvery)
	h.settle()
}

// countingShuffle reverses the order, so a test can tell a shuffled order from
// the ID-sorted one, and counts redraws.
type countingShuffle struct {
	mu    sync.Mutex
	calls int
}

func (c *countingShuffle) shuffle(ids []protocol.NodeID) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
}

func (c *countingShuffle) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestGossipVisitsEachPeerOncePerCycle(t *testing.T) {
	sh := &countingShuffle{}
	h := newHarness(t, "node-a", func(c *NodeConfig) { c.Shuffle = sh.shuffle })
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.peerUp("node-d", 1)
	base := len(h.tr.sentOf(protocol.TypeMembershipDelta)) // the PeerUp welcomes

	for i := 0; i < 3; i++ {
		h.gossipTick()
	}
	// The permutation is applied: reversed ID order, one peer per round.
	requireIDs(t, "first cycle", viewSends(h, base), ids("node-d", "node-c", "node-b"))
	if got := sh.count(); got != 1 {
		t.Fatalf("shuffles after one cycle = %d, want 1", got)
	}

	// The payload is the full view, self included.
	last := h.tr.sentOf(protocol.TypeMembershipDelta)
	p, err := protocol.PayloadOf[protocol.MembershipDeltaPayload](last[len(last)-1].env)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Members) != 4 {
		t.Fatalf("gossip carried %d records, want the full view of 4", len(p.Members))
	}

	// The cursor wraps: a fresh permutation, every peer once again.
	for i := 0; i < 3; i++ {
		h.gossipTick()
	}
	requireIDs(t, "second cycle", viewSends(h, base+3), ids("node-d", "node-c", "node-b"))
	if got := sh.count(); got != 2 {
		t.Fatalf("shuffles after two cycles = %d, want 2", got)
	}
}

// A table change that leaves the alive set alone (here, a score report) must
// not restart the cycle; one that changes the set must.
func TestGossipRedrawsOnlyWhenAlivePeersChange(t *testing.T) {
	sh := &countingShuffle{}
	h := newHarness(t, "node-a", func(c *NodeConfig) { c.Shuffle = sh.shuffle })
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	base := len(h.tr.sentOf(protocol.TypeMembershipDelta))

	h.gossipTick() // node-c
	h.report("node-b", 1, 0.5)
	h.gossipTick() // node-b: same cycle, despite the version bump
	requireIDs(t, "cycle across a score report", viewSends(h, base), ids("node-c", "node-b"))
	if got := sh.count(); got != 1 {
		t.Fatalf("shuffles = %d, want 1: a score report redrew the order", got)
	}

	// Mid-cycle membership change: node-d joins, node-b dies. The order is
	// redrawn over the new set, so node-d is reached and node-b is not.
	h.gossipTick() // wraps: node-c
	h.peerUp("node-d", 1)
	h.peerDown("node-b", network.DispositionPeerDied)
	mark := len(h.tr.sentOf(protocol.TypeMembershipDelta)) // PeerUp welcome to node-d
	h.gossipTick()
	h.gossipTick()
	requireIDs(t, "after membership change", viewSends(h, mark), ids("node-d", "node-c"))
	if got := sh.count(); got != 3 {
		t.Fatalf("shuffles = %d, want 3", got)
	}
}

func TestGossipWithNoPeersSendsNothing(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.gossipTick()
	h.peerUp("node-b", 1)
	h.peerDown("node-b", network.DispositionPeerDied)
	base := len(h.tr.sentOf(protocol.TypeMembershipDelta))
	h.gossipTick()
	h.gossipTick()
	if got := viewSends(h, base); len(got) != 0 {
		t.Fatalf("gossip went to %v with no alive peers", got)
	}
}

// A send to a member we have no connection to is expected (gossip learns
// members before the pool dials them) and must not stop the rotation.
func TestGossipSurvivesSendFailure(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.tr.failSend("node-b", network.ErrUnknownPeer)
	h.tr.failSend("node-c", network.ErrSendQueueFull)
	h.gossipTick()
	h.gossipTick()
	h.tr.mu.Lock()
	delete(h.tr.sendErr, "node-b")
	h.tr.mu.Unlock()
	base := len(h.tr.sentOf(protocol.TypeMembershipDelta))
	h.gossipTick()
	requireIDs(t, "after failures", viewSends(h, base), ids("node-b"))
}

// ---- push-on-change ----

func deltaRecords(t *testing.T, env *protocol.Envelope) []protocol.MemberRecord {
	t.Helper()
	p, err := protocol.PayloadOf[protocol.MembershipDeltaPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	return p.Members
}

func TestFirstHandDeathIsPushed(t *testing.T) {
	for _, tt := range []struct {
		name string
		kill func(h *harness)
	}{
		{"link lost", func(h *harness) { h.peerDown("node-b", network.DispositionPeerDied) }},
		{"clean close", func(h *harness) { h.peerDown("node-b", network.DispositionCleanClose) }},
		{"LEAVE", func(h *harness) { h.frame("node-b", protocol.TypeLeave, protocol.LeavePayload{}) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, "node-a", nil)
			h.peerUp("node-b", 1)
			h.peerUp("node-c", 1)
			base := len(h.tr.broadcastOf(protocol.TypeMembershipDelta))

			tt.kill(h)
			pushed := h.tr.broadcastOf(protocol.TypeMembershipDelta)[base:]
			if len(pushed) != 1 {
				t.Fatalf("delta broadcasts after the death = %d, want 1", len(pushed))
			}
			recs := deltaRecords(t, pushed[0])
			if len(recs) != 1 || recs[0].ID != "node-b" || recs[0].State != "dead" || recs[0].Incarnation != 2 {
				t.Fatalf("pushed %+v, want node-b dead at 2", recs)
			}

			// A repeat of the same death changes nothing and pushes nothing.
			h.peerDown("node-b", network.DispositionPeerDied)
			if n := len(h.tr.broadcastOf(protocol.TypeMembershipDelta)); n != base+1 {
				t.Fatalf("a no-op death was pushed: %d broadcasts", n-base)
			}
		})
	}
}

// A death learned from a peer is not re-pushed: that is what keeps a change
// from echoing around a full mesh.
func TestRelayedDeathIsNotPushed(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	base := len(h.tr.broadcastOf(protocol.TypeMembershipDelta))
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-c", Advertise: addrOf("node-c"), Incarnation: 2, Role: "worker", State: "dead",
	}}})
	if m, _ := h.status().View.Get("node-c"); m.State != StateDead {
		t.Fatalf("relayed death not applied: %+v", m)
	}
	if n := len(h.tr.broadcastOf(protocol.TypeMembershipDelta)); n != base {
		t.Fatalf("a relayed death was re-pushed: %d broadcasts", n-base)
	}
}
