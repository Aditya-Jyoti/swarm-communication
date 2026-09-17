# Blender: simulate the swarm in 3D

One JSON document -- `swarm-scene.json` -- describes the whole swarm: where every
drone is, who leads whom, how far apart they are, what each link costs in
milliseconds, and what traffic is on it. Blender builds a scene from that file,
and pushes your edits back into the running swarm.

```
docker compose stack                     Blender
  |                                         ^
  |  GET /api/state                         |  swarm_blender.py (add-on)
  v                                         |
make_swarm_scene.py  --->  swarm-scene.json +
  ^                                         |
  |  POST /api/sim  <-----------------------+  "Push positions"
  |
dashboard sliders (same endpoint)
```

There is **one** implementation of the scene format (`make_swarm_scene.py`), and
the add-on imports it. Nothing has to be kept in step by hand.

## Files

| File | What it is |
|---|---|
| `make_swarm_scene.py` | Generator: live swarm -> scene file. Plain Python 3, no dependencies. |
| `swarm-scene.json` | A real scene, generated from a running 6-drone swarm. Self-describing: every section carries a `_doc`. |
| `swarm_blender.py` | The Blender add-on: builds the scene, live-syncs, pushes edits back. |
| `test_swarm_blender.py` | Tests for everything that does not need Blender (26 of them). |

## Quick start

```bash
docker compose up -d --build                 # the swarm must be running
python3 blender/make_swarm_scene.py          # -> blender/swarm-scene.json
```

Then in Blender (4.x):

1. `Edit > Preferences > Add-ons > Install...`, pick `blender/swarm_blender.py`,
   and tick **Swarm Net**.
2. In the 3D viewport press `N` and open the **Swarm** tab.
3. Set **Source** to the scene file, or straight to `http://127.0.0.1:8080/api/state`.
4. **Load / update** builds it.
5. **Start live sync** follows the swarm about once a second.

Headless, no install:

```bash
blender --python blender/swarm_blender.py -- --scene blender/swarm-scene.json
blender -b --python blender/swarm_blender.py -- --scene blender/swarm-scene.json --render /tmp/swarm.png
```

## Editing works both ways

| You do this | What happens |
|---|---|
| Move a slider in the dashboard's Advanced panel | The swarm really moves the drone, re-measures latency and re-groups. Blender follows on the next sync. |
| Move a cone in Blender, press **Push positions** | The add-on POSTs to `/api/sim`. Same effect: the swarm re-groups for real. |
| Press **Kill selected drone** | CHAOS kill. It stays dead; `docker compose up -d` brings it back. |
| Edit `location_m` in the JSON by hand | Only the render moves. The swarm never sees it. |

The rule: **positions in swarm units are the truth**, metres are a rendering of
them. `position_units` is what the API speaks, `location_m` is what Blender
speaks, and `world.metres_per_unit` is the only conversion.

## Animation

```bash
python3 blender/make_swarm_scene.py --frames 120 --interval 0.5   # 60s of swarm
```

This records a `timeline`, so you can key positions, roles and cluster colours
over time. Kill a leader while it samples and you capture the failover.

## What the scene contains

| Section | Why it is there |
|---|---|
| `world` | Cube size and `metres_per_unit`. A 100-unit cube at 20 m/unit is 2 km across. |
| `sim` | The live latency model: `base + distance * per_unit + jitter`. |
| `drones` | Position in both unit systems, role, state, cluster colour, and telemetry. |
| `clusters` | Leader plus members, with the colour they share. |
| `links` | Distance in units and metres, the **measured** RTT, and the model's predicted one-way delay. |
| `messages` | Per-link traffic by type, with a colour and a flight time. |
| `legend` | The same colours the dashboard uses, so a render and the web UI agree. |
| `write_back` | The exact requests that change the running swarm. |
| `blender` | Rendering hints (mesh, sizes, what to draw). Safe to edit. |

**Measured versus predicted.** Only the responder delays its PONG, so a measured
round trip is about one one-way delay plus real network time. In the committed
scene, a link with a predicted 151.06 ms one-way delay measured 152.0 ms.

## Scale

`--scale` sets metres per unit (default 20). The whole airspace is
`world.size_m` across:

| `--scale` | Airspace | Good for |
|---|---|---|
| 1 | 100 m | A tabletop-sized swarm |
| 20 (default) | 2 km | Realistic drone spacing, default Blender clipping |
| 100 | 10 km | Wide-area, needs the camera clip end raised |

## Tests

```bash
python3 blender/test_swarm_blender.py
```

These cover the pure half: coordinate conversion both ways, distance and
predicted latency, colour and material choice, the plan, and the write-back
payload. The `bpy` half needs Blender, so it is verified by opening it.

## Notes and limits

- The generator only needs the dashboard port; it does not touch Docker.
- The add-on polls; it never holds a socket open. A missed poll just means one
  stale second.
- `blender/swarm-scene.json` is committed as a worked example, so the format can
  be read without running anything. Regenerate it whenever you want current data.
- Live sync updates objects rather than rebuilding them, so your selection,
  parenting and extra materials survive.
- A drone without a position falls back to a hash of its ID, matching the
  backend's placement.
