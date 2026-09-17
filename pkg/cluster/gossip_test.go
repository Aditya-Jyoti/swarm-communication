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

// ---- anti-entropy over real nodes ----

// phantomUp tells the given nodes that a peer with no Node behind it has
// connected. Sends to it fail with ErrUnknownPeer, like a dial still pending;
// frames "from" it are delivered by calling a node's Handler directly.
func phantomUp(nodes []*meshNode, id protocol.NodeID, incarnation int64) {
	for _, m := range nodes {
		m.ep.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: id, Advertise: addrOf(id), Incarnation: incarnation}}
	}
}

func phantomDown(m *meshNode, id protocol.NodeID) {
	m.ep.events <- network.PeerEvent{Kind: network.PeerDown, Peer: network.PeerInfo{ID: id, Advertise: addrOf(id)}, Disposition: network.DispositionPeerDied}
}

func memberOf(t *testing.T, m *meshNode, id protocol.NodeID) Member {
	t.Helper()
	mem, ok := m.node.Status().View.Get(id)
	if !ok {
		t.Fatalf("%s has no record of %s", m.id, id)
	}
	return mem
}

func dropDeltaBroadcasts(from, to protocol.NodeID) func(protocol.NodeID, protocol.NodeID, *protocol.Envelope, bool) bool {
	return func(f, tt protocol.NodeID, env *protocol.Envelope, broadcast bool) bool {
		return broadcast && f == from && tt == to && env.Type == protocol.TypeMembershipDelta
	}
}

// n1 sees n3 die and pushes it; the push to n2 is lost. n2 keeps believing n3
// is alive until n1's next anti-entropy round delivers the full view.
func TestDroppedPushIsRepairedByAntiEntropy(t *testing.T) {
	ctx, hub, nodes := newMesh(t, ids("n1", "n2"), nil, nil)
	n1, n2 := nodes[0], nodes[1]
	fullMesh(t, ctx, nodes)
	phantomUp(nodes, "n3", 1)
	quiesce(t, ctx, nodes)

	hub.setDrop(dropDeltaBroadcasts("n1", "n2"))
	phantomDown(n1, "n3")
	quiesce(t, ctx, nodes)
	if m := memberOf(t, n1, "n3"); m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("n1: n3 = %+v, want dead at 2", m)
	}
	if m := memberOf(t, n2, "n3"); m.State != StateAlive {
		t.Fatalf("n2 learned of the death despite the dropped push: %+v", m)
	}

	// One gossip round at n1. Its only alive peer is n2.
	n1.clock.Advance(gossipEvery)
	quiesce(t, ctx, nodes)
	if m := memberOf(t, n2, "n3"); m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("n2 after anti-entropy: n3 = %+v, want dead at 2", m)
	}
}

