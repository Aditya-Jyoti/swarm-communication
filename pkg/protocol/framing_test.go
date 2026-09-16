package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustEnvelope(t *testing.T, typ MessageType, from, to NodeID, payload any) *Envelope {
	t.Helper()
	env, err := NewEnvelope(typ, from, to, payload)
	if err != nil {
		t.Fatalf("NewEnvelope(%s): %v", typ, err)
	}
	return env
}

// frameBytes builds a raw frame by hand, bypassing the Encoder, so that tests can
// construct wire sequences the Encoder would refuse to emit.
func frameBytes(length uint32, body []byte) []byte {
	var hdr [headerSize]byte
	binary.BigEndian.PutUint32(hdr[:], length)
	return append(hdr[:], body...)
}

// dripReader returns at most n bytes per Read. With n == 1 it is the meanest
// legal io.Reader: every single Read call splits whatever the caller asked for.
type dripReader struct {
	data []byte
	pos  int
	n    int
}

func (d *dripReader) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	n := d.n
	if n > len(p) {
		n = len(p)
	}
	if rem := len(d.data) - d.pos; n > rem {
		n = rem
	}
	copy(p, d.data[d.pos:d.pos+n])
	d.pos += n
	return n, nil
}

// chunkReader hands out a caller-specified, deliberately pathological sequence of
// chunk sizes — chosen in the tests to straddle header/payload boundaries.
type chunkReader struct {
	data   []byte
	pos    int
	sizes  []int
	sizeIx int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := 8
	if c.sizeIx < len(c.sizes) {
		n = c.sizes[c.sizeIx]
		c.sizeIx++
	}
	if n < 1 {
		n = 1
	}
	if n > len(p) {
		n = len(p)
	}
	if rem := len(c.data) - c.pos; n > rem {
		n = rem
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

// headerOnlyReader yields exactly one length header and then fails the test if it
// is read again. It is how the "oversize frames are rejected without touching the
// payload" claim is made checkable rather than merely asserted in a comment: if
// ReadFrame ever allocated for, or attempted to consume, a declared 3 GiB body,
// this reader would be read a second time and the test would fail.
type headerOnlyReader struct {
	t    *testing.T
	hdr  [headerSize]byte
	done bool
}

func (h *headerOnlyReader) Read(p []byte) (int, error) {
	if h.done {
		h.t.Error("decoder attempted to read the payload of an oversize frame; " +
			"the MaxFrameSize check must happen BEFORE any payload is consumed or allocated")
		return 0, io.EOF
	}
	h.done = true
	return copy(p, h.hdr[:]), nil
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     MessageType
		payload any
	}{
		{"hello", TypeHello, HelloPayload{Advertise: "node-3:7946", Incarnation: 42, KnownPeers: []NodeAddress{"node-1:7946"}}},
		{"ping", TypePing, PingPayload{Nonce: 0xDEADBEEF, Seq: 7}},
		{"pong", TypePong, PongPayload{Nonce: 0xDEADBEEF, Seq: 7}},
		{"heartbeat", TypeHeartbeat, HeartbeatPayload{Seq: 99, Term: 3, LeaderID: "node-1", ClusterSize: 4}},
		{"no payload", TypeLeave, nil},
	}

	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	sent := make([]*Envelope, 0, len(cases))
	for _, c := range cases {
		env := mustEnvelope(t, c.typ, "node-3", "node-1", c.payload)
		if err := enc.WriteEnvelope(env); err != nil {
			t.Fatalf("%s: WriteEnvelope: %v", c.name, err)
		}
		sent = append(sent, env)
	}

	dec := NewDecoder(&buf)
	for i, want := range sent {
		got, err := dec.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: ReadFrame: %v", i, err)
		}
		if got.Type != want.Type || got.From != want.From || got.To != want.To || got.ID != want.ID {
			t.Errorf("frame %d: header mismatch: got %+v want %+v", i, got, want)
		}
		if got.Version != CurrentVersion {
			t.Errorf("frame %d: version %d want %d", i, got.Version, CurrentVersion)
		}
		if got.SentAtUnixNano != want.SentAtUnixNano {
			t.Errorf("frame %d: timestamp mismatch", i)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("frame %d: payload %q want %q", i, got.Payload, want.Payload)
		}
	}
	if _, err := dec.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("after last frame: got %v, want io.EOF", err)
	}
}

