package controlcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
	"swarm-net/pkg/telemetry"
)

const testWait = 10 * time.Second

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type testCC struct {
	t    *testing.T
	srv  *Server
	http *httptest.Server
	stop func()
}

func startCC(t *testing.T, mutate func(*Config)) *testCC {
	t.Helper()
	cfg := Config{
		NodeListen:       "127.0.0.1:0",
		SnapshotInterval: 20 * time.Millisecond,
		KeepAlive:        time.Hour,
		Logger:           quietLog,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	hs := httptest.NewServer(srv.Handler())
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(testWait):
			t.Error("Run did not return after cancel")
		}
		hs.Close()
	}
	t.Cleanup(stop)
	return &testCC{t: t, srv: srv, http: hs, stop: stop}
}

// --- fake node over a real TCP handshake -----------------------------------------

type fakeNode struct {
	t   *testing.T
	id  protocol.NodeID
	raw net.Conn
	enc *protocol.Encoder
	dec *protocol.Decoder
	ack protocol.HelloAckPayload
	env *protocol.Envelope
}

func dialNode(t *testing.T, cc *testCC, id protocol.NodeID) *fakeNode {
	t.Helper()
	raw, err := net.Dial("tcp", cc.srv.NodeAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(testWait))
	n := &fakeNode{t: t, id: id, raw: raw, enc: protocol.NewEncoder(raw), dec: protocol.NewDecoder(raw)}
	hello, _ := protocol.NewEnvelope(protocol.TypeHello, id, "", protocol.HelloPayload{
		Advertise:  protocol.NodeAddress(id + ":7946"),
		KnownPeers: []protocol.NodeAddress{"other:7946"},
	})
	if err := n.enc.WriteEnvelope(hello); err != nil {
		t.Fatal(err)
	}
	ack, err := n.dec.ReadFrame()
	if err != nil {
		t.Fatalf("read HELLO_ACK: %v", err)
	}
	n.env = ack
	if n.ack, err = protocol.PayloadOf[protocol.HelloAckPayload](ack); err != nil {
		t.Fatal(err)
	}
	return n
}

func (n *fakeNode) send(typ protocol.MessageType, payload any) {
	n.t.Helper()
	env, err := protocol.NewEnvelope(typ, n.id, NodeID, payload)
	if err != nil {
		n.t.Fatal(err)
	}
	if err := n.enc.WriteEnvelope(env); err != nil {
		n.t.Fatal(err)
	}
}

func (n *fakeNode) telemetry(role, state string) {
	n.t.Helper()
	n.send(protocol.TypeTelemetry, protocol.TelemetryPayload{
		Node: "liar", Role: role, State: state, Term: 3, Leader: n.id, LedgerSize: 4, Dropped: 2,
		Peers:  []protocol.MemberRecord{{ID: "x", Advertise: "x:7946", Role: "worker", State: "alive", Score: 0.4}},
		Scores: map[protocol.NodeAddress]float64{"x:7946": 0.4},
	})
}

func (n *fakeNode) next(want protocol.MessageType) *protocol.Envelope {
	n.t.Helper()
	for {
		env, err := n.dec.ReadFrame()
		if err != nil {
			n.t.Fatalf("%s waiting for %s: %v", n.id, want, err)
		}
		if env.Type == want {
			return env
		}
	}
}

// waitClosed reads until the CC closes the link. Only SIM_CONFIG may arrive
// first: the CC sends one on every connect. A read timeout (testWait) fails.
func (n *fakeNode) waitClosed() {
	n.t.Helper()
	for {
		env, err := n.dec.ReadFrame()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				n.t.Fatalf("%s: link still open", n.id)
			}
			return
		}
		if env.Type != protocol.TypeSimConfig {
			n.t.Fatalf("%s: got %s while waiting for close", n.id, env.Type)
		}
	}
}

// simConfig waits for the next SIM_CONFIG.
func (n *fakeNode) simConfig() protocol.SimConfigPayload {
	n.t.Helper()
	p, err := protocol.PayloadOf[protocol.SimConfigPayload](n.next(protocol.TypeSimConfig))
	if err != nil {
		n.t.Fatal(err)
	}
	return p
}

