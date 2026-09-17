package cluster

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
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
	// DefaultGossipInterval is the anti-entropy period. With one peer per round
	// (k=1) a full cycle over N peers takes N * 2s. A lost push is repaired
	// within about two cycles in the worst case (a peer visited first in one
	// permutation and last in the next); this is the repair path, not the
	// normal dissemination path.
	DefaultGossipInterval = 2 * time.Second
	// DefaultProbeTimeout is twice the strategy's own per-probe budget. See
	// NodeConfig.ProbeTimeout for why the node's budget must be the looser one.
	DefaultProbeTimeout = 2 * health.DefaultProbeTimeout
	DefaultQueueDepth   = 256
	// DefaultHeartbeatInterval is the leader's beat period and the period of the
	// node's failure-detector tick (suspicion expiry, tombstone GC).
	DefaultHeartbeatInterval = 500 * time.Millisecond
	// DefaultHeartbeatMisses is how many ticks a worker waits without a beat
	// before it suspects its leader: 1.5s at the default interval. Three rather
	// than one because a single late beat is scheduling noise, not a failure.
	DefaultHeartbeatMisses = 3
	// DefaultSuspectAfter and DefaultDeadAfter are consecutive failed probes. At
	// the 1s probe interval a peer is suspect after ~3s of silence and dead after
	// ~6s, which is the same moment DefaultSuspicionTimeout would fire.
	DefaultSuspectAfter = 3
	DefaultDeadAfter    = 6
	// DefaultSuspicionTimeout is how long a first-hand suspicion may stand
	// before it becomes a death. It is sized for the pool's first redial: a link
	// that drops and comes straight back must not cost an incarnation bump and a
	// re-election.
	DefaultSuspicionTimeout = 3 * time.Second
	// DefaultTombstoneTTL is how long a death record is kept. See sweepTombstones
	// for how it relates to the anti-entropy repair bound.
	DefaultTombstoneTTL = 60 * time.Second
	// leaveTimeout bounds the LEAVE broadcast on shutdown. It is a real-time bound,
	// not a synchronisation: shutdown must not wait on a peer whose send queue is
	// full, and a LEAVE that does not get out costs the peers one failure-detector
	// round, which they must survive anyway (a SIGKILL never sends one).
	leaveTimeout = 500 * time.Millisecond
	// maxRejectionsPerRound bounds how many rejected JOINs a worker answers with
	// another JOIN before the next probe round. A rejection re-evaluates at once
	// and may follow the rejecter's hint, which is right when one node is
	// behind. When two nodes disagree about who leads, though, each rejection
	// names the other, and without a bound the worker and the two "leaders"
	// exchange JOIN / JOIN_ACK at network speed (a run under reordering sent
	// ~100k of each in one simulated instant). Past the bound the worker waits,
	// detached, for the next round, by which time the views have converged.
	maxRejectionsPerRound = 3
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
	//
	// Election.Hysteresis follows the Rehome convention at this level: zero
	// means DefaultHysteresis, not "no damping". Elect itself reads zero as
	// "no damping", and before this rule a caller that set only the threshold
	// (cmd/swarm-node does) ran the swarm with no damping at all: every score
	// wobble was re-announced and could move the leader set. A node that
	// really wants no damping passes a tiny positive margin.
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
	// GossipInterval is how often one peer is sent our full view (anti-entropy).
	// Default 2s.
	GossipInterval time.Duration
	// Shuffle permutes the anti-entropy peer order in place. Default: a
	// math/rand/v2 shuffle. Tests inject a deterministic permutation.
	Shuffle func([]protocol.NodeID)
	// Connect is called, on the loop goroutine, for every peer address the node
	// learns about (HELLO's KnownPeers, a MEMBERSHIP_DELTA naming a new member)
	// and has not seen before -- at most once per address. cmd wires it to
	// Pool.Connect, which is how the mesh grows from a single seed. It must not
	// block. Optional.
	Connect func(protocol.NodeAddress)
	// Executor runs tasks assigned to this node. Default: the built-in task
	// kinds (Phase 4). See control.go.
	Executor TaskExecutor
	// OnTaskResult is called, on the loop goroutine, when a task this node
	// leads completes (its own or an attached worker's). cmd wires it to the
	// Control Center uplink. It must not block. Optional.
	OnTaskResult func(protocol.TaskResultPayload)

	// ---- Phase 4: failure detection. Every field defaults when <= 0, so a
	// caller that sets none of them gets the documented behaviour. ----

	// HeartbeatInterval is the leader's beat period, and the period of the
	// failure-detector tick that expires suspicions and tombstones. Default 500ms.
	HeartbeatInterval time.Duration
	// HeartbeatMisses is how many ticks without a beat make a worker suspect its
	// leader and fail over. Default 3.
	HeartbeatMisses int
	// SuspectAfter is how many consecutive failed probes make a peer suspect.
	// Default 3.
	SuspectAfter int
	// DeadAfter is how many consecutive failed probes make a peer dead, whether
	// or not its suspicion has timed out. Default 6.
	DeadAfter int
	// SuspicionTimeout is how long a first-hand suspicion stands before it is
	// confirmed as a death. It is checked on the failure-detector tick, so the
	// effective timeout is rounded up to the next HeartbeatInterval. Default 3s.
	SuspicionTimeout time.Duration
	// TombstoneTTL is how long a dead member's record is kept before it is
	// removed from the table, measured on the Clock from when this node first
	// saw the death. Default 60s. It must comfortably exceed the anti-entropy
	// repair bound, (2N-1) * GossipInterval; see sweepTombstones.
	TombstoneTTL time.Duration
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
	if c.GossipInterval <= 0 {
		c.GossipInterval = DefaultGossipInterval
	}
	if c.Shuffle == nil {
		c.Shuffle = shuffleIDs
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = DefaultProbeTimeout
	}
	if c.Rehome == 0 {
		c.Rehome = DefaultHysteresis
	}
	if c.Election.Hysteresis == 0 {
		c.Election.Hysteresis = DefaultHysteresis
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = DefaultQueueDepth
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.HeartbeatMisses <= 0 {
		c.HeartbeatMisses = DefaultHeartbeatMisses
	}
	if c.SuspectAfter <= 0 {
		c.SuspectAfter = DefaultSuspectAfter
	}
	if c.DeadAfter <= 0 {
		c.DeadAfter = DefaultDeadAfter
	}
	if c.SuspicionTimeout <= 0 {
		c.SuspicionTimeout = DefaultSuspicionTimeout
	}
	if c.TombstoneTTL <= 0 {
		c.TombstoneTTL = DefaultTombstoneTTL
	}
	return c
}

