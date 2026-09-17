package controlcenter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/coder/websocket"
)

// maxRequestBytes caps a POST body or a WebSocket message. A task body is a
// small JSON value; anything near this size is a mistake or an attack.
const maxRequestBytes = 64 << 10

// rootText is served at GET /. The dashboard is a separate static site
// (frontend/, served by nginx), so the CC only says where it is and which
// routes it owns. Plain text keeps it readable with a bare curl.
const rootText = `swarm-net control center API; dashboard is served by the frontend
routes: GET /healthz, GET /api/state, POST /api/tasks, POST /api/chaos,
        GET /api/sim, POST /api/sim, GET /ws
`

// Handler returns the HTTP routes from the contract.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/tasks", s.guardMutation(s.handleTasks))
	mux.HandleFunc("POST /api/chaos", s.guardMutation(s.handleChaos))
	mux.HandleFunc("GET /api/sim", s.handleGetSim)
	mux.HandleFunc("POST /api/sim", s.guardMutation(s.handlePostSim))
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /", handleRoot)
	return mux
}

// handleRoot answers exactly "/" with rootText. Any other unmatched path is a
// 404: the CC serves no files.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, rootText)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var b []byte
	if !s.do(r.Context(), func(h *hub) { b = h.snapshotBytes() }) || b == nil {
		writeError(w, ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	m, err := decodeClientMessage(w, r, TypeTask)
	if err != nil {
		writeError(w, err)
		return
	}
	ids, err := s.submitTasks(r.Context(), m)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"task_ids": ids})
}

