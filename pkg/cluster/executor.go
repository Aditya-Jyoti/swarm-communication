package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"swarm-net/pkg/protocol"
)

// MaxSleepMS caps the built-in "sleep" task, per the control-plane contract.
const MaxSleepMS = 10000

// NewDefaultExecutor returns the built-in task kinds from the control-plane
// contract:
//
//	echo   any JSON        -> the body, verbatim
//	sleep  {"ms": 200}     -> "slept 200ms" (ms clamped to [0, MaxSleepMS])
//	hash   {"data": "abc"} -> hex SHA-256 of data
//
// Anything else fails with "unknown task kind: X". A body the kind cannot use
// fails with a message saying why; nothing panics and nothing is dropped.
//
// sleep waits on clock, not on package time, so a test drives it with a
// FakeClock, and it returns early (as a failure) when ctx ends, so a node
// shutting down is never held up by one. The returned result carries only OK
// and Output; the node fills in TaskID, Worker and DurationMS.
func NewDefaultExecutor(clock Clock) TaskExecutor {
	return func(ctx context.Context, t protocol.TaskPayload) protocol.TaskResultPayload {
		out, err := runBuiltin(ctx, clock, t)
		if err != nil {
			return protocol.TaskResultPayload{OK: false, Output: err.Error()}
		}
		return protocol.TaskResultPayload{OK: true, Output: out}
	}
}

func runBuiltin(ctx context.Context, clock Clock, t protocol.TaskPayload) (string, error) {
	switch t.Kind {
	case "echo":
		return string(t.Body), nil
	case "sleep":
		var b struct {
			MS int64 `json:"ms"`
		}
		if err := decodeBody(t.Body, &b); err != nil {
			return "", fmt.Errorf("sleep: %w", err)
		}
		ms := min(max(b.MS, 0), MaxSleepMS)
		if ms > 0 {
			select {
			case <-clock.After(time.Duration(ms) * time.Millisecond):
			case <-ctx.Done():
				return "", fmt.Errorf("sleep interrupted: %w", ctx.Err())
			}
		}
		return fmt.Sprintf("slept %dms", ms), nil
	case "hash":
		var b struct {
			Data *string `json:"data"`
		}
		if err := decodeBody(t.Body, &b); err != nil {
			return "", fmt.Errorf("hash: %w", err)
		}
		if b.Data == nil {
			return "", fmt.Errorf(`hash: body needs a "data" string`)
		}
		sum := sha256.Sum256([]byte(*b.Data))
		return hex.EncodeToString(sum[:]), nil
	default:
		return "", fmt.Errorf("unknown task kind: %s", t.Kind)
	}
}

// decodeBody reads a JSON object body. A missing body is an empty object, so
// {"kind":"sleep"} sleeps for zero milliseconds rather than failing.
func decodeBody(body json.RawMessage, v any) error {
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("bad body: %w", err)
	}
	return nil
}
