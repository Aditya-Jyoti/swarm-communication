package network

import (
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// The per-peer hook is evaluated for every PONG, with the sender's ID, and its
// result is added to the CHAOS delay.
func TestSetPeerDelayAddsPerPeerDelay(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	defer m.Close()
	ft := newFakeTimer()
	m.timer = ft.timer

	var (
		mu    sync.Mutex
		calls []protocol.NodeID
		next  = 10 * time.Millisecond
	)
	m.SetPeerDelay(func(peer protocol.NodeID) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, peer)
		d := next
		next += 10 * time.Millisecond // stands in for a fresh jitter draw
		return d
	})

	ping, _ := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{Nonce: 1})
	m.HandleFrame(idA.ID, ping)
	if d := <-ft.armed; d != 10*time.Millisecond {
		t.Errorf("first timer = %v, want 10ms", d)
	}
	tr.nothingSent(t) // armed, not fired: no PONG yet
	ft.fire(0)
	if pong := tr.next(t); pong.to != idA.ID || pong.env.Type != protocol.TypePong {
		t.Errorf("sent %s to %q, want PONG to %q", pong.env.Type, pong.to, idA.ID)
	}

	// The CHAOS delay stacks on top, and the hook is asked again.
	m.SetDelay(100 * time.Millisecond)
	m.HandleFrame(idA.ID, ping)
	if d := <-ft.armed; d != 120*time.Millisecond {
		t.Errorf("second timer = %v, want 100ms chaos + 20ms peer", d)
	}
	ft.fire(1)
	tr.next(t)

	mu.Lock()
	if len(calls) != 2 || calls[0] != idA.ID || calls[1] != idA.ID {
		t.Errorf("hook calls = %v, want [%s %s]", calls, idA.ID, idA.ID)
	}
	mu.Unlock()

	// With the hook removed and no CHAOS delay the answer is immediate.
	m.SetPeerDelay(nil)
	m.SetDelay(0)
	m.HandleFrame(idA.ID, ping)
	tr.next(t)
	select {
	case d := <-ft.armed:
		t.Errorf("a timer was armed for %v with no delay set", d)
	default:
	}
}

// A negative hook result neither delays on its own nor shortens a CHAOS delay.
func TestSetPeerDelayIgnoresNegative(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	defer m.Close()
	ft := newFakeTimer()
	m.timer = ft.timer
	m.SetPeerDelay(func(protocol.NodeID) time.Duration { return -time.Second })

	ping, _ := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{})
	m.HandleFrame(idA.ID, ping)
	tr.next(t)

	m.SetDelay(50 * time.Millisecond)
	m.HandleFrame(idA.ID, ping)
	if d := <-ft.armed; d != 50*time.Millisecond {
		t.Errorf("timer = %v, want 50ms", d)
	}
	ft.fire(0)
	tr.next(t)
}

// Close cancels a reply parked on a per-peer delay, exactly as for CHAOS.
func TestCloseCancelsAPeerDelayedReply(t *testing.T) {
	tr := newFakeTransport(idB.ID)
	m := NewMeshProber(tr, resolveAB, nil)
	ft := newFakeTimer()
	m.timer = ft.timer
	m.SetPeerDelay(func(protocol.NodeID) time.Duration { return time.Hour })

	ping, _ := protocol.NewEnvelope(protocol.TypePing, idA.ID, idB.ID, protocol.PingPayload{})
	m.HandleFrame(idA.ID, ping)
	<-ft.armed
	m.Close()
	tr.nothingSent(t)
}
