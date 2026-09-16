package health

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"swarm-net/pkg/protocol"
)

// constantStrategy is a deliberately trivial second implementation standing in for a
// future CPU-load strategy: it reports a fixed 0–1 load fraction and never touches the
// network.
//
// Its real job in this file is the compile-time assertion below. The whole thesis of
// this package is that pkg/cluster can rank peers without knowing what a score means;
// a second implementation whose units are a fraction rather than milliseconds is the
// cheapest proof that the contract does not secretly assume latency.
type constantStrategy struct {
	name string
	load float64
	err  error
}

var _ HealthStrategy = (*constantStrategy)(nil)

func (c *constantStrategy) Name() string { return c.name }

func (c *constantStrategy) EvaluateScore(ctx context.Context, target protocol.NodeAddress) (float64, error) {
	if err := ctx.Err(); err != nil {
		return ScoreUnavailable(), fmt.Errorf("%w: %w", ErrProbeCanceled, err)
	}
	if c.err != nil {
		return ScoreUnavailable(), c.err
	}
	return c.load, nil
}

func TestSubstitutability(t *testing.T) {
	// Both shipped and stand-in strategies satisfy the interface, and cluster-style
	// code can hold either behind the interface without a type switch.
	strategies := []HealthStrategy{
		NewLatencyHealthStrategy(Config{Probe: fixedProber(1)}),
		&constantStrategy{name: "cpu", load: 0.25},
	}
	for _, s := range strategies {
		if s.Name() == "" {
			t.Errorf("strategy has empty Name()")
		}
		score, err := s.EvaluateScore(context.Background(), "peer:9000")
		if err != nil {
			t.Errorf("%s: unexpected error: %v", s.Name(), err)
		}
		if !IsValidScore(score) {
			t.Errorf("%s: invalid score %v", s.Name(), score)
		}
	}
}

func TestScoreUnavailableIsNaN(t *testing.T) {
	if !math.IsNaN(ScoreUnavailable()) {
		t.Fatalf("ScoreUnavailable() = %v, want NaN", ScoreUnavailable())
	}
	if IsValidScore(ScoreUnavailable()) {
		t.Fatal("ScoreUnavailable() must not be a valid score")
	}
}

func TestIsValidScore(t *testing.T) {
	cases := []struct {
		in   float64
		want bool
	}{
		{0, true},
		{1.5, true},
		{-1, true}, // negative is odd but not structurally invalid; the strategy rejects it
		{math.NaN(), false},
		{math.Inf(1), false},
		{math.Inf(-1), false},
	}
	for _, c := range cases {
		if got := IsValidScore(c.in); got != c.want {
			t.Errorf("IsValidScore(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBetterEncodesLowerIsBetter(t *testing.T) {
	if !Better(1.0, 2.0) {
		t.Error("Better(1,2) must be true: lower is better")
	}
	if Better(2.0, 1.0) {
		t.Error("Better(2,1) must be false: lower is better")
	}
	if Better(1.0, 1.0) {
		t.Error("Better is strict; equal scores are not better")
	}
}

// TestUnavailableScoreCannotWinRanking is the safety property behind choosing NaN: a
// caller who ignores the error and ranks naively must not end up electing the dead node.
func TestUnavailableScoreCannotWinRanking(t *testing.T) {
	dead := ScoreUnavailable()
	alive := 42.0 // a genuinely poor but real score

	// The naive comparison a future author will write.
	if dead < alive {
		t.Fatal("NaN won a naive `<` ranking; the poison-value property is broken")
	}
	// And the comparison they should have written.
	if Better(dead, alive) {
		t.Fatal("Better() ranked an unavailable score above a real one")
	}
	if !Better(alive, dead) {
		t.Fatal("Better() failed to rank a real score above an unavailable one")
	}

	// A full ranking loop over a candidate set, as pkg/cluster would write it.
	scores := map[protocol.NodeAddress]float64{
		"dead-a:9000": ScoreUnavailable(),
		"slow-b:9000": 42.0,
		"fast-c:9000": 3.0,
		"dead-d:9000": ScoreUnavailable(),
	}
	var best protocol.NodeAddress
	bestScore := math.NaN()
	for addr, sc := range scores {
		if best == "" || Better(sc, bestScore) {
			best, bestScore = addr, sc
		}
	}
	if best != "fast-c:9000" {
		t.Fatalf("ranking chose %q (score %v), want fast-c:9000", best, bestScore)
	}
}

func TestBetterWithBothInvalid(t *testing.T) {
	// Stability: neither of two unavailable scores is "better", so ranking a set of
	// entirely unreachable peers does not depend on map iteration order.
	if Better(ScoreUnavailable(), ScoreUnavailable()) {
		t.Fatal("neither of two invalid scores may be better than the other")
	}
}

func TestIsCancellation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare canceled", context.Canceled, true},
		{"bare deadline", context.DeadlineExceeded, true},
		{"sentinel canceled", fmt.Errorf("%w: %w", ErrProbeCanceled, context.Canceled), true},
		{"sentinel deadline", fmt.Errorf("%w: %w", ErrProbeCanceled, context.DeadlineExceeded), true},

		// The crux: a probe-timeout error must NOT read as a cancellation, or the swarm
		// stops evicting dead peers.
		{"probe timeout", fmt.Errorf("%w: %w", ErrUnreachable, ErrProbeTimeout), false},
		{"unreachable", fmt.Errorf("%w: refused", ErrUnreachable), false},
		{"invalid target", fmt.Errorf("%w: empty", ErrInvalidTarget), false},
		{"unrelated", errors.New("boom"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsCancellation(c.err); got != c.want {
				t.Errorf("IsCancellation(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestProbeTimeoutDoesNotWrapContextDeadline pins the deliberate non-wrapping decision
// in classify. If someone "tidies" that %v into a %w, this test fails and explains why
// that is not a tidy-up.
func TestProbeTimeoutDoesNotWrapContextDeadline(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: deadlineProber()})
	_, err := s.EvaluateScore(context.Background(), "slow:9000")
	if err == nil {
		t.Fatal("expected an error from a timing-out probe")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a probe timeout must be ErrUnreachable, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe-timeout error must not wrap context.DeadlineExceeded: %v", err)
	}
	if IsCancellation(err) {
		t.Fatalf("probe timeout classified as a cancellation: %v", err)
	}
}
