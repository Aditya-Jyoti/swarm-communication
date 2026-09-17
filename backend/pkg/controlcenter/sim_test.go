package controlcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"swarm-net/pkg/geo"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

func getSim(t *testing.T, cc *testCC) SimView {
	t.Helper()
	resp, err := http.Get(cc.http.URL + "/api/sim")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/sim = %d", resp.StatusCode)
	}
	var v SimView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func postSim(t *testing.T, cc *testCC, body string) (int, SimView) {
	t.Helper()
	resp, err := http.Post(cc.http.URL+"/api/sim", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v SimView
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("POST /api/sim body %s: %v", raw, err)
		}
	} else if !bytes.Contains(raw, []byte(`"error"`)) {
		t.Errorf("POST /api/sim %d without an error body: %s", resp.StatusCode, raw)
	}
	return resp.StatusCode, v
}

// stepRand returns 0.1, 0.2, ... so randomized layouts are predictable.
func stepRand() func() float64 {
	var i atomic.Int64
	return func() float64 { return float64(i.Add(1)%10) / 10 }
}

func TestSimAPIDefaultsValidationAndClamping(t *testing.T) {
	cc := startCC(t, nil)
	v := getSim(t, cc)
	want := SimView{Version: v.Version, Enabled: true, BaseMS: 1, PerUnitMS: 2, JitterMS: 0.5,
		Threshold: 0, Hysteresis: -1, Size: 100, MaxDelayMS: 1500}
	if v != want || v.Version == 0 {
		t.Fatalf("defaults = %+v", v)
	}

	// Out of range is clamped, not rejected. Explicit "type" is allowed.
	code, got := postSim(t, cc, `{"type":"sim","base_ms":9999,"jitter_ms":-5,"per_unit_ms":3,"hysteresis":1e9,"threshold":1,"enabled":false}`)
	if code != 200 || got.BaseMS != geo.MaxBaseMS || got.JitterMS != 0 || got.PerUnitMS != 3 ||
		got.Hysteresis != 1500 || got.Threshold != 1 || got.Enabled || got.Version <= v.Version {
		t.Fatalf("clamped update = %d %+v", code, got)
	}
	// A negative hysteresis clears the override; zero is a legal value.
	if _, got = postSim(t, cc, `{"hysteresis":-7}`); got.Hysteresis != -1 {
		t.Fatalf("negative hysteresis = %+v", got)
	}
	if _, got = postSim(t, cc, `{"hysteresis":0}`); got.Hysteresis != 0 {
		t.Fatalf("zero hysteresis = %+v", got)
	}
	// An empty update changes nothing, so it must not bump the version.
	before := getSim(t, cc)
	if code, got = postSim(t, cc, `{}`); code != 200 || got != before {
		t.Fatalf("no-op update = %d %+v, want %+v", code, got, before)
	}

	for _, body := range []string{
		`{"threshold":0}`,
		`{"threshold":1.5}`,
		`{"threshold":-0.1}`,
		`{"threshold":0.5,"base_ms":1e400}`,
		`{"base_ms":"fast"}`,
		`{"bogus":1}`,
		`{"type":"task","base_ms":1}`,
		`{"base_ms":1,"kind":"echo"}`,
		`{"node":"node-1"}`,
		`{"positions":{"":{"x":1}}}`,
		`{"positions":{"control-center":{"x":1}}}`,
		`{"positions":{"` + strings.Repeat("n", maxSimNodeIDLen+1) + `":{"x":1}}}`,
		`{"base_ms":2} {}`,
	} {
		if code, _ := postSim(t, cc, body); code != http.StatusBadRequest {
			t.Errorf("POST %.60s = %d, want 400", body, code)
		}
	}
	// A rejected request changes nothing at all.
	if after := getSim(t, cc); after != before {
		t.Fatalf("state changed by rejected requests: %+v -> %+v", before, after)
	}
	// Task and chaos routes refuse sim fields.
	if code, _ := post(t, cc, "/api/chaos", `{"node":"n","action":"kill","randomize":true}`); code != http.StatusBadRequest {
		t.Errorf("chaos with sim field = %d", code)
	}
}

