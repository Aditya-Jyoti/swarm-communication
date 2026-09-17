// Package protocol defines the swarm's wire format: the message envelope, the
// message type taxonomy, and the framing codec that turns a TCP byte stream back
// into discrete messages.
//
// This package is the root of the dependency graph. It imports nothing else from
// this module, and nothing in it may depend on cluster roles, health semantics, or
// connection management. If a type here needs to know what a leader is, it is in
// the wrong package.
//
// The serialisation and framing strategy is a Phase 2 decision; see
// docs/concepts/stream-framing.md for the trade-offs and docs/WORKLOG.md for the
// decision record.
package protocol
