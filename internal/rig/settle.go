package rig

import (
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
)

// settleTracker is the cross-cycle quiescence tracker (design frame-lifecycle
// §4 "settle"): a file is a pull candidate only once its size and mtime have
// held unchanged across settle_cycles consecutive observations. Settle is
// cycle-grained by nature — it is observed once per poll_interval — so the
// tracker counts consecutive stable cycles rather than measuring wall time.
//
// It is touched only from the single rig-worker goroutine, so it needs no lock.
type settleTracker struct {
	obs map[string]*settleObs
	tol time.Duration
}

type settleObs struct {
	size   int64
	mtime  clock.DeviceTime
	stable int  // consecutive cycles the (size,mtime) has held
	seen   bool // observed in the current cycle (for eviction of gone keys)
}

// newSettleTracker builds a tracker whose mtime comparison uses tol (the same
// ±2s FAT-granularity tolerance used everywhere else).
func newSettleTracker(tol time.Duration) *settleTracker {
	return &settleTracker{obs: make(map[string]*settleObs), tol: tol}
}

// beginCycle marks every tracked key unseen; keys not re-observed this cycle are
// dropped by endCycle (a file that vanished from the device).
func (s *settleTracker) beginCycle() {
	for _, o := range s.obs {
		o.seen = false
	}
}

// observe records one cycle's sighting of a candidate and returns the number of
// consecutive stable cycles so far. A first sighting is 0 stable cycles; an
// unchanged re-sighting increments; any size/mtime change resets to 0.
func (s *settleTracker) observe(key string, size int64, mtime clock.DeviceTime) int {
	o, ok := s.obs[key]
	if !ok {
		s.obs[key] = &settleObs{size: size, mtime: mtime, stable: 0, seen: true}
		return 0
	}
	if o.size == size && mtime.EqualWithin(o.mtime, s.tol) {
		o.stable++
	} else {
		o.size = size
		o.mtime = mtime
		o.stable = 0
	}
	o.seen = true
	return o.stable
}

// stable returns the current consecutive-stable-cycle count for key (0 if never
// observed). Used by the safe-to-power-off computation.
func (s *settleTracker) stable(key string) int {
	if o, ok := s.obs[key]; ok {
		return o.stable
	}
	return 0
}

// endCycle evicts keys not observed this cycle — a file removed from the device
// must not carry stale settle history into a later reappearance.
func (s *settleTracker) endCycle() {
	for k, o := range s.obs {
		if !o.seen {
			delete(s.obs, k)
		}
	}
}
