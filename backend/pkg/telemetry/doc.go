// Package telemetry is a node's uplink to the Control Center (CC).
//
// It has two halves:
//
//   - FromStatus turns a cluster.Status snapshot into the TELEMETRY wire payload.
//     It is a pure function: display concerns (address-keyed scores, dropping
//     NaN) live here so they never leak into the state machine.
//   - Client keeps one connection to the CC alive, sends TELEMETRY on a ticker
//     and TASK_RESULT on demand, and hands inbound TASK and CHAOS frames to
//     callbacks. The contract is docs/architecture/control-plane.md.
//
// The CC link is deliberately walled off from the mesh. The Client owns a
// private network.Pool, so nothing learned on it (the CC's address, anything in
// its HELLO_ACK) can reach the mesh pool's known-address set, and the node never
// dials a mesh address because of it. Election and heartbeats never depend on
// this package: a node with a dead CC link is a node without a dashboard, not a
// sick node.
package telemetry
