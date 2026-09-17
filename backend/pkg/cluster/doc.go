// Package cluster holds the distributed algorithm: the membership table, the dynamic
// leadership threshold, latency-affinity clustering, heartbeat monitoring, failover
// re-election, and leader-to-worker state replication.
//
// This is where the genuinely hard reasoning lives, so it is deliberately kept free of
// I/O detail: it operates on interfaces from pkg/network and pkg/health and can be
// unit-tested without opening a socket.
//
// Leadership is a state in a machine, not a start-up flag. Every node runs the same
// code and may be promoted at any time.
//
// See docs/architecture/overview.md.
package cluster
