package network

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/protocol"
)

// ErrProberClosed is returned by Probe when the prober was closed while the probe
// was pending, or is called after Close.
var ErrProberClosed = errors.New("network: prober closed")

// MeshProber measures application-level round trip over the established mesh.
//
// It sends a PING with a fresh nonce and Envelope.ID through the Transport and
// resolves when the matching PONG arrives via HandleFrame. It replaces
// health.TCPConnectProber, which measured the kernel's three-way handshake: that
// completes from the listen backlog with zero application involvement, so a node
// in a GC pause or with a starved scheduler still answered a SYN in microseconds
// and would have been elected leader while comatose. A PONG has to pass through
// the peer's reader goroutine, its scheduler and its writer queue -- the things a
// leader actually needs to be fast at.
//
// Probe has the health.Prober signature, so a MeshProber is wired in with
// health.Config{Probe: prober.Probe}.
//
// # Goroutine ownership
//
// One goroutine per PING answered (reply), started by HandleFrame and joined by
// Close. It exists because HandleFrame runs on a Conn's reader goroutine, which
// must never block, and a control-plane Send can wait up to CtrlSendTimeout for
// queue space; the CHAOS "delay" hook also waits here. Each exits on its timer,
// on done, or when the Send returns.
//
// # Synchronisation
//
// mu guards pending and closed. delay is atomic because SetDelay is called from
// the chaos handler while reply goroutines read it. peerDelay is an atomic
// pointer for the same reason: SetPeerDelay may run at any time, and every reply
// reads it without a lock. seq is atomic too; it only needs to be unique per
// link, not ordered.
type MeshProber struct {
	t       Transport
	resolve func(protocol.NodeAddress) (protocol.NodeID, bool)
	now     func() time.Time
	// timer is the seam for the delay hook: it returns a channel that fires after
	// d and a stop function. Tests inject one they control.
	timer func(d time.Duration) (<-chan time.Time, func() bool)

	delay     atomic.Int64
	peerDelay atomic.Pointer[PeerDelayFunc]
	seq       atomic.Uint64

	// ctx is cancelled by Close; it bounds every delayed reply and every Send a
	// reply makes, and is what Probe selects on to fail fast on Close.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	pending map[string]*pendingProbe
	closed  bool
}

// pendingProbe is one outstanding PING. result is buffered so the resolver never
// blocks on a prober that has already given up and gone.
type pendingProbe struct {
	nonce  uint64
	start  time.Time
	result chan probeResult
}

type probeResult struct {
	rtt time.Duration
	err error
}

// NewMeshProber builds a prober over t. resolve maps a probe target (an advertised
// address, which is what HealthStrategy speaks) to the NodeID the transport
// speaks; the cluster's membership table is the usual source. now is the clock
// used for RTT and must be monotonic (time.Now is); nil means time.Now.
func NewMeshProber(t Transport, resolve func(protocol.NodeAddress) (protocol.NodeID, bool), now func() time.Time) *MeshProber {
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &MeshProber{
		t:       t,
		resolve: resolve,
		now:     now,
		timer: func(d time.Duration) (<-chan time.Time, func() bool) {
			tm := time.NewTimer(d)
			return tm.C, tm.Stop
		},
		ctx:     ctx,
		cancel:  cancel,
		pending: make(map[string]*pendingProbe),
	}
}

// SetDelay sets an artificial delay applied before every PING is answered. It is
// the CHAOS "delay" hook; zero clears it.
func (m *MeshProber) SetDelay(d time.Duration) { m.delay.Store(int64(d)) }

// PeerDelayFunc returns the emulated one-way delay to add before answering a
// PING from peer. It is called on reply goroutines, concurrently, so it must be
// safe for concurrent use. A result <= 0 means no delay.
type PeerDelayFunc func(peer protocol.NodeID) time.Duration

// SetPeerDelay installs the per-peer delay hook used by the latency simulation;
// nil removes it. The hook is evaluated once per PONG, at reply time, so a
// jittered model gets a fresh draw for every answer rather than one value
// frozen at install time. Its result is added to the SetDelay (CHAOS) delay.
func (m *MeshProber) SetPeerDelay(f PeerDelayFunc) {
	if f == nil {
		m.peerDelay.Store(nil)
		return
	}
	m.peerDelay.Store(&f)
}

// replyDelay is the total wait before a PONG to peer: the CHAOS delay plus the
// emulated per-peer delay. Negative parts count as zero, so a buggy hook can
// only fail to add delay, never shorten a CHAOS delay.
func (m *MeshProber) replyDelay(peer protocol.NodeID) time.Duration {
	d := max(time.Duration(m.delay.Load()), 0)
	if f := m.peerDelay.Load(); f != nil {
		d += max((*f)(peer), 0)
	}
	return d
}

