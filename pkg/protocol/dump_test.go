package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// errReader fails with a non-EOF error. DumpStream must distinguish "the stream
// ended" from "the stream broke": the first is a normal finish, the second is not.
type errReader struct{ err error }

func (e errReader) Read(p []byte) (int, error) { return 0, e.err }

func TestDumpStreamReportsHeaderReadFailure(t *testing.T) {
	sentinel := errors.New("i/o timeout")
	var out bytes.Buffer

	err := DumpStream(errReader{err: sentinel}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("header read error not wrapped: got %v", err)
	}
	// A clean end-of-stream must NOT be reported for a broken read.
	if strings.Contains(out.String(), "end of stream") {
		t.Errorf("a broken read was rendered as a clean end of stream:\n%s", out.String())
	}
}

// A zero-length frame is a protocol violation, and the dumper stops. It is on a
// frame boundary, but a peer emitting zero-length frames is not speaking this
// protocol and continuing would be inventing meaning.
func TestDumpStreamStopsAtZeroLengthFrame(t *testing.T) {
	var out bytes.Buffer

	err := DumpStream(bytes.NewReader(frameBytes(0, nil)), &out)
	if !errors.Is(err, ErrZeroLengthFrame) {
		t.Fatalf("want ErrZeroLengthFrame, got %v", err)
	}
	if !strings.Contains(out.String(), "zero-length frame") {
		t.Errorf("the violation was not reported to the operator:\n%s", out.String())
	}
}

// The operator needs to see WHERE the stream was truncated, so the offset and the
// declared length must be printed before the error is returned.
func TestDumpStreamReportsTruncatedPayload(t *testing.T) {
	var out bytes.Buffer
	stream := frameBytes(64, []byte(`{"type":"PING"`)) // declares 64, supplies 14

	err := DumpStream(bytes.NewReader(stream), &out)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want io.ErrUnexpectedEOF, got %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "truncated payload") {
		t.Errorf("truncation not reported:\n%s", rendered)
	}
	if !strings.Contains(rendered, "len=64") {
		t.Errorf("declared length not reported, so the operator cannot see the mismatch:\n%s", rendered)
	}
}

// An unknown type belongs to neither plane. The dumper must say so rather than
// silently defaulting it to "control", which would misrepresent the frame.
func TestDumpStreamLabelsUnknownTypeAsNeitherPlane(t *testing.T) {
	body, err := json.Marshal(&Envelope{
		Version: CurrentVersion,
		Type:    MessageType("QUANTUM_ENTANGLE"),
		From:    "node-9",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out bytes.Buffer
	if err := DumpStream(bytes.NewReader(frameBytes(uint32(len(body)), body)), &out); err != nil {
		t.Fatalf("DumpStream on an unknown type should succeed: %v", err)
	}

	rendered := out.String()
	if !strings.Contains(rendered, "UNKNOWN-TYPE") {
		t.Errorf("unknown type not flagged:\n%s", rendered)
	}
	if !strings.Contains(rendered, "QUANTUM_ENTANGLE") {
		t.Errorf("the unrecognised type name was not shown, which is what an operator needs:\n%s", rendered)
	}
}

// An unparseable body is truncated in the output so one garbage frame cannot
// flood the operator's terminal, and the dumper continues to the next frame.
func TestDumpStreamTruncatesLongUnparseableBody(t *testing.T) {
	garbage := []byte(strings.Repeat("A", 4096))
	good, err := json.Marshal(&Envelope{Version: CurrentVersion, Type: TypePong, From: "node-2"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var stream bytes.Buffer
	stream.Write(frameBytes(uint32(len(garbage)), garbage))
	stream.Write(frameBytes(uint32(len(good)), good))

	var out bytes.Buffer
	if err := DumpStream(bytes.NewReader(stream.Bytes()), &out); err != nil {
		t.Fatalf("DumpStream should survive an unparseable body: %v", err)
	}

	rendered := out.String()
	if !strings.Contains(rendered, "UNPARSEABLE") {
		t.Errorf("garbage frame not flagged:\n%s", rendered)
	}
	if strings.Count(rendered, "A") > 1024 {
		t.Errorf("raw body was not truncated: %d 'A' characters rendered", strings.Count(rendered, "A"))
	}
	// The stream was still on a frame boundary, so the next frame must decode.
	if !strings.Contains(rendered, string(TypePong)) {
		t.Errorf("dumper did not continue past the garbage frame:\n%s", rendered)
	}
}
