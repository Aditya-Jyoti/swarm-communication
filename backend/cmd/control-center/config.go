package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
)

// errConfig is wrapped by every configuration failure so exitCode can map it
// to exit 2 ("fix the YAML") rather than exit 1 ("read the logs").
var errConfig = errors.New("invalid configuration")

const (
	defaultNodeListen = ":7000"
	defaultHTTPListen = ":8080"
	defaultLogLevel   = "info"
	envPrefix         = "SWARM_"
)

// Config is everything the control-center binary needs. Precedence: flags,
// then SWARM_* environment, then defaults -- the same rule as swarm-node.
type Config struct {
	// NodeListen is the TCP bind address nodes dial. SWARM_CC_LISTEN.
	NodeListen string
	// HTTPListen is the API and WebSocket bind address. SWARM_CC_HTTP.
	HTTPListen string
	// LogLevel is the slog threshold. SWARM_LOG_LEVEL.
	LogLevel slog.Level
	// Version is set by -version; nothing else is meaningful then.
	Version bool
}

type setting struct {
	flag, env, def, usage string
}

var settings = []setting{
	{"listen", "CC_LISTEN", defaultNodeListen, "TCP bind address for node connections"},
	{"http", "CC_HTTP", defaultHTTPListen, "HTTP bind address for the API and WebSocket"},
	{"log-level", "LOG_LEVEL", defaultLogLevel, "debug|info|warn|error"},
}

// Load builds a Config from args, the environment and defaults. Env values
// seed the flag defaults, so a flag the operator passed wins and one they did
// not still carries the env value.
func Load(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("control-center", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	raw := make(map[string]*string, len(settings))
	for _, s := range settings {
		def := s.def
		if v := getenv(envPrefix + s.env); v != "" {
			def = v
		}
		raw[s.flag] = fs.String(s.flag, def, s.usage)
	}
	version := fs.Bool("version", false, "print build version and exit")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("%w: %v", errConfig, err)
	}
	if *version {
		return Config{Version: true}, nil
	}

	cfg := Config{
		NodeListen: strings.TrimSpace(*raw["listen"]),
		HTTPListen: strings.TrimSpace(*raw["http"]),
	}
	if err := checkBind("listen", cfg.NodeListen); err != nil {
		return Config{}, err
	}
	if err := checkBind("http", cfg.HTTPListen); err != nil {
		return Config{}, err
	}
	var err error
	if cfg.LogLevel, err = parseLogLevel(*raw["log-level"]); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checkBind validates a bind address. An empty host is fine (all interfaces).
func checkBind(name, v string) error {
	if v == "" {
		return fmt.Errorf("%w: %s must not be empty", errConfig, name)
	}
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%w: %s %q is not host:port: %v", errConfig, name, v, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("%w: %s %q has an invalid port", errConfig, name, v)
	}
	return nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%w: log-level %q: want debug|info|warn|error", errConfig, raw)
}
