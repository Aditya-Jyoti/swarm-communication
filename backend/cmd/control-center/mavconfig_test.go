package main

import (
	"errors"
	"testing"

	"swarm-net/pkg/mavlink"
)

// defaultMAV is the MAVLink block every other test expects to see when nothing
// is configured. Spelled out rather than built from the mavlink defaults, so a
// change to a default has to be made deliberately in two places.
func defaultMAV() MAVSettings {
	return MAVSettings{
		Enabled: false,
		Bind:    "0.0.0.0:14550",
		GCS:     "",
		Projection: mavlink.Projection{
			OriginLat:     47.397971,
			OriginLon:     8.546164,
			OriginAltM:    488,
			MetresPerUnit: 20,
		},
		SpeedUnitsPerSec: 3,
		Motion:           mavlink.IdleDrift,
		Drift:            true,
		DriftRadius:      4,
		DriftSpeed:       0.4,
		LiveNode:         "",
		LiveBind:         ":14560",
	}
}

func TestLoadMAV(t *testing.T) {
	// The contract fixes the env var names without the SWARM_ prefix, because
	// that is what an operator's .env and the QGroundControl instructions say.
	env := map[string]string{
		"MAV_ENABLED":         "true",
		"MAV_BIND":            "127.0.0.1:0",
		"MAV_GCS":             "host.docker.internal:14550",
		"MAV_ORIGIN_LAT":      "-33.8688",
		"MAV_ORIGIN_LON":      " 151.2093 ",
		"MAV_ORIGIN_ALT":      "58",
		"MAV_METRES_PER_UNIT": "5",
		"MAV_SPEED_UNITS_S":   "10",
		"MAV_MOTION":          "patrol",
		"MAV_DRIFT":           "false",
		"MAV_DRIFT_RADIUS":    "0",
		"MAV_DRIFT_SPEED":     "0",
		"MAV_LIVE_NODE":       " swarm-net-node-9 ",
		"MAV_LIVE_BIND":       "0.0.0.0:14561",
	}
	getenv := func(k string) string { return env[k] }
	cfg, err := Load(nil, getenv)
	if err != nil {
		t.Fatal(err)
	}
	want := MAVSettings{
		Enabled:    true,
		Bind:       "127.0.0.1:0",
		GCS:        "host.docker.internal:14550",
		Projection: mavlink.Projection{OriginLat: -33.8688, OriginLon: 151.2093, OriginAltM: 58, MetresPerUnit: 5},

		SpeedUnitsPerSec: 10,
		Motion:           mavlink.IdlePatrol,
		LiveNode:         "swarm-net-node-9",
		LiveBind:         "0.0.0.0:14561",
		Drift:            false,
	}
	if cfg.MAV != want {
		t.Fatalf("MAV from env = %+v, want %+v", cfg.MAV, want)
	}

	// The prefixed name is NOT accepted: one spelling only, so an operator
	// cannot set a variable that silently does nothing.
	cfg, err = Load(nil, func(k string) string {
		if k == "SWARM_MAV_ENABLED" {
			return "true"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MAV.Enabled {
		t.Error("SWARM_MAV_ENABLED was honoured; the contract name is MAV_ENABLED")
	}

	// Flags beat env.
	cfg, err = Load([]string{"-mav-drift=true", "-mav-drift-radius=12.5"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MAV.Drift || cfg.MAV.DriftRadius != 12.5 || cfg.MAV.SpeedUnitsPerSec != 10 {
		t.Fatalf("MAV flags = %+v", cfg.MAV)
	}

	for _, args := range [][]string{
		{"-mav-enabled=maybe"},
		{"-mav-drift=maybe"},
		{"-mav-bind="},
		{"-mav-bind=14550"},
		{"-mav-bind=:99999"},
		{"-mav-gcs=qgc"},
		{"-mav-origin-lat=91"},
		{"-mav-origin-lat=-91"},
		{"-mav-origin-lat=NaN"},
		{"-mav-origin-lon=181"},
		{"-mav-origin-alt=-501"},
		{"-mav-origin-alt=20001"},
		// Zero metres per unit collapses the cube onto the origin: every node
		// at the same place, every emulated distance zero.
		{"-mav-metres-per-unit=0"},
		{"-mav-metres-per-unit=-20"},
		{"-mav-speed-units-s=0"},
		{"-mav-speed-units-s=101"},
		{"-mav-drift-radius=-1"},
		// The radius is in cube units, so more than the cube is a typo.
		{"-mav-drift-radius=101"},
		{"-mav-drift-speed=-0.1"},
		{"-mav-drift-speed=fast"},
		// A mistyped motion mode must not silently fall back to drift: the
		// operator would watch for a patrol that never starts.
		{"-mav-motion=patroll"},
		{"-mav-motion=Patrol"},
		{"-mav-live-bind="},
		{"-mav-live-bind=14560"},
		{"-mav-live-bind=:70000"},
		// A node id with a space in it is a quoting mistake in .env, and would
		// silently match no node at all.
		{"-mav-live-node=swarm net node 9"},
		// The live feed cannot share the MAVLink port.
		{"-mav-live-node=seed", "-mav-live-bind=:14550"},
	} {
		if _, err := Load(args, noEnv); !errors.Is(err, errConfig) || exitCode(err) != exitConfig {
			t.Errorf("Load(%v) = %v, want config error", args, err)
		}
	}
}

func TestMAVSettingsBridge(t *testing.T) {
	m := defaultMAV()
	m.Drift = false
	bc := m.Bridge()
	// MAV_DRIFT is positive; MoveConfig is inverted so that its zero value is
	// the contract default. Getting that backwards would leave every node
	// frozen with MAV_DRIFT=true, which looks like a broken bridge.
	if !bc.Move.DriftDisabled {
		t.Error("MAV_DRIFT=false did not disable drift")
	}
	m.Drift = true
	// MAV_MOTION reaches the mover, and MAV_DRIFT=false still overrides it.
	m.Motion = mavlink.IdlePatrol
	if got := m.Bridge().Move.Idle; got != mavlink.IdlePatrol {
		t.Errorf("MAV_MOTION=patrol reached the mover as %q", got)
	}
	if m.Bridge().Move.DriftDisabled {
		t.Error("MAV_DRIFT=true disabled drift")
	}
	if bc.Projection != m.Projection || bc.Bind != m.Bind {
		t.Errorf("Bridge() = %+v", bc)
	}
	// The live feed is off unless a node is named, and reaches the bridge
	// verbatim when one is.
	if bc.Live.Node != "" {
		t.Errorf("live node %q with nothing configured", bc.Live.Node)
	}
	m.LiveNode, m.LiveBind = "swarm-net-node-9", "127.0.0.1:14561"
	if l := m.Bridge().Live; l.Node != "swarm-net-node-9" || l.Bind != "127.0.0.1:14561" {
		t.Errorf("Bridge().Live = %+v", l)
	}
}

// TestSimHysteresisConfig: the start-up election margin. -1 means "leave each
// node's own alone", and 0 is a real value (any improvement wins), which is why
// the Server takes a pointer.
func TestSimHysteresisConfig(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
		bad  bool
	}{
		{raw: "", want: -1},
		{raw: "0", want: 0},
		{raw: "0.8", want: 0.8},
		{raw: "-1", want: -1},
		{raw: "-2", bad: true},
		{raw: "99999", bad: true},
		{raw: "nonsense", bad: true},
	} {
		t.Run("hysteresis="+tc.raw, func(t *testing.T) {
			env := map[string]string{}
			if tc.raw != "" {
				env["SWARM_SIM_HYSTERESIS"] = tc.raw
			}
			cfg, err := Load(nil, func(k string) string { return env[k] })
			if tc.bad {
				if err == nil {
					t.Fatalf("%q was accepted, got %v", tc.raw, cfg.SimHysteresis)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.raw, err)
			}
			if cfg.SimHysteresis != tc.want {
				t.Errorf("SimHysteresis = %v, want %v", cfg.SimHysteresis, tc.want)
			}
		})
	}
}
