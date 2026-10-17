package web

import (
	"sync"
	"time"
)

// loginLimiter throttles login attempts with two layers chosen to avoid trading
// one DoS for another (design architecture §8):
//
//   - a per-source exponential backoff that slows a single brute-forcing client,
//     checked before bcrypt so a hammering source cannot pin the CPU; it resets on
//     a correct password, so the legitimate operator is only ever briefly slowed;
//   - a global cap that throttles rather than hard-locks. When recent failures
//     across all sources exceed the cap, attempts from sources never seen before
//     are rejected (the spoofed-source flood), while a source already tracked — the
//     operator's own — is still admitted to the password check, so a source-spoofing
//     attacker cannot lock the operator out entirely.
type loginLimiter struct {
	mu      sync.Mutex
	sources map[string]*sourceState
	// globalFails holds recent failure timestamps across all sources, trimmed to
	// globalWindow on each consult.
	globalFails []time.Time
	// cap mirrors the last-configured global cap so check (which has no config in
	// hand) can consult it; it is set on every recorded failure.
	cap int
}

// sourceState tracks one client's consecutive failures and the instant before
// which its next attempt is refused.
type sourceState struct {
	failures int
	until    time.Time
}

// globalWindow is the rolling span over which failures count toward the global
// cap; the cap self-heals as old failures age out, so the throttle is temporary.
const globalWindow = time.Minute

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{sources: make(map[string]*sourceState)}
}

// check reports whether source may attempt a login now. On refusal it returns the
// wait until the source's backoff elapses (the per-source case) or the rolling
// window (the global case).
func (l *loginLimiter) check(source string, now time.Time) (retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if st, seen := l.sources[source]; seen && now.Before(st.until) {
		return st.until.Sub(now), false
	}
	// Global throttle: only unknown (untracked) sources are turned away, so the
	// operator's already-tracked source is never fully locked out.
	l.trimLocked(now)
	if _, tracked := l.sources[source]; !tracked && len(l.globalFails) >= l.globalCap() {
		return globalWindow, false
	}
	return 0, true
}

// failure records a failed attempt: it advances the source's exponential backoff
// and appends to the global window. base, cap, and globalCap come from config so
// the knobs stay operator-tunable.
func (l *loginLimiter) failure(source string, now time.Time, base, cap time.Duration, globalCap int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.cap = globalCap
	st := l.sources[source]
	if st == nil {
		st = &sourceState{}
		l.sources[source] = st
	}
	st.failures++
	backoff := backoffFor(st.failures, base, cap)
	st.until = now.Add(backoff)

	l.globalFails = append(l.globalFails, now)
	l.trimLocked(now)
}

// success clears a source's backoff after a correct password.
func (l *loginLimiter) success(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sources, source)
}

func (l *loginLimiter) globalCap() int {
	if l.cap <= 0 {
		return 20 // matches the shipped default (design architecture §7)
	}
	return l.cap
}

// trimLocked drops failure timestamps older than globalWindow. Caller holds mu.
func (l *loginLimiter) trimLocked(now time.Time) {
	cutoff := now.Add(-globalWindow)
	i := 0
	for i < len(l.globalFails) && l.globalFails[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		l.globalFails = l.globalFails[i:]
	}
}

// backoffFor is base * 2^(failures-1), clamped to cap. A zero base falls back to
// one second so a mis-set config still throttles.
func backoffFor(failures int, base, cap time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if cap <= 0 {
		cap = 30 * time.Second
	}
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= cap {
			return cap
		}
	}
	if d > cap {
		return cap
	}
	return d
}
