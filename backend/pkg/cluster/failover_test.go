package cluster

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// failoverBound is how long, on the FakeClock, a 5-node swarm may take to
// converge after a leader crash: the suspicion timeout (the earliest moment
// the swarm may re-elect), plus two failure-detector ticks for the timeout to
// be noticed and for the promotion, the re-homing JOINs and the re-issue to
// settle. Everything before that is detection, which is deliberate.
const failoverBound = DefaultSuspicionTimeout + 2*DefaultHeartbeatInterval

func fiveIDs() []protocol.NodeID {
	return ids("node-1", "node-2", "node-3", "node-4", "node-5")
}

// The Phase 4 end-to-end property. Five nodes over a latency-simulating mesh
// elect two leaders and attach three workers. A leader with workers is handed
// tasks, its workers run them, and it crashes before their results arrive.
//
// Within failoverBound of the crash:
//   - every survivor agrees on a leader set of two that excludes the victim,
//   - every worker is attached to a leader that lists it (and vice versa),
//   - every task the victim had outstanding has been reported by a survivor
//     (at-least-once: the victim's replicas carried them).
//
// Before the bound the dead leader keeps its seat (it is only suspect), which
// is the anti-thrash rule; after the bound the swarm stays converged.
func TestFailoverConvergesAfterLeaderCrash(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			var r results
			s := newSim(t, seed, fiveIDs(), 0.2, 3, func(c *NodeConfig) { c.OnTaskResult = r.record })
			s.jitter = DefaultHysteresis / 5
			s.run(15*time.Second, nil)
			before := s.check()
			if len(before.problems) > 0 {
				t.Fatalf("not converged before the crash: %v", before.problems)
			}
			if len(before.leaders) != 2 {
				t.Fatalf("leaders = %v, want 2", before.leaders)
			}

			// The victim is the leader with the most workers, so there is work
			// in flight to lose.
			victim := before.leaders[0]
			if a, b := s.node(before.leaders[0]).node.Status(), s.node(before.leaders[1]).node.Status(); len(b.Attached) > len(a.Attached) {
				victim = before.leaders[1]
			}
			vn := s.node(victim)
			if len(vn.node.Status().Attached) == 0 {
				t.Fatalf("setup: victim %s has no workers", victim)
			}

			// Tasks reach the workers and are replicated; their results die
			// with the victim.
			s.net.setDrop(func(f simFrame) bool { return f.to == victim && f.env.Type == protocol.TypeTaskResult })
			var tasks []string
			for i := 0; i < 6; i++ {
				id := fmt.Sprintf("task-%d", i)
				tasks = append(tasks, id)
				if err := vn.node.SubmitTask(s.ctx, echo(id)); err != nil {
					t.Fatal(err)
				}
			}
			s.settleAll()
			s.flush()
			pending := 0
			for _, rec := range vn.node.Status().Ledger {
				if rec.State == taskPending {
					pending++
				}
			}
			if pending == 0 {
				t.Fatal("setup: nothing pending at the victim")
			}
			if n := len(r.all()); n != 0 {
				t.Fatalf("setup: %d results reported before the crash", n)
			}

			s.kill(victim)
			s.net.setDrop(nil)
			crashedAt := s.elapsed

			reported := func() []string {
				seen := make(map[string]bool)
				for _, res := range r.all() {
					seen[res.TaskID] = true
				}
				var missing []string
				for _, id := range tasks {
					if !seen[id] {
						missing = append(missing, id)
					}
				}
				return missing
			}

			converged := time.Duration(-1)
			var last agreement
			var lastMissing []string
			s.run(failoverBound+20*time.Second, func() {
				c := s.check()
				missing := reported()
				ok := len(c.problems) == 0 && len(c.leaders) == 2 && !contains(c.leaders, victim) && len(missing) == 0
				switch {
				case converged < 0 && ok:
					converged = s.elapsed - crashedAt
					last = c
				case converged >= 0 && !ok:
					t.Fatalf("t=+%v: diverged after converging at +%v: %v, leaders %v, missing %v",
						s.elapsed-crashedAt, converged, c.problems, c.leaders, missing)
				case converged >= 0 && !equalIDs(c.leaders, last.leaders):
					t.Fatalf("t=+%v: leaders moved %v -> %v after converging", s.elapsed-crashedAt, last.leaders, c.leaders)
				case converged < 0:
					last, lastMissing = c, missing
				}
			})
			if converged < 0 {
				t.Fatalf("never converged: %v, leaders %v, missing %v", last.problems, last.leaders, lastMissing)
			}
			if converged > failoverBound {
				t.Fatalf("converged at +%v, bound %v", converged, failoverBound)
			}
			// The seat was kept while the victim was only suspect: nobody
			// re-elected before the suspicion timed out.
			if converged < DefaultSuspicionTimeout {
				t.Fatalf("converged at +%v, before the suspicion could time out", converged)
			}
			t.Logf("converged %v after the crash; leaders %v -> %v; results %d for %d tasks (%s)",
				converged, before.leaders, last.leaders, len(r.all()), len(tasks), strings.Join(tasks, ","))
		})
	}
}

// A worker crash costs no re-election: the leader set is unchanged, and the
// dead worker's pending tasks are re-run elsewhere once it is confirmed dead.
func TestWorkerCrashKeepsLeadersAndReassignsTasks(t *testing.T) {
	var r results
	s := newSim(t, 21, fiveIDs(), 0.2, 3, func(c *NodeConfig) { c.OnTaskResult = r.record })
	s.jitter = DefaultHysteresis / 5
	s.run(15*time.Second, nil)
	before := s.check()
	if len(before.problems) > 0 {
		t.Fatalf("not converged: %v", before.problems)
	}
	var leader, victim protocol.NodeID
	for _, l := range before.leaders {
		if att := s.node(l).node.Status().Attached; len(att) > 0 {
			leader, victim = l, att[0]
			break
		}
	}
	if victim == "" {
		t.Fatal("setup: no attached worker")
	}
	ln := s.node(leader)
	// Round-robin decides who gets each task, so submit until one lands on the
	// victim. Its result is lost, so the task stays pending.
	s.net.setDrop(func(f simFrame) bool { return f.from == victim && f.env.Type == protocol.TypeTaskResult })
	var onVictim string
	for i := 0; i < 8 && onVictim == ""; i++ {
		id := fmt.Sprintf("w-%d", i)
		if err := ln.node.SubmitTask(s.ctx, echo(id)); err != nil {
			t.Fatal(err)
		}
		s.settleAll()
		s.flush()
		for _, rec := range ln.node.Status().Ledger {
			if rec.TaskID == id && rec.AssignedTo == victim && rec.State == taskPending {
				onVictim = id
			}
		}
	}
	if onVictim == "" {
		t.Fatal("setup: no task landed on the victim")
	}
	s.kill(victim)
	s.net.setDrop(nil)
	s.run(failoverBound, nil)
	after := s.check()
	if len(after.problems) > 0 || !equalIDs(after.leaders, before.leaders) {
		t.Fatalf("after a worker crash: leaders %v -> %v, %v", before.leaders, after.leaders, after.problems)
	}
	found := false
	for _, res := range r.all() {
		if res.TaskID == onVictim && res.Worker != victim {
			found = true
		}
	}
	if !found {
		t.Fatalf("task %s was not re-run: %+v", onVictim, r.all())
	}
}
