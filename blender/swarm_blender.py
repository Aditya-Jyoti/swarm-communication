"""Blender add-on: build and live-sync a swarm scene.

Install (Blender 4.x):

    Edit > Preferences > Add-ons > Install... > pick this file > tick "Swarm Net"
    Press N in the 3D viewport, open the "Swarm" tab.

Or run it headless, without installing:

    blender --python blender/swarm_blender.py
    blender -b --python blender/swarm_blender.py -- --scene blender/swarm-scene.json --render out.png

What it does:

    blender/swarm-scene.json  (or GET /api/state)
        |
        |  build_plan()          pure: JSON -> a list of objects, links, colours
        v
    apply_plan()                 the only part that touches bpy
        |
        v
    Collection "Swarm": one cone per drone, a link curve per cluster link,
    a ground grid, an emissive dot per animated message.

Live sync: a timer re-fetches the same URL the dashboard uses, so moving a
drone with the dashboard's Advanced sliders moves the cone in Blender within a
second. Push works the other way too: move a cone in Blender, press "Push
positions", and the add-on POSTs to /api/sim, the swarm re-measures its
latencies and re-groups for real.

Design note: everything above `--- Blender layer ---` is plain Python with no
bpy import, so it can be unit-tested outside Blender (see
blender/test_swarm_blender.py). Only the layer below talks to Blender.
"""

from __future__ import annotations

import json
import math
import os
import sys
import urllib.error
import urllib.request

bl_info = {
    "name": "Swarm Net",
    "author": "swarm-net",
    "version": (1, 0, 0),
    "blender": (4, 0, 0),
    "location": "View3D > Sidebar (N) > Swarm",
    "description": "Build and live-sync a swarm-net drone scene from a scene file or a running Control Center.",
    "category": "Import-Export",
}

SCHEMA_PREFIX = "swarm-scene/"
DEFAULT_SCENE = "blender/swarm-scene.json"
DEFAULT_URL = "http://127.0.0.1:8080"

# Custom properties written onto each object, so a re-sync can find the drone
# an object belongs to without relying on the name (the user may rename it).
PROP_ID = "swarm_id"
PROP_KIND = "swarm_kind"  # "drone" | "link" | "ground" | "message"


# --------------------------------------------------------------------------
# Pure core: JSON in, a plan out. No bpy.
# --------------------------------------------------------------------------

def load_scene(path_or_url: str, timeout: float = 5.0) -> dict:
    """Read a scene file, or fetch live state and convert it.

    Accepts three things on purpose, because each is convenient somewhere:
      - a path to a scene file  (reproducible, diffable, commit next to a render)
      - .../api/scene           (the Control Center serves the same document)
      - .../api/state           (the dashboard's raw feed, converted here)
    """
    if path_or_url.startswith(("http://", "https://")):
        with urllib.request.urlopen(path_or_url, timeout=timeout) as r:
            doc = json.loads(r.read().decode("utf-8"))
    else:
        with open(path_or_url, "r", encoding="utf-8") as f:
            doc = json.load(f)
    if str(doc.get("schema", "")).startswith(SCHEMA_PREFIX):
        return doc
    if "nodes" in doc:  # raw /api/state
        return scene_from_state(doc, source_url=path_or_url)
    raise ValueError("not a swarm scene file and not /api/state output")


def scene_from_state(state: dict, source_url: str = "") -> dict:
    """Convert raw /api/state into a scene document.

    Imports the generator when it sits next to this file, so the add-on and the
    CLI can never drift into two different conversions. Falls back to a minimal
    inline conversion when the generator is not on disk (for example when only
    this one file was installed as an add-on).
    """
    here = os.path.dirname(os.path.abspath(__file__))
    if here not in sys.path:
        sys.path.insert(0, here)
    try:
        import make_swarm_scene as gen  # type: ignore

        state = dict(state)
        state["_source_url"] = source_url
        return gen.build_scene(state, gen.METRES_PER_UNIT, None)
    except Exception:  # noqa: BLE001 - any import/shape problem falls back
        return _minimal_scene(state, source_url)


