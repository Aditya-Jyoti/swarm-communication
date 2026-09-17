package cluster

import (
	"context"
	"encoding/json"
	"slices"
	"unicode/utf8"

	"swarm-net/pkg/protocol"
)

// STATE_SYNC replication: a leader's task ledger, copied to its workers.
//
// # Snapshot, not log
//
// The leader sends its whole ledger every time. A worker replaces its copy and
// never merges (see protocol.StateSyncPayload for why). The ledger is bounded
// (LedgerSize), so a snapshot is bounded too, and there is no log to truncate,
// no gap to detect and no retransmit logic: a lost snapshot is superseded by the
// next change, or by the periodic resend (StateSyncInterval) at the latest.
//
// # Ordering
//
// A worker takes a snapshot only from the leader it is attached to, and only if
// (Term, Version) is newer than the one it holds. The key is reset whenever the
// worker attaches to a leader (JOIN_ACK), because a different leader numbers
// its versions independently, and a restarted leader starts again from where
// its snapshot left off, not from ours. Within one attachment, frames arrive in
// order on one connection, so the check only has to reject replays and a
// snapshot that raced a reconnect.
//
// # Failover
//
// A worker's copy IS its ledger: on promotion there is nothing to copy, and the
// version numbering simply continues. The re-issue of pending tasks (at-least-
// once delivery) is tasks.go's job.

// replica is the loop-owned replication state.
type replica struct {
	// ledger is the authoritative ledger on a leader and the last accepted
	// snapshot on a worker. Oldest first.
	ledger []protocol.TaskRecord
	// version is the leader's snapshot counter, or the version of the snapshot
	// a worker holds.
	version uint64
	// dirty means the leader has a change its workers have not been sent.
	dirty bool
	// sentWorkers and sentTerm are what the last snapshot said, so a change in
	// the attached set or the term is noticed without instrumenting every
	// place that makes one.
	sentWorkers []protocol.NodeID
	sentTerm    uint64
	// held and term are a worker's ordering key for the current attachment.
	held bool
	term uint64
	// pub is the ledger as last published in Status. It is rebuilt only when
	// pubStale is set, so a publish (which happens on every evaluate) does not
	// copy 500 records every time. It is a clone, so ledger edits never reach
	// it; the Body slices it shares with ledger are safe because the node only
	// ever replaces a Body, never writes into one. Status.clone deep-copies
	// them for readers.
	pub      []protocol.TaskRecord
	pubStale bool
}

// published returns the ControlStatus for a fresh Status.
func (r *replica) published() ControlStatus {
	if r.pubStale {
		r.pub = slices.Clone(r.ledger)
		r.pubStale = false
	}
	return ControlStatus{Ledger: r.pub, SyncVersion: r.version}
}

// changed records a leader-side ledger edit.
func (r *replica) changed() {
	r.version++
	r.dirty = true
	r.pubStale = true
}

// cloneLedger deep-copies a ledger, Body bytes included.
func cloneLedger(in []protocol.TaskRecord) []protocol.TaskRecord {
	if in == nil {
		return nil
	}
	out := make([]protocol.TaskRecord, len(in))
	for i, r := range in {
		if r.Body != nil {
			r.Body = append(json.RawMessage(nil), r.Body...)
		}
		out[i] = r
	}
	return out
}

// Task record states, as carried in protocol.TaskRecord.State.
const (
	taskPending = "pending"
	taskDone    = "done"
	taskFailed  = "failed"
)

// Size caps for what a record carries, so a full ledger stays well inside one
// frame. The task itself is always executed with its full body; only the
// replicated copy is capped.
const (
	// maxRecordBody is the largest body kept for re-issue. A task with a larger
	// body still runs, but a promoted leader cannot re-issue it.
	maxRecordBody = 1 << 10
	// maxRecordResult is the longest result kept in the ledger. The full output
	// still reaches OnTaskResult.
	maxRecordResult = 512
	// syncBudget is the largest STATE_SYNC payload a leader sends: the frame
	// limit minus room for the envelope around it.
	syncBudget = protocol.MaxFrameSize - 64<<10
)

// truncateUTF8 shortens s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ledgerAppend adds rec, evicting to stay within LedgerSize.
//
// Completed records go first, oldest first: they are history, and the Control
// Center already has their results. Only when every record is pending does the
// oldest pending one go, with a warning, because that task can no longer be
// re-issued after a failover.
func (n *Node) ledgerAppend(rec protocol.TaskRecord) {
	for len(n.repl.ledger) >= n.cfg.LedgerSize {
		victim := slices.IndexFunc(n.repl.ledger, func(r protocol.TaskRecord) bool { return r.State != taskPending })
		if victim < 0 {
			victim = 0
			n.log.Warn("ledger full of pending tasks; evicting the oldest", "task", n.repl.ledger[0].TaskID, "size", n.cfg.LedgerSize)
		}
		n.repl.ledger = slices.Delete(n.repl.ledger, victim, victim+1)
	}
	if len(rec.Body) > maxRecordBody {
		n.log.Warn("task body too large to replicate; it will not survive a failover", "task", rec.TaskID, "bytes", len(rec.Body))
		rec.Body = nil
	}
	// A private copy: the caller's body belongs to a decoded frame or to
	// SubmitTask's caller, and the record outlives both.
	if rec.Body != nil {
		rec.Body = append(json.RawMessage(nil), rec.Body...)
	}
	n.repl.ledger = append(n.repl.ledger, rec)
	n.repl.changed()
}

