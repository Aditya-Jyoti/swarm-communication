package protocol

import (
	"strings"
	"testing"
)

// unmarshalable is a payload encoding/json cannot represent. A channel is the
// cheapest such value and keeps the test free of custom MarshalJSON machinery.
type unmarshalable struct {
	Ch chan int `json:"ch"`
}

func TestSetPayloadRejectsUnmarshalableValue(t *testing.T) {
	e := &Envelope{Version: CurrentVersion, Type: TypeTask}

	err := SetPayload(e, unmarshalable{Ch: make(chan int)})
	if err == nil {
		t.Fatal("SetPayload accepted a value json cannot marshal")
	}
	// The message must name the message type, because this error surfaces far from
	// the call site and "json: unsupported type" alone identifies nothing.
	if !strings.Contains(err.Error(), string(TypeTask)) {
		t.Errorf("error does not identify the message type: %v", err)
	}
	if e.Payload != nil {
		t.Errorf("Payload was mutated despite the marshal failure: %q", e.Payload)
	}
}

// A failed payload marshal must abort envelope construction rather than return a
// half-built envelope that would later be framed and sent.
func TestNewEnvelopePropagatesPayloadError(t *testing.T) {
	env, err := NewEnvelope(TypeTask, "node-1", "node-2", unmarshalable{Ch: make(chan int)})
	if err == nil {
		t.Fatal("NewEnvelope accepted an unmarshalable payload")
	}
	if env != nil {
		t.Errorf("NewEnvelope returned a non-nil envelope alongside an error: %+v", env)
	}
}

func TestNewReplyPropagatesPayloadError(t *testing.T) {
	req := mustEnvelope(t, TypePing, "node-1", "node-2", PingPayload{Nonce: 7})

	reply, err := NewReply(req, TypePong, "node-2", unmarshalable{Ch: make(chan int)})
	if err == nil {
		t.Fatal("NewReply accepted an unmarshalable payload")
	}
	if reply != nil {
		t.Errorf("NewReply returned a non-nil envelope alongside an error: %+v", reply)
	}
}

// NewReply must address the response back to the requester. Getting this wrong
// would route a PONG to whoever the request was addressed TO, not who sent it.
func TestNewReplyAddressesRequester(t *testing.T) {
	req := mustEnvelope(t, TypePing, "node-7", "node-3", PingPayload{Nonce: 1})

	reply, err := NewReply(req, TypePong, "node-3", PongPayload{Nonce: 1})
	if err != nil {
		t.Fatalf("NewReply: %v", err)
	}
	if reply.To != req.From {
		t.Errorf("reply.To = %q, want the requester %q", reply.To, req.From)
	}
	if reply.From != "node-3" {
		t.Errorf("reply.From = %q, want node-3", reply.From)
	}
	if reply.ID != req.ID {
		t.Errorf("reply.ID = %q, want the request's ID %q", reply.ID, req.ID)
	}
}
