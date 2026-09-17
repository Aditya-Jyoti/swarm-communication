package health

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"swarm-net/pkg/protocol"
)

// --- deterministic test probers -------------------------------------------------
//
// Every prober here is synchronous and returns immediately. There is no time.Sleep
// anywhere in this file: sleeping to "let something happen" is how a test suite
// acquires flakes, and a latency package whose own tests are timing-dependent has no
// standing to lecture anyone about failure detection. The injectable Prober seam exists
// precisely so that the statistics can be tested without a clock or a socket.

// fixedProber always reports the same latency in milliseconds.
func fixedProber(ms float64) Prober {
	return func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		return time.Duration(ms * float64(time.Millisecond)), nil
	}
}

// sequenceProber walks a list of millisecond samples, repeating the last one forever.
func sequenceProber(ms ...float64) Prober {
	var mu sync.Mutex
	i := 0
	return func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		mu.Lock()
		defer mu.Unlock()
		v := ms[i]
		if i < len(ms)-1 {
			i++
		}
		return time.Duration(v * float64(time.Millisecond)), nil
	}
}

// deadlineProber simulates the strategy's own per-probe budget expiring: the real
// prober observes probeCtx being done and returns its error. Returning it directly is
// exactly what a real prober does, minus the waiting.
func deadlineProber() Prober {
	return func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		return 0, context.DeadlineExceeded
	}
}

// refusedProber simulates ECONNREFUSED: an immediate, non-timeout network failure.
func refusedProber() Prober {
	return func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		return 0, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
}

// errProber returns a fixed error.
func errProber(err error) Prober {
	return func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		return 0, err
	}
}

const eps = 1e-9

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// --- EWMA behaviour --------------------------------------------------------------

func TestEWMAFirstSampleSeedsDirectly(t *testing.T) {
	// The first sample must become the score verbatim. Blending against an implicit
	// zero would make every newly joined peer look ~alpha times faster than it is, and
	// membership churn would then decide leadership.
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(10), Alpha: 0.3})
	score, err := s.EvaluateScore(context.Background(), "a:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	approx(t, score, 10, "first score")
}

func TestEWMAConvergesOverKnownSequence(t *testing.T) {
	const alpha = 0.5
	samples := []float64{10, 20, 30, 40}
	s := NewLatencyHealthStrategy(Config{Probe: sequenceProber(samples...), Alpha: alpha})

	// Hand-computed: seed 10; then 0.5*20+0.5*10=15; 0.5*30+0.5*15=22.5;
	// 0.5*40+0.5*22.5=31.25.
	want := []float64{10, 15, 22.5, 31.25}
	for i := range samples {
		got, err := s.EvaluateScore(context.Background(), "a:9000")
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		approx(t, got, want[i], "ewma after sample")
	}
	if n := s.Samples("a:9000"); n != uint64(len(samples)) {
		t.Fatalf("Samples = %d, want %d", n, len(samples))
	}

	// And it converges toward a steady input rather than oscillating with it.
	s2 := NewLatencyHealthStrategy(Config{Probe: sequenceProber(100, 5), Alpha: alpha})
	var last float64
	for i := 0; i < 40; i++ {
		last, _ = s2.EvaluateScore(context.Background(), "b:9000")
	}
	approx(t, math.Round(last*1e6)/1e6, 5, "converged ewma")
}

func TestEWMADampensASingleOutlier(t *testing.T) {
	// The anti-flap property: one bad sample must not be able to move the score far
	// enough to lose an election on its own.
	const alpha = 0.3
	s := NewLatencyHealthStrategy(Config{Probe: sequenceProber(10, 10, 10, 200), Alpha: alpha})
	var score float64
	for i := 0; i < 4; i++ {
		score, _ = s.EvaluateScore(context.Background(), "a:9000")
	}
	// 0.3*200 + 0.7*10 = 67, not 200.
	approx(t, score, 67, "score after outlier")
	if score >= 200 {
		t.Fatal("outlier was not dampened at all")
	}
}

