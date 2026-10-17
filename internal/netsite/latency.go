package netsite

import (
	"sync"
	"time"
)

const (
	// minLatencySample floors every sample, so a site that measured anything
	// reports a nonzero latency: a remote tested on the same LAN as Commander
	// still compares above the local site's 0, which path selection relies on.
	minLatencySample = time.Millisecond
	// minWindow is how many recent samples MinLatency spans: at one sample per
	// heartbeat, a few minutes, so a transfer that inflates the round trip for
	// a while does not lift the minimum.
	minWindow = 32
)

// LatencyEstimator smooths a site's round-trip samples as TCP does (RFC 6298:
// SRTT and RTTVAR, gains 1/8 and 1/4) and keeps the minimum of the recent ones.
// The zero value is ready to use and reports 0 until the first sample. It is
// safe for concurrent use.
type LatencyEstimator struct {
	mu     sync.Mutex
	srtt   time.Duration
	rttvar time.Duration
	recent [minWindow]time.Duration
	n      int // samples held in recent, at most minWindow
	next   int // the slot the next sample overwrites
}

// Observe records one round-trip sample.
func (e *LatencyEstimator) Observe(rtt time.Duration) {
	rtt = max(rtt, minLatencySample)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == 0 {
		e.srtt, e.rttvar = rtt, rtt/2
	} else {
		dev := e.srtt - rtt
		if dev < 0 {
			dev = -dev
		}
		e.rttvar += (dev - e.rttvar) / 4
		e.srtt += (rtt - e.srtt) / 8
	}
	e.recent[e.next] = rtt
	e.next = (e.next + 1) % minWindow
	e.n = min(e.n+1, minWindow)
}

// Latency is the smoothed round trip plus four times its variation, the
// figure TCP takes as the most a round trip should take: a timeout margin
// built from it rides out ordinary jitter without hiding a dead link.
func (e *LatencyEstimator) Latency() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.srtt + 4*e.rttvar
}

// Smoothed is the smoothed round trip alone, without the variation margin
// Latency adds: the figure to show a user as the link's latency.
func (e *LatencyEstimator) Smoothed() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.srtt
}

// Min is the lowest of the recent samples, 0 before the first.
func (e *LatencyEstimator) Min() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	var lo time.Duration
	for i := 0; i < e.n; i++ {
		if lo == 0 || e.recent[i] < lo {
			lo = e.recent[i]
		}
	}
	return lo
}
