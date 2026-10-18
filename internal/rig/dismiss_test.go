package rig

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// fileUnclassifiedFrame files one genuinely-unclassified frame through the real
// pull path (a BIAS routed by <SENSOR> with the sensor map cleared → quarantine)
// and returns its recorded row, so the dismiss tests operate on a real NAS
// quarantine copy on disk.
func fileUnclassifiedFrame(t *testing.T, h *harness, src string) *model.Frame {
	t.Helper()
	h.cfg.SensorMap = map[string]string{}
	data := fixture(t, "asiair_bias_6200mm.fit")
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
	h.cycle()
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("unclassified frame %q not recorded", src)
	}
	if f.State != model.StateUnclassified {
		t.Fatalf("state=%s, want unclassified", f.State)
	}
	if f.DestPath == "" {
		t.Fatalf("unclassified frame has no dest_path")
	}
	if _, err := os.Stat(f.DestPath); err != nil {
		t.Fatalf("quarantined frame missing on disk: %v", err)
	}
	return f
}

func TestDismissTombstonesAndPurges(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
	dest := f.DestPath

	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("discardOne: %v", err)
	}

	got, ok := h.getFrame(f.SourcePath)
	if !ok {
		t.Fatal("row must survive as a tombstone")
	}
	if got.State != model.StateDismissed {
		t.Fatalf("state=%s, want dismissed", got.State)
	}
	if got.PulledAt != 0 || got.DestPath != "" {
		t.Fatalf("pulled_at/dest_path not cleared: pulled_at=%d dest=%q", got.PulledAt, got.DestPath)
	}
	if got.Size != f.Size || got.Mtime != f.Mtime {
		t.Fatalf("size/mtime must be retained: size=%d mtime=%d", got.Size, got.Mtime)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NAS copy must be purged; stat err=%v", err)
	}
	if n := h.w.countNeverDeletable(ctx); n != 0 {
		t.Fatalf("never-deletable count=%d, want 0 after dismiss", n)
	}
}

func TestDismissedFrameDecideSkipsUnchangedRepullsChanged(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("discardOne: %v", err)
	}
	cfg := h.cfgPtr.Load()

	// Unchanged listed file: skipped (already have the tombstone).
	unchanged := listedFrame{
		share: f.SourceShare, path: f.SourcePath, name: "Bias_0001.fit",
		size: f.Size, mtime: time.Unix(f.Mtime, 0),
	}
	if pull, _ := h.w.decide(ctx, cfg, h.profile, unchanged); pull {
		t.Fatal("unchanged dismissed file must not be re-pulled")
	}

	// Changed content at the same path: re-pull, and NO frame_lost (the copy was
	// deliberately purged, so there is nothing to prove).
	changed := unchanged
	changed.size = f.Size + 100
	if pull, _ := h.w.decide(ctx, cfg, h.profile, changed); !pull {
		t.Fatal("changed source at a dismissed path must be re-pulled")
	}
	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("re-pulling a changed dismissed source must not raise frame_lost; got %v", h.raisedCategories())
	}
}

func TestDismissPurgeFailureLeavesUnclassified(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")

	// Force a deterministic non-NotExist failure only for the purge Remove.
	h.lfs.RemoveErr = func(string) error { return errors.New("nas unreachable") }
	err := h.w.discardOne(ctx, f.ID)
	h.lfs.RemoveErr = nil
	if err == nil {
		t.Fatal("discardOne must return the purge error")
	}

	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateUnclassified {
		t.Fatalf("state=%s, want unclassified after purge failure", got.State)
	}
	if _, statErr := os.Stat(f.DestPath); statErr != nil {
		t.Fatalf("NAS copy must still exist after a failed purge: %v", statErr)
	}
}

func TestDismissSucceedsWhenNASCopyAlreadyGone(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")

	// The NAS copy is already gone: Remove NotExist must not fail the dismiss.
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatalf("pre-remove: %v", err)
	}
	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("discardOne with missing NAS copy: %v", err)
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDismissed {
		t.Fatalf("state=%s, want dismissed", got.State)
	}
}

func TestDismissGuardsStateAndIsIdempotent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	// A verified frame may not be dismissed.
	vf := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Light/v.fit",
		Size: 100, Mtime: 1, State: model.StateVerified, DestPath: "/nas/v.fit", PulledAt: 5,
	}
	if err := h.store.UpsertFrame(ctx, vf); err != nil {
		t.Fatal(err)
	}
	if err := h.w.discardOne(ctx, vf.ID); !errors.Is(err, ErrNotDismissable) {
		t.Fatalf("dismiss verified: err=%v, want ErrNotDismissable", err)
	}
	if got, _, _ := h.store.GetFrameByID(ctx, vf.ID); got.State != model.StateVerified {
		t.Fatalf("verified frame state changed to %s", got.State)
	}

	// An unknown frame id is not found.
	if err := h.w.discardOne(ctx, 999999); !errors.Is(err, ErrFrameNotFound) {
		t.Fatalf("dismiss unknown: err=%v, want ErrFrameNotFound", err)
	}

	// Re-dismissing an already-dismissed frame is a no-op success and never calls
	// Remove again (RemoveErr would surface any second call).
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("first dismiss: %v", err)
	}
	h.lfs.RemoveErr = func(string) error { return errors.New("must not be called") }
	defer func() { h.lfs.RemoveErr = nil }()
	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("re-dismiss must be an idempotent nil, got %v", err)
	}
}

