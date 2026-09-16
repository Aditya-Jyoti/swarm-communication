---
title: Context & Cancellation Propagation
outline: deep
---

# Context & Cancellation Propagation

`context.Context` is the smallest interface in the standard library that anyone argues
about. Four methods, one purpose: to let a caller tell an arbitrarily deep tree of
goroutines, "stop, I no longer need the result."

In a swarm node, almost every operation has a deadline. A health probe that takes longer
than the heartbeat interval is worthless. A replication write to a dead peer must not block
the leader forever. A `SIGTERM` from `docker compose down` must drain connections and exit
before Docker's grace period expires and sends `SIGKILL`. All three are the same mechanism.

Prerequisite: [CSP, Channels & The Go Memory Model](./csp-channels-and-memory-model) --
`Done()` is a closed channel used as a broadcast, and the happens-before edge from `close`
is what makes cancellation observable. See also [The Netpoller](./go-netpoller) and
[TCP Sockets & The Kernel](./tcp-sockets-and-the-kernel) for why interrupting a read is not
as simple as it looks, and [The GMP Scheduler](./go-scheduler-gmp) for the goroutine states
involved.

---

## Core Mental Model

### A tree, not a bag

The single most common misreading is that `Context` is a request-scoped dictionary -- a place
to stash a database handle, a logger, a user ID. `WithValue` exists and has legitimate uses
(request IDs, trace spans, deadlines a middleware injected), but that is a side feature.

A context is a **node in an immutable tree of cancellation signals**.

```
                    context.Background()
                            |
                 signal.NotifyContext(SIGINT/SIGTERM)      <- process lifetime
                            |
            +---------------+---------------+
            |                               |
    WithCancel (cluster loop)        WithCancel (http server)
            |                               |
   +--------+--------+                      |
   |                 |                WithTimeout(5s) per request
 WithTimeout(500ms)  WithTimeout(500ms)
 probe peer-3        probe peer-7

  Cancelling any node cancels its entire subtree, immediately,
  and never anything above it. Edges point downward only.
```

Every `WithX` constructor returns a **new** context that is a child of the one you passed.
The parent is never modified -- contexts are immutable. Cancellation flows strictly downward.
A timed-out peer probe cannot take down the cluster loop; cancelling the cluster loop takes
down every probe beneath it.

The interface:

```go
type Context interface {
	Deadline() (deadline time.Time, ok bool)
	Done() <-chan struct{}
	Err() error
	Value(key any) any
}
```

`Done()` returns a channel that is **closed** -- never sent on -- exactly once, when this
context is cancelled. Closure is the broadcast: every goroutine selecting on it wakes, and
every future receive returns immediately. `Err()` returns `nil` before cancellation and,
afterwards, either `context.Canceled` or `context.DeadlineExceeded`.

### The constructors

| Constructor | Meaning |
| --- | --- |
| `context.Background()` | The root. Never cancelled, no deadline, no values. Use in `main`, in top-level initialisation, in tests. |
| `context.TODO()` | Identical behaviour; a marker meaning "a context belongs here and I have not yet plumbed it". Distinguishable by static analysis. |
| `context.WithCancel(parent)` | Child plus a `CancelFunc`. Call it to cancel the subtree. |
| `context.WithCancelCause(parent)` | As above, but the cancel func takes an `error` retrievable via `context.Cause`. |
| `context.WithTimeout(parent, d)` | Child cancelled after `d`, or when the parent is, or when you call cancel -- whichever is first. |
| `context.WithDeadline(parent, t)` | The absolute-time form. `WithTimeout` is `WithDeadline(parent, time.Now().Add(d))`. If the parent's deadline is earlier, the parent's wins. |
| `context.WithoutCancel(parent)` | Keeps the parent's **values**, drops its cancellation and deadline. For work that must outlive the request that triggered it -- flushing a final telemetry batch during shutdown, for example. |
| `context.AfterFunc(ctx, f)` | Runs `f` in a new goroutine when `ctx` is done. Returns a `stop func() bool` that unregisters it. The clean way to hook cleanup onto a context without writing a goroutine that parks on `Done()`. |
| `context.WithValue(parent, k, v)` | Attaches one immutable key/value. Keys must be an unexported named type, never a bare string. |

