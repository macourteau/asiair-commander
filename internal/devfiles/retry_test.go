package devfiles

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
)

func TestRetryPolicy(t *testing.T) {
	ctx := context.Background()

	t.Run("two network failures then success", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).FailNext(device.FakeOpListDir, 2, errNetworkForTest())
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := fx.opens.Load(); got != 3 {
			t.Errorf("opens = %d, want 3 (each retry on a fresh session)", got)
		}
		if got := fx.closes.Load(); got != 2 {
			t.Errorf("closes = %d, want 2 (failed sessions closed)", got)
		}
	})

	t.Run("four failures are unreachable, naming the rig", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).FailNext(device.FakeOpListDir, 4, errNetworkForTest())
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		e := wantKind(t, err, ErrUnreachable)
		if e.Rig != "r1" || !strings.Contains(e.Msg, "Rig One") || !strings.Contains(e.Msg, "4 times") {
			t.Errorf("error = %+v, want r1 named, tried 4 times", e)
		}
		if got := fx.opens.Load(); got != 4 {
			t.Errorf("opens = %d, want 4", got)
		}
	})

	t.Run("a failed open is retried", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).FailNext(device.FakeOpOpen, 3, errNetworkForTest())
		if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := fx.opens.Load(); got != 4 {
			t.Errorf("opens = %d, want 4", got)
		}
	})

	t.Run("a non-network error is not retried", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).FailNext(device.FakeOpListDir, 1, &os.PathError{Op: "listdir", Path: "", Err: os.ErrPermission})
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		wantKind(t, err, ErrDenied)
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1", got)
		}
		if got := fx.closes.Load(); got != 0 {
			t.Errorf("closes = %d, want 0 (a healthy session is parked)", got)
		}
	})

	t.Run("a refused logon is returned at once", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).FailSessionOpen(errors.New("SMB authentication: STATUS_LOGON_FAILURE"))
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		if err == nil || errors.Is(err, ErrUnreachable) {
			t.Fatalf("error = %v, want the logon failure itself", err)
		}
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1", got)
		}
	})

	t.Run("no known address is unreachable at once", func(t *testing.T) {
		fx := newFixture(t)
		fx.setAddress("r1", "")
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		e := wantKind(t, err, ErrUnreachable)
		if e.Msg != "Commander doesn't know where Rig One is yet" {
			t.Errorf("message = %q", e.Msg)
		}
		if got := fx.opens.Load(); got != 0 {
			t.Errorf("opens = %d, want 0", got)
		}
	})

	t.Run("an unreachable local rig fails within the open budget", func(t *testing.T) {
		fx := newFixture(t)
		fx.blocked[ip1] = true
		fx.svc.openBase = 30 * time.Millisecond
		start := time.Now()
		_, err := fx.svc.List(ctx, "r1", emmc, "")
		elapsed := time.Since(start)
		wantKind(t, err, ErrUnreachable)
		if got := fx.opens.Load(); got != 4 {
			t.Errorf("opens = %d, want 4", got)
		}
		// Four opens bounded by the budget plus 3 ms of backoff, with slack.
		if elapsed < 4*30*time.Millisecond || elapsed > time.Second {
			t.Errorf("elapsed %v, want about 4 x 30 ms", elapsed)
		}
	})

	t.Run("a cancel during a backoff is cancelled, not unreachable", func(t *testing.T) {
		fx := newFixture(t)
		fx.svc.backoffs = []time.Duration{time.Hour, time.Hour, time.Hour}
		fx.dev(ip1).FailNext(device.FakeOpListDir, 1, errNetworkForTest())
		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(20*time.Millisecond, cancel)
		start := time.Now()
		_, err := fx.svc.List(cctx, "r1", emmc, "")
		wantKind(t, err, ErrCancelled)
		if time.Since(start) > time.Second {
			t.Error("the backoff was not cut short by the cancel")
		}
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1", got)
		}
	})

	t.Run("a cancel during an attempt is cancelled, not retried", func(t *testing.T) {
		fx := newFixture(t)
		cctx, cancel := context.WithCancel(ctx)
		fx.dev(ip1).OnListDir = func(string, string) { cancel() }
		fx.dev(ip1).FailNext(device.FakeOpListDir, 1, errNetworkForTest())
		_, err := fx.svc.List(cctx, "r1", emmc, "")
		wantKind(t, err, ErrCancelled)
		if got := fx.opens.Load(); got != 1 {
			t.Errorf("opens = %d, want 1", got)
		}
	})

	t.Run("exhaustion names the side that failed last", func(t *testing.T) {
		fx := newFixture(t)
		unlock, err := fx.svc.lockOp(ctx, "Copy", "r1", "r2")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		calls := 0
		err = fx.svc.withRetry(ctx, "Copy", emmc, []string{"r1", "r2"}, func(ss []*session) error {
			calls++
			return onRig(ss[1].rigID, errNetworkForTest())
		})
		e := wantKind(t, err, ErrUnreachable)
		if e.Rig != "r2" || !strings.Contains(e.Msg, "Rig Two") {
			t.Errorf("error = %+v, want r2 named", e)
		}
		if calls != 4 {
			t.Errorf("calls = %d, want 4", calls)
		}
		if got := fx.opens.Load(); got != 8 {
			t.Errorf("opens = %d, want 8 (both sessions replaced on every retry)", got)
		}
	})
}

// A panic in an operation's body still returns its rig slots, and closes the
// sessions rather than parking them: their state is unknown.
func TestPanicReleasesSessions(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	fx.dev(ip1)
	fx.dev(ip2)
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the operation's panic", r)
			}
		}()
		_ = fx.svc.withRetry(ctx, "Test", emmc, []string{"r1", "r2"}, func([]*session) error {
			panic("boom")
		})
	}()
	if got := fx.closes.Load(); got != 2 {
		t.Errorf("closes = %d, want 2 (both sessions closed, not parked)", got)
	}
	fx.svc.mu.Lock()
	for _, id := range []string{"r1", "r2"} {
		p := fx.svc.pools[id]
		if n := len(p.slots); n != 0 {
			t.Errorf("%s: %d slots held, want 0", id, n)
		}
		if p.idle != nil {
			t.Errorf("%s: a session was parked", id)
		}
	}
	fx.svc.mu.Unlock()
	// The rig is usable again.
	if _, err := fx.svc.List(ctx, "r1", emmc, ""); err != nil {
		t.Errorf("List after the panic: %v", err)
	}
}
