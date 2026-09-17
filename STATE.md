# Project State

Written 2026-09-17 (session 2). Handover snapshot, not a design document: what
exists, what is verified, what is half-finished, what the next session needs to know.

Note: this file was deleted earlier in this same session (commit `cfece07`) on the
theory that `docs/WORKLOG.md` should be the single status record. That was then
undercut by a standing "no documentation writes this session" constraint, which left
the worklog frozen at "Phase 3 Planning" while seven commits landed. The file is
restored because a stale single record is worse than two honest ones. **The worklog
is still the durable decision record and is still badly behind -- see Debt.**

## Where things stand

| Phase | Status |
|---|---|
| 1 -- Scaffolding, agents, VitePress | Complete |
| 2 -- Wire protocol, pluggable health | Complete |
| 3a -- P2P socket mesh, clustering engine | Complete and merged (`d1d94cc`) |
| 3b -- Gossip and anti-entropy | **Designed and approved, NOT started** |
| 4 -- Heartbeats, failover, replication | Not started |
| 5 -- Control Center, dashboard, Compose | Not started |

Branch `chore/reconcile-state-and-ci`, 7 commits ahead of `d1d94cc`. Nothing pushed.

## Verification at the moment of the snapshot

```
go build ./...                clean
go vet ./...                  clean
gofmt -l pkg cmd              clean
go test -race -count=2 ./...  all pass
npm run docs:check            ASCII clean, 55 Mermaid parse, site builds
fuzz: 60s / 4.03M execs       no crashers
```

| Package | Coverage |
|---|---|
| `pkg/health` | 100.0% |
| `pkg/network` | 98.3% |
| `pkg/protocol` | 97.6% |
| `pkg/cluster` | 97.5% |
| `cmd/swarm-node` | 93.5% |
| `pkg/telemetry` | no code, no tests |

## What landed this session

| Commit | What |
|---|---|
| `cfece07` | Deleted STATE.md (superseded by this file) |
| `7684686` | `.github/workflows/ci.yml` -- gofmt, vet, `go test -race -count=2`. The Go suite had NO CI gate before this; `deploy.yml` built docs only |
| `c1da135` | `FuzzDecodeFrame` over the frame decoder. First fuzz target in the repo |
| `fc63794` | Framing throughput benchmarks |
| `b52e2be` | Membership merge benchmarks. First benchmarks in the repo |
| `6a6b523` | `perf(cluster)`: Snapshot 4 -> 1 allocs, Leaders 1.4-1.7x, `Size()` no longer allocates 57 KB to count. NOTE: commit body has a typo, "unin lineable" |
| `f9e3b29` | `fix(cluster)`: HIGH-1, the unmeasurable-node-reports-best-score bug |

## Decisions locked with the user this session

| Decision | Choice |
|---|---|
| STATE.md | Delete it -- later reversed by this file |
| `LEAVE` handling | `markDead` instead of `Remove`. **NOT YET IMPLEMENTED** |
| Gossip peer selection | Shuffled round-robin, k=1, ~2s interval. Amends `docs/WORKLOG.md:585`, which says "one random peer". **NOT YET IMPLEMENTED** |
| 3b scope | Dissemination and repair only. No suspicion state, no indirect probing -- those stay Phase 4 |
| HIGH-2 fix shape | Bump incarnation on death so a death outranks a concurrent refutation. **NOT YET IMPLEMENTED** |
| MEDIUM-2 | Prune `n.local` / `n.missed`. **NOT YET IMPLEMENTED** |
| MEDIUM-1 | Deferred by the user |

## Audit findings -- open

A read-only audit of Phase 3a ran this session. One of four findings is fixed.

**HIGH-1 -- FIXED in `f9e3b29`.** `selfScore` returned 0 (the best value, since
lower-is-better) when it had peers but could measure none of them.
*Still owed:* dedicated regression tests for the join-time displacement case and the
all-unmeasured swarm. The existing suite passes and covers the fallback; those two
scenarios have no test of their own.

**HIGH-2 -- OPEN. This blocks Phase 3b.** A death record is weaker than a newer alive
record, so it loses the merge it most needs to win.
- `pkg/cluster/member.go:301` -- `SetState` records death at the member's CURRENT
  incarnation without bumping it. "B dead at 6" is the strongest a neighbour can say.
- `pkg/cluster/member.go` `Upsert`, higher-incarnation branch -- an unconditional full
  overwrite, including a return to `alive`.
- `pkg/cluster/node.go:833` -- `refuteIfNeeded` returns early on `StateAlive`, so an
  alive-about-self record at a higher incarnation is never refuted.

Interleaving: B refutes ("alive at 7"), that frame queues at A, B is killed, A's
select takes PeerDown first (dead at 6) then the frame (alive at 7, unconditional
overwrite). B is resurrected and nothing ever buries it -- nothing converts missed
probes into a death; that is Phase 4.

**Today this needs one unlucky race. Under anti-entropy every node still holding
"alive at 7" re-asserts it every round and wins every time. One surviving record
re-infects the swarm and never decays. Do not build 3b until this is fixed.**
Approved fix: bump the incarnation when recording death.

**MEDIUM-1 -- OPEN, deferred by the user.** A peer marked dead by `payloadOrPoison`
(`node.go:735`) keeps its TCP connection. A payload decode failure is a framing
SUCCESS, so no framing error follows, no PeerDown/PeerUp pair is generated, and
`Revive` -- reachable only from PeerUp (`node.go:586`) -- never runs. The peer is
permanently invisible to membership while a healthy connection to it stays open.
Reachable via version skew. 3b gossip would make this self-heal in one round.

