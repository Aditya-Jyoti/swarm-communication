package controlcenter

import (
	"fmt"
	"math"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	"swarm-net/pkg/mavlink"
	"swarm-net/pkg/protocol"
)

// GET /metrics: the hub's state in the Prometheus text exposition format
// (docs/architecture/control-center.md).
//
// # Why hand-rolled
//
// The same reason the wire protocol is: the format is a dozen lines of text
// rules, and a client library would pull a dependency tree into a binary whose
// only other dependency is the WebSocket. Nothing here is sampled or
// aggregated over time -- every number already exists in the hub, so there are
// no collectors to register and no registry to own.
//
// # Why a copy, not a borrowed snapshot
//
// A scrape must not run ON the hub goroutine for longer than it takes to copy:
// the hub also drives the 1Hz snapshot tick for every browser, and a scraper
// that formats 2000 lines inside an op would stall it. So collectMetrics (hub
// goroutine, posted through do like every other reader) copies flat values and
// builds its own slices, and formatMetrics runs on the HTTP goroutine from
// that copy alone. It deliberately does not reuse Snapshot: NodeView aliases
// the node's live Peers slice and Scores map, and formatting those off-hub
// would be a read of hub-owned memory, correct only by accident of how
// applyTelemetry happens to replace them.

// metricsContentType is the version the format rules below follow. A scraper
// uses it to pick a parser, so it is not decoration.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// peerScoresParam switches the N^2 per-pair RTT family off for one scrape:
// GET /metrics?peer_scores=0. A query parameter rather than an env var so an
// operator can change it in prometheus.yml without restarting the CC, and so
// a human with curl can ask for the cheap view. See the cardinality note in
// the docs.
const peerScoresParam = "peer_scores"

// Canonical node states, always emitted (with 0) so that a state's series does
// not appear and disappear as nodes change. NodeStateKilled is the CC's own
// overlay, not a node report; see types.go.
var metricsStates = []string{"alive", "suspect", "dead", NodeStateKilled}

// Canonical task states, emitted the same way.
var metricsTaskStates = []string{TaskPending, TaskDone, TaskFailed}

// metricsData is one scrape's copy of hub state. Every field is owned by the
// scrape goroutine once collectMetrics returns.
type metricsData struct {
	sim   SimView
	nodes []metricsNode
	// taskStates counts the RETAINED task window (at most MaxTasks), not
	// every task ever submitted.
	taskStates map[string]int
	// durSum and durCount summarise the settled tasks in that window that
	// reported a duration.
	durSum   float64
	durCount uint64
	// scores is one entry per (observer, peer) pair, nil when the scrape
	// asked for them to be left out.
	scores []metricsScore
	// scoresOn records that the family was requested, so an empty swarm
	// still publishes its HELP/TYPE header rather than looking disabled.
	scoresOn bool
}

// metricsNode is one node's row, copied out of nodeEntry.
type metricsNode struct {
	id         protocol.NodeID
	role       string
	state      string
	term       uint64
	connected  bool
	degraded   bool
	lastSeenMS int64
	dropped    uint64
	ledger     int
	peers      int
	threshold  float64
	hysteresis float64
	simVersion uint64
	pos        protocol.Position
}

// metricsScore is one observer's measured cost towards one peer. Under the
// default LatencyHealthStrategy the unit is milliseconds of round trip, which
// is why the metric says _ms; a different strategy would make the name a lie,
// and the docs say so.
type metricsScore struct {
	from protocol.NodeID
	to   protocol.NodeID
	rtt  float64
}

