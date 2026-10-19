package controlcenter

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarm-net/pkg/mavlink"
	"swarm-net/pkg/protocol"
)

// --- a strict exposition parser --------------------------------------------------
//
// Deliberately not a library and deliberately unforgiving: the point of these
// tests is that a real scraper would accept the response, and a real scraper
// rejects a second TYPE line, an interleaved family, a duplicate series and an
// unescaped quote without telling anyone. The parser below fails the test
// instead.

type expoFamily struct {
	name    string
	typ     string
	help    string
	samples []expoSample
	// order is the position of this family's header in the response, used to
	// prove families are not interleaved.
	order int
}

type expoSample struct {
	name   string
	labels map[string]string
	value  float64
}

// key is the series identity a scraper would use: name plus sorted labels.
func (s expoSample) key() string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, s.labels[k])
	}
	return b.String()
}

var validTypes = map[string]bool{
	"counter": true, "gauge": true, "summary": true, "histogram": true, "untyped": true,
}

func parseExposition(t *testing.T, body string) map[string]*expoFamily {
	t.Helper()
	switch {
	case body == "":
		t.Fatal("empty response")
	case !strings.HasSuffix(body, "\n"):
		t.Fatal("response does not end in a newline")
	case strings.HasSuffix(body, "\n\n"):
		t.Fatal("response ends in a blank line")
	}
	fams := map[string]*expoFamily{}
	series := map[string]bool{}
	var current *expoFamily
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		where := fmt.Sprintf("line %d (%q)", i+1, line)
		switch {
		case line == "":
			t.Fatalf("%s: blank line", where)
		case strings.HasPrefix(line, "# HELP "):
			name, help, ok := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !ok || help == "" {
				t.Fatalf("%s: HELP without text", where)
			}
			checkMetricName(t, where, name)
			if fams[name] != nil {
				t.Fatalf("%s: second HELP for family %s", where, name)
			}
			fams[name] = &expoFamily{name: name, help: help, order: len(fams)}
			current = fams[name]
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !ok {
				t.Fatalf("%s: TYPE without a type", where)
			}
			f := fams[name]
			switch {
			case f == nil:
				t.Fatalf("%s: TYPE before HELP for %s", where, name)
			case f.typ != "":
				t.Fatalf("%s: second TYPE for family %s", where, name)
			case !validTypes[typ]:
				t.Fatalf("%s: unknown type %q", where, typ)
			case len(f.samples) > 0:
				t.Fatalf("%s: TYPE after a sample of %s", where, name)
			}
			f.typ = typ
		case strings.HasPrefix(line, "#"):
			t.Fatalf("%s: unexpected comment", where)
		default:
			s := parseSample(t, where, line)
			f := familyOf(t, where, fams, s.name)
			if current == nil {
				t.Fatalf("%s: sample before any family header", where)
			}
			if f != current {
				t.Fatalf("%s: sample of %s inside family %s (families must not interleave)", where, f.name, current.name)
			}
			if f.typ == "" {
				t.Fatalf("%s: sample before TYPE for %s", where, f.name)
			}
			if k := s.key(); series[k] {
				t.Fatalf("%s: duplicate series %s", where, k)
			} else {
				series[k] = true
			}
			f.samples = append(f.samples, s)
		}
	}
	for _, f := range fams {
		if f.typ == "" {
			t.Fatalf("family %s has HELP but no TYPE", f.name)
		}
	}
	return fams
}

// familyOf resolves a sample name to its family, allowing the _sum and _count
// names a summary is made of.
func familyOf(t *testing.T, where string, fams map[string]*expoFamily, name string) *expoFamily {
	t.Helper()
	if f := fams[name]; f != nil {
		return f
	}
	for _, suffix := range []string{"_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			if f := fams[base]; f != nil && f.typ == "summary" {
				return f
			}
		}
	}
	t.Fatalf("%s: sample %s has no declared family", where, name)
	return nil
}

