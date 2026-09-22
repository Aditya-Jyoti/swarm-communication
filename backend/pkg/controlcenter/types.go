// Package controlcenter is the Control Center (CC): the hub every node dials
// for telemetry, the single ingress for tasks and chaos, and the server behind
// the browser dashboard.
//
// The wire and JSON shapes are fixed by docs/architecture/control-plane.md.
// The CC is not a mesh member: it accepts node connections with its own HELLO
// handling (nodeserver.go) precisely so that it never advertises one node's
// address to another, and nothing it does feeds node membership.
package controlcenter

import (
	"encoding/json"

	"swarm-net/pkg/protocol"
)

// NodeID is the identity the CC announces in HELLO_ACK.
const NodeID protocol.NodeID = "control-center"

// Message type discriminators on the browser WebSocket.
const (
	TypeSnapshot = "snapshot"
	TypeEvent    = "event"
	TypeTask     = "task"
	TypeChaos    = "chaos"
	TypeSim      = "sim"
)

// Event kinds.
const (
	EventNodeUp       = "node_up"
	EventNodeDown     = "node_down"
	EventLeaderChange = "leader_change"
	EventStateChange  = "state_change"
	EventTaskDone     = "task_done"
	EventChaos        = "chaos"
	EventSim          = "sim"
)

// NodeStateKilled is the NodeView.State of a node the CC sent CHAOS kill to
// and whose link has since dropped. It overrides the node's last reported
// state, which was necessarily "alive" the last time it spoke.
const NodeStateKilled = "killed"

// Task states.
const (
	TaskPending = "pending"
	TaskDone    = "done"
	TaskFailed  = "failed"
)

// Snapshot is the periodic full picture sent to every browser.
type Snapshot struct {
	Type     string     `json:"type"`
	AtUnixMS int64      `json:"at_unix_ms"`
	Sim      SimView    `json:"sim"`
	Nodes    []NodeView `json:"nodes"`
	Tasks    []TaskView `json:"tasks"`
}

// SimView is the CC's latency-simulation settings as the browser sees them: in
// the snapshot and from GET/POST /api/sim. Positions are not here; each node's
// position is in its NodeView.
type SimView struct {
	Version   uint64  `json:"version"`
	Enabled   bool    `json:"enabled"`
	BaseMS    float64 `json:"base_ms"`
	PerUnitMS float64 `json:"per_unit_ms"`
	JitterMS  float64 `json:"jitter_ms"`
	// Threshold is the operator override, 0 when there is none.
	Threshold float64 `json:"threshold"`
	// Hysteresis is the operator override, -1 when there is none.
	Hysteresis float64 `json:"hysteresis"`
	// Size is the side of the space cube (geo.Size), so the dashboard does
	// not hard-code it.
	Size float64 `json:"size"`
	// MaxDelayMS is the cap on one emulated delay (geo.MaxDelay).
	MaxDelayMS float64 `json:"max_delay_ms"`
}

// NodeView is one node in a Snapshot: its latest telemetry plus link state.
type NodeView struct {
	ID        protocol.NodeID `json:"id"`
	Role      string          `json:"role"`
	State     string          `json:"state"`
	Term      uint64          `json:"term"`
	Leader    protocol.NodeID `json:"leader"`
	Degraded  bool            `json:"degraded"`
	Connected bool            `json:"connected"`
	// LastSeenMS is how long ago, in milliseconds, the CC last heard from the
	// node. An age rather than a timestamp, so the browser needs no clock sync
	// with the CC to render "3s ago".
	LastSeenMS int64                            `json:"last_seen_ms"`
	Dropped    uint64                           `json:"dropped"`
	LedgerSize int                              `json:"ledger_size"`
	Peers      []protocol.MemberRecord          `json:"peers"`
	Scores     map[protocol.NodeAddress]float64 `json:"scores"`
	// Pos is where the CC has placed the node in the space.
	Pos protocol.Position `json:"pos"`
	// Threshold, Hysteresis and SimVersion are the node's own report: the
	// election settings in force there and the SIM_CONFIG version it applied.
	// All zero until the node's first telemetry sample.
	Threshold  float64 `json:"threshold"`
	Hysteresis float64 `json:"hysteresis"`
	SimVersion uint64  `json:"sim_version"`
	// Flows is the node's latest flows sample, or empty once that sample is
	// older than flowTTL. Never null.
	Flows []protocol.FlowRecord `json:"flows"`
}

