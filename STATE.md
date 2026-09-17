# Project State

Handover snapshot, written 2026-09-17 (session 6). The decision record is `docs/WORKLOG.md`,
current through section 8.

All 5 phases are done. The repo was then split into `backend/` and `frontend/`, and the docs
were simplified (WORKLOG 7). Docs site: <https://aditya-jyoti.github.io/swarm-communication/>

The user has waived phase-gate reviews: the lead picks defaults, records them in the worklog,
and runs work in parallel worktree agents.

## Latest feature: drone simulation (WORKLOG 8)

- Each node is a drone at a 3D position (`backend/pkg/geo`). The CC owns positions and the
  latency model and pushes them as a versioned `SIM_CONFIG` snapshot.
- A node delays each PONG by `base + distance * per_unit + jitter`. The existing median-RTT
  election and nearest-leader affinity then follow the geometry. Heartbeats are not delayed.
- `threshold: 0` and `hysteresis: -1` clear the operator override.
- Telemetry carries `flows` (frame counts per destination and type). The dashboard animates
  them in a hand-written 3D view with an Advanced panel.
- Chaos kill exits 0, and nodes use `restart: on-failure`, so killed drones stay down until
  `docker compose up -d`.
- Contract: `docs/architecture/drone-simulation.md`. Config: `SWARM_SIM_*` (README).

## Layout

| Path | What it holds |
|---|---|
| `backend/` | Go module `swarm-net`: `cmd/`, `pkg/`, `Dockerfile`, `deploy/node-entrypoint.sh` |
| `frontend/` | Dashboard, `nginx.conf.template`, `Dockerfile` (nginx, uid 101, port 8080) |
| `docs/` | VitePress site. `package.json` is at the repo root. |
| `docker-compose.yml` | `frontend`, `control-center`, `seed`, `node`. Only `frontend` is published. |
| `.env.example` | Every tunable. Copy to `.env` (gitignored). |
| `scripts/` | `e2e.sh`, `check-ascii.mjs`, `check-mermaid.mjs` |

## How to run

```bash
cp .env.example .env
docker compose up --build              # dashboard at http://127.0.0.1:8080
docker compose up -d --scale node=11   # any N
```

Without Docker: `cd backend && go run ./cmd/control-center` and `go run ./cmd/swarm-node`
with flags. See `docs/architecture/running-the-swarm.md`.

## Verification

Run by the lead at `c6252a0` (WORKLOG 8.6).

| Check | Result |
|---|---|
| `cd backend && go test -race -count=1 ./...` | all packages pass |
| `scripts/e2e.sh` through the frontend port | PASS, including kill-stays-down and sim-change steps |
| Live geometry check | workers on nearest leader, leaders are the most central drones, about 2 ms RTT per unit |
| Frontend | 80 jsdom checks, plus a headless-browser run |
| `npm run docs:check` (Node 22) | re-run after the parallel docs branch merges |

## Open items

1. **Security audit: done** (WORKLOG 7.10). Accepted, not fixed:
   - The node protocol (port 7000) has no authentication. Any container on the bridge can
     impersonate a node. This also covers item 3.
   - DNS rebinding against the loopback dashboard. Add a Host allow-list if it ever leaves
     loopback.
   - vite/esbuild/vitepress advisories affect only the docs dev server.
2. **MEDIUM-1**, deferred by the user. A poisoned peer keeps its connection but stays dead
   (WORKLOG 5.2). Gossip may mitigate it, but that is untested.
3. **A death at `MaxInt64` cannot be refuted.** Needs a bound on accepted incarnations, or
   authentication.
4. **Task bodies over 1KiB are not replicated**, so such a task cannot be re-issued after
   failover.
5. **Tasks are dropped under load.** With 256 tasks running on a node, a new one fails with
   `busy`. A ledger holding only pending records drops its oldest one.
6. **The tombstone TTL does not grow with N.** 60s covers $(2N-1) \times$ 2s only up to N of
   about 15. Larger swarms can resurrect a dead node as a ghost, which is then killed again.
7. **The CC is the single point of task ingress.** Losing it stops new tasks. Election and
   heartbeats keep running.
8. Incarnation is Unix seconds. A fast restart can come back below its own death record. It
   heals through refutation.
9. The simultaneous-dial flake is fixed (`1aa504d`). Keep stressing `backend/pkg/network` at
   `-cpu 1` under load after any pool change.
10. The convergence-test flake is fixed (WORKLOG 7.11): the simulator is now deterministic and a
   stale JOIN_ACK bug is fixed.
11. **LAN exposure.** The user runs with `BIND_ADDR=0.0.0.0` locally. Without
    `SWARM_CC_API_TOKEN`, the chaos and sim API is open to anyone on the LAN.
12. Flow counts include frames queued but lost on a dying connection.
13. The CC position map is capped at 4096 entries.
14. Emulated latency affects only PONGs, so heartbeat timing ignores distance (by design).

## Suggested next session

1. Ask the user to set `SWARM_CC_API_TOKEN` (or go back to `127.0.0.1`) while
   `BIND_ADDR=0.0.0.0` is in use.
2. Merge the parallel docs branch (drone-simulation final, network emulation, 3D projection,
   hash mixing), then run `npm run docs:check`.
3. Decide whether to add peer authentication to the node protocol.
4. The user decides on MEDIUM-1 and on a tombstone TTL that grows with N.
5. `doc-educator`: pages for the concepts nominated in WORKLOG 6.10 and 7.9.
6. Optional: count flows only when a frame is actually written, to fix item 12.

## Tooling notes

- **Go runs from `backend/`.** `cd backend` before any `go build`, `go test` or `go run`.
- **Docs need Node 22** (jsdom 30 crashes on Node 20):
  `export PATH=$(dirname $(npx --yes node@22 -p process.execPath | tail -1)):$PATH`, then
  `npm ci` and `npm run docs:check`. ASCII check for other files:
  `node scripts/check-ascii.mjs <files>`.
- **Docs cite paths only**, never line numbers (WORKLOG 7.6).
- **GitHub:** the `gh` CLI works. `origin` fetches over HTTPS. Pushes go to
  `gh:Aditya-Jyoti/swarm-communication.git`, where `gh` is an SSH host alias, not a URL scheme.
- **Worktree agents** live under `.claude/worktrees/` (gitignored). Each runs `git merge main`
  first, commits on its own branch, and the lead merges. The stash stack is shared, so never
  use bare `git stash`.
- `backend/pkg/` has no `time.Sleep`. Determinism comes from `Prober`, `ConnConfig.Now`,
  `cluster.FakeClock` and `NodeConfig.Shuffle`.
- `backend/pkg/cluster/converge_test.go` is a deterministic simulator. Use it before Docker to
  reproduce convergence bugs. Only the Docker e2e run catches dialling bugs.
- `geo.DefaultPosition` is mirrored in the frontend and pinned by a golden test. Change both
  together.
- Killed drones do not restart. Run `docker compose up -d` before re-running checks by hand.
- A semicolon in a Mermaid label silently cuts off the rest of the label.
