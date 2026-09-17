package controlcenter

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// # Threat model for the HTTP API
//
// POST /api/tasks and POST /api/chaos change the swarm; chaos "kill" stops a
// node. Who can reach them:
//
//   - The default Compose file publishes only the frontend (nginx), bound to
//     ${BIND_ADDR:-127.0.0.1}. The CC's own port is not published. So by
//     default the callers are: processes on the host, and every container on
//     the swarm bridge network (including every node).
//   - A web page in the operator's browser can also reach 127.0.0.1. It cannot
//     read responses cross-origin, but it can SEND a "simple" request (a
//     text/plain POST needs no CORS preflight). guardMutation closes
//     that: JSON only, and a browser-sent Origin / Sec-Fetch-Site must say
//     same-origin.
//
// APIToken (SWARM_CC_API_TOKEN) is optional. When set, mutating requests need
// "Authorization: Bearer <token>", and a WebSocket may only send commands if
// its upgrade request carried the header. Reads (GET /api/state, snapshots on
// /ws) stay open: they reveal topology, not secrets.
//
// The dashboard never sees the token. The frontend's nginx adds the header
// server-side on /api/ and /ws (CC_API_TOKEN in that container). So the token
// protects the CC from everything that does NOT come through nginx -- other
// containers on the bridge, or a directly published CC port. Anyone who can
// reach the dashboard can still act through it: guard that port (BIND_ADDR,
// or authentication in front of nginx) if it leaves loopback.
//
// Not covered: DNS rebinding against the loopback dashboard (nginx accepts any
// Host), and the node protocol on :7000, which is unauthenticated by design
// and trusts every peer on the bridge.

// minTokenLen is the shortest token accepted. A token is compared on every
// request with no rate limit, so it must not be guessable.
const minTokenLen = 16

// ErrBadToken is wrapped by CheckAPIToken rejections.
var ErrBadToken = errors.New("controlcenter: invalid API token")

// CheckAPIToken validates a non-empty token. The character set is limited to
// what survives being pasted into an HTTP header and into the nginx config
// template verbatim: no spaces, quotes, "$" (an nginx variable) or ";".
// `openssl rand -hex 32` always qualifies.
func CheckAPIToken(tok string) error {
	if len(tok) < minTokenLen {
		return fmt.Errorf("%w: need at least %d characters, got %d", ErrBadToken, minTokenLen, len(tok))
	}
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-._~+/=", r):
		default:
			return fmt.Errorf("%w: character %q not allowed (use A-Z a-z 0-9 - . _ ~ + / =)", ErrBadToken, r)
		}
	}
	return nil
}

// authorized reports whether r carries the API token, or no token is
// configured.
//
// Both sides are hashed before the constant-time compare so that neither the
// content nor the length of the token leaks through timing
// (ConstantTimeCompare returns early on a length mismatch).
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.APIToken == "" {
		return true
	}
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return subtle.ConstantTimeCompare(got[:], s.tokenSum[:]) == 1
}

// sameOrigin reports whether a request is not a cross-site browser request.
//
// Browsers send Origin on every cross-origin POST, and Sec-Fetch-Site on
// every request in current engines; a non-browser client (curl, scripts/e2e.sh)
// sends neither and is allowed. "same-site" is refused too: another port on
// localhost is same-site but is a different application.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // includes "null" from sandboxed frames and file: pages
	}
	// Host is what the browser addressed. Behind nginx it is passed through
	// unchanged ($http_host), the same rule the WebSocket origin check uses.
	return strings.EqualFold(u.Host, r.Host)
}

// guardMutation wraps a POST handler with, in order: the cross-site check
// (403), the JSON-only check (415), the token check (401), and a deadline on
// reading the body.
//
// Why a per-request deadline rather than http.Server.ReadTimeout: that one
// also applies to /ws, where coder/websocket takes over the hijacked
// connection with whatever deadline net/http left on it. ReadHeaderTimeout
// covers the headers only, so without this a client could send a
// Content-Length and then never send the body, holding a goroutine for ever.
func (s *Server) guardMutation(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
			return
		}
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="swarm-net"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
			return
		}
		rc := http.NewResponseController(w)
		// Real time, not cfg.Now: this is a socket deadline.
		deadline := time.Now().Add(s.cfg.RequestTimeout)
		// Best effort: a ResponseWriter that cannot set deadlines (a test
		// recorder) is still bounded by maxRequestBytes.
		_ = rc.SetReadDeadline(deadline)
		_ = rc.SetWriteDeadline(deadline)
		h(w, r)
	}
}
