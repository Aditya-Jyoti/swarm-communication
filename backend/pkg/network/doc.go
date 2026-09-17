// Package network owns the transport: the TCP listener, the dialer, the per-peer
// connection pool, and the read/write loops that move frames across sockets.
//
// It moves opaque frames and has no opinion about their contents. Message meaning
// belongs to pkg/protocol and above; everything here is about sockets, deadlines,
// backpressure, and the lifecycle of a connection that may die at any moment.
//
// Two invariants this package exists to enforce:
//
//   - No unbounded read. Every read carries a deadline, because a peer that has been
//     SIGKILLed leaves a socket that never returns an error on its own.
//   - No assumption that a Read returns a whole message, or that a Write sends one.
//
// See docs/concepts/tcp-sockets-and-the-kernel.md and docs/concepts/go-netpoller.md.
package network
