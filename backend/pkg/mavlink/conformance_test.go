package mavlink

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// Conformance against pymavlink.
//
// # Why an external oracle
//
// Every other test in this package compares this package against itself. They
// catch a codec that is inconsistent, but not one that is consistently wrong --
// and "consistently wrong" is the failure mode that matters, because the only
// peer that counts is a ground station built from someone else's bindings. A
// mis-sorted field order, a CRC_EXTRA from the wrong dialect release or a
// truncation rule applied one byte out all produce frames that round-trip
// perfectly here and are silently discarded by QGroundControl, with no error
// message anywhere in the system.
//
// pymavlink is the reference implementation of the generator this package
// replaces, so agreeing with it byte for byte is the strongest statement
// available without a real ground station in the loop. It is checked in BOTH
// directions: Go encodes and the reference parses, and the reference encodes and
// Go parses. One direction alone would miss a symmetric error, such as a field
// order that is wrong in the same way in both the encoder and the decoder.
//
// # Why it is allowed to skip
//
// pymavlink is a Python package, so requiring it would put an interpreter and a
// virtualenv between a clone of this repository and a passing `go test`. The
// test skips cleanly when it cannot find an interpreter; CRC_EXTRA goldens and
// whole-frame goldens captured FROM this oracle live in the ordinary tests, so
// a machine without Python still fails if the wire format changes.

// candidateInterpreters are tried in order when MAVLINK_PY is unset. A venv
// beside the repository is the usual way to get pymavlink without touching the
// system Python:
//
//	python3 -m venv .venv && .venv/bin/pip install pymavlink
//
// Nothing is installed by the test. When none of these can import pymavlink the
// test skips.
var candidateInterpreters = []string{
	"../../../.venv/bin/python", // repo-root venv, from this package's directory
	"python3",
	"python",
}

// oracle runs testdata/pymav_oracle.py.
type oracle struct {
	python string
	script string
}

// oracleInfo is the reference's view of one message.
type oracleInfo struct {
	Name         string   `json:"name"`
	Fields       []string `json:"fields"`
	Types        []string `json:"types"`
	ArrayLengths []int    `json:"array_lengths"`
	CRCExtra     byte     `json:"crc_extra"`
}

type oracleEncodeCase struct {
	MsgID  uint32 `json:"msgid"`
	Seq    byte   `json:"seq"`
	SysID  byte   `json:"sysid"`
	CompID byte   `json:"compid"`
	Values []any  `json:"values"`
}

type oracleRequest struct {
	InfoIDs []uint32           `json:"info_ids,omitempty"`
	Encode  []oracleEncodeCase `json:"encode,omitempty"`
	Decode  []string           `json:"decode,omitempty"`
}

type oracleDecoded struct {
	MsgID     uint32 `json:"msgid"`
	Name      string `json:"name"`
	Seq       byte   `json:"seq"`
	SysID     byte   `json:"sysid"`
	CompID    byte   `json:"compid"`
	Values    []any  `json:"values"`
	Reencoded string `json:"reencoded"`
}

type oracleResponse struct {
	Info    map[string]oracleInfo `json:"info"`
	Encoded []string              `json:"encoded"`
	Decoded []oracleDecoded       `json:"decoded"`
}

// newOracle locates the interpreter and the script, or skips the test.
func newOracle(t *testing.T) *oracle {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("testdata", "pymav_oracle.py"))
	if err != nil {
		t.Fatalf("locate oracle script: %v", err)
	}

	candidates := candidateInterpreters
	if env := os.Getenv("MAVLINK_PY"); env != "" {
		// An explicit interpreter is a promise, not a hint: if it cannot run the
		// oracle, say so rather than silently falling back to another Python.
		candidates = []string{env}
	}

	// Probe each candidate before the real work, so that a missing pymavlink is a
	// clean skip rather than a failure in the middle of a table of 23 subtests.
	var last error
	for _, python := range candidates {
		o := &oracle{python: python, script: script}
		if _, err := o.call(context.Background(), oracleRequest{InfoIDs: []uint32{0}}); err != nil {
			last = fmt.Errorf("%s: %w", python, err)
			continue
		}
		return o
	}
	t.Skipf("no interpreter with pymavlink (set MAVLINK_PY, or pip install pymavlink): %v", last)
	return nil
}

