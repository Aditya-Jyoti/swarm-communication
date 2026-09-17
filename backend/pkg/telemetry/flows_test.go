package telemetry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// stubTransport is a Transport with scripted results. It does not implement
// network.RecipientBroadcaster; exactTransport below does.
type stubTransport struct {
	peers     []network.PeerInfo
	sendErr   map[protocol.NodeID]error
	broadcast int
	events    chan network.PeerEvent
}

func (s *stubTransport) Self() protocol.NodeID { return "self" }
func (s *stubTransport) Send(_ context.Context, to protocol.NodeID, env *protocol.Envelope) error {
	if env == nil {
		return errors.New("nil envelope")
	}
	return s.sendErr[to]
}
func (s *stubTransport) Broadcast(context.Context, *protocol.Envelope) int { return s.broadcast }
func (s *stubTransport) Peers() []network.PeerInfo                         { return s.peers }
func (s *stubTransport) Events() <-chan network.PeerEvent                  { return s.events }

type exactTransport struct {
	stubTransport
	recipients []protocol.NodeID
}

func (e *exactTransport) BroadcastRecipients(context.Context, *protocol.Envelope) []protocol.NodeID {
	return slices.Clone(e.recipients)
}

func env(t *testing.T, typ protocol.MessageType) *protocol.Envelope {
	t.Helper()
	e, err := protocol.NewEnvelope(typ, "self", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func peersOf(ids ...protocol.NodeID) []network.PeerInfo {
	out := make([]network.PeerInfo, len(ids))
	for i, id := range ids {
		out[i] = network.PeerInfo{ID: id}
	}
	return out
}

func TestFlowRecorderCountsSends(t *testing.T) {
	inner := &stubTransport{
		sendErr: map[protocol.NodeID]error{"down": network.ErrUnknownPeer},
		events:  make(chan network.PeerEvent),
		peers:   peersOf("b"),
	}
	r := NewFlowRecorder(inner)
	ctx := context.Background()

	if r.Self() != "self" || r.Events() != (<-chan network.PeerEvent)(inner.events) || len(r.Peers()) != 1 {
		t.Fatal("pass-through methods do not reach the wrapped transport")
	}

	for range 3 {
		if err := r.Send(ctx, "b", env(t, protocol.TypePing)); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Send(ctx, "b", env(t, protocol.TypePong))
	if err := r.Send(ctx, "down", env(t, protocol.TypePing)); !errors.Is(err, network.ErrUnknownPeer) {
		t.Fatalf("error not passed through: %v", err)
	}
	if err := r.Send(ctx, "b", nil); err == nil {
		t.Fatal("nil envelope accepted")
	}

	got := r.Drain()
	want := []protocol.FlowRecord{
		{To: "b", Type: protocol.TypePing, Count: 3},
		{To: "b", Type: protocol.TypePong, Count: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Drain = %+v, want %+v", got, want)
	}
	// Drain resets.
	if got := r.Drain(); got == nil || len(got) != 0 {
		t.Fatalf("second Drain = %#v, want empty non-nil", got)
	}
}

func TestFlowRecorderBroadcastExact(t *testing.T) {
	inner := &exactTransport{recipients: []protocol.NodeID{"c", "a"}}
	inner.peers = peersOf("a", "b", "c") // must not be consulted
	r := NewFlowRecorder(inner)

	got := r.Broadcast(context.Background(), env(t, protocol.TypeHeartbeat))
	if got != 2 {
		t.Fatalf("Broadcast = %d, want 2", got)
	}
	if n := r.Broadcast(context.Background(), nil); n != 0 {
		t.Fatalf("Broadcast(nil) = %d", n)
	}
	want := []protocol.FlowRecord{
		{To: "a", Type: protocol.TypeHeartbeat, Count: 1},
		{To: "c", Type: protocol.TypeHeartbeat, Count: 1},
	}
	if d := r.Drain(); !slices.Equal(d, want) {
		t.Fatalf("Drain = %+v, want %+v", d, want)
	}
}

func TestFlowRecorderBroadcastApproximate(t *testing.T) {
	inner := &stubTransport{peers: peersOf("a", "b", "c"), broadcast: 3}
	r := NewFlowRecorder(inner)
	ctx := context.Background()

	if n := r.Broadcast(ctx, env(t, protocol.TypeElectionResult)); n != 3 {
		t.Fatalf("Broadcast = %d", n)
	}
	// Two accepted out of three: attributed to the first two listed peers.
	inner.broadcast = 2
	if got := r.BroadcastRecipients(ctx, env(t, protocol.TypeElectionResult)); !slices.Equal(got, []protocol.NodeID{"a", "b"}) {
		t.Fatalf("recipients = %v", got)
	}
	// None accepted: nothing counted.
	inner.broadcast = 0
	r.Broadcast(ctx, env(t, protocol.TypeElectionResult))

	want := []protocol.FlowRecord{
		{To: "a", Type: protocol.TypeElectionResult, Count: 2},
		{To: "b", Type: protocol.TypeElectionResult, Count: 2},
		{To: "c", Type: protocol.TypeElectionResult, Count: 1},
	}
	if d := r.Drain(); !slices.Equal(d, want) {
		t.Fatalf("Drain = %+v, want %+v", d, want)
	}
}

// Stacked recorders both see exact recipients.
func TestFlowRecorderStacks(t *testing.T) {
	inner := &exactTransport{recipients: []protocol.NodeID{"x"}}
	lower := NewFlowRecorder(inner)
	upper := NewFlowRecorder(lower)
	upper.Broadcast(context.Background(), env(t, protocol.TypeMembershipDelta))
	for _, r := range []*FlowRecorder{lower, upper} {
		if d := r.Drain(); len(d) != 1 || d[0].To != "x" || d[0].Count != 1 {
			t.Fatalf("Drain = %+v", d)
		}
	}
}

// Over the cap, the busiest pairs are kept, in a deterministic order.
func TestFlowRecorderDrainCapsKeepingBusiest(t *testing.T) {
	r := NewFlowRecorder(&stubTransport{})
	ctx := context.Background()
	total := protocol.MaxFlowRecords + 50
	for i := range total {
		to := protocol.NodeID(fmt.Sprintf("n-%04d", i))
		// Peers with a higher index get more frames.
		for range 1 + i/10 {
			_ = r.Send(ctx, to, env(t, protocol.TypePing))
		}
	}
	d := r.Drain()
	if len(d) != protocol.MaxFlowRecords {
		t.Fatalf("len = %d, want %d", len(d), protocol.MaxFlowRecords)
	}
	if cap(d) != len(d) {
		t.Errorf("cap = %d, want clipped to len", cap(d))
	}
	for i := 1; i < len(d); i++ {
		a, b := d[i-1], d[i]
		if a.Count < b.Count || (a.Count == b.Count && a.To > b.To) {
			t.Fatalf("not sorted at %d: %+v then %+v", i, a, b)
		}
	}
	// The quietest kept entry is at least as busy as anything dropped: the
	// 50 dropped are the lowest indices, count 1..5.
	if last := d[len(d)-1]; last.Count < 1+49/10 {
		t.Fatalf("kept a quiet pair %+v", last)
	}
	// The top count is shared by indices (total-1)/10*10 onwards; the tie-break
	// by name puts the lowest of those first.
	if d[0].To != protocol.NodeID(fmt.Sprintf("n-%04d", (total-1)/10*10)) {
		t.Fatalf("busiest first = %+v", d[0])
	}
}

// Concurrent senders, broadcasters and drainers: counts are conserved. Run
// under -race.
func TestFlowRecorderConcurrent(t *testing.T) {
	inner := &exactTransport{recipients: []protocol.NodeID{"a", "b"}}
	r := NewFlowRecorder(inner)
	ctx := context.Background()
	ping := env(t, protocol.TypePing)
	hb := env(t, protocol.TypeHeartbeat)

	const workers, per = 4, 250
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	add := func(recs []protocol.FlowRecord) {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range recs {
			total += f.Count
		}
	}
	done := make(chan struct{})
	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		for {
			select {
			case <-done:
				return
			default:
				add(r.Drain())
			}
		}
	}()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				_ = r.Send(ctx, "a", ping)
				r.Broadcast(ctx, hb)
			}
		}()
	}
	wg.Wait()
	close(done)
	<-drainerDone
	add(r.Drain())
	if want := workers * per * 3; total != want {
		t.Fatalf("counted %d frames, want %d", total, want)
	}
}
