#!/bin/sh
# Entrypoint for the swarm-node image. Its only job is to pick a readable,
# unique, dialable identity when the node runs as one replica of a scaled
# Compose service, then exec the binary so it becomes PID 1 and receives
# SIGTERM from `docker stop` directly.
#
# Why this exists: under `docker compose up --scale node=N` every replica
# shares one service definition, so SWARM_NODE_ID cannot be set per replica in
# YAML. The container hostname is the 12-char container ID. Docker's embedded
# DNS (127.0.0.11) on a user-defined bridge does resolve that ID, so the
# binary's default (id = hostname, advertise = hostname:port) already works --
# but "3f9c0a1b2c4d" is unreadable on the dashboard.
#
# The same DNS server answers a reverse (PTR) lookup of our own IP with the
# Compose container name, e.g. "swarm-net-node-3.swarm-net_swarmnet". The part
# before the first dot ("swarm-net-node-3") is unique, and it survives a
# restart because the container keeps its name. It is also one of the
# container's DNS aliases, so peers can dial it. We use it for both the id and
# the advertise host.
#
# Anything the operator set explicitly wins: the seed service sets both
# variables and this script leaves them alone. If any lookup fails we export
# nothing and the binary falls back to the hostname, which still works.
set -eu

port="${SWARM_LISTEN:-:7000}"
port="${port##*:}"

if [ -z "${SWARM_NODE_ID:-}" ] || [ -z "${SWARM_ADVERTISE:-}" ]; then
	self="$(hostname)"
	# getent reads /etc/hosts, where Docker writes our own address first.
	ip="$(getent hosts "$self" 2>/dev/null | awk 'NR==1 {print $1}')" || ip=""
	name=""
	if [ -n "$ip" ]; then
		# busybox nslookup prints "... name = <fqdn>" for a PTR answer.
		name="$(nslookup "$ip" 2>/dev/null | awk -F'name = ' 'NF>1 {print $2; exit}')" || name=""
		name="${name%%.*}"
	fi
	if [ -n "$name" ]; then
		: "${SWARM_NODE_ID:=$name}"
		: "${SWARM_ADVERTISE:=$name:$port}"
		export SWARM_NODE_ID SWARM_ADVERTISE
	fi
fi

echo "swarm-node entrypoint: id=${SWARM_NODE_ID:-<hostname>} advertise=${SWARM_ADVERTISE:-<hostname>:$port}" >&2
exec /usr/local/bin/swarm-node "$@"
