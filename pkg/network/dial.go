package network

// This file is the outbound half of the Pool: the per-address redial loop, the
// single dial attempt, and the backoff schedule.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"swarm-net/pkg/protocol"
)

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
	if info.ID != "" {
		// The ACK has named the peer. From here on the claim is by ID as well,
		// which covers a peer that advertises a different string from the one we
		// dialled; and the address is remembered against the ID so the next dial
		// to it is covered from its first packet.
		p.mu.Lock()
		p.claims[info.ID]++
		if p.addrOf[info.ID] == nil {
			p.addrOf[info.ID] = make(map[protocol.NodeAddress]struct{})
		}
		p.addrOf[info.ID][addr] = struct{}{}
		p.changed.Broadcast()
		p.mu.Unlock()
		defer p.releaseClaim(info.ID)
	}
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
