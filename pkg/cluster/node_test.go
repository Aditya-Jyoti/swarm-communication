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
	mu       sync.Mutex
	scores   map[protocol.NodeAddress]float64
	errs     map[protocol.NodeAddress]error
	block    map[protocol.NodeAddress]bool
	retained []map[protocol.NodeAddress]struct{}
	calls    int
}

func newFakeHealth() *fakeHealth {
	return &fakeHealth{
		scores: make(map[protocol.NodeAddress]float64),
		errs:   make(map[protocol.NodeAddress]error),
		block:  make(map[protocol.NodeAddress]bool),
	}
}

func (h *fakeHealth) Name() string { return "fake" }

func (h *fakeHealth) EvaluateScore(ctx context.Context, target protocol.NodeAddress) (float64, error) {
	h.mu.Lock()
	h.calls++
	blocked := h.block[target]
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
	probeEvery = time.Second
	floorEvery = 30 * time.Second
)

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
		Self:          self,
		Advertise:     addrOf(self),
		Transport:     tr,
		Health:        hs,
		Clock:         clock,
		ProbeInterval: probeEvery,
		ElectionFloor: floorEvery,
		ProbeTimeout:  probeEvery / 2,
		Election:      Config{Threshold: DefaultThreshold, Hysteresis: 0.1},
		Rehome:        0.1,
		Logger:        quietLogger(),
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
			c.ProbeTimeout != DefaultProbeTimeout || c.Rehome != DefaultHysteresis ||
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

// threeNodes builds a swarm where self is the median and node-b is the clear
// winner, then runs one probe round.
func threeNodes(t *testing.T, self protocol.NodeID) *harness {
	t.Helper()
	h := newHarness(t, self, nil)
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 3.0)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
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
	// Self is scored as the median of its peers.
	if got := st.Scores["node-z"]; got != 2.0 {
		t.Fatalf("self score = %v, want median 2.0", got)
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

	// Roles are applied in the table for every member.
	if m, _ := st.View.Get("node-b"); m.Role != RoleLeader {
		t.Fatalf("node-b role = %s, want leader", m.Role)
	}
	if m, _ := st.View.Get("node-c"); m.Role != RoleWorker {
		t.Fatalf("node-c role = %s, want worker", m.Role)
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
	// Four nodes at threshold 0.5 elect two leaders: node-b (1.0) and node-c (1.5).
	// Self scores the median 1.5 and loses the tie to node-c on ID.
	h := newHarness(t, "node-z", func(c *NodeConfig) { c.Election.Threshold = 0.5 })
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 1.5)
	h.hs.set(addrOf("node-d"), 10.0)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.peerUp("node-d", 1)
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

func TestPeerDownFailureTriggersReelection(t *testing.T) {
	h := threeNodes(t, "node-a")
	before := h.status()
	requireIDs(t, "Leaders", before.Leaders, ids("node-b"))
	broadcastsBefore := len(h.tr.broadcastOf(protocol.TypeElectionResult))

	h.peerDown("node-b", network.DispositionPeerDied)

	st := h.status()
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

func TestPeerDownDispositions(t *testing.T) {
	tests := []struct {
		name  string
		disp  network.Disposition
		alive bool // still present in the view
		state State
	}{
		{"clean close removes", network.DispositionCleanClose, false, StateAlive},
		{"protocol violation marks dead", network.DispositionProtocolViolation, true, StateDead},
		{"timeout marks dead", network.DispositionTimeout, true, StateDead},
		{"other marks dead", network.DispositionOther, true, StateDead},
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
	if len(h.tr.broadcastOf(protocol.TypeMembershipDelta)) != 0 {
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
	ann := h.tr.broadcastOf(protocol.TypeMembershipDelta)
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

func TestLeaveRemovesPeer(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))

	h.frame("node-b", protocol.TypeLeave, protocol.LeavePayload{Reason: "scale-down"})
	st := h.status()
	if _, ok := st.View.Get("node-b"); ok {
		t.Fatal("node-b still in view after LEAVE")
	}
	requireIDs(t, "Attached", st.Attached, nil)

	// A LEAVE for a peer we never knew is harmless.
	h.frame("node-q", protocol.TypeLeave, nil)
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
	// With node-b unmeasurable, self (score 0: no valid peers) leads.
	requireIDs(t, "Leaders", st.Leaders, ids("node-a"))
	if st.Scores["node-a"] != 0 {
		t.Fatalf("self score = %v with no valid peers, want 0", st.Scores["node-a"])
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

func TestDemotedLeaderDropsAttached(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.peerUp("node-b", 1)
	h.peerUp("node-c", 1)
	h.frame("node-b", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-b"})
	requireIDs(t, "Attached", h.status().Attached, ids("node-b"))

	// Scores arrive; node-b is clearly best and self is the median.
	h.hs.set(addrOf("node-b"), 1.0)
	h.hs.set(addrOf("node-c"), 3.0)
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
	if _, ok := h.status().View.Get("node-b"); !ok {
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
