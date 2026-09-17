package network

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/protocol"
)

// Dialer is the subset of *net.Dialer the pool needs. It is an interface so tests
// can hand the pool one end of a net.Pipe and drive the other end as the peer.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// PoolConfig configures a Pool. The zero value of every field except Self is
// usable; withDefaults fills the rest.
type PoolConfig struct {
	// Self is what this node announces in every HELLO and HELLO_ACK.
	Self Identity
	// Conn tunes every connection the pool creates.
	Conn ConnConfig
	// Dialer opens outbound sockets. Default: &net.Dialer{}.
	Dialer Dialer
	// HandshakeTimeout bounds one dial plus HELLO/HELLO_ACK. Default: 3s.
	HandshakeTimeout time.Duration
	// Backoff returns how long to wait before the next dial. attempt is the number
	// of consecutive failures so far: 0 means the previous connection succeeded and
	// then dropped. Default: 200ms << attempt, capped at 10s, +-25% jitter from Rand.
	Backoff func(attempt int) time.Duration
	// MaxRedials caps consecutive failed redials to one address; 0 means unlimited.
	// The pool gives up on the address after MaxRedials redials in a row fail and a
	// later Connect starts it over.
	MaxRedials int
	// Rand feeds jitter into the default Backoff. Default: math/rand/v2.
	Rand func() float64
	// Now is the clock for deadlines, injectable for tests. Default: time.Now.
	Now func() time.Time
	// EventBuffer sizes the Events channel. Default: 64.
	EventBuffer int
	// Handler receives every inbound frame from every peer, on that peer's reader
	// goroutine. It must not block (see Handler).
	Handler Handler
}

