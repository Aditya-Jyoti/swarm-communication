#!/usr/bin/env bash
# End-to-end self-healing check for swarm-net.
#
#   scripts/e2e.sh                 # 1 seed + 5 nodes
#   E2E_NODES=8 scripts/e2e.sh     # 1 seed + 8 nodes
#   E2E_KEEP=1 scripts/e2e.sh      # leave the stack running afterwards
#
# Steps:
#   1. build and start the stack under its own Compose project
#   2. wait until every node is connected to the control center, at least one
#      leader exists, and every worker is attached to a live leader
#   3. submit tasks over HTTP and wait until all are done
#   4. `docker kill` a current leader
#   5. wait until a leader other than the victim exists and every surviving
#      node is attached again
#   6. submit tasks again and wait until all are done
#
# Exits non-zero with a FAIL line (plus recent logs) on the first failed step.
# The stack is always torn down (`down -v`) on exit unless E2E_KEEP=1.
#
# Env:
#   E2E_NODES            replicas of the `node` service       (default 5)
#   E2E_PROJECT          compose project name                 (default swarm-net-e2e)
#   E2E_HTTP_PORT        host port for the control center     (default 18080)
#   E2E_READY_TIMEOUT    seconds for initial convergence      (default 120)
#   E2E_HEAL_TIMEOUT     seconds for failover to complete     (default 60)
#   E2E_TASK_TIMEOUT     seconds for a task batch to finish   (default 60)
#   E2E_TASKS            tasks per batch                      (default 5)
#   E2E_NO_BUILD=1       skip --build (reuse existing images)
#   E2E_KEEP=1           do not tear down on exit
#   E2E_JSON=python3     force the python3 JSON path (default: jq if present)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODES="${E2E_NODES:-5}"
PROJECT="${E2E_PROJECT:-swarm-net-e2e}"
PORT="${E2E_HTTP_PORT:-18080}"
READY_TIMEOUT="${E2E_READY_TIMEOUT:-120}"
HEAL_TIMEOUT="${E2E_HEAL_TIMEOUT:-60}"
TASK_TIMEOUT="${E2E_TASK_TIMEOUT:-60}"
TASKS="${E2E_TASKS:-5}"
BASE="http://127.0.0.1:${PORT}"
TOTAL=$((NODES + 1)) # + seed

export SWARM_HTTP_PORT="$PORT"
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
		# "<connected> <leaders> <unattached>" ignoring node ARG (may be empty).
		# A node is attached if it is a leader, or its leader is a connected
		# leader that is not ARG.
		health)
			jq -r --arg skip "$arg" '
				[.nodes // [] | .[] | select(.id != $skip and .connected == true)] as $live
				| [$live[] | select(.role == "leader") | .id] as $leaders
				| [$live[] | select(.role != "leader")
				           | select((.leader // "") as $l | ($leaders | map(. == $l) | any) | not)] as $loose
				| "\($live | length) \($leaders | length) \($loose | length)"'
			;;
		leaders) jq -r '[.nodes // [] | .[] | select(.connected == true and .role == "leader") | .id] | join(" ")' ;;
		task_ids) jq -r '.task_ids // [] | join(" ")' ;;
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
    live = [n for n in nodes if n.get("id") != arg and n.get("connected") is True]
    leaders = [n.get("id") for n in live if n.get("role") == "leader"]
    loose = [n for n in live if n.get("role") != "leader" and (n.get("leader") or "") not in leaders]
    print(len(live), len(leaders), len(loose))
elif name == "leaders":
    print(" ".join(n.get("id") for n in nodes if n.get("connected") is True and n.get("role") == "leader"))
elif name == "task_ids":
    print(" ".join(d.get("task_ids") or []))
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

# ------------------------------------------------------------------ steps

# wait_healthy SKIP_ID WANT_LIVE TIMEOUT LABEL
wait_healthy() {
	local skip="$1" want="$2" timeout="$3" label="$4"
	local deadline=$((SECONDS + timeout)) last="" live leaders loose s
	while [ "$SECONDS" -lt "$deadline" ]; do
		if s="$(state 2>/dev/null)" && read -r live leaders loose < <(printf '%s' "$s" | q health "$skip"); then
			last="connected=$live/$want leaders=$leaders unattached=$loose"
			if [ "$live" -ge "$want" ] && [ "$leaders" -ge 1 ] && [ "$loose" -eq 0 ]; then
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

# ------------------------------------------------------------------ main

log "starting project $PROJECT: seed + $NODES nodes, dashboard on $BASE"
build=(--build)
[ "${E2E_NO_BUILD:-0}" = "1" ] && build=()
"${COMPOSE[@]}" up -d "${build[@]}" --scale "node=$NODES" ||
	fail "docker compose up failed"

wait_healthy "" "$TOTAL" "$READY_TIMEOUT" "converge"
run_tasks "before"

leaders="$(state | q leaders)" || fail "could not read leaders from /api/state"
victim="${leaders%% *}"
[ -n "$victim" ] || fail "no leader to kill"
cid="$(container_for "$victim")" || fail "no container found for leader $victim"
log "killing leader $victim (container ${cid:0:12}); current leaders: $leaders"
docker kill "$cid" >/dev/null || fail "docker kill $victim failed"

# The victim is excluded by id: the control center keeps it listed as
# disconnected for a while, and restart: unless-stopped does not bring back a
# container killed from the host.
wait_healthy "$victim" "$((TOTAL - 1))" "$HEAL_TIMEOUT" "failover"
log "leaders after failover: $(state | q leaders || echo unknown)"
run_tasks "after"

log "PASS: swarm self-healed after losing leader $victim"