func parseSample(t *testing.T, where, line string) expoSample {
	t.Helper()
	s := expoSample{labels: map[string]string{}}
	rest := line
	if i := strings.IndexAny(line, "{ "); i < 0 {
		t.Fatalf("%s: no value", where)
	} else {
		s.name = line[:i]
		rest = line[i:]
	}
	checkMetricName(t, where, s.name)
	if strings.HasPrefix(rest, "{") {
		rest = parseLabels(t, where, rest, s.labels)
	}
	if !strings.HasPrefix(rest, " ") {
		t.Fatalf("%s: expected a space before the value, got %q", where, rest)
	}
	v, err := strconv.ParseFloat(strings.TrimPrefix(rest, " "), 64)
	if err != nil {
		t.Fatalf("%s: value: %v", where, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		t.Fatalf("%s: value is not finite", where)
	}
	s.value = v
	return s
}

// parseLabels consumes {a="1",b="2"} and returns what follows. It unescapes by
// hand so that an unescaped quote or newline inside a value is a failure, not
// a silently shorter value.
func parseLabels(t *testing.T, where, in string, out map[string]string) string {
	t.Helper()
	i := 1 // past '{'
	for {
		j := i
		for j < len(in) && in[j] != '=' {
			j++
		}
		if j >= len(in) {
			t.Fatalf("%s: label without '='", where)
		}
		name := in[i:j]
		checkLabelName(t, where, name)
		if _, dup := out[name]; dup {
			t.Fatalf("%s: duplicate label %s", where, name)
		}
		if j+1 >= len(in) || in[j+1] != '"' {
			t.Fatalf("%s: label %s value is not quoted", where, name)
		}
		var val strings.Builder
		k := j + 2
		for {
			if k >= len(in) {
				t.Fatalf("%s: unterminated label value", where)
			}
			c := in[k]
			if c == '"' {
				k++
				break
			}
			if c == '\n' {
				t.Fatalf("%s: raw newline in a label value", where)
			}
			if c == '\\' {
				if k+1 >= len(in) {
					t.Fatalf("%s: trailing backslash", where)
				}
				switch in[k+1] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					t.Fatalf("%s: unknown escape \\%c", where, in[k+1])
				}
				k += 2
				continue
			}
			val.WriteByte(c)
			k++
		}
		out[name] = val.String()
		switch {
		case k < len(in) && in[k] == ',':
			i = k + 1
		case k < len(in) && in[k] == '}':
			return in[k+1:]
		default:
			t.Fatalf("%s: expected ',' or '}' after a label", where)
		}
	}
}

func checkMetricName(t *testing.T, where, name string) {
	t.Helper()
	if name == "" {
		t.Fatalf("%s: empty metric name", where)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			t.Fatalf("%s: metric name %q has an illegal character %q", where, name, c)
		}
	}
	if !strings.HasPrefix(name, "swarm_") {
		t.Fatalf("%s: metric %q is not in the swarm_ namespace", where, name)
	}
	if strings.ToLower(name) != name {
		t.Fatalf("%s: metric %q is not snake_case", where, name)
	}
}

func checkLabelName(t *testing.T, where, name string) {
	t.Helper()
	if name == "" {
		t.Fatalf("%s: empty label name", where)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			t.Fatalf("%s: label name %q has an illegal character %q", where, name, c)
		}
	}
}

// --- helpers ---------------------------------------------------------------------

// fixedClock is a stopped clock: ages such as last_seen_ms are then the same
// on every run, so the tests can compare exact values and exact bytes without
// sleeping or polling for time to pass.
//
// It stops at the current instant rather than a literal date because the same
// clock is handed to every node connection (Config.Conn.Now), and a socket
// deadline computed from a date in the past has already expired -- the
// handshake would fail before HELLO_ACK.
func fixedClock() func() time.Time {
	at := time.Now()
	return func() time.Time { return at }
}

