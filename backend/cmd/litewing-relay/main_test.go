package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const line = `TLM {"src":"litewing","t":2385102,"roll":2.35,"pitch":1.92,"yaw":-69.82,` +
	`"thrust":0,"alt":0.032,"vbat":4.168,"pct":90,"armed":0,"rssi":-33,"m":[0,0,0,0]}`

// port is a fake serial port: whatever is written to w comes out of Read.
type port struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func newPort() *port {
	r, w := io.Pipe()
	return &port{r: r, w: w, closed: make(chan struct{})}
}

func (p *port) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *port) Close() error {
	p.once.Do(func() { close(p.closed); _ = p.r.Close() })
	return nil
}

// sink records forwarded datagrams.
type sink struct {
	mu   sync.Mutex
	got  [][]byte
	fail error
	seen chan struct{}
}

func newSink() *sink { return &sink{seen: make(chan struct{}, 100)} }

func (s *sink) send(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, append([]byte(nil), b...))
	s.seen <- struct{}{}
	return s.fail
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *sink) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for s.count() < n {
		select {
		case <-s.seen:
		case <-deadline:
			t.Fatalf("forwarded %d datagrams, want %d", s.count(), n)
		}
	}
}

// opener hands out ports, or errors, in order; after the list it blocks until
// the context ends, so a test sees exactly the opens it scripted.
type opener struct {
	mu    sync.Mutex
	steps []any // *port or error
	calls int
	ctx   context.Context
}

func (o *opener) open() (io.ReadCloser, error) {
	o.mu.Lock()
	o.calls++
	if len(o.steps) == 0 {
		o.mu.Unlock()
		<-o.ctx.Done()
		return nil, o.ctx.Err()
	}
	s := o.steps[0]
	o.steps = o.steps[1:]
	o.mu.Unlock()
	if err, ok := s.(error); ok {
		return nil, err
	}
	return s.(*port), nil
}

func (o *opener) n() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

type harness struct {
	cancel context.CancelFunc
	done   chan error
	sink   *sink
	logs   *syncBuf
	st     *stats
}

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

func start(t *testing.T, steps ...any) (*harness, *opener) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	o := &opener{steps: steps, ctx: ctx}
	h := &harness{cancel: cancel, done: make(chan error, 1), sink: newSink(), logs: &syncBuf{}, st: &stats{}}
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	cfg := config{Retry: 10 * time.Millisecond, Report: time.Hour}
	go func() { h.done <- run(ctx, cfg, o.open, h.sink.send, log, h.st) }()
	t.Cleanup(func() { cancel(); <-h.done })
	return h, o
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		h.done <- nil // for Cleanup
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on cancel")
	}
}

func TestForwardsValidLinesWithoutPrefix(t *testing.T) {
	p := newPort()
	h, _ := start(t, p)
	go func() { _, _ = io.WriteString(p.w, line+"\r\n") }()
	h.sink.wait(t, 1)
	got := string(h.sink.got[0])
	if strings.HasPrefix(got, "TLM ") || !strings.HasPrefix(got, `{"src":"litewing"`) || strings.ContainsAny(got, "\r\n") {
		t.Fatalf("forwarded %q", got)
	}
	h.stop(t)
	if h.st.forwarded.Load() != 1 || h.st.lines.Load() != 1 {
		t.Fatalf("stats %+v", h.st.snapshot())
	}
}

func TestSkipsLogLinesAndRejectsBadTelemetry(t *testing.T) {
	p := newPort()
	h, _ := start(t, p)
	go func() {
		_, _ = io.WriteString(p.w, "[LW] Joined, ip 192.168.43.43\r\n")
		_, _ = io.WriteString(p.w, "TLM {\"src\":\"litewing\"}\r\n") // invalid
		_, _ = io.WriteString(p.w, "\r\n")
		_, _ = io.WriteString(p.w, line+"\n")
	}()
	h.sink.wait(t, 1)
	h.stop(t)
	s := h.st.snapshot()
	if s.Forwarded != 1 || s.Rejected != 1 || s.Lines != 4 {
		t.Fatalf("stats %+v", s)
	}
}

