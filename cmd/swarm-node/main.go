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
	"swarm-net/pkg/telemetry"
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
// running it (run) so a test can reach the node and swap the process-level
// side effects (exit, delay injection) before anything runs.
type app struct {
	cfg    Config
	log    *slog.Logger
	pool   *network.Pool
	prober *network.MeshProber
	node   *cluster.Node
	ln     listener
	// client is the Control Center uplink; nil when SWARM_CONTROL_CENTER is
	// unset. The CC is optional: nothing in the mesh depends on it.
	client *telemetry.Client
	// exit ends the process on CHAOS kill. os.Exit in production; a test
	// substitutes a recorder so the test binary survives.
	exit func(code int)
	// setDelays applies a CHAOS delay (0 clears it) to both reply paths: the
	// prober's PONGs and the node's HEARTBEAT_ACKs. A seam for the same reason.
	setDelays func(d time.Duration)
}

// uplinkIdleFloor is the smallest read deadline the Control Center link may
// use. The CC is the only sender of unsolicited traffic on that link and it
// PINGs every 5s (controlcenter.DefaultKeepAlive), so a deadline equal to the
// ping period reaps a healthy link whenever a ping is a few milliseconds late.
// Three periods tolerates two lost or late pings.
const uplinkIdleFloor = 15 * time.Second

// uplinkIdleTimeout decouples the CC link's idle timeout from the mesh's. A
// short mesh timeout (Compose runs 5s to make failover visible) is right for
// peers that heartbeat every 500ms and wrong for a link pinged every 5s.
func uplinkIdleTimeout(mesh time.Duration) time.Duration {
	return max(mesh, uplinkIdleFloor)
}

// newApp builds and binds everything but dials nobody and runs no loop.
//
// # Construction order
//
// There is a cycle in the graph: the pool must be given its frame Handler at
// construction, the handler is the node's, and the node must be given its
// Transport (the pool) and its OnFrame hook (the prober's), while the prober
// needs the pool to send PINGs and the node's table to resolve addresses. The
// Control Center client closes a second loop: the node's OnTaskResult needs
// the client, while the client's Snapshot, SubmitTask and OnChaos need the
// node (and the app). None of the pkg constructors offers a setter, so the
// cycles are broken with late-bound closures over a *cluster.Node that is nil
// until the node exists, and over an *app that is filled in as we go.
//
// The order is therefore: pool, prober, client (closures only), node, and the
// listener last.
//
// This is safe without a lock because of ordering, not luck: the closures are
// only ever invoked from goroutines the pool starts (connection readers, dial
// loops), that the node's loop starts (probes), or that app.run starts (the
// client's Run), and every one of those is created by a `go` statement that
// runs after the assignments below. The Go memory model makes the writes
// visible to them. A test under -race confirms it. If a future change hands
// the pool a socket, or starts the client, before newApp returns, this comment
// is the thing to reread.
func newApp(cfg Config, stderr io.Writer) (*app, error) {
	// The node tags its own lines with "node"; the base logger goes to it
	// untagged so the key is not emitted twice, and the binary's own lines get
	// the tag here.
	base := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log := base.With("node", cfg.NodeID)

	var node *cluster.Node
	a := &app{cfg: cfg, log: log, exit: os.Exit}

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

	a.setDelays = func(d time.Duration) {
		prober.SetDelay(d)
		node.SetChaosDelay(d)
	}

	var (
		client       *telemetry.Client
		onTaskResult func(protocol.TaskResultPayload)
		err          error
	)
	if cfg.ControlCenter != "" {
		client, err = telemetry.New(telemetry.Config{
			Self:     network.Identity{ID: cfg.NodeID, Advertise: cfg.Advertise},
			Addr:     cfg.ControlCenter,
			Interval: cfg.TelemetryInterval,
			Snapshot: func() protocol.TelemetryPayload { return telemetry.FromStatus(node.Status()) },
			SubmitTask: func(ctx context.Context, t protocol.TaskPayload) error {
				return node.SubmitTask(ctx, t)
			},
			OnChaos: a.applyChaos,
			Conn:    network.ConnConfig{IdleTimeout: uplinkIdleTimeout(cfg.IdleTimeout)},
			Logger:  base,
		})
		if err != nil {
			prober.Close()
			_ = pool.Close()
			return nil, fmt.Errorf("swarm-node %q: build control center client: %w", cfg.NodeID, err)
		}
		// SendResult never blocks, which is what OnTaskResult (called on the
		// node loop) requires.
		onTaskResult = client.SendResult
	}

	node, err = cluster.NewNode(cluster.NodeConfig{
		Self:           cfg.NodeID,
		Advertise:      cfg.Advertise,
		Incarnation:    time.Now().Unix(),
		Transport:      pool,
		Health:         strategy,
		Election:       cluster.Config{Threshold: cfg.Threshold},
		ProbeInterval:  cfg.ProbeInterval,
		ElectionFloor:  cfg.ElectionFloor,
		GossipInterval: cfg.GossipInterval,
		Logger:         base,
		OnFrame:        prober.HandleFrame,
		OnTaskResult:   onTaskResult,
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
	a.pool, a.prober, a.node, a.ln, a.client = pool, prober, node, ln, client
	return a, nil
}

// applyChaos acts on a validated CHAOS instruction, on the client's goroutine.
func (a *app) applyChaos(c telemetry.Chaos) {
	switch c.Action {
	case telemetry.ChaosKill:
		// Deliberately abrupt: no LEAVE, no deferred cleanup. The point of
		// kill is to look like a crash to the swarm; Compose restarts the
		// container as a new incarnation.
		a.log.Error("chaos kill received: exiting")
		a.exit(exitRuntime)
	default: // delay and clear; Delay is 0 for clear
		a.log.Warn("chaos delay applied", "delay", c.Delay)
		a.setDelays(c.Delay)
	}
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
// LEAVE is broadcast while connections still exist; the Control Center uplink
// next (it has its own pool, so nothing else waits on it, but joining it here
// means run never returns with the goroutine alive); the listener so no new
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

	// Control Center uplink goroutine: owned by run, joined below via
	// clientDone. It ends when runCtx is cancelled. An error (a misconfigured
	// address) is logged and does not stop the node: the mesh never depends
	// on the CC.
	var clientDone chan struct{}
	if a.client != nil {
		clientDone = make(chan struct{})
		go func() {
			defer close(clientDone)
			if err := a.client.Run(runCtx); err != nil {
				a.log.Error("control center uplink stopped", "err", err)
			}
		}()
	}

	a.log.Info("node starting", "seeds", len(a.cfg.Seeds), "threshold", a.cfg.Threshold,
		"control_center", a.cfg.ControlCenter)
	err := a.node.Run(runCtx)
	a.log.Info("node loop stopped", "err", err)

	cancel()
	if clientDone != nil {
		a.log.Info("closing control center uplink")
		<-clientDone
	}

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
