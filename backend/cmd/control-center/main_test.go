package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"swarm-net/pkg/protocol"
)

const testWait = 10 * time.Second

func noEnv(string) string { return "" }

func TestLoad(t *testing.T) {
	cfg, err := Load(nil, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (Config{NodeListen: ":7000", HTTPListen: ":8080", LogLevel: slog.LevelInfo}) {
		t.Fatalf("defaults = %+v", cfg)
	}

	env := map[string]string{"SWARM_CC_LISTEN": "0.0.0.0:9000", "SWARM_CC_HTTP": ":9090", "SWARM_LOG_LEVEL": "debug"}
	cfg, err = Load(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (Config{NodeListen: "0.0.0.0:9000", HTTPListen: ":9090", LogLevel: slog.LevelDebug}) {
		t.Fatalf("env = %+v", cfg)
	}

	// Flags beat env.
	cfg, err = Load([]string{"-http=127.0.0.1:1", "-log-level=WARN"}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPListen != "127.0.0.1:1" || cfg.NodeListen != "0.0.0.0:9000" || cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("flags = %+v", cfg)
	}

	cfg, err = Load([]string{"-version", "-http=garbage"}, noEnv)
	if err != nil || !cfg.Version {
		t.Fatalf("-version = %+v, %v", cfg, err)
	}

	for _, args := range [][]string{
		{"-listen="},
		{"-listen=nope"},
		{"-http=:99999"},
		{"-http=:x"},
		{"-log-level=loud"},
		{"-bogus"},
	} {
		if _, err := Load(args, noEnv); !errors.Is(err, errConfig) || exitCode(err) != exitConfig {
			t.Errorf("Load(%v) = %v, want config error", args, err)
		}
	}
	// The API token comes from the environment only (a flag would show in
	// `ps`), and a weak one refuses to start.
	tokEnv := func(v string) func(string) string {
		return func(k string) string {
			if k == "SWARM_CC_API_TOKEN" {
				return v
			}
			return ""
		}
	}
	if cfg, err := Load(nil, tokEnv("0123456789abcdef0123")); err != nil || cfg.APIToken != "0123456789abcdef0123" {
		t.Errorf("token from env = %+v, %v", cfg, err)
	}
	for _, bad := range []string{"short", "has space in the token", "dollar$0123456789abc"} {
		if _, err := Load(nil, tokEnv(bad)); !errors.Is(err, errConfig) {
			t.Errorf("token %q = %v, want config error", bad, err)
		}
	}
	if _, err := Load([]string{"-api-token=0123456789abcdef0123"}, noEnv); !errors.Is(err, errConfig) {
		t.Errorf("-api-token flag accepted: %v", err)
	}
	for _, lvl := range []string{"error", "warning", "info"} {
		if _, err := Load([]string{"-log-level=" + lvl}, noEnv); err != nil {
			t.Errorf("level %s: %v", lvl, err)
		}
	}
}

func TestExitCode(t *testing.T) {
	if exitCode(nil) != exitOK || exitCode(errors.New("x")) != exitRuntime || exitCode(errConfig) != exitConfig {
		t.Fatal("exit code mapping wrong")
	}
}

// syncBuf is a goroutine-safe log sink: slog writes from several goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs syncBuf
	type addrs struct{ node, http net.Addr }
	ready := make(chan addrs, 1)
	done := make(chan error, 1)
	cfg := Config{NodeListen: "127.0.0.1:0", HTTPListen: "127.0.0.1:0", LogLevel: slog.LevelDebug}
	go func() {
		done <- run(ctx, cfg, &logs, func(n, h net.Addr) { ready <- addrs{n, h} })
	}()
	var a addrs
	select {
	case a = <-ready:
	case err := <-done:
		t.Fatalf("run returned early: %v", err)
	case <-time.After(testWait):
		t.Fatal("never listened")
	}
	base := "http://" + a.http.String()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("/healthz = %q", body)
	}
	// "/" points at the frontend; the CC no longer serves the dashboard.
	resp, err = http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "served by the frontend") {
		t.Fatalf("/ = %d %q", resp.StatusCode, body)
	}

	// A browser, then a node over a real TCP handshake.
	wctx, wcancel := context.WithTimeout(ctx, testWait)
	defer wcancel()
	ws, _, err := websocket.Dial(wctx, "ws://"+a.http.String()+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	raw, err := net.Dial("tcp", a.node.String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(testWait))
	hello, _ := protocol.NewEnvelope(protocol.TypeHello, "node-1", "", protocol.HelloPayload{Advertise: "node-1:7000"})
	enc, dec := protocol.NewEncoder(raw), protocol.NewDecoder(raw)
	if err := enc.WriteEnvelope(hello); err != nil {
		t.Fatal(err)
	}
	ack, err := dec.ReadFrame()
	if err != nil || ack.From != "control-center" {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	tel, _ := protocol.NewEnvelope(protocol.TypeTelemetry, "node-1", "control-center", protocol.TelemetryPayload{Role: "leader", State: "alive"})
	if err := enc.WriteEnvelope(tel); err != nil {
		t.Fatal(err)
	}

	for {
		_, data, err := ws.Read(wctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		var m struct {
			Type, Kind, Node string
		}
		_ = json.Unmarshal(data, &m)
		if m.Type == "event" && m.Kind == "leader_change" && m.Node == "node-1" {
			break
		}
	}

	resp, err = http.Post(base+"/api/tasks", "application/json", strings.NewReader(`{"type":"task","kind":"echo","body":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "{\"task_ids\":[\"t-1\"]}\n" {
		t.Fatalf("POST /api/tasks = %d %s", resp.StatusCode, body)
	}
	for {
		env, err := dec.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if env.Type == protocol.TypeTask {
			break
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(testWait):
		t.Fatal("run did not stop")
	}
	for _, want := range []string{"control center listening", "node connected", "hub stopped", "shutdown complete"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestRunListenFailures(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	for _, cfg := range []Config{
		{NodeListen: taken.Addr().String(), HTTPListen: "127.0.0.1:0"},
		{NodeListen: "127.0.0.1:0", HTTPListen: taken.Addr().String()},
	} {
		err := run(context.Background(), cfg, io.Discard, func(net.Addr, net.Addr) { t.Error("must not report listening") })
		if err == nil || exitCode(err) != exitRuntime {
			t.Errorf("run(%+v) = %v, want runtime error", cfg, err)
		}
	}
}