// flushSync sends a snapshot to every attached worker if anything they hold is
// out of date. Called once after every loop event.
func (n *Node) flushSync(ctx context.Context) {
	if !n.isLeader() {
		return
	}
	workers := n.attachedIDs()
	if !equalIDs(workers, n.repl.sentWorkers) {
		// The roster is part of the snapshot, so a roster change is a new
		// version even if the ledger did not move.
		n.repl.changed()
	}
	if n.term != n.repl.sentTerm {
		n.repl.dirty = true
	}
	if !n.repl.dirty {
		return
	}
	n.repl.dirty = false
	n.repl.sentWorkers = workers
	n.repl.sentTerm = n.term
	n.sendSync(ctx, workers)
	n.publish()
}

// periodicSync re-sends the current snapshot. A worker that already holds it
// ignores the copy (same version), and one that missed it catches up.
func (n *Node) periodicSync(ctx context.Context) {
	if n.isLeader() {
		n.sendSync(ctx, n.attachedIDs())
	}
}

// sendSync encodes the snapshot once and sends it to each worker.
func (n *Node) sendSync(ctx context.Context, workers []protocol.NodeID) {
	if len(workers) == 0 {
		return
	}
	payload, err := n.syncPayload(workers)
	if err != nil {
		n.log.Error("STATE_SYNC encode failed", "err", err)
		return
	}
	for _, w := range workers {
		env := &protocol.Envelope{
			Version:        protocol.CurrentVersion,
			Type:           protocol.TypeStateSync,
			From:           n.cfg.Self,
			To:             w,
			ID:             protocol.NewMessageID(),
			SentAtUnixNano: n.cfg.Clock.Now().UnixNano(),
			Payload:        payload,
		}
		if err := n.cfg.Transport.Send(ctx, w, env); err != nil {
			// The periodic resend is the retry.
			n.log.Log(ctx, sendFailureLevel(err), "STATE_SYNC send failed", "worker", w, "err", err)
		}
	}
}

// syncPayload encodes the snapshot, trimming completed records from the copy
// it sends until it fits syncBudget. The ledger itself is not touched: this is
// a transport limit, not a retention policy.
func (n *Node) syncPayload(workers []protocol.NodeID) (json.RawMessage, error) {
	p := protocol.StateSyncPayload{
		Term:    n.term,
		Leader:  n.cfg.Self,
		Workers: workers,
		Ledger:  n.repl.ledger,
		Version: n.repl.version,
	}
	for {
		b, err := json.Marshal(p)
		if err != nil || len(b) <= syncBudget {
			return b, err
		}
		// Drop the older half of the completed records, and if none are left,
		// the older half of everything. Halving keeps this to a handful of
		// encodes even for a pathological ledger.
		done := 0
		for _, r := range p.Ledger {
			if r.State != taskPending {
				done++
			}
		}
		drop := max(done/2, 1)
		kept := make([]protocol.TaskRecord, 0, len(p.Ledger))
		for _, r := range p.Ledger {
			if drop > 0 && (r.State != taskPending || done == 0) {
				drop--
				continue
			}
			kept = append(kept, r)
		}
		n.log.Warn("STATE_SYNC over budget; trimming the sent copy", "bytes", len(b), "records", len(p.Ledger), "kept", len(kept))
		p.Ledger = kept
	}
}

// handleStateSync is the worker side: replace the held copy if the snapshot is
// from our leader and newer.
func (n *Node) handleStateSync(from protocol.NodeID, p protocol.StateSyncPayload) {
	if n.isLeader() || from != n.leader || (p.Leader != "" && p.Leader != from) {
		n.log.Debug("STATE_SYNC not from our leader; ignored", "from", from, "leader", n.leader)
		return
	}
	if p.Term > n.term {
		n.term = p.Term
	}
	if n.repl.held && !newerSnapshot(p.Term, p.Version, n.repl.term, n.repl.version) {
		n.publish()
		return
	}
	ledger := p.Ledger
	if extra := len(ledger) - n.cfg.LedgerSize; extra > 0 {
		// A leader configured with a larger ledger than ours. Keep the newest.
		ledger = ledger[extra:]
	}
	n.repl.ledger = ledger
	n.repl.version = p.Version
	n.repl.term = p.Term
	n.repl.held = true
	n.repl.pubStale = true
	n.publish()
}

func newerSnapshot(term, version, heldTerm, heldVersion uint64) bool {
	if term != heldTerm {
		return term > heldTerm
	}
	return version > heldVersion
}

// roleChanged runs after an evaluate that promoted or demoted this node.
func (n *Node) roleChanged(leading bool) {
	// Either way the replication session is over. A new leader re-sends to
	// whoever attaches; a new worker takes whatever its leader sends first.
	n.repl.sentWorkers = nil
	n.repl.held = false
	if leading {
		n.log.Info("promoted; adopting replicated ledger", "records", len(n.repl.ledger), "version", n.repl.version)
		n.repl.dirty = true
	}
	n.publish()
}
