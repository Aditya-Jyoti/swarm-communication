#!/usr/bin/env bash
# End-to-end self-healing check for swarm-net.
#
#   scripts/e2e.sh                 # 1 seed + 5 nodes
#   E2E_NODES=8 scripts/e2e.sh     # 1 seed + 8 nodes
#   E2E_KEEP=1 scripts/e2e.sh      # leave the stack running afterwards
#
# Everything goes through the FRONTEND port (nginx), so the reverse proxy to
# the control center is exercised on every request. The CC itself is not
# published.
#
# Steps:
#   1. build and start the stack (backend/ + frontend/) under its own Compose
#      project
#   2. check the proxy: the dashboard is served at /, /healthz answers through
#      nginx, and a same-origin WebSocket upgrade on /ws succeeds while a
#      cross-origin one is refused
#   3. wait until every node is connected to the control center, exactly
#      max(1, ceil(N * threshold)) leaders exist, and every worker is attached
#      to a live leader
#   4. submit tasks over HTTP and wait until all are done
#   5. `docker kill` a current leader
#   6. wait until a leader other than the victim exists and every surviving
#      node is attached again
#   7. submit tasks again and wait until all are done
#   8. send CHAOS kill to a worker through POST /api/chaos, then check that its
#      container exits with code 0, is NOT restarted ~10s later
#      (restart: on-failure), is shown as "killed", and that the swarm is
#      healthy again with the right leader count
#   9. POST /api/sim with a new per_unit_ms and check that GET /api/sim
#      reflects it with a newer version, that the connected drones apply it
#      (sim_version), and that the swarm is still healthy afterwards
#
# Exits non-zero with a FAIL line (plus recent logs) on the first failed step.
# The stack is always torn down (`down -v`) on exit unless E2E_KEEP=1.
#
# Env:
#   E2E_NODES            replicas of the `node` service       (default 5)
#   E2E_PROJECT          compose project name                 (default swarm-net-e2e)
#   E2E_HTTP_PORT        host port for the frontend (nginx)   (default 18080)
#   E2E_READY_TIMEOUT    seconds for initial convergence      (default 120)
#   E2E_HEAL_TIMEOUT     seconds for failover to complete     (default 60)
#   E2E_TASK_TIMEOUT     seconds for a task batch to finish   (default 60)
#   E2E_TASKS            tasks per batch                      (default 5)
#   E2E_KILL_WAIT        seconds a killed container must stay down (default 10)
#   E2E_SIM_PER_UNIT     per_unit_ms sent in step 9            (default 3.5)
#   E2E_NO_BUILD=1       skip --build (reuse existing images)
#   E2E_KEEP=1           do not tear down on exit
#   E2E_JSON=python3     force the python3 JSON path (default: jq if present)
#   SWARM_THRESHOLD      leader fraction, passed to the stack (default 0.3)
#
# The script exports FRONTEND_PORT, BIND_ADDR and SWARM_THRESHOLD, which take
# precedence over a local .env; the other .env settings still apply.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODES="${E2E_NODES:-5}"
PROJECT="${E2E_PROJECT:-swarm-net-e2e}"
PORT="${E2E_HTTP_PORT:-18080}"
READY_TIMEOUT="${E2E_READY_TIMEOUT:-120}"
HEAL_TIMEOUT="${E2E_HEAL_TIMEOUT:-60}"
TASK_TIMEOUT="${E2E_TASK_TIMEOUT:-60}"
# Must match the stack's SWARM_THRESHOLD (docker-compose.yml passes it through).
THRESHOLD="${SWARM_THRESHOLD:-0.3}"
export SWARM_THRESHOLD="$THRESHOLD"
TASKS="${E2E_TASKS:-5}"
KILL_WAIT="${E2E_KILL_WAIT:-10}"
SIM_PER_UNIT="${E2E_SIM_PER_UNIT:-3.5}"
BASE="http://127.0.0.1:${PORT}"
TOTAL=$((NODES + 1)) # + seed
# Steps 5 and 8 each remove one node for good; step 8 needs a worker to spare.
if [ "$NODES" -lt 3 ]; then
	echo "E2E_NODES must be at least 3" >&2
	exit 2
