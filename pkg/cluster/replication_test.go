package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"swarm-net/pkg/protocol"
)

func (h *harness) syncsTo(to protocol.NodeID) []protocol.StateSyncPayload {
	h.t.Helper()
	var out []protocol.StateSyncPayload
	for _, s := range h.tr.sentOf(protocol.TypeStateSync) {
		if s.to != to {
			continue
		}
		p, err := protocol.PayloadOf[protocol.StateSyncPayload](s.env)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (h *harness) lastSyncTo(to protocol.NodeID) protocol.StateSyncPayload {
	h.t.Helper()
	s := h.syncsTo(to)
	if len(s) == 0 {
		h.t.Fatalf("no STATE_SYNC to %s", to)
	}
	return s[len(s)-1]
}

// onLoop runs fn on the node's loop and flushes replication, like an event.
func (h *harness) onLoop(fn func(n *Node)) {
	h.t.Helper()
	if err := h.node.barrier(h.ctx, func() { fn(h.node) }); err != nil {
		h.t.Fatal(err)
	}
}

func rec(id, state string) protocol.TaskRecord {
	return protocol.TaskRecord{TaskID: id, State: state, Kind: "echo"}
}

func taskIDs(ledger []protocol.TaskRecord) []string {
	out := make([]string, 0, len(ledger))
	for _, r := range ledger {
		out = append(out, r.TaskID)
	}
	return out
}

// ---- leader side ----

// A worker that attaches gets a snapshot at once, and so does every other
// worker, because the roster is part of it.
func TestLeaderSyncsOnAttach(t *testing.T) {
	h := leaderWith(t, "node-b")
	st := h.status()
	p := h.lastSyncTo("node-b")
	if p.Term != st.Term || p.Leader != "node-a" || p.Version != 1 || len(p.Ledger) != 0 {
		t.Fatalf("first snapshot = %+v (term %d)", p, st.Term)
	}
	requireIDs(t, "Workers", p.Workers, ids("node-b"))

	h.peerUp("node-c", 1)
	h.frame("node-c", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-c"})
	for _, w := range ids("node-b", "node-c") {
		p := h.lastSyncTo(w)
		if p.Version != 2 {
			t.Fatalf("%s holds version %d, want 2", w, p.Version)
		}
		requireIDs(t, string(w)+" Workers", p.Workers, ids("node-b", "node-c"))
	}
	if got := h.status().SyncVersion; got != 2 {
		t.Fatalf("SyncVersion = %d, want 2", got)
	}
}

// Several ledger edits made while handling one event leave as one snapshot.
func TestLedgerChangesAreCoalescedPerEvent(t *testing.T) {
	h := leaderWith(t, "node-b")
	before := len(h.syncsTo("node-b"))
	h.onLoop(func(n *Node) {
		n.ledgerAppend(rec("t-1", taskPending))
		n.ledgerAppend(rec("t-2", taskPending))
	})
	syncs := h.syncsTo("node-b")
	if len(syncs) != before+1 {
		t.Fatalf("snapshots = %d, want one more than %d", len(syncs), before)
	}
	p := syncs[len(syncs)-1]
	if p.Version != 3 || strings.Join(taskIDs(p.Ledger), ",") != "t-1,t-2" {
		t.Fatalf("snapshot = %+v", p)
	}
	// Nothing changed: nothing sent.
	h.onLoop(func(*Node) {})
	if n := len(h.syncsTo("node-b")); n != before+1 {
		t.Fatalf("an unchanged ledger was re-sent: %d", n)
	}
}

// Every StateSyncInterval the current snapshot is re-sent unchanged.
func TestPeriodicSyncResendsTheSameVersion(t *testing.T) {
	h := leaderWith(t, "node-b")
	before := h.syncsTo("node-b")
	h.step(DefaultStateSyncInterval)
	after := h.syncsTo("node-b")
	if len(after) != len(before)+1 {
		t.Fatalf("snapshots = %d after one interval, want %d", len(after), len(before)+1)
	}
	if after[len(after)-1].Version != before[len(before)-1].Version {
		t.Fatal("the periodic resend changed the version")
	}
}

// A term change alone is worth a snapshot: workers order snapshots by term.
func TestTermChangeIsSynced(t *testing.T) {
	h := leaderWith(t, "node-b")
	before := len(h.syncsTo("node-b"))
	h.frame("node-b", protocol.TypeHeartbeatAck, protocol.HeartbeatAckPayload{Term: 77, ObservedLeader: "node-a"})
	if p := h.lastSyncTo("node-b"); len(h.syncsTo("node-b")) != before+1 || p.Term != 77 {
		t.Fatalf("snapshot after term change = %+v", p)
	}
}

func TestNoSyncWithoutWorkers(t *testing.T) {
	h := leaderWith(t)
	h.step(hbEvery) // past the boot-time promotion's re-issue tick
	h.onLoop(func(n *Node) { n.ledgerAppend(rec("t-1", taskPending)) })
	h.step(DefaultStateSyncInterval)
	if got := h.tr.sentOf(protocol.TypeStateSync); len(got) != 0 {
		t.Fatalf("snapshots sent to nobody: %d", len(got))
	}
	if st := h.status(); len(st.Ledger) != 1 || st.SyncVersion != 1 {
		t.Fatalf("ControlStatus = %+v", st.ControlStatus)
	}
}

// The ledger is bounded: completed records go first, oldest first; a ledger
// that is all pending loses its oldest.
func TestLedgerEvictsCompletedFirst(t *testing.T) {
	h := newHarness(t, "node-a", func(c *NodeConfig) { phase4(c); c.LedgerSize = 3 })
	h.onLoop(func(n *Node) {
		n.ledgerAppend(rec("p1", taskPending))
		n.ledgerAppend(rec("d1", taskDone))
		n.ledgerAppend(rec("p2", taskPending))
		n.ledgerAppend(rec("p3", taskPending))
	})
	if got := strings.Join(taskIDs(h.status().Ledger), ","); got != "p1,p2,p3" {
		t.Fatalf("ledger = %s, want the done record evicted", got)
	}
	h.onLoop(func(n *Node) { n.ledgerAppend(rec("p4", taskPending)) })
	if got := strings.Join(taskIDs(h.status().Ledger), ","); got != "p2,p3,p4" {
		t.Fatalf("ledger = %s, want the oldest pending evicted", got)
	}
}

// A body too large to replicate is dropped from the record (the task still
// runs); a small one is copied, so the caller's buffer is not retained.
func TestLedgerBodyIsCappedAndCopied(t *testing.T) {
	h := leaderWith(t)
	small := json.RawMessage(`{"ms":1}`)
	big := json.RawMessage(`"` + strings.Repeat("x", maxRecordBody) + `"`)
	h.onLoop(func(n *Node) {
		r := rec("small", taskPending)
		r.Body = small
		n.ledgerAppend(r)
		r = rec("big", taskPending)
		r.Body = big
		n.ledgerAppend(r)
	})
	small[1] = 'X'
	l := h.status().Ledger
	if string(l[0].Body) != `{"ms":1}` {
		t.Fatalf("ledger kept the caller's buffer: %s", l[0].Body)
	}
	if l[1].Body != nil {
		t.Fatalf("oversized body replicated: %d bytes", len(l[1].Body))
	}
}

// A snapshot that would not fit one frame is trimmed, completed records first,
// and never at the cost of a pending one while completed ones remain.
func TestOversizedSnapshotIsTrimmedToFit(t *testing.T) {
	h := leaderWith(t, "node-b")
	h.onLoop(func(n *Node) {
		for i := 0; i < DefaultLedgerSize-5; i++ {
			r := rec(fmt.Sprintf("d-%03d", i), taskDone)
			r.Result = strings.Repeat("\x01", maxRecordResult) // six bytes each once escaped
			n.ledgerAppend(r)
		}
		for i := 0; i < 5; i++ {
			n.ledgerAppend(rec(fmt.Sprintf("p-%d", i), taskPending))
		}
	})
	sends := h.tr.sentOf(protocol.TypeStateSync)
	last := sends[len(sends)-1].env
	if len(last.Payload) > syncBudget {
		t.Fatalf("snapshot is %d bytes, budget %d", len(last.Payload), syncBudget)
	}
	p := h.lastSyncTo("node-b")
	pending := 0
	for _, r := range p.Ledger {
		if r.State == taskPending {
			pending++
		}
	}
	if pending != 5 || len(p.Ledger) >= DefaultLedgerSize {
		t.Fatalf("trimmed snapshot has %d records, %d pending", len(p.Ledger), pending)
	}
	if n := len(h.status().Ledger); n != DefaultLedgerSize {
		t.Fatalf("trimming touched the ledger itself: %d records", n)
	}
}

// ---- worker side ----

func (h *harness) syncFrom(from protocol.NodeID, p protocol.StateSyncPayload) {
	h.t.Helper()
	if p.Leader == "" {
		p.Leader = from
	}
	h.frame(from, protocol.TypeStateSync, p)
}

// A worker replaces its copy with a newer snapshot from its own leader, and
// ignores anything older, equal, or from anyone else.
func TestWorkerReplacesOnlyWithNewerSnapshotFromItsLeader(t *testing.T) {
	h := attachedWorker(t)
	term := h.status().Term
	ledgerIs := func(want string) {
		t.Helper()
		st := h.status()
		if got := strings.Join(taskIDs(st.Ledger), ","); got != want {
			t.Fatalf("ledger = %q (version %d), want %q", got, st.SyncVersion, want)
		}
	}

	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term, Version: 5, Ledger: []protocol.TaskRecord{rec("a", taskPending)}})
	ledgerIs("a")
	if v := h.status().SyncVersion; v != 5 {
		t.Fatalf("SyncVersion = %d", v)
	}
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term, Version: 4, Ledger: []protocol.TaskRecord{rec("old", taskPending)}})
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term, Version: 5, Ledger: []protocol.TaskRecord{rec("same", taskPending)}})
	ledgerIs("a")
	h.syncFrom("node-c", protocol.StateSyncPayload{Term: term + 9, Version: 99, Ledger: []protocol.TaskRecord{rec("stranger", taskPending)}})
	ledgerIs("a")
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term + 9, Leader: "node-c", Version: 99, Ledger: []protocol.TaskRecord{rec("relayed", taskPending)}})
	ledgerIs("a")

	// A newer term wins even with a lower version, and is adopted.
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term + 1, Version: 1, Ledger: []protocol.TaskRecord{rec("b", taskDone)}})
	ledgerIs("b")
	if got := h.status().Term; got != term+1 {
		t.Fatalf("Term = %d, want %d", got, term+1)
	}
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term, Version: 50, Ledger: []protocol.TaskRecord{rec("older-term", taskDone)}})
	ledgerIs("b")

	// A new attachment is a new session: a lower version is accepted.
	h.frame("node-b", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-b"})
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: term, Version: 1, Ledger: []protocol.TaskRecord{rec("restarted", taskPending)}})
	ledgerIs("restarted")
}

