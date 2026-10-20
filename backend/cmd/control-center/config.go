package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"swarm-net/pkg/controlcenter"
	"swarm-net/pkg/geo"
	"swarm-net/pkg/mavlink"
	"swarm-net/pkg/protocol"
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
	// SimHysteresis is the starting election margin pushed to every node, or
	// negative for "leave each node's own". SWARM_SIM_HYSTERESIS.
	SimHysteresis float64
	// MAV is the MAVLink bridge configuration. Off unless MAV_ENABLED.
	MAV MAVSettings
	// Version is set by -version; nothing else is meaningful then.
	Version bool
}

// MAVSettings is the MAVLink bridge's slice of the configuration
// (docs/architecture/mavlink-bridge.md sections 2, 6, 7 and 8).
//
// A plain comparable struct rather than a mavlink.BridgeConfig: BridgeConfig
// carries a logger, a clock and the swarm source, none of which belong to
// flag parsing, and all of which would make Config incomparable. main turns
// this into a BridgeConfig.
type MAVSettings struct {
	// Enabled runs the bridge. MAV_ENABLED, default false: a UDP port is
	// bound and telemetry is emitted only when someone asked for it.
	Enabled bool
	// Bind is the UDP bind address. MAV_BIND.
	Bind string
	// GCS is the ground station address, may be empty. MAV_GCS.
	GCS string
	// Projection is the lab origin and scale. MAV_ORIGIN_LAT, MAV_ORIGIN_LON,
	// MAV_ORIGIN_ALT, MAV_METRES_PER_UNIT.
	Projection mavlink.Projection
	// SpeedUnitsPerSec is flight speed. MAV_SPEED_UNITS_S.
	SpeedUnitsPerSec float64
	// Motion is what an idle node does: drift, patrol or still. MAV_MOTION,
	// default drift (the behaviour that predates this knob).
	Motion mavlink.IdleMode
	// Drift turns idle drift on. MAV_DRIFT, default true. Positive here and
	// inverted for mavlink.MoveConfig, because an operator reads MAV_DRIFT,
	// not a zero value.
	Drift bool
	// DriftRadius bounds drift from home, in cube units. MAV_DRIFT_RADIUS.
	DriftRadius float64
	// DriftSpeed is drift speed in units per second. MAV_DRIFT_SPEED.
	DriftSpeed float64
	// LiveNode is the node whose instruments come from a real aircraft, empty
	// for none. MAV_LIVE_NODE. See mavlink.LiveConfig.
	LiveNode string
	// LiveBind is the live feed's UDP address. MAV_LIVE_BIND.
	LiveBind string
}

// Bridge turns the settings into a bridge configuration. The logger, the clock
// and the swarm source are filled in by the Control Center.
func (m MAVSettings) Bridge() mavlink.BridgeConfig {
	return mavlink.BridgeConfig{
		Bind:       m.Bind,
		GCS:        m.GCS,
		Projection: m.Projection,
		Move: mavlink.MoveConfig{
			SpeedUnitsPerSec: m.SpeedUnitsPerSec,
			Idle:             m.Motion,
			DriftDisabled:    !m.Drift,
			DriftRadius:      m.DriftRadius,
			DriftSpeed:       m.DriftSpeed,
		},
		Live: mavlink.LiveConfig{Node: protocol.NodeID(m.LiveNode), Bind: m.LiveBind},
	}
}

type setting struct {
	flag, env, def, usage string
	// noPrefix looks the environment variable up under its bare name instead
	// of SWARM_*. The MAV_* names are fixed by the bridge contract, which is
	// what an operator's .env and QGroundControl documentation both say, so
	// they are not renamed for internal consistency.
	noPrefix bool
}