func scrapeRaw(t *testing.T, cc *testCC, query string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(cc.http.URL + "/metrics" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func scrape(t *testing.T, cc *testCC, query string) map[string]*expoFamily {
	t.Helper()
	resp, body := scrapeRaw(t, cc, query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != metricsContentType {
		t.Errorf("Content-Type = %q, want %q", ct, metricsContentType)
	}
	return parseExposition(t, body)
}

// sampleValue is the value of one series, found by its labels.
func sampleValue(t *testing.T, fams map[string]*expoFamily, name string, labels ...string) float64 {
	t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	f := fams[name]
	if f == nil {
		// A summary's _sum / _count live under the base family.
		for _, suffix := range []string{"_sum", "_count"} {
			if base, ok := strings.CutSuffix(name, suffix); ok {
				f = fams[base]
			}
		}
	}
	if f == nil {
		t.Fatalf("no family %s in the response", name)
	}
	for _, s := range f.samples {
		if s.name == name && reflect.DeepEqual(s.labels, want) {
			return s.value
		}
	}
	t.Fatalf("no sample %s%v in the response", name, want)
	return 0
}

// --- format tests ----------------------------------------------------------------

func TestMetricsEmptySwarmParses(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Now = fixedClock() })
	fams := scrape(t, cc, "")

	// A swarm with nothing in it still publishes every swarm-level family, so
	// that a missing series means a broken scrape and never "no nodes yet".
	for _, name := range []string{
		"swarm_build_info", "swarm_nodes", "swarm_nodes_known", "swarm_nodes_connected",
		"swarm_leaders", "swarm_leader_threshold", "swarm_leader_threshold_effective",
		"swarm_leader_target", "swarm_leader_hysteresis", "swarm_tasks",
		"swarm_task_duration_ms", "swarm_sim_enabled", "swarm_sim_base_ms",
		"swarm_sim_per_unit_ms", "swarm_sim_jitter_ms", "swarm_sim_max_delay_ms",
		"swarm_sim_size_units", "swarm_sim_config_version", "swarm_mavlink_enabled",
		"swarm_metrics_skipped_samples", "swarm_peer_rtt_ms",
	} {
		if fams[name] == nil {
			t.Errorf("family %s missing from an empty swarm", name)
		}
	}
	// Per-node families have a header and no samples; that is valid and is
	// what keeps a dashboard query from erroring on a fresh CC.
	if f := fams["swarm_node_connected"]; f == nil || len(f.samples) != 0 {
		t.Errorf("swarm_node_connected = %v, want a header with no samples", f)
	}
	for _, tc := range []struct {
		name  string
		want  float64
		state string
	}{
		{name: "swarm_nodes_known", want: 0},
		{name: "swarm_nodes_connected", want: 0},
		{name: "swarm_leaders", want: 0},
		{name: "swarm_leader_target", want: 0},
		{name: "swarm_leader_hysteresis", want: -1},
		{name: "swarm_mavlink_enabled", want: 0},
		{name: "swarm_sim_enabled", want: 1},
		{name: "swarm_sim_size_units", want: 100},
		{name: "swarm_metrics_skipped_samples", want: 0},
		{name: "swarm_task_duration_ms_count", want: 0},
		{name: "swarm_nodes", state: "alive", want: 0},
		{name: "swarm_nodes", state: "dead", want: 0},
		{name: "swarm_tasks", state: "pending", want: 0},
	} {
		var labels []string
		if tc.state != "" {
			labels = []string{"state", tc.state}
		}
		if got := sampleValue(t, fams, tc.name, labels...); got != tc.want {
			t.Errorf("%s%v = %v, want %v", tc.name, labels, got, tc.want)
		}
	}
	// No bridge is configured, so no bridge counter is published at all.
	for _, name := range []string{
		"swarm_mavlink_frames_sent_total", "swarm_mavlink_send_errors_total",
		"swarm_mavlink_malformed_total", "swarm_mavlink_dropped_total",
	} {
		if fams[name] != nil {
			t.Errorf("family %s published with no bridge configured", name)
		}
	}
}

func TestMetricsBuildInfoCarriesAVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  string
		want string
	}{
		{name: "configured", set: "v1.2.3", want: "v1.2.3"},
		{name: "trimmed", set: "  v9  ", want: "v9"},
		{name: "unset falls back", set: "", want: moduleVersion()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := startCC(t, func(c *Config) {
				c.Now = fixedClock()
				c.Version = tc.set
			})
			fams := scrape(t, cc, "")
			f := fams["swarm_build_info"]
			if len(f.samples) != 1 {
				t.Fatalf("swarm_build_info has %d samples, want 1", len(f.samples))
			}
			s := f.samples[0]
			if s.value != 1 || s.labels["version"] != tc.want {
				t.Errorf("swarm_build_info%v = %v, want version %q and 1", s.labels, s.value, tc.want)
			}
		})
	}
}

