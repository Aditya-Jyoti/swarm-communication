package network

// This file is the Pool's connection registry: registration with the tie-break,
// the watcher that turns a dead connection into a PeerDown, and the claims that
// decide whether that PeerDown is emitted, parked or discarded. See the Pool type
// comment for the reasoning.

import (
	"context"
	"net"

	"swarm-net/pkg/protocol"
)

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
		p.downInFlight[id] = e
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
// that is no longer covered by any claim, as releaseClaim does by ID.
func (p *Pool) releaseDialing(addr protocol.NodeAddress) {
	p.mu.Lock()
	if p.dialing[addr]--; p.dialing[addr] <= 0 {
		delete(p.dialing, addr)
	}
	var resolved []*peerEntry
	for id, e := range p.deferred {
		if !p.claimed(e) {
			delete(p.deferred, id)
			p.downInFlight[id] = e
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
	if p.claims[e.info.ID] > 0 || p.dialing[e.info.Advertise] > 0 {
		return true
	}
	for a := range p.addrOf[e.info.ID] {
		if p.dialing[a] > 0 {
			return true
		}
	}
	return false
}

// emitDown emits PeerDown for an entry that has left the map, then retires it.
// The caller has put e in downInFlight under mu; it is cleared only after the
// event is delivered, and retire follows that, so both a redial loop waiting on
// gone and a register waiting on downInFlight see the Down land first.
func (p *Pool) emitDown(e *peerEntry) {
	d, err := e.conn.Disposition()
	p.emit(PeerEvent{Kind: PeerDown, Peer: copyInfo(e.info), Disposition: d, Err: err})
	p.mu.Lock()
	if p.downInFlight[e.info.ID] == e {
		delete(p.downInFlight, e.info.ID)
	}
	p.changed.Broadcast()
	p.mu.Unlock()
	e.retire()
}

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
	// A PeerDown for this peer may be decided but not yet delivered (emission
	// happens outside mu). The PeerUp below must queue behind it.
	inFlight := p.downInFlight[info.ID]
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
		// Close the loser, then move its unsent control frames onto the winner.
		// Without this, a heartbeat or election frame queued in the instant
		// between the two handshakes completing would vanish with no PeerDown
		// to explain it. Data frames are not moved: they are shed under
		// pressure anyway, and moving them would count their drops against a
		// connection that never saw them. Re-queued control frames land behind
		// anything already queued on the winner, so control traffic may be
		// reordered across a swap; the cluster layer is idempotent on its
		// inputs, which is what makes that tolerable. What the loser had
		// already handed to the socket may still be lost if the peer closed
		// first -- the same exposure as any close.
		_ = existing.conn.Close()
		for _, env := range existing.conn.drainUnsent() {
			if env.Type.IsDataPlane() {
				continue
			}
			_ = e.conn.Send(context.Background(), env)
		}
		existing.retire()
		return e
	}
	if parked == nil {
		if inFlight != nil {
			<-inFlight.gone
		}
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
	} else {
		p.downInFlight[e.info.ID] = e
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