func TestAlphaOneDisablesSmoothing(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: sequenceProber(10, 99), Alpha: 1})
	_, _ = s.EvaluateScore(context.Background(), "a:9000")
	score, _ := s.EvaluateScore(context.Background(), "a:9000")
	approx(t, score, 99, "alpha=1 score")
}

func TestInvalidConfigFallsBackToDefaults(t *testing.T) {
	// A tuning mistake must not panic or produce nonsense; it degrades to the default.
	for _, alpha := range []float64{0, -1, 1.5, math.NaN()} {
		s := NewLatencyHealthStrategy(Config{Probe: fixedProber(1), Alpha: alpha})
		if s.alpha != DefaultAlpha {
			t.Errorf("alpha %v: got %v, want default %v", alpha, s.alpha, DefaultAlpha)
		}
	}
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(1), ProbeTimeout: -1, FailurePenalty: 0.5})
	if s.probeTimeout != DefaultProbeTimeout {
		t.Errorf("probeTimeout = %v, want %v", s.probeTimeout, DefaultProbeTimeout)
	}
	if s.failurePenalty != DefaultFailurePenalty {
		t.Errorf("failurePenalty = %v, want %v", s.failurePenalty, DefaultFailurePenalty)
	}
}

func TestZeroConfigIsUsable(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{})
	if s.probe == nil {
		t.Fatal("zero Config must install the default prober")
	}
	if s.Name() != "latency" {
		t.Fatalf("Name() = %q", s.Name())
	}
}

// --- failure semantics ------------------------------------------------------------

func TestUnreachableReturnsNaNAndSentinel(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: refusedProber()})
	score, err := s.EvaluateScore(context.Background(), "dead:9000")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if IsCancellation(err) {
		t.Fatalf("a refused connection must not classify as a cancellation: %v", err)
	}
	if !math.IsNaN(score) {
		t.Fatalf("score = %v, want NaN", score)
	}
	// The property that makes the NaN worth choosing.
	if score < 1.0 {
		t.Fatal("unavailable score won a naive `<` comparison")
	}
}

func TestInvalidTarget(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(1)})
	score, err := s.EvaluateScore(context.Background(), "")
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("err = %v, want ErrInvalidTarget", err)
	}
	if !math.IsNaN(score) {
		t.Fatalf("score = %v, want NaN", score)
	}
	if IsCancellation(err) {
		t.Fatal("a caller bug must not be classified as a cancellation")
	}
	if s.Samples("") != 0 {
		t.Fatal("an invalid target must not create map state")
	}
}

func TestNegativeDurationIsRejected(t *testing.T) {
	// A wall-clock subtraction across an NTP step is the usual source. Feeding it into
	// the EWMA would make a broken peer the most attractive leader in the swarm.
	bad := func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		return -5 * time.Millisecond, nil
	}
	s := NewLatencyHealthStrategy(Config{Probe: bad})
	score, err := s.EvaluateScore(context.Background(), "skewed:9000")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if !math.IsNaN(score) {
		t.Fatalf("score = %v, want NaN", score)
	}
	if got, _ := s.Score("skewed:9000"); got < 0 {
		t.Fatalf("negative sample reached the EWMA: %v", got)
	}
}

// --- coordinated omission ----------------------------------------------------------

