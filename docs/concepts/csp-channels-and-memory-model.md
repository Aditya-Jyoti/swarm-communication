---
title: CSP, Channels & The Go Memory Model
outline: deep
---

# CSP, Channels & The Go Memory Model

Every concurrent bug in a swarm node comes from one of two places: two goroutines
disagreeing about *when* something happened, or two goroutines disagreeing about *what
the value currently is*. Channels are Go's answer to the first. The memory model is the
rulebook that tells you when the second is actually defined behaviour and when it is not.

This page covers both, plus the honest case for not using channels at all.

Related reading: [The GMP Scheduler](./go-scheduler-gmp) explains the goroutine states a
blocked channel operation moves through; [Context & Cancellation
Propagation](./context-cancellation) builds directly on closed-channel semantics;
[The Netpoller](./go-netpoller) explains the other major reason a goroutine parks.

---

## Core Mental Model

### Communicating Sequential Processes

Hoare's CSP model says: a concurrent program is a set of independent sequential processes
that own their own state exclusively, and coordinate only by passing messages over named
channels. There is no shared memory to protect, because there is no shared memory.

Go's slogan is the operational version of this:

> Do not communicate by sharing memory; share memory by communicating.

The practical reading is narrower than the slogan suggests. It does **not** say "never use
a mutex". It says: when a *value* needs to move from one goroutine's control to another's,
move it, and let the transfer itself be the synchronisation. Ownership is transferred, not
shared.

```
  Shared-memory model                   CSP model
  -------------------                   ---------

   G1 ----\                              G1 --[ v ]--> ch --[ v ]--> G2
           >--> [ mutex ][ state ]
   G2 ----/                              G1 no longer touches v.
                                         G2 now owns v exclusively.
   Both goroutines may touch
   state at any time; the lock
   only serialises access. The
   invariant lives in your head.        The invariant lives in the type system:
                                        only one goroutine holds the value.
```

### When a mutex is simply the right answer

This is the part most Go tutorials get wrong. Channels are a *transfer* primitive. They are
a poor fit for *state* that is inherently shared, long-lived, and read far more often than
it is written.

The swarm's cluster membership table is exactly that shape: one authoritative map of peer
identity -> role -> health score, written by heartbeat goroutines every few hundred
milliseconds and read by the telemetry path, the election path, and the dashboard fan-out,
possibly many times per second.

Modelling it as CSP means funnelling every read through a request channel into a single
owner goroutine, which then replies on a per-request reply channel:

```go
// The "actor" pattern. Correct, and usually the wrong tool for read-mostly state.
type query struct {
	id    string
	reply chan Peer
}

func (t *table) run() {
	state := map[string]Peer{}
	for {
		select {
		case q := <-t.queries:
			q.reply <- state[q.id] // two channel ops + a heap allocation per read
		case u := <-t.updates:
			state[u.ID] = u
		}
	}
}
```

That is two channel operations, two scheduler parks, and one allocation for what a
`RWMutex` resolves with an uncontended atomic increment. Worse, it serialises reads that
could have proceeded in parallel, and it introduces a single goroutine whose death takes
the whole subsystem with it.

The heuristic:

| Situation | Reach for |
| --- | --- |
| Handing a unit of work to a worker | channel |
| Signalling "stop" / "done" to N goroutines | `close(chan struct{})` |
| Collecting results from a fan-out | channel |
| Guarding a struct's invariants across several fields | `sync.Mutex` |
| Read-mostly lookup table | `sync.RWMutex` or `atomic.Pointer` |
| A single counter or flag | `sync/atomic` |
| Anything where you find yourself sending a pointer and *also* locking it | you have both problems; pick one |

A useful tell: if the value you are about to send over a channel is a pointer that the
sender will keep touching, you are not doing CSP. You are doing shared memory with extra
scheduling overhead.

---

## Under the Hood

### hchan

A channel value is a pointer to a runtime `hchan`. Conceptually (field names match
`runtime/chan.go`):

