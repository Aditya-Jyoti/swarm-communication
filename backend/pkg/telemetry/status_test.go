package telemetry

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/protocol"
)

func TestFromStatus(t *testing.T) {
	s := cluster.Status{
		Self:   "node-1",
		Role:   cluster.RoleLeader,
		Term:   3,
		Leader: "node-1",
		View: cluster.View{Members: []cluster.Member{
			{ID: "node-1", Addr: "node-1:7000", Role: cluster.RoleLeader, State: cluster.StateAlive, Incarnation: 4, Score: 0.1},
			{ID: "node-2", Addr: "node-2:7000", Role: cluster.RoleWorker, State: cluster.StateSuspect, Incarnation: 5, Score: 0.4},
			{ID: "node-3", Addr: "node-3:7000", Role: cluster.RoleWorker, State: cluster.StateAlive, Incarnation: 6, Score: math.NaN()},
		}},
		Scores: map[protocol.NodeID]float64{
			"node-1": 0.1,         // self: no peer entry, dropped
			"node-2": 0.4,         // kept
			"node-3": math.Inf(1), // unmeasurable, dropped
			"node-9": 0.2,         // not in view, no address, dropped
		},
		Dropped:       7,
		ControlStatus: cluster.ControlStatus{Ledger: make([]protocol.TaskRecord, 2), ChaosDelay: 300 * time.Millisecond},
	}
	p := FromStatus(s)

	if p.Node != "node-1" || p.Role != "leader" || p.State != "alive" || p.Term != 3 || p.Leader != "node-1" {
		t.Fatalf("identity fields wrong: %+v", p)
	}
	if !p.Degraded || p.Dropped != 7 || p.LedgerSize != 2 {
		t.Fatalf("degraded/dropped/ledger wrong: %+v", p)
	}
	if len(p.Peers) != 2 || p.Peers[0].ID != "node-2" || p.Peers[1].ID != "node-3" {
		t.Fatalf("peers should be node-2, node-3 (self excluded): %+v", p.Peers)
	}
	want := protocol.MemberRecord{ID: "node-2", Advertise: "node-2:7000", Incarnation: 5, Role: "worker", State: "suspect", Score: 0.4}
	if p.Peers[0] != want {
		t.Fatalf("peer record = %+v, want %+v", p.Peers[0], want)
	}
	if len(p.Scores) != 1 || p.Scores["node-2:7000"] != 0.4 {
		t.Fatalf("scores = %v, want only node-2:7000", p.Scores)
	}
	// The NaN peer score must not break the encode: that is the whole point.
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("payload does not marshal: %v", err)
	}
}

func TestFromStatusEmpty(t *testing.T) {
	p := FromStatus(cluster.Status{Self: "solo"})
	if p.Role != "worker" || p.State != "alive" || p.Degraded {
		t.Fatalf("zero status mapped to %+v", p)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["peers"]) != "[]" || string(m["scores"]) != "{}" {
		t.Fatalf("empty collections must encode as [] and {}: %s", b)
	}
}

func TestParseChaos(t *testing.T) {
	cases := []struct {
		in      protocol.ChaosPayload
		want    Chaos
		wantErr bool
	}{
		{protocol.ChaosPayload{Action: "kill"}, Chaos{Action: ChaosKill}, false},
		{protocol.ChaosPayload{Action: "clear", DelayMS: 50}, Chaos{Action: ChaosClear}, false},
		{protocol.ChaosPayload{Action: "delay"}, Chaos{Action: ChaosDelay}, false},
		{protocol.ChaosPayload{Action: "delay", DelayMS: 300}, Chaos{Action: ChaosDelay, Delay: 300 * time.Millisecond}, false},
		{protocol.ChaosPayload{Action: "delay", DelayMS: 5000}, Chaos{Action: ChaosDelay, Delay: MaxChaosDelay}, false},
		{protocol.ChaosPayload{Action: "delay", DelayMS: 5001}, Chaos{}, true},
		{protocol.ChaosPayload{Action: "delay", DelayMS: -1}, Chaos{}, true},
		// DelayMS * 1ms overflows int64 for these. The first wraps negative,
		// the second wraps to ~448us; both used to be accepted.
		{protocol.ChaosPayload{Action: "delay", DelayMS: 9223372036855}, Chaos{}, true},
		{protocol.ChaosPayload{Action: "delay", DelayMS: 18446744073710}, Chaos{}, true},
		{protocol.ChaosPayload{Action: "delay", DelayMS: math.MaxInt}, Chaos{}, true},
		{protocol.ChaosPayload{Action: "reboot"}, Chaos{}, true},
		{protocol.ChaosPayload{}, Chaos{}, true},
	}
	for _, tc := range cases {
		got, err := ParseChaos(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseChaos(%+v) err = %v, wantErr %t", tc.in, err, tc.wantErr)
			continue
		}
		if err != nil && !errors.Is(err, ErrInvalidChaos) {
			t.Errorf("ParseChaos(%+v) err %v does not wrap ErrInvalidChaos", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseChaos(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