var settings = []setting{
	{"listen", "CC_LISTEN", defaultNodeListen, "TCP bind address for node connections", false},
	{"http", "CC_HTTP", defaultHTTPListen, "HTTP bind address for the API and WebSocket", false},
	{"log-level", "LOG_LEVEL", defaultLogLevel, "debug|info|warn|error", false},
	{"sim-enabled", "SIM_ENABLED", "true", "start with emulated latency on", false},
	{"sim-base-ms", "SIM_BASE_MS", fmtFloat(geo.DefaultBaseMS), "emulated latency: fixed part, ms", false},
	{"sim-per-unit-ms", "SIM_PER_UNIT_MS", fmtFloat(geo.DefaultPerUnitMS), "emulated latency: ms per unit of distance", false},
	{"sim-jitter-ms", "SIM_JITTER_MS", fmtFloat(geo.DefaultJitterMS), "emulated latency: maximum random extra, ms", false},
	{"sim-hysteresis", "SIM_HYSTERESIS", "-1", "election margin pushed to every node; -1 leaves each node's own", false},

	{flag: "mav-enabled", env: "MAV_ENABLED", def: "false", usage: "expose every node to a ground station over MAVLink", noPrefix: true},
	{flag: "mav-bind", env: "MAV_BIND", def: mavlink.DefaultBind, usage: "UDP bind address for MAVLink", noPrefix: true},
	{flag: "mav-gcs", env: "MAV_GCS", def: "", usage: "ground station address to send to; empty learns it from inbound packets", noPrefix: true},
	{flag: "mav-origin-lat", env: "MAV_ORIGIN_LAT", def: fmtFloat(mavlink.DefaultOriginLat), usage: "lab origin latitude, degrees", noPrefix: true},
	{flag: "mav-origin-lon", env: "MAV_ORIGIN_LON", def: fmtFloat(mavlink.DefaultOriginLon), usage: "lab origin longitude, degrees", noPrefix: true},
	{flag: "mav-origin-alt", env: "MAV_ORIGIN_ALT", def: fmtFloat(mavlink.DefaultOriginAltM), usage: "lab origin altitude, metres AMSL", noPrefix: true},
	{flag: "mav-metres-per-unit", env: "MAV_METRES_PER_UNIT", def: fmtFloat(mavlink.DefaultMetresPerUnit), usage: "size of one cube unit in metres", noPrefix: true},
	{flag: "mav-speed-units-s", env: "MAV_SPEED_UNITS_S", def: fmtFloat(mavlink.DefaultSpeedUnitsPerSec), usage: "flight speed toward a waypoint, units per second", noPrefix: true},
	{flag: "mav-motion", env: "MAV_MOTION", def: string(mavlink.IdleDrift), usage: "what an idle node does: drift|patrol|still. patrol flies a slow circuit around home, which shows continuous movement in a demo -- but position drives emulated latency, so it also perturbs the election; raise SWARM_SIM_HYSTERESIS with it", noPrefix: true},
	{flag: "mav-drift", env: "MAV_DRIFT", def: "true", usage: "let idle nodes drift around their home position", noPrefix: true},
	{flag: "mav-drift-radius", env: "MAV_DRIFT_RADIUS", def: fmtFloat(mavlink.DefaultDriftRadius), usage: "maximum drift from home, units", noPrefix: true},
	{flag: "mav-drift-speed", env: "MAV_DRIFT_SPEED", def: fmtFloat(mavlink.DefaultDriftSpeed), usage: "drift speed, units per second", noPrefix: true},
	{flag: "mav-live-node", env: "MAV_LIVE_NODE", def: "", usage: "node whose instruments come from a real LiteWing via cmd/litewing-relay; empty for none", noPrefix: true},
	{flag: "mav-live-bind", env: "MAV_LIVE_BIND", def: mavlink.DefaultLiveBind, usage: "UDP bind address for the live LiteWing feed", noPrefix: true},
}