func TestDismissedFrameDoesNotBlockPowerOff(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("discardOne: %v", err)
	}

	// The corrupt source is still on the device; a dismissed frame skipped every
	// cycle must not pin pendingWork > 0 (which would freeze the rig at Pulling).
	h.cycle()
	if st := h.status(); st.State == model.RigStatePulling {
		t.Fatalf("dismissed frame pinned state at Pulling")
	}
	// SafeYes additionally needs a stable listing across two cycles.
	h.cycle()
	st := h.status()
	if st.State != model.RigStateIdle {
		t.Fatalf("state=%s, want idle", st.State)
	}
	if st.SafeToPowerOff != model.SafeYes {
		t.Fatalf("safe_to_power_off=%s, want safe after a stable dismissed listing", st.SafeToPowerOff)
	}
}

func TestDismissSharedDestPathKeepsSiblingCopy(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	// Two unclassified rows sharing one NAS copy (the H12 dual-share case).
	dest := h.dir + "/light/unclassified/rig-01/shared.fit"
	writeLocal(t, dest, []byte("shared-corrupt-bytes"))
	mk := func(path string) *model.Frame {
		fr := &model.Frame{
			RigID: "rig-01", SourceShare: emmc, SourcePath: path,
			Size: 20, Mtime: 1, State: model.StateUnclassified, DestPath: dest, PulledAt: 5,
		}
		if err := h.store.UpsertFrame(ctx, fr); err != nil {
			t.Fatal(err)
		}
		return fr
	}
	a := mk("Autorun/Bias/a.fit")
	b := mk("Autorun/Bias/b.fit")

	// Dismissing the first leaves the shared file in place (a sibling still points at it).
	if err := h.w.discardOne(ctx, a.ID); err != nil {
		t.Fatalf("dismiss a: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("shared NAS copy must survive while a sibling references it: %v", err)
	}

	// Dismissing the second (last) row now purges it.
	if err := h.w.discardOne(ctx, b.ID); err != nil {
		t.Fatalf("dismiss b: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared NAS copy must be purged once no row references it; stat err=%v", err)
	}
}

func TestDismissClearsTransferHistorySurfaces(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")

	if _, total, err := h.store.ListTransferHistory(ctx, "rig-01", 0, 0, 0, 50); err != nil || total != 1 {
		t.Fatalf("pre-dismiss transfer history total=%d err=%v, want 1", total, err)
	}
	if n, err := h.store.CountFramesTonight(ctx, "rig-01", 0); err != nil || n != 1 {
		t.Fatalf("pre-dismiss frames-tonight=%d err=%v, want 1", n, err)
	}

	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("discardOne: %v", err)
	}

	if _, total, err := h.store.ListTransferHistory(ctx, "rig-01", 0, 0, 0, 50); err != nil || total != 0 {
		t.Fatalf("post-dismiss transfer history total=%d err=%v, want 0", total, err)
	}
	if n, err := h.store.CountFramesTonight(ctx, "rig-01", 0); err != nil || n != 0 {
		t.Fatalf("post-dismiss frames-tonight=%d err=%v, want 0", n, err)
	}
}

func TestDismissAutoAcksBacklogAlert(t *testing.T) {
	seedBacklog := func(h *harness, ctx context.Context) {
		if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
			RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertBacklog,
			Message: "never-deletable frames remain", CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			h.t.Fatal(err)
		}
	}
	backlogOpen := func(h *harness, ctx context.Context) bool {
		alerts, err := h.store.ListAlerts(ctx, false, 100)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, a := range alerts {
			if a.RigID == "rig-01" && a.Category == model.AlertBacklog {
				return true
			}
		}
		return false
	}

	t.Run("last unclassified dismissed acks the backlog", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
		seedBacklog(h, ctx)
		if err := h.w.discardOne(ctx, f.ID); err != nil {
			t.Fatalf("discardOne: %v", err)
		}
		if backlogOpen(h, ctx) {
			t.Fatal("backlog alert must be acked once the last never-deletable frame is dismissed")
		}
	})

	t.Run("a remaining unclassified frame keeps the backlog open", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f1 := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
		fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0002.fit")
		seedBacklog(h, ctx)
		if err := h.w.discardOne(ctx, f1.ID); err != nil {
			t.Fatalf("discardOne: %v", err)
		}
		if !backlogOpen(h, ctx) {
			t.Fatal("backlog alert must stay open while another unclassified frame remains")
		}
	})

	t.Run("a remaining held frame keeps the backlog open", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")
		held := &model.Frame{
			RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Light/held.fit",
			Size: 100, Mtime: 1, State: model.StateHeld, DestPath: "/nas/held.fit",
		}
		if err := h.store.UpsertFrame(ctx, held); err != nil {
			t.Fatal(err)
		}
		seedBacklog(h, ctx)
		if err := h.w.discardOne(ctx, f.ID); err != nil {
			t.Fatalf("discardOne: %v", err)
		}
		if !backlogOpen(h, ctx) {
			t.Fatal("backlog alert must stay open while a held frame remains")
		}
	})
}

// TestDiscardFrameThroughWorkerLoop exercises the full concurrency wiring —
// DiscardFrame → the dismissReqs channel → the Run select loop → discardOne — end
// to end, not just discardOne in isolation. A per-request timeout makes broken
// wiring fail fast instead of hanging the suite.
func TestDiscardFrameThroughWorkerLoop(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileUnclassifiedFrame(t, h, "Autorun/Bias/Bias_0001.fit")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.w.Run(ctx); close(done) }()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := h.w.DiscardFrame(reqCtx, f.ID)
	reqCancel()
	cancel()
	<-done

	if err != nil {
		t.Fatalf("DiscardFrame via worker loop: %v", err)
	}
	got, ok := h.getFrame(f.SourcePath)
	if !ok {
		t.Fatal("row must survive as a tombstone")
	}
	if got.State != model.StateDismissed {
		t.Fatalf("state=%s, want dismissed after dismiss through the worker loop", got.State)
	}
}
