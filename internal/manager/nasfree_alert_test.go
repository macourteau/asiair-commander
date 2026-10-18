package manager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/notify"
)

// The free-space read is retried every maintenance tick. Through the real
// notifier and store, an acknowledged failure that persists stays acknowledged,
// and only a failure that clears and returns raises a new alert.
func TestNASFreeSpaceAlertStaysAcknowledgedUntilAReadSucceeds(t *testing.T) {
	cfg := testConfig(t)
	cfg.PasswordHash = "x"
	cfg.Roots.Light = filepath.Join(t.TempDir(), "light")
	lf := localfs.NewFake(t.TempDir())
	m, st := newManagerWithSeams(t, cfg, lf, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.notify = notify.New(st, m.hub, nil, nil)
	ctx := context.Background()
	m.baseCtx = ctx // set by Start, which this test does not run

	rows := func() []model.Alert {
		all, err := st.ListAlerts(ctx, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		return all
	}

	lf.FreeSpaceErr = errors.New("no such file or directory")
	for range 5 {
		m.publishNASFree()
	}
	if got := rows(); len(got) != 1 {
		t.Fatalf("rows after 5 failing ticks = %d, want 1", len(got))
	}
	if err := st.AckAlert(ctx, rows()[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		m.publishNASFree()
	}
	if got := rows(); len(got) != 1 {
		t.Fatalf("rows after ack and 5 more failing ticks = %d, want 1", len(got))
	}

	lf.FreeSpaceErr = nil
	m.publishNASFree()
	lf.FreeSpaceErr = errors.New("no such file or directory")
	m.publishNASFree()
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows after recover and fail again = %d, want 2", len(got))
	}
	if n, _ := st.UnackedCount(ctx); n != 1 {
		t.Fatalf("unacked = %d, want 1", n)
	}
}
