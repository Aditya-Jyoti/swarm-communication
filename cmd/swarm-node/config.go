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
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/protocol"
)

// errConfig is the sentinel every configuration failure wraps, so exitCode can
// tell "the operator typed it wrong" (exit 2) from "the world broke" (exit 1)
// without inspecting messages.
var errConfig = errors.New("invalid configuration")

// Defaults not owned by a pkg constant.
const (
	defaultListen        = ":7000"
	defaultIdleTimeout   = 15 * time.Second
	defaultLogLevel      = "info"
	envPrefix            = "SWARM_"
	seedSeparator        = ","
	idleTimeoutMinFactor = 3
)

// Config is everything the swarm-node binary needs to start. It is assembled by
// Load from flags (highest precedence), environment (SWARM_*) and defaults, and
// is validated before any socket is opened: a bad value should fail the process
// at exit 2 in milliseconds, not after it has joined the mesh.
type Config struct {
	// NodeID is announced in every handshake and envelope. SWARM_NODE_ID, default
	// os.Hostname, which in Docker Compose is the container name -- exactly the
	// stable, human-meaningful identity NodeID is meant to be.
	NodeID protocol.NodeID
	// Listen is the bind address. SWARM_LISTEN, default ":7000".
	Listen string
	// Advertise is the host:port peers dial to reach this node. It differs from
	// Listen because a bind of ":7000" is not dialable; peers need a name. Default
	// "<hostname>:<listen port>", the Compose service DNS name.
	Advertise protocol.NodeAddress
	// Seeds are the addresses dialled at start-up. SWARM_SEEDS, comma-separated.
	// Empty is legal: the first node has nobody to call.
	Seeds []protocol.NodeAddress
	// ControlCenter is the coordinator's address. SWARM_CONTROL_CENTER, optional.
	// Parsed and carried now; Phase 5 wires it.
	ControlCenter protocol.NodeAddress
	// Threshold is the leader fraction in (0,1]. SWARM_THRESHOLD.
	Threshold float64
	// ProbeInterval is how often peers are scored. SWARM_PROBE_INTERVAL.
	ProbeInterval time.Duration
	// ElectionFloor is the periodic re-election safety net. SWARM_ELECTION_FLOOR.
	ElectionFloor time.Duration
	// GossipInterval is how often one peer is sent this node's full membership
	// view (anti-entropy). SWARM_GOSSIP_INTERVAL. A full repair cycle over N
	// peers takes N intervals.
	GossipInterval time.Duration
	// IdleTimeout is how long a connection may be silent before it is declared
	// dead. SWARM_IDLE_TIMEOUT. Must exceed 3*ProbeInterval; see validate.
	IdleTimeout time.Duration
	// LogLevel is the slog threshold. SWARM_LOG_LEVEL debug|info|warn|error.
	LogLevel slog.Level
	// Version is set by the -version flag. When true nothing else in the Config is
	// meaningful and main prints the build string and exits 0.
	Version bool
}

// setting is one configurable value: its flag name, env suffix, default, and a
// one-line description. Keeping these in a table means the flag and the env var
// cannot drift apart and the precedence rule is applied uniformly.
type setting struct {
	flag, env, def, usage string
}

var settings = []setting{
	{"node-id", "NODE_ID", "", "node identity (default: hostname)"},
	{"listen", "LISTEN", defaultListen, "TCP bind address"},
	{"advertise", "ADVERTISE", "", "host:port peers dial to reach this node (default: <hostname>:<listen port>)"},
	{"seeds", "SEEDS", "", "comma-separated peer addresses to dial at start-up"},
	{"control-center", "CONTROL_CENTER", "", "control-center address (optional)"},
	{"threshold", "THRESHOLD", strconv.FormatFloat(cluster.DefaultThreshold, 'g', -1, 64), "leader fraction in (0,1]"},
	{"probe-interval", "PROBE_INTERVAL", cluster.DefaultProbeInterval.String(), "how often peers are scored"},
	{"election-floor", "ELECTION_FLOOR", cluster.DefaultElectionFloor.String(), "periodic re-election interval"},
	{"gossip-interval", "GOSSIP_INTERVAL", cluster.DefaultGossipInterval.String(), "how often one peer is sent the full membership view"},
	{"idle-timeout", "IDLE_TIMEOUT", defaultIdleTimeout.String(), "silence before a connection is declared dead (> 3*probe-interval)"},
	{"log-level", "LOG_LEVEL", defaultLogLevel, "debug|info|warn|error"},
}

