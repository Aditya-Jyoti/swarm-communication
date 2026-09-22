#!/usr/bin/env python3
"""Tests for the parts of the Blender tooling that do not need Blender.

Run: python3 blender/test_swarm_blender.py

Everything above the "Blender layer" comment in swarm_blender.py is plain
Python, so the interesting logic -- coordinate conversion, colour choice, link
labels, the plan, and the round trip back to /api/sim -- is testable here. The
bpy half is exercised by actually opening Blender; see blender/README.md.
"""

import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)

import make_swarm_scene as gen  # noqa: E402
import swarm_blender as sb  # noqa: E402


def sample_state():
    """A two-cluster swarm: one leader with a worker, plus a killed node."""
    return {
        "sim": {"version": 42, "enabled": True, "base_ms": 1, "per_unit_ms": 2,
                "jitter_ms": 0.5, "max_delay_ms": 1500, "threshold": 0, "hysteresis": -1, "size": 100},
        "nodes": [
            {"id": "lead-1", "role": "leader", "state": "alive", "connected": True, "leader": "lead-1",
             "pos": {"x": 50, "y": 50, "z": 50}, "term": 3, "ledger_size": 1, "dropped": 0,
             "threshold": 0.3, "hysteresis": 0.5, "sim_version": 42, "last_seen_ms": 100,
             "peers": [{"id": "work-1", "advertise": "work-1:7000", "state": "alive", "role": "worker", "score": 21.0, "incarnation": 1}],
             "scores": {"work-1:7000": 21.0},
             "flows": [{"to": "work-1", "type": "HEARTBEAT", "count": 2}]},
            {"id": "work-1", "role": "worker", "state": "alive", "connected": True, "leader": "lead-1",
             "pos": {"x": 60, "y": 50, "z": 50}, "term": 3, "ledger_size": 0, "dropped": 0,
             "threshold": 0.3, "hysteresis": 0.5, "sim_version": 42, "last_seen_ms": 120,
             "peers": [{"id": "lead-1", "advertise": "lead-1:7000", "state": "alive", "role": "leader", "score": 21.0, "incarnation": 1}],
             "scores": {"lead-1:7000": 21.0},
             "flows": [{"to": "lead-1", "type": "PING", "count": 1}]},
            {"id": "gone-1", "role": "worker", "state": "killed", "connected": False, "leader": "",
             "pos": {"x": 10, "y": 10, "z": 10}, "peers": [], "scores": {}, "flows": []},
        ],
        "tasks": [],
    }


class TestGenerator(unittest.TestCase):
    def setUp(self):
        self.scene = gen.build_scene(sample_state(), gen.METRES_PER_UNIT, None)

    def test_units_and_metres_are_both_present_and_consistent(self):
        d = {x["id"]: x for x in self.scene["nodes"]}["lead-1"]
        self.assertEqual(d["position_units"], {"x": 50.0, "y": 50.0, "z": 50.0})
        # Centre of the cube in X and Y -> origin; Z stays a height.
        self.assertEqual(d["location_m"], [0.0, 0.0, 50.0 * gen.METRES_PER_UNIT])

    def test_distance_uses_three_dimensions(self):
        s = gen.build_scene(sample_state(), 1.0, None)
        link = [l for l in s["links"] if {l["from"], l["to"]} == {"lead-1", "work-1"}][0]
        self.assertAlmostEqual(link["distance_units"], 10.0, places=3)
        # base 1 + 10 * 2 + jitter/2 = 21.25
        self.assertAlmostEqual(link["predicted_one_way_ms"], 21.25, places=3)
        self.assertAlmostEqual(link["rtt_ms"], 21.0, places=3)

    def test_cluster_link_is_marked(self):
        link = [l for l in self.scene["links"] if {l["from"], l["to"]} == {"lead-1", "work-1"}][0]
        self.assertEqual(link["kind"], "cluster")

    def test_killed_node_keeps_its_state(self):
        d = {x["id"]: x for x in self.scene["nodes"]}["gone-1"]
        self.assertEqual(d["state"], "killed")
        self.assertFalse(d["connected"])

    def test_disconnected_is_derived_when_state_still_says_alive(self):
        st = sample_state()
        st["nodes"][1]["connected"] = False
        s = gen.build_scene(st, gen.METRES_PER_UNIT, None)
        self.assertEqual({x["id"]: x for x in s["nodes"]}["work-1"]["state"], "disconnected")

    def test_messages_carry_a_colour_and_a_flight_time(self):
        m = [x for x in self.scene["messages"] if x["type"] == "HEARTBEAT"][0]
        self.assertEqual(m["colour"], gen.MESSAGE_COLOURS["HEARTBEAT"])
        self.assertGreater(m["flight_ms"], 0)

    def test_emulation_off_means_no_predicted_delay(self):
        st = sample_state()
        st["sim"]["enabled"] = False
        s = gen.build_scene(st, gen.METRES_PER_UNIT, None)
        self.assertEqual(s["links"][0]["predicted_one_way_ms"], 0.0)

    def test_document_is_self_describing(self):
        self.assertTrue(self.scene["schema"].startswith("swarm-scene/"))
        for key in ("_doc", "meta", "world", "sim", "nodes", "links", "messages", "write_back", "blender", "legend"):
            self.assertIn(key, self.scene)
        self.assertIn("positions", self.scene["write_back"]["move_example"])

    def test_json_round_trip(self):
        again = json.loads(json.dumps(self.scene))
        self.assertEqual(again["meta"]["node_count"], 3)


