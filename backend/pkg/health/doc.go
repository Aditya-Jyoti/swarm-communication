// Package health defines the pluggable health-evaluation contract used to rank peers
// for leadership and to choose cluster affinity.
//
// The point of this package is substitutability: LatencyHealthStrategy ships as the
// default, but CPU, memory, packet-loss or composite strategies must drop in without
// pkg/cluster learning anything new. Cluster logic therefore compares scores and never
// interprets them. Any code that assumes "score is milliseconds" is a bug against this
// contract.
//
// The exact contract — score direction, normalisation, probe-failure semantics, and
// context propagation — is settled in Phase 2. See docs/WORKLOG.md.
package health
