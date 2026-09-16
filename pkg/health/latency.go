package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"swarm-net/pkg/protocol"
)

// Prober performs one measurement against target and reports how long the peer took
// to answer.
//
// It exists so that LatencyHealthStrategy is testable without a network. The P2P
// socket mesh is Phase 3 work and does not exist yet; injecting the measurement keeps
// the statistics (EWMA, penalties, cancellation discrimination) implementable and
// fully testable today, and lets Phase 3 swap in the real transport by passing a
// different function rather than by rewriting this file.
//
// A Prober MUST honour ctx and return promptly when it is done. It MUST NOT retry
// internally: retries hide failures from the EWMA, which is the statistic that is
// supposed to notice them.
type Prober func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error)

// Tuning defaults. They are package-level constants rather than magic numbers inside
// the constructor so that the documentation can reference them by name.
const (
	// DefaultAlpha is the EWMA smoothing factor. 0.3 weights the newest sample at 30%
	// and the accumulated history at 70%, so a leader's score needs roughly three
	// consecutive bad probes to move decisively. That is the intended trade: fast
	// enough to react to a genuine degradation within a few probe intervals, slow
	// enough that one unlucky GC pause on the peer cannot cost it the leadership.
	DefaultAlpha = 0.3

	// DefaultProbeTimeout bounds a single probe. See Config.ProbeTimeout.
	DefaultProbeTimeout = 2 * time.Second

	// DefaultFailurePenalty multiplies ProbeTimeout to produce the sample recorded when
	// a probe fails. See Config.FailurePenalty for why a failure must record a sample
	// at all.
	DefaultFailurePenalty = 2.0
)

// Config configures a LatencyHealthStrategy. The zero Config is valid: every field
// falls back to its documented default.
type Config struct {
	// Probe performs one measurement. Defaults to TCPConnectProber(nil).
	Probe Prober

	// ProbeTimeout bounds a single probe, independently of the caller's context.
	//
	// This is not redundant with ctx. A caller is entitled to pass context.Background()
	// — the health package cannot force every call site to carry a deadline — and
	// without an internal bound such a probe blocks forever on a black-holed peer,
	// leaking the probing goroutine and, worse, never producing the ErrUnreachable that
	// would have got the peer evicted. The strategy derives its own timeout from the
	// caller's context, so the effective deadline is whichever of the two is nearer.
	//
	// Defaults to DefaultProbeTimeout.
	ProbeTimeout time.Duration

	// Alpha is the EWMA smoothing factor in (0, 1]. 1 means "no smoothing, use the last
	// sample". Defaults to DefaultAlpha. Out-of-range values fall back to the default
	// rather than panicking, because a misconfigured alpha must not take the node down.
	Alpha float64

	// FailurePenalty multiplies ProbeTimeout to produce the synthetic sample recorded
	// when a probe fails. Must be >= 1; lower values fall back to the default.
	//
	// It exists to close the coordinated-omission hole. See the comment on
	// LatencyHealthStrategy.
	//
	// Defaults to DefaultFailurePenalty.
	FailurePenalty float64
}