// handleMetrics answers one scrape.
//
// Auth matches GET /api/state exactly: neither route checks the API token.
// Reads are open by design (see the threat model in auth.go) and the token
// guards mutations only, so /metrics is not a way around it -- it exposes the
// same topology /api/state already does, in another format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	wantScores := peerScoresRequested(r)
	var d metricsData
	if !s.do(r.Context(), func(h *hub) { d = h.collectMetrics(wantScores) }) {
		// Same shape as every other unavailable answer: a 503 is what a
		// scraper acts on, and the JSON body is for the human with curl.
		writeError(w, ErrUnavailable)
		return
	}
	// Outside the hub op: Bridge.Stats reads four independent atomics and is
	// safe from any goroutine. nil when no bridge is configured.
	var stats *mavlink.BridgeStats
	if s.mav != nil {
		st := s.mav.Stats()
		stats = &st
	}
	body := formatMetrics(d, stats, s.buildVersion())
	w.Header().Set("Content-Type", metricsContentType)
	_, _ = w.Write(body)
}

// peerScoresRequested reports whether this scrape wants the per-pair family.
// Absent means yes: the default is the complete picture, and an operator who
// has measured the cardinality turns it off deliberately.
func peerScoresRequested(r *http.Request) bool {
	v, ok := r.URL.Query()[peerScoresParam]
	if !ok || len(v) == 0 {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v[0])) {
	case "0", "f", "false", "no", "off":
		return false
	}
	return true
}

// collectMetrics copies what one scrape needs. Hub goroutine; no allocation
// here is shared with hub state afterwards.
func (h *hub) collectMetrics(wantScores bool) metricsData {
	now := h.now()
	d := metricsData{
		sim:        h.simView(),
		nodes:      make([]metricsNode, 0, len(h.nodes)),
		taskStates: make(map[string]int, len(metricsTaskStates)),
		scoresOn:   wantScores,
	}
	for id, n := range h.nodes {
		state := n.tel.State
		if _, killed := h.killed[id]; killed && n.link == nil {
			state = NodeStateKilled
		}
		d.nodes = append(d.nodes, metricsNode{
			id:         id,
			role:       n.tel.Role,
			state:      state,
			term:       n.tel.Term,
			connected:  n.link != nil,
			degraded:   n.tel.Degraded,
			lastSeenMS: now.Sub(n.lastSeen).Milliseconds(),
			dropped:    n.tel.Dropped,
			ledger:     n.tel.LedgerSize,
			peers:      len(n.tel.Peers),
			threshold:  n.tel.Threshold,
			hysteresis: n.tel.Hysteresis,
			simVersion: n.tel.SimVersion,
			pos:        h.positionOf(id),
		})
		if wantScores {
			d.scores = append(d.scores, nodeScores(n)...)
		}
	}
	// Sorted so a scrape is byte-stable for the same state, which makes the
	// output diffable by hand and the tests independent of map order.
	sort.Slice(d.nodes, func(i, j int) bool { return d.nodes[i].id < d.nodes[j].id })
	sort.Slice(d.scores, func(i, j int) bool {
		if d.scores[i].from != d.scores[j].from {
			return d.scores[i].from < d.scores[j].from
		}
		return d.scores[i].to < d.scores[j].to
	})
	for _, t := range h.tasks {
		d.taskStates[t.view.State]++
		// A task that failed on the way out (the leader's queue was full)
		// never reported a duration, and a 0 would drag the mean down for a
		// handler that never ran.
		if t.view.State != TaskPending && t.view.DurationMS > 0 && finite(t.view.DurationMS) {
			d.durSum += t.view.DurationMS
			d.durCount++
		}
	}
	return d
}

// nodeScores turns one node's telemetry into (from, to, rtt) triples.
//
// Scores are keyed by advertise address; the peer's node id is recovered from
// the same report's Peers list, which is the join the dashboard does. An
// address with no matching peer record keeps the address as its "to" label
// rather than being dropped: an unjoinable score is still a measurement, and
// losing it silently would be worse than a label that reads like a host:port.
func nodeScores(n *nodeEntry) []metricsScore {
	if len(n.tel.Scores) == 0 {
		return nil
	}
	byAddr := make(map[protocol.NodeAddress]protocol.NodeID, len(n.tel.Peers))
	for _, p := range n.tel.Peers {
		if p.Advertise != "" && p.ID != "" {
			byAddr[p.Advertise] = p.ID
		}
	}
	out := make([]metricsScore, 0, len(n.tel.Scores))
	for addr, v := range n.tel.Scores {
		// Unmeasured (UnmeasuredScore, NaN, Inf) is not a latency of -1 or a
		// huge one: it is the absence of a sample, so no series is published.
		if v < 0 || !finite(v) {
			continue
		}
		to, ok := byAddr[addr]
		if !ok {
			to = protocol.NodeID(addr)
		}
		out = append(out, metricsScore{from: n.id, to: to, rtt: v})
	}
	return out
}

