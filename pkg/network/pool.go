package network

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sort"
	"sync"
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

// defaultBackoff is exponential with full-range jitter of +-25%.
//
// Jitter is not optional. When a leader is SIGKILLed, every worker notices within
// the same idle timeout and every worker redials at the same moment; without
// jitter they all retry in lockstep on every subsequent attempt too, and the
// restarted container is hit by a synchronised wave each time it comes back. The
// cap keeps the wait bounded so a peer that returns after a long outage is
// noticed within ten seconds rather than minutes.
func defaultBackoff(random func() float64) func(int) time.Duration {
	const (
		base = 200 * time.Millisecond
		cap_ = 10 * time.Second
	)
	return func(attempt int) time.Duration {
		if attempt < 0 {
			attempt = 0
		}
		// Shifting past 2^6 already exceeds the cap; clamping the exponent keeps
		// the shift well clear of overflowing a Duration for large attempt counts.
		if attempt > 6 {
			attempt = 6
		}
		d := base << attempt
		if d > cap_ {
			d = cap_
		}
		factor := 1 + (random()*2-1)*0.25
		return time.Duration(float64(d) * factor)
	}
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
// deferred, known, closed. Nothing blocking is ever done under mu -- handshakes, event
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
	// changed is broadcast, with mu held, after every mutation of peers, claims,
	// dialing or deferred. Nothing in the pool waits on it. It exists because the
	// tie-break's silent transitions are, by design, invisible on Events, and a
	// test that needs to observe "the winner is registered on both sides" would
	// otherwise have to poll. A Cond is the honest primitive for that.
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
		known:    make(map[protocol.NodeAddress]struct{}),
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
func (p *Pool) Broadcast(ctx context.Context, env *protocol.Envelope) int {
	if env == nil {
		return 0
	}
	// Snapshot under the lock, send outside it: Conn.Send can block for up to
	// CtrlSendTimeout per peer, and holding mu for that long would freeze Admit
	// and every other Send.
	p.mu.Lock()
	conns := make([]*Conn, 0, len(p.peers))
	for _, e := range p.peers {
		conns = append(conns, e.conn)
	}
	p.mu.Unlock()

	n := 0
	for _, c := range conns {
		if c.Send(ctx, env) == nil {
			n++
		}
	}
	return n
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

// dialLoop keeps one address connected until it is told to stop.
//
// The loop has two resting states: waiting on a live connection to the peer at
// this address (whether or not we initiated it), and waiting out a backoff. It
// never sleeps: every wait is a select against the loop's context.
func (p *Pool) dialLoop(ctx context.Context, addr protocol.NodeAddress, self *dialEntry) {
	defer p.wg.Done()
	defer p.releaseDial(addr, self)

	attempt := 0
	for {
		info, entry, err := p.dialOnce(ctx, addr)
		switch {
		case err == nil:
			attempt = 0
			if entry != nil {
				p.watch(ctx, entry)
			}
			// Whether we kept our own connection or lost the tie-break to an
			// inbound one, stay put while *any* connection to this peer is
			// live. Redialing a peer we are already connected to would just
			// lose the tie-break again, on every backoff, for ever.
			p.waitWhileConnected(ctx, info.ID)
		case errors.Is(err, ErrSelfConnect):
			// Terminal: the address is us. No backoff will change that.
			return
		case errors.Is(err, ErrHandshakeRejected) && p.isConnected(info.ID):
			// "Rejected because you are already connected to me" is the
			// tie-break outcome, not a failure. Same resting state as success.
			attempt = 0
			p.waitWhileConnected(ctx, info.ID)
		default:
			attempt++
			if p.cfg.MaxRedials > 0 && attempt > p.cfg.MaxRedials {
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		if !p.wait(ctx, p.cfg.Backoff(attempt)) {
			return
		}
	}
}

// dialOnce performs one dial and handshake. On success it registers the
// connection and returns its entry, or nil if the tie-break discarded it. On a
// rejection the PeerInfo is still populated with what the peer disclosed.
func (p *Pool) dialOnce(ctx context.Context, addr protocol.NodeAddress) (PeerInfo, *peerEntry, error) {
	// Claim the address for the whole attempt, before the first packet, so a
	// loser closed by the peer cannot be mistaken for the peer going away.
	p.mu.Lock()
	p.dialing[addr]++
	p.changed.Broadcast()
	p.mu.Unlock()
	defer p.releaseDialing(addr)

	// The dial itself is bounded by the same budget as the handshake. The address
	// is resolved by name here, on every attempt, never cached: a restarted
	// container gets a new IP and the old one may now belong to someone else.
	dctx, cancel := context.WithTimeout(ctx, p.cfg.HandshakeTimeout)
	raw, err := p.cfg.Dialer.DialContext(dctx, "tcp", string(addr))
	cancel()
	if err != nil {
		return PeerInfo{}, nil, fmt.Errorf("network: node %q: dial %s: %w", p.cfg.Self.ID, addr, err)
	}
	if !p.trackPending(raw) {
		_ = raw.Close()
		return PeerInfo{}, nil, fmt.Errorf("network: node %q: dial %s: %w", p.cfg.Self.ID, addr, context.Canceled)
	}
	info, err := dialHandshake(raw, p.cfg.Self, p.Known(), p.cfg.Now().Add(p.cfg.HandshakeTimeout))
	p.untrackPending(raw)
	if err != nil {
		_ = raw.Close()
		return info, nil, err
	}
	return info, p.register(info, raw, true), nil
}

// waitWhileConnected blocks while any registered connection to id is live, or
// while a PeerDown for it is parked (the peer is still "up" until that resolves,
// and a redial now would only fight the handshake being waited on). It returns
// when neither holds or ctx is cancelled; on cancellation it closes whatever
// connection it was watching, which is what gives Forget its "closes any
// connection to it" semantics.
func (p *Pool) waitWhileConnected(ctx context.Context, id protocol.NodeID) {
	for {
		p.mu.Lock()
		e := p.peers[id]
		if e == nil {
			e = p.deferred[id]
		}
		p.mu.Unlock()
		if e == nil {
			return
		}
		select {
		case <-e.gone:
		case <-ctx.Done():
			_ = e.conn.Close()
			return
		}
	}
}

// wait blocks for d or until ctx is cancelled, reporting whether it ran to
// completion. A timer, not a Sleep, so Close never waits out a backoff.
func (p *Pool) wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseDial removes the loop's entry so a later Connect can start a new one.
// The identity check matters: Forget may already have replaced or removed it.
func (p *Pool) releaseDial(addr protocol.NodeAddress, self *dialEntry) {
	p.mu.Lock()
	if p.dials[addr] == self {
		delete(p.dials, addr)
	}
	p.mu.Unlock()
}

// --- inbound -------------------------------------------------------------------------

func (p *Pool) admitLoop(raw net.Conn) {
	defer p.wg.Done()
	// claimed records which ID admitPolicy took a claim for, so it can be
	// released on every exit path after that point.
	var claimed protocol.NodeID
	policy := func(info PeerInfo) (bool, string) {
		ok, reason := p.admitPolicy(info)
		if ok {
			claimed = info.ID
		}
		return ok, reason
	}
	info, err := acceptHandshake(raw, p.cfg.Self, p.Known(), p.cfg.Now().Add(p.cfg.HandshakeTimeout), policy)
	p.untrackPending(raw)
	if err != nil {
		_ = raw.Close()
		if claimed != "" {
			p.releaseClaim(claimed)
		}
		return
	}
	e := p.register(info, raw, false)
	p.releaseClaim(info.ID)
	if e != nil {
		p.watch(p.ctx, e)
	}
}

// admitPolicy is the accept callback for inbound handshakes. It applies the
// tie-break early so a peer that is going to lose learns why in the HELLO_ACK
// instead of being accepted and then cut off, and it takes a claim on the ID for
// every HELLO it approves -- before the ACK is written, which is before the peer
// can register the winner and close whatever it replaces. It is advisory:
// register re-checks under the lock, because the world can change in between.
func (p *Pool) admitPolicy(info PeerInfo) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing := p.peers[info.ID]
	if existing != nil && !p.prefer(info, existing.info, false) {
		return false, fmt.Sprintf("already connected to %q; the connection initiated by the lower node id (%q) wins", info.ID, p.cfg.Self.ID)
	}
	p.claims[info.ID]++
	p.changed.Broadcast()
	return true, ""
}

// releaseClaim drops one inbound claim on id and, if that leaves a deferred
// PeerDown for id unclaimed, emits it.
func (p *Pool) releaseClaim(id protocol.NodeID) {
	p.mu.Lock()
	if p.claims[id]--; p.claims[id] <= 0 {
		delete(p.claims, id)
	}
	e := p.deferred[id]
	if e != nil && !p.claimed(e) {
		delete(p.deferred, id)
	} else {
		e = nil
	}
	p.changed.Broadcast()
	p.mu.Unlock()
	if e != nil {
		p.emitDown(e)
	}
}

// releaseDialing drops one dial claim on addr and resolves any deferred PeerDown
// whose advertised address matches, as releaseClaim does by ID.
func (p *Pool) releaseDialing(addr protocol.NodeAddress) {
	p.mu.Lock()
	if p.dialing[addr]--; p.dialing[addr] <= 0 {
		delete(p.dialing, addr)
	}
	var resolved []*peerEntry
	for id, e := range p.deferred {
		if e.info.Advertise == addr && !p.claimed(e) {
			delete(p.deferred, id)
			resolved = append(resolved, e)
		}
	}
	p.changed.Broadcast()
	p.mu.Unlock()
	for _, e := range resolved {
		p.emitDown(e)
	}
}

// claimed reports whether a handshake that may replace e is in flight. mu held.
func (p *Pool) claimed(e *peerEntry) bool {
	return p.claims[e.info.ID] > 0 || p.dialing[e.info.Advertise] > 0
}

// emitDown emits PeerDown for an entry that has left the map, then retires it.
// Event first, retire second: a redial loop released by retire must not get its
// next PeerUp ahead of this PeerDown.
func (p *Pool) emitDown(e *peerEntry) {
	d, err := e.conn.Disposition()
	p.emit(PeerEvent{Kind: PeerDown, Peer: copyInfo(e.info), Disposition: d, Err: err})
	e.retire()
}

// --- registration and the tie-break ----------------------------------------------------

// prefer reports whether a new connection to a peer should replace an existing one.
//
// A newer incarnation always wins: the process behind the old connection is
// gone, we just have not timed it out yet, and making a restarted peer wait out
// our idle timeout would add seconds to every recovery. At equal incarnation the
// simultaneous-dial tie-break applies: keep the connection initiated by the lower
// NodeID. Both sides compute this from the same two IDs, so both keep the same
// connection and neither closes both.
func (p *Pool) prefer(candidate, existing PeerInfo, weInitiated bool) bool {
	if candidate.Incarnation != existing.Incarnation {
		return candidate.Incarnation > existing.Incarnation
	}
	if weInitiated {
		return p.cfg.Self.ID < candidate.ID
	}
	return candidate.ID < p.cfg.Self.ID
}

// register wraps raw in a Conn and installs it as the connection to info.ID,
// applying the tie-break against any existing connection. It returns the new
// entry, or nil if raw lost and was closed.
//
// Events: a fresh slot emits PeerUp. Replacing a slot emits nothing -- the peer
// was up and still is -- and the superseded connection's owner will find its
// entry gone from the map and stay silent too.
func (p *Pool) register(info PeerInfo, raw net.Conn, weInitiated bool) *peerEntry {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = raw.Close()
		return nil
	}
	existing := p.peers[info.ID]
	if existing != nil && !p.prefer(info, existing.info, weInitiated) {
		p.mu.Unlock()
		_ = raw.Close()
		return nil
	}
	// A PeerDown parked for this peer is moot: it is reachable again, and from
	// the consumer's point of view it never left -- so no PeerUp either.
	parked := p.deferred[info.ID]
	if parked != nil {
		delete(p.deferred, info.ID)
		parked.retire()
	}
	e := &peerEntry{
		info: copyInfo(info),
		conn: NewConn(info.ID, raw, p.cfg.Handler, p.cfg.Conn),
		gone: make(chan struct{}),
	}
	p.peers[info.ID] = e
	p.learnAddr(info.Advertise)
	for _, a := range info.KnownPeers {
		p.learnAddr(a)
	}
	p.changed.Broadcast()
	p.mu.Unlock()

	if existing != nil {
		// Close the loser, then move whatever it had not yet written onto the
		// winner. Without this, a frame queued in the instant between the two
		// handshakes completing would vanish with no PeerDown to explain it.
		// What the loser had already handed to the socket may still be lost if
		// the peer closed first; that is the same exposure as any close and is
		// why control traffic is periodic rather than one-shot.
		_ = existing.conn.Close()
		for _, env := range existing.conn.drainUnsent() {
			_ = e.conn.Send(context.Background(), env)
		}
		existing.retire()
		return e
	}
	if parked == nil {
		p.emit(PeerEvent{Kind: PeerUp, Peer: copyInfo(info)})
	}
	return e
}

// watch waits for e's connection to end and then decides what that means. It is
// called by exactly one goroutine per entry: the one that registered it.
//
// Three outcomes: the entry was already replaced (silent); the entry is still
// current and nothing is about to replace it (PeerDown now); or the entry is
// still current but a claim is in flight (park the PeerDown and let the claim
// decide, which is also when the entry is retired).
func (p *Pool) watch(ctx context.Context, e *peerEntry) {
	select {
	case <-e.conn.Done():
	case <-ctx.Done():
		_ = e.conn.Close()
		<-e.conn.Done()
	}

	p.mu.Lock()
	if p.peers[e.info.ID] != e {
		p.mu.Unlock()
		e.retire()
		return
	}
	delete(p.peers, e.info.ID)
	parked := p.claimed(e)
	if parked {
		p.deferred[e.info.ID] = e
	}
	p.changed.Broadcast()
	p.mu.Unlock()
	if parked {
		return
	}
	p.emitDown(e)
}

// emit delivers an event, blocking for buffer space unless the pool is closing.
func (p *Pool) emit(ev PeerEvent) {
	select {
	case p.events <- ev:
		return
	default:
	}
	select {
	case p.events <- ev:
	case <-p.ctx.Done():
	}
}

// --- small helpers -----------------------------------------------------------------------------

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
	return true
}

func (p *Pool) untrackPending(raw net.Conn) {
	p.mu.Lock()
	delete(p.pending, raw)
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
