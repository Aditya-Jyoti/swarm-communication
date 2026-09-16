---
title: Engineering Worklog
outline: [2, 3]
---

# Engineering Worklog

Append-only. Newest entries at the bottom. Every entry records who executed the work, what was
decided, what was rejected, why, and which concepts entered the syllabus as a result.

---

## 2026-09-16 — Phase 1 — Environment, Scaffolding & Curriculum Initialisation

### 1.1 Module and repository skeleton
**Agent:** `system-architect` (planning) → direct execution
**Decision:** `go mod init swarm-net`; Go 1.27 toolchain; standard `cmd/` + `pkg/` layout; git
initialised.

**Alternatives evaluated:**
- *`internal/` instead of `pkg/`* — `internal/` is the idiomatic choice for a binary-only module and
  would enforce that nothing outside imports these packages. **Rejected**: this repository's primary
  audience is readers, and `internal/` signals "not your business" to exactly the people we want
  reading it. The import-guard benefit is worth less than the invitation.
- *Flat single package* — **Rejected**: the package seams are where the teaching happens. See
  [Repository Layout](/architecture/repo-layout).

### 1.2 Package boundaries fixed
**Agent:** `system-architect`
**Decision:** Five library packages with a strictly acyclic downward dependency graph:
`cmd` → `cluster`/`telemetry` → `health`/`network` → `protocol`. `pkg/protocol` imports nothing
local.

**Rationale:** Each package gets one reason to change. The critical constraint is that `pkg/cluster`
never learns byte-level detail and `pkg/network` never learns message meaning — otherwise the
`HealthStrategy` pluggability the brief demands becomes a refactor rather than an injection.

**Alternative rejected:** folding `health` into `cluster` as a single file. A strategy living beside
its only consumer reliably grows a dependency on that consumer's internals; separating it makes the
extension point real rather than aspirational.

### 1.3 Subagent definitions
**Agent:** direct execution
**Decision:** Four agents written to `.claude/agents/`: `system-architect`, `doc-educator`,
`go-engineer`, `sim-engineer`. Each carries explicit hard rules, not just a role description —
notably "no production code" for the architect and "never assume a `Read` returns a whole message"
for the engineer.

**Note:** agent definitions are loaded at session start, so the four roles were executed within this
session by direct delegation to general-purpose workers carrying the `doc-educator` brief verbatim.
From the next session onward they are addressable by name.

### 1.4 Documentation framework
**Agent:** `doc-educator`
**Decision:** VitePress 1.x, local search provider, sidebar grouped as
Start Here / Architecture / Concepts (three sub-groups) / — with a Distributed Systems Theory group
created empty, to be filled in Phases 3 and 4.

**Alternatives evaluated:** Docusaurus (heavier, React-based, unnecessary for a docs-only site);
raw Markdown in the repo with no site (loses navigation and search, which is most of the value when
the curriculum crosses twenty pages). VitePress wins on zero-config Markdown fidelity and because
the Go build never depends on Node.

### 1.5 First Concept Discovery pass
**Agent:** `doc-educator`

Audit question: *given that Phase 2 and 3 will implement a framed TCP protocol with goroutine-driven
peer connections inside containers, what must an engineer understand before a single line of it
makes sense?*

Concepts identified and added to the syllabus:

| Page | Why it was nominated |
|---|---|
| `concepts/tcp-sockets-and-the-kernel` | Everything sits on the socket lifecycle. Accept queues, buffer sizing, TIME_WAIT and half-open sockets are all load-bearing for a swarm that kills containers on purpose. |
| `concepts/stream-framing` | The single most common source of bugs in hand-rolled TCP protocols. Also the direct prerequisite for the Phase 2 serialisation decision. |
| `concepts/nonblocking-io-and-epoll` | Explains why one-goroutine-per-connection is affordable at all, and what the runtime is hiding. |
| `concepts/docker-bridge-networking` | The mesh's discovery mechanism *is* the user-defined bridge's DNS. Container IP churn after a restart is a real failure mode for a cached peer table. |
| `concepts/go-scheduler-gmp` | Heartbeat timing is scheduler timing. A GC pause or a starved P looks exactly like a slow peer. |
| `concepts/go-netpoller` | The bridge between the epoll page and the Go code. Also where read deadlines come from — the only safe way to bound a network read. |
| `concepts/csp-channels-and-memory-model` | The membership table is written by heartbeat goroutines and read by telemetry. That is a data race unless the memory model is understood, not guessed at. |
| `concepts/context-cancellation` | Cancellation, shutdown and failure detection are three different things. Fusing them causes a cancelled probe to be miscounted as a missed heartbeat — a self-inflicted failover. |

### 1.6 Syllabus backlog nominated during the first pass

The authoring agents nominated further concepts while writing. Recorded here so the backlog is a
decision record rather than a memory. None are written yet; the phase column is when they become
blocking rather than merely useful.