```
hchan
+---------------------+
| qcount    uint       |  elements currently in the buffer
| dataqsiz  uint       |  buffer capacity (0 for unbuffered)
| buf       unsafe.Ptr |  pointer to a dataqsiz-element ring array
| elemsize  uint16     |
| closed    uint32     |
| elemtype  *_type     |
| sendx     uint       |  ring write index
| recvx     uint       |  ring read index
| recvq     waitq      |  linked list of blocked receivers (sudog)
| sendq     waitq      |  linked list of blocked senders (sudog)
| lock      mutex      |  runtime mutex guarding ALL of the above
+---------------------+
```

The buffer is a plain ring:

```
 dataqsiz = 4, qcount = 2

   idx:    0     1     2     3
        +-----+-----+-----+-----+
 buf:   |     |  C  |  D  |     |
        +-----+-----+-----+-----+
                 ^           ^
              recvx=1     sendx=3

 Next receive takes buf[1] and advances recvx to 2.
 Next send writes buf[3] and wraps sendx to 0.
```

`recvq` and `sendq` are queues of `sudog` -- a runtime struct representing "a goroutine
parked on this channel", carrying a `*g` (the goroutine), an `elem` pointer into that
goroutine's stack (where the value should be read from or written to), and the list links.
`sudog`s are pooled per-P to avoid allocating on every block.

Crucially: **every channel operation takes `hchan.lock`.** A channel is a mutex-protected
queue with a scheduler integration bolted on. It is not lock-free, and it is not free. A
buffered channel send on an uncontended channel is roughly an atomic-lock acquire, a memory
copy, an index bump, and a release -- fast, but measurably slower than a bare mutex around
the same copy, because it also has to check the wait queues.

### The direct handoff

The optimisation that makes unbuffered channels viable: if a sender arrives and `recvq` is
non-empty, the runtime does **not** write into the buffer. There may not even be one. It
pops a `sudog` off `recvq` and copies the value *directly from the sender's stack into the
receiver's stack*, via `sudog.elem`, then marks the receiving goroutine runnable.

```
 Unbuffered send with a waiting receiver:

   G1 (sender)                          G2 (parked in recvq)
   +-------------+                      +-------------+
   | v = Peer{...} |   memmove(dst,src)   | var p Peer  |
   |   &v  ------------------------------->  &p       |
   +-------------+                      +-------------+
                                        goready(G2)

   Zero trips through hchan.buf. One copy, not two.
```

The reverse also holds: a receiver arriving at a channel with a non-empty `sendq` on an
unbuffered channel takes the value straight out of the blocked sender's stack.

There is a second-order effect worth knowing. `send` calls `goready` on the receiver with a
hint that encourages the scheduler to run it on the current P soon -- good for latency and
cache locality, because the data was just written by this core and is hot in its L1.

### Unbuffered vs buffered is a statement about synchronisation

`make(chan T)` (unbuffered) means: *the send does not complete until a receiver has taken
the value*. It is a rendezvous. Both goroutines are, for an instant, synchronised -- and you
get a happens-before edge in **both** directions of reasoning: the receiver knows the sender
reached the send, and the sender knows the receiver reached the receive.

`make(chan T, n)` means: *the sender may run ahead of the receiver by up to n items*. That
is a deliberate decoupling with a deliberate bound. The bound is the point: it is
backpressure. An unbounded queue is a memory leak with good manners.

The common mistake is choosing the capacity to "make it faster". If you cannot state what
the capacity *means* -- "the telemetry fan-out may lag the heartbeat loop by at most 64
samples before we start dropping" -- the number is wrong, whatever it is. Capacity 1 has a
specific and useful meaning: a mailbox holding the latest handoff, used so the producer need
not block on a consumer that is momentarily busy.

### select

`select` is compiled to `runtime.selectgo`. The algorithm:

1. Build a list of the cases and generate a random permutation of the poll order
   (`fastrandn`), plus a lock order sorted by channel address.
