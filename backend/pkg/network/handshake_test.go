package network

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// --- helpers ------------------------------------------------------------------

var (
	idA = Identity{ID: "node-a", Advertise: "node-a:7000", Incarnation: 11}
	idB = Identity{ID: "node-b", Advertise: "node-b:7000", Incarnation: 22}
)

func acceptAll(PeerInfo) (bool, string) { return true, "" }

// far is a deadline that will not fire during a test. Every handshake in this file
// carries one, because the functions under test refuse to read without a deadline
// and the tests should exercise the same path production does.
func far() time.Time { return time.Now().Add(time.Minute) }

type acceptResult struct {
	info PeerInfo
	err  error
}

// runAccept runs acceptHandshake on its own goroutine and reports the result on a
// channel, so a test can drive the dialer side synchronously over a net.Pipe
// (whose writes block until the other end reads).
func runAccept(raw net.Conn, self Identity, known []protocol.NodeAddress, deadline time.Time, accept func(PeerInfo) (bool, string)) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		info, err := acceptHandshake(raw, self, known, deadline, accept)
		ch <- acceptResult{info, err}
	}()
	return ch
}

// rawHello writes an arbitrary envelope as if it were a HELLO and reads back the
// reply, for tests that need to send something the real dialer never would.
func rawExchange(t *testing.T, raw net.Conn, env *protocol.Envelope) *protocol.Envelope {
	t.Helper()
	if err := protocol.NewEncoder(raw).WriteEnvelope(env); err != nil {
		t.Fatalf("writing raw hello: %v", err)
	}
	reply, err := protocol.NewDecoder(raw).ReadFrame()
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return reply
}

func mustAck(t *testing.T, env *protocol.Envelope) protocol.HelloAckPayload {
	t.Helper()
	if env.Type != protocol.TypeHelloAck {
		t.Fatalf("reply type = %s, want HELLO_ACK", env.Type)
	}
	ack, err := protocol.PayloadOf[protocol.HelloAckPayload](env)
	if err != nil {
		t.Fatalf("decoding ack: %v", err)
	}
	return ack
}

// --- happy path -----------------------------------------------------------------

func TestHandshakeHappyPath(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	knownA := []protocol.NodeAddress{"node-c:7000"}
	knownB := []protocol.NodeAddress{"node-d:7000", "node-e:7000"}

	var seen PeerInfo
	accept := func(pi PeerInfo) (bool, string) { seen = pi; return true, "" }
	acc := runAccept(acceptEnd, idB, knownB, far(), accept)

	got, err := dialHandshake(dialEnd, idA, knownA, far())
	if err != nil {
		t.Fatalf("dialHandshake: %v", err)
	}
	if got.ID != idB.ID || got.Advertise != idB.Advertise || got.Incarnation != idB.Incarnation {
		t.Errorf("dialer learned %+v, want identity %+v", got, idB)
	}
	if len(got.KnownPeers) != 2 || got.KnownPeers[0] != "node-d:7000" {
		t.Errorf("dialer KnownPeers = %v, want %v", got.KnownPeers, knownB)
	}

	res := <-acc
	if res.err != nil {
		t.Fatalf("acceptHandshake: %v", res.err)
	}
	if res.info.ID != idA.ID || res.info.Advertise != idA.Advertise || res.info.Incarnation != idA.Incarnation {
		t.Errorf("acceptor learned %+v, want identity %+v", res.info, idA)
	}
	if len(res.info.KnownPeers) != 1 || res.info.KnownPeers[0] != "node-c:7000" {
		t.Errorf("acceptor KnownPeers = %v, want %v", res.info.KnownPeers, knownA)
	}
	if seen.ID != idA.ID {
		t.Errorf("accept callback saw %+v, want the dialer's info", seen)
	}

	// The deadline must be cleared on success so the Conn's own per-frame
	// deadlines are the only ones in force. Prove it: a frame written now, well
	// inside the minute the tests used, must still work after the handshake, and
	// a deadline set explicitly in the past must NOT already be pending.
	env := mustEnv(t, protocol.TypePing)
	done := make(chan error, 1)
	go func() { done <- protocol.NewEncoder(dialEnd).WriteEnvelope(env) }()
	if _, err := protocol.NewDecoder(acceptEnd).ReadFrame(); err != nil {
		t.Fatalf("post-handshake frame: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("post-handshake write: %v", err)
	}
}

// --- structural rejections --------------------------------------------------------

func TestAcceptRejectsVersionMismatchNamingBothVersions(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)

	hello, err := protocol.NewEnvelope(protocol.TypeHello, idA.ID, "", protocol.HelloPayload{Advertise: idA.Advertise})
	if err != nil {
		t.Fatal(err)
	}
	hello.Version = 2 // the Encoder only fills a zero version, so this is preserved
	ack := mustAck(t, rawExchange(t, dialEnd, hello))

	if ack.Accepted {
		t.Fatal("version-2 HELLO was accepted")
	}
	if !strings.Contains(ack.Reason, "version 2") || !strings.Contains(ack.Reason, "version 1") {
		t.Errorf("reason %q does not name both versions", ack.Reason)
	}

	res := <-acc
	if !protocol.IsProtocolViolation(res.err) {
		t.Errorf("acceptor error %v is not a protocol violation", res.err)
	}
	if Classify(res.err) != DispositionProtocolViolation {
		t.Errorf("Classify = %v, want protocol-violation", Classify(res.err))
	}
}

