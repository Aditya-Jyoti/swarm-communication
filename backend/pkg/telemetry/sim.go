package telemetry

import (
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"swarm-net/pkg/geo"
	"swarm-net/pkg/protocol"
)

// SanitizeSimConfig returns p with every value forced into its legal range, the
// node-side rule for SIM_CONFIG (docs/architecture/drone-simulation.md):
//
//   - positions are clamped into the airspace (geo.ClampPosition);
//   - base, per-unit and jitter are clamped (geo.Params.Clamp);
//   - a threshold outside (0, 1], or NaN, becomes 0, "keep the node's own";
//   - a NaN or infinite hysteresis becomes -1, "keep the node's own". A
//     negative one already means that, and 0 stays 0 ("no damping").
//
// Clamping rather than rejecting is deliberate: the message is a snapshot of
// the whole emulation, and one bad slider value must not throw away the
// positions that arrived with it.
//
// The input is not modified; Positions is copied.
func SanitizeSimConfig(p protocol.SimConfigPayload) protocol.SimConfigPayload {
	out := p
	out.Positions = make(map[protocol.NodeID]protocol.Position, len(p.Positions))
	for id, pos := range p.Positions {
		out.Positions[id] = geo.ClampPosition(pos)
	}
	params := geo.Params{BaseMS: p.BaseMS, PerUnitMS: p.PerUnitMS, JitterMS: p.JitterMS}.Clamp()
	out.BaseMS, out.PerUnitMS, out.JitterMS = params.BaseMS, params.PerUnitMS, params.JitterMS
	if !(p.Threshold > 0 && p.Threshold <= 1) { // also true for NaN
		out.Threshold = 0
	}
	if math.IsNaN(p.Hysteresis) || math.IsInf(p.Hysteresis, 0) {
		out.Hysteresis = -1
	}
	return out
}

// simSnapshot is one applied SIM_CONFIG. It is immutable once published:
// Emulation swaps the pointer, never the contents, so a reader holding an old
// snapshot keeps a consistent view (positions and parameters from the same
// version) without a lock.
type simSnapshot struct {
	version   uint64
	enabled   bool
	self      protocol.Position
	hasSelf   bool
	positions map[protocol.NodeID]protocol.Position
	params    geo.Params
}

// Emulation is a node's view of the drone simulation: the last SIM_CONFIG it
// applied, and the per-peer PONG delay derived from it.
//
// # Synchronisation
//
// Apply runs on the telemetry client's goroutine while PeerDelay runs on the
// prober's reply goroutines, many at once. The state is a pointer to an
// immutable simSnapshot held in an atomic.Pointer: readers do one Load and
// never block, and a writer builds a complete new snapshot before one
// CompareAndSwap publishes it. A mutex would also be correct, but it would put
// a lock on the path of every PONG for a value that changes only when an
// operator moves a slider.
type Emulation struct {
	self protocol.NodeID
	// rnd draws the jitter factor in [0, 1). It is called concurrently.
	rnd  func() float64
	snap atomic.Pointer[simSnapshot]
}

// NewEmulation returns an Emulation for node self with nothing applied (no
// delay). rnd supplies the jitter draw in [0, 1) and must be safe for
// concurrent use; nil means math/rand/v2's top-level Float64, which is.
func NewEmulation(self protocol.NodeID, rnd func() float64) *Emulation {
	if rnd == nil {
		rnd = rand.Float64 // #nosec G404 -- emulated jitter, not security
	}
	e := &Emulation{self: self, rnd: rnd}
	e.snap.Store(&simSnapshot{})
	return e
}

// Apply installs p if its version is newer than the one held, and reports
// whether it did. p should already be sanitized (SanitizeSimConfig); Apply
// copies what it keeps, so the caller may reuse p. Safe from any goroutine.
func (e *Emulation) Apply(p protocol.SimConfigPayload) bool {
	next := &simSnapshot{
		version:   p.Version,
		enabled:   p.Enabled,
		positions: make(map[protocol.NodeID]protocol.Position, len(p.Positions)),
		params:    geo.Params{BaseMS: p.BaseMS, PerUnitMS: p.PerUnitMS, JitterMS: p.JitterMS},
	}
	for id, pos := range p.Positions {
		next.positions[id] = pos
	}
	next.self, next.hasSelf = next.positions[e.self]
	// CAS loop rather than Load-then-Store: with two concurrent Apply calls a
	// plain Store could let the older version land last.
	for {
		cur := e.snap.Load()
		if p.Version <= cur.version {
			return false
		}
		if e.snap.CompareAndSwap(cur, next) {
			return true
		}
	}
}

// Version is the version of the last applied SIM_CONFIG, 0 if none.
func (e *Emulation) Version() uint64 { return e.snap.Load().version }

// PeerDelay is the emulated one-way delay to add before answering a PING from
// peer: geo.Delay between this node's position and peer's, with a fresh jitter
// draw. It is 0 when the emulation is disabled, when nothing has been applied,
// or when either position is unknown. It has the network.PeerDelayFunc shape
// and is safe for concurrent use.
func (e *Emulation) PeerDelay(peer protocol.NodeID) time.Duration {
	s := e.snap.Load()
	if !s.enabled || !s.hasSelf {
		return 0
	}
	pos, ok := s.positions[peer]
	if !ok {
		return 0
	}
	return geo.Delay(s.self, pos, s.params, e.rnd())
}