// ---------------------------------------------------------------------------
// Partial reads — the headline property.
// ---------------------------------------------------------------------------

// TestPartialReadsOneByteAtATime is the test the entire wire format exists to
// pass. TCP is a byte stream with no message boundaries: a Read returns whatever
// happened to have arrived, which may be two bytes of a four-byte length header,
// or one frame and a third of the next. Every naive "conn.Read(buf); unmarshal"
// implementation passes on loopback with small messages and then corrupts every
// message in production. Feeding the decoder one byte per Read simulates the worst
// legal case and proves the io.ReadFull loops are doing their job.
func TestPartialReadsOneByteAtATime(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	const n = 12
	for i := 0; i < n; i++ {
		env := mustEnvelope(t, TypeHeartbeat, "node-1", "node-2",
			HeartbeatPayload{Seq: uint64(i), Term: 1, LeaderID: "node-1", ClusterSize: n})
		if err := enc.WriteEnvelope(env); err != nil {
			t.Fatalf("WriteEnvelope: %v", err)
		}
	}

	dec := NewDecoder(&dripReader{data: buf.Bytes(), n: 1})
	for i := 0; i < n; i++ {
		env, err := dec.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		hb, err := PayloadOf[HeartbeatPayload](env)
		if err != nil {
			t.Fatalf("frame %d payload: %v", i, err)
		}
		if hb.Seq != uint64(i) {
			t.Errorf("frame %d: Seq = %d, want %d", i, hb.Seq, i)
		}
	}
	if _, err := dec.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("got %v, want io.EOF", err)
	}
}

// TestPartialReadsStraddlingBoundaries uses chunk sizes chosen to split reads
// across the header/payload seam and across the seam between two frames — the
// specific alignments where an off-by-one in the framing logic hides.
func TestPartialReadsStraddlingBoundaries(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	const n = 6
	for i := 0; i < n; i++ {
		env := mustEnvelope(t, TypePing, "node-1", "node-2", PingPayload{Nonce: uint64(i) + 1, Seq: uint64(i)})
		if err := enc.WriteEnvelope(env); err != nil {
			t.Fatalf("WriteEnvelope: %v", err)
		}
	}

	// 2 splits the header; 3 leaves 1 header byte plus payload; the large sizes
	// span a frame boundary so one Read delivers the tail of one frame and the
	// head of the next.
	sizes := []int{2, 3, 200, 1, 7, 1, 150, 4, 5, 300, 2}

	dec := NewDecoder(&chunkReader{data: buf.Bytes(), sizes: sizes})
	for i := 0; i < n; i++ {
		env, err := dec.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		p, err := PayloadOf[PingPayload](env)
		if err != nil {
			t.Fatalf("frame %d payload: %v", i, err)
		}
		if p.Seq != uint64(i) || p.Nonce != uint64(i)+1 {
			t.Errorf("frame %d: got %+v", i, p)
		}
	}
}

// ---------------------------------------------------------------------------
// Malformed input
// ---------------------------------------------------------------------------

func TestOversizeFrameRejectedBeforeAllocating(t *testing.T) {
	r := &headerOnlyReader{t: t}
	// Declare 3 GiB. If the bounds check were performed after allocation, this test
	// would either OOM the test binary or block forever waiting for the body.
	binary.BigEndian.PutUint32(r.hdr[:], 3<<30)

	dec := NewDecoder(r)
	_, err := dec.ReadFrame()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
	if !IsProtocolViolation(err) {
		t.Errorf("ErrFrameTooLarge should classify as a protocol violation")
	}
	if dec.buf != nil {
		t.Errorf("payload scratch buffer was allocated (%d bytes) for a rejected frame", cap(dec.buf))
	}
}

func TestOversizeByOneByteRejected(t *testing.T) {
	// Exactly at the boundary: MaxFrameSize is legal, MaxFrameSize+1 is not.
	dec := NewDecoder(bytes.NewReader(frameBytes(MaxFrameSize+1, nil)))
	if _, err := dec.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}