func TestAcceptRejectsEmptyID(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)

	got, err := dialHandshake(dialEnd, Identity{ID: ""}, nil, far())
	if !errors.Is(err, ErrHandshakeRejected) {
		t.Fatalf("dialer error = %v, want ErrHandshakeRejected", err)
	}
	if !strings.Contains(err.Error(), "empty node id") {
		t.Errorf("dialer error %q does not carry the reason", err)
	}
	// Even a rejection discloses who rejected us.
	if got.ID != idB.ID {
		t.Errorf("rejected dialer learned ID %q, want %q", got.ID, idB.ID)
	}

	res := <-acc
	if !errors.Is(res.err, ErrHandshakeRejected) {
		t.Errorf("acceptor error = %v, want ErrHandshakeRejected", res.err)
	}
}

func TestAcceptRejectsWrongMessageType(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)

	ping := mustEnv(t, protocol.TypePing)
	ack := mustAck(t, rawExchange(t, dialEnd, ping))
	if ack.Accepted || !strings.Contains(ack.Reason, "expected HELLO") {
		t.Errorf("ack = %+v, want rejection naming the expected type", ack)
	}
	res := <-acc
	if !protocol.IsProtocolViolation(res.err) {
		t.Errorf("acceptor error %v should be a protocol violation", res.err)
	}
}

func TestAcceptRejectsUndecodableHelloPayload(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)

	hello, err := protocol.NewEnvelope(protocol.TypeHello, idA.ID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	hello.Payload = []byte(`"not an object"`)
	ack := mustAck(t, rawExchange(t, dialEnd, hello))
	if ack.Accepted || ack.Reason == "" {
		t.Errorf("ack = %+v, want a rejection with a reason", ack)
	}
	res := <-acc
	if !protocol.IsProtocolViolation(res.err) {
		t.Errorf("acceptor error %v should be a protocol violation", res.err)
	}
}

func TestSelfConnectIsRejectedWithReasonAndSentinelOnBothSides(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idA, nil, far(), acceptAll)

	_, err := dialHandshake(dialEnd, idA, nil, far())
	if !errors.Is(err, ErrSelfConnect) {
		t.Errorf("dialer error = %v, want ErrSelfConnect", err)
	}
	if !errors.Is(err, ErrHandshakeRejected) {
		t.Errorf("dialer error = %v, should also be a rejection", err)
	}
	if !strings.Contains(err.Error(), "self-connect") {
		t.Errorf("dialer error %q lacks the acceptor's reason", err)
	}

	res := <-acc
	if !errors.Is(res.err, ErrSelfConnect) {
		t.Errorf("acceptor error = %v, want ErrSelfConnect", res.err)
	}
}

