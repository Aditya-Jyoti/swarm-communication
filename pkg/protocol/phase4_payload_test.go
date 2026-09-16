package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// STATE_SYNC and CHAOS are control plane. A shed STATE_SYNC would leave a
// promoted worker serving a stale ledger; a shed CHAOS "clear" would leave a node
// degraded forever. Neither is a tolerable load-shedding outcome.
func TestPhase4TypesAreControlPlane(t *testing.T) {
	for _, typ := range []MessageType{TypeStateSync, TypeChaos} {
		if !typ.Valid() {
			t.Errorf("%s is not in knownTypes", typ)
		}
		if !typ.IsControlPlane() {
			t.Errorf("%s must be control plane", typ)
		}
		if typ.IsDataPlane() {
			t.Errorf("%s must not be data plane", typ)
		}
	}
}

// Every new payload must survive the real codec, not just json.Marshal. The
// populated cases pin field names and types; a renamed JSON tag would silently
// zero the field on the peer and pass a Marshal-only test.
func TestPhase4PayloadsRoundTripThroughFraming(t *testing.T) {
	tests := []struct {
		name    string
		typ     MessageType
		payload any
		decode  func(*Envelope) (any, error)
	}{
		{
			name: "state sync populated",
			typ:  TypeStateSync,
			payload: StateSyncPayload{
				Term:    3,
				Leader:  "node-1",
				Workers: []NodeID{"node-2", "node-3"},
				Ledger: []TaskRecord{
					{TaskID: "t-1", AssignedTo: "node-2", State: "done", Result: "42"},
					{TaskID: "t-2", AssignedTo: "node-3", State: "pending"},
					{TaskID: "t-3", AssignedTo: "node-2", State: "failed", Result: "boom"},
				},
				Version: 17,
			},
			decode: func(e *Envelope) (any, error) { return PayloadOf[StateSyncPayload](e) },
		},
		{
			name:    "task with body",
			typ:     TypeTask,
			payload: TaskPayload{TaskID: "t-9", Kind: "hash", Body: json.RawMessage(`{"input":"abc","rounds":3}`)},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[TaskPayload](e) },
		},
		{
			name:    "task result success",
			typ:     TypeTaskResult,
			payload: TaskResultPayload{TaskID: "t-9", Worker: "node-4", OK: true, Output: "deadbeef", DurationMS: 12.5},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[TaskResultPayload](e) },
		},
		{
			name:    "task result failure with zero duration",
			typ:     TypeTaskResult,
			payload: TaskResultPayload{TaskID: "t-10", Worker: "node-4", OK: false, Output: "unknown kind"},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[TaskResultPayload](e) },
		},
		{
			name: "telemetry populated",
			typ:  TypeTelemetry,
			payload: TelemetryPayload{
				Node: "node-2", Role: "worker", State: "alive", Term: 3, Leader: "node-1", Degraded: true,
				Peers: []MemberRecord{
					{ID: "node-1", Advertise: "node-1:7946", Incarnation: 2, Role: "leader", State: "alive"},
				},
				Scores:     map[NodeAddress]float64{"node-1:7946": 1.5, "node-3:7946": 0},
				Dropped:    7,
				LedgerSize: 3,
			},
			decode: func(e *Envelope) (any, error) { return PayloadOf[TelemetryPayload](e) },
		},
		{
			name:    "chaos delay",
			typ:     TypeChaos,
			payload: ChaosPayload{Action: "delay", DelayMS: 250},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[ChaosPayload](e) },
		},
		{
			// Validation is the receiver's job; the codec must carry a bad value
			// faithfully so the receiver can log exactly what it rejected.
			name:    "chaos negative delay round-trips unchanged",
			typ:     TypeChaos,
			payload: ChaosPayload{Action: "delay", DelayMS: -1},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[ChaosPayload](e) },
		},
		{
			name:    "chaos kill",
			typ:     TypeChaos,
			payload: ChaosPayload{Action: "kill"},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[ChaosPayload](e) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := mustEnvelope(t, tt.typ, "node-1", "node-2", tt.payload)

			if err := NewEncoder(&buf).WriteEnvelope(env); err != nil {
				t.Fatalf("WriteEnvelope: %v", err)
			}
			got, err := NewDecoder(&buf).ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			decoded, err := tt.decode(got)
			if err != nil {
				t.Fatalf("PayloadOf: %v", err)
			}
			if !reflect.DeepEqual(decoded, tt.payload) {
				t.Errorf("round trip changed the payload:\n got %#v\nwant %#v", decoded, tt.payload)
			}
		})
	}
}

