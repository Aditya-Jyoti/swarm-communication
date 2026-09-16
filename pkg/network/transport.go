package network

import (
	"context"
	"errors"

	"swarm-net/pkg/protocol"
)

// PeerEventKind says whether a peer link came up or went down.
type PeerEventKind uint8

const (
	// PeerUp means a handshaken connection to Peer is registered and Send will
	// reach it.
	PeerUp PeerEventKind = iota + 1
	// PeerDown means the connection to Peer ended. Disposition says how, and only
	// Disposition.IsFailure() is evidence that the peer is unhealthy.
	PeerDown
)

func (k PeerEventKind) String() string {
	switch k {
	case PeerUp:
		return "peer-up"
	case PeerDown:
		return "peer-down"
	default:
		return "unknown"
	}
}

// PeerEvent is one membership-relevant change on the transport.
//
// The pool emits exactly one PeerUp when a peer becomes reachable and exactly one
// PeerDown when it stops being reachable. A connection that is silently replaced
// by a better one to the same peer (the simultaneous-dial tie-break, or a restarted
// peer with a newer incarnation) produces neither: from the consumer's point of
// view the peer never went away.
type PeerEvent struct {
	Kind PeerEventKind
	Peer PeerInfo
	// Disposition is meaningful for PeerDown only.
	Disposition Disposition
	// Err is the read/write error that ended the connection. PeerDown only.
	Err error
}

// Transport is what pkg/cluster consumes: a way to reach peers by NodeID without
// knowing anything about sockets. *Pool implements it.
type Transport interface {
	// Self returns this node's identity.
	Self() protocol.NodeID
	// Send queues env to one peer. It returns ErrUnknownPeer if no handshaken
	// connection to that peer exists, and otherwise whatever Conn.Send returns.
	Send(ctx context.Context, to protocol.NodeID, env *protocol.Envelope) error
	// Broadcast queues env to every connected peer and returns how many peers it
	// was successfully queued to. Per-peer failures are not reported: a broadcast
	// is best-effort by nature and the caller has PeerDown for the rest.
	Broadcast(ctx context.Context, env *protocol.Envelope) int
	// Peers lists every connected peer, sorted by ID so two calls are comparable.
	Peers() []PeerInfo
	// Events delivers PeerUp/PeerDown. It is closed by Close after the last event.
	Events() <-chan PeerEvent
}

// ErrUnknownPeer is returned by Send when no connection to the peer exists. It is
// not a transport failure: the caller asked for a node the mesh has not (or has
// no longer) got, and the right response is to consult membership, not retry.
var ErrUnknownPeer = errors.New("network: unknown peer")
