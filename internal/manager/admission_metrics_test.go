package manager

import (
	"context"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/localfs"
)

// blockingLFS holds inside FreeSpace until released, so a test can pin Acquire with
// a.mu held across the (simulated) I/O — the exact hazard P2 warns about.
type blockingLFS struct {
	*localfs.Fake
	entered chan struct{}
	release chan struct{}
}

func (b *blockingLFS) FreeSpace(string) (int64, int64, error) {
	close(b.entered)
	<-b.release
	return 1 << 40, 1 << 40, nil
}

// TestAdmissionInFlightIsLockFree is the authentic guard for the P2 fix: while a
// goroutine is inside Acquire holding a.mu across FreeSpace, InFlight() must still
// return immediately, because it reads mirrored atomics rather than a.mu. If someone
// "simplifies" InFlight to read under the lock, this test hangs.
func TestAdmissionInFlightIsLockFree(t *testing.T) {
	lfs := &blockingLFS{
		Fake:    localfs.NewFake(t.TempDir()),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	adm := NewAdmission(1, lfs, "staging", 0)

	go func() { _, _ = adm.Acquire(context.Background(), localReq, 1) }()
	<-lfs.entered // Acquire now holds a.mu, blocked in FreeSpace

	done := make(chan struct{})
	go func() {
		adm.InFlight()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("InFlight blocked behind a.mu held across FreeSpace — P2 regression")
	}
	close(lfs.release)
}
