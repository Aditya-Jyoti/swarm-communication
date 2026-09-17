package cluster

import (
	"context"
	"fmt"
	"slices"

	"swarm-net/pkg/protocol"
)

// Tasks: Control Center -> leader -> worker -> leader -> Control Center.
//
//	SubmitTask on a leader  : record pending, assign round-robin to an attached
//	                          worker (or run locally), send TASK.
//	SubmitTask on a worker  : forward TASK to its leader.
//	TASK from our leader    : run it, send TASK_RESULT back to that leader.
//	TASK from a worker      : a forwarded submission; lead it.
//	TASK_RESULT on a leader : mark the record done/failed, call OnTaskResult.
//
// # Delivery is at-least-once
//
// A task can run twice: a leader that dies with pending tasks has them
// re-issued by whoever is promoted (from its replicated ledger), a worker that
// dies has its pending tasks re-assigned, and a demoted leader hands its
// pending tasks to its new leader. In each case the original run may also
// finish. The ledger keeps the first result for a task ID, and the Control
// Center keeps the first it hears. Exactly-once would need the result to be
// recorded atomically with the work, which a best-effort ledger replicated by
// snapshot cannot give.
//
// # TASK and TASK_RESULT are data plane
//
// They are shed when a node's inbound data queue is full. A shed TASK leaves
// its record pending until a failover re-issues it; a shed result costs the
// same. That is the approved backpressure policy (WORKLOG 4.1): under overload
// the control plane stays healthy and work is dropped, not the other way round.

// maxInflightTasks bounds the task goroutines one node runs at once. A task
// assigned beyond it fails at once with "busy" instead of queueing without
// bound. It also sizes taskDone, so a finished task never blocks.
const maxInflightTasks = 256

type taskState struct {
	// inflight counts running task goroutines. Loop-owned: incremented at
	// launch, decremented when the outcome is handled.
	inflight int
	// next is the round-robin cursor over the sorted attached set.
	next int
	// reissueDue is set on promotion; the next tick re-issues every pending
	// task in the adopted ledger, once.
	reissueDue bool
	// handoff holds the pending tasks a demoted leader still owed; they are
	// forwarded to the next leader that accepts us.
	handoff []protocol.TaskPayload
}

type submitReq struct {
	task  protocol.TaskPayload
	reply chan error
}

// taskOutcome is a finished task. origin is the leader to report to, or empty
// for a task this node ran as its own leader.
type taskOutcome struct {
	origin protocol.NodeID
	result protocol.TaskResultPayload
}

// submit is SubmitTask on the loop.
func (n *Node) submit(ctx context.Context, t protocol.TaskPayload) error {
	switch {
	case n.isLeader():
		n.leadTask(ctx, t)
		return nil
	case n.leader == "":
		return ErrNoLeader
	}
	if err := n.sendTask(ctx, n.leader, t); err != nil {
		return fmt.Errorf("cluster: node %s: forward task %s to leader %s: %w", n.cfg.Self, t.TaskID, n.leader, err)
	}
	return nil
}

// leadTask records t as pending and assigns it. A task ID already in the
// ledger is a duplicate (a re-issue racing the original, a handoff of a task
// we already hold) and is ignored: the ledger keeps the first.
func (n *Node) leadTask(ctx context.Context, t protocol.TaskPayload) {
	if n.ledgerIndex(t.TaskID) >= 0 {
		n.log.Debug("duplicate task ignored", "task", t.TaskID)
		return
	}
	n.ledgerAppend(protocol.TaskRecord{TaskID: t.TaskID, State: taskPending, Kind: t.Kind, Body: t.Body})
	n.assign(ctx, t)
}