// --- browser -------------------------------------------------------------------

type browser struct {
	t *testing.T
	c *websocket.Conn
}

func dialWS(t *testing.T, cc *testCC) *browser {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	url := "ws" + strings.TrimPrefix(cc.http.URL, "http") + "/ws"
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	c.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	return &browser{t: t, c: c}
}

type wsMsg struct {
	Type string `json:"type"`
	Snapshot
	Kind   string          `json:"kind"`
	Node   protocol.NodeID `json:"node"`
	Detail string          `json:"detail"`
}

// until reads messages until match returns true.
func (b *browser) until(what string, match func(m wsMsg, raw []byte) bool) wsMsg {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	for {
		_, data, err := b.c.Read(ctx)
		if err != nil {
			b.t.Fatalf("waiting for %s: %v", what, err)
		}
		var m wsMsg
		if err := json.Unmarshal(data, &m); err != nil {
			b.t.Fatalf("bad message %s: %v", data, err)
		}
		if match(m, data) {
			return m
		}
	}
}

func (b *browser) event(kind string, node protocol.NodeID) wsMsg {
	b.t.Helper()
	return b.until(kind+" "+string(node), func(m wsMsg, _ []byte) bool {
		return m.Type == TypeEvent && m.Kind == kind && m.Node == node
	})
}

func (b *browser) snapshotWhere(what string, pred func(Snapshot) bool) Snapshot {
	b.t.Helper()
	return b.until(what, func(m wsMsg, _ []byte) bool {
		return m.Type == TypeSnapshot && pred(m.Snapshot)
	}).Snapshot
}

func (b *browser) write(v any) {
	b.t.Helper()
	data, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	if err := b.c.Write(ctx, websocket.MessageText, data); err != nil {
		b.t.Fatal(err)
	}
}

func findNode(s Snapshot, id protocol.NodeID) (NodeView, bool) {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return NodeView{}, false
}