func TestSimPostIsGuarded(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.APIToken = "0123456789abcdef0123" })
	resp, err := http.Post(cc.http.URL+"/api/sim", "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d", resp.StatusCode)
	}
	if code, _ := postSim(t, cc, `{"base_ms":3}`); code != http.StatusUnauthorized {
		t.Errorf("no token = %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, cc.http.URL+"/api/sim", strings.NewReader(`{"base_ms":3}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer 0123456789abcdef0123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("with token = %d", resp.StatusCode)
	}
	// Reading stays open, like /api/state.
	if v := getSim(t, cc); v.BaseMS != 3 {
		t.Errorf("GET after update = %+v", v)
	}
}

func TestSimConfigOnConnect(t *testing.T) {
	p := geo.Params{BaseMS: 7, PerUnitMS: 0, JitterMS: 1}
	cc := startCC(t, func(c *Config) { c.SimParams = &p; c.SimDisabled = true })

	// A placement for an ID that has not connected is kept (and clamped).
	if code, _ := postSim(t, cc, `{"positions":{"node-9":{"x":1,"y":2,"z":300}}}`); code != 200 {
		t.Fatalf("pre-placement = %d", code)
	}

	n := dialNode(t, cc, "node-1")
	got := n.simConfig()
	if got.Enabled || got.BaseMS != 7 || got.PerUnitMS != 0 || got.JitterMS != 1 ||
		got.Threshold != 0 || got.Hysteresis != -1 || got.Version == 0 {
		t.Fatalf("first SIM_CONFIG = %+v", got)
	}
	if pos, ok := got.Positions["node-1"]; !ok || pos != geo.DefaultPosition("node-1") {
		t.Fatalf("own position = %+v, %v; want default %+v", pos, ok, geo.DefaultPosition("node-1"))
	}

	m := dialNode(t, cc, "node-9")
	if pos := m.simConfig().Positions["node-9"]; pos != (protocol.Position{X: 1, Y: 2, Z: 100}) {
		t.Fatalf("pre-placed node got %+v", pos)
	}
	// Everyone else already had the pre-placement before node-9 arrived.
	if _, ok := got.Positions["node-9"]; !ok {
		t.Fatal("pre-placement missing from the first node's config")
	}
}

func TestSimBroadcastOnChangeAndJoin(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	a := dialNode(t, cc, "node-a")
	first := a.simConfig()
	if _, ok := first.Positions["node-b"]; ok {
		t.Fatal("node-b known before it joined")
	}

	z := dialNode(t, cc, "node-b")
	zFirst := z.simConfig()
	if zFirst.Version <= first.Version {
		t.Fatalf("join did not bump: %d then %d", first.Version, zFirst.Version)
	}
	// node-a learns node-b's position from the coalesced broadcast.
	for {
		p := a.simConfig()
		if _, ok := p.Positions["node-b"]; ok {
			if p.Version < zFirst.Version {
				t.Fatalf("broadcast version %d older than the join's %d", p.Version, zFirst.Version)
			}
			break
		}
	}

	if code, v := postSim(t, cc, `{"per_unit_ms":3}`); code != 200 || v.PerUnitMS != 3 {
		t.Fatalf("update = %d %+v", code, v)
	}
	if e := b.event(EventSim, ""); e.Detail != "per_unit_ms 2 -> 3" {
		t.Fatalf("sim event = %+v", e)
	}
	want := getSim(t, cc).Version
	for _, n := range []*fakeNode{a, z} {
		for {
			p := n.simConfig()
			if p.Version == want {
				if p.PerUnitMS != 3 || len(p.Positions) != 2 {
					t.Fatalf("%s got %+v", n.id, p)
				}
				break
			}
			if p.Version > want {
				t.Fatalf("%s got version %d beyond %d", n.id, p.Version, want)
			}
		}
	}
	// The snapshot reports the same config.
	s := b.snapshotWhere("sim in snapshot", func(s Snapshot) bool { return s.Sim.Version == want })
	if s.Sim.PerUnitMS != 3 || s.Sim.Size != geo.Size {
		t.Fatalf("snapshot sim = %+v", s.Sim)
	}
}

