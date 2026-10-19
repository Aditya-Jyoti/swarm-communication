package mavlink

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// Wire constants.
const (
	// MagicV2 starts a MAVLink 2 frame.
	MagicV2 byte = 0xFD
	// MagicV1 starts a MAVLink 1 frame. Accepted on input, never emitted.
	MagicV1 byte = 0xFE

	// ProtocolVersion is what HEARTBEAT.MavlinkVersion must carry: 3 is the
	// value the MAVLink 2 dialects define, not 2. It is "the version of the
	// message set", which went 0, 2, 3, and the mismatch with the framing
	// version catches everyone once.
	ProtocolVersion = 3

	// MaxPayloadLen is the largest payload a frame can declare: the length field
	// is one byte.
	MaxPayloadLen = 255

	headerLenV2  = 10 // magic, len, incompat, compat, seq, sysid, compid, msgid[3]
	headerLenV1  = 6  // magic, len, seq, sysid, compid, msgid[1]
	crcLen       = 2
	signatureLen = 13

	// flagSigned is incompat_flags bit 0: a 13-byte signature follows the CRC.
	flagSigned byte = 0x01

	// MaxFrameLen bounds one frame on the wire, signature included. It is the
	// most a stream parser can ever need to buffer before it can either emit a
	// frame or declare the bytes garbage.
	MaxFrameLen = headerLenV2 + MaxPayloadLen + crcLen + signatureLen
)

// ---------------------------------------------------------------------------
// CRC-16/MCRF4XX
// ---------------------------------------------------------------------------

// crcInit is the CRC's initial value. MAVLink calls this algorithm "X.25", but
// it is CRC-16/MCRF4XX: init 0xFFFF, reflected polynomial 0x1021, no final
// XOR and no reflection of the output. Real X.25 (CRC-16/IBM-SDLC) finishes with
// a 0xFFFF XOR, which MAVLink omits. Using an off-the-shelf "X.25" routine is
// therefore a reliable way to produce checksums nothing accepts.
const crcInit uint16 = 0xFFFF

// crcAccumulate folds one byte into the running CRC.
//
// This is the bit-twiddled form of eight shift-and-xor steps through the
// reflected polynomial, and it is deliberately the same expression as the one in
// the reference C implementation (mavlink_helpers.h) and in the generator, so
// that all three can be compared by eye. A table-driven version would be faster,
// but a frame is at most 280 bytes at a few hundred frames a second, which is
// not a budget worth spending correctness on.
func crcAccumulate(crc uint16, b byte) uint16 {
	tmp := b ^ byte(crc)
	tmp ^= tmp << 4
	return (crc >> 8) ^ (uint16(tmp) << 8) ^ (uint16(tmp) << 3) ^ (uint16(tmp) >> 4)
}

// frameCRC computes the checksum of a frame body.
//
// body is everything from the length field through the last payload byte -- the
// magic byte is excluded, because it is a sync marker rather than data, and
// including it would make v1 and v2 frames of the same message checksum
// differently.
//
// crcExtra is then accumulated as if it were a 281st payload byte and is NOT
// transmitted. That is the clever part of the format: the checksum of a frame
// depends on the sender's *definition* of the message, so a sender and receiver
// that disagree about a message's fields -- a field inserted, a type widened, a
// unit changed -- produce mutually unacceptable frames instead of quietly
// misreading each other's numbers. It is a schema version check that costs zero
// bytes on the wire.
func frameCRC(body []byte, crcExtra byte) uint16 {
	crc := crcInit
	for _, b := range body {
		crc = crcAccumulate(crc, b)
	}
	return crcAccumulate(crc, crcExtra)
}

// ---------------------------------------------------------------------------
// Frame
// ---------------------------------------------------------------------------