func TestMetricsNodeAndPeerSamples(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Now = fixedClock() })
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	w := dialNode(t, cc, "node-2")
	w.telemetry("worker", "suspect")
	b := dialWS(t, cc)
	b.snapshotWhere("both nodes", func(s Snapshot) bool {
		a, okA := findNode(s, "node-1")
		z, okZ := findNode(s, "node-2")
		return okA && okZ && a.Role == "leader" && z.State == "suspect"
	})

	fams := scrape(t, cc, "")
	for _, tc := range []struct {
		name   string
		labels []string
		want   float64
	}{
		{name: "swarm_nodes_known", want: 2},
		{name: "swarm_nodes_connected", want: 2},
		{name: "swarm_leaders", want: 1},
		{name: "swarm_nodes", labels: []string{"state", "alive"}, want: 1},
		{name: "swarm_nodes", labels: []string{"state", "suspect"}, want: 1},
		{name: "swarm_node_leader", labels: []string{"node", "node-1"}, want: 1},
		{name: "swarm_node_leader", labels: []string{"node", "node-2"}, want: 0},
		{name: "swarm_node_connected", labels: []string{"node", "node-2"}, want: 1},
		{name: "swarm_node_state", labels: []string{"node", "node-2", "state", "suspect"}, want: 1},
		{name: "swarm_node_state", labels: []string{"node", "node-2", "state", "alive"}, want: 0},
		{name: "swarm_node_term", labels: []string{"node", "node-1"}, want: 3},
		{name: "swarm_node_ledger_entries", labels: []string{"node", "node-1"}, want: 4},
		{name: "swarm_node_dropped_frames_total", labels: []string{"node", "node-1"}, want: 2},
		{name: "swarm_node_peers", labels: []string{"node", "node-1"}, want: 1},
		{name: "swarm_node_degraded", labels: []string{"node", "node-1"}, want: 0},
		// The fake node's telemetry reports one score for peer "x" at
		// advertise "x:7946", which the join resolves back to the id.
		{name: "swarm_peer_rtt_ms", labels: []string{"from", "node-1", "to", "x"}, want: 0.4},
		{name: "swarm_peer_rtt_ms", labels: []string{"from", "node-2", "to", "x"}, want: 0.4},
	} {
		if got := sampleValue(t, fams, tc.name, tc.labels...); got != tc.want {
			t.Errorf("%s%v = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
	// Positions: every node has all three axes, inside the cube.
	pos := fams["swarm_node_position_units"]
	if len(pos.samples) != 6 {
		t.Fatalf("position samples = %d, want 6 (2 nodes x 3 axes)", len(pos.samples))
	}
	for _, s := range pos.samples {
		if s.value < 0 || s.value > 100 {
			t.Errorf("position %v = %v, outside the cube", s.labels, s.value)
		}
	}
	// last_seen is a frozen clock's answer: the node spoke at "now".
	if got := sampleValue(t, fams, "swarm_node_last_seen_ms", "node", "node-1"); got != 0 {
		t.Errorf("last_seen_ms = %v, want 0 on a stopped clock", got)
	}
}

func TestMetricsPeerScoresCanBeTurnedOff(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Now = fixedClock() })
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b := dialWS(t, cc)
	b.snapshotWhere("scores", func(s Snapshot) bool {
		v, ok := findNode(s, "node-1")
		return ok && len(v.Scores) > 0
	})
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{query: "", want: true},
		{query: "?peer_scores=1", want: true},
		{query: "?peer_scores=yes", want: true},
		{query: "?peer_scores=0", want: false},
		{query: "?peer_scores=false", want: false},
		{query: "?peer_scores=OFF", want: false},
		{query: "?peer_scores=no", want: false},
	} {
		fams := scrape(t, cc, tc.query)
		f := fams["swarm_peer_rtt_ms"]
		if tc.want {
			if f == nil || len(f.samples) != 1 {
				t.Errorf("GET /metrics%s: swarm_peer_rtt_ms = %v, want 1 sample", tc.query, f)
			}
			continue
		}
		if f != nil {
			t.Errorf("GET /metrics%s: swarm_peer_rtt_ms still published", tc.query)
		}
		// The rest of the response is unaffected.
		if fams["swarm_node_connected"] == nil {
			t.Errorf("GET /metrics%s: dropped more than the scores family", tc.query)
		}
	}
}

