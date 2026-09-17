---
title: "io.Reader, io.Writer, and the ReadFull Family"
description: "What Read and Write actually promise, why io.ReadFull is needed on a TCP stream, and how EOF vs ErrUnexpectedEOF tells a clean close from a crash."
outline: deep
---

# io.Reader, io.Writer, and the ReadFull Family

`io.Reader` and `io.Writer` are Go's two basic I/O interfaces. Their promises are **not**
symmetric:

- `Write(p)` must write all of `p`, or return an error.
- `Read(p)` may return **fewer** bytes than `len(p)`, even 1, with no error. It is a request,
  not a command.

Analogy: `Write` is posting a whole letter. `Read` is checking the mailbox: you get whatever
has arrived so far.

On TCP, a short read is normal. A 4-byte header can arrive as 2 bytes, then 2 more.
`io.ReadFull(r, buf)` keeps calling `Read` until `buf` is full. Its errors carry a useful
difference:

- `io.EOF`: the stream ended **before any byte** was read. The peer closed cleanly.
- `io.ErrUnexpectedEOF`: the stream ended **partway** through. The peer died mid-message.

## How swarm-net uses it

- `NewEncoder` and `NewDecoder` in `backend/pkg/protocol/framing.go` take `io.Writer` and
  `io.Reader`, not `net.Conn`. Tests feed them a `bytes.Buffer` or a reader that returns one
  byte at a time.
- `Decoder.ReadFrame` uses `io.ReadFull` for the header and for the payload. Never a bare `Read`.
- If the payload read hits `io.EOF`, it is changed to `io.ErrUnexpectedEOF`: the peer promised
  n bytes and vanished, which is a death, not a clean close.
- `Encoder.WriteEnvelope` builds header plus payload in one buffer and calls `Write` once.
- The codec cannot set deadlines (an `io.Reader` has none). `backend/pkg/network/conn.go` sets
  a read deadline before every `ReadFrame`.

```go
hdr := make([]byte, 4)
if _, err := io.ReadFull(r, hdr); err != nil {
    return err // io.EOF = clean close, io.ErrUnexpectedEOF = died mid-header
}
n := binary.BigEndian.Uint32(hdr)
body := make([]byte, n) // only after checking n against a limit
if _, err := io.ReadFull(r, body); errors.Is(err, io.EOF) {
    return io.ErrUnexpectedEOF // started a frame, never finished it
}
```

## Common pitfalls

- **Bare `Read` for a fixed size.** Works on localhost, breaks under real load when data
  arrives split. Every later frame is then garbage.
- **Ignoring `n` when `err != nil`.** `Read` may return data and an error together.
- **Treating every EOF the same.** A graceful leave then looks like a crash, or the reverse.
- **Allocating before checking the length.** A bogus header of 4 GiB becomes a 4 GiB
  allocation. Check the limit first (see [Stream Framing](./stream-framing)).

## Further reading

- [Stream Framing](./stream-framing)
- [Error Wrapping & Classification](./error-wrapping-and-classification)
- Go docs: [io.ReadFull](https://pkg.go.dev/io#ReadFull)
