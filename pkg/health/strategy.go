package health

import (
	"context"
	"errors"
	"math"

	"swarm-net/pkg/protocol"
)

// HealthStrategy evaluates how good a peer is, as a *cost*.
//
// This is the seam that makes the swarm's ranking logic pluggable: pkg/cluster
// ranks peers by calling EvaluateScore and comparing the results, and it never
// looks at what a score means. LatencyHealthStrategy ships as the default; a CPU,
// free-memory, packet-loss or composite strategy must drop in without pkg/cluster
// learning anything new.
//
// Four properties of this contract are load-bearing. Implementers who break any of
// them will produce a swarm that elects the wrong leader, and will do it silently.
//
// # 1. LOWER IS BETTER. The score is a cost, not a fitness.
//
// A score of 0.4 beats a score of 12.0. This is the single assumption most likely to
// be inverted by accident, and inverting it elects the *worst* node as leader — a bug
// that looks like "the cluster is just slow" rather than like a bug. Use Better to
// compare rather than writing a bare `<`, so the direction is stated once, here, and
// never re-derived at each call site.
//
// A capacity-style metric (free memory, idle CPU, spare connection slots) must be
// inverted by the strategy before it is returned. That inversion belongs inside the
// strategy, because the strategy is the only code that knows the metric's units.
//
// # 2. Scores are only comparable within one strategy instance.
//
// LatencyHealthStrategy returns milliseconds. A CPU strategy may return a 0–1 load
// fraction. Both are valid. Neither is convertible into the other, and
//
//	latencyStrategy.EvaluateScore(ctx, a) < cpuStrategy.EvaluateScore(ctx, b)
//
// is meaningless — it compares milliseconds against a fraction and will reliably pick
// the CPU-scored node, because 0.3 is always less than 4.1 ms-expressed-as-4.1. Any
// code that assumes "score is milliseconds" is a bug against this contract. Rank a
// candidate set with exactly one strategy instance, the same one, for every candidate.
//
// There is deliberately no normalisation step. Normalising to 0–1 requires knowing the
// range in advance, and the range of a latency distribution is not knowable in advance
// — a normaliser calibrated on a healthy swarm silently compresses the exact tail that
// election is supposed to detect.
//
// # 3. An error is NOT a score of zero.
//
// "Unreachable" and "excellent" are different facts and must never collapse into the
// same float. The (float64, error) signature exists to keep them apart; implementations
// must not defeat it by returning a magic sentinel float like 0 or 999999.
//
// On error, an implementation returns ScoreUnavailable() — a NaN. Callers MUST check
// err before reading the float, but the NaN is a second line of defence for the caller
// who forgets: NaN compares false against everything, so `nan < best` is false and a
// NaN score can never win a `<` ranking. A zero would have won every ranking, making a
// dead node the most attractive leader in the swarm. That is not a trick; it is the
// cheapest available way to make the ignore-the-error bug fail safe. See Better, which
// rejects NaN explicitly rather than relying on the comparison's accident.
//
// # 4. A cancelled probe is not an unhealthy peer.
//
// When the process shuts down, every in-flight probe is cancelled at once. If the
// caller records those as failures, a clean shutdown manufactures a swarm-wide burst of
// missed heartbeats and triggers a spurious re-election of a cluster that was fine.
// Callers MUST route every probe error through IsCancellation first and drop the sample
// when it returns true — no missed-beat counter increment, no eviction, no election.
// Only errors for which IsCancellation is false (notably ErrUnreachable) are statements
// about the peer.
//
// # Implementation requirements
//
//   - EvaluateScore must be safe for concurrent use. Many worker goroutines probe many
//     leaders at once; per-target state must be synchronised.
//   - EvaluateScore must honour ctx and must additionally bound itself, so that a caller
//     passing context.Background() still cannot hang a goroutine forever.
//   - Name must be stable and cheap; it is used in telemetry and in logs that explain
//     why a leader was chosen.
type HealthStrategy interface {
	// Name identifies the strategy in logs and telemetry, e.g. "latency".
	Name() string

	// EvaluateScore probes target and returns its cost (lower is better).
	//
	// On any error the returned float is ScoreUnavailable() and must not be used.
	// Callers must classify the error with IsCancellation before treating it as a
	// health signal about target.
	EvaluateScore(ctx context.Context, target protocol.NodeAddress) (float64, error)
}

// Sentinel errors. These are the conditions callers are expected to branch on; every
// other failure is wrapped and reported but not distinguished.
var (
	// ErrUnreachable means the probe ran to completion and the peer failed it: the
	// connection was refused, reset, or the probe's own deadline elapsed. This IS a
	// health signal. It is the only error class that may increment a missed-beat
	// counter or trigger eviction.
	ErrUnreachable = errors.New("health: target unreachable")

	// ErrProbeTimeout means the strategy's own per-probe deadline elapsed before the
	// peer answered. It is wrapped by ErrUnreachable, because from the swarm's point of
	// view a peer that does not answer within the probe budget is unreachable.
	//
	// Note carefully what this error does NOT wrap: context.DeadlineExceeded. That
	// omission is deliberate and is the crux of the cancellation discrimination — see
	// IsCancellation. Wrapping it would make IsCancellation report true for a genuine
	// peer failure, and the swarm would stop detecting dead nodes entirely.
	ErrProbeTimeout = errors.New("health: probe deadline exceeded")

	// ErrProbeCanceled means the CALLER's context ended — shutdown, a superseded
	// election round, a caller-imposed deadline covering a whole batch of probes. It
	// says nothing about the peer. It wraps the underlying context error, so both
	// errors.Is(err, ErrProbeCanceled) and errors.Is(err, context.Canceled) hold.
	ErrProbeCanceled = errors.New("health: probe canceled by caller")

	// ErrInvalidTarget means the address was empty or otherwise unusable. It is a
	// programming error in the caller, not a fact about any peer, and must never be
	// counted as a missed beat.
	ErrInvalidTarget = errors.New("health: invalid target address")
)