// shuffleIDs is the production Shuffle. The top-level math/rand/v2 functions
// are safe for concurrent use and randomly seeded, so there is no source to own.
func shuffleIDs(ids []protocol.NodeID) {
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
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
	// Scores are this node's LOCAL measurements of each peer (and its own
	// self-score), used for affinity: which leader is closest to me is a question
	// only I can answer.
	Scores map[protocol.NodeID]float64
	// Reported are the peers' SELF-REPORTED scores, learned from gossip, which
	// election ranks on: the one input every observer agrees about.
	Reported map[protocol.NodeID]float64
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
	// ControlStatus carries the replicated ledger and chaos state (control.go).
	ControlStatus
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
	// bgWG joins the short-lived goroutines that deliver chaos-delayed replies
	// (sendDelayed). Each one waits on a Clock timer or Run's context, so
	// shutdown, which runs after that context is done, never waits long.
	bgWG sync.WaitGroup
	// chaosDelay is the injected reply delay in nanoseconds. Atomic: written by
	// SetChaosDelay from any goroutine, read by the loop and by Status.
	chaosDelay atomic.Int64

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
	hint protocol.NodeID
	// rejections counts JOIN_ACK rejections since the last probe round; see
	// maxRejectionsPerRound.
	rejections int
	attached   map[protocol.NodeID]struct{}
	// local is my measured score per peer, plus my own self-score. Affinity only.
	local map[protocol.NodeID]float64
	// reported is each member's self-reported score as of the last evaluate,
	// derived from the table (the table is the source of truth; this is the
	// published copy). Election only.
	reported map[protocol.NodeID]float64
	// dialed records addresses handed to cfg.Connect, so each is dialled once.
	dialed    map[protocol.NodeAddress]struct{}
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
	// Anti-entropy peer order; see gossipRound. gossipOrder is a shuffled
	// permutation of the alive peers, walked by gossipNext. gossipSet holds the
	// same IDs for membership comparison, and gossipVersion is the table version
	// the order was last checked against.
	gossipOrder   []protocol.NodeID
	gossipNext    int
	gossipSet     map[protocol.NodeID]struct{}
	gossipVersion uint64
	// suspects holds the suspicions this node raised FIRST-HAND (a lost link,
	// missed probes, missed beats) and is timing. A suspect record merely
	// relayed to us has no entry: it is the originator's job to confirm it, and
	// the member's job to refute it. See failure.go.
	suspects map[protocol.NodeID]suspicion
	// probeSeq numbers probe rounds, so a round that started before a suspicion
	// cannot clear it with a stale success.
	probeSeq uint64
	// tombstones times every death record in the table, first-hand or
	// relayed, keyed by member. tombVersion is the table version they were
	// last reconciled against. See sweepTombstones.
	tombstones  map[protocol.NodeID]tombstone
	tombVersion uint64
	// Heartbeats; see heartbeat.go. beatSeq is the leader's per-worker beat
	// counter. sinceBeat is the worker's count of ticks since the last valid
	// beat from n.leader.
	beatSeq   map[protocol.NodeID]uint64
	sinceBeat int
}

type inboundFrame struct {
	peer protocol.NodeID
	env  *protocol.Envelope
}

