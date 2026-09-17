package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/health"
	"swarm-net/pkg/network"
	"swarm-net/pkg/protocol"
)

// results collects OnTaskResult calls.
type results struct {
	mu  sync.Mutex
	got []protocol.TaskResultPayload
}

func (r *results) record(res protocol.TaskResultPayload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, res)
}

func (r *results) all() []protocol.TaskResultPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.TaskResultPayload(nil), r.got...)
}

func withResults(r *results) func(*NodeConfig) {
	return func(c *NodeConfig) {
		phase4(c)
		c.OnTaskResult = r.record
	}
}

func echo(id string) protocol.TaskPayload {
	return protocol.TaskPayload{TaskID: id, Kind: "echo", Body: json.RawMessage(`"` + id + `"`)}
}

func (h *harness) submit(t protocol.TaskPayload) error {
	h.t.Helper()
	err := h.node.SubmitTask(h.ctx, t)
	h.settle()
	return err
}

func (h *harness) record(id string) protocol.TaskRecord {
	h.t.Helper()
	for _, r := range h.status().Ledger {
		if r.TaskID == id {
			return r
		}
	}
	h.t.Fatalf("no ledger record for %s in %+v", id, h.status().Ledger)
	return protocol.TaskRecord{}
}

func (h *harness) tasksTo(to protocol.NodeID) []string {
	h.t.Helper()
	var out []string
	for _, s := range h.tr.sentOf(protocol.TypeTask) {
		if s.to != to {
			continue
		}
		p, err := protocol.PayloadOf[protocol.TaskPayload](s.env)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, p.TaskID)
	}
	return out
}

