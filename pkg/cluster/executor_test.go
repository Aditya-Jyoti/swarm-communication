package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

func TestDefaultExecutorKinds(t *testing.T) {
	exec := NewDefaultExecutor(NewFakeClock(epoch))
	tests := []struct {
		name   string
		kind   string
		body   string
		ok     bool
		output string
	}{
		{"echo object", "echo", `{"a": [1, 2]}`, true, `{"a": [1, 2]}`},
		{"echo string", "echo", `"hi"`, true, `"hi"`},
		{"echo nothing", "echo", ``, true, ``},
		{"sleep zero", "sleep", `{"ms": 0}`, true, "slept 0ms"},
		{"sleep negative clamps to zero", "sleep", `{"ms": -5}`, true, "slept 0ms"},
		{"sleep no body", "sleep", ``, true, "slept 0ms"},
		{"sleep bad body", "sleep", `{"ms": "soon"}`, false, "sleep: bad body"},
		{"hash abc", "hash", `{"data": "abc"}`, true, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"hash empty string", "hash", `{"data": ""}`, true, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"hash missing data", "hash", `{}`, false, `hash: body needs a "data" string`},
		{"hash bad body", "hash", `[1]`, false, "hash: bad body"},
		{"unknown", "frobnicate", `{}`, false, "unknown task kind: frobnicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := exec(context.Background(), protocol.TaskPayload{TaskID: "t", Kind: tt.kind, Body: json.RawMessage(tt.body)})
			if res.OK != tt.ok || !strings.HasPrefix(res.Output, tt.output) {
				t.Fatalf("result = %+v, want ok=%v output %q", res, tt.ok, tt.output)
			}
		})
	}
}

// sleep waits on the injected Clock, exactly as long as asked, capped.
func TestDefaultExecutorSleepUsesTheClock(t *testing.T) {
	for _, tt := range []struct {
		ms   string
		wait time.Duration
		out  string
	}{
		{"200", 200 * time.Millisecond, "slept 200ms"},
		{"999999", MaxSleepMS * time.Millisecond, "slept 10000ms"},
	} {
		clock := NewFakeClock(epoch)
		exec := NewDefaultExecutor(clock)
		done := make(chan protocol.TaskResultPayload, 1)
		go func() {
			done <- exec(context.Background(), protocol.TaskPayload{Kind: "sleep", Body: json.RawMessage(`{"ms":` + tt.ms + `}`)})
		}()
		waitForTimers(t, clock, 1)
		clock.Advance(tt.wait - time.Millisecond)
		select {
		case res := <-done:
			t.Fatalf("returned before its time: %+v", res)
		default:
		}
		clock.Advance(time.Millisecond)
		select {
		case res := <-done:
			if !res.OK || res.Output != tt.out {
				t.Fatalf("result = %+v, want %q", res, tt.out)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("sleep never returned")
		}
	}
}

// A cancelled sleep returns at once, as a failure.
func TestDefaultExecutorSleepHonoursContext(t *testing.T) {
	clock := NewFakeClock(epoch)
	exec := NewDefaultExecutor(clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan protocol.TaskResultPayload, 1)
	go func() {
		done <- exec(ctx, protocol.TaskPayload{Kind: "sleep", Body: json.RawMessage(`{"ms":5000}`)})
	}()
	waitForTimers(t, clock, 1)
	cancel()
	select {
	case res := <-done:
		if res.OK || !strings.Contains(res.Output, "interrupted") {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled sleep never returned")
	}
}

// waitForTimers blocks until clock has at least n timers registered: the
// goroutine under test has reached its timer. The real-time bound is only a
// hang guard; the wait itself is a condition variable, not a poll. (On a
// timeout the waiting goroutine is left parked; the test has failed anyway.)
func waitForTimers(t *testing.T, clock *FakeClock, n int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		clock.awaitTimers(n)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timers = %d, want %d", clock.pending(), n)
	}
}
