package mavlink

import (
	"errors"
	"fmt"
)

// MsgID is a MAVLink message id: the 24-bit number in a v2 frame header.
//
// It is a defined type rather than a bare uint32 so that a message id cannot be
// passed where a sysid, a sequence number or a payload length is wanted. Those
// are all small integers and the compiler is the only thing that will ever
// notice them being swapped.
type MsgID uint32

// String names the message when this build knows it, and otherwise says so
// rather than pretending. An error mentioning MSG_283 is a lead; one mentioning
// "message 283" reads like a corrupted number.
func (id MsgID) String() string {
	if name, ok := messageNames[id]; ok {
		return name
	}
	return fmt.Sprintf("MSG_%d", uint32(id))
}

// Message is implemented by every generated message type.
//
// All methods except Unmarshal are constant for a given type and are defined on
// the pointer receiver with no field access, so they cost nothing; they exist as
// methods rather than as lookups in a table because that is what lets the frame
// encoder be written once, against the interface, instead of once per message.
type Message interface {
	// MsgID is the message's wire id.
	MsgID() MsgID

	// MsgName is the MAVLink name, e.g. "GLOBAL_POSITION_INT". Diagnostics only.
	MsgName() string

	// CRCExtra is the message-definition checksum byte. It is mixed into every
	// frame's CRC and never transmitted; see frame.go.
	CRCExtra() byte

	// PayloadLen is the full, untruncated payload length in bytes, extensions
	// included. A v2 sender may transmit fewer bytes than this.
	PayloadLen() int

	// Marshal appends the little-endian wire payload to dst and returns the
	// extended slice. It writes exactly PayloadLen bytes and never fails.
	Marshal(dst []byte) []byte

	// Unmarshal decodes a payload of at least PayloadLen bytes. Shorter input is
	// an error: zero-extending a truncated payload is the frame decoder's job,
	// because only it can tell truncation from corruption.
	Unmarshal(payload []byte) error
}

// Sentinel errors. The classification matters more than the text: a bridge that
// treats all of these the same either dies on a malformed packet or silently
// accepts a corrupt one.
var (
	// ErrShortPayload means a payload was smaller than the message requires. From
	// Unmarshal it is a caller error; from the frame decoder it cannot happen,
	// because the decoder zero-extends first.
	ErrShortPayload = errors.New("mavlink: payload shorter than message")

	// ErrUnknownMsgID means the frame is well-formed but carries a message this
	// build does not implement.
	//
	// This is the common case in real life, not an exotic one: a GCS emits
	// messages for hardware the swarm does not have, and an autopilot dialect
	// grows between releases. It is therefore a *recoverable* error -- the parser
	// skips the frame and carries on. Note that an unknown id also means the
	// frame's CRC could not be verified at all, since verification needs the
	// message's CRC_EXTRA byte, which only a known definition supplies.
	ErrUnknownMsgID = errors.New("mavlink: unknown message id")

	// ErrBadCRC means the checksum did not match. Either the bytes were corrupted
	// or -- far more likely on a healthy link -- what looked like a frame header
	// was actually payload bytes that happened to begin with 0xFD.
	ErrBadCRC = errors.New("mavlink: checksum mismatch")

	// ErrBadMagic means the first byte was not a MAVLink start-of-frame marker.
	ErrBadMagic = errors.New("mavlink: not a MAVLink frame")

	// ErrTruncated means the buffer ended before the frame the header declared.
	// On a datagram that is a malformed packet; on a stream it just means the
	// rest has not arrived yet, which is why Parser translates it to ErrNeedMore.
	ErrTruncated = errors.New("mavlink: truncated frame")

	// ErrUnsupportedIncompatFlags means the sender set an incompat_flags bit this
	// build does not understand.
	//
	// The MAVLink rule for incompat_flags is strict and worth stating: a receiver
	// that does not understand a bit MUST drop the frame, because the bit may
	// change the frame layout itself. compat_flags, by contrast, may be ignored.
	// The signing bit is understood here -- the signature is skipped, see frame.go.
	ErrUnsupportedIncompatFlags = errors.New("mavlink: unsupported incompat_flags")

	// ErrNeedMore means the parser has no complete frame buffered. It is a normal
	// control-flow signal, not a failure.
	ErrNeedMore = errors.New("mavlink: need more bytes")
)

// IsMalformed reports whether err means "that packet was rubbish, carry on",
// as opposed to something the caller has to act on.
//
// The bridge calls this rather than switching on errors individually, because
// the whole point is a single, auditable policy: a malformed packet increments a
// counter and is dropped. It must never kill the reader goroutine, and it must
// never be retried -- there is nothing to retry, the bytes are already gone. A
// GCS on a lossy link, or anything else sharing UDP port 14550, will produce
// these in the ordinary course of events.
func IsMalformed(err error) bool {
	return errors.Is(err, ErrBadCRC) ||
		errors.Is(err, ErrBadMagic) ||
		errors.Is(err, ErrTruncated) ||
		errors.Is(err, ErrUnknownMsgID) ||
		errors.Is(err, ErrUnsupportedIncompatFlags) ||
		errors.Is(err, ErrShortPayload)
}

// NewMessage returns an empty message of the given id.
//
// It returns an error rather than nil for an unknown id so that the caller
// cannot accidentally dereference its way to a panic on the first malformed
// packet of the day.
func NewMessage(id MsgID) (Message, error) {
	factory, ok := messageFactory[id]
	if !ok {
		return nil, fmt.Errorf("mavlink: msgid %d: %w", uint32(id), ErrUnknownMsgID)
	}
	return factory(), nil
}

// Known reports whether this build implements the message id.
func Known(id MsgID) bool {
	_, ok := messageFactory[id]
	return ok
}

// ---------------------------------------------------------------------------
// Char array helpers
// ---------------------------------------------------------------------------

// Chars decodes a MAVLink char array into a string.
//
// MAVLink char arrays are fixed-size and NOT necessarily NUL-terminated: a
// string that exactly fills the array has no terminator at all, which is why
// PARAM_VALUE can carry a 16-character parameter name in char[16]. The rule is
// "up to the first NUL, or the whole array", and getting it wrong shows up as a
// parameter name with a trailing nul glyph in the ground station's table.
func Chars(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// SetChars writes s into a fixed-size MAVLink char array, zero-padding the
// remainder and truncating anything that does not fit.
//
// Truncation is silent and deliberate. The alternative -- an error -- would
// force every call site that formats a STATUSTEXT to handle a failure it cannot
// do anything about, and a clipped status message is strictly better than a
// dropped one.
func SetChars(dst []byte, s string) {
	n := copy(dst, s)
	for i := n; i < len(dst); i++ {
		dst[i] = 0
	}
}
