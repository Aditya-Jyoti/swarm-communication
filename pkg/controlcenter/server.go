package controlcenter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"swarm-net/pkg/network"
)

// Defaults for Config.
const (
	DefaultSnapshotInterval = time.Second
	DefaultNodeExpiry       = 30 * time.Second
	DefaultKeepAlive        = 5 * time.Second
	DefaultHandshakeTimeout = 3 * time.Second
	DefaultClientBuffer     = 64
	DefaultWSWriteTimeout   = 5 * time.Second
	opsDepth                = 256
)

// ErrAlreadyRunning is returned by a second call to Run.
var ErrAlreadyRunning = errors.New("controlcenter: already running")

// Config configures a Server. Every zero value has a usable default.
type Config struct {
	// NodeListen is the TCP bind address for node connections. Default ":7000".
	NodeListen string
	// Static serves GET /. Nil, or a tree without index.html, serves a small
	// placeholder page instead.
	Static fs.FS
	// SnapshotInterval is how often every browser gets a snapshot. Default 1s.
	SnapshotInterval time.Duration
	// NodeExpiry is how long a disconnected node stays listed. Default 30s.
	NodeExpiry time.Duration
	// KeepAlive is how often the CC PINGs each node. The link is otherwise
	// one-way (node -> CC) most of the time, and the node's read deadline
	// would reap a healthy link without it. Must be well under the node's
	// idle timeout (15s by default). Default 5s.
	KeepAlive time.Duration
	// HandshakeTimeout bounds one node HELLO exchange. Default 3s.
	HandshakeTimeout time.Duration
	// Conn tunes every node connection.
	Conn network.ConnConfig
	// ClientBuffer is each browser's outbound queue depth. A browser whose
	// queue fills is dropped. Default 64.
	ClientBuffer int
	// WSWriteTimeout bounds one WebSocket write. Default 5s.
	WSWriteTimeout time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.NodeListen == "" {
		c.NodeListen = ":7000"
	}
	if c.SnapshotInterval <= 0 {
		c.SnapshotInterval = DefaultSnapshotInterval
	}
	if c.NodeExpiry <= 0 {
		c.NodeExpiry = DefaultNodeExpiry
	}
	if c.KeepAlive <= 0 {
		c.KeepAlive = DefaultKeepAlive
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.ClientBuffer <= 0 {
		c.ClientBuffer = DefaultClientBuffer
	}
	if c.WSWriteTimeout <= 0 {
		c.WSWriteTimeout = DefaultWSWriteTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Conn.Now == nil {
		c.Conn.Now = c.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Server is the Control Center.
//
// # Goroutine ownership
//
//   - The hub (Run's goroutine) is the single owner of all CC state: nodes,
//     tasks, browsers. Everyone else talks to it by posting closures on ops
//     (do waits for the result, post does not). One writer means no locks on
//     that state and one answer to "who changed this node's role".
//   - The accept loop and one goroutine per node socket (serveNode), plus
//     short-lived senders (sendAsync). All counted in wg and joined by Run
//     before it returns. They exit because Run closes the listener and every
//     node socket.
//   - Each browser has its HTTP handler goroutine (the writer) and one reader;
//     the handler joins its reader. The hub cancels a browser to drop it, and
//     cancels them all when it exits.
//
// hubDone is closed when the hub stops. Every channel operation towards the
// hub selects on it, so nothing blocks on a hub that has gone.
type Server struct {
	cfg Config
	log *slog.Logger
	ln  net.Listener

	ops     chan func(*hub)
	hubDone chan struct{}
	started atomic.Bool

	wg sync.WaitGroup

	// mu guards socks and closing. socks holds every node socket from accept
	// until serveNode returns, so shutdown can close sockets that are still
	// mid-handshake as well as established ones.
	mu      sync.Mutex
	socks   map[net.Conn]struct{}
	closing bool
}

// New binds the node listener. Nothing is accepted until Run.
func New(cfg Config) (*Server, error) {
	cfg = cfg.withDefaults()
	ln, err := net.Listen("tcp", cfg.NodeListen)
	if err != nil {
		return nil, fmt.Errorf("controlcenter: listen for nodes on %s: %w", cfg.NodeListen, err)
	}
	return &Server{
		cfg:     cfg,
		log:     cfg.Logger.With("node", NodeID),
		ln:      ln,
		ops:     make(chan func(*hub), opsDepth),
		hubDone: make(chan struct{}),
		socks:   make(map[net.Conn]struct{}),
	}, nil
}

// NodeAddr is the bound node listener address.
func (s *Server) NodeAddr() net.Addr { return s.ln.Addr() }

// Run serves nodes and runs the hub until ctx is cancelled, then tears
// everything down and joins every goroutine it started. A Server runs once.
func (s *Server) Run(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	s.wg.Add(1)
	go s.acceptLoop()
	s.log.Info("accepting nodes", "addr", s.ln.Addr())

	h := newHub(s)
	h.run(ctx)

	// Order: stop the hub first (done above; it has dropped every browser),
	// then stop accepting, then cut every node socket so each serveNode
	// returns, then wait for all of them.
	s.mu.Lock()
	s.closing = true
	socks := make([]net.Conn, 0, len(s.socks))
	for c := range s.socks {
		socks = append(socks, c)
	}
	s.mu.Unlock()
	_ = s.ln.Close()
	for _, c := range socks {
		_ = c.Close()
	}
	s.wg.Wait()
	s.log.Info("control center stopped")
	return nil
}

// do runs fn on the hub and waits for it. It reports false if the hub is not
// (or no longer) running or ctx ended first.
func (s *Server) do(ctx context.Context, fn func(*hub)) bool {
	// state arbitrates between the hub starting fn and the caller giving up:
	// whichever CAS wins decides. Without it a caller that timed out could
	// return while fn later runs anyway -- registering a browser whose handler
	// has already gone, for instance.
	const (
		queued int32 = iota
		running
		abandoned
	)
	if s.stopped() {
		return false
	}
	var state atomic.Int32
	done := make(chan struct{})
	op := func(h *hub) {
		if !state.CompareAndSwap(queued, running) {
			return
		}
		fn(h)
		close(done)
	}
	select {
	case s.ops <- op:
	case <-s.hubDone:
		return false
	case <-ctx.Done():
		return false
	}
	select {
	case <-done:
		return true
	case <-s.hubDone:
	case <-ctx.Done():
	}
	if state.CompareAndSwap(queued, abandoned) {
		return false
	}
	// fn has started; the hub always finishes an op it has started.
	<-done
	return true
}

// stopped reports whether the hub has exited. Checked before offering an op:
// ops is buffered, so a select between "ops has room" and "hub is gone" would
// otherwise pick at random and queue ops nobody will ever run.
func (s *Server) stopped() bool {
	select {
	case <-s.hubDone:
		return true
	default:
		return false
	}
}

// post queues fn for the hub without waiting for it to run. It blocks while
// ops is full: that is backpressure on node readers, bounded by hubDone.
func (s *Server) post(fn func(*hub)) bool {
	if s.stopped() {
		return false
	}
	select {
	case s.ops <- fn:
		return true
	case <-s.hubDone:
		return false
	}
}

// track registers a socket for shutdown. It reports false if the server is
// already closing, in which case the caller must close the socket itself.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.socks[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.socks, c)
	s.mu.Unlock()
	s.wg.Done()
}

// spawn runs fn on a goroutine counted in wg, unless the server is closing.
func (s *Server) spawn(fn func()) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		fn()
	}()
}
