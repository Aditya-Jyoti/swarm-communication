package cluster

import (
	"context"
	"math"
)

// electionTuning is one pending runtime override. hasThreshold and
// hasHysteresis say which fields are set: the caller's "unchanged" values
// (threshold 0, hysteresis < 0) never get this far.
type electionTuning struct {
	threshold     float64
	hysteresis    float64
	hasThreshold  bool
	hasHysteresis bool
}

// SetElectionParams changes the election settings of a running node. It is the
// SIM_CONFIG hook: the dashboard's threshold and hysteresis sliders end here.
//
//   - threshold is the leader fraction in (0, 1]. 0 means "unchanged"; any other
//     value outside (0, 1], or NaN, is ignored as well.
//   - hysteresis is the margin in score units. A negative value means
//     "unchanged"; NaN and +Inf are ignored. 0 is legal and means NO damping.
//     It sets both the election margin (Election.Hysteresis) and the re-home
//     margin (Rehome).
//
// # Why an explicit 0 really means 0
//
// NodeConfig's rule "Hysteresis 0 means DefaultHysteresis" is applied once, by
// withDefaults, when NewNode builds the node. After that the loop reads
// cfg.Election.Hysteresis through Elect, and cfg.Rehome through ShouldRehome,
// and both of those read 0 as "no damping". So the override is written into cfg
// directly and never passes through withDefaults again; no sentinel is needed
// to tell a configured 0 from a runtime 0.
//
// # Why not the calls channel
//
// It never blocks. The caller is the telemetry client's reader goroutine, and a
// send on the unbuffered calls channel would stall it (and its ping deadline)
// until the loop came round, or forever before Run starts. Instead the override
// is merged into a mutex-guarded pending slot and the loop is woken through a
// one-slot channel. Several calls before the loop wakes collapse into one, the
// latest value of each field winning, which is the right answer for a slider.
// A call made before Run is applied when Run starts.
//
// Safe to call from any goroutine. The values take effect on the loop
// goroutine, followed by an evaluate, and show up in Status.
func (n *Node) SetElectionParams(threshold, hysteresis float64) {
	var t electionTuning
	if threshold > 0 && threshold <= 1 { // false for NaN
		t.threshold, t.hasThreshold = threshold, true
	}
	if hysteresis >= 0 && !math.IsInf(hysteresis, 1) { // false for NaN
		t.hysteresis, t.hasHysteresis = hysteresis, true
	}
	if !t.hasThreshold && !t.hasHysteresis {
		return
	}

	n.tuneMu.Lock()
	if t.hasThreshold {
		n.tunePending.threshold, n.tunePending.hasThreshold = t.threshold, true
	}
	if t.hasHysteresis {
		n.tunePending.hysteresis, n.tunePending.hasHysteresis = t.hysteresis, true
	}
	n.tuneMu.Unlock()

	// A full slot already holds a wake-up that will read the merged value.
	select {
	case n.tuneCh <- struct{}{}:
	default:
	}
}

// takeTuning returns and clears the pending override. Any goroutine.
func (n *Node) takeTuning() electionTuning {
	n.tuneMu.Lock()
	defer n.tuneMu.Unlock()
	t := n.tunePending
	n.tunePending = electionTuning{}
	return t
}

// applyTuning installs the pending override and re-runs the election if
// anything changed. Loop goroutine only: cfg.Election and cfg.Rehome are
// loop-owned once Run has started.
func (n *Node) applyTuning(ctx context.Context) {
	t := n.takeTuning()
	changed := false
	if t.hasThreshold && t.threshold != n.cfg.Election.Threshold {
		n.cfg.Election.Threshold = t.threshold
		changed = true
	}
	if t.hasHysteresis && (t.hysteresis != n.cfg.Election.Hysteresis || t.hysteresis != n.cfg.Rehome) {
		n.cfg.Election.Hysteresis = t.hysteresis
		n.cfg.Rehome = t.hysteresis
		changed = true
	}
	if !changed {
		return
	}
	n.log.Info("election settings changed",
		"threshold", n.effectiveThreshold(), "hysteresis", n.cfg.Election.Hysteresis)
	// evaluate ends by publishing, so Status carries the new settings even when
	// the leader set does not move.
	n.evaluate(ctx)
}

// effectiveThreshold is the leader fraction Elect actually uses.
func (n *Node) effectiveThreshold() float64 {
	return n.cfg.Election.withDefaults().Threshold
}
