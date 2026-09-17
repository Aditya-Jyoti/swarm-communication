package main

import (
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"swarm-net/pkg/cluster"
	"swarm-net/pkg/protocol"
)

func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func host(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(nil, noEnv, host("box"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		NodeID:         "box",
		Listen:         ":7000",
		Advertise:      "box:7000",
		Threshold:      cluster.DefaultThreshold,
		ProbeInterval:  cluster.DefaultProbeInterval,
		ElectionFloor:  cluster.DefaultElectionFloor,
		GossipInterval: cluster.DefaultGossipInterval,
		IdleTimeout:    15 * time.Second,
		LogLevel:       slog.LevelInfo,
	}
	if len(cfg.Seeds) != 0 || cfg.ControlCenter != "" || cfg.Version {
		t.Fatalf("unexpected non-default fields: %+v", cfg)
	}
	cfg.Seeds = nil
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("defaults:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestLoadEnv(t *testing.T) {
	env := envOf(map[string]string{
		"SWARM_NODE_ID":         "n1",
		"SWARM_LISTEN":          "0.0.0.0:9000",
		"SWARM_ADVERTISE":       "node1:9000",
		"SWARM_SEEDS":           "node2:9000,node3:9000",
		"SWARM_CONTROL_CENTER":  "cc:8080",
		"SWARM_THRESHOLD":       "0.5",
		"SWARM_PROBE_INTERVAL":  "500ms",
		"SWARM_ELECTION_FLOOR":  "10s",
		"SWARM_GOSSIP_INTERVAL": "750ms",
		"SWARM_IDLE_TIMEOUT":    "4s",
		"SWARM_LOG_LEVEL":       "debug",
	})
	failHost := func() (string, error) { return "", errors.New("must not be called") }
	cfg, err := Load(nil, env, failHost)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeID != "n1" || cfg.Listen != "0.0.0.0:9000" || cfg.Advertise != "node1:9000" ||
		cfg.ControlCenter != "cc:8080" || cfg.Threshold != 0.5 ||
		cfg.ProbeInterval != 500*time.Millisecond || cfg.ElectionFloor != 10*time.Second ||
		cfg.GossipInterval != 750*time.Millisecond ||
		cfg.IdleTimeout != 4*time.Second || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("env not applied: %+v", cfg)
	}
	if len(cfg.Seeds) != 2 || cfg.Seeds[0] != "node2:9000" || cfg.Seeds[1] != "node3:9000" {
		t.Fatalf("seeds: %v", cfg.Seeds)
	}
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	env := envOf(map[string]string{
		"SWARM_NODE_ID":         "env-id",
		"SWARM_LISTEN":          ":1",
		"SWARM_ADVERTISE":       "env:1",
		"SWARM_SEEDS":           "env-seed:1",
		"SWARM_CONTROL_CENTER":  "env-cc:1",
		"SWARM_THRESHOLD":       "0.1",
		"SWARM_PROBE_INTERVAL":  "1s",
		"SWARM_ELECTION_FLOOR":  "1m",
		"SWARM_GOSSIP_INTERVAL": "1s",
		"SWARM_IDLE_TIMEOUT":    "1m",
		"SWARM_LOG_LEVEL":       "error",
	})
	args := []string{
		"-node-id=flag-id", "-listen=:2", "-advertise=flag:2", "-seeds=flag-seed:2",
		"-control-center=flag-cc:2", "-threshold=0.9", "-probe-interval=2s",
		"-election-floor=2m", "-gossip-interval=5s", "-idle-timeout=2m", "-log-level=warn",
	}
	cfg, err := Load(args, env, host("box"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeID != "flag-id" || cfg.Listen != ":2" || cfg.Advertise != "flag:2" ||
		cfg.ControlCenter != "flag-cc:2" || cfg.Threshold != 0.9 ||
		cfg.ProbeInterval != 2*time.Second || cfg.ElectionFloor != 2*time.Minute ||
		cfg.GossipInterval != 5*time.Second ||
		cfg.IdleTimeout != 2*time.Minute || cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("flags did not override env: %+v", cfg)
	}
	if len(cfg.Seeds) != 1 || cfg.Seeds[0] != "flag-seed:2" {
		t.Fatalf("seeds: %v", cfg.Seeds)
	}
}

func TestLoadAdvertiseDefaultUsesListenPort(t *testing.T) {
	cfg, err := Load([]string{"-listen=:8123"}, noEnv, host("h"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Advertise != "h:8123" {
		t.Fatalf("advertise = %q, want h:8123", cfg.Advertise)
	}
}

func TestLoadSeeds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []protocol.NodeAddress
	}{
		{"self dropped", "a:1,self:7000,b:1", []protocol.NodeAddress{"a:1", "b:1"}},
		{"duplicates deduped", "a:1,b:1,a:1", []protocol.NodeAddress{"a:1", "b:1"}},
		{"whitespace trimmed", " a:1 , b:1 ,, ", []protocol.NodeAddress{"a:1", "b:1"}},
		{"only self", "self:7000", nil},
		{"empty", "", nil},
		{"ipv6", "[::1]:1", []protocol.NodeAddress{"[::1]:1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load([]string{"-advertise=self:7000", "-seeds=" + tc.raw}, noEnv, host("self"))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Seeds) != len(tc.want) {
				t.Fatalf("seeds = %v, want %v", cfg.Seeds, tc.want)
			}
			for i := range tc.want {
				if cfg.Seeds[i] != tc.want[i] {
					t.Fatalf("seeds = %v, want %v", cfg.Seeds, tc.want)
				}
			}
		})
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"empty node id", nil, map[string]string{"SWARM_NODE_ID": "  "}, "hostname lookup failed"},
		{"listen not host:port", []string{"-listen=7000"}, nil, "listen \"7000\" is not host:port"},
		{"listen empty", []string{"-listen="}, nil, "listen must not be empty"},
		{"listen bad port", []string{"-listen=:abc"}, nil, "invalid port"},
		{"listen port too large", []string{"-listen=:70000"}, nil, "invalid port"},
		{"advertise not host:port", []string{"-advertise=node1"}, nil, "advertise \"node1\" is not host:port"},
		{"advertise no host", []string{"-advertise=:7000"}, nil, "needs a host"},
		{"seed not host:port", []string{"-seeds=a:1,bad"}, nil, "seed \"bad\" is not host:port"},
		{"seed no host", []string{"-seeds=:1"}, nil, "seed \":1\" needs a host"},
		{"control center bad", []string{"-control-center=cc"}, nil, "control-center \"cc\" is not host:port"},
		{"threshold zero", []string{"-threshold=0"}, nil, "threshold 0 must be in (0,1]"},
		{"threshold negative", []string{"-threshold=-0.5"}, nil, "must be in (0,1]"},
		{"threshold above one", []string{"-threshold=1.5"}, nil, "must be in (0,1]"},
		{"threshold nan", []string{"-threshold=NaN"}, nil, "must be in (0,1]"},
		{"threshold garbage", []string{"-threshold=abc"}, nil, "threshold \"abc\""},
		{"probe interval zero", []string{"-probe-interval=0s"}, nil, "probe-interval 0s must be positive"},
		{"probe interval negative", []string{"-probe-interval=-1s"}, nil, "must be positive"},
		{"probe interval garbage", []string{"-probe-interval=soon"}, nil, "probe-interval \"soon\""},
		{"election floor zero", []string{"-election-floor=0"}, nil, "election-floor 0s must be positive"},
		{"election floor garbage", []string{"-election-floor=x"}, nil, "election-floor \"x\""},
		{"gossip interval zero", []string{"-gossip-interval=0s"}, nil, "gossip-interval 0s must be positive"},
		{"gossip interval negative", []string{"-gossip-interval=-2s"}, nil, "gossip-interval -2s must be positive"},
		{"gossip interval garbage", []string{"-gossip-interval=often"}, nil, "gossip-interval \"often\""},
		{"gossip interval env garbage", nil, map[string]string{"SWARM_GOSSIP_INTERVAL": "often"}, "gossip-interval \"often\""},
		{"idle timeout zero", []string{"-idle-timeout=0"}, nil, "idle-timeout 0s must be positive"},
		{"idle timeout garbage", []string{"-idle-timeout=x"}, nil, "idle-timeout \"x\""},
		{"idle timeout equals 3x probe", []string{"-probe-interval=1s", "-idle-timeout=3s"}, nil, "idle-timeout 3s must exceed 3*probe-interval (3s)"},
		{"idle timeout below 3x probe", []string{"-probe-interval=5s"}, nil, "must exceed 3*probe-interval"},
		{"unknown log level", []string{"-log-level=loud"}, nil, "log-level \"loud\""},
		{"unknown flag", []string{"-bogus"}, nil, "flag provided but not defined"},
		{"help", []string{"-h"}, nil, "help requested"},
		{"env error surfaces too", nil, map[string]string{"SWARM_IDLE_TIMEOUT": "never"}, "idle-timeout \"never\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hostname := host("box")
			if tc.name == "empty node id" {
				hostname = func() (string, error) { return "", errors.New("no hostname") }
			}
			_, err := Load(tc.args, envOf(tc.env), hostname)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !errors.Is(err, errConfig) {
				t.Fatalf("error %v does not wrap errConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadHostnameErrorPropagates(t *testing.T) {
	boom := errors.New("gethostname: boom")
	failing := func() (string, error) { return "", boom }

	_, err := Load(nil, noEnv, failing)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "node id") {
		t.Fatalf("node id path: %v", err)
	}
	_, err = Load([]string{"-node-id=n1"}, noEnv, failing)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "advertise") {
		t.Fatalf("advertise path: %v", err)
	}
	// With both supplied the hostname is never needed.
	if _, err := Load([]string{"-node-id=n1", "-advertise=n1:7000"}, noEnv, failing); err != nil {
		t.Fatalf("hostname should not be required: %v", err)
	}
}

func TestLoadVersionFlagShortCircuits(t *testing.T) {
	// An otherwise-invalid configuration must not stop -version from working.
	cfg, err := Load([]string{"-version", "-threshold=9"}, noEnv, host("box"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Version {
		t.Fatal("Version not set")
	}
}

func TestParseLogLevelAliases(t *testing.T) {
	for raw, want := range map[string]slog.Level{
		"DEBUG": slog.LevelDebug, " info ": slog.LevelInfo, "warning": slog.LevelWarn, "Error": slog.LevelError,
	} {
		got, err := parseLogLevel(raw)
		if err != nil || got != want {
			t.Fatalf("parseLogLevel(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
}