func TestEncoderRejectsOversizeEnvelope(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	env := mustEnvelope(t, TypeTelemetry, "node-1", "", map[string]string{
		"blob": strings.Repeat("x", MaxFrameSize+1),
	})
	if err := enc.WriteEnvelope(env); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
	// The stream must be untouched: a locally-rejected envelope is our bug, not the
	// peer's, and it must not desynchronise a connection that is otherwise fine.
	if buf.Len() != 0 {
		t.Errorf("encoder wrote %d bytes for a rejected envelope; the stream must be left clean", buf.Len())
	}
}

func TestZeroLengthFrameRejected(t *testing.T) {
	dec := NewDecoder(bytes.NewReader(frameBytes(0, nil)))
	_, err := dec.ReadFrame()
	if !errors.Is(err, ErrZeroLengthFrame) {
		t.Fatalf("got %v, want ErrZeroLengthFrame", err)
	}
	if !IsProtocolViolation(err) {
		t.Errorf("ErrZeroLengthFrame should classify as a protocol violation")
	}
}

// TestTruncatedPayloadIsUnexpectedEOF pins the distinction that drives failover:
// a peer that stops mid-frame died, and must be treated differently from a peer
// that closed on a frame boundary.
func TestTruncatedPayloadIsUnexpectedEOF(t *testing.T) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).WriteEnvelope(mustEnvelope(t, TypePing, "a", "b", PingPayload{Nonce: 1})); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()

	for _, cut := range []int{headerSize, headerSize + 1, len(full) - 1} {
		dec := NewDecoder(bytes.NewReader(full[:cut]))
		_, err := dec.ReadFrame()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("cut at %d: got %v, want io.ErrUnexpectedEOF", cut, err)
		}
		if errors.Is(err, io.EOF) {
			t.Errorf("cut at %d: a death mid-frame must not be reported as a clean close", cut)
		}
	}
}

func TestTruncatedHeaderIsUnexpectedEOF(t *testing.T) {
	dec := NewDecoder(bytes.NewReader([]byte{0x00, 0x00}))
	if _, err := dec.ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestCleanEOFAtFrameBoundary(t *testing.T) {
	dec := NewDecoder(bytes.NewReader(nil))
	_, err := dec.ReadFrame()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want io.EOF", err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a clean close must not be reported as a mid-frame death")
	}
	if IsProtocolViolation(err) {
		t.Errorf("io.EOF is not a protocol violation")
	}
}