| Nominated concept | Why | Needed by |
|---|---|---|
| Failure detectors: the impossibility of distinguishing slow from dead | FLP, Chandra-Toueg unreliable detectors, phi-accrual vs fixed K-missed-beats. $K$ is currently an unjustified constant. | Phase 4 |
| Heartbeat interval, jitter & the thundering herd | Every node probing on the same tick produces correlated spikes that themselves cause false-positive failures. | Phase 4 |
| Split-brain, quorum, and why `ceil(N*threshold)` is not consensus | The docs must be candid that this election gives up Raft-like safety, and say exactly what it gives up. | Phase 3 |
| Latency measurement as a statistic | Coordinated omission, tail vs mean, EWMA lag. Determines whether `LatencyHealthStrategy` elects *good* leaders or merely lucky ones. | Phase 2 |
| Backoff, retry & connection storms | Synchronised redial after a leader dies is a self-inflicted DDoS on the survivors. | Phase 4 |
| Monotonic vs wall clocks in Go | `time.Time`'s embedded monotonic reading, what survives serialisation, NTP step. Small page, high bug-prevention value. | Phase 2 |
| Go timers and the per-P timer heap | `Ticker` never fires early but can fire very late — underpins every heartbeat timing claim already made. | Phase 4 |
| Backpressure & bounded queues | An unbounded send channel in front of a stalled socket is an OOM primitive; needs explicit per-frame-class drop policy. | Phase 3 |
| cgroups v2, CPU quota & throttling | Throttling presents as multi-millisecond latency cliffs, which latency-based election will read as peer sickness. | Phase 5 |
| The Go GC and tail latency | The other half of "why did this heartbeat arrive 30 ms late". | Phase 4 |
| Graceful shutdown & teardown ordering | FIN vs RST, SO_LINGER, draining an accept loop — relevant the moment chaos controls start killing containers. | Phase 5 |
| Edge-triggered epoll vs a framing decoder | ET requires draining to `EAGAIN`, which interacts badly with a decoder that stops at a message boundary. | Phase 3 |
| WebSocket framing & the HTTP upgrade | The dashboard transport, hand-rolled or otherwise. | Phase 5 |
| The race detector in practice | What it can and cannot observe, and why a clean run is not a proof. | Phase 4 |
| Structured concurrency: WaitGroup, joins & goroutine ownership | Every spawned goroutine needs a named owner and a join. The spawn side is covered; the join side is not. | Phase 4 |
| Goroutine leak detection & pprof in practice | Reading `goroutine?debug=2`, mapping park states to root causes, leak-checked tests. Several pages point at symptoms that need this to diagnose. | Phase 4 |
| Backpressure & load shedding policy | What a dropped telemetry sample means versus a dropped heartbeat. Currently spread across pages with no home. | Phase 3 |
| Socket error taxonomy in Go | `ECONNREFUSED` vs `ETIMEDOUT` vs `ECONNRESET` vs `EOF` vs deadline-exceeded — which are health signals and which are start-up noise. Recurs everywhere; needs one canonical page to link to. | Phase 3 |
| Kernel TCP timers & retry schedules | `tcp_syn_retries`, `tcp_retries2`, keepalive, `TCP_USER_TIMEOUT`. This knob set decides how fast failover *can* be, independent of our own $K$. | Phase 4 |
| IP identity vs node identity | IPAM recycling means a restarted node can inherit a dead peer's address. A distributed-systems problem wearing a Docker costume; belongs in `architecture/`. | Phase 3 |
| Container images & PID 1 | Exec-form `ENTRYPOINT`, signal forwarding, zombie reaping, minimal-image vs debuggability. Referenced as a commitment in the Docker page, not yet explained. | Phase 5 |

**Standing constraint recorded:** concept pages written before their code exists must not cite
`file.go:line` references. They state forward commitments instead, and are revised to cite real
locations as each phase lands. Fabricated line numbers would poison the one property that makes
this curriculum worth reading.


### 1.7 Build verification

`go build ./...` passes over the package skeleton (each `pkg/` directory carries a `doc.go` stating
its charter and its forbidden dependencies, so the layout is compiler-enforced from day one).

`npx vitepress build docs` passes with all 11 pages rendered and no dead links — VitePress fails the
build on unresolvable internal links, so the cross-links between concept pages are verified, not
assumed.

One fix was needed: `config.js` uses ESM `import`, which requires `"type": "module"` in
`package.json`. The alternative — renaming to `config.mts` — was rejected because the brief names
`.vitepress/config.js` explicitly, and the package is docs-only so the module type has no bearing on
anything else.

---

## 2026-09-16 — Phase 1 Checkpoint — Decisions locked by the user

### 2.1 Wire format: length-prefixed JSON
**Decided:** `uint32` big-endian length prefix + JSON body.

```
┌────────────┬────────────────────────────────┐
│ uint32 BE  │ JSON payload                   │
│ length     │ {"type":"HEARTBEAT","node":...}│
└────────────┴────────────────────────────────┘
```

**Rejected:** newline-delimited JSON — no bound before the delimiter arrives, so a peer that never
sends `\n` is a one-packet OOM primitive; `bufio.Reader.ReadString` has no maximum. Also rejected:
hand-rolled binary — schema evolution becomes manual versioning work for a saving we do not need at
this message rate.

