package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// headerSize is the width of the length prefix: one big-endian uint32.
//
// Big-endian ("network byte order") rather than little-endian, even though every
// machine this will realistically run on is little-endian, because it is the
// convention every other wire protocol and every packet-capture tool assumes. A
// length prefix that reads backwards in Wireshark costs more in debugging time
// than the byte-swap costs in CPU — which is, on any modern CPU, a single BSWAP
// instruction.
const headerSize = 4

// MaxFrameSize bounds the payload of a single frame at 1 MiB.
//
// # Why there is a bound at all
//
// This constant is the entire reason the wire format is length-prefixed rather
// than newline-delimited. With a delimiter, a receiver has no idea how much data
// is coming until the delimiter arrives, so a peer that is hostile — or merely
// broken, which is far more likely here — can stream bytes forever and the
// receiver's buffer grows until the container is OOM-killed. One peer turns into
// a swarm-wide outage. With a length prefix, the receiver learns the size first
// and can refuse before allocating anything.
//
// The ordering in ReadFrame is therefore load-bearing and not a matter of taste:
// decode the header, compare against this constant, and only then allocate. Read
// the length, allocate that many bytes, and *then* discover it was 4 GiB, and the
// bound has bought nothing.
//
// # Why 1 MiB specifically
//
// The bound has to sit above the largest legitimate message and below anything
// that threatens a container. The largest legitimate message is a telemetry
// snapshot: one JSON object per node with an ID, an address, a role, a score, a
// cluster attachment and a handful of counters — call it 500 bytes of JSON per
// node once field names are counted, which they must be, because JSON pays for its
// readability in key bytes on every single record. 1 MiB therefore accommodates
// roughly two thousand nodes in one snapshot, against a design target of a few
// hundred. That is an order of magnitude of headroom for a swarm that will
// realistically run ten containers.
//
// It is also small enough that a receiver hit with the maximum from every peer at
// once is inconvenienced rather than killed, which is the property that actually
// matters: the bound is per-frame, but the exposure is per-frame times fan-in.
//
// If a message class ever genuinely needs more than this, the answer is to chunk
// it at the application layer, not to raise this number. A protocol whose frame
// bound tracks its largest message has no bound.
const MaxFrameSize = 1 << 20

// Sentinel errors. Every one of these is a protocol violation by the peer, and
// every one of them is terminal for the connection — see the resynchronisation
// note on Decoder.
var (
	// ErrFrameTooLarge means the length header declared more than MaxFrameSize.
	// No payload was allocated and no payload bytes were consumed.
	ErrFrameTooLarge = errors.New("protocol: frame exceeds maximum size")

	// ErrZeroLengthFrame means the length header declared zero bytes.
	//
	// A zero-length frame is a protocol violation, explicitly NOT a keepalive.
	// Some protocols overload an empty frame as a liveness tick; this one refuses
	// to, because liveness here is a first-class message (TypePing, TypeHeartbeat)
	// carrying a sequence number and a correlation ID that the failure detector
	// needs. An untyped empty frame would be a second, weaker liveness channel that
	// the detector cannot account for, and a peer emitting them in a loop would be
	// a busy-loop that looks healthy.
	ErrZeroLengthFrame = errors.New("protocol: zero-length frame")

	// ErrUnsupportedVersion means the envelope decoded but declared a wire version
	// this build cannot interpret. Unlike an unknown MessageType, this is fatal:
	// an unknown version means the envelope *layout* may differ, so we cannot even
	// trust the fields we think we just read.
	ErrUnsupportedVersion = errors.New("protocol: unsupported wire version")

	// ErrMalformedFrame means the payload bytes were read successfully but are not
	// a valid envelope. The stream position is now untrustworthy.
	ErrMalformedFrame = errors.New("protocol: malformed frame")
)