def _minimal_scene(state: dict, source_url: str) -> dict:
    sim = state.get("sim") or {}
    scale = 20.0
    size = float(sim.get("size", 100) or 100)
    drones = []
    for n in state.get("nodes") or []:
        if not n.get("id"):
            continue
        p = n.get("pos") or {}
        drones.append({
            "id": n["id"],
            "object_name": "Drone_" + str(n["id"]).replace(".", "_"),
            "role": n.get("role", "worker"),
            "state": n.get("state", "alive"),
            "connected": bool(n.get("connected")),
            "leader": n.get("leader", ""),
            "cluster_colour": [0.6, 0.6, 0.6, 1.0],
            "state_colour": [0.85, 0.87, 0.9, 1.0],
            "position_units": {"x": p.get("x", 0), "y": p.get("y", 0), "z": p.get("z", 0)},
            "location_m": [
                (float(p.get("x", 0)) - size / 2) * scale,
                (float(p.get("y", 0)) - size / 2) * scale,
                float(p.get("z", 0)) * scale,
            ],
            "telemetry": {},
        })
    return {
        "schema": SCHEMA_PREFIX + "1",
        "meta": {"source_url": source_url, "node_count": len(drones)},
        "world": {"size_units": size, "metres_per_unit": scale, "size_m": size * scale, "fps": 24},
        "sim": sim,
        "clusters": [],
        "drones": drones,
        "links": [],
        "messages": [],
        "blender": {"collection": "Swarm", "drone_size_m": 12.0, "draw_links": "cluster", "sync": {"url": source_url, "interval_s": 1.0}},
    }


def build_plan(scene: dict) -> dict:
    """Turn a scene document into the concrete things to create in Blender.

    Pure and deterministic: same document in, same plan out. Everything the
    Blender layer needs is decided here, so all the interesting logic is
    testable without Blender.
    """
    hints = scene.get("blender") or {}
    world = scene.get("world") or {}
    size_m = float(world.get("size_m") or 2000.0)
    drone_size = float(hints.get("drone_size_m") or 12.0)
    draw_links = hints.get("draw_links", "cluster")

    drones = []
    by_id = {}
    for d in scene.get("drones") or []:
        if not d.get("id"):
            continue
        loc = d.get("location_m") or [0.0, 0.0, 0.0]
        is_leader = d.get("role") == "leader"
        item = {
            "id": d["id"],
            "name": d.get("object_name") or ("Drone_" + str(d["id"])),
            "location": [float(v) for v in loc[:3]],
            # A leader is drawn larger: size is a signal, not only colour, which
            # keeps a greyscale render readable.
            "scale": drone_size * (1.6 if is_leader else 1.0),
            "role": d.get("role", "worker"),
            "state": d.get("state", "alive"),
            "leader": d.get("leader", ""),
            "material": material_name(d),
            "colour": drone_colour(d),
            "label": "{} ({})".format(d["id"], d.get("role", "worker")),
            # A dead or killed drone falls to the ground and lies flat: the
            # render should show a crash, not a hovering corpse.
            "grounded": d.get("state") in ("dead", "killed"),
        }
        drones.append(item)
        by_id[d["id"]] = item

    links = []
    for l in scene.get("links") or []:
        if draw_links == "none":
            break
        if draw_links == "cluster" and l.get("kind") != "cluster":
            continue
        a, b = by_id.get(l.get("from")), by_id.get(l.get("to"))
        if not a or not b:
            continue
        links.append({
            "name": "Link_{}__{}".format(a["name"], b["name"]),
            "from": a["id"],
            "to": b["id"],
            "points": [a["location"], b["location"]],
            "kind": l.get("kind", "peer"),
            "radius": 1.6 if l.get("kind") == "cluster" else 0.6,
            "colour": (by_id.get(l.get("to"), {}).get("colour") if l.get("kind") == "cluster" else [0.5, 0.5, 0.5, 1.0]),
            "label": link_label(l),
        })

    messages = []
    if hints.get("animate_messages", True):
        for m in scene.get("messages") or []:
            a, b = by_id.get(m.get("from")), by_id.get(m.get("to"))
            if not a or not b:
                continue
            count = max(1, min(int(m.get("count") or 1), 6))
            messages.append({
                "name": "Msg_{}_{}_{}".format(a["name"], b["name"], m.get("type", "OTHER")),
                "from_location": a["location"],
                "to_location": b["location"],
                "type": m.get("type", "OTHER"),
                "count": count,
                "colour": m.get("colour") or [0.6, 0.6, 0.6, 1.0],
                # Dots fly for as long as the model says the hop takes, floored
                # so a near-zero delay is still visible.
                "flight_frames": max(4, int(round(float(m.get("flight_ms") or 60.0) / 1000.0 * float(world.get("fps") or 24)))),
            })

    return {
        "collection": hints.get("collection", "Swarm"),
        "world": {
            "size_m": size_m,
            "grid": bool(hints.get("draw_ground_grid", True)),
            "fps": int(world.get("fps") or 24),
            "metres_per_unit": float(world.get("metres_per_unit") or 20.0),
        },
        "labels": bool(hints.get("label_drones", True)),
        "drones": drones,
        "links": links,
        "messages": messages,
        "sync": (hints.get("sync") or {}),
        "source_url": (scene.get("meta") or {}).get("source_url", ""),
    }