func TestTimeoutDegradesScoreRatherThanBeingOmitted(t *testing.T) {
	// The hazard: if a timed-out probe recorded nothing, the slowest peer in the swarm
	// would contribute the fewest bad samples and would keep whatever score it had when
	// it was last healthy — getting more attractive the sicker it got.
	const timeout = 100 * time.Millisecond
	seq := []Prober{fixedProber(10), deadlineProber(), deadlineProber(), fixedProber(10)}
	i := 0
	chained := func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		p := seq[i]
		i++
		return p(ctx, target)
	}
	sick := NewLatencyHealthStrategy(Config{
		Probe: chained, Alpha: 0.5, ProbeTimeout: timeout, FailurePenalty: 2,
	})
	healthy := NewLatencyHealthStrategy(Config{
		Probe: fixedProber(10), Alpha: 0.5, ProbeTimeout: timeout, FailurePenalty: 2,
	})

	for n := 0; n < 4; n++ {
		_, _ = sick.EvaluateScore(context.Background(), "sick:9000")
		_, _ = healthy.EvaluateScore(context.Background(), "healthy:9000")
	}

	sickScore, ok := sick.Score("sick:9000")
	if !ok {
		t.Fatal("no history recorded for the timing-out peer: samples were omitted")
	}
	healthyScore, _ := healthy.Score("healthy:9000")

	if sick.Samples("sick:9000") != 4 {
		t.Fatalf("Samples = %d, want 4: a timeout must record a sample",
			sick.Samples("sick:9000"))
	}
	// Penalty is 200ms per timeout: seed 10; 0.5*200+0.5*10=105; 0.5*200+0.5*105=152.5;
	// 0.5*10+0.5*152.5=81.25.
	approx(t, sickScore, 81.25, "sick score")
	if !Better(healthyScore, sickScore) {
		t.Fatalf("timing-out peer (%v) not ranked worse than healthy peer (%v)",
			sickScore, healthyScore)
	}
}

func TestFailurePenaltyExceedsProbeTimeout(t *testing.T) {
	// A peer pinned exactly at the timeout must rank strictly worse than an honest slow
	// peer that always answers just under it, so the penalty has to exceed the budget.
	const timeout = 100 * time.Millisecond
	timingOut := NewLatencyHealthStrategy(Config{
		Probe: deadlineProber(), Alpha: 1, ProbeTimeout: timeout, FailurePenalty: 2,
	})
	honestSlow := NewLatencyHealthStrategy(Config{
		Probe: fixedProber(99), Alpha: 1, ProbeTimeout: timeout,
	})
	_, _ = timingOut.EvaluateScore(context.Background(), "a:9000")
	_, _ = honestSlow.EvaluateScore(context.Background(), "b:9000")

	bad, _ := timingOut.Score("a:9000")
	ok, _ := honestSlow.Score("b:9000")
	if !Better(ok, bad) {
		t.Fatalf("honest slow peer (%v) did not beat the timing-out peer (%v)", ok, bad)
	}
}

// --- cancellation discrimination ---------------------------------------------------

func TestPreCancelledContextIsCancellationAndRecordsNothing(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(10)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	score, err := s.EvaluateScore(ctx, "a:9000")
	if !IsCancellation(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if !errors.Is(err, ErrProbeCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want both ErrProbeCanceled and context.Canceled in the chain", err)
	}
	if !math.IsNaN(score) {
		t.Fatalf("score = %v, want NaN", score)
	}
	if s.Samples("a:9000") != 0 {
		t.Fatal("a cancelled probe polluted the EWMA")
	}
}

func TestCancellationMidProbeRecordsNothing(t *testing.T) {
	// Shutdown arrives while the probe is in flight. Deterministic: the prober itself
	// performs the cancellation, so there is no race to sleep on.
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(10), Alpha: 1})
	if _, err := s.EvaluateScore(context.Background(), "a:9000"); err != nil {
		t.Fatalf("warm-up probe: %v", err)
	}
	before, _ := s.Score("a:9000")

	ctx, cancel := context.WithCancel(context.Background())
	s.probe = func(pctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
		cancel()             // the caller shuts down mid-probe
		return 0, pctx.Err() // which the prober observes on the derived context
	}

	score, err := s.EvaluateScore(ctx, "a:9000")
	if !IsCancellation(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if !math.IsNaN(score) {
		t.Fatalf("score = %v, want NaN", score)
	}
	if s.Samples("a:9000") != 1 {
		t.Fatalf("Samples = %d, want 1: the cancelled probe recorded a sample",
			s.Samples("a:9000"))
	}
	if after, _ := s.Score("a:9000"); after != before {
		t.Fatalf("score moved from %v to %v on a cancelled probe", before, after)
	}
}

