package platesolve

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The default-catalog auto-download retries until it succeeds. Through the real
// notifier and store, a persistent failure is one alert (not one per attempt), an
// acknowledgement holds while it keeps failing, and a successful download
// resolves it.
func TestAutoDownloadFailureAlertIsOneStandingAlert(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New(nil)
	p := NewProvisioner(t.TempDir(), &model.Config{PlateSolveEnabled: true}, st, notify.New(st, h, nil, nil), nil, h, nil, nil)
	p.retryBackoff = func(int) time.Duration { return 0 }
	ctx := context.Background()

	rows := func() []model.Alert {
		all, err := st.ListAlerts(ctx, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		return all
	}
	run := func(failures int) {
		attempts := 0
		p.download = func(context.Context, string) error {
			attempts++
			if attempts <= failures {
				return fmt.Errorf("connection reset (attempt %d)", attempts)
			}
			return nil
		}
		p.autoDownloadDefault(ctx)
	}

	// Fails 8 times with changing errors, then succeeds; acknowledged mid-way.
	attempts := 0
	p.download = func(context.Context, string) error {
		attempts++
		if attempts == 5 {
			if a := rows(); len(a) == 1 {
				if err := st.AckAlert(ctx, a[0].ID, 1); err != nil {
					t.Fatal(err)
				}
			}
		}
		if attempts <= 8 {
			return fmt.Errorf("connection reset (attempt %d)", attempts)
		}
		return nil
	}
	p.autoDownloadDefault(ctx)
	if got := rows(); len(got) != 1 {
		t.Fatalf("rows after 8 failed attempts = %d, want 1", len(got))
	}
	if got := rows()[0]; got.AckedAt != 1 {
		t.Fatalf("acked_at = %d, want the operator's ack (1) kept", got.AckedAt)
	}

	// The success resolved it, so a later failing run raises anew.
	run(3)
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows after a later failing run = %d, want 2", len(got))
	}
	if n, _ := st.UnackedCount(ctx); n != 0 {
		t.Fatalf("unacked = %d, want 0: each run ended in a successful download, which resolves", n)
	}
}
