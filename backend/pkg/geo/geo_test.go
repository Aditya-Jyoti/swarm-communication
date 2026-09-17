package geo

import (
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
