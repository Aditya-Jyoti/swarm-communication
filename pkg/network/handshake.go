package network

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"swarm-net/pkg/protocol"
)

// PeerInfo is what a handshake learns about the other side.
type PeerInfo struct {
	ID          protocol.NodeID
	Advertise   protocol.NodeAddress
	Incarnation int64
	KnownPeers  []protocol.NodeAddress
}

// Identity is what this node announces about itself.
type Identity struct {
	ID          protocol.NodeID
	Advertise   protocol.NodeAddress
	Incarnation int64
}

// Sentinel errors returned by the handshake functions. Each is wrapped with the
// local node, the remote address and (where known) the peer's reason, so a log line
// from a chaos run locates both ends without a packet capture.
var (
	// ErrHandshakeRejected means the acceptor answered HELLO with a rejection. The
	// wrapping error carries the peer's Reason verbatim.
	ErrHandshakeRejected = errors.New("network: handshake rejected")
	// ErrHandshakeTimeout means the deadline elapsed before HELLO/HELLO_ACK completed.
	// It is distinct from a read timeout on an established Conn: a half-open
	// handshake says nothing about the health of a peer we never identified.
	ErrHandshakeTimeout = errors.New("network: handshake timed out")
	// ErrSelfConnect means the peer's NodeID equals ours. This happens when a node's
	// advertised address resolves to itself (a misconfigured seed list), and it is
	// terminal: redialing cannot fix it.
	ErrSelfConnect = errors.New("network: connected to self")
)

// dialHandshake runs the initiator side: write HELLO, read HELLO_ACK.
//
// It works on the raw socket with the codec directly rather than through a Conn,
// because a Conn's reader would deliver the HELLO_ACK to the Handler, on a
// goroutine, for a peer whose identity is not yet known. Running the exchange
// synchronously here, under one absolute deadline, keeps the identity question
// answered before any goroutine is spawned for the link.
//
// On a rejection the returned PeerInfo is still populated with whatever the
// rejecting side disclosed (at minimum its NodeID from the envelope's From). The
// pool uses that to recognise "rejected because you are already connected to me",
// which is the tie-break outcome and not a failure to retry.
func dialHandshake(raw net.Conn, self Identity, known []protocol.NodeAddress, deadline time.Time) (PeerInfo, error) {
	var info PeerInfo
	wrap := func(err error) error {
		return fmt.Errorf("network: node %q: dial handshake with %s: %w", self.ID, raw.RemoteAddr(), err)
	}

	// One absolute deadline for the whole exchange, set before the first byte moves.
	// A per-operation timeout would let a peer that trickles one byte per timeout
	// hold the goroutine indefinitely.
	if err := raw.SetDeadline(deadline); err != nil {
		return info, wrap(err)
	}

	hello, err := protocol.NewEnvelope(protocol.TypeHello, self.ID, "", protocol.HelloPayload{
		Advertise:   self.Advertise,
		Incarnation: self.Incarnation,
		KnownPeers:  known,
	})
	if err != nil {
		return info, wrap(err)
	}
	if err := protocol.NewEncoder(raw).WriteEnvelope(hello); err != nil {
		return info, wrap(timeoutOr(err))
	}

	// The reply is read with the Decoder, never a bare Read: the ACK is a framed
	// message and can arrive split across segments like any other.
	ack, err := protocol.NewDecoder(raw).ReadFrame()
	if err != nil {
		return info, wrap(timeoutOr(err))
	}
	if ack.Type != protocol.TypeHelloAck {
		return info, wrap(fmt.Errorf("expected %s, got %s: %w", protocol.TypeHelloAck, ack.Type, protocol.ErrMalformedFrame))
	}
	payload, err := protocol.PayloadOf[protocol.HelloAckPayload](ack)
	if err != nil {
		return info, wrap(err)
	}
	info = PeerInfo{
		ID:          ack.From,
		Advertise:   payload.Advertise,
		Incarnation: payload.Incarnation,
		KnownPeers:  payload.KnownPeers,
	}

	switch {
	case !payload.Accepted && ack.From == self.ID:
		// The acceptor recognised our own ID and said so. Surface both sentinels:
		// callers that only know about rejection still classify it correctly, and
		// callers that check for self-connect stop redialing.
		return info, wrap(fmt.Errorf("%w: %w by %q: %s", ErrSelfConnect, ErrHandshakeRejected, ack.From, payload.Reason))
	case !payload.Accepted:
		return info, wrap(fmt.Errorf("%w by %q: %s", ErrHandshakeRejected, ack.From, payload.Reason))
	case ack.From == "":
		return info, wrap(fmt.Errorf("HELLO_ACK carries an empty node id: %w", protocol.ErrMalformedFrame))
	case ack.From == self.ID:
		return info, wrap(ErrSelfConnect)
	}

	// Clear the deadline before the Conn takes over; it sets its own per-frame
	// deadlines and must not inherit a handshake deadline that is about to fire.
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return info, wrap(err)
	}
	return info, nil
}