**MEDIUM-2 -- OPEN, approved for fixing.** `n.local` and `n.missed` are never pruned
(only writes are around `node.go:981-986`; nothing deletes). `Table.Remove` and
`markDead` do not touch them. So `selfScore` takes a median over peers that have
LEFT: ten short-lived slow workers permanently skew a node's self-report, and the
stale entries are never re-probed so they never go NaN and never drop out. Both maps
grow unbounded and are deep-copied on every `Status()` call.

**Not filed, but know about it:** `n.incarnation` is set to a peer-supplied `int64`
plus one with no sanity check (`node.go:836`). A peer claiming `math.MaxInt64`
overflows it negative. Needs a malicious or corrupt peer, and Phase 3a has no
authentication at all, so it is not the weakest link -- but if 3b adds any bound on
incarnation, that is the place.

## Benchmark baseline

Established this session; re-measure against these before/after any 3b change.

- `Table.Upsert`: flat and ZERO-allocation across all branches, 85-99 ns/op from
  n=10 to n=1000. The steady-state anti-entropy merge is a mutex plus a map lookup.
  This is the result 3b wants; do not regress it.
- `Table.Snapshot`: 1 alloc, but per-member CPU still triples 10 -> 1000 (~362 us at
  n=1000). Profiled: 88% is the sort, and 43% of that is moving 64-byte `Member`
  values. Removing reflection removed the closure allocations, not the dominant cost.
  The remaining cost is element width.
- `protocol.SetPayload` on a 50-member roster: **163 allocs / 125 us**, about 5x the
  cost of framing the whole envelope. Cause is `MemberRecord.MarshalJSON`
  (`pkg/protocol/message.go:369`) forcing `encoding/json` down the reflection path per
  record. `WriteEnvelope` over an ALREADY-populated payload is 1 alloc.
  At the approved k=1 fan-out this costs 125 us per 2s and does not bite. It would
  bite hard if fan-out ever goes per-peer.

## Known trap for Phase 3b

`handleMembershipDelta` calls `n.table.Snapshot()` **once per record**, inside the
per-record loop (`node.go:765`). Invisible today because deltas are mostly one
record. Anti-entropy makes every message an N-record delta, and this becomes
O(N^2 log N) per received delta on the single event loop. Hoist the snapshot out of
the loop. Note it slightly changes semantics: records within one delta would no
longer see each other's effects, which for role lookup is arguably more correct.

Also: **anti-entropy as designed repairs liveness but NOT roles.** `node.go:760-768`
deliberately ignores a relayed role (only `rec.ID == from` is trusted), so a lost
`announceSelf` role broadcast is never repaired by gossip. Accepted cost or a Phase 4
item -- undecided.

## Debt

1. **`docs/WORKLOG.md` is frozen at "Phase 3 Planning"** and is the single largest
   debt. Unrecorded: Phase 3a completion, all 7 commits above, 4 audit findings,
   and 6 decisions locked with the user. Its section 4.4 debt table also lists two
   items as unpaid that are actually done (`why-not-consensus.md` exists;
   `MeshProber` exists and is wired).
2. **`README.md:17` and `docs/index.md:61` both still say "Phase 1 of 5 complete".**
   Three phases stale. README also claims in the present tense that the system runs
   under `docker compose up` -- there is no Dockerfile or compose file in the repo.
3. **Concept page citations are stale**, worse than the worklog records.
   `docs/concepts/gossip-and-anti-entropy.md` cites `Table.Upsert` at `member.go:165`
   (now 211), `Snapshot` at 245 (now ~352), `Remove` at 232 (now 339), and quotes
   `sort.Slice`-era code that no longer exists. Its "Epidemics" section describes
   fanout arithmetic for a mechanism this system does not use -- over a full mesh,
   push-on-change reaches everyone in one hop.
   `docs/concepts/failure-detectors.md` reads as though suspicion ships;
   `StateSuspect` is defined in `member.go:46` and assigned NOWHERE in `pkg/`.
4. `pkg/telemetry` is still `doc.go` only. `cmd/control-center/`, `web/static/` and
   `deploy/` are all empty directories.
5. `Config.ControlCenter` (`cmd/swarm-node/config.go:52`) is parsed, validated, never
   used. `go.mod` has zero dependencies; Phase 5 pre-decided `github.com/coder/websocket`.

## Suggested next session

1. Fix HIGH-2 (`member.go` `SetState` incarnation bump + `member_test.go`
   regressions). Blocks everything else.
2. `node.go`: `LEAVE` -> `markDead`, prune `n.local`/`n.missed`, hoist the snapshot
   out of the per-record loop, add the two owed HIGH-1 regression tests.
3. Reconcile the docs (items 1-3 above). Needs the no-docs constraint lifted.
4. Then build 3b: gossip ticker, `gossipRound`, shuffled round-robin peer selection
   with an injectable `Shuffle` seam, `NodeConfig.GossipInterval`.

## Tooling notes

- `pkg/` contains NO `time.Sleep` and that must stay true. Determinism comes from
  injectable seams: the `Prober` func type, `ConnConfig.Now`, `cluster.FakeClock`.
- `FakeClock.Advance` blocks on each tick until the receiver takes it, so when it
  returns the loop has observed the tick; `barrier` then guarantees the handler ran.
  If two tickers share a deadline, `nextDue` breaks the tie on REGISTRATION ORDER --
  so the order of `NewTicker` calls in `Run` is load-bearing for those tests.
- A semicolon is a statement separator in Mermaid and silently truncates label text.
- `npm run docs:check` runs ASCII + Mermaid + build.
- Subagents sharing a file clobber each other. Two file collisions happened this
  session, and a stopped agent's writes and commits can land AFTER the stop returns.
  Split agent work by file, and re-check `git status` and `git log` after any stop.
