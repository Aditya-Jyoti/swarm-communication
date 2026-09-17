package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DumpStream reads length-prefixed frames from r and writes an annotated,
// human-readable rendering of each one to w. It returns nil at a clean end of
// stream and an error otherwise.
//
// # Why this exists
//
// Newline-delimited JSON was rejected as the wire format (see docs/WORKLOG.md
// §2.1), and the one genuine thing that decision cost was readability: with NDJSON
// you can point `nc` or `tcpdump -A` at a connection and simply read the
// conversation. Length-prefixed frames put a 4-byte binary header in front of
// every message, which is enough to make a terminal dump unreadable and, if the
// length happens to contain a newline byte, actively misleading.
//
// This function buys that back. It is the debugging affordance that makes the
// framing decision affordable, and writing it was part of accepting that decision
// rather than an afterthought.
//
// # This is NOT production code
//
// It is a diagnostic tool, intended to be driven from a test, a `go run` one-liner
// over a captured file, or a debug endpoint — never wired into the swarm's normal
// data path. Specifically:
//
//   - It re-implements the frame read instead of calling Decoder.ReadFrame,
//     because it must report the raw bytes of frames that ReadFrame would reject
//     outright (a bad version, an unknown type, a body that is not valid JSON).
//     A diagnostic that refuses to show you the broken frame is useless exactly
//     when you need it.
//   - It pretty-prints, which allocates freely.
//   - It has no deadline, so pointing it at a live socket inherits the
//     block-forever behaviour documented on Decoder.
//
// Because it is a diagnostic, it is deliberately more permissive than the codec:
// it prints and continues past a malformed *body*. It still stops dead at a bad
// length header, because at that point the stream position is unknown and
// everything after it would be fiction.
func DumpStream(r io.Reader, w io.Writer) error {
	var (
		offset int64
		index  int
		hdr    [headerSize]byte
	)

	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Fprintf(w, "-- end of stream at offset %d after %d frame(s)\n", offset, index)
				return nil
			}
			return fmt.Errorf("dump: header at offset %d: %w", offset, err)
		}

		n := binary.BigEndian.Uint32(hdr[:])
		frameStart := offset
		offset += headerSize

		// Same ordering discipline as ReadFrame: validate the declared length
		// before allocating for it. A debugging tool is not exempt from this, and
		// is in fact more likely to be pointed at a stream that is garbage.
		if n == 0 {
			fmt.Fprintf(w, "[%04d] offset=%d ERROR zero-length frame (protocol violation)\n", index, frameStart)
			return ErrZeroLengthFrame
		}
		if n > MaxFrameSize {
			fmt.Fprintf(w, "[%04d] offset=%d ERROR declared length %d exceeds max %d\n", index, frameStart, n, MaxFrameSize)
			return fmt.Errorf("dump: %w", ErrFrameTooLarge)
		}

		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			fmt.Fprintf(w, "[%04d] offset=%d len=%d ERROR truncated payload\n", index, frameStart, n)
			return fmt.Errorf("dump: payload at offset %d: %w", frameStart+headerSize, err)
		}
		offset += int64(n)

		var env Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			// Print the raw bytes and keep going. The stream is still on a frame
			// boundary — we consumed exactly n bytes — so continuing is safe here,
			// unlike in ReadFrame where the caller has a connection to protect.
			fmt.Fprintf(w, "[%04d] offset=%d len=%d UNPARSEABLE %s\n       raw: %q\n",
				index, frameStart, n, err, truncate(body, 240))
			index++
			continue
		}

		plane := "control"
		switch {
		case env.Type.IsDataPlane():
			plane = "data"
		case !env.Type.Valid():
			plane = "UNKNOWN-TYPE"
		}

		fmt.Fprintf(w, "[%04d] offset=%d len=%d v=%d %s (%s) %s -> %s id=%s sent_at=%d\n",
			index, frameStart, n, env.Version, env.Type, plane,
			orDash(string(env.From)), orDash(string(env.To)), orDash(env.ID), env.SentAtUnixNano)

		if len(env.Payload) > 0 {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, env.Payload, "       ", "  "); err != nil {
				fmt.Fprintf(w, "       payload (raw): %q\n", truncate(env.Payload, 240))
			} else {
				fmt.Fprintf(w, "       payload: %s\n", pretty.String())
			}
		}

		index++
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
