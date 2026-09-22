#!/usr/bin/env python3
"""Build a swarm-scene file from the Control Center's live state.

The scene file is ONE self-describing JSON document that is enough, on its own,
to build a Blender project of the swarm: where every node is, who leads whom,
how far apart they are, how long a message takes to fly between them, and what
traffic is on each link right now.

    swarm (docker compose)
        |  GET /api/state          the dashboard's own feed
        v
    make_swarm_scene.py  ---->  blender/swarm-scene.json
                                        |
                                        v
                                blender/swarm_blender.py   (runs inside Blender)

Usage:

    python3 blender/make_swarm_scene.py                      # from localhost:8080
    python3 blender/make_swarm_scene.py --url http://host:8080 --out scene.json
    python3 blender/make_swarm_scene.py --frames 120 --interval 0.5

With --frames it keeps sampling and records a timeline, so Blender can animate
the swarm moving, re-electing and re-homing instead of showing one still frame.

Why a file and not a live socket: Blender's Python runs on its own thread and a
.blend is a document, not a process. A file (or one HTTP GET of the same JSON)
is replayable, diffable, and can be committed next to a render. The Blender
add-on polls the same JSON, so editing a node in the dashboard moves the node
in Blender within a second.
"""

from __future__ import annotations

import argparse
import json
import math
import signal
import sys
import time
import urllib.request
from datetime import datetime, timezone

SCHEMA = "swarm-scene/1"

# Blender is Z-up and metres-based, and the swarm's own coordinates are an
# abstract 0..100 cube (backend/pkg/geo). One unit becomes METRES_PER_UNIT
# metres, so the default 100-unit cube is a 2km x 2km x 2km block of space:
# large enough that nodes do not intersect at default scale, small enough to
# stay inside Blender's default clip range.
METRES_PER_UNIT = 20.0

# Message colours, matched to the dashboard's legend so a render and the web UI
# tell the same story. RGBA, linear, 0..1.
MESSAGE_COLOURS = {
    "PING": [0.18, 0.55, 0.92, 1.0],
    "PONG": [0.35, 0.72, 0.98, 1.0],
    "HEARTBEAT": [0.15, 0.75, 0.42, 1.0],
    "HEARTBEAT_ACK": [0.45, 0.85, 0.55, 1.0],
    "MEMBERSHIP_DELTA": [0.85, 0.55, 0.15, 1.0],
    "ELECTION_RESULT": [0.90, 0.30, 0.30, 1.0],
    "JOIN_CLUSTER": [0.70, 0.45, 0.90, 1.0],
    "JOIN_ACK": [0.80, 0.60, 0.95, 1.0],
    "STATE_SYNC": [0.25, 0.65, 0.70, 1.0],
    "TASK": [0.95, 0.80, 0.20, 1.0],
    "TASK_RESULT": [0.95, 0.90, 0.55, 1.0],
    "LEAVE": [0.55, 0.55, 0.55, 1.0],
    "SIM_CONFIG": [0.60, 0.60, 0.75, 1.0],
}
OTHER_COLOUR = [0.60, 0.60, 0.60, 1.0]

# One colour per cluster, assigned by leader order so a leader and its workers
# share a colour. Extra clusters cycle.
CLUSTER_COLOURS = [
    [0.20, 0.60, 0.95, 1.0],
    [0.95, 0.45, 0.25, 1.0],
    [0.35, 0.80, 0.45, 1.0],
    [0.80, 0.40, 0.85, 1.0],
    [0.95, 0.75, 0.20, 1.0],
    [0.30, 0.75, 0.80, 1.0],
]

# State colours for the node body, so a still render shows liveness.
STATE_COLOURS = {
    "alive": [0.85, 0.87, 0.90, 1.0],
    "suspect": [0.95, 0.75, 0.25, 1.0],
    "dead": [0.35, 0.35, 0.38, 1.0],
    "killed": [0.25, 0.25, 0.27, 1.0],
    "disconnected": [0.45, 0.45, 0.48, 1.0],
}


def fetch(url: str, timeout: float = 5.0) -> dict:
    """GET url and parse JSON. The Control Center needs no auth by default."""
    with urllib.request.urlopen(url, timeout=timeout) as r:
        return json.loads(r.read().decode("utf-8"))


def to_metres(pos: dict, scale: float) -> list:
    """Swarm units -> Blender metres, Z-up, origin at the cube's floor centre.

    X and Y are centred so the space straddles the world origin (nicer for
    orbiting a camera), while Z is left as a height above the ground plane:
    a node at z=0 sits on the ground, not below it.
    """
    size = 100.0
    return [
        round((float(pos.get("x", 0.0)) - size / 2.0) * scale, 4),
        round((float(pos.get("y", 0.0)) - size / 2.0) * scale, 4),
        round(float(pos.get("z", 0.0)) * scale, 4),
    ]