func TestSimMoveRandomizeReset(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Rand = stepRand() })
	b := dialWS(t, cc)
	n := dialNode(t, cc, "node-3")
	n.simConfig()

	if code, _ := postSim(t, cc, `{"positions":{"node-3":{"x":10,"y":80,"z":-4}}}`); code != 200 {
		t.Fatal(code)
	}
	if e := b.event(EventSim, "node-3"); e.Detail != "moved node-3" {
		t.Fatalf("move event = %+v", e)
	}
	if v, _ := findNode(getState(t, cc), "node-3"); v.Pos != (protocol.Position{X: 10, Y: 80, Z: 0}) {
		t.Fatalf("moved pos = %+v", v.Pos)
	}
	// Moving to the same place is not a change.
	before := getSim(t, cc).Version
	if _, v := postSim(t, cc, `{"positions":{"node-3":{"x":10,"y":80,"z":0}}}`); v.Version != before {
		t.Fatal("same position bumped the version")
	}

	if code, _ := postSim(t, cc, `{"randomize":true}`); code != 200 {
		t.Fatal(code)
	}
	if e := b.event(EventSim, ""); e.Detail != "randomized 1 position" {
		t.Fatalf("randomize event = %+v", e)
	}
	if v, _ := findNode(getState(t, cc), "node-3"); v.Pos != (protocol.Position{X: 10, Y: 20, Z: 30}) {
		t.Fatalf("randomized pos = %+v", v.Pos)
	}

	// Reset then pin in one request: explicit positions win.
	if code, _ := postSim(t, cc, `{"reset_positions":true,"positions":{"node-4":{"x":5,"y":5,"z":5}}}`); code != 200 {
		t.Fatal(code)
	}
	if e := b.event(EventSim, "node-4"); e.Detail != "reset 1 position, moved node-4" {
		t.Fatalf("reset event = %+v", e)
	}
	if v, _ := findNode(getState(t, cc), "node-3"); v.Pos != geo.DefaultPosition("node-3") {
		t.Fatalf("reset pos = %+v", v.Pos)
	}
	p := n.simConfig()
	for p.Version != getSim(t, cc).Version {
		p = n.simConfig()
	}
	if p.Positions["node-4"] != (protocol.Position{X: 5, Y: 5, Z: 5}) || p.Positions["node-3"] != geo.DefaultPosition("node-3") {
		t.Fatalf("node config after reset = %+v", p.Positions)
	}
}

func TestSimWSMessage(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	b.write(map[string]any{"type": "sim", "threshold": 2}) // out of range: dropped
	b.write(map[string]any{"type": "task", "base_ms": 3})  // wrong fields: dropped
	b.write(map[string]any{"type": "sim", "kind": "echo"}) // wrong fields: dropped
	b.write(map[string]any{"type": "sim", "per_unit_ms": 4, "threshold": 0.4})
	e := b.event(EventSim, "")
	if e.Detail != "per_unit_ms 2 -> 4, threshold 0 -> 0.4" {
		t.Fatalf("event = %+v", e)
	}
	s := b.snapshotWhere("ws update", func(s Snapshot) bool { return s.Sim.PerUnitMS == 4 })
	if s.Sim.Threshold != 0.4 || s.Sim.BaseMS != 1 {
		t.Fatalf("sim = %+v", s.Sim)
	}
}

