package geo

import (
	"fmt"
	"math"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

func TestDelayGrowsWithDistance(t *testing.T) {
	p := Params{BaseMS: 1, PerUnitMS: 2}
	a := protocol.Position{X: 0, Y: 0}
	near := Delay(a, protocol.Position{X: 3, Y: 4}, p, 0)
	far := Delay(a, protocol.Position{X: 30, Y: 40}, p, 0)
	// 3D: (2, 3, 6) is 7 units away, so 1 + 7*2 = 15ms.
	if up := Delay(a, protocol.Position{X: 2, Y: 3, Z: 6}, p, 0); up != 15*time.Millisecond {
		t.Errorf("3D = %v, want 15ms", up)
	}
	if near != 11*time.Millisecond {
		t.Errorf("near = %v, want 11ms (1 + 5*2)", near)
	}
	if far != 101*time.Millisecond {
		t.Errorf("far = %v, want 101ms (1 + 50*2)", far)
	}
}

func TestDelayJitterScalesWithDraw(t *testing.T) {
	p := Params{JitterMS: 10}
	a := protocol.Position{}
	if got := Delay(a, a, p, 0.5); got != 5*time.Millisecond {
		t.Errorf("Delay with u=0.5 = %v, want 5ms", got)
	}
	if got := Delay(a, a, p, 2); got != 10*time.Millisecond {
		t.Errorf("u is clamped to 1: got %v, want 10ms", got)
	}
}

func TestDelayIsCapped(t *testing.T) {
	p := Params{BaseMS: 1e9, PerUnitMS: 1e9, JitterMS: 1e9}
	got := Delay(protocol.Position{}, protocol.Position{X: Size, Y: Size, Z: Size}, p, 0.99)
	if got > MaxDelay || got <= 0 {
		t.Errorf("Delay = %v, want in (0, %v]", got, MaxDelay)
	}
}

func TestClampRejectsNaNAndNegatives(t *testing.T) {
	c := Params{BaseMS: math.NaN(), PerUnitMS: -3, JitterMS: 1e6}.Clamp()
	if c.BaseMS != 0 || c.PerUnitMS != 0 || c.JitterMS != MaxJitterMS {
		t.Errorf("Clamp = %+v", c)
	}
	pos := ClampPosition(protocol.Position{X: -1, Y: math.Inf(1), Z: 250})
	if pos.X != 0 || pos.Y != Size || pos.Z != Size {
		t.Errorf("ClampPosition = %+v", pos)
	}
}

// Names that differ only in a trailing digit, as Compose replicas do, must
// still spread across the space on every axis.
func TestDefaultPositionSpreadsSimilarNames(t *testing.T) {
	var pos []protocol.Position
	for i := 1; i <= 12; i++ {
		pos = append(pos, DefaultPosition(protocol.NodeID(fmt.Sprintf("swarm-net-node-%d", i))))
	}
	for i := range pos {
		for j := i + 1; j < len(pos); j++ {
			if d := Distance(pos[i], pos[j]); d < 3 {
				t.Errorf("node-%d and node-%d are %.2f apart: %+v %+v", i+1, j+1, d, pos[i], pos[j])
			}
		}
	}
	axes := []func(protocol.Position) float64{
		func(p protocol.Position) float64 { return p.X },
		func(p protocol.Position) float64 { return p.Y },
		func(p protocol.Position) float64 { return p.Z },
	}
	for a, get := range axes {
		lo, hi := Size, 0.0
		for _, p := range pos {
			lo, hi = min(lo, get(p)), max(hi, get(p))
		}
		if hi-lo < 40 {
			t.Errorf("axis %d spans only %.1f units across 12 nodes", a, hi-lo)
		}
	}
}

// TestDefaultPositionGolden pins exact values; frontend/app.js mirrors this
// function and must produce the same numbers.
func TestDefaultPositionGolden(t *testing.T) {
	got := DefaultPosition("swarm-net-node-1")
	want := protocol.Position{X: 43.078932, Y: 19.334290, Z: 75.466986}
	if math.Abs(got.X-want.X) > 1e-6 || math.Abs(got.Y-want.Y) > 1e-6 || math.Abs(got.Z-want.Z) > 1e-6 {
		t.Errorf("DefaultPosition(swarm-net-node-1) = %+v, want %+v (update frontend/app.js too)", got, want)
	}
}

func TestDefaultPositionIsStableAndOnTheMap(t *testing.T) {
	a := DefaultPosition("swarm-net-node-1")
	if b := DefaultPosition("swarm-net-node-1"); a != b {
		t.Errorf("not stable: %+v vs %+v", a, b)
	}
	if c := DefaultPosition("swarm-net-node-2"); c == a {
		t.Errorf("two ids share a position: %+v", a)
	}
	for _, id := range []protocol.NodeID{"", "seed", "x", "swarm-net-node-42"} {
		p := DefaultPosition(id)
		if p.X < 5 || p.X > Size-5 || p.Y < 5 || p.Y > Size-5 || p.Z < 5 || p.Z > Size-5 {
			t.Errorf("DefaultPosition(%q) = %+v, off the map", id, p)
		}
	}
}