// LatencyHealthStrategy is the shipped default strategy: it scores a peer by the
// exponentially weighted moving average of its recent probe latencies, in
// milliseconds. Lower is better, per the HealthStrategy contract.
//
// # Why a moving average and not the last sample
//
// Scoring on the most recent sample makes leadership flap. Latency on a loopback
// bridge is a heavy-tailed distribution: a single sample is routinely 3–5x the median
// because of a GC pause, a scheduler hiccup, or a delayed ACK. With a raw last-sample
// score, whichever node happened to be probed during its quiet microsecond wins, the
// election re-runs on the next tick, and a different node wins. The swarm then spends
// its time re-clustering instead of doing work, and every flap costs reconnections.
//
// An EWMA is used rather than a median because it is O(1) in both time and memory per
// target and needs no sample window. A median over a sliding window is more robust to a
// single outlier — genuinely better statistics — but requires retaining k samples per
// peer and re-sorting them, and, more importantly, its rejection of outliers is exactly
// wrong at the moment that matters: a peer whose latency has just stepped up by 10x is
// a peer we want to demote *quickly*, and a median deliberately ignores the first few
// samples that say so. The EWMA's bias toward recent samples is the desired behaviour
// for a failure detector, even though it is the undesired behaviour for a metric
// dashboard.
//
// # Coordinated omission
//
// The hazard: a sample only exists when a probe *completes*. A peer that is slow enough
// to blow the probe timeout produces no measurement, so if timeouts are simply dropped,
// the slowest peer in the swarm contributes the fewest bad samples and its EWMA stays
// pinned to whatever it looked like when it was last healthy. The worse it gets, the
// better it looks. It then wins the election, and the whole cluster attaches to the one
// node that cannot answer.
//
// The fix: a failed probe records a synthetic sample of ProbeTimeout * FailurePenalty
// (see recordFailure) instead of recording nothing. The penalty exceeds the timeout so
// that a peer that times out repeatedly is ranked strictly worse than a peer that
// merely answers slowly but always answers — otherwise a peer pinned exactly at the
// timeout would tie with the honest slow one.
//
// Note that the *call* still returns ScoreUnavailable() and ErrUnreachable: this probe
// produced no valid measurement, so there is no valid score to report for it. The
// penalty is recorded in the target's history and shows up in the next successful
// probe's score. That separation keeps rule 3 of the contract intact — an error is
// never a number — while still letting failures accumulate.
//
// Cancelled probes are the exception: they record nothing at all, because they are not
// evidence about the peer. See classify.
//
// # Concurrency
//
// Safe for concurrent use by many goroutines against the same or different targets.
type LatencyHealthStrategy struct {
	probe          Prober
	probeTimeout   time.Duration
	alpha          float64
	failurePenalty float64

	// mu guards samples AND the *ewmaState values it points at. Every read and write of
	// an ewmaState happens under mu; the pointers never escape a critical section.
	//
	// A plain mutex-guarded map is used rather than sync.Map because every operation
	// here is a read-modify-write (load the EWMA, blend the new sample, store it).
	// sync.Map offers no atomic RMW, so it would need a per-entry mutex anyway — the
	// same lock, with an extra layer of indirection and worse escape analysis. sync.Map
	// is tuned for read-mostly, write-rarely maps; this one is written on every probe.
	//
	// The lock is never held across a probe. It is taken only for the arithmetic, which
	// is a handful of instructions, so contention at swarm scale (tens of peers, tens
	// of probing goroutines) is not a consideration. Holding it across the network call
	// would serialise every probe in the process behind the slowest peer — precisely
	// the peer we are trying to detect.
	mu      sync.Mutex
	samples map[protocol.NodeAddress]*ewmaState
}

// ewmaState is the per-target history. Guarded by LatencyHealthStrategy.mu.
type ewmaState struct {
	value float64 // current EWMA, in milliseconds
	count uint64  // samples folded in, for observability and first-sample seeding
}

// compile-time proof that the shipped default satisfies the contract.
var _ HealthStrategy = (*LatencyHealthStrategy)(nil)

// NewLatencyHealthStrategy builds a strategy from cfg. The zero Config is valid.
func NewLatencyHealthStrategy(cfg Config) *LatencyHealthStrategy {
	s := &LatencyHealthStrategy{
		probe:          cfg.Probe,
		probeTimeout:   cfg.ProbeTimeout,
		alpha:          cfg.Alpha,
		failurePenalty: cfg.FailurePenalty,
		samples:        make(map[protocol.NodeAddress]*ewmaState),
	}
	if s.probe == nil {
		s.probe = TCPConnectProber(nil)
	}
	if s.probeTimeout <= 0 {
		s.probeTimeout = DefaultProbeTimeout
	}
	// Out-of-range configuration degrades to the default instead of panicking: a bad
	// alpha is a tuning mistake, and a tuning mistake must not prevent the node from
	// joining the swarm.
	if !(s.alpha > 0 && s.alpha <= 1) {
		s.alpha = DefaultAlpha
	}
	if !(s.failurePenalty >= 1) {
		s.failurePenalty = DefaultFailurePenalty
	}
	return s
}

