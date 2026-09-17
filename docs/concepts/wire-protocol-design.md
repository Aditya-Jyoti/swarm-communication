---
title: "Wire Protocol Design: Envelopes, Versions & Identity"
description: "Why every frame carries a fixed envelope with an opaque payload, why an unknown message type is survivable but an unknown version is fatal, and why nodes are named by ID, not address."
outline: deep
---

# Wire Protocol Design: Envelopes, Versions & Identity

A **wire protocol** is the agreed format of bytes between programs. A common design is an
**envelope**: a small fixed outer structure (who, what type, which version) around a payload
whose shape depends on the type. The receiver reads the envelope first, then decides how to
decode the payload.

Analogy: a postal envelope. The mail sorter reads the address and stamp. Only the recipient
opens the letter.

## How swarm-net uses it

- `Envelope` in `backend/pkg/protocol/message.go` has `Version`, `Type`, `From`, `To`, `ID`,
  `SentAtUnixNano` and `Payload`. The payload stays as raw JSON until the type is known
  (`PayloadOf`).
- **Unknown type:** drop the message, keep the connection. A newer node may send types an
  older node has never seen.
- **Unknown version:** close the connection (`ErrUnsupportedVersion`). The envelope layout
  itself may differ, so no field can be trusted. `CurrentVersion` is 1.
- `ID` matches a reply to its request (PING to PONG). A reply reuses the request's ID
  (`NewReply`).
- Nodes are addressed by a stable `NodeID` (`backend/pkg/protocol/identity.go`), not an IP. A
  restarted container keeps its ID but may get a new IP.
- Message types are split into a control plane (heartbeats, elections) and a data plane
  (tasks, telemetry), so control traffic is not stuck behind bulk data.

```go
type Envelope struct {
    Version        uint8           `json:"version"`
    Type           MessageType     `json:"type"`
    From           NodeID          `json:"from"`
    To             NodeID          `json:"to,omitempty"`
    ID             string          `json:"id,omitempty"`
    SentAtUnixNano int64           `json:"sent_at_unix_nano"`
    Payload        json.RawMessage `json:"payload,omitempty"`
}
```

## Common pitfalls

- **Treating an unknown type as fatal.** A rolling upgrade then disconnects every old node.
- **Guessing at an unknown version.** Misread fields become wrong elections.
- **Using the IP as identity.** A restart looks like a brand new node, and the old one lingers.
- **Subtracting `SentAtUnixNano` across nodes.** It is a wall-clock value for display only (see
  [Monotonic vs Wall Clocks](./monotonic-vs-wall-clocks)).

## Further reading

- [Stream Framing](./stream-framing)
- [Interface Polymorphism](./interface-polymorphism)
- [Architecture Overview](../architecture/overview)