// A leader ignores snapshots: it is the author of its own.
func TestLeaderIgnoresSnapshots(t *testing.T) {
	h := leaderWith(t, "node-b")
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: 99, Version: 99, Ledger: []protocol.TaskRecord{rec("x", taskPending)}})
	if st := h.status(); len(st.Ledger) != 0 || st.Term == 99 {
		t.Fatalf("leader took a snapshot: %+v term %d", st.ControlStatus, st.Term)
	}
}

// A snapshot larger than our own bound keeps its newest records.
func TestWorkerTrimsOversizedSnapshot(t *testing.T) {
	h := attachedWorkerWith(t, func(c *NodeConfig) { c.LedgerSize = 2 })
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 1, Ledger: []protocol.TaskRecord{
		rec("1", taskDone), rec("2", taskDone), rec("3", taskPending),
	}})
	if got := strings.Join(taskIDs(h.status().Ledger), ","); got != "2,3" {
		t.Fatalf("ledger = %s", got)
	}
}

// Status hands out a deep copy: a reader editing its ledger, bodies included,
// reaches neither the node nor other readers.
func TestStatusLedgerIsADeepCopy(t *testing.T) {
	h := attachedWorker(t)
	r := rec("a", taskPending)
	r.Body = json.RawMessage(`{"k":1}`)
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 1, Ledger: []protocol.TaskRecord{r}})
	a := h.status()
	a.Ledger[0].TaskID = "mutated"
	a.Ledger[0].Body[2] = 'X'
	b := h.status()
	if b.Ledger[0].TaskID != "a" || string(b.Ledger[0].Body) != `{"k":1}` {
		t.Fatalf("a reader's edit reached the node: %+v %s", b.Ledger[0], b.Ledger[0].Body)
	}
}

