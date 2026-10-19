package mavlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/protocol"
)

// Bridge defaults and bounds.
const (
	// DefaultBind is the port every ground station expects a vehicle on.
	DefaultBind = "0.0.0.0:14550"
	// DefaultStatusInterval is the HEARTBEAT and SYS_STATUS period. 1 Hz is the
	// MAVLink convention and what a ground station times its link-lost
	// detection against, so it is not a tuning knob.
	DefaultStatusInterval = time.Second
	// DefaultPositionInterval is the GLOBAL_POSITION_INT period. 5 Hz: slow
	// enough to be nothing on a LAN, fast enough that a ground station
	// interpolates smooth motion instead of showing a dot that jumps.
	DefaultPositionInterval = 200 * time.Millisecond
	// DefaultSyncInterval is how often positions are pushed into the swarm and
	// swarm state is read back. See the "Two clocks" section in doc.go.
	DefaultSyncInterval = time.Second

	// writeTimeout bounds one datagram write. A UDP send normally completes
	// into the socket buffer immediately, but "normally" is not a deadline, and
	// a full send buffer must not stall the loop that is also answering a
	// ground station.
	writeTimeout = time.Second
	// readTimeout bounds one blocking read. The reader also exits when the
	// socket is closed; the deadline is the second exit path, so a read can
	// never outlive a cancelled context by more than this.
	readTimeout = time.Second

	// inboxDepth bounds datagrams queued from the reader to the loop. Deep
	// enough for a ground station's connect-time burst (a parameter list
	// request per vehicle), shallow enough that a flood is dropped rather than
	// buffered.
	inboxDepth = 64
	// eventDepth bounds swarm events queued for STATUSTEXT. One per election or
	// failure, so a burst is a whole swarm restarting.
	eventDepth = 64

	// positionEpsilon is how far the swarm's idea of a node's position may
	// differ from what the bridge last pushed before the bridge concludes
	// somebody else moved it. In cube units: 1e-6 of a 20 m unit is 20 um, far
	// below any real move and above float64 round-tripping through JSON.
	positionEpsilon = 1e-6

	// maxStatusText is STATUSTEXT's text field. Longer messages are truncated
	// rather than chunked; the id/chunk_seq extension exists for long text, and
	// a swarm event that does not fit in 50 characters is badly phrased.
	maxStatusText = 50
)

// MAV_SEVERITY values, which dialect.go does not generate (the generator emits
// only the enums it was asked for). MAVLink reuses the RFC-5424 levels
// verbatim, lower being worse.
const (
	SeverityCritical = 2
	SeverityWarning  = 4
	SeverityInfo     = 6
)

// Swarm event kinds the bridge turns into STATUSTEXT. They are the Control
// Center's own event kind strings; they live here as constants so the bridge
// does not import the Control Center (the dependency runs the other way).
const (
	EventLeaderChange = "leader_change"
	EventNodeDown     = "node_down"
	EventNodeUp       = "node_up"
)

// ErrBridgeRunning is returned by a second call to Bridge.Run.
var ErrBridgeRunning = errors.New("mavlink: bridge already running")

// NodeState is one swarm node as the bridge needs to see it.
//
// A flat value, not a pointer into somebody's state: the Control Center's hub
// owns its node table on one goroutine, and the only safe thing to hand out of
// it is a copy.
type NodeState struct {
	// ID is the swarm node id, which maps to a system id via Vehicles.
	ID protocol.NodeID
	// Leader is true while the node reports role leader. It becomes
	// HEARTBEAT.custom_mode.
	Leader bool
	// Connected is false for a node that is killed, disconnected or expired. A
	// vehicle heartbeats exactly while this is true, which is how a failover
	// looks like a lost link in a ground station.
	Connected bool
	// Pos is the swarm's current position for the node, in cube units. The
	// bridge compares it against what it last pushed to detect an external
	// move; see Bridge.
	Pos protocol.Position
	// Dropped and LedgerSize are mapped onto SYS_STATUS counters; see
	// Bridge.sysStatus for the mapping and why it is a lie worth telling.
	Dropped    uint64
	LedgerSize int
}

// SwarmSource is everything the bridge needs from the swarm.
//
// Two methods, both taking a context, because both are calls into another
// goroutine's state and either may be in flight when the process is shutting
// down. Keeping the interface this small is what lets the bridge be tested
// against a fake swarm with no Control Center, no TCP and no hub.
type SwarmSource interface {
	// Nodes returns the swarm's current nodes. Called once per sync interval.
	Nodes(ctx context.Context) ([]NodeState, error)

	// SetPositions writes the bridge's positions back into the swarm. Called
	// once per sync interval with one entry per flyable vehicle, which is why
	// the whole map is passed rather than a delta: the receiver stores a
	// snapshot and the call is idempotent.
	SetPositions(ctx context.Context, positions map[protocol.NodeID]protocol.Position) error
}