def drone_colour(d: dict) -> list:
    """Cluster colour when alive, state colour otherwise.

    A live drone should read as "whose cluster am I in", and a sick one as
    "what is wrong with me": the second question wins when both apply.
    """
    if d.get("state") in ("alive",) and d.get("connected", True):
        c = d.get("cluster_colour") or d.get("state_colour")
    else:
        c = d.get("state_colour") or [0.5, 0.5, 0.5, 1.0]
    return [float(v) for v in (list(c) + [1.0, 1.0, 1.0, 1.0])[:4]]


def material_name(d: dict) -> str:
    """One material per (state, cluster) pair, so Blender reuses materials
    instead of creating one per drone per sync."""
    if d.get("state") == "alive" and d.get("connected", True):
        return "Swarm_Cluster_" + (d.get("leader") or "none")
    return "Swarm_State_" + str(d.get("state", "alive"))


def link_label(l: dict) -> str:
    """'41.2 u / 824 m, 83.4 ms' -- distance first, then what it cost."""
    parts = []
    if l.get("distance_units") is not None:
        parts.append("{:.1f} u".format(float(l["distance_units"])))
    if l.get("distance_m") is not None:
        parts.append("{:.0f} m".format(float(l["distance_m"])))
    head = " / ".join(parts)
    rtt = l.get("rtt_ms")
    pred = l.get("predicted_one_way_ms")
    if rtt is not None:
        return "{}, {:.1f} ms rtt".format(head, float(rtt))
    if pred:
        return "{}, ~{:.1f} ms one way".format(head, float(pred))
    return head


def positions_payload(moved: dict, metres_per_unit: float, size_units: float = 100.0) -> dict:
    """Blender metres -> the body for POST /api/sim.

    The inverse of the generator's to_metres: X and Y are centred on the origin,
    Z is an altitude. Values are clamped to the cube, because the backend clamps
    anyway and a clamped value here keeps the viewport honest.
    """
    out = {}
    for node_id, loc in moved.items():
        x = float(loc[0]) / metres_per_unit + size_units / 2.0
        y = float(loc[1]) / metres_per_unit + size_units / 2.0
        z = float(loc[2]) / metres_per_unit
        out[node_id] = {
            "x": round(min(max(x, 0.0), size_units), 3),
            "y": round(min(max(y, 0.0), size_units), 3),
            "z": round(min(max(z, 0.0), size_units), 3),
        }
    return {"positions": out}


def post_json(base_url: str, path: str, body: dict, timeout: float = 5.0) -> dict:
    """POST helper. The Control Center requires a JSON content type and
    answers {"error": ...} with a 4xx/5xx it explains."""
    url = base_url.rstrip("/") + path
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:300]
        raise RuntimeError("{} {}: {}".format(e.code, e.reason, detail)) from e


def base_url_of(scene_or_plan: dict, fallback: str = DEFAULT_URL) -> str:
    """Strip /api/... off a source URL to get the dashboard's base."""
    url = (scene_or_plan.get("source_url") or "") or ((scene_or_plan.get("sync") or {}).get("url") or "")
    if not url:
        return fallback
    for suffix in ("/api/state", "/api/scene"):
        if url.endswith(suffix):
            return url[: -len(suffix)]
    return url.rstrip("/")


# --------------------------------------------------------------------------
# --- Blender layer --------------------------------------------------------
# Everything below needs bpy. Imported lazily so the module can be imported
# (and tested) by plain Python.
# --------------------------------------------------------------------------

def _bpy():
    import bpy  # noqa: PLC0415 - deliberate lazy import

    return bpy


def get_collection(name: str):
    bpy = _bpy()
    col = bpy.data.collections.get(name)
    if col is None:
        col = bpy.data.collections.new(name)
        bpy.context.scene.collection.children.link(col)
    return col


