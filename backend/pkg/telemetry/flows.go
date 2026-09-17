package telemetry

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// flowKey is one (destination, message type) pair.
type flowKey struct {
	to  protocol.NodeID
	typ protocol.MessageType
}

// FlowRecorder is a network.Transport that counts the frames sent through it,
// per destination and message type, for the dashboard's message animation.
//
// It wraps the mesh transport for both the cluster node and the MeshProber, so
// heartbeats, gossip, PINGs and PONGs are all counted. It must NOT wrap the
// Control Center uplink: the contract counts mesh frames only.
//
// Only frames the transport accepted are counted: a Send that returns nil
// counts once, a broadcast counts once per peer that accepted it. A frame
// accepted into a queue can still be lost if the connection dies before the
// writer drains it; the animation is a picture of traffic, not an audit.
//
// # Broadcast attribution
//
// Transport.Broadcast returns only a count. When the wrapped transport also
// implements network.RecipientBroadcaster (*network.Pool does), the recorder
// uses it and the attribution is exact. Otherwise it falls back to calling
// Broadcast and attributing the frame to the peers listed by Peers() just
// afterwards, at most as many as Broadcast reported. That is an
// approximation: a peer that connected or dropped in between is miscounted,
// and when some sends failed the recorder cannot know which ones.
//
// # Synchronisation
//
// mu guards counts. Send is called from the node loop, the prober's reply
// goroutines and probe goroutines, and Drain from the telemetry client, so a
// mutex around one map increment is the simplest correct choice; it is held
// for a map operation only, never across a send.
type FlowRecorder struct {
	inner network.Transport

	mu     sync.Mutex
	counts map[flowKey]int
}

// NewFlowRecorder wraps t.
func NewFlowRecorder(t network.Transport) *FlowRecorder {
	return &FlowRecorder{inner: t, counts: make(map[flowKey]int)}
}

var (
	_ network.Transport            = (*FlowRecorder)(nil)
	_ network.RecipientBroadcaster = (*FlowRecorder)(nil)
)

// Self returns the wrapped transport's identity.
func (r *FlowRecorder) Self() protocol.NodeID { return r.inner.Self() }

// Peers returns the wrapped transport's peers.
func (r *FlowRecorder) Peers() []network.PeerInfo { return r.inner.Peers() }

// Events returns the wrapped transport's event channel.
func (r *FlowRecorder) Events() <-chan network.PeerEvent { return r.inner.Events() }

// Send forwards to the wrapped transport and counts the frame if it was
// accepted.
func (r *FlowRecorder) Send(ctx context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	err := r.inner.Send(ctx, to, env)
	if err == nil && env != nil {
		r.mu.Lock()
		r.counts[flowKey{to, env.Type}]++
		r.mu.Unlock()
	}
	return err
}

// Broadcast forwards to the wrapped transport and counts the frame once per
// recipient. See the type comment for how recipients are known.
func (r *FlowRecorder) Broadcast(ctx context.Context, env *protocol.Envelope) int {
	return len(r.BroadcastRecipients(ctx, env))
}

// BroadcastRecipients is Broadcast that reports the recipients, so recorders
// can be stacked without losing exactness. With a wrapped transport that does
// not implement network.RecipientBroadcaster, the list is the approximation
// described on the type.
func (r *FlowRecorder) BroadcastRecipients(ctx context.Context, env *protocol.Envelope) []protocol.NodeID {
	if env == nil {
		return nil
	}
	var to []protocol.NodeID
	if rb, ok := r.inner.(network.RecipientBroadcaster); ok {
		to = rb.BroadcastRecipients(ctx, env)
	} else {
		n := r.inner.Broadcast(ctx, env)
		if n > 0 {
			for _, p := range r.inner.Peers() {
				if len(to) == n {
					break
				}
				to = append(to, p.ID)
			}
		}
	}
	if len(to) == 0 {
		return to
	}
	r.mu.Lock()
	for _, id := range to {
		r.counts[flowKey{id, env.Type}]++
	}
	r.mu.Unlock()
	return to
}

// Drain returns the counts accumulated since the previous Drain and resets
// them. The result is sorted busiest first (ties by destination, then type, so
// it is deterministic) and capped at protocol.MaxFlowRecords; the quietest
// pairs are the ones dropped, since they matter least to the animation. It
// never returns nil. Safe from any goroutine.
func (r *FlowRecorder) Drain() []protocol.FlowRecord {
	r.mu.Lock()
	counts := r.counts
	// Sized like the last interval: traffic is steady, so this avoids regrowing.
	r.counts = make(map[flowKey]int, len(counts))
	r.mu.Unlock()

	all := make([]protocol.FlowRecord, 0, len(counts))
	for k, c := range counts {
		all = append(all, protocol.FlowRecord{To: k.to, Type: k.typ, Count: c})
	}
	slices.SortFunc(all, func(a, b protocol.FlowRecord) int {
		if c := cmp.Compare(b.Count, a.Count); c != 0 {
			return c
		}
		if c := cmp.Compare(a.To, b.To); c != 0 {
			return c
		}
		return cmp.Compare(a.Type, b.Type)
	})
	// Clipped with a full slice expression so the caller cannot append into
	// the dropped tail.
	n := min(len(all), protocol.MaxFlowRecords)
	return all[:n:n]
}
