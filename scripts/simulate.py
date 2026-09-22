#!/usr/bin/env python3
"""Drive a running swarm through a scripted scenario and report what it did.

    docker compose up -d                 # any N; NODE_REPLICAS=9 gives 10 nodes
    python3 scripts/simulate.py          # against http://127.0.0.1:8080
    python3 scripts/simulate.py --record blender/scenario.json   # also record a Blender timeline

Every phase changes one thing through the same public API the dashboard uses,
waits for the swarm to settle, and checks the result against what the design
predicts. Nothing here reaches into a container: it only talks HTTP to the
Control Center, so it tests the system the way an operator drives it.

Phases:
  1. baseline     leaders are the most central nodes; every worker is on its nearest leader
  2. workload     a batch of tasks completes, spread over the clusters
  3. fly          move a worker next to a different leader; it re-homes there, or is
                  elected leader itself if that spot made it the most central node
  4. stretch      scale latency per unit x2.5; grouping is proportional, so it holds
  5. kill leader  CHAOS kill a leader; a new one is elected, its workers re-home, it stays down
  6. threshold    raise the leader fraction; more leaders appear
  7. restore      clear the overrides; the leader count returns to the formula
"""

from __future__ import annotations

import argparse
import json
import math
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


class Swarm:
    def __init__(self, base: str):
        self.base = base.rstrip("/")

    def _req(self, method: str, path: str, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return json.loads(r.read().decode() or "{}")
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"{method} {path}: {e.code} {e.read().decode()[:200]}") from e

    def state(self):
        return self._req("GET", "/api/state")

    def sim(self, **fields):
        return self._req("POST", "/api/sim", fields)

    def tasks(self, kind: str, body, count: int):
        return self._req("POST", "/api/tasks", {"kind": kind, "body": body, "count": count})

    def kill(self, node: str):
        return self._req("POST", "/api/chaos", {"node": node, "action": "kill"})


# ---------------------------------------------------------------- analysis

def live(state):
    return {n["id"]: n for n in state.get("nodes", []) if n.get("connected") and n.get("state") == "alive"}


def dist(a, b):
    return math.dist([a["x"], a["y"], a["z"]], [b["x"], b["y"], b["z"]])


def median(xs):
    xs = sorted(xs)
    if not xs:
        return float("nan")
    m = len(xs) // 2
    return xs[m] if len(xs) % 2 else (xs[m - 1] + xs[m]) / 2


def want_leaders(n: int, threshold: float) -> int:
    return max(1, math.ceil(n * threshold - 1e-9))


def analyse(state):
    """What the swarm did versus what the geometry predicts."""
    nodes = live(state)
    leaders = sorted(i for i, n in nodes.items() if n["role"] == "leader")
    workers = sorted(i for i, n in nodes.items() if n["role"] != "leader")
    central = {i: median([dist(n["pos"], m["pos"]) for j, m in nodes.items() if j != i]) for i, n in nodes.items()}
    wrong_home = []
    for w in workers:
        if not leaders:
            break
        near = min(leaders, key=lambda l: dist(nodes[w]["pos"], nodes[l]["pos"]))
        if nodes[w]["leader"] != near:
            wrong_home.append((w, nodes[w]["leader"], near))
    clusters = {l: sorted(w for w in workers if nodes[w]["leader"] == l) for l in leaders}
    return {
        "alive": len(nodes),
        "leaders": leaders,
        "clusters": clusters,
        "unattached": [w for w in workers if nodes[w]["leader"] not in leaders],
        "wrong_home": wrong_home,
        "central": central,
        "threshold": sorted({n.get("threshold") for n in nodes.values()}),
    }


STABLE_S = 8