// BridgeConfig configures a Bridge. Every zero value has a usable default
// except Source, which is required.
type BridgeConfig struct {
	// Bind is the UDP address to listen on. Default DefaultBind.
	Bind string
	// GCS is the ground station to send to, host:port. It may be empty: the
	// bridge also learns the address of any ground station that sends to it, so
	// an inbound link works unconfigured. Resolved once, at NewBridge.
	GCS string
	// Projection maps cube units to WGS-84. Default DefaultProjection().
	Projection Projection
	// Move tunes flight and idle drift.
	Move MoveConfig
	// StatusInterval is the HEARTBEAT and SYS_STATUS period. Default 1s.
	StatusInterval time.Duration
	// PositionInterval is the GLOBAL_POSITION_INT period. Default 200ms.
	PositionInterval time.Duration
	// SyncInterval is the swarm read/write period. Default 1s.
	SyncInterval time.Duration
	// Source is the swarm. Required.
	Source SwarmSource
	// Now is the clock. Default time.Now. Socket deadlines do not use it: a
	// deadline is a real instant to the kernel, and a test clock that stands
	// still would be an instant timeout.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c BridgeConfig) withDefaults() BridgeConfig {
	if c.Bind == "" {
		c.Bind = DefaultBind
	}
	if c.Projection == (Projection{}) {
		c.Projection = DefaultProjection()
	}
	if c.StatusInterval <= 0 {
		c.StatusInterval = DefaultStatusInterval
	}
	if c.PositionInterval <= 0 {
		c.PositionInterval = DefaultPositionInterval
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = DefaultSyncInterval
	}
	c.Move = c.Move.withDefaults()
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// vehicle is one swarm node as a MAVLink vehicle. Loop-owned; see Bridge.
type vehicle struct {
	id     protocol.NodeID
	sysID  byte
	mover  *Mover
	leader bool
	// connected gates every outbound message for this vehicle.
	connected bool
	// pushed is the position last handed to the swarm. An external move is
	// detected by the swarm reporting something else.
	pushed protocol.Position
	// dropped and ledger are the node's last reported counters; lastDropped is
	// the value at the previous SYS_STATUS, for the drop rate.
	dropped     uint64
	lastDropped uint64
	ledger      int
	// seen marks presence in the newest swarm snapshot, for pruning.
	seen bool
}

// datagram is one received packet, handed from the reader to the loop.
type datagram struct {
	// b is a private copy: the reader reuses its receive buffer immediately.
	b    []byte
	from *net.UDPAddr
}

// swarmEvent is one swarm event waiting to become a STATUSTEXT.
type swarmEvent struct {
	kind   string
	node   protocol.NodeID
	detail string
}

// syncReq hands a swarm snapshot to the loop and takes the positions to push
// back. reply is buffered, so the loop never blocks answering.
type syncReq struct {
	nodes []NodeState
	reply chan map[protocol.NodeID]protocol.Position
}

// Bridge presents every swarm node to a ground station as its own MAVLink
// vehicle over UDP.
//
// # Goroutine ownership
//
// Three goroutines, started and joined by Run:
//
//   - The loop (Run's own goroutine) is the single owner of every piece of
//     mutable state here: the vehicle table, the movers, the destination
//     addresses and the encode scratch buffer. It is also the only writer to
//     the socket. Nothing else touches any of it, so there are no locks on it,
//     and "who moved this vehicle" has one answer.
//   - The reader blocks on the socket and forwards datagrams to the loop over
//     inbox. It cannot leak: the socket has a read deadline, and Run closes the
//     socket, either of which ends the read.
//   - The syncer drives the 1 Hz exchange with the swarm, so a slow or stuck
//     SwarmSource delays positions rather than stopping heartbeats. It cannot
//     leak: every channel operation it makes also selects on the context.
//
// The exceptions to loop ownership are stated where they are: Vehicles and
// Sequencer carry their own mutexes, and the counters are atomics, so a test or
// a metrics endpoint can read them from anywhere.
//
// # External moves win
//
// The dashboard and POST /api/sim also write positions. At each sync the bridge
// compares the swarm's position for a node against what it last pushed; a
// difference beyond positionEpsilon means a human moved it, and the bridge
// adopts that position as the node's current and home position rather than
// dragging it back. A vehicle flying a mission is the one case that should
// override an operator, and that hook is in applyNodes.
type Bridge struct {
	cfg  BridgeConfig
	log  *slog.Logger
	conn *net.UDPConn
	// configured is the GCS from configuration, resolved once. Read-only.
	configured *net.UDPAddr

	started   atomic.Bool
	closeOnce sync.Once

	// vehicles and seq carry their own locks; see their types.
	vehicles *Vehicles
	seq      Sequencer

	// Counters, atomic so anything may read them. Written only by the loop.
	malformed atomic.Uint64
	dropped   atomic.Uint64 // inbound datagrams dropped because inbox was full
	sent      atomic.Uint64 // frames written (per destination)
	sendErrs  atomic.Uint64

	// --- loop-owned, no synchronisation ---
	vs      map[protocol.NodeID]*vehicle
	order   []protocol.NodeID // vs keys, sorted: deterministic emission order
	learned *net.UDPAddr      // newest inbound sender
	out     []byte            // encode scratch, reused
	boot    time.Time
	lastPos time.Time

	inbox  chan datagram
	events chan swarmEvent
	syncs  chan syncReq
}

// NewBridge opens the UDP socket. Nothing is sent or received until Run.
//
// The socket is opened here, not in Run, for the same reason the Control Center
// binds its listener in New: a port already in use is a configuration error the
// process should report before it claims to be up.
func NewBridge(cfg BridgeConfig) (*Bridge, error) {
	cfg = cfg.withDefaults()
	if cfg.Source == nil {
		return nil, errors.New("mavlink: bridge needs a SwarmSource")
	}
	bindAddr, err := net.ResolveUDPAddr("udp", cfg.Bind)
	if err != nil {
		return nil, fmt.Errorf("mavlink: resolve bind address %q: %w", cfg.Bind, err)
	}
	var gcs *net.UDPAddr
	if cfg.GCS != "" {
		// Resolved once. A ground station that moves is picked up by the
		// learned address instead; re-resolving on every send would put a DNS
		// lookup in a 5 Hz loop.
		if gcs, err = net.ResolveUDPAddr("udp", cfg.GCS); err != nil {
			return nil, fmt.Errorf("mavlink: resolve GCS address %q: %w", cfg.GCS, err)
		}
	}
	conn, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("mavlink: listen on %s: %w", cfg.Bind, err)
	}
	return &Bridge{
		cfg:        cfg,
		log:        cfg.Logger.With("component", "mavlink"),
		conn:       conn,
		configured: gcs,
		vehicles:   NewVehicles(),
		vs:         make(map[protocol.NodeID]*vehicle),
		out:        make([]byte, 0, MaxFrameLen),
		boot:       cfg.Now(),
		lastPos:    cfg.Now(),
		inbox:      make(chan datagram, inboxDepth),
		events:     make(chan swarmEvent, eventDepth),
		syncs:      make(chan syncReq),
	}, nil
}

