package mavlink

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// TestCRCExtraGoldens pins the CRC_EXTRA byte of every generated message
// against values read out of pymavlink.
//
// This is the one test in the package that is worth having even though
// conformance_test.go subsumes it. CRC_EXTRA is derived from the dialect XML, so
// regenerating against the wrong XML -- a different MAVLink release, a vendor
// dialect, an edited file -- changes these bytes and nothing else. The symptom
// without this test is a ground station that silently ignores every frame, with
// no error anywhere, because a rejected checksum is indistinguishable from line
// noise. These goldens turn that into a failing unit test that runs without
// Python.
func TestCRCExtraGoldens(t *testing.T) {
	golden := []struct {
		id       MsgID
		crcExtra byte
	}{
		{MsgIDHeartbeat, 50},
		{MsgIDSysStatus, 124},
		{MsgIDParamRequestRead, 214},
		{MsgIDParamRequestList, 159},
		{MsgIDParamValue, 220},
		{MsgIDGPSRawInt, 24},
		{MsgIDAttitude, 39},
		{MsgIDGlobalPositionInt, 104},
		{MsgIDMissionItem, 254},
		{MsgIDMissionRequest, 230},
		{MsgIDMissionSetCurrent, 28},
		{MsgIDMissionCurrent, 28},
		{MsgIDMissionRequestList, 132},
		{MsgIDMissionCount, 221},
		{MsgIDMissionClearAll, 232},
		{MsgIDMissionItemReached, 11},
		{MsgIDMissionAck, 153},
		{MsgIDGPSGlobalOrigin, 39},
		{MsgIDMissionRequestInt, 196},
		{MsgIDMissionItemInt, 38},
		{MsgIDVfrHud, 20},
		{MsgIDCommandLong, 152},
		{MsgIDCommandAck, 143},
		{MsgIDBatteryStatus, 154},
		{MsgIDAutopilotVersion, 178},
		{MsgIDHomePosition, 104},
		{MsgIDExtendedSysState, 130},
		{MsgIDStatustext, 83},
	}

	if len(golden) != len(messageFactory) {
		t.Fatalf("golden table covers %d messages, registry has %d: a message was "+
			"added to gen/main.go without a golden CRC_EXTRA", len(golden), len(messageFactory))
	}
	for _, tc := range golden {
		t.Run(tc.id.String(), func(t *testing.T) {
			m, err := NewMessage(tc.id)
			if err != nil {
				t.Fatalf("NewMessage(%d): %v", tc.id, err)
			}
			if got := m.CRCExtra(); got != tc.crcExtra {
				t.Errorf("CRCExtra = %d, want %d", got, tc.crcExtra)
			}
		})
	}
}

// TestRegistryAgreesWithMessages checks that the lookup tables the frame decoder
// uses say the same thing as the message types themselves. The decoder reads
// crcExtras and knownPayloadLen before it has a message value, so a table that
// drifted from its type would produce frames that fail their own round trip.
func TestRegistryAgreesWithMessages(t *testing.T) {
	for _, m := range allMessages() {
		t.Run(m.MsgName(), func(t *testing.T) {
			id := m.MsgID()
			if got := messageNames[id]; got != m.MsgName() {
				t.Errorf("messageNames[%d] = %q, MsgName = %q", id, got, m.MsgName())
			}
			if got := crcExtras[id]; got != m.CRCExtra() {
				t.Errorf("crcExtras[%d] = %d, CRCExtra = %d", id, got, m.CRCExtra())
			}
			if got := knownPayloadLen[id]; got != m.PayloadLen() {
				t.Errorf("knownPayloadLen[%d] = %d, PayloadLen = %d", id, got, m.PayloadLen())
			}
			if !Known(id) {
				t.Errorf("Known(%d) = false for a registered message", id)
			}
			if id.String() != m.MsgName() {
				t.Errorf("MsgID.String() = %q, want %q", id.String(), m.MsgName())
			}
		})
	}
}

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	for _, m := range allMessages() {
		t.Run(m.MsgName(), func(t *testing.T) {
			fillMessage(m, -1)

			// A non-empty prefix: Marshal must append rather than overwrite, and
			// the frame encoder relies on exactly that.
			prefix := []byte{0xAA, 0xBB}
			buf := m.Marshal(append([]byte(nil), prefix...))
			if !bytes.Equal(buf[:2], prefix) {
				t.Fatalf("Marshal clobbered dst: % x", buf[:2])
			}
			payload := buf[2:]
			if len(payload) != m.PayloadLen() {
				t.Fatalf("Marshal wrote %d bytes, PayloadLen is %d", len(payload), m.PayloadLen())
			}

			got, err := NewMessage(m.MsgID())
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			if err := got.Unmarshal(payload); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, m) {
				t.Errorf("round trip changed the message\n got %+v\nwant %+v", got, m)
			}
		})
	}
}

func TestUnmarshalShortPayload(t *testing.T) {
	for _, m := range allMessages() {
		t.Run(m.MsgName(), func(t *testing.T) {
			short := make([]byte, m.PayloadLen()-1)
			err := m.Unmarshal(short)
			if !errors.Is(err, ErrShortPayload) {
				t.Errorf("Unmarshal(%d bytes) error = %v, want ErrShortPayload", len(short), err)
			}
		})
	}
}