def get_material(name: str, colour):
    """Fetch or create an emissive-ish material. Principled BSDF keeps it
    renderable in both EEVEE and Cycles with no node wiring."""
    bpy = _bpy()
    mat = bpy.data.materials.get(name)
    if mat is None:
        mat = bpy.data.materials.new(name)
        mat.use_nodes = True
    bsdf = mat.node_tree.nodes.get("Principled BSDF") if mat.use_nodes else None
    if bsdf:
        bsdf.inputs["Base Color"].default_value = colour
        if "Emission Color" in bsdf.inputs:  # Blender 4.x
            bsdf.inputs["Emission Color"].default_value = colour
            bsdf.inputs["Emission Strength"].default_value = 0.35
    mat.diffuse_color = colour
    return mat


def find_object(col, node_id: str, kind: str):
    for ob in col.objects:
        if ob.get(PROP_ID) == node_id and ob.get(PROP_KIND) == kind:
            return ob
    return None


def ensure_drone(col, item: dict):
    """Create or update one drone object. A cone points along +Z, which reads
    as a nose-up quadcopter without importing a mesh."""
    bpy = _bpy()
    ob = find_object(col, item["id"], "drone")
    if ob is None:
        mesh = bpy.data.meshes.new(item["name"] + "_mesh")
        ob = bpy.data.objects.new(item["name"], mesh)
        col.objects.link(ob)
        ob[PROP_ID] = item["id"]
        ob[PROP_KIND] = "drone"
        _cone_mesh(mesh)
    ob.location = item["location"]
    ob.scale = (item["scale"], item["scale"], item["scale"])
    # A crashed drone lies on its side on the ground.
    ob.rotation_euler = (math.radians(90) if item["grounded"] else 0.0, 0.0, 0.0)
    mat = get_material(item["material"], item["colour"])
    if ob.data.materials:
        ob.data.materials[0] = mat
    else:
        ob.data.materials.append(mat)
    return ob


def _cone_mesh(mesh, segments: int = 12):
    """A unit cone, built by hand so the add-on never depends on operator
    context (bpy.ops needs a view layer and fails in headless scripts)."""
    verts = [(0.0, 0.0, 1.0)]
    for i in range(segments):
        a = 2 * math.pi * i / segments
        verts.append((math.cos(a) * 0.5, math.sin(a) * 0.5, -0.5))
    faces = [(0, i + 1, (i + 1) % segments + 1) for i in range(segments)]
    faces.append(tuple(range(1, segments + 1)))
    mesh.from_pydata(verts, [], faces)
    mesh.update()


def ensure_link(col, item: dict):
    """One curve per link, with bevel so it renders as a tube."""
    bpy = _bpy()
    key = item["from"] + "->" + item["to"]
    ob = find_object(col, key, "link")
    if ob is None:
        curve = bpy.data.curves.new(item["name"], type="CURVE")
        curve.dimensions = "3D"
        curve.bevel_depth = item["radius"]
        spline = curve.splines.new("POLY")
        spline.points.add(1)
        ob = bpy.data.objects.new(item["name"], curve)
        col.objects.link(ob)
        ob[PROP_ID] = key
        ob[PROP_KIND] = "link"
    spline = ob.data.splines[0]
    for i, p in enumerate(item["points"]):
        spline.points[i].co = (p[0], p[1], p[2], 1.0)
    ob.data.bevel_depth = item["radius"]
    mat = get_material("Swarm_Link_" + item["kind"], item["colour"])
    if ob.data.materials:
        ob.data.materials[0] = mat
    else:
        ob.data.materials.append(mat)
    return ob


def ensure_ground(col, plan: dict):
    bpy = _bpy()
    ob = find_object(col, "ground", "ground")
    size = plan["world"]["size_m"]
    if ob is None:
        mesh = bpy.data.meshes.new("Swarm_Ground_mesh")
        half = size / 2.0
        mesh.from_pydata(
            [(-half, -half, 0.0), (half, -half, 0.0), (half, half, 0.0), (-half, half, 0.0)],
            [],
            [(0, 1, 2, 3)],
        )
        mesh.update()
        ob = bpy.data.objects.new("Swarm_Ground", mesh)
        col.objects.link(ob)
        ob[PROP_ID] = "ground"
        ob[PROP_KIND] = "ground"
        mat = get_material("Swarm_Ground", [0.05, 0.06, 0.08, 1.0])
        ob.data.materials.append(mat)
    return ob


