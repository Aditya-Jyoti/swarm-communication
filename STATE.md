# Project State

Written 2026-09-17 (session 3). Handover snapshot, not a design document. The durable
decision record is `docs/WORKLOG.md` (now current through section 5.7).

## Where things stand

| Phase | Status |
|---|---|
| 1 -- Scaffolding, agents, VitePress | Complete |
| 2 -- Wire protocol, pluggable health | Complete |
| 3a -- P2P socket mesh, clustering engine | Complete, audit findings closed except MEDIUM-1 |
| 3b -- Gossip and anti-entropy | **Complete** on `feat/phase-3b-gossip` |
| 4 -- Heartbeats, failover, replication | Not started -- **awaiting user review of 3b** |
| 5 -- Control Center, dashboard, Compose | Not started |

Branch `feat/phase-3b-gossip`, 16 commits ahead of `main` (`05680c9`). Not pushed, no PR.
`main` == `origin/main`.

## Verification at the moment of the snapshot

```
go build / go vet / gofmt       clean
go test -race -count=2 ./...    all pass
fuzz FuzzDecodeFrame 20s        no crashers
docs:check:ascii                clean
check-mermaid (under Node 22)   63 blocks, 0 failures
docs:build                      builds
grep time.Sleep pkg/            comments only
```

Coverage: cluster 97.6%, swarm-node 93.6%, network 98.3%, protocol 97.6%, health 100%.

## What landed this session

| Commit | What |
|---|---|
| `2cc3060` | HIGH-2 fixed: `SetState` bumps incarnation on death (`NextIncarnation` saturates) |
| `c9c7dd9` | `refuteIfNeeded` clamps instead of overflowing |
| `edb713f` | LEAVE and clean close -> `markLeft` (dead record, not `Remove`) |
| `610a210` | MEDIUM-2 fixed: `pruneLocal` in `membershipChanged`; late probe results for gone peers ignored |
| `4012bf2` | Delta handling linear: one snapshot + binary search. n=1000: 125 ms -> ~211 us |
| `7f5af55` | HIGH-1 regression tests |
| `a983c1a` | `gossipRound`: shuffled round-robin, k=1, `GossipInterval` 2s, `Shuffle` seam |
| `c01ec88` | `pushRecord`: push-on-change for first-hand deaths only |
| `cd3e6e4` | Multi-node convergence tests; fixed a race in the `quiesce` test helper |
| `b048c37` | `SWARM_GOSSIP_INTERVAL` |
| `871c185` | Precise repair-bound comments (2N-1 rounds worst case) |
| docs x5 | WORKLOG section 5, README/index status, gossip + failure-detector rewrites, new `monotonic-merge-and-incarnation` page |

## Open items

1. **CI docs pipeline is likely broken (pre-existing, not fixed).** `deploy.yml` uses
   Node 20 + `npm ci`. `package-lock.json` is out of sync with `package.json` (missing
   `jsdom@30.1.0`), and jsdom 30 / undici 8 crash on Node 20
   (`webidl.util.markAsUncloneable is not a function`). Fix: bump `node-version` to 22
   and regenerate the lockfile. Needs user sign-off (dependency change).
   A gitignored `node_modules/` now exists locally from a `--no-save` install.
2. **MEDIUM-1 open, deferred by user.** Gossip is expected to mitigate it; untested.
3. **Tombstones are never garbage-collected.** Dead/left members stay in the table
   forever. Phase 4 item.
4. **Dead at `MaxInt64` is unrefutable.** Needs a hostile peer; no auth exists anyway.
5. **Incarnation is Unix seconds.** A fast restart can come back below the death its peers
   hold; it still heals via refutation, one round trip later.
6. Anti-entropy repairs each sender's OWN role and score, not relayed roles (by design).
7. `pkg/telemetry` is `doc.go` only; `cmd/control-center/`, `web/static/`, `deploy/` empty.
   `Config.ControlCenter` parsed but unused. Phase 5 pre-decided `github.com/coder/websocket`.

## Suggested next session

1. User reviews 3b (CLAUDE.md section 6 gate); decide on push/PR and the CI Node fix.
2. Phase 4 planning with `system-architect`: suspicion state + K-missed-beats in
   `markDead`, heartbeats, tombstone GC, `STATE_SYNC` replication (WORKLOG 4.2).

## Tooling notes

- `pkg/` contains NO `time.Sleep`. Determinism comes from `Prober`, `ConnConfig.Now`,
  `cluster.FakeClock`, and now `NodeConfig.Shuffle`.
- Ticker registration order in `Run` is load-bearing (FakeClock ties): probe, floor, gossip.
- `quiesce` in tests now loops until a full pass delivers no frame.
- Mermaid check needs Node >= 22 locally: `npx --yes node@22 scripts/check-mermaid.mjs docs/*.md docs/**/*.md`.
- A semicolon in Mermaid label text silently truncates it.
- Agents sharing a file clobber each other; this session docs agents did not commit and
  the lead committed per file, which avoided collisions.