// call runs the oracle once. The context carries a deadline: this is a
// subprocess over pipes, and a test that hangs forever on a wedged interpreter
// is worse than one that fails.
func (o *oracle) call(ctx context.Context, req oracleRequest) (*oracleResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal oracle request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// -I isolates the interpreter: no site-packages shadowing from the current
	// directory, no PYTHONPATH, no user site. The script lives in testdata/ and
	// is passed as an argument rather than being run from inside it.
	cmd := exec.CommandContext(ctx, o.python, "-I", o.script)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run oracle: %w: %s", err, stderr.String())
	}

	var resp oracleResponse
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	// UseNumber keeps a uint64 field (AUTOPILOT_VERSION.capabilities, .uid)
	// exact. Decoding it into a float64 would lose the low bits of anything
	// above 2^53 and the comparison would fail for reasons that have nothing to
	// do with MAVLink.
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode oracle response %q: %w", stdout.String(), err)
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Value plumbing
// ---------------------------------------------------------------------------

// goValues extracts the first n struct fields of m as JSON-ready values, in wire
// order.
//
// Positional, because the oracle identifies fields by their index in
// ordered_fieldnames. Teaching the Python side this package's Go naming rules
// would make the test share an assumption with the code it is checking.
func goValues(m Message, n int, info oracleInfo) []any {
	v := reflect.ValueOf(m).Elem()
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, goValue(v.Field(i), info.Types[i], info.ArrayLengths[i]))
	}
	return out
}

func goValue(f reflect.Value, mavType string, arrayLen int) any {
	if arrayLen > 0 {
		if mavType == "char" {
			// pymavlink hands char arrays back as a string cut at the first NUL.
			return Chars(f.Slice(0, f.Len()).Bytes())
		}
		out := make([]any, f.Len())
		for i := range out {
			out[i] = goValue(f.Index(i), mavType, 0)
		}
		return out
	}
	switch f.Kind() {
	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return f.Uint()
	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return f.Int()
	case reflect.Float32, reflect.Float64:
		return f.Float()
	}
	panic("goValue: unhandled kind " + f.Kind().String())
}

// sameValue compares a Go-side value with the oracle's JSON value.
func sameValue(want, got any) (bool, string) {
	switch w := want.(type) {
	case string:
		g, ok := got.(string)
		return ok && g == w, fmt.Sprintf("%q vs %q", w, got)
	case uint64:
		g, err := jsonUint(got)
		return err == nil && g == w, fmt.Sprintf("%d vs %v", w, got)
	case int64:
		g, err := jsonInt(got)
		return err == nil && g == w, fmt.Sprintf("%d vs %v", w, got)
	case float64:
		g, err := jsonFloat(got)
		return err == nil && g == w, fmt.Sprintf("%v vs %v", w, got)
	case []any:
		gs, ok := got.([]any)
		if !ok || len(gs) != len(w) {
			return false, fmt.Sprintf("%v vs %v", w, got)
		}
		for i := range w {
			if ok, why := sameValue(w[i], gs[i]); !ok {
				return false, fmt.Sprintf("element %d: %s", i, why)
			}
		}
		return true, ""
	}
	return false, fmt.Sprintf("unhandled Go value %T", want)
}

func jsonNumber(v any) (json.Number, error) {
	n, ok := v.(json.Number)
	if !ok {
		return "", fmt.Errorf("oracle returned %T, want a number", v)
	}
	return n, nil
}

func jsonInt(v any) (int64, error) {
	n, err := jsonNumber(v)
	if err != nil {
		return 0, err
	}
	return n.Int64()
}

func jsonUint(v any) (uint64, error) {
	n, err := jsonNumber(v)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(n.String(), 10, 64)
}

