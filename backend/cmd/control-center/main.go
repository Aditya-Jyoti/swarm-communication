// Command control-center runs the swarm's Control Center: the TCP endpoint
// every node dials for telemetry, and the HTTP/WebSocket server behind the
// dashboard.
//
// It is wiring only. The hub, the node server and the API live in
// pkg/controlcenter. The dashboard is not embedded: it is a static site served
// by the frontend container (frontend/), which proxies /api, /healthz and /ws
// here.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"swarm-net/pkg/controlcenter"
)

// version is overridden at link time with -ldflags "-X main.version=<tag>".
var version = "dev"

const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2

	// shutdownGrace bounds how long in-flight HTTP requests get on SIGTERM.
	// Compose waits 10s before SIGKILL; finishing well inside that keeps the
	// "shutdown complete" log line reachable.
	shutdownGrace = 5 * time.Second
	// readHeaderTimeout stops a client that opens a socket and dribbles
	// headers from holding a goroutine (Slowloris).
	readHeaderTimeout = 5 * time.Second
)

func main() {
	cfg, err := Load(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "control-center:", err)
		os.Exit(exitCode(err))
	}
	if cfg.Version {
		fmt.Fprintf(os.Stdout, "control-center %s\n", version)
		return
	}
	// SIGTERM from `docker stop`, SIGINT from Ctrl-C: both become one
	// cancelled context. A second signal kills via the restored default.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, cfg, os.Stderr, nil)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "control-center:", err)
		os.Exit(exitCode(err))
	}
}

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

// run binds both listeners, reports their addresses through onListening (nil
// is fine), and serves until ctx is cancelled or the HTTP server fails.
//
// Shutdown order: stop the hub first, which drops every WebSocket (a hijacked
// connection is invisible to http.Server.Shutdown, which would otherwise wait
// on nothing and leave them open) and closes every node link; then drain the
// ordinary HTTP requests.
func run(ctx context.Context, cfg Config, stderr io.Writer, onListening func(nodeAddr, httpAddr net.Addr)) error {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// HTTP is bound first because a bare net.Listener is trivial to release
	// if the node listener then fails; the reverse would need a Server that
	// was never run to be torn down.
	httpLn, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("control-center: listen http on %s: %w", cfg.HTTPListen, err)
	}
	cc, err := controlcenter.New(controlcenter.Config{
		NodeListen: cfg.NodeListen,
		Logger:     log,
	})
	if err != nil {
		_ = httpLn.Close()
		return fmt.Errorf("control-center: %w", err)
	}
	if onListening != nil {
		onListening(cc.NodeAddr(), httpLn.Addr())
	}
	log.Info("control center listening", "nodes", cc.NodeAddr(), "http", httpLn.Addr(), "version", version)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv := &http.Server{
		Handler:           cc.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// HTTP server goroutine: owned by run, joined via httpErr below. It ends
	// when Shutdown is called or Serve fails; a failure cancels runCtx so the
	// hub stops too.
	httpErr := make(chan error, 1)
	go func() {
		err := srv.Serve(httpLn)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else {
			cancel()
		}
		httpErr <- err
	}()

	_ = cc.Run(runCtx) // returns once runCtx is done and everything is joined
	log.Info("hub stopped; draining http")

	sctx, scancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("http shutdown incomplete", "err", err)
	}
	serveErr := <-httpErr
	log.Info("shutdown complete")
	if serveErr != nil {
		return fmt.Errorf("control-center: http server: %w", serveErr)
	}
	return nil
}