// The HIGH-2 scenario under anti-entropy. n3 refuted a rumour ("alive at 2"),
// then died. n2 received the refutation; n1 processed n3's death first and the
// refutation second, and its push of the death to n2 was lost. So the swarm
// holds a live "alive at 2" and a "dead at 2". Before the fix n1 held "dead at
// 1", applied the refutation, and both nodes gossiped n3 back to life forever.
// Now the death wins at n2 on the first round and nothing ever revives it.
func TestDeadRecordConvergesAndStaysDead(t *testing.T) {
	ctx, hub, nodes := newMesh(t, ids("n1", "n2"), nil, nil)
	n1, n2 := nodes[0], nodes[1]
	fullMesh(t, ctx, nodes)
	phantomUp(nodes, "n3", 1)
	quiesce(t, ctx, nodes)

	refutation, err := protocol.NewEnvelope(protocol.TypeMembershipDelta, "n3", "", protocol.MembershipDeltaPayload{
		Members: []protocol.MemberRecord{{ID: "n3", Advertise: addrOf("n3"), Incarnation: 2, Role: "worker", State: "alive"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	n2.node.Handler()("n3", refutation)
	quiesce(t, ctx, nodes)

	hub.setDrop(dropDeltaBroadcasts("n1", "n2"))
	phantomDown(n1, "n3")
	quiesce(t, ctx, nodes) // death processed first...
	n1.node.Handler()("n3", refutation)
	quiesce(t, ctx, nodes) // ...then the queued refutation
	hub.setDrop(nil)

	if m := memberOf(t, n1, "n3"); m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("n1: n3 = %+v, want dead at 2", m)
	}
	if m := memberOf(t, n2, "n3"); m.State != StateAlive || m.Incarnation != 2 {
		t.Fatalf("n2: n3 = %+v, want the stale alive at 2 before gossip", m)
	}

	// Several full cycles on both nodes, in both directions.
	for round := 0; round < 4; round++ {
		advanceMesh(t, ctx, nodes, gossipEvery)
		for _, m := range nodes {
			if mem := memberOf(t, m, "n3"); mem.State != StateDead || mem.Incarnation != 2 {
				t.Fatalf("round %d: %s holds n3 = %+v, want dead at 2", round, m.id, mem)
			}
		}
	}
}

// A lost self-announcement leaves n1 with a wrong role and score for n2.
// Anti-entropy from n2 carries n2's own record, which n1 trusts because
// rec.ID == from, so both are repaired without any announcement getting
// through.
func TestAntiEntropyRepairsSendersOwnRoleAndScore(t *testing.T) {
	local := map[protocol.NodeID]map[protocol.NodeID]float64{
		"n1": {"n2": 10},
		// n2 starts blind; phase 2 lets it measure n1.
	}
	ctx, hub, nodes := newMesh(t, ids("n1", "n2"), local, nil)
	n1, n2 := nodes[0], nodes[1]
	fullMesh(t, ctx, nodes)
	// Every self-announcement from n2 to n1 is lost from here on.
	hub.setDrop(dropDeltaBroadcasts("n2", "n1"))

	// A stale leadership claim from n2, delivered late.
	stale, err := protocol.NewEnvelope(protocol.TypeMembershipDelta, "n2", "n1", protocol.MembershipDeltaPayload{
		Members: []protocol.MemberRecord{{ID: "n2", Advertise: addrOf("n2"), Incarnation: 1, Role: "leader", State: "alive", Score: protocol.UnmeasuredScore}},
	})
	if err != nil {
		t.Fatal(err)
	}
	n1.node.Handler()("n2", stale)
	quiesce(t, ctx, nodes)
	if m := memberOf(t, n1, "n2"); m.Role != RoleLeader {
		t.Fatalf("setup: n1 holds n2 = %+v, want the stale leader claim", m)
	}
	if st := n2.node.Status(); st.Role != RoleWorker {
		t.Fatalf("setup: n2 is %s, want worker", st.Role)
	}

	// Phase 1: n2 gossips. Its only peer is n1, and its view says "worker".
	n2.clock.Advance(gossipEvery)
	quiesce(t, ctx, nodes)
	if m := memberOf(t, n1, "n2"); m.Role != RoleWorker {
		t.Fatalf("n1 still holds n2 as %s after anti-entropy, want worker", m.Role)
	}
	requireIDs(t, "n1 View.Leaders after phase 1", n1.node.Status().View.Leaders(), ids("n1"))

	// Phase 2: n2 can now measure n1, reports 1.0, and elects itself -- and
	// both announcements are lost. Gossip carries them anyway.
	// The probe result is asynchronous, so step to the gossip deadline one
	// probe interval at a time: the round at 3s is applied before the gossip
	// tick at 4s reads the view. (Unstepped, the tick can win the race and ship
	// the pre-probe view -- which the following round would repair anyway.)
	n2.hs.set(addrOf("n1"), 1.0)
	n2.clock.Advance(probeEvery)
	quiesce(t, ctx, nodes)
	n2.clock.Advance(gossipEvery - probeEvery)
	quiesce(t, ctx, nodes)
	if st := n2.node.Status(); st.Role != RoleLeader {
		t.Fatalf("n2 is %s, want leader", st.Role)
	}
	m := memberOf(t, n1, "n2")
	if m.Role != RoleLeader || m.Score != 1.0 {
		t.Fatalf("n1 holds n2 = %+v, want leader reporting 1.0", m)
	}
	for _, mn := range nodes {
		st := mn.node.Status()
		requireIDs(t, string(mn.id)+" Leaders", st.Leaders, ids("n2"))
		requireIDs(t, string(mn.id)+" View.Leaders", st.View.Leaders(), ids("n2"))
	}
}

// Three nodes, but n2 and n3 are never connected to each other and n3's
// announcements cannot reach n2. n2 learns of n3 only from n1's anti-entropy,
// and every view converges on the same membership.
func TestThreeNodesConvergeThroughGossipAlone(t *testing.T) {
	local := map[protocol.NodeID]map[protocol.NodeID]float64{
		"n1": {"n2": 2, "n3": 2},
		"n2": {"n1": 2},
		"n3": {"n1": 2},
	}
	ctx, hub, nodes := newMesh(t, ids("n1", "n2", "n3"), local, nil)
	n1, n2, n3 := nodes[0], nodes[1], nodes[2]
	hub.setDrop(func(from, to protocol.NodeID, _ *protocol.Envelope, _ bool) bool {
		return (from == "n2" && to == "n3") || (from == "n3" && to == "n2")
	})

	// n1-n2 first, so n2's welcome from n1 predates n3 entirely.
	linkUp(n1, n2)
	quiesce(t, ctx, nodes)
	linkUp(n1, n3)
	quiesce(t, ctx, nodes)
	if _, ok := n2.node.Status().View.Get("n3"); ok {
		t.Fatal("setup: n2 learned of n3 before any gossip")
	}

	for round := 0; round < 3; round++ {
		advanceMesh(t, ctx, nodes, gossipEvery)
	}

	type key struct {
		id    protocol.NodeID
		state State
		inc   int64
		score float64
	}
	var want []key
	for _, mem := range n1.node.Status().View.Members {
		want = append(want, key{mem.ID, mem.State, mem.Incarnation, mem.Score})
	}
	if len(want) != 3 {
		t.Fatalf("n1 view has %d members, want 3", len(want))
	}
	for _, m := range nodes {
		st := m.node.Status()
		var got []key
		for _, mem := range st.View.Members {
			got = append(got, key{mem.ID, mem.State, mem.Incarnation, mem.Score})
		}
		if len(got) != len(want) {
			t.Fatalf("%s view = %+v, want %+v", m.id, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s view = %+v, want %+v", m.id, got, want)
			}
		}
		requireIDs(t, string(m.id)+" Leaders", st.Leaders, ids("n1"))
	}
	// n2 knows n3's score only by relay.
	if m := memberOf(t, n2, "n3"); m.Score != 2 {
		t.Fatalf("n2: n3 score = %v, want 2 learned by relay", m.Score)
	}
}
