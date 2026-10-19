package mavlink

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"swarm-net/pkg/protocol"
)

func TestTrailingSysID(t *testing.T) {
	tests := []struct {
		node string
		want byte
		ok   bool
	}{
		{"swarm-net-node-3", 3, true},
		{"swarm-net-node-12", 12, true},
		{"node254", 254, true},
		{"12", 12, true},
		// Leading zeroes are still digits; "node-0003" is node 3.
		{"node-0003", 3, true},
		{"seed", 0, false},
		{"", 0, false},
		{"node-3x", 0, false},
		// 0 is the broadcast address and 255 is the ground station's own id, so
		// neither can be a vehicle even though both parse.
		{"node-0", 0, false},
		{"node-255", 0, false},
		// Out of range falls through to the lowest-free path rather than being
		// clamped or wrapped: vehicle 44 or vehicle 255 would both be worse than
		// an arbitrary free id.
		{"node-300", 0, false},
		{"node-1000", 0, false},
		// A digit run too long for a uint64 must not overflow into range.
		{"node-99999999999999999999999", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.node, func(t *testing.T) {
			got, ok := trailingSysID(tc.node)
			if got != tc.want || ok != tc.ok {
				t.Errorf("trailingSysID(%q) = (%d, %v), want (%d, %v)", tc.node, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSysIDAssignment(t *testing.T) {
	v := NewVehicles()

	// Rule 1: trailing digits win, so a packet capture can be read without a
	// lookup table.
	if id, err := v.SysID("swarm-net-node-3"); err != nil || id != 3 {
		t.Fatalf("node-3 got (%d, %v), want 3", id, err)
	}
	// Rule 2: no digits means highest free, so the low ids stay clear for the
	// nodes that have a claim on them. Assigning upward instead let "seed" take
	// id 1 and pushed every numbered node off its own number.
	if id, err := v.SysID("seed"); err != nil || id != 254 {
		t.Fatalf("seed got (%d, %v), want 254", id, err)
	}
	if id, err := v.SysID("control"); err != nil || id != 253 {
		t.Fatalf("control got (%d, %v), want 253", id, err)
	}
	if id, err := v.SysID("observer"); err != nil || id != 252 {
		t.Fatalf("observer got (%d, %v), want 252", id, err)
	}
	// A collision on the preferred id is a naming clash, not an error: the second
	// node still gets a working vehicle.
	if id, err := v.SysID("other-3"); err != nil || id != 251 {
		t.Fatalf("other-3 got (%d, %v), want 251 (highest free after a collision)", id, err)
	}

	if v.Len() != 5 {
		t.Errorf("Len = %d, want 5", v.Len())
	}
}

func TestSysIDIsSticky(t *testing.T) {
	v := NewVehicles()

	first, err := v.SysID("swarm-net-node-7")
	if err != nil {
		t.Fatalf("SysID: %v", err)
	}
	// Idempotent: the bridge asks on every telemetry tick.
	for i := 0; i < 5; i++ {
		again, err := v.SysID("swarm-net-node-7")
		if err != nil || again != first {
			t.Fatalf("call %d got (%d, %v), want %d", i, again, err, first)
		}
	}

	// A node going down must NOT free its id. Nothing is called here, which is
	// the point: a killed node's vehicle is held so that the ground station sees
	// the link return rather than a second aircraft appear.
	if _, err := v.SysID("filler-a"); err != nil {
		t.Fatalf("SysID: %v", err)
	}
	if node, ok := v.Node(first); !ok || node != "swarm-net-node-7" {
		t.Errorf("Node(%d) = (%q, %v), want the original node", first, node, ok)
	}
	if again, _ := v.SysID("swarm-net-node-7"); again != first {
		t.Errorf("restarted node got sysid %d, want %d", again, first)
	}

	// Release is the explicit, deliberate way to give an id back.
	v.Release("swarm-net-node-7")
	if _, ok := v.Lookup("swarm-net-node-7"); ok {
		t.Error("Lookup still resolves a released node")
	}
	if _, ok := v.Node(first); ok {
		t.Errorf("sysid %d still maps to a node after Release", first)
	}
	// Releasing twice, or releasing an unknown node, must be a no-op.
	v.Release("swarm-net-node-7")
	v.Release("never-seen")
}

func TestLookupDoesNotAssign(t *testing.T) {
	v := NewVehicles()
	if id, ok := v.Lookup("swarm-net-node-3"); ok {
		t.Fatalf("Lookup on an unknown node returned (%d, true)", id)
	}
	if v.Len() != 0 {
		t.Errorf("Lookup assigned an id: Len = %d", v.Len())
	}
}

func TestSysIDExhaustion(t *testing.T) {
	v := NewVehicles()

	// Fill every assignable id. Digit-less names, so every assignment goes
	// through the lowest-free path.
	for i := 0; i < int(MaxSysID); i++ {
		node := protocol.NodeID(fmt.Sprintf("node-%c%c", 'a'+i/26, 'a'+i%26))
		if _, err := v.SysID(node); err != nil {
			t.Fatalf("assigning %q (number %d): %v", node, i, err)
		}
	}
	if v.Len() != int(MaxSysID) {
		t.Fatalf("Len = %d, want %d", v.Len(), MaxSysID)
	}

	_, err := v.SysID("one-too-many")
	if !errors.Is(err, ErrNoSysID) {
		t.Errorf("error = %v, want ErrNoSysID", err)
	}
	// An already-known node must still resolve when the table is full: the
	// exhaustion path must not shadow the lookup.
	if _, err := v.SysID("node-aa"); err != nil {
		t.Errorf("known node failed on a full table: %v", err)
	}
	// And freeing one id must let exactly one more node in.
	v.Release("node-aa")
	if _, err := v.SysID("one-too-many"); err != nil {
		t.Errorf("after Release: %v", err)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	v := NewVehicles()
	if _, err := v.SysID("swarm-net-node-3"); err != nil {
		t.Fatal(err)
	}

	snap := v.Snapshot()
	snap["swarm-net-node-3"] = 99
	delete(snap, "swarm-net-node-3")
	snap["injected"] = 1

	if id, ok := v.Lookup("swarm-net-node-3"); !ok || id != 3 {
		t.Errorf("mutating the snapshot changed the table: got (%d, %v)", id, ok)
	}
	if _, ok := v.Lookup("injected"); ok {
		t.Error("snapshot shares storage with the table")
	}
}

// TestSysIDConcurrent is the test that justifies the mutex. The invariant is
// global -- no two nodes may hold the same id -- so it cannot be upheld by
// anything that does not cover the whole read-decide-write sequence. Run under
// -race it also catches a plain unsynchronised map.
func TestSysIDConcurrent(t *testing.T) {
	v := NewVehicles()
	const goroutines = 16
	const perGoroutine = 8

	var wg sync.WaitGroup
	ids := make([][]byte, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// Half the nodes have digits that collide on purpose, so the
				// collision fallback is exercised concurrently too.
				node := protocol.NodeID(fmt.Sprintf("node-%d", i))
				if i%2 == 0 {
					node = protocol.NodeID(fmt.Sprintf("worker-%d-%d", g, i))
				}
				id, err := v.SysID(node)
				if err != nil {
					t.Errorf("SysID(%q): %v", node, err)
					return
				}
				ids[g] = append(ids[g], id)
				// The same node must always come back with the same id, even
				// while other goroutines are assigning.
				if again, _ := v.SysID(node); again != id {
					t.Errorf("SysID(%q) = %d then %d", node, id, again)
				}
			}
		}(g)
	}
	wg.Wait()

	// Every assigned id maps back to exactly one node, and every node to one id.
	snap := v.Snapshot()
	seen := make(map[byte]protocol.NodeID, len(snap))
	for node, id := range snap {
		if other, dup := seen[id]; dup {
			t.Errorf("sysid %d assigned to both %q and %q", id, other, node)
		}
		seen[id] = node
		if back, ok := v.Node(id); !ok || back != node {
			t.Errorf("Node(%d) = (%q, %v), want %q", id, back, ok, node)
		}
	}
	if len(snap) != v.Len() {
		t.Errorf("Snapshot has %d entries, Len = %d", len(snap), v.Len())
	}
}

func TestVehiclesZeroValueMapsAreInitialised(t *testing.T) {
	// NewVehicles is the intended constructor, but a zero Vehicles embedded in a
	// larger struct must not panic on first use -- that failure would land in
	// production and not in a test.
	var v Vehicles
	id, err := v.SysID("swarm-net-node-9")
	if err != nil || id != 9 {
		t.Fatalf("zero-value SysID = (%d, %v), want 9", id, err)
	}
	if node, ok := v.Node(9); !ok || node != "swarm-net-node-9" {
		t.Errorf("Node(9) = (%q, %v)", node, ok)
	}
}

// TestSysIDIgnoresArrivalOrder is a regression test for a bug that reached a
// running swarm: ids were allocated upward from 1, so "seed" (no digits, and it
// sorts first) took id 1 and every numbered node was pushed off its own number.
// A ground station then showed node-3 as vehicle 4, and a kill of node-3
// reported vehicle 4 going down -- the wrong aircraft.
//
// A node with digits must get its number whatever order the swarm connects in.
func TestSysIDIgnoresArrivalOrder(t *testing.T) {
	orders := [][]protocol.NodeID{
		{"seed", "swarm-net-node-1", "swarm-net-node-2", "swarm-net-node-3"},
		{"swarm-net-node-3", "seed", "swarm-net-node-1", "swarm-net-node-2"},
		{"swarm-net-node-2", "swarm-net-node-1", "swarm-net-node-3", "seed"},
	}
	for _, order := range orders {
		t.Run(string(order[0])+"-first", func(t *testing.T) {
			v := NewVehicles()
			for _, node := range order {
				if _, err := v.SysID(node); err != nil {
					t.Fatalf("SysID(%q): %v", node, err)
				}
			}
			for node, want := range map[protocol.NodeID]byte{
				"swarm-net-node-1": 1,
				"swarm-net-node-2": 2,
				"swarm-net-node-3": 3,
				"seed":             254,
			} {
				if got, _ := v.Lookup(node); got != want {
					t.Errorf("%s got sysid %d, want %d", node, got, want)
				}
			}
		})
	}
}