def distance_units(a: dict, b: dict) -> float:
    return math.dist(
        [float(a.get("x", 0)), float(a.get("y", 0)), float(a.get("z", 0))],
        [float(b.get("x", 0)), float(b.get("y", 0)), float(b.get("z", 0))],
    )


def predicted_ms(d: float, sim: dict) -> float:
    """The emulated one-way delay the backend would apply over distance d.

    Mirrors geo.Delay with the jitter at its mean. Only the responder delays its
    PONG, so a measured round trip is roughly ONE of these plus real network
    time -- which is why `rtt_ms` and `predicted_one_way_ms` are both recorded
    rather than one being derived from the other.
    """
    if not sim.get("enabled", False):
        return 0.0
    ms = float(sim.get("base_ms", 0)) + d * float(sim.get("per_unit_ms", 0)) + float(sim.get("jitter_ms", 0)) / 2.0
    return round(min(ms, float(sim.get("max_delay_ms", 1500))), 3)


def safe_name(node_id: str) -> str:
    """A Blender-safe object name. Blender allows most characters but spaces
    and dots make scripting and drivers awkward."""
    return "Node_" + "".join(c if (c.isalnum() or c in "-_") else "_" for c in node_id)


def build_scene(state: dict, scale: float, timeline: list | None = None) -> dict:
    sim = state.get("sim") or {}
    nodes = [n for n in state.get("nodes") or [] if n.get("id")]
    by_id = {n["id"]: n for n in nodes}

    # Leaders first, so cluster colours are stable between runs.
    leaders = sorted(n["id"] for n in nodes if n.get("role") == "leader" and n.get("connected"))
    colour_of_cluster = {lid: CLUSTER_COLOURS[i % len(CLUSTER_COLOURS)] for i, lid in enumerate(leaders)}

    scene_nodes = []
    for n in sorted(nodes, key=lambda x: x["id"]):
        pos = n.get("pos") or {}
        state_name = n.get("state") or "alive"
        if not n.get("connected") and state_name not in ("killed", "dead"):
            state_name = "disconnected"
        cluster = n.get("leader") or ""
        scene_nodes.append({
            "id": n["id"],
            "object_name": safe_name(n["id"]),
            "role": n.get("role") or "worker",
            "state": state_name,
            "connected": bool(n.get("connected")),
            "leader": cluster,
            "cluster_colour": colour_of_cluster.get(cluster, OTHER_COLOUR),
            "state_colour": STATE_COLOURS.get(state_name, OTHER_COLOUR),
            # Both coordinate systems are kept on purpose: `position_units` is
            # what you POST back to /api/sim to move the node, `location_m` is
            # what Blender puts in object.location.
            "position_units": {
                "x": round(float(pos.get("x", 0.0)), 4),
                "y": round(float(pos.get("y", 0.0)), 4),
                "z": round(float(pos.get("z", 0.0)), 4),
            },
            "location_m": to_metres(pos, scale),
            "telemetry": {
                "term": n.get("term", 0),
                "ledger_size": n.get("ledger_size", 0),
                "dropped": n.get("dropped", 0),
                "degraded": bool(n.get("degraded")),
                "threshold": n.get("threshold", 0),
                "hysteresis": n.get("hysteresis", 0),
                "sim_version": n.get("sim_version", 0),
                "last_seen_ms": n.get("last_seen_ms", 0),
            },
        })

    # Address -> id, so a node's `scores` map (keyed by advertise address) can be
    # turned into per-peer RTT. Every node's peer list carries the mapping.
    addr_to_id = {}
    for n in nodes:
        for p in n.get("peers") or []:
            if p.get("advertise") and p.get("id"):
                addr_to_id[p["advertise"]] = p["id"]

    def rtt_between(src: dict, dst_id: str):
        for addr, sid in addr_to_id.items():
            if sid == dst_id:
                v = (src.get("scores") or {}).get(addr)
                if isinstance(v, (int, float)) and v >= 0:
                    return round(float(v), 3)
        return None

    links = []
    seen_pairs = set()
    for n in nodes:
        src_pos = n.get("pos") or {}
        for peer in n.get("peers") or []:
            pid = peer.get("id")
            if not pid or pid == n["id"] or pid not in by_id:
                continue
            key = tuple(sorted((n["id"], pid)))
            if key in seen_pairs:
                continue
            seen_pairs.add(key)
            d = distance_units(src_pos, by_id[pid].get("pos") or {})
            is_cluster = (n.get("leader") == pid) or (by_id[pid].get("leader") == n["id"])
            links.append({
                "from": n["id"],
                "to": pid,
                # "cluster" links are worker->its leader: the ones worth drawing
                # solid. "peer" links are the rest of the full mesh.
                "kind": "cluster" if is_cluster else "peer",
                "distance_units": round(d, 3),
                "distance_m": round(d * scale, 3),
                "rtt_ms": rtt_between(n, pid),
                "predicted_one_way_ms": predicted_ms(d, sim),
            })

    messages = []
    for n in nodes:
        for f in n.get("flows") or []:
            if not f.get("to") or f["to"] not in by_id:
                continue
            mtype = f.get("type") or "OTHER"
            messages.append({
                "from": n["id"],
                "to": f["to"],
                "type": mtype,
                "count": int(f.get("count") or 0),
                "colour": MESSAGE_COLOURS.get(mtype, OTHER_COLOUR),
                # How long one dot should take to fly the link, from the model.
                "flight_ms": predicted_ms(
                    distance_units(n.get("pos") or {}, by_id[f["to"]].get("pos") or {}), sim
                ),
            })

    scene = {
        "schema": SCHEMA,
        "_doc": {
            "what": "One self-describing snapshot of the swarm, enough to build or update a Blender scene.",
            "coordinates": (
                "The swarm speaks an abstract 0..100 cube (backend/pkg/geo). Blender speaks "
                "metres, Z up. Every node therefore carries BOTH: position_units (what you "
                "POST to /api/sim to move it) and location_m (what goes in object.location). "
                "world.metres_per_unit is the only conversion factor; X and Y are centred on "
                "the origin, Z stays a height above the ground plane."
            ),
            "latency": (
                "The Control Center emulates network latency from distance: a node delays "
                "its PONG by base + distance * per_unit + jitter. Election ranks nodes by "
                "their median measured RTT, so central nodes become leaders, and every "
                "worker joins the leader it measures as closest. links[].rtt_ms is what was "
                "really measured; links[].predicted_one_way_ms is what the model asks for. "
                "A measured round trip is about one one-way delay plus real network time."
            ),
            "editing": (
                "This file is a snapshot, not the source of truth. To MOVE a node, POST its "
                "position_units to the Control Center (see write_back) and the swarm re-groups "
                "for real; the next snapshot then shows it. Editing location_m here only moves "
                "the render."
            ),
            "animation": (
                "timeline is optional. With --frames the generator samples repeatedly and "
                "records one entry per sample, so the Blender add-on can key positions, roles "
                "and cluster colours over time and you can watch a failover as an animation."
            ),
        },
        "meta": {
            "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "generator": "blender/make_swarm_scene.py",
            "source_url": state.get("_source_url", ""),
            "sim_version": sim.get("version", 0),
            "node_count": len(scene_nodes),
            "leader_count": len(leaders),
        },
        "world": {
            "_doc": "Space and unit conversion. size is in swarm units; the cube is size^3.",
            "size_units": sim.get("size", 100),
            "metres_per_unit": scale,
            "size_m": round(float(sim.get("size", 100)) * scale, 3),
            "up_axis": "Z",
            "ground_z_m": 0.0,
            "fps": 24,
        },
        "sim": {
            "_doc": "The live latency model. Change these through /api/sim, never by editing this file.",
            "enabled": sim.get("enabled", False),
            "base_ms": sim.get("base_ms", 0),
            "per_unit_ms": sim.get("per_unit_ms", 0),
            "jitter_ms": sim.get("jitter_ms", 0),
            "max_delay_ms": sim.get("max_delay_ms", 1500),
            "threshold_override": sim.get("threshold", 0),
            "hysteresis_override": sim.get("hysteresis", -1),
            "version": sim.get("version", 0),
        },
        "clusters": [
            {
                "leader": lid,
                "colour": colour_of_cluster[lid],
                "members": sorted(d["id"] for d in scene_nodes if d["leader"] == lid and d["id"] != lid),
            }
            for lid in leaders
        ],
        "nodes": scene_nodes,
        "links": links,
        "messages": messages,
        "tasks": [
            {
                "task_id": t.get("task_id"),
                "kind": t.get("kind"),
                "leader": t.get("leader"),
                "worker": t.get("worker"),
                "state": t.get("state"),
                "duration_ms": t.get("duration_ms"),
            }
            for t in (state.get("tasks") or [])[-25:]
        ],
        "legend": {
            "_doc": "Colour keys, shared with the dashboard so a render matches the web UI.",
            "message_colours": MESSAGE_COLOURS,
            "state_colours": STATE_COLOURS,
            "cluster_colours": CLUSTER_COLOURS,
        },
        "write_back": {
            "_doc": (
                "How to push a change back into the running swarm. The Blender add-on uses "
                "this when you drag a node and press Push; the dashboard's Advanced panel "
                "sends the same request."
            ),
            "endpoint": "POST {base}/api/sim",
            "content_type": "application/json",
            "move_example": {"positions": {"<node id>": {"x": 10.0, "y": 80.0, "z": 35.0}}},
            "model_example": {"base_ms": 1, "per_unit_ms": 3.5, "jitter_ms": 0.5},
            "election_example": {"threshold": 0.5, "hysteresis": 0.5},
            "clear_overrides_example": {"threshold": 0, "hysteresis": -1},
            "kill_example": {"endpoint": "POST {base}/api/chaos", "body": {"node": "<node id>", "action": "kill"}},
            "notes": [
                "Positions are in swarm units (0..100), not metres: divide metres by world.metres_per_unit.",
                "A killed node stays dead; bring it back with `docker compose up -d`.",
            ],
        },
        "blender": {
            "_doc": "Hints for the add-on. Safe to edit: they change the render, not the swarm.",
            "collection": "Swarm",
            "node_mesh": "cone",
            "node_size_m": 12.0,
            "label_nodes": True,
            "draw_links": "cluster",
            "draw_ground_grid": True,
            "animate_messages": True,
            "sync": {
                "mode": "poll",
                "url": (state.get("_source_url", "") or "http://127.0.0.1:8080/api/state"),
                "interval_s": 1.0,
                "_doc": (
                    "The add-on re-fetches this URL on a timer and moves the objects, so a "
                    "slider change in the dashboard shows up in Blender within interval_s."
                ),
            },
        },
    }
    if timeline:
        scene["timeline"] = {
            "_doc": (
                "One entry per sample, oldest first. Each entry is a stripped snapshot: "
                "just what changes. The add-on keys location, cluster colour and role."
            ),
            "interval_s": timeline[0].get("interval_s", 1.0) if timeline else 1.0,
            "frames": timeline,
        }
    return scene


