// Package telemetry produces the node-state snapshots the dashboard renders: roles,
// cluster membership, health scores, and transition events.
//
// It is separated from pkg/cluster so that display concerns never leak into the state
// machine. Telemetry reads cluster state concurrently with the heartbeat goroutines
// that write it, which makes the synchronisation discipline here load-bearing rather
// than incidental — see docs/concepts/csp-channels-and-memory-model.md.
package telemetry
