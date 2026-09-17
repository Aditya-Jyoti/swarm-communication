package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarm-net/pkg/health"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// ---- fakes ----

type sentEnv struct {
	to  protocol.NodeID
	env *protocol.Envelope
}

// fakeTransport records everything the node sends and lets the test inject
// PeerEvents. Sends never block: the loop under test must never be parked on a
// test fixture, or a hang would be indistinguishable from a bug.
type fakeTransport struct {
	self   protocol.NodeID
	events chan network.PeerEvent

	mu        sync.Mutex
	sent      []sentEnv
	broadcast []*protocol.Envelope
	peers     map[protocol.NodeID]network.PeerInfo
	sendErr   map[protocol.NodeID]error
}

func newFakeTransport(self protocol.NodeID) *fakeTransport {
	return &fakeTransport{
		self:    self,
		events:  make(chan network.PeerEvent, 64),
		peers:   make(map[protocol.NodeID]network.PeerInfo),
		sendErr: make(map[protocol.NodeID]error),
	}
}

func (f *fakeTransport) Self() protocol.NodeID { return f.self }

func (f *fakeTransport) Send(_ context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.sendErr[to]; ok {
		return err
	}
	f.sent = append(f.sent, sentEnv{to: to, env: env})
	return nil
}

func (f *fakeTransport) Broadcast(_ context.Context, env *protocol.Envelope) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcast = append(f.broadcast, env)
	return len(f.peers)
}

func (f *fakeTransport) Peers() []network.PeerInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]network.PeerInfo, 0, len(f.peers))
	for _, p := range f.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (f *fakeTransport) Events() <-chan network.PeerEvent { return f.events }

func (f *fakeTransport) sentOf(t protocol.MessageType) []sentEnv {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sentEnv
	for _, s := range f.sent {
		if s.env.Type == t {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeTransport) broadcastOf(t protocol.MessageType) []*protocol.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*protocol.Envelope
	for _, e := range f.broadcast {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeTransport) failSend(to protocol.NodeID, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr[to] = err
}

// fakeHealth scores from a map, with per-target error injection. A target in
// block parks until ctx ends and then reports cancellation, which is what a real
// strategy does when the round is cancelled under it.
type fakeHealth struct {
	mu     sync.Mutex
	scores map[protocol.NodeAddress]float64
	errs   map[protocol.NodeAddress]error
	block  map[protocol.NodeAddress]bool
	// gate holds a probe of the target until the channel is closed, then lets it
	// report its configured result. Unlike block, the outcome is a real sample.
	gate     map[protocol.NodeAddress]chan struct{}
	retained []map[protocol.NodeAddress]struct{}
	calls    int
}

func newFakeHealth() *fakeHealth {
	return &fakeHealth{
		scores: make(map[protocol.NodeAddress]float64),
		errs:   make(map[protocol.NodeAddress]error),
		block:  make(map[protocol.NodeAddress]bool),
		gate:   make(map[protocol.NodeAddress]chan struct{}),
	}
}

func (h *fakeHealth) Name() string { return "fake" }

func (h *fakeHealth) EvaluateScore(ctx context.Context, target protocol.NodeAddress) (float64, error) {
	h.mu.Lock()
	h.calls++
	blocked := h.block[target]
	gate := h.gate[target]
	h.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return health.ScoreUnavailable(), fmt.Errorf("%w: %w", health.ErrProbeCanceled, ctx.Err())
		}
	}
	h.mu.Lock()
	err := h.errs[target]
	score, ok := h.scores[target]
	h.mu.Unlock()

	if blocked {
		<-ctx.Done()
		return health.ScoreUnavailable(), fmt.Errorf("%w: %w", health.ErrProbeCanceled, ctx.Err())
	}
	if err != nil {
		return health.ScoreUnavailable(), err
	}
	if !ok {
		return health.ScoreUnavailable(), health.ErrUnreachable
	}
	return score, nil
}

func (h *fakeHealth) Retain(keep map[protocol.NodeAddress]struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retained = append(h.retained, keep)
}

func (h *fakeHealth) set(addr protocol.NodeAddress, score float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scores[addr] = score
	delete(h.errs, addr)
}

func (h *fakeHealth) fail(addr protocol.NodeAddress, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs[addr] = err
}

func (h *fakeHealth) stall(addr protocol.NodeAddress) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.block[addr] = true
}

// hold gates probes of addr until the returned func is called.
func (h *fakeHealth) hold(addr protocol.NodeAddress) (release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{})
	h.gate[addr] = ch
	return func() { close(ch) }
}

func (h *fakeHealth) lastRetained() map[protocol.NodeAddress]struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.retained) == 0 {
		return nil
	}
	return h.retained[len(h.retained)-1]
}

// ---- harness ----

const (
	probeEvery  = time.Second
	floorEvery  = 30 * time.Second
	gossipEvery = 2 * time.Second
)

// keepOrder is the test Shuffle: it leaves the (ID-sorted) order alone, so which
// peer a gossip round targets is deterministic.
func keepOrder([]protocol.NodeID) {}

type harness struct {
	t      *testing.T
	node   *Node
	tr     *fakeTransport
	hs     *fakeHealth
	clock  *FakeClock
	ctx    context.Context
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func addrOf(id protocol.NodeID) protocol.NodeAddress {
	return protocol.NodeAddress(string(id) + ":7000")
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func baseConfig(self protocol.NodeID, tr *fakeTransport, hs *fakeHealth, clock *FakeClock) NodeConfig {
	return NodeConfig{
		Self:           self,
		Advertise:      addrOf(self),
		Transport:      tr,
		Health:         hs,
		Clock:          clock,
		ProbeInterval:  probeEvery,
		ElectionFloor:  floorEvery,
		GossipInterval: gossipEvery,
		Shuffle:        keepOrder,
		ProbeTimeout:   probeEvery / 2,
		Election:       Config{Threshold: DefaultThreshold, Hysteresis: 0.1},
		Rehome:         0.1,
		Logger:         quietLogger(),
		// Phase 3 tests advance the clock by probe and gossip periods and count
		// what was sent. A 500ms failure-detector tick would add heartbeats and
		// missed-beat failovers to every one of them, so the shared config
		// parks it; Phase 4 tests opt in with hbEvery.
		HeartbeatInterval: parkedTick,
		// Likewise, many Phase 3 tests leave peers unscored, so every probe of
		// them fails; with the production thresholds those peers would die
		// part-way through tests that are about something else.
		SuspectAfter: parkedMisses,
		DeadAfter:    parkedMisses,
	}
}

// parkedTick is a tick period no test ever advances to, and parkedMisses a
// probe-miss count no test ever reaches.
const (
	parkedTick   = 1000 * time.Hour
	parkedMisses = 1 << 30
)

// confirmSuspicions runs the failure detector's confirmation step on n's loop
// as if SuspicionTimeout had already elapsed, without firing any other timer.
// Phase 3 tests use it to turn "link lost" into the death they are about.
func confirmSuspicions(ctx context.Context, n *Node) error {
	return n.settle(ctx, func() {
		if n.expireSuspicions(ctx, n.cfg.Clock.Now().Add(n.cfg.SuspicionTimeout)) {
			n.membershipChanged(ctx)
		}
	})
}

// peerDead reports a lost link to id and confirms the resulting suspicion.
func (h *harness) peerDead(id protocol.NodeID) {
	h.t.Helper()
	h.peerDown(id, network.DispositionPeerDied)
	if err := confirmSuspicions(h.ctx, h.node); err != nil {
		h.t.Fatalf("confirm: %v", err)
	}
}

// newHarness starts a node named self and blocks until its loop is running.
func newHarness(t *testing.T, self protocol.NodeID, mutate func(*NodeConfig)) *harness {
	t.Helper()
	tr := newFakeTransport(self)
	hs := newFakeHealth()
	clock := NewFakeClock(epoch)
	cfg := baseConfig(self, tr, hs, clock)
	if mutate != nil {
		mutate(&cfg)
	}
	n, err := NewNode(cfg)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, node: n, tr: tr, hs: hs, clock: clock, ctx: ctx, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- n.Run(ctx) }()
	h.settle()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) settle() {
	h.t.Helper()
	if err := h.node.settle(h.ctx, nil); err != nil {
		h.t.Fatalf("settle: %v", err)
	}
}

// stop cancels the node and waits for Run to return. Idempotent, because tests
// that stop explicitly are also stopped by Cleanup.
// barrier waits for the loop to finish its current event without waiting for
// probes. Use it after Advance when the next step depends on a timer the tick
// registered.
func (h *harness) barrier() {
	h.t.Helper()
	if err := h.node.barrier(h.ctx, nil); err != nil {
		h.t.Fatalf("barrier: %v", err)
	}
}

func (h *harness) stop() {
	h.once.Do(func() {
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				h.t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			h.t.Fatal("Run did not return after cancel")
		}
	})
}

func (h *harness) peerUp(id protocol.NodeID, incarnation int64) {
	h.t.Helper()
	h.tr.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: id, Advertise: addrOf(id), Incarnation: incarnation}}
	h.settle()
}

func (h *harness) peerDown(id protocol.NodeID, d network.Disposition) {
	h.t.Helper()
	h.tr.events <- network.PeerEvent{Kind: network.PeerDown, Peer: network.PeerInfo{ID: id, Advertise: addrOf(id)}, Disposition: d, Err: errors.New("boom")}
	h.settle()
}

// frame delivers an envelope through the node's Handler, exactly as a reader
// goroutine would, and waits for the loop to process it.
func (h *harness) frame(from protocol.NodeID, t protocol.MessageType, payload any) *protocol.Envelope {
	h.t.Helper()
	env, err := protocol.NewEnvelope(t, from, h.node.cfg.Self, payload)
	if err != nil {
		h.t.Fatalf("NewEnvelope: %v", err)
	}
	h.node.Handler()(from, env)
	h.settle()
	return env
}

// report delivers a peer's self-announcement (a MEMBERSHIP_DELTA carrying its own
// record and score), which is how a peer becomes electable.
func (h *harness) report(id protocol.NodeID, incarnation int64, score float64) {
	h.t.Helper()
	h.reportRole(id, incarnation, score, RoleWorker)
}