`CancelFunc` is idempotent and safe to call from multiple goroutines. Calling it after the
context has already timed out is a no-op that still releases resources. **Always
`defer cancel()`.** Always. The reason is in the next section.

### Deadline monotonicity

Deadlines only ever tighten going down the tree. `WithTimeout(parent, 10*time.Second)` on a
parent with 200 ms remaining gives you a context with 200 ms remaining. You never have to
check whether your child deadline is compatible with your caller's; the package enforces it.

---

## Under the Hood

### cancelCtx

`WithCancel` returns a `*cancelCtx`, which looks roughly like this (see `context/context.go`):

```
cancelCtx
+--------------------------------+
| Context  (the parent)          |
| mu       sync.Mutex            |
| done     atomic.Value          |  lazily-created chan struct{}
| children map[canceler]struct{} |  the subtree
| err      error                 |  Canceled / DeadlineExceeded
| cause    error                 |
+--------------------------------+
```

Three details are worth internalising.

**The done channel is lazy.** It is created on the first call to `Done()` (or on cancel).
A context nobody ever selects on never allocates a channel. This is why `Done()` may not be
free on first call and why storing the result of `ctx.Done()` in a local before a hot loop
is a legitimate micro-optimisation.

**Parent registration walks up to the nearest cancellable ancestor.** `propagateCancel`
looks upward for a parent that is itself a `*cancelCtx` (or, via the `Done()` channel, any
custom implementation). If the parent is already cancelled, the child is cancelled
immediately at construction. If the parent is not cancellable at all -- `Background()` -- no
registration happens and no goroutine is spawned. If the parent is a *custom* `Context`
implementation that the package cannot recognise, `context` falls back to spawning a
goroutine that selects on the parent's `Done()`. That is one concrete reason to avoid
hand-rolled `Context` implementations.

**`cancel` walks down.** The cancellation routine is, in essence:

```
cancel(ctx, removeFromParent, err, cause):
    lock ctx.mu
    if ctx.err != nil: unlock; return        // already cancelled: idempotent
    ctx.err, ctx.cause = err, cause
    close(ctx.done)                          // the broadcast
    for child in ctx.children:
        child.cancel(false, err, cause)      // recurse, depth-first
    ctx.children = nil
    unlock
    if removeFromParent: detach ctx from parent.children
```

`close` happens before the recursion, so a goroutine selecting on the parent may observe
cancellation before the children are marked. That is fine: the only guarantee is that every
descendant becomes done, not the order.

The `removeFromParent` flag is why `defer cancel()` matters: it is what unlinks this child
from the parent's `children` map.

### timerCtx

`WithDeadline` wraps a `cancelCtx` with a `time.Timer`:

```go
// Conceptually:
c := &timerCtx{cancelCtx: newCancelCtx(parent), deadline: d}
dur := time.Until(d)
if dur <= 0 {
	c.cancel(true, DeadlineExceeded, nil) // already expired
	return c, func() { c.cancel(false, Canceled, nil) }
}
c.timer = time.AfterFunc(dur, func() {
	c.cancel(true, DeadlineExceeded, nil)
})
```

The returned `CancelFunc` stops the timer and unlinks from the parent. If you do not call
it, the timer stays armed in the runtime's timer heap and the context stays in its parent's
`children` map until the deadline fires.

**This is a real leak in a hot path.** Consider a heartbeat loop at 200 ms with a 500 ms
per-probe timeout, across a 10-node swarm:

```
 Per node: 5 probes/second/peer x 9 peers = 45 contexts/second
 Each leaked context: one runtime timer + one map entry in the parent,
 retained for the full 500 ms.

 Steady-state leak: ~22 live timers and map entries that should be zero.
 Over a 24h run with the parent being the long-lived cluster context,
 the parent's children map churns 3.9M insertions.
```

Timers in the heap are not free: every `addtimer`/`deltimer` is a heap operation on a
per-P timer bucket, and the netpoller's `checkTimers` walks them. A few thousand stale
timers is survivable; the more insidious effect is that the *parent* context holds a
reference to every un-cancelled child, so nothing in the subtree can be garbage collected
until its deadline fires. `go vet`'s `lostcancel` check catches the simple cases; it does not
catch a `cancel` stored in a struct field and never called.