// A promoted worker's ledger is the snapshot it held; the version numbering
// continues, and it syncs that ledger to whoever attaches.
func TestPromotedWorkerServesItsSnapshot(t *testing.T) {
	h := attachedWorker(t)
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 7, Ledger: []protocol.TaskRecord{rec("a", taskDone)}})
	// Both leaders leave; node-z is the best remaining candidate.
	h.frame("node-b", protocol.TypeLeave, nil)
	h.frame("node-c", protocol.TypeLeave, nil)
	st := h.status()
	if st.Role != RoleLeader {
		t.Fatalf("not promoted: %s", st)
	}
	if got := strings.Join(taskIDs(st.Ledger), ","); got != "a" || st.SyncVersion != 7 {
		t.Fatalf("ControlStatus after promotion = %+v", st.ControlStatus)
	}
	h.frame("node-d", protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: "node-d"})
	p := h.lastSyncTo("node-d")
	if p.Version != 8 || strings.Join(taskIDs(p.Ledger), ",") != "a" || p.Leader != "node-z" {
		t.Fatalf("snapshot from the promoted node = %+v", p)
	}
}

func TestTruncateUTF8(t *testing.T) {
	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"aéb", 2, "a"}, // e-acute is two bytes; never split it
		{"é", 1, ""},
	} {
		if got := truncateUTF8(tt.in, tt.n); got != tt.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}