func (c PoolConfig) withDefaults() PoolConfig {
	if c.Dialer == nil {
		c.Dialer = &net.Dialer{}
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 3 * time.Second
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	if c.Backoff == nil {
		c.Backoff = defaultBackoff(c.Rand)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Conn.Now == nil {
		c.Conn.Now = c.Now
	}
	if c.EventBuffer <= 0 {
		c.EventBuffer = 64
	}
	return c
}

// peerEntry is one registered connection. Entries are compared by pointer: a
// connection's owner checks whether the entry in the map is still *its* entry
// before emitting PeerDown, which is how a superseded connection stays silent.
type peerEntry struct {
	info PeerInfo
	conn *Conn
	// gone is closed once the entry's fate is settled: replaced, or removed with
	// its PeerDown emitted, or parked and then resolved either way. A redial loop
	// waits on this rather than on conn.Done so that it stays parked through the
	// window in which a PeerDown is deferred -- redialing then would only lose
	// the tie-break to the very handshake the deferral is waiting on.
	gone     chan struct{}
	goneOnce sync.Once
}

// retire closes gone exactly once. Called only after the entry is out of the map.
func (e *peerEntry) retire() { e.goneOnce.Do(func() { close(e.gone) }) }

// dialEntry is one managed outbound address.
type dialEntry struct {
	cancel context.CancelFunc
}

// Pool owns every connection to every peer and implements Transport.
//
// # Goroutine ownership
//
//   - One redial loop per Connect'ed address (dialLoop). Exits when its context is
//     cancelled by Forget or Close, when the address turns out to be ourselves, or
//     when MaxRedials is exhausted.
//   - One admit goroutine per Admit'ed socket (admitLoop). Runs the acceptor
//     handshake, registers the Conn, then waits for it to end. Exits when the
//     connection ends, which Close forces by closing every socket.
//   - Two goroutines per Conn, owned by the Conn itself (see Conn).
//
// All of the pool's own goroutines are counted in wg, so Close's return is the
// proof that none of them leaked.
//
// # Synchronisation
//
// mu guards every mutable field below it: peers, dials, pending, claims, dialing,
// deferred, downInFlight, addrOf, known, closed. Nothing blocking is ever done under mu -- handshakes, event
// emission and Conn.Close all happen outside it -- so a stalled peer or a slow
// Events consumer cannot make Send block on the lock.
//
// # Claims: why a PeerDown can be deferred
//
// The tie-break replaces a connection with a better one to the same peer, and the
// two sides register the winner independently. Whichever side registers first
// closes the loser, and the loser's FIN can reach the other side before that side
// has finished its own handshake for the winner -- reliably, in fact, because
// decoding a HELLO_ACK takes longer than a loopback FIN. Without care that side
// would emit PeerDown for a peer it is about to register again, and every
// simultaneous dial would look like a flap to membership.
//
// So a handshake that has passed the tie-break policy (claims, by peer ID) or a
// dial that is in flight (dialing, by address) is recorded before it can possibly
// cause the loser to be closed. When a connection ends while a claim exists for
// its peer, its entry leaves the map as usual but its PeerDown is parked in
// deferred instead of being emitted. The claim's resolution decides: a successful
// registration discards the parked event, because the peer never went away; a
// failure releases the claim, and the parked PeerDown is emitted after all. The
// event is therefore delayed by at most one handshake, never lost, and never
// spurious.
type Pool struct {
	cfg    PoolConfig
	events chan PeerEvent
	// ctx is the parent of every dial loop's context; cancel is called by Close.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// changed is broadcast, with mu held, after every mutation of peers, pending,
	// claims, dialing, deferred or downInFlight. Nothing in the pool waits on it.
	// It exists because the tie-break's silent transitions are, by design,
	// invisible on Events, and a test that needs to observe "the winner is
	// registered on both sides" would otherwise have to poll. A Cond is the
	// honest primitive for that -- but only if *every* mutation a waiter's
	// predicate reads is followed by a Broadcast. One that is not is a lost
	// wakeup: the waiter sleeps on a condition that is already true, for ever.
	changed *sync.Cond
	// peers maps a connected peer's ID to its registered connection.
	peers map[protocol.NodeID]*peerEntry
	// dials holds the addresses we are responsible for keeping connected.
	dials map[protocol.NodeAddress]*dialEntry
	// pending holds sockets in mid-handshake, so Close can cut them short rather
	// than waiting out HandshakeTimeout.
	pending map[net.Conn]struct{}
	// claims counts inbound handshakes that passed admitPolicy and are about to
	// register a connection to that ID. dialing counts dials in flight to an
	// address. Both defer PeerDown for a matching entry; see the type comment.
	claims  map[protocol.NodeID]int
	dialing map[protocol.NodeAddress]int
	// deferred holds, per peer, the entry whose PeerDown is waiting on a claim.
	deferred map[protocol.NodeID]*peerEntry
	// downInFlight holds, per peer, an entry whose PeerDown has been decided but
	// not yet delivered (emission happens outside mu). register consults it so a
	// PeerUp for the same peer is never delivered ahead of that PeerDown.
	downInFlight map[protocol.NodeID]*peerEntry
	// addrOf records which dialled addresses have turned out to reach which peer,
	// so a dial claim on an address also covers the peer it is known to reach.
	// Needed because a node is dialled by whatever name the seed list or gossip
	// used, which need not be the string it advertises.
	addrOf map[protocol.NodeID]map[protocol.NodeAddress]struct{}
	// known accumulates every address we have learned from handshakes and Connect.
	known map[protocol.NodeAddress]struct{}
	// closed is set once by Close. Every entry point checks it under mu before
	// calling wg.Add, which is what makes Close's wg.Wait race-free.
	closed bool
}

// Compile-time proof that *Pool satisfies the contract pkg/cluster codes against.
var _ Transport = (*Pool)(nil)

// NewPool creates a pool. It opens nothing until Connect, Admit or Listen.
func NewPool(cfg PoolConfig) *Pool {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		cfg:      cfg,
		events:   make(chan PeerEvent, cfg.EventBuffer),
		ctx:      ctx,
		cancel:   cancel,
		peers:    make(map[protocol.NodeID]*peerEntry),
		dials:    make(map[protocol.NodeAddress]*dialEntry),
		pending:  make(map[net.Conn]struct{}),
		claims:   make(map[protocol.NodeID]int),
		dialing:  make(map[protocol.NodeAddress]int),
		deferred: make(map[protocol.NodeID]*peerEntry),

		downInFlight: make(map[protocol.NodeID]*peerEntry),
		addrOf:       make(map[protocol.NodeID]map[protocol.NodeAddress]struct{}),
		known:        make(map[protocol.NodeAddress]struct{}),
	}
	p.changed = sync.NewCond(&p.mu)
	return p
}

// Self returns this node's ID.
func (p *Pool) Self() protocol.NodeID { return p.cfg.Self.ID }

// Events returns the PeerUp/PeerDown stream.
//
// Consumers must drain it until it is closed. Emission blocks when the buffer is
// full, so a consumer that stops reading eventually stalls every connection
// watcher -- deliberately: silently dropping a PeerDown would leave a dead peer
// looking alive to membership for ever, which is worse than backpressure. The one
// exception is during Close, when a full buffer is skipped rather than deadlocking
// the closer; a closed Events channel already means "everything is down".
func (p *Pool) Events() <-chan PeerEvent { return p.events }

// Connect starts (or reuses) a managed connection to addr. It returns once the
// dial loop has been scheduled, and is idempotent per address.
func (p *Pool) Connect(addr protocol.NodeAddress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.dials[addr] != nil {
		return
	}
	p.learnAddr(addr)
	ctx, cancel := context.WithCancel(p.ctx)
	entry := &dialEntry{cancel: cancel}
	p.dials[addr] = entry
	p.wg.Add(1)
	go p.dialLoop(ctx, addr, entry)
}

// Forget stops redialing addr and closes any connection the dial loop holds to it.
func (p *Pool) Forget(addr protocol.NodeAddress) {
	p.mu.Lock()
	entry := p.dials[addr]
	delete(p.dials, addr)
	p.mu.Unlock()
	if entry != nil {
		entry.cancel()
	}
}

// Admit takes ownership of an accepted socket that has not yet handshaken. The
// handshake runs on its own goroutine so the accept loop never blocks on a peer
// that connects and then says nothing.
func (p *Pool) Admit(raw net.Conn) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = raw.Close()
		return
	}
	p.pending[raw] = struct{}{}
	p.changed.Broadcast()
	p.wg.Add(1)
	p.mu.Unlock()
	go p.admitLoop(raw)
}

