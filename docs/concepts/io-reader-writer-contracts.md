---
title: "io.Reader, io.Writer, and the ReadFull Family"
description: "The exact contracts behind Go's two most important interfaces, why Read is harder to use correctly than Write, and how io.ReadFull's EOF/ErrUnexpectedEOF asymmetry encodes the difference between a peer that left and a peer that died."
outline: deep
---

# io.Reader, io.Writer, and the ReadFull Family

`io.Reader` and `io.Writer` are two methods between them. They are also, by a wide margin, the two
interfaces most often used incorrectly by people who have been writing Go for years -- because the
incorrect usage works. It works on `bytes.Buffer`, it works on `os.File`, it works on loopback TCP
with small messages, and it fails the first time a message crosses a segment boundary on a real
network under load.

This page is the Go-level counterpart to
[Stream Framing](./stream-framing), which explains why message boundaries do not exist in TCP. Here
we cover the mechanics of consuming such a stream correctly: what the contracts actually permit,
what `io.ReadFull` adds, and the one error asymmetry in the standard library that this swarm's
entire failure-detection story rests on.

## Core Mental Model

### `io.Reader` is a *request*, not a *command*

```go
type Reader interface {
	Read(p []byte) (n int, err error)
}
```

The mental model people arrive with is "fill this buffer." The actual contract is closer to **"here
is space for up to `len(p)` bytes; give me whatever you have right now, and tell me how much that
was."**

The full return space is wider than most callers imagine:

Read(p) with len(p) == 4:

| Return Values | Description |
|---|---|
| n == len(p), err == nil | the case everyone codes for |
| 0 < n < len(p), err == nil | PARTIAL. Completely legal. Common. |
| 0 < n < len(p), err == io.EOF | DATA *AND* EOF IN ONE CALL. You must process the n bytes. |
| n == 0, err == io.EOF | clean end, nothing more coming |
| n == 0, err != nil | a real error |
| n == 0, err == nil | legal but discouraged. Must not be treated as EOF. Retry. |

Three rules follow directly from the documented contract, and each one corresponds to a bug you
will otherwise write:

1. **A short read is not an error.** `Read` returning 2 when you asked for 4 has succeeded.
2. **`n > 0` with `err == io.EOF` is legal and you must handle the bytes before the error.** The
   `io` package documentation says so explicitly, and says that callers "should always process the
   n > 0 bytes returned before considering the error err."
3. **`(0, nil)` is legal.** The docs call implementations that return it "discouraged" except when
   `len(p) == 0`, and instruct callers to treat it as a no-op rather than as EOF.

### `io.Writer` is a *command*

```go
type Writer interface {
	Write(p []byte) (n int, err error)
}
```

`Write` carries a strictly stronger guarantee: it **must** write all of `p` or return a non-nil
error, and it must return a non-nil error if it returns `n < len(p)`.

Write(p):

- err == nil   =>  n == len(p), ALWAYS. Every byte was accepted.
- err != nil   =>  n may be anything in [0, len(p)]. The stream's state
                   is now unknown from your side, which is why an error
                   on a framed connection means "drop the connection",
                   not "retry the remainder".

**Why the asymmetry?** Because the writer can loop internally and the reader cannot. A `Write` that
accepts 1448 of your 4096 bytes can simply call the underlying `write(2)` again -- the remaining data
is sitting right there in your buffer, owned by you, unchanging. A `Read` that obtains 2 of the 4
bytes you asked for has no such option: the other 2 bytes are *on a different machine*, or in a
router queue, or have not been sent yet, and may never be sent at all. The reader cannot invent data
that has not arrived; the only thing it can do is block, and the contract deliberately refuses to
require that, because blocking until a buffer is full is a deadlock waiting for a peer that has
nothing more to say.

That single sentence -- *the writer can retry internally, the reader cannot invent data* -- explains
every awkwardness on this page.

### `io.ReadFull` restores the symmetry, at a price

`io.ReadFull(r, buf)` is the loop you would otherwise write: keep calling `Read` until `buf` is
full. It converts the reader's weak contract into the writer's strong one -- **on return with a nil
error, exactly `len(buf)` bytes were read** -- and the price is that it blocks until that is true,
which is why the deadline discussion at the end of this page is not optional reading.

