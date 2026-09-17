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
)

// Event kinds.
const (
	EventNodeUp       = "node_up"
	EventNodeDown     = "node_down"
	EventLeaderChange = "leader_change"
	EventStateChange  = "state_change"
	EventTaskDone     = "task_done"
	EventChaos        = "chaos"
)

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
	Nodes    []NodeView `json:"nodes"`
	Tasks    []TaskView `json:"tasks"`
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

// ClientMessage is a browser (or POST body) request: a task or a chaos action.
// One struct for both, discriminated by Type, because that is how it arrives.
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
