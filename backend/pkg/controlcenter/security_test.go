package controlcenter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"swarm-net/pkg/protocol"
)

// postWith sends a POST with full control over the headers.
func postWith(t *testing.T, cc *testCC, path, body string, hdr map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, cc.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func leaderCC(t *testing.T, mutate func(*Config)) (*testCC, *fakeNode, *browser) {
	t.Helper()
	cc := startCC(t, mutate)
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b := dialWS(t, cc)
	b.snapshotWhere("leader", func(s Snapshot) bool { v, _ := findNode(s, "node-1"); return v.Role == "leader" })
	return cc, n, b
}

// A cross-site page can send a "simple" POST (text/plain, no preflight) to a
// dashboard on 127.0.0.1. Without these checks it kills nodes (CSRF).
func TestPostRejectsCrossSiteRequests(t *testing.T) {
	cc, _, _ := leaderCC(t, nil)
	const task = `{"kind":"echo"}`
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"text/plain simple request", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"form simple request", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"no content type", map[string]string{}, http.StatusUnsupportedMediaType},
		{"cross origin", map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, http.StatusForbidden},
		{"cross site fetch metadata", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same origin", map[string]string{"Content-Type": "application/json", "Origin": cc.http.URL}, http.StatusOK},
		{"json with charset, no origin (curl)", map[string]string{"Content-Type": "application/json; charset=utf-8"}, http.StatusOK},
	}
	for _, tc := range cases {
		if got := postWith(t, cc, "/api/tasks", task, tc.hdr); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestStrictRequestJSON(t *testing.T) {
	cc, _, _ := leaderCC(t, nil)
	cases := []string{
		`{"kind":"echo","unknown":1}`,
		`{"kind":"echo"} {"kind":"echo"}`,
		`{"kind":"echo"} trailing`,
		`{"kind":"` + strings.Repeat("k", maxKindLen+1) + `"}`,
	}
	for _, body := range cases {
		if code, _ := post(t, cc, "/api/tasks", body); code != http.StatusBadRequest {
			t.Errorf("POST %.50q = %d, want 400", body, code)
		}
	}
	// Trailing whitespace is not trailing data.
	if code, _ := post(t, cc, "/api/tasks", "{\"kind\":\"echo\"}\n\t "); code != http.StatusOK {
		t.Errorf("trailing whitespace = %d, want 200", code)
	}
}

// A client that sends headers and then dribbles (or never sends) the body must
// not hold a server goroutine for ever. ReadHeaderTimeout does not cover the
// body.
func TestSlowBodyIsCut(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.RequestTimeout = 200 * time.Millisecond })
	raw, err := net.Dial("tcp", strings.TrimPrefix(cc.http.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, _ = io.WriteString(raw, "POST /api/tasks HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", resp.StatusCode)
		}
	} else if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Fatalf("server still waiting for the body after %v", time.Since(start))
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// A worker's output is echoed into every snapshot, every second, to every
// browser. Uncapped, 200 tasks of echo output make a snapshot of many MiB.
func TestTaskOutputIsCapped(t *testing.T) {
	cc, n, _ := leaderCC(t, nil)
	if code, _ := post(t, cc, "/api/tasks", `{"kind":"echo"}`); code != 200 {
		t.Fatalf("submit = %d", code)
	}
	p, _ := protocol.PayloadOf[protocol.TaskPayload](n.next(protocol.TypeTask))
	big := strings.Repeat("\u00e9", 300<<10) // 2 bytes per rune: a cut can split one
	n.send(protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: p.TaskID, Worker: "node-1", OK: true, Output: big})
	var got TaskView
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if ts := getState(t, cc).Tasks; len(ts) == 1 && ts[0].State == TaskDone {
			got = ts[0]
			break
		}
	}
	if got.State != TaskDone {
		t.Fatal("task never finished")
	}
	if len(got.Output) > maxTaskOutput {
		t.Fatalf("output is %d bytes, want <= %d", len(got.Output), maxTaskOutput)
	}
	if !strings.HasSuffix(got.Output, truncatedMark) || strings.ContainsRune(got.Output, '\uFFFD') {
		t.Fatalf("output not cut cleanly: ...%q", got.Output[len(got.Output)-20:])
	}
}