def apply_plan(plan: dict, rebuild: bool = False) -> dict:
    """Create or update every object in the plan. Returns a small summary.

    Update, not rebuild, is the default: re-creating objects on every sync
    would drop the user's selection, their materials and any parenting they
    added, and would make the viewport flicker once a second.
    """
    bpy = _bpy()
    col = get_collection(plan["collection"])
    if rebuild:
        for ob in list(col.objects):
            bpy.data.objects.remove(ob, do_unlink=True)

    if plan["world"]["grid"]:
        ensure_ground(col, plan)

    seen = set()
    for item in plan["drones"]:
        ob = ensure_drone(col, item)
        seen.add(ob.name)
    live_links = set()
    for item in plan["links"]:
        ob = ensure_link(col, item)
        live_links.add(ob.name)

    # A link that is gone (a worker re-homed) must not linger.
    for ob in list(col.objects):
        if ob.get(PROP_KIND) == "link" and ob.name not in live_links:
            bpy.data.objects.remove(ob, do_unlink=True)

    bpy.context.scene.render.fps = plan["world"]["fps"]
    return {"drones": len(plan["drones"]), "links": len(plan["links"])}


def collect_moved(plan: dict) -> dict:
    """Drones whose Blender object has been moved away from the scene file.

    The tolerance is one centimetre: floating point round trips through the
    file must not look like an edit.
    """
    col = get_collection(plan["collection"])
    moved = {}
    for item in plan["drones"]:
        ob = find_object(col, item["id"], "drone")
        if ob is None:
            continue
        want = item["location"]
        have = [ob.location[0], ob.location[1], ob.location[2]]
        if any(abs(a - b) > 0.01 for a, b in zip(want, have)):
            moved[item["id"]] = have
    return moved


# --------------------------------------------------------------------------
# Operators, panel and the sync timer.
# --------------------------------------------------------------------------

_STATE = {"plan": None, "timer": False, "status": "idle"}


def _load_and_apply(source: str, rebuild: bool = False) -> str:
    scene = load_scene(source)
    plan = build_plan(scene)
    summary = apply_plan(plan, rebuild=rebuild)
    _STATE["plan"] = plan
    return "{} drones, {} links from {}".format(summary["drones"], summary["links"], source)


def _sync_tick():
    """Timer callback: re-fetch and re-apply. Returns the delay until the next
    call, or None to stop. Never raises: a Blender timer that raises is removed
    silently, which would look like the sync 'just stopped'."""
    if not _STATE["timer"]:
        return None
    plan = _STATE["plan"] or {}
    url = (plan.get("sync") or {}).get("url") or (base_url_of(plan) + "/api/state")
    interval = float((plan.get("sync") or {}).get("interval_s") or 1.0)
    try:
        _load_and_apply(url)
        _STATE["status"] = "live"
    except Exception as e:  # noqa: BLE001
        _STATE["status"] = "error: {}".format(e)[:60]
    return interval


