package protocol

import (
	"encoding/json"
	"errors"
	"testing"
)

// FuzzPayloadOf feeds peer-controlled payload bytes to every payload type a
// node or the control center decodes from the wire. ReadFrame only vouches for
// the envelope; the body is a json.RawMessage until PayloadOf runs, so this is
// the second parser a hostile peer reaches.
//
// Properties: no panic; every failure is a protocol violation (so the caller
// drops the peer instead of mis-classifying it); anything that decodes can be
// encoded again, because nodes relay these values (deltas, ledgers).
func FuzzPayloadOf(f *testing.F) {
	seeds := []string{
		`{"advertise":"n:7000","incarnation":1,"known_peers":["a:1","b:2"]}`,
		`{"members":[{"id":"n","advertise":"n:1","incarnation":9223372036854775807,"role":"leader","state":"alive","score":-1,"seq":18446744073709551615}],"view_version":1}`,
		`{"members":[{"score":1e308},{"score":-0},{"id":""}]}`,
		`{"task_id":"t-1","kind":"hash","body":{"data":"x"}}`,
		`{"task_id":"t","kind":"echo","body":"` + "\\u0000\\ud800" + `"}`,
		`{"term":1,"leader":"n","workers":null,"ledger":[{"task_id":"t","body":[1,2]}],"version":1}`,
		`{"node":"n","peers":null,"scores":{"a:1":1e-300}}`,
		`{"action":"delay","delay_ms":-9223372036854775808}`,
		`null`, `[]`, `"s"`, `{"members":{}}`, `{`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		env := &Envelope{Version: CurrentVersion, Type: TypeMembershipDelta, Payload: data}
		checkPayload[HelloPayload](t, env)
		checkPayload[HelloAckPayload](t, env)
		checkPayload[MembershipDeltaPayload](t, env)
		checkPayload[TaskPayload](t, env)
		checkPayload[TaskResultPayload](t, env)
		checkPayload[StateSyncPayload](t, env)
		checkPayload[TelemetryPayload](t, env)
		checkPayload[ChaosPayload](t, env)
		checkPayload[ElectionResultPayload](t, env)
	})
}

func checkPayload[T any](t *testing.T, env *Envelope) {
	t.Helper()
	v, err := PayloadOf[T](env)
	if err != nil {
		if !errors.Is(err, ErrMalformedFrame) || !IsProtocolViolation(err) {
			t.Fatalf("%T: error %v is not a protocol violation", v, err)
		}
		return
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatalf("%T decoded from %q but cannot be re-encoded: %v", v, env.Payload, err)
	}
}
