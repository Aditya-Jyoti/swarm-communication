---
layout: home

hero:
  name: swarm-net
  text: A self-healing P2P swarm in Go
  tagline: Nodes find each other over TCP, pick their own leaders, and recover when one dies.
  actions:
    - theme: brand
      text: System Overview
      link: /architecture/overview
    - theme: alt
      text: Run it
      link: /architecture/running-the-swarm
    - theme: alt
      text: Start with sockets
      link: /concepts/tcp-sockets-and-the-kernel

features:
  - title: A real distributed system
    details: Any number of nodes in Docker, talking raw TCP. Leaders are elected from measured health, and workers join the fastest leader.
  - title: Pluggable health
    details: Health is an interface. Latency is the default. Other metrics drop in without touching the cluster code.
  - title: Docs as a course
    details: Architecture pages explain this system. Concept pages explain the networking, Go and distributed-systems ideas under it.
---

## What this is

Two things at once:

- **A system.** A swarm of identical Go nodes. They discover each other, elect leaders, form
  clusters by latency, detect failures with heartbeats, and promote a new leader when one dies.
  A browser dashboard shows it live.
- **A course.** Each part of the system links to the ideas it depends on, such as TCP framing,
  Go's scheduler, and failure detectors.

## How to read it

```mermaid
flowchart LR
    A[Architecture] --> B[Concepts]
    B --> C[Worklog]
```

1. Read the [System Overview](/architecture/overview) for the big picture.
2. Follow the Architecture pages in sidebar order.
3. Dip into Concepts when a page links to one.
4. The [Worklog](/WORKLOG) records every design decision.

## Status

All five phases are done:

| Phase | What it built |
|---|---|
| 1 | Scaffolding, CI, this site |
| 2 | Wire protocol, pluggable health |
| 3 | TCP mesh, leader election, latency clusters, gossip |
| 4 | Failure detection, failover, replication, task routing |
| 5 | Control Center, dashboard, Docker Compose, end-to-end test |

## Run it

```bash
cp .env.example .env
docker compose up --build
```

Open the dashboard (served by the frontend container) at `http://127.0.0.1:8080`. Scale with `docker compose up -d --scale node=11`.
To test self-healing, run `scripts/e2e.sh`. It kills a leader and waits for the swarm to recover.

More: [Running the Swarm](/architecture/running-the-swarm).