// Name implements HealthStrategy.
func (s *LatencyHealthStrategy) Name() string { return "latency" }

// EvaluateScore implements HealthStrategy. It probes target once and returns the
// updated EWMA of its latency in milliseconds — lower is better.
//
// On any error the returned score is ScoreUnavailable() (NaN) and must not be used.
// Classify the error with IsCancellation before treating it as a health signal.
func (s *LatencyHealthStrategy) EvaluateScore(ctx context.Context, target protocol.NodeAddress) (float64, error) {
	if target == "" {
		return ScoreUnavailable(), fmt.Errorf("%w: empty address", ErrInvalidTarget)
	}
	// Fail fast if the caller is already done. Without this, a probe launched during
	// shutdown still dials, still costs a file descriptor, and still has to be
	// classified — and on a fast-failing target it can even return a valid sample that
	// we would then record for a node we are in the middle of abandoning.
	if err := ctx.Err(); err != nil {
		return ScoreUnavailable(), fmt.Errorf("%w: %w", ErrProbeCanceled, err)
	}

	// Derive our own bound from the caller's context. WithTimeout takes the *earlier*
	// of the caller's deadline and ours, so the caller can always tighten the budget
	// and can never loosen it past ProbeTimeout. cancel is called unconditionally on
	// every path: the derived context holds a timer registered with the runtime, and
	// leaving it unreleased leaks that timer until the deadline fires — the classic
	// context leak, invisible at low rates and fatal at a probe every 500 ms per peer.
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()

	// The duration is measured by the Prober, not here, so that a Phase 3 mesh prober
	// can time the application-level PING/PONG rather than the transport handshake.
	// Whatever measures it must use a monotonic reading — see TCPConnectProber.
	rtt, err := s.probe(probeCtx, target)
	if err != nil {
		return s.classify(ctx, target, err)
	}
	// A prober that reports a negative duration is broken (a wall-clock subtraction
	// across an NTP step is the usual cause). Treat it as a failed probe rather than
	// feeding a negative sample into the EWMA, where it would make a broken peer the
	// most attractive leader in the swarm.
	if rtt < 0 {
		return s.classify(ctx, target, fmt.Errorf("prober returned negative duration %v", rtt))
	}

	return s.record(target, float64(rtt)/float64(time.Millisecond)), nil
}