func (h *harness) reportRole(id protocol.NodeID, incarnation int64, score float64, role Role) {
	h.t.Helper()
	h.frame(id, protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: id, Advertise: addrOf(id), Incarnation: incarnation, Role: role.String(), State: "alive", Score: score,
	}}})
}

// probeRound fires the probe ticker and waits for the round to be applied.
func (h *harness) probeRound() {
	h.t.Helper()
	h.clock.Advance(probeEvery)
	h.settle()
}

func (h *harness) status() Status { return h.node.Status() }

func ids(s ...protocol.NodeID) []protocol.NodeID { return s }

func requireIDs(t *testing.T, what string, got, want []protocol.NodeID) {
	t.Helper()
	if !equalIDs(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// ---- tests ----

func TestNewNodeValidation(t *testing.T) {
	tr := newFakeTransport("node-a")
	hs := newFakeHealth()
	full := NodeConfig{Self: "node-a", Advertise: "node-a:7000", Transport: tr, Health: hs}

	tests := []struct {
		name   string
		mutate func(*NodeConfig)
		want   string
	}{
		{"missing self", func(c *NodeConfig) { c.Self = "" }, "Self"},
		{"missing advertise", func(c *NodeConfig) { c.Advertise = "" }, "Advertise"},
		{"missing transport", func(c *NodeConfig) { c.Transport = nil }, "Transport"},
		{"missing health", func(c *NodeConfig) { c.Health = nil }, "Health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full
			tt.mutate(&cfg)
			n, err := NewNode(cfg)
			if err == nil || n != nil {
				t.Fatalf("NewNode = (%v, %v), want error mentioning %s", n, err, tt.want)
			}
			if !contains1(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %s", err, tt.want)
			}
		})
	}

	t.Run("defaults applied", func(t *testing.T) {
		n, err := NewNode(full)
		if err != nil {
			t.Fatal(err)
		}
		c := n.cfg
		if c.ProbeInterval != DefaultProbeInterval || c.ElectionFloor != DefaultElectionFloor ||
			c.GossipInterval != DefaultGossipInterval || c.Shuffle == nil ||
			c.ProbeTimeout != DefaultProbeTimeout || c.Rehome != DefaultHysteresis ||
			c.Election.Hysteresis != DefaultHysteresis ||
			c.HeartbeatInterval != DefaultHeartbeatInterval || c.HeartbeatMisses != DefaultHeartbeatMisses ||
			c.SuspectAfter != DefaultSuspectAfter || c.DeadAfter != DefaultDeadAfter ||
			c.SuspicionTimeout != DefaultSuspicionTimeout ||
			c.ProbeTimeout <= health.DefaultProbeTimeout ||
			c.QueueDepth != DefaultQueueDepth || c.Logger == nil {
			t.Fatalf("defaults not applied: %+v", c)
		}
		if _, ok := c.Clock.(RealClock); !ok {
			t.Fatalf("Clock = %T, want RealClock", c.Clock)
		}
		// Self is a member before Run, so Resolve works for the prober immediately.
		if id, ok := n.Resolve("node-a:7000"); !ok || id != "node-a" {
			t.Fatalf("Resolve(self) = (%q, %v)", id, ok)
		}
		if _, ok := n.Resolve("nobody:1"); ok {
			t.Fatal("Resolve of an unknown address succeeded")
		}
		if n.Table().Snapshot().Size() != 1 {
			t.Fatal("table should hold exactly self")
		}
		st := n.Status()
		if st.Self != "node-a" || st.Role != RoleWorker || st.Term != 0 {
			t.Fatalf("initial status = %s", st)
		}
	})

	t.Run("negative probe timeout rejected", func(t *testing.T) {
		cfg := full
		cfg.ProbeTimeout = -time.Second
		if _, err := NewNode(cfg); err == nil || !contains1(err.Error(), "ProbeTimeout") {
			t.Fatalf("NewNode = %v, want ProbeTimeout error", err)
		}
	})

	t.Run("negative rehome means no damping", func(t *testing.T) {
		cfg := full
		cfg.Rehome = -1
		n, err := NewNode(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if n.cfg.Rehome != -1 {
			t.Fatalf("Rehome = %v, want -1 preserved", n.cfg.Rehome)
		}
	})
}

func contains1(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}

func TestSingleNodeElectsItself(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	st := h.status()
	if st.Role != RoleLeader || st.Leader != "node-a" || st.Term != 1 {
		t.Fatalf("status = %s", st)
	}
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))
	if st.HighWater != 1 || st.Degraded {
		t.Fatalf("HighWater=%d Degraded=%v", st.HighWater, st.Degraded)
	}

	res := h.tr.broadcastOf(protocol.TypeElectionResult)
	if len(res) != 1 {
		t.Fatalf("ELECTION_RESULT broadcasts = %d, want 1", len(res))
	}
	p, err := protocol.PayloadOf[protocol.ElectionResultPayload](res[0])
	if err != nil {
		t.Fatal(err)
	}
	if p.Term != 1 || p.ClusterSize != 1 || !equalIDs(p.Leaders, ids("node-a")) {
		t.Fatalf("payload = %+v", p)
	}
	// A leader with no one attached sent no JOIN.
	if got := h.tr.sentOf(protocol.TypeJoinCluster); len(got) != 0 {
		t.Fatalf("a lone leader sent JOIN_CLUSTER: %v", got)
	}
}

// threeNodes builds a swarm where node-b reports the best score, node-c the
// worst, and self's local RTTs make it the median (2.0), then runs one probe
// round so self has reported that median.
func threeNodes(t *testing.T, self protocol.NodeID) *harness {
	t.Helper()
	h := newHarness(t, self, nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 3.0)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.report("node-b", 1, 1.0)
	h.report("node-c", 1, 3.0)
	h.probeRound()
	return h
}

func TestThreeNodesElectLeaderCountAndWorkerJoins(t *testing.T) {
	h := threeNodes(t, "node-z")
	st := h.status()

	if st.View.Size() != 3 {
		t.Fatalf("alive = %d, want 3", st.View.Size())
	}
	if want := LeaderCount(3, DefaultThreshold); len(st.Leaders) != want {
		t.Fatalf("len(Leaders) = %d, want %d", len(st.Leaders), want)
	}
	requireIDs(t, "Leaders", st.Leaders, ids("node-b"))
	if st.Role != RoleLeader && st.Leader != "node-b" {
		t.Fatalf("worker not provisionally attached to node-b: %s", st)
	}
	// Self is scored as the median of its peers, and reports it.
	if got := st.Scores["node-z"]; got != 2.0 {
		t.Fatalf("self score = %v, want median 2.0", got)
	}
	if got := st.Reported["node-z"]; got != 2.0 {
		t.Fatalf("self reported = %v, want 2.0", got)
	}
	if st.Reported["node-b"] != 1.0 || st.Reported["node-c"] != 3.0 {
		t.Fatalf("Reported = %v", st.Reported)
	}
	if got := st.Missed["node-b"]; got != 0 {
		t.Fatalf("Missed[node-b] = %d, want 0", got)
	}

	joins := h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != 1 || joins[0].to != "node-b" {
		t.Fatalf("JOIN_CLUSTER sends = %+v, want exactly one to node-b", joins)
	}
	p, err := protocol.PayloadOf[protocol.JoinClusterPayload](joins[0].env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Worker != "node-z" || p.Score != 1.0 {
		t.Fatalf("JOIN payload = %+v", p)
	}

	// Our own role is written from the result; peers' roles are their claims,
	// and node-b has only ever claimed worker.
	if st.Role != RoleWorker {
		t.Fatalf("self role = %s, want worker", st.Role)
	}
	if m, _ := st.View.Get("node-b"); m.Role != RoleWorker {
		t.Fatalf("node-b role = %s before it claims leadership, want worker", m.Role)
	}
	h.reportRole("node-b", 1, 1.0, RoleLeader)
	if m, _ := h.status().View.Get("node-b"); m.Role != RoleLeader {
		t.Fatalf("node-b role = %s after claiming leadership, want leader", m.Role)
	}

	// The ACK confirms attachment.
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("Leader after JOIN_ACK = %q", st.Leader)
	}

	// Another probe round with identical scores sends nothing new: the worker is
	// already home and hysteresis holds.
	h.probeRound()
	if got := h.tr.sentOf(protocol.TypeJoinCluster); len(got) != 1 {
		t.Fatalf("JOIN_CLUSTER re-sent with unchanged inputs: %d sends", len(got))
	}
}

func TestJoinAckWithEmptyLeaderUsesSender(t *testing.T) {
	h := threeNodes(t, "node-z")
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true})
	if st := h.status(); st.Leader != "node-b" {
		t.Fatalf("Leader = %q, want node-b", st.Leader)
	}
}

func TestJoinClusterAcceptedByLeader(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1) // unscored: fallback elects the lowest ID, which is self
	if st := h.status(); st.Role != RoleLeader {
		t.Fatalf("expected self to lead by lowest-ID fallback: %s", st)
	}

	req := h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b", Score: 0.7})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))

	acks := h.tr.sentOf(protocol.TypeJoinAck)
	if len(acks) != 1 || acks[0].to != "node-b" || acks[0].env.ID != req.ID {
		t.Fatalf("JOIN_ACK sends = %+v", acks)
	}
	p, err := protocol.PayloadOf[protocol.JoinAckPayload](acks[0].env)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Accepted || p.Leader != "node-a" {
		t.Fatalf("JOIN_ACK payload = %+v", p)
	}

	// An empty Worker field falls back to the sender.
	h.frame("node-c", protocol.TypeJoinCluster, protocol.JoinClusterPayload{})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b", "node-c"))
	// node-c is not a member, so evaluate prunes it on the next pass; node-b stays.
	h.clock.Advance(floorEvery)
	h.settle()
	requireIDs(t, "Attached after prune", h.status().Attached, ids("node-b"))

	// Attached workers that die are pruned.
	h.peerDown("node-b", network.DispositionPeerDied)
	requireIDs(t, "Attached after death", h.status().Attached, nil)
}

