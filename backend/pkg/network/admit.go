package network

// This file is the inbound half of the Pool: the per-socket admit goroutine and
// the admission policy that applies the tie-break to a HELLO before it is
// answered.

import (
	"fmt"
	"net"

	"swarm-net/pkg/protocol"
)

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
		if info.Incarnation < existing.info.Incarnation {
			return false, fmt.Sprintf("already connected to %q at incarnation %d; incarnation %d is older", info.ID, existing.info.Incarnation, info.Incarnation)
		}
		return false, fmt.Sprintf("already connected to %q; the connection initiated by the lower node id (%q) wins", info.ID, p.cfg.Self.ID)
	}
	p.claims[info.ID]++
	p.changed.Broadcast()
	return true, ""
}