// IsProtocolViolation reports whether err indicates that the peer is not speaking
// this protocol, as opposed to the connection having ended (io.EOF) or died
// (io.ErrUnexpectedEOF) or timed out.
//
// This three-way classification exists for the self-healing logic in Phase 4,
// which must treat the cases differently and must not collapse them into "the read
// failed":
//
//   - io.EOF at a frame boundary — the peer closed cleanly. It sent a FIN. That is
//     a graceful departure: the peer probably sent TypeLeave first, and even if it
//     did not, it chose to go. Not evidence of sickness, and not a reason to
//     penalise its health score for a future rejoin.
//   - io.ErrUnexpectedEOF mid-frame — the peer died holding the pen. Half a frame
//     arrived and then the socket ended. This is the SIGKILL signature and it IS a
//     failure signal.
//   - A protocol violation — the peer is alive and talking, but talking nonsense.
//     Version skew, a corrupted stream, or something that is not this protocol on
//     the port at all. Drop the connection; do NOT redial in a tight loop, because
//     it will do the same thing again.
//
// A read deadline (os.ErrDeadlineExceeded) is a fourth case, but it is raised by
// pkg/network, not here — see the deadline note on Decoder.
func IsProtocolViolation(err error) bool {
	return errors.Is(err, ErrFrameTooLarge) ||
		errors.Is(err, ErrZeroLengthFrame) ||
		errors.Is(err, ErrUnsupportedVersion) ||
		errors.Is(err, ErrMalformedFrame)
}

// ---------------------------------------------------------------------------
// Encoder
// ---------------------------------------------------------------------------

// Encoder writes length-prefixed JSON frames to an io.Writer.
//
// # Concurrency
//
// An Encoder IS safe for concurrent use by multiple goroutines. This is not
// defensive over-engineering; it is a requirement of the design. A single
// connection to a peer is written by at least two independent goroutines — the
// heartbeat ticker and whatever is dispatching tasks or telemetry — and they do
// not coordinate with each other. Without the mutex, two WriteEnvelope calls can
// interleave their header and payload writes on the same socket, and the receiver
// then reads one frame's length header followed by another frame's payload bytes.
// That is not a dropped message; it is a permanently desynchronised stream, and
// because every subsequent length is plausible garbage there is no recovery from
// it. The mutex is what makes a frame atomic with respect to other writers.
//
// The alternative design — a single writer goroutine owning the socket and a
// channel in front of it — is a good design and is what pkg/network may well build
// on top of this. It is not what belongs *here*, because it forces a buffering and
// backpressure policy (how deep is the channel? what gets dropped when it is
// full?) into the codec, and that policy differs per traffic plane. The codec
// guarantees atomicity; the transport chooses the queueing.
type Encoder struct {
	// mu guards both w and buf. It is held across the whole marshal-and-write so
	// that the frame reaches the writer as one uninterrupted unit.
	mu sync.Mutex
	// w is guarded by mu.
	w io.Writer
	// buf is the scratch frame buffer, guarded by mu. It is retained across calls
	// so that steady-state heartbeat traffic does not allocate a fresh buffer per
	// beat; it never escapes to the caller, so reuse is safe without qualification.
	buf []byte
}

