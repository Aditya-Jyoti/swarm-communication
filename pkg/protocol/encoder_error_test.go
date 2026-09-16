package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// failWriter fails every Write with a sentinel the test can identify, so we can
// assert the error is wrapped rather than swallowed or replaced.
type failWriter struct{ err error }

func (f failWriter) Write(p []byte) (int, error) { return 0, f.err }

// shortWriter accepts the first n bytes and then fails. It exists to document a
// property of io.Writer rather than of this package: a Writer that returns
// n < len(p) MUST return a non-nil error, which is what lets WriteEnvelope get
// away with a single Write and no partial-frame recovery path.
type shortWriter struct {
	n   int
	err error
}

func (s shortWriter) Write(p []byte) (int, error) {
	if len(p) > s.n {
		return s.n, s.err
	}
	return len(p), nil
}

func TestWriteEnvelopeNilIsRejected(t *testing.T) {
	enc := NewEncoder(&bytes.Buffer{})
	if err := enc.WriteEnvelope(nil); err == nil {
		t.Fatal("WriteEnvelope(nil) returned nil error")
	}
}

// A caller that builds an Envelope literal without setting Version must still
// produce a decodable frame. If the stamp were dropped, every hand-built envelope
// would fail the peer's version check and the connection would be dropped.
func TestWriteEnvelopeStampsMissingVersion(t *testing.T) {
	var buf bytes.Buffer
	env := &Envelope{Type: TypePing, From: "node-1"} // Version deliberately zero

	if err := NewEncoder(&buf).WriteEnvelope(env); err != nil {
		t.Fatalf("WriteEnvelope: %v", err)
	}
	if env.Version != CurrentVersion {
		t.Errorf("Version not stamped on the caller's envelope: got %d, want %d",
			env.Version, CurrentVersion)
	}

	got, err := NewDecoder(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame on a version-stamped frame: %v", err)
	}
	if got.Version != CurrentVersion {
		t.Errorf("decoded Version = %d, want %d", got.Version, CurrentVersion)
	}
}

// json.RawMessage marshals itself verbatim, so an Envelope carrying a malformed
// Payload fails at marshal time. The frame must never reach the writer: a peer
// that receives half an envelope has no way to resynchronise.
func TestWriteEnvelopeMarshalFailureLeavesStreamUntouched(t *testing.T) {
	var buf bytes.Buffer
	env := &Envelope{
		Version: CurrentVersion,
		Type:    TypeTask,
		Payload: []byte("{not json"),
	}

	err := NewEncoder(&buf).WriteEnvelope(env)
	if err == nil {
		t.Fatal("expected a marshal error for an invalid RawMessage payload")
	}
	if !strings.Contains(err.Error(), "marshal envelope") {
		t.Errorf("error does not identify the marshal stage: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("stream was written to despite a marshal failure: %d bytes", buf.Len())
	}
}

func TestWriteEnvelopeWrapsWriterError(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	enc := NewEncoder(failWriter{err: sentinel})

	err := enc.WriteEnvelope(mustEnvelope(t, TypeHeartbeat, "node-1", "node-2", nil))
	if err == nil {
		t.Fatal("expected the writer error to surface")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("writer error not wrapped with %%w: got %v", err)
	}
}

// A short Write is reported as an error by contract, so WriteEnvelope surfaces it
// rather than silently emitting a truncated frame.
func TestWriteEnvelopeSurfacesShortWrite(t *testing.T) {
	enc := NewEncoder(shortWriter{n: 2, err: io.ErrShortWrite})

	err := enc.WriteEnvelope(mustEnvelope(t, TypePing, "node-1", "node-2", PingPayload{Nonce: 1}))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("short write not surfaced: got %v", err)
	}
}

// An oversize envelope is our bug, not the peer's, so it must fail locally and
// leave the stream usable. This is the one non-terminal protocol error.
func TestOversizeEnvelopeLeavesEncoderUsable(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)

	huge := &Envelope{Version: CurrentVersion, Type: TypeTask}
	if err := SetPayload(huge, strings.Repeat("x", MaxFrameSize)); err != nil {
		t.Fatalf("SetPayload: %v", err)
	}
	if err := enc.WriteEnvelope(huge); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("oversize envelope reached the stream: %d bytes", buf.Len())
	}

	// The same encoder must still work: the failure was local.
	if err := enc.WriteEnvelope(mustEnvelope(t, TypePong, "node-2", "node-1", nil)); err != nil {
		t.Fatalf("encoder unusable after a local oversize rejection: %v", err)
	}
	if _, err := NewDecoder(&buf).ReadFrame(); err != nil {
		t.Fatalf("frame after a rejected oversize envelope did not decode: %v", err)
	}
}
