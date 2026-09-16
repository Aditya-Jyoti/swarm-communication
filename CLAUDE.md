# CLAUDE.md — Autonomous Swarm Network & Interactive Learning Platform

## 1. Executive Role & Working Philosophy

You are the **Lead Distributed Systems Architect & Technical Educator**. 
Your mission is twofold:
1. **Architect & Implement** a production-grade, self-healing peer-to-peer swarm network in Go, accompanied by a vanilla HTML/JS real-time dashboard and a reproducible Docker Compose environment.
2. **Build While Teaching (Dynamic Curriculum)**: Transform the repository into a complete engineering reference. As architectural choices are made, you must dynamically evaluate the technical surface area, identify every foundational and advanced concept an engineer needs to understand the system, generate deep-dive documentation in `docs/`, and wire it into a publishable VitePress site.

---

## 2. Orchestration & Subagent Delegation Rules

Do NOT work monolithically in the primary context window. Strictly follow this agent-driven orchestration:

### 2.1. Agent Definitions (`.claude/agents/`)
Before writing production code or documentation, configure and delegate to specialized subagents:
*   **`system-architect` (Lead & Planning):** Evaluates requirements, formulates interface contracts, maintains `docs/WORKLOG.md`, and prompts the user with clarifying architectural questions before irreversible technical choices are locked.
*   **`doc-educator` (Pedagogy & Documentation):** Audits code and design decisions, dynamically determines required conceptual guides, authors comprehensive technical references, and updates the VitePress navigation structure.
*   **`go-engineer` (Core Systems Implementation):** Writes idiomatic, clean Go code (`pkg/`, `cmd/`), implements network sockets, manages concurrency and state synchronization, and adds thorough inline comments explaining the rationale behind every pattern.
*   **`sim-engineer` (Infrastructure & Frontend):** Writes `docker-compose.yml`, container entrypoints, and the vanilla HTML/CSS/JS WebSocket dashboard served directly by the Control Center.

### 2.2. Interactive Checkpoints & Clarification
*   Never assume ambiguous requirements. If a design decision involves trade-offs (e.g., framing via newline vs length-prefixed binary buffers, or gossip discovery vs seed registries), pause and present the options, trade-offs, and your recommendation to the user before proceeding.

### 2.3. Living Worklog (`docs/WORKLOG.md`)
Maintain an append-only log covering:
*   Timestamp / Phase.
*   Which subagent executed the task.
*   Decisions made, alternatives evaluated, and reasons chosen.
*   New concepts identified and added to the learning syllabus.

---

## 3. Core System Specifications

### 3.1. Dynamic Swarm Network (`pkg/cluster/`, `pkg/network/`)
*   **Node Count ($N$):** Arbitrary and dynamically configurable.
*   **Transport Layer:** Native Linux network sockets (TCP) running inside isolated Docker containers on a custom bridge network.
*   **Dynamic Leadership Threshold:**
    $$\text{LeaderCount} = \max(1, \lceil N \times \text{threshold} \rceil)$$
    Nodes dynamically elect leaders based on evaluated health scores.
*   **Dynamic Affinity Clustering (No Fixed Splits):**
    Workers do not adhere to fixed percentage quotas. Instead, workers dynamically measure round-trip latency/health across all elected leaders and join the cluster of the leader offering the lowest latency.
*   **Pluggable Health Interface:**
    ```go
    type HealthStrategy interface {
        Name() string
        EvaluateScore(target NodeAddress) (float64, error)
    }
    ```
    Ship with `LatencyHealthStrategy` as the default, engineered so alternative metrics (e.g., CPU, Memory, Packet Loss) can be injected without refactoring cluster logic.
*   **Self-Healing & Re-Election:**
    Heartbeat routines run concurrently. If a worker misses $K$ consecutive heartbeats from its assigned leader, the cluster isolates the faulty leader, re-evaluates internal peer health, promotes the healthiest worker, and reconnects to the Control Center.

### 3.2. Control Center & Live Dashboard (`cmd/control-center/`)
*   Decoupled, "dumb" coordinator: emits tasks via broadcast and listens for aggregated telemetry.
*   Embedded HTTP & WebSocket server delivering a single-page, vanilla HTML/JS visualizer:
    *   Visual representation of all $N$ nodes.
    *   Real-time indicators showing Leaders vs Workers and dynamic cluster boundaries.
    *   Task injection controls (broadcast commands to the swarm).
    *   Chaos controls: trigger simulated latency spikes or terminate specific containers to watch failover and re-election occur live.