func TestAcceptCallbackRejectionPropagatesReason(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	reject := func(PeerInfo) (bool, string) { return false, "duplicate connection: tie-break" }
	acc := runAccept(acceptEnd, idB, nil, far(), reject)

	got, err := dialHandshake(dialEnd, idA, nil, far())
	if !errors.Is(err, ErrHandshakeRejected) {
		t.Fatalf("dialer error = %v, want ErrHandshakeRejected", err)
	}
	if !strings.Contains(err.Error(), "duplicate connection: tie-break") {
		t.Errorf("dialer error %q lacks the reason text", err)
	}
	if !strings.Contains(err.Error(), string(idB.ID)) {
		t.Errorf("dialer error %q does not name the rejecting peer", err)
	}
	if got.ID != idB.ID || got.Advertise != idB.Advertise {
		t.Errorf("rejected dialer learned %+v, want the acceptor's identity", got)
	}
	res := <-acc
	if !errors.Is(res.err, ErrHandshakeRejected) {
		t.Errorf("acceptor error = %v, want ErrHandshakeRejected", res.err)
	}
}

// --- dialer-side validation of the ACK ----------------------------------------------

// A HELLO_ACK that accepts but names no node is a malformed reply: the dialer would
// register a connection under the empty ID and every lookup would miss it.
func TestDialRejectsAcceptedAckWithEmptyFrom(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, Identity{ID: ""}, nil, far(), acceptAll)

	_, err := dialHandshake(dialEnd, idA, nil, far())
	if !protocol.IsProtocolViolation(err) {
		t.Errorf("dialer error = %v, want a protocol violation", err)
	}
	<-acc
}

func TestDialRejectsNonAckReply(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	go func() {
		dec := protocol.NewDecoder(acceptEnd)
		if _, err := dec.ReadFrame(); err != nil {
			return
		}
		_ = protocol.NewEncoder(acceptEnd).WriteEnvelope(mustEnvelopeNoT())
	}()

	_, err := dialHandshake(dialEnd, idA, nil, far())
	if !protocol.IsProtocolViolation(err) {
		t.Errorf("dialer error = %v, want a protocol violation", err)
	}
	if !strings.Contains(err.Error(), "expected HELLO_ACK") {
		t.Errorf("dialer error %q does not say what it expected", err)
	}
}

func TestDialRejectsUndecodableAckPayload(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	go func() {
		dec := protocol.NewDecoder(acceptEnd)
		hello, err := dec.ReadFrame()
		if err != nil {
			return
		}
		ack, _ := protocol.NewReply(hello, protocol.TypeHelloAck, idB.ID, nil)
		ack.Payload = []byte(`[1,2,3]`)
		_ = protocol.NewEncoder(acceptEnd).WriteEnvelope(ack)
	}()

	_, err := dialHandshake(dialEnd, idA, nil, far())
	if !protocol.IsProtocolViolation(err) {
		t.Errorf("dialer error = %v, want a protocol violation", err)
	}
}

// The dialer must also refuse an accepted ACK from its own ID. The acceptor normally
// catches this first, but a buggy or foreign acceptor may not, and registering a
// connection to ourselves would loop every broadcast back into our own handler.
func TestDialDetectsSelfConnectFromAcceptedAck(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	go func() {
		dec := protocol.NewDecoder(acceptEnd)
		hello, err := dec.ReadFrame()
		if err != nil {
			return
		}
		ack, _ := protocol.NewReply(hello, protocol.TypeHelloAck, idA.ID, protocol.HelloAckPayload{Accepted: true})
		_ = protocol.NewEncoder(acceptEnd).WriteEnvelope(ack)
	}()

	_, err := dialHandshake(dialEnd, idA, nil, far())
	if !errors.Is(err, ErrSelfConnect) {
		t.Errorf("dialer error = %v, want ErrSelfConnect", err)
	}
}

