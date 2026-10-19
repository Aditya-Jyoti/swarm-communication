package mavlink

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex literal in test: %v", err)
	}
	return b
}

// drain pulls every frame the parser currently holds, collecting errors so that
// a test can assert on the whole sequence instead of one call at a time.
func drain(t *testing.T, p *Parser) ([]*Frame, []error) {
	t.Helper()
	var frames []*Frame
	var errs []error
	for {
		f, err := p.Next()
		switch {
		case errors.Is(err, ErrNeedMore):
			return frames, errs
		case err != nil:
			errs = append(errs, err)
		default:
			frames = append(frames, f)
		}
	}
}

// buildV1 assembles a MAVLink 1 frame. There is no v1 encoder in the package --
// nothing should emit v1 -- so the test writes the format out by hand, which also
// documents it.
func buildV1(seq, sysID, compID byte, id MsgID, payload []byte, crcExtra byte) []byte {
	b := []byte{MagicV1, byte(len(payload)), seq, sysID, compID, byte(id)}
	b = append(b, payload...)
	return binary.LittleEndian.AppendUint16(b, frameCRC(b[1:], crcExtra))
}

// setIncompat sets incompat_flags on a v2 frame and recomputes the checksum,
// producing the frame a (hypothetically newer) sender would really have emitted.
// Setting the bit without recomputing would only ever test the CRC check, since
// the flag byte is covered by the checksum.
func setIncompat(frame []byte, flags byte) []byte {
	out := append([]byte(nil), frame...)
	out[2] = flags
	payLen := int(out[1])
	id := MsgID(uint32(out[7]) | uint32(out[8])<<8 | uint32(out[9])<<16)
	crc := frameCRC(out[1:headerLenV2+payLen], crcExtras[id])
	binary.LittleEndian.PutUint16(out[headerLenV2+payLen:], crc)
	return out
}

// sign marks a frame as signed and appends 13 bytes of signature. The signature
// is not computed: this package skips signatures rather than verifying them, and
// the test's job is to prove that the skip keeps the stream in sync.
func sign(frame []byte) []byte {
	return append(setIncompat(frame, flagSigned), bytes.Repeat([]byte{0x5A}, signatureLen)...)
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

// TestEncodeGoldenFrames compares whole frames, byte for byte, against output
// captured from pymavlink. Unlike the conformance test these goldens need no
// Python, so a CI machine without the venv still catches a change in the header
// layout, the payload order, the truncation rule or the CRC.
func TestEncodeGoldenFrames(t *testing.T) {
	tests := []struct {
		name    string
		seq     byte
		sysID   byte
		compID  byte
		msg     Message
		wantHex string
	}{
		{
			name: "heartbeat", seq: 0, sysID: 7, compID: 1,
			msg: &Heartbeat{
				CustomMode: 1, Type: MavTypeQuadrotor, Autopilot: MavAutopilotGeneric,
				BaseMode:     MavModeFlagManualInputEnabled | MavModeFlagStabilizeEnabled | MavModeFlagCustomModeEnabled,
				SystemStatus: MavStateActive, MavlinkVersion: ProtocolVersion,
			},
			wantHex: "fd090000000701000000010000000200510403e197",
		},
		{
			// Full 28-byte payload: hdg is the last field and is non-zero, so
			// nothing is trimmed.
			name: "global_position_int", seq: 5, sysID: 9, compID: 1,
			msg: &GlobalPositionInt{
				TimeBootMs: 1234, Lat: 473979710, Lon: 85461640, Alt: 488000,
				RelativeAlt: 0, Vx: 10, Vy: -20, Vz: 0, Hdg: 9000,
			},
			wantHex: "fd1c0000050901210000d20400003e5b401c880a180540720700000000000a00ecff0000282" +
				"3f15b",
		},
		{
			// Truncation: STATUSTEXT is 54 bytes of struct and 14 bytes on the
			// wire, because the tail of char[50] and both extension fields are
			// zero.
			name: "statustext truncated", seq: 200, sysID: 3, compID: 1,
			msg:     statustext(6, "leader_change"),
			wantHex: "fd0e0000c80301fd0000066c65616465725f6368616e67659c2d",
		},
		{
			// A message whose extensions this dialect has but pymavlink's build
			// does not: 12 extension bytes trimmed away, leaving the 6 bytes a
			// 2022-era ground station expects.
			name: "mission_current", seq: 0, sysID: 1, compID: 1,
			msg:     &MissionCurrent{Seq: 3, Total: 6, MissionState: 2, MissionMode: 1},
			wantHex: "fd0600000001012a000003000600020168c4",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Encode(nil, tc.seq, tc.sysID, tc.compID, tc.msg)
			want := mustHex(t, tc.wantHex)
			if !bytes.Equal(got, want) {
				t.Errorf("frame mismatch\n got %x\nwant %x", got, want)
			}
		})
	}
}

