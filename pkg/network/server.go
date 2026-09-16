package network

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"
)

// ListenerConfig configures a Listener.
type ListenerConfig struct {
	// Addr is the "host:port" to bind, e.g. ":7000" or "127.0.0.1:0".
	Addr string
	// Now is accepted for symmetry with PoolConfig and ConnConfig. The accept
	// loop itself sets no deadlines -- Accept blocks until a connection arrives or
	// the listener is closed, and the handshake deadline is the pool's business --
	// so it is currently unused.
	Now func() time.Time
}

// Listener binds a TCP port and feeds every accepted socket to a Pool.
//
// # Goroutine ownership
//
// One goroutine, acceptLoop, started by Listen and joined by Close. It cannot
// leak: net.Listener.Close makes a blocked Accept return an error, and the loop
// returns on any error that is not classified as temporary.
//
// # Why no keep-alive
//
// SetKeepAlive is deliberately not called on accepted sockets. Keep-alive probes
// run on kernel timers measured in minutes and would be a second, slower failure
// detector arguing with the first. The Conn's per-frame read deadline is the
// failure detector; a peer that stops sending is dead to us at IdleTimeout, and
// nothing the kernel could add to that is faster.
type Listener struct {
	ln   net.Listener
	pool *Pool

	// done is closed by Close, before ln.Close, so the accept loop can tell a
	// deliberate shutdown from an error it should back off on. closeOnce guards
	// against a double close of done.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Listen binds cfg.Addr and starts accepting into pool.
func Listen(cfg ListenerConfig, pool *Pool) (*Listener, error) {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("network: node %q: listen %s: %w", pool.Self(), cfg.Addr, err)
	}
	return serve(ln, pool), nil
}

// serve wraps an already-bound listener. It is the seam tests use to inject a
// listener that fails in controlled ways.
func serve(ln net.Listener, pool *Pool) *Listener {
	l := &Listener{ln: ln, pool: pool, done: make(chan struct{})}
	l.wg.Add(1)
	go l.acceptLoop()
	return l
}

// Addr returns the bound address, which is how a caller that asked for port 0
// learns what it got.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close stops the accept loop and joins it. Idempotent. Sockets already handed to
// the pool are the pool's to close.
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.done)
		err = l.ln.Close()
	})
	l.wg.Wait()
	return err
}

// Accept backoff bounds. The first retry is fast because the common temporary
// error (ECONNABORTED: the client hung up while in the backlog) is over already;
// the cap exists for EMFILE, where retrying in a tight loop just burns CPU until
// something else closes a descriptor.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	delay := time.Duration(0)
	for {
		raw, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if !isTemporaryAcceptError(err) {
				// Anything else means the listener itself is broken (closed by
				// someone other than us, or the interface went away). There is
				// no accepting left to do.
				return
			}
			if delay == 0 {
				delay = acceptBackoffMin
			} else if delay *= 2; delay > acceptBackoffMax {
				delay = acceptBackoffMax
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-l.done:
				timer.Stop()
				return
			}
			continue
		}
		delay = 0
		// Admit never blocks: the handshake runs on the pool's goroutine, so one
		// client that connects and says nothing cannot stall every other accept.
		l.pool.Admit(raw)
	}
}

// isTemporaryAcceptError reports whether Accept's error is worth retrying.
//
// Descriptor exhaustion and a client that aborted while queued are the two real
// cases. The Temporary() check is on a local interface rather than net.Error
// because net.Error.Temporary is deprecated -- its meaning was never well defined
// -- but it is still what the standard library sets for exactly these conditions.
func isTemporaryAcceptError(err error) bool {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EAGAIN) {
		return true
	}
	var temp interface{ Temporary() bool }
	return errors.As(err, &temp) && temp.Temporary()
}
