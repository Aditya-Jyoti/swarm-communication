package network

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/protocol"
)

// Disposition classifies why a connection's read loop stopped.
//
// pkg/protocol defines three of these cases and explicitly delegates the fourth --
// the read deadline -- to this package, because a Decoder holds an io.Reader and
// cannot set one. The distinction is not cosmetic: Phase 4 failover branches on it,
// and conflating a clean shutdown with a death means a graceful scale-down triggers
// a swarm-wide re-election.
type Disposition int

const (
	// DispositionOther is an unclassified transport error.
	DispositionOther Disposition = iota
	// DispositionCleanClose is io.EOF arriving exactly at a frame boundary: the peer
	// finished the frame it was sending and then closed. Not evidence of sickness.
	DispositionCleanClose
	// DispositionPeerDied is io.ErrUnexpectedEOF: the peer vanished mid-frame. This
	// is the SIGKILL signature and it IS a failure signal.
	DispositionPeerDied
	// DispositionProtocolViolation is a frame this codec refuses. The stream cannot
	// be resynchronised, so the connection is finished -- but the peer is not
	// necessarily dead, and redialing in a tight loop would be a bug.
	DispositionProtocolViolation
	// DispositionTimeout is our own read deadline elapsing. It means silence, which
	// is exactly what a SIGKILLed peer produces: the socket stays ESTABLISHED and
	// never errors on its own, so without this deadline the reader waits forever.
	DispositionTimeout
)

func (d Disposition) String() string {
	switch d {
	case DispositionCleanClose:
		return "clean-close"
	case DispositionPeerDied:
		return "peer-died"
	case DispositionProtocolViolation:
		return "protocol-violation"
	case DispositionTimeout:
		return "timeout"
	default:
		return "other"
	}
}

// IsFailure reports whether a disposition is evidence that the peer is unhealthy.
//
// A clean close is not: the peer told us it was leaving. A protocol violation is
// not either -- it says the peer is speaking nonsense, not that it is dead, and
// counting it as a missed beat would evict a node over a version skew.
func (d Disposition) IsFailure() bool {
	return d == DispositionPeerDied || d == DispositionTimeout
}

// Classify maps a read error onto the four-way taxonomy.
//
// Order matters. os.ErrDeadlineExceeded is checked before the EOF cases because a
// deadline firing mid-frame surfaces as a read error that could otherwise be
// mistaken for truncation.
func Classify(err error) Disposition {
	switch {
	case err == nil:
		return DispositionOther
	case errors.Is(err, os.ErrDeadlineExceeded):
		return DispositionTimeout
	case errors.Is(err, io.ErrUnexpectedEOF):
		return DispositionPeerDied
	case errors.Is(err, io.EOF):
		return DispositionCleanClose
	case protocol.IsProtocolViolation(err):
		return DispositionProtocolViolation
	default:
		return DispositionOther
	}
}

// Sentinel errors returned by Send.
var (
	// ErrConnClosed means the connection is gone. Callers redial; they do not retry.
	ErrConnClosed = errors.New("network: connection closed")
	// ErrSendQueueFull means a control-plane send could not be queued within its
	// deadline. It is deliberately distinct from a write error: the frame never
	// reached the socket, so the stream is still intact and the peer is merely slow.
	ErrSendQueueFull = errors.New("network: send queue full")
	// ErrDataDropped means a data-plane frame was shed because its queue was full.
	ErrDataDropped = errors.New("network: data frame dropped")
)

// ConnConfig tunes one connection. The zero value is usable; every field has a
// default applied by newConn.
type ConnConfig struct {
	// IdleTimeout bounds how long a read may block. It MUST exceed the heartbeat
	// interval with margin, or healthy peers are reaped for being quiet.
	IdleTimeout time.Duration
	// WriteTimeout bounds a single frame write. A peer whose receive window is full
	// would otherwise block the writer goroutine indefinitely.
	WriteTimeout time.Duration
	// CtrlDepth and DataDepth size the two send queues.
	CtrlDepth int
	DataDepth int
	// CtrlSendTimeout is how long a control-plane send waits for queue space before
	// giving up. Short: a caller blocked here is usually the election loop.
	CtrlSendTimeout time.Duration
	// Now is injectable so deadline arithmetic is testable without a wall clock.
	Now func() time.Time
}