2. Lock every involved channel in address order -- this is why the lock order is sorted:
   it prevents deadlock between two `select`s covering the same channels.
3. Pass 1: walk cases in the *random* order looking for one that can proceed immediately.
   If found, execute it, unlock, return.
4. If none is ready and there is a `default`, take it, unlock, return.
5. Otherwise enqueue a `sudog` for this goroutine on **every** case's wait queue, unlock all
   channels, and `gopark`.
6. When woken, the waking party has already recorded which case fired. Dequeue the `sudog`s
   from all the other queues and proceed.

Two consequences follow directly.

**The random order prevents starvation.** If cases were polled top-to-bottom, a hot channel
in case 1 would permanently mask case 2. In the swarm's node event loop -- which will
simultaneously select over inbound frames, a heartbeat ticker, and a shutdown signal -- a
node under heavy inbound traffic would never observe its own heartbeat tick. Randomisation
makes the choice among *ready* cases uniform, so every ready case is served in expectation.

Note the precise claim: randomisation is over the cases that are *ready*, not over all
cases. It gives fairness, not priority. If you genuinely need priority, you must express it
as a nested select:

```go
// Shutdown wins over work, deterministically.
select {
case <-ctx.Done():
	return ctx.Err()
default:
}

select {
case <-ctx.Done():
	return ctx.Err()
case f := <-frames:
	handle(f)
}
```

**`default` turns select into a non-blocking poll.** `select { case ch <- v: default: }` is
the standard "drop if the consumer is behind" idiom, which is exactly what a telemetry
broadcast wants: dropping a sample is correct, blocking the heartbeat loop is not.

An empty `select{}` blocks forever and is deadlock-detected. A `select` over a nil channel
never fires -- which is a useful trick: setting a case's channel variable to `nil` disables
that case for subsequent iterations without restructuring the loop.

### Closed channels

The rules, exactly:

- Receive from a closed channel returns the element type's zero value **immediately**, and
  forever. `v, ok := <-ch` gives `ok == false`.
- If the channel is closed but the buffer still has elements, receives drain the buffer
  first and only then start returning zeros.
- Send on a closed channel **panics**.
- `close` on an already-closed channel **panics**.
- `close` on a nil channel **panics**.
- Send or receive on a nil channel blocks forever.

Because close is broadcast -- every current and future receiver observes it, with no value
consumed -- `close(chan struct{})` is the canonical one-to-many signal. This is precisely
what `context.Context.Done()` is (see [Context & Cancellation
Propagation](./context-cancellation)).

**Only the sender closes.** The rule exists because closing is the only channel operation
that can make another goroutine panic. If two goroutines send on a channel, neither can
safely close it: each would have to know the other is finished, which is the very problem
the channel was supposed to solve.

The fan-in exception is not an exception to the rule so much as a way of manufacturing a
single logical sender:

```go
// Fan-in: N producers, one channel. A WaitGroup creates the single
// "all senders are done" event, and exactly one goroutine acts on it.
func merge[T any](srcs ...<-chan T) <-chan T {
	out := make(chan T)
	var wg sync.WaitGroup
	wg.Add(len(srcs))
	for _, src := range srcs {
		// Go 1.22+ gives each iteration its own `src`; no shadow copy needed.
		go func() {
			defer wg.Done()
			for v := range src {
				out <- v
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out) // the closer is a goroutine that sends nothing
	}()
	return out
}
```

If you need to stop a receiver without closing the data channel, close a separate
`done chan struct{}` instead, and have senders `select` on it. This is the shape
`context.Context` generalises.

### The memory model: happens-before

A data race is: two goroutines access the same memory location, at least one access is a
write, and neither access happens-before the other. The Go memory model defines exactly
which edges exist. The ones that matter here:

1. **Goroutine creation.** The `go` statement happens-before the goroutine body begins.
2. **Channel send/receive.** A send on a channel happens-before the corresponding receive
   *completes*.