// Addr is the bound local address.
func (b *Bridge) Addr() net.Addr { return b.conn.LocalAddr() }

// Close closes the socket. Idempotent, and safe to call while Run is in
// flight: that is how Run unblocks its reader.
func (b *Bridge) Close() error {
	var err error
	b.closeOnce.Do(func() { err = b.conn.Close() })
	return err
}

// BridgeStats reports what the bridge has seen. Safe to read from any
// goroutine.
type BridgeStats struct {
	// Malformed datagrams were not MAVLink, or were corrupt, or carried a
	// message this build does not implement. Counted and dropped.
	Malformed uint64
	// DroppedIn datagrams arrived faster than the loop could handle them.
	DroppedIn uint64
	// Sent is frames written, counted once per destination.
	Sent uint64
	// SendErrors is failed writes.
	SendErrors uint64
}

// Stats reads the counters. They are independent atomics, so the four values
// may be a few microseconds apart from each other; they are diagnostics, not an
// invariant.
func (b *Bridge) Stats() BridgeStats {
	return BridgeStats{
		Malformed:  b.malformed.Load(),
		DroppedIn:  b.dropped.Load(),
		Sent:       b.sent.Load(),
		SendErrors: b.sendErrs.Load(),
	}
}

// OnEvent queues a swarm event for STATUSTEXT. Safe from any goroutine, and it
// never blocks: an event is dropped when the queue is full.
//
// It is non-blocking because the caller is the Control Center's hub goroutine,
// which owns all of the CC's state. A bridge that could block there would stall
// every node's telemetry to get a status message onto a map.
func (b *Bridge) OnEvent(kind string, node protocol.NodeID, detail string) {
	switch kind {
	case EventLeaderChange, EventNodeDown, EventNodeUp:
	default:
		return // not a swarm topology change; the dashboard still gets it
	}
	select {
	case b.events <- swarmEvent{kind: kind, node: node, detail: detail}:
	default:
		// No counter and no log: the only way to get here is an event storm,
		// and logging per dropped event would turn one storm into two.
	}
}

