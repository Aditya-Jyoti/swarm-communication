package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// ControlCenterID is the NodeID the Control Center announces in its HELLO_ACK.
const ControlCenterID protocol.NodeID = "control-center"

// Defaults for Config.
const (
	DefaultInterval      = time.Second
	DefaultSubmitTimeout = 2 * time.Second
	DefaultResultBuffer  = 256
	inboundDepth         = 64
)

// Sentinel errors.
var (
	// ErrNotControlCenter means the configured address answered with a NodeID
	// other than ControlCenterID -- almost certainly SWARM_CONTROL_CENTER pointing
	// at a mesh node. Terminal: Run stops rather than keep a bogus link that the
	// mesh node would treat as a second connection from us.
	ErrNotControlCenter = errors.New("telemetry: peer is not the control center")
	// ErrAlreadyRunning is returned by a second call to Run.
	ErrAlreadyRunning = errors.New("telemetry: client already running")
)

// Config configures a Client. Self, Addr and Snapshot are required.
type Config struct {
	// Self is the node's identity. Only ID and Advertise are announced; see New
	// for why the incarnation is not.
	Self network.Identity
	// Addr is the Control Center's host:port.
	Addr protocol.NodeAddress
	// Interval is the TELEMETRY period. Default 1s.
	Interval time.Duration
	// Snapshot produces the payload to send. Called on the Run goroutine.
	// Typically func() { return FromStatus(node.Status()) }.
	Snapshot func() protocol.TelemetryPayload
	// SubmitTask receives every TASK from the CC. Optional: nil logs and drops.
	// It is called on the Run goroutine under a SubmitTimeout context.
	SubmitTask func(context.Context, protocol.TaskPayload) error
	// OnChaos receives every valid CHAOS instruction, on the Run goroutine.
	// Invalid ones are logged and never reach it. Optional.
	OnChaos func(Chaos)
	// SubmitTimeout bounds one SubmitTask call. Default 2s.
	SubmitTimeout time.Duration
	// ResultBuffer bounds TASK_RESULTs held while the link is down. Default 256.
	ResultBuffer int
	// Clock drives the telemetry ticker. Default cluster.RealClock.
	Clock cluster.Clock
	// Conn, Dialer and Backoff are passed to the private pool. Zero values take
	// the pool's defaults (exponential backoff with jitter, capped at 10s).
	Conn    network.ConnConfig
	Dialer  network.Dialer
	Backoff func(attempt int) time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Client is a node's connection to the Control Center.
//
// # Goroutine ownership
//
// Run owns one goroutine (the caller's) and the private pool. Everything that
// touches the link -- sends, the connected flag, the pending result buffer --
// happens on it, so none of that state needs a lock. Two things cross in from
// other goroutines, both through channels:
//
//   - SendResult (the node's loop goroutine) -> results. Non-blocking: the node
//     loop must never wait on the dashboard.
//   - the pool's reader goroutine (handle) -> inbound. Blocking, but bounded by
//     done: a CHAOS "clear" is control plane and must not be shed, and a reader
//     parked here only delays the CC link's own read deadline.
//
// Run closes done before closing the pool, so a reader parked in handle is
// released before Pool.Close waits for the connection to end.
type Client struct {
	cfg Config
	log *slog.Logger

	results chan protocol.TaskResultPayload
	inbound chan *protocol.Envelope
	done    chan struct{}

	started atomic.Bool
	// droppedResults counts results shed because a buffer was full. Atomic:
	// incremented by SendResult on the node loop and by Run.
	droppedResults atomic.Uint64
	// connected mirrors Run's local flag for observers (tests, logs). Atomic
	// because it is read from other goroutines; Run is its only writer.
	connected atomic.Bool
}

// New validates cfg and returns a Client. It opens nothing until Run.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.Self.ID == "":
		return nil, errors.New("telemetry: Config.Self.ID is required")
	case cfg.Self.ID == ControlCenterID:
		return nil, fmt.Errorf("telemetry: node id %q is reserved for the control center", ControlCenterID)
	case cfg.Addr == "":
		return nil, fmt.Errorf("telemetry: node %q: Config.Addr is required", cfg.Self.ID)
	case cfg.Snapshot == nil:
		return nil, fmt.Errorf("telemetry: node %q: Config.Snapshot is required", cfg.Self.ID)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.SubmitTimeout <= 0 {
		cfg.SubmitTimeout = DefaultSubmitTimeout
	}
	if cfg.ResultBuffer <= 0 {
		cfg.ResultBuffer = DefaultResultBuffer
	}
	if cfg.Clock == nil {
		cfg.Clock = cluster.RealClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// Incarnation 0 on the CC link is a guard, not an omission. If Addr is
	// misconfigured to point at a mesh node, that node's pool sees a second
	// HELLO from our ID; at incarnation 0 it loses the tie-break against our
	// real mesh connection instead of replacing it. The CC does not use the
	// incarnation: its newest connection per node simply wins.
	cfg.Self.Incarnation = 0
	return &Client{
		cfg:     cfg,
		log:     cfg.Logger.With("node", cfg.Self.ID, "control_center", cfg.Addr),
		results: make(chan protocol.TaskResultPayload, cfg.ResultBuffer),
		inbound: make(chan *protocol.Envelope, inboundDepth),
		done:    make(chan struct{}),
	}, nil
}

// SendResult queues a TASK_RESULT for the CC. It never blocks; a result that
// does not fit is dropped and counted. Safe from any goroutine, before or
// during Run.
func (c *Client) SendResult(r protocol.TaskResultPayload) {
	select {
	case c.results <- r:
	default:
		c.droppedResults.Add(1)
	}
}

// DroppedResults reports how many TASK_RESULTs were shed.
func (c *Client) DroppedResults() uint64 { return c.droppedResults.Load() }

// Connected reports whether the CC link is currently up.
func (c *Client) Connected() bool { return c.connected.Load() }

// Run maintains the link until ctx is cancelled. It returns nil on
// cancellation, ErrNotControlCenter if the address is not a CC, and
// ErrAlreadyRunning on a second call.
func (c *Client) Run(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	pool := network.NewPool(network.PoolConfig{
		Self:    c.cfg.Self,
		Conn:    c.cfg.Conn,
		Dialer:  c.cfg.Dialer,
		Backoff: c.cfg.Backoff,
		Handler: c.handle,
	})
	var closeOnce sync.Once
	shutdown := func() {
		closeOnce.Do(func() {
			close(c.done)
			_ = pool.Close()
			c.connected.Store(false)
		})
	}
	defer shutdown()

	ticker := c.cfg.Clock.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	// Only this address is ever dialled. The pool also accumulates addresses
	// from the handshake, but nothing reads them: this pool is private, so they
	// can never reach the mesh pool's Known() or NodeConfig.Connect.
	pool.Connect(c.cfg.Addr)
	c.log.Info("control center uplink starting", "interval", c.cfg.Interval)

	var (
		connected bool
		// pending holds results produced while the link was down, oldest
		// first, so a task finished during a CC restart is still reported.
		pending []protocol.TaskResultPayload
	)
	for {
		select {
		case <-ctx.Done():
			c.log.Info("control center uplink stopping")
			return nil

		case ev, ok := <-pool.Events():
			if !ok {
				return nil
			}
			switch ev.Kind {
			case network.PeerUp:
				if ev.Peer.ID != ControlCenterID {
					err := fmt.Errorf("%w: node %q dialled %s and reached %q", ErrNotControlCenter, c.cfg.Self.ID, c.cfg.Addr, ev.Peer.ID)
					c.log.Error("control center uplink misconfigured", "err", err)
					return err
				}
				connected = true
				c.connected.Store(true)
				c.log.Info("control center connected")
				// Report at once rather than up to one interval later: the
				// dashboard should show a restarted node immediately.
				c.sendTelemetry(ctx, pool)
				for _, r := range pending {
					c.sendResult(ctx, pool, r)
				}
				pending = nil
			case network.PeerDown:
				connected = false
				c.connected.Store(false)
				c.log.Warn("control center disconnected", "disposition", ev.Disposition, "err", ev.Err)
			}

		case <-ticker.C():
			if connected {
				c.sendTelemetry(ctx, pool)
			}

		case r := <-c.results:
			if connected {
				c.sendResult(ctx, pool, r)
				continue
			}
			if len(pending) >= c.cfg.ResultBuffer {
				// Drop the oldest: the newest results are the ones the
				// dashboard has not yet had any chance to see.
				pending = pending[1:]
				c.droppedResults.Add(1)
			}
			pending = append(pending, r)

		case env := <-c.inbound:
			c.dispatch(ctx, pool, env)
		}
	}
}

// handle is the pool's frame handler, on the connection's reader goroutine.
func (c *Client) handle(peer protocol.NodeID, env *protocol.Envelope) {
	if peer != ControlCenterID {
		return
	}
	switch env.Type {
	case protocol.TypeTask, protocol.TypeChaos, protocol.TypePing:
	default:
		// Unknown or unexpected types are dropped, never fatal (see
		// protocol.MessageType's forward-compatibility policy).
		return
	}
	select {
	case c.inbound <- env:
	case <-c.done:
	}
}

// dispatch acts on one inbound frame. Run goroutine only.
func (c *Client) dispatch(ctx context.Context, pool *network.Pool, env *protocol.Envelope) {
	switch env.Type {
	case protocol.TypeTask:
		t, err := protocol.PayloadOf[protocol.TaskPayload](env)
		if err != nil {
			c.log.Warn("bad TASK from control center", "err", err)
			return
		}
		if c.cfg.SubmitTask == nil {
			c.log.Warn("TASK dropped: node accepts no tasks", "task_id", t.TaskID)
			return
		}
		// Bounded, and on this goroutine: SubmitTask hands the task to the
		// node loop and returns, so the wait is short, and doing it here keeps
		// tasks in the order the CC sent them.
		sctx, cancel := context.WithTimeout(ctx, c.cfg.SubmitTimeout)
		err = c.cfg.SubmitTask(sctx, t)
		cancel()
		if err != nil {
			c.log.Warn("task submit failed", "task_id", t.TaskID, "kind", t.Kind, "err", err)
			// Report the failure so the task does not sit "pending" on the
			// dashboard for ever. A node that cannot run it will never
			// produce a result any other way.
			c.sendResult(ctx, pool, protocol.TaskResultPayload{
				TaskID: t.TaskID,
				Worker: c.cfg.Self.ID,
				OK:     false,
				Output: "submit failed: " + err.Error(),
			})
		}

	case protocol.TypeChaos:
		p, err := protocol.PayloadOf[protocol.ChaosPayload](env)
		if err != nil {
			c.log.Warn("bad CHAOS from control center", "err", err)
			return
		}
		ch, err := ParseChaos(p)
		if err != nil {
			c.log.Warn("CHAOS rejected", "err", err)
			return
		}
		c.log.Info("chaos received", "action", ch.Action, "delay", ch.Delay)
		if c.cfg.OnChaos != nil {
			c.cfg.OnChaos(ch)
		}

	case protocol.TypePing:
		// The CC pings so this side's read deadline is refreshed on an
		// otherwise one-way link; answering does the same for the CC when the
		// telemetry interval is long.
		p, err := protocol.PayloadOf[protocol.PingPayload](env)
		if err != nil {
			return
		}
		//lint:ignore S1016 field by field on purpose; see network.MeshProber.answer
		pong, err := protocol.NewReply(env, protocol.TypePong, c.cfg.Self.ID, protocol.PongPayload{Nonce: p.Nonce, Seq: p.Seq})
		if err != nil {
			return
		}
		_ = pool.Send(ctx, ControlCenterID, pong)
	}
}

func (c *Client) sendTelemetry(ctx context.Context, pool *network.Pool) {
	payload := c.cfg.Snapshot()
	env, err := protocol.NewEnvelope(protocol.TypeTelemetry, c.cfg.Self.ID, ControlCenterID, payload)
	if err != nil {
		c.log.Error("encode TELEMETRY", "err", err)
		return
	}
	// A data-plane send never blocks: a full queue sheds the sample, which the
	// next tick replaces anyway.
	if err := pool.Send(ctx, ControlCenterID, env); err != nil {
		c.log.Debug("TELEMETRY not sent", "err", err)
	}
}

func (c *Client) sendResult(ctx context.Context, pool *network.Pool, r protocol.TaskResultPayload) {
	env, err := protocol.NewEnvelope(protocol.TypeTaskResult, c.cfg.Self.ID, ControlCenterID, r)
	if err != nil {
		c.log.Error("encode TASK_RESULT", "task_id", r.TaskID, "err", err)
		return
	}
	if err := pool.Send(ctx, ControlCenterID, env); err != nil {
		c.droppedResults.Add(1)
		c.log.Warn("TASK_RESULT not sent", "task_id", r.TaskID, "err", err)
	}
}