func (c ConnConfig) withDefaults() ConnConfig {
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 15 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	if c.CtrlDepth <= 0 {
		c.CtrlDepth = 64
	}
	if c.DataDepth <= 0 {
		c.DataDepth = 256
	}
	if c.CtrlSendTimeout <= 0 {
		c.CtrlSendTimeout = 250 * time.Millisecond
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Conn is one framed, full-duplex link to a peer.
//
// # Goroutine ownership
//
// A Conn owns exactly two goroutines, both started by newConn and both guaranteed
// to exit when done is closed:
//
//   - the reader, which is the sole owner of the Decoder. protocol.Decoder carries
//     no mutex by design, so a second reader would be a data race that -race
//     catches immediately rather than a subtle interleaving bug.
//   - the writer, which is the sole owner of the socket's write side. The Encoder
//     is itself mutex-safe, but routing every frame through one goroutine is what
//     buys the queueing and backpressure policy that the codec deliberately
//     refuses to choose.
//
// Neither can leak: the reader returns on any read error (and the idle deadline
// guarantees one arrives), the writer returns when done is closed, and Close is
// idempotent.
type Conn struct {
	peer protocol.NodeID
	raw  net.Conn
	enc  *protocol.Encoder
	dec  *protocol.Decoder
	cfg  ConnConfig

	// ctrl and data are the two send queues. Separating them is the whole point:
	// a heartbeat must never queue behind a large task payload.
	ctrl chan *protocol.Envelope
	data chan *protocol.Envelope

	// done is closed exactly once by Close and is the shutdown signal for both
	// goroutines. Guarded by closeOnce so a reader error and a writer error racing
	// to close cannot double-close it.
	done      chan struct{}
	closeOnce sync.Once
	// writerDone is closed by writeLoop on exit. done alone does not prove the
	// writer has stopped touching the queues: it can dequeue a frame after done
	// closes and lose it on the closed socket. drainUnsent waits on this.
	writerDone chan struct{}

	// dropped counts shed data-plane frames. Atomic because it is written by
	// senders on arbitrary goroutines and read by telemetry.
	dropped atomic.Uint64

	// disposition records why the reader stopped. Written once by the reader before
	// it closes done; readable only after Done is closed, which establishes the
	// happens-before edge that makes the plain field safe.
	disposition Disposition
	closeErr    error
}

// Handler receives every decoded frame, on the reader goroutine.
//
// It must not block: doing so stalls the read loop and stops the idle deadline from
// being refreshed, which eventually kills the connection it is running on.
type Handler func(peer protocol.NodeID, env *protocol.Envelope)

// NewConn wraps an established socket and starts its two goroutines.
//
// The caller has already completed the handshake and knows the peer's identity;
// this constructor does not negotiate.
func NewConn(peer protocol.NodeID, raw net.Conn, h Handler, cfg ConnConfig) *Conn {
	cfg = cfg.withDefaults()
	c := &Conn{
		peer: peer,
		raw:  raw,
		enc:  protocol.NewEncoder(raw),
		dec:  protocol.NewDecoder(raw),
		cfg:  cfg,
		ctrl: make(chan *protocol.Envelope, cfg.CtrlDepth),
		data: make(chan *protocol.Envelope, cfg.DataDepth),
		done: make(chan struct{}),

		writerDone: make(chan struct{}),
	}
	go c.readLoop(h)
	go c.writeLoop()
	return c
}

// Peer returns the remote node's identity.
func (c *Conn) Peer() protocol.NodeID { return c.peer }

// Done is closed when the connection has finished shutting down.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Dropped returns the number of data-plane frames shed due to backpressure.
func (c *Conn) Dropped() uint64 { return c.dropped.Load() }

// Disposition reports why the connection ended. It is only meaningful after Done is
// closed; reading it earlier races with the reader goroutine.
func (c *Conn) Disposition() (Disposition, error) {
	select {
	case <-c.done:
		return c.disposition, c.closeErr
	default:
		return DispositionOther, nil
	}
}

// Send queues env for transmission, routing it by traffic plane.
//
// Control-plane frames block briefly for queue space and report ErrSendQueueFull on
// timeout. Data-plane frames are dropped immediately when their queue is full and
// report ErrDataDropped. The asymmetry is the design: losing a telemetry sample is a
// gap in a graph, losing an election result is a split brain.
func (c *Conn) Send(ctx context.Context, env *protocol.Envelope) error {
	if env == nil {
		return errors.New("network: Send(nil)")
	}

	// Check for closure before offering the frame to a queue.
	//
	// Without this, a select with both `c.ctrl <- env` and `<-c.done` ready picks
	// one at random, so a closed connection would accept frames about half the time
	// and report success for bytes that can never be written. The check cannot make
	// the race disappear -- Close may land immediately after it -- but it makes a
	// definitely-closed connection reject deterministically, which is the contract
	// callers can actually rely on.
	select {
	case <-c.done:
		return ErrConnClosed
	default:
	}

	if env.Type.IsDataPlane() {
		select {
		case c.data <- env:
			return nil
		case <-c.done:
			return ErrConnClosed
		default:
			// Shed rather than block. A caller sending telemetry must not be
			// stalled by a peer that has stopped reading.
			c.dropped.Add(1)
			return ErrDataDropped
		}
	}

	// Control plane, including unknown types: when in doubt, do not drop.
	timer := time.NewTimer(c.cfg.CtrlSendTimeout)
	defer timer.Stop()

	select {
	case c.ctrl <- env:
		return nil
	case <-c.done:
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrSendQueueFull
	}
}

// Close shuts the connection down. It is safe to call from any goroutine, any number
// of times, and always returns the same result.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		// Closing the socket unblocks a reader parked in ReadFull and a writer
		// parked in Write. Without this, shutdown would wait out the idle timeout.
		_ = c.raw.Close()
	})
	return nil
}

