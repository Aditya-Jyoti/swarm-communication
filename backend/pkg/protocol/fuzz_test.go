package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// ---------------------------------------------------------------------------
// Fuzzing the decoder
// ---------------------------------------------------------------------------
//
// ReadFrame is the only function in the repository that parses bytes it did not
// produce. Everything else in the swarm consumes an *Envelope that ReadFrame has
// already vouched for, so every hostile or merely corrupt input the process will
// ever see arrives through this one door. The unit tests cover the failure modes we
// thought of; the fuzzer covers the ones we did not, which is the entire point.
//
// The properties asserted, in order of importance:
//
//  1. No input panics. A panic in a decode loop takes the node down, and a node
//     that can be killed by four crafted bytes is worse than a node that is merely
//     slow -- the failure detector is built to survive death, not to survive a
//     peer that can induce death remotely.
//  2. Every error is classifiable. pkg/network's failure handling branches three
//     ways (clean close, death mid-frame, peer talking nonsense); a fourth,
//     unclassified error category would fall through every branch.
//  3. The bounds check precedes allocation. Asserted per frame, not just once.
//  4. Encoder and Decoder agree. Anything that decodes must survive being put back
//     on the wire, because that is precisely what a relaying node does with it.

// countingReader records how many bytes the decoder actually pulled, which is what
// makes the "rejected without allocating" claim measurable rather than asserted.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// maxFramesPerInput bounds the decode loop. Each iteration consumes at least a
// 4-byte header so the loop terminates on its own, but a cap keeps a pathological
// corpus entry from dominating the fuzzing budget.
const maxFramesPerInput = 64

func FuzzDecodeFrame(f *testing.F) {
	// Seed corpus. The fuzzer mutates these, so each one is chosen to put the
	// decoder in a different state for the mutation to work from.

	// A valid encoded envelope: the only seed that reaches the JSON decoder and the
	// round-trip assertion, and the base for mutations that corrupt a good frame.
	var valid bytes.Buffer
	env, err := NewEnvelope(TypeHeartbeat, "node-1", "node-2",
		HeartbeatPayload{Seq: 7, Term: 2, LeaderID: "node-1", ClusterSize: 3})
	if err != nil {
		f.Fatal(err)
	}
	if err := NewEncoder(&valid).WriteEnvelope(env); err != nil {
		f.Fatal(err)
	}
	f.Add(valid.Bytes())

	// Two frames back to back, so mutations can desynchronise the stream at a frame
	// boundary rather than only at its start.
	f.Add(append(append([]byte(nil), valid.Bytes()...), valid.Bytes()...))

	f.Add([]byte(nil))                               // empty input: clean EOF
	f.Add([]byte{0x00, 0x01})                        // truncated header
	f.Add(frameBytes(16, nil))                       // header only, body never arrives
	f.Add(frameBytes(0, nil))                        // zero-length frame
	f.Add(frameBytes(3<<30, nil))                    // oversize length prefix
	f.Add(frameBytes(MaxFrameSize+1, nil))           // oversize by one byte
	f.Add(frameBytes(5, []byte(`{"ve`)))             // valid length, invalid JSON
	f.Add(frameBytes(2, []byte(`{}`)))               // valid JSON, version 0
	f.Add(frameBytes(15, []byte(`{"version":99} `))) // decodes, wrong version

	f.Fuzz(func(t *testing.T, data []byte) {
		cr := &countingReader{r: bytes.NewReader(data)}
		dec := NewDecoder(cr)

		for i := 0; i < maxFramesPerInput; i++ {
			readBefore := cr.n
			capBefore := cap(dec.buf)

			env, err := dec.ReadFrame()
			if err != nil {
				assertClassifiable(t, err)

				// THE ALLOCATION GUARD. An oversize declaration must cost exactly one
				// header read and nothing else: no payload bytes consumed, and no
				// growth of the scratch buffer. If these two ever stop holding, the
				// bounds check has been moved below the allocation and a peer can
				// name a number that OOM-kills the container.
				if errors.Is(err, ErrFrameTooLarge) {
					if got := cr.n - readBefore; got != headerSize {
						t.Fatalf("oversize frame consumed %d bytes, want %d (header only): "+
							"the MaxFrameSize check must precede any payload read", got, headerSize)
					}
					if cap(dec.buf) != capBefore {
						t.Fatalf("oversize frame grew the scratch buffer %d -> %d; "+
							"the MaxFrameSize check must precede any payload allocation",
							capBefore, cap(dec.buf))
					}
				}
				return
			}

			if env == nil {
				t.Fatal("ReadFrame returned a nil envelope and a nil error")
			}
			assertReEncodes(t, env)
		}
	})
}