// classify turns a probe error into the (score, error) pair the contract requires, and
// decides whether the failure is evidence about the peer.
//
// This is where the caller-cancellation / own-timeout ambiguity documented on
// IsCancellation is resolved. The discrimination is made from the PARENT context's
// state, never from the error value, because the caller's own deadline and our derived
// probeTimeout both surface as the identical context.DeadlineExceeded value and cannot
// be told apart by inspecting it.
//
//	parent ctx.Err() != nil  -> the caller stopped us. Not evidence. Record nothing.
//	parent ctx.Err() == nil  -> only our own budget can have expired, or the dial
//	                            genuinely failed. Evidence. Record a penalty.
//
// The race — caller cancels in the same instant our budget expires — resolves to
// "cancelled", because losing one sample costs a probe interval whereas a false missed
// beat costs an election.
func (s *LatencyHealthStrategy) classify(ctx context.Context, target protocol.NodeAddress, cause error) (float64, error) {
	if cerr := ctx.Err(); cerr != nil {
		// Shutdown or a superseded election round. Deliberately no recordFailure call:
		// polluting the EWMA here would mean every peer's score degrades on every clean
		// shutdown, and on restart the stale penalties would already be gone anyway —
		// so the only effect would be to mis-rank peers during the shutdown itself.
		//
		// Double %w so that both errors.Is(err, ErrProbeCanceled) and
		// errors.Is(err, context.Canceled) hold for callers who check either one.
		return ScoreUnavailable(), fmt.Errorf("%w: probing %s: %w", ErrProbeCanceled, target, cerr)
	}

	// The peer failed the probe. Record the penalty so the failure is visible in this
	// target's history — the coordinated-omission fix.
	s.recordFailure(target)

	if errorIsDeadline(cause) {
		// Our own budget, not the caller's: the parent is demonstrably still live.
		//
		// Note the %v on cause rather than %w. Wrapping context.DeadlineExceeded here
		// would make IsCancellation report true for this error, and a peer that never
		// answers would be classified as "we cancelled it" forever — the swarm would
		// never evict a dead node. The cause is preserved in the message, where it
		// helps a human, and kept out of the chain, where it would break the
		// classification.
		return ScoreUnavailable(), fmt.Errorf("%w: %w: %s did not answer within %v (%v)",
			ErrUnreachable, ErrProbeTimeout, target, s.probeTimeout, cause)
	}
	// Connection refused, host unknown, reset — an immediate, unambiguous failure.
	// Wrapped with %w because the underlying net error carries diagnostics a human
	// needs, and unlike a context error it cannot confuse IsCancellation.
	return ScoreUnavailable(), fmt.Errorf("%w: probing %s: %w", ErrUnreachable, target, cause)
}

// errorIsDeadline reports whether cause is a deadline/timeout failure, from either the
// context machinery or the net package. net.Dialer surfaces an expired DialContext
// deadline as a *net.OpError wrapping os.ErrDeadlineExceeded rather than as
// context.DeadlineExceeded, so both spellings must be recognised.
func errorIsDeadline(cause error) bool {
	if cause == nil {
		return false
	}
	var ne net.Error
	if errors.As(cause, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(cause, context.DeadlineExceeded)
}

// record folds one good sample (milliseconds) into target's EWMA and returns the new
// value.
//
// The standard recurrence, with the first sample seeding the average directly:
//
//	ewma = alpha*sample + (1-alpha)*ewma
//
// Seeding matters. Initialising to zero instead would make a brand-new peer's first
// score alpha*sample — roughly a third of its real latency — so every freshly joined
// node would look like the fastest node in the swarm and would win the next election
// on the strength of having no history. Membership churn would then drive leadership.
func (s *LatencyHealthStrategy) record(target protocol.NodeAddress, sampleMS float64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.samples[target]
	if !ok {
		st = &ewmaState{}
		s.samples[target] = st
	}
	if st.count == 0 {
		st.value = sampleMS
	} else {
		st.value = s.alpha*sampleMS + (1-s.alpha)*st.value
	}
	st.count++
	return st.value
}

// recordFailure folds the synthetic failure penalty into target's EWMA. See the
// coordinated-omission section on LatencyHealthStrategy for why a failure must record
// something rather than nothing.
func (s *LatencyHealthStrategy) recordFailure(target protocol.NodeAddress) {
	penaltyMS := float64(s.probeTimeout) / float64(time.Millisecond) * s.failurePenalty
	s.record(target, penaltyMS)
}

// Score returns the current EWMA for target without probing, and whether any sample
// has been recorded for it.
//
// It exists for telemetry and for tests. Ranking code must call EvaluateScore: this
// method reports history, and history with no fresh probe behind it is exactly how a
// dead peer keeps its old good score.
func (s *LatencyHealthStrategy) Score(target protocol.NodeAddress) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.samples[target]
	if !ok {
		return ScoreUnavailable(), false
	}
	return st.value, true
}

// Samples returns how many samples (successful probes plus recorded failures) have been
// folded into target's EWMA.
func (s *LatencyHealthStrategy) Samples(target protocol.NodeAddress) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.samples[target]; ok {
		return st.count
	}
	return 0
}

