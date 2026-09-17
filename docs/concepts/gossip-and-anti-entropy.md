---
title: "Gossip and Anti-Entropy"
description: "How this swarm spreads membership: push-on-change for first-hand news, shuffled round-robin anti-entropy for repair, the N x T_gossip bound and the tombstone TTL it sizes, Seq-ordered relays, why a full view is just a large delta, and why gossip converges but never agrees."
outline: deep
---

# Gossip and Anti-Entropy

No node holds the membership list. Every node holds its own *belief*, and the beliefs converge
because nodes keep telling each other what they know. That is gossip. It is what
`MEMBERSHIP_DELTA` carries, and it is why two nodes can hold different $N$ at the moment they
each run an election.

This page covers how a record travels, the repair bound, and the one thing
gossip does not give you. The merge rule that makes it all safe has its own page:
[Monotonic Merge and Incarnation](/concepts/monotonic-merge-and-incarnation).

---

## Core Mental Model

### Two paths, two jobs

| Path | Sends | To | When | Job |
|---|---|---|---|---|
| **Push** (`pushRecord`) | one record | every connected peer | this node saw a death, departure or suspicion first-hand | fast |
| **Announce** (`announceSelf`) | this node's own record | every connected peer | its own score, role or incarnation changed | fast |
| **Anti-entropy** (`gossipRound`) | the full view | ONE alive peer | every $T_{gossip}$ (default 2 s) | repair |

A push is fast but lossy: a dropped frame or a full send queue leaves a hole. Anti-entropy is
the slow, reliable sweep that fills holes. The name is literal: it reduces the disorder between
two views.

```mermaid
sequenceDiagram
    participant A as node-a
    participant B as node-b
    participant C as node-c
    Note over A: PeerDown for node-x
    Note over A: markDead records x dead at inc 8
    A->>B: MEMBERSHIP_DELTA [x dead 8] push
    A-xC: MEMBERSHIP_DELTA [x dead 8] push lost
    Note over B: merged. B does NOT re-push
    Note over A,C: later, A gossip tick picks C
    A->>C: MEMBERSHIP_DELTA [full view]
    Note over C: x dead 8 merges. Hole repaired
```

### Epidemics, and why they barely apply here

Classic gossip (Demers et al., 1987) assumes a node can only reach a few random peers per
round. A rumour then spreads like an infection, in about $\log_{1+f} N$ rounds for fanout $f$.

This swarm is a **full mesh** (every node holds a connection to every other). One broadcast
reaches everyone in one hop, so there is no infection curve to climb. What the full mesh does
*not* remove is loss. That is the only reason anti-entropy exists here: not to spread news,
but to repair news that did not arrive.

### Only first-hand news is pushed

`pushRecord` fires from `markDead`, `markLeft` and a new first-hand suspicion only
(`pkg/cluster/node.go:944`, `pkg/cluster/node.go:967`, `pkg/cluster/failure.go:98`). A record
merged from someone else's delta is not re-pushed. A suspicion is pushed because the suspect
itself is among the receivers, and hearing the rumour is its only way to refute it.

| Rule | Frames per death |
|---|---|
| push on first-hand observation | about $N$ per observer |
| push on every receipt too | about $N^2$ per observer, most of them no-ops |

In a full mesh the original broadcast already reached everyone, so re-pushing buys nothing
except $N^2$ traffic. A crash that *every* node observes still costs about $N^2$ frames (each
observer broadcasts once), which the comment at `pkg/cluster/node.go:983` accepts because
deaths are rare.

### A full view is just a large delta

There is no snapshot message. `MembershipDeltaPayload` (`pkg/protocol/message.go:401`) is
the only membership payload, and its comment (`pkg/protocol/message.go:397`) says a delta
"asserts facts about the members it names and says nothing about members it omits". The
receiver merges; it never replaces.

That works because `Table.Upsert` (`pkg/cluster/member.go:225`) is per record and
idempotent. Merging a record you already hold returns `false`. So a full view of 50 is 50
independent upserts, most of them no-ops, and the welcome view (`pkg/cluster/node.go:870`),
the gossip view and a one-record push all run through the same code.