// TaskView is one task in a Snapshot.
type TaskView struct {
	TaskID          string          `json:"task_id"`
	Kind            string          `json:"kind"`
	Leader          protocol.NodeID `json:"leader"`
	Worker          protocol.NodeID `json:"worker"`
	State           string          `json:"state"`
	OK              bool            `json:"ok"`
	Output          string          `json:"output"`
	DurationMS      float64         `json:"duration_ms"`
	SubmittedUnixMS int64           `json:"submitted_unix_ms"`
}

// Event is pushed to browsers as it happens.
type Event struct {
	Type     string          `json:"type"`
	AtUnixMS int64           `json:"at_unix_ms"`
	Kind     string          `json:"kind"`
	Node     protocol.NodeID `json:"node"`
	Detail   string          `json:"detail"`
}

// ClientMessage is a browser (or POST body) request: a task, a chaos action or
// a sim update. One struct for all three, discriminated by Type, because that
// is how it arrives, and because decodeStrict rejects fields it does not know.
type ClientMessage struct {
	Type string `json:"type"`
	// task
	Kind  string          `json:"kind,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
	Count int             `json:"count,omitempty"`
	// chaos
	Node    protocol.NodeID `json:"node,omitempty"`
	Action  string          `json:"action,omitempty"`
	DelayMS int             `json:"delay_ms,omitempty"`
	// sim: a partial update. Pointers, because "absent" (leave it alone) and
	// "zero" (set it to zero) are different requests: base_ms 0 is legal.
	Enabled        *bool                                 `json:"enabled,omitempty"`
	BaseMS         *float64                              `json:"base_ms,omitempty"`
	PerUnitMS      *float64                              `json:"per_unit_ms,omitempty"`
	JitterMS       *float64                              `json:"jitter_ms,omitempty"`
	Threshold      *float64                              `json:"threshold,omitempty"`
	Hysteresis     *float64                              `json:"hysteresis,omitempty"`
	Positions      map[protocol.NodeID]protocol.Position `json:"positions,omitempty"`
	Randomize      bool                                  `json:"randomize,omitempty"`
	ResetPositions bool                                  `json:"reset_positions,omitempty"`
}

// hasSimFields reports whether m carries a sim-only field. On a task or chaos
// request that is a client bug, worth a 400 rather than a silent ignore.
func (m ClientMessage) hasSimFields() bool {
	return m.Enabled != nil || m.BaseMS != nil || m.PerUnitMS != nil || m.JitterMS != nil ||
		m.Threshold != nil || m.Hysteresis != nil || m.Positions != nil || m.Randomize || m.ResetPositions
}

// hasCommandFields reports whether m carries a task or chaos field.
func (m ClientMessage) hasCommandFields() bool {
	return m.Kind != "" || m.Body != nil || m.Count != 0 || m.Node != "" || m.Action != "" || m.DelayMS != 0
}

// Limits from the contract.
const (
	MaxTasks     = 200
	MaxTaskCount = 100
)

// Local limits on what the CC stores and re-broadcasts. Everything in the task
// store is sent to every browser in every snapshot, once a second, so its size
// is multiplied by MaxTasks and by the number of dashboards.
const (
	// maxKindLen caps a task kind. Built-in kinds are a few bytes long.
	maxKindLen = 64
	// maxTaskOutput caps TaskView.Output, including truncatedMark. It
	// matches the node ledger's own cap on a stored result. Uncapped, 100
	// echo tasks with 64KiB bodies made a 39MB snapshot.
	maxTaskOutput = 512
	// truncatedMark ends an output that was cut.
	truncatedMark = "...[truncated]"
)