// Known returns every advertised address the pool has learned, sorted, excluding
// our own. It seeds HELLO.KnownPeers so a joining node learns the swarm from
// whichever peer it reached first.
func (p *Pool) Known() []protocol.NodeAddress {
	p.mu.Lock()
	out := make([]protocol.NodeAddress, 0, len(p.known))
	for a := range p.known {
		out = append(out, a)
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Peers lists connected peers sorted by ID.
func (p *Pool) Peers() []PeerInfo {
	p.mu.Lock()
	out := make([]PeerInfo, 0, len(p.peers))
	for _, e := range p.peers {
		out = append(out, copyInfo(e.info))
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Send queues env to one connected peer.
func (p *Pool) Send(ctx context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	if env == nil {
		return fmt.Errorf("network: node %q: Send(nil) to %q", p.cfg.Self.ID, to)
	}
	p.mu.Lock()
	e := p.peers[to]
	p.mu.Unlock()
	if e == nil {
		return fmt.Errorf("network: node %q: send %s to %q: %w", p.cfg.Self.ID, env.Type, to, ErrUnknownPeer)
	}
	return e.conn.Send(ctx, env)
}

// Broadcast queues env to every connected peer and reports how many accepted it.
//
// The sends fan out, one goroutine per peer, joined before returning. A
// control-plane Send can wait up to CtrlSendTimeout for queue space, so a
// sequential loop would cost the caller N * CtrlSendTimeout when every peer is
// slow -- and the caller is the cluster's election loop, which must not be
// stalled by the peers it is trying to tell about a failover. Fanned out, the
// worst case is a single CtrlSendTimeout regardless of N. The goroutines are
// short-lived and cannot leak: each ends when its Send returns, and Send is
// bounded by ctx, by CtrlSendTimeout, and by the connection closing.
func (p *Pool) Broadcast(ctx context.Context, env *protocol.Envelope) int {
	if env == nil {
		return 0
	}
	// Snapshot under the lock, send outside it: holding mu across the sends
	// would freeze Admit and every other Send for the duration.
	p.mu.Lock()
	conns := make([]*Conn, 0, len(p.peers))
	for _, e := range p.peers {
		conns = append(conns, e.conn)
	}
	p.mu.Unlock()

	var (
		wg sync.WaitGroup
		n  atomic.Int64
	)
	for _, c := range conns {
		wg.Add(1)
		go func(c *Conn) {
			defer wg.Done()
			if c.Send(ctx, env) == nil {
				n.Add(1)
			}
		}(c)
	}
	wg.Wait()
	return int(n.Load())
}

// Close shuts everything down: every connection, every dial loop, every pending
// handshake. It joins all pool goroutines and then closes Events. Idempotent.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	conns := make([]*Conn, 0, len(p.peers))
	for _, e := range p.peers {
		conns = append(conns, e.conn)
	}
	pending := make([]net.Conn, 0, len(p.pending))
	for raw := range p.pending {
		pending = append(pending, raw)
	}
	p.mu.Unlock()

	// Cancel first so dial loops parked in a backoff timer or DialContext wake,
	// then close sockets so loops parked in a handshake or on Conn.Done wake.
	p.cancel()
	for _, raw := range pending {
		_ = raw.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	p.wg.Wait()
	close(p.events)
	return nil
}

// --- outbound ------------------------------------------------------------------

// isConnected reports whether a connection to id is registered or its PeerDown is
// parked -- in both cases the peer is "up" as far as the consumer knows.
func (p *Pool) isConnected(id protocol.NodeID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return id != "" && (p.peers[id] != nil || p.deferred[id] != nil)
}

// trackPending records a socket in mid-handshake so Close can cut the handshake
// short. It reports false if the pool is already closed, in which case the caller
// must close the socket itself.
func (p *Pool) trackPending(raw net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.pending[raw] = struct{}{}
	p.changed.Broadcast()
	return true
}

// untrackPending must broadcast. On the paths that go on to release a claim or a
// dial, a later Broadcast would cover it anyway; but an inbound handshake that
// fails or is rejected before admitPolicy takes a claim -- the loser of a
// simultaneous dial, for one -- touches nothing else, so this is the only signal
// that pending has emptied.
func (p *Pool) untrackPending(raw net.Conn) {
	p.mu.Lock()
	delete(p.pending, raw)
	p.changed.Broadcast()
	p.mu.Unlock()
}

// learnAddr must be called with mu held.
func (p *Pool) learnAddr(a protocol.NodeAddress) {
	if a == "" || a == p.cfg.Self.Advertise {
		return
	}
	p.known[a] = struct{}{}
}

// copyInfo detaches the KnownPeers slice so a caller cannot mutate pool state
// through a returned PeerInfo, and vice versa.
func copyInfo(in PeerInfo) PeerInfo {
	out := in
	if in.KnownPeers != nil {
		out.KnownPeers = append([]protocol.NodeAddress(nil), in.KnownPeers...)
	}
	return out
}