// Load builds a Config from args (flags), the environment and defaults, in that
// order of precedence, then validates it. Every error wraps errConfig.
//
// Precedence is implemented by seeding each flag's default with the environment
// value: after Parse, a flag the operator did not pass still holds the env value,
// and one they did pass holds theirs. Values are kept as strings until after
// parsing so that a malformed duration produces the same error whether it came
// from a flag or from the environment.
func Load(args []string, getenv func(string) string, hostname func() (string, error)) (Config, error) {
	fs := flag.NewFlagSet("swarm-node", flag.ContinueOnError)
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

	var cfg Config
	var err error

	cfg.NodeID = protocol.NodeID(strings.TrimSpace(*raw["node-id"]))
	if cfg.NodeID == "" {
		h, herr := hostname()
		if herr != nil {
			return Config{}, fmt.Errorf("%w: node id unset and hostname lookup failed: %v", errConfig, herr)
		}
		cfg.NodeID = protocol.NodeID(h)
	}

	cfg.Listen = strings.TrimSpace(*raw["listen"])
	if err := checkHostPort("listen", cfg.Listen, false); err != nil {
		return Config{}, err
	}

	cfg.Advertise = protocol.NodeAddress(strings.TrimSpace(*raw["advertise"]))
	if cfg.Advertise == "" {
		h, herr := hostname()
		if herr != nil {
			return Config{}, fmt.Errorf("%w: advertise unset and hostname lookup failed: %v", errConfig, herr)
		}
		_, port, _ := net.SplitHostPort(cfg.Listen) // validated above
		cfg.Advertise = protocol.NodeAddress(net.JoinHostPort(h, port))
	}
	if err := checkHostPort("advertise", string(cfg.Advertise), true); err != nil {
		return Config{}, err
	}

	if cfg.Seeds, err = parseSeeds(*raw["seeds"], cfg.Advertise); err != nil {
		return Config{}, err
	}

	cfg.ControlCenter = protocol.NodeAddress(strings.TrimSpace(*raw["control-center"]))
	if cfg.ControlCenter != "" {
		if err := checkHostPort("control-center", string(cfg.ControlCenter), true); err != nil {
			return Config{}, err
		}
	}

	if cfg.Threshold, err = strconv.ParseFloat(strings.TrimSpace(*raw["threshold"]), 64); err != nil {
		return Config{}, fmt.Errorf("%w: threshold %q: %v", errConfig, *raw["threshold"], err)
	}
	if !(cfg.Threshold > 0 && cfg.Threshold <= 1) {
		return Config{}, fmt.Errorf("%w: threshold %v must be in (0,1]", errConfig, cfg.Threshold)
	}

	if cfg.ProbeInterval, err = parseDuration("probe-interval", *raw["probe-interval"]); err != nil {
		return Config{}, err
	}
	if cfg.ElectionFloor, err = parseDuration("election-floor", *raw["election-floor"]); err != nil {
		return Config{}, err
	}
	if cfg.GossipInterval, err = parseDuration("gossip-interval", *raw["gossip-interval"]); err != nil {
		return Config{}, err
	}
	if cfg.IdleTimeout, err = parseDuration("idle-timeout", *raw["idle-timeout"]); err != nil {
		return Config{}, err
	}
	// The idle deadline is the transport's failure detector: a link with no frame
	// for IdleTimeout is closed. The probe round is the only guaranteed traffic
	// on an otherwise quiet link, and one round can be late by up to a probe
	// timeout and a scheduling hiccup. Requiring three intervals of margin means
	// a single slow or lost round cannot reap a healthy peer; without it every
	// GC pause would look like a death and the mesh would flap.
	if cfg.IdleTimeout <= idleTimeoutMinFactor*cfg.ProbeInterval {
		return Config{}, fmt.Errorf("%w: idle-timeout %v must exceed %d*probe-interval (%v)",
			errConfig, cfg.IdleTimeout, idleTimeoutMinFactor, idleTimeoutMinFactor*cfg.ProbeInterval)
	}

	if cfg.LogLevel, err = parseLogLevel(*raw["log-level"]); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checkHostPort validates a "host:port" string. needHost is true for addresses
// that peers must dial (a bare ":7000" cannot be dialled) and false for a bind
// address, where an empty host means "every interface".
func checkHostPort(name, v string, needHost bool) error {
	if v == "" {
		return fmt.Errorf("%w: %s must not be empty", errConfig, name)
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%w: %s %q is not host:port: %v", errConfig, name, v, err)
	}
	if needHost && host == "" {
		return fmt.Errorf("%w: %s %q needs a host: peers cannot dial a bare port", errConfig, name, v)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("%w: %s %q has an invalid port", errConfig, name, v)
	}
	return nil
}

// parseSeeds splits, trims, validates and dedupes the seed list. A seed equal to
// our own advertise address is dropped: dialling yourself is detected by the
// handshake and abandoned, but only after a wasted dial, and the log line it
// leaves reads like a fault.
func parseSeeds(raw string, self protocol.NodeAddress) ([]protocol.NodeAddress, error) {
	var out []protocol.NodeAddress
	seen := make(map[protocol.NodeAddress]struct{})
	for _, part := range strings.Split(raw, seedSeparator) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if err := checkHostPort("seed", part, true); err != nil {
			return nil, err
		}
		addr := protocol.NodeAddress(part)
		if addr == self {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	return out, nil
}

func parseDuration(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%w: %s %q: %v", errConfig, name, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %s %v must be positive", errConfig, name, d)
	}
	return d, nil
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