// TestMetricsFormatUnits renders crafted hub copies. It is the only way to
// cover values a real swarm cannot be made to report on demand: a hostile node
// id, an unmeasured score, a NaN.
func TestMetricsFormatUnits(t *testing.T) {
	base := metricsData{
		sim:        SimView{Version: 7, Enabled: true, Size: 100, Hysteresis: -1},
		taskStates: map[string]int{},
		scoresOn:   true,
	}
	withNodes := func(nodes ...metricsNode) metricsData {
		d := base
		d.nodes = nodes
		return d
	}

	for _, tc := range []struct {
		name string
		data metricsData
		// want lines that must appear verbatim.
		want []string
		// absent lines that must not appear at all.
		absent []string
	}{
		{
			name: "label values are escaped",
			data: func() metricsData {
				d := withNodes(metricsNode{id: `a"b\c` + "\nd", state: "alive", connected: true})
				d.scores = []metricsScore{{from: `a"b\c` + "\nd", to: `x"y`, rtt: 12.5}}
				return d
			}(),
			want: []string{
				`swarm_node_connected{node="a\"b\\c\nd"} 1`,
				`swarm_peer_rtt_ms{from="a\"b\\c\nd",to="x\"y"} 12.5`,
			},
		},
		{
			name: "non finite samples are skipped not clamped",
			data: withNodes(
				metricsNode{id: "n1", state: "alive", connected: true, threshold: math.NaN(), hysteresis: math.Inf(1)},
				metricsNode{id: "n2", state: "alive", connected: true, threshold: 0.5},
			),
			want: []string{
				`swarm_node_threshold{node="n2"} 0.5`,
				`swarm_metrics_skipped_samples 2`,
			},
			absent: []string{`swarm_node_threshold{node="n1"}`, `swarm_node_hysteresis{node="n1"}`},
		},
		{
			name: "unknown states are published after the canonical ones",
			data: withNodes(metricsNode{id: "n1", state: "quarantined", connected: true}),
			want: []string{
				`swarm_nodes{state="killed"} 0`,
				`swarm_nodes{state="quarantined"} 1`,
			},
		},
		{
			name: "target leader count follows the threshold",
			data: func() metricsData {
				d := withNodes(
					metricsNode{id: "n1", state: "alive", connected: true, role: "leader", threshold: 0.3},
					metricsNode{id: "n2", state: "alive", connected: true, threshold: 0.3},
					metricsNode{id: "n3", state: "alive", connected: true, threshold: 0.3},
					metricsNode{id: "n4", state: "dead"},
				)
				return d
			}(),
			want: []string{
				"swarm_nodes_known 4",
				"swarm_nodes_connected 3",
				"swarm_leaders 1",
				"swarm_leader_threshold 0",
				"swarm_leader_threshold_effective 0.3",
				"swarm_leader_target 1", // ceil(3 * 0.3) = 1
			},
		},
		{
			name: "task window summary",
			data: func() metricsData {
				d := base
				d.taskStates = map[string]int{TaskPending: 2, TaskDone: 3, TaskFailed: 1}
				d.durSum = 12.5
				d.durCount = 3
				return d
			}(),
			want: []string{
				"# TYPE swarm_task_duration_ms summary",
				`swarm_tasks{state="done"} 3`,
				"swarm_task_duration_ms_sum 12.5",
				"swarm_task_duration_ms_count 3",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := string(formatMetrics(tc.data, nil, "v0"))
			parseExposition(t, body)
			lines := map[string]bool{}
			for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
				lines[l] = true
			}
			for _, w := range tc.want {
				if !lines[w] {
					t.Errorf("missing line %q in:\n%s", w, body)
				}
			}
			for _, a := range tc.absent {
				for l := range lines {
					if strings.HasPrefix(l, a) {
						t.Errorf("unwanted line %q", l)
					}
				}
			}
		})
	}
}

