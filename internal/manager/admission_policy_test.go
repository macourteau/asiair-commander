package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// localReq is a local rig's request, the only kind that existed before sites.
var localReq = rig.Requester{Rig: "rig-01", Site: netsite.Local}

// acquireWithin acquires for req and fails the test unless it is granted before
// d elapses.
func acquireWithin(t *testing.T, a *Admission, req rig.Requester, size int64, d time.Duration) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	rel, err := a.Acquire(ctx, req, size)
	if err != nil {
		t.Fatalf("Acquire(%+v): %v", req, err)
	}
	return rel
}

// blocked reports whether an Acquire for req is still waiting after a short
// grace, cancelling it either way.
func blocked(a *Admission, req rig.Requester) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rel, err := a.Acquire(ctx, req, 1)
	if err == nil {
		rel()
		return false
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// TestAdmissionPolicyPerRemoteSlot pins the first admission policy: each
// non-local site admits one transfer at a time, outside the global cap; local
// rigs keep the global cap; and the disk reserve is one gate shared by every
// rig on every site. Retarget names the local site (rig side, below), so it
// takes the global cap whatever the rig's site.
func TestAdmissionPolicyPerRemoteSlot(t *testing.T) {
	t.Run("each remote site admits one transfer, outside the global cap", func(t *testing.T) {
		lf := localfs.NewFake(t.TempDir())
		a := NewAdmission(1, lf, "/staging", 0)

		local := acquireWithin(t, a, localReq, 1, time.Second)
		defer local()
		// The global cap is full, yet each remote still admits one.
		r1 := acquireWithin(t, a, rig.Requester{Rig: "rig-02", Site: "r1"}, 1, time.Second)
		r2 := acquireWithin(t, a, rig.Requester{Rig: "rig-03", Site: "r2"}, 1, time.Second)

		// A second transfer on r1, even from another rig, waits for r1's slot.
		if !blocked(a, rig.Requester{Rig: "rig-04", Site: "r1"}) {
			t.Fatal("a second transfer on r1 was admitted while r1's slot was held")
		}
		// And a local one waits for the global cap, not for any remote slot.
		if !blocked(a, rig.Requester{Rig: "rig-05", Site: netsite.Local}) {
			t.Fatal("a second local transfer was admitted past a global cap of 1")
		}
		if n, _ := a.InFlight(); n != 3 {
			t.Fatalf("InFlight count = %d, want 3 (one local, one per remote)", n)
		}

		r1()
		next := acquireWithin(t, a, rig.Requester{Rig: "rig-04", Site: "r1"}, 1, time.Second)
		next()
		r2()
	})

	t.Run("local rigs keep the global cap", func(t *testing.T) {
		lf := localfs.NewFake(t.TempDir())
		a := NewAdmission(2, lf, "/staging", 0)
		rel1 := acquireWithin(t, a, rig.Requester{Rig: "rig-01"}, 1, time.Second)
		rel2 := acquireWithin(t, a, rig.Requester{Rig: "rig-02"}, 1, time.Second)
		if !blocked(a, rig.Requester{Rig: "rig-03"}) {
			t.Fatal("a third local transfer was admitted past a global cap of 2")
		}
		rel1()
		rel2()
	})

	t.Run("the disk reserve is shared across all sites", func(t *testing.T) {
		lf := localfs.NewFake(t.TempDir())
		// Free 100, reserve 50: room for 50 bytes in flight across every rig.
		lf.FreeBytes = 100
		a := NewAdmission(4, lf, "/staging", 50)

		r1 := acquireWithin(t, a, rig.Requester{Rig: "rig-02", Site: "r1"}, 30, time.Second)
		// r2's slot is free, but r1's bytes already count against the reserve.
		if _, err := a.Acquire(context.Background(), rig.Requester{Rig: "rig-03", Site: "r2"}, 30); !errors.Is(err, ErrDiskReserve) {
			t.Fatalf("remote r2 past the shared reserve: err = %v, want ErrDiskReserve", err)
		}
		if _, err := a.Acquire(context.Background(), localReq, 30); !errors.Is(err, ErrDiskReserve) {
			t.Fatalf("local past the shared reserve: err = %v, want ErrDiskReserve", err)
		}
		// A denial gives r2's slot back.
		r2 := acquireWithin(t, a, rig.Requester{Rig: "rig-03", Site: "r2"}, 20, time.Second)
		if _, b := a.InFlight(); b != 50 {
			t.Fatalf("InFlight bytes = %d, want 50", b)
		}
		r1()
		r2()
		if n, b := a.InFlight(); n != 0 || b != 0 {
			t.Fatalf("InFlight after release = %d/%d, want 0/0", n, b)
		}
	})
}
