package network

import (
	"net"
	"testing"
	"time"
)

// A flood of sockets that connect and say nothing must not park one goroutine
// each for the whole handshake timeout: beyond the cap, Admit closes at once,
// and a freed slot is usable again.
func TestAdmitCapsPendingHandshakes(t *testing.T) {
	h := newHarness(t, idA, func(c *PoolConfig) {
		c.MaxPendingHandshakes = 2
		c.HandshakeTimeout = time.Minute
	})
	var silent []net.Conn
	for i := 0; i < 2; i++ {
		remote, local, err := loopbackPair()
		if err != nil {
			t.Fatal(err)
		}
		defer remote.Close()
		h.pool.Admit(local)
		silent = append(silent, remote)
	}

	remote, local, err := loopbackPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	h.pool.Admit(local)
	_ = remote.SetReadDeadline(time.Now().Add(failsafe))
	if _, err := remote.Read(make([]byte, 1)); err == nil || isTimeoutErr(err) {
		t.Fatalf("socket beyond the cap was not closed at once: %v", err)
	}

	// Free a slot: the peer hangs up, the admit goroutine returns. The release
	// happens on that goroutine, so retry until a handshake gets through.
	_ = silent[0].Close()
	deadline := time.Now().Add(failsafe)
	for {
		c, _, err := h.inboundFrom(idB)
		if err == nil {
			defer c.Close()
			h.event(PeerUp)
			return
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("no slot freed after a pending peer hung up: %v", err)
		}
	}
}

func isTimeoutErr(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