func (s *Server) handleChaos(w http.ResponseWriter, r *http.Request) {
	m, err := decodeClientMessage(w, r, TypeChaos)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.chaos(r.Context(), m); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGetSim(w http.ResponseWriter, r *http.Request) {
	var v SimView
	if !s.do(r.Context(), func(h *hub) { v = h.simView() }) {
		writeError(w, ErrUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handlePostSim(w http.ResponseWriter, r *http.Request) {
	m, err := decodeClientMessage(w, r, TypeSim)
	if err != nil {
		writeError(w, err)
		return
	}
	v, err := s.updateSim(r.Context(), m)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) updateSim(ctx context.Context, m ClientMessage) (SimView, error) {
	var (
		v   SimView
		err error
	)
	if !s.do(ctx, func(h *hub) { v, err = h.updateSim(m) }) {
		return SimView{}, ErrUnavailable
	}
	return v, err
}

func (s *Server) submitTasks(ctx context.Context, m ClientMessage) ([]string, error) {
	var (
		ids []string
		err error
	)
	if !s.do(ctx, func(h *hub) { ids, err = h.submitTasks(m) }) {
		return nil, ErrUnavailable
	}
	return ids, err
}

func (s *Server) chaos(ctx context.Context, m ClientMessage) error {
	var err error
	if !s.do(ctx, func(h *hub) { err = h.chaos(m) }) {
		return ErrUnavailable
	}
	return err
}

// decodeClientMessage reads a POST body. The "type" field is optional for the
// REST routes (the path already says what it is) but must match if present.
func decodeClientMessage(w http.ResponseWriter, r *http.Request, want string) (ClientMessage, error) {
	m, err := decodeStrict(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		return m, err
	}
	if m.Type == "" {
		m.Type = want
	}
	if m.Type != want {
		return m, fmt.Errorf("%w: type %q, want %q", ErrBadRequest, m.Type, want)
	}
	return m, checkFields(m)
}

// checkFields rejects fields that belong to a different message type. They
// all share ClientMessage, so DisallowUnknownFields cannot catch
// {"type":"task","base_ms":5} on its own.
func checkFields(m ClientMessage) error {
	switch {
	case m.Type == TypeSim && m.hasCommandFields():
		return fmt.Errorf("%w: sim message carries task or chaos fields", ErrBadRequest)
	case m.Type != TypeSim && m.hasSimFields():
		return fmt.Errorf("%w: %s message carries sim fields", ErrBadRequest, m.Type)
	}
	return nil
}

// decodeStrict reads exactly one ClientMessage: unknown fields and anything
// after the value other than whitespace are errors. A typo such as
// "delay-ms" would otherwise be silently ignored and the request would run
// with a zero value, which for chaos is a different action than was meant.
func decodeStrict(r io.Reader) (ClientMessage, error) {
	var m ClientMessage
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return m, fmt.Errorf("%w: unexpected data after the JSON value", ErrBadRequest)
	}
	return m, nil
}

func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrBadRequest):
		code = http.StatusBadRequest
	case errors.Is(err, ErrUnknownNode):
		code = http.StatusNotFound
	case errors.Is(err, ErrNoLeader), errors.Is(err, ErrUnavailable):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleWS serves one browser.
//
// This goroutine is the writer; one more goroutine reads. The split exists
// because the two directions are independent: the browser may send a task at
// any moment, and a reader is also what processes WebSocket control frames
// (ping, close), which coder/websocket only handles while someone is reading.
// The handler does not return until its reader has, so nothing outlives it.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// Default AcceptOptions: same-origin only, i.e. the Origin header's
	// host[:port] must equal the request's Host. A cross-origin page has no
	// business injecting chaos. The dashboard is served by the frontend's
	// nginx, which proxies /ws here with the browser's Host header passed
	// through unchanged ($http_host), so Origin and Host still match.
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("websocket accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	c.SetReadLimit(maxRequestBytes)
	// Decided once, at the upgrade: the browser API cannot set headers on a
	// WebSocket, so the token (when configured) arrives from nginx on the
	// upgrade request or not at all. Without it the socket is read-only.
	canCommand := s.authorized(r)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cl := &wsClient{out: make(chan []byte, s.cfg.ClientBuffer), cancel: cancel}
	// A hijacked request's context is not cancelled when the browser goes
	// away, so registration gets its own bound: a hub that is not running
	// must not park this handler for ever.
	rctx, rcancel := context.WithTimeout(ctx, s.cfg.WSWriteTimeout)
	registered := s.do(rctx, func(h *hub) { h.addClient(cl) })
	rcancel()
	if !registered {
		_ = c.Close(websocket.StatusTryAgainLater, "control center shutting down")
		return
	}
	s.log.Debug("dashboard client connected", "remote", r.RemoteAddr)

	// The reader gets its own context, NOT ctx. coder/websocket closes the
	// socket outright when a Read's context is cancelled, so sharing ctx would
	// kill the connection the moment the hub drops this client -- before the
	// writer could send a close frame with a reason. The reader ends when the
	// connection is closed below.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		s.readWS(context.WithoutCancel(ctx), c, canCommand)
	}()

	status, reason := s.writeWS(ctx, c, cl)
	cancel()
	// Remove from the hub unless it has already dropped us (or has gone).
	// Bounded by hubDone, so shutdown cannot hang here.
	s.do(context.Background(), func(h *hub) { h.removeClient(cl) })
	_ = c.Close(status, reason)
	// Close waits a bounded time for the peer's close frame; CloseNow makes
	// sure the reader's Read returns even if the peer never answers.
	_ = c.CloseNow()
	<-readerDone
	s.log.Debug("dashboard client gone", "remote", r.RemoteAddr, "reason", reason)
}

// writeWS drains the client's queue until the client is dropped or a write
// fails, and reports how to close.
func (s *Server) writeWS(ctx context.Context, c *websocket.Conn, cl *wsClient) (websocket.StatusCode, string) {
	for {
		select {
		case <-ctx.Done():
			select {
			case <-s.hubDone:
				return websocket.StatusGoingAway, "control center shutting down"
			default:
				return websocket.StatusPolicyViolation, "client too slow or gone"
			}
		case msg := <-cl.out:
			wctx, cancel := context.WithTimeout(ctx, s.cfg.WSWriteTimeout)
			err := c.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return websocket.StatusInternalError, "write failed"
			}
		}
	}
}

// readWS handles browser messages until the connection ends. A reader runs
// even when canCommand is false: it is what processes ping and close frames.
func (s *Server) readWS(ctx context.Context, c *websocket.Conn, canCommand bool) {
	warned := false
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		if !canCommand {
			if !warned {
				s.log.Warn("ignoring commands from a dashboard socket without the API token")
				warned = true
			}
			continue
		}
		m, err := decodeStrict(bytes.NewReader(data))
		if err != nil {
			s.log.Debug("ignoring malformed dashboard message", "err", err)
			continue
		}
		switch err = checkFields(m); {
		case err != nil:
		case m.Type == TypeTask:
			_, err = s.submitTasks(ctx, m)
		case m.Type == TypeChaos:
			err = s.chaos(ctx, m)
		case m.Type == TypeSim:
			// The browser learns the outcome from the sim event and the
			// next snapshot, like every other command.
			_, err = s.updateSim(ctx, m)
		default:
			err = fmt.Errorf("%w: unknown message type %q", ErrBadRequest, m.Type)
		}
		if err != nil {
			// The contract has no error message for the browser; the
			// outcome (or its absence) shows up in the next snapshot.
			s.log.Info("dashboard request rejected", "type", m.Type, "err", err)
		}
	}
}