// assertClassifiable pins the error taxonomy ReadFrame promises: an error is
// exactly one of a clean close, a death mid-frame, or a protocol violation.
//
// "Exactly one" is the strong form and it is what the code actually guarantees.
// The mid-frame path deliberately normalises io.EOF to io.ErrUnexpectedEOF, so the
// two EOF classes are disjoint; the sentinels are plain errors.New values that wrap
// nothing, so a violation can never also be an EOF. If any of that changes, the
// three-way branch in pkg/network silently grows a fall-through case, and this is
// the assertion that catches it first.
func assertClassifiable(t *testing.T, err error) {
	t.Helper()

	cleanClose := errors.Is(err, io.EOF)
	diedMidFrame := errors.Is(err, io.ErrUnexpectedEOF)
	violation := IsProtocolViolation(err)

	n := 0
	for _, b := range []bool{cleanClose, diedMidFrame, violation} {
		if b {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("error %v (%T) matched %d of the 3 classes "+
			"(io.EOF=%v io.ErrUnexpectedEOF=%v protocolViolation=%v); "+
			"exactly one must hold or the failure handling in pkg/network has no branch for it",
			err, err, n, cleanClose, diedMidFrame, violation)
	}
}

// assertReEncodes pushes a successfully decoded envelope back through the Encoder
// and decodes it again. This is the asymmetry check: a relaying node re-encodes
// what it received, so anything the Decoder accepts but the Encoder mangles is a
// message that mutates as it crosses the swarm.
func assertReEncodes(t *testing.T, first *Envelope) {
	t.Helper()

	second, ok := reframe(t, first)
	if !ok {
		return
	}

	if second.Version != first.Version || second.Type != first.Type ||
		second.From != first.From || second.To != first.To ||
		second.ID != first.ID || second.SentAtUnixNano != first.SentAtUnixNano {
		t.Fatalf("envelope changed across a re-encode:\n first:  %+v\n second: %+v", first, second)
	}

	// The payload is compared with one documented allowance. encoding/json escapes
	// '<', '>', '&' and U+2028/U+2029 when it marshals, including inside a
	// json.RawMessage, and it strips insignificant whitespace. Those rewrites are
	// the standard library's behaviour and not an asymmetry in this codec, so the
	// byte-for-byte comparison is made only when neither applies -- after
	// compacting, which accounts for the whitespace.
	if !escapedByEncodingJSON(first.Payload) {
		var want bytes.Buffer
		if len(first.Payload) > 0 {
			if err := json.Compact(&want, first.Payload); err != nil {
				t.Fatalf("payload that just decoded is not valid JSON: %v", err)
			}
		}
		if !bytes.Equal(want.Bytes(), second.Payload) {
			t.Fatalf("payload changed across a re-encode:\n want: %s\n got:  %s",
				want.Bytes(), second.Payload)
		}
	}

	// Fixed point: whatever the first re-encode normalised, the second must not
	// change again. This catches the payload rewrites the allowance above skips --
	// an escape that is not idempotent would show up here as drift, byte for byte.
	third, ok := reframe(t, second)
	if !ok {
		return
	}
	if third.Version != second.Version || third.Type != second.Type ||
		third.From != second.From || third.To != second.To ||
		third.ID != second.ID || third.SentAtUnixNano != second.SentAtUnixNano ||
		!bytes.Equal(third.Payload, second.Payload) {
		t.Fatalf("re-encoding is not a fixed point:\n second: %+v\n third:  %+v", second, third)
	}
}

// reframe writes env as a frame and reads it straight back. It reports false when
// the envelope legitimately cannot be re-encoded, which happens only at the size
// boundary: escaping can expand a body that arrived at exactly MaxFrameSize past
// the limit. That is the Encoder refusing to emit something illegal, not a defect.
func reframe(t *testing.T, env *Envelope) (*Envelope, bool) {
	t.Helper()

	var buf bytes.Buffer
	if err := NewEncoder(&buf).WriteEnvelope(env); err != nil {
		if errors.Is(err, ErrFrameTooLarge) {
			return nil, false
		}
		t.Fatalf("a decoded envelope failed to re-encode: %v (%+v)", err, env)
	}
	out, err := NewDecoder(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("an encoder-produced frame failed to decode: %v (%+v)", err, env)
	}
	return out, true
}

// escapedByEncodingJSON reports whether encoding/json's marshaller would rewrite
// any byte of b. See the comment in assertReEncodes.
func escapedByEncodingJSON(b []byte) bool {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '<', '>', '&':
			return true
		case 0xE2:
			// U+2028 (e2 80 a8) and U+2029 (e2 80 a9), the two line terminators
			// encoding/json escapes because they are statement separators in
			// JavaScript and would break a JSONP-style consumer.
			if i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xA8 || b[i+2] == 0xA9) {
				return true
			}
		}
	}
	return false
}
