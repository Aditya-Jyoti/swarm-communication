# Project State

Written 2026-09-17 (session 4). This is a handover snapshot, not a design document. The durable
decision record is `docs/WORKLOG.md`, which is current through section 6.10.

## Where things stand

| Phase | Status |
|---|---|
| 1 -- Scaffolding, agents, VitePress | Complete |
| 2 -- Wire protocol, pluggable health | Complete |
| 3a -- P2P socket mesh, clustering engine | Complete. The audit findings are closed except MEDIUM-1. |
| 3b -- Gossip and anti-entropy | Complete |
| 4 -- Suspicion, heartbeats, failover, replication, tasks | **Complete** |
| 5 -- Control Center, dashboard, Compose, e2e | **Complete** |

`main` is at `a6b270f`. The user allowed Phases 4 and 5 to run autonomously, with no phase gates,
using parallel agents in worktrees (WORKLOG 6.1). The docs site is live at
<https://aditya-jyoti.github.io/swarm-communication/>.

## Verification at the moment of the snapshot

```
go build / go vet / gofmt          clean
go test -race -count=1 ./...       all 9 packages pass
docs check-ascii                   clean
check-mermaid (Node 22) WORKLOG    6 blocks, 0 failures
scripts/e2e.sh (seed + 5)          2/2 leaders, failover ~1s, tasks done before and after
chaos delay / kill                 node degraded / node restarts and rejoins
hash task                          correct SHA-256
```

The simultaneous-dial tests pass 18,000 runs at `-cpu 1,2,4` on 2 loaded cores after
`1aa504d` (see Worklog 6.11).

## What landed this session

| Commit(s) | What |
|---|---|
| `6694871`, `b31dd33`, `bb6956d` | Phase 4/5 contract: control stubs, `control-plane.md`, `TaskRecord.Kind/Body` |
| `ac36fbd`, `9e85f40` | Suspicion (3 missed probes -> suspect, 3s or 6 misses -> dead). A suspect leader keeps its seat. |
| `dc62692` | Leader heartbeats every 500ms (a worker detaches after 3 misses). Lamport-style terms. `SetChaosDelay`. |
| `b3f6616` | Tombstone GC after 60s |
| `69ab6d3`, `19c2d3d`, `ab5d13d`, `e63dd28` | `STATE_SYNC` ledger (500 records), built-in executor, task routing, at-least-once re-issue |
| `efd8b03` | Convergence cause 1: `Seq` orders score and role. Also hysteresis default, JOIN cap, stale JOIN_ACK, and attachment fixes. |
| `f3e5a5e`, `3daab95` | Bounded-failover simulator test. FakeClock waits without polling. |
| `870b4d5`, `c05708a`, `d6a5de5`, `dc6de42` | `pkg/telemetry`, `pkg/controlcenter`, `cmd/control-center`, node uplink |
| `0fba1cb`, `2e785e7`, `11eebdc`, `b30fb9c`, `8b7b762` | Dashboard, Dockerfile, Compose, `scripts/e2e.sh`, CI docker job |
| `68707bb`, `af1b2b4` | Uplink idle floor of 15s. CC `stop_grace_period` of 10s. |
| `c08ec5e`, `a6b270f` | Convergence cause 2: `NodeConfig.Connect` was never wired, so the mesh was a star. e2e now asserts the exact leader count. |
| `51a7829`, `e7872ac`, `37cc49c`, `b85e5c2` | Docs site `base`, `markdown.math`, lockfile, Node 22 in CI |
| docs | Control Center, running-the-swarm, and 4 concept pages. WORKLOG section 6, README, home page. |

## Open items

1. **Flaky dial test: FIXED** (`1aa504d` plus 4 test fixes). Keep stressing `pkg/network`
   at `-cpu 1` under load after any pool change.
2. **MEDIUM-1**, deferred by the user. A poisoned peer keeps its connection but stays dead
   (WORKLOG 5.2). Gossip may mitigate it, but that is untested.
3. **A death at `MaxInt64` cannot be refuted.** Needs a bound on accepted incarnations, or
   authentication.
4. **Task bodies over 1KiB are not replicated**, so such a task cannot be re-issued after
   failover.
5. **Tasks are dropped under load.** When 256 tasks are already running on a node, a new task
   fails with `busy`. A ledger that holds only pending records drops its oldest one.
6. **The tombstone TTL does not grow with N.** 60s covers $(2N-1) \times$ 2s only up to
   N of about 15. Larger swarms can bring back a dead node as a ghost, which is then detected
   and killed again.
7. **The CC is the single point of task ingress.** If it is lost, no new tasks arrive, but
   election and heartbeats keep running.
8. Incarnation is Unix seconds. A fast restart can still come back below its own death record.
   It heals through refutation.

## Suggested next session

1. Watch CI for any remaining flake at `-cpu 1`.
2. The user decides on MEDIUM-1, and whether the tombstone TTL should grow with N.
3. `doc-educator`: pages for the two concepts nominated in WORKLOG 6.10.

## Tooling notes

- **Docs need Node 22** (jsdom 30 crashes on Node 20). Mermaid check:
  `npx --yes node@22 scripts/check-mermaid.mjs <files>`. ASCII check:
  `node scripts/check-ascii.mjs <files>`.
- **GitHub:** the `gh` CLI works. `origin` fetches over HTTPS, and pushes go to
  `gh:Aditya-Jyoti/swarm-communication.git`, where `gh` is an SSH host alias, not a URL scheme.
- **Agents run isolated in git worktrees** under `.claude/worktrees/` (gitignored). Each commits
  on its own branch, and the lead merges. Worktree agents must run `git merge main` first. The
  stash stack is shared, so never use bare `git stash`.
- `pkg/` has no `time.Sleep`. Determinism comes from `Prober`, `ConnConfig.Now`,
  `cluster.FakeClock` (it now signals when timers register) and `NodeConfig.Shuffle`.
- `pkg/cluster/converge_test.go` is a deterministic simulator with per-link RTTs, reordering,
  kill and drop filters. Use it before Docker to reproduce convergence bugs.
- Tests that advertise port-0 loopback addresses cannot catch dialling bugs. Only the Docker
  e2e run can (WORKLOG 6.6).
- A semicolon in a Mermaid label silently cuts off the rest of the label.