// probeRound is the outcome of scoring every peer once.
type probeRound struct {
	seq     uint64
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
		local:         make(map[protocol.NodeID]float64),
		reported:      make(map[protocol.NodeID]float64),
		dialed:        make(map[protocol.NodeAddress]struct{}),
		missed:        make(map[protocol.NodeID]int),
		claims:        make(map[protocol.NodeID]protocol.ElectionResultPayload),
		unknownLogged: make(map[protocol.MessageType]struct{}),
		gossipSet:     make(map[protocol.NodeID]struct{}),
		suspects:      make(map[protocol.NodeID]suspicion),
		tombstones:    make(map[protocol.NodeID]tombstone),
		beatSeq:       make(map[protocol.NodeID]uint64),
	}
	n.table.Upsert(Member{
		ID:          cfg.Self,
		Addr:        cfg.Advertise,
		Incarnation: cfg.Incarnation,
		Role:        RoleWorker,
		State:       StateAlive,
		// Unmeasured until the first probe round: see selfScore. Seeding 0 here
		// would make a booting node claim the best score in the swarm before it has
		// measured a single link, and a newcomer would displace a healthy incumbent
		// on that claim alone. A node that really is alone still leads, via Elect's
		// nobody-is-measurable fallback.
		Score: health.ScoreUnavailable(),
		// Seq starts at 1 so that anything we ever say about ourselves outranks
		// the Seq-0 placeholder a peer creates from our handshake.
		Seq: 1,
	})
	n.dialed[cfg.Advertise] = struct{}{}
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
	// Likewise: SetChaosDelay is called by cmd, not the loop.
	s.ChaosDelay = n.ChaosDelay()
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
	out.Reported = make(map[protocol.NodeID]float64, len(s.Reported))
	for k, v := range s.Reported {
		out.Reported[k] = v
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
	// Registered last on purpose. FakeClock breaks deadline ties on registration
	// order, and tests that advance to a shared deadline (2s is both a probe
	// tick and a gossip tick) rely on the probe round starting first.
	gossipT := n.cfg.Clock.NewTicker(n.cfg.GossipInterval)
	defer gossipT.Stop()
	// Phase 4 tickers go after the Phase 3 ones for the same reason: at a shared
	// deadline a probe round must already be in flight before a suspicion
	// expires, and existing tests depend on that order.
	tickT := n.cfg.Clock.NewTicker(n.cfg.HeartbeatInterval)
	defer tickT.Stop()

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
			n.applyProbeRound(ctx, r)
			n.evaluate(ctx)
		case <-floorT.C():
			n.evaluate(ctx)
		case <-gossipT.C():
			n.gossipRound(ctx)
		case <-tickT.C():
			n.controlTick(ctx)
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
			n.applyProbeRound(ctx, r)
			n.evaluate(ctx)
			continue
		default:
		}
		if !n.probing {
			return
		}
		select {
		case r := <-n.probeCh:
			n.applyProbeRound(ctx, r)
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
	n.bgWG.Wait()
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
		// A known member keeps its role: a leader that reconnects is still the
		// leader until the next evaluate says otherwise, and demoting it here
		// would broadcast a spurious leader change.
		role, score := RoleWorker, health.ScoreUnavailable()
		if m, ok := n.table.Snapshot().Get(id); ok {
			role, score = m.Role, m.Score
		}
		changed = n.table.Upsert(Member{
			ID:          id,
			Addr:        ev.Peer.Advertise,
			Incarnation: ev.Peer.Incarnation,
			Role:        role,
			State:       StateAlive,
			Score:       score,
		})
		n.dialed[ev.Peer.Advertise] = struct{}{}
		for _, addr := range ev.Peer.KnownPeers {
			n.connect(addr)
		}
		// A completed handshake is first-hand evidence of life, which Upsert's
		// rumour rule cannot express; see Table.Revive. Without this, a peer that
		// dropped and reconnected at the same incarnation stays dead forever.
		if n.table.Revive(id, ev.Peer.Incarnation) {
			changed = true
		}
		// A reconnect inside the suspicion window is exactly what the window is
		// for: the suspicion is resolved, not confirmed. Revive has already
		// cleared a suspect record held at the member's own incarnation.
		delete(n.suspects, id)
		n.log.Info("peer up", "peer", id, "addr", ev.Peer.Advertise, "incarnation", ev.Peer.Incarnation)
		// Hand the newcomer our whole view so it can refute anything stale we
		// or our peers believe about it, and learn who else is here. Sent before
		// evaluate so a refutation is already on its way when we elect.
		n.sendView(ctx, id, n.table.Snapshot())
	case network.PeerDown:
		delete(n.attached, id)
		switch {
		case ev.Disposition == network.DispositionCleanClose:
			// The peer told us it was leaving. Recorded as a death, not deleted:
			// see markLeft.
			changed = n.markLeft(ctx, id)
		case ev.Disposition == network.DispositionProtocolViolation:
			// Not evidence of death, but the link cannot be resynchronised and
			// the pool will not redial it in a loop, so for membership purposes
			// the peer is unusable. Logged distinctly: an operator seeing this
			// wants to check for a version skew, not a crash.
			changed = n.markDead(ctx, id, "incompatible protocol", ev.Err)
		default:
			// PeerDied, Timeout, and the unclassified Other all mean the link is
			// gone without a goodbye. That is suspicion, not death.
			//
			// Why not dead at once, as in Phase 3: a TCP reset is evidence about
			// a link, and links break for reasons that leave the peer healthy (a
			// bridge hiccup, an idle-timeout race). A death bumps the incarnation,
			// so a peer declared dead on a blip has to refute before anyone will
			// talk to it again, and the election reshuffles twice in between.
			// The pool redials straight away; SuspicionTimeout (3s) is sized to
			// let that redial land, and a PeerUp inside the window clears the
			// suspicion through Revive.
			//
			// Why not ignore link loss and wait for missed probes: a crashed
			// container resets its sockets, and that reset is the fastest failure
			// signal we will ever get. Suspecting on it immediately lets workers
			// leave the leader now, and the timeout, not the probe count, decides
			// when the swarm re-elects: a crash is confirmed in 3s instead of ~6s.
			changed = n.markSuspect(ctx, id, "link lost ("+ev.Disposition.String()+")", ev.Err)
		}
	default:
		return
	}
	if changed {
		n.membershipChanged(ctx)
	}
}