// formatMetrics renders one exposition. Pure: same input, same bytes.
func formatMetrics(d metricsData, mav *mavlink.BridgeStats, version string) []byte {
	e := &expo{}

	e.family("swarm_build_info", "gauge", "Control Center build, as a constant 1 labelled with the version string.")
	e.sample("swarm_build_info", 1, "version", version)

	// --- swarm shape ---------------------------------------------------------
	var connected, leaders int
	byState := make(map[string]int, len(metricsStates))
	for _, n := range d.nodes {
		byState[n.state]++
		if n.connected {
			connected++
			if n.role == roleLeader {
				leaders++
			}
		}
	}
	e.family("swarm_nodes", "gauge", "Nodes the Control Center lists, by last known state.")
	for _, st := range metricsStates {
		e.sample("swarm_nodes", float64(byState[st]), "state", st)
	}
	// A state a future node build reports that this build does not know
	// about is published too, after the canonical ones, so it is visible
	// rather than silently missing from the total.
	for _, st := range sortedExtra(byState, metricsStates) {
		e.sample("swarm_nodes", float64(byState[st]), "state", st)
	}
	e.family("swarm_nodes_known", "gauge", "Nodes in the Control Center's table, connected or awaiting expiry.")
	e.sample("swarm_nodes_known", float64(len(d.nodes)))
	e.family("swarm_nodes_connected", "gauge", "Nodes with a live TCP link to the Control Center.")
	e.sample("swarm_nodes_connected", float64(connected))
	e.family("swarm_leaders", "gauge", "Connected nodes currently reporting role leader.")
	e.sample("swarm_leaders", float64(leaders))

	// The threshold in force: the operator override when there is one,
	// otherwise what the connected nodes say they are using. The CC does not
	// hold the swarm's compiled-in default, so with no override and no
	// telemetry yet the answer is honestly 0 ("unknown"), not a guess.
	effective := d.sim.Threshold
	if effective <= 0 {
		for _, n := range d.nodes {
			if n.connected && n.threshold > effective && finite(n.threshold) {
				effective = n.threshold
			}
		}
	}
	e.family("swarm_leader_threshold", "gauge", "Operator override for the leader fraction, 0 when there is none.")
	e.sample("swarm_leader_threshold", d.sim.Threshold)
	e.family("swarm_leader_threshold_effective", "gauge", "Leader fraction actually in force: the override, else the highest value connected nodes report, else 0 when unknown.")
	e.sample("swarm_leader_threshold_effective", effective)
	e.family("swarm_leader_target", "gauge", "Leaders the swarm should have: max(1, ceil(connected * effective threshold)), or 0 while either input is unknown.")
	e.sample("swarm_leader_target", leaderTarget(connected, effective))
	e.family("swarm_leader_hysteresis", "gauge", "Operator override for the election margin in score units, -1 when there is none.")
	e.sample("swarm_leader_hysteresis", d.sim.Hysteresis)

	// --- per node ------------------------------------------------------------
	//
	// Role is a 0/1 gauge (swarm_node_leader), not an info metric with a role
	// label: a label would make every promotion start a new series and leave
	// the old one stale, so "is node-2 a leader" could not be answered by one
	// instant vector. State needs more than two values, so it is the same idea
	// widened: one series per canonical state, exactly one of them 1.
	e.family("swarm_node_connected", "gauge", "1 when the node has a live link to the Control Center.")
	for _, n := range d.nodes {
		e.sample("swarm_node_connected", b2f(n.connected), "node", string(n.id))
	}
	e.family("swarm_node_leader", "gauge", "1 when the node reports role leader and is connected.")
	for _, n := range d.nodes {
		e.sample("swarm_node_leader", b2f(n.connected && n.role == roleLeader), "node", string(n.id))
	}
	e.family("swarm_node_state", "gauge", "1 for the node's current state, 0 for the others.")
	for _, n := range d.nodes {
		for _, st := range metricsStates {
			e.sample("swarm_node_state", b2f(n.state == st), "node", string(n.id), "state", st)
		}
	}
	e.family("swarm_node_term", "gauge", "Election term the node last reported.")
	for _, n := range d.nodes {
		e.sample("swarm_node_term", float64(n.term), "node", string(n.id))
	}
	e.family("swarm_node_last_seen_ms", "gauge", "Milliseconds since the Control Center last heard from the node.")
	for _, n := range d.nodes {
		e.sample("swarm_node_last_seen_ms", float64(n.lastSeenMS), "node", string(n.id))
	}
	// A counter kept by the node process, so it resets to 0 when that node
	// restarts. Prometheus handles a counter reset; a rate() over it is the
	// shedding rate, which is the question worth asking.
	e.family("swarm_node_dropped_frames_total", "counter", "Data-plane frames the node has shed under backpressure since it started.")
	for _, n := range d.nodes {
		e.sample("swarm_node_dropped_frames_total", float64(n.dropped), "node", string(n.id))
	}
	e.family("swarm_node_ledger_entries", "gauge", "Entries in the node's replicated ledger.")
	for _, n := range d.nodes {
		e.sample("swarm_node_ledger_entries", float64(n.ledger), "node", string(n.id))
	}
	e.family("swarm_node_peers", "gauge", "Members in the node's own membership view.")
	for _, n := range d.nodes {
		e.sample("swarm_node_peers", float64(n.peers), "node", string(n.id))
	}
	e.family("swarm_node_degraded", "gauge", "1 while a chaos delay is in effect on the node.")
	for _, n := range d.nodes {
		e.sample("swarm_node_degraded", b2f(n.degraded), "node", string(n.id))
	}
	e.family("swarm_node_threshold", "gauge", "Leader fraction the node reports it is using.")
	for _, n := range d.nodes {
		e.sample("swarm_node_threshold", n.threshold, "node", string(n.id))
	}
	e.family("swarm_node_hysteresis", "gauge", "Election margin the node reports it is using, in score units.")
	for _, n := range d.nodes {
		e.sample("swarm_node_hysteresis", n.hysteresis, "node", string(n.id))
	}
	// Compared with swarm_sim_config_version this is the drift check: a node
	// whose version lags has not applied the operator's last change.
	e.family("swarm_node_sim_version", "gauge", "Version of the last SIM_CONFIG the node applied, 0 for none.")
	for _, n := range d.nodes {
		e.sample("swarm_node_sim_version", float64(n.simVersion), "node", string(n.id))
	}
	// One family with an axis label, not three families: the three numbers
	// are the same quantity in the same unit, and a label is how a scraper
	// asks for all of them at once.
	e.family("swarm_node_position_units", "gauge", "Node position in the emulated cube, in cube units (0..swarm_sim_size_units).")
	for _, n := range d.nodes {
		e.sample("swarm_node_position_units", n.pos.X, "node", string(n.id), "axis", "x")
		e.sample("swarm_node_position_units", n.pos.Y, "node", string(n.id), "axis", "y")
		e.sample("swarm_node_position_units", n.pos.Z, "node", string(n.id), "axis", "z")
	}

	// --- health scores -------------------------------------------------------
	if d.scoresOn {
		e.family("swarm_peer_rtt_ms", "gauge", "Health score node from measured for peer to, in milliseconds under the latency strategy. Cardinality is O(N^2); pass ?peer_scores=0 to omit this family.")
		for _, sc := range d.scores {
			e.sample("swarm_peer_rtt_ms", sc.rtt, "from", string(sc.from), "to", string(sc.to))
		}
	}

	// --- tasks ---------------------------------------------------------------
	e.family("swarm_tasks", "gauge", "Tasks in the Control Center's retained window (at most 200), by state.")
	for _, st := range metricsTaskStates {
		e.sample("swarm_tasks", float64(d.taskStates[st]), "state", st)
	}
	for _, st := range sortedExtra(d.taskStates, metricsTaskStates) {
		e.sample("swarm_tasks", float64(d.taskStates[st]), "state", st)
	}
	// A summary with no quantiles: _sum and _count are the only aggregates the
	// data supports. There is no histogram because the CC keeps no buckets and
	// inventing boundaries would publish numbers nothing measured. Both values
	// are over the same retained window, so they FALL when old tasks are
	// evicted; read them as a windowed mean (sum/count), not with rate().
	e.family("swarm_task_duration_ms", "summary", "Worker-measured duration of the settled tasks in the retained window. Sum and count fall as the window evicts; use sum/count, not rate().")
	e.sample("swarm_task_duration_ms_sum", d.durSum)
	e.sample("swarm_task_duration_ms_count", float64(d.durCount))

	// --- latency simulation --------------------------------------------------
	e.family("swarm_sim_enabled", "gauge", "1 when the emulated latency is switched on.")
	e.sample("swarm_sim_enabled", b2f(d.sim.Enabled))
	e.family("swarm_sim_base_ms", "gauge", "Fixed part of one emulated hop, in milliseconds.")
	e.sample("swarm_sim_base_ms", d.sim.BaseMS)
	e.family("swarm_sim_per_unit_ms", "gauge", "Emulated milliseconds added per cube unit of distance.")
	e.sample("swarm_sim_per_unit_ms", d.sim.PerUnitMS)
	e.family("swarm_sim_jitter_ms", "gauge", "Upper bound of the random jitter added to one emulated hop, in milliseconds.")
	e.sample("swarm_sim_jitter_ms", d.sim.JitterMS)
	e.family("swarm_sim_max_delay_ms", "gauge", "Cap on one emulated delay, in milliseconds.")
	e.sample("swarm_sim_max_delay_ms", d.sim.MaxDelayMS)
	e.family("swarm_sim_size_units", "gauge", "Side of the emulated cube, in cube units.")
	e.sample("swarm_sim_size_units", d.sim.Size)
	e.family("swarm_sim_config_version", "gauge", "Version of the SIM_CONFIG the Control Center currently publishes.")
	e.sample("swarm_sim_config_version", float64(d.sim.Version))

	// --- MAVLink bridge ------------------------------------------------------
	// Always published, so "the bridge is off" is a 0 rather than a missing
	// series an alert cannot distinguish from a dead scrape.
	e.family("swarm_mavlink_enabled", "gauge", "1 when the MAVLink bridge is configured.")
	e.sample("swarm_mavlink_enabled", b2f(mav != nil))
	if mav != nil {
		e.family("swarm_mavlink_frames_sent_total", "counter", "MAVLink frames written, counted once per destination.")
		e.sample("swarm_mavlink_frames_sent_total", float64(mav.Sent))
		e.family("swarm_mavlink_send_errors_total", "counter", "Failed MAVLink writes.")
		e.sample("swarm_mavlink_send_errors_total", float64(mav.SendErrors))
		e.family("swarm_mavlink_malformed_total", "counter", "Datagrams dropped as not MAVLink, corrupt, or carrying an unimplemented message.")
		e.sample("swarm_mavlink_malformed_total", float64(mav.Malformed))
		e.family("swarm_mavlink_dropped_total", "counter", "Datagrams dropped because they arrived faster than the bridge loop.")
		e.sample("swarm_mavlink_dropped_total", float64(mav.DroppedIn))
	}

	// Non-finite values are skipped, never clamped: a 0 or a MaxFloat64 would
	// be read as a measurement. This counter is how an operator finds out it
	// happened at all.
	e.family("swarm_metrics_skipped_samples", "gauge", "Samples omitted from this response because their value was NaN or Inf.")
	e.sample("swarm_metrics_skipped_samples", float64(e.skipped))

	return e.bytes()
}