**Binding consequences for `pkg/protocol`:**
- A `MaxFrameSize` constant is enforced *before* the payload buffer is allocated. Reading the length,
  then allocating that many bytes, then discovering it was 4 GB, is the bug this ordering exists to
  prevent.
- A frame decode error is unrecoverable on a stream: once the reader is off a frame boundary, every
  subsequent length is plausible garbage. The connection is dropped, never resynchronised.
- `TCP_NODELAY` stays on (Go's default). See
  [Stream Framing](/concepts/stream-framing) for why the Nagle/delayed-ACK interaction is a
  correctness bug in a latency-derived health model, not a performance nuisance.

### 2.2 Discovery: gossip from a seed
**Decided:** a joining node contacts a seed, receives the membership view, then propagates deltas
peer-to-peer. $N$ is genuinely dynamic — not known at compose time.

**Rejected:** a static peer list from compose environment (simpler, but fixes $N$ at deploy time and
makes "scale the swarm live" impossible); a shared Docker network alias (Docker-specific, and the
DNS view lags real membership).

**Accepted costs, recorded honestly:**
- The seed is a bootstrap single point of failure. Mitigation: accept a *list* of seeds and treat
  reachability of any one as sufficient. The seed matters only at join time — an established swarm
  survives seed loss.
- This materially expands Phase 3. Gossip needs a membership CRDT or versioned deltas, anti-entropy
  to repair divergence, and a decision on whether membership is eventually consistent (it is).
- Peer addresses are resolved by name at dial time and never cached. Container IP recycling after a
  chaos kill means a cached address can point at a *different, live* node — the worst failure mode,
  because it fails silently. See [Docker Bridge Networking](/concepts/docker-bridge-networking).

### 2.3 `HealthStrategy` contract amended
**Decided:** the brief's signature is amended to carry a context, and score direction is fixed as
**lower-is-better (a cost)**.

```go
type HealthStrategy interface {
    Name() string
    EvaluateScore(ctx context.Context, target NodeAddress) (float64, error)
}
```

**Rationale for deviating from the brief:** without a context, a probe cannot be cancelled or
bounded by its caller, the deadline hides inside each implementation, and shutdown must wait out
every in-flight probe. That contradicts the deadline discipline committed to in
[The Netpoller](/concepts/go-netpoller) and
[Context & Cancellation](/concepts/context-cancellation). Lower-is-better was chosen over
higher-is-better because latency and CPU load map directly, and only capacity-style metrics need
inverting; inverting a latency near zero is numerically awkward in a way inverting free memory is not.

**Binding consequences:** `pkg/cluster` compares scores and never interprets them. A probe returning
an error is *not* a score of zero — the distinction between "unreachable" and "excellent" must be
represented in the type, not encoded in a magic float. And per
[Context & Cancellation](/concepts/context-cancellation), a probe cancelled by shutdown must never be
counted as a missed heartbeat.

### 2.4 Election trigger: event-driven with a periodic floor
**Decided:** elect on membership change (join, or $K$-missed-beat eviction), and additionally
re-evaluate on a slow timer (~30 s) as a safety net.

**Rejected:** periodic-only (failover latency bounded below by $T$ even when death was detected
instantly); event-only (a single missed event leaves the swarm in a stale topology forever, with no
self-correction path).

**Accepted cost:** two paths into the same election routine, which must therefore be idempotent and
must produce the same result from the same membership view regardless of which path invoked it. This
is a testable property and will be tested as one. The periodic path must also not cause churn —
re-electing because a score moved by 0.3 ms is thrash, so hysteresis is required.

### 2.5 Syllabus additions triggered by these decisions

| Nominated concept | Why | Needed by |
|---|---|---|
| Gossip protocols & SWIM | The chosen discovery mechanism. Infection-style dissemination, fanout vs convergence time, piggybacked failure detection. | Phase 3 |
| Anti-entropy & eventual consistency of membership | Gossip converges; it does not agree. What that means when two nodes hold different views of $N$ at election time. | Phase 3 |
| Idempotence & hysteresis in control loops | Two triggers into one election routine, and the thrash that follows if a 0.3 ms score change can flip a leader. | Phase 4 |

---

## Open questions — resolved 2026-09-16, retained for the record

1. **Wire serialisation and framing.** JSON over newline-delimited frames (readable with `netcat`,
   trivially debuggable, allocation-heavy) versus a length-prefixed binary envelope (bounded,
   cheap, requires tooling to inspect) versus a length-prefixed envelope carrying a JSON payload
   (the hybrid). Recommendation recorded when asked.
2. **Peer discovery.** Static seed list from compose environment versus gossip from a single seed
   versus Docker DNS round-robin on a shared network alias.
3. **`HealthStrategy` contract details.** Score direction (lower-is-better vs higher-is-better),
   normalisation, probe-failure semantics, and whether `EvaluateScore` should take a `context.Context`
   — it must, if probes are ever to be cancellable, which means amending the interface in the brief.
4. **Election trigger.** Periodic re-evaluation versus event-driven on membership change.
5. **Split-brain policy.** What a minority partition is permitted to do.