// connect hands a newly learned address to cfg.Connect, once. This is how a
// node that knows one seed ends up connected to the whole mesh: the seed's
// HELLO lists who it knows, and every MEMBERSHIP_DELTA names members' addresses.
func (n *Node) connect(addr protocol.NodeAddress) {
	if addr == "" || n.cfg.Connect == nil {
		return
	}
	if _, seen := n.dialed[addr]; seen {
		return
	}
	if _, known := n.Resolve(addr); known {
		n.dialed[addr] = struct{}{}
		return
	}
	n.dialed[addr] = struct{}{}
	n.log.Info("learned peer address; connecting", "addr", addr)
	n.cfg.Connect(addr)
}

// markDead is the single place a peer is declared dead by inference. It is the
// verdict step of the failure detector: callers are a confirmed suspicion
// (timeout or DeadAfter missed probes, see failure.go) and the evidence that
// needs no confirmation (a poisoned payload, a protocol violation). A doubt goes
// through markSuspect instead. It reports whether the table changed; the caller
// decides whether to re-evaluate. A change is pushed to every connected peer;
// see pushRecord.
func (n *Node) markDead(ctx context.Context, id protocol.NodeID, reason string, err error) bool {
	delete(n.attached, id)
	delete(n.suspects, id)
	changed := n.table.SetState(id, StateDead)
	if changed {
		n.log.Warn("peer marked dead", "peer", id, "reason", reason, "err", err)
		n.pushRecord(ctx, id)
	}
	return changed
}

// markLeft records a voluntary departure (LEAVE, or a clean close) and reports
// whether the table changed.
//
// It records a death rather than deleting the member. A deleted record has no
// incarnation left to defend, so the first peer that has not yet heard the
// LEAVE -- and under anti-entropy every peer re-sends its whole view -- would
// re-insert the node as alive, and it would be gossiped around a swarm it has
// already left. A dead record at inc+1 outranks every such echo (SetState).
//
// It does not go through markDead: a goodbye is a fact, not an inference, so
// Phase 4's suspicion step must not apply to it, and it is not worth a warning.
func (n *Node) markLeft(ctx context.Context, id protocol.NodeID) bool {
	delete(n.attached, id)
	delete(n.suspects, id)
	changed := n.table.SetState(id, StateDead)
	if changed {
		n.log.Info("peer left", "peer", id)
		n.pushRecord(ctx, id)
	}
	return changed
}

// pushRecord broadcasts our record of id: push-on-change for a death this node
// observed first-hand.
//
// Before this, a death travelled only as far as each peer's own PeerDown. In a
// full mesh that is everyone, but a peer that lost only its link to the victim
// would never be told, and would wait a full anti-entropy cycle.
//
// Only first-hand deaths are pushed. Relayed changes (merged from a delta) are
// deliberately NOT re-pushed: a broadcast in a full mesh has already reached
// everyone, and re-pushing on receipt would turn every change into N^2 frames.
// Anything a push loses is repaired by gossipRound. A crash that every node
// observes still costs about N^2 frames (one broadcast per observer, a no-op at
// all but the first receiver); deaths are rare enough that the simple rule wins.
func (n *Node) pushRecord(ctx context.Context, id protocol.NodeID) {
	view := n.table.Snapshot()
	m, ok := view.Get(id)
	if !ok {
		return
	}
	env, err := protocol.NewEnvelope(protocol.TypeMembershipDelta, n.cfg.Self, "", protocol.MembershipDeltaPayload{
		Members:     []protocol.MemberRecord{memberRecord(m)},
		ViewVersion: view.Version,
	})
	if err == nil {
		n.cfg.Transport.Broadcast(ctx, env)
	}
}

// sendView sends view, our full membership, to one peer as a MEMBERSHIP_DELTA.
// A full view is just a large delta under Table.Upsert's merge rule, which is
// what makes this the right shape for both a welcome and anti-entropy.
func (n *Node) sendView(ctx context.Context, to protocol.NodeID, view View) {
	records := make([]protocol.MemberRecord, 0, len(view.Members))
	for _, m := range view.Members {
		records = append(records, memberRecord(m))
	}
	env, err := protocol.NewEnvelope(protocol.TypeMembershipDelta, n.cfg.Self, to, protocol.MembershipDeltaPayload{
		Members:     records,
		ViewVersion: view.Version,
	})
	if err != nil {
		return
	}
	if err := n.cfg.Transport.Send(ctx, to, env); err != nil {
		// Membership and the connection table legitimately disagree for a while
		// (a member learned by gossip that we have not dialled yet), and gossip
		// retries every cycle, so a missing connection is not worth a warning.
		level := slog.LevelWarn
		if errors.Is(err, network.ErrUnknownPeer) {
			level = slog.LevelDebug
		}
		n.log.Log(ctx, level, "view send failed", "peer", to, "err", err)
	}
}

