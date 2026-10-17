package devfiles

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

func TestSessionIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("foreign answer is refused and not retried", func(t *testing.T) {
		fx := newFixture(t)
		fx.prober.script(ip1, probeAnswer{guid: "someone-else", ok: true})
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		e := wantKind(t, err, ErrIdentity)
		if !strings.Contains(e.Msg, ip1) || !strings.Contains(e.Msg, "Rig One") {
			t.Errorf("message %q should name the address and the rig", e.Msg)
		}
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1 (identity is not retried)", got)
		}
		if got := fx.closes.Load(); got != 1 {
			t.Errorf("closes = %d, want 1 (the foreign session is closed)", got)
		}
	})

	t.Run("cpu id match is accepted", func(t *testing.T) {
		fx := newFixture(t)
		fx.editRig("r1", func(p *model.RigProfile) { p.CPUID = "cpu1" })
		fx.prober.script(ip1, probeAnswer{guid: "reset-guid", cpuID: "cpu1", ok: true})
		l, err := fx.svc.List(ctx, "r1", emmc, "")
		if err != nil {
			t.Fatal(err)
		}
		if !l.Writable {
			t.Error("a session proved by cpu id should be writable")
		}
	})

	t.Run("no reply fails closed and is retried", func(t *testing.T) {
		fx := newFixture(t)
		fx.prober.script(ip1, probeAnswer{}, probeAnswer{}, probeAnswer{guid: "g1", ok: true})
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatalf("List after two silent probes: %v", err)
		}
		if got := fx.opens.Load(); got != 3 {
			t.Errorf("opens = %d, want 3", got)
		}
		if got := fx.closes.Load(); got != 2 {
			t.Errorf("closes = %d, want 2 (each unconfirmed session closed)", got)
		}
	})

	t.Run("no reply ever is unreachable", func(t *testing.T) {
		fx := newFixture(t)
		fx.prober.script(ip1, probeAnswer{})
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		wantKind(t, err, ErrUnreachable)
		if got := fx.opens.Load(); got != 4 {
			t.Errorf("opens = %d, want 4", got)
		}
	})

	t.Run("host-bound rig reads but never writes", func(t *testing.T) {
		fx := newFixture(t)
		fx.editRig("r1", func(p *model.RigProfile) { p.GUID = "" })
		fx.dev(ip1).AddFile(emmc, "Preview/a.jpg", []byte("x"), t0)
		l, err := fx.svc.List(ctx, "r1", emmc, "Preview")
		if err != nil {
			t.Fatalf("List on a host-bound rig: %v", err)
		}
		if l.Writable || len(l.Entries) != 1 {
			t.Errorf("listing = %+v, want one entry and writable false", l)
		}
		err = fx.svc.Mkdir(ctx, "r1", emmc, "Preview", "New")
		wantKind(t, err, ErrHostBound)
		if got := fx.dev(ip1).Mutations(); got != 0 {
			t.Errorf("mutations = %d, want 0", got)
		}
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1 (host_bound is not retried)", got)
		}
	})

	t.Run("unmanaged and unknown rigs are refused", func(t *testing.T) {
		fx := newFixture(t)
		for _, id := range []string{"r3", "nope"} {
			_, err := fx.svc.List(ctx, id, "", "")
			wantKind(t, err, ErrUnknownRig)
		}
		if got := fx.opens.Load(); got != 0 {
			t.Errorf("opens = %d, want 0", got)
		}
	})
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("reused within the idle window, one mount per share", func(t *testing.T) {
		fx := newFixture(t)
		for range 3 {
			if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := fx.svc.List(ctx, "r1", udisk, ""); err != nil {
			t.Fatal(err)
		}
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1", got)
		}
		if got := fx.mounts.Load(); got != 2 {
			t.Errorf("mounts = %d, want 2 (each share once)", got)
		}
		if got := fx.prober.count(); got != 1 {
			t.Errorf("probes = %d, want 1 (one per session)", got)
		}
	})

	t.Run("closed after the idle window", func(t *testing.T) {
		fx := newFixture(t)
		fx.svc.idleTTL = 20 * time.Millisecond
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the idle close", func() bool { return fx.closes.Load() == 1 })
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		if got := fx.opens.Load(); got != 2 {
			t.Errorf("opens = %d, want 2 (a fresh session after the idle close)", got)
		}
	})

	t.Run("closed on an endpoint change", func(t *testing.T) {
		fx := newFixture(t)
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		fx.devs["10.0.0.9"] = fx.dev(ip1)
		fx.prober.guids["10.0.0.9"] = "g1"
		fx.setAddress("r1", "10.0.0.9")
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		if got, want := [2]int64{fx.opens.Load(), fx.closes.Load()}, [2]int64{2, 1}; got != want {
			t.Errorf("opens, closes = %v, want %v", got, want)
		}
	})

	t.Run("closed on a config change", func(t *testing.T) {
		fx := newFixture(t)
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		fx.editRig("r1", func(p *model.RigProfile) { p.Password = "changed" })
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		if got, want := [2]int64{fx.opens.Load(), fx.closes.Load()}, [2]int64{2, 1}; got != want {
			t.Errorf("opens, closes = %v, want %v", got, want)
		}
		if got := fx.prober.count(); got != 2 {
			t.Errorf("probes = %d, want 2 (the new session re-proves identity)", got)
		}
	})

	t.Run("closed, not parked, after a network error", func(t *testing.T) {
		fx := newFixture(t)
		c, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		fx.svc.release(c, errors.Join(errors.New("read"), errNetworkForTest()))
		if got := fx.closes.Load(); got != 1 {
			t.Fatalf("closes = %d, want 1", got)
		}
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		if got := fx.opens.Load(); got != 2 {
			t.Errorf("opens = %d, want 2 (nothing was parked)", got)
		}
	})

	t.Run("closed after a broken mark even on a clean error", func(t *testing.T) {
		fx := newFixture(t)
		c, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		c.markBroken(errNetworkForTest())
		fx.svc.release(c, nil)
		if got := fx.closes.Load(); got != 1 {
			t.Errorf("closes = %d, want 1", got)
		}
	})

	t.Run("closed, not parked, after a cancel", func(t *testing.T) {
		fx := newFixture(t)
		cctx, cancel := context.WithCancel(ctx)
		c, err := fx.svc.acquire(cctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		fx.svc.release(c, nil)
		if got := fx.closes.Load(); got != 1 {
			t.Fatalf("closes = %d, want 1", got)
		}
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatal(err)
		}
		if got := fx.opens.Load(); got != 2 {
			t.Errorf("opens = %d, want 2 (nothing was parked)", got)
		}
	})

	t.Run("a parked session is reused only by a later checkout", func(t *testing.T) {
		fx := newFixture(t)
		a, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		b, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		fx.svc.release(a, nil)
		fx.svc.release(b, nil) // parks b, closing a: one idle session per rig
		if got, want := [2]int64{fx.opens.Load(), fx.closes.Load()}, [2]int64{2, 1}; got != want {
			t.Errorf("opens, closes = %v, want %v", got, want)
		}
	})

	t.Run("drained by Close", func(t *testing.T) {
		fx := newFixture(t)
		for _, id := range []string{"r1", "r2"} {
			if _, err := fx.svc.List(ctx, id, emmc, ""); err != nil {
				t.Fatal(err)
			}
		}
		fx.svc.Close()
		if got := fx.closes.Load(); got != 2 {
			t.Errorf("closes = %d, want 2", got)
		}
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		wantKind(t, err, ErrClosed)
	})

	t.Run("checked in after Close is closed", func(t *testing.T) {
		fx := newFixture(t)
		c, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		fx.svc.Close()
		fx.svc.release(c, nil)
		if got := fx.closes.Load(); got != 1 {
			t.Errorf("closes = %d, want 1", got)
		}
	})
}