// Run serves the bridge until ctx is cancelled, then closes the socket and
// joins every goroutine it started. A Bridge runs once.
func (b *Bridge) Run(ctx context.Context) error {
	if !b.started.CompareAndSwap(false, true) {
		return ErrBridgeRunning
	}
	defer func() { _ = b.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.readLoop(ctx) }()
	go func() { defer wg.Done(); b.syncLoop(ctx) }()

	b.log.Info("mavlink bridge listening", "addr", b.conn.LocalAddr(), "gcs", b.cfg.GCS)
	b.loop(ctx)

	// Order: cancel so the syncer stops offering work, then close the socket so
	// a blocked read returns at once rather than after readTimeout.
	cancel()
	_ = b.Close()
	wg.Wait()
	b.log.Info("mavlink bridge stopped", "stats", b.Stats())
	return nil
}

// loop is the bridge's single state owner. See Bridge.
func (b *Bridge) loop(ctx context.Context) {
	status := time.NewTicker(b.cfg.StatusInterval)
	defer status.Stop()
	pos := time.NewTicker(b.cfg.PositionInterval)
	defer pos.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case d := <-b.inbox:
			b.onDatagram(d)
		case e := <-b.events:
			b.onEvent(e)
		case r := <-b.syncs:
			r.reply <- b.applyNodes(r.nodes)
		case <-status.C:
			b.tickStatus()
		case <-pos.C:
			b.tickPositions()
		}
	}
}

// readLoop owns the read side of the socket. It never decodes anything: parsing
// on the loop keeps all protocol state in one place, and keeps this goroutine
// short enough to be obviously leak-free.
func (b *Bridge) readLoop(ctx context.Context) {
	buf := make([]byte, MaxFrameLen)
	for {
		if ctx.Err() != nil {
			return
		}
		// A real instant, not b.cfg.Now: see BridgeConfig.Now.
		if err := b.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return // socket closed
		}
		n, from, err := b.conn.ReadFromUDP(buf)
		if n > 0 {
			d := datagram{b: append([]byte(nil), buf[:n]...), from: from}
			select {
			case b.inbox <- d:
			case <-ctx.Done():
				return
			default:
				// Shed rather than queue. The alternative, blocking, would stop
				// this goroutine reading and let the kernel's receive buffer
				// absorb the flood instead -- same loss, later, with a
				// goroutine stuck in it.
				b.dropped.Add(1)
			}
		}
		if err == nil {
			continue
		}
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			continue // the deadline, not a failure
		}
		if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
			return // shutting down
		}
		// Anything else on an unconnected UDP socket is terminal enough to be
		// worth a line, and the loop above will exit on the next closed-socket
		// read if the socket is really gone.
		b.log.Warn("mavlink read failed", "err", err)
	}
}

// syncLoop drives the exchange with the swarm: read the swarm, hand it to the
// loop, push back the positions the loop produced.
//
// Nodes before SetPositions, deliberately. The loop needs the swarm's idea of a
// position to notice an external move, and reading it after writing would only
// ever show the bridge its own value.
func (b *Bridge) syncLoop(ctx context.Context) {
	t := time.NewTicker(b.cfg.SyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b.syncOnce(ctx)
	}
}

func (b *Bridge) syncOnce(ctx context.Context) {
	nodes, err := b.cfg.Source.Nodes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("mavlink: read swarm state", "err", err)
		}
		return
	}
	req := syncReq{nodes: nodes, reply: make(chan map[protocol.NodeID]protocol.Position, 1)}
	select {
	case b.syncs <- req:
	case <-ctx.Done():
		return
	}
	var positions map[protocol.NodeID]protocol.Position
	select {
	case positions = <-req.reply:
	case <-ctx.Done():
		return
	}
	if len(positions) == 0 {
		return
	}
	if err := b.cfg.Source.SetPositions(ctx, positions); err != nil && ctx.Err() == nil {
		b.log.Warn("mavlink: push positions", "err", err)
	}
}

// --- loop: swarm state ---------------------------------------------------------