// gossipRound is one anti-entropy step: send our full view to one alive peer.
//
// # Peer selection: shuffled round-robin, k=1
//
// Peers are visited in a random permutation, one per round, and the
// permutation is redrawn when it is exhausted. A uniformly random pick each
// round would be simpler but gives no bound: by chance some peer goes unvisited
// for many rounds. A permutation guarantees every alive peer hears our view
// once per permutation of N rounds, so a lost push is repaired within at most 2N-1 rounds
// (first in one permutation, last in the next), and within N when the loss
// itself changed the alive set, since that forces a fresh permutation. The
// shuffle, rather than ID order, keeps the swarm from all targeting the same
// peer at once. A redraw at the wrap may put the last peer first again; that
// costs one redundant send and nothing else.
//
// The order is redrawn early only when the SET of alive peers changes. The
// table version is the cheap trigger for checking, but most version bumps are
// score reports, and redrawing on each would keep resetting the cursor and
// quietly turn the bound back into random selection.
//
// The view includes our own record. That record is first-hand, and receivers
// trust a role only when rec.ID is the sender, so gossip repairs our own role
// and score at every peer within a cycle. It cannot repair a role we merely
// relay; that stays the owner's job.
func (n *Node) gossipRound(ctx context.Context) {
	view := n.table.Snapshot()
	if view.Version != n.gossipVersion {
		n.gossipVersion = view.Version
		if !n.sameGossipSet(view) {
			n.gossipNext = len(n.gossipOrder) // force a redraw below
		}
	}
	if n.gossipNext >= len(n.gossipOrder) {
		n.gossipOrder = n.gossipOrder[:0]
		clear(n.gossipSet)
		for _, m := range view.Members {
			// Suspect peers are gossiped to as well: our view is how a suspect
			// learns it is suspected, and it can only refute what it hears.
			if m.State != StateDead && m.ID != n.cfg.Self {
				n.gossipOrder = append(n.gossipOrder, m.ID)
				n.gossipSet[m.ID] = struct{}{}
			}
		}
		n.cfg.Shuffle(n.gossipOrder)
		n.gossipNext = 0
	}
	if len(n.gossipOrder) == 0 {
		return
	}
	peer := n.gossipOrder[n.gossipNext]
	n.gossipNext++
	n.sendView(ctx, peer, view)
}

// sameGossipSet reports whether view's non-dead peers are exactly gossipSet.
func (n *Node) sameGossipSet(view View) bool {
	count := 0
	for _, m := range view.Members {
		if m.State == StateDead || m.ID == n.cfg.Self {
			continue
		}
		if _, ok := n.gossipSet[m.ID]; !ok {
			return false
		}
		count++
	}
	return count == len(n.gossipSet)
}

// membershipChanged is the single path after any table mutation: bound the health
// history and the per-peer maps to the live set, then re-evaluate.
func (n *Node) membershipChanged(ctx context.Context) {
	n.retainLive()
	n.evaluate(ctx)
}

// retainLive is membershipChanged without the evaluate, for callers that are
// about to evaluate anyway (a finished probe round).
func (n *Node) retainLive() {
	view := n.table.Snapshot()
	if r, ok := n.cfg.Health.(HealthRetainer); ok {
		r.Retain(view.Addresses())
	}
	n.pruneLocal(view)
}

// pruneLocal drops local and missed entries for every peer that is dead or gone
// in view. Self is kept: its local entry is the previous self-score. Suspect
// peers are kept too: they are still probed, and their miss count is what
// decides whether the suspicion becomes a death.
//
// Without this both maps only ever grow. Worse than the memory, selfScore takes
// its median over n.local, and an entry for a peer that has left is never
// re-probed (probe rounds target alive members only), so it never turns NaN
// and never drops out: ten short-lived slow workers would skew this node's
// self-report, and therefore every election in the swarm, for good. A peer that
// comes back is simply measured afresh on the next round.
func (n *Node) pruneLocal(view View) {
	alive := make(map[protocol.NodeID]struct{}, len(view.Members))
	for _, m := range view.Members {
		if m.State != StateDead {
			alive[m.ID] = struct{}{}
		}
	}
	for id := range n.local {
		if _, ok := alive[id]; !ok && id != n.cfg.Self {
			delete(n.local, id)
		}
	}
	for id := range n.missed {
		if _, ok := alive[id]; !ok {
			delete(n.missed, id)
		}
	}
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
		if n.markLeft(ctx, f.peer) {
			n.membershipChanged(ctx)
		}
	case protocol.TypeHeartbeat:
		p, ok := payloadOrPoison[protocol.HeartbeatPayload](ctx, n, f)
		if ok {
			n.handleHeartbeat(ctx, f, p)
		}
	case protocol.TypeHeartbeatAck:
		p, ok := payloadOrPoison[protocol.HeartbeatAckPayload](ctx, n, f)
		if ok {
			n.handleHeartbeatAck(f.peer, p)
		}
	default:
		// Unknown to this build, or known but not handled by the node (CHAOS and
		// TELEMETRY belong to the Control Center link, not the mesh). Both are
		// dropped without touching the connection, per the forward-compatibility
		// policy in pkg/protocol.
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
		if n.markDead(ctx, f.peer, "malformed "+string(f.env.Type)+" payload", err) {
			n.membershipChanged(ctx)
		}
		var zero T
		return zero, false
	}
	return p, true
}