fi

# The frontend is the only published port; bind it to loopback for the test.
export FRONTEND_PORT="$PORT"
export BIND_ADDR=127.0.0.1
COMPOSE=(docker compose -f "$ROOT/docker-compose.yml" -p "$PROJECT")

# ------------------------------------------------------------------ output

ts() { date +%H:%M:%S; }
log() { printf '[%s] %s\n' "$(ts)" "$*"; }
fail() {
	printf '[%s] FAIL: %s\n' "$(ts)" "$*" >&2
	FAILED=1
	exit 1
}
FAILED=0

cleanup() {
	local code=$?
	if [ "$code" -ne 0 ] || [ "$FAILED" -ne 0 ]; then
		echo "---- last /api/state ----" >&2
		curl -fsS "$BASE/api/state" 2>/dev/null | head -c 4000 >&2 || true
		echo >&2
		echo "---- recent logs ----" >&2
		"${COMPOSE[@]}" logs --no-color --tail=40 >&2 2>/dev/null || true
	fi
	if [ "${E2E_KEEP:-0}" = "1" ]; then
		log "E2E_KEEP=1: leaving project $PROJECT running (${COMPOSE[*]} down -v)"
	else
		log "tearing down project $PROJECT"
		"${COMPOSE[@]}" down -v --remove-orphans -t 3 >/dev/null 2>&1 || true
	fi
	exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ------------------------------------------------------------------ deps

command -v docker >/dev/null || fail "docker not found"
command -v curl >/dev/null || fail "curl not found"
if [ -n "${E2E_JSON:-}" ]; then
	JSON="$E2E_JSON"
elif command -v jq >/dev/null; then
	JSON=jq
elif command -v python3 >/dev/null; then
	JSON=python3
else
	fail "need jq or python3 to parse JSON"
fi

# q NAME [ARG] < json
#
# Every question the script asks of /api/state or a POST response, written
# once for jq and once for python3 so the script runs on either. Each prints a
# single line.
q() {
	local name="$1" arg="${2:-}"
	if [ "$JSON" = jq ]; then
		case "$name" in
		# "<connected> <leaders> <unattached>" ignoring the space-separated node
		# ids in ARG (may be empty). A node is attached if it is a leader, or
		# its leader is a connected leader that is not in ARG.
		health)
			jq -r --arg skip "$arg" '
				($skip | split(" ") | map(select(. != ""))) as $skips
				| [.nodes // [] | .[] | select(.connected == true) | select(.id as $i | any($skips[]; . == $i) | not)] as $live
				| [$live[] | select(.role == "leader") | .id] as $leaders
				| [$live[] | select(.role != "leader")
				           | select((.leader // "") as $l | ($leaders | map(. == $l) | any) | not)] as $loose
				| "\($live | length) \($leaders | length) \($loose | length)"'
			;;
		leaders) jq -r '[.nodes // [] | .[] | select(.connected == true and .role == "leader") | .id] | join(" ")' ;;
		task_ids) jq -r '.task_ids // [] | join(" ")' ;;
		# A connected worker that is not the seed and not in ARG (space-separated).
		pick_worker)
			jq -r --arg skip "$arg" '
				($skip | split(" ")) as $skips
				| [.nodes // [] | .[] | select(.connected == true and .role != "leader" and .id != "seed")
				  | select(.id as $i | any($skips[]; . == $i) | not) | .id] | first // ""'
			;;
		# "<state> <connected>" of node ARG, or "missing missing".
		node_state)
			jq -r --arg id "$arg" '
				([.nodes // [] | .[] | select(.id == $id)] | first) as $n
				| if $n == null then "missing missing" else "\($n.state // "") \($n.connected)" end'
			;;
		# One field of a sim object (GET /api/sim or a POST /api/sim reply).
		sim_field) jq -r --arg f "$arg" '.[$f] // "missing" | tostring' ;;
		# "<applied> <live>": connected, non-killed drones whose sim_version is
		# at least ARG, out of all of them.
		sim_applied)
			jq -r --argjson v "$arg" '
				[.nodes // [] | .[] | select(.connected == true and .state != "killed")] as $live
				| "\([$live[] | select((.sim_version // 0) >= $v)] | length) \($live | length)"'
			;;
		# "<done_ok> <failed> <missing_or_pending>" for the space-separated ids in ARG.
		task_states)
			jq -r --arg ids "$arg" '
				($ids | split(" ") | map(select(. != ""))) as $want
				| (.tasks // []) as $t
				| [$want[] as $id | ([$t[] | select(.task_id == $id)] | first)] as $got
				| [$got[] | select(. != null and .state == "done" and .ok == true)] as $ok
				| [$got[] | select(. != null and (.state == "failed" or (.state == "done" and .ok != true)))] as $bad
				| "\($ok | length) \($bad | length) \(($want | length) - ($ok | length) - ($bad | length))"'
			;;
		*) fail "unknown query $name" ;;
		esac
	else
		python3 -c '
import json, sys
name, arg = sys.argv[1], sys.argv[2]
d = json.load(sys.stdin) or {}
nodes = d.get("nodes") or []
if name == "health":
    skips = arg.split()
    live = [n for n in nodes if n.get("id") not in skips and n.get("connected") is True]
    leaders = [n.get("id") for n in live if n.get("role") == "leader"]
    loose = [n for n in live if n.get("role") != "leader" and (n.get("leader") or "") not in leaders]
    print(len(live), len(leaders), len(loose))
elif name == "leaders":
    print(" ".join(n.get("id") for n in nodes if n.get("connected") is True and n.get("role") == "leader"))
elif name == "task_ids":
    print(" ".join(d.get("task_ids") or []))
elif name == "pick_worker":
    skips = arg.split()
    ids = [n.get("id") for n in nodes if n.get("connected") is True and n.get("role") != "leader"
           and n.get("id") != "seed" and n.get("id") not in skips]
    print(ids[0] if ids else "")
elif name == "node_state":
    n = next((n for n in nodes if n.get("id") == arg), None)
    print("missing missing" if n is None else "%s %s" % (n.get("state") or "", json.dumps(n.get("connected"))))
elif name == "sim_field":
    v = d.get(arg)
    print("missing" if v is None else (v if isinstance(v, str) else json.dumps(v)))
elif name == "sim_applied":
    live = [n for n in nodes if n.get("connected") is True and n.get("state") != "killed"]
    print(sum(1 for n in live if (n.get("sim_version") or 0) >= float(arg)), len(live))
elif name == "task_states":
    want = [i for i in arg.split(" ") if i]
    first = {}
    for t in d.get("tasks") or []:
        first.setdefault(t.get("task_id"), t)
    ok = sum(1 for i in want if i in first and first[i].get("state") == "done" and first[i].get("ok") is True)
    bad = sum(1 for i in want if i in first and (first[i].get("state") == "failed" or (first[i].get("state") == "done" and first[i].get("ok") is not True)))
    print(ok, bad, len(want) - ok - bad)
else:
    sys.exit("unknown query " + name)
' "$name" "$arg"
	fi
}