// assign sends t to the next attached worker that will take it, or runs it
// here when there is none.
func (n *Node) assign(ctx context.Context, t protocol.TaskPayload) {
	workers := n.attachedIDs()
	for k := range workers {
		w := workers[(n.tasks.next+k)%len(workers)]
		if err := n.sendTask(ctx, w, t); err != nil {
			n.log.Log(ctx, sendFailureLevel(err), "TASK send failed; trying the next worker", "task", t.TaskID, "worker", w, "err", err)
			continue
		}
		n.tasks.next = (n.tasks.next + k + 1) % len(workers)
		n.setAssignee(t.TaskID, w)
		return
	}
	n.setAssignee(t.TaskID, n.cfg.Self)
	n.launch(t, "")
}

func (n *Node) sendTask(ctx context.Context, to protocol.NodeID, t protocol.TaskPayload) error {
	env, err := protocol.NewEnvelope(protocol.TypeTask, n.cfg.Self, to, t)
	if err != nil {
		return err
	}
	return n.cfg.Transport.Send(ctx, to, env)
}

func (n *Node) setAssignee(taskID string, w protocol.NodeID) {
	if i := n.ledgerIndex(taskID); i >= 0 && n.repl.ledger[i].AssignedTo != w {
		n.repl.ledger[i].AssignedTo = w
		n.repl.changed()
	}
}

// ledgerIndex returns the position of taskID, or -1. Linear: the ledger is at
// most LedgerSize records, and lookups happen once per task event.
func (n *Node) ledgerIndex(taskID string) int {
	return slices.IndexFunc(n.repl.ledger, func(r protocol.TaskRecord) bool { return r.TaskID == taskID })
}

// launch runs t on its own goroutine and reports to origin (empty: ourselves).
//
// The goroutine never touches node state. It hands its outcome to the loop
// through taskDone, and it is bound to Run's context, which shutdown has
// already cancelled by the time it waits on taskWG.
func (n *Node) launch(t protocol.TaskPayload, origin protocol.NodeID) {
	if n.tasks.inflight >= maxInflightTasks {
		n.log.Warn("too many running tasks; refusing", "task", t.TaskID, "running", n.tasks.inflight)
		n.handleOutcomeNow(taskOutcome{origin: origin, result: protocol.TaskResultPayload{
			TaskID: t.TaskID, Worker: n.cfg.Self, OK: false, Output: "busy: too many running tasks",
		}})
		return
	}
	n.tasks.inflight++
	ctx := n.runCtx
	n.taskWG.Add(1)
	go func() {
		defer n.taskWG.Done()
		res := n.execute(ctx, t)
		select {
		case n.taskDone <- taskOutcome{origin: origin, result: res}:
		case <-ctx.Done():
		}
	}()
}

// handleOutcomeNow handles an outcome that never had a goroutine.
func (n *Node) handleOutcomeNow(o taskOutcome) {
	n.tasks.inflight++ // balanced by handleTaskOutcome
	n.handleTaskOutcome(n.runCtx, o)
}

// execute runs the Executor with the node's bookkeeping around it. Task
// goroutine only: it reads cfg (immutable after NewNode) and the Clock (safe
// for concurrent use), and nothing else.
//
// A panicking executor is caught and reported as a failed task. It is
// user-supplied code on a goroutine the node owns, and an unrecovered panic
// there would take the whole node down with it.
func (n *Node) execute(ctx context.Context, t protocol.TaskPayload) (res protocol.TaskResultPayload) {
	start := n.cfg.Clock.Now()
	defer func() {
		if p := recover(); p != nil {
			res = protocol.TaskResultPayload{OK: false, Output: fmt.Sprintf("task panicked: %v", p)}
		}
		res.TaskID = t.TaskID
		res.Worker = n.cfg.Self
		// Measured here, on this node's clock (monotonic in production):
		// never derived from a timestamp another node wrote.
		res.DurationMS = float64(n.cfg.Clock.Now().Sub(start)) / 1e6
	}()
	return n.cfg.Executor(ctx, t)
}

// handleTaskOutcome reports a finished task. Loop goroutine only.
func (n *Node) handleTaskOutcome(ctx context.Context, o taskOutcome) {
	n.tasks.inflight--
	if o.origin == "" {
		n.completeTask(o.result)
		return
	}
	n.sendResult(ctx, o.origin, o.result)
}

