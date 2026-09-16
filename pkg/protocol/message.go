package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// CurrentVersion is the wire-protocol version this build speaks. It is carried in
// every Envelope so that a decoder can reject a peer it cannot parse *before* it
// tries to interpret the payload.
//
// It is deliberately a separate axis from MessageType. Adding a message type is a
// backward-compatible change (see the forward-compatibility policy on MessageType);
// changing the *envelope* layout is not, and only the latter bumps this number.
const CurrentVersion uint8 = 1

// MessageType is the discriminator that tells a receiver how to interpret
// Envelope.Payload. It is a string rather than an integer enum on purpose: the
// frames are JSON, the whole point of that choice was that a human with
// DumpStream (or a packet capture) can read them, and "HEARTBEAT" is legible
// where 0x07 is not. The cost is a few bytes per frame at heartbeat rates, which
// is nothing next to the JSON object that follows it.
//
// # Forward-compatibility policy
//
// An *unknown MessageType is not an error*. Discovery is gossip-based, so a node
// running a newer build will talk to a node running an older one, and the older
// node must survive hearing about a message it has never been taught. The
// required behaviour is: decode the envelope, observe that Type.Valid() is false,
// log it once, drop the message, keep the connection.
//
// A *malformed envelope* is a different thing entirely and IS fatal to the
// connection. The distinction is where the damage stops:
//
//   - Unknown type: the frame decoded cleanly. The stream is still exactly on a
//     frame boundary, the next ReadFull lands on the next length header, and the
//     only thing lost is one message we could not act on. Recoverable.
//   - Malformed envelope / bad length / JSON that will not parse: we no longer
//     know where this frame ended, so we no longer know where the next one begins.
//     Every subsequent uint32 we read is plausible garbage. Unrecoverable — see
//     the resynchronisation note in framing.go.
type MessageType string

// ---------------------------------------------------------------------------
// Control plane.
//
// Node-to-node coordination traffic: discovery, health probing, election and
// liveness. These messages are small, near-constant-rate, and latency-sensitive —
// a heartbeat that arrives 200 ms late is indistinguishable from a node that died,
// so anything that delays this class of message (Nagle, a task payload queued
// ahead of it, a stalled writer) is a correctness problem, not a performance one.
// See docs/architecture/overview.md for why the two planes share a mesh but not a
// priority.
// ---------------------------------------------------------------------------
const (
	// TypeHello opens a connection and announces identity and advertised address.
	// Carries HelloPayload.
	TypeHello MessageType = "HELLO"
	// TypeHelloAck answers a HELLO, accepting or rejecting the peer and seeding it
	// with a membership view. Carries HelloAckPayload.
	TypeHelloAck MessageType = "HELLO_ACK"

	// TypePing is a health probe. Carries PingPayload. The RTT it measures is
	// computed by the *sender*, locally, from a monotonic clock — never from the
	// timestamps in the envelopes. See Envelope.SentAtUnixNano.
	TypePing MessageType = "PING"
	// TypePong echoes a PING's nonce back unchanged. Carries PongPayload.
	TypePong MessageType = "PONG"

	// TypeHeartbeat is a leader's liveness beat to its workers. Carries
	// HeartbeatPayload. K consecutive misses trigger failover in Phase 4.
	TypeHeartbeat MessageType = "HEARTBEAT"
	// TypeHeartbeatAck is a worker's acknowledgement of a heartbeat, which is how a
	// leader learns its cluster is still attached. Carries HeartbeatAckPayload.
	TypeHeartbeatAck MessageType = "HEARTBEAT_ACK"

	// TypeMembershipDelta gossips an incremental change to the membership view
	// (peer joined, peer suspected, peer evicted) rather than a full snapshot.
	TypeMembershipDelta MessageType = "MEMBERSHIP_DELTA"
	// TypeElectionResult announces the outcome of an election round.
	TypeElectionResult MessageType = "ELECTION_RESULT"

	// TypeJoinCluster is a worker asking a specific leader for affinity attachment
	// after it has probed every leader and picked the cheapest.
	TypeJoinCluster MessageType = "JOIN_CLUSTER"
	// TypeJoinAck is the leader's answer to a JOIN_CLUSTER.
	TypeJoinAck MessageType = "JOIN_ACK"

	// TypeLeave is a voluntary, graceful departure. It is an optimisation, never a
	// requirement: the failure detector must reach the same conclusion without it,
	// because a SIGKILLed container never gets to send one.
	TypeLeave MessageType = "LEAVE"
)