// ScoreUnavailable is the score returned alongside every error: a NaN.
//
// It is a function rather than a package-level var because a var is writable, and a
// single stray assignment elsewhere in the program would turn every failed probe into
// whatever value was assigned — most likely zero, i.e. "perfect health for every dead
// node". A func cannot be reassigned.
//
// NaN was chosen over 0 (wins every ranking, catastrophic), over math.Inf(1) (loses
// every ranking, which is correct but silently supplies a *valid* comparable number, so
// an ignored error stays invisible forever) and over a magic large constant (arbitrary,
// and comparable against a genuinely large real score). NaN poisons arithmetic: a NaN
// that leaks into an average makes the average NaN, so the bug surfaces loudly at the
// first aggregation instead of quietly skewing a leader election.
func ScoreUnavailable() float64 { return math.NaN() }

// IsValidScore reports whether score is usable for ranking: finite, and not NaN.
//
// +Inf is rejected as well as NaN. A strategy that returns +Inf has expressed "worst
// possible" rather than a measurement, and admitting it into an EWMA or an average
// would make every subsequent aggregate +Inf.
func IsValidScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0)
}

// Better reports whether score a is strictly better than score b, under the
// lower-is-better contract.
//
// Use this instead of `a < b`. It states the direction in exactly one place, and it
// treats invalid scores explicitly rather than depending on NaN's comparison behaviour
// being remembered correctly by whoever writes the next ranking loop: an invalid a is
// never better than anything, and a valid a is always better than an invalid b.
//
// Both invalid is reported as "not better", which makes ranking a set of entirely
// unreachable peers stable rather than dependent on iteration order.
func Better(a, b float64) bool {
	switch {
	case !IsValidScore(a):
		return false
	case !IsValidScore(b):
		return true
	default:
		return a < b
	}
}

// IsCancellation reports whether err means "we stopped this probe", as opposed to
// "the peer failed it".
//
// Callers MUST consult this before treating any probe error as a health signal:
//
//	score, err := strategy.EvaluateScore(ctx, peer)
//	switch {
//	case health.IsCancellation(err):
//	    return // shutdown or superseded round: record nothing, elect nothing
//	case err != nil:
//	    missedBeats[peer]++ // a real statement about the peer
//	default:
//	    record(peer, score)
//	}
//
// Getting this wrong is not a small bug. Every probe in the process is cancelled at the
// same instant during shutdown, so conflating the two classes turns a clean shutdown
// into a synchronised swarm-wide burst of "missed heartbeat", which is precisely the
// signature of a network partition, which triggers a re-election of a healthy cluster
// while it is trying to stop.
//
// # The ambiguity this resolves
//
// A strategy bounds each probe with context.WithTimeout(ctx, probeTimeout). Three
// different events can end the derived context, and two of them produce the *identical*
// error value, context.DeadlineExceeded:
//
//	(a) caller cancels ctx            -> derived ctx.Err() == context.Canceled
//	(b) the caller's OWN deadline hits -> derived ctx.Err() == context.DeadlineExceeded
//	(c) our probeTimeout hits          -> derived ctx.Err() == context.DeadlineExceeded
//
// (a) and (b) are the caller stopping us. (c) is the peer being too slow — the health
// signal the whole package exists to produce. Distinguishing (b) from (c) by inspecting
// the returned error is impossible, because the value is the same object.
//
// So the discrimination is not made from the error value. It is made by inspecting the
// PARENT context: at the moment a probe fails, if parent ctx.Err() != nil the caller
// stopped us — case (a) or (b) — and the strategy returns ErrProbeCanceled; otherwise
// the parent is still live and the only thing that can have expired is our own budget,
// so it is case (c) and the strategy returns ErrProbeTimeout wrapped in ErrUnreachable.
// See LatencyHealthStrategy.classify.
//
// Two deliberate consequences:
//
//   - ErrProbeTimeout does not wrap context.DeadlineExceeded. If it did, this function
//     would report true for a genuinely dead peer and the swarm would never evict
//     anyone. The underlying cause is preserved in the error's text, not its chain.
//   - There is an unavoidable race: the caller may cancel in the same microsecond our
//     own budget expires. That is resolved in favour of ErrProbeCanceled, i.e. in
//     favour of discarding the sample. Losing one sample costs a probe interval; a
//     false missed beat costs an election.
func IsCancellation(err error) bool {
	if err == nil {
		return false
	}
	// ErrProbeCanceled wraps the context error, so the first clause is usually
	// redundant. It is kept so that a strategy which returns a bare context error —
	// or a future strategy that forgets the sentinel — is still classified correctly.
	// Under-reporting here is the expensive direction.
	return errors.Is(err, ErrProbeCanceled) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
