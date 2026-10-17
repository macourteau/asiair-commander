package manager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/localfs"
)

// acquireOrFatal acquires with plenty of room and fails the test on error.
func acquireOrFatal(t *testing.T, a *Admission, size int64) func() {
	t.Helper()
	rel, err := a.Acquire(context.Background(), localReq, size)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return rel
}

func TestAdmissionConcurrencyCapAllSucceed(t *testing.T) {
	lf := localfs.NewFake(t.TempDir())
	a := NewAdmission(3, lf, "/staging", 0)

	var releases []func()
	for i := 0; i < 3; i++ {
		releases = append(releases, acquireOrFatal(t, a, 1))
	}
	// The N+1th must block until a slot frees.
	got := make(chan func(), 1)
	go func() {
		rel, err := a.Acquire(context.Background(), localReq, 1)
		if err != nil {
			t.Errorf("blocked Acquire returned error: %v", err)
			return
		}
		got <- rel
	}()

	select {
	case <-got:
		t.Fatal("4th Acquire succeeded before any release")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	releases[0]() // free one slot

	select {
	case rel := <-got:
		rel()
	case <-time.After(2 * time.Second):
		t.Fatal("4th Acquire did not proceed after a release")
	}

	for _, r := range releases[1:] {
		r()
	}
}

func TestAdmissionCtxCancelAbortsQueuedAcquire(t *testing.T) {
	lf := localfs.NewFake(t.TempDir())
	a := NewAdmission(1, lf, "/staging", 0)

	hold := acquireOrFatal(t, a, 1)
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.Acquire(ctx, localReq, 1)
		errc <- err
	}()

	select {
	case <-errc:
		t.Fatal("queued Acquire returned before cancel")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not abort queued Acquire")
	}
}

func TestAdmissionDiskReserveDeniesAndFreesSlot(t *testing.T) {
	lf := localfs.NewFake(t.TempDir())
	lf.FreeBytes = 100
	// reserve 90; a 20-byte pull would leave 80 < 90 → denied.
	a := NewAdmission(2, lf, "/staging", 90)

	rel, err := a.Acquire(context.Background(), localReq, 20)
	if !errors.Is(err, ErrDiskReserve) {
		t.Fatalf("want ErrDiskReserve, got err=%v (rel nil=%v)", err, rel == nil)
	}
	if rel != nil {
		t.Fatal("denied Acquire returned a non-nil release")
	}

	// The slot must be free: raise free space and a normal Acquire succeeds.
	lf.FreeBytes = 1000
	rel2 := acquireOrFatal(t, a, 20)
	rel2()
}

func TestAdmissionInFlightReserveAccounting(t *testing.T) {
	lf := localfs.NewFake(t.TempDir())
	// Free 100, reserve 50. One 40-byte pull is fine (60 >= 50). A concurrent
	// second 40-byte pull while the first is in flight would leave
	// 100-40-40=20 < 50 → must be denied even though the disk itself has not moved.
	lf.FreeBytes = 100
	a := NewAdmission(4, lf, "/staging", 50)

	rel1 := acquireOrFatal(t, a, 40)

	_, err := a.Acquire(context.Background(), localReq, 40)
	if !errors.Is(err, ErrDiskReserve) {
		t.Fatalf("second concurrent admission: want ErrDiskReserve, got %v", err)
	}

	// After the first releases its reserve, the same pull fits again.
	rel1()
	rel2 := acquireOrFatal(t, a, 40)
	rel2()
}

func TestAdmissionReleaseIdempotent(t *testing.T) {
	lf := localfs.NewFake(t.TempDir())
	a := NewAdmission(1, lf, "/staging", 0)

	rel := acquireOrFatal(t, a, 10)
	rel()
	rel() // double release must be a no-op (no negative reserve, no extra slot).

	// inFlightBytes must be exactly zero after balanced release.
	a.mu.Lock()
	inflight := a.inFlightBytes
	a.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inFlightBytes = %d, want 0", inflight)
	}

	// The single slot must not have been over-released: only one Acquire may hold
	// it, and a second must block.
	hold := acquireOrFatal(t, a, 10)
	got := make(chan struct{}, 1)
	go func() {
		r, _ := a.Acquire(context.Background(), localReq, 10)
		if r != nil {
			r()
		}
		got <- struct{}{}
	}()
	select {
	case <-got:
		t.Fatal("double-release leaked a semaphore slot")
	case <-time.After(50 * time.Millisecond):
	}
	hold()
	<-got
}

func TestAdmissionConcurrentAcquireRelease(t *testing.T) {
	lf := localfs.NewFake(t.TempDir())
	a := NewAdmission(4, lf, "/staging", 0)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := a.Acquire(context.Background(), localReq, 1)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			rel()
		}()
	}
	wg.Wait()

	a.mu.Lock()
	inflight := a.inFlightBytes
	a.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("inFlightBytes = %d after balanced churn, want 0", inflight)
	}
}