func TestJoinClusterRejectedByWorkerWithHint(t *testing.T) {
	h := threeNodes(t, "node-z")
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})

	req := h.frame("node-c", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-c"})
	acks := h.tr.sentOf(protocol.TypeJoinAck)
	if len(acks) != 1 || acks[0].to != "node-c" || acks[0].env.ID != req.ID {
		t.Fatalf("JOIN_ACK sends = %+v", acks)
	}
	p, err := protocol.PayloadOf[protocol.JoinAckPayload](acks[0].env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Accepted || p.Leader != "node-b" || p.Reason == "" {
		t.Fatalf("JOIN_ACK payload = %+v, want rejection hinting node-b", p)
	}
	if len(h.status().Attached) != 0 {
		t.Fatal("a worker recorded an attachment")
	}
}

func TestJoinAckRejectionFollowsHint(t *testing.T) {
	// Four nodes at threshold 0.5 elect two leaders. Reported: node-b 1.0,
	// node-c 1.0, node-d 10, self the median of its local RTTs (1.5) -- so b and
	// c win the seats by more than the hysteresis margin. Locally node-b is the
	// closest leader, so affinity picks it first.
	h := newHarness(t, "node-z", func(c *NodeConfig) { c.Election.Threshold = 0.5 })
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 1.5)
	h.hs.set(addrOf("node-d"), 10.0)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.peerUp("node-d", 1)
	h.report("node-b", 1, 1.0)
	h.report("node-c", 1, 1.0)
	h.report("node-d", 1, 10.0)
	h.probeRound()

	st := h.status()
	requireIDs(t, "Leaders", st.Leaders, ids("node-b", "node-c"))
	joins := h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != 1 || joins[0].to != "node-b" {
		t.Fatalf("first JOIN went to %+v, want node-b", joins)
	}

	// node-b refuses and points at node-c.
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: false, Reason: "full", Leader: "node-c"})
	joins = h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != 2 || joins[1].to != "node-c" {
		t.Fatalf("after rejection, JOINs = %+v, want a second one to node-c", joins)
	}
	if st := h.status(); st.Leader != "node-c" {
		t.Fatalf("Leader = %q, want provisional node-c", st.Leader)
	}

	// A hint that is not a leader is ignored and ranking wins: back to node-b.
	h.frame("node-c", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: false, Leader: "node-d"})
	joins = h.tr.sentOf(protocol.TypeJoinCluster)
	if len(joins) != 3 || joins[2].to != "node-b" {
		t.Fatalf("after bad hint, JOINs = %+v, want a third one to node-b", joins)
	}
}

func TestJoinSendFailureLeavesNodeDetached(t *testing.T) {
	h := newHarness(t, "node-z", nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 3.0)
	h.tr.failSend("node-b", network.ErrUnknownPeer)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.report("node-b", 1, 1.0)
	h.report("node-c", 1, 3.0)
	h.probeRound()
	if st := h.status(); st.Leader != "" {
		t.Fatalf("Leader = %q after a failed JOIN send, want detached", st.Leader)
	}
	// The next evaluate retries.
	h.probeRound()
	if got := h.tr.sentOf(protocol.TypeJoinCluster); len(got) != 0 {
		t.Fatalf("failed sends were recorded: %v", got)
	}
}

// A lost link to the leader is a suspicion first: the worker leaves the leader
// at once, but the swarm does not re-elect until the suspicion is confirmed.
// Confirmation is what triggers the re-election.
func TestPeerDownFailureTriggersReelection(t *testing.T) {
	h := threeNodes(t, "node-a")
	h.reportRole("node-b", 1, 1.0, RoleLeader) // node-b has announced its seat
	before := h.status()
	requireIDs(t, "Leaders", before.Leaders, ids("node-b"))
	if before.Leader != "node-b" {
		t.Fatalf("setup: %s, want attached to node-b", before)
	}
	broadcastsBefore := len(h.tr.broadcastOf(protocol.TypeElectionResult))

	h.peerDown("node-b", network.DispositionPeerDied)

	st := h.status()
	if m, _ := st.View.Get("node-b"); m.State != StateSuspect || m.Incarnation != 1 {
		t.Fatalf("node-b = %+v, want suspect at 1", m)
	}
	requireIDs(t, "Leaders while suspect", st.Leaders, ids("node-b"))
	if st.Term != before.Term || st.Leader != "" {
		t.Fatalf("while suspect: %s, want same term and detached", st)
	}

	if err := confirmSuspicions(h.ctx, h.node); err != nil {
		t.Fatal(err)
	}
	st = h.status()
	if m, ok := st.View.Get("node-b"); !ok || m.State != StateDead {
		t.Fatalf("node-b = %+v, want dead", m)
	}
	// Alive: node-a (2.0, the median from the last round) and node-c (3.0).
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))
	if st.Term != before.Term+1 {
		t.Fatalf("Term = %d, want %d", st.Term, before.Term+1)
	}
	if st.Role != RoleLeader || st.Leader != "node-a" {
		t.Fatalf("status = %s", st)
	}
	res := h.tr.broadcastOf(protocol.TypeElectionResult)
	if len(res) != broadcastsBefore+1 {
		t.Fatalf("ELECTION_RESULT broadcasts = %d, want %d", len(res), broadcastsBefore+1)
	}
	p, _ := protocol.PayloadOf[protocol.ElectionResultPayload](res[len(res)-1])
	if p.Term != st.Term || p.ClusterSize != 2 || !equalIDs(p.Leaders, ids("node-a")) {
		t.Fatalf("payload = %+v", p)
	}
	// Health history is bounded to the live set.
	keep := h.hs.lastRetained()
	if _, ok := keep[addrOf("node-b")]; ok {
		t.Fatal("dead peer still retained in health history")
	}
	if _, ok := keep[addrOf("node-c")]; !ok {
		t.Fatal("live peer dropped from health history")
	}
}

// A peer that dropped and reconnected at its old incarnation (a partition, not a
// restart) is NOT revived by its handshake: the death was recorded one
// incarnation past it (HIGH-2). It heals through the refutation path instead --
// the view we send on reconnect tells it "you are dead at 2", it bumps to 3 and
// announces, and that first-hand record wins. The peer here is a real Node, so
// the refutation is the production refuteIfNeeded, not a hand-built frame.
func TestPeerReconnectAtOldIncarnationHealsByRefutation(t *testing.T) {
	a := threeNodes(t, "node-a")
	requireIDs(t, "Leaders", a.status().Leaders, ids("node-b"))

	b := newHarness(t, "node-b", func(c *NodeConfig) { c.Incarnation = 1 })
	b.hs.set(addrOf("node-a"), 1.0)
	b.peerUp("node-a", 1)
	b.probeRound() // node-b now reports 1.0 about itself

	a.peerDead("node-b")
	st := a.status()
	if m, _ := st.View.Get("node-b"); m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("node-b = %+v, want dead at 2", m)
	}
	requireIDs(t, "Leaders after death", st.Leaders, ids("node-a"))

	// Same incarnation: the container was partitioned, not restarted. The
	// handshake is older than the death, so it does not revive.
	a.peerUp("node-b", 1)
	if m, _ := a.status().View.Get("node-b"); m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("node-b after stale handshake = %+v, want still dead at 2", m)
	}

	// The reconnecting peer is handed our full view, which carries its death.
	views := a.tr.sentOf(protocol.TypeMembershipDelta)
	if len(views) == 0 || views[len(views)-1].to != "node-b" {
		t.Fatalf("no view sent to the reconnecting peer: %+v", views)
	}
	welcome := views[len(views)-1].env
	p, err := protocol.PayloadOf[protocol.MembershipDeltaPayload](welcome)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Members) != 3 {
		t.Fatalf("view carried %d members, want 3", len(p.Members))
	}

	// node-b hears it is dead at 2, refutes at 3, and announces.
	before := len(b.tr.broadcastOf(protocol.TypeMembershipDelta))
	b.node.Handler()("node-a", welcome)
	b.settle()
	if got := b.status().Incarnation; got != 3 {
		t.Fatalf("node-b incarnation = %d after refuting, want 3", got)
	}
	anns := b.tr.broadcastOf(protocol.TypeMembershipDelta)
	if len(anns) != before+1 {
		t.Fatalf("node-b announcements = %d, want %d", len(anns), before+1)
	}

	// The refutation reaches node-a and wins.
	a.node.Handler()("node-b", anns[len(anns)-1])
	a.settle()
	m, _ := a.status().View.Get("node-b")
	if m.State != StateAlive || m.Incarnation != 3 {
		t.Fatalf("node-b after refutation = %+v, want alive at 3", m)
	}

	// Probed and electable again.
	a.probeRound()
	st = a.status()
	if got := st.Scores["node-b"]; got != 1.0 {
		t.Fatalf("node-b not probed after revive: score=%v", got)
	}
	requireIDs(t, "Leaders after revive", st.Leaders, ids("node-b"))
	if st.Leader != "node-b" {
		t.Fatalf("worker did not re-home to the revived leader: %s", st)
	}
}

// HIGH-2, the STATE.md interleaving at node level: node-b's refutation ("alive
// at 7") is queued behind the PeerDown that reports its death. Whichever order
// the loop takes them in, node-b ends dead -- before the fix, death-first
// recorded "dead at 6" and the refutation resurrected it.
func TestQueuedRefutationCannotResurrectAfterDeath(t *testing.T) {
	t.Run("death processed first", func(t *testing.T) {
		h := newHarness(t, "node-a", nil)
		h.peerUp("node-b", 6)
		h.peerDead("node-b")
		h.report("node-b", 7, 1.0)
		if m, _ := h.status().View.Get("node-b"); m.State != StateDead || m.Incarnation != 7 {
			t.Fatalf("node-b = %+v, want dead at 7", m)
		}
	})
	t.Run("refutation processed first", func(t *testing.T) {
		h := newHarness(t, "node-a", nil)
		h.peerUp("node-b", 6)
		h.report("node-b", 7, 1.0)
		h.peerDead("node-b")
		if m, _ := h.status().View.Get("node-b"); m.State != StateDead || m.Incarnation != 8 {
			t.Fatalf("node-b = %+v, want dead at 8", m)
		}
	})
	// The Phase 4 variant: the refutation lands while the death is still only
	// a suspicion. A refutation is a legitimate claim of life, so it wins and
	// the suspicion is dropped rather than confirmed at a stale incarnation.
	// The peer really is gone, though, so its probes keep failing and the
	// probe thresholds kill it at the new incarnation. Nothing is lost; the
	// verdict just takes the probe path.
	t.Run("refutation during suspicion", func(t *testing.T) {
		h := newHarness(t, "node-a", func(c *NodeConfig) {
			c.SuspectAfter, c.DeadAfter = 1, 2
		})
		h.peerUp("node-b", 6)
		h.peerDown("node-b", network.DispositionPeerDied)
		h.report("node-b", 7, 1.0)
		if m, _ := h.status().View.Get("node-b"); m.State != StateAlive || m.Incarnation != 7 {
			t.Fatalf("node-b = %+v, want the refutation to win: alive at 7", m)
		}
		if err := confirmSuspicions(h.ctx, h.node); err != nil {
			t.Fatal(err)
		}
		if m, _ := h.status().View.Get("node-b"); m.State != StateAlive {
			t.Fatalf("a resolved suspicion was confirmed: %+v", m)
		}
		h.probeRound() // unscored: fails
		h.probeRound()
		if m, _ := h.status().View.Get("node-b"); m.State != StateDead || m.Incarnation != 8 {
			t.Fatalf("node-b = %+v, want dead at 8 via missed probes", m)
		}
	})
}