state() { curl -fsS --max-time 3 "$BASE/api/state"; }

# post PATH JSON -> response body; fails on a non-2xx status.
post() { curl -fsS --max-time 5 -H 'Content-Type: application/json' -d "$2" "$BASE$1"; }

# num_cmp A OP B -> numeric comparison (OP is ==, > or >=), so 3.5 == 3.50.
num_cmp() {
	awk -v a="$1" -v op="$2" -v b="$3" 'BEGIN {
		if (a !~ /^-?[0-9.]+$/ || b !~ /^-?[0-9.]+$/) exit 1
		if (op == "==") exit !(a + 0 == b + 0)
		if (op == ">") exit !(a + 0 > b + 0)
		if (op == ">=") exit !(a + 0 >= b + 0)
		exit 1
	}'
}

# ------------------------------------------------------------------ steps

# want_leaders N -> max(1, ceil(N * THRESHOLD)), the LeaderCount formula.
want_leaders() {
	awk -v n="$1" -v t="$THRESHOLD" 'BEGIN { x = n * t; c = int(x); if (c < x) c++; if (c < 1) c = 1; print c }'
}

# wait_healthy SKIP_IDS WANT_LIVE TIMEOUT LABEL
#
# SKIP_IDS is a space-separated list of node ids to leave out (killed ones).
#
# Healthy means: every expected node connected, EXACTLY the formula's number of
# self-declared leaders, and every worker attached to one of them. Counting
# "at least one leader" is not enough: a swarm whose mesh never formed has
# every node leading itself, and every node is then trivially "attached".
wait_healthy() {
	local skip="$1" want="$2" timeout="$3" label="$4"
	local deadline=$((SECONDS + timeout)) last="" live leaders loose s wantl
	wantl="$(want_leaders "$want")"
	while [ "$SECONDS" -lt "$deadline" ]; do
		if s="$(state 2>/dev/null)" && read -r live leaders loose < <(printf '%s' "$s" | q health "$skip"); then
			last="connected=$live/$want leaders=$leaders/$wantl unattached=$loose"
			if [ "$live" -ge "$want" ] && [ "$leaders" -eq "$wantl" ] && [ "$loose" -eq 0 ]; then
				log "$label: $last (after $((SECONDS - deadline + timeout))s)"
				return 0
			fi
		else
			last="control center not answering"
		fi
		sleep 1
	done
	fail "$label: not healthy within ${timeout}s (last: $last)"
}