// Probe sends one PING to target and returns the round trip to its PONG. It
// satisfies health.Prober: it honours ctx, returns promptly when ctx ends, and
// never retries -- a retry would hide exactly the failure the EWMA is meant to
// see.
func (m *MeshProber) Probe(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
	id, ok := m.resolve(target)
	if !ok {
		return 0, fmt.Errorf("network: node %q: probe %s: no node id for address: %w", m.t.Self(), target, ErrUnknownPeer)
	}

	// The nonce matches a PONG to its PING; it is not a secret. The peer that
	// could guess it is the one answering the PING anyway, and it controls the
	// latency we measure whether or not it can predict the nonce.
	nonce := rand.Uint64() // #nosec G404 -- correlation value, not a credential
	env, err := protocol.NewEnvelope(protocol.TypePing, m.t.Self(), id, protocol.PingPayload{
		Nonce: nonce,
		Seq:   m.seq.Add(1),
	})
	if err != nil {
		return 0, fmt.Errorf("network: node %q: probe %s: %w", m.t.Self(), target, err)
	}

	// Register before sending. The PONG can arrive on the reader goroutine before
	// Send has even returned on a loopback link; a probe registered afterwards
	// would miss its own answer and time out.
	p := &pendingProbe{nonce: nonce, result: make(chan probeResult, 1)}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, ErrProberClosed
	}
	m.pending[env.ID] = p
	m.mu.Unlock()

	// The clock is read after registration and immediately before the send, so
	// the measurement covers the send path but not the bookkeeping above.
	p.start = m.now()
	if err := m.t.Send(ctx, id, env); err != nil {
		m.forget(env.ID)
		return 0, fmt.Errorf("network: node %q: probe %s (%s): %w", m.t.Self(), target, id, err)
	}

	select {
	case r := <-p.result:
		return r.rtt, r.err
	case <-ctx.Done():
		m.forget(env.ID)
		return 0, ctx.Err()
	case <-m.ctx.Done():
		m.forget(env.ID)
		return 0, ErrProberClosed
	}
}

// HandleFrame answers PINGs and resolves pending probes on PONGs. It must be
// called for every inbound frame; it reports whether it consumed the frame so a
// dispatcher can pass everything else on. It never blocks: the reply is sent on
// its own goroutine.
func (m *MeshProber) HandleFrame(peer protocol.NodeID, env *protocol.Envelope) bool {
	switch env.Type {
	case protocol.TypePing:
		m.answer(peer, env)
		return true
	case protocol.TypePong:
		m.resolvePong(env)
		return true
	default:
		return false
	}
}

func (m *MeshProber) answer(peer protocol.NodeID, ping *protocol.Envelope) {
	req, err := protocol.PayloadOf[protocol.PingPayload](ping)
	if err != nil {
		return // a malformed PING is the peer's problem; there is nothing to echo
	}
	// Field by field on purpose: a conversion would tie PONG's layout to
	// PING's, and a field later added to PING must not be echoed silently.
	//lint:ignore S1016 see above
	pong, err := protocol.NewReply(ping, protocol.TypePong, m.t.Self(), protocol.PongPayload{Nonce: req.Nonce, Seq: req.Seq})
	if err != nil {
		return
	}

	// wg.Add under mu with the closed check is what makes Close's wg.Wait
	// race-free: a reply cannot be started after Close has begun joining.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()

	go m.reply(peer, pong)
}

// reply sends one PONG after the configured delay, if any (see replyDelay). The
// delay is computed here rather than in answer so the hook runs off the reader
// goroutine and each PONG gets its own jitter draw. The wait is a timer
// selected against the prober's context -- never a Sleep -- so Close cancels a
// pending reply instead of waiting it out, and the Send itself is bounded by the
// same context so Close is not held up by a full queue either.
func (m *MeshProber) reply(peer protocol.NodeID, pong *protocol.Envelope) {
	defer m.wg.Done()
	if d := m.replyDelay(peer); d > 0 {
		fire, stop := m.timer(d)
		select {
		case <-fire:
		case <-m.ctx.Done():
			stop()
			return
		}
	}
	_ = m.t.Send(m.ctx, peer, pong)
}

func (m *MeshProber) resolvePong(pong *protocol.Envelope) {
	payload, err := protocol.PayloadOf[protocol.PongPayload](pong)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pending[pong.ID]
	if p == nil {
		return // late answer to a probe that already gave up; not evidence
	}
	if p.nonce != payload.Nonce {
		// Same ID, wrong nonce: a replayed or mis-routed PONG. Ignoring it keeps
		// the probe pending so a stale answer cannot be credited as a fast one.
		return
	}
	delete(m.pending, pong.ID)
	p.result <- probeResult{rtt: m.now().Sub(p.start)}
}

// forget drops a pending probe that its caller has abandoned.
func (m *MeshProber) forget(id string) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
}

// Close fails every pending probe with ErrProberClosed, cancels delayed replies
// and joins their goroutines. Idempotent.
func (m *MeshProber) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	for id, p := range m.pending {
		delete(m.pending, id)
		p.result <- probeResult{err: ErrProberClosed}
	}
	m.mu.Unlock()
	m.wg.Wait()
}
