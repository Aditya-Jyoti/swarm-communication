package protocol

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// Every payload type must survive a real encode/decode cycle through the codec, not
// just json.Marshal in isolation. A field that fails to round-trip here is a field
// that silently becomes its zero value on the peer.
func TestMembershipPayloadsRoundTripThroughFraming(t *testing.T) {
	tests := []struct {
		name    string
		typ     MessageType
		payload any
		decode  func(*Envelope) (any, error)
	}{
		{
			name: "membership delta",
			typ:  TypeMembershipDelta,
			payload: MembershipDeltaPayload{
				ViewVersion: 42,
				Members: []MemberRecord{
					{ID: "node-1", Advertise: "node-1:7946", Incarnation: 3, Role: "leader", State: "alive", Score: 1.25},
					{ID: "node-2", Advertise: "node-2:7946", Incarnation: 1, Role: "worker", State: "suspect", Score: UnmeasuredScore},
				},
			},
			decode: func(e *Envelope) (any, error) { return PayloadOf[MembershipDeltaPayload](e) },
		},
		{
			name:    "election result",
			typ:     TypeElectionResult,
			payload: ElectionResultPayload{Term: 7, Leaders: []NodeID{"node-1", "node-4"}, ClusterSize: 9},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[ElectionResultPayload](e) },
		},
		{
			name:    "join cluster",
			typ:     TypeJoinCluster,
			payload: JoinClusterPayload{Worker: "node-5", Score: 1.75},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[JoinClusterPayload](e) },
		},
		{
			name:    "join ack rejection names a different leader",
			typ:     TypeJoinAck,
			payload: JoinAckPayload{Accepted: false, Reason: "demoted", Leader: "node-8"},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[JoinAckPayload](e) },
		},
		{
			name:    "leave",
			typ:     TypeLeave,
			payload: LeavePayload{Reason: "shutdown"},
			decode:  func(e *Envelope) (any, error) { return PayloadOf[LeavePayload](e) },
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

// All five new types are control plane. If one were classified as data plane it could
// be dropped under load by the transport's backpressure policy, which for a
// membership delta or an election result would be a correctness failure rather than
// a shed load.
func TestNewMembershipTypesAreControlPlane(t *testing.T) {
	for _, typ := range []MessageType{
		TypeMembershipDelta, TypeElectionResult, TypeJoinCluster, TypeJoinAck, TypeLeave,
	} {
		if !typ.Valid() {
			t.Errorf("%s is not in knownTypes", typ)
		}
		if !typ.IsControlPlane() {
			t.Errorf("%s must be control plane, or backpressure may drop it", typ)
		}
	}
}

// An omitted member list must decode as empty rather than failing. A delta that
// names nobody is legal: it is how a node reports only its view version.
func TestMembershipDeltaWithNoMembersIsLegal(t *testing.T) {
	env := mustEnvelope(t, TypeMembershipDelta, "node-1", "", MembershipDeltaPayload{ViewVersion: 1})

	got, err := PayloadOf[MembershipDeltaPayload](env)
	if err != nil {
		t.Fatalf("PayloadOf: %v", err)
	}
	if len(got.Members) != 0 {
		t.Errorf("Members = %v, want empty", got.Members)
	}
	if got.ViewVersion != 1 {
		t.Errorf("ViewVersion = %d, want 1", got.ViewVersion)
	}
}

// A self-reported score that is NaN or Inf cannot be carried by JSON; the
// sanitiser must turn it into the unmeasured sentinel rather than fail the
// encode, and a receiver must read it back as unmeasured.
func TestMemberRecordScoreSanitisedOnTheWire(t *testing.T) {
	tests := []struct {
		name     string
		score    float64
		wantWire float64
		measured bool
	}{
		{"finite", 2.5, 2.5, true},
		{"zero", 0, 0, true},
		{"NaN", math.NaN(), UnmeasuredScore, false},
		{"+Inf", math.Inf(1), UnmeasuredScore, false},
		{"-Inf", math.Inf(-1), UnmeasuredScore, false},
		{"negative", -7, UnmeasuredScore, false},
		{"sentinel", UnmeasuredScore, UnmeasuredScore, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := mustEnvelope(t, TypeMembershipDelta, "node-1", "", MembershipDeltaPayload{
				Members: []MemberRecord{{ID: "node-1", Score: tt.score}},
			})
			var buf bytes.Buffer
			if err := NewEncoder(&buf).WriteEnvelope(env); err != nil {
				t.Fatalf("WriteEnvelope: %v", err)
			}
			got, err := NewDecoder(&buf).ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			p, err := PayloadOf[MembershipDeltaPayload](got)
			if err != nil {
				t.Fatalf("PayloadOf: %v", err)
			}
			if p.Members[0].Score != tt.wantWire {
				t.Errorf("wire score = %v, want %v", p.Members[0].Score, tt.wantWire)
			}
			if p.Members[0].Measured() != tt.measured {
				t.Errorf("Measured = %v, want %v", p.Members[0].Measured(), tt.measured)
			}
		})
	}

	// Measured on a value that never touched the wire still rejects NaN/Inf.
	if (MemberRecord{Score: math.NaN()}).Measured() || (MemberRecord{Score: math.Inf(1)}).Measured() {
		t.Error("Measured accepted NaN or Inf")
	}
	// The sanitiser also protects the larger payloads that embed MemberRecord.
	if _, err := json.Marshal(TelemetryPayload{Peers: []MemberRecord{{ID: "x", Score: math.NaN()}}}); err != nil {
		t.Errorf("TelemetryPayload with a NaN member score failed to marshal: %v", err)
	}
}
