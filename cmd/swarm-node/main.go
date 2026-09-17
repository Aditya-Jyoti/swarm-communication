// Command swarm-node is the one binary every node in the swarm runs.
//
// It is wiring only: it parses configuration, builds the dependency graph from
// pkg/network, pkg/health and pkg/cluster, installs signal handling, blocks in the
// node's event loop, and tears everything down in the reverse order it was built.
// No distributed-systems rule lives here; if one seems to be needed, it belongs in
// pkg/ (docs/architecture/repo-layout.md, "cmd/ holds wiring, not logic").
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/health"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// version is the build string, overridden at link time with
// -ldflags "-X main.version=<tag>".
var version = "dev"

// Exit codes. Distinct so a Compose healthcheck or a CI script can tell a typo
// in the environment (fix the YAML) from a runtime failure (look at the logs).
const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

func main() {
	cfg, err := Load(os.Args[1:], os.Getenv, os.Hostname)
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarm-node:", err)
		os.Exit(exitCode(err))
	}
	if cfg.Version {
		printVersion(os.Stdout)
		return
	}

	// SIGTERM is what `docker stop` and Compose send; SIGINT is the operator's
	// Ctrl-C. Both become one cancelled context, which is the only shutdown
	// signal the rest of the program knows about. A second signal during
	// shutdown kills the process via the default handler, because stop() has
	// restored it -- the escape hatch when a peer is holding the LEAVE hostage.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, cfg, os.Stderr, nil)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "swarm-node:", err)
		os.Exit(exitCode(err))
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "swarm-node %s\n", version)
}

// exitCode maps an error from Load or run to a process exit status.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errConfig):
		return exitConfig
	default:
		return exitRuntime
	}
}

// listener is the subset of *network.Listener that run supervises. An interface
// so a test can substitute one whose accept loop dies on demand; the real
// Listener only dies when the kernel takes the socket away.
type listener interface {
	Addr() net.Addr
	Done() <-chan struct{}
	Err() error
	Close() error
}

// app is the constructed dependency graph. Building it (newApp) is separate from
// running it (run) so a test can reach the node for Status without the binary
// growing a telemetry endpoint it does not yet need.
type app struct {
	cfg    Config
	log    *slog.Logger
	pool   *network.Pool
	prober *network.MeshProber
	node   *cluster.Node
	ln     listener
}

// newApp builds and binds everything but dials nobody and runs no loop.
//
// # Construction order
//
// There is a cycle in the graph: the pool must be given its frame Handler at
// construction, the handler is the node's, and the node must be given its
// Transport (the pool) and its OnFrame hook (the prober's), while the prober
// needs the pool to send PINGs and the node's table to resolve addresses. None
// of the pkg constructors offers a setter, so the cycle is broken with two
// late-bound closures over a *cluster.Node that is nil until the node exists.
//
// This is safe without a lock because of ordering, not luck: the closures are
// only ever invoked from goroutines the pool starts (connection readers, dial
// loops) or that the node's loop starts (probes), and every one of those is
// created by a `go` statement that runs after the assignment below. The Go
// memory model makes the write visible to them. A test under -race confirms
// it. If a future change hands the pool a socket before newApp returns, this
// comment is the thing to reread.
func newApp(cfg Config, stderr io.Writer) (*app, error) {
	// The node tags its own lines with "node"; the base logger goes to it
	// untagged so the key is not emitted twice, and the binary's own lines get
	// the tag here.
	base := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log := base.With("node", cfg.NodeID)

	var node *cluster.Node

	pool := network.NewPool(network.PoolConfig{
		Self: network.Identity{
			ID:        cfg.NodeID,
			Advertise: cfg.Advertise,
			// Wall-clock seconds at start-up. Nothing here persists across a
			// restart, and the membership rule is "higher incarnation wins", so
			// a restarted node must announce a number larger than anything it
			// announced before it died. A clock is the only monotonic-across-
			// restarts source this binary has; a clock step backwards costs one
			// refutation round, which the node already handles.
			Incarnation: time.Now().Unix(),
		},
		Conn: network.ConnConfig{IdleTimeout: cfg.IdleTimeout},
		Handler: func(peer protocol.NodeID, env *protocol.Envelope) {
			node.Handler()(peer, env)
		},
	})

	prober := network.NewMeshProber(pool, func(addr protocol.NodeAddress) (protocol.NodeID, bool) {
		return node.Resolve(addr)
	}, nil)

	strategy := health.NewLatencyHealthStrategy(health.Config{Probe: prober.Probe})

	var err error
	node, err = cluster.NewNode(cluster.NodeConfig{
		Self:          cfg.NodeID,
		Advertise:     cfg.Advertise,
		Incarnation:   time.Now().Unix(),
		Transport:     pool,
		Health:        strategy,
		Election:      cluster.Config{Threshold: cfg.Threshold},
		ProbeInterval: cfg.ProbeInterval,
		ElectionFloor: cfg.ElectionFloor,
		Logger:        base,
		OnFrame:       prober.HandleFrame,
	})
	if err != nil {
		prober.Close()
		_ = pool.Close()
		return nil, fmt.Errorf("swarm-node %q: build node: %w", cfg.NodeID, err)
	}

	// Bind last: from here on sockets can arrive and the closures above are live.
	ln, err := network.Listen(network.ListenerConfig{Addr: cfg.Listen}, pool)
	if err != nil {
		prober.Close()
		_ = pool.Close()
		return nil, fmt.Errorf("swarm-node %q: %w", cfg.NodeID, err)
	}
	log.Info("listening", "addr", ln.Addr(), "advertise", cfg.Advertise, "version", version)
	return &app{cfg: cfg, log: log, pool: pool, prober: prober, node: node, ln: ln}, nil
}