func TestCallerDeadlineIsCancellationNotUnreachable(t *testing.T) {
	// The subtle case: the CALLER's own deadline expires. The derived context reports
	// context.DeadlineExceeded — the identical value our own probe budget produces — so
	// the discrimination cannot come from the error and must come from the parent's
	// state. Here the parent is done, so this is shutdown-class, not peer-class.
	s := NewLatencyHealthStrategy(Config{Probe: deadlineProber(), ProbeTimeout: time.Hour})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := s.EvaluateScore(ctx, "a:9000")
	if !IsCancellation(err) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Fatalf("caller's expired deadline was blamed on the peer: %v", err)
	}
	if s.Samples("a:9000") != 0 {
		t.Fatal("the caller's expired deadline degraded the peer's score")
	}
}

func TestOwnProbeTimeoutIsUnreachableNotCancellation(t *testing.T) {
	// The mirror image of the test above, with the same underlying error value: the
	// parent is live, so the only thing that can have expired is our own budget, and
	// that IS evidence about the peer.
	s := NewLatencyHealthStrategy(Config{Probe: deadlineProber(), ProbeTimeout: time.Hour})
	_, err := s.EvaluateScore(context.Background(), "a:9000")
	if IsCancellation(err) {
		t.Fatalf("own probe timeout classified as a cancellation: %v", err)
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if s.Samples("a:9000") != 1 {
		t.Fatal("own probe timeout did not record a penalty sample")
	}
}

func TestProbeTimeoutBoundsABackgroundContext(t *testing.T) {
	// The caller may pass context.Background(); the probe must still be bounded. The
	// prober here asserts that a deadline was installed on the context it received.
	var gotDeadline bool
	var budget time.Duration
	s := NewLatencyHealthStrategy(Config{
		ProbeTimeout: 250 * time.Millisecond,
		Probe: func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
			dl, ok := ctx.Deadline()
			gotDeadline = ok
			budget = time.Until(dl)
			return time.Millisecond, nil
		},
	})
	if _, err := s.EvaluateScore(context.Background(), "a:9000"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotDeadline {
		t.Fatal("no deadline installed: a probe from context.Background() is unbounded")
	}
	if budget <= 0 || budget > 250*time.Millisecond {
		t.Fatalf("derived budget %v, want (0, 250ms]", budget)
	}
}

func TestCallerDeadlineTightensButCannotLoosen(t *testing.T) {
	var budget time.Duration
	s := NewLatencyHealthStrategy(Config{
		ProbeTimeout: time.Hour,
		Probe: func(ctx context.Context, target protocol.NodeAddress) (time.Duration, error) {
			dl, _ := ctx.Deadline()
			budget = time.Until(dl)
			return time.Millisecond, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.EvaluateScore(ctx, "a:9000"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if budget > 50*time.Millisecond {
		t.Fatalf("derived budget %v exceeds the caller's 50ms deadline", budget)
	}
}

func TestNetErrorTimeoutClassifiesAsUnreachable(t *testing.T) {
	// net.Dialer surfaces an expired DialContext deadline as a *net.OpError wrapping
	// os.ErrDeadlineExceeded, not as context.DeadlineExceeded. Both spellings must be
	// recognised as our-budget-expired, and neither may read as a cancellation.
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errTimeout{}}
	s := NewLatencyHealthStrategy(Config{Probe: errProber(opErr)})
	_, err := s.EvaluateScore(context.Background(), "a:9000")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if IsCancellation(err) {
		t.Fatalf("a net timeout was classified as a cancellation: %v", err)
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

// --- map lifecycle ------------------------------------------------------------------

func TestForgetAndRetainBoundTheMap(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(5)})
	for _, a := range []protocol.NodeAddress{"a:1", "b:1", "c:1"} {
		if _, err := s.EvaluateScore(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	s.Forget("a:1")
	if _, ok := s.Score("a:1"); ok {
		t.Fatal("Forget did not drop the entry")
	}
	s.Retain(map[protocol.NodeAddress]struct{}{"b:1": {}})
	if _, ok := s.Score("c:1"); ok {
		t.Fatal("Retain did not evict a departed peer")
	}
	if _, ok := s.Score("b:1"); !ok {
		t.Fatal("Retain evicted a live peer")
	}
	if got, ok := s.Score("gone:1"); ok || !math.IsNaN(got) {
		t.Fatalf("Score for an unknown peer = (%v, %v), want (NaN, false)", got, ok)
	}
}

// --- concurrency ---------------------------------------------------------------------

func TestConcurrentEvaluateScore(t *testing.T) {
	// Run under -race. Many worker goroutines probe many leaders at once, which is the
	// real access pattern: contention on the same target and on different targets
	// simultaneously.
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(7), Alpha: 0.3})
	targets := []protocol.NodeAddress{"a:1", "b:1", "c:1", "shared:1"}

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				// Half the goroutines hammer one shared target; the rest spread out.
				target := targets[len(targets)-1]
				if g%2 == 0 {
					target = targets[i%(len(targets)-1)]
				}
				if _, err := s.EvaluateScore(context.Background(), target); err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				_, _ = s.Score(target)
				_ = s.Samples(target)
			}
		}(g)
	}
	wg.Wait()

	// With a constant prober every score must equal the sample regardless of interleaving.
	for _, target := range targets {
		got, ok := s.Score(target)
		if !ok {
			t.Fatalf("no score for %s", target)
		}
		approx(t, got, 7, string(target)+" score")
	}
	// Exactly 16 goroutines * 50 iterations landed on the shared target; no lost updates.
	if n := s.Samples("shared:1"); n != 16*50 {
		t.Fatalf("Samples(shared) = %d, want %d", n, 16*50)
	}
}

func TestConcurrentForgetAndEvaluate(t *testing.T) {
	// Eviction races probing in production: membership changes while probes are in
	// flight. This must be race-clean and must not panic on a deleted entry.
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(7)})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = s.EvaluateScore(context.Background(), "a:1")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.Forget("a:1")
			s.Retain(map[protocol.NodeAddress]struct{}{})
		}
	}()
	wg.Wait()
}