// Nil-vs-empty is NOT preserved by encoding/json, and callers must not rely on
// it. This test documents what actually comes back:
//
//   - A nil slice or map (no omitempty) encodes as JSON null and decodes as nil.
//   - An empty, non-nil slice or map encodes as [] / {} and decodes as empty,
//     non-nil.
//
// So the codec is faithful to nil-ness in both directions, but a receiver must
// still treat nil and empty identically (len == 0), because the sender's choice
// between them is an implementation detail it can change without a protocol bump.
func TestPhase4EmptyCollectionsRoundTrip(t *testing.T) {
	t.Run("state sync nil collections decode as nil", func(t *testing.T) {
		env := mustEnvelope(t, TypeStateSync, "node-1", "node-2", StateSyncPayload{Term: 1, Leader: "node-1"})
		if !bytes.Contains(env.Payload, []byte(`"workers":null`)) || !bytes.Contains(env.Payload, []byte(`"ledger":null`)) {
			t.Fatalf("nil slices should encode as null, got %s", env.Payload)
		}
		got, err := PayloadOf[StateSyncPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		if got.Workers != nil || got.Ledger != nil {
			t.Errorf("null should decode as nil; got Workers=%#v Ledger=%#v", got.Workers, got.Ledger)
		}
		if len(got.Workers) != 0 || len(got.Ledger) != 0 {
			t.Error("len of a nil slice must be 0; receivers key off len, never nil-ness")
		}
	})

	t.Run("state sync empty collections decode as empty non-nil", func(t *testing.T) {
		env := mustEnvelope(t, TypeStateSync, "node-1", "node-2", StateSyncPayload{
			Term: 1, Leader: "node-1", Workers: []NodeID{}, Ledger: []TaskRecord{},
		})
		if !bytes.Contains(env.Payload, []byte(`"workers":[]`)) || !bytes.Contains(env.Payload, []byte(`"ledger":[]`)) {
			t.Fatalf("empty slices should encode as [], got %s", env.Payload)
		}
		got, err := PayloadOf[StateSyncPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		if got.Workers == nil || len(got.Workers) != 0 {
			t.Errorf("Workers = %#v, want empty non-nil", got.Workers)
		}
		if got.Ledger == nil || len(got.Ledger) != 0 {
			t.Errorf("Ledger = %#v, want empty non-nil", got.Ledger)
		}
	})

	t.Run("telemetry nil peers and scores decode as nil", func(t *testing.T) {
		env := mustEnvelope(t, TypeTelemetry, "node-1", "", TelemetryPayload{Node: "node-1", Role: "worker", State: "alive"})
		if !bytes.Contains(env.Payload, []byte(`"peers":null`)) || !bytes.Contains(env.Payload, []byte(`"scores":null`)) {
			t.Fatalf("nil peers/scores should encode as null, got %s", env.Payload)
		}
		got, err := PayloadOf[TelemetryPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		if got.Peers != nil || got.Scores != nil {
			t.Errorf("null should decode as nil; got Peers=%#v Scores=%#v", got.Peers, got.Scores)
		}
		// Reading a missing key from a nil map is safe; a sender that omits Scores
		// must not make the dashboard code panic.
		if v, ok := got.Scores["node-9:7946"]; ok || v != 0 {
			t.Errorf("nil map lookup = (%v, %v), want (0, false)", v, ok)
		}
	})

	t.Run("telemetry empty peers and scores decode as empty non-nil", func(t *testing.T) {
		env := mustEnvelope(t, TypeTelemetry, "node-1", "", TelemetryPayload{
			Node: "node-1", Role: "worker", State: "alive",
			Peers: []MemberRecord{}, Scores: map[NodeAddress]float64{},
		})
		if !bytes.Contains(env.Payload, []byte(`"peers":[]`)) || !bytes.Contains(env.Payload, []byte(`"scores":{}`)) {
			t.Fatalf("empty peers/scores should encode as []/{}, got %s", env.Payload)
		}
		got, err := PayloadOf[TelemetryPayload](env)
		if err != nil {
			t.Fatal(err)
		}
		if got.Peers == nil || len(got.Peers) != 0 {
			t.Errorf("Peers = %#v, want empty non-nil", got.Peers)
		}
		if got.Scores == nil || len(got.Scores) != 0 {
			t.Errorf("Scores = %#v, want empty non-nil", got.Scores)
		}
	})
}

// A task with no body is legal (a "ping the worker" kind, say). Body is omitempty,
// so a nil RawMessage is omitted from the wire and decodes back as nil rather than
// as the four bytes "null" -- which matters because a handler that does
// json.Unmarshal(task.Body, &args) on "null" gets a zero args and no error, while
// on nil it gets an error it can act on.
func TestTaskPayloadNilBody(t *testing.T) {
	env := mustEnvelope(t, TypeTask, "control-center", "", TaskPayload{TaskID: "t-1", Kind: "noop"})
	if bytes.Contains(env.Payload, []byte(`"body"`)) {
		t.Fatalf("nil Body should be omitted, got %s", env.Payload)
	}
	got, err := PayloadOf[TaskPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != nil {
		t.Errorf("Body = %q, want nil", got.Body)
	}
	if got.TaskID != "t-1" || got.Kind != "noop" {
		t.Errorf("got %+v", got)
	}
}

// An explicit JSON null Body is a distinct case from an omitted one: the sender
// wrote json.RawMessage("null"). It must decode without error and come back as
// the literal bytes, because RawMessage is verbatim by contract.
func TestTaskPayloadExplicitNullBody(t *testing.T) {
	env := mustEnvelope(t, TypeTask, "control-center", "", TaskPayload{TaskID: "t-1", Kind: "noop", Body: json.RawMessage("null")})
	got, err := PayloadOf[TaskPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != "null" {
		t.Errorf("Body = %q, want the literal null", got.Body)
	}
}

// A Body that is not valid JSON must be rejected at the SENDER, by SetPayload,
// because json.RawMessage is emitted verbatim and a malformed one would corrupt the
// enclosing envelope for every peer downstream.
func TestTaskPayloadInvalidBodyFailsAtSender(t *testing.T) {
	_, err := NewEnvelope(TypeTask, "control-center", "", TaskPayload{TaskID: "t-1", Kind: "x", Body: json.RawMessage(`{not json`)})
	if err == nil {
		t.Fatal("expected an error for a malformed RawMessage body")
	}
	if !strings.Contains(err.Error(), "marshal TASK payload") {
		t.Errorf("error should name the message type: %v", err)
	}
}

// encoding/json cannot represent NaN or Inf, so a telemetry sample carrying one
// is lost in its entirety. The sender must sanitise Scores first; this test pins
// the failure so that the requirement has a name and a reproduction.
func TestTelemetryPayloadNaNScoreFailsToMarshal(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			env := &Envelope{Version: CurrentVersion, Type: TypeTelemetry, From: "node-1"}
			err := SetPayload(env, TelemetryPayload{
				Node: "node-1", Role: "worker", State: "alive",
				Scores: map[NodeAddress]float64{"node-2:7946": bad},
			})
			if err == nil {
				t.Fatal("SetPayload accepted an unrepresentable float; the sample would be silently lost later")
			}
			// The wrapper must carry the message type so the log line locates the
			// sender's code path, and must preserve the codec's error so callers
			// can classify it with errors.As.
			if !strings.Contains(err.Error(), "marshal TELEMETRY payload") {
				t.Errorf("error should name the message type: %v", err)
			}
			var unsupported *json.UnsupportedValueError
			if !errors.As(err, &unsupported) {
				t.Errorf("error should wrap *json.UnsupportedValueError, got %T: %v", err, err)
			}
			if len(env.Payload) != 0 {
				t.Errorf("a failed SetPayload must not leave a partial payload behind; got %q", env.Payload)
			}
		})
	}

	t.Run("NewEnvelope surfaces the same failure", func(t *testing.T) {
		env, err := NewEnvelope(TypeTelemetry, "node-1", "", TelemetryPayload{
			Scores: map[NodeAddress]float64{"node-2:7946": math.NaN()},
		})
		if err == nil || env != nil {
			t.Fatalf("NewEnvelope = (%v, %v), want (nil, error)", env, err)
		}
	})
}

// A worker's ledger grows with the run. 1000 records is far beyond what a demo
// swarm produces between snapshots, and it must fit in one frame with margin,
// because STATE_SYNC has no chunking: a ledger that overflows MaxFrameSize is
// unreplicable, not merely slow.
func TestStateSyncLargeLedgerFitsInFrame(t *testing.T) {
	const n = 1000
	want := StateSyncPayload{Term: 9, Leader: "node-1", Version: 1234}
	for i := 0; i < n; i++ {
		want.Workers = append(want.Workers, NodeID(fmt.Sprintf("node-%d", i%50)))
		want.Ledger = append(want.Ledger, TaskRecord{
			TaskID:     fmt.Sprintf("task-%06d", i),
			AssignedTo: NodeID(fmt.Sprintf("node-%d", i%50)),
			State:      []string{"pending", "done", "failed"}[i%3],
			Result:     strings.Repeat("r", 32),
		})
	}

	var buf bytes.Buffer
	env := mustEnvelope(t, TypeStateSync, "node-1", "node-2", want)
	if len(env.Payload) > MaxFrameSize/4 {
		t.Fatalf("a %d-record ledger encodes to %d bytes, more than a quarter of MaxFrameSize (%d); "+
			"STATE_SYNC needs chunking sooner than expected", n, len(env.Payload), MaxFrameSize)
	}
	if err := NewEncoder(&buf).WriteEnvelope(env); err != nil {
		t.Fatalf("WriteEnvelope: %v", err)
	}
	if buf.Len() > MaxFrameSize {
		t.Fatalf("frame is %d bytes, exceeds MaxFrameSize %d", buf.Len(), MaxFrameSize)
	}
	got, err := NewDecoder(&buf).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	decoded, err := PayloadOf[StateSyncPayload](got)
	if err != nil {
		t.Fatalf("PayloadOf: %v", err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("large ledger did not round-trip: got %d records, want %d", len(decoded.Ledger), len(want.Ledger))
	}
}

// Pin the wire field names. The dashboard is vanilla JS reading these keys by
// string; a Go-side rename that reflect.DeepEqual would never notice breaks it.
func TestPhase4WireFieldNames(t *testing.T) {
	cases := []struct {
		payload any
		keys    []string
	}{
		{TaskRecord{}, []string{"task_id", "assigned_to", "state"}},
		{StateSyncPayload{}, []string{"term", "leader", "workers", "ledger", "version"}},
		{TaskPayload{}, []string{"task_id", "kind"}},
		{TaskResultPayload{}, []string{"task_id", "worker", "ok", "duration_ms"}},
		{TelemetryPayload{}, []string{"node", "role", "state", "term", "leader", "degraded", "peers", "scores", "dropped", "ledger_size"}},
		{ChaosPayload{}, []string{"action"}},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.payload)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range c.keys {
			if _, ok := m[k]; !ok {
				t.Errorf("%T: wire key %q missing from %s", c.payload, k, b)
			}
		}
		if len(m) != len(c.keys) {
			t.Errorf("%T: encoded %d keys, test lists %d: %s", c.payload, len(m), len(c.keys), b)
		}
	}
}