def sample_frame(state: dict, scale: float, t: float, interval: float) -> dict:
    out = {"t": round(t, 3), "interval_s": interval, "nodes": []}
    for n in sorted((state.get("nodes") or []), key=lambda x: x.get("id", "")):
        if not n.get("id"):
            continue
        pos = n.get("pos") or {}
        out["nodes"].append({
            "id": n["id"],
            "location_m": to_metres(pos, scale),
            "role": n.get("role") or "worker",
            "state": n.get("state") or "alive",
            "leader": n.get("leader") or "",
            "connected": bool(n.get("connected")),
        })
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://127.0.0.1:8080", help="dashboard base URL (default: %(default)s)")
    ap.add_argument("--out", default="blender/swarm-scene.json", help="output file, - for stdout")
    ap.add_argument("--scale", type=float, default=METRES_PER_UNIT, help="metres per swarm unit")
    ap.add_argument("--frames", type=int, default=0,
                    help="samples to record as a timeline (0: still snapshot); Ctrl-C stops early and still writes")
    ap.add_argument("--interval", type=float, default=1.0, help="seconds between samples with --frames")
    args = ap.parse_args()

    base = args.url.rstrip("/")
    state_url = base + "/api/state"
    try:
        state = fetch(state_url)
    except Exception as e:  # noqa: BLE001 - a CLI wants the plain reason
        print(f"could not read {state_url}: {e}", file=sys.stderr)
        print("is the stack up? `docker compose up -d`", file=sys.stderr)
        return 1
    state["_source_url"] = state_url

    timeline = []
    if args.frames > 0:
        # Recording is open-ended in practice: you start it, make the swarm do
        # something, then stop it. Ctrl-C or SIGTERM therefore ends sampling and
        # still WRITES the frames gathered so far, instead of losing them.
        def stop(_signum, _frame):
            raise KeyboardInterrupt

        signal.signal(signal.SIGTERM, stop)
        start = time.monotonic()
        timeline.append(sample_frame(state, args.scale, 0.0, args.interval))
        try:
            for _ in range(args.frames - 1):
                time.sleep(args.interval)
                try:
                    s = fetch(state_url)
                except Exception as e:  # noqa: BLE001
                    print(f"sample failed, stopping early: {e}", file=sys.stderr)
                    break
                timeline.append(sample_frame(s, args.scale, time.monotonic() - start, args.interval))
                state = s
        except KeyboardInterrupt:
            print(f"recording stopped after {len(timeline)} frames", file=sys.stderr)
        state["_source_url"] = state_url

    scene = build_scene(state, args.scale, timeline)
    text = json.dumps(scene, indent=2, sort_keys=False) + "\n"
    if args.out == "-":
        sys.stdout.write(text)
    else:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(text)
        print(
            f"wrote {args.out}: {scene['meta']['node_count']} nodes, "
            f"{scene['meta']['leader_count']} leaders, {len(scene['links'])} links"
            + (f", {len(timeline)} frames" if timeline else "")
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