// Frame is one decoded MAVLink frame.
type Frame struct {
	// Version is 1 or 2, the framing version actually seen on the wire.
	Version byte
	// IncompatFlags and CompatFlags are zero for v1 frames, which have no such
	// fields.
	IncompatFlags byte
	CompatFlags   byte
	// Seq is the sender's rolling frame counter. Gaps in it are how a ground
	// station computes link loss, which is why Sequencer keeps one counter per
	// sender rather than one per process.
	Seq byte
	// SysID and CompID identify the sender. For this bridge SysID is the vehicle
	// and CompID is always MavCompIDAutopilot1.
	SysID  byte
	CompID byte
	// ID is the message id from the header. It is kept alongside Msg because a
	// caller routing frames should switch on this, not on a type assertion.
	ID MsgID
	// Payload is the payload exactly as it arrived -- possibly truncated, so
	// len(Payload) may be less than Msg.PayloadLen(). It is a private copy; the
	// parser never hands out a window into its own buffer.
	Payload []byte
	// Msg is the decoded message. Never nil on a frame returned without error.
	Msg Message
	// Signed reports that the sender signed the frame. The signature is skipped,
	// not verified; see the package doc.
	Signed bool
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

// Encode appends a MAVLink v2 frame carrying m to dst and returns the extended
// slice. It emits incompat_flags 0 and compat_flags 0: unsigned, no extensions.
//
// Passing a dst with spare capacity (a per-link scratch buffer, reset to [:0]
// each time) makes the whole operation allocation-free, which matters because
// the bridge emits one frame per vehicle per message type per second and GC
// pressure in a 1 Hz loop is pure waste.
func Encode(dst []byte, seq, sysID, compID byte, m Message) []byte {
	start := len(dst)
	id := uint32(m.MsgID())
	dst = append(dst,
		MagicV2,
		0, // payload length, patched once truncation is known
		0, // incompat_flags
		0, // compat_flags
		seq, sysID, compID,
		byte(id), byte(id>>8), byte(id>>16), // 24-bit little-endian msgid
	)
	dst = m.Marshal(dst)

	// MAVLink 2 payload truncation.
	//
	// Trailing zero bytes of the payload are not transmitted, and the receiver
	// restores them. This is not an optimisation bolted on: it is what makes
	// extension fields free. A message that grows four new fields costs nothing
	// on the wire until someone sets one, so the whole dialect can keep evolving
	// without inflating a 1 Hz telemetry stream. HEARTBEAT, GLOBAL_POSITION_INT
	// and friends typically lose a few bytes each; MISSION_CURRENT, which has
	// twelve bytes of extensions this bridge leaves at zero, loses two thirds.
	//
	// Trimming to zero length is legal and happens for real: MISSION_REQUEST_LIST
	// to target 0/0 is an all-zero payload. Nothing downstream may assume a
	// non-empty payload.
	n := len(dst) - start - headerLenV2
	for n > 0 && dst[start+headerLenV2+n-1] == 0 {
		n--
	}
	dst = dst[:start+headerLenV2+n]
	dst[start+1] = byte(n)

	crc := frameCRC(dst[start+1:], m.CRCExtra())
	return binary.LittleEndian.AppendUint16(dst, crc)
}

// Sequencer hands out frame sequence numbers, one counter per (sysid, compid).
//
// # Why per sender and not per process
//
// A ground station derives link quality from gaps in the sequence number of each
// (sysid, compid) stream it sees. This bridge speaks for every node in the swarm
// at once, from one socket. With a single shared counter, each of ten vehicles
// would appear to advance its sequence by ten per beat, and QGroundControl would
// report 90% packet loss on all of them -- permanently, and with no other
// symptom. Per-sender counters are what make the loss figure mean something,
// which matters here because losing a vehicle is the demo.
//
// # Concurrency
//
// Safe for concurrent use. The zero value is ready; the map is built on first
// use under the mutex. A mutex rather than a sync.Map or an atomic: the critical
// section is a map lookup and an increment, the contention is a handful of
// goroutines at 1 Hz, and correctness here is "two callers never get the same
// number", which is exactly what a mutex says without further argument.
type Sequencer struct {
	mu   sync.Mutex
	next map[uint16]byte // key is sysid<<8 | compid; guarded by mu
}

// Next returns the next sequence number for a sender. It wraps at 255, as the
// protocol intends.
func (s *Sequencer) Next(sysID, compID byte) byte {
	key := uint16(sysID)<<8 | uint16(compID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next == nil {
		s.next = make(map[uint16]byte)
	}
	seq := s.next[key]
	s.next[key] = seq + 1 // deliberate byte wrap
	return seq
}

// ---------------------------------------------------------------------------
// Decoding
// ---------------------------------------------------------------------------

// DecodeDatagram decodes the single frame at the start of b.
//
// For UDP, which is how a ground station connects, this is the normal path: a
// datagram either contains a frame starting at byte zero or is not ours, so
// there is nothing to resynchronise and a short buffer means a mangled packet
// rather than "more to come". Trailing bytes after the frame are ignored.
//
// If a peer packs several frames into one datagram -- a MAVLink router will --
// feed the datagram to a Parser instead.
func DecodeDatagram(b []byte) (*Frame, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("mavlink: empty datagram: %w", ErrTruncated)
	}
	if b[0] != MagicV2 && b[0] != MagicV1 {
		return nil, fmt.Errorf("mavlink: datagram starts with 0x%02x: %w", b[0], ErrBadMagic)
	}
	f, _, err := decodeFrame(b)
	return f, err
}

// decodeFrame decodes the frame at b[0], which the caller must already have
// confirmed is a magic byte.
//
// It returns the number of bytes the caller should consume EVEN WHEN it returns
// an error, which is what lets a stream parser resynchronise; see Parser.Next
// for the two different resync policies and why they differ.
func decodeFrame(b []byte) (*Frame, int, error) {
	f := &Frame{}
	var hdrLen int

	switch b[0] {
	case MagicV2:
		if len(b) < headerLenV2 {
			return nil, 0, fmt.Errorf("mavlink: %d bytes, need a %d-byte v2 header: %w",
				len(b), headerLenV2, ErrTruncated)
		}
		f.Version = 2
		f.IncompatFlags = b[2]
		f.CompatFlags = b[3]
		f.Seq, f.SysID, f.CompID = b[4], b[5], b[6]
		f.ID = MsgID(uint32(b[7]) | uint32(b[8])<<8 | uint32(b[9])<<16)
		f.Signed = f.IncompatFlags&flagSigned != 0
		hdrLen = headerLenV2
	case MagicV1:
		// v1 is accepted but never produced. A ground station that has been told
		// to talk to a legacy autopilot, or an old mavproxy in the path, will
		// downgrade; refusing those frames would turn a cosmetic configuration
		// mistake into "the bridge is dead". The cost is small: v1 is the same
		// layout minus the two flag bytes and with a one-byte msgid.
		if len(b) < headerLenV1 {
			return nil, 0, fmt.Errorf("mavlink: %d bytes, need a %d-byte v1 header: %w",
				len(b), headerLenV1, ErrTruncated)
		}
		f.Version = 1
		f.Seq, f.SysID, f.CompID = b[2], b[3], b[4]
		f.ID = MsgID(b[5])
		hdrLen = headerLenV1
	default:
		return nil, 1, fmt.Errorf("mavlink: byte 0x%02x: %w", b[0], ErrBadMagic)
	}

	payLen := int(b[1])
	total := hdrLen + payLen + crcLen
	if f.Signed {
		total += signatureLen
	}
	if len(b) < total {
		return nil, 0, fmt.Errorf("mavlink: %s declared %d payload bytes, have %d of %d: %w",
			f.ID, payLen, len(b), total, ErrTruncated)
	}

	// The message must be identified BEFORE the checksum can be verified, because
	// verification needs its CRC_EXTRA byte. An unknown id is therefore
	// unverifiable: there is no way to tell a genuine newer message from garbage
	// that happened to begin with 0xFD.
	//
	// Only the HEADER is consumed, not the declared length. Trusting the length
	// of a frame that passed no check is how a parser swallows the good frame
	// sitting behind a corrupt one, which the resync tests demonstrate. The
	// MAVLink reference C parser makes the same choice: it looks the message up
	// at GOT_MSGID3 and, on a miss, drops straight back to IDLE in the middle of
	// the stream rather than skipping ahead. The cost is that a genuine unknown
	// message gets its payload rescanned for frame starts, which can produce a
	// spurious bad-CRC report but cannot produce a spurious frame.
	msg, err := NewMessage(f.ID)
	if err != nil {
		return nil, hdrLen, err
	}

	// The checksum sits immediately after the payload, which is NOT the same as
	// "at the end of the frame": a signed frame has 13 more bytes behind it.
	crcAt := hdrLen + payLen
	want := binary.LittleEndian.Uint16(b[crcAt:])
	if got := frameCRC(b[1:crcAt], msg.CRCExtra()); got != want {
		return nil, 1, fmt.Errorf("mavlink: %s crc 0x%04x, computed 0x%04x: %w",
			f.ID, want, got, ErrBadCRC)
	}

	// Unknown incompat_flags bits mean the frame layout itself may differ from
	// what was just parsed, so MAVLink requires the frame to be dropped. (A bit
	// in compat_flags, by contrast, may be ignored, which is the whole point of
	// having two flag bytes.)
	//
	// This check comes AFTER the checksum on purpose: the frame has now been
	// verified intact, so its declared length can be trusted and the whole frame
	// skipped in one step. Doing it before would mean consuming a length that
	// nothing had checked.
	if unknown := f.IncompatFlags & ^flagSigned; unknown != 0 {
		return nil, total, fmt.Errorf("mavlink: incompat_flags 0x%02x: %w",
			unknown, ErrUnsupportedIncompatFlags)
	}

	// Zero-extension, in one allocation that doubles as the payload copy.
	//
	// Needed for both framing versions, for different reasons: a v2 sender trims
	// trailing zeroes, and a v1 sender has never heard of extension fields and so
	// sends only the base ones. Either way the missing bytes are defined to be
	// zero. make() zeroes the whole backing array, so slicing up to cap gives the
	// extended view for free and Payload keeps the true wire length.
	//
	// The copy is not optional: on the stream path b is the parser's reusable
	// buffer, and on the datagram path it is the caller's reusable read buffer.
	// Handing out a sub-slice of either would mean a Frame whose contents change
	// under its owner on the next read -- the kind of bug that only shows up under
	// load.
	full := payLen
	if n := msg.PayloadLen(); n > full {
		full = n
	}
	pay := make([]byte, payLen, full)
	copy(pay, b[hdrLen:hdrLen+payLen])
	if err := msg.Unmarshal(pay[:full]); err != nil {
		return nil, total, fmt.Errorf("mavlink: decode %s: %w", f.ID, err)
	}
	f.Payload = pay
	f.Msg = msg
	return f, total, nil
}

// Stats counts what a Parser has seen. Copied out by value; see Parser.Stats.
type Stats struct {
	// Frames is the number of frames decoded successfully.
	Frames uint64
	// GarbageBytes is the number of bytes discarded while looking for a frame
	// boundary. Non-zero after a resync, and a good health signal: a link that
	// keeps accumulating garbage is not merely lossy, it is carrying something
	// that is not MAVLink.
	GarbageBytes uint64
	// BadCRC frames failed their checksum. Note that a resync normally produces
	// one or two of these before it succeeds, so a small count is not alarming.
	BadCRC uint64
	// Unknown frames were well-formed but carried an unimplemented message id.
	Unknown uint64
}

// Parser turns a byte stream into frames, resynchronising after corruption.
//
// # Concurrency
//
// NOT safe for concurrent use, and carries no mutex, for the same reason
// protocol.Decoder does not: exactly one goroutine owns the read side of a link.
// A second caller would get frames in an arbitrary interleaving with no way to
// tell which link they came from, and leaving the type unsynchronised means the
// race detector reports that design error on the first test run.
//
// # No io.Reader, no deadlines
//
// Parser is fed bytes rather than reading them, so this package never has to
// decide a timeout policy and every test here runs without a socket. The caller
// reads with a deadline -- see the deadline warning on protocol.Decoder, which
// applies verbatim -- and feeds what it got.
//
// # Buffering contract
//
// Call Next repeatedly until it returns ErrNeedMore before the next Feed. Under
// that contract at most MaxFrameLen-1 bytes are ever retained. A caller that
// feeds without draining grows the buffer without bound, which is why the
// contract is stated here rather than defended with a silent byte-dropping
// policy that would desynchronise the stream it was meant to protect.
type Parser struct {
	// buf holds unconsumed bytes from off onward. Both are owned by the single
	// calling goroutine; no synchronisation.
	buf   []byte
	off   int
	stats Stats
}

// Feed appends bytes read from the link. The slice is copied, so the caller may
// reuse its read buffer immediately.
func (p *Parser) Feed(b []byte) {
	// Compact before growing, so that a long-lived link does not accumulate a
	// buffer made entirely of already-consumed bytes.
	if p.off > 0 {
		n := copy(p.buf, p.buf[p.off:])
		p.buf = p.buf[:n]
		p.off = 0
	}
	p.buf = append(p.buf, b...)
}

// Buffered reports how many fed bytes are not yet consumed.
func (p *Parser) Buffered() int { return len(p.buf) - p.off }

// Stats returns a snapshot of the parser's counters.
func (p *Parser) Stats() Stats { return p.stats }

// Reset discards all buffered bytes, keeping the counters. Use it when the
// underlying link is re-established: the tail of a dead connection's stream can
// only ever desynchronise the new one.
func (p *Parser) Reset() {
	p.buf = p.buf[:0]
	p.off = 0
}

// Next returns the next frame.
//
// It returns ErrNeedMore -- a normal signal, not a failure -- when no complete
// frame is buffered. Any other error concerns ONE frame, which has already been
// skipped: the parser stays usable and the caller should log, count, and call
// Next again. That is the property a bridge needs. A reader goroutine that exits
// on a bad packet hands anything that can send a UDP datagram to port 14550 the
// ability to disconnect the swarm from its ground station.
//
// # Resynchronisation
//
// Finding a frame boundary in a damaged stream is guesswork, because 0xFD is
// just a byte and appears inside payloads constantly. The policy, which differs
// by failure:
//
//   - Bytes before the first magic byte are garbage. Dropped and counted.
//   - A bad CRC means this probably was not a frame header at all, so only ONE
//     byte is dropped and the search resumes immediately after it. Trusting the
//     length field of a frame that failed its checksum would risk swallowing the
//     real frame that follows.
//   - An unknown message id passed no checksum either, so only its header is
//     consumed and the scan resumes inside what it claimed was its payload. See
//     decodeFrame.
func (p *Parser) Next() (*Frame, error) {
	for {
		b := p.buf[p.off:]

		i := indexMagic(b)
		if i < 0 {
			// Nothing in the buffer can begin a frame. Drop all of it -- except,
			// and this is the subtle part, nothing is lost by dropping bytes that
			// contain no magic byte at all.
			p.stats.GarbageBytes += uint64(len(b))
			p.off = len(p.buf)
			return nil, ErrNeedMore
		}
		if i > 0 {
			p.stats.GarbageBytes += uint64(i)
			p.off += i
			b = b[i:]
		}

		f, consumed, err := decodeFrame(b)
		switch {
		case err == nil:
			p.off += consumed
			p.stats.Frames++
			return f, nil
		case consumed == 0:
			// ErrTruncated: the frame's own header says the rest has not arrived.
			// Keep the bytes and wait. Bounded by MaxFrameLen, so this cannot grow.
			return nil, ErrNeedMore
		default:
			if errors.Is(err, ErrBadCRC) {
				p.stats.BadCRC++
			} else {
				p.stats.Unknown++
			}
			// The offending bytes are consumed before returning, so the caller's
			// next call resumes after them rather than re-reporting the same
			// frame forever.
			p.off += consumed
			return nil, err
		}
	}
}

// indexMagic finds the first byte that could start a frame, or -1.
//
// Both markers are searched for in one pass rather than preferring v2, because
// scanning for 0xFD first and 0xFE second would skip past a v1 frame that sits
// in front of a byte that merely looks like a v2 start.
func indexMagic(b []byte) int {
	for i, c := range b {
		if c == MagicV2 || c == MagicV1 {
			return i
		}
	}
	return -1
}
