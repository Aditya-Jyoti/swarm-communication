package protocol

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMessageTypeTaxonomy(t *testing.T) {
	control := []MessageType{
		TypeHello, TypeHelloAck, TypePing, TypePong, TypeHeartbeat, TypeHeartbeatAck,
		TypeMembershipDelta, TypeElectionResult, TypeJoinCluster, TypeJoinAck, TypeLeave,
	}
	data := []MessageType{TypeTask, TypeTaskResult, TypeTelemetry}

	for _, tp := range control {
		if !tp.Valid() {
			t.Errorf("%s: Valid() = false", tp)
		}
		if !tp.IsControlPlane() {
			t.Errorf("%s: should be control plane", tp)
		}
		if tp.IsDataPlane() {
			t.Errorf("%s: should not be data plane", tp)
		}
	}
	for _, tp := range data {
		if !tp.Valid() {
			t.Errorf("%s: Valid() = false", tp)
		}
		if !tp.IsDataPlane() {
			t.Errorf("%s: should be data plane", tp)
		}
		if tp.IsControlPlane() {
			t.Errorf("%s: should not be control plane", tp)
		}
	}
	if len(knownTypes) != len(control)+len(data) {
		t.Errorf("knownTypes has %d entries; the taxonomy test lists %d — a type was added "+
			"without being classified into a plane", len(knownTypes), len(control)+len(data))
	}

	for _, tp := range []MessageType{"", "hello", "HELLO ", "SOMETHING_NEW"} {
		if tp.Valid() {
			t.Errorf("%q: Valid() = true, want false", tp)
		}
		if tp.IsControlPlane() || tp.IsDataPlane() {
			t.Errorf("%q: an unknown type belongs to neither plane", tp)
		}
	}
}

func TestNewEnvelopeStampsFields(t *testing.T) {
	env, err := NewEnvelope(TypePing, "node-1", "node-2", PingPayload{Nonce: 7, Seq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if env.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", env.Version, CurrentVersion)
	}
	if env.ID == "" {
		t.Error("ID was not minted; request/response correlation would be impossible")
	}
	if env.SentAtUnixNano == 0 {
		t.Error("SentAtUnixNano was not stamped")
	}
	if len(env.Payload) == 0 {
		t.Error("payload was not marshalled")
	}
}

func TestNewEnvelopeNilPayload(t *testing.T) {
	env, err := NewEnvelope(TypeLeave, "node-1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Payload) != 0 {
		t.Errorf("Payload = %q, want empty for a nil payload", env.Payload)
	}
	// An empty To means broadcast, and must survive the round trip as empty rather
	// than becoming the literal string "broadcast" or similar.
	if env.To != "" {
		t.Errorf("To = %q, want empty (broadcast)", env.To)
	}
}

// TestNewReplyEchoesID pins the correlation rule: a response carries the
// request's ID. Minting a fresh one here would make several in-flight PINGs on one
// connection indistinguishable, and the resulting RTTs would be wrong rather than
// missing — the worse failure, because it feeds leader election silently.
func TestNewReplyEchoesID(t *testing.T) {
	req, err := NewEnvelope(TypePing, "node-1", "node-2", PingPayload{Nonce: 99, Seq: 3})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := NewReply(req, TypePong, "node-2", PongPayload{Nonce: 99, Seq: 3})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ID != req.ID {
		t.Errorf("reply ID = %q, want %q", reply.ID, req.ID)
	}
	if reply.To != req.From {
		t.Errorf("reply To = %q, want %q", reply.To, req.From)
	}
	if reply.From != "node-2" {
		t.Errorf("reply From = %q", reply.From)
	}
}

func TestPayloadOfRoundTrip(t *testing.T) {
	want := HelloPayload{Advertise: "node-5:7946", Incarnation: 1234, KnownPeers: []NodeAddress{"a:1", "b:2"}}
	env, err := NewEnvelope(TypeHello, "node-5", "seed", want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := PayloadOf[HelloPayload](env)
	if err != nil {
		t.Fatal(err)
	}
	if got.Advertise != want.Advertise || got.Incarnation != want.Incarnation || len(got.KnownPeers) != 2 {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestPayloadOfEmptyIsMalformed(t *testing.T) {
	env := &Envelope{Version: CurrentVersion, Type: TypePing}
	if _, err := PayloadOf[PingPayload](env); !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("got %v, want ErrMalformedFrame", err)
	}
}

func TestPayloadOfWrongShapeIsMalformed(t *testing.T) {
	env := &Envelope{Version: CurrentVersion, Type: TypePing, Payload: json.RawMessage(`{"nonce":"not-a-number"}`)}
	_, err := PayloadOf[PingPayload](env)
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("got %v, want ErrMalformedFrame", err)
	}
	if !IsProtocolViolation(err) {
		t.Errorf("a payload that does not match its declared type is a protocol violation")
	}
}

func TestNewMessageIDIsUnique(t *testing.T) {
	// Not a statistical proof, just a smoke test that the generator is not returning
	// a constant — the failure mode if math/rand were used unseeded across
	// containers started from one image.
	const n = 4096
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewMessageID()
		if len(id) != 16 {
			t.Fatalf("id %q has length %d, want 16", id, len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate message ID %q after %d draws", id, i)
		}
		seen[id] = struct{}{}
	}
}

// TestSentAtIsNotUsedForDurations is documentation in executable form. It asserts
// nothing about behaviour; it exists so the invariant has a name a grep will find.
// There is no clock sync in this swarm, so subtracting two nodes' SentAtUnixNano
// values is meaningless and can be negative. RTT is measured locally, monotonically,
// by the sender of a PING.
func TestSentAtIsNotUsedForDurations(t *testing.T) {
	a := &Envelope{SentAtUnixNano: 1_000_000_000}
	b := &Envelope{SentAtUnixNano: 500_000_000} // a peer whose wall clock is behind
	if d := b.SentAtUnixNano - a.SentAtUnixNano; d >= 0 {
		t.Fatal("this test's premise is broken")
	}
	// A negative "latency" is exactly what cross-node wall-clock subtraction yields,
	// and exactly why no code in this repository may perform it.
}