// ---------------------------------------------------------------------------
// Data plane.
//
// Work and observability traffic: bursty, larger, throughput-sensitive, and
// tolerant of tens of milliseconds of delay in a way the control plane is not. A
// dropped telemetry sample costs a gap in a dashboard; a dropped heartbeat costs a
// spurious failover. That asymmetry is why the two planes are labelled here rather
// than left implicit — the load-shedding policy in later phases keys off it.
// ---------------------------------------------------------------------------
const (
	// TypeTask is work injected at the Control Center and broadcast down through
	// leaders to workers.
	TypeTask MessageType = "TASK"
	// TypeTaskResult is a worker's completion report, aggregated back up.
	TypeTaskResult MessageType = "TASK_RESULT"
	// TypeTelemetry is a periodic node/cluster snapshot destined for the dashboard.
	// This is the largest legitimate message class and therefore the one that sets
	// the floor under MaxFrameSize.
	TypeTelemetry MessageType = "TELEMETRY"
)

// knownTypes is the set of MessageTypes this build understands.
//
// It is a package-level map that is written exactly once, by this composite
// literal, at init time and never mutated afterwards. That immutability is the
// entire synchronisation story: concurrent reads of a map that is never written
// after initialisation are safe, and no mutex is required. Adding a runtime
// registration API would destroy that property and is therefore deliberately
// absent.
var knownTypes = map[MessageType]plane{
	TypeHello:           planeControl,
	TypeHelloAck:        planeControl,
	TypePing:            planeControl,
	TypePong:            planeControl,
	TypeHeartbeat:       planeControl,
	TypeHeartbeatAck:    planeControl,
	TypeMembershipDelta: planeControl,
	TypeElectionResult:  planeControl,
	TypeJoinCluster:     planeControl,
	TypeJoinAck:         planeControl,
	TypeLeave:           planeControl,

	TypeTask:       planeData,
	TypeTaskResult: planeData,
	TypeTelemetry:  planeData,
}

type plane uint8

const (
	planeUnknown plane = iota
	planeControl
	planeData
)

// Valid reports whether this build knows how to interpret t.
//
// Callers must treat a false result as "ignore this message and continue", not as
// a connection error. See the forward-compatibility policy on MessageType.
func (t MessageType) Valid() bool {
	_, ok := knownTypes[t]
	return ok
}

// IsControlPlane reports whether t belongs to the latency-sensitive coordination
// plane. An unknown type belongs to neither plane.
func (t MessageType) IsControlPlane() bool { return knownTypes[t] == planeControl }

// IsDataPlane reports whether t belongs to the throughput-sensitive work plane. An
// unknown type belongs to neither plane.
func (t MessageType) IsDataPlane() bool { return knownTypes[t] == planeData }

func (t MessageType) String() string { return string(t) }