func jsonFloat(v any) (float64, error) {
	n, err := jsonNumber(v)
	if err != nil {
		return 0, err
	}
	return n.Float64()
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// conformanceCase is one message under test, with the header values it is framed
// with. The seq/sysid/compid are varied per message so that a decoder that
// ignored the header, or read it from the wrong offset, cannot pass.
type conformanceCase struct {
	msg    Message
	info   oracleInfo
	values []any
	seq    byte
	sysID  byte
	compID byte
	frame  []byte
}

func TestConformanceWithPymavlink(t *testing.T) {
	o := newOracle(t)
	ctx := context.Background()

	msgs := allMessages()
	ids := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, uint32(m.MsgID()))
	}

	info, err := o.call(ctx, oracleRequest{InfoIDs: ids})
	if err != nil {
		t.Fatalf("oracle info: %v", err)
	}

	cases := make([]conformanceCase, 0, len(msgs))
	req := oracleRequest{}
	for i, m := range msgs {
		id := uint32(m.MsgID())
		in, ok := info.Info[strconv.FormatUint(uint64(id), 10)]
		if !ok {
			t.Fatalf("%s: the reference dialect does not define message %d", m.MsgName(), id)
		}
		if in.Name != m.MsgName() {
			t.Errorf("message %d is %q here and %q in the reference", id, m.MsgName(), in.Name)
		}
		if in.CRCExtra != m.CRCExtra() {
			t.Errorf("%s: CRC_EXTRA is %d here and %d in the reference", m.MsgName(), m.CRCExtra(), in.CRCExtra)
		}

		// The reference may know FEWER fields than this build: common.xml gains
		// extension fields over time (MISSION_ACK.opaque_id and friends), and
		// extensions are always appended, so the reference's field list is a
		// prefix of ours. Only the shared prefix is filled, which keeps the
		// comparison exact in both directions: anything beyond it would be
		// trimmed off the wire as trailing zeroes anyway.
		n := len(in.Fields)
		if got := reflect.ValueOf(m).Elem().NumField(); n > got {
			t.Fatalf("%s: the reference has %d fields, this build has only %d", m.MsgName(), n, got)
		}
		checkFieldTypes(t, m, in)

		fillMessage(m, n)
		c := conformanceCase{
			msg:    m,
			info:   in,
			values: goValues(m, n, in),
			// Distinct per message, and chosen to span the byte range.
			seq:    byte(i * 11),
			sysID:  byte(1 + i*7%250),
			compID: MavCompIDAutopilot1,
		}
		c.frame = Encode(nil, c.seq, c.sysID, c.compID, m)

		req.Decode = append(req.Decode, hex.EncodeToString(c.frame))
		req.Encode = append(req.Encode, oracleEncodeCase{
			MsgID: id, Seq: c.seq, SysID: c.sysID, CompID: c.compID, Values: c.values,
		})
		cases = append(cases, c)
	}

	resp, err := o.call(ctx, req)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(resp.Decoded) != len(cases) || len(resp.Encoded) != len(cases) {
		t.Fatalf("oracle returned %d decoded and %d encoded results, want %d of each",
			len(resp.Decoded), len(resp.Encoded), len(cases))
	}

	for i, c := range cases {
		t.Run(c.msg.MsgName(), func(t *testing.T) {
			t.Run("go_encodes_reference_parses", func(t *testing.T) {
				got := resp.Decoded[i]
				if got.MsgID != uint32(c.msg.MsgID()) {
					t.Fatalf("reference read msgid %d, want %d", got.MsgID, c.msg.MsgID())
				}
				if got.Seq != c.seq || got.SysID != c.sysID || got.CompID != c.compID {
					t.Errorf("reference read header seq %d sys %d comp %d, want %d/%d/%d",
						got.Seq, got.SysID, got.CompID, c.seq, c.sysID, c.compID)
				}
				if len(got.Values) != len(c.values) {
					t.Fatalf("reference read %d fields, want %d", len(got.Values), len(c.values))
				}
				for f := range c.values {
					if ok, why := sameValue(c.values[f], got.Values[f]); !ok {
						t.Errorf("field %d (%s): %s", f, c.info.Fields[f], why)
					}
				}
				// Re-encoding what it parsed must reproduce our bytes exactly.
				// This covers the header, the truncation point and the checksum
				// in one comparison, including any field the loop above somehow
				// did not look at.
				if got.Reencoded != hex.EncodeToString(c.frame) {
					t.Errorf("reference re-encoded\n got %s\nwant %s", got.Reencoded, hex.EncodeToString(c.frame))
				}
			})

			t.Run("reference_encodes_go_parses", func(t *testing.T) {
				refFrame, err := hex.DecodeString(resp.Encoded[i])
				if err != nil {
					t.Fatalf("bad hex from oracle: %v", err)
				}
				// Byte equality of the two encoders. Payload truncation makes this
				// a stronger claim than it looks: both sides must agree on the
				// field order AND on exactly how many trailing zero bytes to drop.
				if !bytes.Equal(refFrame, c.frame) {
					t.Errorf("encoders disagree\n go  %x\n ref %x", c.frame, refFrame)
				}

				f, err := DecodeDatagram(refFrame)
				if err != nil {
					t.Fatalf("decoding the reference's frame: %v", err)
				}
				if f.ID != c.msg.MsgID() || f.Seq != c.seq || f.SysID != c.sysID || f.CompID != c.compID {
					t.Errorf("decoded %s seq %d sys %d comp %d", f.ID, f.Seq, f.SysID, f.CompID)
				}
				// Compare field by field rather than with DeepEqual, so that a
				// failure names the field instead of dumping two structs.
				want := reflect.ValueOf(c.msg).Elem()
				have := reflect.ValueOf(f.Msg).Elem()
				for fi := 0; fi < want.NumField(); fi++ {
					if !reflect.DeepEqual(have.Field(fi).Interface(), want.Field(fi).Interface()) {
						t.Errorf("field %d (%s) = %v, want %v", fi, want.Type().Field(fi).Name,
							have.Field(fi).Interface(), want.Field(fi).Interface())
					}
				}
			})
		})
	}
}