def settled(swarm: Swarm, check, timeout: float, what: str, stable: float = STABLE_S):
    """Poll until check(analysis) holds AND the clusters stay unchanged for
    `stable` seconds. Returns (analysis, seconds until it first became stable).

    "Momentarily correct" is not "settled". Scores are EWMA-smoothed over
    several probe rounds and hysteresis holds a leader until a rival is
    clearly better, so after a change the swarm can pass the check once and
    then legitimately re-elect a few seconds later. A scenario that moves on
    at the first pass blames the NEXT phase for that late re-election.
    """
    start = time.monotonic()
    last, since, key = None, None, None
    while time.monotonic() - start < timeout:
        try:
            last = analyse(swarm.state())
            ok = check(last)
        except Exception as e:  # noqa: BLE001 - a CC restart mid-poll is survivable
            last, ok = {"error": str(e)}, False
        k = json.dumps(last.get("clusters"), sort_keys=True) if ok else None
        if ok and k == key:
            if time.monotonic() - since >= stable:
                return last, since - start
        elif ok:
            key, since = k, time.monotonic()
        else:
            key, since = None, None
        time.sleep(1)
    raise TimeoutError(f"{what}: not stable in {timeout:.0f}s (last: {json.dumps(last, default=str)[:400]})")


def short(i: str) -> str:
    return i.replace("swarm-net-", "")


def show_clusters(a):
    return "  ".join(f"{short(l)}[{', '.join(short(w) for w in ws) or '-'}]" for l, ws in a["clusters"].items())


# ---------------------------------------------------------------- scenario