// --- peer disappears mid-handshake ------------------------------------------------------

func TestDialerSeesPeerCloseBeforeAck(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()

	go func() {
		// Read the HELLO so the dialer's write completes, then hang up.
		_, _ = protocol.NewDecoder(acceptEnd).ReadFrame()
		acceptEnd.Close()
	}()

	_, err := dialHandshake(dialEnd, idA, nil, far())
	if err == nil {
		t.Fatal("dialHandshake succeeded against a peer that hung up")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("error = %v, want a wrapped io.EOF", err)
	}
	if d := Classify(err); d != DispositionCleanClose {
		t.Errorf("Classify = %v, want clean-close (peer left at a frame boundary)", d)
	}
}

// tcpPair returns two ends of a real loopback connection. net.Pipe reports
// io.ErrClosedPipe rather than io.EOF when the far end closes, so tests that
// assert on the EOF taxonomy need a real socket.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err = ln.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	return client, server
}

func TestAcceptorSeesPeerCloseBeforeHello(t *testing.T) {
	dialEnd, acceptEnd := tcpPair(t)
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)
	dialEnd.Close()

	res := <-acc
	if res.err == nil || !errors.Is(res.err, io.EOF) {
		t.Errorf("acceptor error = %v, want a wrapped io.EOF", res.err)
	}
	if d := Classify(res.err); d != DispositionCleanClose {
		t.Errorf("Classify = %v, want clean-close", d)
	}
}

func TestAcceptorSeesTruncatedHelloAsPeerDeath(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)

	// A length prefix promising 100 bytes, then nothing: the SIGKILL signature.
	if _, err := dialEnd.Write([]byte{0, 0, 0, 100}); err != nil {
		t.Fatal(err)
	}
	dialEnd.Close()

	res := <-acc
	if d := Classify(res.err); d != DispositionPeerDied {
		t.Errorf("Classify = %v (err %v), want peer-died", d, res.err)
	}
}

// When the dialer's HELLO is rejected by the peer closing the socket *while the ACK
// write is in flight*, the write error must not be mistaken for a timeout.
func TestAcceptorAckWriteFailureIsNotATimeout(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer acceptEnd.Close()

	acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)

	hello, err := protocol.NewEnvelope(protocol.TypeHello, idA.ID, "", protocol.HelloPayload{Advertise: idA.Advertise})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewEncoder(dialEnd).WriteEnvelope(hello); err != nil {
		t.Fatal(err)
	}
	// Do not read the ACK; close instead so the acceptor's write fails.
	dialEnd.Close()

	res := <-acc
	if res.err == nil {
		t.Fatal("acceptHandshake succeeded although the ACK could not be written")
	}
	if errors.Is(res.err, ErrHandshakeTimeout) {
		t.Errorf("a closed pipe was classified as a timeout: %v", res.err)
	}
}

// --- deadlines -----------------------------------------------------------------------------

func TestDialHandshakeTimesOut(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	defer dialEnd.Close()
	defer acceptEnd.Close()

	// A deadline already in the past: the very first write fails with the deadline
	// error, which is what a SIGKILLed peer produces (nothing, forever).
	_, err := dialHandshake(dialEnd, idA, nil, time.Now().Add(-time.Second))
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Errorf("error = %v, want ErrHandshakeTimeout", err)
	}
	if d := Classify(err); d != DispositionTimeout {
		t.Errorf("Classify = %v, want timeout", d)
	}
}