// checkFieldTypes compares the Go struct's field types with the reference's
// MAVLink types, position by position. The value comparisons would catch a
// permutation anyway, but this names the problem: "field 3 is uint16 here and
// uint32_t there" is a one-line fix, where a wrong value five fields later is an
// afternoon.
func checkFieldTypes(t *testing.T, m Message, in oracleInfo) {
	t.Helper()
	typ := reflect.ValueOf(m).Elem().Type()

	wantKind := map[string]reflect.Kind{
		"char": reflect.Uint8, "uint8_t": reflect.Uint8, "int8_t": reflect.Int8,
		"uint16_t": reflect.Uint16, "int16_t": reflect.Int16,
		"uint32_t": reflect.Uint32, "int32_t": reflect.Int32,
		"uint64_t": reflect.Uint64, "int64_t": reflect.Int64,
		"float": reflect.Float32, "double": reflect.Float64,
	}

	for i, mavType := range in.Types {
		f := typ.Field(i)
		kind, known := wantKind[mavType]
		if !known {
			t.Errorf("%s field %d (%s): the reference reports type %q, which this test does not know",
				m.MsgName(), i, in.Fields[i], mavType)
			continue
		}
		if n := in.ArrayLengths[i]; n > 0 {
			if f.Type.Kind() != reflect.Array {
				t.Errorf("%s field %d (%s) is %s, want [%d]%s", m.MsgName(), i, in.Fields[i], f.Type, n, kind)
				continue
			}
			if f.Type.Len() != n || f.Type.Elem().Kind() != kind {
				t.Errorf("%s field %d (%s) is %s, want [%d]%s", m.MsgName(), i, in.Fields[i], f.Type, n, kind)
			}
			continue
		}
		if f.Type.Kind() != kind {
			t.Errorf("%s field %d (%s) is %s, want %s (%s)",
				m.MsgName(), i, in.Fields[i], f.Type, kind, mavType)
		}
	}
}

// TestConformanceOracleIsHonest makes sure a passing conformance run means
// something: the oracle must REJECT a frame this package would also reject, so
// that a broken oracle cannot silently approve everything.
func TestConformanceOracleIsHonest(t *testing.T) {
	o := newOracle(t)

	good := Encode(nil, 1, 7, 1, &Heartbeat{
		Type: MavTypeQuadrotor, CustomMode: 2, SystemStatus: MavStateActive,
	})
	bad := append([]byte(nil), good...)
	bad[len(bad)-1] ^= 0xFF // corrupt the checksum

	if _, err := DecodeDatagram(bad); !errors.Is(err, ErrBadCRC) {
		t.Fatalf("this package accepted a corrupt frame: %v", err)
	}
	_, err := o.call(context.Background(), oracleRequest{
		Decode: []string{hex.EncodeToString(bad)},
	})
	if err == nil {
		t.Error("the oracle accepted a frame with a corrupt checksum, so it is not checking CRCs")
	}
}