# run_tasks LABEL
run_tasks() {
	local label="$1" resp ids deadline ok bad rest last=""
	local body
	body=$(printf '{"type":"task","kind":"hash","body":{"data":"e2e-%s"},"count":%d}' "$label" "$TASKS")
	resp="$(curl -fsS --max-time 5 -H 'Content-Type: application/json' -d "$body" "$BASE/api/tasks")" ||
		fail "$label: POST /api/tasks failed"
	ids="$(printf '%s' "$resp" | q task_ids)"
	[ -n "$ids" ] || fail "$label: POST /api/tasks returned no task_ids: $resp"
	log "$label: submitted $(wc -w <<<"$ids") tasks"
	deadline=$((SECONDS + TASK_TIMEOUT))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if read -r ok bad rest < <(state | q task_states "$ids"); then
			last="done=$ok failed=$bad pending=$rest"
			[ "$bad" -eq 0 ] || fail "$label: task(s) failed ($last)"
			if [ "$rest" -eq 0 ]; then
				log "$label: all tasks done ($last)"
				return 0
			fi
		fi
		sleep 1
	done
	fail "$label: tasks not done within ${TASK_TIMEOUT}s (last: $last)"
}

# check_proxy
#
# The stack is reached only through nginx. Checks that it serves the
# dashboard itself, proxies /healthz to the CC, and passes a WebSocket upgrade
# through with Host intact: the CC accepts an upgrade only when Origin matches
# Host, so a proxy that rewrote Host would fail the first upgrade, and one
# that skipped the check would pass the second.
check_proxy() {
	local deadline=$((SECONDS + READY_TIMEOUT)) page code
	while [ "$SECONDS" -lt "$deadline" ]; do
		[ "$(curl -fsS --max-time 3 "$BASE/healthz" 2>/dev/null)" = "ok" ] && break
		sleep 1
	done
	[ "$(curl -fsS --max-time 3 "$BASE/healthz" 2>/dev/null)" = "ok" ] ||
		fail "proxy: GET /healthz through the frontend did not return ok"
	page="$(curl -fsS --max-time 3 "$BASE/")" || fail "proxy: GET / failed"
	grep -q 'src="app.js"' <<<"$page" || fail "proxy: GET / is not the dashboard"
	curl -fsS --max-time 3 -o /dev/null "$BASE/app.js" || fail "proxy: GET /app.js failed"
	code="$(ws_upgrade "$BASE")"
	[ "$code" = "101" ] || fail "proxy: same-origin WebSocket upgrade returned $code, want 101"
	code="$(ws_upgrade "http://evil.example")"
	[ "$code" = "403" ] || fail "proxy: cross-origin WebSocket upgrade returned $code, want 403"
	log "proxy: dashboard served, /healthz ok, /ws upgrade 101 (same origin) / 403 (foreign origin)"
}

