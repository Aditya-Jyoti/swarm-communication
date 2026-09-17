package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/health"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// Defaults for NodeConfig. Each is a duration the loop keys off; none is a
// synchronisation primitive.
const (
	DefaultProbeInterval = 1 * time.Second
	DefaultElectionFloor = 30 * time.Second
	// DefaultProbeTimeout is twice the strategy's own per-probe budget. See
	// NodeConfig.ProbeTimeout for why the node's budget must be the looser one.
	DefaultProbeTimeout = 2 * health.DefaultProbeTimeout
	DefaultQueueDepth   = 256
	// leaveTimeout bounds the LEAVE broadcast on shutdown. It is a real-time bound,
	// not a synchronisation: shutdown must not wait on a peer whose send queue is
	// full, and a LEAVE that does not get out costs the peers one failure-detector
	// round, which they must survive anyway (a SIGKILL never sends one).
	leaveTimeout = 500 * time.Millisecond
)

// ErrAlreadyRunning is returned by Run when the node's loop is already running or
// has already exited. A Node runs exactly once.
var ErrAlreadyRunning = errors.New("cluster: node already running")

// HealthRetainer is implemented by strategies that keep per-target history and
// want it bounded to live membership (health.LatencyHealthStrategy does). It is
// optional: the HealthStrategy contract does not include it, and a strategy with
// no history has nothing to retain.
type HealthRetainer interface {
	Retain(keep map[protocol.NodeAddress]struct{})
}

// NodeConfig configures a Node. Self, Advertise, Transport and Health are required.
type NodeConfig struct {
	Self        protocol.NodeID
	Advertise   protocol.NodeAddress
	Incarnation int64
	Transport   network.Transport
	// Health scores peers. Its EvaluateScore MUST honour ctx: the node cancels
	// every in-flight probe on shutdown and then joins the probe goroutines, so a
	// strategy that ignores ctx blocks Run from returning.
	Health health.HealthStrategy
	// Clock defaults to RealClock.
	Clock Clock
	// Election parameterises Elect. The zero value takes election.go's defaults.
	Election Config
	// ProbeInterval is how often every alive peer is scored. Default 1s.
	ProbeInterval time.Duration
	// ElectionFloor is the periodic re-election safety net (WORKLOG 2.4). Default 30s.
	ElectionFloor time.Duration
	// ProbeTimeout is the budget for one round of scoring every peer. Default
	// 2 * health.DefaultProbeTimeout. Must be > 0.
	//
	// INVARIANT: the round budget must strictly exceed the strategy's own
	// per-probe timeout. LatencyHealthStrategy decides "cancelled" versus
	// "unreachable" by asking whether the *caller's* context is done at the
	// moment a probe fails. If the node's deadline fires first, every probe of a
	// black-holed peer reports cancellation, the loop discards the sample as the
	// contract requires, and the peer keeps its last good score forever. The
	// node's budget exists to bound the round against a strategy that misbehaves,
	// not to be the deadline that normally fires. Whoever assembles the two
	// (cmd/swarm-node) must keep this ordering.
	ProbeTimeout time.Duration
	// Rehome is the hysteresis margin for ShouldRehome. Zero means
	// DefaultHysteresis; a negative value means "no damping" (ShouldRehome treats
	// it as 0), which is legal in tests and unwise in production.
	Rehome float64
	// QueueDepth sizes each inbound frame queue. Default 256.
	QueueDepth int
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// OnFrame is an optional pre-handler, typically MeshProber.HandleFrame. It runs
	// on the reader goroutine; if it returns true the frame is consumed and the
	// node never sees it.
	OnFrame func(peer protocol.NodeID, env *protocol.Envelope) bool
}