func TestOperationLockAndCap(t *testing.T) {
	ctx := context.Background()

	t.Run("a second operation is busy and names the first", func(t *testing.T) {
		fx := newFixture(t)
		// The server's zone (UTC in Docker) need not be the reader's, so the
		// start time carries its offset.
		fx.svc.now = func() time.Time { return time.Date(2026, 10, 7, 22, 15, 3, 0, time.FixedZone("", -4*3600)) }
		unlock, err := fx.svc.lockOp(ctx, "Copy", "r1", "r2")
		if err != nil {
			t.Fatal(err)
		}
		_, err = fx.svc.lockOp(ctx, "Delete", "r1")
		e := wantKind(t, err, ErrBusy)
		want := "Copy on Rig One and Rig Two is running (started 2026-10-07T22:15:03-04:00). Try again when it has finished."
		if e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
		unlock()
		unlock() // a second call is harmless
		again, err := fx.svc.lockOp(ctx, "Delete", "r1")
		if err != nil {
			t.Fatalf("lock after unlock: %v", err)
		}
		again()
	})

	t.Run("a listing runs beside an operation", func(t *testing.T) {
		fx := newFixture(t)
		unlock, err := fx.svc.lockOp(ctx, "Delete", "r1")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Errorf("List under a held operation lock: %v", err)
		}
	})

	t.Run("a third checkout on one rig waits", func(t *testing.T) {
		fx := newFixture(t)
		a, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		b, err := fx.svc.acquire(ctx, "r1", "test")
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan *session, 1)
		go func() {
			c, err := fx.svc.acquire(ctx, "r1", "test")
			if err != nil {
				t.Error(err)
			}
			got <- c
		}()
		select {
		case <-got:
			t.Fatal("a third checkout did not wait")
		case <-time.After(50 * time.Millisecond):
		}
		// Another rig is not held up by r1's cap.
		other, err := fx.svc.acquire(ctx, "r2", "test")
		if err != nil {
			t.Fatal(err)
		}
		fx.svc.release(other, nil)

		fx.svc.release(a, nil)
		c := <-got
		if c != a {
			t.Error("the waiting checkout should take the session just parked")
		}
		if open := fx.opens.Load() - 1 - fx.closes.Load(); open > 2 { // minus r2's session
			t.Errorf("%d sessions open on r1, want at most 2", open)
		}
		fx.svc.release(b, nil)
		fx.svc.release(c, nil)
	})

	t.Run("waiting for a slot honours the context", func(t *testing.T) {
		fx := newFixture(t)
		a, _ := fx.svc.acquire(ctx, "r1", "test")
		b, _ := fx.svc.acquire(ctx, "r1", "test")
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err := fx.svc.List(cctx, "r1", emmc, "")
		wantKind(t, err, ErrCancelled)
		fx.svc.release(a, nil)
		fx.svc.release(b, nil)
	})

	t.Run("a two-rig run under the lock gets one session per rig", func(t *testing.T) {
		fx := newFixture(t)
		unlock, err := fx.svc.lockOp(ctx, "Copy", "r1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		err = fx.svc.withRetry(ctx, "Copy", emmc, []string{"r1", "r1"}, func(ss []*session) error {
			if ss[0] != ss[1] {
				t.Error("a same-rig request should read and write through one session")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1", got)
		}
	})
}

func TestBusyWhileStopping(t *testing.T) {
	fx := newFixture(t)
	hctx, cancel := context.WithCancel(context.Background())
	unlock, err := fx.svc.lockOp(hctx, "Copy", "r1")
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the holder was stopped but is still waiting on its device

	_, err = fx.svc.lockOp(context.Background(), "Delete", "r2")
	e := wantKind(t, err, ErrBusy)
	want := "Copy on Rig One is still stopping: waiting up to 30s for Rig One to respond."
	if e.Msg != want {
		t.Errorf("message = %q, want %q", e.Msg, want)
	}

	unlock()
	next, err := fx.svc.lockOp(context.Background(), "Delete", "r2")
	if err != nil {
		t.Fatalf("lock after the holder released: %v", err)
	}
	next()
}