func TestOverlongLineIsDiscardedToTheNextNewline(t *testing.T) {
	p := newPort()
	h, _ := start(t, p)
	go func() {
		// A dropped newline runs two lines together; the whole run is noise.
		_, _ = io.WriteString(p.w, "TLM "+strings.Repeat("x", 4000))
		_, _ = io.WriteString(p.w, line+"\n") // still part of the long line
		_, _ = io.WriteString(p.w, line+"\n") // this one is clean
	}()
	h.sink.wait(t, 1)
	h.stop(t)
	s := h.st.snapshot()
	if s.Forwarded != 1 || s.Rejected != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestReopensAfterEOFAndReadError(t *testing.T) {
	p1, p2, p3 := newPort(), newPort(), newPort()
	h, o := start(t, p1, p2, p3)
	go func() {
		_, _ = io.WriteString(p1.w, line+"\n")
		_ = p1.w.Close() // EOF: unplugged
	}()
	h.sink.wait(t, 1)
	go func() {
		_, _ = io.WriteString(p2.w, line+"\n")
		_ = p2.w.CloseWithError(errors.New("input/output error")) // re-enumerated
	}()
	h.sink.wait(t, 2)
	go func() { _, _ = io.WriteString(p3.w, line+"\n") }()
	h.sink.wait(t, 3)
	h.stop(t)
	if o.n() != 3 {
		t.Fatalf("opened %d times, want 3", o.n())
	}
	if !strings.Contains(h.logs.String(), "input/output error") {
		t.Fatalf("read error not logged:\n%s", h.logs.String())
	}
	<-p1.closed
	<-p2.closed
}

func TestRetriesOpenErrorsAndHintsOnPermission(t *testing.T) {
	p := newPort()
	perm := &fs.PathError{Op: "open", Path: "/dev/ttyUSB0", Err: fs.ErrPermission}
	missing := &fs.PathError{Op: "open", Path: "/dev/ttyUSB0", Err: fs.ErrNotExist}
	h, o := start(t, perm, perm, missing, missing, missing, p)
	go func() { _, _ = io.WriteString(p.w, line+"\n") }()
	h.sink.wait(t, 1)
	h.stop(t)
	if o.n() < 6 {
		t.Fatalf("opened %d times", o.n())
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "uucp") {
		t.Fatalf("no permission hint:\n%s", logs)
	}
	// One line per distinct error, not one per retry.
	if c := strings.Count(logs, "permission denied"); c != 1 {
		t.Fatalf("permission error logged %d times:\n%s", c, logs)
	}
	if c := strings.Count(logs, "file does not exist"); c != 1 {
		t.Fatalf("missing-port error logged %d times:\n%s", c, logs)
	}
	if h.st.opens.Load() != 1 || h.st.openErrors.Load() != 5 {
		t.Fatalf("stats %+v", h.st.snapshot())
	}
}

func TestSendErrorsAreCountedNotFatal(t *testing.T) {
	p := newPort()
	h, _ := start(t, p)
	h.sink.fail = errors.New("connection refused")
	go func() {
		_, _ = io.WriteString(p.w, line+"\n")
		_, _ = io.WriteString(p.w, line+"\n")
	}()
	h.sink.wait(t, 2)
	h.stop(t)
	if s := h.st.snapshot(); s.SendErrors != 2 || s.Forwarded != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestCancelUnblocksAReadingPort(t *testing.T) {
	p := newPort()
	h, o := start(t, p)
	for o.n() < 1 {
		time.Sleep(time.Millisecond)
	}
	h.stop(t) // Read is blocked on the pipe; cancel must close the port
	select {
	case <-p.closed:
	case <-time.After(time.Second):
		t.Fatal("port not closed on cancel")
	}
}

func TestCancelDuringRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	o := &opener{steps: []any{errors.New("busy")}, ctx: ctx}
	done := make(chan error, 1)
	cfg := config{Retry: time.Hour, Report: time.Hour}
	go func() { done <- run(ctx, cfg, o.open, newSink().send, slog.New(slog.DiscardHandler), &stats{}) }()
	for o.n() < 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("run stuck in the retry wait")
	}
}

func TestPeriodicReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newPort()
	o := &opener{steps: []any{p}, ctx: ctx}
	logs := &syncBuf{}
	done := make(chan error, 1)
	cfg := config{Retry: time.Millisecond, Report: 20 * time.Millisecond}
	go func() {
		done <- run(ctx, cfg, o.open, newSink().send, slog.New(slog.NewTextHandler(logs, nil)), &stats{})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "relay stats") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "relay stats") {
		t.Fatalf("no periodic report:\n%s", logs.String())
	}
}

func TestParseFlags(t *testing.T) {
	cfg, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config{Port: "/dev/ttyUSB0", Baud: 115200, To: "127.0.0.1:14560", Retry: time.Second, Report: 10 * time.Second}
	if cfg != want {
		t.Fatalf("defaults %+v, want %+v", cfg, want)
	}
	cfg, err = parseFlags([]string{"-port", "/dev/ttyUSB1", "-baud", "921600", "-to", "localhost:9", "-retry", "250ms", "-report", "0s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "/dev/ttyUSB1" || cfg.Baud != 921600 || cfg.To != "localhost:9" || cfg.Retry != 250*time.Millisecond || cfg.Report != 0 {
		t.Fatalf("parsed %+v", cfg)
	}
	for _, bad := range [][]string{
		{"-baud", "12345"},
		{"-baud", "x"},
		{"-port", ""},
		{"-to", "nohostport"},
		{"-to", "127.0.0.1:0"},
		{"-to", "127.0.0.1:99999"},
		{"-retry", "0s"},
		{"-report", "-1s"},
		{"-nope"},
		{"extra-arg"},
	} {
		if _, err := parseFlags(bad, io.Discard); err == nil {
			t.Errorf("parseFlags(%q) accepted", bad)
		}
	}
}

func TestReportDisabled(t *testing.T) {
	// -report 0 turns the summary off rather than spinning a zero-period ticker.
	ctx, cancel := context.WithCancel(context.Background())
	o := &opener{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, config{Retry: time.Millisecond}, o.open, newSink().send, slog.New(slog.DiscardHandler), &stats{})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUDPSenderReachesAListener(t *testing.T) {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	send, closeFn, err := udpSender(ln.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if err := send([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_ = ln.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := ln.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("got %q, %v", buf[:n], err)
	}
	if _, _, err := udpSender("not an address"); err == nil {
		t.Fatal("udpSender accepted a bad address")
	}
}

func TestCancelDuringWaitAfterPortLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newPort()
	o := &opener{steps: []any{p}, ctx: ctx}
	done := make(chan error, 1)
	cfg := config{Retry: time.Hour, Report: time.Hour}
	go func() { done <- run(ctx, cfg, o.open, newSink().send, slog.New(slog.DiscardHandler), &stats{}) }()
	_ = p.w.Close() // EOF: run now waits an hour before reopening
	<-p.closed
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("run stuck waiting to reopen")
	}
	if o.n() != 1 {
		t.Fatalf("opened %d times", o.n())
	}
}

func TestWithSignalsReportsBadFlags(t *testing.T) {
	if code := withSignals([]string{"-help"}, io.Discard); code != exitConfig {
		t.Fatalf("exit %d", code)
	}
}
