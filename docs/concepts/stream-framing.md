---
title: "Stream Framing: There Are No Messages in TCP"
outline: deep
---

# Stream Framing: There Are No Messages in TCP

Two nodes in this swarm need to exchange discrete things: a heartbeat, a health score, a task, a
membership update. TCP offers none of those. TCP offers an ordered, reliable, bidirectional
**stream of bytes**, with no record boundaries whatsoever. The concept of a "message" is something
your application invents and must defend, on both ends, in the presence of an adversarial network
and a peer that may be lying, slow, or already dead.

This page assumes you have read [TCP Sockets & The Kernel](./tcp-sockets-and-the-kernel), which
covers where the bytes are buffered and why `read` behaves as it does. It prepares the ground for
a Phase 2 decision — JSON-over-newline versus length-prefixed binary framing — which is
**explicitly not made here**. See [The Pending Decision](#the-pending-decision) at the end.

## Core Mental Model

Think of the connection as a conveyor belt of bytes with no dividers. The sender drops bytes on;
the receiver picks bytes off. Nothing on the belt records where one sender-side `Write` ended and
the next began. That information is destroyed by the transport, irrecoverably, the moment it
enters the send queue.

```
  SENDER                          THE WIRE                       RECEIVER
                                                             
  Write("{\"t\":\"hb\"}\n")   ─┐
  Write("{\"t\":\"hb\"}\n")   ─┤  coalesced into one segment
  Write("{\"t\":\"hb\"}\n")   ─┘
                                 ┌──────────────────────┐
                                 │ {"t":"hb"}\n{"t":"hb"│ ─> Read() → 20 bytes
                                 │ "}\n{"t":"hb"}\n     │ ─> Read() → 13 bytes
                                 └──────────────────────┘
                                                             3 writes → 2 reads
                                                             neither matching a message

  Write(<4096-byte task>)        ┌──────┬──────┬────────┐
                                 │ 1448 │ 1448 │ 1200   │ ─> Read() → 1448
                                 └──────┴──────┴────────┘ ─> Read() → 1448
                                  segmented by MSS          ─> Read() → 1200
                                                             1 write → 3 reads
```

The only correct mental model is:

> `Read` hands you *some* bytes. Zero information about message boundaries accompanies them.
> You append them to a buffer, then you ask your own parser whether a complete message is present.

Every framing scheme is an answer to one question: **given a prefix of the stream, how do I know
where the current message ends?** There are exactly three families of answer — a delimiter, a
length, or a self-describing header — and they differ in cost, debuggability, and what happens
after something goes wrong.

## Under the Hood

### Why writes coalesce and reads fragment

Three independent mechanisms conspire.

**1. Segmentation by MSS.** TCP splits your byte stream into segments no larger than the
**Maximum Segment Size**, negotiated in the SYN and derived from the path MTU. On a standard
Ethernet path with MTU 1500, MSS is 1448 (1500 − 20 IP − 20 TCP − 12 bytes of timestamp option).
On the Docker bridge in this project the MTU may differ; `ss -tni` reports both:

```
     cubic wscale:7,7 rto:204 rtt:0.412/0.123 mss:1448 pmtu:1500 advmss:1448
```

Any message larger than the MSS is *guaranteed* to be delivered as multiple segments, and each
segment can wake the receiver independently. A 4 KB task payload will essentially never arrive in
a single `Read`.

**2. Coalescing by Nagle and by the send queue.** Small consecutive writes accumulate in
`sk_write_queue` and are transmitted together. The receiver's `read` then drains whatever has
accumulated in `sk_receive_queue` at that instant, which may be two messages, or one and a half.

**3. Receiver-side timing.** Even with one segment per message on the wire, the receiver's
`read` may be called after two segments have arrived, returning both at once. Or it may be called
with a 4 KB buffer against a receive queue containing 1448 bytes, returning a partial message. The
kernel returns *what it has*, never *what you asked for*.

There is a fourth, rarer mechanism worth knowing: **path MTU discovery**. If an intermediate link
has a smaller MTU and the DF bit is set, the router replies with ICMP "fragmentation needed" and
the sender lowers its MSS mid-connection. If that ICMP is firewalled — an alarmingly common
misconfiguration, known as an ICMP black hole — the connection establishes fine, small messages
work fine, and large messages hang forever. This presents as "heartbeats work, task broadcast
doesn't", which is a maddening bug if you do not know to look for it.

### Partial reads and partial writes in Go

```go
// WRONG. Silently corrupts on any message larger than one segment,
// and on any pair of messages that coalesce.
buf := make([]byte, 4096)
n, err := conn.Read(buf)
if err != nil {
	return err
}
var msg Message
return json.Unmarshal(buf[:n], &msg) // buf[:n] is a prefix, a suffix, or one and a half messages
```

```go
// RIGHT for length-prefixed framing: ReadFull loops until the exact count is satisfied.
const maxFrameSize = 1 << 20 // 1 MiB — mandatory, see below

func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF here means a clean boundary; ErrUnexpectedEOF means truncation
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrameSize {
		return nil, fmt.Errorf("frame size %d out of bounds", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("truncated frame body: %w", err)
	}
	return body, nil
}
```

Note the asymmetry in errors from `io.ReadFull`: it returns `io.EOF` if *zero* bytes were read
(the peer closed cleanly on a message boundary — a normal shutdown) and `io.ErrUnexpectedEOF` if
*some* bytes were read (the peer died mid-message — a real fault). Distinguishing these is how you
avoid logging a graceful disconnect as an error, and how you avoid logging a truncation as a
graceful disconnect.

On the write side, `net.Conn.Write` already loops internally, so a short count always accompanies
an error. But framing correctness demands that a header and its body reach the wire as one logical
unit *from the perspective of concurrent writers*:

```go
// A single Write of header+body avoids interleaving two frames from two goroutines,
// and avoids the extra segment that a separate header write would produce.
func WriteFrame(w io.Writer, body []byte) error {
	if len(body) > maxFrameSize {
		return fmt.Errorf("frame too large: %d", len(body))
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	_, err := w.Write(frame)
	return err
}
```

`net.Conn` is safe for concurrent use in the sense that it will not corrupt kernel state, but two
concurrent `Write` calls may interleave their bytes on the stream, which destroys framing
absolutely. **Every connection needs exactly one writer**, either a mutex or, better, a dedicated
writer goroutine fed by a channel.

### The three framing strategies

**Delimiter-delimited.** A byte that cannot appear inside a message terminates it. Newline is the
usual choice, paired with JSON (which never emits a raw newline inside a compact encoding, since
newlines inside strings are escaped as `\n`).

```go
sc := bufio.NewScanner(conn)
sc.Buffer(make([]byte, 0, 4096), maxFrameSize) // MUST bound it; default is 64 KiB
for sc.Scan() {
	var msg Message
	if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
		return err // framing is fine, content is not — still fatal for this connection
	}
	handle(msg)
}
return sc.Err() // bufio.ErrTooLong if a line exceeded the buffer
```

**Length-prefixed binary.** A fixed-width integer states the body length; the body follows. Shown
above. The length is conventionally big-endian (network byte order), and `binary.BigEndian` in Go
compiles to a single `bswap` on amd64, so the choice costs nothing.

```
 ┌────────────┬──────────────────────────────┐
 │ len: u32BE │ body: len bytes              │
 └────────────┴──────────────────────────────┘
      4 B                 N B
```

**Self-describing / typed header.** A fixed header carrying length *and* metadata: a magic number,
a protocol version, a message type, a flags field, sometimes a checksum. This is what real
protocols converge on.

```
 ┌────────┬─────┬──────┬───────┬────────────┬──────────────────┐
 │ magic  │ ver │ type │ flags │ len: u32BE │ body: len bytes  │
 │ u16    │ u8  │ u8   │ u16   │            │                  │
 └────────┴─────┴──────┴───────┴────────────┴──────────────────┘
    2 B     1B    1B     2 B        4 B             N B
```

The magic number is not decoration. It is how a receiver detects that it is talking to something
that is not this protocol at all — a health-check prober, a port scanner, an HTTP client that got
the wrong port — and rejects it on the first four bytes rather than allocating a 3 GB buffer
because the ASCII of `GET ` happens to be a very large `uint32`. The version byte is how you
survive a rolling upgrade where half the swarm speaks v1 and half speaks v2.

### Trade-offs

| | Delimiter (newline + JSON) | Length-prefixed binary | Typed header |
|---|---|---|---|
| **Parse cost** | Must scan every byte for the delimiter; JSON decode allocates heavily (reflection, interface boxing, string copies) | No scan: read 4 bytes, read N bytes. Body decode cost depends on the codec | Same as length-prefixed plus a few fixed-offset field reads |
| **Encode cost** | JSON marshal allocates; escaping must be verified | One length write, one copy | Header write is a handful of stores |
| **`tcpdump -A` / `netcat`** | Fully readable. You can `nc node-3 7946` and type a message by hand | Opaque without a decoder; `tcpdump -X` shows hex; needs a custom dissector to be useful | Opaque body, but magic/version/type are readable in hex and identify the frame |
| **Max message bound** | Must be imposed externally via `Scanner.Buffer`; nothing in the format states a size | Stated in the frame itself; can be rejected before allocating | Same, and the type field allows per-type limits |
| **Allocation before validation** | Buffer grows as the scan proceeds — you allocate before you know the size | You know the size first, so you validate then allocate | Same, plus you can reject on magic before reading the length at all |
| **Resynchronisation after corruption** | Theoretically possible: discard to the next delimiter and resume. In practice the "corruption" is usually an encoder bug producing an unescaped delimiter, so resync lands mid-message | Impossible. A wrong length consumes an arbitrary number of following bytes as body | Impossible, but the magic number lets you *detect* desync immediately on the next frame rather than silently misparsing |
| **Binary payload** | Requires base64 or escaping: ~33% overhead plus encode cost | Native | Native |
| **Schema evolution** | Trivial — unknown JSON fields are ignored by `encoding/json` | Depends entirely on the body codec | Version + type fields make it explicit and enforceable |
| **Debuggability of a bug at 3am** | Highest. Logs are the wire format | Lowest without tooling | Middling; you will write a `swarmdump` helper |

There is no free answer. Delimiter framing optimises for the human reading the wire; length
prefixing optimises for the machine parsing it; typed headers pay a few fixed bytes to buy failure
detection and versioning.

### Bounded reads are a security property, not an optimisation

An unbounded length field is a remote out-of-memory primitive. Consider:

```go
n := binary.BigEndian.Uint32(hdr[:]) // attacker sends 0xFFFFFFFF
body := make([]byte, n)              // 4 GiB allocation, immediately
```

One packet, four bytes of header, and the node is dead. It need not even be malicious: a
desynchronised stream will produce a garbage length just as effectively as an attacker. The same
applies to delimiter framing — `bufio.Scanner` without an explicit `Buffer` call caps lines at
`bufio.MaxScanTokenSize` (64 KiB) and returns `bufio.ErrTooLong`, which is a safe default, but
`bufio.Reader.ReadString('\n')` has **no bound at all** and will grow until the process dies.

The rule is absolute: **every framing decoder validates the length against a compile-time maximum
before allocating a single byte.** The maximum is a protocol constant, not a configuration knob,
because both ends must agree on it.

### Nagle's algorithm and delayed ACK: the 40 ms stall

Nagle's algorithm (RFC 896) says: if there is unacknowledged data outstanding, buffer any new
small write until either the outstanding data is acknowledged or a full MSS has accumulated. Its
purpose is to stop a telnet session putting one byte in every 41-byte packet.

Delayed ACK (RFC 1122) says: do not acknowledge immediately; wait up to `ato` (40 ms on Linux,
visible as `ato:40` in `ss -tni`) in the hope that outgoing data will come along to piggyback on,
or that a second segment will arrive so one ACK can cover both.

Each is reasonable. Together, on a small request/response exchange, they deadlock against each
other:

```
  t=0ms    A: write(heartbeat)         → sent immediately (nothing outstanding)
  t=0.4ms  B: receives it
           B: delayed ACK timer starts — B has no data to send yet
  t=0.5ms  B: application writes reply → but B's Nagle has nothing outstanding, so it sends
           A: receives reply, starts ITS delayed ACK timer
  t=1ms    A: write(next heartbeat)    → A has unacked data outstanding (the reply's ACK
                                          is not sent yet) AND the write is < MSS
           A: Nagle buffers it. Nothing goes out.
  t=41ms   B: delayed ACK timer fires, ACK arrives
           A: Nagle releases the buffered heartbeat
  ────────────────────────────────────────────────────────────────
  Measured RTT: 41 ms. Actual network RTT: 0.4 ms.
```

The observable signature is a latency distribution clustered at multiples of 40 ms — 40, 80,
sometimes 200 — on a path whose `ss -tni` reports `rtt:0.4`. For a system that elects leaders by
measured latency, this is not a performance nuisance; it is a **correctness bug in the health
model**, because the stall is an artefact of framing behaviour rather than of node health, and it
will not be evenly distributed across peers.

The fix is `TCP_NODELAY`, which disables Nagle. Go sets it by default on every `TCPConn` —
`setNoDelay(true)` is applied in `net.Dial` and on accepted connections — so you get the right
behaviour for free, and the failure mode only returns if someone "optimises" by calling
`SetNoDelay(false)`.

The second-order lesson survives even with Nagle disabled: **the number of `Write` calls per
logical message matters.** Writing a 4-byte header and then a body as two separate `Write` calls
can produce two segments, two receiver wakeups, and two trips through the scheduler. Building the
frame in one buffer, or using `net.Buffers` (which maps to `writev(2)` and emits a single
segment), keeps one message to one syscall.

### Buffered readers change the failure modes

Wrapping a connection in `bufio.Reader` is close to mandatory: without it, a length-prefixed
decoder issues one syscall for the 4-byte header and another for the body, doubling the syscall
count and the scheduler round-trips per message. With a 4 KiB buffered reader, several small
frames are typically satisfied from one `read`.

But buffering moves bytes from kernel space into a Go-heap buffer your connection object owns,
and that has consequences:

- **Data can be lost on abandonment.** If you discard a `bufio.Reader` and go back to reading the
  raw `net.Conn`, everything already buffered is silently gone. Never mix the two.
- **`ss` lies to you about progress.** `Recv-Q` on the socket shows 0 because the buffered reader
  drained the kernel, even though your application has not processed any of it. Kernel-level
  backpressure is delayed by exactly the buffer size.
- **Read deadlines apply to the underlying syscall, not to your logical read.** A
  `SetReadDeadline` that expires while `bufio` has a complete frame in its buffer will not fire;
  one that expires mid-refill will, leaving partial bytes buffered and the stream desynchronised.
- **Buffer size is a latency/syscall trade-off.** A large buffer reduces syscalls but increases
  the worst-case delay between a byte arriving in the kernel and your code seeing it, because a
  refill only happens when the buffer is exhausted.

### Framing errors are unrecoverable

This is the point that most often has to be learned the expensive way. If your decoder concludes
"this is not a valid frame", you have lost track of where you are in the stream. There is no
mechanism to ask TCP where the next message starts, because TCP does not know — the concept does
not exist at that layer.

```
  stream:  [ frame A ][ frame B ][ frame C ][ frame D ]...
                         ^
                         decoder read a bad length here and consumed 9000 bytes
                         it is now positioned in the middle of frame D
                         every subsequent parse is garbage, possibly for hours
```

Continuing to parse after a framing error does not degrade gracefully. It produces *plausible*
messages — a heartbeat that decodes cleanly from the middle of a task payload — and those
plausible messages will be acted on. A swarm that demotes a healthy leader because it misparsed a
membership update is far worse off than a swarm that dropped a connection and redialled.

The only correct response to a framing error is:

1. Close the connection. Do not attempt to resynchronise.
2. Record which peer and which error, because repeated framing errors from one peer indicate a
   version mismatch or a bug, not a transient fault.
3. Re-dial with backoff. A fresh connection starts at a known boundary — the only boundary you
   can ever be certain of.

A *content* error — valid frame, invalid JSON, or a field out of range — is a different matter.
Framing is intact, so the stream position is known, and you may log and continue. Keeping these
two categories distinct in the error types is worth doing from the first commit.

## Why It Matters in This Swarm

No Go code exists yet. What follows are commitments the implementation will be held to.

**`pkg/network/` — the framing codec.** This package will own the sole `Encoder`/`Decoder` pair
used by every connection in the system. It commits to:

- A compile-time `MaxFrameSize` constant, validated before any allocation sized by remote input.
- `io.ReadFull` for every fixed-width read, never a bare `Read`.
- Distinguishing `io.EOF` (clean close on a boundary) from `io.ErrUnexpectedEOF` (truncation), and
  surfacing them as different error types so the cluster layer can distinguish "peer left" from
  "peer crashed".
- A typed `ErrFraming` that the connection manager treats as unconditionally fatal to the
  connection, separate from decode errors that are not.
- One writer per connection, enforced structurally — a writer goroutine fed by a channel, not a
  mutex that a future contributor can forget to take.
- A single `Write` (or `net.Buffers`) per logical frame, so one message is one segment where it
  fits in one.
- A `bufio.Reader` per connection with an explicitly chosen size, documented against the expected
  frame size distribution rather than picked at random.

**`pkg/protocol/` — wire schemas.** Will define the message set (heartbeat, health report, join,
promote, task, telemetry) and their encoding. Whatever framing is chosen, this package commits to
carrying an explicit protocol version in the handshake, so that a rolling upgrade produces a clean
refusal rather than a silent misparse.

**`pkg/health/` — `LatencyHealthStrategy`.** Measures application-level round trip. It commits to
issuing probes over a connection with `TCP_NODELAY` in force, and to recording a distribution
rather than a mean — because the 40 ms Nagle signature, and the tail latency that actually
predicts a bad leader, are both invisible in an average. Election quality depends directly on this
measurement not being an artefact of framing.

**`pkg/cluster/` — heartbeats.** Heartbeat frames are small and frequent, which is exactly the
traffic pattern that framing overhead and small-write stalls distort. The heartbeat path commits
to a single-allocation encode where practical, and to a read deadline armed before every read so a
silent peer is detected by the application rather than by nothing at all.

**`cmd/control-center/`** bridges swarm frames to a browser WebSocket. WebSocket is itself a
length-prefixed framing layer bolted onto TCP for exactly the reasons described on this page —
worth noting as a sanity check on the design space, since the people who wrote RFC 6455 reached
the same conclusion.

## The Pending Decision

Phase 2 must choose between **JSON over newline framing** and **length-prefixed binary framing**
(with or without a typed header). This page deliberately does not make that choice. The trade-off
table above is the input to it, and the axes that matter most for this system are:

- The swarm is a *control plane*, not a data plane: message rate is bounded by heartbeat interval
  and node count, not by throughput. Parse cost is therefore unlikely to be the binding
  constraint.
- The repository is a **teaching artifact**. A wire format you can read with `tcpdump -A` during a
  chaos experiment has direct pedagogical value that a binary format does not.
- Conversely, implementing length-prefixed framing correctly — bounds, partial reads,
  resynchronisation policy, version negotiation — *is itself the lesson*, and a format that hands
  you `bufio.Scanner` teaches less about why any of this is hard.

**This decision is pending user input and will be recorded in `docs/WORKLOG.md` when made.**

## Further Reading in This Curriculum

- [TCP Sockets & The Kernel](./tcp-sockets-and-the-kernel) — where the bytes are buffered and why
  `Read` returns what it does.
- [epoll, select & Non-Blocking I/O](./nonblocking-io-and-epoll) — how readiness notification
  interacts with partial frames.
- [The Netpoller](./go-netpoller) — why a goroutine blocked in `io.ReadFull` costs you almost
  nothing.