func TestStalePeerUpCannotResurrectNewerDeath(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 5)
	h.peerDead("node-b")  // dead at 6
	h.peerUp("node-b", 3) // an old socket registering late
	if m, _ := h.status().View.Get("node-b"); m.State != StateDead || m.Incarnation != 6 {
		t.Fatalf("node-b = %+v, want still dead at 6", m)
	}
}

func TestPeerUpPreservesRole(t *testing.T) {
	h := threeNodes(t, "node-a")
	requireIDs(t, "Leaders", h.status().Leaders, ids("node-b"))
	h.reportRole("node-b", 1, 1.0, RoleLeader)
	term := h.status().Term
	// A duplicate PeerUp (replacement connection) must not demote the leader.
	h.peerUp("node-b", 1)
	st := h.status()
	if m, _ := st.View.Get("node-b"); m.Role != RoleLeader {
		t.Fatalf("node-b role = %s after PeerUp, want leader", m.Role)
	}
	if st.Term != term {
		t.Fatalf("term moved %d -> %d on a duplicate PeerUp", term, st.Term)
	}
}

func TestPeerDownDispositions(t *testing.T) {
	tests := []struct {
		name  string
		disp  network.Disposition
		alive bool // still present in the view
		state State
	}{
		{"clean close marks dead", network.DispositionCleanClose, true, StateDead},
		{"protocol violation marks dead", network.DispositionProtocolViolation, true, StateDead},
		{"peer died marks suspect", network.DispositionPeerDied, true, StateSuspect},
		{"timeout marks suspect", network.DispositionTimeout, true, StateSuspect},
		{"other marks suspect", network.DispositionOther, true, StateSuspect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, "node-a", nil)
			h.peerUp("node-b", 1)
			h.peerDown("node-b", tt.disp)
			m, ok := h.status().View.Get("node-b")
			if ok != tt.alive {
				t.Fatalf("present = %v, want %v", ok, tt.alive)
			}
			if ok && m.State != tt.state {
				t.Fatalf("state = %s, want %s", m.State, tt.state)
			}
		})
	}
}

func TestPeerEventsIgnored(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.tr.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: ""}}
	h.tr.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: "node-a", Advertise: "elsewhere:1"}}
	h.tr.events <- network.PeerEvent{Kind: network.PeerEventKind(99), Peer: network.PeerInfo{ID: "node-b"}}
	h.settle()
	st := h.status()
	if st.View.Size() != 1 {
		t.Fatalf("view = %+v, want only self", st.View)
	}
	if m, _ := st.View.Get("node-a"); m.Addr != addrOf("node-a") {
		t.Fatalf("self address overwritten by a self PeerUp: %s", m.Addr)
	}
}

func TestMembershipDeltaMergesAndIgnoresStale(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)

	rec := func(id protocol.NodeID, inc int64, role, state string) protocol.MemberRecord {
		return protocol.MemberRecord{ID: id, Advertise: addrOf(id), Incarnation: inc, Role: role, State: state}
	}
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{
		rec("node-d", 2, "worker", "alive"),
		{ID: ""}, // ignored
	}})
	st := h.status()
	if m, ok := st.View.Get("node-d"); !ok || m.State != StateAlive || m.Incarnation != 2 {
		t.Fatalf("node-d = %+v, want alive at incarnation 2", m)
	}
	if st.HighWater != 3 {
		t.Fatalf("HighWater = %d, want 3", st.HighWater)
	}
	// Gossiped peers have addresses, so they are probed; Resolve sees them.
	if id, ok := h.node.Resolve(addrOf("node-d")); !ok || id != "node-d" {
		t.Fatalf("Resolve(node-d) = (%q, %v)", id, ok)
	}

	// Stale rumour: lower incarnation, ignored.
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{rec("node-d", 1, "worker", "dead")}})
	if m, _ := h.status().View.Get("node-d"); m.State != StateAlive {
		t.Fatal("stale death rumour was applied")
	}

	// Same incarnation, worse state: applied.
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{rec("node-d", 2, "worker", "dead")}})
	if m, _ := h.status().View.Get("node-d"); m.State != StateDead {
		t.Fatal("death at equal incarnation was not applied")
	}
	if _, ok := h.node.Resolve(addrOf("node-d")); ok {
		t.Fatal("Resolve returned a dead member")
	}
}

func TestDeadRumourAboutSelfBumpsIncarnation(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) { c.Incarnation = 5 })
	h.peerUp("node-b", 1)
	base := len(h.tr.broadcastOf(protocol.TypeMembershipDelta)) // the self-election announcement
	self := func(inc int64, state string) protocol.MembershipDeltaPayload {
		return protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{ID: "node-a", Advertise: addrOf("node-a"), Incarnation: inc, Role: "worker", State: state}}}
	}

	// Stale rumour (older incarnation): ignored.
	h.frame("node-b", protocol.TypeMembershipDelta, self(3, "dead"))
	if st := h.status(); st.Incarnation != 5 {
		t.Fatalf("Incarnation = %d after a stale rumour, want 5", st.Incarnation)
	}
	// Alive rumour, even newer: ignored -- we own our own record.
	h.frame("node-b", protocol.TypeMembershipDelta, self(9, "alive"))
	if st := h.status(); st.Incarnation != 5 {
		t.Fatalf("Incarnation = %d after an alive rumour, want 5", st.Incarnation)
	}
	if len(h.tr.broadcastOf(protocol.TypeMembershipDelta)) != base {
		t.Fatal("re-announced without cause")
	}

	// Current rumour of death: refute by bumping past it and re-announcing.
	h.frame("node-b", protocol.TypeMembershipDelta, self(7, "suspect"))
	st := h.status()
	if st.Incarnation != 8 {
		t.Fatalf("Incarnation = %d, want 8", st.Incarnation)
	}
	m, _ := st.View.Get("node-a")
	if m.State != StateAlive || m.Incarnation != 8 || m.Role != RoleLeader {
		t.Fatalf("self record = %+v, want alive leader at 8", m)
	}
	ann := h.tr.broadcastOf(protocol.TypeMembershipDelta)[base:]
	if len(ann) != 1 {
		t.Fatalf("re-announcements = %d, want 1", len(ann))
	}
	p, err := protocol.PayloadOf[protocol.MembershipDeltaPayload](ann[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Members) != 1 || p.Members[0].ID != "node-a" || p.Members[0].Incarnation != 8 ||
		p.Members[0].State != "alive" || p.Members[0].Role != "leader" {
		t.Fatalf("announcement = %+v", p)
	}
}

// A rumour at the top of the int64 range must not wrap our incarnation
// negative, which would make every future record about us lose every merge.
func TestRefutationOfMaxIncarnationRumourSaturates(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) { c.Incarnation = 5 })
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-a", Advertise: addrOf("node-a"), Incarnation: math.MaxInt64, Role: "worker", State: "dead",
	}}})
	st := h.status()
	if st.Incarnation != math.MaxInt64 {
		t.Fatalf("Incarnation = %d, want clamped to MaxInt64", st.Incarnation)
	}
	if m, _ := st.View.Get("node-a"); m.State != StateAlive || m.Incarnation != math.MaxInt64 {
		t.Fatalf("self record = %+v, want alive at MaxInt64", m)
	}
}

func TestElectionResultIsRecordedAsClaimOnly(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	claim := protocol.ElectionResultPayload{Term: 42, Leaders: ids("node-b"), ClusterSize: 2}
	h.frame("node-b", protocol.TypeElectionResult, claim)

	st := h.status()
	got, ok := st.Claims["node-b"]
	if !ok || got.Term != 42 || !equalIDs(got.Leaders, ids("node-b")) {
		t.Fatalf("Claims[node-b] = %+v, %v", got, ok)
	}
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))
	if st.Term != 1 {
		t.Fatalf("Term = %d; a peer's claim must not move our term", st.Term)
	}
	if m, _ := st.View.Get("node-b"); m.Role != RoleWorker {
		t.Fatal("a claim changed a role")
	}
}

func TestLeaveMarksPeerDead(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))

	h.frame("node-b", protocol.TypeLeave, protocol.LeavePayload{Reason: "scale-down"})
	st := h.status()
	m, ok := st.View.Get("node-b")
	if !ok || m.State != StateDead || m.Incarnation != 2 {
		t.Fatalf("node-b after LEAVE = %+v (present=%v), want dead at 2", m, ok)
	}
	requireIDs(t, "Attached", st.Attached, nil)

	// The point of keeping the record: a peer that has not heard the LEAVE
	// echoes node-b as alive at its old incarnation, and must not re-insert it.
	h.frame("node-c", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 1, Role: "worker", State: "alive",
	}}})
	if m, _ := h.status().View.Get("node-b"); m.State != StateDead {
		t.Fatalf("stale echo resurrected a node that left: %+v", m)
	}

	// The clean close that follows a LEAVE is a no-op, not a second bump.
	version := h.status().View.Version
	h.peerDown("node-b", network.DispositionCleanClose)
	if st := h.status(); st.View.Version != version {
		t.Fatalf("clean close after LEAVE changed the table: %d -> %d", version, st.View.Version)
	}

	// A LEAVE for a peer we never knew is harmless.
	h.frame("node-q", protocol.TypeLeave, nil)
	if _, ok := h.status().View.Get("node-q"); ok {
		t.Fatal("a LEAVE from a stranger created a record")
	}
}