func statustext(severity byte, text string) *Statustext {
	m := &Statustext{Severity: severity}
	SetChars(m.Text[:], text)
	return m
}

func TestEncodeTruncatesTrailingZeros(t *testing.T) {
	tests := []struct {
		name       string
		msg        Message
		wantOnWire int
	}{
		// Every field zero: the payload disappears entirely. Legal, and what
		// MISSION_REQUEST_LIST to a broadcast target actually looks like.
		{"all zero collapses to nothing", &MissionRequestList{}, 0},
		{"one leading byte survives", &MissionRequestList{TargetSystem: 7}, 1},
		// Heartbeat's custom_mode is 4 bytes at offset 0; the rest is zero.
		{"little-endian zeros trim from the end", &Heartbeat{CustomMode: 1}, 1},
		{"a non-zero last field defeats trimming", &Heartbeat{MavlinkVersion: 3}, 9},
		{"statustext trims the char array tail", statustext(1, "x"), 2},
		{"full payload when nothing trails", mustFilled(MsgIDAttitude), 28},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := Encode(nil, 0, 1, 1, tc.msg)
			if got := int(frame[1]); got != tc.wantOnWire {
				t.Errorf("declared payload length = %d, want %d (frame %x)", got, tc.wantOnWire, frame)
			}
			if got := len(frame); got != headerLenV2+tc.wantOnWire+crcLen {
				t.Errorf("frame is %d bytes, want %d", got, headerLenV2+tc.wantOnWire+crcLen)
			}
		})
	}
}

func mustFilled(id MsgID) Message {
	m, err := NewMessage(id)
	if err != nil {
		panic(err)
	}
	fillMessage(m, -1)
	return m
}

func TestEncodeAppendsToDst(t *testing.T) {
	// The bridge reuses one buffer per link, so Encode must append and must not
	// reallocate when there is room. Both are checked: the prefix survives, and
	// a second encode into the same capacity does not move the backing array.
	dst := make([]byte, 0, 512)
	dst = append(dst, 0xEE)
	dst = Encode(dst, 1, 2, 1, &Heartbeat{Type: 2})
	if dst[0] != 0xEE {
		t.Fatalf("Encode clobbered the prefix byte: %x", dst[0])
	}
	before := &dst[0]
	dst = Encode(dst, 2, 2, 1, &Heartbeat{Type: 2})
	if &dst[0] != before {
		t.Error("Encode reallocated a buffer that had spare capacity")
	}
}

func TestFrameCRCGolden(t *testing.T) {
	// The heartbeat golden above ends in e197, i.e. 0x97e1 little-endian.
	frame := mustHex(t, "fd090000000701000000010000000200510403e197")
	body := frame[1 : len(frame)-crcLen]
	if got, want := frameCRC(body, 50), uint16(0x97e1); got != want {
		t.Errorf("frameCRC = 0x%04x, want 0x%04x", got, want)
	}
	// Omitting CRC_EXTRA must change the answer; if it did not, the schema check
	// the byte exists to provide would be doing nothing.
	if plain := frameCRC(body, 0); plain == 0x97e1 {
		t.Error("CRC_EXTRA had no effect on the checksum")
	}
}

func TestSequencer(t *testing.T) {
	var s Sequencer // zero value must work

	// Per-sender counters: interleaving two senders must not perturb either
	// sequence, because a ground station reads link loss out of the gaps.
	for i := 0; i < 3; i++ {
		if got := s.Next(7, 1); got != byte(i) {
			t.Errorf("sysid 7 call %d = %d, want %d", i, got, i)
		}
		if got := s.Next(9, 1); got != byte(i) {
			t.Errorf("sysid 9 call %d = %d, want %d", i, got, i)
		}
	}
	// Same sysid, different component, is a different stream.
	if got := s.Next(7, 2); got != 0 {
		t.Errorf("sysid 7 comp 2 first call = %d, want 0", got)
	}

	// Wrap at 255 rather than panic or stall.
	var w Sequencer
	for i := 0; i < 255; i++ {
		w.Next(1, 1)
	}
	if got := w.Next(1, 1); got != 255 {
		t.Fatalf("255th call = %d, want 255", got)
	}
	if got := w.Next(1, 1); got != 0 {
		t.Errorf("256th call = %d, want wrap to 0", got)
	}
}

