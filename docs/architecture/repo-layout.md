---
title: Repository Layout
outline: deep
---

# Repository Layout

```
swarm-net/
├── CLAUDE.md                     # the operating charter for the agent team
├── go.mod                        # module swarm-net
├── package.json                  # VitePress toolchain only — the Go build needs no Node
│
├── .claude/
│   └── agents/                   # subagent definitions (see below)
│       ├── system-architect.md
│       ├── doc-educator.md
│       ├── go-engineer.md
│       └── sim-engineer.md
│
├── cmd/
│   ├── swarm-node/               # the node binary — identical for every node in the swarm
│   └── control-center/           # coordinator + embedded HTTP/WebSocket server
│
├── pkg/
│   ├── protocol/                 # wire schemas + framing codec. Imports nothing local.
│   ├── network/                  # TCP listener, dialer, connection pool, read/write loops
│   ├── health/                   # HealthStrategy interface + LatencyHealthStrategy
│   ├── cluster/                  # membership, election math, heartbeats, failover, replication
│   └── telemetry/                # node state snapshots destined for the dashboard
│
├── web/
│   └── static/                   # vanilla HTML/CSS/JS dashboard, embedded into the CC binary
│
├── deploy/                       # Dockerfiles, docker-compose.yml, bridge network definition
│
└── docs/                         # VitePress site — the curriculum
    ├── .vitepress/config.js      # sidebar; extended after every Concept Discovery pass
    ├── index.md
    ├── WORKLOG.md
    ├── architecture/             # how THIS system works
    ├── concepts/                 # transferable foundations it stands on
    └── guides/                   # operational how-tos (running, scaling, chaos)
```

## Why `pkg/` splits this way

The temptation in a project this size is a single `swarm` package. It is rejected for one reason:
**the seams are where the teaching happens**. A reader who wants to understand framing should be
able to read `pkg/protocol` without election logic in their way, and a reader who wants election
should not have to skip past byte-slice arithmetic.

The boundaries are drawn so each package has exactly one reason to change:

- **`protocol` changes** when the wire format changes. Nothing else should have to.
- **`network` changes** when connection management changes — pooling, deadlines, backpressure. It
  moves opaque frames and has no opinion about their contents.
- **`health` changes** when a new metric is introduced. This is the extension point the brief calls
  out explicitly, so it gets its own package rather than a file inside `cluster`. A strategy living
  next to the code that consumes it is a strategy that will accidentally grow a dependency on it.
- **`cluster` changes** when the distributed algorithm changes — thresholds, affinity rules,
  failover behaviour. This is where the genuinely hard reasoning lives, and it is kept free of I/O
  detail so it can be unit-tested without a socket.
- **`telemetry` changes** when the dashboard needs to show something new. Keeping it separate stops
  display concerns from leaking into the state machine.

The dependency graph is acyclic and points one way:

```
   cmd/swarm-node ─┐
   cmd/control-center ─┤
                       ├──▶ pkg/cluster ──┬──▶ pkg/health ──┐
                       │                  │                 ├──▶ pkg/protocol
                       └──▶ pkg/telemetry └──▶ pkg/network ─┘
```

`pkg/protocol` is the root and imports nothing from this module. If a future change appears to
require `protocol` to import `cluster`, the correct response is to hoist the shared type into
`protocol` or to introduce an interface — never to merge the packages.

## `cmd/` holds wiring, not logic

Both binaries should read as configuration, construction, and lifecycle: parse flags and
environment, build the dependency graph, install signal handlers, block, shut down cleanly. Any
`if` statement in `cmd/` that expresses a distributed-systems rule belongs in `pkg/`. This keeps the
interesting code testable without spawning processes.

`cmd/swarm-node` is deliberately one binary for all roles. A node does not know at start-up whether
it will be a leader; roles are states in a machine, not build targets. Two binaries would make the
promotion path untestable and would invite the two code paths to drift.

## `web/static` is embedded, not mounted

The dashboard is served by the Control Center from `embed.FS`, not from a bind mount. One binary,
no volume wiring, no path that works in development and breaks in the container. The cost is a
rebuild to see a CSS change, which is the right trade for a demonstration system that must come up
identically on someone else's machine.

## `docs/` mirrors the two kinds of knowledge

`architecture/` answers *"how does this system work?"* — it is allowed to be specific, opinionated,
and obsolete the moment the code changes, which is why it cites code directly.

`concepts/` answers *"what must I understand for that to make sense?"* — it is transferable. A
reader should be able to take the framing or netpoller page to an unrelated project and still get
value. These pages reference this repository, but they are not *about* it.

`guides/` is operational: how to run it, how to scale $N$, how to break it on purpose.

## The agent definitions in `.claude/agents/`

Four roles, so that no single context window carries the whole system:

| Agent | Owns | Never does |
|---|---|---|
| `system-architect` | Interface contracts, trade-off analysis, the worklog, escalating questions to the user | Write production Go |
| `doc-educator` | Concept discovery, the curriculum, sidebar wiring | Make architectural decisions |
| `go-engineer` | Everything in `pkg/` and `cmd/` | Invent contracts unilaterally |
| `sim-engineer` | `deploy/` and `web/static/` | Touch cluster logic |

The separation is not ceremony. The architect asking "what breaks if a leader is slow rather than
dead?" and the engineer asking "what does this `Read` return on a half-open socket?" are different
modes of attention, and interleaving them in one pass produces worse answers to both.