### Cause and error inspection

```go
ctx, cancel := context.WithCancelCause(parent)
cancel(fmt.Errorf("leader %s failed %d heartbeats", id, k))

<-ctx.Done()
ctx.Err()            // context.Canceled -- the coarse reason
context.Cause(ctx)   // the specific error you supplied
```

`context.Cause` on a timed-out `timerCtx` returns `context.DeadlineExceeded` unless
`WithDeadlineCause`/`WithTimeoutCause` supplied a richer one. Always compare with
`errors.Is`, never `==`, because the error will usually arrive wrapped:

```go
if err := probe(ctx, peer); err != nil {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// The probe ran out of time. This IS evidence about the peer.
	case errors.Is(err, context.Canceled):
		// WE gave up. This is NOT evidence about the peer.
	default:
		// A real network or protocol error.
	}
}
```

That distinction is the single most important idea on this page, and it returns below.

### Contexts cannot interrupt a syscall

This is the part that surprises people. A context is a closed channel. A goroutine blocked
in `read(2)` on a socket is not selecting on anything -- it is parked by the netpoller
waiting for the kernel to report readability, or, worse, blocked in a syscall on an OS
thread. Closing a channel does not reach it.

```
  ctx cancelled
       |
       v
  close(done)  ----X----> goroutine parked in internal/poll.(*FD).Read
                          waiting on runtime netpoll for fd 7

  There is no path from a channel close to an fd. Something must
  either change the fd's deadline or close the fd.
```

Go's answer is `SetReadDeadline`/`SetWriteDeadline`/`SetDeadline` on `net.Conn`. These do
**not** map to `SO_RCVTIMEO`; they are implemented entirely in the runtime. Each `pollDesc`
carries `rd`/`wd` deadline fields and an associated runtime timer. When the timer fires,
`runtime.netpolldeadlineimpl` marks the fd's read (or write) side as timed out and calls
`netpollgoready` on any goroutine parked there. The goroutine wakes and `Read` returns a
`*net.OpError` wrapping `os.ErrDeadlineExceeded`, whose `Timeout() bool` reports true. The
fd remains valid and usable -- unlike closing it. See [The Netpoller](./go-netpoller).

Importantly, deadlines are **absolute times, not durations**, and they are sticky: once set,
every subsequent `Read` is measured against the same instant until you set a new one. A
long-lived connection must refresh its deadline before every read, or the second read will
return immediately with a timeout.

There are two correct ways to bridge a context onto a connection.

**Method 1 -- derive the deadline up front.** Simplest, and correct whenever the only
cancellation you care about is the deadline.

```go
// readFrame reads one length-prefixed frame, honouring ctx's deadline.
// It does NOT react to explicit cancellation mid-read; see Method 2 for that.
func readFrame(ctx context.Context, c net.Conn, max uint32) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		if err := c.SetReadDeadline(dl); err != nil {
			return nil, err
		}
	} else {
		// Clear any previous deadline; the zero time means "no deadline".
		if err := c.SetReadDeadline(time.Time{}); err != nil {
			return nil, err
		}
	}

	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > max {
		return nil, fmt.Errorf("frame too large: %d > %d", n, max)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
```

**Method 2 -- a watchdog via `context.AfterFunc`.** Reacts to explicit cancellation as well
as to deadlines, and costs no goroutine when the context is never cancelled.

```go
// withConnCancel makes ctx cancellation interrupt blocked I/O on c by
// forcing an immediate deadline. The returned func must always be called.
func withConnCancel(ctx context.Context, c net.Conn) (stop func()) {
	unregister := context.AfterFunc(ctx, func() {
		// time.Now() is in the past by the time the runtime processes it,
		// so any in-flight and any subsequent Read/Write returns at once.
		_ = c.SetDeadline(time.Now())
	})
	return func() {
		if unregister() {
			// AfterFunc had not yet run: the context was never cancelled,
			// so it is safe to clear the deadline for reuse.
			_ = c.SetDeadline(time.Time{})
		}
	}
}

// readFrameCancellable reads one frame, aborting on ctx cancellation
// or deadline, and normalises the resulting error.
func readFrameCancellable(ctx context.Context, c net.Conn, max uint32) ([]byte, error) {
	stop := withConnCancel(ctx, c)
	defer stop()

	b, err := readFrame(context.WithoutCancel(ctx), c, max) // don't double-set the deadline
	if err != nil {
		// A forced deadline surfaces as os.ErrDeadlineExceeded; report the
		// context's own error so callers can distinguish Canceled from timeout.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return b, nil
}
```

