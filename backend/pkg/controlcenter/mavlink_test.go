package controlcenter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"swarm-net/pkg/mavlink"
	"swarm-net/pkg/protocol"
)

// The mission state the MAVLink bridge publishes into the snapshot, which the
// dashboard draws. The shape is a contract (docs/architecture/mavlink-bridge.md
// section 5), so it is asserted as bytes and not as a struct.

func pos(x, y, z float64) *protocol.Position { return &protocol.Position{X: x, Y: y, Z: z} }

// sampleMissions is the documented example, built the way the bridge builds it.
func sampleMissions() []mavlink.MissionView {
	return []mavlink.MissionView{{
		Owner:     3,
		OwnerNode: "swarm-net-node-3",
		Exec:      3,
		ExecNode:  "swarm-net-node-3",
		Started:   true,
		Current:   2,
		Items: []mavlink.MissionItemView{
			{Seq: 0, Cmd: 16, State: mavlink.MissionItemDone, Node: "swarm-net-node-4", Pos: pos(19.4, 9.2, 30)},
			{Seq: 1, Cmd: 16, State: mavlink.MissionItemFlying, Node: "swarm-net-node-9", Pos: pos(60, 60, 25)},
			{Seq: 2, Cmd: 16, State: mavlink.MissionItemPending, Pos: pos(80, 20, 25)},
			{Seq: 3, Cmd: 206, State: mavlink.MissionItemSkipped},
		},
	}}
}

const sampleMissionsJSON = `[{"owner":3,"owner_node":"swarm-net-node-3","exec":3,"exec_node":"swarm-net-node-3",` +
	`"started":true,"current":2,"items":[` +
	`{"seq":0,"cmd":16,"state":"done","node":"swarm-net-node-4","pos":{"x":19.4,"y":9.2,"z":30}},` +
	`{"seq":1,"cmd":16,"state":"flying","node":"swarm-net-node-9","pos":{"x":60,"y":60,"z":25}},` +
	`{"seq":2,"cmd":16,"state":"pending","node":"","pos":{"x":80,"y":20,"z":25}},` +
	`{"seq":3,"cmd":206,"state":"skipped","node":""}]}]`

// publishMissions plays the bridge's part: the push crosses from the bridge's
// loop into the hub.
func publishMissions(t *testing.T, cc *testCC, ms []mavlink.MissionView) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := (mavSource{cc.srv}).SetMissions(ctx, ms); err != nil {
		t.Fatalf("SetMissions: %v", err)
	}
}

// rawSnapshot reads one snapshot and returns its top-level keys unparsed, so
// an assertion can be about the bytes a browser receives.
func rawSnapshot(t *testing.T, b *browser, what string, pred func(map[string]json.RawMessage) bool) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	b.until(what, func(m wsMsg, data []byte) bool {
		if m.Type != TypeSnapshot {
			return false
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(data, &keys); err != nil {
			t.Fatalf("snapshot is not an object: %v", err)
		}
		if !pred(keys) {
			return false
		}
		out = keys
		return true
	})
	return out
}

// TestSnapshotMissionShape pins the JSON the dashboard is written against.
func TestSnapshotMissionShape(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)

	// Absent, not null and not an empty array, while there is no mission: a
	// dashboard tests for the key.
	keys := rawSnapshot(t, b, "a snapshot", func(map[string]json.RawMessage) bool { return true })
	if raw, ok := keys["missions"]; ok {
		t.Errorf("missions = %s on a swarm with no mission, want the key absent", raw)
	}
	for _, k := range []string{"nodes", "tasks", "sim"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("the snapshot lost its %q key", k)
		}
	}

	publishMissions(t, cc, sampleMissions())
	keys = rawSnapshot(t, b, "a snapshot with missions", func(m map[string]json.RawMessage) bool {
		_, ok := m["missions"]
		return ok
	})
	if got := string(keys["missions"]); got != sampleMissionsJSON {
		t.Errorf("missions JSON changed; the dashboard is written against this shape.\ngot  %s\nwant %s",
			got, sampleMissionsJSON)
	}

	// A cleared mission clears the key again, rather than leaving the
	// dashboard drawing the last plan for ever.
	publishMissions(t, cc, nil)
	rawSnapshot(t, b, "a snapshot without missions", func(m map[string]json.RawMessage) bool {
		_, ok := m["missions"]
		return !ok
	})
}

// TestSnapshotTruncatedMission: truncation is visible in the JSON, so a
// dashboard can say the plan is longer than what it drew.
func TestSnapshotTruncatedMission(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)

	ms := sampleMissions()
	ms[0].Truncated = true
	publishMissions(t, cc, ms)

	keys := rawSnapshot(t, b, "a truncated mission", func(m map[string]json.RawMessage) bool {
		_, ok := m["missions"]
		return ok
	})
	var got []mavlink.MissionView
	if err := json.Unmarshal(keys["missions"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Truncated {
		t.Fatalf("truncated did not survive the round trip: %s", keys["missions"])
	}
}

// TestSnapshotNeverWaitsOnTheBridge is the ordering rule: the snapshot is built
// on the hub goroutine, and mission state is owned by the bridge's loop, so the
// hub serves the LAST VIEW IT WAS GIVEN and never calls the bridge.
//
// The stand-in bridge here pushes one view and then wedges: it is inside a call
// that will not return until the test releases it, and it will never push
// again. A hub that pulled from the bridge instead of serving its last push
// would block here -- the pull would wait on a loop that is stuck -- and every
// browser's snapshot would stop with it. Instead the snapshots keep arriving,
// carrying the view from before the wedge.
func TestSnapshotNeverWaitsOnTheBridge(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)

	publishMissions(t, cc, sampleMissions())

	// The wedge. A real bridge loop busy with a ground station looks like this
	// from the hub's side: no answer, for as long as it takes.
	release := make(chan struct{})
	wedged := make(chan struct{})
	go func() {
		close(wedged)
		<-release
	}()
	defer close(release)
	<-wedged

	// Three consecutive snapshots, each one a full SnapshotInterval apart,
	// all still carrying the pushed view.
	for i := 0; i < 3; i++ {
		keys := rawSnapshot(t, b, "a snapshot while the bridge is wedged", func(m map[string]json.RawMessage) bool {
			_, ok := m["missions"]
			return ok
		})
		if got := string(keys["missions"]); got != sampleMissionsJSON {
			t.Fatalf("snapshot %d served %s, want the last pushed view", i, got)
		}
	}
}

// TestMissionsPushIsWired runs the real bridge beside the hub and checks the
// publishing path exists end to end: with no ground station and no mission,
// every tick publishes an empty view, so the key stays absent and nothing
// deadlocks between the two goroutines.
func TestMissionsPushIsWired(t *testing.T) {
	cc := startCC(t, func(c *Config) {
		c.MAVLink = &mavlink.BridgeConfig{
			Bind: "127.0.0.1:0",
			// Faster than the snapshot tick, so several pushes land inside the
			// window the assertions below read.
			SyncInterval: 5 * time.Millisecond,
			Logger:       quietLog,
		}
	})
	b := dialWS(t, cc)

	for i := 0; i < 3; i++ {
		keys := rawSnapshot(t, b, "a snapshot", func(map[string]json.RawMessage) bool { return true })
		if raw, ok := keys["missions"]; ok {
			t.Fatalf("the bridge published %s with no mission uploaded", raw)
		}
	}
	if cc.srv.MAVAddr() == nil {
		t.Error("the bridge is not listening")
	}
}