// applyNodes folds a swarm snapshot into the vehicle table and returns the
// positions to push back. Loop-owned.
func (b *Bridge) applyNodes(nodes []NodeState) map[protocol.NodeID]protocol.Position {
	for _, v := range b.vs {
		v.seen = false
	}
	for _, n := range nodes {
		if n.ID == "" {
			continue
		}
		v := b.vs[n.ID]
		if v == nil {
			sysID, err := b.vehicles.SysID(n.ID)
			if err != nil {
				// More than 254 nodes. Nothing to retry; the swarm is larger
				// than MAVLink addressing, and the rest of the swarm still
				// works.
				b.log.Warn("mavlink: no system id for node", "peer", n.ID, "err", err)
				continue
			}
			v = &vehicle{
				id:     n.ID,
				sysID:  sysID,
				mover:  NewMover(n.ID, n.Pos, b.cfg.Move),
				pushed: n.Pos,
			}
			b.vs[n.ID] = v
			b.reorder()
			b.log.Info("mavlink vehicle", "peer", n.ID, "sysid", sysID)
		}
		v.seen = true
		v.leader = n.Leader
		v.connected = n.Connected
		v.dropped = n.Dropped
		v.ledger = n.LedgerSize

		// External moves win. A mission in flight is the one case that should
		// not be overridden by a stale slider -- that is the hook the mission
		// task needs: skip the adoption while v.mover.Flying().
		if lenP(subP(n.Pos, v.pushed)) > positionEpsilon {
			v.mover.Adopt(n.Pos)
			v.pushed = n.Pos
			b.log.Debug("mavlink: adopted external move", "peer", n.ID, "pos", n.Pos)
		}
	}
	pruned := false
	for id, v := range b.vs {
		if !v.seen {
			// The swarm has expired the node. Its system id is NOT released:
			// Vehicles holds an assignment for the process's lifetime, so a
			// node that comes back is the same vehicle rather than a second
			// aircraft next to the dead one.
			delete(b.vs, id)
			pruned = true
		}
	}
	if pruned {
		b.reorder()
	}

	out := make(map[protocol.NodeID]protocol.Position, len(b.vs))
	for id, v := range b.vs {
		if !v.connected {
			continue // nothing is flying a node that is not there
		}
		p := v.mover.Pos()
		out[id] = p
		v.pushed = p
	}
	return out
}

// reorder rebuilds the sorted key list. Emission order is sorted by node id so
// that a packet capture of one tick is reproducible, which is the difference
// between a diffable test and a flaky one.
func (b *Bridge) reorder() {
	b.order = b.order[:0]
	for id := range b.vs {
		b.order = append(b.order, id)
	}
	sort.Slice(b.order, func(i, j int) bool { return b.order[i] < b.order[j] })
}

// --- loop: outbound ------------------------------------------------------------

// tickStatus emits HEARTBEAT and SYS_STATUS for every connected vehicle.
func (b *Bridge) tickStatus() {
	for _, id := range b.order {
		v := b.vs[id]
		if v == nil || !v.connected {
			continue
		}
		b.send(v.sysID, b.heartbeat(v))
		b.send(v.sysID, b.sysStatus(v))
	}
}

// tickPositions advances every vehicle and emits GLOBAL_POSITION_INT.
//
// dt comes from the injected clock, not from the ticker period: a tick that was
// late (a stalled process, a loaded host) must advance the vehicle by the time
// that really passed, or the swarm's positions drift away from the ground
// station's. It is clamped because the other failure -- a clock that jumped --
// must not teleport the swarm.
func (b *Bridge) tickPositions() {
	now := b.cfg.Now()
	dt := now.Sub(b.lastPos)
	if maxStep := 5 * b.cfg.PositionInterval; dt > maxStep {
		dt = maxStep
	}
	b.lastPos = now
	for _, id := range b.order {
		v := b.vs[id]
		if v == nil || !v.connected {
			continue
		}
		v.mover.Step(dt)
		b.send(v.sysID, b.globalPosition(v, now))
	}
}

func (b *Bridge) heartbeat(v *vehicle) *Heartbeat {
	// custom_mode carries the swarm role, which is the one piece of swarm state
	// a ground station can show without a plugin: it renders as the flight
	// mode. MAV_AUTOPILOT_GENERIC, not the ArduPilot id -- claiming ArduPilot
	// makes a ground station demand a full parameter set and real flight modes.
	var mode uint32
	if v.leader {
		mode = 1
	}
	base := uint8(MavModeFlagCustomModeEnabled)
	state := uint8(MavStateStandby)
	if v.mover.Flying() {
		base |= MavModeFlagSafetyArmed
		state = MavStateActive
	}
	return &Heartbeat{
		CustomMode:     mode,
		Type:           MavTypeQuadrotor,
		Autopilot:      MavAutopilotGeneric,
		BaseMode:       base,
		SystemStatus:   state,
		MavlinkVersion: ProtocolVersion,
	}
}