func TestSequencerConcurrent(t *testing.T) {
	// Under -race this asserts the mutex exists; the count assertion also
	// catches a lost update, which a RWMutex misuse would produce.
	var s Sequencer
	const goroutines, each = 8, 32

	var wg sync.WaitGroup
	seen := make([][]byte, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seen[g] = append(seen[g], s.Next(1, 1))
			}
		}(g)
	}
	wg.Wait()

	counts := make(map[byte]int)
	for _, got := range seen {
		for _, v := range got {
			counts[v]++
		}
	}
	if len(counts) != goroutines*each {
		t.Errorf("got %d distinct sequence numbers from %d calls: a number was handed out twice",
			len(counts), goroutines*each)
	}
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

func TestFrameRoundTripAllMessages(t *testing.T) {
	for _, want := range allMessages() {
		t.Run(want.MsgName(), func(t *testing.T) {
			fillMessage(want, -1)
			frame := Encode(nil, 17, 42, MavCompIDAutopilot1, want)

			for _, path := range []struct {
				name   string
				decode func([]byte) (*Frame, error)
			}{
				{"datagram", DecodeDatagram},
				{"parser", func(b []byte) (*Frame, error) {
					p := &Parser{}
					p.Feed(b)
					return p.Next()
				}},
			} {
				t.Run(path.name, func(t *testing.T) {
					f, err := path.decode(frame)
					if err != nil {
						t.Fatalf("decode: %v", err)
					}
					if f.Version != 2 || f.Seq != 17 || f.SysID != 42 || f.CompID != 1 {
						t.Errorf("header = v%d seq %d sys %d comp %d", f.Version, f.Seq, f.SysID, f.CompID)
					}
					if f.ID != want.MsgID() {
						t.Fatalf("msgid = %d, want %d", f.ID, want.MsgID())
					}
					if f.Signed {
						t.Error("Signed = true for an unsigned frame")
					}
					if !reflect.DeepEqual(f.Msg, want) {
						t.Errorf("message\n got %+v\nwant %+v", f.Msg, want)
					}
				})
			}
		})
	}
}

// TestDecodeZeroExtendsTruncatedPayload is the receiving half of v2 truncation.
// The sender dropped 40 bytes; the decoder must put them back as zeroes before
// Unmarshal, and must still report the true wire length in Payload.
func TestDecodeZeroExtendsTruncatedPayload(t *testing.T) {
	frame := mustHex(t, "fd0e0000c80301fd0000066c65616465725f6368616e67659c2d")

	f, err := DecodeDatagram(frame)
	if err != nil {
		t.Fatalf("DecodeDatagram: %v", err)
	}
	if got := len(f.Payload); got != 14 {
		t.Errorf("Payload is %d bytes, want the 14 that were on the wire", got)
	}
	msg, ok := f.Msg.(*Statustext)
	if !ok {
		t.Fatalf("decoded %T, want *Statustext", f.Msg)
	}
	if got := Chars(msg.Text[:]); got != "leader_change" {
		t.Errorf("Text = %q", got)
	}
	if msg.Severity != 6 {
		t.Errorf("Severity = %d, want 6", msg.Severity)
	}
	// The fields that were never transmitted.
	if msg.ID != 0 || msg.ChunkSeq != 0 {
		t.Errorf("extension fields = (%d, %d), want zeroes", msg.ID, msg.ChunkSeq)
	}
	// And the bytes between the wire payload and the full payload must be
	// readable zeroes, not whatever was in the decoder's buffer.
	for i, c := range msg.Text[13:] {
		if c != 0 {
			t.Fatalf("Text[%d] = 0x%02x, want 0", 13+i, c)
			break
		}
	}
}

