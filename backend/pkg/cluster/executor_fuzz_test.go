package cluster

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// instantClock fires every After at once, so a fuzzed "sleep" costs nothing
// and the clamp is observable through the output rather than the wall clock.
type instantClock struct{ RealClock }

func (instantClock) After(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

// FuzzDefaultExecutor runs the built-in task kinds on bodies a submitter (and
// every relay) controls. Properties: no panic, the output of anything but echo
// is small, and a successful output is valid UTF-8 JSON-encodable text.
func FuzzDefaultExecutor(f *testing.F) {
	for _, s := range []struct{ kind, body string }{
		{"sleep", `{"ms":200}`},
		{"sleep", `{"ms":-1}`},
		{"sleep", `{"ms":9223372036854775807}`},
		{"sleep", `{"ms":1e400}`},
		{"hash", `{"data":"abc"}`},
		{"hash", `{"data":null}`},
		{"hash", `{"data":"\ud800"}`},
		{"echo", `[1,2,3]`},
		{"nope", ``},
	} {
		f.Add(s.kind, []byte(s.body))
	}
	exec := NewDefaultExecutor(instantClock{})
	f.Fuzz(func(t *testing.T, kind string, body []byte) {
		res := exec(context.Background(), protocol.TaskPayload{TaskID: "t", Kind: kind, Body: body})
		if _, err := json.Marshal(res); err != nil {
			t.Fatalf("result not encodable: %v", err)
		}
		if res.OK && kind == "sleep" && len(res.Output) > len("slept 10000ms") {
			t.Fatalf("sleep output %q exceeds the clamp", res.Output)
		}
		if res.OK && kind == "hash" && len(res.Output) != 64 {
			t.Fatalf("hash output %q is not a sha256 hex digest", res.Output)
		}
	})
}