func (h *harness) resultsTo(to protocol.NodeID) []protocol.TaskResultPayload {
	h.t.Helper()
	var out []protocol.TaskResultPayload
	for _, s := range h.tr.sentOf(protocol.TypeTaskResult) {
		if s.to != to {
			continue
		}
		p, err := protocol.PayloadOf[protocol.TaskResultPayload](s.env)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// ---- SubmitTask ----

// A leader with nobody attached runs the task itself, records it, and reports
// the result once.
func TestLoneLeaderRunsSubmittedTask(t *testing.T) {
	var r results
	h := newHarness(t, "node-a", withResults(&r))
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	rec := h.record("t-1")
	if rec.State != taskDone || rec.AssignedTo != "node-a" || rec.Result != `"t-1"` || rec.Body != nil || rec.Kind != "echo" {
		t.Fatalf("record = %+v", rec)
	}
	got := r.all()
	if len(got) != 1 {
		t.Fatalf("OnTaskResult calls = %d", len(got))
	}
	if g := got[0]; g.TaskID != "t-1" || g.Worker != "node-a" || !g.OK || g.Output != `"t-1"` || g.DurationMS != 0 {
		t.Fatalf("result = %+v", g)
	}
	// A duplicate submission is a no-op.
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	if n := len(r.all()); n != 1 || len(h.status().Ledger) != 1 {
		t.Fatalf("duplicate ran again: %d results, %d records", n, len(h.status().Ledger))
	}
}

func TestSubmitTaskErrors(t *testing.T) {
	t.Run("no id", func(t *testing.T) {
		h := newHarness(t, "node-a", phase4)
		if err := h.node.SubmitTask(h.ctx, protocol.TaskPayload{Kind: "echo"}); !errors.Is(err, ErrNoTaskID) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("detached", func(t *testing.T) {
		h := attachedWorker(t)
		h.peerDown("node-b", network.DispositionPeerDied)
		h.peerDown("node-c", network.DispositionPeerDied)
		if st := h.status(); st.Leader != "" {
			t.Fatalf("setup: %s", st)
		}
		if err := h.node.SubmitTask(h.ctx, echo("t")); !errors.Is(err, ErrNoLeader) {
			t.Fatalf("err = %v, want ErrNoLeader", err)
		}
	})
	t.Run("forward fails", func(t *testing.T) {
		h := attachedWorker(t)
		h.tr.failSend("node-b", network.ErrSendQueueFull)
		err := h.node.SubmitTask(h.ctx, echo("t"))
		if !errors.Is(err, network.ErrSendQueueFull) || !strings.Contains(err.Error(), "node-z") || !strings.Contains(err.Error(), "node-b") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		h := newHarness(t, "node-a", phase4)
		h.stop()
		if err := h.node.SubmitTask(context.Background(), echo("t")); !errors.Is(err, ErrStopped) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not running, ctx ends", func(t *testing.T) {
		n, err := NewNode(baseConfig("node-a", newFakeTransport("node-a"), newFakeHealth(), NewFakeClock(epoch)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := n.SubmitTask(ctx, echo("t")); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

// The node copies the body: the caller may reuse its buffer at once.
func TestSubmitTaskCopiesTheBody(t *testing.T) {
	h := leaderWith(t, "node-b")
	body := json.RawMessage(`{"ms":5}`)
	if err := h.node.SubmitTask(h.ctx, protocol.TaskPayload{TaskID: "t", Kind: "sleep", Body: body}); err != nil {
		t.Fatal(err)
	}
	body[2] = 'X'
	h.settle()
	if got := string(h.record("t").Body); got != `{"ms":5}` {
		t.Fatalf("record body = %s", got)
	}
}

// A worker forwards a submission to its leader and records nothing itself.
func TestWorkerForwardsSubmission(t *testing.T) {
	h := attachedWorker(t)
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	if got := h.tasksTo("node-b"); len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("TASKs to node-b = %v", got)
	}
	if n := len(h.status().Ledger); n != 0 {
		t.Fatalf("a worker recorded a forwarded task: %d", n)
	}
}

// ---- leader: assignment and results ----

// Tasks go round-robin over the attached workers and are recorded as pending
// with their assignee; a result completes the record once.
func TestLeaderAssignsRoundRobinAndRecordsResults(t *testing.T) {
	var r results
	h := newHarness(t, "node-a", withResults(&r))
	for _, w := range ids("node-b", "node-c") {
		h.peerUp(w, 1)
		h.frame(w, protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: w})
	}
	for i := 1; i <= 3; i++ {
		if err := h.submit(echo(fmt.Sprintf("t-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if b, c := h.tasksTo("node-b"), h.tasksTo("node-c"); strings.Join(b, ",") != "t-1,t-3" || strings.Join(c, ",") != "t-2" {
		t.Fatalf("node-b got %v, node-c got %v", b, c)
	}
	if rec := h.record("t-2"); rec.State != taskPending || rec.AssignedTo != "node-c" || string(rec.Body) != `"t-2"` {
		t.Fatalf("t-2 = %+v", rec)
	}
	// The workers hold the pending records too.
	if p := h.lastSyncTo("node-b"); len(p.Ledger) != 3 {
		t.Fatalf("worker snapshot = %+v", p)
	}

	h.frame("node-c", protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-2", OK: false, Output: "boom", DurationMS: 3})
	if rec := h.record("t-2"); rec.State != taskFailed || rec.Result != "boom" || rec.AssignedTo != "node-c" || rec.Body != nil {
		t.Fatalf("t-2 = %+v", rec)
	}
	got := r.all()
	if len(got) != 1 || got[0].Worker != "node-c" || got[0].DurationMS != 3 {
		t.Fatalf("OnTaskResult = %+v", got)
	}

	// Duplicates and strangers change nothing.
	h.frame("node-b", protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-2", Worker: "node-b", OK: true})
	h.frame("node-b", protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-nope", OK: true})
	if n := len(r.all()); n != 1 {
		t.Fatalf("OnTaskResult calls = %d", n)
	}
	if rec := h.record("t-2"); rec.State != taskFailed {
		t.Fatalf("a duplicate overwrote the first result: %+v", rec)
	}

	// A long output is truncated in the ledger, not in the report.
	long := strings.Repeat("y", 2*maxRecordResult)
	h.frame("node-b", protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "t-1", OK: true, Output: long})
	if rec := h.record("t-1"); rec.State != taskDone || len(rec.Result) != maxRecordResult {
		t.Fatalf("t-1 result length = %d", len(rec.Result))
	}
	if got := r.all(); got[len(got)-1].Output != long {
		t.Fatal("the reported output was truncated")
	}
}

// A worker the TASK cannot reach is skipped; with nobody reachable the leader
// runs the task itself.
func TestLeaderSkipsUnreachableWorkers(t *testing.T) {
	var r results
	h := newHarness(t, "node-a", withResults(&r))
	for _, w := range ids("node-b", "node-c") {
		h.peerUp(w, 1)
		h.frame(w, protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: w})
	}
	h.tr.failSend("node-b", network.ErrSendQueueFull)
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	if rec := h.record("t-1"); rec.AssignedTo != "node-c" {
		t.Fatalf("t-1 = %+v", rec)
	}
	h.tr.failSend("node-c", network.ErrUnknownPeer)
	if err := h.submit(echo("t-2")); err != nil {
		t.Fatal(err)
	}
	if rec := h.record("t-2"); rec.AssignedTo != "node-a" || rec.State != taskDone {
		t.Fatalf("t-2 = %+v", rec)
	}
}

// A worker that dies with pending tasks has them re-assigned.
func TestDeadWorkersTasksAreReassigned(t *testing.T) {
	var r results
	h := newHarness(t, "node-a", withResults(&r))
	for _, w := range ids("node-b", "node-c") {
		h.peerUp(w, 1)
		h.frame(w, protocol.TypeJoinCluster, protocol.JoinClusterPayload{Worker: w})
	}
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	if rec := h.record("t-1"); rec.AssignedTo != "node-b" {
		t.Fatalf("setup: %+v", rec)
	}
	h.peerDead("node-b")
	if got := h.tasksTo("node-c"); len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("TASKs to node-c = %v", got)
	}
	if rec := h.record("t-1"); rec.AssignedTo != "node-c" || rec.State != taskPending {
		t.Fatalf("t-1 = %+v", rec)
	}
}

// ---- TASK routing ----

// A task from our leader runs here and its result goes back to that leader,
// with the duration measured on our own clock.
func TestWorkerRunsAssignedTaskOnItsClock(t *testing.T) {
	h := attachedWorker(t)
	baseline := h.clock.pending()
	env, err := protocol.NewEnvelope(protocol.TypeTask, "node-b", "node-z", protocol.TaskPayload{TaskID: "s", Kind: "sleep", Body: json.RawMessage(`{"ms":200}`)})
	if err != nil {
		t.Fatal(err)
	}
	h.node.Handler()("node-b", env)
	h.barrier()
	waitForTimers(t, h.clock, baseline+1)
	h.clock.Advance(200 * time.Millisecond)
	h.settle()
	got := h.resultsTo("node-b")
	if len(got) != 1 {
		t.Fatalf("results to node-b = %+v", got)
	}
	if g := got[0]; g.TaskID != "s" || g.Worker != "node-z" || !g.OK || g.Output != "slept 200ms" || g.DurationMS != 200 {
		t.Fatalf("result = %+v", g)
	}
}

func TestTaskRouting(t *testing.T) {
	t.Run("worker drops a task from a non-leader", func(t *testing.T) {
		h := attachedWorker(t)
		h.frame("node-d", protocol.TypeTask, echo("x"))
		h.frame("node-b", protocol.TypeTask, protocol.TaskPayload{Kind: "echo"}) // no ID
		if n := len(h.tr.sentOf(protocol.TypeTaskResult)); n != 0 {
			t.Fatalf("results sent: %d", n)
		}
	})
	t.Run("worker runs a task from another leader and reports to it", func(t *testing.T) {
		h := attachedWorker(t)
		h.frame("node-c", protocol.TypeTask, echo("x"))
		if got := h.resultsTo("node-c"); len(got) != 1 || got[0].TaskID != "x" {
			t.Fatalf("results to node-c = %+v", got)
		}
	})
	t.Run("leader leads a task from its worker", func(t *testing.T) {
		h := leaderWith(t, "node-b")
		h.frame("node-b", protocol.TypeTask, echo("x"))
		if rec := h.record("x"); rec.AssignedTo != "node-b" {
			t.Fatalf("x = %+v", rec)
		}
	})
	t.Run("leader leads a forward from an unattached worker", func(t *testing.T) {
		h := leaderWith(t)
		h.peerUp("node-q", 1)
		h.frame("node-q", protocol.TypeTask, echo("x"))
		if rec := h.record("x"); rec.AssignedTo != "node-a" || rec.State != taskDone {
			t.Fatalf("x = %+v", rec)
		}
	})
	t.Run("a worker drops results it receives", func(t *testing.T) {
		var r results
		h := attachedWorkerWith(t, func(c *NodeConfig) { c.OnTaskResult = r.record })
		h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 1, Ledger: []protocol.TaskRecord{rec("x", taskPending)}})
		h.frame("node-d", protocol.TypeTaskResult, protocol.TaskResultPayload{TaskID: "x", OK: true})
		if len(r.all()) != 0 || h.record("x").State != taskPending {
			t.Fatal("a worker recorded a result")
		}
	})
}

// A result whose leader has gone goes to our current leader instead.
func TestResultFallsBackToCurrentLeader(t *testing.T) {
	h := attachedWorker(t)
	h.tr.failSend("node-c", network.ErrUnknownPeer)
	h.frame("node-c", protocol.TypeTask, echo("x"))
	if got := h.resultsTo("node-b"); len(got) != 1 || got[0].TaskID != "x" {
		t.Fatalf("results to node-b = %+v", got)
	}
	// Our own leader unreachable too: logged, nothing else to try.
	h.tr.failSend("node-b", network.ErrUnknownPeer)
	h.frame("node-b", protocol.TypeTask, echo("y"))
	h.frame("node-c", protocol.TypeTask, echo("z"))
	if got := h.resultsTo("node-b"); len(got) != 1 {
		t.Fatalf("results to node-b = %+v", got)
	}
}

// A node that ran a task for a leader that died, and now leads itself,
// records the result directly.
func TestResultFallsBackToSelfWhenLeading(t *testing.T) {
	var r results
	release := make(chan struct{})
	h := attachedWorkerWith(t, func(c *NodeConfig) {
		c.OnTaskResult = r.record
		c.Executor = func(ctx context.Context, t protocol.TaskPayload) protocol.TaskResultPayload {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return protocol.TaskResultPayload{OK: true, Output: "late"}
		}
	})
	p := echo("x")
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 1, Ledger: []protocol.TaskRecord{
		{TaskID: "x", State: taskPending, AssignedTo: "node-z", Kind: p.Kind, Body: p.Body},
	}})
	env, err := protocol.NewEnvelope(protocol.TypeTask, "node-b", "node-z", p)
	if err != nil {
		t.Fatal(err)
	}
	h.node.Handler()("node-b", env)
	h.barrier()
	// Both leaders leave while the task runs; node-z is promoted.
	h.tr.failSend("node-b", network.ErrUnknownPeer)
	h.node.Handler()("node-b", mustEnv(t, protocol.TypeLeave, "node-b"))
	h.node.Handler()("node-c", mustEnv(t, protocol.TypeLeave, "node-c"))
	h.barrier()
	h.barrier()
	close(release)
	h.settle()
	if st := h.status(); st.Role != RoleLeader {
		t.Fatalf("not promoted: %s", st)
	}
	if rec := h.record("x"); rec.State != taskDone || rec.Result != "late" {
		t.Fatalf("x = %+v", rec)
	}
	if got := r.all(); len(got) != 1 || got[0].Output != "late" {
		t.Fatalf("OnTaskResult = %+v", got)
	}
}

func mustEnv(t *testing.T, typ protocol.MessageType, from protocol.NodeID) *protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(typ, from, "", protocol.LeavePayload{})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// ---- failover ----

// A promoted worker re-issues the pending tasks of the ledger it adopted,
// once, on its next tick. Completed records and records it cannot re-issue
// (no kind) are left alone.
func TestPromotedWorkerReissuesPendingTasksOnce(t *testing.T) {
	var r results
	h := attachedWorkerWith(t, func(c *NodeConfig) { c.OnTaskResult = r.record })
	p := echo("p")
	h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 3, Ledger: []protocol.TaskRecord{
		{TaskID: "d", State: taskDone, AssignedTo: "node-d", Result: "old", Kind: "echo"},
		{TaskID: "p", State: taskPending, AssignedTo: "node-d", Kind: p.Kind, Body: p.Body},
		{TaskID: "legacy", State: taskPending, AssignedTo: "node-d"},
	}})
	h.frame("node-b", protocol.TypeLeave, nil)
	h.frame("node-c", protocol.TypeLeave, nil)
	if st := h.status(); st.Role != RoleLeader {
		t.Fatalf("not promoted: %s", st)
	}
	if len(r.all()) != 0 {
		t.Fatal("re-issued before the tick")
	}
	h.step(hbEvery)
	got := r.all()
	if len(got) != 1 || got[0].TaskID != "p" || got[0].Output != `"p"` {
		t.Fatalf("OnTaskResult = %+v", got)
	}
	if rec := h.record("d"); rec.Result != "old" {
		t.Fatalf("a completed task was re-run: %+v", rec)
	}
	if rec := h.record("legacy"); rec.State != taskPending {
		t.Fatalf("legacy = %+v", rec)
	}
	h.step(4 * hbEvery)
	if n := len(r.all()); n != 1 {
		t.Fatalf("re-issued more than once: %d", n)
	}
}

// A demoted leader hands its pending tasks to the leader that accepts it.
func TestDemotedLeaderHandsOffPendingTasks(t *testing.T) {
	h := leaderWith(t, "node-b")
	h.step(hbEvery) // past the boot-time re-issue
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	// node-c turns up, measurable and claiming leadership: node-a steps down.
	h.hs.set(addrOf("node-c"), 1.0)
	h.peerUp("node-c", 1)
	h.deltaAbout("node-c", "node-c", RoleLeader, 0.1, 5)
	h.probeRound()
	st := h.status()
	if st.Role != RoleWorker || st.Leader != "node-c" {
		t.Fatalf("setup: %s", st)
	}
	if got := h.tasksTo("node-c"); len(got) != 0 {
		t.Fatalf("handed off before being accepted: %v", got)
	}
	h.frame("node-c", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-c"})
	if got := h.tasksTo("node-c"); len(got) != 1 || got[0] != "t-1" {
		t.Fatalf("TASKs to node-c = %v", got)
	}
	// Once only.
	h.frame("node-c", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-c"})
	h.tr.failSend("node-c", network.ErrSendQueueFull) // and a failed handoff is only logged
	if got := h.tasksTo("node-c"); len(got) != 1 {
		t.Fatalf("TASKs to node-c = %v", got)
	}
}

// A worker whose leader fails carries that leader's pending tasks to its next
// leader; one that merely re-homes away from a live leader does not.
func TestOrphanedWorkerCarriesPendingTasks(t *testing.T) {
	// Threshold 0.3: two leaders among four nodes, one among three, so node-z
	// stays a worker whichever way node-b goes.
	attachedWorker := func(t *testing.T) *harness {
		return attachedWorkerWith(t, func(c *NodeConfig) { c.Election.Threshold = 0.3 })
	}
	syncPending := func(h *harness) {
		t.Helper()
		p := echo("p")
		h.syncFrom("node-b", protocol.StateSyncPayload{Term: h.status().Term, Version: 1, Ledger: []protocol.TaskRecord{
			{TaskID: "p", State: taskPending, AssignedTo: "node-d", Kind: p.Kind, Body: p.Body},
			rec("d", taskDone),
		}})
	}
	acceptedByC := func(h *harness) []string {
		t.Helper()
		st := h.status()
		if st.Leader != "node-c" {
			t.Fatalf("not re-homed to node-c: %s", st)
		}
		h.frame("node-c", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-c"})
		return h.tasksTo("node-c")
	}
	t.Run("leader left", func(t *testing.T) {
		h := attachedWorker(t)
		syncPending(h)
		h.frame("node-b", protocol.TypeLeave, nil)
		if got := acceptedByC(h); strings.Join(got, ",") != "p" {
			t.Fatalf("TASKs to node-c = %v", got)
		}
	})
	t.Run("link lost", func(t *testing.T) {
		h := attachedWorker(t)
		syncPending(h)
		h.peerDown("node-b", network.DispositionPeerDied)
		if got := acceptedByC(h); strings.Join(got, ",") != "p" {
			t.Fatalf("TASKs to node-c = %v", got)
		}
	})
	t.Run("silence", func(t *testing.T) {
		h := attachedWorker(t)
		syncPending(h)
		h.step(time.Duration(DefaultHeartbeatMisses) * hbEvery)
		if got := acceptedByC(h); strings.Join(got, ",") != "p" {
			t.Fatalf("TASKs to node-c = %v", got)
		}
	})
	t.Run("plain re-home", func(t *testing.T) {
		h := attachedWorker(t)
		syncPending(h)
		// node-b loses its seat to node-d (claims worker, reports badly); it
		// is alive.
		h.deltaAbout("node-d", "node-d", RoleWorker, 1.2, 9)
		h.deltaAbout("node-b", "node-b", RoleWorker, 50, 9)
		if got := acceptedByC(h); len(got) != 0 {
			t.Fatalf("tasks of a live leader were handed over: %v", got)
		}
	})
	t.Run("no replica", func(t *testing.T) {
		h := attachedWorker(t)
		h.frame("node-b", protocol.TypeLeave, nil)
		if got := acceptedByC(h); len(got) != 0 {
			t.Fatalf("TASKs to node-c = %v", got)
		}
	})
}

// A handoff that cannot be sent is logged and dropped.
func TestFailedHandoffIsDropped(t *testing.T) {
	h := leaderWith(t, "node-b")
	h.step(hbEvery)
	if err := h.submit(echo("t-1")); err != nil {
		t.Fatal(err)
	}
	h.hs.set(addrOf("node-c"), 1.0)
	h.peerUp("node-c", 1)
	h.deltaAbout("node-c", "node-c", RoleLeader, 0.1, 5)
	h.probeRound()
	h.tr.failSend("node-c", network.ErrSendQueueFull)
	h.frame("node-c", protocol.TypeJoinAck, protocol.JoinAckPayload{Accepted: true, Leader: "node-c"})
	if err := h.node.barrier(h.ctx, func() {
		if len(h.node.tasks.handoff) != 0 {
			t.Error("handoff kept after a failed send")
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// ---- execution safety ----

// A panicking executor fails the task instead of killing the node.
func TestExecutorPanicIsAFailedTask(t *testing.T) {
	var r results
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		withResults(&r)(c)
		c.Executor = func(context.Context, protocol.TaskPayload) protocol.TaskResultPayload { panic("kaboom") }
	})
	if err := h.submit(echo("t")); err != nil {
		t.Fatal(err)
	}
	got := r.all()
	if len(got) != 1 || got[0].OK || !strings.Contains(got[0].Output, "kaboom") || got[0].TaskID != "t" || got[0].Worker != "node-a" {
		t.Fatalf("result = %+v", got)
	}
}

// Past maxInflightTasks a task fails at once with "busy"; the running ones are
// unaffected.
func TestTooManyRunningTasksFailFast(t *testing.T) {
	var r results
	release := make(chan struct{})
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		withResults(&r)(c)
		c.LedgerSize = maxInflightTasks + 10
		c.Executor = func(ctx context.Context, t protocol.TaskPayload) protocol.TaskResultPayload {
			select {
			case <-release:
				return protocol.TaskResultPayload{OK: true, Output: "ran"}
			case <-ctx.Done():
				return protocol.TaskResultPayload{OK: false, Output: "cancelled"}
			}
		}
	})
	h.step(hbEvery)
	for i := 0; i <= maxInflightTasks; i++ {
		if err := h.node.SubmitTask(h.ctx, echo(fmt.Sprintf("t-%03d", i))); err != nil {
			t.Fatal(err)
		}
	}
	h.barrier()
	got := r.all()
	if len(got) != 1 || got[0].OK || !strings.HasPrefix(got[0].Output, "busy") {
		t.Fatalf("results before release = %+v", got)
	}
	close(release)
	h.settle()
	if n := len(r.all()); n != maxInflightTasks+1 {
		t.Fatalf("results = %d, want %d", n, maxInflightTasks+1)
	}
}

// Shutdown cancels running tasks and waits for them.
func TestShutdownCancelsRunningTasks(t *testing.T) {
	var cancelled sync.WaitGroup
	cancelled.Add(1)
	h := newHarness(t, "node-a", func(c *NodeConfig) {
		phase4(c)
		c.Executor = func(ctx context.Context, t protocol.TaskPayload) protocol.TaskResultPayload {
			<-ctx.Done()
			cancelled.Done()
			return protocol.TaskResultPayload{}
		}
	})
	if err := h.node.SubmitTask(h.ctx, echo("t")); err != nil {
		t.Fatal(err)
	}
	h.barrier()
	h.stop() // fails the test if Run does not return
	cancelled.Wait()
}

// settle waits for a running task and an in-flight probe round together, in
// whichever order they finish.
func TestSettleWaitsForTasksAndRounds(t *testing.T) {
	var r results
	h := newHarness(t, "node-a", withResults(&r))
	h.hs.set(addrOf("node-b"), 1.0)
	h.peerUp("node-b", 1)
	release := h.hs.hold(addrOf("node-b"))
	h.clock.Advance(probeEvery)
	h.barrier()
	if err := h.node.SubmitTask(h.ctx, echo("t")); err != nil {
		t.Fatal(err)
	}
	go release()
	h.settle()
	if n := len(r.all()); n != 1 {
		t.Fatalf("results after settle = %d", n)
	}
	if got := h.status().Scores["node-b"]; got != 1.0 || !health.IsValidScore(got) {
		t.Fatalf("probe not applied: %v", got)
	}
}
