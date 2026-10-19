// Package mavlink is a minimal, dependency-free MAVLink v2 implementation: the
// framing, the CRC, codecs for the messages the swarm bridge speaks, the cube
// -> WGS-84 projection, and sysid bookkeeping.
//
// It is the protocol core only. Nothing here opens a socket, reads a clock or
// knows what a cluster is; the bridge that wires this into the Control Center
// lives in pkg/controlcenter. That split is what lets every byte of the wire
// format be tested against an independent implementation (see
// conformance_test.go) with no network in the way.
//
// # Layout
//
//	dialect.go      generated from gen/dialect/common.xml -- messages, enums, CRC_EXTRA
//	message.go      the Message interface, the registry, error classification
//	frame.go        v2 (and lenient v1) framing, CRC-16/MCRF4XX, stream parser
//	project.go      cube units <-> WGS-84 in MAVLink integer units
//	vehicles.go     node id -> system id assignment
//	bridge.go       the UDP bridge: goroutines, streams, parameters, commands
//	mission.go      the mission protocol and swarm-level mission execution
//	mover.go        simulated motion: flight, idle drift, idle patrol
//	health.go       synthesised sensor health, GPS, battery, home position
//	instruments.go  synthesised VFR_HUD and ATTITUDE
//
// # What is real and what is synthesised
//
// This package presents the swarm to a ground station, and a ground station
// draws everything it is told as a measurement. Most of what it is told here is
// not one. Read this before believing a number on a QGroundControl screen.
//
// REAL -- swarm state, which the Control Center owns and the bridge only reads
// or writes:
//
//   - Position. The cube position of every node, projected to WGS-84. The
//     bridge WRITES it when a mission or an operator command flies a vehicle,
//     and that write feeds the latency model and therefore the election.
//   - Role. HEARTBEAT.custom_mode is 1 for an elected leader, 0 for a worker.
//   - Liveness. A vehicle heartbeats exactly while its node is connected, so a
//     killed node really does look like a lost link.
//   - Dropped frames and ledger size, in SYS_STATUS's errors_comm, errors_count1,
//     drop_rate_comm and load.
//   - Degraded. The swarm's own chaos-delay flag, which withdraws one sensor
//     health bit and one GPS fix level.
//   - Mission progress. MISSION_CURRENT and MISSION_ITEM_REACHED reflect real
//     assignment and arrival, and every arrival submits a real swarm task.
//
// SYNTHESISED -- there is no airframe, so these are invented to the minimum
// fidelity a ground station needs in order to show the swarm at all:
//
//   - Sensor health bits. Twelve sensors are claimed present, enabled and
//     healthy. None of them exist. See sensorsPresent.
//   - GPS fix type, satellite count and dilution of position. The POSITION is
//     real; the receiver reporting it is not.
//   - Battery: voltage, current, consumed charge, percentage, cell voltages,
//     charge state. A model, drained by the mover's own speed. See health.go.
//   - Attitude: roll, pitch, yaw and their rates, read backwards off the
//     motion the mover already performed. Kinematic, not dynamic.
//   - Airspeed, which equals groundspeed because there is no wind, and the
//     throttle percentage.
//   - AUTOPILOT_VERSION's firmware numbers and uid.
//   - Arming. A flag, not an airframe being energised -- though it does gate
//     mission execution.
//
// Nothing here adds noise to make a value look convincing. Each synthetic
// quantity is a function of real state where it can be (the GPS position is the
// same projection the fused position uses; the battery drains faster when the
// vehicle flies) and a stated constant where it cannot be. Section 9 of
// docs/architecture/mavlink-bridge.md says the same thing for an operator.
//
// # Reading order, if you are here to learn the protocol
//
// Start at frame.go. A MAVLink frame is a ten-byte header, up to 255 bytes of
// payload and a two-byte checksum, and the three interesting decisions are all
// visible there: why the payload may be shorter than the message (truncation),
// why the checksum covers a byte that is never transmitted (CRC_EXTRA), and why
// a decoder has to be able to find a frame boundary in a stream of garbage
// (resynchronisation).
//
// Then read the header of gen/main.go, which explains why the field order in
// dialect.go is not the field order in the XML.
//
// # What is deliberately missing
//
//   - Signing (MAVLink 2 message authentication). The bridge runs on a lab
//     network behind Docker; signing would add a key-management story and a
//     13-byte tail to every frame for no threat this is defending against.
//     Incoming signed frames are still parsed -- the signature is skipped, not
//     rejected, because refusing them would strand a GCS that has it switched on.
//   - Every message common.xml defines that the bridge does not use. See
//     wantMessages in gen/main.go.
package mavlink

// Regenerate dialect.go with `go generate ./pkg/mavlink/`. See gen/main.go.
//go:generate go run ./gen/main.go -out dialect.go ./gen/dialect/common.xml