func TestUnknownAndUnhandledTypesAreDropped(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	before := h.status()
	h.frame("node-b", protocol.MessageType("FROBNICATE"), map[string]int{"x": 1})
	h.frame("node-b", protocol.MessageType("FROBNICATE"), nil) // second: the once-logged path
	h.frame("node-b", protocol.TypeHeartbeat, protocol.HeartbeatPayload{Seq: 1})
	// Data-plane frames travel the other queue and are owned by Phase 5.
	h.frame("node-b", protocol.TypeTask, protocol.TaskPayload{TaskID: "t1", Kind: "noop"})
	after := h.status()
	if after.View.Version != before.View.Version || after.Term != before.Term {
		t.Fatalf("unhandled frames changed state: %s -> %s", before, after)
	}
	if m, _ := after.View.Get("node-b"); m.State != StateAlive {
		t.Fatal("an unknown type poisoned the peer")
	}
}

func TestMalformedPayloadMarksPeerDead(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})

	env := &protocol.Envelope{Version: protocol.CurrentVersion, Type: protocol.TypeJoinAck, From: "node-b", To: "node-a", Payload: json.RawMessage(`{"accepted":`)}
	h.node.Handler()("node-b", env)
	h.settle()

	st := h.status()
	if m, _ := st.View.Get("node-b"); m.State != StateDead {
		t.Fatalf("node-b = %+v, want dead", m)
	}
	requireIDs(t, "Attached", st.Attached, nil)

	// Poisoning an already-dead peer is a no-op rather than a second evaluate.
	term := st.Term
	h.node.Handler()("node-b", env)
	h.settle()
	if h.status().Term != term {
		t.Fatal("term moved on a no-op")
	}
}

func TestFloorTickerIsIdempotent(t *testing.T) {
	h := threeNodes(t, "node-z")
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	before := h.status()
	results := len(h.tr.broadcastOf(protocol.TypeElectionResult))
	joins := len(h.tr.sentOf(protocol.TypeJoinCluster))

	// Thirty floor ticks with unchanged inputs (the probe ticker fires along the
	// way with the same scores).
	for i := 0; i < 3; i++ {
		h.clock.Advance(floorEvery)
		h.settle()
	}

	after := h.status()
	if after.Term != before.Term {
		t.Fatalf("Term moved %d -> %d with unchanged inputs", before.Term, after.Term)
	}
	requireIDs(t, "Leaders", after.Leaders, before.Leaders)
	if after.Leader != before.Leader {
		t.Fatalf("Leader moved %q -> %q", before.Leader, after.Leader)
	}
	if n := len(h.tr.broadcastOf(protocol.TypeElectionResult)); n != results {
		t.Fatalf("ELECTION_RESULT broadcasts %d -> %d", results, n)
	}
	if n := len(h.tr.sentOf(protocol.TypeJoinCluster)); n != joins {
		t.Fatalf("JOIN_CLUSTER sends %d -> %d", joins, n)
	}
}

func TestProbeCancellationRecordsNothing(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	h.probeRound()
	if got := h.status().Scores["node-b"]; got != 1.0 {
		t.Fatalf("score = %v, want 1.0", got)
	}

	h.hs.fail(addrOf("node-b"), fmt.Errorf("%w: %w", health.ErrProbeCanceled, context.Canceled))
	h.probeRound()
	st := h.status()
	if got := st.Scores["node-b"]; got != 1.0 {
		t.Fatalf("a cancelled probe changed the score to %v", got)
	}
	if st.Missed["node-b"] != 0 {
		t.Fatalf("Missed = %d after cancellation, want 0", st.Missed["node-b"])
	}

	// A bare context error counts as cancellation too.
	h.hs.fail(addrOf("node-b"), context.DeadlineExceeded)
	h.probeRound()
	if st := h.status(); st.Missed["node-b"] != 0 {
		t.Fatalf("Missed = %d after a bare deadline error, want 0", st.Missed["node-b"])
	}
}

func TestProbeUnreachableIncrementsMissed(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	h.probeRound()

	h.hs.fail(addrOf("node-b"), health.ErrUnreachable)
	h.probeRound()
	st := h.status()
	if !math.IsNaN(st.Scores["node-b"]) {
		t.Fatalf("score = %v, want NaN", st.Scores["node-b"])
	}
	if st.Missed["node-b"] != 1 {
		t.Fatalf("Missed = %d, want 1", st.Missed["node-b"])
	}
	// We have a peer and could not measure it, so we know nothing about ourselves
	// either: the self-report is unavailable, not 0. Leadership still lands on
	// node-a, but through Elect's nobody-is-measurable fallback (lowest alive ID)
	// rather than because node-a claimed the best score in the swarm.
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))
	if !math.IsNaN(st.Scores["node-a"]) {
		t.Fatalf("self score = %v with peers but no valid measurement, want NaN", st.Scores["node-a"])
	}

	h.probeRound()
	if got := h.status().Missed["node-b"]; got != 2 {
		t.Fatalf("Missed = %d, want 2", got)
	}

	h.hs.set(addrOf("node-b"), 1.0)
	h.probeRound()
	if st := h.status(); st.Missed["node-b"] != 0 || st.Scores["node-b"] != 1.0 {
		t.Fatalf("after recovery: Missed=%d score=%v", st.Missed["node-b"], st.Scores["node-b"])
	}
}

func TestSelfScoreMedian(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	for i, s := range []float64{4, 1, 3} {
		id := protocol.NodeID(fmt.Sprintf("node-%c", 'b'+i))
		h.hs.set(addrOf(id), s)
		h.peerUp(id, 1)
	}
	h.probeRound()
	if got := h.status().Scores["node-a"]; got != 3 {
		t.Fatalf("median of {4,1,3} = %v, want 3", got)
	}
	h.hs.set(addrOf("node-e"), 10)
	h.peerUp("node-e", 1)
	h.probeRound()
	if got := h.status().Scores["node-a"]; got != 3.5 {
		t.Fatalf("median of {4,1,3,10} = %v, want 3.5", got)
	}
}

// MEDIUM-2: peers that have gone must stop counting towards the self-score, and
// their per-peer entries must not accumulate.
func TestDepartedPeersStopSkewingSelfScore(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)

	// Ten slow workers join, are measured, and drag the median up.
	var slow []protocol.NodeID
	for i := 0; i < 10; i++ {
		id := protocol.NodeID(fmt.Sprintf("slow-%02d", i))
		slow = append(slow, id)
		h.hs.set(addrOf(id), 50.0)
		h.peerUp(id, 1)
	}
	h.probeRound()
	if got := h.status().Scores["node-a"]; got != 50.0 {
		t.Fatalf("self score with ten slow peers = %v, want 50", got)
	}

	// Half leave politely, half vanish: both kinds of departure must prune.
	for i, id := range slow {
		if i%2 == 0 {
			h.frame(id, protocol.TypeLeave, protocol.LeavePayload{Reason: "scale-down"})
		} else {
			h.peerDead(id)
		}
	}
	st := h.status()
	for _, id := range slow {
		if _, ok := st.Scores[id]; ok {
			t.Fatalf("Scores still holds departed %s: %v", id, st.Scores)
		}
		if _, ok := st.Missed[id]; ok {
			t.Fatalf("Missed still holds departed %s: %v", id, st.Missed)
		}
	}
	if _, ok := st.Scores["node-b"]; !ok {
		t.Fatal("a live peer was pruned")
	}
	if _, ok := st.Scores["node-a"]; !ok {
		t.Fatal("the self entry was pruned")
	}

	// The next self-report is the median over the one peer still here.
	h.probeRound()
	if got := h.status().Scores["node-a"]; got != 1.0 {
		t.Fatalf("self score after departures = %v, want 1.0", got)
	}
}

// A probe result for a peer that died while the probe was in flight must not
// re-insert the entry that the death just pruned.
func TestInFlightProbeOfDeadPeerIsDiscarded(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.fail(addrOf("node-c"), health.ErrUnreachable) // a real failure, not a cancel
	release := h.hs.hold(addrOf("node-c"))
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)

	h.clock.Advance(probeEvery)
	h.barrier() // the round is in flight: node-c's probe is held at the gate

	// Deliver the death on the loop goroutine, deterministically ahead of the
	// round's result. Pushing it on the events channel would race the result in
	// the loop's select.
	if err := h.node.barrier(h.ctx, func() {
		h.node.handlePeerEvent(h.ctx, network.PeerEvent{
			Kind: network.PeerDown, Peer: network.PeerInfo{ID: "node-c"}, Disposition: network.DispositionPeerDied,
		})
		// ...and its confirmation, so node-c is dead rather than suspect.
		if h.node.expireSuspicions(h.ctx, h.clock.Now().Add(h.node.cfg.SuspicionTimeout)) {
			h.node.membershipChanged(h.ctx)
		}
	}); err != nil {
		t.Fatal(err)
	}
	release()
	h.settle() // waits for the round to be applied

	st := h.status()
	if _, ok := st.Scores["node-c"]; ok {
		t.Fatalf("dead peer's in-flight result was recorded: %v", st.Scores)
	}
	if _, ok := st.Missed["node-c"]; ok {
		t.Fatalf("dead peer's in-flight result was counted: %v", st.Missed)
	}
	if got := st.Scores["node-b"]; got != 1.0 {
		t.Fatalf("live peer's result from the same round = %v, want 1.0", got)
	}
}

func TestDemotedLeaderDropsAttached(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))

	// Scores arrive; node-b is clearly best and self is the median.
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 3.0)
	h.report("node-b", 1, 1.0)
	h.report("node-c", 1, 3.0)
	h.probeRound()
	st := h.status()
	requireIDs(t, "Leaders", st.Leaders, ids("node-b"))
	if st.Role != RoleLeader && len(st.Attached) != 0 {
		t.Fatalf("demoted leader kept attachments: %v", st.Attached)
	}
	if st.Leader != "node-b" {
		t.Fatalf("demoted leader did not re-home: Leader=%q", st.Leader)
	}
	if got := h.tr.sentOf(protocol.TypeJoinCluster); len(got) != 1 || got[0].to != "node-b" {
		t.Fatalf("JOINs = %+v", got)
	}
}