func (n *Node) handleMembershipDelta(ctx context.Context, from protocol.NodeID, p protocol.MembershipDeltaPayload) {
	changed := false
	// One snapshot for the whole delta, not one per record. Under anti-entropy
	// every delta is a full view, and a per-record Snapshot (copy + sort of the
	// table) made this O(N^2 log N) on the event loop: ~120ms per message at
	// N=1000. The snapshot is used only for role lookup, and reading roles as
	// they stood before the delta is the right semantics anyway: a record later
	// in the same delta must not see a role an earlier record just relayed.
	before := n.table.Snapshot()
	for _, rec := range p.Members {
		if rec.ID == "" {
			continue
		}
		if rec.ID == n.cfg.Self {
			n.refuteIfNeeded(ctx, from, rec)
			continue
		}
		score := health.ScoreUnavailable()
		if rec.Measured() {
			score = rec.Score
		}
		i, known := slices.BinarySearchFunc(before.Members, rec.ID, compareMemberID)
		// A tombstone for a member we do not hold is dropped, not inserted.
		// Once a node has collected a death (sweepTombstones), a peer that has
		// not collected it yet would otherwise re-insert it on its next gossip
		// round, restart our TTL, and the swarm would pass the same tombstone
		// back and forth forever. Nothing is lost by skipping it: we hold no
		// "alive" copy of the member for the tombstone to outrank.
		if !known && ParseState(rec.State) == StateDead {
			continue
		}
		// Roles are ordered by Seq, so a relayed role is as good as a first-hand
		// one: Upsert keeps whichever claim is newer. A record without Seq
		// (an older build) is unordered, and for those a role is only believed
		// first-hand: a relayed one is whatever the relay knew when it last
		// looked, and applying it could erase an incumbency on this node alone.
		//
		// Looked up by binary search rather than View.Get, which scans: a scan
		// per record would leave the loop O(N^2) even with the snapshot hoisted.
		// before came from Snapshot, so it is sorted by ID; View.Get cannot
		// assume that for an arbitrary View.
		role := ParseRole(rec.Role)
		if rec.Seq == 0 && rec.ID != from {
			role = RoleWorker
			if known {
				role = before.Members[i].Role
			}
		}
		// Dial before merging: once the record is in the table, Resolve knows
		// the address and connect would take it for an existing peer.
		if ParseState(rec.State) == StateAlive {
			n.connect(rec.Advertise)
		}
		if n.table.Upsert(Member{
			ID:          rec.ID,
			Addr:        rec.Advertise,
			Incarnation: rec.Incarnation,
			Role:        role,
			State:       ParseState(rec.State),
			Score:       score,
			Seq:         rec.Seq,
		}) {
			changed = true
		}
	}
	if changed {
		n.voidAttachmentIfLeaderStepped(before)
		n.membershipChanged(ctx)
	}
}

// voidAttachmentIfLeaderStepped forgets our attachment when the leader we are
// attached to has just claimed to be a worker.
//
// A leader that steps down clears its attached set (evaluate), and it does
// not get it back when it steps up again. Our own election may never have
// dropped it -- views pass through different intermediate states on different
// nodes -- so without this we would stay "attached" to a leader that no longer
// lists us, and never JOIN again. The claim is Seq-ordered, so a stale one
// cannot trigger this. Voiding makes the next evaluate re-JOIN if the node is
// still our best leader, which is exactly the repair.
func (n *Node) voidAttachmentIfLeaderStepped(before View) {
	if n.leader == "" || n.leader == n.cfg.Self {
		return
	}
	i, ok := slices.BinarySearchFunc(before.Members, n.leader, compareMemberID)
	if !ok || before.Members[i].Role != RoleLeader {
		return
	}
	if m, ok := n.table.Snapshot().Get(n.leader); ok && m.Role != RoleLeader {
		n.log.Info("leader stepped down; attachment void", "leader", n.leader)
		n.leader = ""
	}
}

// compareMemberID orders a Member against an ID, for binary search over a
// Snapshot's ID-sorted Members.
func compareMemberID(m Member, id protocol.NodeID) int { return cmp.Compare(m.ID, id) }

// memberRecord is the wire form of m. Score is passed through as-is; the
// record's MarshalJSON turns an invalid score into the unmeasured sentinel.
func memberRecord(m Member) protocol.MemberRecord {
	return protocol.MemberRecord{
		ID:          m.ID,
		Advertise:   m.Addr,
		Incarnation: m.Incarnation,
		Role:        m.Role.String(),
		State:       m.State.String(),
		Score:       m.Score,
		Seq:         m.Seq,
	}
}

// announceSelf broadcasts this node's own record: its incarnation, role, and
// self-reported score. It is the one message that carries the score election
// needs, so it goes out on every change that matters (a refutation, a score
// move beyond the hysteresis margin) and is otherwise silent.
func (n *Node) announceSelf(ctx context.Context) {
	view := n.table.Snapshot()
	self, ok := view.Get(n.cfg.Self)
	if !ok {
		return
	}
	env, err := protocol.NewEnvelope(protocol.TypeMembershipDelta, n.cfg.Self, "", protocol.MembershipDeltaPayload{
		Members:     []protocol.MemberRecord{memberRecord(self)},
		ViewVersion: view.Version,
	})
	if err == nil {
		n.cfg.Transport.Broadcast(ctx, env)
	}
}