---

## Under the Hood

### One gossip round

```go
// pkg/cluster/node.go:1052 (abridged)
func (n *Node) gossipRound(ctx context.Context) {
	view := n.table.Snapshot()
	if view.Version != n.gossipVersion {
		n.gossipVersion = view.Version
		if !n.sameGossipSet(view) {
			n.gossipNext = len(n.gossipOrder) // force a redraw below
		}
	}
	if n.gossipNext >= len(n.gossipOrder) {
		// rebuild gossipOrder from the alive peers, excluding self
		n.cfg.Shuffle(n.gossipOrder)
		n.gossipNext = 0
	}
	if len(n.gossipOrder) == 0 {
		return
	}
	peer := n.gossipOrder[n.gossipNext]
	n.gossipNext++
	n.sendView(ctx, peer, view)
}
```

It runs on the node's single event loop, from its own ticker
(`pkg/cluster/node.go:618`, handled at `pkg/cluster/node.go:665`). No lock is held across the
send, and there is no extra goroutine.

### Peer selection: shuffled round-robin, k = 1

```mermaid
flowchart LR
    T[gossip tick] --> V{table version changed?}
    V -->|no| C{cursor at end?}
    V -->|yes| S{alive set changed?}
    S -->|no| C
    S -->|yes| R[force redraw]
    R --> C
    C -->|yes| D[rebuild alive list and Shuffle]
    C -->|no| P[pick order at cursor]
    D --> P
    P --> F[sendView full view to that peer]
```

| Choice | Why |
|---|---|
| one peer per round ($k = 1$) | a full view is $O(N)$ bytes. $k$ peers per round costs $k$ times that |
| a permutation, not a random pick | a random pick has no bound: by chance a peer can go unvisited for many rounds |
| shuffled, not ID order | ID order makes every node target the same peer on the same tick |
| redraw only when the *alive set* changes | most version bumps are score reports. Redrawing on each would keep resetting the cursor and quietly turn the permutation back into random selection |

`Shuffle` is a config seam (`pkg/cluster/node.go:144`), defaulting to `math/rand/v2`
(`pkg/cluster/node.go:260`). Tests inject a deterministic one.

### The repair bound

Let $P$ be the number of alive peers, so $P = N - 1$. One cycle of the permutation takes $P$
rounds, and every alive peer is visited once in it. A lost death push is repaired within

$$
t_{repair} \le N \times T_{gossip}
$$

The bound holds for a death because the death itself changes the alive set, which forces a
fresh permutation on the next tick. The peer that missed the push is then at most $P$ rounds
away, plus up to one tick of waiting for that first round.

Two caveats worth knowing:

- **A lost update that does not change the alive set** (for example a self-announcement) gets
  no fresh permutation. A peer visited first in one cycle and last in the next waits
  $2P - 1$ rounds, so its worst case is nearly $2N \times T_{gossip}$.
- **Continuous churn** keeps forcing redraws, and a peer can keep landing late in each new
  permutation. The bound assumes the alive set settles.

| $N$ | $T_{gossip}$ | $N \times T_{gossip}$ |
|---|---|---|
| 10 | 2 s | 20 s |
| 50 | 2 s | 100 s |
| 50 | 200 ms | 10 s |

This is a *repair* bound, not the normal path. Normally the push arrives in milliseconds.
Tune it with `SWARM_GOSSIP_INTERVAL` or `-gossip-interval` (`cmd/swarm-node/config.go:95`).

### The repair bound sizes the tombstone TTL

A death record (tombstone) is collected after `TombstoneTTL`, 60 s by default
(`pkg/cluster/node.go:55`). It must outlive every stale "alive" copy, so it must outlive the
worst-case repair, which the code takes as the non-alive-set bound
(`pkg/cluster/failure.go:219`):

$$
TTL > (2N - 1) \cdot T_{gossip}
$$

| $N$ | $(2N - 1) \cdot 2\,\text{s}$ | 60 s enough? |
|---|---|---|
| 10 | 38 s | yes |
| 15 | 58 s | barely |
| 25 | 98 s | no |