// sendResult sends a result to the leader that assigned the task. If that
// leader is gone, the result goes to our current leader instead: after a
// failover that is the node holding the task in its adopted ledger, and
// delivering there saves a re-run. If we lead now, we record it ourselves.
func (n *Node) sendResult(ctx context.Context, to protocol.NodeID, res protocol.TaskResultPayload) {
	err := n.sendResultTo(ctx, to, res)
	if err == nil {
		return
	}
	switch fallback := n.leader; {
	case fallback == n.cfg.Self:
		n.completeTask(res)
	case fallback != "" && fallback != to:
		if ferr := n.sendResultTo(ctx, fallback, res); ferr != nil {
			n.log.Warn("TASK_RESULT undeliverable", "task", res.TaskID, "to", to, "fallback", fallback, "err", ferr)
		}
	default:
		n.log.Warn("TASK_RESULT undeliverable", "task", res.TaskID, "to", to, "err", err)
	}
}

func (n *Node) sendResultTo(ctx context.Context, to protocol.NodeID, res protocol.TaskResultPayload) error {
	env, err := protocol.NewEnvelope(protocol.TypeTaskResult, n.cfg.Self, to, res)
	if err != nil {
		return err
	}
	return n.cfg.Transport.Send(ctx, to, env)
}

// completeTask records a result in the ledger and reports it upward, once per
// task. Only a leader records results: a worker's ledger is a replica, and
// editing it would be overwritten by the next snapshot anyway.
func (n *Node) completeTask(res protocol.TaskResultPayload) {
	if !n.isLeader() {
		n.log.Debug("result for a task we do not lead; dropped", "task", res.TaskID)
		return
	}
	i := n.ledgerIndex(res.TaskID)
	if i < 0 {
		n.log.Debug("result for an unknown task; dropped", "task", res.TaskID, "worker", res.Worker)
		return
	}
	r := &n.repl.ledger[i]
	if r.State != taskPending {
		n.log.Debug("duplicate result; keeping the first", "task", res.TaskID, "worker", res.Worker)
		return
	}
	r.State = taskDone
	if !res.OK {
		r.State = taskFailed
	}
	r.Result = truncateUTF8(res.Output, maxRecordResult)
	r.Body = nil // only needed for a re-issue, which a finished task never gets
	if res.Worker != "" {
		r.AssignedTo = res.Worker
	}
	n.repl.changed()
	n.log.Debug("task finished", "task", res.TaskID, "worker", res.Worker, "ok", res.OK)
	if n.cfg.OnTaskResult != nil {
		n.cfg.OnTaskResult(res)
	}
}

// handleTask routes a TASK frame. The sender's relationship to us decides what
// it means, because the frame itself is the same either way:
//
//   - from the leader we are attached to: an assignment. Run it and report to
//     the sender.
//   - from one of our own workers: a forwarded submission (or a demoted
//     leader's handoff). Lead it. Checked before the next case because a
//     worker that just stepped down may still be a leader in our view.
//   - from any other node we believe leads: an assignment from a leader that
//     has not yet noticed we left it. Doing the work is cheaper than bouncing
//     it, and delivery is at-least-once anyway.
//   - otherwise, if we lead: a forward from a worker not (yet) attached. Lead it.
//   - otherwise: nobody we should take work from. Dropped, with a warning. A
//     worker never forwards a forward, which is what rules out loops.
func (n *Node) handleTask(ctx context.Context, from protocol.NodeID, t protocol.TaskPayload) {
	if t.TaskID == "" {
		n.log.Warn("TASK without an ID; dropped", "from", from)
		return
	}
	_, ours := n.attached[from]
	switch {
	case from == n.leader && !n.isLeader():
		n.launch(t, from)
	case n.isLeader() && ours:
		n.leadTask(ctx, t)
	case contains(n.leaders, from):
		n.launch(t, from)
	case n.isLeader():
		n.leadTask(ctx, t)
	default:
		n.log.Warn("TASK from a non-leader while not leading; dropped", "task", t.TaskID, "from", from)
	}
}