// refuteIfNeeded handles a rumour about this node.
//
// A record saying we are suspect or dead at an incarnation at least as new as our
// own would, if applied, be believed by every peer that hears it, and Upsert's
// equal-incarnation rule means we could never talk them out of it. The only
// refutation is a higher incarnation: bump past the rumour and re-announce. A
// record about us that is stale (lower incarnation) or benign (alive) is ignored;
// we are the authority on ourselves. The re-announce is a broadcast; if it is
// lost, anti-entropy carries our refuted record to each peer within a cycle.
func (n *Node) refuteIfNeeded(ctx context.Context, from protocol.NodeID, rec protocol.MemberRecord) {
	if ParseState(rec.State) == StateAlive || rec.Incarnation < n.incarnation {
		return
	}
	// rec.Incarnation is peer-supplied and unauthenticated. A bare +1 on
	// math.MaxInt64 wraps negative, and a node with a negative incarnation loses
	// every merge about itself for the rest of its life. NextIncarnation clamps.
	n.incarnation = NextIncarnation(rec.Incarnation)
	self, _ := n.table.Snapshot().Get(n.cfg.Self)
	n.table.Upsert(Member{
		ID:          n.cfg.Self,
		Addr:        n.cfg.Advertise,
		Incarnation: n.incarnation,
		Role:        self.Role,
		State:       StateAlive,
		Score:       self.Score,
		Seq:         self.Seq,
	})
	n.log.Warn("refuting rumour about self", "from", from, "rumour", rec.State, "incarnation", n.incarnation)
	n.announceSelf(ctx)
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
		// An acceptance only counts for the JOIN we are still waiting on. One
		// that arrives after we were promoted, or after we moved on to another
		// leader, is stale: applying it would leave a leader attached to someone
		// else, or a worker pointing at a leader it has abandoned. The sender
		// learns from our next HEARTBEAT_ACK that we are not its worker.
		if n.isLeader() || n.leader != leader {
			n.log.Debug("stale JOIN_ACK ignored", "from", from, "leader", leader, "current", n.leader)
			return
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
	n.rejections++
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
	// Suspect peers are probed too: a probe that keeps failing is what turns the
	// suspicion into a death (DeadAfter), and one that succeeds clears it.
	var targets []Member
	for _, m := range n.table.Snapshot().Members {
		if m.State != StateDead && m.ID != n.cfg.Self && m.Addr != "" {
			targets = append(targets, m)
		}
	}
	n.probing = true
	n.probeSeq++
	seq := n.probeSeq

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
		case n.probeCh <- probeRound{seq: seq, results: results}:
		case <-ctx.Done():
		}
	}()
}

// applyProbeRound folds a finished round into scores using the call shape
// mandated in WORKLOG 3.4. Loop goroutine only.
func (n *Node) applyProbeRound(ctx context.Context, r probeRound) {
	n.probing = false
	n.rejections = 0
	view := n.table.Snapshot()
	// Sorted, so that when several peers cross a threshold in one round the
	// pushes and log lines come out in the same order every run.
	peers := make([]protocol.NodeID, 0, len(r.results))
	for id := range r.results {
		peers = append(peers, id)
	}
	sortIDs(peers)
	changed := false
	for _, id := range peers {
		res := r.results[id]
		// A round outlives the membership it was started from. A peer that died
		// or left while its probe was in flight has already been pruned by
		// membershipChanged; writing its result now would re-insert exactly the
		// stale entry pruneLocal exists to remove.
		m, ok := view.Get(id)
		if !ok || m.State == StateDead {
			continue
		}
		switch {
		case health.IsCancellation(res.err):
			// Shutdown or a superseded round: not a fact about the peer. The old
			// score stands.
		case res.err != nil:
			n.local[id] = health.ScoreUnavailable()
			n.missed[id]++
			n.log.Debug("probe failed", "peer", id, "missed", n.missed[id], "err", res.err)
			if n.probeMissed(ctx, id, res.err) {
				changed = true
			}
		default:
			n.local[id] = res.score
			n.missed[id] = 0
			if m.State == StateSuspect && n.probeClears(id, r.seq, m.Incarnation) {
				changed = true
			}
		}
	}
	if changed {
		n.retainLive()
	}
	n.updateSelfScore(ctx)
}

