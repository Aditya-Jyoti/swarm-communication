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
//	dialect.go   generated from gen/dialect/common.xml -- messages, enums, CRC_EXTRA
//	message.go   the Message interface, the registry, error classification
//	frame.go     v2 (and lenient v1) framing, CRC-16/MCRF4XX, stream parser
//	project.go   cube units <-> WGS-84 in MAVLink integer units
//	vehicles.go  node id -> system id assignment
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
