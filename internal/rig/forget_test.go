package rig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// seedRow upserts a rig-01 frame in the given state and, when marked, sets its
// missing mark, through the real store path.
func (h *harness) seedRow(t *testing.T, name string, state model.FrameState, marked bool) int64 {
	t.Helper()
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/" + name + ".fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: state,
		DestPath: filepath.Join(h.cfg.Roots.Light, name+".fit")}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	if marked {
		h.setMissing(t, f.ID, 60)
	}
	return f.ID
}

// design library-reconciliation I38: the worker forgets only this rig's marked
// deleted rows with no restore pending, refusing everything else with a sentinel
// and leaving the row untouched.
func TestServiceForgetGuards(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	leftover := h.seedRow(t, "left", model.StateDeleted, true)
	if err := h.w.serviceForget(ctx, leftover); err != nil {
		t.Fatalf("forget a leftover: %v", err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, leftover)
	if got.MissingSeenAt != 0 || got.DestPath != "" || got.State != model.StateDeleted {
		t.Fatalf("forgotten row: seen=%d dest=%q state=%s, want unmarked, no dest, still deleted",
			got.MissingSeenAt, got.DestPath, got.State)
	}
	if err := h.w.serviceForget(ctx, leftover); !errors.Is(err, ErrNotLeftover) {
		t.Fatalf("forgetting an already-forgotten row: got %v, want ErrNotLeftover", err)
	}

	for _, c := range []struct {
		name   string
		state  model.FrameState
		marked bool
	}{
		{"verified", model.StateVerified, true},
		{"unclassified", model.StateUnclassified, true},
		{"held", model.StateHeld, true},
		{"unmarked-deleted", model.StateDeleted, false},
	} {
		id := h.seedRow(t, c.name, c.state, c.marked)
		before, _, _ := h.store.GetFrameByID(ctx, id)
		if err := h.w.serviceForget(ctx, id); !errors.Is(err, ErrNotLeftover) {
			t.Errorf("%s: got %v, want ErrNotLeftover", c.name, err)
		}
		after, _, _ := h.store.GetFrameByID(ctx, id)
		if after.DestPath != before.DestPath || after.MissingSeenAt != before.MissingSeenAt {
			t.Errorf("%s: a refused forget changed the row", c.name)
		}
	}

	rp := h.seedRow(t, "rp", model.StateDeleted, true)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", rp, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	if err := h.w.serviceForget(ctx, rp); !errors.Is(err, ErrRestorePending) {
		t.Fatalf("restore-pending: got %v, want ErrRestorePending", err)
	}

	// Another rig's frame is not this worker's to forget.
	other := &model.Frame{RigID: "rig-02", SourceShare: emmc, SourcePath: "Autorun/o.fit",
		SHA256: "abc", Size: 10, Mtime: 1, State: model.StateDeleted,
		DestPath: filepath.Join(h.cfg.Roots.Light, "o.fit")}
	if err := h.store.UpsertFrame(ctx, other); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, other.ID, 60)
	if err := h.w.serviceForget(ctx, other.ID); !errors.Is(err, ErrFrameNotFound) {
		t.Fatalf("cross-rig forget: got %v, want ErrFrameNotFound", err)
	}
	if err := h.w.serviceForget(ctx, 999999); !errors.Is(err, ErrFrameNotFound) {
		t.Fatalf("unknown frame: got %v, want ErrFrameNotFound", err)
	}

	// The bulk path applies the same guard to its bound ids and reports the rows
	// actually changed.
	l2 := h.seedRow(t, "left2", model.StateDeleted, true)
	ver := h.seedRow(t, "ver2", model.StateVerified, true)
	n, err := h.w.serviceForgetIDs(ctx, "", 0, []int64{l2, ver, rp, other.ID})
	if err != nil || n != 1 {
		t.Fatalf("bulk forget: n=%d err=%v, want 1", n, err)
	}
}