// updateSelfScore recomputes the self-reported score and, if it moved by more
// than the election hysteresis, records it and gossips it. The margin is the
// rate limit: a score that wobbles by less than the amount that could change an
// election is not worth a broadcast to every peer. At most one announcement per
// probe round follows from this being called once per round.
func (n *Node) updateSelfScore(ctx context.Context) {
	next := n.selfScore()
	n.local[n.cfg.Self] = next
	prev := health.ScoreUnavailable()
	if m, ok := n.table.Snapshot().Get(n.cfg.Self); ok {
		prev = m.Score
	}
	margin := n.cfg.Election.withDefaults().Hysteresis
	if health.IsValidScore(prev) && health.IsValidScore(next) && abs(next-prev) < margin {
		return
	}
	if n.table.SetScore(n.cfg.Self, next) {
		n.announceSelf(ctx)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// selfScore is the score this node reports about itself: the median of its
// valid local round trips to its peers.
//
// # Why election runs on self-reported scores, not measured ones
//
// A round trip is a fact about a pair, not about a node: A's RTT to B is not
// C's RTT to B. Elect is documented as giving every node the same answer from
// the same view, and that only holds if its inputs are the same on every node.
// Ranking on locally measured RTTs breaks it outright -- three nodes elect three
// different leader sets, workers JOIN nodes that do not believe they are
// leaders, and the rejections loop forever. The cheapest input that IS the same
// everywhere is one each node computes about itself and gossips: this median.
// It is a heuristic (a node with one slow link and one fast link reports the
// average), but it is a symmetric heuristic, and symmetry is the property the
// election needs. Affinity, by contrast, is meant to be observer-relative --
// "which leader is closest to ME" -- so it keeps using the local map.
//
// # Two different kinds of "no median"
//
// Scores are lower-is-better, so 0 is the best value in the ranking and must only
// ever be reported as a claim, never as a shrug. Two states produce an empty
// sample set and they are not the same fact:
//
//   - No peer entries at all. We are alone: nobody has been probed because there
//     is nobody to probe. 0 is right. A lone node must be electable, and it is the
//     value every node reports before it has company, so the first election is the
//     deterministic lowest-ID tie-break.
//   - Peer entries exist and not one of them is valid. Our probe path is broken --
//     PINGs unanswered, PONGs firewalled, the prober rejecting every peer -- and we
//     know nothing about ourselves. Reporting 0 here would rank us FIRST in every
//     election in the swarm on the strength of our own blindness: we would displace
//     healthy incumbents, and then workers could not even attach to us, because
//     their ChooseLeader sees our NaN and skips us. Elected and unjoinable. So the
//     honest answer is ScoreUnavailable, which Elect's eligibility gate reads as
//     "not a leadership candidate".
//
// Excluding ourselves is safe: if every node is unmeasured, Elect falls back to the
// lowest alive ID rather than leaving the swarm leaderless, so an all-blind swarm
// still converges on one leader instead of stalling.
func (n *Node) selfScore() float64 {
	var (
		valid []float64
		peers int
	)
	for id, s := range n.local {
		if id == n.cfg.Self {
			// Our own entry is the previous answer written back by
			// updateSelfScore, not evidence about a link. Counting it would make
			// "alone" indistinguishable from "blind" from the second round on.
			continue
		}
		peers++
		if health.IsValidScore(s) {
			valid = append(valid, s)
		}
	}
	switch {
	case peers == 0:
		return 0
	case len(valid) == 0:
		return health.ScoreUnavailable()
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
	// Election ranks on the self-reported scores in the view; affinity below
	// ranks on local measurements. See selfScore for why they must differ.
	n.reported = view.Scores()
	res := Elect(view, n.reported, n.cfg.Election)

	// Only OUR OWN role is written from the local result. Peers' roles in the
	// table are their own claims, learned from their announcements, and that is
	// deliberate: hysteresis protects incumbents, so "who is an incumbent" must
	// be the same fact on every node or the election is not symmetric. Every
	// node boots alone and elects itself; if each then treated its own local
	// result as incumbency, near-equal scores (a loopback Docker network) would
	// leave every node protecting itself forever, N leaders for N nodes. With
	// claimed roles, N bootstrap claimants collapse to the deterministic
	// tie-break everywhere. A role change is announced so peers learn it.
	myRole := RoleWorker
	if contains(res.Leaders, n.cfg.Self) {
		myRole = RoleLeader
	}
	if n.table.SetRole(n.cfg.Self, myRole) {
		n.announceSelf(ctx)
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
		// ours to lead. A suspect worker stays: its suspicion resolves one way
		// or the other within SuspicionTimeout, and dropping it on the doubt
		// would force a re-JOIN for nothing if it is refuted.
		//
		// "Promoted" means the worker CLAIMS leadership, not that our election
		// momentarily includes it. Views pass through different intermediate
		// states on different nodes; dropping a worker on our own transient
		// result leaves it believing it is attached (it never promoted, so it
		// never re-JOINs) while we no longer list it.
		for id := range n.attached {
			m, ok := view.Get(id)
			if !ok || m.State == StateDead || m.Role == RoleLeader {
				delete(n.attached, id)
			}
		}
	} else {
		// A demoted leader has no cluster; whoever was attached will re-home.
		for id := range n.attached {
			delete(n.attached, id)
		}
		usable := n.usableLeaders(res.Leaders)
		// A leader we suspect first-hand is not one we can stay with, even while
		// the election keeps its seat: this is the failover step. Detaching now,
		// rather than waiting for ShouldRehome to find a better leader, is what
		// lets the worker be "detached" (and say so in telemetry) when the
		// suspect was the only leader.
		if n.leader != "" && !contains(usable, n.leader) {
			n.log.Info("detaching from unusable leader", "leader", n.leader)
			n.leader = ""
		}
		best, ok := ChooseLeader(n.local, usable)
		if n.hint != "" && contains(usable, n.hint) {
			best, ok = n.hint, true
		}
		n.hint = ""
		if n.rejections > maxRejectionsPerRound {
			ok = false
		}
		if ok && ShouldRehome(n.leader, best, n.local, usable, n.cfg.Rehome) {
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
		Score:  n.local[leader],
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
	n.log.Info("joining leader", "leader", leader, "score", n.local[leader])
	n.leader = leader
	// A new attachment gets a full HeartbeatMisses window before its silence
	// counts against the leader.
	n.sinceBeat = 0
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
		Scores:      make(map[protocol.NodeID]float64, len(n.local)),
		Reported:    make(map[protocol.NodeID]float64, len(n.reported)),
		Missed:      make(map[protocol.NodeID]int, len(n.missed)),
		Degraded:    n.degraded,
		HighWater:   n.highWater,
		Attached:    make([]protocol.NodeID, 0, len(n.attached)),
		Claims:      make(map[protocol.NodeID]protocol.ElectionResultPayload, len(n.claims)),
		Dropped:     n.dropped.Load(),
	}
	for id, v := range n.local {
		s.Scores[id] = v
	}
	for id, v := range n.reported {
		s.Reported[id] = v
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
