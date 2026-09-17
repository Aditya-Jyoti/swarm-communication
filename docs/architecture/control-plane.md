---
title: Control Plane Contract
---

# Control Plane Contract

The fixed interface between the swarm nodes, the Control Center (CC), and the
browser dashboard. Phase 4 and Phase 5 are built in parallel against this page.

```mermaid
flowchart LR
    B[Browser dashboard] -- "WebSocket /ws (JSON)" --> CC[control-center]
    CC -- "TCP framed: TASK, CHAOS" --> L[Leader nodes]
    L -- "TCP framed: TELEMETRY, TASK_RESULT" --> CC
    W[Worker nodes] -- "TELEMETRY" --> CC
    L -- "mesh: TASK, HEARTBEAT, STATE_SYNC" --> W
    W -- "mesh: TASK_RESULT, HEARTBEAT_ACK" --> L
```

## 1. Node to Control Center (TCP)

- Every node dials `SWARM_CONTROL_CENTER` (host:port) and completes the normal
  `HELLO` handshake from `pkg/network`. The CC's `NodeID` is `control-center`.
- The CC is NOT a mesh member. Its connections never feed node membership, and the
  CC never advertises node addresses to nodes.
- Node -> CC: `TELEMETRY` every `SWARM_TELEMETRY_INTERVAL` (default 1s), and
  `TASK_RESULT` whenever a task led by that node completes
  (`NodeConfig.OnTaskResult`).
- CC -> node: `TASK` (only to leaders) and `CHAOS` (to any node).
- CC -> node: `PING` every 5s on every connected link; the node answers `PONG`.
  Without it the link is one-way and the node's read deadline (`SWARM_IDLE_TIMEOUT`)
  would reap it. The keep-alive period MUST stay well under the node's idle timeout.
- The CC's `HELLO_ACK` carries no `Advertise` and an empty `KnownPeers`.
- A node announces **incarnation 0** on its CC link. If `SWARM_CONTROL_CENTER`
  wrongly points at a mesh node, that node's pool rejects the older incarnation and
  the real mesh link survives. If a handshake completes with any peer ID other than
  `control-center`, the node stops its uplink (`ErrNotControlCenter`) and keeps
  running without one. The CC ignores the incarnation: the newest link per node wins.
- A lost CC link is redialled with backoff. Election and heartbeats never depend on it.

Node-side handling:

| Frame | Action |
|---|---|
| `TASK` | `Node.SubmitTask(ctx, payload)` |
| `CHAOS kill` | log, then `os.Exit(1)` (Compose restarts it as a new incarnation) |
| `CHAOS delay` | `MeshProber.SetDelay(d)` and `Node.SetChaosDelay(d)`; `0 <= delay_ms <= 5000` |
| `CHAOS clear` | both delays set to 0 |
| `PING` | reply `PONG` with the same nonce and seq, never delayed by chaos |

## 2. Tasks

Built-in task kinds (`NodeConfig.Executor` default):

| kind | body | output |
|---|---|---|
| `echo` | any JSON | the body, verbatim |
| `sleep` | `{"ms": 200}` | `"slept 200ms"` (capped at 10000) |
| `hash` | `{"data": "abc"}` | hex SHA-256 of `data` |

Unknown kind: `ok=false`, output `"unknown task kind: X"`.

The CC picks a leader round-robin from the latest telemetry (`role == "leader"`).
Delivery is at-least-once: a task pending at failover is re-issued by the promoted
leader, so a result for the same `task_id` may arrive twice. The CC keeps the first.

On the mesh (not the CC link), each `STATE_SYNC` ledger record also carries the task
itself while it is pending, so a promoted worker can re-issue it
(`pkg/protocol/message.go:468`):

```json
{"task_id": "t-1", "assigned_to": "node-2", "state": "pending",
 "kind": "sleep", "body": {"ms": 100}}
```

`kind` and `body` are omitted when empty. A body over 1 KiB is not replicated, so that
task cannot be re-issued after a failover. The body is dropped once the task completes.
See [Replication and Tasks](/architecture/replication-and-tasks).

## 3. HTTP and WebSocket (CC, default `:8080`)

| Path | Purpose |
|---|---|
| `GET /` | dashboard (embedded from `web/static/`) |
| `GET /ws` | WebSocket, JSON text messages |
| `GET /api/state` | the latest `snapshot` message as JSON (used by scripts) |
| `POST /api/tasks` | body = a `task` client message; returns `{"task_ids":[...]}` |
| `POST /api/chaos` | body = a `chaos` client message; returns `{"ok":true}` |
| `GET /healthz` | `ok` |

The `type` field is optional in POST bodies (the path implies it) but must match
if present. Bodies are capped at 64 KiB. Every non-2xx response is JSON:

```json
{"error": "bad request: task kind is required"}
```

| Status | When |
|---|---|
| 400 | malformed JSON, wrong `type`, missing `kind` or `node`, negative `count`, invalid chaos action or `delay_ms` |
| 404 | chaos target is not a connected node |
| 503 | no connected leader for a task, or the CC is shutting down |

WebSocket requests that fail validation are logged and dropped; there is no error
message to the browser.

Server to browser, sent once on connect and then every 1s:

```json
{
  "type": "snapshot",
  "at_unix_ms": 1790000000000,
  "nodes": [
    {
      "id": "node-1", "role": "leader", "state": "alive", "term": 3,
      "leader": "node-1", "degraded": false, "connected": true,
      "last_seen_ms": 120, "dropped": 0, "ledger_size": 4,
      "peers": [{"id": "node-2", "advertise": "node-2:7946", "incarnation": 5,
                 "role": "worker", "state": "alive", "score": 0.4, "seq": 12}],
      "scores": {"node-2:7946": 0.4}
    }
  ],
  "tasks": [
    {"task_id": "t-1", "kind": "echo", "leader": "node-1", "worker": "node-2",
     "state": "done", "ok": true, "output": "...", "duration_ms": 0.3,
     "submitted_unix_ms": 1790000000000}
  ]
}
```

- `nodes` is sorted by `id`. A node whose CC link dropped stays listed with
  `connected: false` for 30s, then disappears.
- `tasks` holds the newest 200 tasks, newest last. `state` is
  `pending` | `done` | `failed`.
- `peers` entries are `MemberRecord` exactly as on the wire. `score: -1` means
  **unmeasured** (`protocol.UnmeasuredScore`), never "excellent". Any negative
  score must be treated the same way.
- `scores` are the reporting node's own local measurements, keyed by peer
  address. NaN and Inf values are omitted.
- `peers` entries carry `seq`, the member's own claim counter (`pkg/protocol/message.go:367`).
  It orders `role` and `score` within one `incarnation`, and a higher `seq` is the newer claim.
  It is omitted when 0 (a build without it). Two nodes showing different `score` for a member
  at the same `seq` is a bug; at different `seq` it is propagation in progress.
- `peers` and `scores` are always `[]` / `{}`, never `null`.

Server to browser, sent as they happen:

```json
{"type": "event", "at_unix_ms": 1790000000000, "kind": "leader_change",
 "node": "node-3", "detail": "worker -> leader"}
```

`kind` is one of `node_up`, `node_down`, `leader_change`, `state_change`,
`task_done`, `chaos`.

Browser to server:

```json
{"type": "task", "kind": "sleep", "body": {"ms": 100}, "count": 5}
{"type": "chaos", "node": "node-2", "action": "delay", "delay_ms": 300}
```

`count` defaults to 1 and is capped at 100.
