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

	"swarm-net/pkg/controlcenter"
	"swarm-net/pkg/geo"
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
	// APIToken, if set, is required on mutating API calls.
	// SWARM_CC_API_TOKEN, environment only: a flag would expose it in `ps`.
	APIToken string
	// SimEnabled starts the latency simulation switched on.
	// SWARM_SIM_ENABLED, default true.
	SimEnabled bool
	// SimParams is the starting latency model. SWARM_SIM_BASE_MS,
	// SWARM_SIM_PER_UNIT_MS and SWARM_SIM_JITTER_MS; defaults from geo.
	// Out of range is a config error here, unlike the API, which clamps: a
	// typo in a compose file should stop the CC, not be quietly corrected.
	SimParams geo.Params
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
	{"sim-enabled", "SIM_ENABLED", "true", "start with emulated latency on"},
	{"sim-base-ms", "SIM_BASE_MS", fmtFloat(geo.DefaultBaseMS), "emulated latency: fixed part, ms"},
	{"sim-per-unit-ms", "SIM_PER_UNIT_MS", fmtFloat(geo.DefaultPerUnitMS), "emulated latency: ms per unit of distance"},
	{"sim-jitter-ms", "SIM_JITTER_MS", fmtFloat(geo.DefaultJitterMS), "emulated latency: maximum random extra, ms"},
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

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
		APIToken:   getenv(envPrefix + "CC_API_TOKEN"),
	}
	if cfg.APIToken != "" {
		if err := controlcenter.CheckAPIToken(cfg.APIToken); err != nil {
			return Config{}, fmt.Errorf("%w: %s%s: %v", errConfig, envPrefix, "CC_API_TOKEN", err)
		}
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
	if cfg.SimEnabled, err = strconv.ParseBool(strings.TrimSpace(*raw["sim-enabled"])); err != nil {
		return Config{}, fmt.Errorf("%w: sim-enabled %q: want true or false", errConfig, *raw["sim-enabled"])
	}
	for _, f := range []struct {
		name string
		max  float64
		dst  *float64
	}{
		{"sim-base-ms", geo.MaxBaseMS, &cfg.SimParams.BaseMS},
		{"sim-per-unit-ms", geo.MaxPerUnitMS, &cfg.SimParams.PerUnitMS},
		{"sim-jitter-ms", geo.MaxJitterMS, &cfg.SimParams.JitterMS},
	} {
		if *f.dst, err = parseRange(f.name, *raw[f.name], f.max); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// parseRange parses a float in [0, max]. The negated comparison also rejects
// NaN, which ParseFloat accepts and every ordinary comparison lets through.
func parseRange(name, raw string, max float64) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %q: %v", errConfig, name, raw, err)
	}
	if !(v >= 0 && v <= max) {
		return 0, fmt.Errorf("%w: %s %v must be in [0,%v]", errConfig, name, v, max)
	}
	return v, nil
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
