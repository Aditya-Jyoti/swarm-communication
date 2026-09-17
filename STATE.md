# Project State

Handover snapshot, written 2026-09-17 (session 5). The decision record is `docs/WORKLOG.md`,
current through section 7.

All 5 phases are done. The repo was then split into `backend/` and `frontend/`, and the docs
were simplified (WORKLOG 7). Docs site: <https://aditya-jyoti.github.io/swarm-communication/>

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

To be re-run by the lead after merging, and this table updated.

| Check | Result |
|---|---|
| `cd backend && go build ./... && go vet ./...` | pending re-run |
| `cd backend && go test -race -count=1 ./...` | pending re-run |
| `npm run docs:check` (Node 22) | passes (docs follow-up branch) |
| `scripts/e2e.sh` through the frontend port | pending re-run |

## Open items

1. **Security audit results pending.** An audit ran in parallel with the restructure. Record
   its findings in the WORKLOG and here.
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

## Suggested next session

1. Merge and record the security audit.
2. The user decides on MEDIUM-1 and on a tombstone TTL that grows with N.
3. `doc-educator`: pages for the concepts nominated in WORKLOG 6.10 and 7.9.

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
- A semicolon in a Mermaid label silently cuts off the rest of the label.