func TestDegradedFlipsAndClears(t *testing.T) {
	h := threeNodes(t, "node-a")
	if st := h.status(); st.Degraded || st.HighWater != 3 {
		t.Fatalf("intact swarm: %s highwater=%d", st, st.HighWater)
	}
	h.peerDown("node-b", network.DispositionPeerDied)
	if st := h.status(); st.Degraded {
		t.Fatalf("2 of 3 is a majority, not degraded: %s", st)
	}
	h.peerDown("node-c", network.DispositionTimeout)
	st := h.status()
	if !st.Degraded {
		t.Fatalf("1 of 3 should be degraded: %s", st)
	}
	// Still leading: AP means a minority keeps a leader.
	if st.Role != RoleLeader {
		t.Fatalf("minority partition lost its leader: %s", st)
	}

	// A peer returning with a newer incarnation clears the flag.
	h.peerUp("node-b", 2)
	st = h.status()
	if st.Degraded {
		t.Fatalf("2 of 3 again, still degraded: %s", st)
	}
	if m, _ := st.View.Get("node-b"); m.State != StateAlive || m.Incarnation != 2 {
		t.Fatalf("returned peer = %+v", m)
	}
	if st.HighWater != 3 {
		t.Fatalf("HighWater moved to %d", st.HighWater)
	}
}

func TestStatusIsASnapshot(t *testing.T) {
	h := threeNodes(t, "node-a")
	h.frame("node-b", protocol.TypeElectionResult, protocol.ElectionResultPayload{Term: 1, Leaders: ids("node-b")})
	st := h.status()
	st.Scores["node-b"] = 999
	st.Missed["node-b"] = 999
	st.Leaders[0] = "mallory"
	st.Claims["node-b"].Leaders[0] = "mallory"
	st.View.Members[0].ID = "mallory"
	delete(st.Claims, "node-b")

	again := h.status()
	if again.Scores["node-b"] == 999 || again.Missed["node-b"] == 999 || again.Leaders[0] == "mallory" ||
		again.View.Members[0].ID == "mallory" || again.Claims["node-b"].Leaders[0] == "mallory" {
		t.Fatalf("mutating a Status reached the node: %+v", again)
	}
	if s := again.String(); s == "" {
		t.Fatal("Status.String is empty")
	}
}

func TestHandlerDropsDataPlaneWhenFull(t *testing.T) {
	tr := newFakeTransport("node-a")
	n, err := NewNode(NodeConfig{Self: "node-a", Advertise: "a:1", Transport: tr, Health: newFakeHealth(), QueueDepth: 1, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	h := n.Handler()
	h("node-b", nil) // a nil envelope is ignored, not dereferenced
	task, _ := protocol.NewEnvelope(protocol.TypeTask, "node-b", "node-a", protocol.TaskPayload{TaskID: "t1"})
	// The loop is not running, so nothing drains: the first fills the queue, the
	// next two are shed. Neither call blocks.
	h("node-b", task)
	h("node-b", task)
	h("node-b", task)
	if got := n.Status().Dropped; got != 2 {
		t.Fatalf("Dropped = %d, want 2", got)
	}
}

func TestHandlerControlPlaneUnblocksWhenNodeStops(t *testing.T) {
	tr := newFakeTransport("node-a")
	n, err := NewNode(NodeConfig{Self: "node-a", Advertise: "a:1", Transport: tr, Health: newFakeHealth(), QueueDepth: 1, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	h := n.Handler()
	ack, _ := protocol.NewEnvelope(protocol.TypeJoinAck, "node-b", "node-a", protocol.JoinAckPayload{Accepted: true})
	h("node-b", ack) // fills the queue

	blocked := make(chan struct{})
	go func() {
		h("node-b", ack) // control plane: waits for space or for the node to stop
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("control-plane Handler returned with a full queue and no loop")
	default:
	}

	// Run with an already-cancelled context: the loop must exit rather than
	// drain, and exiting must release the parked reader.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("Handler stayed blocked after Run returned")
	}
	if err := n.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run = %v, want ErrAlreadyRunning", err)
	}
	if err := n.settle(context.Background(), nil); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("settle after stop = %v, want ErrAlreadyRunning", err)
	}
	// Handler after stop is a no-op for control frames too.
	h("node-b", ack)
}

func TestShutdownBroadcastsLeaveAndJoinsProbes(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.hs.stall(addrOf("node-b"))
	h.peerUp("node-b", 1)

	// Start a round that will never finish on its own, then tick again: the
	// second tick is skipped rather than stacking a second round.
	h.clock.Advance(probeEvery)
	h.clock.Advance(probeEvery)

	h.stop()
	leaves := h.tr.broadcastOf(protocol.TypeLeave)
	if len(leaves) != 1 {
		t.Fatalf("LEAVE broadcasts = %d, want 1", len(leaves))
	}
	p, err := protocol.PayloadOf[protocol.LeavePayload](leaves[0])
	if err != nil || p.Reason != "shutdown" {
		t.Fatalf("LEAVE payload = %+v, %v", p, err)
	}
	// The cancelled probe was discarded, not counted.
	if st := h.node.Status(); st.Missed["node-b"] != 0 {
		t.Fatalf("shutdown manufactured a missed beat: %d", st.Missed["node-b"])
	}
	h.hs.mu.Lock()
	calls := h.hs.calls
	h.hs.mu.Unlock()
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1 (second tick skipped while in flight)", calls)
	}
}

func TestProbeRoundBudgetFiresFromTheClock(t *testing.T) {
	// A strategy that never returns is bounded by the node's round budget, which
	// is a Clock timer, so the test fires it. The strategy reports cancellation
	// for that (it is the caller's deadline), which the loop correctly discards:
	// the node's budget is a backstop against a broken strategy, not the signal.
	h := newHarness(t, "node-a", nil)
	h.hs.stall(addrOf("node-b"))
	h.peerUp("node-b", 1)
	h.clock.Advance(probeEvery)
	h.barrier() // the round, and its deadline timer, are registered
	h.clock.Advance(h.node.cfg.ProbeTimeout)
	h.settle() // waits for the cancelled round to be applied
	st := h.status()
	if st.Missed["node-b"] != 0 {
		t.Fatalf("Missed = %d, want 0", st.Missed["node-b"])
	}
	if h.node.probing {
		t.Fatal("round still marked in flight after its budget fired")
	}
	// The loop is free again: the next tick starts a new round.
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.mu.Lock()
	delete(h.hs.block, addrOf("node-b"))
	h.hs.mu.Unlock()
	h.probeRound()
	if got := h.status().Scores["node-b"]; got != 1.0 {
		t.Fatalf("score after recovery = %v, want 1.0", got)
	}
}

// The real strategy against a black-holed peer. The strategy's own timeout is
// the tighter one, so it -- not the node's round budget -- fires, the parent
// context is still live, and the failure is classified as unreachable.
func TestRealLatencyStrategyTimesOutBlackHoledPeer(t *testing.T) {
	const strategyTimeout = 20 * time.Millisecond
	stalling := func(ctx context.Context, _ protocol.NodeAddress) (time.Duration, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	strategy := health.NewLatencyHealthStrategy(health.Config{Probe: stalling, ProbeTimeout: strategyTimeout})
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		c.Health = strategy
		c.ProbeTimeout = 10 * strategyTimeout // strictly looser than the strategy's
	})
	h.peerUp("node-b", 1)
	h.probeRound() // settle waits out the strategy's real-time timeout

	st := h.status()
	if st.Missed["node-b"] != 1 {
		t.Fatalf("Missed = %d, want 1", st.Missed["node-b"])
	}
	if !math.IsNaN(st.Scores["node-b"]) {
		t.Fatalf("score = %v, want NaN", st.Scores["node-b"])
	}
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))
	h.probeRound()
	if got := h.status().Missed["node-b"]; got != 2 {
		t.Fatalf("Missed = %d after second round, want 2", got)
	}
}

// The failure mode this guards against: with the node's budget at or below the
// strategy's, the node cancels first and the black-holed peer is never marked
// missed. Pinned so the invariant cannot be quietly lost.
func TestRoundBudgetTighterThanStrategyHidesFailures(t *testing.T) {
	const strategyTimeout = 200 * time.Millisecond
	stalling := func(ctx context.Context, _ protocol.NodeAddress) (time.Duration, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	strategy := health.NewLatencyHealthStrategy(health.Config{Probe: stalling, ProbeTimeout: strategyTimeout})
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		c.Health = strategy
		c.ProbeTimeout = strategyTimeout / 2 // misconfigured: tighter than the strategy
	})
	h.peerUp("node-b", 1)
	h.clock.Advance(probeEvery)
	h.barrier()
	h.clock.Advance(h.node.cfg.ProbeTimeout) // the node's budget fires first
	h.settle()
	if got := h.status().Missed["node-b"]; got != 0 {
		t.Fatalf("Missed = %d; expected the misconfiguration to hide the failure", got)
	}
}

func TestOnFrameConsumesBeforeDispatch(t *testing.T) {
	var seen int
	var mu sync.Mutex
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		c.OnFrame = func(peer protocol.NodeID, env *protocol.Envelope) bool {
			mu.Lock()
			defer mu.Unlock()
			seen++
			return env.Type == protocol.TypeLeave
		}
	})
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeLeave, protocol.LeavePayload{})
	if m, _ := h.status().View.Get("node-b"); m.State != StateAlive {
		t.Fatal("a consumed LEAVE reached the node")
	}
	h.frame("node-b", protocol.TypeElectionResult, protocol.ElectionResultPayload{Term: 3})
	if _, ok := h.status().Claims["node-b"]; !ok {
		t.Fatal("an unconsumed frame did not reach the node")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 2 {
		t.Fatalf("OnFrame saw %d frames, want 2", seen)
	}
}

func TestTransportEventsClosedIsSurvived(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	close(h.tr.events)
	h.settle()
	h.settle() // the drain path sees the closed channel too
	if st := h.status(); st.View.Size() != 2 {
		t.Fatalf("view = %d after events closed, want 2", st.View.Size())
	}
	h.clock.Advance(floorEvery)
	h.settle()
}

func TestSettleHonoursContext(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.node.settle(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("settle with cancelled ctx = %v", err)
	}
	ran := false
	if err := h.node.settle(h.ctx, func() { ran = true }); err != nil || !ran {
		t.Fatalf("settle(fn) = %v, ran=%v", err, ran)
	}
}

// ---- self-reported scores and convergence ----

