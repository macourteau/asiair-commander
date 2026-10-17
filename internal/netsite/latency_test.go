package netsite

import (
	"testing"
	"time"
)

func TestLatencyEstimator(t *testing.T) {
	var e LatencyEstimator
	if e.Latency() != 0 || e.Min() != 0 {
		t.Fatal("an estimator with no sample reports a latency")
	}

	// A sub-millisecond sample is floored, so a measured site is never 0.
	e.Observe(10 * time.Microsecond)
	if e.Min() != minLatencySample || e.Latency() <= 0 {
		t.Fatalf("floored sample: min %v latency %v", e.Min(), e.Latency())
	}

	// Steady samples converge on the round trip, the variation decaying.
	var s LatencyEstimator
	for i := 0; i < 100; i++ {
		s.Observe(100 * time.Millisecond)
	}
	if got := s.Latency(); got < 100*time.Millisecond || got > 101*time.Millisecond {
		t.Fatalf("steady 100 ms: latency %v", got)
	}
	if got := s.Smoothed(); got != 100*time.Millisecond {
		t.Fatalf("steady 100 ms: smoothed %v", got)
	}

	// One slow sample widens the margin at once through the variation term,
	// and steady samples narrow it again.
	s.Observe(500 * time.Millisecond)
	if got := s.Latency(); got <= 150*time.Millisecond {
		t.Fatalf("after a 500 ms outlier: latency %v, want the margin widened", got)
	}
	// The smoothed figure moves by an eighth of the excess only.
	if got := s.Smoothed(); got != 150*time.Millisecond {
		t.Fatalf("after a 500 ms outlier: smoothed %v, want 150ms", got)
	}
	for i := 0; i < 50; i++ {
		s.Observe(100 * time.Millisecond)
	}
	if got := s.Latency(); got > 110*time.Millisecond {
		t.Fatalf("50 steady samples after the outlier: latency %v", got)
	}
	if s.Min() != 100*time.Millisecond {
		t.Fatalf("min %v, want 100ms", s.Min())
	}

	// The minimum spans only the recent window: once minWindow slower samples
	// pass, the old low falls out.
	for i := 0; i < minWindow; i++ {
		s.Observe(300 * time.Millisecond)
	}
	if s.Min() != 300*time.Millisecond {
		t.Fatalf("min %v after the window moved, want 300ms", s.Min())
	}
}