3. **Unbuffered receive.** A receive from an unbuffered channel happens-before the
   corresponding send *completes*. (This is the reverse edge, and it is what makes
   unbuffered channels a rendezvous rather than a queue of one.)
4. **Buffered capacity.** The *k*-th receive on a channel of capacity *C* happens-before the
   *(k+C)*-th send completes. This is the formal statement of backpressure.
5. **Close.** A `close` happens-before a receive that returns the zero value because of it.
6. **Mutex.** For `sync.Mutex`/`RWMutex`, the *n*-th `Unlock` happens-before the *(n+1)*-th
   `Lock` returns. For `RWMutex`, `Unlock` happens-before any subsequent `RLock` returns,
   and `RUnlock` happens-before a subsequent `Lock` returns.
7. **Once.** The function passed to `Once.Do` completes before any `Do` call returns.
8. **Atomics.** Since Go 1.19, the memory model explicitly specifies `sync/atomic` operations
   as **sequentially consistent**: all atomic operations across the whole program behave as
   if executed in a single total order consistent with each goroutine's program order. An
   atomic store therefore synchronises with an atomic load that observes it, giving a real
   happens-before edge -- not just an indivisible read.

```
  G1                                  G2
  ---------------------------         ---------------------------
  table[id] = peer      (a)
  ch <- token           (b)  -------> tok := <-ch            (c)
                                      read table[id]         (d)

  (a) hb (b) by program order
  (b) hb (c) by the channel rule
  (c) hb (d) by program order
  => (a) hb (d). The read at (d) is defined and sees `peer`.

  Remove the channel and (a)/(d) are concurrent: a data race.
```

### Why a data race is undefined behaviour, not a stale read

The seductive mental model is "worst case I read the old value". That is wrong, and the
reasons are concrete:

- **The compiler is allowed to assume no races.** It may keep a value in a register across
  a loop, rematerialise a load, reorder non-dependent loads and stores, or duplicate a load
  so that two syntactic reads of one variable observe different values. Code such as
  `if p != nil { p.Use() }` can, in principle, load `p` twice.
- **The CPU reorders.** x86-64 is relatively strong (TSO) but still permits store->load
  reordering; arm64, which Docker on Apple silicon and most cloud ARM instances run, permits
  far more. A racy publish of a pointer to a freshly-built struct can be observed as a
  non-nil pointer to a *zero* struct on arm64.
- **Multi-word values tear.** Go guarantees no tearing only for aligned, machine-word-sized
  values accessed atomically. An interface value, a slice header, a string header, or a map
  entry is multiple words. A racing read of a slice header can yield one goroutine's pointer
  with another's length: an out-of-bounds read of arbitrary memory.
- **Concurrent map access is explicitly fatal.** The runtime has a race flag on `hmap`; on
  detection it calls `throw("concurrent map writes")`, which is **not recoverable**. No
  `recover` will save the process. A racy membership map does not misreport a peer's health;
  it kills the node.

So the range of outcomes is not "old value or new value". It is "old value, new value, half
a value, a value that was never written, or an unrecoverable crash under load in
production and never in your tests".

### Choosing a lock for the membership table

The swarm's membership table is read-mostly: heartbeat goroutines write a peer's score
every interval; telemetry, election evaluation, and dashboard fan-out read it constantly.

**`sync.Mutex`.** One `futex`-backed lock with a spin phase, a normal mode, and a starvation
mode (a waiter blocked >1 ms flips the mutex into FIFO handoff, sacrificing throughput for
tail latency). Simplest, and the correct default. All readers serialise, which for a map
lookup of a few hundred nanoseconds is fine at swarm scale.

**`sync.RWMutex`.** Allows concurrent readers, one writer. Two costs that matter:

- *Writer starvation is bounded but reader starvation is not free.* Go's `RWMutex` blocks
  *new* readers once a writer is waiting (`readerCount` is decremented by `rwmutexMaxReaders`
  to signal this), so a stream of readers cannot starve a writer indefinitely. But readers
  arriving while a writer waits must block, so a write-heavy phase -- a mass re-election
  updating many peers -- degrades every reader.
