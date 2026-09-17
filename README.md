# swarm-net

A self-healing peer-to-peer swarm network in Go, with a live dashboard, and a full technical
curriculum documenting every layer it stands on.

$N$ identical Go nodes run in isolated containers on a user-defined Docker bridge network and
speak native TCP to each other. They measure peer health through a pluggable strategy, elect
$\max(1, \lceil N \times \text{threshold} \rceil)$ leaders from the scores, and cluster themselves
by latency affinity: each worker joins whichever leader answers fastest, with no fixed quotas. When
a leader stops answering heartbeats, its workers suspect it, the swarm confirms the death,
promotes the healthiest worker, and the workers reattach. Pending tasks are re-issued. A Control
Center sends tasks, collects telemetry, and serves a vanilla HTML/JS dashboard where you can
watch all of this happen. The dashboard also has chaos controls for killing nodes and adding
latency on purpose.

**Docs site:** <https://aditya-jyoti.github.io/swarm-communication/>

## Status

**All 5 phases are implemented.**

| Phase | Status |
|---|---|
| 1 -- Scaffolding, CI, docs site | Complete |
| 2 -- Wire protocol, pluggable health | Complete |
| 3a -- TCP mesh, election, latency affinity | Complete |
| 3b -- Gossip and anti-entropy | Complete |
| 4 -- Suspicion, heartbeat failover, state replication, task routing | Complete |
| 5 -- Control Center, dashboard, Docker Compose | Complete |

Known open items are listed in [`STATE.md`](STATE.md) and in section 6.9 of the
[Worklog](docs/WORKLOG.md).

## Quick start

```bash
docker compose up --build
```

Open <http://localhost:8080>. The default stack is a Control Center, a `seed` node and 5
replicas (N = 6, so 2 leaders). Pick any N:

```bash
docker compose up --build --scale node=11
```

From the dashboard, submit tasks, kill a leader, or add latency, then watch the swarm re-elect.

## End-to-end check

```bash
scripts/e2e.sh                # seed + 5 nodes
E2E_NODES=8 scripts/e2e.sh    # seed + 8 nodes
```

The script starts the stack under its own Compose project and waits for the exact leader count.
It runs a batch of tasks, `docker kill`s a leader, waits for the swarm to heal, then runs a
second batch. It needs Docker, plus `jq` or `python3`.

## Layout

```
cmd/       swarm-node and control-center binaries
pkg/       protocol, network, health, cluster, telemetry, controlcenter
web/       vanilla JS dashboard, embedded into the control-center binary
deploy/    node container entrypoint
docs/      the VitePress curriculum
scripts/   e2e.sh and the docs checkers
```

`Dockerfile` and `docker-compose.yml` are in the repository root. See
[`docs/architecture/repo-layout.md`](docs/architecture/repo-layout.md) for why the package
boundaries are drawn where they are.

## Documentation

Read it online at <https://aditya-jyoti.github.io/swarm-communication/>, or build it locally
(needs Node 22):

```bash
npm install
npm run docs:dev
```

Start with the [System Overview](docs/architecture/overview.md), then
[Running the Swarm](docs/architecture/running-the-swarm.md). The [Worklog](docs/WORKLOG.md)
records every decision, the alternatives that were rejected, and why.

## Building

```bash
go build ./...
```

Requires Go 1.27+. The Go build does not need Node. `package.json` exists only for the docs
site.

## Running without Docker

A three-node swarm on one machine, one terminal each:

```bash
SWARM_NODE_ID=n1 SWARM_LISTEN=:7001 SWARM_ADVERTISE=127.0.0.1:7001 go run ./cmd/swarm-node
SWARM_NODE_ID=n2 SWARM_LISTEN=:7002 SWARM_ADVERTISE=127.0.0.1:7002 SWARM_SEEDS=127.0.0.1:7001 go run ./cmd/swarm-node
SWARM_NODE_ID=n3 SWARM_LISTEN=:7003 SWARM_ADVERTISE=127.0.0.1:7003 SWARM_SEEDS=127.0.0.1:7001 go run ./cmd/swarm-node
```

To add the dashboard, run `go run ./cmd/control-center -listen :7000` and start each node with
`SWARM_CONTROL_CENTER=127.0.0.1:7000`.

Every setting can be set with an env var or with the matching flag, e.g. `-gossip-interval`. If
both are set, the flag wins.

### swarm-node

| Env var | Default | Meaning |
|---|---|---|
| `SWARM_NODE_ID` | hostname | Node identity |
| `SWARM_LISTEN` | `:7000` | TCP bind address |
| `SWARM_ADVERTISE` | `<hostname>:<listen port>` | Address peers dial |
| `SWARM_SEEDS` | empty | Comma-separated peers to dial at start-up |
| `SWARM_CONTROL_CENTER` | empty | Control Center address. Empty means no uplink. |
| `SWARM_TELEMETRY_INTERVAL` | `1s` | How often telemetry is sent to the Control Center |
| `SWARM_THRESHOLD` | `0.3` | Leader fraction in (0,1] |
| `SWARM_PROBE_INTERVAL` | `1s` | How often peers are scored |
| `SWARM_ELECTION_FLOOR` | `30s` | Interval of the periodic re-election |
| `SWARM_GOSSIP_INTERVAL` | `2s` | Anti-entropy: send the full view to one peer per interval |
| `SWARM_IDLE_TIMEOUT` | `15s` | Silence before a connection is dead (must exceed 3x probe interval) |
| `SWARM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

### control-center

| Env var | Flag | Default | Meaning |
|---|---|---|---|
| `SWARM_CC_LISTEN` | `-listen` | `:7000` | TCP bind address that nodes dial |
| `SWARM_CC_HTTP` | `-http` | `:8080` | Dashboard and API bind address |
| `SWARM_LOG_LEVEL` | `-log-level` | `info` | `debug`, `info`, `warn`, `error` |

The HTTP API (`/api/state`, `/api/tasks`, `/api/chaos`, `/ws`) is specified in
[Control Plane Contract](docs/architecture/control-plane.md).

## Testing

```bash
go test -race ./...
scripts/e2e.sh
```
