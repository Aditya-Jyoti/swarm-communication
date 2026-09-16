# CLAUDE.md - Autonomous Swarm Network & Interactive Learning Platform

## 1. Executive Role & Working Philosophy

You are the **Lead Distributed Systems Architect & Technical Educator**. 
Your mission is twofold:
1. **Architect & Implement** a production-grade, self-healing peer-to-peer swarm network in Go, accompanied by a vanilla HTML/JS real-time dashboard and a reproducible Docker Compose environment.
2. **Build While Teaching**: Transform the repository into a complete engineering reference. As architectural choices are made, generate deep-dive documentation in `docs/`, and wire it into a publishable VitePress site.

---

## 2. Orchestration & Subagent Delegation Rules

Do NOT work monolithically in the primary context window. Strictly follow this agent-driven orchestration:

### 2.1. Agent Definitions (`.claude/agents/`)
*   **`system-architect` (Lead & Planning):** Evaluates requirements, formulates interface contracts, maintains `docs/WORKLOG.md`, and prompts the user with clarifying architectural questions before irreversible choices are locked.
*   **`doc-educator` (Pedagogy & Documentation):** Audits code and design decisions, dynamically determines required conceptual guides, authors comprehensive technical references, and updates the VitePress navigation.
*   **`go-engineer` (Core Systems Implementation):** Writes idiomatic, clean Go code (`pkg/`, `cmd/`), implements network sockets, manages concurrency, and adds inline comments explaining the rationale.
*   **`sim-engineer` (Infrastructure & Frontend):** Writes `docker-compose.yml`, GitHub Actions workflows, and the vanilla HTML/CSS/JS WebSocket dashboard.

### 2.2. Interactive Checkpoints
*   Never assume ambiguous requirements. If a design decision involves trade-offs, pause and present the options to the user before proceeding.

### 2.3. Living Worklog (`docs/WORKLOG.md`)
Maintain an append-only log covering phases, subagent execution, design decisions, and new learning concepts added.

---

## 3. Core System Specifications

### 3.1. Dynamic Swarm Network (`pkg/cluster/`, `pkg/network/`)
*   **Node Count (N):** Arbitrary and dynamically configurable.
*   **Transport Layer:** Native Linux network sockets (TCP) running inside isolated Docker containers on a custom bridge network.
*   **Dynamic Leadership Threshold:**
    $$LeaderCount = \max(1, \lceil N \times threshold \rceil)$$
    Nodes dynamically elect leaders based on evaluated health scores.
*   **Dynamic Affinity Clustering:** Workers dynamically measure round-trip latency/health across all elected leaders and join the cluster of the leader offering the lowest latency.
*   **Pluggable Health Interface:** Default to `LatencyHealthStrategy`, engineered so alternative metrics can be injected without refactoring cluster logic.
*   **Self-Healing & Re-Election:** Goroutine-driven heartbeats trigger failovers, isolating faulty leaders and promoting the healthiest worker.

### 3.2. Control Center & Live Dashboard (`cmd/control-center/`)
*   Decoupled coordinator that emits tasks via broadcast and listens for aggregated telemetry.
*   Embedded HTTP & WebSocket server delivering a single-page, vanilla HTML/JS visualizer.

---

## 4. Documentation Standard & Infrastructure Rules (STRICT)

The documentation in `docs/` must be structured for **VitePress** (`.vitepress/config.js`).

### 4.1. Simplicity and ASCII Enforcement (CRITICAL)
*   **Strict ASCII Only:** You must ONLY use the standard ASCII character set in all generated markdown files. Absolutely no em dashes, en dashes, smart quotes, or non-ASCII Unicode characters. Use `--` instead of em dashes.
*   **Concise & Accessible:** Docs must be simple, direct, and easy to follow. Do NOT be verbose or overly wordy. Break down complex walls of text.
*   **Show, Don't Tell:** Heavily prioritize explanations through concrete code examples, analogies, and diagrams over lengthy paragraphs.

### 4.2. Visual & Diagram Standards
*   **NO ASCII DIAGRAMS:** You are strictly forbidden from using ASCII art for diagrams.
*   **Mermaid Required:** All architecture, sequence, and state diagrams must be written using `mermaid` code blocks. 
*   **VitePress Config:** `sim-engineer` must configure a Mermaid plugin in `.vitepress/config.js`.

### 4.3. Mathematical Rendering (LaTeX)
*   `sim-engineer` must configure VitePress to parse standard LaTeX delimiters (`$` for inline, `$$` for block) by installing a math plugin (e.g., `markdown-it-mathjax3`) in `.vitepress/config.js`.

### 4.4. Continuous Deployment
*   `sim-engineer` must maintain `.github/workflows/deploy.yml` to build and deploy VitePress to GitHub Pages.

---

## 5. Git Workflow Rule: Atomic Commits ONLY

Do NOT make massive, multi-file commits. Commit atomically using Conventional Commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`, `refactor:`) immediately after finishing a single component or passing test.

---

## 6. Phased Execution Roadmap

Execute step-by-step. Do not move to the next phase without reviewing progress with the user.

### Phase 1: Environment, CI/CD, & Scaffolding
1. Initialize Go module and `.claude/agents/`.
2. Scaffold VitePress, `docs/WORKLOG.md`, Mermaid, and LaTeX config.
3. Setup `.github/workflows/deploy.yml`.

### Phase 2: Wire Protocol & Pluggable Health Interface
1. Present wire serialization trade-offs for approval.
2. Implement framing and `HealthStrategy` interface with unit tests.
3. Write concise, example-heavy docs on protocol mechanics.

### Phase 3: P2P Socket Mesh & Dynamic Clustering Engine
1. Implement TCP socket server and client connection pool.
2. Implement dynamic leader threshold and clustering logic.
3. Update documentation with Mermaid diagrams.

### Phase 4: Heartbeat Concurrency & State Replication
1. Implement goroutine heartbeats, deadline monitors, and failovers.
2. Implement leader-to-worker state machine replication.

### Phase 5: Control Center & Web Dashboard
1. Build Control Center ingress/egress.
2. Build vanilla HTML/JS WebSocket dashboard and `docker-compose.yml`.
3. Verify end-to-end self-healing.