- *Cacheline contention on the reader counter.* `RLock` atomically increments a single
  `readerCount` field. Every reader on every core does a read-modify-write on the *same*
  cacheline, forcing it to bounce between cores' L1 caches. Above roughly a handful of
  concurrent readers on separate cores, `RLock`/`RUnlock` can cost more than the critical
  section it protects. `RWMutex` only wins when the read critical section is substantially
  longer than the atomic contention -- iterating the whole table qualifies; looking up one
  key often does not.

**`atomic.Pointer[T]` / copy-on-write.** The read path becomes a single atomic load with no
contention at all: readers do not write anything, so nothing bounces. Writers build a new
map and swap the pointer.

```go
// Read-mostly membership snapshot. Readers never block or write.
type Members struct {
	m atomic.Pointer[map[string]Peer]
	// mu serialises writers only; readers do not take it.
	mu sync.Mutex
}

func NewMembers() *Members {
	ms := &Members{}
	empty := map[string]Peer{}
	ms.m.Store(&empty)
	return ms
}

// Snapshot is safe for concurrent use and must be treated as immutable.
func (ms *Members) Snapshot() map[string]Peer { return *ms.m.Load() }

func (ms *Members) Get(id string) (Peer, bool) {
	p, ok := (*ms.m.Load())[id]
	return p, ok
}

// Update copies the whole map. Cheap at swarm scale (tens of peers),
// catastrophic at a million.
func (ms *Members) Update(p Peer) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	old := *ms.m.Load()
	next := make(map[string]Peer, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[p.ID] = p
	ms.m.Store(&next)
}
```

The trade-off is explicit: O(N) per write, zero-cost reads, and readers observe a consistent
but possibly one-generation-stale snapshot. For a membership table of tens of entries
updated a few times a second, that is an excellent trade. It also gives the telemetry path
something a mutex cannot: a stable map it can iterate without holding any lock, so a slow
JSON encode or a blocked WebSocket write cannot stall the heartbeat loop.

`atomic.Value` is the pre-generics version; it panics if you store inconsistently-typed
values. Prefer `atomic.Pointer[T]` in new code.

### The race detector

`go build -race`, `go run -race`, `go test -race`. The implementation is ThreadSanitizer:
the compiler instruments every memory access with a call into the runtime, which maintains
vector clocks per goroutine and shadow state (four shadow words) per eight bytes of
application memory. Channel operations, mutex operations, and atomics publish happens-before
edges into those clocks.

What it gives you:

- **No false positives.** A report is a real race under the memory model.
- Precise stacks for both conflicting accesses, plus where the goroutines were created.

What it cannot give you:

- **It only finds races it observes.** If the two conflicting accesses never actually execute
  concurrently during the run, there is no report. A race that needs a leader to die at the
  exact moment telemetry iterates the table will not appear unless your test makes that
  happen.
- **Bounded history.** Shadow memory keeps a limited number of prior accesses per location;
  very old accesses are evicted and the race can be missed.
- **Cost.** Roughly 5-10x CPU and 5-10x memory. It changes timing enough to hide some races
  and expose others, which is an argument for running it routinely rather than once.
- **It does not see into C, into `unsafe` tricks the instrumentation cannot follow, or
  across process boundaries.**

The practical discipline: run the full test suite under `-race` in CI, and additionally run
the chaos scenarios (kill a leader mid-replication) under `-race`, because those are the
schedules that produce the races a happy-path test never reaches.

---

## Why It Matters in This Swarm

`pkg/network` now exists and its paragraph below is cited to the code. The rest remain
commitments for the packages still landing.