def register():  # noqa: C901 - Blender registration is inherently flat
    bpy = _bpy()

    class SWARM_OT_load(bpy.types.Operator):
        bl_idname = "swarm.load"
        bl_label = "Load scene"
        bl_description = "Build or update the swarm from the scene file or URL"

        rebuild: bpy.props.BoolProperty(name="Rebuild", default=False)  # type: ignore

        def execute(self, context):
            try:
                msg = _load_and_apply(context.scene.swarm_source, rebuild=self.rebuild)
            except Exception as e:  # noqa: BLE001
                self.report({"ERROR"}, str(e))
                return {"CANCELLED"}
            self.report({"INFO"}, msg)
            return {"FINISHED"}

    class SWARM_OT_sync(bpy.types.Operator):
        bl_idname = "swarm.sync"
        bl_label = "Toggle live sync"
        bl_description = "Poll the Control Center and follow the swarm"

        def execute(self, context):
            _STATE["timer"] = not _STATE["timer"]
            if _STATE["timer"]:
                if _STATE["plan"] is None:
                    try:
                        _load_and_apply(context.scene.swarm_source)
                    except Exception as e:  # noqa: BLE001
                        _STATE["timer"] = False
                        self.report({"ERROR"}, str(e))
                        return {"CANCELLED"}
                bpy.app.timers.register(_sync_tick, first_interval=0.5)
                _STATE["status"] = "live"
            else:
                _STATE["status"] = "idle"
            return {"FINISHED"}

    class SWARM_OT_push(bpy.types.Operator):
        bl_idname = "swarm.push"
        bl_label = "Push positions"
        bl_description = "Send drones you moved in Blender to the running swarm"

        def execute(self, context):
            plan = _STATE["plan"]
            if not plan:
                self.report({"ERROR"}, "load a scene first")
                return {"CANCELLED"}
            moved = collect_moved(plan)
            if not moved:
                self.report({"INFO"}, "nothing moved")
                return {"FINISHED"}
            body = positions_payload(moved, plan["world"]["metres_per_unit"])
            try:
                post_json(base_url_of(plan, context.scene.swarm_base_url), "/api/sim", body)
            except Exception as e:  # noqa: BLE001
                self.report({"ERROR"}, str(e))
                return {"CANCELLED"}
            self.report({"INFO"}, "moved {} drone(s) in the swarm".format(len(moved)))
            return {"FINISHED"}

    class SWARM_OT_kill(bpy.types.Operator):
        bl_idname = "swarm.kill"
        bl_label = "Kill selected drone"
        bl_description = "CHAOS kill the selected drone. It stays dead"

        def execute(self, context):
            ob = context.active_object
            node_id = ob.get(PROP_ID) if ob else None
            if not node_id or ob.get(PROP_KIND) != "drone":
                self.report({"ERROR"}, "select a drone")
                return {"CANCELLED"}
            plan = _STATE["plan"] or {}
            try:
                post_json(base_url_of(plan, context.scene.swarm_base_url), "/api/chaos",
                          {"node": node_id, "action": "kill"})
            except Exception as e:  # noqa: BLE001
                self.report({"ERROR"}, str(e))
                return {"CANCELLED"}
            self.report({"WARNING"}, "killed {}".format(node_id))
            return {"FINISHED"}

    class SWARM_PT_panel(bpy.types.Panel):
        bl_label = "Swarm"
        bl_idname = "SWARM_PT_panel"
        bl_space_type = "VIEW_3D"
        bl_region_type = "UI"
        bl_category = "Swarm"

        def draw(self, context):
            layout = self.layout
            layout.prop(context.scene, "swarm_source", text="Source")
            layout.prop(context.scene, "swarm_base_url", text="Dashboard")
            row = layout.row()
            row.operator("swarm.load", text="Load / update")
            row.operator("swarm.load", text="Rebuild").rebuild = True
            layout.operator("swarm.sync", text=("Stop live sync" if _STATE["timer"] else "Start live sync"))
            layout.operator("swarm.push", icon="EXPORT")
            layout.operator("swarm.kill", icon="X")
            layout.label(text="Status: " + _STATE["status"])

    classes = (SWARM_OT_load, SWARM_OT_sync, SWARM_OT_push, SWARM_OT_kill, SWARM_PT_panel)
    for c in classes:
        bpy.utils.register_class(c)
    bpy.types.Scene.swarm_source = bpy.props.StringProperty(
        name="Source", default=DEFAULT_SCENE,
        description="Scene file path, or a URL ending in /api/scene or /api/state")
    bpy.types.Scene.swarm_base_url = bpy.props.StringProperty(
        name="Dashboard", default=DEFAULT_URL,
        description="Base URL used when pushing changes back")
    register._classes = classes  # type: ignore[attr-defined]


def unregister():
    bpy = _bpy()
    _STATE["timer"] = False
    for c in getattr(register, "_classes", ()):
        try:
            bpy.utils.unregister_class(c)
        except Exception:  # noqa: BLE001
            pass
    for prop in ("swarm_source", "swarm_base_url"):
        if hasattr(bpy.types.Scene, prop):
            delattr(bpy.types.Scene, prop)


def _cli():
    """`blender --python swarm_blender.py -- --scene X [--render out.png]`."""
    argv = sys.argv[sys.argv.index("--") + 1:] if "--" in sys.argv else []
    source = DEFAULT_SCENE
    render = ""
    for i, a in enumerate(argv):
        if a in ("--scene", "--source") and i + 1 < len(argv):
            source = argv[i + 1]
        elif a == "--render" and i + 1 < len(argv):
            render = argv[i + 1]
    register()
    print(_load_and_apply(source, rebuild=True))
    if render:
        bpy = _bpy()
        bpy.context.scene.render.filepath = render
        bpy.ops.render.render(write_still=True)
        print("rendered", render)


if __name__ == "__main__":
    try:
        import bpy  # noqa: F401

        _cli()
    except ImportError:
        print(__doc__)
        print("This script builds a Blender scene; run it with Blender:")
        print("  blender --python blender/swarm_blender.py -- --scene blender/swarm-scene.json")