func TestPayloadDoesNotAliasParserBuffer(t *testing.T) {
	// The parser reuses its buffer, so a Frame holding a window into it would
	// mutate under its owner on the next Feed. This is the test that keeps the
	// copy in decodeFrame from being "optimised" away.
	p := &Parser{}
	p.Feed(Encode(nil, 1, 1, 1, statustext(3, "first")))
	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	keep := append([]byte(nil), f.Payload...)
	first := f.Msg.(*Statustext).Text

	for i := 0; i < 8; i++ {
		p.Feed(Encode(nil, byte(i+2), 1, 1, statustext(4, "a much longer message than the first")))
		if _, err := p.Next(); err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
	}
	if !bytes.Equal(f.Payload, keep) {
		t.Errorf("Payload changed after later reads\n got %x\nwant %x", f.Payload, keep)
	}
	if f.Msg.(*Statustext).Text != first {
		t.Error("decoded message changed after later reads")
	}
}

// ---------------------------------------------------------------------------
// MAVLink 1 leniency
// ---------------------------------------------------------------------------

func TestDecodeV1Frames(t *testing.T) {
	tests := []struct {
		name    string
		frame   string
		wantID  MsgID
		wantSys byte
		wantMsg Message
	}{
		{
			name:  "heartbeat",
			frame: "fe092affbe000000000006080004034196",
			// A GCS heartbeat: MAV_TYPE_GCS, MAV_AUTOPILOT_INVALID.
			wantID: MsgIDHeartbeat, wantSys: 255,
			wantMsg: &Heartbeat{
				Type: MavTypeGcs, Autopilot: MavAutopilotInvalid,
				SystemStatus: MavStateActive, MavlinkVersion: ProtocolVersion,
			},
		},
		{
			// The reason v1 needs zero-extension too: a v1 sender has never heard
			// of extension fields, so mission_type is simply absent from a 2-byte
			// payload and must come out as zero rather than as ErrShortPayload.
			name:   "message with an extension field the sender lacks",
			frame:  "fe022affbe2b0701" + "9627",
			wantID: MsgIDMissionRequestList, wantSys: 255,
			wantMsg: &MissionRequestList{TargetSystem: 7, TargetComponent: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := DecodeDatagram(mustHex(t, tc.frame))
			if err != nil {
				t.Fatalf("DecodeDatagram: %v", err)
			}
			if f.Version != 1 {
				t.Errorf("Version = %d, want 1", f.Version)
			}
			if f.ID != tc.wantID || f.SysID != tc.wantSys {
				t.Errorf("msgid %d sysid %d, want %d/%d", f.ID, f.SysID, tc.wantID, tc.wantSys)
			}
			if !reflect.DeepEqual(f.Msg, tc.wantMsg) {
				t.Errorf("message\n got %+v\nwant %+v", f.Msg, tc.wantMsg)
			}
		})
	}
}

func TestV1AndV2InOneStream(t *testing.T) {
	// A GCS that downgrades mid-session, or a router mixing sources. Both markers
	// are searched for in one pass precisely so the v1 frame here is not skipped
	// as garbage.
	var stream []byte
	stream = append(stream, mustHex(t, "fe092affbe000000000006080004034196")...)
	stream = append(stream, Encode(nil, 1, 7, 1, &Heartbeat{Type: MavTypeQuadrotor})...)
	stream = append(stream, buildV1(2, 255, 190, MsgIDMissionRequestList,
		[]byte{7, 1}, crcExtras[MsgIDMissionRequestList])...)

	p := &Parser{}
	p.Feed(stream)
	frames, errs := drain(t, p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	wantVersions := []byte{1, 2, 1}
	if len(frames) != len(wantVersions) {
		t.Fatalf("got %d frames, want %d", len(frames), len(wantVersions))
	}
	for i, want := range wantVersions {
		if frames[i].Version != want {
			t.Errorf("frame %d is v%d, want v%d", i, frames[i].Version, want)
		}
	}
	if p.Stats().GarbageBytes != 0 {
		t.Errorf("GarbageBytes = %d, want 0", p.Stats().GarbageBytes)
	}
}

// ---------------------------------------------------------------------------
// Signing
// ---------------------------------------------------------------------------

func TestSignedFrameIsAcceptedAndSkipped(t *testing.T) {
	// Signatures are not verified, but their 13 bytes MUST be accounted for, or
	// the parser resumes mid-signature and loses the next frame as well.
	signed := sign(Encode(nil, 3, 7, 1, &Heartbeat{Type: MavTypeQuadrotor, CustomMode: 1}))
	next := Encode(nil, 4, 7, 1, statustext(2, "after the signature"))

	p := &Parser{}
	p.Feed(append(signed, next...))
	frames, errs := drain(t, p)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (the signature was probably not skipped)", len(frames))
	}
	if !frames[0].Signed {
		t.Error("frames[0].Signed = false")
	}
	if frames[1].ID != MsgIDStatustext {
		t.Errorf("second frame is %s, want STATUSTEXT", frames[1].ID)
	}
	if p.Buffered() != 0 {
		t.Errorf("Buffered = %d after draining, want 0", p.Buffered())
	}
}