// sysStatus maps swarm health onto an autopilot's health fields.
//
// There is no battery, no mainloop and no I2C bus here, so the mapping is a
// deliberate reinterpretation of fields a ground station already displays:
//
//	load          <- ledger size, in d% (10 = 1%), capped at 1000. The
//	                 replicated ledger is the closest thing a node has to a
//	                 work queue, so it is what "how busy is this node" means.
//	errors_comm   <- total frames the node has dropped, saturating.
//	drop_rate_comm<- frames dropped since the previous SYS_STATUS, in c%
//	                 (100 = 1%), so one drop per second reads as 1%.
//	errors_count1 <- ledger size again, unscaled, for the raw number.
//
// The battery fields carry their documented "not sent by autopilot" sentinels
// rather than invented values, so no ground station draws a fuel gauge for a
// vehicle that has no fuel.
func (b *Bridge) sysStatus(v *vehicle) *SysStatus {
	delta := v.dropped - v.lastDropped
	if v.dropped < v.lastDropped {
		delta = 0 // node restarted and its counter went back to zero
	}
	v.lastDropped = v.dropped
	return &SysStatus{
		// Nothing is claimed present, so nothing can be reported unhealthy. A
		// bitmap of invented sensors would earn a sensor warning per vehicle.
		OnboardControlSensorsPresent: 0,
		OnboardControlSensorsEnabled: 0,
		OnboardControlSensorsHealth:  0,
		Load:                         uint16(min(v.ledger*10, 1000)),
		VoltageBattery:               math.MaxUint16, // "not sent by autopilot"
		CurrentBattery:               -1,
		BatteryRemaining:             -1,
		DropRateComm:                 saturateU16(delta * 100),
		ErrorsComm:                   saturateU16(v.dropped),
		ErrorsCount1:                 saturateU16(uint64(max(v.ledger, 0))),
	}
}

func (b *Bridge) globalPosition(v *vehicle, now time.Time) *GlobalPositionInt {
	g := b.cfg.Projection.Project(v.mover.Pos())
	homeAlt := b.cfg.Projection.Project(v.mover.Home()).AltMM
	vel := v.mover.Vel()
	k := b.cfg.Projection.MetresPerUnit
	// cm/s in NED: x north, y east, z DOWN. The sign on z is the one that is
	// always wrong the first time -- the cube's Z is up.
	toCMS := func(unitsPerSec float64) int16 {
		return saturateI16(math.Round(unitsPerSec * k * 100))
	}
	hdg := uint16(math.Round(v.mover.Heading() * 100))
	if hdg >= 36000 {
		hdg = 0
	}
	return &GlobalPositionInt{
		TimeBootMs:  uint32(now.Sub(b.boot).Milliseconds()),
		Lat:         g.LatE7,
		Lon:         g.LonE7,
		Alt:         g.AltMM,
		RelativeAlt: g.AltMM - homeAlt,
		Vx:          toCMS(vel.Y),
		Vy:          toCMS(vel.X),
		Vz:          toCMS(-vel.Z),
		Hdg:         hdg,
	}
}

// onEvent turns a swarm event into a STATUSTEXT from the node it concerns.
func (b *Bridge) onEvent(e swarmEvent) {
	sysID, ok := b.vehicles.Lookup(e.node)
	if !ok {
		// An event about a node that has no vehicle yet (its first node_up can
		// arrive before the first sync). There is no sysid to attribute it to,
		// and MAVLink has no "from the system as a whole"; the next sync
		// creates the vehicle, so only this first line is lost.
		return
	}
	sev := uint8(SeverityInfo)
	if e.kind == EventNodeDown {
		// CRITICAL, not WARNING: a node going down is the event the whole
		// self-healing demo turns on, and a ground station only pops a toast
		// for CRITICAL and worse.
		sev = SeverityCritical
	}
	text := e.kind + ": " + string(e.node)
	if e.detail != "" {
		text += " (" + e.detail + ")"
	}
	m := &Statustext{Severity: sev}
	SetChars(m.Text[:], text) // truncates; see maxStatusText
	b.send(sysID, m)
}

// send encodes m as sysID and writes it to every known destination.
//
// One scratch buffer, reused, because this runs per vehicle per message type at
// 5 Hz and the frame is gone the moment the write returns.
func (b *Bridge) send(sysID byte, m Message) {
	dests := b.destinations()
	if len(dests) == 0 {
		return // nowhere to send yet; a ground station has not appeared
	}
	b.out = Encode(b.out[:0], b.seq.Next(sysID, MavCompIDAutopilot1), sysID, MavCompIDAutopilot1, m)
	for _, d := range dests {
		// A real instant: see BridgeConfig.Now.
		if err := b.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return
		}
		if _, err := b.conn.WriteToUDP(b.out, d); err != nil {
			b.onSendError(d, m, err)
			continue
		}
		b.sent.Add(1)
	}
}

// onSendError classifies a failed write. Transient failures (a full send
// buffer, an ICMP-unreachable reply from a ground station that is not running
// yet) are counted and left at debug: they happen once a second per vehicle
// while nobody is listening, and logging them at warn would bury the log in the
// ordinary case. Anything else is a real fault and is said out loud once per
// occurrence.
func (b *Bridge) onSendError(d *net.UDPAddr, m Message, err error) {
	b.sendErrs.Add(1)
	var nerr net.Error
	switch {
	case errors.Is(err, net.ErrClosed):
		return // shutting down
	case errors.As(err, &nerr) && nerr.Timeout(),
		errors.Is(err, os.ErrDeadlineExceeded):
		b.log.Debug("mavlink send timed out", "to", d, "msg", m.MsgName(), "err", err)
	default:
		b.log.Debug("mavlink send failed", "to", d, "msg", m.MsgName(), "err", err)
	}
}