// acceptHandshake runs the acceptor side: read HELLO, validate, write HELLO_ACK.
//
// accept decides admission for a structurally valid HELLO (the pool uses it for the
// duplicate-connection tie-break). Structural rejections -- wrong version, wrong
// type, empty ID, our own ID -- are decided here and never reach accept.
//
// Every rejection is written to the peer with a reason before the error is
// returned. A peer that is simply disconnected learns nothing and redials in a
// loop; a peer that is told "version 2 vs 1" can log something actionable.
func acceptHandshake(raw net.Conn, self Identity, known []protocol.NodeAddress, deadline time.Time, accept func(PeerInfo) (bool, string)) (PeerInfo, error) {
	var info PeerInfo
	wrap := func(err error) error {
		return fmt.Errorf("network: node %q: accept handshake from %s: %w", self.ID, raw.RemoteAddr(), err)
	}

	if err := raw.SetDeadline(deadline); err != nil {
		return info, wrap(err)
	}
	enc := protocol.NewEncoder(raw)

	hello, err := protocol.NewDecoder(raw).ReadFrame()
	if err != nil {
		if errors.Is(err, protocol.ErrUnsupportedVersion) {
			// The decoder refuses the envelope before we can inspect it, so the
			// peer's version is only available in the decoder's text; the reason
			// names our version explicitly and carries the decoder's message for
			// theirs. The peer may not be able to parse our reply either; sending
			// it is best-effort, and the error we return is the decoder's, so it
			// still classifies as a protocol violation.
			sendRejection(enc, nil, self, fmt.Sprintf("this build speaks wire version %d: %v", protocol.CurrentVersion, err))
		}
		return info, wrap(timeoutOr(err))
	}
	if hello.Type != protocol.TypeHello {
		reason := fmt.Sprintf("expected %s, got %s", protocol.TypeHello, hello.Type)
		sendRejection(enc, hello, self, reason)
		return info, wrap(fmt.Errorf("%s: %w", reason, protocol.ErrMalformedFrame))
	}
	payload, err := protocol.PayloadOf[protocol.HelloPayload](hello)
	if err != nil {
		sendRejection(enc, hello, self, err.Error())
		return info, wrap(err)
	}
	info = PeerInfo{
		ID:          hello.From,
		Advertise:   payload.Advertise,
		Incarnation: payload.Incarnation,
		KnownPeers:  payload.KnownPeers,
	}

	switch {
	case info.ID == "":
		const reason = "HELLO carries an empty node id"
		sendRejection(enc, hello, self, reason)
		return info, wrap(fmt.Errorf("%w: %s", ErrHandshakeRejected, reason))
	case info.ID == self.ID:
		reason := fmt.Sprintf("node id %q is my own id (self-connect)", info.ID)
		sendRejection(enc, hello, self, reason)
		return info, wrap(fmt.Errorf("%w: %s", ErrSelfConnect, reason))
	}

	if ok, reason := accept(info); !ok {
		sendRejection(enc, hello, self, reason)
		return info, wrap(fmt.Errorf("%w: %s", ErrHandshakeRejected, reason))
	}

	ack, err := protocol.NewReply(hello, protocol.TypeHelloAck, self.ID, protocol.HelloAckPayload{
		Accepted:    true,
		Advertise:   self.Advertise,
		Incarnation: self.Incarnation,
		KnownPeers:  known,
	})
	if err != nil {
		return info, wrap(err)
	}
	if err := enc.WriteEnvelope(ack); err != nil {
		return info, wrap(timeoutOr(err))
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return info, wrap(err)
	}
	return info, nil
}

// sendRejection writes a HELLO_ACK with Accepted=false. It is best-effort: the
// caller is about to return an error and close the socket regardless, so a failed
// write of the rejection changes nothing and is deliberately not reported.
//
// req may be nil when the HELLO itself could not be decoded; the reply then has
// no correlation ID and an empty To, which is the most we can say.
func sendRejection(enc *protocol.Encoder, req *protocol.Envelope, self Identity, reason string) {
	payload := protocol.HelloAckPayload{
		Accepted:    false,
		Reason:      reason,
		Advertise:   self.Advertise,
		Incarnation: self.Incarnation,
	}
	var ack *protocol.Envelope
	var err error
	if req == nil {
		ack, err = protocol.NewEnvelope(protocol.TypeHelloAck, self.ID, "", payload)
	} else {
		ack, err = protocol.NewReply(req, protocol.TypeHelloAck, self.ID, payload)
	}
	if err != nil {
		return
	}
	_ = enc.WriteEnvelope(ack)
}

// timeoutOr maps a deadline error onto ErrHandshakeTimeout and leaves everything
// else alone, so callers can errors.Is against the sentinel while Classify still
// sees the original error for the non-timeout cases.
func timeoutOr(err error) error {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrHandshakeTimeout, err)
	}
	return err
}