class TestPlan(unittest.TestCase):
    def setUp(self):
        self.scene = gen.build_scene(sample_state(), gen.METRES_PER_UNIT, None)
        self.plan = sb.build_plan(self.scene)

    def test_leader_is_bigger_than_a_worker(self):
        by = {d["id"]: d for d in self.plan["nodes"]}
        self.assertGreater(by["lead-1"]["scale"], by["work-1"]["scale"])

    def test_alive_node_takes_its_cluster_colour_and_a_sick_one_its_state_colour(self):
        by = {d["id"]: d for d in self.plan["nodes"]}
        self.assertEqual(by["work-1"]["colour"], self.scene["clusters"][0]["colour"])
        self.assertEqual(by["gone-1"]["colour"], gen.STATE_COLOURS["killed"])

    def test_killed_node_is_grounded(self):
        by = {d["id"]: d for d in self.plan["nodes"]}
        self.assertTrue(by["gone-1"]["grounded"])
        self.assertFalse(by["lead-1"]["grounded"])

    def test_only_cluster_links_are_drawn_by_default(self):
        self.assertTrue(all(l["kind"] == "cluster" for l in self.plan["links"]))
        self.assertEqual(len(self.plan["links"]), 1)

    def test_draw_links_all_includes_peer_links(self):
        scene = dict(self.scene)
        scene["blender"] = dict(scene["blender"], draw_links="all")
        self.assertGreaterEqual(len(sb.build_plan(scene)["links"]), 1)

    def test_link_label_mentions_distance_and_rtt(self):
        label = self.plan["links"][0]["label"]
        self.assertIn("u", label)
        self.assertIn("ms", label)

    def test_materials_are_shared_per_state_and_cluster(self):
        names = {d["id"]: d["material"] for d in self.plan["nodes"]}
        self.assertEqual(names["lead-1"], names["work-1"])  # same cluster
        self.assertTrue(names["gone-1"].endswith("killed"))

    def test_message_flight_frames_never_zero(self):
        for m in self.plan["messages"]:
            self.assertGreaterEqual(m["flight_frames"], 4)

    def test_plan_is_deterministic(self):
        self.assertEqual(sb.build_plan(self.scene), self.plan)


class TestWriteBack(unittest.TestCase):
    def test_metres_convert_back_to_the_units_they_came_from(self):
        scene = gen.build_scene(sample_state(), gen.METRES_PER_UNIT, None)
        plan = sb.build_plan(scene)
        by = {d["id"]: d for d in plan["nodes"]}
        body = sb.positions_payload({"lead-1": by["lead-1"]["location"]}, plan["world"]["metres_per_unit"])
        self.assertEqual(body["positions"]["lead-1"], {"x": 50.0, "y": 50.0, "z": 50.0})

    def test_positions_are_clamped_to_the_cube(self):
        body = sb.positions_payload({"a": [1e9, -1e9, -5.0]}, 20.0)
        self.assertEqual(body["positions"]["a"], {"x": 100.0, "y": 0.0, "z": 0.0})

    def test_base_url_strips_the_api_path(self):
        self.assertEqual(sb.base_url_of({"source_url": "http://h:8080/api/state"}), "http://h:8080")
        self.assertEqual(sb.base_url_of({"sync": {"url": "http://h:8080/api/scene"}}), "http://h:8080")
        self.assertEqual(sb.base_url_of({}, "http://fallback"), "http://fallback")


class TestLoading(unittest.TestCase):
    def test_a_scene_file_loads_unchanged(self):
        scene = gen.build_scene(sample_state(), gen.METRES_PER_UNIT, None)
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(scene, f)
            path = f.name
        try:
            self.assertEqual(sb.load_scene(path)["meta"]["node_count"], 3)
        finally:
            os.unlink(path)

    def test_raw_state_is_converted(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(sample_state(), f)
            path = f.name
        try:
            scene = sb.load_scene(path)
            self.assertTrue(scene["schema"].startswith("swarm-scene/"))
            self.assertEqual(len(scene["nodes"]), 3)
        finally:
            os.unlink(path)

    def test_a_foreign_document_is_rejected(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump({"hello": "world"}, f)
            path = f.name
        try:
            with self.assertRaises(ValueError):
                sb.load_scene(path)
        finally:
            os.unlink(path)

    def test_the_committed_scene_file_is_valid(self):
        path = os.path.join(ROOT, "blender", "swarm-scene.json")
        if not os.path.exists(path):
            self.skipTest("no committed scene file")
        plan = sb.build_plan(sb.load_scene(path))
        self.assertTrue(plan["nodes"])

    def test_the_addon_runs_without_blender(self):
        """Running the add-on with plain Python must explain itself, not crash."""
        out = subprocess.run([sys.executable, os.path.join(HERE, "swarm_blender.py")],
                             capture_output=True, text=True, timeout=60)
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("run it with Blender", out.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