// Forget drops all history for target.
//
// The samples map is keyed by peer address and would otherwise grow for the lifetime of
// the process: $N$ is dynamic, peers leave, and the chaos controls restart containers
// which come back on recycled IPs under new addresses. Without eviction a long-running
// node accumulates an entry per address ever seen — a slow leak, and worse, a source of
// stale scores if an address is ever reused by a different container.
//
// Membership logic owns the lifecycle and must call Forget when a peer leaves the view.
// The strategy deliberately does not expire entries on a timer of its own: it has no
// membership knowledge, and a timer would be a second, competing opinion about who is
// in the swarm.
func (s *LatencyHealthStrategy) Forget(target protocol.NodeAddress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.samples, target)
}

// Retain drops history for every target not present in keep, in one pass.
//
// This is the form membership logic actually wants after a view change: it holds the
// new member set, not the diff. Doing it here keeps the map bounded by live membership
// by construction, rather than by the caller remembering to pair every departure with a
// Forget.
func (s *LatencyHealthStrategy) Retain(keep map[protocol.NodeAddress]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for addr := range s.samples {
		if _, ok := keep[addr]; !ok {
			delete(s.samples, addr)
		}
	}
}

// TCPConnectProber returns a Prober that measures how long it takes to complete a TCP
// connection to the target, using dialer (nil means a zero net.Dialer).
//
// # This is a placeholder, and the difference matters
//
// Connect time is NOT application round-trip time, and Phase 3 replaces this with a
// PING/PONG over the established mesh connection. The TCP handshake is completed by the
// peer's KERNEL: the SYN is answered with SYN-ACK from the network stack, with no
// involvement from the peer's Go process at all. A node whose scheduler is saturated,
// whose goroutines are starved, or which is stopped in a GC pause still completes a
// handshake in microseconds — its listen backlog absorbs the connection and the
// application never runs.
//
// So this prober measures network distance while reporting nothing about application
// responsiveness, and application responsiveness is precisely what leader selection
// needs: a leader must schedule goroutines to fan tasks out, not merely reply to SYNs.
// Used as the only signal, it would happily elect a node that is comatose above the
// socket layer. It is shipped because it is correct for what it measures, needs no
// mesh, and gives Phase 2 a working default; a PING/PONG that traverses the peer's
// accept loop, scheduler and encoder measures the thing that matters.
//
// It also costs a connection per probe — a handshake, a file descriptor, and a socket
// in TIME_WAIT afterwards. At one probe per peer per interval that is affordable; it is
// another reason the real implementation reuses the mesh connection.
//
// Monotonic clocks: the elapsed time is computed with time.Since on a time.Time
// captured by time.Now. Both carry Go's monotonic clock reading, and time.Since
// subtracts monotonically, so the result is immune to wall-clock steps — an NTP
// correction mid-probe would otherwise produce a negative or wildly inflated sample and
// silently corrupt the EWMA. For the same reason a peer's own timestamp is never used
// to compute a duration anywhere in this package: two machines' wall clocks have no
// defined relationship, and their monotonic clocks are incomparable by construction.
func TCPConnectProber(dialer *net.Dialer) Prober {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		start := time.Now()
		// DialContext, not Dial: it aborts the in-flight handshake when ctx ends. Dial
		// with a Timeout field would ignore the context entirely and keep the socket
		// alive past a shutdown.
		conn, err := dialer.DialContext(ctx, "tcp", string(target))
		if err != nil {
			return 0, err
		}
		elapsed := time.Since(start)
		// Closed immediately and before returning: the measurement is complete, and an
		// unclosed conn here would leak a file descriptor per probe. Close's error is
		// deliberately ignored — nothing was written, so there is no flush to fail, and
		// a close error says nothing about the peer's health.
		_ = conn.Close()
		return elapsed, nil
	}
}
