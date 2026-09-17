---
title: System Overview
outline: deep
---

# System Overview

`swarm-net` is a peer-to-peer swarm of $N$ identical Go processes, each in its own container on a
user-defined Docker bridge network, plus one Control Center that coordinates nothing and observes
everything.

The design constraint that shapes every other decision: **there are no special nodes**. Every node
ships the same binary and the same configuration schema. Leadership is a role a node acquires from
measured health, not a flag it is started with. Kill any container and the swarm reshapes.

## The map

```mermaid
flowchart TD
    BR["Browser dashboard (vanilla JS)"]
    CC["Control Center: cmd/control-center"]

    subgraph CLA["Cluster A"]
        LA{{"Leader A"}}
        WA1(["worker"])
        WA2(["worker"])
        WA3(["worker"])
        WA4(["worker"])
    end

    subgraph CLB["Cluster B"]
        LB{{"Leader B"}}
        WB1(["worker"])
        WB2(["worker"])
    end

    subgraph CLC["Cluster C"]
        LC{{"Leader C"}}
        WC1(["worker"])
        WC2(["worker"])
        WC3(["worker"])
    end

    BR <-->|"WebSocket: telemetry, tasks, chaos"| CC

    CC ==>|"TCP, framed"| LA
    CC ==>|"TCP, framed"| LB
    CC ==>|"TCP, framed"| LC

    WA1 -->|"lowest latency"| LA
    WA2 --> LA
    WA3 --> LA
    WA4 --> LA
    WB1 -->|"lowest latency"| LB
    WB2 --> LB
    WC1 -->|"lowest latency"| LC
    WC2 --> LC
    WC3 --> LC

    LA -.->|"health probe"| LB
    LB -.->|"health probe"| LC
    LA -.->|"health probe"| LC
    WA4 -.->|"health probe"| LB
    WB2 -.->|"health probe"| LC
    WC3 -.->|"health probe"| LA
```

Solid double arrows are the Control Center's task and telemetry path. Solid single arrows are
cluster attachment: a worker attaches to whichever leader answers fastest. Dashed arrows are health
probing, which runs across the **full P2P mesh** -- every node can probe every other, not only its
own leader. Only a representative subset is drawn; at $N=10$ the real mesh is 45 edges.

The number of leaders is $\lceil N \times \text{threshold} \rceil$, minimum 1. Cluster sizes are
whatever affinity produces.

Two distinct traffic planes overlay the same TCP mesh:

- **The control plane.** Peer discovery, health probes, election messages, heartbeats, membership
  gossip. Node-to-node, constant, small, latency-sensitive.
- **The data plane.** Tasks injected at the Control Center, sent to one leader, assigned by it to
  one of its workers; results and telemetry sent back up. Bursty, larger, throughput-sensitive.

They share a transport but not a priority. Heartbeats must not queue behind a task payload -- a
fact that constrains the framing and connection-pool design in `pkg/network`.

## The four mechanisms

### 1. Dynamic leadership threshold

The number of leaders is a function of swarm size, not a constant:

$$\text{LeaderCount} = \max(1, \lceil N \times \text{threshold} \rceil)$$

With `threshold = 0.3`: $N=3 \Rightarrow 1$ leader, $N=10 \Rightarrow 3$, $N=25 \Rightarrow 8$. The
$\max(1, \ldots)$ clamp keeps a single-node swarm legal -- a degenerate case that must work,
because it is the first thing anyone runs.

Candidates are ranked by health score; the top `LeaderCount` become leaders. The interesting
question is not the formula but what happens when two nodes compute different values of $N$ at the
same instant. That is a split-brain scenario, covered in
[Split-Brain and Quorum](/concepts/split-brain-and-quorum) and
[Convergence Debugging](/architecture/convergence-debugging).

Because leadership is acquired rather than configured, role is a state in a machine:

```mermaid
stateDiagram-v2
    [*] --> Leader: alone at start, elects itself
    Leader --> Detached: demoted, a healthier peer outranks it
    Detached --> Worker: JOIN_ACK accepted by the best leader
    Worker --> Detached: 3 silent ticks, leader suspected, or leader stepped down
    Worker --> Worker: re-home to a leader better by the margin
    Detached --> Leader: elected
    Worker --> Leader: promoted, healthiest survivor after failover
    Leader --> [*]: shutdown
    Worker --> [*]: shutdown
```

A detached node is a worker with no leader. Status reports it with an empty `leader`.

### 2. Affinity clustering, not quotas

A worker is not assigned to a leader. It probes every elected leader, measures the round-trip
health score, and joins the best one. There are no fixed percentage splits and no balancing
authority.

The consequence is worth stating up front: **clusters will be uneven**, and that is the intended
behaviour. If one leader sits on a fast path from seven workers, it gets seven workers. Load
balancing is a separate concern from affinity, and conflating them produces a system that is bad at
both. If capacity limits are later required, they enter as an explicit admission-control policy on
the leader, not as a quota baked into the clustering rule.

### 3. Health as an interface

```go
// pkg/health/strategy.go:83 (comments trimmed)
type HealthStrategy interface {
    Name() string
    EvaluateScore(ctx context.Context, target protocol.NodeAddress) (float64, error)
}
```

`LatencyHealthStrategy` ships as the default. The contract exists so that CPU load, free memory,
packet loss, or a composite weighted score can be substituted without `pkg/cluster` learning
anything new. The cluster logic must therefore never inspect *what* a score means -- only compare
scores. Any code that assumes "score is milliseconds" is a bug against this contract.