// Envelope is the single outermost structure of every frame on the wire. Exactly
// one Envelope is encoded per frame; there is no batching, because batching would
// couple the control plane's latency to the data plane's buffering.
type Envelope struct {
	// Version is the wire-protocol version of the sender. A receiver that does not
	// recognise it must drop the connection rather than guess at the layout.
	Version uint8 `json:"version"`

	// Type discriminates the Payload. May be unknown to this build; see
	// MessageType's forward-compatibility policy.
	Type MessageType `json:"type"`

	// From is the sender's stable NodeID — not its address. A node that is killed
	// and restarted by the chaos controls keeps its NodeID and gets a new IP.
	From NodeID `json:"from"`

	// To is the intended recipient. The empty string means broadcast: deliver to
	// every peer on this link's fan-out. It is addressed by NodeID rather than
	// NodeAddress for the same reason From is.
	To NodeID `json:"to,omitempty"`

	// ID correlates a request with its response (PING→PONG, JOIN_CLUSTER→JOIN_ACK).
	// A response echoes the request's ID; it does not mint a new one. Without this,
	// a node with several probes in flight on one connection cannot tell which PONG
	// answers which PING, and its RTT measurements silently become wrong rather
	// than absent.
	ID string `json:"id,omitempty"`

	// SentAtUnixNano is the sender's WALL-CLOCK reading at the moment of encoding.
	//
	// READ THIS BEFORE USING IT. It must NEVER be used to compute a duration by
	// subtracting one node's timestamp from another's. There is no clock
	// synchronisation in this swarm — no NTP inside the containers, nothing. Two
	// nodes' wall clocks can differ by seconds, and each can be stepped backwards
	// at any moment by the host's own time discipline. Subtracting across nodes
	// therefore yields a number that is not merely imprecise but can be negative,
	// and a latency-derived election that ingests it will confidently promote the
	// node with the furthest-skewed clock.
	//
	// Round-trip time is measured ONLY by the sender of a PING, locally: read a
	// monotonic clock before the write, read it again when the matching PONG
	// arrives (matched by Envelope.ID), subtract. That subtraction is valid because
	// both readings come from one monotonic source on one machine, which cannot go
	// backwards and is immune to NTP steps.
	//
	// What this field is legitimately for: display in the dashboard, and as a weak
	// ordering hint between messages from the SAME sender. That is all.
	SentAtUnixNano int64 `json:"sent_at_unix_nano"`

	// Payload is the type-specific body, left undecoded until the receiver has
	// dispatched on Type. json.RawMessage is a []byte that survives a round trip
	// through the JSON codec verbatim, which is what lets an unknown message type
	// be dropped without ever being parsed.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ---------------------------------------------------------------------------
// Typed payloads.
// ---------------------------------------------------------------------------

// HelloPayload opens a connection: who I am and where to reach me.
type HelloPayload struct {
	// Advertise is the address peers should dial to reach this node. It is sent
	// explicitly rather than inferred from the accepted connection's remote address,
	// because that remote address carries an ephemeral source port, not the
	// listening port.
	Advertise NodeAddress `json:"advertise"`

	// Incarnation distinguishes this process from a previous process with the same
	// NodeID. A restarted node is a *new* incarnation, and membership state carried
	// about the old one must not be applied to it. It is a wall-clock start time
	// used purely as a monotonically-increasing-per-node tag, never as a duration.
	Incarnation int64 `json:"incarnation"`

	// KnownPeers seeds gossip: the addresses this node already believes are in the
	// swarm. May be empty for a node that has only just contacted its seed.
	KnownPeers []NodeAddress `json:"known_peers,omitempty"`
}

// HelloAckPayload answers a HelloPayload.
type HelloAckPayload struct {
	Accepted bool `json:"accepted"`
	// Reason is populated only when Accepted is false. A rejection with an empty
	// reason is a debugging dead end during a chaos run, so senders should always
	// fill it.
	Reason      string        `json:"reason,omitempty"`
	Advertise   NodeAddress   `json:"advertise"`
	Incarnation int64         `json:"incarnation"`
	KnownPeers  []NodeAddress `json:"known_peers,omitempty"`
}

// PingPayload is a health probe.
type PingPayload struct {
	// Nonce is echoed verbatim by the PONG. It is belt-and-braces alongside
	// Envelope.ID: it makes a replayed or mis-routed PONG detectable at the payload
	// level, so a stale response cannot be credited as a fast one.
	Nonce uint64 `json:"nonce"`
	// Seq is the probe sequence number within this link, for loss accounting.
	Seq uint64 `json:"seq"`
}

// PongPayload answers a PingPayload, echoing its Nonce and Seq unchanged. It
// deliberately carries no timestamp: any timestamp here would be a cross-node wall
// clock reading and therefore useless for RTT. See Envelope.SentAtUnixNano.
type PongPayload struct {
	Nonce uint64 `json:"nonce"`
	Seq   uint64 `json:"seq"`
}

// HeartbeatPayload is a leader's liveness beat to an attached worker.
type HeartbeatPayload struct {
	// Seq increments per beat on this link. A worker counts *gaps* in Seq, not
	// merely elapsed silence, which distinguishes "the leader is slow" from "the
	// leader stopped".
	Seq uint64 `json:"seq"`
	// Term is the election term this leader believes it holds. A worker receiving a
	// heartbeat from an older term is looking at a leader that has not yet learned
	// it was replaced — a split-brain symptom, and the reason the field exists.
	Term uint64 `json:"term"`
	// LeaderID is the sender's claimed leadership identity. Normally equal to
	// Envelope.From; carried separately so a relayed beat stays attributable.
	LeaderID NodeID `json:"leader_id"`
	// ClusterSize is the leader's current view of its attached worker count, for
	// telemetry only. It is a hint, not an authority.
	ClusterSize int `json:"cluster_size"`
}

// HeartbeatAckPayload acknowledges a HeartbeatPayload.
type HeartbeatAckPayload struct {
	// Seq echoes the acknowledged beat's Seq.
	Seq uint64 `json:"seq"`
	// Term echoes the term the worker believes is current, so a leader can detect
	// that it has been superseded.
	Term uint64 `json:"term"`
	// ObservedLeader is the leader this worker currently considers itself attached
	// to. If it is not the sender, the worker has already re-homed and the sender
	// should stop beating at it.
	ObservedLeader NodeID `json:"observed_leader"`
}

// ---------------------------------------------------------------------------
// Envelope construction and payload access.
// ---------------------------------------------------------------------------

// NewEnvelope builds an Envelope of type t addressed from -> to, marshalling
// payload into the body. Pass a nil payload for message types with no body.
//
// It stamps SentAtUnixNano and mints a fresh correlation ID. A response must NOT
// be built with this function if it needs to echo a request's ID — build it and
// then overwrite ID, or use NewReply.
func NewEnvelope(t MessageType, from, to NodeID, payload any) (*Envelope, error) {
	e := &Envelope{
		Version:        CurrentVersion,
		Type:           t,
		From:           from,
		To:             to,
		ID:             NewMessageID(),
		SentAtUnixNano: nowUnixNano(),
	}
	if payload != nil {
		if err := SetPayload(e, payload); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// NewReply builds a response envelope that echoes req's correlation ID, which is
// what makes request/response matching work when several requests are in flight on
// one connection. The reply is addressed back to req.From.
func NewReply(req *Envelope, t MessageType, from NodeID, payload any) (*Envelope, error) {
	e, err := NewEnvelope(t, from, req.From, payload)
	if err != nil {
		return nil, err
	}
	e.ID = req.ID
	return e, nil
}

// SetPayload marshals v into e.Payload.
//
// It is not generic because the write side gains nothing from it: v is
// immediately erased to `any` by json.Marshal regardless.
func SetPayload(e *Envelope, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("protocol: marshal %s payload: %w", e.Type, err)
	}
	e.Payload = b
	return nil
}

// PayloadOf decodes an Envelope's body into T.
//
// This one IS generic, because here the type parameter does real work: it lets the
// caller write `PayloadOf[HeartbeatPayload](env)` and get a value back, instead of
// declaring a variable and passing its address, which is the pattern that invites
// passing the wrong type's address to the wrong message's handler.
//
// A decode failure here is a malformed payload from a peer claiming a type it did
// not actually send. It is wrapped with ErrMalformedFrame so callers can classify
// it with the same check they use for framing-level garbage; both mean "this peer
// is not speaking our protocol" and both should drop the connection.
func PayloadOf[T any](e *Envelope) (T, error) {
	var v T
	if len(e.Payload) == 0 {
		return v, fmt.Errorf("protocol: %s envelope has no payload: %w", e.Type, ErrMalformedFrame)
	}
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return v, fmt.Errorf("protocol: unmarshal %s payload: %w: %v", e.Type, ErrMalformedFrame, err)
	}
	return v, nil
}

// NewMessageID mints a correlation ID: 16 hex characters of cryptographic
// randomness.
//
// crypto/rand rather than math/rand because these IDs are compared for equality
// across a network by nodes that started simultaneously from an identical image.
// math/rand's global source would hand several containers the same sequence unless
// each is seeded distinctly, and a collision here silently mis-attributes a PONG to
// the wrong PING — corrupting a latency measurement rather than producing an error.
// 8 bytes is ample for IDs whose uniqueness only has to hold within one
// connection's in-flight window.
//
// crypto/rand.Read is documented never to return an error on any platform this
// runs on; the panic is a genuine "the machine is broken" assertion, not an
// unhandled case.
func NewMessageID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("protocol: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// nowUnixNano reads the wall clock for Envelope.SentAtUnixNano.
//
// It is a named function rather than an inline time.Now().UnixNano() so that there
// is exactly one place in the package where a wall-clock reading is taken, and so
// that place can carry this warning: calling .UnixNano() strips the monotonic
// reading that time.Now() embedded in the time.Time. That is correct here — a
// monotonic reading is meaningless once it leaves this process — and it is exactly
// why the resulting number must never be subtracted across nodes.
func nowUnixNano() int64 { return time.Now().UnixNano() }