func post(t *testing.T, cc *testCC, path, body string) (int, map[string]json.RawMessage) {
	t.Helper()
	resp, err := http.Post(cc.http.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func getState(t *testing.T, cc *testCC) Snapshot {
	t.Helper()
	resp, err := http.Get(cc.http.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/state = %d", resp.StatusCode)
	}
	var s Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// --- tests ---------------------------------------------------------------------

func TestHealthzAndRootPointer(t *testing.T) {
	cc := startCC(t, nil)
	for path, want := range map[string]string{
		"/healthz": "ok",
		"/":        "dashboard is served by the frontend",
	} {
		resp, err := http.Get(cc.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d %q", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("GET %s Content-Type = %q, want text/plain", path, ct)
		}
	}
	// The CC serves no files: the dashboard assets live in the frontend image.
	for _, path := range []string{"/app.js", "/index.html", "/style.css"} {
		resp, err := http.Get(cc.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	resp, err := http.Post(cc.http.URL+"/api/state", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/state = %d, want 405", resp.StatusCode)
	}
}

// TestWSOriginThroughProxy pins the contract the frontend's nginx relies on:
// the upgrade is accepted when Origin matches the Host header (nginx passes
// the browser's Host through), and refused when it does not.
func TestWSOriginThroughProxy(t *testing.T) {
	cc := startCC(t, nil)
	url := "ws" + strings.TrimPrefix(cc.http.URL, "http") + "/ws"
	dial := func(host, origin string) error {
		ctx, cancel := context.WithTimeout(context.Background(), testWait)
		defer cancel()
		h := http.Header{}
		h.Set("Origin", origin)
		c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{Host: host, HTTPHeader: h})
		if err == nil {
			c.CloseNow()
		}
		return err
	}
	if err := dial("localhost:8080", "http://localhost:8080"); err != nil {
		t.Errorf("same-origin via proxy Host: %v", err)
	}
	if err := dial("localhost:8080", "http://evil.example"); err == nil {
		t.Error("cross-origin upgrade accepted")
	}
	// What a proxy that rewrote Host to its upstream would produce.
	if err := dial("control-center:8080", "http://localhost:8080"); err == nil {
		t.Error("Host/Origin mismatch accepted")
	}
}

func TestHandshakeAcceptsWithoutGossip(t *testing.T) {
	cc := startCC(t, nil)
	n := dialNode(t, cc, "node-1")
	if !n.ack.Accepted || n.env.From != NodeID {
		t.Fatalf("ack from %q = %+v", n.env.From, n.ack)
	}
	// Connect a second node: the CC must not tell it about the first.
	m := dialNode(t, cc, "node-2")
	if len(m.ack.KnownPeers) != 0 || m.ack.Advertise != "" {
		t.Fatalf("CC advertised addresses: %+v", m.ack)
	}
}

func TestHandshakeRejects(t *testing.T) {
	cc := startCC(t, nil)
	for _, id := range []protocol.NodeID{"", NodeID} {
		n := dialNode(t, cc, id)
		if n.ack.Accepted || n.ack.Reason == "" {
			t.Errorf("id %q: ack = %+v, want rejection with reason", id, n.ack)
		}
	}

	// Wrong first frame.
	raw, err := net.Dial("tcp", cc.srv.NodeAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(testWait))
	ping, _ := protocol.NewEnvelope(protocol.TypePing, "node-1", "", protocol.PingPayload{})
	_ = protocol.NewEncoder(raw).WriteEnvelope(ping)
	ack, err := protocol.NewDecoder(raw).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := protocol.PayloadOf[protocol.HelloAckPayload](ack)
	if p.Accepted {
		t.Fatal("a PING was accepted as a handshake")
	}

	// A HELLO with an unparseable payload.
	raw2, err := net.Dial("tcp", cc.srv.NodeAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	_ = raw2.SetDeadline(time.Now().Add(testWait))
	bad, _ := protocol.NewEnvelope(protocol.TypeHello, "node-1", "", "string payload")
	_ = protocol.NewEncoder(raw2).WriteEnvelope(bad)
	ack, err = protocol.NewDecoder(raw2).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := protocol.PayloadOf[protocol.HelloAckPayload](ack); p.Accepted {
		t.Fatal("a malformed HELLO was accepted")
	}

	// And a client that says nothing is cut off at the handshake deadline
	// without disturbing anyone else (covered by the -race run finishing).
	raw3, err := net.Dial("tcp", cc.srv.NodeAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = raw3.Close()
}

func TestNodeLifecycle(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.NodeExpiry = 100 * time.Millisecond })
	b := dialWS(t, cc)
	first := b.until("initial snapshot", func(m wsMsg, _ []byte) bool { return true })
	if first.Type != TypeSnapshot || len(first.Nodes) != 0 || first.Tasks == nil {
		t.Fatalf("first message = %+v", first)
	}

	n := dialNode(t, cc, "node-1")
	up := b.event(EventNodeUp, "node-1")
	if up.AtUnixMS == 0 || up.Detail == "" {
		t.Fatalf("node_up = %+v", up)
	}
	// Listed immediately, with defaults, before any telemetry.
	s := b.snapshotWhere("node listed", func(s Snapshot) bool { _, ok := findNode(s, "node-1"); return ok })
	v, _ := findNode(s, "node-1")
	if v.Role != "worker" || v.State != "alive" || !v.Connected || v.Peers == nil || v.Scores == nil {
		t.Fatalf("pre-telemetry view = %+v", v)
	}

	n.telemetry("leader", "alive")
	if e := b.event(EventLeaderChange, "node-1"); e.Detail != "worker -> leader" {
		t.Fatalf("leader_change detail = %q", e.Detail)
	}
	n.telemetry("leader", "suspect")
	if e := b.event(EventStateChange, "node-1"); e.Detail != "alive -> suspect" {
		t.Fatalf("state_change detail = %q", e.Detail)
	}
	s = b.snapshotWhere("telemetry applied", func(s Snapshot) bool {
		v, ok := findNode(s, "node-1")
		return ok && v.State == "suspect"
	})
	v, _ = findNode(s, "node-1")
	want := NodeView{
		ID: "node-1", Role: "leader", State: "suspect", Term: 3, Leader: "node-1",
		Connected: true, LastSeenMS: v.LastSeenMS, Dropped: 2, LedgerSize: 4,
	}
	gotCore := v
	gotCore.Peers, gotCore.Scores, gotCore.Flows = nil, nil, nil
	gotCore.Pos = protocol.Position{}
	if !reflect.DeepEqual(gotCore, want) || len(v.Peers) != 1 || v.Scores["x:7946"] != 0.4 {
		t.Fatalf("view = %+v", v)
	}

	// The REST snapshot is the same shape.
	if st := getState(t, cc); len(st.Nodes) != 1 || st.Type != TypeSnapshot {
		t.Fatalf("/api/state = %+v", st)
	}

	_ = n.raw.Close()
	b.event(EventNodeDown, "node-1")
	b.snapshotWhere("disconnected but listed", func(s Snapshot) bool {
		v, ok := findNode(s, "node-1")
		return ok && !v.Connected
	})
	b.snapshotWhere("expired", func(s Snapshot) bool { return len(s.Nodes) == 0 })
}

func TestNewestLinkWinsSilently(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	old := dialNode(t, cc, "node-1")
	b.event(EventNodeUp, "node-1")
	fresh := dialNode(t, cc, "node-1")

	// The CC closes the superseded socket.
	old.waitClosed()
	// Telemetry on the new link lands, and no node_down was emitted between.
	fresh.telemetry("leader", "alive")
	b.until("leader_change without node_down", func(m wsMsg, _ []byte) bool {
		if m.Type == TypeEvent && m.Kind == EventNodeDown {
			t.Fatalf("spurious node_down: %+v", m)
		}
		return m.Type == TypeEvent && m.Kind == EventLeaderChange
	})
}

func TestTasksRoundRobinAndFirstResultWins(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	a := dialNode(t, cc, "node-a")
	z := dialNode(t, cc, "node-z")
	w := dialNode(t, cc, "node-w")
	a.telemetry("leader", "alive")
	z.telemetry("leader", "alive")
	w.telemetry("worker", "alive")
	b.snapshotWhere("two leaders", func(s Snapshot) bool {
		va, _ := findNode(s, "node-a")
		vz, _ := findNode(s, "node-z")
		return va.Role == "leader" && vz.Role == "leader"
	})

	code, out := post(t, cc, "/api/tasks", `{"type":"task","kind":"sleep","body":{"ms":100},"count":3}`)
	if code != 200 || string(out["task_ids"]) != `["t-1","t-2","t-3"]` {
		t.Fatalf("POST /api/tasks = %d %s", code, out["task_ids"])
	}
	for _, step := range []struct {
		n  *fakeNode
		id string
	}{{a, "t-1"}, {a, "t-3"}, {z, "t-2"}} {
		env := step.n.next(protocol.TypeTask)
		p, _ := protocol.PayloadOf[protocol.TaskPayload](env)
		if p.TaskID != step.id || p.Kind != "sleep" || string(p.Body) != `{"ms":100}` {
			t.Fatalf("%s got %+v, want %s", step.n.id, p, step.id)
		}
	}

	// Results: the first wins, a duplicate is ignored, an unknown id too.
	a.send(protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-1", Worker: "node-w", OK: true, Output: "slept 100ms", DurationMS: 0.3})
	a.send(protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-1", Worker: "node-w", OK: false, Output: "dup"})
	a.send(protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-99", OK: true})
	z.send(protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-2", OK: false, Output: "boom"})
	// The two nodes' frames arrive on different reader goroutines, so their
	// events may be emitted in either order.
	done := map[protocol.NodeID]string{}
	b.until("both task_done events", func(m wsMsg, _ []byte) bool {
		if m.Type == TypeEvent && m.Kind == EventTaskDone {
			done[m.Node] = m.Detail
		}
		return len(done) == 2
	})
	if done["node-w"] != "t-1 ok" || done["node-z"] != "t-2 failed" {
		t.Fatalf("task_done events = %v", done)
	}

	s := getState(t, cc)
	if len(s.Tasks) != 3 {
		t.Fatalf("tasks = %+v", s.Tasks)
	}
	t1 := s.Tasks[0]
	if t1.TaskID != "t-1" || t1.State != TaskDone || !t1.OK || t1.Output != "slept 100ms" ||
		t1.Worker != "node-w" || t1.Leader != "node-a" || t1.DurationMS != 0.3 || t1.SubmittedUnixMS == 0 || t1.Kind != "sleep" {
		t.Fatalf("t-1 = %+v", t1)
	}
	if t2 := s.Tasks[1]; t2.State != TaskFailed || t2.Worker != "node-z" || t2.Output != "boom" {
		t.Fatalf("t-2 = %+v (worker defaults to the reporting node)", t2)
	}
	if t3 := s.Tasks[2]; t3.State != TaskPending || t3.Worker != "" {
		t.Fatalf("t-3 = %+v", t3)
	}

	// The raw snapshot carries every contract key, including empty ones.
	resp, err := http.Get(cc.http.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, key := range []string{`"worker":""`, `"output":""`, `"ok":false`, `"at_unix_ms":`, `"last_seen_ms":`, `"submitted_unix_ms":`, `"ledger_size":`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Errorf("snapshot lacks %s: %s", key, raw)
		}
	}
}

func TestTaskRequestErrors(t *testing.T) {
	cc := startCC(t, nil)
	cases := []struct {
		body string
		code int
	}{
		{`{"type":"task","kind":"echo"}`, http.StatusServiceUnavailable}, // no leader yet
		{`not json`, http.StatusBadRequest},
		{`{"type":"task"}`, http.StatusBadRequest},
		{`{"type":"chaos","kind":"echo"}`, http.StatusBadRequest},
		{`{"kind":"echo","count":-1}`, http.StatusBadRequest},
		{`{"kind":"echo","body":` + strings.Repeat(" ", maxRequestBytes) + `1}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if code, out := post(t, cc, "/api/tasks", tc.body); code != tc.code || out["error"] == nil {
			t.Errorf("POST %.40q = %d %v, want %d with error", tc.body, code, out, tc.code)
		}
	}

	// Type may be omitted on the REST route; count is capped at 100.
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b := dialWS(t, cc)
	b.snapshotWhere("leader", func(s Snapshot) bool { v, _ := findNode(s, "node-1"); return v.Role == "leader" })
	code, out := post(t, cc, "/api/tasks", `{"kind":"echo","count":500}`)
	var ids []string
	_ = json.Unmarshal(out["task_ids"], &ids)
	if code != 200 || len(ids) != MaxTaskCount {
		t.Fatalf("capped submit = %d, %d ids", code, len(ids))
	}
}

func TestTaskSendFailureMarksTaskFailed(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Conn = network.ConnConfig{DataDepth: 1} })
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b := dialWS(t, cc)
	b.snapshotWhere("leader", func(s Snapshot) bool { v, _ := findNode(s, "node-1"); return v.Role == "leader" })
	// A queue of one and a burst of 100 cannot all fit: some are shed, and a
	// shed task must be failed, not left pending for ever.
	if code, _ := post(t, cc, "/api/tasks", `{"kind":"echo","count":100}`); code != 200 {
		t.Fatalf("submit = %d", code)
	}
	failed := 0
	for _, task := range getState(t, cc).Tasks {
		if task.State == TaskFailed && strings.HasPrefix(task.Output, "send to leader failed") {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("no task was marked failed after a shed send")
	}
}

func TestChaosRequests(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	n := dialNode(t, cc, "node-2")
	b.event(EventNodeUp, "node-2")

	code, out := post(t, cc, "/api/chaos", `{"type":"chaos","node":"node-2","action":"delay","delay_ms":300}`)
	if code != 200 || string(out["ok"]) != "true" {
		t.Fatalf("POST /api/chaos = %d %v", code, out)
	}
	p, _ := protocol.PayloadOf[protocol.ChaosPayload](n.next(protocol.TypeChaos))
	if p.Action != "delay" || p.DelayMS != 300 {
		t.Fatalf("node got %+v", p)
	}
	if e := b.event(EventChaos, "node-2"); e.Detail != "delay 300ms" {
		t.Fatalf("chaos event = %+v", e)
	}

	cases := []struct {
		body string
		code int
	}{
		{`{"node":"node-9","action":"kill"}`, http.StatusNotFound},
		{`{"node":"node-2","action":"delay","delay_ms":5001}`, http.StatusBadRequest},
		{`{"node":"node-2","action":"explode"}`, http.StatusBadRequest},
		{`{"action":"kill"}`, http.StatusBadRequest},
		{`{"type":"task","node":"node-2","action":"kill"}`, http.StatusBadRequest},
		{`[`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if code, out := post(t, cc, "/api/chaos", tc.body); code != tc.code || out["error"] == nil {
			t.Errorf("POST %q = %d %v, want %d", tc.body, code, out, tc.code)
		}
	}
}

func TestBrowserMessages(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b.snapshotWhere("leader", func(s Snapshot) bool { v, _ := findNode(s, "node-1"); return v.Role == "leader" })

	b.c.Write(context.Background(), websocket.MessageBinary, []byte("ignored"))
	b.write("not an object")
	b.write(map[string]any{"type": "dance"})
	b.write(map[string]any{"type": "task"}) // no kind: rejected, logged
	b.write(map[string]any{"type": "task", "kind": "hash", "body": map[string]string{"data": "abc"}})
	p, _ := protocol.PayloadOf[protocol.TaskPayload](n.next(protocol.TypeTask))
	if p.Kind != "hash" || p.TaskID != "t-1" {
		t.Fatalf("task = %+v", p)
	}
	b.write(map[string]any{"type": "chaos", "node": "node-1", "action": "clear"})
	c, _ := protocol.PayloadOf[protocol.ChaosPayload](n.next(protocol.TypeChaos))
	if c.Action != "clear" {
		t.Fatalf("chaos = %+v", c)
	}
}

func TestKeepAlivePings(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.KeepAlive = 10 * time.Millisecond })
	n := dialNode(t, cc, "node-1")
	ping := n.next(protocol.TypePing)
	if ping.From != NodeID {
		t.Fatalf("ping from %q", ping.From)
	}
	pong, _ := protocol.NewReply(ping, protocol.TypePong, n.id, protocol.PongPayload{})
	if err := n.enc.WriteEnvelope(pong); err != nil {
		t.Fatal(err)
	}
	n.next(protocol.TypePing) // still alive, still pinging
}

func TestRealTelemetryClient(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	chaos := make(chan telemetry.Chaos, 1)
	client, err := telemetry.New(telemetry.Config{
		Self:     network.Identity{ID: "node-7", Advertise: "node-7:7946"},
		Addr:     protocol.NodeAddress(cc.srv.NodeAddr().String()),
		Interval: 10 * time.Millisecond,
		Snapshot: func() protocol.TelemetryPayload {
			return protocol.TelemetryPayload{Node: "node-7", Role: "leader", State: "alive", Degraded: true}
		},
		OnChaos: func(c telemetry.Chaos) { chaos <- c },
		Logger:  quietLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	b.snapshotWhere("node-7 degraded leader", func(s Snapshot) bool {
		v, ok := findNode(s, "node-7")
		return ok && v.Connected && v.Role == "leader" && v.Degraded
	})
	if code, _ := post(t, cc, "/api/chaos", `{"node":"node-7","action":"delay","delay_ms":250}`); code != 200 {
		t.Fatalf("chaos = %d", code)
	}
	select {
	case c := <-chaos:
		if c.Delay != 250*time.Millisecond {
			t.Fatalf("chaos = %+v", c)
		}
	case <-time.After(testWait):
		t.Fatal("chaos never reached the client")
	}
}

func TestShutdownClosesBrowsersAndNodes(t *testing.T) {
	cc := startCC(t, nil)
	b := dialWS(t, cc)
	b.until("initial snapshot", func(wsMsg, []byte) bool { return true })
	n := dialNode(t, cc, "node-1")
	b.event(EventNodeUp, "node-1")

	cc.stop()

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	for {
		if _, _, err := b.c.Read(ctx); err != nil {
			if s := websocket.CloseStatus(err); s != websocket.StatusGoingAway {
				t.Fatalf("browser closed with %v (%v), want going-away", s, err)
			}
			break
		}
	}
	n.waitClosed()
	// Requests after shutdown fail fast instead of hanging.
	if _, err := cc.srv.submitTasks(context.Background(), ClientMessage{Kind: "echo"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("submit after stop = %v", err)
	}
	if err := cc.srv.chaos(context.Background(), ClientMessage{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("chaos after stop = %v", err)
	}
	if cc.srv.post(func(*hub) {}) {
		t.Fatal("post after stop succeeded")
	}
	if err := cc.srv.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run = %v", err)
	}
}

func TestStateUnavailableBeforeRun(t *testing.T) {
	srv, err := New(Config{NodeListen: "127.0.0.1:0", Logger: quietLog})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("state before Run = %d", rec.Code)
	}
	// A WebSocket that cannot register is closed, not leaked.
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	wctx, wcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer wcancel()
	c, _, err := websocket.Dial(wctx, "ws"+strings.TrimPrefix(hs.URL, "http")+"/ws", nil)
	if err == nil {
		_, _, rerr := c.Read(wctx)
		if rerr == nil {
			t.Fatal("unregistered websocket delivered a message")
		}
		_ = c.CloseNow()
	}
}

func TestNewListenFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if _, err := New(Config{NodeListen: taken.Addr().String()}); err == nil {
		t.Fatal("New bound a taken port")
	}
}

// --- hub unit tests --------------------------------------------------------------

func TestSlowBrowserIsDropped(t *testing.T) {
	h := newHub(&Server{cfg: Config{}.withDefaults(), log: quietLog})
	h.s.cfg.Logger = quietLog
	cancelled := false
	c := &wsClient{out: make(chan []byte, 1), cancel: func() { cancelled = true }}
	h.clients[c] = struct{}{}
	h.broadcast([]byte("1"))
	if cancelled || len(h.clients) != 1 {
		t.Fatal("dropped a client with room in its queue")
	}
	h.broadcast([]byte("2")) // queue full: drop, do not block
	if !cancelled || len(h.clients) != 0 {
		t.Fatal("slow client was not dropped")
	}
	h.broadcast(nil) // no-op
}

func TestTaskStoreKeepsNewest(t *testing.T) {
	h := newHub(&Server{cfg: Config{}.withDefaults(), log: quietLog})
	for i := 1; i <= MaxTasks+50; i++ {
		h.nextTask++
		h.addTask(&taskEntry{view: TaskView{TaskID: "t-" + itoa(i), State: TaskPending}})
	}
	if len(h.tasks) != MaxTasks || len(h.taskByID) != MaxTasks {
		t.Fatalf("store holds %d/%d", len(h.tasks), len(h.taskByID))
	}
	if h.tasks[0].view.TaskID != "t-51" || h.tasks[MaxTasks-1].view.TaskID != "t-250" {
		t.Fatalf("window = %s..%s", h.tasks[0].view.TaskID, h.tasks[MaxTasks-1].view.TaskID)
	}
	// A result for an evicted task is ignored.
	h.applyResult("n", protocol.TaskResultPayload{TaskID: "t-1", OK: true})
	if _, ok := h.taskByID["t-1"]; ok {
		t.Fatal("evicted task resurrected")
	}
}

func TestStaleLinkFramesIgnored(t *testing.T) {
	h := newHub(&Server{cfg: Config{}.withDefaults(), log: quietLog})
	stale := &nodeLink{id: "n"}
	env, _ := protocol.NewEnvelope(protocol.TypeTelemetry, "n", NodeID, protocol.TelemetryPayload{Role: "leader"})
	h.onFrame(stale, env) // unknown node
	h.onConnDown(stale, network.DispositionCleanClose, nil)
	if len(h.nodes) != 0 {
		t.Fatal("stale link created state")
	}
	h.nodes["n"] = &nodeEntry{id: "n", tel: defaultTelemetry("n")}
	bad := &protocol.Envelope{Type: protocol.TypeTelemetry, Payload: json.RawMessage(`"x"`)}
	live := &nodeLink{id: "n"}
	h.nodes["n"].link = live
	h.onFrame(live, bad)
	h.onFrame(live, &protocol.Envelope{Type: protocol.TypeTaskResult, Payload: json.RawMessage(`"x"`)})
	h.applyTelemetry(h.nodes["n"], protocol.TelemetryPayload{})
	if tel := h.nodes["n"].tel; tel.Role != "worker" || tel.State != "alive" || tel.Node != "n" {
		t.Fatalf("empty telemetry not defaulted: %+v", tel)
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