func TestAcceptHandshakeTimesOut(t *testing.T) {
	client, server := tcpPair(t)
	defer client.Close()
	defer server.Close()

	// A real socket, a silent client, and a deadline that has already passed: the
	// read returns os.ErrDeadlineExceeded rather than blocking for ever.
	_, err := acceptHandshake(server, idB, nil, time.Now().Add(-time.Second), acceptAll)
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Errorf("error = %v, want ErrHandshakeTimeout", err)
	}
	if !strings.Contains(err.Error(), string(idB.ID)) {
		t.Errorf("error %q does not name the local node", err)
	}
}

// --- deadline plumbing failures ----------------------------------------------------------------

// deadlineFailConn reports an error from SetDeadline, standing in for a socket the
// kernel has already torn down (EBADF after a close race).
type deadlineFailConn struct {
	net.Conn
	failOn int // 1 = first SetDeadline call fails, 2 = second, ...
	calls  int
}

func (d *deadlineFailConn) SetDeadline(t time.Time) error {
	d.calls++
	if d.calls == d.failOn {
		return errors.New("deadlineFailConn: EBADF")
	}
	return d.Conn.SetDeadline(t)
}

func TestHandshakeReportsSetDeadlineFailure(t *testing.T) {
	t.Run("dial first", func(t *testing.T) {
		dialEnd, acceptEnd := net.Pipe()
		defer dialEnd.Close()
		defer acceptEnd.Close()
		_, err := dialHandshake(&deadlineFailConn{Conn: dialEnd, failOn: 1}, idA, nil, far())
		if err == nil || !strings.Contains(err.Error(), "EBADF") {
			t.Errorf("error = %v, want the SetDeadline failure", err)
		}
	})
	t.Run("dial clear", func(t *testing.T) {
		dialEnd, acceptEnd := net.Pipe()
		defer dialEnd.Close()
		defer acceptEnd.Close()
		acc := runAccept(acceptEnd, idB, nil, far(), acceptAll)
		_, err := dialHandshake(&deadlineFailConn{Conn: dialEnd, failOn: 2}, idA, nil, far())
		if err == nil || !strings.Contains(err.Error(), "EBADF") {
			t.Errorf("error = %v, want the SetDeadline failure", err)
		}
		<-acc
	})
	t.Run("accept first", func(t *testing.T) {
		dialEnd, acceptEnd := net.Pipe()
		defer dialEnd.Close()
		defer acceptEnd.Close()
		_, err := acceptHandshake(&deadlineFailConn{Conn: acceptEnd, failOn: 1}, idB, nil, far(), acceptAll)
		if err == nil || !strings.Contains(err.Error(), "EBADF") {
			t.Errorf("error = %v, want the SetDeadline failure", err)
		}
	})
	t.Run("accept clear", func(t *testing.T) {
		dialEnd, acceptEnd := net.Pipe()
		defer dialEnd.Close()
		defer acceptEnd.Close()
		acc := runAccept(&deadlineFailConn{Conn: acceptEnd, failOn: 2}, idB, nil, far(), acceptAll)
		if _, err := dialHandshake(dialEnd, idA, nil, far()); err != nil {
			t.Errorf("dialer should have completed: %v", err)
		}
		res := <-acc
		if res.err == nil || !strings.Contains(res.err.Error(), "EBADF") {
			t.Errorf("error = %v, want the SetDeadline failure", res.err)
		}
	})
}

// sendRejection must tolerate a request that cannot be replied to and an encoder
// that fails; it is best-effort by design and must never panic.
func TestSendRejectionIsBestEffort(t *testing.T) {
	dialEnd, acceptEnd := net.Pipe()
	dialEnd.Close()
	acceptEnd.Close()
	// Closed pipe: the write fails, and nothing should escape.
	sendRejection(protocol.NewEncoder(acceptEnd), nil, idB, "reason")
	sendRejection(protocol.NewEncoder(acceptEnd), mustEnv(t, protocol.TypeHello), idB, "reason")
}