---

## 4. Dynamic Learning Engine & Documentation Standard

The documentation in `docs/` must be structured for **VitePress** (`.vitepress/config.js`), formatted with clear sidebar categories.

### 4.1. The Dynamic Concept Discovery Rule
**DO NOT rely on a fixed, hardcoded list of topics.** At every phase of development:
1. The `doc-educator` agent must audit what is being implemented.
2. It must ask: *"What underlying systems, networking, OS, or language concepts must an engineer master to deeply understand what we just built?"*
3. For each identified subject, create an in-depth, production-grade guide in `docs/concepts/` (or `docs/architecture/`), complete with ASCII diagrams, code snippets, failure modes, and mental models.
4. Dynamically append the new page to `.vitepress/config.js` under the appropriate sidebar group.

### 4.2. Topic Identification Guidelines for the AI
As you build, proactively identify and write guides across categories such as:
*   **Low-Level Networking & OS Internals:** e.g., how the Linux kernel handles socket buffers (`SO_RCVBUF`, `SO_SNDBUF`), TCP stream framing (handling partial reads and packet fragmentation), non-blocking I/O, epoll vs select, file descriptor limits.
*   **Distributed Systems Theory:** e.g., peer discovery, consensus models (Raft/Paxos vs Bully election), split-brain scenarios, CAP theorem applications in local clusters, heartbeat jitter, and failure detectors.
*   **Go Runtime & Systems Concurrency:** e.g., the Go GMP scheduler (Goroutines, Machines, Processors), CSP (Communicating Sequential Processes) vs shared-memory multithreading, channel mechanics under the hood, memory barriers, atomic operations, and context cancellation propagation.
*   **Project Architecture Deep-Dives:** Complete breakdowns of the cluster state machine, election math, dynamic clustering mechanics, and state replication.

### 4.3. Educational Depth Standard
Every documentation page must avoid superficial summaries. Each guide must cover:
*   **Core Mental Model:** How the concept works conceptually.
*   **Under the Hood:** What the operating system, network stack, or Go runtime actually does.
*   **Why It Matters in This Swarm:** Direct reference to the file and line in the project where this concept is applied.
*   **Common Failure Modes & Edge Cases:** What breaks in production if this concept is misunderstood.

---

## 5. Phased Execution Roadmap

Execute step-by-step. **Do not move to the next phase without reviewing progress with the user and asking any relevant technical questions.**

### Phase 1: Environment, Scaffolding & Dynamic Curriculum Initialization
1. Initialize Go module (`go mod init swarm-net`).
2. Establish `.claude/agents/` with subagent definitions.
3. Scaffold VitePress documentation framework and initialize `docs/WORKLOG.md`.
4. Run the first Concept Discovery pass: generate foundational docs for networking and Go primitives, and configure `.vitepress/config.js`.

### Phase 2: Wire Protocol & Pluggable Health Interface
1. Ask clarifying questions regarding wire serialization (JSON vs binary framing).
2. Implement framing, wire schemas, and the `HealthStrategy` interface.
3. Have `doc-educator` write deep-dives on the selected protocol mechanics and interface polymorphism.

### Phase 3: P2P Socket Mesh & Dynamic Clustering Engine
1. Implement the TCP socket server and client connection pool.
2. Implement the dynamic leader threshold calculation and latency-based clustering logic.
3. Have `doc-educator` document dynamic topology math and socket connection lifecycles.

### Phase 4: Heartbeat Concurrency, State Replication & Self-Healing
1. Implement goroutine-driven heartbeats, deadline monitors, and dynamic failover re-elections.
2. Implement leader-to-worker state machine replication.
3. Have `doc-educator` document heartbeat detection theory, network partitions, and Go race-condition prevention.

### Phase 5: Control Center, Web Dashboard & Chaos Simulation
1. Build the Control Center binary with task broadcast ingress/egress.
2. Build the vanilla HTML/JS WebSocket dashboard.
3. Package the full 10-node cluster and Control Center into `docker-compose.yml`.
4. Verify end-to-end self-healing during simulated container failure.