// --- the shipped default prober -------------------------------------------------------

func TestTCPConnectProberMeasuresARealConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	defer ln.Close()
	// Accept and immediately close, in a goroutine owned by this test and terminated by
	// the listener's Close in the deferred call above: Accept returns an error once the
	// listener is closed, so the goroutine cannot outlive the test.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	s := NewLatencyHealthStrategy(Config{ProbeTimeout: 5 * time.Second})
	score, err := s.EvaluateScore(context.Background(), protocol.NodeAddress(ln.Addr().String()))
	if err != nil {
		t.Fatalf("probing a live listener: %v", err)
	}
	// No upper bound is asserted: this is a real connect and a bound would be a flake.
	// Non-negative and finite is the only honest claim.
	if !IsValidScore(score) || score < 0 {
		t.Fatalf("score = %v, want a finite non-negative measurement", score)
	}
}

func TestTCPConnectProberOnADeadPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing is listening now: connect is refused

	s := NewLatencyHealthStrategy(Config{ProbeTimeout: 2 * time.Second})
	score, err := s.EvaluateScore(context.Background(), protocol.NodeAddress(addr))
	if err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
	if IsCancellation(err) {
		t.Fatalf("a refused connection classified as a cancellation: %v", err)
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if !math.IsNaN(score) {
		t.Fatalf("score = %v, want NaN", score)
	}
}