func TestSelfScoreIsGossipedWhenItMovesBeyondHysteresis(t *testing.T) {
	h := newHarness(t, "node-a", nil) // Election.Hysteresis 0.1 in baseConfig
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	// The only announcement so far is the role change from electing itself.
	base := len(h.tr.broadcastOf(protocol.TypeMembershipDelta))
	if base != 1 {
		t.Fatalf("announcements before any probe = %d, want 1 (self-election)", base)
	}
	announced := func() []*protocol.Envelope { return h.tr.broadcastOf(protocol.TypeMembershipDelta)[base:] }

	h.probeRound() // self: 0 -> 1.0, announced
	ann := announced()
	if len(ann) != 1 {
		t.Fatalf("announcements = %d, want 1", len(ann))
	}
	p, _ := protocol.PayloadOf[protocol.MembershipDeltaPayload](ann[0])
	if len(p.Members) != 1 || p.Members[0].ID != "node-a" || p.Members[0].Score != 1.0 {
		t.Fatalf("announcement = %+v", p)
	}
	if got := h.status().Reported["node-a"]; got != 1.0 {
		t.Fatalf("Reported[self] = %v, want 1.0", got)
	}

	h.probeRound() // unchanged: silent
	h.hs.set(addrOf("node-b"), 1.05)
	h.probeRound() // moved by less than the margin: silent, table unchanged
	if n := len(announced()); n != 1 {
		t.Fatalf("announcements after a sub-margin wobble = %d, want 1", n)
	}
	if got := h.status().Reported["node-a"]; got != 1.0 {
		t.Fatalf("Reported[self] = %v after a sub-margin wobble, want 1.0 retained", got)
	}

	h.hs.set(addrOf("node-b"), 5.0)
	h.probeRound()
	ann = announced()
	if len(ann) != 2 {
		t.Fatalf("announcements after a real move = %d, want 2", len(ann))
	}
	p, _ = protocol.PayloadOf[protocol.MembershipDeltaPayload](ann[1])
	if p.Members[0].Score != 5.0 {
		t.Fatalf("announced score = %v, want 5.0", p.Members[0].Score)
	}
}

func TestUnmeasuredReportOnTheWireStaysIneligible(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.report("node-b", 1, math.NaN()) // MarshalJSON turns this into -1 on the wire
	st := h.status()
	if got := st.Reported["node-b"]; !math.IsNaN(got) {
		t.Fatalf("Reported[node-b] = %v, want NaN (unmeasured)", got)
	}
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))

	// A later report with a real score makes it electable, and a relayed
	// record without a score does not erase that report.
	h.report("node-b", 1, 0.5)
	h.hs.set(addrOf("node-b"), 0.5)
	h.probeRound() // self reports 0.5 too; node-a wins the tie on ID
	h.frame("node-c", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 1, Role: "worker", State: "alive", Score: protocol.UnmeasuredScore,
	}}})
	if got := h.status().Reported["node-b"]; got != 0.5 {
		t.Fatalf("a scoreless relay erased node-b's report: %v", got)
	}
}

// meshHub routes envelopes between real Nodes in-process: Send calls the
// target's Handler on the caller's goroutine, exactly as a reader goroutine
// would, so the whole exchange is driven by settle rather than by time.
type meshHub struct {
	// delivered counts frames handed to a node's Handler; quiesce uses it to
	// tell a quiet mesh from one that is still talking.
	delivered atomic.Uint64

	mu    sync.Mutex
	nodes map[protocol.NodeID]*Node
	// drop, if set, is asked about every frame; true loses it silently, as a
	// network would. broadcast tells a Broadcast fan-out from a Send. Guarded
	// by mu.
	drop func(from, to protocol.NodeID, env *protocol.Envelope, broadcast bool) bool
}

func (h *meshHub) setDrop(fn func(from, to protocol.NodeID, env *protocol.Envelope, broadcast bool) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop = fn
}

type meshEndpoint struct {
	self   protocol.NodeID
	hub    *meshHub
	events chan network.PeerEvent

	mu        sync.Mutex
	broadcast []*protocol.Envelope
	sent      []sentEnv
}

func (e *meshEndpoint) Self() protocol.NodeID { return e.self }

func (e *meshEndpoint) Send(_ context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	e.hub.mu.Lock()
	target := e.hub.nodes[to]
	drop := e.hub.drop
	e.hub.mu.Unlock()
	if target == nil {
		return network.ErrUnknownPeer
	}
	e.mu.Lock()
	e.sent = append(e.sent, sentEnv{to: to, env: env})
	e.mu.Unlock()
	if drop != nil && drop(e.self, to, env, false) {
		return nil
	}
	e.hub.delivered.Add(1)
	target.Handler()(e.self, env)
	return nil
}

func (e *meshEndpoint) Broadcast(_ context.Context, env *protocol.Envelope) int {
	e.hub.mu.Lock()
	targets := make([]*Node, 0, len(e.hub.nodes))
	for id, n := range e.hub.nodes {
		if id != e.self && (e.hub.drop == nil || !e.hub.drop(e.self, id, env, true)) {
			targets = append(targets, n)
		}
	}
	e.hub.mu.Unlock()
	e.mu.Lock()
	e.broadcast = append(e.broadcast, env)
	e.mu.Unlock()
	for _, n := range targets {
		e.hub.delivered.Add(1)
		n.Handler()(e.self, env)
	}
	return len(targets)
}

func (e *meshEndpoint) Peers() []network.PeerInfo        { return nil }
func (e *meshEndpoint) Events() <-chan network.PeerEvent { return e.events }

func (e *meshEndpoint) lastElectionResult(t *testing.T) protocol.ElectionResultPayload {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.broadcast) - 1; i >= 0; i-- {
		if e.broadcast[i].Type == protocol.TypeElectionResult {
			p, err := protocol.PayloadOf[protocol.ElectionResultPayload](e.broadcast[i])
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("no ELECTION_RESULT broadcast")
	return protocol.ElectionResultPayload{}
}

func (e *meshEndpoint) countSent(typ protocol.MessageType) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, s := range e.sent {
		if s.env.Type == typ {
			n++
		}
	}
	return n
}

type meshNode struct {
	id    protocol.NodeID
	node  *Node
	ep    *meshEndpoint
	hs    *fakeHealth
	clock *FakeClock
}

// quiesce settles every node repeatedly until a full pass delivers no new frame.
//
// Queue length is not a usable idleness signal: a loop that has dequeued a
// frame and is still handling it shows an empty queue. A delivery count is.
// Every frame delivered before a pass is either already handled or is handled
// inside that node's settle during the pass (settle runs between events, then
// drains), so a pass with no new deliveries means nothing is left in flight.
func quiesce(t *testing.T, ctx context.Context, nodes []*meshNode) {
	t.Helper()
	hub := nodes[0].ep.hub
	for pass := 0; pass < 64; pass++ {
		before := hub.delivered.Load()
		for _, m := range nodes {
			if err := m.node.settle(ctx, nil); err != nil {
				t.Fatalf("settle %s: %v", m.id, err)
			}
		}
		if hub.delivered.Load() == before {
			return
		}
	}
	t.Fatal("mesh did not quiesce")
}

// Three real nodes with ASYMMETRIC local measurements. Ranking on local RTTs
// would have n2 elect n3 (its closest peer) while n1 and n3 elect differently;
// ranking on self-reported medians makes all three agree on n2, and the
// affinity JOINs are accepted because the node they target believes it leads.
// newMesh starts one real Node per id, all routed through one meshHub. local
// gives each node's measured score per peer (absent means unreachable). Nodes
// are registered with the hub only once all have started, so no frame reaches a
// node that is not yet running. The mesh is torn down by t.Cleanup.
func newMesh(t *testing.T, idList []protocol.NodeID, local map[protocol.NodeID]map[protocol.NodeID]float64, mutate func(*NodeConfig)) (context.Context, *meshHub, []*meshNode) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := &meshHub{nodes: make(map[protocol.NodeID]*Node)}
	var (
		nodes []*meshNode
		wg    sync.WaitGroup
	)
	t.Cleanup(func() { cancel(); wg.Wait() })
	for _, id := range idList {
		ep := &meshEndpoint{self: id, hub: hub, events: make(chan network.PeerEvent, 64)}
		hs := newFakeHealth()
		for peer, s := range local[id] {
			hs.set(addrOf(peer), s)
		}
		clock := NewFakeClock(epoch)
		cfg := baseConfig(id, nil, hs, clock)
		cfg.Transport = ep
		cfg.Incarnation = 1 // must match what PeerUp reports, as a real HELLO would
		if mutate != nil {
			mutate(&cfg)
		}
		n, err := NewNode(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m := &meshNode{id: id, node: n, ep: ep, hs: hs, clock: clock}
		nodes = append(nodes, m)
		wg.Add(1)
		go func() { defer wg.Done(); _ = n.Run(ctx) }()
		if err := n.settle(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	hub.mu.Lock()
	for _, m := range nodes {
		hub.nodes[m.id] = m.node
	}
	hub.mu.Unlock()
	return ctx, hub, nodes
}

// linkUp reports a handshaken connection between a and b to both ends.
func linkUp(a, b *meshNode) {
	a.ep.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: b.id, Advertise: addrOf(b.id), Incarnation: 1}}
	b.ep.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{ID: a.id, Advertise: addrOf(a.id), Incarnation: 1}}
}

// fullMesh links every pair and waits for the swarm to settle.
func fullMesh(t *testing.T, ctx context.Context, nodes []*meshNode) {
	t.Helper()
	for i, a := range nodes {
		for _, b := range nodes[i+1:] {
			linkUp(a, b)
		}
	}
	quiesce(t, ctx, nodes)
}

// advanceMesh moves every node's clock by d, one node at a time, and waits for
// the swarm to settle after each.
func advanceMesh(t *testing.T, ctx context.Context, nodes []*meshNode, d time.Duration) {
	t.Helper()
	for _, m := range nodes {
		m.clock.Advance(d)
		if err := m.node.settle(ctx, nil); err != nil {
			t.Fatal(err)
		}
		quiesce(t, ctx, nodes)
	}
}