**`pkg/cluster/` -- the membership table.** This is the central shared mutable structure: peer
ID -> role (leader/worker) -> assigned leader -> last-heartbeat timestamp -> health score.
Written by the heartbeat goroutines, read by the election evaluator, the telemetry
collector, and the dashboard fan-out. It will be implemented as a copy-on-write snapshot
behind `atomic.Pointer`, with a writer-side mutex, for the reasons above: the read path must
never block the heartbeat path, and the telemetry path must be able to iterate a stable view
without holding a lock across a network write. Access to the underlying map will never be
exposed; only immutable snapshots will leave the package.

**`pkg/cluster/` -- the node event loop.** Each node will run a single `select` loop over:
inbound decoded frames, the heartbeat ticker, the re-election trigger, and `ctx.Done()`.
The pseudo-random case order is what stops a node saturated with inbound frames from
silently ceasing to emit its own heartbeats -- which would present as that node being
declared dead by its peers while it is perfectly healthy and busy. The shutdown case will
additionally be checked in a non-blocking pre-select, so termination is not merely fair but
prompt.

**`pkg/cluster/` -- heartbeat failure counters.** The K-consecutive-missed-beats counter is a
single integer per peer, incremented by one goroutine and read by the election path. It will
live either inside the copy-on-write snapshot or as an `atomic.Int64`; it will not be a
plain `int` read without synchronisation, because "worst case we read a stale count" is
exactly the reasoning the memory model does not permit.

**`pkg/network/` -- the connection pool.** The reader goroutine delivers decoded frames by
calling a `Handler` directly on its own goroutine (`pkg/network/conn.go:194`), with the contract
that the handler must not block; the backpressure bound lives on the *write* side instead. Writes
to a socket are owned by exactly one goroutine per connection (`pkg/network/conn.go:390`) --
`net.Conn` writes are safe to call concurrently but give no framing guarantee, so two concurrent
`Write`s of two frames can interleave bytes and corrupt the stream. See
[Stream Framing](./stream-framing). Ownership of the write side is transferred to the writer
goroutine via two bounded channels, control and data (`pkg/network/conn.go:170`), whose depths
are stated bounds, not tuning knobs. The shutdown signal is a `close(done)` guarded by
`sync.Once` (`pkg/network/conn.go:176`), and the disposition written before that close is
published by it (`pkg/network/conn.go:187`). See
[Backpressure & Bounded Queues](./backpressure-and-bounded-queues).

**`pkg/telemetry/` -- the dashboard fan-out.** Broadcasting to WebSocket subscribers will use
`select` with `default` to drop samples for subscribers that are behind, rather than blocking.
A slow browser must never be able to stall a heartbeat. Subscriber teardown will use
`close(done)` as a broadcast, with the subscriber's own goroutine as the sole closer.

**`pkg/health/` -- `HealthStrategy` implementations.** Strategy instances will be invoked
concurrently from multiple probe goroutines, so the interface carries an implicit contract:
`EvaluateScore` must be safe for concurrent use. Any strategy holding internal state (an
EWMA of past latencies, say) owns its own synchronisation.

**Build and CI.** `go test -race ./...` will be the default test invocation, and the chaos
scenarios will run under it.

---

## Common Failure Modes & Edge Cases

**Goroutine leak on an unreceived send.** A goroutine blocked forever on `ch <- v` because
the receiver returned early. *Symptom:* memory and goroutine count climb monotonically over
hours; `curl /debug/pprof/goroutine?debug=1` shows hundreds of goroutines all parked in
`chansend`. *Fix:* every send in a goroutine that can outlive its consumer must be
`select { case ch <- v: case <-ctx.Done(): return }`, or the channel must be buffered with a
capacity that provably covers the number of sends.

**Send on closed channel panic during shutdown.** *Symptom:* the process dies at shutdown,
never in steady state, with `panic: send on closed channel` in a goroutine unrelated to the
one that called `close`. *Cause:* using the data channel's closure as the shutdown signal
while producers are still running. *Fix:* close a separate done/context channel; let
producers observe it and return; close the data channel only from the single logical sender,
after a `WaitGroup` confirms producers have stopped.