func TestMetricsBridgeStats(t *testing.T) {
	stats := &mavlink.BridgeStats{Sent: 10, SendErrors: 1, Malformed: 2, DroppedIn: 3}
	fams := parseExposition(t, string(formatMetrics(metricsData{
		sim: SimView{Size: 100, Hysteresis: -1}, taskStates: map[string]int{},
	}, stats, "v0")))
	for name, want := range map[string]float64{
		"swarm_mavlink_enabled":           1,
		"swarm_mavlink_frames_sent_total": 10,
		"swarm_mavlink_send_errors_total": 1,
		"swarm_mavlink_malformed_total":   2,
		"swarm_mavlink_dropped_total":     3,
	} {
		if got := sampleValue(t, fams, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
		if name != "swarm_mavlink_enabled" && fams[name].typ != "counter" {
			t.Errorf("%s is a %s, want a counter", name, fams[name].typ)
		}
	}
}

func TestFormatValueRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{in: 0, want: "0"},
		{in: 1, want: "1"},
		{in: -1, want: "-1"},
		{in: 0.3, want: "0.3"},
		{in: 12.25, want: "12.25"},
		{in: 1711000000000, want: "1711000000000"},
		{in: 1e300, want: "1e+300"},
		{in: 1.0 / 3.0, want: "0.3333333333333333"},
	} {
		got := formatValue(tc.in)
		if got != tc.want {
			t.Errorf("formatValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
		back, err := strconv.ParseFloat(got, 64)
		if err != nil || back != tc.in {
			t.Errorf("formatValue(%v) = %q does not round-trip (%v, %v)", tc.in, got, back, err)
		}
	}
}

func TestLeaderTarget(t *testing.T) {
	for _, tc := range []struct {
		connected int
		threshold float64
		want      float64
	}{
		{connected: 0, threshold: 0.3, want: 0},  // nothing to lead
		{connected: 5, threshold: 0, want: 0},    // threshold unknown
		{connected: 5, threshold: -1, want: 0},   // nonsense, not a target
		{connected: 1, threshold: 0.3, want: 1},  // max(1, ...) floor
		{connected: 10, threshold: 0.3, want: 3}, // ceil(3) = 3
		{connected: 11, threshold: 0.3, want: 4}, // ceil(3.3) = 4
		{connected: 4, threshold: 1, want: 4},    // everyone leads
		{connected: 4, threshold: math.NaN(), want: 0},
	} {
		if got := leaderTarget(tc.connected, tc.threshold); got != tc.want {
			t.Errorf("leaderTarget(%d, %v) = %v, want %v", tc.connected, tc.threshold, got, tc.want)
		}
	}
}

func TestNodeScoresJoinAndSanitise(t *testing.T) {
	for _, tc := range []struct {
		name string
		tel  protocol.TelemetryPayload
		want []metricsScore
	}{
		{
			name: "joined through the advertise address",
			tel: protocol.TelemetryPayload{
				Peers:  []protocol.MemberRecord{{ID: "node-2", Advertise: "node-2:7946"}},
				Scores: map[protocol.NodeAddress]float64{"node-2:7946": 4},
			},
			want: []metricsScore{{from: "node-1", to: "node-2", rtt: 4}},
		},
		{
			name: "unjoinable address keeps the address",
			tel: protocol.TelemetryPayload{
				Scores: map[protocol.NodeAddress]float64{"ghost:7946": 9},
			},
			want: []metricsScore{{from: "node-1", to: "ghost:7946", rtt: 9}},
		},
		{
			name: "unmeasured and non finite scores are dropped",
			tel: protocol.TelemetryPayload{
				Scores: map[protocol.NodeAddress]float64{
					"a:1": protocol.UnmeasuredScore,
					"b:1": math.NaN(),
					"c:1": math.Inf(1),
				},
			},
		},
		{
			name: "no scores at all",
			tel:  protocol.TelemetryPayload{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeScores(&nodeEntry{id: "node-1", tel: tc.tel})
			sort.Slice(got, func(i, j int) bool { return got[i].to < got[j].to })
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("nodeScores = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --- behaviour tests -------------------------------------------------------------

// TestMetricsAuthMatchesState pins the decision that /metrics is exactly as
// open as /api/state: reads need no token, mutations do. If someone later
// gates /api/state, this test fails and /metrics must follow.
func TestMetricsAuthMatchesState(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "no token configured"},
		{name: "token configured", token: token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := startCC(t, func(c *Config) {
				c.Now = fixedClock()
				c.APIToken = tc.token
			})
			var codes []int
			for _, path := range []string{"/metrics", "/api/state"} {
				resp, err := http.Get(cc.http.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				codes = append(codes, resp.StatusCode)
			}
			if codes[0] != codes[1] {
				t.Errorf("GET /metrics = %d but GET /api/state = %d; they must agree", codes[0], codes[1])
			}
			if codes[0] != http.StatusOK {
				t.Errorf("GET /metrics = %d, want 200", codes[0])
			}
			// The token still guards what it was always for.
			if tc.token != "" {
				if code, _ := post(t, cc, "/api/tasks", `{"kind":"echo"}`); code != http.StatusUnauthorized {
					t.Errorf("POST /api/tasks without the token = %d, want 401", code)
				}
			}
		})
	}
	// A correct bearer token is accepted on /metrics too, so a scrape job can
	// send the same header nginx injects.
	cc := startCC(t, func(c *Config) {
		c.Now = fixedClock()
		c.APIToken = token
	})
	req, _ := http.NewRequest(http.MethodGet, cc.http.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics with a bearer token = %d, want 200", resp.StatusCode)
	}
}

func TestMetricsRejectsOtherMethods(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Now = fixedClock() })
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, err := http.NewRequest(method, cc.http.URL+"/metrics", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /metrics = %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodGet) {
			t.Errorf("%s /metrics Allow = %q, want it to name GET", method, allow)
		}
	}
	// HEAD is served by net/http from the GET handler, headers only.
	resp, err := http.Head(cc.http.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /metrics = %d, want 200", resp.StatusCode)
	}
}

// TestMetricsScrapeDoesNotMutateHub is the whole reason collectMetrics copies:
// a scrape is a read. With a stopped clock the snapshot is a pure function of
// hub state, so two identical snapshots around a scrape prove nothing moved --
// including the sim version, which a stray bumpSim would change.
func TestMetricsScrapeDoesNotMutateHub(t *testing.T) {
	cc := startCC(t, func(c *Config) { c.Now = fixedClock() })
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b := dialWS(t, cc)
	b.snapshotWhere("node-1 leading", func(s Snapshot) bool {
		v, ok := findNode(s, "node-1")
		return ok && v.Role == "leader"
	})
	if code, _ := post(t, cc, "/api/tasks", `{"kind":"echo"}`); code != http.StatusOK {
		t.Fatal("submit failed")
	}

	before := getState(t, cc)
	for i := 0; i < 3; i++ {
		scrape(t, cc, "")
		scrape(t, cc, "?peer_scores=0")
	}
	after := getState(t, cc)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("hub state changed across scrapes:\nbefore %+v\nafter  %+v", before, after)
	}

	// And two scrapes of unchanged state are byte-identical, which is what
	// makes the rest of these tests deterministic.
	_, first := scrapeRaw(t, cc, "")
	_, second := scrapeRaw(t, cc, "")
	if first != second {
		t.Error("two scrapes of unchanged state differ")
	}
}

// TestMetricsScrapeDoesNotStallTheHub keeps the "format outside the hub"
// property honest: the hub must keep ticking snapshots while a scrape formats.
func TestMetricsScrapeDoesNotStallTheHub(t *testing.T) {
	cc := startCC(t, nil) // real clock: this one is about liveness
	n := dialNode(t, cc, "node-1")
	n.telemetry("leader", "alive")
	b := dialWS(t, cc)
	b.snapshotWhere("first", func(s Snapshot) bool { _, ok := findNode(s, "node-1"); return ok })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			scrape(t, cc, "")
		}
	}()
	// Snapshots keep arriving during the scrapes; the browser read has its
	// own timeout, so a stalled hub fails the test rather than hanging it.
	for i := 0; i < 3; i++ {
		b.snapshotWhere("snapshot during scrapes", func(Snapshot) bool { return true })
	}
	<-done
}