// design library-reconciliation I39 / AC-T3: a forgotten leftover is not re-marked
// by the next scan, raises no alert, and is removed by PruneDeleted; a source that
// later reappears on the device is re-adopted like any frame after a grace delete.
func TestForgottenRowNotRemarkedAndPruned(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := makeLeftover(t, h)

	if err := h.w.serviceForget(ctx, f.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}

	before := h.countAlerts(model.AlertLibraryReconciled)
	h.nowVal = h.nowVal.Add(7 * time.Hour) // past reconcileInterval
	h.w.scanLibrary(ctx, h.cfg, h.profile)
	got, ok := h.getFrame(f.SourcePath)
	if !ok || got.MissingSeenAt != 0 {
		t.Fatalf("a forgotten row must not be re-marked by a later scan; got %+v", got)
	}
	if after := h.countAlerts(model.AlertLibraryReconciled); after != before {
		t.Fatalf("a forgotten row must raise no library_reconciled alert; before=%d after=%d", before, after)
	}

	n, err := h.store.PruneDeleted(ctx, h.nowVal.Unix(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("PruneDeleted removed %d rows, want the forgotten one", n)
	}
	if _, ok := h.getFrame(f.SourcePath); ok {
		t.Fatal("the forgotten row must be pruned")
	}
}

// AC-T3: Forget can put a culled frame back on the NAS but never lose one — a
// forgotten row whose device source reappears is re-adopted and pulled.
func TestForgottenLeftoverReadoptedWhenSourceReappears(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := makeLeftover(t, h)

	if err := h.w.serviceForget(ctx, f.ID); err != nil {
		t.Fatalf("forget: %v", err)
	}
	// A card still holding the original is reinserted: same bytes, same mtime.
	h.dev.AddFile(emmc, f.SourcePath, fixture(t, "asiair_light_2600mm_ha.fit"), time.Unix(f.Mtime, 0))
	h.cycle()

	got, _ := h.getFrame(f.SourcePath)
	if got.DestPath == "" || got.MissingSeenAt != 0 {
		t.Fatalf("a reappeared source must be re-adopted; dest=%q seen=%d state=%s",
			got.DestPath, got.MissingSeenAt, got.State)
	}
	if _, err := os.Stat(got.DestPath); err != nil {
		t.Fatalf("the re-adopted frame must be back on the NAS: %v", err)
	}
}

// makeLeftover files a frame, lets the grace delete remove its device original,
// then culls its NAS copy and scans, producing a marked `deleted` row: a leftover
// (design library-reconciliation §13.1).
func makeLeftover(t *testing.T, h *harness) *model.Frame {
	t.Helper()
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	h.cycle() // grace delete
	if h.deviceHas(f.SourcePath) {
		t.Fatal("precondition: the grace delete should have removed the device original")
	}
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.nowVal = h.nowVal.Add(7 * time.Hour)
	h.w.scanLibrary(ctx, h.cfg, h.profile)
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted || got.MissingSeenAt == 0 || got.DestPath == "" {
		t.Fatalf("precondition: want a marked deleted row; state=%s seen=%d dest=%q",
			got.State, got.MissingSeenAt, got.DestPath)
	}
	return got
}

// design library-reconciliation §13.4: Forget is serviced on the worker goroutine.
// ForgetFrame and ForgetLeftovers go through the restore request channel and the
// Run select into serviceForget and serviceForgetIDs, end to end, refusals
// included. A per-request timeout makes broken wiring fail fast instead of
// hanging the suite.
func TestForgetThroughWorkerLoop(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	one := h.seedRow(t, "one", model.StateDeleted, true)
	a := h.seedRow(t, "a", model.StateDeleted, true)
	b := h.seedRow(t, "b", model.StateDeleted, true)
	ver := h.seedRow(t, "ver", model.StateVerified, true)
	rp := h.seedRow(t, "rp", model.StateDeleted, true)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", rp, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { h.w.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	call := func(fn func(context.Context) error) error {
		reqCtx, reqCancel := context.WithTimeout(ctx, 5*time.Second)
		defer reqCancel()
		return fn(reqCtx)
	}

	if err := call(func(c context.Context) error { return h.w.ForgetFrame(c, one) }); err != nil {
		t.Fatalf("ForgetFrame via worker loop: %v", err)
	}
	if err := call(func(c context.Context) error { return h.w.ForgetFrame(c, rp) }); !errors.Is(err, ErrRestorePending) {
		t.Fatalf("ForgetFrame on a restore-pending row: got %v, want ErrRestorePending", err)
	}
	var n int
	if err := call(func(c context.Context) (err error) {
		n, err = h.w.ForgetLeftovers(c, "", 7, []int64{a, b, ver, rp})
		return err
	}); err != nil || n != 2 {
		t.Fatalf("ForgetLeftovers via worker loop: n=%d err=%v, want 2", n, err)
	}

	for _, id := range []int64{one, a, b} {
		got, _, _ := h.store.GetFrameByID(ctx, id)
		if got.MissingSeenAt != 0 || got.DestPath != "" {
			t.Errorf("frame %d: seen=%d dest=%q, want forgotten", id, got.MissingSeenAt, got.DestPath)
		}
	}
	for _, id := range []int64{ver, rp} {
		got, _, _ := h.store.GetFrameByID(ctx, id)
		if got.MissingSeenAt == 0 || got.DestPath == "" {
			t.Errorf("frame %d (%s, restore %d): a refused row must keep its mark and path",
				id, got.State, got.RestoreRequestedAt)
		}
	}
}