// run builds the app, reports the bound address through onListening (nil is
// fine), and blocks until ctx is cancelled or the listener dies. It returns nil
// on a clean shutdown and a non-nil error, mapped to exitRuntime, otherwise.
func run(ctx context.Context, cfg Config, stderr io.Writer, onListening func(net.Addr)) error {
	a, err := newApp(cfg, stderr)
	if err != nil {
		return err
	}
	if onListening != nil {
		onListening(a.ln.Addr())
	}
	return a.run(ctx)
}

// run dials the seeds, runs the node loop in the foreground, and shuts down.
//
// Shutdown order is the reverse of the data flow: the node goes first so its
// LEAVE is broadcast while connections still exist; the listener next so no new
// socket is admitted into a pool about to close; the prober before the pool so
// its delayed-reply goroutines are joined before the sends they would make have
// nowhere to go; the pool last, which is also what closes Events and every
// socket.
func (a *app) run(ctx context.Context) error {
	for _, seed := range a.cfg.Seeds {
		a.log.Info("dialling seed", "seed", seed)
		a.pool.Connect(seed)
	}

	// The node loop owns the foreground. The listener is supervised by one
	// goroutine (listener watcher) that turns "accept loop died" into a
	// cancellation, so the node shuts down the same way it would on SIGTERM.
	// The watcher cannot leak: it also returns when runCtx ends, which happens
	// on every path out of this function.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lnErr := make(chan error, 1)
	go func() {
		select {
		case <-a.ln.Done():
			if err := a.ln.Err(); err != nil {
				lnErr <- err
				cancel()
			}
		case <-runCtx.Done():
		}
	}()

	a.log.Info("node starting", "seeds", len(a.cfg.Seeds), "threshold", a.cfg.Threshold)
	err := a.node.Run(runCtx)
	a.log.Info("node loop stopped", "err", err)

	a.log.Info("closing listener")
	_ = a.ln.Close()
	a.log.Info("closing prober")
	a.prober.Close()
	a.log.Info("closing pool")
	_ = a.pool.Close()
	a.log.Info("shutdown complete")

	if err != nil {
		return fmt.Errorf("swarm-node %q: node loop: %w", a.cfg.NodeID, err)
	}
	select {
	case lerr := <-lnErr:
		return fmt.Errorf("swarm-node %q: listener died: %w", a.cfg.NodeID, lerr)
	default:
	}
	return nil
}
