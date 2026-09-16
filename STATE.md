# Project State

Written 2026-09-17. This file is a handover snapshot, not a design document. It
records what exists, what is verified, what is half-finished, and what the next
session needs to know. The durable decision record is `docs/WORKLOG.md`; this file is
the "where did we stop" note that sits beside it.

## Where things stand

| Phase | Status |
|---|---|
| 1 -- Scaffolding, agents, VitePress, first concept pass | Complete |
| 2 -- Wire protocol and pluggable health interface | Complete, tested, committed |
| 3a -- P2P socket mesh and clustering engine | **Partially built, stopped mid-phase** |
| 3b -- Gossip and anti-entropy | Not started |
| 4 -- Heartbeats, failover, replication | Not started |
| 5 -- Control Center, dashboard, Compose | Not started |

Everything is committed. The working tree is clean.

## Verification at the moment of the snapshot

```
gofmt -l pkg cmd      clean
go vet ./...          clean
go test ./pkg/...     129 tests pass
npm run docs:check    ASCII clean, 41 Mermaid diagrams parse
```

Coverage by package:

| Package | Coverage |
|---|---|
| `pkg/health` | 100.0% |
| `pkg/protocol` | 97.3% |
| `pkg/cluster` | 97.1% |
| `pkg/network` | 85.9% |

The three uncovered branches in `pkg/protocol` are deliberate defensive assertions:
the `crypto/rand` panic in `NewMessageID`, an unreachable zero-length marshal guard,
and a `json.Indent` failure that cannot occur because a decoded `Payload` is always
valid JSON. They were left uncovered on purpose rather than papered over with
contrived tests.

## What exists

### `pkg/protocol` -- complete

Length-prefixed JSON framing. `uint32` big-endian length, then a JSON body.

- `Envelope` with a deferred-decode `json.RawMessage` payload.
- 14 message types split into control and data planes.
- `Encoder` (mutex-guarded, safe for concurrent use) and `Decoder` (single-reader by
  design, deliberately carries no mutex so a second reader is a race the detector
  catches immediately).
- `MaxFrameSize` validated against the header **before** any payload allocation.
- `DumpStream`, a non-production diagnostic that buys back the `netcat` readability
  length-prefixing cost us.
- Phase 3 payload schemas added: `MemberRecord`, `MembershipDeltaPayload`,
  `ElectionResultPayload`, `JoinClusterPayload`, `JoinAckPayload`, `LeavePayload`.

### `pkg/health` -- complete

- `HealthStrategy` with the amended context-carrying signature.
- `LatencyHealthStrategy` with an injectable `Prober`, so the statistics are testable
  with no clock and no socket.
- `TCPConnectProber` is an explicit **placeholder**. It measures a kernel handshake,
  which a node in a GC pause still completes promptly, so it would happily elect a
  comatose node. Phase 3 is supposed to replace it with PING/PONG over the mesh.

### `pkg/network` -- partially built

`conn.go` only. `Conn` owns exactly two goroutines:

- a reader that is the sole owner of the `Decoder` and sets a fresh read deadline
  before every `ReadFrame`;
- a writer that is the sole owner of the socket write side, draining two bounded
  queues so a heartbeat never queues behind a task payload.

`Classify` implements the four-way read-error taxonomy (clean close, died mid-frame,
protocol violation, deadline). Only silence and mid-frame death count as failures.

### `pkg/cluster` -- partially built

`member.go` and `election.go` only.

- `Table` merges membership by incarnation, with `Snapshot()` returning an immutable,
  deterministically ordered `View`.
- `Elect` is a pure function of a view and a score map. Idempotent, order-independent,
  hysteresis-damped, and it ranks with `health.Better` so a NaN can never win a seat.

## What is NOT built

These were planned for 3a and do not exist:

- `pkg/network/pool.go` -- peer pool, dial dedup with the lower-NodeID tie-break for
  simultaneous dials, redial backoff.
- `pkg/network/server.go` -- listener, accept loop, HELLO/HELLO_ACK handshake.
- `pkg/network/prober.go` -- `MeshProber`, the PING/PONG replacement for
  `TCPConnectProber`.