func TestUnsupportedIncompatFlagsRejected(t *testing.T) {
	// A valid frame -- checksum and all -- that merely sets an incompat bit this
	// build does not know. That is what a future protocol extension looks like
	// on the wire, and MAVLink requires it to be dropped rather than guessed at.
	frame := setIncompat(Encode(nil, 0, 1, 1, &Heartbeat{Type: 2}), 0x02)
	_, err := DecodeDatagram(frame)
	if !errors.Is(err, ErrUnsupportedIncompatFlags) {
		t.Fatalf("error = %v, want ErrUnsupportedIncompatFlags", err)
	}
	if !IsMalformed(err) {
		t.Error("IsMalformed = false: the bridge would treat this as fatal")
	}
}

// ---------------------------------------------------------------------------
// Malformed input
// ---------------------------------------------------------------------------

func TestDecodeDatagramMalformed(t *testing.T) {
	good := Encode(nil, 0, 7, 1, &Heartbeat{Type: 2, CustomMode: 9})

	corruptPayload := append([]byte(nil), good...)
	corruptPayload[headerLenV2] ^= 0xFF

	corruptLen := append([]byte(nil), good...)
	corruptLen[1] = 200 // claims far more payload than is present

	unknownID := append([]byte(nil), good...)
	unknownID[7], unknownID[8], unknownID[9] = 0xFF, 0xFF, 0x00 // msgid 65535

	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, ErrTruncated},
		{"not mavlink at all", []byte{0x47, 0x45, 0x54, 0x20}, ErrBadMagic},
		{"magic only", good[:1], ErrTruncated},
		{"partial header", good[:headerLenV2-1], ErrTruncated},
		{"header but no payload", good[:headerLenV2], ErrTruncated},
		{"payload cut short", good[:len(good)-3], ErrTruncated},
		{"crc cut short", good[:len(good)-1], ErrTruncated},
		{"corrupted payload", corruptPayload, ErrBadCRC},
		{"length field lies", corruptLen, ErrTruncated},
		{"unknown message id", unknownID, ErrUnknownMsgID},
		// The flag bytes are covered by the checksum, so a flipped incompat bit
		// is reported as corruption rather than as an unsupported extension --
		// which is the right order: an unverified frame tells you nothing about
		// what its sender meant.
		{"incompat bit flipped without a new crc", flipBit(good, 2, 0x02), ErrBadCRC},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := DecodeDatagram(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if f != nil {
				t.Errorf("frame = %+v, want nil", f)
			}
			if !IsMalformed(err) {
				t.Error("IsMalformed = false, so the bridge would not count this as a bad packet")
			}
		})
	}
}