func TestSnapshotShape(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	n := dialNode(t, cc, "node-1")
	// Before any telemetry every new key is present, flows is [].
	b.snapshotWhere("listed", func(s Snapshot) bool { _, ok := findNode(s, "node-1"); return ok })
	resp, err := http.Get(cc.http.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, key := range []string{
		`"sim":{"version":`, `"enabled":true`, `"base_ms":1,`, `"per_unit_ms":2,`, `"jitter_ms":0.5,`,
		`"threshold":0,`, `"hysteresis":-1,`, `"size":100,`, `"max_delay_ms":1500}`,
		`"pos":{"x":`, `"sim_version":0`, `"flows":[]`,
	} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Errorf("snapshot lacks %s: %s", key, raw)
		}
	}
	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("snapshot has a null: %s", raw)
	}

	n.send(protocol.TypeTelemetry, protocol.TelemetryPayload{
		Role: "worker", State: "alive", Threshold: 0.3, Hysteresis: 0.5, SimVersion: 7,
		Flows: []protocol.FlowRecord{{To: "node-2", Type: protocol.TypeHeartbeat, Count: 2}},
	})
	s := b.snapshotWhere("telemetry", func(s Snapshot) bool { v, _ := findNode(s, "node-1"); return v.SimVersion == 7 })
	v, _ := findNode(s, "node-1")
	if v.Threshold != 0.3 || v.Hysteresis != 0.5 || len(v.Flows) != 1 || v.Flows[0].Count != 2 || v.Flows[0].To != "node-2" {
		t.Fatalf("view = %+v", v)
	}
	// A sample without flows replaces the old one: nothing lingers.
	n.send(protocol.TypeTelemetry, protocol.TelemetryPayload{Role: "worker", State: "alive", SimVersion: 8})
	s = b.snapshotWhere("second sample", func(s Snapshot) bool { v, _ := findNode(s, "node-1"); return v.SimVersion == 8 })
	if v, _ := findNode(s, "node-1"); v.Flows == nil || len(v.Flows) != 0 {
		t.Fatalf("flows after empty sample = %#v", v.Flows)
	}
}

func TestFlowsExpireAndAreCapped(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1000, 0).UnixNano())
	cfg := Config{Now: func() time.Time { return time.Unix(0, now.Load()) }}.withDefaults()
	h := newHub(&Server{cfg: cfg, log: quietLog})
	n := &nodeEntry{id: "n", tel: defaultTelemetry("n"), link: &nodeLink{id: "n"}}
	h.nodes["n"] = n

	many := make([]protocol.FlowRecord, protocol.MaxFlowRecords+44)
	h.applyTelemetry(n, protocol.TelemetryPayload{Flows: many})
	if got := len(h.snapshot().Nodes[0].Flows); got != protocol.MaxFlowRecords {
		t.Fatalf("flows = %d, want capped at %d", got, protocol.MaxFlowRecords)
	}
	now.Add(int64(flowTTL))
	if got := len(h.snapshot().Nodes[0].Flows); got != protocol.MaxFlowRecords {
		t.Fatalf("flows at exactly the TTL = %d", got)
	}
	now.Add(1)
	if f := h.snapshot().Nodes[0].Flows; f == nil || len(f) != 0 {
		t.Fatalf("stale flows = %#v", f)
	}
}

func TestSimVersionSurvivesRestart(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Unix(1000, 0).UnixNano())
	cfg := Config{Now: func() time.Time { return time.Unix(0, now.Load()) }}.withDefaults()
	h := newHub(&Server{cfg: cfg, log: quietLog})
	v1 := h.sim.version
	if v1 != 1_000_000 {
		t.Fatalf("initial version = %d, want the clock in ms", v1)
	}
	// Changes faster than the clock still increase the version.
	h.bumpSim()
	h.bumpSim()
	if h.sim.version != v1+2 {
		t.Fatalf("version = %d", h.sim.version)
	}
	// A later CC starts above everything the earlier one sent.
	now.Add(int64(time.Second))
	if h2 := newHub(&Server{cfg: cfg, log: quietLog}); h2.sim.version <= h.sim.version {
		t.Fatalf("restarted version %d not above %d", h2.sim.version, h.sim.version)
	}
	// A clock step back does not make versions go backwards.
	now.Add(-int64(time.Hour))
	h.bumpSim()
	if h.sim.version != v1+3 {
		t.Fatalf("version after clock step back = %d", h.sim.version)
	}
}