func (n *Node) handleTaskResult(from protocol.NodeID, res protocol.TaskResultPayload) {
	if res.Worker == "" {
		res.Worker = from
	}
	n.completeTask(res)
}

// reassignFrom re-assigns the pending tasks of a worker that has died or left.
func (n *Node) reassignFrom(ctx context.Context, worker protocol.NodeID) {
	if !n.isLeader() {
		return
	}
	for _, t := range n.pendingTasks(func(r protocol.TaskRecord) bool { return r.AssignedTo == worker }) {
		n.log.Info("re-assigning task of a departed worker", "task", t.TaskID, "worker", worker)
		n.assign(ctx, t)
	}
}

// pendingTasks returns the re-issuable pending tasks matching keep, as
// payloads. A record without a Kind came from a build that did not replicate
// the task itself and cannot be re-issued.
func (n *Node) pendingTasks(keep func(protocol.TaskRecord) bool) []protocol.TaskPayload {
	var out []protocol.TaskPayload
	for _, r := range n.repl.ledger {
		if r.State != taskPending || !keep(r) {
			continue
		}
		if r.Kind == "" {
			n.log.Warn("pending task has no kind; cannot re-issue", "task", r.TaskID)
			continue
		}
		out = append(out, protocol.TaskPayload{TaskID: r.TaskID, Kind: r.Kind, Body: r.Body})
	}
	return out
}

// tasksRoleChanged arms the failover paths: a promoted node re-issues the
// pending tasks of the ledger it adopted (on the next tick, so the workers
// re-homing to it have a moment to arrive); a demoted node hands its pending
// tasks to its next leader.
func (n *Node) tasksRoleChanged(leading bool) {
	if leading {
		n.tasks.reissueDue = true
		n.tasks.handoff = nil // still in our ledger; the re-issue covers them
		return
	}
	n.tasks.reissueDue = false
	n.tasks.handoff = n.pendingTasks(func(protocol.TaskRecord) bool { return true })
}

// adoptOrphans is called when a worker leaves its leader because that leader
// failed (a first-hand suspicion, or silence). The replica's pending tasks are
// the only surviving copy of that leader's outstanding work unless the node
// promoted in its place happens to be one of its workers, which election does
// not promise: it promotes the healthiest node, from any cluster. So every
// orphaned worker carries the tasks to its next leader, which keeps the first
// copy of each ID and ignores the rest.
//
// Not called on an ordinary re-home: a leader that is alive still owns its
// tasks, and handing them over would only run them twice for nothing.
func (n *Node) adoptOrphans() {
	if !n.repl.held {
		return // no replica from the leader we are leaving
	}
	n.tasks.handoff = n.pendingTasks(func(protocol.TaskRecord) bool { return true })
	if len(n.tasks.handoff) > 0 {
		n.log.Info("carrying a failed leader's pending tasks", "count", len(n.tasks.handoff))
	}
}

// reissueIfDue re-issues every pending task once after a promotion.
func (n *Node) reissueIfDue(ctx context.Context) {
	if !n.tasks.reissueDue {
		return
	}
	n.tasks.reissueDue = false
	pending := n.pendingTasks(func(protocol.TaskRecord) bool { return true })
	if len(pending) > 0 {
		n.log.Info("re-issuing pending tasks after promotion", "count", len(pending))
	}
	for _, t := range pending {
		n.assign(ctx, t)
	}
}

// handOff forwards a demoted leader's pending tasks to the leader that just
// accepted us.
func (n *Node) handOff(ctx context.Context) {
	if len(n.tasks.handoff) == 0 || n.leader == "" || n.leader == n.cfg.Self {
		return
	}
	n.log.Info("handing pending tasks to the new leader", "count", len(n.tasks.handoff), "leader", n.leader)
	for _, t := range n.tasks.handoff {
		if err := n.sendTask(ctx, n.leader, t); err != nil {
			n.log.Warn("task handoff failed", "task", t.TaskID, "leader", n.leader, "err", err)
		}
	}
	n.tasks.handoff = nil
}
