# swarm-net

A self-healing peer-to-peer swarm network in Go, with a live dashboard — and a full technical
curriculum documenting every layer it stands on.

$N$ identical Go nodes run in isolated containers on a user-defined Docker bridge network and speak
native TCP to each other. They measure peer health through a pluggable strategy, elect
$\max(1, \lceil N \times \text{threshold} \rceil)$ leaders from the scores, and cluster themselves
by latency affinity — workers join whichever leader answers fastest, with no fixed quotas. When a
leader stops answering heartbeats, its cluster isolates it, re-evaluates the survivors, promotes the
healthiest worker, and reattaches. A Control Center broadcasts tasks, aggregates telemetry, and
serves a vanilla HTML/JS dashboard where you can watch all of it happen — including chaos controls
for killing nodes and injecting latency on purpose.

## Status

**Phase 1 of 5 complete** — scaffolding, agent definitions, documentation framework, and the first
concept discovery pass. The Go implementation starts in Phase 2.

## Layout

```
cmd/       swarm-node and control-center binaries
pkg/       protocol, network, health, cluster, telemetry
web/       vanilla JS dashboard (embedded into the control-center binary)
deploy/    Dockerfiles and docker-compose.yml
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