Note `unregister()`'s return value: `true` means the function was removed before running,
`false` means it has already run (or is running). Clearing the deadline is only safe in the
first case; in the second, another goroutine may be mid-`SetDeadline` and the connection is
being torn down anyway.

The third approach -- closing the connection from another goroutine -- also unblocks the read
(the runtime marks the `pollDesc` closed and wakes waiters with `net.ErrClosed`), but it is
destructive: the connection cannot be reused, and you must guard against double-close. Use
it for teardown, not for per-operation timeouts.

`net.Dialer.DialContext` already does all of this internally for connection establishment,
so use it rather than `net.Dial` plus your own timer.

---

## Why It Matters in This Swarm

No Go code exists yet; the following are commitments the implementation will have to honour.

**`cmd/swarm-node/` and `cmd/control-center/` -- the root of the tree.** `main` will build
its root context with `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`
and pass it into every subsystem. Nothing below `main` will call `context.Background()`.
Docker sends `SIGTERM` and waits ten seconds before `SIGKILL`, so the shutdown path has a
hard budget; it will be given its own `WithTimeout` derived from `WithoutCancel(root)` so
that draining is not itself cancelled by the very signal that started it.

**`pkg/health/` -- `HealthStrategy.EvaluateScore`.** The interface as specified in the project
brief takes only a `NodeAddress`. A latency probe that cannot be bounded is a liability, so
implementations will either accept a context in an extended method or construct their own
bounded one internally from a configured probe timeout; either way, every probe will carry a
deadline strictly shorter than the heartbeat interval, and every probe context will be
`defer cancel()`-ed. The probe timeout being shorter than the heartbeat interval is a hard
invariant: if probes can outlive their interval, probes accumulate and the node measures its
own scheduling backlog rather than the network.

**`pkg/network/` -- the framing codec and connection pool.** Every read of a length-prefixed
frame will set a read deadline derived from the caller's context before touching the socket,
using the pattern above, and will refresh it per frame. Idle connections in the pool will
carry a long read deadline as a dead-peer detector of last resort, since a silently dropped
TCP connection on a Docker bridge may never produce a `RST`. See
[TCP Sockets & The Kernel](./tcp-sockets-and-the-kernel).

**`pkg/cluster/` -- heartbeats and failure detection.** This is where the distinction below
is load-bearing. The heartbeat loop will classify probe outcomes into three categories, and
only one of them increments the K-missed-beats counter:

- `errors.Is(err, context.DeadlineExceeded)` or a `net.Error` with `Timeout()` -> the peer
  failed to respond in time. **Counts as a missed beat.**
- `errors.Is(err, context.Canceled)` -> *we* are shutting down or re-electing. **Does not
  count.** The peer told us nothing.
- Any other error (connection refused, protocol error, decode failure) -> evidence about the
  peer, counted, and logged distinctly.

**`pkg/cluster/` -- re-election.** When a leader is declared dead, the leader's per-cluster
context is cancelled with `WithCancelCause`, so that every replication stream, probe, and
pending task under it unwinds with a cause explaining *which* leader failed and *why*. The
cause will be surfaced to telemetry so the dashboard can show the reason for a failover
rather than a bare state change.

**`pkg/telemetry/` -- the WebSocket fan-out.** Each subscriber will get a context derived from
the server's, cancelled when the socket closes, and write deadlines on every frame so that a
paused browser tab cannot pin a goroutine indefinitely.

**Struct storage.** No struct in the codebase will carry a `ctx` field, with one deliberate
exception class noted below.

---

### Why contexts must not live in structs

The convention is `func (s *Server) Do(ctx context.Context, ...) error` -- context as the first
parameter, named `ctx`. The reasons are not stylistic:

1. **A struct outlives a call.** A `ctx` captured at construction encodes the *constructor's*
   cancellation, which is almost never the caller's. A connection pool built during startup
   would give every future request the startup context.