// drainUnsent returns every frame that was queued but never handed to the socket,
// control frames first. It blocks until the writer goroutine has exited: only then
// is nothing else receiving from the queues, so the non-blocking receives below
// see exactly what was left. Waiting on done would not be enough -- the writer can
// dequeue one more frame after done closes and fail to write it.
//
// The pool uses this when it replaces a connection with a better one to the same
// peer, so a caller's frame is not silently lost in the swap. A frame the writer
// had already dequeued (whether or not the write then failed) is not recoverable
// and is not returned.
func (c *Conn) drainUnsent() []*protocol.Envelope {
	<-c.writerDone
	var out []*protocol.Envelope
	for drained := false; !drained; {
		select {
		case env := <-c.ctrl:
			out = append(out, env)
		default:
			drained = true
		}
	}
	for drained := false; !drained; {
		select {
		case env := <-c.data:
			out = append(out, env)
		default:
			drained = true
		}
	}
	return out
}

// closeWith records why the connection ended, then closes it. Only the reader
// goroutine calls this, so the unsynchronised writes to disposition and closeErr
// happen before close(done) and are safely published by it.
func (c *Conn) closeWith(d Disposition, err error) {
	c.closeOnce.Do(func() {
		c.disposition = d
		c.closeErr = err
		close(c.done)
		_ = c.raw.Close()
	})
}

// readLoop owns the Decoder for this connection's lifetime.
//
// Every iteration sets a fresh read deadline before calling ReadFrame. This is
// mandatory, not defensive: a peer that was SIGKILLed leaves a socket that stays
// ESTABLISHED and never returns an error, so the deadline -- not the absence of
// bytes -- is what makes failure detection possible at all.
func (c *Conn) readLoop(h Handler) {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		if err := c.raw.SetReadDeadline(c.cfg.Now().Add(c.cfg.IdleTimeout)); err != nil {
			c.closeWith(DispositionOther, err)
			return
		}

		env, err := c.dec.ReadFrame()
		if err != nil {
			c.closeWith(Classify(err), err)
			return
		}
		if h != nil {
			h(c.peer, env)
		}
	}
}

// writeLoop owns the socket's write side.
//
// Control frames are drained in preference to data frames so that a saturated data
// queue cannot delay a heartbeat. The nested select is how Go expresses that
// priority: the outer non-blocking attempt on ctrl runs first, and only when ctrl is
// empty does the inner select allow a data frame through.
func (c *Conn) writeLoop() {
	defer close(c.writerDone)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		var env *protocol.Envelope
		select {
		case env = <-c.ctrl:
		default:
			select {
			case env = <-c.ctrl:
			case env = <-c.data:
			case <-c.done:
				return
			}
		}

		if err := c.raw.SetWriteDeadline(c.cfg.Now().Add(c.cfg.WriteTimeout)); err != nil {
			c.closeWith(DispositionOther, err)
			return
		}
		if err := c.enc.WriteEnvelope(env); err != nil {
			// A failed write leaves the peer holding a length prefix for a frame
			// that will never arrive, so the stream is unrecoverable regardless of
			// why it failed. Closing is the only correct response.
			c.closeWith(Classify(err), err)
			return
		}
	}
}