Past about $N = 15$ at the default interval, raise the TTL or shorten $T_{gossip}$. A
tombstone collected too early lets a stale "alive" record **re-infect** the node. The ghost is
then probed, fails, and dies again, which costs one more tombstone cycle but is not permanent.

One more rule stops collected tombstones from bouncing: a relayed dead record for a member this
node does not hold is dropped, not inserted (`pkg/cluster/node.go:1256`). Otherwise two nodes
that collected the same death seconds apart would hand it back and forth forever, each time
restarting the other's TTL. Details on
[Monotonic Merge and Incarnation](/concepts/monotonic-merge-and-incarnation).

### Receiving a view: one snapshot, binary search

```go
// pkg/cluster/node.go:1236, 1249-1275 (abridged)
before := n.table.Snapshot()
for _, rec := range p.Members {
	// ...
	i, known := slices.BinarySearchFunc(before.Members, rec.ID, compareMemberID)
	if !known && ParseState(rec.State) == StateDead {
		continue // a tombstone we already collected
	}
	role := ParseRole(rec.Role)
	if rec.Seq == 0 && rec.ID != from {
		// pre-Seq build: believe a role only first-hand
		role = RoleWorker
		if known {
			role = before.Members[i].Role
		}
	}
	// ... Upsert, which keeps role and score unless rec.Seq is newer
}
```

Before commit `4012bf2`, the handler took a `Snapshot` (copy and sort of the whole table) *per
record*. Under anti-entropy every delta is a full view, so that was $O(N^2 \log N)$ on the event
loop. `BenchmarkHandleMembershipDelta` (`pkg/cluster/bench_test.go:184`), steady state:

| $N$ | before | after |
|---|---|---|
| 100 | 817 us, 100 allocs | 14.7 us, 1 alloc |
| 1000 | 125 ms, 1000 allocs | 211 us, 1 alloc |

The binary search is valid only because `Snapshot` sorts by ID
(`pkg/cluster/member.go:456`). `View.Get` scans instead, because it cannot assume an arbitrary
`View` is sorted.

### What gossip can and cannot repair

Role and score are the member's own **claims**, ordered by the member's `Seq`
(`pkg/cluster/member.go:251`). A relay can carry a claim, but it can never make an old claim
win: a relayed record replaces role and score only if its `Seq` is newer than the one held.

Before `Seq` existed, relayed roles were ignored and relayed scores were last-writer-wins by
arrival order. TCP keeps order within a link but not across links, so a full view built before a
score change could land after the announcement of that change, and nodes elected from different
scores. See [Convergence Debugging](/architecture/convergence-debugging).

| Field in a gossiped record | Repaired by anti-entropy? |
|---|---|
| death or suspicion of a third node | yes |
| the *sender's own* role and score | yes, the view includes the sender's own record |
| a third node's role and score | yes, if the relay's `Seq` is newer (`TestRelayedClaimsAreOrderedBySeq`) |
| a third node's claims from a pre-`Seq` build | score yes, role **no** |

---

## Why It Matters in This Swarm

### Gossip converges. It does not agree.

After enough rounds every view is the same. But at any given instant two views may differ, and
no node ever *knows* that everyone holds its view. Convergence is a property of the limit.
Agreement is a property of a moment, and gossip has no such moment.

`Elect` (`pkg/cluster/election.go:126`) runs on a `View` snapshot and sizes itself from
its electorate (`pkg/cluster/election.go:129`). Two nodes with different views can compute
different leader sets. `ELECTION_RESULT` (`pkg/protocol/message.go:85`) makes that visible,
and the next gossip round makes the views, and then the elections, converge. Quorum reasoning
needs an *agreed* $n$ -- see [Split-Brain and Quorum](/concepts/split-brain-and-quorum).

### Where the mechanism lives

