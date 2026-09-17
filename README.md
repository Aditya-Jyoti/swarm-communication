# swarm-net

A self-healing peer-to-peer swarm in Go, with a live browser dashboard.

Identical nodes run in Docker containers and talk raw TCP. They score each other by latency,
elect $\max(1, \lceil N \times threshold \rceil)$ leaders, and each worker joins the fastest
leader. When a leader dies, the swarm promotes the healthiest worker and re-issues pending tasks.
A Control Center sends tasks and collects telemetry. The dashboard shows it all live and has
chaos buttons to break things on purpose.

**Docs:** <https://aditya-jyoti.github.io/swarm-communication/>

## Layout

| Path | What it holds |
|---|---|
| `backend/` | Go module: `cmd/` (swarm-node, control-center), `pkg/`, `Dockerfile`, `deploy/` |
| `frontend/` | Vanilla HTML/JS dashboard, `nginx.conf.template`, `Dockerfile` |
| `docs/` | The VitePress site, including the [Worklog](docs/WORKLOG.md) |
| `docker-compose.yml` | The local stack |
| `.env.example` | Every tunable, commented |
| `scripts/` | `e2e.sh` and the docs checkers |

The frontend container (nginx) serves the dashboard and proxies `/api/`, `/healthz` and `/ws`
to the Control Center. The Control Center itself is not published.

## Quick start

```bash
cp .env.example .env
docker compose up --build
```

Open <http://127.0.0.1:8080>. The default stack is a Control Center, a `seed` node, 5 more
nodes (N = 6, so 2 leaders) and the frontend.

## Scaling

```bash
docker compose up -d --scale node=11     # N = 12, 4 leaders
```

Or set `NODE_REPLICAS` in `.env`. No code or YAML edits.

## Drone simulation

Each node is a drone at a 3D position in a 100-unit cube. The Control Center turns distance
into emulated latency on PONG replies:

$$delay = base + distance \times perUnit + jitter \times U[0, 1)$$

Central drones get the lowest median RTT and become leaders. Each worker joins its nearest
leader. The dashboard draws the airspace in 3D (drag to orbit, wheel to zoom), labels links
with distance and RTT, and animates messages between drones.

- **Advanced panel** (collapsed by default): sliders for the latency model, `threshold` and
  `hysteresis`, an enable switch, randomize/reset positions, and x/y/z sliders per drone.
  A button clears the threshold and hysteresis overrides. The same settings are available at
  `GET` and `POST /api/sim`.
- **Killed drones stay down.** A chaos kill exits 0, and nodes use `restart: on-failure`. To
  bring them back, run `docker compose up -d`.

| Variable | Default | Meaning |
|---|---|---|
| `SWARM_SIM_ENABLED` | `true` | Emulated latency on or off |
| `SWARM_SIM_BASE_MS` | `1` | Fixed delay per PONG (0..500) |
| `SWARM_SIM_PER_UNIT_MS` | `2` | Delay per unit of distance (0..10) |
| `SWARM_SIM_JITTER_MS` | `0.5` | Maximum random extra delay (0..200) |

These are start-up values for the CC. The panel changes them at run time. Details:
[Drone Simulation](docs/architecture/drone-simulation.md).

## Blender

```bash
python3 blender/make_swarm_scene.py                        # live swarm -> blender/swarm-scene.json
blender --python blender/swarm_blender.py -- --scene blender/swarm-scene.json
```

`blender/swarm-scene.json` is one self-describing JSON document that holds the whole swarm:
every drone in both unit systems, the clusters, each link with its measured RTT and the
model's predicted one-way delay, the traffic on it, the dashboard's colour legend, and the
exact requests that change the running swarm. That single file is enough to build the scene.

Editing works from both sides. A slider in the dashboard's Advanced panel really moves the
drone, and Blender follows on the next sync. Moving a cone in Blender and pressing **Push
positions** POSTs to the same `/api/sim`, so the swarm re-measures and re-groups for real.
Positions in swarm units are the truth; metres are a rendering of them.

The add-on also live-syncs straight from `/api/state` and can chaos-kill the selected drone.
Details, options and limits: [`blender/README.md`](blender/README.md).

## Running without Docker

```bash
cd backend
go run ./cmd/control-center -listen 127.0.0.1:7000 -http 127.0.0.1:8080
go run ./cmd/swarm-node -node-id n1 -listen 127.0.0.1:7001 \
  -advertise 127.0.0.1:7001 -control-center 127.0.0.1:7000
go run ./cmd/swarm-node -node-id n2 -listen 127.0.0.1:7002 \
  -advertise 127.0.0.1:7002 -seeds 127.0.0.1:7001 -control-center 127.0.0.1:7000
```

One terminal each. Without the frontend there is no dashboard; use the API with `curl`. Every
flag also has a `SWARM_*` env var (flags win). See
[Running the Swarm](docs/architecture/running-the-swarm.md).

## Tests

```bash
cd backend && go test -race ./...    # unit tests (Go 1.27+)
scripts/e2e.sh                       # full stack: kill a leader, check it heals
npm ci && npm run docs:check         # docs: ASCII, Mermaid, build (Node 22)
```

`scripts/e2e.sh` goes through the frontend port (`E2E_HTTP_PORT`, default 18080). It needs
Docker, `curl`, and `jq` or `python3`.

## Configuration

Compose reads `.env`. Every variable has a default, so `.env` is optional. The full commented
list is in [`.env.example`](.env.example).

| Variable | Default | Meaning |
|---|---|---|
| `BIND_ADDR` | `127.0.0.1` | Host address for the published port. Keep it local unless the network is trusted. |
| `SWARM_CC_API_TOKEN` | empty | Optional API token (`openssl rand -hex 32`). nginx injects it, so the dashboard keeps working. |
| `FRONTEND_PORT` | `8080` | Host port for the dashboard |
| `CC_HTTP_PORT` | `18081` | Host port for the CC, only if its `ports:` block is uncommented |
| `NODE_REPLICAS` | `5` | Nodes besides `seed` |
| `SWARM_THRESHOLD` | `0.3` | Leader fraction in (0, 1] |
| `SWARM_PROBE_INTERVAL` | `1s` | How often peers are scored |
| `SWARM_IDLE_TIMEOUT` | `5s` | Silence before a link is dead (more than 3x the probe interval) |
| `SWARM_GOSSIP_INTERVAL` | `2s` | How often a full membership view is sent |
| `SWARM_ELECTION_FLOOR` | `30s` | Periodic re-election |
| `SWARM_TELEMETRY_INTERVAL` | `1s` | How often nodes report to the CC |
| `SWARM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SWARM_IMAGE_TAG` | `dev` | Tag for the three images |
| `SWARM_VERSION` | `dev` | Version baked into the binaries |
| `GO_VERSION`, `ALPINE_VERSION`, `NGINX_VERSION` | `1.27`, `3.22`, `1.31-alpine` | Base images |

Known open items are in [`STATE.md`](STATE.md).
