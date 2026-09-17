package cluster

import (
	"context"
	"errors"
	"time"

	"swarm-net/pkg/protocol"
)

// This file is the Phase 4/5 contract between pkg/cluster and its callers
// (pkg/telemetry, cmd/swarm-node). The signatures are fixed; the bodies are
// filled in by the Phase 4 implementation.

// ErrNotImplemented is returned by contract stubs that have no body yet.
var ErrNotImplemented = errors.New("cluster: not implemented")

// ErrNoLeader is returned by SubmitTask when this node is detached: it is not a
// leader and has no leader to forward to.
var ErrNoLeader = errors.New("cluster: no leader to accept task")

// TaskExecutor runs one task on a worker. It must honour ctx and must not
// touch node state; it is called on its own goroutine.
type TaskExecutor func(ctx context.Context, t protocol.TaskPayload) protocol.TaskResultPayload

// SubmitTask hands a task injected by the Control Center to this node.
//
// On a leader: the task is recorded in the ledger as pending and assigned to an
// attached worker (or executed locally when none is attached). On a worker: the
// task is forwarded to its leader. Detached: ErrNoLeader. Safe to call from any
// goroutine; the work happens on the loop.
func (n *Node) SubmitTask(ctx context.Context, t protocol.TaskPayload) error {
	return ErrNotImplemented
}

// SetChaosDelay records an injected reply delay (CHAOS "delay"; 0 clears it).
// The node delays its HEARTBEAT_ACK replies by d and reports the delay in
// Status.ChaosDelay. cmd is responsible for also calling MeshProber.SetDelay so
// PONGs are delayed too. Safe to call from any goroutine.
//
// d is clamped to [0, MaxChaosDelay]. The contract range is enforced by cmd;
// the clamp is the node's own guard, because the delay also bounds how long a
// delayed-reply goroutine lives, and a negative delay has no meaning.
func (n *Node) SetChaosDelay(d time.Duration) {
	d = min(max(d, 0), MaxChaosDelay)
	if prev := time.Duration(n.chaosDelay.Swap(int64(d))); prev != d {
		n.log.Info("chaos delay set", "delay", d)
	}
}

// MaxChaosDelay is the largest reply delay SetChaosDelay accepts: the 5000ms
// ceiling of the CHAOS "delay" contract.
const MaxChaosDelay = 5 * time.Second

// ChaosDelay returns the injected reply delay currently in force.
func (n *Node) ChaosDelay() time.Duration { return time.Duration(n.chaosDelay.Load()) }

// ControlStatus is the Phase 4 addition to Status: the replicated cluster state
// this node holds (its own if leading, the last STATE_SYNC if a worker).
type ControlStatus struct {
	// Ledger is the task ledger, newest last.
	Ledger []protocol.TaskRecord
	// SyncVersion is the (leader-local) version of the ledger held.
	SyncVersion uint64
	// ChaosDelay is the currently injected reply delay; 0 when none.
	ChaosDelay time.Duration
}