2. **Lifetime becomes invisible.** A reader of `pool.Send(frame)` cannot tell what cancels it.
3. **Concurrency.** Two callers with different deadlines cannot share one stored context;
   whoever wrote last wins.
4. **Reuse.** A once-cancelled stored context is permanently dead, so the object silently
   becomes unusable.

The pragmatic exceptions, both of which are really "the struct *is* the operation":

- **A long-running worker whose lifetime genuinely equals the struct's.** A
  `heartbeatLoop{ctx}` created by `NewHeartbeatLoop(ctx, ...)` and torn down with it is
  defensible, though a stored `stop func()` plus a `done chan struct{}` is clearer.
- **Request-shaped structs.** `http.Request` holds a context and exposes
  `WithContext`/`Context()`. The struct represents exactly one in-flight operation, so its
  lifetime and the context's coincide by construction. The swarm's own frame-handling types
  may follow this, but only where a value corresponds to a single in-flight request.

---

## Graceful shutdown, composed from the stdlib

The three concepts beginners fuse -- **cancellation**, **shutdown**, and **failure
detection** -- are distinct:

```
  Cancellation        "I no longer need this result."
                      Source: us. Says nothing about the peer.

  Shutdown            "This process is terminating; finish and release."
                      Source: the operator or the orchestrator.
                      Implemented WITH cancellation, plus draining.

  Failure detection   "That peer did not answer within its budget."
                      Source: the network or the peer.
                      Evidence. Feeds the election. Must be counted.
```

Conflating the first and the third is how a rolling restart becomes a cascading re-election:
every node shuts down, every node's in-flight probes return `context.Canceled`, every
surviving node counts those as missed beats, and the swarm demotes leaders that were never
unhealthy.

A shutdown that composes correctly, using only the standard library:

```go
func main() {
	root, stopSignals := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	node := cluster.New(cfg)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: dashboard.Handler(node)}

	// errc collects the first terminal error from any subsystem.
	errc := make(chan error, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errc <- node.Run(root) }()
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	// Wait for either a signal or a subsystem failure.
	var first error
	select {
	case <-root.Done():
		log.Printf("shutdown: %v", context.Cause(root))
	case first = <-errc:
		log.Printf("subsystem failed: %v", first)
		stopSignals() // stop trapping; a second Ctrl-C should kill us
	}

	// Drain with a hard budget, deliberately independent of `root`, which
	// is already cancelled. WithoutCancel keeps values, drops cancellation.
	drain, cancelDrain := context.WithTimeout(
		context.WithoutCancel(root), 8*time.Second)
	defer cancelDrain()

	if err := srv.Shutdown(drain); err != nil {
		log.Printf("http drain incomplete: %v", err)
	}
	if err := node.Drain(drain); err != nil {
		log.Printf("cluster drain incomplete: %v", err)
	}

	wg.Wait()
	if first != nil {
		os.Exit(1)
	}
}
```

Points to notice:

- `errc` is buffered to the number of producers, so a subsystem returning after we have
  stopped reading does not leak a goroutine on a blocked send -- the failure mode described
  in [CSP, Channels & The Go Memory Model](./csp-channels-and-memory-model).
- The drain context is derived from `WithoutCancel(root)`, not `root`. Deriving from an
  already-cancelled context yields an already-cancelled context, and the drain would do
  nothing.
- The 8-second budget sits inside Docker's 10-second grace period with room for the final
  `wg.Wait()`.
- `signal.NotifyContext`'s stop function restores default signal handling, so a second
  `SIGTERM` during a stuck drain kills the process rather than being swallowed.
- `node.Drain` is a separate method from context cancellation, because "stop accepting new
  work and finish what you have" is not the same instruction as "abandon everything".

An errgroup-style join without the dependency is exactly the `WaitGroup` + buffered-channel
pattern above; `golang.org/x/sync/errgroup` adds a derived context cancelled on the first
error, which you can reproduce by calling a shared `cancel` before sending to `errc`.

---

## Common Failure Modes & Edge Cases