| What | Where |
|---|---|
| gossip interval default (2 s) | `pkg/cluster/node.go:31` |
| config fields `GossipInterval`, `Shuffle` | `pkg/cluster/node.go:141`, `pkg/cluster/node.go:144` |
| welcome view on every `PeerUp` | `pkg/cluster/node.go:870` |
| first-hand death or suspicion push | `pkg/cluster/node.go:986` |
| full view send | `pkg/cluster/node.go:1004` |
| departure kept as a dead record | `pkg/cluster/node.go:961` |
| self-rumour refutation | `pkg/cluster/node.go:1369` |
| bootstrap seed list in `HELLO` | `pkg/protocol/message.go:264` |
| unknown message types are ignored, not fatal | `pkg/protocol/message.go:30` |

Tests: `pkg/cluster/gossip_test.go` covers one-visit-per-cycle, redraw-only-on-set-change,
push of first-hand deaths only, and the dropped-push repair over real nodes.

---

## Common Failure Modes & Edge Cases

### The immortal alive record

Symptom: a killed node keeps coming back to "alive" in every view, round after round. Cause: a
death recorded at the victim's *current* incarnation loses to a newer "alive" still circulating,
and anti-entropy re-asserts that record every round. Fixed by bumping the incarnation on death.
The full interleaving is on
[Monotonic Merge and Incarnation](/concepts/monotonic-merge-and-incarnation).

### The departed node that re-joins by echo

Symptom: a node that sent `LEAVE` reappears as alive a few seconds later, although its process
is gone. Cause: deleting the record on `LEAVE`. The next peer that had not heard the `LEAVE`
gossips its old "alive" record, and with nothing to outrank it, `Upsert` inserts it as new.
This is why `markLeft` keeps a dead record (`pkg/cluster/node.go:961`), and why the only
`Table.Remove` call is tombstone collection after the TTL (`pkg/cluster/failure.go:263`).

### Stale scores from relays

Symptom: nodes hold different scores for the same member, elect different leaders, and never
settle, under real (reordering) links but not in an in-process test. Cause: role or score
merged by arrival order instead of by `Seq`. Check the `seq` values in `/api/state` peers.

### Tombstone ping-pong

Symptom: a long-dead member keeps reappearing in views as dead, and `tombstone collected`
logs repeat for it. Cause: relayed dead records re-inserted for members already collected. The
receiver must drop them (`pkg/cluster/node.go:1256`).

### Replace instead of merge

Symptom: a node's member count drops to 1 or 2 on every push, then recovers on a gossip tick.
Cause: treating `MEMBERSHIP_DELTA` as a complete view. A one-record push then evicts
everyone it did not name.

### Gossip that looks random

Symptom: under steady score reporting, some peer is occasionally not repaired for far longer
than $N \times T_{gossip}$. Cause: redrawing the permutation on every table version change
instead of on alive-set changes. `TestGossipRedrawsOnlyWhenAlivePeersChange` pins this.

### The event loop stalls on large views

Symptom: at a few hundred nodes, heartbeats and probe results queue up behind membership
handling, and nodes start timing each other out. Cause: per-record snapshots or linear
lookups in the delta handler. One `MEMBERSHIP_DELTA` at $N = 1000$ used to take 125 ms of loop
time.

### Interval too long for the swarm size

Symptom: after a partition heals, some nodes disagree on who is dead for minutes. Cause:
$N \times T_{gossip}$ grew with $N$ and nobody retuned. The cost of shortening $T_{gossip}$ is
one full view ($O(N)$ bytes) per node per tick, so total bytes per second grow as
$N^2 / T_{gossip}$.

---

## See Also

- [Monotonic Merge and Incarnation](/concepts/monotonic-merge-and-incarnation) -- the merge
  rule, and why death bumps the incarnation.
- [Failure Detectors](/concepts/failure-detectors) -- who decides a node is dead in the first
  place.
- [Split-Brain and Quorum](/concepts/split-brain-and-quorum) -- why a converging $n$ is not an
  agreed $n$.
- [Why Not Consensus](/architecture/why-not-consensus) -- the system-level consequence.
- [Wire Protocol Design](/concepts/wire-protocol-design) -- the framing `MEMBERSHIP_DELTA` rides
  on.