**Double close on re-election.** Two goroutines both decide the current leader is dead and
both close the same "leader lost" channel. *Symptom:* `panic: close of closed channel`,
reproducible only when two heartbeat deadlines expire in the same millisecond. *Fix:*
`sync.Once`, or make the decision in a single owner goroutine and have the detectors merely
report.

**Nil channel deadlock.** A struct field of channel type left at its zero value. *Symptom:*
a goroutine parked forever in `chanrecv` with no obvious counterparty; if it is the *only*
runnable goroutine, `fatal error: all goroutines are asleep - deadlock!`, but in a real
server there are always other goroutines, so the deadlock detector never fires and the
subsystem is just silently dead. *Fix:* construct channels in a constructor, never rely on
the zero value, and treat "works in a test, hangs in the binary" as a nil-channel smell.

**Unbounded buffer as a leak.** Choosing a large capacity to "avoid blocking" converts
backpressure into latency and then into OOM. *Symptom:* RSS grows under load; consumer
lag grows without bound; the system appears healthy until the container is OOM-killed and
the node vanishes from the swarm, triggering a spurious re-election. *Fix:* pick a capacity
you can justify in a sentence, and drop explicitly with `default` when it is exceeded.

**Range over a channel that is never closed.** `for v := range ch` blocks forever after the
last value. *Symptom:* a worker that appears to hang after processing all its work; the
program does not exit. *Fix:* close from the sender, or use an explicit
`for { select { case v, ok := <-ch: if !ok { return } ... } }`.

**RWMutex held across I/O.** Taking `RLock` on the membership table and then writing JSON to
a WebSocket while holding it. *Symptom:* a single slow or malicious dashboard client stalls
every heartbeat update in the process; the node is declared dead by its peers. Diagnosed by
a goroutine dump showing many goroutines in `sync.(*RWMutex).Lock` and one in `syscall.Write`.
*Fix:* snapshot under the lock, release, then do I/O. The copy-on-write design makes this
the only possible shape.

**RWMutex recursive read lock.** Goroutine A holds `RLock`, calls a function that takes
`RLock` again; meanwhile goroutine B calls `Lock` between the two. B blocks waiting for
readers; A's second `RLock` blocks because a writer is waiting. *Symptom:* a hard deadlock
that only appears under concurrent load. `RWMutex` read locks are explicitly **not**
reentrant. *Fix:* never call outward -- into an interface, a callback, or another package --
while holding a read lock.

**Concurrent map write crash.** *Symptom:* `fatal error: concurrent map writes` or
`concurrent map read and map write`, with a stack in `runtime.mapassign`. Unrecoverable; the
container restarts. Often shows up first in production because the race window is a few
nanoseconds wide. *Fix:* the copy-on-write table; and CI under `-race`.

**Interface value tearing on arm64.** Publishing a `HealthStrategy` by assigning to an
unsynchronised interface field. A reader can observe the new type word with the old data
word. *Symptom:* a segfault or a method dispatch into the wrong type, on ARM containers
only, never on the developer's x86 laptop. *Fix:* `atomic.Pointer`, or a mutex, or set the
strategy once at construction and never mutate it.

**Assuming `select` gives priority.** Believing that putting `<-ctx.Done()` first makes it
win. *Symptom:* shutdown takes an unbounded time under load; `docker compose down` hits its
10-second timeout and SIGKILLs the node mid-replication. *Fix:* the non-blocking pre-select
shown above.

**A clean `-race` run mistaken for proof.** *Symptom:* a crash in production that CI has
never reproduced. *Cause:* the detector reports only observed races. *Fix:* deliberately
exercise adversarial interleavings -- kill a leader during replication, spike latency during
an election -- under `-race`, rather than relying on the happy path.

---

Next: [Context & Cancellation Propagation](./context-cancellation) applies closed-channel
broadcast semantics to shutdown and deadlines, and explains why a context alone cannot
interrupt a blocked socket read.