func (c NodeConfig) withDefaults() NodeConfig {
	if c.Clock == nil {
		c.Clock = RealClock{}
	}
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = DefaultProbeInterval
	}
	if c.ElectionFloor <= 0 {
		c.ElectionFloor = DefaultElectionFloor
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = DefaultProbeTimeout
	}
	if c.Rehome == 0 {
		c.Rehome = DefaultHysteresis
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = DefaultQueueDepth
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Status is an immutable snapshot of a node for telemetry. Every map and slice is
// a copy; mutating one cannot reach the node.
type Status struct {
	Self        protocol.NodeID
	Incarnation int64
	Role        Role
	Term        uint64
	// Leader is the leader this node is attached to: self when leading, empty
	// when detached and waiting for a JOIN_ACK or a usable score.
	Leader  protocol.NodeID
	Leaders []protocol.NodeID
	View    View
	Scores  map[protocol.NodeID]float64
	// Missed counts consecutive failed probes per peer; reset on success.
	Missed   map[protocol.NodeID]int
	Degraded bool
	// HighWater is the largest alive count this node has observed.
	HighWater int
	// Attached lists the workers attached to this node. Leaders only.
	Attached []protocol.NodeID
	// Claims records the last ELECTION_RESULT each peer announced. Observability
	// only: a peer's claim never changes this node's roles.
	Claims map[protocol.NodeID]protocol.ElectionResultPayload
	// Dropped counts data-plane frames shed at the inbound queue.
	Dropped uint64
}

// Node is the state machine that wires membership, election, affinity and the
// transport together.
//
// # Goroutine ownership
//
// Run owns one goroutine, the event loop, and it is the sole owner of every field
// in the "loop-owned" block below. Nothing else reads or writes them; the loop
// publishes a Status snapshot and everything outside consumes that. This is the
// whole concurrency design: one writer, no locks on the hot state, and the
// question "which goroutine mutated the leader set" has exactly one answer.
//
// The loop starts short-lived probe goroutines (one per peer per round, plus one
// collector per round). They never touch node state: they compute a result and
// hand it back through probeCh. probeWG joins them on shutdown, and their context
// is derived from Run's, so a cancelled Run cannot leave one behind.
//
// Transport reader goroutines call Handler. It touches only the inbound queues
// and the dropped counter.
type Node struct {
	cfg   NodeConfig
	log   *slog.Logger
	table *Table // has its own mutex; safe to read from any goroutine

	// Inbound queues, fed by Handler on reader goroutines and drained by the loop.
	// Two queues so a flood of TASK frames cannot delay a JOIN_ACK: the control
	// queue is never shed, the data queue is dropped on overflow.
	ctrlIn chan inboundFrame
	dataIn chan inboundFrame
	// dropped counts shed data-plane frames. Atomic: written by reader goroutines,
	// read by Status.
	dropped atomic.Uint64
	// probeCh carries finished probe rounds back to the loop.
	probeCh chan probeRound
	probeWG sync.WaitGroup
	// calls carries functions to be run on the loop goroutine; see settle.
	calls chan func()

	// running flips once, when Run starts. stopped is closed when Run returns;
	// Handler selects on it so a control-plane send cannot outlive the loop.
	running atomic.Bool
	stopped chan struct{}

	// statusMu guards status. Copy-on-write: the loop builds a complete Status and
	// swaps the pointer, so a reader never sees a half-built snapshot and the lock
	// is held for one pointer assignment.
	statusMu sync.RWMutex
	status   *Status

	// ---- loop-owned state: only the Run goroutine touches anything below ----
	incarnation int64
	term        uint64
	leaders     []protocol.NodeID
	leader      protocol.NodeID
	// hint is a leader named by a JOIN_ACK rejection; consumed by the next evaluate.
	hint      protocol.NodeID
	attached  map[protocol.NodeID]struct{}
	scores    map[protocol.NodeID]float64
	missed    map[protocol.NodeID]int
	claims    map[protocol.NodeID]protocol.ElectionResultPayload
	highWater int
	degraded  bool
	probing   bool // a probe round is in flight
	// events is the transport's event channel; set to nil once it closes so the
	// loop stops selecting on an always-ready closed channel.
	events <-chan network.PeerEvent
	// unknownLogged makes "unknown message type" a once-per-type log line rather
	// than a line per frame from a newer build.
	unknownLogged map[protocol.MessageType]struct{}
}

type inboundFrame struct {
	peer protocol.NodeID
	env  *protocol.Envelope
}

// probeRound is the outcome of scoring every peer once.
type probeRound struct {
	results map[protocol.NodeID]probeResult
}

type probeResult struct {
	score float64
	err   error
}

// NewNode validates cfg and builds a Node with itself as the only member. Run
// must be called to start the loop.
func NewNode(cfg NodeConfig) (*Node, error) {
	switch {
	case cfg.Self == "":
		return nil, errors.New("cluster: NodeConfig.Self is required")
	case cfg.Advertise == "":
		return nil, errors.New("cluster: NodeConfig.Advertise is required")
	case cfg.Transport == nil:
		return nil, errors.New("cluster: NodeConfig.Transport is required")
	case cfg.Health == nil:
		return nil, errors.New("cluster: NodeConfig.Health is required")
	}
	cfg = cfg.withDefaults()
	if cfg.ProbeTimeout <= 0 {
		return nil, fmt.Errorf("cluster: NodeConfig.ProbeTimeout must be > 0, got %v", cfg.ProbeTimeout)
	}

	n := &Node{
		cfg:           cfg,
		log:           cfg.Logger.With("node", cfg.Self),
		table:         NewTable(),
		ctrlIn:        make(chan inboundFrame, cfg.QueueDepth),
		dataIn:        make(chan inboundFrame, cfg.QueueDepth),
		probeCh:       make(chan probeRound),
		calls:         make(chan func()),
		stopped:       make(chan struct{}),
		incarnation:   cfg.Incarnation,
		attached:      make(map[protocol.NodeID]struct{}),
		scores:        make(map[protocol.NodeID]float64),
		missed:        make(map[protocol.NodeID]int),
		claims:        make(map[protocol.NodeID]protocol.ElectionResultPayload),
		unknownLogged: make(map[protocol.MessageType]struct{}),
	}
	n.table.Upsert(Member{
		ID:          cfg.Self,
		Addr:        cfg.Advertise,
		Incarnation: cfg.Incarnation,
		Role:        RoleWorker,
		State:       StateAlive,
	})
	n.publish()
	return n, nil
}

// Table exposes the membership table, for the MeshProber's resolve function and
// for telemetry. It is safe for concurrent use; callers must go through Snapshot.
func (n *Node) Table() *Table { return n.table }

// Resolve maps an advertised address to the NodeID of a non-dead member. It is
// the shape network.NewMeshProber wants for its resolve argument.
func (n *Node) Resolve(addr protocol.NodeAddress) (protocol.NodeID, bool) {
	for _, m := range n.table.Snapshot().Members {
		if m.Addr == addr && m.State != StateDead {
			return m.ID, true
		}
	}
	return "", false
}

// Status returns the latest published snapshot. Safe from any goroutine.
//
// The published snapshot is never handed out directly: two callers would share
// its maps and slices, and one caller's edit would be the other's corruption.
// Every call clones, which costs a few allocations at telemetry rate and buys
// the property the type promises.
func (n *Node) Status() Status {
	n.statusMu.RLock()
	s := n.status.clone()
	n.statusMu.RUnlock()
	// Dropped is read live: it is written by reader goroutines, not the loop, so a
	// snapshot published by the loop could be arbitrarily stale on it.
	s.Dropped = n.dropped.Load()
	return s
}

// clone deep-copies every map and slice in s.
func (s *Status) clone() Status {
	out := *s
	out.Leaders = append([]protocol.NodeID(nil), s.Leaders...)
	out.Attached = append([]protocol.NodeID(nil), s.Attached...)
	out.View.Members = append([]Member(nil), s.View.Members...)
	out.Scores = make(map[protocol.NodeID]float64, len(s.Scores))
	for k, v := range s.Scores {
		out.Scores[k] = v
	}
	out.Missed = make(map[protocol.NodeID]int, len(s.Missed))
	for k, v := range s.Missed {
		out.Missed[k] = v
	}
	out.Claims = make(map[protocol.NodeID]protocol.ElectionResultPayload, len(s.Claims))
	for k, v := range s.Claims {
		v.Leaders = append([]protocol.NodeID(nil), v.Leaders...)
		out.Claims[k] = v
	}
	return out
}

// Handler returns the frame handler to install as PoolConfig.Handler.
//
// It runs on a connection's reader goroutine and routes by plane. Data-plane
// frames are dropped when their queue is full: a lost telemetry sample is a gap
// in a graph. Control-plane frames wait for space, because a lost JOIN_ACK or
// ELECTION_RESULT is a wrong topology; the wait is bounded by the node stopping,
// so a reader can never be parked on a loop that has exited. In practice the
// control queue only fills if the loop is stalled, which the loop's own bounded
// sends are designed to prevent.
func (n *Node) Handler() network.Handler {
	return func(peer protocol.NodeID, env *protocol.Envelope) {
		if env == nil {
			return
		}
		if n.cfg.OnFrame != nil && n.cfg.OnFrame(peer, env) {
			return
		}
		f := inboundFrame{peer: peer, env: env}
		if env.Type.IsDataPlane() {
			select {
			case n.dataIn <- f:
			default:
				n.dropped.Add(1)
				n.log.Warn("inbound data queue full; frame dropped", "peer", peer, "type", env.Type)
			}
			return
		}
		select {
		case n.ctrlIn <- f:
		case <-n.stopped:
		}
	}
}

// Run is the event loop. It returns nil after ctx is done and shutdown has
// completed (LEAVE broadcast, probe goroutines joined), or ErrAlreadyRunning.
func (n *Node) Run(ctx context.Context) error {
	if !n.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer close(n.stopped)

	probeT := n.cfg.Clock.NewTicker(n.cfg.ProbeInterval)
	defer probeT.Stop()
	floorT := n.cfg.Clock.NewTicker(n.cfg.ElectionFloor)
	defer floorT.Stop()

	// A single node must lead itself without waiting for a peer or a tick.
	n.evaluate(ctx)

	n.events = n.cfg.Transport.Events()
	for {
		// ctx is checked before the select rather than only inside it. select
		// picks at random among ready cases, so a loop with a full inbound queue
		// and a cancelled context would keep processing frames about half the
		// time instead of stopping.
		if ctx.Err() != nil {
			n.shutdown()
			return nil
		}
		select {
		case <-ctx.Done():
			n.shutdown()
			return nil
		case ev, ok := <-n.events:
			if !ok {
				// Transport closed under us. Nothing more will arrive on it; stop
				// selecting on a closed channel (which is always ready) and let ctx
				// end the loop.
				n.events = nil
				continue
			}
			n.handlePeerEvent(ctx, ev)
		case f := <-n.ctrlIn:
			n.handleFrame(ctx, f)
		case f := <-n.dataIn:
			n.handleFrame(ctx, f)
		case <-probeT.C():
			n.startProbeRound(ctx)
		case r := <-n.probeCh:
			n.applyProbeRound(r)
			n.evaluate(ctx)
		case <-floorT.C():
			n.evaluate(ctx)
		case fn := <-n.calls:
			fn()
		}
	}
}

// settle runs fn on the loop goroutine after the loop has processed everything
// already queued and finished any in-flight probe round.
//
// It is the test seam that replaces sleeping: a test pushes events, calls settle,
// and then inspects Status knowing the loop has caught up. It works because the
// queues are drained non-blockingly *on the loop goroutine*, so anything enqueued
// before settle was called is visible to the drain, and because a probe round in
// flight always completes (the strategy honours its timeout, and shutdown cancels).
func (n *Node) settle(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	call := func() {
		n.drain(ctx)
		if fn != nil {
			fn()
		}
		close(done)
	}
	select {
	case n.calls <- call:
	case <-ctx.Done():
		return ctx.Err()
	case <-n.stopped:
		return ErrAlreadyRunning
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// barrier runs fn on the loop goroutine as soon as the loop is between events,
// without draining or waiting for probes. It is the lighter test seam: "the loop
// has finished handling whatever it was handling", which is enough to know that
// a tick's side effects (a registered timer, a started round) are in place.
func (n *Node) barrier(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	call := func() {
		if fn != nil {
			fn()
		}
		close(done)
	}
	select {
	case n.calls <- call:
	case <-ctx.Done():
		return ctx.Err()
	case <-n.stopped:
		return ErrAlreadyRunning
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drain handles everything already queued, then waits for an in-flight probe
// round if there is one, and repeats until nothing is pending. Loop goroutine only.
func (n *Node) drain(ctx context.Context) {
	for {
		select {
		case ev, ok := <-n.events:
			if !ok {
				n.events = nil
			} else {
				n.handlePeerEvent(ctx, ev)
			}
			continue
		case f := <-n.ctrlIn:
			n.handleFrame(ctx, f)
			continue
		case f := <-n.dataIn:
			n.handleFrame(ctx, f)
			continue
		case r := <-n.probeCh:
			n.applyProbeRound(r)
			n.evaluate(ctx)
			continue
		default:
		}
		if !n.probing {
			return
		}
		select {
		case r := <-n.probeCh:
			n.applyProbeRound(r)
			n.evaluate(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// shutdown broadcasts LEAVE and joins the probe goroutines. Their context is
// derived from Run's, which is already done, so they are returning; the wait is
// there so Run's return means "no goroutine of mine is still alive".
func (n *Node) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), leaveTimeout)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeLeave, n.cfg.Self, "", protocol.LeavePayload{Reason: "shutdown"})
	if err == nil {
		sent := n.cfg.Transport.Broadcast(ctx, env)
		n.log.Info("leaving", "reason", "shutdown", "notified", sent)
	}
	n.probeWG.Wait()
	n.publish()
}

// ---- transport events ----

func (n *Node) handlePeerEvent(ctx context.Context, ev network.PeerEvent) {
	id := ev.Peer.ID
	if id == "" || id == n.cfg.Self {
		return
	}
	changed := false
	switch ev.Kind {
	case network.PeerUp:
		changed = n.table.Upsert(Member{
			ID:          id,
			Addr:        ev.Peer.Advertise,
			Incarnation: ev.Peer.Incarnation,
			Role:        RoleWorker,
			State:       StateAlive,
		})
		n.log.Info("peer up", "peer", id, "addr", ev.Peer.Advertise, "incarnation", ev.Peer.Incarnation)
	case network.PeerDown:
		delete(n.attached, id)
		switch {
		case ev.Disposition == network.DispositionCleanClose:
			// The peer told us it was leaving. There is no rumour to outlive, so
			// the record goes rather than lingering as dead.
			changed = n.table.Remove(id)
			n.log.Info("peer left", "peer", id)
		case ev.Disposition == network.DispositionProtocolViolation:
			// Not evidence of death, but the link cannot be resynchronised and
			// the pool will not redial it in a loop, so for membership purposes
			// the peer is unusable. Logged distinctly: an operator seeing this
			// wants to check for a version skew, not a crash.
			changed = n.table.SetState(id, StateDead)
			n.log.Warn("peer speaking an incompatible protocol; treating as dead", "peer", id, "err", ev.Err)
		default:
			// PeerDied, Timeout, and the unclassified Other all mean the link is
			// gone without a goodbye. Phase 4 refines this with heartbeat-based
			// suspicion; until then a vanished peer is dead.
			changed = n.table.SetState(id, StateDead)
			n.log.Warn("peer down", "peer", id, "disposition", ev.Disposition, "err", ev.Err)
		}
	default:
		return
	}
	if changed {
		n.membershipChanged(ctx)
	}
}

// membershipChanged is the single path after any table mutation: bound the health
// history to the live set, then re-evaluate.
func (n *Node) membershipChanged(ctx context.Context) {
	if r, ok := n.cfg.Health.(HealthRetainer); ok {
		r.Retain(n.table.Snapshot().Addresses())
	}
	n.evaluate(ctx)
}

// ---- inbound frames ----

func (n *Node) handleFrame(ctx context.Context, f inboundFrame) {
	env := f.env
	switch env.Type {
	case protocol.TypeMembershipDelta:
		p, ok := payloadOrPoison[protocol.MembershipDeltaPayload](ctx, n, f)
		if ok {
			n.handleMembershipDelta(ctx, f.peer, p)
		}
	case protocol.TypeElectionResult:
		p, ok := payloadOrPoison[protocol.ElectionResultPayload](ctx, n, f)
		if ok {
			n.claims[f.peer] = p
			n.publish()
		}
	case protocol.TypeJoinCluster:
		p, ok := payloadOrPoison[protocol.JoinClusterPayload](ctx, n, f)
		if ok {
			n.handleJoinCluster(ctx, f, p)
		}
	case protocol.TypeJoinAck:
		p, ok := payloadOrPoison[protocol.JoinAckPayload](ctx, n, f)
		if ok {
			n.handleJoinAck(ctx, f.peer, p)
		}
	case protocol.TypeLeave:
		// The payload is informational; a LEAVE with a bad body is still a LEAVE.
		delete(n.attached, f.peer)
		if n.table.Remove(f.peer) {
			n.log.Info("peer left", "peer", f.peer)
			n.membershipChanged(ctx)
		}
	default:
		// Unknown to this build, or known but owned by a later phase (HEARTBEAT,
		// STATE_SYNC, TASK ...). Both are dropped without touching the connection,
		// per the forward-compatibility policy in pkg/protocol.
		if _, seen := n.unknownLogged[env.Type]; !seen {
			n.unknownLogged[env.Type] = struct{}{}
			n.log.Warn("unhandled message type; dropping (logged once per type)", "type", env.Type, "peer", f.peer)
		}
	}
}

// payloadOrPoison decodes f's payload. A payload that does not parse is a peer
// speaking garbage: it is marked dead and the frame dropped. The transport will
// end the connection on the next framing error anyway; this makes membership
// agree without waiting for it.
func payloadOrPoison[T any](ctx context.Context, n *Node, f inboundFrame) (T, bool) {
	p, err := protocol.PayloadOf[T](f.env)
	if err != nil {
		n.log.Warn("malformed payload; marking peer dead", "peer", f.peer, "type", f.env.Type, "err", err)
		delete(n.attached, f.peer)
		if n.table.SetState(f.peer, StateDead) {
			n.membershipChanged(ctx)
		}
		var zero T
		return zero, false
	}
	return p, true
}

func (n *Node) handleMembershipDelta(ctx context.Context, from protocol.NodeID, p protocol.MembershipDeltaPayload) {
	changed := false
	for _, rec := range p.Members {
		if rec.ID == "" {
			continue
		}
		if rec.ID == n.cfg.Self {
			n.refuteIfNeeded(ctx, from, rec)
			continue
		}
		if n.table.Upsert(Member{
			ID:          rec.ID,
			Addr:        rec.Advertise,
			Incarnation: rec.Incarnation,
			Role:        ParseRole(rec.Role),
			State:       ParseState(rec.State),
		}) {
			changed = true
		}
	}
	if changed {
		n.membershipChanged(ctx)
	}
}

// refuteIfNeeded handles a rumour about this node.
//
// A record saying we are suspect or dead at an incarnation at least as new as our
// own would, if applied, be believed by every peer that hears it, and Upsert's
// equal-incarnation rule means we could never talk them out of it. The only
// refutation is a higher incarnation: bump past the rumour and re-announce. A
// record about us that is stale (lower incarnation) or benign (alive) is ignored;
// we are the authority on ourselves. Phase 3b's gossip extends the re-announce.
func (n *Node) refuteIfNeeded(ctx context.Context, from protocol.NodeID, rec protocol.MemberRecord) {
	if ParseState(rec.State) == StateAlive || rec.Incarnation < n.incarnation {
		return
	}
	n.incarnation = rec.Incarnation + 1
	self, _ := n.table.Snapshot().Get(n.cfg.Self)
	n.table.Upsert(Member{
		ID:          n.cfg.Self,
		Addr:        n.cfg.Advertise,
		Incarnation: n.incarnation,
		Role:        self.Role,
		State:       StateAlive,
	})
	n.log.Warn("refuting rumour about self", "from", from, "rumour", rec.State, "incarnation", n.incarnation)

	env, err := protocol.NewEnvelope(protocol.TypeMembershipDelta, n.cfg.Self, "", protocol.MembershipDeltaPayload{
		Members: []protocol.MemberRecord{{
			ID:          n.cfg.Self,
			Advertise:   n.cfg.Advertise,
			Incarnation: n.incarnation,
			Role:        self.Role.String(),
			State:       StateAlive.String(),
		}},
		ViewVersion: n.table.Snapshot().Version,
	})
	if err == nil {
		n.cfg.Transport.Broadcast(ctx, env)
	}
	n.publish()
}

func (n *Node) handleJoinCluster(ctx context.Context, f inboundFrame, p protocol.JoinClusterPayload) {
	worker := p.Worker
	if worker == "" {
		worker = f.peer
	}
	var ack protocol.JoinAckPayload
	if n.isLeader() {
		n.attached[worker] = struct{}{}
		ack = protocol.JoinAckPayload{Accepted: true, Leader: n.cfg.Self}
		n.log.Info("worker attached", "worker", worker, "score", p.Score)
	} else {
		// The hint is our own current leader: the best information we have about
		// where the worker should go instead.
		ack = protocol.JoinAckPayload{Accepted: false, Reason: "not a leader", Leader: n.leader}
	}
	reply, err := protocol.NewReply(f.env, protocol.TypeJoinAck, n.cfg.Self, ack)
	if err == nil {
		if err := n.cfg.Transport.Send(ctx, f.peer, reply); err != nil {
			n.log.Warn("JOIN_ACK send failed", "peer", f.peer, "err", err)
		}
	}
	n.publish()
}

func (n *Node) handleJoinAck(ctx context.Context, from protocol.NodeID, p protocol.JoinAckPayload) {
	if p.Accepted {
		leader := p.Leader
		if leader == "" {
			leader = from
		}
		n.leader = leader
		n.hint = ""
		n.log.Info("attached to leader", "leader", leader)
		n.publish()
		return
	}
	// Rejected. We are not attached anywhere; the named leader is a hint that the
	// next evaluate prefers over its own ranking if it is a leader we can score.
	n.log.Info("join rejected", "by", from, "reason", p.Reason, "hint", p.Leader)
	if n.leader == from {
		n.leader = ""
	}
	n.hint = p.Leader
	n.evaluate(ctx)
}

// ---- probing ----

// startProbeRound scores every alive peer concurrently. Rounds do not overlap: if
// the previous round is still running (a peer is at its timeout), this tick is
// skipped rather than queued, or a slow peer would accumulate an unbounded
// backlog of probes against it.
func (n *Node) startProbeRound(ctx context.Context) {
	if n.probing {
		n.log.Debug("probe round still in flight; skipping tick")
		return
	}
	var targets []Member
	for _, m := range n.table.Snapshot().Alive() {
		if m.ID != n.cfg.Self && m.Addr != "" {
			targets = append(targets, m)
		}
	}
	n.probing = true

	// One derived context bounds the whole round. Cancelling it on shutdown makes
	// every in-flight probe return ErrProbeCanceled, which the WORKLOG 3.4 call
	// shape discards, so shutdown never manufactures a burst of missed beats.
	//
	// The round deadline comes from the Clock, not from context.WithTimeout, so
	// that a test can fire it deterministically. The timer goroutine below is the
	// only thing that turns the tick into a cancel; it ends when the round ends.
	rctx, cancel := context.WithCancel(ctx)
	roundDone := make(chan struct{})
	deadline := n.cfg.Clock.After(n.cfg.ProbeTimeout)
	n.probeWG.Add(1)
	go func() {
		defer n.probeWG.Done()
		select {
		case <-deadline:
			n.log.Warn("probe round exceeded its budget; cancelling", "budget", n.cfg.ProbeTimeout)
			cancel()
		case <-roundDone:
		}
	}()

	var (
		mu      sync.Mutex
		results = make(map[protocol.NodeID]probeResult, len(targets))
		wg      sync.WaitGroup
	)
	for _, m := range targets {
		wg.Add(1)
		n.probeWG.Add(1)
		go func(m Member) {
			defer wg.Done()
			defer n.probeWG.Done()
			s, err := n.cfg.Health.EvaluateScore(rctx, m.Addr)
			mu.Lock()
			results[m.ID] = probeResult{score: s, err: err}
			mu.Unlock()
		}(m)
	}

	// The collector is the only goroutine that speaks to the loop, and it does so
	// through probeCh. The select on ctx.Done is what stops it leaking when the
	// loop has exited and nobody will ever receive.
	n.probeWG.Add(1)
	go func() {
		defer n.probeWG.Done()
		defer cancel()
		wg.Wait()
		close(roundDone)
		select {
		case n.probeCh <- probeRound{results: results}:
		case <-ctx.Done():
		}
	}()
}

// applyProbeRound folds a finished round into scores using the call shape
// mandated in WORKLOG 3.4. Loop goroutine only.
func (n *Node) applyProbeRound(r probeRound) {
	n.probing = false
	for id, res := range r.results {
		switch {
		case health.IsCancellation(res.err):
			// Shutdown or a superseded round: not a fact about the peer. The old
			// score stands.
		case res.err != nil:
			n.scores[id] = health.ScoreUnavailable()
			n.missed[id]++
			n.log.Debug("probe failed", "peer", id, "missed", n.missed[id], "err", res.err)
		default:
			n.scores[id] = res.score
			n.missed[id] = 0
		}
	}
	n.scores[n.cfg.Self] = n.selfScore()
}

// selfScore is the score this node gives itself in its own election.
//
// A node cannot probe itself over the mesh, and the two obvious substitutes are
// both wrong: 0 makes every node win its own election (each thinks it is the
// best leader, and the swarm disagrees N ways), and NaN makes a node never
// eligible in its own view while every peer may elect it. The median of the
// valid peer scores is a placeholder that puts self in the middle of the pack, so
// the local result tracks what peers are likely to compute rather than being
// biased either way. It is a heuristic, not a measurement, and Phase 4 should
// replace it with a peer-reported score for self (a HEARTBEAT_ACK carries one).
// With no scored peers it is 0: a node alone must be able to lead.
func (n *Node) selfScore() float64 {
	var valid []float64
	for id, s := range n.scores {
		if id != n.cfg.Self && health.IsValidScore(s) {
			valid = append(valid, s)
		}
	}
	if len(valid) == 0 {
		return 0
	}
	sort.Float64s(valid)
	mid := len(valid) / 2
	if len(valid)%2 == 1 {
		return valid[mid]
	}
	return (valid[mid-1] + valid[mid]) / 2
}

// ---- election and affinity ----

// evaluate is the single election-and-affinity routine, reached from every
// trigger: membership change, a finished probe round, the periodic floor, and a
// JOIN_ACK rejection. It is idempotent by construction -- Elect, ChooseLeader and
// ShouldRehome are pure, and every side effect below is guarded by "did the
// answer change" -- which is what makes having several entry points safe
// (WORKLOG 2.4).
func (n *Node) evaluate(ctx context.Context) {
	view := n.table.Snapshot()
	res := Elect(view, n.scores, n.cfg.Election)

	for _, m := range view.Members {
		role := RoleWorker
		if contains(res.Leaders, m.ID) {
			role = RoleLeader
		}
		n.table.SetRole(m.ID, role)
	}

	if !equalIDs(res.Leaders, n.leaders) {
		n.leaders = append([]protocol.NodeID(nil), res.Leaders...)
		n.term++
		n.log.Info("leaders changed", "term", n.term, "leaders", n.leaders, "size", res.Size, "want", res.Want)
		env, err := protocol.NewEnvelope(protocol.TypeElectionResult, n.cfg.Self, "", protocol.ElectionResultPayload{
			Term:        n.term,
			Leaders:     n.leaders,
			ClusterSize: res.Size,
		})
		if err == nil {
			n.cfg.Transport.Broadcast(ctx, env)
		}
	}

	if contains(res.Leaders, n.cfg.Self) {
		n.leader = n.cfg.Self
		n.hint = ""
		// Attached workers that have died, left, or been promoted are no longer
		// ours to lead.
		for id := range n.attached {
			m, ok := view.Get(id)
			if !ok || m.State != StateAlive || contains(res.Leaders, id) {
				delete(n.attached, id)
			}
		}
	} else {
		// A demoted leader has no cluster; whoever was attached will re-home.
		for id := range n.attached {
			delete(n.attached, id)
		}
		best, ok := ChooseLeader(n.scores, res.Leaders)
		if n.hint != "" && contains(res.Leaders, n.hint) {
			best, ok = n.hint, true
		}
		n.hint = ""
		if ok && ShouldRehome(n.leader, best, n.scores, res.Leaders, n.cfg.Rehome) {
			n.join(ctx, best)
		}
	}

	if size := view.Size(); size > n.highWater {
		n.highWater = size
	}
	n.degraded = Degraded(view.Size(), n.highWater)
	n.publish()
}

// join sends JOIN_CLUSTER to leader and provisionally records it as ours. The
// provisional assignment is what stops the next evaluate from sending the same
// JOIN again before the ACK arrives; a rejection clears it.
func (n *Node) join(ctx context.Context, leader protocol.NodeID) {
	env, err := protocol.NewEnvelope(protocol.TypeJoinCluster, n.cfg.Self, leader, protocol.JoinClusterPayload{
		Worker: n.cfg.Self,
		Score:  n.scores[leader],
	})
	if err != nil {
		return
	}
	if err := n.cfg.Transport.Send(ctx, leader, env); err != nil {
		// Not attached; the next evaluate retries. ErrUnknownPeer here means
		// membership and the connection table disagree, which PeerDown resolves.
		n.log.Warn("JOIN_CLUSTER send failed", "leader", leader, "err", err)
		n.leader = ""
		return
	}
	n.log.Info("joining leader", "leader", leader, "score", n.scores[leader])
	n.leader = leader
}

func (n *Node) isLeader() bool { return contains(n.leaders, n.cfg.Self) }

// publish builds a fresh Status and swaps it in. Loop goroutine only (and once
// from NewNode, before the loop exists).
func (n *Node) publish() {
	view := n.table.Snapshot()
	role := RoleWorker
	if self, ok := view.Get(n.cfg.Self); ok {
		role = self.Role
	}
	s := &Status{
		Self:        n.cfg.Self,
		Incarnation: n.incarnation,
		Role:        role,
		Term:        n.term,
		Leader:      n.leader,
		Leaders:     append([]protocol.NodeID(nil), n.leaders...),
		View:        view,
		Scores:      make(map[protocol.NodeID]float64, len(n.scores)),
		Missed:      make(map[protocol.NodeID]int, len(n.missed)),
		Degraded:    n.degraded,
		HighWater:   n.highWater,
		Attached:    make([]protocol.NodeID, 0, len(n.attached)),
		Claims:      make(map[protocol.NodeID]protocol.ElectionResultPayload, len(n.claims)),
		Dropped:     n.dropped.Load(),
	}
	for id, v := range n.scores {
		s.Scores[id] = v
	}
	for id, v := range n.missed {
		s.Missed[id] = v
	}
	for id, v := range n.claims {
		v.Leaders = append([]protocol.NodeID(nil), v.Leaders...)
		s.Claims[id] = v
	}
	for id := range n.attached {
		s.Attached = append(s.Attached, id)
	}
	sortIDs(s.Attached)

	n.statusMu.Lock()
	n.status = s
	n.statusMu.Unlock()
}

func equalIDs(a, b []protocol.NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// String renders a Status for logs and tests.
func (s Status) String() string {
	return fmt.Sprintf("%s role=%s term=%d leader=%s leaders=%v alive=%d degraded=%t",
		s.Self, s.Role, s.Term, s.Leader, s.Leaders, s.View.Size(), s.Degraded)
}
