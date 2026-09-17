package health

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"testing"
)

// errorIsDeadline decides whether a failed probe is evidence about the peer or
// evidence about our own budget. Every branch matters: a false positive suppresses
// a real eviction, a false negative evicts a peer for our own timeout.
func TestErrorIsDeadlineClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a deadline", nil, false},
		{"context deadline", context.DeadlineExceeded, true},
		{"wrapped context deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), true},
		{"context cancellation is not a deadline", context.Canceled, false},
		{"connection refused is not a deadline", errors.New("connect: connection refused"), false},
		// net.Dialer surfaces an expired DialContext deadline this way, not as
		// context.DeadlineExceeded, so the net.Error spelling must also match.
		{"net.OpError wrapping os.ErrDeadlineExceeded", &net.OpError{
			Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded,
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorIsDeadline(tt.err); got != tt.want {
				t.Errorf("errorIsDeadline(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// An address never probed has no score. Returning (0, true) here would read as
// "perfect health" to a ranking loop, which is the failure this bool prevents.
func TestScoreAndSamplesForUnknownTarget(t *testing.T) {
	s := NewLatencyHealthStrategy(Config{Probe: fixedProber(5)})

	score, ok := s.Score("never-probed:7946")
	if ok {
		t.Errorf("Score reported history for an unprobed target: %v", score)
	}
	if !math.IsNaN(score) && score != 0 {
		t.Errorf("unexpected score for an unprobed target: %v", score)
	}
	if n := s.Samples("never-probed:7946"); n != 0 {
		t.Errorf("Samples = %d for an unprobed target, want 0", n)
	}
}

// Forget must reset history, not merely hide it. If a forgotten target kept its
// EWMA, a container restarting onto a recycled address would inherit the scores of
// whatever used to live there.
func TestForgetResetsHistoryRatherThanHidingIt(t *testing.T) {
	const target = "node-3:7946"
	s := NewLatencyHealthStrategy(Config{Probe: sequenceProber(100, 100, 100), Alpha: 1})
	ctx := context.Background()

	if _, err := s.EvaluateScore(ctx, target); err != nil {
		t.Fatalf("EvaluateScore: %v", err)
	}
	if n := s.Samples(target); n != 1 {
		t.Fatalf("Samples = %d after one probe, want 1", n)
	}

	s.Forget(target)
	if n := s.Samples(target); n != 0 {
		t.Fatalf("Samples = %d after Forget, want 0", n)
	}

	// The next sample must seed directly rather than blend against the old value.
	if _, err := s.EvaluateScore(ctx, target); err != nil {
		t.Fatalf("EvaluateScore after Forget: %v", err)
	}
	if n := s.Samples(target); n != 1 {
		t.Errorf("Samples = %d after re-probing a forgotten target, want 1", n)
	}
}