# ws_upgrade ORIGIN -> HTTP status of a WebSocket handshake on $BASE/ws.
# The key is RFC 6455's sample nonce (it must decode to 16 bytes).
# curl cannot speak WebSocket frames here, so it only reads the status line;
# --max-time ends the (then open) connection, hence the `|| true`.
ws_upgrade() {
	curl -sS -o /dev/null -w '%{http_code}' --http1.1 --max-time 2 \
		-H 'Connection: Upgrade' -H 'Upgrade: websocket' \
		-H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
		-H "Origin: $1" "$BASE/ws" 2>/dev/null || true
}

# container_for NODE_ID -> container id
#
# Node ids are either set explicitly (seed) or derived from the container name
# by the entrypoint, or fall back to the hostname. Match any of the three.
container_for() {
	local id="$1" c name host envid
	for c in $("${COMPOSE[@]}" ps -q); do
		name="$(docker inspect -f '{{.Name}}' "$c")"
		host="$(docker inspect -f '{{.Config.Hostname}}' "$c")"
		envid="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$c" | sed -n 's/^SWARM_NODE_ID=//p')"
		if [ "${name#/}" = "$id" ] || [ "$host" = "$id" ] || [ "$envid" = "$id" ]; then
			echo "$c"
			return 0
		fi
	done
	return 1
}

# container_state CID -> "<status> <exit code> <restart count>"
container_state() {
	docker inspect -f '{{.State.Status}} {{.State.ExitCode}} {{.RestartCount}}' "$1"
}

# chaos_kill_worker SKIP_IDS -> sets KILLED_WORKER
#
# A CHAOS kill must look like a clean exit: code 0, which restart: on-failure
# leaves alone, so the drone stays dead and the failover stays visible. A
# non-zero exit would be restarted (status "restarting", or a higher
# RestartCount), and this step fails.
KILLED_WORKER=""
chaos_kill_worker() {
	local skip="$1" worker cid deadline st status code restarts restarts2 status2 code2 nstate nconn
	worker="$(state | q pick_worker "$skip")" || fail "chaos kill: could not read /api/state"
	[ -n "$worker" ] || fail "chaos kill: no connected worker to kill"
	cid="$(container_for "$worker")" || fail "chaos kill: no container found for worker $worker"
	read -r _ _ restarts < <(container_state "$cid")
	log "chaos kill: POST /api/chaos kill $worker (container ${cid:0:12})"
	post /api/chaos "$(printf '{"node":"%s","action":"kill"}' "$worker")" >/dev/null ||
		fail "chaos kill: POST /api/chaos failed"

	deadline=$((SECONDS + 15))
	st=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		st="$(container_state "$cid")" || fail "chaos kill: docker inspect failed"
		case "$st" in exited\ *) break ;; esac
		sleep 1
	done
	read -r status code restarts2 <<<"$st"
	[ "$status" = exited ] || fail "chaos kill: $worker did not exit within 15s (status: $st)"
	[ "$code" = 0 ] || fail "chaos kill: $worker exited with code $code, want 0"
	[ "$restarts2" = "$restarts" ] ||
		fail "chaos kill: $worker was restarted before it stayed down (restarts $restarts -> $restarts2)"

	log "chaos kill: $worker exited with code 0; checking it stays down for ${KILL_WAIT}s"
	sleep "$KILL_WAIT"
	read -r status2 code2 restarts2 < <(container_state "$cid")
	if [ "$status2" != exited ] || [ "$restarts2" != "$restarts" ]; then
		fail "chaos kill: $worker came back (status $status2, exit $code2, restarts $restarts -> $restarts2); want it to stay down"
	fi

	read -r nstate nconn < <(state | q node_state "$worker") || fail "chaos kill: could not read /api/state"
	if [ "$nstate" != killed ] || [ "$nconn" != false ]; then
		fail "chaos kill: /api/state shows $worker as state=$nstate connected=$nconn, want killed/false"
	fi
	log "chaos kill: $worker is down, was not restarted, and is shown as killed"
	KILLED_WORKER="$worker"
}