func TestMalformedJSONIsProtocolViolation(t *testing.T) {
	body := []byte(`{"version":1,"type":`)
	dec := NewDecoder(bytes.NewReader(frameBytes(uint32(len(body)), body)))
	_, err := dec.ReadFrame()
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("got %v, want ErrMalformedFrame", err)
	}
	if !IsProtocolViolation(err) {
		t.Errorf("malformed JSON should classify as a protocol violation")
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	body, err := json.Marshal(&Envelope{Version: 99, Type: TypePing, From: "node-9"})
	if err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(bytes.NewReader(frameBytes(uint32(len(body)), body)))
	_, err = dec.ReadFrame()
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

// TestUnknownTypeDecodesButIsFlagged is the forward-compatibility contract: a
// newer node sending a message this build has never heard of must not kill the
// connection. The frame decodes, the stream stays on a boundary, and the caller is
// told the type is unknown via Valid().
func TestUnknownTypeDecodesButIsFlagged(t *testing.T) {
	body, err := json.Marshal(&Envelope{
		Version: CurrentVersion,
		Type:    MessageType("QUANTUM_ENTANGLE"),
		From:    "node-from-the-future",
		Payload: json.RawMessage(`{"spin":"up"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Follow it with a frame we DO understand, to prove the stream survived.
	var buf bytes.Buffer
	buf.Write(frameBytes(uint32(len(body)), body))
	if err := NewEncoder(&buf).WriteEnvelope(mustEnvelope(t, TypePing, "a", "b", PingPayload{Nonce: 5})); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(&buf)
	env, err := dec.ReadFrame()
	if err != nil {
		t.Fatalf("an unknown message type must not be a decode error, got: %v", err)
	}
	if env.Type.Valid() {
		t.Errorf("QUANTUM_ENTANGLE should not be Valid()")
	}
	if env.Type.IsControlPlane() || env.Type.IsDataPlane() {
		t.Errorf("an unknown type belongs to neither plane")
	}

	next, err := dec.ReadFrame()
	if err != nil {
		t.Fatalf("stream did not survive an unknown type: %v", err)
	}
	if next.Type != TypePing {
		t.Errorf("got %s, want PING", next.Type)
	}
}

// ---------------------------------------------------------------------------
// Buffer reuse and aliasing
// ---------------------------------------------------------------------------

// TestReadFramePayloadDoesNotAliasScratchBuffer verifies the claim the Decoder's
// buffer reuse rests on: the Payload handed to the caller must not point into
// d.buf, or the next ReadFrame would silently rewrite a message the caller is
// still holding. encoding/json copies into json.RawMessage, but that is another
// package's implementation detail, so it is tested rather than assumed.
func TestReadFramePayloadDoesNotAliasScratchBuffer(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for i := 0; i < 2; i++ {
		env := mustEnvelope(t, TypeHeartbeat, "node-1", "node-2",
			HeartbeatPayload{Seq: uint64(i), Term: 1, LeaderID: "node-1", ClusterSize: i})
		if err := enc.WriteEnvelope(env); err != nil {
			t.Fatal(err)
		}
	}

	dec := NewDecoder(&buf)
	first, err := dec.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	held := append([]byte(nil), first.Payload...)

	if _, err := dec.ReadFrame(); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(held, first.Payload) {
		t.Fatalf("first frame's payload was mutated by the second read:\n held: %s\n now:  %s", held, first.Payload)
	}
	hb, err := PayloadOf[HeartbeatPayload](first)
	if err != nil {
		t.Fatal(err)
	}
	if hb.Seq != 0 {
		t.Errorf("first frame Seq = %d after a second read, want 0", hb.Seq)
	}
}

func TestDecoderReusesBufferAcrossFrames(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for i := 0; i < 3; i++ {
		if err := enc.WriteEnvelope(mustEnvelope(t, TypePing, "a", "b", PingPayload{Seq: uint64(i)})); err != nil {
			t.Fatal(err)
		}
	}
	dec := NewDecoder(&buf)
	if _, err := dec.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	capAfterFirst := cap(dec.buf)
	for i := 0; i < 2; i++ {
		if _, err := dec.ReadFrame(); err != nil {
			t.Fatal(err)
		}
	}
	if cap(dec.buf) != capAfterFirst {
		t.Errorf("scratch buffer was reallocated for same-sized frames: %d -> %d", capAfterFirst, cap(dec.buf))
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// syncWriter is a writer that records each Write call's bytes separately, so the
// test can observe whether the encoder issued one Write per frame. Its own mutex
// protects only its records; it deliberately does NOT serialise the encoder's
// writes, because that is the property under test.
type syncWriter struct {
	mu     sync.Mutex
	writes [][]byte
}

func (s *syncWriter) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	s.mu.Lock()
	s.writes = append(s.writes, cp)
	s.mu.Unlock()
	return len(p), nil
}

func (s *syncWriter) all() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for _, w := range s.writes {
		out = append(out, w...)
	}
	return out
}

// TestConcurrentEncodersDoNotInterleave is the reason Encoder holds a mutex. In
// the real system a leader's heartbeat goroutine and its task-dispatch goroutine
// write to the same connection with no coordination between them. If a frame were
// not atomic, one frame's length header would be followed by another frame's body
// and the peer's stream would be permanently desynchronised — a failure that is
// invisible on loopback with small messages and catastrophic in production.
//
// Run under -race this also asserts the encoder has no unguarded shared state.
func TestConcurrentEncodersDoNotInterleave(t *testing.T) {
	const (
		writers        = 8
		framesPerWrite = 50
	)

	w := &syncWriter{}
	enc := NewEncoder(w)

	var wg sync.WaitGroup
	wg.Add(writers)
	for g := 0; g < writers; g++ {
		// Owner: this test function. Shutdown path: each goroutine runs a bounded
		// loop and returns; wg.Wait below joins every one of them before the test
		// reads any shared state. No goroutine can outlive the test.
		go func(g int) {
			defer wg.Done()
			for i := 0; i < framesPerWrite; i++ {
				env := mustEnvelopeNoT(TypeHeartbeat, NodeID("node-"+string(rune('a'+g))), "leader",
					HeartbeatPayload{Seq: uint64(i), Term: uint64(g), LeaderID: "leader", ClusterSize: g})
				if err := enc.WriteEnvelope(env); err != nil {
					t.Errorf("writer %d frame %d: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// Every frame must have been emitted by exactly one Write. More writes than
	// frames would mean a frame was split, which is the interleaving hazard.
	if got, want := len(w.writes), writers*framesPerWrite; got != want {
		t.Errorf("%d Write calls for %d frames; a frame must be one contiguous Write", got, want)
	}

	// And the concatenated stream must decode cleanly, frame for frame.
	dec := NewDecoder(bytes.NewReader(w.all()))
	seen := map[uint64]int{}
	for i := 0; i < writers*framesPerWrite; i++ {
		env, err := dec.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d of %d: %v", i, writers*framesPerWrite, err)
		}
		hb, err := PayloadOf[HeartbeatPayload](env)
		if err != nil {
			t.Fatalf("frame %d payload: %v", i, err)
		}
		seen[hb.Term]++
	}
	if _, err := dec.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("trailing bytes after the expected frames: %v", err)
	}
	for g := 0; g < writers; g++ {
		if seen[uint64(g)] != framesPerWrite {
			t.Errorf("writer %d: decoded %d frames, want %d", g, seen[uint64(g)], framesPerWrite)
		}
	}
}

// mustEnvelopeNoT is the t-free variant for use inside goroutines, where calling
// t.Fatalf would be a testing API violation.
func mustEnvelopeNoT(typ MessageType, from, to NodeID, payload any) *Envelope {
	env, err := NewEnvelope(typ, from, to, payload)
	if err != nil {
		panic(err)
	}
	return env
}

// ---------------------------------------------------------------------------
// DumpStream
// ---------------------------------------------------------------------------

func TestDumpStream(t *testing.T) {
	var wire bytes.Buffer
	enc := NewEncoder(&wire)
	if err := enc.WriteEnvelope(mustEnvelope(t, TypeHeartbeat, "node-1", "node-2",
		HeartbeatPayload{Seq: 5, Term: 2, LeaderID: "node-1", ClusterSize: 3})); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteEnvelope(mustEnvelope(t, TypeTelemetry, "node-1", "", map[string]int{"cpu": 42})); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := DumpStream(&wire, &out); err != nil {
		t.Fatalf("DumpStream: %v", err)
	}
	s := out.String()
	for _, want := range []string{"HEARTBEAT", "(control)", "TELEMETRY", "(data)", "node-1", "end of stream"} {
		if !strings.Contains(s, want) {
			t.Errorf("dump missing %q:\n%s", want, s)
		}
	}
}

func TestDumpStreamSurvivesUnparseableBody(t *testing.T) {
	body := []byte(`not json at all`)
	var wire bytes.Buffer
	wire.Write(frameBytes(uint32(len(body)), body))
	if err := NewEncoder(&wire).WriteEnvelope(mustEnvelope(t, TypePing, "a", "b", PingPayload{Nonce: 1})); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := DumpStream(&wire, &out); err != nil {
		t.Fatalf("DumpStream should continue past an unparseable body: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "UNPARSEABLE") {
		t.Errorf("expected the bad frame to be reported:\n%s", s)
	}
	if !strings.Contains(s, "PING") {
		t.Errorf("expected the following good frame to be dumped:\n%s", s)
	}
}

func TestDumpStreamStopsAtBadLength(t *testing.T) {
	var out bytes.Buffer
	err := DumpStream(bytes.NewReader(frameBytes(3<<30, nil)), &out)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}