func TestSimPositionCap(t *testing.T) {
	h := newHub(&Server{cfg: Config{}.withDefaults(), log: quietLog})
	listed := &nodeEntry{id: "listed", tel: defaultTelemetry("listed")}
	h.nodes["listed"] = listed
	h.sim.positions["listed"] = protocol.Position{X: 1}
	for i := 0; len(h.sim.positions) < maxSimPositions; i++ {
		h.sim.positions[protocol.NodeID("ghost-"+itoa(i))] = protocol.Position{}
	}
	big := make(map[protocol.NodeID]protocol.Position, maxSimPositions+1)
	for i := 0; i <= maxSimPositions; i++ {
		big[protocol.NodeID("new-"+itoa(i))] = protocol.Position{}
	}
	if _, err := h.updateSim(ClientMessage{Type: TypeSim, Positions: big}); err == nil {
		t.Fatal("over-cap placement accepted")
	}
	// A full map makes room by dropping unlisted IDs, never listed ones.
	if _, err := h.updateSim(ClientMessage{Type: TypeSim, Positions: map[protocol.NodeID]protocol.Position{"fresh": {}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.sim.positions["listed"]; !ok || len(h.sim.positions) != 2 {
		t.Fatalf("after pruning: %d entries, listed kept = %v", len(h.sim.positions), ok)
	}
}

func TestKilledNodeLifecycle(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.NodeExpiry = 300 * time.Millisecond })
	b := dialWS(t, cc)
	n := dialNode(t, cc, "node-2")
	b.event(EventNodeUp, "node-2")
	n.telemetry("leader", "alive")

	if code, _ := post(t, cc, "/api/chaos", `{"node":"node-2","action":"kill"}`); code != 200 {
		t.Fatalf("kill = %d", code)
	}
	n.next(protocol.TypeChaos)
	// Still connected: the mark alone changes nothing on the dashboard.
	if v, _ := findNode(getState(t, cc), "node-2"); v.State == NodeStateKilled {
		t.Fatalf("connected node shown killed: %+v", v)
	}
	_ = n.raw.Close()
	if e := b.event(EventNodeDown, "node-2"); e.Detail != "killed" {
		t.Fatalf("node_down detail = %q", e.Detail)
	}
	b.snapshotWhere("killed", func(s Snapshot) bool {
		v, ok := findNode(s, "node-2")
		return ok && v.State == NodeStateKilled && !v.Connected
	})

	// The same ID again is a new node: node_up, alive, mark cleared.
	n2 := dialNode(t, cc, "node-2")
	b.event(EventNodeUp, "node-2")
	v, _ := findNode(getState(t, cc), "node-2")
	if v.State != "alive" || !v.Connected || v.Role != "worker" {
		t.Fatalf("reconnected view = %+v", v)
	}
	_ = n2.raw.Close()
	if e := b.event(EventNodeDown, "node-2"); e.Detail == "killed" {
		t.Fatal("killed mark survived a reconnect")
	}

	// Killed, then expired: the entry and the mark both go.
	n3 := dialNode(t, cc, "node-2")
	b.event(EventNodeUp, "node-2")
	if code, _ := post(t, cc, "/api/chaos", `{"node":"node-2","action":"kill"}`); code != 200 {
		t.Fatalf("kill = %d", code)
	}
	_ = n3.raw.Close()
	b.event(EventNodeDown, "node-2")
	b.snapshotWhere("expired", func(s Snapshot) bool { _, ok := findNode(s, "node-2"); return !ok })
	var marked bool
	if !cc.srv.do(context.Background(), func(h *hub) { _, marked = h.killed["node-2"] }) || marked {
		t.Fatal("killed mark outlived the node's expiry")
	}
}

func TestKilledMarkOnlyForKill(t *testing.T) {
	h := newHub(&Server{cfg: Config{}.withDefaults(), log: quietLog})
	l := &nodeLink{id: "n"}
	h.nodes["n"] = &nodeEntry{id: "n", tel: defaultTelemetry("n"), link: l}
	// A link without a Conn cannot be sent to; spawn is skipped because
	// closing is set, which keeps this a pure state test.
	h.s.closing = true
	if err := h.chaos(ClientMessage{Node: "n", Action: "clear"}); err != nil {
		t.Fatal(err)
	}
	if len(h.killed) != 0 {
		t.Fatal("clear marked the node killed")
	}
	h.onConnDown(l, network.DispositionCleanClose, nil)
	if v := h.snapshot().Nodes[0]; v.State != "alive" {
		t.Fatalf("plain disconnect state = %q", v.State)
	}
}
