package controlcenter

import (
	"errors"
	"fmt"
	"net"
	"time"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// Why not network.Pool + network.Listen for the node side?
//
// A Pool answers every HELLO with HELLO_ACK.KnownPeers = every address it has
// learned, and it learns every connecting node's advertise address. Used here,
// the CC would hand each new node the addresses of all the others -- gossip
// the contract forbids ("the CC never advertises node addresses to nodes"),
// and a second discovery path nobody designed for. PoolConfig has no switch
// for that, so the CC runs the acceptor half of the handshake itself (below,
// about thirty lines) and then hands the socket to network.NewConn, which is
// where the framing, deadlines and queueing live anyway.
//
// The tie-break a Pool applies is not needed either: nodes never dial each
// other through the CC, so a second connection from the same NodeID is always
// a newer process (or a redial after a half-open link), and the newest wins.

// nodeLink is one established node connection.
//
// conn is written once by serveNode before the connUp op is posted, and read
// only by hub ops, so the channel send is the happens-before edge that makes
// the plain field safe.
type nodeLink struct {
	id     protocol.NodeID
	remote string
	conn   *network.Conn
}

// acceptLoop feeds sockets to serveNode until the listener is closed.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	delay := time.Duration(0)
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Descriptor exhaustion and aborted clients are the realistic
			// cases; neither is fixed by retrying in a tight loop.
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else if delay *= 2; delay > time.Second {
				delay = time.Second
			}
			s.log.Warn("accept failed; backing off", "err", err, "delay", delay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-s.hubDone:
				timer.Stop()
				return
			}
			continue
		}
		delay = 0
		if !s.track(raw) {
			_ = raw.Close()
			return
		}
		// The handshake runs on its own goroutine so a client that connects
		// and says nothing cannot stall the accept loop.
		go s.serveNode(raw)
	}
}

// serveNode owns one node socket for its whole life.
func (s *Server) serveNode(raw net.Conn) {
	defer s.untrack(raw)
	remote := raw.RemoteAddr().String()

	id, err := s.acceptHello(raw)
	if err != nil {
		_ = raw.Close()
		s.log.Warn("node handshake failed", "remote", remote, "err", err)
		return
	}

	link := &nodeLink{id: id, remote: remote}
	// ready holds back the frame handler until connUp is queued. The node
	// sends TELEMETRY the instant its handshake completes, and the reader
	// goroutine NewConn starts could otherwise deliver it to the hub before
	// the hub knows the link exists -- and the hub drops frames from links it
	// does not know.
	ready := make(chan struct{})
	handler := func(_ protocol.NodeID, env *protocol.Envelope) {
		<-ready
		switch env.Type {
		case protocol.TypeTelemetry, protocol.TypeTaskResult, protocol.TypePong:
			s.post(func(h *hub) { h.onFrame(link, env) })
		}
	}
	link.conn = network.NewConn(id, raw, handler, s.cfg.Conn)
	s.post(func(h *hub) { h.onConnUp(link) })
	close(ready)

	<-link.conn.Done()
	disp, cerr := link.conn.Disposition()
	s.post(func(h *hub) { h.onConnDown(link, disp, cerr) })
}

// acceptHello runs the acceptor side of the HELLO exchange under one deadline.
func (s *Server) acceptHello(raw net.Conn) (protocol.NodeID, error) {
	wrap := func(err error) error {
		return fmt.Errorf("controlcenter: accept handshake from %s: %w", raw.RemoteAddr(), err)
	}
	// One absolute deadline for the whole exchange: a per-read timeout would
	// let a peer trickling one byte at a time hold the goroutine for ever.
	if err := raw.SetDeadline(s.cfg.Now().Add(s.cfg.HandshakeTimeout)); err != nil {
		return "", wrap(err)
	}
	enc := protocol.NewEncoder(raw)
	hello, err := protocol.NewDecoder(raw).ReadFrame()
	if err != nil {
		return "", wrap(err)
	}
	reject := func(reason string) (protocol.NodeID, error) {
		ack, aerr := protocol.NewReply(hello, protocol.TypeHelloAck, NodeID, protocol.HelloAckPayload{Accepted: false, Reason: reason})
		if aerr == nil {
			_ = enc.WriteEnvelope(ack) // best effort: we close either way
		}
		return "", wrap(fmt.Errorf("%w: %s", network.ErrHandshakeRejected, reason))
	}
	if hello.Type != protocol.TypeHello {
		return reject(fmt.Sprintf("expected %s, got %s", protocol.TypeHello, hello.Type))
	}
	if _, err := protocol.PayloadOf[protocol.HelloPayload](hello); err != nil {
		return reject(err.Error())
	}
	switch hello.From {
	case "":
		return reject("HELLO carries an empty node id")
	case NodeID:
		return reject(fmt.Sprintf("node id %q is reserved for the control center", NodeID))
	}

	// Accepted, and deliberately empty: no Advertise (nobody should dial the
	// CC's node port by a name learned here) and no KnownPeers (see the top
	// of this file).
	ack, err := protocol.NewReply(hello, protocol.TypeHelloAck, NodeID, protocol.HelloAckPayload{Accepted: true})
	if err != nil {
		return "", wrap(err)
	}
	if err := enc.WriteEnvelope(ack); err != nil {
		return "", wrap(err)
	}
	// Clear the handshake deadline; the Conn sets its own per frame.
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return "", wrap(err)
	}
	return hello.From, nil
}
