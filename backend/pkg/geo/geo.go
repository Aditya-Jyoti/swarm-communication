// Package geo is the latency model behind the emulated swarm map.
//
// Every node (a drone) has a 3D Position: X and Y on the ground, Z the altitude.
// The one-way delay a node adds before answering a
// peer's PING is
//
//	base + distance(a, b) * perUnit + jitter * u      (u uniform in [0, 1))
//
// clamped to [0, MaxDelay]. The prober measures that delay as part of the round
// trip, so health scores, election and affinity all follow the map without any
// of them knowing a map exists. The package is pure: callers supply the random
// draw, which keeps it deterministic under test.
package geo

import (
	"hash/fnv"
	"math"
	"time"

	"swarm-net/pkg/protocol"
)

// Size is the side of the cubic airspace. Every coordinate is clamped to
// [0, Size].
const Size = 100.0

// MaxDelay caps one emulated delay. It stays well under the health strategy's
// 2s probe timeout, so the largest map distance reads as "far", never as
// "unreachable".
const MaxDelay = 1500 * time.Millisecond

// Limits for the model parameters, enforced by Params.Clamp. The dashboard's
// sliders use the same ranges.
const (
	MaxBaseMS    = 500.0
	MaxPerUnitMS = 10.0
	MaxJitterMS  = 200.0
)

// Defaults for a fresh Control Center.
const (
	DefaultBaseMS    = 1.0
	DefaultPerUnitMS = 2.0
	DefaultJitterMS  = 0.5
)

// Params is the latency model.
type Params struct {
	BaseMS    float64
	PerUnitMS float64
	JitterMS  float64
}

// DefaultParams returns the model a Control Center starts with.
func DefaultParams() Params {
	return Params{BaseMS: DefaultBaseMS, PerUnitMS: DefaultPerUnitMS, JitterMS: DefaultJitterMS}
}

// Clamp returns p with every field forced into its legal range. NaN becomes 0.
func (p Params) Clamp() Params {
	return Params{
		BaseMS:    clamp(p.BaseMS, 0, MaxBaseMS),
		PerUnitMS: clamp(p.PerUnitMS, 0, MaxPerUnitMS),
		JitterMS:  clamp(p.JitterMS, 0, MaxJitterMS),
	}
}

// ClampPosition forces pos into the airspace. NaN becomes 0.
func ClampPosition(pos protocol.Position) protocol.Position {
	return protocol.Position{X: clamp(pos.X, 0, Size), Y: clamp(pos.Y, 0, Size), Z: clamp(pos.Z, 0, Size)}
}

// Distance is the 3D Euclidean distance between a and b.
func Distance(a, b protocol.Position) float64 {
	dx, dy, dz := a.X-b.X, a.Y-b.Y, a.Z-b.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

// Delay is the one-way emulated delay between a and b. u is a uniform draw in
// [0, 1) that scales the jitter; pass 0 for the deterministic part only.
func Delay(a, b protocol.Position, p Params, u float64) time.Duration {
	p = p.Clamp()
	u = clamp(u, 0, 1)
	ms := p.BaseMS + Distance(a, b)*p.PerUnitMS + p.JitterMS*u
	d := time.Duration(ms * float64(time.Millisecond))
	return min(max(d, 0), MaxDelay)
}

// DefaultPosition places id on the map from a hash of its name, so a node keeps
// its spot across Control Center restarts and container restarts without any
// stored state.
func DefaultPosition(id protocol.NodeID) protocol.Position {
	// Three independent 21-bit fields of one 64-bit hash, one per axis.
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	v := h.Sum64()
	const mask = 1<<21 - 1
	unit := func(shift uint) float64 { return float64((v>>shift)&mask) / mask }
	// Keep a margin so no drone sits exactly on the edge of the airspace.
	place := func(u float64) float64 { return 5 + u*(Size-10) }
	return protocol.Position{X: place(unit(0)), Y: place(unit(21)), Z: place(unit(42))}
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return min(max(v, lo), hi)
}