// Bounds for the MAVLink settings. Out of range is a configuration error, not
// a clamp: a mistyped origin puts the whole swarm in the sea, and silently
// correcting it would hide that.
const (
	maxMetresPerUnit = 10000
	maxSpeedUnitsS   = 100
	maxDriftSpeed    = 100
	// maxOriginAltM is above Everest and below any plausible typo-as-metres.
	maxOriginAltM = 20000
	minOriginAltM = -500
)

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
		key := envPrefix + s.env
		if s.noPrefix {
			key = s.env
		}
		if v := getenv(key); v != "" {
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
	if cfg.SimEnabled, err = parseBool("sim-enabled", *raw["sim-enabled"]); err != nil {
		return Config{}, err
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
	// Hysteresis spans [-1, maxSimHysteresis]: -1 is the documented "leave each
	// node's own margin alone", so it cannot go through parseRange's [0, max].
	if cfg.SimHysteresis, err = parseSpan("sim-hysteresis", *raw["sim-hysteresis"], -1, maxSimHysteresis); err != nil {
		return Config{}, err
	}
	if cfg.MAV, err = loadMAV(raw); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadMAV validates the MAVLink settings. Separate from Load only because
// eleven more settings in one function stops being readable.
func loadMAV(raw map[string]*string) (MAVSettings, error) {
	m := MAVSettings{
		Bind:     strings.TrimSpace(*raw["mav-bind"]),
		GCS:      strings.TrimSpace(*raw["mav-gcs"]),
		LiveNode: strings.TrimSpace(*raw["mav-live-node"]),
		LiveBind: strings.TrimSpace(*raw["mav-live-bind"]),
	}
	var err error
	if m.Enabled, err = parseBool("mav-enabled", *raw["mav-enabled"]); err != nil {
		return MAVSettings{}, err
	}
	if m.Drift, err = parseBool("mav-drift", *raw["mav-drift"]); err != nil {
		return MAVSettings{}, err
	}
	// A mistyped mode is a configuration error, like a mistyped origin: silently
	// falling back to drift would leave an operator watching for a patrol that
	// never starts.
	if m.Motion, err = mavlink.ParseIdleMode(strings.TrimSpace(*raw["mav-motion"])); err != nil {
		return MAVSettings{}, fmt.Errorf("%w: mav-motion: %v", errConfig, err)
	}
	if err = checkBind("mav-bind", m.Bind); err != nil {
		return MAVSettings{}, err
	}
	// An empty GCS is legal: the bridge learns the address from any inbound
	// packet (contract section 7).
	if m.GCS != "" {
		if err = checkBind("mav-gcs", m.GCS); err != nil {
			return MAVSettings{}, err
		}
	}
	if err = checkLive(m); err != nil {
		return MAVSettings{}, err
	}
	for _, f := range []struct {
		name     string
		min, max float64
		dst      *float64
	}{
		{"mav-origin-lat", -90, 90, &m.Projection.OriginLat},
		{"mav-origin-lon", -180, 180, &m.Projection.OriginLon},
		{"mav-origin-alt", minOriginAltM, maxOriginAltM, &m.Projection.OriginAltM},
		// Zero metres per unit would collapse the whole cube onto the origin,
		// so the lower bound is exclusive in effect: the smallest accepted
		// value is the smallest positive float64.
		{"mav-metres-per-unit", math.SmallestNonzeroFloat64, maxMetresPerUnit, &m.Projection.MetresPerUnit},
		{"mav-speed-units-s", math.SmallestNonzeroFloat64, maxSpeedUnitsS, &m.SpeedUnitsPerSec},
		// Drift radius and speed may be zero: that is "drift, but do not
		// move", which MAV_DRIFT=false says more clearly but which a sweep
		// over radii should still be allowed to express.
		{"mav-drift-radius", 0, geo.Size, &m.DriftRadius},
		{"mav-drift-speed", 0, maxDriftSpeed, &m.DriftSpeed},
	} {
		if *f.dst, err = parseSpan(f.name, *raw[f.name], f.min, f.max); err != nil {
			return MAVSettings{}, err
		}
	}
	return m, nil
}

// parseRange parses a float in [0, max].
// maxSimHysteresis caps the start-up election margin. It matches the API's own
// cap: the margin is in score units, and a score is an emulated round trip in
// milliseconds, so geo.MaxDelay is the largest margin that can mean anything.
const maxSimHysteresis = float64(geo.MaxDelay / time.Millisecond)

func parseRange(name, raw string, max float64) (float64, error) {
	return parseSpan(name, raw, 0, max)
}

// parseSpan parses a float in [min, max]. The negated comparison also rejects
// NaN, which ParseFloat accepts and every ordinary comparison lets through.
func parseSpan(name, raw string, min, max float64) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s %q: %v", errConfig, name, raw, err)
	}
	if !(v >= min && v <= max) {
		return 0, fmt.Errorf("%w: %s %v must be in [%v,%v]", errConfig, name, v, min, max)
	}
	return v, nil
}

// parseBool is strconv.ParseBool with a configuration error.
func parseBool(name, raw string) (bool, error) {
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%w: %s %q: want true or false", errConfig, name, raw)
	}
	return v, nil
}

// checkBind validates a bind address. An empty host is fine (all interfaces).
// checkLive validates the live-feed settings. The bind is checked even with no
// live node, so a typo is caught before someone turns the feed on.
func checkLive(m MAVSettings) error {
	if err := checkBind("mav-live-bind", m.LiveBind); err != nil {
		return err
	}
	// A node id with whitespace inside is a quoting mistake, and would
	// silently match no node.
	if strings.ContainsFunc(m.LiveNode, unicode.IsSpace) {
		return fmt.Errorf("%w: mav-live-node %q contains whitespace", errConfig, m.LiveNode)
	}
	// Two sockets on one port: the bridge would fail to start, so say why now.
	_, livePort, _ := net.SplitHostPort(m.LiveBind)
	_, mavPort, _ := net.SplitHostPort(m.Bind)
	if m.LiveNode != "" && livePort == mavPort {
		return fmt.Errorf("%w: mav-live-bind %q shares a port with mav-bind %q", errConfig, m.LiveBind, m.Bind)
	}
	return nil
}

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