# sim_update SKIP_IDS
#
# POST /api/sim changes per_unit_ms; GET /api/sim must then show it with a
# newer version, and every connected drone must report that version.
sim_update() {
	local skip="$1" before resp v ver got gotver deadline applied live last=""
	before="$(curl -fsS --max-time 3 "$BASE/api/sim")" || fail "sim: GET /api/sim failed"
	ver="$(printf '%s' "$before" | q sim_field version)"
	log "sim: before: per_unit_ms=$(printf '%s' "$before" | q sim_field per_unit_ms) version=$ver"

	resp="$(post /api/sim "$(printf '{"per_unit_ms":%s}' "$SIM_PER_UNIT")")" || fail "sim: POST /api/sim failed"
	v="$(printf '%s' "$resp" | q sim_field version)"
	num_cmp "$(printf '%s' "$resp" | q sim_field per_unit_ms)" == "$SIM_PER_UNIT" ||
		fail "sim: POST /api/sim reply does not carry per_unit_ms=$SIM_PER_UNIT: $resp"
	num_cmp "$v" ">" "$ver" || fail "sim: version did not increase ($ver -> $v)"

	got="$(curl -fsS --max-time 3 "$BASE/api/sim")" || fail "sim: GET /api/sim failed"
	gotver="$(printf '%s' "$got" | q sim_field version)"
	num_cmp "$(printf '%s' "$got" | q sim_field per_unit_ms)" == "$SIM_PER_UNIT" ||
		fail "sim: GET /api/sim does not reflect per_unit_ms=$SIM_PER_UNIT: $got"
	num_cmp "$gotver" ">=" "$v" || fail "sim: GET /api/sim version $gotver is older than the POST reply's $v"
	log "sim: per_unit_ms=$SIM_PER_UNIT is in force at the control center (version $gotver)"

	deadline=$((SECONDS + 15))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if read -r applied live < <(state | q sim_applied "$gotver"); then
			last="$applied/$live"
			if [ "$live" -gt 0 ] && [ "$applied" -eq "$live" ]; then
				log "sim: every connected drone reports sim_version >= $gotver ($last)"
				return 0
			fi
		fi
		sleep 1
	done
	fail "sim: drones did not apply version $gotver within 15s (applied: $last)"
}

# ------------------------------------------------------------------ main

log "starting project $PROJECT: seed + $NODES nodes, dashboard on $BASE"
build=(--build)
[ "${E2E_NO_BUILD:-0}" = "1" ] && build=()
"${COMPOSE[@]}" up -d "${build[@]}" --scale "node=$NODES" ||
	fail "docker compose up failed"

check_proxy
wait_healthy "" "$TOTAL" "$READY_TIMEOUT" "converge"
run_tasks "before"

leaders="$(state | q leaders)" || fail "could not read leaders from /api/state"
victim="${leaders%% *}"
[ -n "$victim" ] || fail "no leader to kill"
cid="$(container_for "$victim")" || fail "no container found for leader $victim"
log "killing leader $victim (container ${cid:0:12}); current leaders: $leaders"
docker kill "$cid" >/dev/null || fail "docker kill $victim failed"

# The victim is excluded by id: the control center keeps it listed as
# disconnected for a while, and Docker does not restart a container killed
# from the host, whatever its restart policy.
wait_healthy "$victim" "$((TOTAL - 1))" "$HEAL_TIMEOUT" "failover"
log "leaders after failover: $(state | q leaders || echo unknown)"
run_tasks "after"

chaos_kill_worker "$victim"
wait_healthy "$victim $KILLED_WORKER" "$((TOTAL - 2))" "$HEAL_TIMEOUT" "after chaos kill"
log "leaders after chaos kill: $(state | q leaders || echo unknown)"

sim_update "$victim $KILLED_WORKER"
# A new latency model can move leadership; the swarm must settle again.
wait_healthy "$victim $KILLED_WORKER" "$((TOTAL - 2))" "$HEAL_TIMEOUT" "after sim change"

log "PASS: swarm healed after losing leader $victim and worker $KILLED_WORKER, and applied a sim change"