// TestCRCCatchesEverySingleByteCorruption walks a bit flip through an entire
// frame. Nothing may decode as a *valid* frame of the original message with
// different contents -- that is the silent-corruption case the CRC exists to
// prevent.
func TestCRCCatchesEverySingleByteCorruption(t *testing.T) {
	want := &GlobalPositionInt{
		TimeBootMs: 1234, Lat: 473979710, Lon: 85461640, Alt: 488000,
		RelativeAlt: -1, Vx: 10, Vy: -20, Vz: 3, Hdg: 9000,
	}
	good := Encode(nil, 5, 9, 1, want)

	for i := range good {
		for _, mask := range []byte{0x01, 0x80} {
			corrupt := append([]byte(nil), good...)
			corrupt[i] ^= mask

			f, err := DecodeDatagram(corrupt)
			if err != nil {
				if !IsMalformed(err) {
					t.Errorf("byte %d mask 0x%02x: error %v is not classified as malformed", i, mask, err)
				}
				continue
			}
			// A decode that succeeded is only acceptable if nothing material
			// changed, which happens for the sequence number and for flipping a
			// bit in the compat_flags byte -- neither is covered by the CRC in a
			// way that matters, and neither can alter a field.
			if f.ID == want.MsgID() && !reflect.DeepEqual(f.Msg, want) {
				t.Errorf("byte %d mask 0x%02x decoded as a valid but DIFFERENT message:\n got %+v\nwant %+v",
					i, mask, f.Msg, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Parser: streaming and resynchronisation
// ---------------------------------------------------------------------------

func TestParserResync(t *testing.T) {
	frame := Encode(nil, 1, 7, 1, &Heartbeat{Type: MavTypeQuadrotor, CustomMode: 2})

	// Junk that contains a byte that looks like a frame start. The false header
	// it yields is complete within the junk, so the parser must fail its
	// checksum, drop one byte, and find the real frame behind it.
	falseStart := []byte{0x00, MagicV2, 0, 0, 0, 0, 0, 0, 0, 0, 0xFF, 0xFF}

	tests := []struct {
		name string
		// stream is fed in one go; the drip test covers split feeds.
		stream     []byte
		wantFrames int
		wantErrs   int
		// wantGarbage is an exact count where it is obvious by inspection, and 0
		// means "not asserted" -- resync arithmetic on adversarial input is not
		// worth pinning to the byte.
		wantGarbage uint64
	}{
		{
			name:       "clean stream",
			stream:     bytes.Join([][]byte{frame, frame}, nil),
			wantFrames: 2,
		},
		{
			// Leading junk with no frame marker in it at all: dropped wholesale,
			// in one step, which is the cheap and common case (a GCS pointed at
			// the wrong port, a stray HTTP probe).
			name:        "leading garbage",
			stream:      append([]byte("GET / HTTP/1.1\r\n"), frame...),
			wantFrames:  1,
			wantGarbage: 16,
		},
		{
			name:       "garbage containing a false start marker",
			stream:     append(falseStart, frame...),
			wantFrames: 1,
			wantErrs:   1,
		},
		{
			name:        "junk between two frames",
			stream:      bytes.Join([][]byte{frame, []byte("xxxx"), frame}, nil),
			wantFrames:  2,
			wantGarbage: 4,
		},
		{
			// A frame whose checksum byte is damaged, followed by a good one. The
			// damaged frame's declared length is deliberately NOT trusted, so
			// recovery is a one-byte-at-a-time rescan; it finds the next real
			// frame and nothing is lost.
			name:       "bad crc then a good frame",
			stream:     bytes.Join([][]byte{corrupt(frame, len(frame)-1), frame}, nil),
			wantFrames: 1,
			wantErrs:   1,
		},
		{
			// The honest limit of byte-level resynchronisation, asserted rather
			// than hidden. Corrupting this frame's first payload byte turns it
			// into 0xFD, so the rescan finds a forged frame start whose header
			// overlaps the real frame behind it; the forged msgid is unknown, its
			// header is consumed, and the real frame is lost with it.
			//
			// Recovery costs one frame and the parser stays synchronised, which is
			// the property that matters. Nothing short of a framing layer with a
			// non-occurring delimiter can do better, and MAVLink does not have one.
			name:       "corruption that forges a frame start costs the next frame",
			stream:     bytes.Join([][]byte{corrupt(frame, headerLenV2), frame}, nil),
			wantFrames: 0,
			wantErrs:   2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Parser{}
			p.Feed(tc.stream)
			frames, errs := drain(t, p)

			if len(frames) != tc.wantFrames {
				t.Errorf("decoded %d frames, want %d", len(frames), tc.wantFrames)
			}
			if len(errs) != tc.wantErrs {
				t.Errorf("got %d errors, want %d: %v", len(errs), tc.wantErrs, errs)
			}
			for _, err := range errs {
				if !IsMalformed(err) {
					t.Errorf("error %v is not classified as malformed", err)
				}
			}
			if tc.wantGarbage != 0 && p.Stats().GarbageBytes != tc.wantGarbage {
				t.Errorf("GarbageBytes = %d, want %d", p.Stats().GarbageBytes, tc.wantGarbage)
			}
			if got := p.Stats().Frames; got != uint64(tc.wantFrames) {
				t.Errorf("Stats().Frames = %d, want %d", got, tc.wantFrames)
			}
			for _, f := range frames {
				if f.ID != MsgIDHeartbeat {
					t.Errorf("decoded %s, want HEARTBEAT", f.ID)
				}
			}
			// Whatever happened, the parser must not be sitting on a backlog.
			if p.Buffered() >= MaxFrameLen {
				t.Errorf("Buffered = %d after draining, want less than %d", p.Buffered(), MaxFrameLen)
			}
		})
	}
}

// flipBit flips specific bits, where corrupt flips the whole byte. The
// distinction matters for the flag bytes: XORing 0xFF into incompat_flags would
// also set the signing bit and change the frame's length.
func flipBit(frame []byte, at int, mask byte) []byte {
	out := append([]byte(nil), frame...)
	out[at] ^= mask
	return out
}

func corrupt(frame []byte, at int) []byte {
	out := append([]byte(nil), frame...)
	out[at] ^= 0xFF
	return out
}

func TestParserDripFeed(t *testing.T) {
	// One byte per Feed is the meanest legal stream: every header field, the
	// payload and the checksum are all split. A decoder that assumes a Read
	// returns a whole message fails here, which is the point.
	var stream []byte
	for i := 0; i < 3; i++ {
		stream = append(stream, Encode(nil, byte(i), 7, 1, statustext(4, "drip"))...)
	}

	p := &Parser{}
	var frames []*Frame
	for i := range stream {
		p.Feed(stream[i : i+1])
		for {
			f, err := p.Next()
			if err != nil {
				if !errors.Is(err, ErrNeedMore) {
					t.Fatalf("byte %d: %v", i, err)
				}
				break
			}
			frames = append(frames, f)
		}
		if p.Buffered() > MaxFrameLen {
			t.Fatalf("buffer grew to %d bytes", p.Buffered())
		}
	}
	if len(frames) != 3 {
		t.Fatalf("decoded %d frames, want 3", len(frames))
	}
	for i, f := range frames {
		if f.Seq != byte(i) {
			t.Errorf("frame %d has seq %d", i, f.Seq)
		}
		if got := Chars(f.Msg.(*Statustext).Text[:]); got != "drip" {
			t.Errorf("frame %d text = %q", i, got)
		}
	}
}

func TestParserUnknownMsgIDSkipsWholeFrame(t *testing.T) {
	// A well-formed frame carrying a message id this build does not implement.
	// Its checksum cannot be verified -- that needs the definition's CRC_EXTRA --
	// so the frame is skipped by its declared length, and the frame behind it
	// must still decode. Any other policy desynchronises the stream on the first
	// message type the dialect gains.
	unknown := []byte{MagicV2, 4, 0, 0, 9, 7, 1, 0xFF, 0xFF, 0x00, 1, 2, 3, 4, 0x00, 0x00}
	good := Encode(nil, 10, 7, 1, &Heartbeat{Type: MavTypeQuadrotor})

	p := &Parser{}
	p.Feed(append(unknown, good...))

	if _, err := p.Next(); !errors.Is(err, ErrUnknownMsgID) {
		t.Fatalf("first Next error = %v, want ErrUnknownMsgID", err)
	}
	f, err := p.Next()
	if err != nil {
		t.Fatalf("second Next: %v (the unknown frame was not skipped cleanly)", err)
	}
	if f.ID != MsgIDHeartbeat || f.Seq != 10 {
		t.Errorf("recovered frame = %s seq %d, want HEARTBEAT seq 10", f.ID, f.Seq)
	}
	if got := p.Stats().Unknown; got != 1 {
		t.Errorf("Stats().Unknown = %d, want 1", got)
	}
}

func TestParserResetDropsPartialFrame(t *testing.T) {
	frame := Encode(nil, 1, 7, 1, &Heartbeat{Type: 2, CustomMode: 5})

	p := &Parser{}
	p.Feed(frame[:6]) // half a header, as if the link died mid-frame
	if _, err := p.Next(); !errors.Is(err, ErrNeedMore) {
		t.Fatalf("Next on a partial frame = %v, want ErrNeedMore", err)
	}

	p.Reset()
	if p.Buffered() != 0 {
		t.Fatalf("Buffered = %d after Reset", p.Buffered())
	}

	// Without the Reset, the stale half-frame would prefix the new link's first
	// frame and cost a resync.
	p.Feed(frame)
	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next after Reset: %v", err)
	}
	if f.ID != MsgIDHeartbeat {
		t.Errorf("decoded %s", f.ID)
	}
	if p.Stats().GarbageBytes != 0 {
		t.Errorf("GarbageBytes = %d, want 0: Reset should discard, not re-scan", p.Stats().GarbageBytes)
	}
}
