package telemetry

import (
	"math"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/protocol"
)

// FromStatus builds the TELEMETRY payload for one Status snapshot.
//
// Mapping notes, each one a decision rather than a transcription:
//
//   - State is this node's own record in its view. A node always believes
//     itself alive, but reading it from the view (rather than hard-coding
//     "alive") keeps the field honest if that ever changes.
//   - Degraded means "a chaos delay is in effect", per the contract. It is not
//     Status.Degraded, which is the partition guard's flag: the dashboard uses
//     this bit to tell injected latency from real trouble, so it must mean
//     exactly that.
//   - Peers excludes self. The dashboard draws edges from a node to its peers,
//     and a self-edge is noise.
//   - Scores are Status.Scores (this node's LOCAL measurements) re-keyed by
//     address, because the payload joins them to Peers via Advertise. A score
//     whose peer is not in the view has no address and is dropped, as is any
//     NaN or Inf: encoding/json refuses both, and one bad value would lose the
//     whole sample (see TelemetryPayload).
//   - Threshold and Hysteresis are the election settings in force (Status
//     already applies defaults and overrides). SimVersion and Flows are not
//     part of a node's Status; SnapshotFunc fills them.
func FromStatus(s cluster.Status) protocol.TelemetryPayload {
	p := protocol.TelemetryPayload{
		Node:       s.Self,
		Role:       s.Role.String(),
		State:      cluster.StateAlive.String(),
		Term:       s.Term,
		Leader:     s.Leader,
		Degraded:   s.ChaosDelay > 0,
		Peers:      make([]protocol.MemberRecord, 0, len(s.View.Members)),
		Scores:     make(map[protocol.NodeAddress]float64, len(s.Scores)),
		Dropped:    s.Dropped,
		LedgerSize: len(s.Ledger),
		Threshold:  s.Threshold,
		Hysteresis: s.Hysteresis,
	}
	addrOf := make(map[protocol.NodeID]protocol.NodeAddress, len(s.View.Members))
	for _, m := range s.View.Members {
		if m.ID == s.Self {
			p.State = m.State.String()
			continue
		}
		addrOf[m.ID] = m.Addr
		p.Peers = append(p.Peers, protocol.MemberRecord{
			ID:          m.ID,
			Advertise:   m.Addr,
			Incarnation: m.Incarnation,
			Role:        m.Role.String(),
			State:       m.State.String(),
			// NaN (no report yet) is safe here: MemberRecord.MarshalJSON
			// substitutes UnmeasuredScore.
			Score: m.Score,
		})
	}
	for id, v := range s.Scores {
		addr, ok := addrOf[id]
		if !ok || addr == "" || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		p.Scores[addr] = v
	}
	return p
}

// SnapshotFunc builds a Config.Snapshot: FromStatus(status()), plus the sim
// version from simVersion and the flow counts from flows (typically
// Emulation.Version and FlowRecorder.Drain). simVersion and flows may be nil.
//
// flows is a Drain: every call resets the counts, so each sample carries the
// frames sent since the previous one. Use the returned func as the client's
// only Snapshot, or samples will steal each other's counts.
func SnapshotFunc(status func() cluster.Status, simVersion func() uint64, flows func() []protocol.FlowRecord) func() protocol.TelemetryPayload {
	return func() protocol.TelemetryPayload {
		p := FromStatus(status())
		if simVersion != nil {
			p.SimVersion = simVersion()
		}
		if flows != nil {
			p.Flows = flows()
		}
		return p
	}
}
