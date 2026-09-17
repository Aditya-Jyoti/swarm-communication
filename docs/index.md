---
layout: home

hero:
  name: swarm-net
  text: A self-healing P2P swarm, built in the open
  tagline: N Go nodes. Raw TCP. Dynamic leaders. Every design decision documented as a lesson.
  actions:
    - theme: brand
      text: System Overview
      link: /architecture/overview
    - theme: alt
      text: Start with sockets
      link: /concepts/tcp-sockets-and-the-kernel
    - theme: alt
      text: Engineering Worklog
      link: /WORKLOG

features:
  - title: A real distributed system
    details: Arbitrary N nodes in isolated containers, discovering each other over native TCP sockets, electing leaders from measured health, clustering by latency affinity, and re-electing when a leader dies.
  - title: Pluggable by construction
    details: Health is an interface, not a number. Latency ships as the default strategy; CPU, memory or packet-loss strategies drop in without touching cluster logic.
  - title: Documented as a curriculum
    details: Every page explains the mental model, what the kernel or Go runtime actually does underneath, where this system depends on it, and what breaks when it is misunderstood.
---

## What this repository is

Two things at once.

**A system.** A peer-to-peer swarm of Go processes that finds its own peers, measures their health,
elects a dynamic number of leaders, partitions itself into latency-affine clusters, detects leader
death through missed heartbeats, promotes a replacement, and keeps serving work while it happens.
It runs under one `docker compose up`, and you can watch it heal in a browser.

**A curriculum.** The system is an excuse to go all the way down. TCP does not have messages, so we
explain framing. A goroutine per connection only works because of the netpoller, so we explain
epoll. A membership table read by one goroutine and written by another is a data race, so we explain
the Go memory model. Every guide points back at the code that depends on it.

## How to read it

```mermaid
flowchart TD
  A["Architecture"] --> A1["What this system does,<br/>and why it is shaped this way"]
  A --> B["Concepts"]
  B --> B1["Networking and OS internals"]
  B --> B2["Go runtime and concurrency"]
  B --> B3["Distributed systems theory"]
  B --> C["Worklog"]
  C --> C1["The decisions, the alternatives<br/>rejected, and the reasoning"]
```

If you are here to learn rather than to operate, read the Architecture overview first for the map,
then work through Concepts in sidebar order. The concept pages are written to stand alone, but they
cross-link heavily, and they get more out of you if read in order.

## Status

All 5 phases are implemented:

| Phase | What it built |
|---|---|
| 1 | Scaffolding, CI, this site |
| 2 | Wire protocol, pluggable health strategy |
| 3 | TCP mesh, leader election, latency affinity, gossip with anti-entropy |
| 4 | Suspicion, heartbeat failover, `STATE_SYNC` replication, at-least-once task routing |
| 5 | Control Center, live dashboard, Docker Compose, end-to-end test |

## Run it

```bash
docker compose up --build
```

Open `http://localhost:8080`. Scale with `--scale node=11`. To check self-healing end to end,
run `scripts/e2e.sh`, which kills a leader and waits for the swarm to recover. See
[Running the Swarm](/architecture/running-the-swarm).

Every decision is recorded in the [Worklog](/WORKLOG), and the open items are in its section 6.9.