**Forgotten `cancel` in the heartbeat path.** *Symptom:* goroutine count is stable but
`runtime.MemStats` and timer counts climb; `/debug/pprof/heap` shows `context.propagateCancel`
retaining megabytes; the parent context's `children` map grows without bound over a long
run. *Cause:* `ctx, _ := context.WithTimeout(...)` with the cancel discarded, or a `cancel`
stored and never invoked. *Fix:* `defer cancel()` unconditionally; enable `go vet`'s
`lostcancel` in CI. Note the `_` form does not even compile as a two-value assignment with a
blank -- but `ctx, cancel := ...; _ = cancel` does, and vet catches that one.

**Cancelled probes counted as missed heartbeats.** *Symptom:* during any coordinated event --
a deploy, a re-election, a config reload -- nodes mass-demote healthy leaders, and the swarm
oscillates for several intervals before settling. *Cause:* treating every non-nil error from
a probe as evidence about the peer. *Fix:* the three-way classification above, checked with
`errors.Is`, not string matching.

**A context that never interrupts a read.** *Symptom:* `docker compose down` hangs for the
full grace period and the node is SIGKILLed; goroutine dumps show goroutines in
`internal/poll.(*FD).Read` long after cancellation. *Cause:* passing a context into a
function that never converts it into a socket deadline. *Fix:* the bridge patterns above;
treat "this function takes a ctx" as a claim that must be honoured, not documentation.

**Stale sticky deadline.** A deadline set once on a pooled connection, then the connection
is reused. *Symptom:* the first operation on a recycled connection returns
`i/o timeout` immediately, with zero bytes transferred, and the pool evicts a perfectly
healthy connection. Under load this presents as an escalating connection-churn spiral. *Fix:*
set the deadline before **every** read and write, or explicitly clear it with
`SetReadDeadline(time.Time{})` on return to the pool.

**Comparing errors with `==`.** *Symptom:* a timeout branch that never executes, because the
error arrived as `*net.OpError` wrapping `os.ErrDeadlineExceeded`, or because a package
wrapped it with `fmt.Errorf("%w", ...)`. *Fix:* `errors.Is` / `errors.As` throughout.

**Deriving the drain context from the cancelled root.** *Symptom:* `srv.Shutdown(ctx)`
returns instantly with `context.Canceled`, in-flight WebSocket clients are cut mid-frame,
and the dashboard shows a torn final state. *Fix:* `context.WithoutCancel` as shown.

**Racing the parent's deadline.** A child given 500 ms under a parent with 50 ms left will
be cancelled with `DeadlineExceeded` at 50 ms. *Symptom:* probe timeouts far shorter than
configured, appearing only when the system is already under pressure. *Fix:* this is correct
behaviour; the bug is expecting otherwise. If a probe genuinely must complete regardless,
`WithoutCancel` plus its own timeout -- and accept that it now cannot be cancelled by
shutdown.

**Leaking the goroutine you spawned to watch `Done()`.** Writing
`go func() { <-ctx.Done(); cleanup() }()` for a context that is never cancelled parks that
goroutine for the process lifetime. *Symptom:* a slow goroutine leak proportional to
connection count. *Fix:* `context.AfterFunc`, which registers a callback on the context
itself and costs nothing until cancellation, and whose `stop` lets you unregister.

**`WithValue` used as dependency injection.** *Symptom:* nil-pointer panics deep in a call
tree because a value was not set on some code path; unreadable call sites; no compile-time
safety. *Fix:* pass dependencies as parameters or struct fields. Reserve `WithValue` for
genuinely cross-cutting, request-scoped data with unexported key types.

**Calling `cancel` from the cancelled subtree.** Safe -- `CancelFunc` is idempotent and
concurrency-safe -- but cancelling a context you did not create is a layering violation that
makes shutdown order unanalysable. *Symptom:* subsystems dying in a different order on every
run. *Fix:* only the code that created a context may cancel it.

---

See also: [CSP, Channels & The Go Memory Model](./csp-channels-and-memory-model) for the
channel semantics `Done()` relies on, [The Netpoller](./go-netpoller) for how deadlines wake
a parked goroutine, [The GMP Scheduler](./go-scheduler-gmp) for what "parked" means, and
[TCP Sockets & The Kernel](./tcp-sockets-and-the-kernel) for why a socket read can block
indefinitely in the first place.