// NewEncoder returns an Encoder writing to w.
//
// It takes an io.Writer rather than a net.Conn deliberately: that is what lets the
// codec be tested against a bytes.Buffer, and what keeps deadline policy out of
// this package. See the note on Decoder.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// WriteEnvelope marshals e and writes it as a single framed message.
//
// The size check happens before any bytes reach the writer, so an oversized
// envelope fails locally and leaves the stream untouched and still usable — the
// one protocol error in this package that is NOT terminal, because it is our bug
// rather than the peer's.
func (e *Encoder) WriteEnvelope(env *Envelope) error {
	if env == nil {
		return errors.New("protocol: WriteEnvelope(nil)")
	}
	if env.Version == 0 {
		env.Version = CurrentVersion
	}

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("protocol: marshal envelope %s: %w", env.Type, err)
	}
	if len(body) == 0 {
		// Unreachable via json.Marshal of a struct, which always emits at least
		// "{}". Asserted anyway because emitting a zero-length frame would hand the
		// peer an ErrZeroLengthFrame and kill a connection for our mistake.
		return ErrZeroLengthFrame
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("protocol: %s envelope is %d bytes (max %d): %w",
			env.Type, len(body), MaxFrameSize, ErrFrameTooLarge)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Assemble header and payload into ONE contiguous buffer and issue ONE Write.
	//
	// Two reasons, and the second is the serious one:
	//
	//  1. TCP_NODELAY is on (Go's default, and we keep it on because Nagle's
	//     interaction with delayed ACK injects tens of milliseconds into exactly
	//     the small control-plane messages whose latency we then feed into leader
	//     election). With Nagle disabled, two Write syscalls become two TCP
	//     segments: a 4-byte segment followed by a payload segment. That is two
	//     syscalls, two packets and two trips through the receiver's stack per
	//     heartbeat, for a message that fits in one.
	//
	//  2. A write that fails *between* the header and the payload leaves the peer
	//     holding a length prefix for a frame that will never arrive. It will block
	//     in ReadFull waiting for bytes the sender has already given up on, and the
	//     stream is unrecoverable. One Write cannot fail in that gap: io.Writer's
	//     contract is that Write returns a non-nil error if it returns n < len(p),
	//     so either the frame went out whole or the connection is already broken.
	//
	// The buffer is reused across calls; it is package-private, never handed out,
	// and only ever touched with e.mu held.
	need := headerSize + len(body)
	if cap(e.buf) < need {
		e.buf = make([]byte, need)
	}
	e.buf = e.buf[:need]
	// len(body) <= MaxFrameSize (1MiB) was checked above, so it fits a uint32.
	binary.BigEndian.PutUint32(e.buf[:headerSize], uint32(len(body))) // #nosec G115 -- bounded by MaxFrameSize above
	copy(e.buf[headerSize:], body)

	if _, err := e.w.Write(e.buf); err != nil {
		return fmt.Errorf("protocol: write %s frame: %w", env.Type, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

// Decoder reads length-prefixed JSON frames from an io.Reader.
//
// # Concurrency
//
// A Decoder is NOT safe for concurrent use, and deliberately carries no mutex.
// Exactly one goroutine owns the read side of a connection for that connection's
// lifetime.
//
// The absence of the mutex is a design statement rather than an omission. A mutex
// here would serialise nothing useful: two goroutines calling ReadFrame in turn
// would each get *a* frame, arbitrarily interleaved, with no way to know which
// logical stream it belonged to. It would convert a design error — two owners of
// one stream — into a silent correctness bug in which messages are delivered to
// the wrong handler. Leaving the type unsynchronised means the race detector finds
// that mistake on the first test run, which is exactly where it should be found.
//
// # Deadlines are not this package's job
//
// A Decoder takes an io.Reader, not a net.Conn, so it cannot set a read deadline.
// That is intentional: it is what allows every test in this package to run against
// a bytes.Buffer with no sockets involved.
//
// The consequence must be stated plainly because it is the single most dangerous
// property of this type. A Decoder wrapping a net.Conn with NO READ DEADLINE SET
// WILL BLOCK FOREVER against a peer that was SIGKILLed. The peer's container is
// gone, but no FIN was ever sent, so the local socket is "established" as far as
// the kernel is concerned and the read simply never returns. There is no error to
// observe, no EOF, nothing — just a goroutine parked in the netpoller until the
// process exits. Kernel TCP keepalive would eventually notice, on a default
// timescale measured in hours.
//
// Setting SetReadDeadline before every ReadFrame is therefore mandatory and is
// pkg/network's responsibility. That deadline, not the absence of bytes, is what
// makes failure detection possible at all — and this exact bug is why the swarm's
// chaos controls SIGKILL containers rather than stopping them politely.
type Decoder struct {
	// r is owned by the single reader goroutine; no synchronisation.
	r io.Reader
	// hdr is the reusable 4-byte length-header buffer. Reused because a fresh
	// 4-byte allocation per heartbeat is pure garbage-collector pressure, and
	// because an array field inside the Decoder does not escape.
	hdr [headerSize]byte
	// buf is the reusable payload scratch buffer. The frame's bytes live here only
	// between ReadFull and json.Unmarshal; see ReadFrame for why the returned
	// Envelope cannot alias it.
	buf []byte
}

// NewDecoder returns a Decoder reading from r. Only one goroutine may use it.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r}
}

// ReadFrame reads one frame and returns the Envelope it carried.
//
// Errors are classified for the caller; use IsProtocolViolation and errors.Is
// against io.EOF / io.ErrUnexpectedEOF to distinguish a clean close from a death
// from garbage. Any error other than a nil one is terminal for the connection:
// there is no resynchronisation, by design.
//
// # Why there is no resync
//
// A delimited protocol can recover from a bad message by scanning forward to the
// next delimiter. A length-prefixed one cannot, and no API is provided that
// pretends otherwise. Once the reader is off a frame boundary, the next four bytes
// it reads are some arbitrary slice of a JSON body, and that slice is a perfectly
// valid uint32. It might say 12, or it might say 1.7 billion. There is no bit
// pattern that means "frame starts here", so there is nothing to scan for and no
// way to test a guess. A resync would be a plausible-looking function that
// silently delivers fabricated messages to the election logic — strictly worse
// than dropping the connection and redialling, which is cheap, correct, and the
// swarm already knows how to do it.
func (d *Decoder) ReadFrame() (*Envelope, error) {
	// io.ReadFull, never a bare Read. A TCP Read returns whatever happened to have
	// arrived — the header can and does arrive split across two segments, and a
	// bare Read of 4 bytes returning 2 would silently corrupt every subsequent
	// frame. io.ReadFull loops until the buffer is full or the stream ends.
	//
	// The error is returned unwrapped. At this point we are exactly on a frame
	// boundary, so io.EOF here means the peer closed cleanly with nothing
	// outstanding, and callers check for it with errors.Is(err, io.EOF). ReadFull
	// itself supplies the other half of the distinction: if it read 1..3 bytes of
	// the header and then hit the end, it returns io.ErrUnexpectedEOF, meaning the
	// peer died partway through sending a length prefix.
	if _, err := io.ReadFull(d.r, d.hdr[:]); err != nil {
		return nil, err
	}

	n := binary.BigEndian.Uint32(d.hdr[:])

	if n == 0 {
		return nil, ErrZeroLengthFrame
	}

	// THE BOUNDS CHECK. This comparison happens against the decoded header, before
	// a single byte of payload is allocated or consumed. Inverting these two
	// statements — allocate, then validate — is the OOM bug that length-prefixing
	// exists to prevent, and it is a one-line edit away at all times.
	if n > MaxFrameSize {
		return nil, fmt.Errorf("protocol: peer declared %d-byte frame (max %d): %w",
			n, MaxFrameSize, ErrFrameTooLarge)
	}

	// Grow the scratch buffer only when it is genuinely too small. In steady state
	// (heartbeats and pongs, a few hundred bytes each) this allocates once and then
	// never again for the life of the connection.
	if cap(d.buf) < int(n) {
		d.buf = make([]byte, n)
	}
	d.buf = d.buf[:n]

	if _, err := io.ReadFull(d.r, d.buf); err != nil {
		// A header arrived and then the payload did not. Whether ReadFull read zero
		// payload bytes (io.EOF) or some of them (io.ErrUnexpectedEOF), the meaning
		// is identical and is NOT a clean close: the peer committed to sending n
		// bytes and then vanished. Normalising io.EOF to io.ErrUnexpectedEOF here is
		// what preserves the "clean close vs died mid-frame" distinction that the
		// failover logic depends on — without it, a peer killed immediately after
		// writing a length prefix would be indistinguishable from a graceful leave.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("protocol: short payload for %d-byte frame: %w", n, err)
	}

	// Unmarshal out of the scratch buffer.
	//
	// ALIASING: this is the hazard that reusing d.buf creates, and it is handled
	// rather than hoped about. Envelope.Payload is a json.RawMessage, i.e. a
	// []byte, and a naive decoder implementation could hand back a slice pointing
	// into d.buf — which the very next ReadFrame would overwrite underneath the
	// caller. encoding/json does not do that: json.RawMessage implements
	// json.Unmarshaler, and its UnmarshalJSON is `*m = append((*m)[0:0], data...)`,
	// which copies the bytes into the RawMessage's own backing array. Because env
	// is freshly allocated on every call, (*m)[0:0] has zero capacity and the
	// append always allocates. The returned Payload therefore never aliases d.buf.
	//
	// This is verified by TestReadFramePayloadDoesNotAliasScratchBuffer rather than
	// asserted, because it is a property of another package's implementation and a
	// comment is not a test. If encoding/json ever changed it, that test fails and
	// the fix is an explicit copy here.
	env := &Envelope{}
	if err := json.Unmarshal(d.buf, env); err != nil {
		return nil, fmt.Errorf("protocol: decode envelope: %w: %v", ErrMalformedFrame, err)
	}

	// Version is checked here, not by the caller, because an unrecognised version
	// means the field layout itself is in question — including the fields we just
	// read. Contrast MessageType, which is deliberately NOT validated here: an
	// unknown type decodes successfully and is the caller's business to ignore.
	if env.Version != CurrentVersion {
		return nil, fmt.Errorf("protocol: peer %q speaks version %d, this build speaks %d: %w",
			env.From, env.Version, CurrentVersion, ErrUnsupportedVersion)
	}

	return env, nil
}