// destinations is where to send: the configured ground station and the newest
// one that has spoken to us, deduplicated.
//
// Two slots rather than a set of every address ever seen. A ground station that
// reconnects comes from a new source port, and a set would keep shouting at the
// dead one for ever; a single learned slot follows the live link, and the
// configured address keeps working for a ground station that never sends
// anything.
func (b *Bridge) destinations() []*net.UDPAddr {
	switch {
	case b.configured == nil && b.learned == nil:
		return nil
	case b.configured == nil:
		return []*net.UDPAddr{b.learned}
	case b.learned == nil, sameAddr(b.configured, b.learned):
		return []*net.UDPAddr{b.configured}
	default:
		return []*net.UDPAddr{b.configured, b.learned}
	}
}

func sameAddr(a, b *net.UDPAddr) bool {
	return a.Port == b.Port && a.IP.Equal(b.IP)
}

// --- loop: inbound -------------------------------------------------------------

// onDatagram handles one received packet. Loop-owned.
func (b *Bridge) onDatagram(d datagram) {
	// Learn the sender before parsing. Section 7 of the contract: a ground
	// station that dials in works without configuration. Doing it before the
	// parse would mean learning from garbage, so it is done after the frame is
	// known good -- see below.
	f, err := DecodeDatagram(d.b)
	if err != nil {
		if IsMalformed(err) {
			// One auditable policy for every bad packet: count it, drop it,
			// carry on. Port 14550 is a well-known port and whatever else is
			// on the network will find it.
			b.malformed.Add(1)
			b.log.Debug("mavlink: dropped malformed datagram", "from", d.from, "bytes", len(d.b), "err", err)
			return
		}
		b.log.Warn("mavlink: decode failed", "from", d.from, "err", err)
		return
	}
	if d.from != nil && (b.learned == nil || !sameAddr(b.learned, d.from)) {
		b.learned = d.from
		b.log.Info("mavlink: learned ground station", "addr", d.from, "sysid", f.SysID)
	}

	switch msg := f.Msg.(type) {
	case *ParamRequestList:
		b.onParamRequestList(msg)
	case *ParamRequestRead:
		b.onParamRequestRead(msg)
	case *CommandLong:
		b.onCommandLong(f, msg)
	default:
		// Everything else, HEARTBEAT from the ground station included, needs no
		// answer. The mission protocol lands here: MISSION_* handling is a
		// separate piece of work, and this is where it hooks in.
		b.log.Debug("mavlink: unhandled message", "msg", f.ID, "from", d.from)
	}
}

// onCommandLong refuses every command, but refuses it explicitly.
//
// Silence is the one unacceptable answer: a ground station retries an
// unacknowledged COMMAND_LONG for ever, so an ignored arm request becomes a
// permanent retry storm and a user staring at a spinner. MAV_RESULT_UNSUPPORTED
// is the honest reply -- these vehicles are a simulation of a swarm, and
// nothing here can arm, take off or change mode.
func (b *Bridge) onCommandLong(f *Frame, m *CommandLong) {
	sysID, ok := b.targetSysID(m.TargetSystem)
	if !ok {
		return
	}
	b.send(sysID, &CommandAck{
		Command:         m.Command,
		Result:          MavResultUnsupported,
		TargetSystem:    f.SysID,
		TargetComponent: f.CompID,
	})
}

// targetSysID resolves a request's target_system to one of our vehicles.
// target_system 0 is the broadcast address; the lowest vehicle answers for it,
// so a broadcast command is acked once rather than N times.
func (b *Bridge) targetSysID(target byte) (byte, bool) {
	if target != 0 {
		if _, ok := b.vehicles.Node(target); ok {
			return target, true
		}
		return 0, false
	}
	for _, id := range b.order {
		if v := b.vs[id]; v != nil {
			return v.sysID, true
		}
	}
	return 0, false
}

// --- parameters ----------------------------------------------------------------

// param is one read-only parameter.
//
// # Why there are parameters at all
//
// A ground station will not show a vehicle as ready until it has downloaded the
// parameter set, and it asks for it again and again until it does. These
// vehicles have nothing to configure, so the list is the shortest one that ends
// that conversation, plus a few values that are genuinely useful to see: which
// node this vehicle is, whether it is a leader, and the scale factors the
// simulation is running with.
//
// Every parameter is read-only. PARAM_SET is not handled, so a write is simply
// never confirmed, and a ground station shows the old value -- which is the
// truth.
type param struct {
	name string
	typ  uint8
	// value is read on the loop goroutine, with the vehicle it belongs to.
	value func(b *Bridge, v *vehicle) float32
}

