# swarm-net

A self-healing peer-to-peer swarm network in Go, with a live dashboard (planned) -- and a full
technical curriculum documenting every layer it stands on.

The target design: $N$ identical Go nodes run in isolated containers on a user-defined Docker bridge
network and speak native TCP to each other. They measure peer health through a pluggable strategy, elect
$\max(1, \lceil N \times \text{threshold} \rceil)$ leaders from the scores, and cluster themselves
by latency affinity -- workers join whichever leader answers fastest, with no fixed quotas. When a
leader stops answering heartbeats, its cluster isolates it, re-evaluates the survivors, promotes the
healthiest worker, and reattaches. A Control Center broadcasts tasks, aggregates telemetry, and
serves a vanilla HTML/JS dashboard where you can watch all of it happen -- including chaos controls
for killing nodes and injecting latency on purpose.

Not all of this exists yet. See Status.

## Status

**Phases 1-3 of 5 complete.**

| Phase | Status |
|---|---|
| 1 -- Scaffolding, CI, docs site | Complete |
| 2 -- Wire protocol, pluggable health | Complete |
| 3a -- TCP mesh, election, latency affinity | Complete |
| 3b -- Gossip and anti-entropy | Complete |
| 4 -- Heartbeat failover, suspicion, state replication | Not started |
| 5 -- Control Center, dashboard, Docker Compose | Not started |

There is no Dockerfile, `docker-compose.yml`, Control Center or dashboard yet. Those arrive in
Phase 5. Today the swarm runs as plain processes (see Running).

## Layout

```
cmd/       swarm-node binary (control-center arrives in Phase 5)
pkg/       protocol, network, health, cluster, telemetry
web/       vanilla JS dashboard (Phase 5, not yet created)
deploy/    Dockerfiles and docker-compose.yml (Phase 5, not yet created)
docs/      the VitePress curriculum
```

See [`docs/architecture/repo-layout.md`](docs/architecture/repo-layout.md) for why the boundaries
fall where they do.

## Documentation

```bash
npm install
npm run docs:dev
```

Start at the [System Overview](docs/architecture/overview.md), then work through the concept guides
in sidebar order. The [Worklog](docs/WORKLOG.md) records every decision, the alternatives rejected,
and the reasoning.

## Building

```bash
go build ./...
```

Requires Go 1.27+. The Go build has no Node dependency; `package.json` exists only for the docs site.

## Running

Start a three-node swarm on one machine, one terminal per node:

```bash
SWARM_NODE_ID=n1 SWARM_LISTEN=:7001 SWARM_ADVERTISE=127.0.0.1:7001 go run ./cmd/swarm-node
SWARM_NODE_ID=n2 SWARM_LISTEN=:7002 SWARM_ADVERTISE=127.0.0.1:7002 SWARM_SEEDS=127.0.0.1:7001 go run ./cmd/swarm-node
SWARM_NODE_ID=n3 SWARM_LISTEN=:7003 SWARM_ADVERTISE=127.0.0.1:7003 SWARM_SEEDS=127.0.0.1:7001 go run ./cmd/swarm-node
```

Every setting is an env var or the matching flag, e.g. `-gossip-interval` (flags win).

| Env var | Default | Meaning |
|---|---|---|
| `SWARM_NODE_ID` | hostname | Node identity |
| `SWARM_LISTEN` | `:7000` | TCP bind address |
| `SWARM_ADVERTISE` | `<hostname>:<listen port>` | Address peers dial |
| `SWARM_SEEDS` | empty | Comma-separated peers to dial at start-up |
| `SWARM_THRESHOLD` | `0.3` | Leader fraction in (0,1] |
| `SWARM_PROBE_INTERVAL` | `1s` | How often peers are scored |
| `SWARM_ELECTION_FLOOR` | `30s` | Periodic re-election |
| `SWARM_GOSSIP_INTERVAL` | `2s` | Anti-entropy: full view to one peer per interval |
| `SWARM_IDLE_TIMEOUT` | `15s` | Silence before a connection is dead (must exceed 3x probe interval) |
| `SWARM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SWARM_CONTROL_CENTER` | empty | Parsed but unused until Phase 5 |

## Testing

```bash
go test -race ./...
```