// Three real nodes with ASYMMETRIC local measurements. Ranking on local RTTs
// would have n2 elect n3 (its closest peer) while n1 and n3 elect differently;
// ranking on self-reported medians makes all three agree on n2, and the
// affinity JOINs are accepted because the node they target believes it leads.
func TestThreeRealNodesConvergeOnSelfReportedScores(t *testing.T) {
	local := map[protocol.NodeID]map[protocol.NodeID]float64{
		"n1": {"n2": 10, "n3": 10}, // n1 reports 10
		"n2": {"n1": 1, "n3": 2},   // n2 reports 1.5, but locally sees n1 as closest
		"n3": {"n1": 1, "n2": 9},   // n3 reports 5, and locally sees n1 as closest
	}
	ctx, _, nodes := newMesh(t, ids("n1", "n2", "n3"), local, nil)

	// Full mesh: every node sees every other come up.
	fullMesh(t, ctx, nodes)

	// Every node booted alone and elected itself. Before any measurement all
	// three report 0 and all three claim leadership; the claims are the
	// incumbency input to hysteresis, and being the same on every node they
	// collapse to the deterministic tie-break everywhere: n1.
	for _, m := range nodes {
		requireIDs(t, string(m.id)+" initial Leaders", m.node.Status().Leaders, ids("n1"))
	}

	// Two probe rounds each; every node announces its median and the swarm
	// re-elects on the reported scores. The second round is what a real
	// deployment has anyway, and it retries any JOIN that landed on n2 before
	// n2 had processed its own promotion.
	for round := 0; round < 2; round++ {
		for _, m := range nodes {
			m.clock.Advance(probeEvery)
			if err := m.node.settle(ctx, nil); err != nil {
				t.Fatal(err)
			}
		}
		quiesce(t, ctx, nodes)
	}

	wantReported := map[protocol.NodeID]float64{"n1": 10, "n2": 1.5, "n3": 5}
	for _, m := range nodes {
		st := m.node.Status()
		requireIDs(t, string(m.id)+" Leaders", st.Leaders, ids("n2"))
		for id, want := range wantReported {
			if got := st.Reported[id]; got != want {
				t.Errorf("%s: Reported[%s] = %v, want %v", m.id, id, got, want)
			}
		}
		if m.id == "n2" {
			if st.Role != RoleLeader || st.Leader != "n2" {
				t.Errorf("n2: %s, want leader", st)
			}
			requireIDs(t, "n2 Attached", st.Attached, ids("n1", "n3"))
		} else {
			if st.Role != RoleWorker || st.Leader != "n2" {
				t.Errorf("%s: %s, want worker attached to n2", m.id, st)
			}
		}
		// No rejection storm: a handful of JOINs at most, not thousands.
		if joins := m.ep.countSent(protocol.TypeJoinCluster); joins > 3 {
			t.Errorf("%s sent %d JOINs", m.id, joins)
		}
		// Roles converged too: every view shows n2 as the only claimant.
		requireIDs(t, string(m.id)+" View.Leaders", st.View.Leaders(), ids("n2"))
	}

	// The election result every node announced is byte-identical apart from
	// the local term counter.
	var first []byte
	for _, m := range nodes {
		p := m.ep.lastElectionResult(t)
		p.Term = 0
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = b
		} else if string(b) != string(first) {
			t.Errorf("%s announced %s, n1 announced %s", m.id, b, first)
		}
	}
}

func TestConnectHookCalledOncePerNewAddress(t *testing.T) {
	var (
		mu     sync.Mutex
		dialed []protocol.NodeAddress
	)
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		c.Connect = func(addr protocol.NodeAddress) {
			mu.Lock()
			defer mu.Unlock()
			dialed = append(dialed, addr)
		}
	})
	got := func() []protocol.NodeAddress {
		mu.Lock()
		defer mu.Unlock()
		return append([]protocol.NodeAddress(nil), dialed...)
	}

	// The seed's HELLO lists who it knows: ourselves, itself, and two strangers.
	h.tr.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{
		ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 1,
		KnownPeers: []protocol.NodeAddress{addrOf("node-a"), addrOf("node-b"), addrOf("node-c"), addrOf("node-d"), ""},
	}}
	h.settle()
	if want := []protocol.NodeAddress{addrOf("node-c"), addrOf("node-d")}; fmt.Sprint(got()) != fmt.Sprint(want) {
		t.Fatalf("dialed = %v, want %v", got(), want)
	}

	// The same HELLO again, and node-c actually connecting: nothing new.
	h.tr.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{
		ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 1,
		KnownPeers: []protocol.NodeAddress{addrOf("node-c"), addrOf("node-d")},
	}}
	h.peerUp("node-c", 1)
	if len(got()) != 2 {
		t.Fatalf("dialed = %v after repeats, want 2 entries", got())
	}

	// Gossip naming a new alive member dials it; a dead one, a known one and a
	// blank address do not.
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{
		{ID: "node-e", Advertise: addrOf("node-e"), Incarnation: 1, State: "alive"},
		{ID: "node-f", Advertise: addrOf("node-f"), Incarnation: 1, State: "dead"},
		{ID: "node-c", Advertise: addrOf("node-c"), Incarnation: 1, State: "alive"},
		{ID: "node-g", Advertise: "", Incarnation: 1, State: "alive"},
	}})
	if want := []protocol.NodeAddress{addrOf("node-c"), addrOf("node-d"), addrOf("node-e")}; fmt.Sprint(got()) != fmt.Sprint(want) {
		t.Fatalf("dialed = %v, want %v", got(), want)
	}
}

func TestNoConnectHookIsFine(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.tr.events <- network.PeerEvent{Kind: network.PeerUp, Peer: network.PeerInfo{
		ID: "node-b", Advertise: addrOf("node-b"), Incarnation: 1, KnownPeers: []protocol.NodeAddress{addrOf("node-c")},
	}}
	h.settle()
}

// Records without Seq (an older build) are unordered, so their roles keep the
// first-hand-only rule. Seq-ordered relays are covered in converge_test.go.
func TestRelayedRoleIsNotBelieved(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	// node-c claims leadership itself: believed.
	h.reportRole("node-c", 1, 0.5, RoleLeader)
	if m, _ := h.status().View.Get("node-c"); m.Role != RoleLeader {
		t.Fatalf("first-hand claim ignored: %+v", m)
	}
	// node-b relays a stale view saying node-c is a worker: not believed, but
	// the rest of the record (state, score) merges as usual.
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-c", Advertise: addrOf("node-c"), Incarnation: 1, Role: "worker", State: "alive", Score: 0.7,
	}}})
	m, _ := h.status().View.Get("node-c")
	if m.Role != RoleLeader || m.Score != 0.7 {
		t.Fatalf("relayed record: %+v, want role leader kept and score 0.7 merged", m)
	}
	// A relay about a member we have never heard of introduces it as a worker.
	h.frame("node-b", protocol.TypeMembershipDelta, protocol.MembershipDeltaPayload{Members: []protocol.MemberRecord{{
		ID: "node-d", Advertise: addrOf("node-d"), Incarnation: 1, Role: "leader", State: "alive", Score: 0.1,
	}}})
	if m, _ := h.status().View.Get("node-d"); m.Role != RoleWorker {
		t.Fatalf("relayed leader claim believed for a new member: %+v", m)
	}
}

// ---- HIGH-1 regressions: an unmeasurable node must not claim the best score ----

// A newcomer that can measure none of its peers joins a swarm with a healthy
// incumbent. Before f9e3b29 it seeded and then reported 0 -- the best score --
// and, being the lowest ID as well, displaced node-b on its first evaluate. It
// must stay a worker, both before its first probe round and after it.
func TestBlindNewcomerCannotDisplaceHealthyIncumbent(t *testing.T) {
	h := newHarness(t, "node-a", nil) // no scores configured: every probe fails
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.reportRole("node-b", 1, 1.0, RoleLeader)
	h.report("node-c", 1, 2.0)

	check := func(when string) {
		t.Helper()
		st := h.status()
		requireIDs(t, "Leaders "+when, st.Leaders, ids("node-b"))
		if st.Role != RoleWorker {
			t.Fatalf("%s: blind newcomer role = %s, want worker", when, st.Role)
		}
		if got := st.Reported["node-a"]; !math.IsNaN(got) {
			t.Fatalf("%s: blind newcomer reports %v, want unmeasured", when, got)
		}
		// Nothing it said about itself may have carried a score.
		for _, env := range h.tr.broadcastOf(protocol.TypeMembershipDelta) {
			p, err := protocol.PayloadOf[protocol.MembershipDeltaPayload](env)
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range p.Members {
				if rec.ID == "node-a" && rec.Measured() {
					t.Fatalf("%s: node-a announced a measured score %v", when, rec.Score)
				}
			}
		}
	}
	check("before any probe round")

	h.probeRound()
	st := h.status()
	if st.Missed["node-b"] != 1 || st.Missed["node-c"] != 1 {
		t.Fatalf("probes did not fail as arranged: Missed = %v", st.Missed)
	}
	check("after a failed probe round")
}

// Every node in the swarm is blind. Nobody is eligible, so Elect falls back to
// the lowest alive ID, every node agrees on it, and no node -- despite having
// peers -- reports the score 0 that would have made that agreement accidental.
func TestAllUnmeasuredSwarmFallsBackToLowestID(t *testing.T) {
	ctx, _, nodes := newMesh(t, ids("n1", "n2", "n3"), nil, nil)
	fullMesh(t, ctx, nodes)
	for round := 0; round < 2; round++ {
		advanceMesh(t, ctx, nodes, probeEvery)
	}

	for _, m := range nodes {
		st := m.node.Status()
		requireIDs(t, string(m.id)+" Leaders", st.Leaders, ids("n1"))
		requireIDs(t, string(m.id)+" View.Leaders", st.View.Leaders(), ids("n1"))
		if st.View.Size() != 3 {
			t.Fatalf("%s sees %d alive, want 3", m.id, st.View.Size())
		}
		for _, peer := range nodes {
			if peer.id != m.id && st.Missed[peer.id] == 0 {
				t.Fatalf("%s: probe of %s did not fail as arranged: %v", m.id, peer.id, st.Missed)
			}
		}
		if got := st.Scores[m.id]; !math.IsNaN(got) {
			t.Errorf("%s self score = %v with peers and no measurement, want NaN", m.id, got)
		}
		for _, mem := range st.View.Members {
			if !math.IsNaN(mem.Score) {
				t.Errorf("%s holds a score %v for %s in an all-blind swarm", m.id, mem.Score, mem.ID)
			}
		}
	}
}