// TestUnmarshalIgnoresTrailingBytes is the forward-compatibility half of the
// extension mechanism: a peer built against a newer dialect sends fields this
// build has never heard of, and the right response is to decode what is
// understood and ignore the rest, not to reject the frame.
func TestUnmarshalIgnoresTrailingBytes(t *testing.T) {
	for _, m := range allMessages() {
		t.Run(m.MsgName(), func(t *testing.T) {
			fillMessage(m, -1)
			payload := m.Marshal(nil)
			payload = append(payload, 0xDE, 0xAD, 0xBE, 0xEF)

			got, err := NewMessage(m.MsgID())
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			if err := got.Unmarshal(payload); err != nil {
				t.Fatalf("Unmarshal with 4 extra bytes: %v", err)
			}
			if !reflect.DeepEqual(got, m) {
				t.Errorf("trailing bytes leaked into the message\n got %+v\nwant %+v", got, m)
			}
		})
	}
}

func TestNewMessageUnknownID(t *testing.T) {
	m, err := NewMessage(MsgID(0xFFFFFF))
	if !errors.Is(err, ErrUnknownMsgID) {
		t.Errorf("error = %v, want ErrUnknownMsgID", err)
	}
	if m != nil {
		t.Errorf("message = %v, want nil so a caller cannot dereference it", m)
	}
	if Known(0xFFFFFF) {
		t.Error("Known(0xFFFFFF) = true")
	}
	if got := MsgID(283).String(); got != "MSG_283" {
		t.Errorf("MsgID(283).String() = %q, want %q", got, "MSG_283")
	}
}

// TestWireFieldOrder pins the field order of a few messages against
// pymavlink's ordered_fieldnames.
//
// This is the property the generator exists for and the one no reader can check
// by eye: common.xml declares HEARTBEAT as type, autopilot, base_mode,
// system_status, mavlink_version, custom_mode, and the wire puts the uint32
// FIRST because fields are sorted by descending type size. Get this wrong and
// every field of every message decodes as a neighbouring field -- a failure that
// still passes a Go-only round-trip test, because both sides are wrong the same
// way. conformance_test.go checks all 23 messages this way against the
// reference; these four are here so the invariant is still covered when Python
// is not installed.
func TestWireFieldOrder(t *testing.T) {
	tests := []struct {
		id     MsgID
		fields []string
	}{
		{MsgIDHeartbeat, []string{
			"CustomMode", "Type", "Autopilot", "BaseMode", "SystemStatus", "MavlinkVersion",
		}},
		{MsgIDGlobalPositionInt, []string{
			"TimeBootMs", "Lat", "Lon", "Alt", "RelativeAlt", "Vx", "Vy", "Vz", "Hdg",
		}},
		{MsgIDMissionItemInt, []string{
			"Param1", "Param2", "Param3", "Param4", "X", "Y", "Z", "Seq", "Command",
			"TargetSystem", "TargetComponent", "Frame", "Current", "Autocontinue",
			"MissionType",
		}},
		// STATUSTEXT is the extension case: text is char[50] and so sorts with
		// the 1-byte fields, while id (uint16) and chunk_seq stay at the end
		// because they are extensions and extensions are never sorted.
		{MsgIDStatustext, []string{"Severity", "Text", "ID", "ChunkSeq"}},
	}
	for _, tc := range tests {
		t.Run(tc.id.String(), func(t *testing.T) {
			m, err := NewMessage(tc.id)
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			typ := reflect.ValueOf(m).Elem().Type()
			var got []string
			for i := 0; i < typ.NumField(); i++ {
				got = append(got, typ.Field(i).Name)
			}
			if !reflect.DeepEqual(got, tc.fields) {
				t.Errorf("wire order\n got %v\nwant %v", got, tc.fields)
			}
		})
	}
}

func TestChars(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty array", []byte{}, ""},
		{"all nul", []byte{0, 0, 0}, ""},
		{"nul terminated", []byte{'A', 'B', 0, 0}, "AB"},
		// The case that costs you an afternoon: a string that exactly fills the
		// array has no terminator, which is legal and common for PARAM_VALUE.
		{"unterminated full array", []byte{'A', 'B', 'C'}, "ABC"},
		{"junk after nul is ignored", []byte{'A', 0, 'X', 'Y'}, "A"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Chars(tc.in); got != tc.want {
				t.Errorf("Chars(% x) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSetChars(t *testing.T) {
	tests := []struct {
		name string
		size int
		in   string
		want []byte
	}{
		{"pads with nul", 4, "AB", []byte{'A', 'B', 0, 0}},
		{"exact fit leaves no terminator", 2, "AB", []byte{'A', 'B'}},
		{"truncates silently", 2, "ABC", []byte{'A', 'B'}},
		{"overwrites previous contents", 3, "Z", []byte{'Z', 0, 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dst := bytes.Repeat([]byte{'x'}, tc.size)
			SetChars(dst, tc.in)
			if !bytes.Equal(dst, tc.want) {
				t.Errorf("SetChars(%q) = % x, want % x", tc.in, dst, tc.want)
			}
			if round := Chars(dst); round != tc.in && len(tc.in) <= tc.size {
				t.Errorf("Chars(SetChars(%q)) = %q", tc.in, round)
			}
		})
	}
}
