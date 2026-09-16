package protocol

import (
	"bytes"
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
					{ID: "node-1", Advertise: "node-1:7946", Incarnation: 3, Role: "leader", State: "alive"},
					{ID: "node-2", Advertise: "node-2:7946", Incarnation: 1, Role: "worker", State: "suspect"},
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