## Under the Hood

### The three wrong loops

**Wrong #1 -- the bare read.** By far the most common.

```go
// WRONG: assumes Read fills the buffer.
var hdr [4]byte
if _, err := r.Read(hdr[:]); err != nil {
	return err
}
n := binary.BigEndian.Uint32(hdr[:])
```

If `Read` returns 2, `hdr[2]` and `hdr[3]` are whatever was in the array before -- zeros on the first
call, *the previous frame's header bytes* on a reused buffer. The decoded length is garbage, and
because the error is `nil` nothing indicates a problem. The stream desynchronises silently. On a
length-prefixed protocol this is unrecoverable; see
[Stream Framing](./stream-framing#why-there-is-no-resync-api-and-why-that-absence-is-the-design).

**Wrong #2 -- discarding data on EOF.**

```go
// WRONG: throws away up to len(p) bytes of real data.
n, err := r.Read(p)
if err != nil {
	return err          // <- if err == io.EOF and n == 3, those 3 bytes are gone
}
process(p[:n])
```

The last message of every connection is the one that disappears, which makes this bug maddeningly
intermittent: it only manifests when a peer's final write and its `close` coalesce into one segment,
so it depends on the peer's shutdown timing rather than on anything local.

**Wrong #3 -- treating `(0, nil)` as the end.**

```go
// WRONG: a conforming-but-unusual Reader terminates the loop early.
for {
	n, err := r.Read(p)
	if n == 0 {        // <- not EOF. Just "nothing right now".
		break
	}
	...
}
```

Rare against `net.Conn`, but `io.Reader` implementations in the wild -- decompressors, decryptors,
`io.MultiReader` over an empty member, rate limiters -- can and do return `(0, nil)`.

**Right:**

```go
for {
	n, err := r.Read(p)
	if n > 0 {
		process(p[:n])   // ALWAYS before the error check
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil   // clean end
		}
		return err
	}
	// n may be 0 with err == nil: loop again.
}
```

...and, for anything with a fixed width, don't write the loop at all. Write `io.ReadFull`.

### What `io.ReadFull` actually does

It is a one-line call into `io.ReadAtLeast`, and the four lines after that function's loop are the
entire reason this page exists:

```go
func ReadFull(r Reader, buf []byte) (n int, err error) {
	return ReadAtLeast(r, buf, len(buf))
}

func ReadAtLeast(r Reader, buf []byte, min int) (n int, err error) {
	if len(buf) < min {
		return 0, ErrShortBuffer
	}
	for n < min && err == nil {
		var nn int
		nn, err = r.Read(buf[n:])
		n += nn
	}
	if n >= min {
		err = nil                 // (A) enough bytes => suppress a trailing EOF
	} else if n > 0 && err == EOF {
		err = ErrUnexpectedEOF    // (B) THE ASYMMETRY
	}
	return
}
```

Line (A) handles wrong-loop #2 for you: a `Read` that returned the final bytes *together with*
`io.EOF` still counts, and `ReadFull` reports success. Line (B) is the subject of the next section.

Note what `ReadAtLeast` does *not* do: it has no notion of a deadline, no retry policy, and no
special handling for `(0, nil)` beyond looping. It will spin on a pathological reader that returns
`(0, nil)` forever. Against a `net.Conn` that cannot happen -- the read blocks in the netpoller
instead of returning zero -- but it is worth knowing that `ReadFull` inherits whatever liveness
properties the underlying reader has, and adds none.

### THE ASYMMETRY: `io.EOF` vs `io.ErrUnexpectedEOF`

This is the single highest-value idea on the page. `io.ReadFull` returns:

- **`io.EOF`** -- if **zero** bytes were read before the stream ended.
- **`io.ErrUnexpectedEOF`** -- if **some but not all** bytes were read before the stream ended.
- **`nil`** -- if all bytes were read.

Applied at a frame boundary, those three outcomes are not three error codes. They are three
*distinct physical events on another machine*:

Decoder is positioned exactly at a frame boundary. It calls io.ReadFull(conn, hdr[:4]).

| Bytes Read | Error | Meaning |
|---|---|---|
| 4 | nil | A frame is arriving. Proceed. |
| 0 | io.EOF | The peer sent FIN with nothing outstanding. It *chose* to close, at a boundary, having finished a thought. This is a GRACEFUL DEPARTURE. Do not penalise its health score; it may rejoin. |
| 1..3 | io.ErrUnexpectedEOF | The peer was mid-sentence when the socket ended. It committed to a frame and vanished before finishing it. This is the SIGKILL SIGNATURE. A genuine failure signal for the failure detector. |

Same three outcomes again for the payload read -- but there, even "zero bytes read" means death, because the header already promised n bytes. That case therefore gets NORMALISED from io.EOF to io.ErrUnexpectedEOF.

Collapse these into `if err != nil { log("read failed") }` and you have thrown away the only
information the transport layer offers about *how* a peer went away. A system that cannot
distinguish "node-4 left" from "node-4 was killed" cannot score health honestly, cannot decide
whether to demote a leader or merely deregister it, and cannot avoid punishing a node for a planned
restart. In this repository that three-way classification is the input to the Phase 4 self-healing
logic; `IsProtocolViolation` adds a fourth category (the peer is alive and talking nonsense) so the
caller can branch on all four without string-matching error text. See
[Error Wrapping & Classification](./error-wrapping-and-classification).

### Why a 4-byte header genuinely arrives split

It is tempting to dismiss "the header arrived in two pieces" as a theoretical concern. Four bytes;
surely the kernel gives you four bytes. It does not, and there are at least four independent
mechanisms that break it. Each of these is a real thing that has happened to real systems:

1. SEGMENT BOUNDARY

   The sender's previous frame ended 2 bytes into an MSS-sized segment.

   ```mermaid
   packet-beta
     0-95: "tail of frame A"
     96-111: "hdr[0]"
     112-127: "hdr[1]"
     128-200: "segment k ends"
     201-215: "hdr[2]"
     216-231: "hdr[3]"
     232-255: "next frames"
   ```

   The receiver's read drains segment k and returns. Two header bytes.

2. PMTU CHANGE MID-CONNECTION

   An ICMP "fragmentation needed" lowers MSS from 1448 to 1400 between
   two writes. Every subsequent frame lands at a different alignment,
   so a split that never occurred in ten thousand frames starts occurring.

3. A PEER THAT WRITES HEADER AND BODY SEPARATELY

   Not this codebase -- but a future contributor's, or a different
   implementation of the same protocol. Two writes with TCP_NODELAY on
   produce two segments. The header arrives alone, guaranteed, every time.

4. SIGKILL BETWEEN TWO WRITES

   The peer wrote the header, was killed before writing the body, and the
   kernel flushed what was already queued and then sent RST/FIN.
   The receiver sees exactly 4 header bytes and then the end of the stream.
   In this swarm that is not an edge case -- it is a button on the dashboard.

Mechanism 4 is why this project treats truncation as routine. The chaos controls exist to produce
it on demand, so code paths that only execute on a truncated frame are not rare paths; they are
demo paths.

You can watch a split happen. Against a peer writing header and body separately, `strace` shows the
shape plainly:

```
$ strace -f -e trace=read,write -p <pid>
read(7, "\0\0\1\24", 4)                = 4        <- header alone, one syscall
read(7, "{\"version\":1,\"type\":\"heart"..., 276) = 276
...
read(7, "\0\0", 4)                     = 2        <- SPLIT HEADER: 2 of 4
read(7, "\1\24", 2)                    = 2        <- ReadFull's second iteration
```

Note the second `read` requests only 2 bytes -- that is `ReadFull` calling `r.Read(buf[n:])` with the
remaining slice. If you ever see a second `read` requesting the full 4 again, someone has written a
retry loop that restarts from the beginning of the buffer, which is a different and much worse bug.

And you can prove your decoder survives it without any network at all, by feeding it a reader that
splits everything. That is what `pkg/protocol/framing_test.go:37` (`dripReader`, one byte per
`Read`) and `pkg/protocol/framing_test.go:61` (`chunkReader`, a hand-picked pathological size
sequence) exist for.

### `bufio`: when it helps and when it hides the lesson

Wrapping a `net.Conn` in a `bufio.Reader` is usually right. Without it, a length-prefixed decoder
issues two syscalls per frame -- one for the header, one for the payload -- and each syscall on an
empty socket is a park/unpark round trip through the [netpoller](./go-netpoller) and the
[scheduler](./go-scheduler-gmp). With a 4 KiB buffer, several small heartbeats are typically served
from a single `read(2)`.

What it does **not** do is remove the need to understand partial reads. `bufio.Reader.Read` has
exactly the same weak contract as any other `io.Reader` -- it will happily return fewer bytes than
you asked for when its internal buffer is partly full. `bufio` reduces the *frequency* of short
reads; it does not eliminate them, and code that is only correct because short reads are rare is
code that is incorrect and untested.

Three specific hazards:

- **`bufio.Reader.ReadString(delim)` has no maximum.** It grows its return slice until the delimiter
  arrives or the stream ends. Against a hostile or merely broken peer that never sends `\n`, it is a
  one-connection OOM primitive: no header, no allocation cap, nothing to reject. `bufio.Scanner` is
  better -- it caps at `bufio.MaxScanTokenSize` (64 KiB) by default and returns `bufio.ErrTooLong` --
  but "better" here means it has a bound *at all*, and you should still set it explicitly with
  `Scanner.Buffer`. This is a significant part of why the framing decision went the way it did.
- **Buffered bytes are invisible to `ss`.** `Recv-Q` reads 0 because `bufio` drained the kernel
  queue into user space. You lose the kernel's backpressure signal for exactly one buffer's worth of
  data, and your monitoring stops reflecting your application's actual backlog.
- **Abandoning a `bufio.Reader` loses data silently.** Anything buffered is gone. One connection
  gets exactly one reader, for its whole life.

## Why It Matters in This Swarm

### The codec takes `io.Reader`/`io.Writer`, not `net.Conn`

`protocol.NewEncoder` (`pkg/protocol/framing.go:161`) and `protocol.NewDecoder`
(`pkg/protocol/framing.go:285`) accept the interfaces, not the concrete connection type. Two
consequences, one good and one dangerous.

**The good one: testability.** Every test in `pkg/protocol` runs against a `bytes.Buffer` or a
hand-written adversarial reader, with no sockets, no ports, no timing, and no flakes.
`TestPartialReadsOneByteAtATime` (`pkg/protocol/framing_test.go:175`) encodes twelve heartbeats into
a buffer and reads them back through a reader that yields one byte per call -- a scenario that a real
network produces perhaps once a month and that this test produces on every `go test` run.
`TestPartialReadsStraddlingBoundaries` (`pkg/protocol/framing_test.go:209`) does the same with chunk
sizes `{2, 3, 200, 1, 7, 1, 150, 4, 5, 300, 2}`, chosen to split reads across the header/payload
seam and across the seam between two frames -- the alignments where an off-by-one hides.

You cannot write either test against a `net.Conn` parameter without a real socket and a cooperating
peer that agrees to write two bytes at a time.

**The dangerous one: deadline custody.** An `io.Reader` has no `SetReadDeadline`. The codec
therefore *cannot* arm one, and the doc comment at `pkg/protocol/framing.go:252` -- the heading reads
`# Deadlines are not this package's job` -- states the consequence in capital letters rather than
leaving it to be discovered:

> A Decoder wrapping a net.Conn with NO READ DEADLINE SET WILL BLOCK FOREVER against a peer that was
> SIGKILLed.

This is the exact bug this project exists to teach, so it is worth walking through slowly. When a
container is `SIGKILL`ed:

```
   Normal close (SIGTERM, graceful):
     peer sends FIN --> local socket transitions to CLOSE_WAIT
                    --> read(2) returns 0
                    --> Go's conn.Read returns (0, io.EOF)
                    --> ReadFull returns io.EOF at a boundary. Detected instantly.

   SIGKILL of a container (chaos control):
     the process is gone, but nothing is *sent*. The container's network
     namespace may vanish with it, so not even an RST is generated.
     local socket state:  ESTABLISHED   // the kernel believes all is well
     Recv-Q:              0
     read(2):             blocks
     Go's conn.Read:      parks the goroutine in the netpoller
     ReadFull:            never returns
     ss -tn output:       ESTAB  0  0  172.18.0.4:7946  172.18.0.7:44312
                          ...indistinguishable from an idle healthy peer.
```

There is no error to observe, no EOF, and nothing in the socket state to inspect. The goroutine is
parked forever. Kernel TCP keepalive would eventually notice, but only if it is enabled on the
socket and only on its own timescale: `net.ipv4.tcp_keepalive_time` defaults to 7200 seconds -- two
hours of idling -- before the first of nine probes is even sent. Distributions retune it (this
project's dev host reports 120), which is exactly why you must not depend on it: it is a per-host
setting you do not control, not a protocol guarantee. A failure detector that must react in seconds
arms its own deadline.

**`SetReadDeadline` before every `ReadFrame` is therefore mandatory, and it is `pkg/network`'s
responsibility.** That deadline, not the absence of bytes, is what makes failure detection possible
at all. The absence of bytes is indistinguishable from a healthy quiet peer; a *deadline* converts
"nothing arrived" into an event. The codec documents this rather than silently owning it, which is
recorded as a locked contract in `docs/WORKLOG.md` S3.2: *"Deadlines are not `pkg/protocol`'s
concern... deadline custody belongs to `pkg/network`. The codec documents the consequence rather than
silently owning it."*

### `ReadFull` at both reads, and the normalisation between them

`Decoder.ReadFrame` (`pkg/protocol/framing.go:308`) opens with the rule stated as a comment --
`pkg/protocol/framing.go:309`: *"io.ReadFull, never a bare Read."* -- and then uses it twice:

- `pkg/protocol/framing.go:320` reads the 4-byte header. Its error is returned **unwrapped**, and
  that is deliberate: at this point the decoder sits exactly on a frame boundary, so `io.EOF` here
  carries the full meaning "clean close" and callers test it with `errors.Is(err, io.EOF)`. Wrapping
  it would still satisfy `errors.Is`, but returning it bare keeps the boundary semantics obvious at
  the call site.
- `pkg/protocol/framing.go:347` reads the payload, and the three lines at
  `pkg/protocol/framing.go:355-357` are the highest-value lines in the package:

```go
if errors.Is(err, io.EOF) {
	err = io.ErrUnexpectedEOF
}
```

Here -- and only here -- `io.EOF` and `io.ErrUnexpectedEOF` mean the *same* thing, so they are
normalised to one value. The header already declared `n` payload bytes. Whether the peer sent zero
of them or `n-1` of them, it committed and then vanished. Without this normalisation a peer killed
in the instant between writing a length prefix and writing its body would surface as `io.EOF` -- and
would be recorded as a graceful departure. The chaos controls make that timing common, not exotic.

`TestTruncatedPayloadIsUnexpectedEOF` (`pkg/protocol/framing_test.go:302`) cuts a real encoded frame
at exactly the header boundary, one byte past it, and one byte short of the end, and asserts all
three yield `io.ErrUnexpectedEOF` and none yields `io.EOF`.
`TestCleanEOFAtFrameBoundary` (`pkg/protocol/framing_test.go:328`) asserts the converse, and
additionally that `io.EOF` is not classified as a protocol violation. The two tests together are the
executable specification of the asymmetry.

### The `io.Writer` contract is load-bearing on the encode side

`Encoder.WriteEnvelope` (`pkg/protocol/framing.go:171`) assembles the header and payload into one
contiguous buffer and issues exactly one `Write` (`pkg/protocol/framing.go:226`). The rationale
block at `pkg/protocol/framing.go:197` gives two reasons, and the second is a direct appeal to the
`io.Writer` contract: a write that failed *between* a header write and a body write would leave the
peer blocked in `ReadFull` waiting for bytes the sender has abandoned. One `Write` cannot fail in
that gap, because `Write` must return a non-nil error whenever `n < len(p)` -- so either the frame
went out whole or the connection is already broken. There is no third state to design for.

This is the payoff for `Write`'s stronger contract, and it is why the encoder needs no partial-write
loop while the decoder needs `ReadFull` twice.

## Common Failure Modes & Edge Cases

**Partial header, silently accepted.** *Symptom:* a node runs correctly for hours and then every
subsequent frame from one peer is rejected as `ErrFrameTooLarge` or `ErrMalformedFrame`, or worse,
decodes into plausible nonsense. *Cause:* a bare `Read` of a fixed-width field somewhere, usually in
code added later by someone who did not know the rule. *Why it took hours:* the frame sizes only
started straddling a segment boundary when the cluster grew and the telemetry snapshot got bigger.

**The last message of a connection disappears.** *Symptom:* a peer's `TypeLeave` is never observed,
so it is recorded as a crash rather than a departure and its health score is penalised for a clean
shutdown. *Cause:* `if err != nil { return err }` placed before the `n > 0` check. *Why it is
intermittent:* it needs the peer's final write and its `close` to land in the same segment.

**`ErrUnexpectedEOF` collapsed into `EOF`.** *Symptom:* the swarm never re-elects after a chaos kill,
because a killed leader was classified as having left voluntarily and the eviction path was never
entered. *Cause:* either a missing normalisation on the payload read, or a caller doing
`if err == io.EOF || err == io.ErrUnexpectedEOF` in one branch for tidiness. This is the failure that
`pkg/protocol/framing.go:355-357` and its two tests exist to make impossible.

**`ReadFull` with no deadline.** *Symptom:* the swarm appears healthy and does nothing. Goroutine
count is stable, CPU is zero, `ss -tn` shows `ESTAB` with empty queues, and a `SIGQUIT` dump shows
goroutines parked in `internal/poll.(*FD).Read`. *Cause:* the connection layer forgot
`SetReadDeadline`. This does not reproduce with `docker stop` -- which sends SIGTERM and lets the
process close its sockets -- only with `docker kill`. Test the kill path, or you will not find it.

**Retrying a read from the start of the buffer.** *Symptom:* corrupted frames only under load.
*Cause:* a hand-rolled loop doing `r.Read(buf)` instead of `r.Read(buf[n:])`, overwriting bytes it
already has. Use `io.ReadFull` and the bug is unwritable.

**`ReadString` on a hostile stream.** *Symptom:* one peer, one connection, and the container hits its
memory limit and is OOM-killed by the kernel -- with no log line explaining why, because the process
never got to write one. *Cause:* an unbounded delimiter scan. This is the concrete failure that
`MaxFrameSize` (`pkg/protocol/framing.go:58`) and validate-before-allocate
(`pkg/protocol/framing.go:330`) exist to prevent, and it is covered in depth in
[Stream Framing](./stream-framing#bounded-reads-are-a-security-property-not-an-optimisation).

**Two readers on one `Decoder`.** *Symptom:* frames delivered to the wrong handler; the race detector
fires on `d.buf`. `Decoder` deliberately carries no mutex (`pkg/protocol/framing.go:240`) precisely
so this shows up as a race-detector hit on the first test run rather than as a silent correctness bug
-- a mutex here would serialise the calls without making them *correct*, since neither goroutine
could know which logical frame it received.

**Mixing a `bufio.Reader` and the raw `net.Conn`.** *Symptom:* a frame goes missing at a handoff
point, typically after a handshake, and the stream desynchronises immediately afterwards. *Cause:*
the handshake read through `bufio`, which buffered ahead into the first real frame, and then the
steady-state loop read from the `net.Conn` directly. One reader per connection, for its lifetime.

## Further Reading in This Curriculum

- [Stream Framing](./stream-framing) -- why boundaries must be reconstructed at all, and the
  length-prefixed format these contracts are used to implement.
- [TCP Sockets & The Kernel](./tcp-sockets-and-the-kernel) -- `sk_receive_queue`, what `read(2)`
  returns and when, and why `ESTABLISHED` proves nothing about a peer.
- [epoll, select & Non-Blocking I/O](./nonblocking-io-and-epoll) -- readiness notification, and why
  "readable" means "at least one byte", never "a whole frame".
- [The Netpoller](./go-netpoller) -- where a goroutine blocked inside `io.ReadFull` actually goes, and
  why that is cheap right up until it is forever.
- [Wire Protocol Design](./wire-protocol-design) -- the envelope these framed bytes carry.
- [Error Wrapping & Classification](./error-wrapping-and-classification) -- turning the three-way
  read outcome into a decision the cluster layer can act on.