The contract's rules (lower is better, NaN means unmeasured, a cancelled probe is not a failure)
are in `pkg/health/strategy.go` and the [Worklog](/WORKLOG), section 2.3. Election ranks on each
node's **self-reported** score, while a worker picks its leader on its **own** measurements.

### 4. Self-healing

```mermaid
sequenceDiagram
    participant L as Assigned leader
    participant W as Worker
    participant P as Surviving peers
    participant CC as Control Center

    L->>W: HEARTBEAT
    W-->>L: HEARTBEAT_ACK
    L->>W: HEARTBEAT
    W-->>L: HEARTBEAT_ACK
    Note over L: crash
    Note over W,P: socket reset, or 3 ticks with no beat
    W->>W: suspect the leader, detach, carry its pending tasks
    Note over W,P: leader keeps its seat while only suspect
    Note over W,P: 3s later the suspicion is confirmed, leader dead
    Note over P: election without it promotes the healthiest node
    W->>P: JOIN_CLUSTER to the best leader, hand over the tasks
    P->>CC: TELEMETRY now reports role leader
```

$K = 3$ ticks, not one, because a single missed beat is indistinguishable from a scheduler hiccup
or a GC pause. The gap between "slow" and "dead" is unknowable from the outside -- this is the
core insight of failure-detector theory, and $K$ and the heartbeat interval are the two knobs
that trade detection latency against false positives. A leader crash is repaired within about
$3\,\text{s} + 2 \times 0.5\,\text{s}$. The mechanics are in
[Failure Detection and Failover](./failure-detection-and-failover), and what happens to in-flight
work in [Replication and Tasks](./replication-and-tasks).

## Package boundaries

| Package | Owns | Must not know about |
|---|---|---|
| `pkg/protocol` | Wire schemas, message types, framing codec | Cluster roles, health semantics |
| `pkg/network` | TCP listener, dialer, connection pool, read/write loops | Message *meaning* |
| `pkg/health` | `HealthStrategy` + `LatencyHealthStrategy` | Election rules, membership |
| `pkg/cluster` | Membership table, election math, heartbeats, failover, replication | Byte-level framing, socket details |
| `pkg/telemetry` | The node's CC uplink: status to `TELEMETRY`, `TASK`/`CHAOS` dispatch | Election internals, the dashboard |
| `pkg/controlcenter` | Hub, node server, task routing, HTTP API, WebSocket | Election internals, mesh membership |
| `web` | Embedded dashboard assets | Everything in Go beyond `embed` |
| `cmd/swarm-node` | Wiring, config, lifecycle, signal handling | -- |
| `cmd/control-center` | Wiring, config, signals, `http.Server` | -- |

The dependency direction is strictly downward: `cmd` -> {`controlcenter`, `telemetry`} ->
`cluster` -> {`health`, `network`} -> `protocol`. `pkg/protocol` imports nothing from this repo.
If that ever inverts, the abstraction has failed and the fix is a new interface, not an import
cycle break. The full graph is in [Repository Layout](./repo-layout).

## Deployment

```mermaid
flowchart LR
    DF["Dockerfile: one build stage"] --> IN["image swarm-net/node"]
    DF --> IC["image swarm-net/control-center"]
    IN --> SEED["service seed"]
    IN --> NODE["service node, --scale N"]
    IC --> CCS["service control-center, port 8080"]
    EP["deploy/node-entrypoint.sh"] --> IN
```

`docker compose up --build` starts the CC, `seed`, and 5 replicas of `node`. Each replica derives
a readable ID from Docker DNS. Details: [Running the Swarm](./running-the-swarm) and
[Container Images & PID 1](/concepts/container-images-and-pid-1).

## What is deliberately not here

- **No consensus algorithm.** This is not Raft. Leaders are elected from health scores, and the
  system accepts that two partitions may each elect leaders. The comparison to Raft/Paxos, and an
  honest account of what guarantees we are giving up, is in
  [Why Not Consensus](/architecture/why-not-consensus).
- **No persistence.** State lives in memory and is replicated leader-to-worker. A total swarm
  shutdown loses it. That is a legitimate choice for this system's purpose and an illegitimate one
  for a database; the docs will say which is which.
- **No authentication or encryption on the mesh.** The mesh is trusted because it is a private
  bridge network. Publishing any mesh port to the host would invalidate that assumption, which is
  why `docker-compose.yml` publishes only the Control Center's HTTP port. The dashboard API has
  no authentication either; it relies on the WebSocket same-origin check and on binding to a
  trusted host.

## Where to go next

- [Repository Layout](./repo-layout) -- what lives where, and why the boundaries fall there.
- [Failure Detection and Failover](./failure-detection-and-failover) -- suspicion, heartbeats and
  the failover bound.
- [Replication and Tasks](./replication-and-tasks) -- `STATE_SYNC` and at-least-once task
  delivery.
- [Convergence Debugging](./convergence-debugging) -- two real bugs and what they teach.
- [The Control Center](./control-center) -- how telemetry, tasks and chaos flow.
- [Running the Swarm](./running-the-swarm) -- start, scale, break, and test it.
- [TCP Sockets & The Kernel](/concepts/tcp-sockets-and-the-kernel) -- the substrate everything
  above is built on.
- [Stream Framing](/concepts/stream-framing) -- why "send a message" is not an operation TCP
  offers.
- [Engineering Worklog](/WORKLOG) -- the decisions and the open questions.
