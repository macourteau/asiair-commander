package config

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestGuardSerializesReadModifyWrite proves two concurrent writers' read-modify-
// writes never interleave: without the guard the deliberate read-sleep-write
// window below would lose updates, so a correct final count proves mutual
// exclusion (a migration bind racing a web save).
func TestGuardSerializesReadModifyWrite(t *testing.T) {
	var g Guard
	counter := 0
	const writers = 50

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Do(func() error {
				// Read, yield, then write — the classic lost-update window the guard
				// must close.
				seen := counter
				time.Sleep(time.Millisecond)
				counter = seen + 1
				return nil
			})
		}()
	}
	wg.Wait()

	if counter != writers {
		t.Fatalf("counter = %d, want %d — writers interleaved (lost update)", counter, writers)
	}
}

// TestGuardPropagatesError confirms fn's error is returned unchanged and the lock
// is released so a later Do still runs.
func TestGuardPropagatesError(t *testing.T) {
	var g Guard
	sentinel := errors.New("boom")

	if err := g.Do(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Do error = %v, want %v", err, sentinel)
	}
	// The lock must have been released: a subsequent Do proceeds.
	ran := false
	if err := g.Do(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("second Do err=%v ran=%v, want nil/true (lock released)", err, ran)
	}
}