// leaderTarget is the dynamic threshold from the spec:
// LeaderCount = max(1, ceil(N * threshold)). 0 means "cannot say yet": with no
// connected node or no known threshold there is no target, and claiming 1
// would alert on a swarm that is simply still starting.
func leaderTarget(connected int, threshold float64) float64 {
	if connected <= 0 || threshold <= 0 || !finite(threshold) {
		return 0
	}
	return max(1, math.Ceil(float64(connected)*threshold))
}

// roleLeader is the telemetry role string for a leader. Node telemetry carries
// it as a free-form string (see protocol.MemberRecord), so the comparison
// lives in one place.
const roleLeader = "leader"

// sortedExtra returns the keys of m that are not in known, sorted. It keeps an
// unexpected state out of the canonical block (which must stay in a fixed
// order) while still publishing it.
func sortedExtra[V any](m map[string]V, known []string) []string {
	var out []string
	for k := range m {
		if !contains(known, k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// expo builds one exposition response.
//
// Not safe for concurrent use and not meant to be: one lives on the stack of
// one scrape.
type expo struct {
	b strings.Builder
	// skipped counts samples dropped for a non-finite value.
	skipped int
}

// family writes the two header lines for one metric family. It is called once
// per family, immediately before that family's samples, and families are never
// interleaved: a second TYPE line for a name makes a scraper reject the whole
// response, and it does so quietly on its side, not ours.
func (e *expo) family(name, typ, help string) {
	// help is a compile-time constant at every call site, so it cannot carry
	// the newline that would end the comment early.
	fmt.Fprintf(&e.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes one line. labels are alternating key, value; a trailing odd
// key is ignored rather than written half-formed.
func (e *expo) sample(name string, v float64, labels ...string) {
	if !finite(v) {
		e.skipped++
		return
	}
	e.b.WriteString(name)
	for i := 0; i+1 < len(labels); i += 2 {
		if i == 0 {
			e.b.WriteByte('{')
		} else {
			e.b.WriteByte(',')
		}
		e.b.WriteString(labels[i])
		e.b.WriteString(`="`)
		writeEscaped(&e.b, labels[i+1])
		e.b.WriteByte('"')
	}
	if len(labels) >= 2 {
		e.b.WriteByte('}')
	}
	e.b.WriteByte(' ')
	e.b.WriteString(formatValue(v))
	e.b.WriteByte('\n')
}

// bytes returns the response. Every sample line already ends in \n, so the
// body ends with exactly one and carries no trailing anything else.
func (e *expo) bytes() []byte { return []byte(e.b.String()) }

// writeEscaped writes a label value with the three escapes the format defines:
// backslash, double quote, newline. Node ids are tame today -- but ids come
// from the node's own HELLO and from operator-supplied position keys, so the
// escaping is done where it cannot be forgotten rather than argued about.
func writeEscaped(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
}

// formatValue renders a sample value.
//
// Both forms are legal and both round-trip exactly; the choice is for the
// human reading a curl. Counters and versions are whole numbers, and
// 'f' keeps a SIM_CONFIG version readable as 1711000000000 instead of
// 1.711e+12. Above 2^53 a float64 cannot be trusted to be whole anyway, so
// anything that large falls back to the shortest round-tripping form.
func formatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func b2f(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// buildVersion is the string for swarm_build_info.
//
// Config.Version (set by the binary from its -ldflags value) wins; otherwise
// the module version from the build info, which is "(devel)" for a local
// build; otherwise "dev". Never empty: a label a scraper cannot join on is
// worse than an honest placeholder.
func (s *Server) buildVersion() string {
	if v := strings.TrimSpace(s.cfg.Version); v != "" {
		return v
	}
	return moduleVersion()
}

// moduleVersion is read once: debug.ReadBuildInfo walks the build metadata,
// and a scrape must not pay for that every time.
var moduleVersion = sync.OnceValue(func() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
})