def run(swarm: Swarm, threshold: float, settle: float):
    results = []

    def phase(name, detail, ok, seconds=None, **extra):
        mark = "PASS" if ok else "FAIL"
        took = f" ({seconds:.0f}s)" if seconds is not None else ""
        print(f"[{mark}] {name}{took}: {detail}")
        for k, v in extra.items():
            print(f"       {k}: {v}")
        results.append((name, ok))

    healthy = lambda a: (not a["unattached"] and not a["wrong_home"]
                         and len(a["leaders"]) == want_leaders(a["alive"], threshold))

    # 1. baseline -------------------------------------------------------------
    swarm.sim(reset_positions=True, threshold=0, hysteresis=-1, per_unit_ms=2, base_ms=1, jitter_ms=0.5, enabled=True)
    a, s = settled(swarm, healthy, settle, "baseline")
    most_central = sorted(a["central"], key=a["central"].get)[: len(a["leaders"])]
    phase("1 baseline",
          f"{a['alive']} nodes, {len(a['leaders'])} leaders (formula wants {want_leaders(a['alive'], threshold)})",
          set(most_central) == set(a["leaders"]) and not a["wrong_home"], s,
          clusters=show_clusters(a),
          leaders_are_most_central=f"{set(most_central) == set(a['leaders'])} "
          f"(median distances {', '.join(f'{short(i)} {a['central'][i]:.0f}' for i in sorted(a['central'], key=a['central'].get)[:4])})")

    # 2. workload -------------------------------------------------------------
    ids = swarm.tasks("hash", {"data": "simulate"}, 30).get("task_ids", [])
    start = time.monotonic()
    done = []
    while time.monotonic() - start < settle:
        tasks = {t["task_id"]: t for t in swarm.state().get("tasks", [])}
        done = [tasks[i] for i in ids if i in tasks and tasks[i]["state"] != "pending"]
        if len(done) == len(ids):
            break
        time.sleep(1)
    ok = [t for t in done if t["state"] == "done"]
    by_leader = {}
    for t in ok:
        by_leader[short(t["leader"])] = by_leader.get(short(t["leader"]), 0) + 1
    phase("2 workload", f"{len(ok)}/{len(ids)} tasks done", len(ok) == len(ids), time.monotonic() - start,
          per_leader=by_leader)

    # 3. fly a worker next to another leader ------------------------------------
    a = analyse(swarm.state())
    nodes = live(swarm.state())
    mover, target = None, None
    for w in sorted(set(nodes) - set(a["leaders"])):
        others = [l for l in a["leaders"] if l != nodes[w]["leader"]]
        if others:
            mover, target = w, others[0]
            break
    tp = nodes[target]["pos"]
    dest = {"x": min(100, tp["x"] + 3), "y": min(100, tp["y"] + 3), "z": tp["z"]}
    swarm.sim(positions={mover: dest})
    # Two outcomes are both correct, and which one happens depends on the
    # geometry: the mover joins the target's cluster, or -- if parking it there
    # made it one of the most central nodes -- it is elected leader itself.
    # What the design guarantees is a stable, correct state either way.
    a, s = settled(swarm, healthy, settle, "fly")
    if mover in a["leaders"]:
        outcome = f"it became central enough to be elected leader itself (median distance {a['central'][mover]:.0f})"
    elif mover in a["clusters"].get(target, []):
        outcome = f"it re-homed to {short(target)}"
    else:
        outcome = f"it joined {short(live(swarm.state())[mover]['leader'])}, now its nearest leader"
    phase("3 fly", f"moved {short(mover)} beside leader {short(target)}; {outcome}", True, s,
          clusters=show_clusters(a))

    # 4. stretch latency --------------------------------------------------------
    before = analyse(swarm.state())["clusters"]
    swarm.sim(per_unit_ms=5)
    time.sleep(8)  # several probe rounds for the EWMA to follow
    a, s = settled(swarm, healthy, settle, "stretch")
    phase("4 stretch", "per_unit_ms 2 -> 5: every RTT grows x2.5, so the ranking and the groups hold",
          a["clusters"] == before, s + 8, clusters=show_clusters(a))

    # 5. kill a leader ----------------------------------------------------------
    a = analyse(swarm.state())
    victim = max(a["leaders"], key=lambda l: len(a["clusters"][l]))
    orphans = a["clusters"][victim]
    swarm.kill(victim)
    t0 = time.monotonic()
    a, s = settled(swarm, lambda a: victim not in a["leaders"] and healthy(a), settle, "kill leader")
    st = {n["id"]: n for n in swarm.state()["nodes"]}.get(victim, {})
    phase("5 kill leader",
          f"killed {short(victim)} (led {len(orphans)}); {a['alive']} nodes left, leaders now "
          f"{', '.join(short(l) for l in a['leaders'])}",
          st.get("state") == "killed" or not st.get("connected"), s,
          clusters=show_clusters(a),
          victim=f"state={st.get('state', 'gone')} connected={st.get('connected', False)}")

    # 6. raise the leader fraction -----------------------------------------------
    swarm.sim(threshold=0.5)
    a, s = settled(swarm, lambda a: len(a["leaders"]) == want_leaders(a["alive"], 0.5)
                   and not a["unattached"], settle, "threshold")
    phase("6 threshold", f"threshold 0.5: {len(a['leaders'])} leaders of {a['alive']}", True, s,
          clusters=show_clusters(a))

    # 7. restore ----------------------------------------------------------------
    swarm.sim(threshold=0, hysteresis=-1, per_unit_ms=2)
    a, s = settled(swarm, healthy, settle, "restore")
    phase("7 restore", f"overrides cleared: {len(a['leaders'])} leaders of {a['alive']} "
          f"(each node back on its own threshold {a['threshold']})", True, s,
          clusters=show_clusters(a))

    failed = [n for n, ok in results if not ok]
    print()
    print("RESULT:", "all phases passed" if not failed else f"FAILED: {', '.join(failed)}")
    print("Killed nodes stay down. Bring them back with: docker compose up -d")
    return 0 if not failed else 1


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://127.0.0.1:8080")
    ap.add_argument("--threshold", type=float, default=0.3, help="the nodes' configured SWARM_THRESHOLD")
    ap.add_argument("--settle", type=float, default=90, help="seconds to wait for each phase")
    ap.add_argument("--record", default="", help="also record a Blender timeline to this file")
    args = ap.parse_args()

    swarm = Swarm(args.url)
    recorder = None
    if args.record:
        # One sample a second for the whole run; the generator stops early if the
        # stack disappears, so an over-long frame count is harmless.
        recorder = subprocess.Popen([sys.executable, os.path.join(ROOT, "blender", "make_swarm_scene.py"),
                                     "--url", args.url, "--out", args.record, "--frames", "600", "--interval", "1"])
    try:
        code = run(swarm, args.threshold, args.settle)
    finally:
        if recorder:
            # SIGTERM makes the generator stop sampling and write what it has.
            recorder.terminate()
            recorder.wait(timeout=30)
            print(f"Blender timeline written to {args.record}")
    return code


if __name__ == "__main__":
    raise SystemExit(main())
