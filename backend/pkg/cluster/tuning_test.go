package cluster

import (
	"context"
	"math"
	"testing"
)

// loopCfg reads the loop-owned election settings on the loop goroutine.
func (h *harness) loopCfg() (election Config, rehome float64) {
	h.t.Helper()
	if err := h.node.barrier(h.ctx, func() {
		election, rehome = h.node.cfg.Election, h.node.cfg.Rehome
	}); err != nil {
		h.t.Fatalf("barrier: %v", err)
	}
	return election, rehome
}

// Raising the threshold makes every scored node a leader; lowering it again
// converges back to the single best one.
func TestSetElectionParamsThresholdChangesLeaderCount(t *testing.T) {
	h := threeNodes(t, "node-z")
	st := h.status()
	requireIDs(t, "Leaders before", st.Leaders, ids("node-b"))
	if st.Threshold != DefaultThreshold {
		t.Fatalf("Threshold = %v, want %v", st.Threshold, DefaultThreshold)
	}
	termBefore := st.Term

	h.node.SetElectionParams(1, -1)
	h.settle()
	st = h.status()
	if st.Threshold != 1 {
		t.Fatalf("Threshold = %v, want 1", st.Threshold)
	}
	requireIDs(t, "Leaders at threshold 1", st.Leaders, ids("node-b", "node-c", "node-z"))
	if st.Term <= termBefore {
		t.Fatalf("Term = %d, want > %d after the leader set changed", st.Term, termBefore)
	}
	if st.Role != RoleLeader || st.Leader != "node-z" {
		t.Fatalf("self = %s leading %q, want leader of itself", st.Role, st.Leader)
	}

	h.node.SetElectionParams(0.3, -1)
	h.settle()
	h.probeRound() // a later trigger must not move it again
	st = h.status()
	requireIDs(t, "Leaders back at 0.3", st.Leaders, ids("node-b"))
	if st.Role != RoleWorker {
		t.Fatalf("self role = %s, want worker again", st.Role)
	}
	if st.Hysteresis != 0.1 {
		t.Fatalf("Hysteresis = %v, want the configured 0.1 untouched", st.Hysteresis)
	}
}

// An explicit hysteresis of 0 is applied as 0 to both margins, unlike a 0 in
// NodeConfig, and "unchanged" values leave the settings alone.
func TestSetElectionParamsHysteresis(t *testing.T) {
	h := threeNodes(t, "node-z")
	if st := h.status(); st.Hysteresis != 0.1 {
		t.Fatalf("Hysteresis = %v, want 0.1", st.Hysteresis)
	}

	h.node.SetElectionParams(0, 0.7)
	h.settle()
	el, rehome := h.loopCfg()
	if el.Hysteresis != 0.7 || rehome != 0.7 || el.Threshold != DefaultThreshold {
		t.Fatalf("after (0, 0.7): election %+v rehome %v", el, rehome)
	}
	if st := h.status(); st.Hysteresis != 0.7 || st.Threshold != DefaultThreshold {
		t.Fatalf("Status threshold/hysteresis = %v/%v, want %v/0.7", st.Threshold, st.Hysteresis, DefaultThreshold)
	}

	h.node.SetElectionParams(0, 0)
	h.settle()
	el, rehome = h.loopCfg()
	if el.Hysteresis != 0 || rehome != 0 {
		t.Fatalf("after (0, 0): election %+v rehome %v, want explicit zero", el, rehome)
	}
	if st := h.status(); st.Hysteresis != 0 {
		t.Fatalf("Status.Hysteresis = %v, want 0", st.Hysteresis)
	}

	// Every one of these means "unchanged".
	for _, p := range [][2]float64{
		{0, -1},
		{math.NaN(), math.NaN()},
		{1.5, math.Inf(1)},
		{-0.2, math.Inf(-1)},
	} {
		h.node.SetElectionParams(p[0], p[1])
	}
	h.settle()
	el, rehome = h.loopCfg()
	if el.Hysteresis != 0 || rehome != 0 || el.Threshold != DefaultThreshold {
		t.Fatalf("ignored values changed settings: election %+v rehome %v", el, rehome)
	}
	requireIDs(t, "Leaders", h.status().Leaders, ids("node-b"))
}

// Several calls before the loop runs merge field by field, latest wins.
func TestSetElectionParamsMergesPendingCalls(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.node.SetElectionParams(0.5, -1)
	h.node.SetElectionParams(0, 0.2)
	h.node.SetElectionParams(0.6, -1)
	h.settle()
	st := h.status()
	if st.Threshold != 0.6 || st.Hysteresis != 0.2 {
		t.Fatalf("Threshold/Hysteresis = %v/%v, want 0.6/0.2", st.Threshold, st.Hysteresis)
	}
}

// An override set before Run is in force from the first election, and a node
// configured with zeros reports the defaults.
func TestSetElectionParamsBeforeRun(t *testing.T) {
	tr := newFakeTransport("node-a")
	cfg := baseConfig("node-a", tr, newFakeHealth(), NewFakeClock(epoch))
	cfg.Election = Config{}
	cfg.Rehome = 0
	n, err := NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st := n.Status(); st.Threshold != DefaultThreshold || st.Hysteresis != DefaultHysteresis {
		t.Fatalf("defaults = %v/%v, want %v/%v", st.Threshold, st.Hysteresis, DefaultThreshold, DefaultHysteresis)
	}

	// Never blocks, even though no loop is running yet.
	n.SetElectionParams(0.9, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	if err := n.settle(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if st := n.Status(); st.Threshold != 0.9 || st.Hysteresis != 0 {
		t.Fatalf("after Run = %v/%v, want 0.9/0", st.Threshold, st.Hysteresis)
	}
	if st := n.Status(); len(st.Leaders) != 1 || st.Leaders[0] != "node-a" {
		t.Fatalf("Leaders = %v, want [node-a]", st.Leaders)
	}
}

// SetElectionParams after the loop has exited must not block or panic.
func TestSetElectionParamsAfterStop(t *testing.T) {
	h := newHarness(t, "node-a", nil)
	h.stop()
	h.node.SetElectionParams(0.5, 0.5)
	h.node.SetElectionParams(0.4, 0.4)
}
