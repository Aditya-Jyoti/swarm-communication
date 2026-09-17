package protocol

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// Throughput and allocation baselines for the codec, taken before gossip fan-out
// is layered on top of these paths.
//
// The number to watch is allocs/op, not ns/op. A node relays every membership
// frame it learns to every peer it knows, so one allocation inside WriteEnvelope
// is one allocation times fan-out times gossip rate -- and a garbage-collector
// pause is indistinguishable, from a peer's side, from the node having stalled.
// Latency that gets attributed to a stall feeds straight into health scores and
// then into leader election, so allocation pressure here is not merely a
// performance concern.
//
// Every benchmark calls b.ReportAllocs() for that reason.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// benchRosterSize is the member count for the large fixture.
//
// 50 is the representative gossip frame rather than the worst case: it is a
// plausible steady-state swarm, big enough that the per-record JSON key bytes
// dominate the envelope, and small enough that it stays far below MaxFrameSize.
// The worst case (a 1000-record ledger) is already pinned by
// TestStateSyncLargeLedgerFitsInFrame; what is measured here is the cost the
// swarm pays continuously.
const benchRosterSize = 50

// benchHeartbeat is the high-frequency fixture: a few dozen bytes of payload,
// emitted by every node on every tick.
func benchHeartbeat() HeartbeatPayload {
	return HeartbeatPayload{Seq: 99, Term: 3, LeaderID: "node-1", ClusterSize: benchRosterSize}
}

// benchMembershipDelta builds a roster of n members with realistic field widths.
// The IDs and addresses are formatted rather than repeated so that the JSON is
// not artificially compressible -- nothing here compresses it, but a fixture of
// identical records would also be a fixture whose escape analysis and string
// interning behaviour differs from the real thing.
func benchMembershipDelta(n int) MembershipDeltaPayload {
	p := MembershipDeltaPayload{ViewVersion: 42, Members: make([]MemberRecord, 0, n)}
	for i := 0; i < n; i++ {
		role, state := "worker", "alive"
		if i%10 == 0 {
			role = "leader"
		}
		if i%13 == 0 {
			state = "suspect"
		}
		p.Members = append(p.Members, MemberRecord{
			ID:          NodeID(fmt.Sprintf("node-%03d", i)),
			Advertise:   NodeAddress(fmt.Sprintf("node-%03d.swarm.local:7946", i)),
			Incarnation: int64(i + 1),
			Role:        role,
			State:       state,
			Score:       1.25 + float64(i)/100,
		})
	}
	return p
}

// benchSizes are the two fixtures every direction is measured against, as
// sub-benchmarks so the sizes show up as separate rows.
var benchSizes = []struct {
	name    string
	typ     MessageType
	payload any
}{
	{"heartbeat", TypeHeartbeat, benchHeartbeat()},
	{fmt.Sprintf("membership-delta-%d", benchRosterSize), TypeMembershipDelta, benchMembershipDelta(benchRosterSize)},
}

// ---------------------------------------------------------------------------
// Encode
// ---------------------------------------------------------------------------

// BenchmarkEncoderWriteEnvelope measures marshal + frame + write.
//
// The sink is io.Discard, not a bytes.Buffer: a Buffer would grow for b.N
// iterations and the resulting amortised allocations would be attributed to the
// Encoder. What is under test is the codec, and the Encoder's own scratch buffer
// reuse is exactly the thing the allocs/op figure should expose.
func BenchmarkEncoderWriteEnvelope(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			env := mustEnvelopeNoT(size.typ, "node-001", "node-002", size.payload)
			enc := NewEncoder(io.Discard)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := enc.WriteEnvelope(env); err != nil {
					b.Fatalf("WriteEnvelope: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Decode
// ---------------------------------------------------------------------------

// BenchmarkDecoderReadFrame measures read + unmarshal only.
//
// The frame is encoded once, outside the timed loop, and replayed by seeking a
// bytes.Reader back to zero each iteration. Encoding inside the loop would fold
// the marshal cost into the decode row; allocating the frame bytes inside the
// loop would fold in a make() the decoder never performs. Seek on a bytes.Reader
// is arithmetic on two fields and allocates nothing, so it does not contaminate
// the measurement either.
//
// One Decoder is reused across iterations, which is deliberate: that is the
// steady state on a live connection, where the scratch buffer has already grown
// to frame size and ReadFrame's remaining allocations are the Envelope and its
// RawMessage payload.
func BenchmarkDecoderReadFrame(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			var wire bytes.Buffer
			env := mustEnvelopeNoT(size.typ, "node-001", "node-002", size.payload)
			if err := NewEncoder(&wire).WriteEnvelope(env); err != nil {
				b.Fatalf("WriteEnvelope: %v", err)
			}
			frame := wire.Bytes()
			r := bytes.NewReader(frame)
			dec := NewDecoder(r)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := r.Seek(0, io.SeekStart); err != nil {
					b.Fatalf("Seek: %v", err)
				}
				if _, err := dec.ReadFrame(); err != nil {
					b.Fatalf("ReadFrame: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

// BenchmarkRoundTrip measures encode-then-decode through one buffer, which is
// what a relaying node actually costs per frame it forwards.
//
// The buffer is Reset rather than reallocated each iteration so that its
// capacity survives; a fresh bytes.Buffer per iteration would measure the
// allocator warming up, not the codec.
func BenchmarkRoundTrip(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.name, func(b *testing.B) {
			env := mustEnvelopeNoT(size.typ, "node-001", "node-002", size.payload)
			var wire bytes.Buffer
			enc := NewEncoder(&wire)
			dec := NewDecoder(&wire)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				wire.Reset()
				if err := enc.WriteEnvelope(env); err != nil {
					b.Fatalf("WriteEnvelope: %v", err)
				}
				if _, err := dec.ReadFrame(); err != nil {
					b.Fatalf("ReadFrame: %v", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Payload marshalling
// ---------------------------------------------------------------------------

// BenchmarkPayloadMarshalling isolates SetPayload and PayloadOf for the large
// roster, separately from the framing around them.
//
// This split matters because gossip fan-out will call these per peer. If the
// per-peer cost turns out to be dominated by re-marshalling an identical roster
// once per destination, the fix is to marshal once and reuse the RawMessage
// across envelopes -- but that is only worth the complexity if these rows say
// so, which is the whole point of taking the baseline before writing 3b.
func BenchmarkPayloadMarshalling(b *testing.B) {
	payload := benchMembershipDelta(benchRosterSize)
	name := fmt.Sprintf("membership-delta-%d", benchRosterSize)

	b.Run("SetPayload/"+name, func(b *testing.B) {
		env := mustEnvelopeNoT(TypeMembershipDelta, "node-001", "node-002", nil)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := SetPayload(env, payload); err != nil {
				b.Fatalf("SetPayload: %v", err)
			}
		}
	})

	b.Run("PayloadOf/"+name, func(b *testing.B) {
		env := mustEnvelopeNoT(TypeMembershipDelta, "node-001", "node-002", payload)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := PayloadOf[MembershipDeltaPayload](env); err != nil {
				b.Fatalf("PayloadOf: %v", err)
			}
		}
	})
}