func TestAPIToken(t *testing.T) {
	const token = "s3cret-token-0123456789"
	cc, n, _ := leaderCC(t, func(c *Config) { c.APIToken = token })
	jsonOnly := map[string]string{"Content-Type": "application/json"}
	with := func(auth string) map[string]string {
		return map[string]string{"Content-Type": "application/json", "Authorization": auth}
	}
	for _, path := range []string{"/api/tasks", "/api/chaos"} {
		body := `{"kind":"echo"}`
		if path == "/api/chaos" {
			body = `{"node":"node-1","action":"clear"}`
		}
		for name, hdr := range map[string]map[string]string{
			"missing":      jsonOnly,
			"wrong":        with("Bearer wrong-token-0123456789"),
			"prefix":       with("Bearer " + token[:len(token)-1]),
			"wrong scheme": with("Basic " + token),
			"bare token":   with(token),
		} {
			if code := postWith(t, cc, path, body, hdr); code != http.StatusUnauthorized {
				t.Errorf("%s %s: %d, want 401", path, name, code)
			}
		}
		if code := postWith(t, cc, path, body, with("Bearer "+token)); code != http.StatusOK {
			t.Errorf("%s with token: %d, want 200", path, code)
		}
		if code := postWith(t, cc, path, body, with("bearer "+token)); code != http.StatusOK {
			t.Errorf("%s with lower-case scheme: %d, want 200", path, code)
		}
	}
	// Reads stay open.
	getState(t, cc)
	// Drain the two tasks and the two chaos frames sent above.
	n.next(protocol.TypeTask)
	n.next(protocol.TypeTask)

	// A WebSocket without the token still gets snapshots, but its commands
	// are ignored.
	anon := dialWS(t, cc)
	anon.write(map[string]any{"type": "task", "kind": "echo"})
	anon.snapshotWhere("any", func(Snapshot) bool { return true })

	// One with the token (nginx injects it on the upgrade) may command.
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	url := "ws" + strings.TrimPrefix(cc.http.URL, "http") + "/ws"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	authed := &browser{t: t, c: c}
	authed.write(map[string]any{"type": "task", "kind": "hash", "body": map[string]string{"data": "x"}})
	// The anonymous task, had it been accepted, would arrive first.
	p, _ := protocol.PayloadOf[protocol.TaskPayload](n.next(protocol.TypeTask))
	if p.Kind != "hash" {
		t.Fatalf("first task after the WS writes is %q, want the authenticated hash task", p.Kind)
	}
	var tasks int
	for _, tv := range getState(t, cc).Tasks {
		if tv.Kind == "echo" {
			tasks++
		}
	}
	if tasks != 2 {
		b, _ := json.Marshal(getState(t, cc).Tasks)
		t.Fatalf("echo tasks = %d, want 2 (anonymous WS task must be dropped): %s", tasks, b)
	}
}

func TestTokenCheck(t *testing.T) {
	cases := map[string]bool{
		"":                      false,
		"short":                 false,
		"0123456789abcdef":      true,
		"a-b_c.d~e+f/g=0123456": true,
		"has space 0123456789":  false,
		"has$dollar0123456789":  false,
		`has"quote0123456789`:   false,
	}
	for tok, ok := range cases {
		if err := CheckAPIToken(tok); (err == nil) != ok {
			t.Errorf("CheckAPIToken(%q) = %v, want ok=%v", tok, err, ok)
		}
	}
}

// A flood of sockets that connect and say nothing must not pin one goroutine
// (and a potential 1MiB frame buffer) each for the whole handshake timeout.
func TestNodeHandshakeFloodIsCapped(t *testing.T) {
	cc := startCC(t, func(c *Config) {
		c.MaxPendingHandshakes = 2
		c.HandshakeTimeout = time.Minute
	})
	var idle []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", cc.srv.NodeAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		idle = append(idle, c)
	}
	// The third is refused at once rather than parked.
	c, err := net.Dial("tcp", cc.srv.NodeAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); isTimeout(err) {
		t.Fatal("excess handshake was parked, not refused")
	}
	// Once a slot frees, a real node gets in.
	_ = idle[0].Close()
	deadline := time.Now().Add(testWait)
	for {
		raw, err := net.Dial("tcp", cc.srv.NodeAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = raw.SetDeadline(time.Now().Add(time.Second))
		hello, _ := protocol.NewEnvelope(protocol.TypeHello, "node-9", "", protocol.HelloPayload{})
		_ = protocol.NewEncoder(raw).WriteEnvelope(hello)
		_, err = protocol.NewDecoder(raw).ReadFrame()
		_ = raw.Close()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot freed: %v", err)
		}
	}
}
