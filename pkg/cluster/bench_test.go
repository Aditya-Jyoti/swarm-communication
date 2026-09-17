package cluster

import (
	"fmt"
	"testing"

	"swarm-net/pkg/protocol"
)

// Baselines for the membership merge path, taken before gossip and anti-entropy
// are layered on top of it in Phase 3b. Once every peer periodically ships a
// digest, Upsert is the hot path: it runs once per member per round per peer, so
// allocations per op -- not wall time -- are what decide whether the design
// scales. Every benchmark therefore reports allocs.
//
// Sizes span two orders of magnitude so superlinear behaviour is visible rather
// than inferred. Snapshot sorts, so it is expected to be O(n log n); Upsert is a
// map write and must stay flat per op.

// benchSizes is the swarm size sweep: small, plausible, and larger than we ever
// expect to run, so the shape of the curve is legible.
var benchSizes = []int{10, 100, 1000}

// benchMembers builds n member records with zero-padded IDs. The padding keeps
// lexicographic order equal to numeric order, which matters because Snapshot
// sorts by ID and we want the sort to see realistic, not pathological, input.
func benchMembers(n int) []Member {
	out := make([]Member, n)
	for i := range out {
		id := protocol.NodeID(fmt.Sprintf("node-%04d", i))
		out[i] = Member{
			ID:          id,
			Addr:        protocol.NodeAddress(string(id) + ":7946"),
			Role:        RoleWorker,
			State:       StateAlive,
			Incarnation: 1,
			Score:       float64(i%7) + 0.5,
		}
	}
	// A realistic table has a few leaders, so Leaders() is not measured against
	// an empty result that never grows its slice.
	for i := 0; i < n; i += 10 {
		out[i].Role = RoleLeader
	}
	return out
}

// benchTable returns a table populated with the given records.
func benchTable(ms []Member) *Table {
	tbl := NewTable()
	for _, m := range ms {
		tbl.Upsert(m)
	}
	return tbl
}

func benchEachSize(b *testing.B, fn func(b *testing.B, n int)) {
	b.Helper()
	for _, n := range benchSizes {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			fn(b, n)
		})
	}
}

// The overwhelmingly common anti-entropy case: the digest tells us exactly what
// we already know. The merge falls through to the equal-incarnation comparison
// and returns false without writing. This is the steady-state cost of gossip.
func BenchmarkTableUpsertCurrent(b *testing.B) {
	benchEachSize(b, func(b *testing.B, n int) {
		ms := benchMembers(n)
		tbl := benchTable(ms)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Rotate keys so the map lookup sees the whole keyspace rather than
			// one permanently cached bucket.
			if tbl.Upsert(ms[i%n]) {
				b.Fatal("a current record reported a change")
			}
		}
	})
}

// The real state change: a node speaking about itself after a restart. Every
// iteration must advance the incarnation, or after the first op we would be
// re-measuring the rejected-stale branch instead.
//
// Using int64(i)+2 as the incarnation keeps that advance constant-cost per op:
// a given key is revisited every n iterations and its incarnation jumps by n,
// which is still strictly increasing, so the accepted branch is taken every
// time without any per-op bookkeeping.
func BenchmarkTableUpsertAdvanceIncarnation(b *testing.B) {
	benchEachSize(b, func(b *testing.B, n int) {
		ms := benchMembers(n)
		tbl := benchTable(ms)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			m := ms[i%n]
			m.Incarnation = int64(i) + 2
			if !tbl.Upsert(m) {
				b.Fatal("a higher incarnation was rejected")
			}
		}
	})
}

// The third branch: a stale rumour arriving late. It returns before any write,
// so the table is identical at every iteration and the numbers stay comparable
// across b.N. Worth measuring apart from the no-op merge because it exits
// earlier -- it never reaches the state, score and address comparisons.
func BenchmarkTableUpsertStale(b *testing.B) {
	benchEachSize(b, func(b *testing.B, n int) {
		ms := benchMembers(n)
		for i := range ms {
			ms[i].Incarnation = 100
		}
		tbl := benchTable(ms)

		stale := make([]Member, n)
		copy(stale, ms)
		for i := range stale {
			stale[i].Incarnation = 1
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if tbl.Upsert(stale[i%n]) {
				b.Fatal("a stale rumour was applied")
			}
		}
	})
}

// Snapshot copies and sorts the whole table on every call, and the election
// evaluates against a fresh one each round. It is the per-round floor cost.
func BenchmarkTableSnapshot(b *testing.B) {
	benchEachSize(b, func(b *testing.B, n int) {
		tbl := benchTable(benchMembers(n))

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = tbl.Snapshot()
		}
	})
}

// The View accessors each build a fresh collection. They are cheap in isolation
// and run on every election evaluation, so their combined allocation footprint
// is what shows up in a gossip round. Size is not benchmarked separately: it is
// Alive plus a len.
func BenchmarkViewAccessors(b *testing.B) {
	accessors := []struct {
		name string
		fn   func(View)
	}{
		{"Alive", func(v View) { _ = v.Alive() }},
		{"Scores", func(v View) { _ = v.Scores() }},
		{"Leaders", func(v View) { _ = v.Leaders() }},
		{"Addresses", func(v View) { _ = v.Addresses() }},
	}
	for _, a := range accessors {
		b.Run(a.name, func(b *testing.B) {
			benchEachSize(b, func(b *testing.B, n int) {
				v := benchTable(benchMembers(n)).Snapshot()

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					a.fn(v)
				}
			})
		})
	}
}