// bridgeParams is the parameter list. The order is the index order on the wire
// and must stay stable: a ground station caches parameters by index.
var bridgeParams = []param{
	// SYSID_THISMAV is the one parameter name a ground station actually looks
	// for; without it some versions keep the vehicle in "waiting for
	// parameters" for ever.
	{"SYSID_THISMAV", MavParamTypeInt32, func(_ *Bridge, v *vehicle) float32 { return float32(v.sysID) }},
	{"SWARM_LEADER", MavParamTypeInt32, func(_ *Bridge, v *vehicle) float32 {
		if v.leader {
			return 1
		}
		return 0
	}},
	{"SWARM_M_PER_UNIT", MavParamTypeReal32, func(b *Bridge, _ *vehicle) float32 {
		return float32(b.cfg.Projection.MetresPerUnit)
	}},
	{"SWARM_SPEED", MavParamTypeReal32, func(b *Bridge, _ *vehicle) float32 {
		return float32(b.cfg.Move.SpeedUnitsPerSec)
	}},
	{"SWARM_DRIFT_R", MavParamTypeReal32, func(b *Bridge, _ *vehicle) float32 {
		if b.cfg.Move.DriftDisabled {
			return 0
		}
		return float32(b.cfg.Move.DriftRadius)
	}},
}

// onParamRequestList answers with the whole list, one PARAM_VALUE per
// parameter. A broadcast request (target_system 0) is answered for every
// vehicle, because that is what a ground station scanning a link expects.
func (b *Bridge) onParamRequestList(m *ParamRequestList) {
	if m.TargetSystem == 0 {
		for _, id := range b.order {
			if v := b.vs[id]; v != nil {
				b.sendParams(v)
			}
		}
		return
	}
	if v := b.vehicleBySysID(m.TargetSystem); v != nil {
		b.sendParams(v)
	}
}

func (b *Bridge) sendParams(v *vehicle) {
	for i := range bridgeParams {
		b.send(v.sysID, b.paramValue(v, i))
	}
}

// onParamRequestRead answers one parameter, by index or by name.
//
// param_index -1 means "use param_id". An unknown name or an out-of-range index
// gets no reply, because PARAM_VALUE is the only answer the protocol has and
// inventing one for a parameter that does not exist is worse than the ground
// station's own timeout: it would cache a parameter this vehicle never had.
func (b *Bridge) onParamRequestRead(m *ParamRequestRead) {
	v := b.vehicleBySysID(m.TargetSystem)
	if v == nil {
		return
	}
	idx := -1
	if m.ParamIndex >= 0 {
		idx = int(m.ParamIndex)
	} else {
		name := Chars(m.ParamID[:])
		for i := range bridgeParams {
			if bridgeParams[i].name == name {
				idx = i
				break
			}
		}
	}
	if idx < 0 || idx >= len(bridgeParams) {
		b.log.Debug("mavlink: unknown parameter requested",
			"sysid", m.TargetSystem, "index", m.ParamIndex, "id", Chars(m.ParamID[:]))
		return
	}
	b.send(v.sysID, b.paramValue(v, idx))
}

// paramValue builds one PARAM_VALUE.
//
// The value travels in a float32 whatever param_type says. That is MAVLink's
// long-standing quirk: param_type describes how to interpret the number, not
// how it is encoded, and sending the bit pattern of an int32 instead (which
// PX4's "bytewise" convention does) makes every integer parameter read as a
// denormal in a ground station configured the other way. The numeric
// convention is what ArduPilot and QGroundControl's default both use.
func (b *Bridge) paramValue(v *vehicle, idx int) *ParamValue {
	p := bridgeParams[idx]
	m := &ParamValue{
		ParamValue: p.value(b, v),
		ParamCount: uint16(len(bridgeParams)),
		ParamIndex: uint16(idx),
		ParamType:  p.typ,
	}
	SetChars(m.ParamID[:], p.name)
	return m
}

func (b *Bridge) vehicleBySysID(sysID byte) *vehicle {
	node, ok := b.vehicles.Node(sysID)
	if !ok {
		return nil
	}
	return b.vs[node]
}

// --- small numeric helpers -----------------------------------------------------

// saturateU16 clamps rather than wraps. A dropped-frame counter that wraps at
// 65535 reads as a healthy link, which is the opposite of what it means.
func saturateU16(v uint64) uint16 {
	if v > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(v)
}

func saturateI16(f float64) int16 {
	switch {
	case math.IsNaN(f):
		return 0
	case f > math.MaxInt16:
		return math.MaxInt16
	case f < math.MinInt16:
		return math.MinInt16
	}
	return int16(f)
}