- The `Transport` interface that `pkg/cluster` is meant to consume.
- `pkg/cluster/affinity.go` -- latency-affinity worker-to-leader assignment.
- `pkg/cluster/node.go` -- the state machine wiring election triggers together.
- `pkg/cluster/partition.go` -- degraded-mode detection.
- `cmd/swarm-node` and `cmd/control-center` are still empty directories.
- `pkg/telemetry` is still only a `doc.go`.

**Nothing currently opens a socket.** There is no runnable binary yet.

## Bugs found and fixed during this work

Two real defects, both caught by tests rather than review:

1. **`Conn.Send` accepted frames after `Close`.** A `select` with both the queue send
   and the done channel ready picks a ready case at random, so a closed connection
   reported success for bytes that could never be written, roughly half the time.
   Fixed with an explicit closed-check before the frame is offered to a queue.

2. **A role change could resurrect a dead node.** In `Table.Upsert`, a record at equal
   incarnation carrying a different role took a branch that applied the whole record,
   dragging its stale `alive` state along with it. Fixed by clamping state before role
   and address are compared.

No defects were found in `pkg/protocol` or `pkg/health` during the Phase 2 test
hardening pass. That is reported as-is rather than dressed up.

## Decisions locked with the user

Phase 1/2 decisions are recorded in `docs/WORKLOG.md` sections 2.1 to 2.5. Phase 3
decisions taken during planning, not yet written to the worklog:

| Decision | Choice | Why it matters |
|---|---|---|
| Phase 3 scope | Split into 3a and 3b | Debugging gossip on an unproven transport means never knowing which layer is lying |
| Mesh topology | Full mesh | Any node can probe any node; `N^2/2` connections, fine for tens of nodes |
| Write model | Per-conn writer goroutine, bounded queues | Control blocks briefly, data drops with a counter; a dead peer cannot stall the election loop |
| Split-brain | AP: any partition elects, marks itself degraded | No consensus algorithm. Minority keeps serving. This is not Raft and the docs must say so |

The approved Phase 3a plan lives at `~/.claude/plans/harmonic-sleeping-lemon.md`.

## Outstanding debt

1. **The worklog has no Phase 3 entry.** Sections 3.1 to 3.6 cover Phase 2. The four
   Phase 3 decisions above, and the two bugs, still need recording. This did not
   happen because the session was scoped to Go only and forbidden from editing
   `docs/`.

2. **Phase 3 owes a documentation deliverable.** `docs/architecture/overview.md`
   promises an honest Raft/Paxos comparison and an account of what guarantees the
   system gives up. Not written.

3. **Concept pages need new citations.** Pages written before `pkg/network` and
   `pkg/cluster` existed state forward commitments instead of `file.go:NN` references.
   Those can now be promoted to real citations. The standing rule is that a citation
   is verified with `grep -n` at the moment it is written; fabricated line numbers
   would poison the one property that makes the curriculum worth reading.

4. **`TCPConnectProber` is still the default health prober**, and it measures the
   wrong thing. Until `MeshProber` exists, leader election would be driven by kernel
   handshake time rather than application responsiveness.

## Tooling notes for the next session

- `scripts/check-ascii.mjs` and `scripts/check-mermaid.mjs` enforce the CLAUDE.md
  section 4 rules. The Mermaid gate matters more than it looks: diagrams render
  client-side, so a malformed one builds perfectly clean and breaks only in the
  reader's browser. `npm run docs:check` runs both plus the build.
- **A semicolon is a statement separator in Mermaid.** It silently truncates label and
  note text. This has already broken one diagram here.
- `pkg/` contains **no `time.Sleep`**, and that should stay true. Determinism comes
  from injectable seams: the `Prober` function type, `ConnConfig.Now`, and gating a
  fake socket's `Write` so a test can wait on a channel for the writer goroutine to
  park rather than guessing at scheduling.
- `net.Pipe` is a useful in-memory `net.Conn` but its `SetReadDeadline` returns
  `io.ErrClosedPipe` when **either** end is closed, which differs from a real socket.
  One test was rewritten to engage the reader before closing the peer because of it.
- Custom subagent types in `.claude/agents/` load at session start. They were not
  addressable by name in this session, so the four roles were carried verbatim in
  prompts to general-purpose agents instead.
