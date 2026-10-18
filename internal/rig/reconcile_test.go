package rig

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// linkRoot links a destination root so the scan will reconcile it, and returns.
func (h *harness) linkRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.w.linkLibrary(context.Background(), root); err != nil {
		t.Fatalf("link root %s: %v", root, err)
	}
}

// setMissing marks a filed frame missing at the given age (seconds ago).
func (h *harness) setMissing(t *testing.T, id int64, ageSec int64) {
	t.Helper()
	if err := h.store.SetFrameMissing(context.Background(), id, h.nowVal.Unix()-ageSec); err != nil {
		t.Fatal(err)
	}
}

// design library-reconciliation I1: a frame confirmed missing is never
// re-pulled while the device holds the same-size content, in any state except
// failed.
func TestDecideNeverRepullsMissingFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	for _, st := range []model.FrameState{model.StateVerified, model.StateDeleted, model.StateUnclassified} {
		f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/m_" + string(st) + ".fit",
			SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: st, PulledAt: h.nowVal.Unix(),
			DestPath: filepath.Join(h.cfg.Roots.Light, "m.fit"), MissingSeenAt: h.nowVal.Unix()}
		if err := h.store.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
		// UpsertFrame clears the mark (a re-file means the file is back), so re-set it.
		if err := h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix()); err != nil {
			t.Fatal(err)
		}
		// A FAT mtime jitter (within tolerance is fine; use an out-of-tolerance shift
		// to prove even that does not re-pull) with the SAME size must not re-pull.
		lf := listedFrame{share: emmc, path: f.SourcePath, name: "m.fit", size: 10, mtime: h.nowVal.Add(time.Hour)}
		if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); pull {
			t.Fatalf("state %s: a missing frame must not be re-pulled", st)
		}
	}
}

// design library-reconciliation I2: a failed row that is marked missing still
// retries on its normal backoff.
func TestFailedRetryUnaffectedByMissing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/f.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateFailed,
		DestPath: filepath.Join(h.cfg.Roots.Light, "f.fit"), MissingSeenAt: h.nowVal.Unix(),
		NextRetryAt: h.nowVal.Add(-time.Minute).Unix()}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	_ = h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix())
	lf := listedFrame{share: emmc, path: f.SourcePath, name: "f.fit", size: 10, mtime: h.nowVal}
	if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); !pull {
		t.Fatalf("a failed+missing frame past its backoff must still retry")
	}
}

// design library-reconciliation I2a: a marked frame whose device size/mtime
// shifts WITHOUT a content change is not re-pulled by the differ branch — the
// gate is on size, before differ.
func TestDifferDoesNotResurrectOnWeakSignal(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/d.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified, PulledAt: h.nowVal.Unix(),
		DestPath: filepath.Join(h.cfg.Roots.Light, "d.fit"), MissingSeenAt: h.nowVal.Unix()}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	_ = h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix())
	// Same size, wildly different mtime (a card reader / FAT shift): still no re-pull.
	lf := listedFrame{share: emmc, path: f.SourcePath, name: "d.fit", size: 10, mtime: h.nowVal.Add(48 * time.Hour)}
	if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); pull {
		t.Fatalf("a same-size mtime shift must not resurrect a culled frame")
	}
}

// A culled frame's device original is deleted once its mark settles —
// and not one pass before.
func TestCulledFrameDeletedAfterSettle(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // operator culled the NAS copy
		t.Fatal(err)
	}

	h.cycle() // the scan marks it missing; the settle window has NOT elapsed
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a freshly culled frame must not be deleted before its settle window elapses")
	}
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt == 0 {
		t.Fatal("the scan should have marked the frame missing")
	}

	h.nowVal = h.nowVal.Add(25 * time.Hour) // past the 24h settle
	h.cycle()

	if h.deviceHas(f.SourcePath) {
		t.Fatal("once the settle window elapses the device original of a culled frame is deleted")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", got.State)
	}
}

// The device copy must still hash to what was archived. A counter-reset run that
// overwrote the path means this is NOT the frame the operator culled, so deleting
// it would destroy a never-archived frame: hold instead.
func TestCulledFrameWithChangedDeviceContentIsHeld(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.cycle() // marks it missing

	// Same size AND mtime (so the pre-delete stat still matches and the never-re-pull
	// gate holds), but different bytes — the mismatch must surface at the hash.
	mutated := append([]byte(nil), fixture(t, "asiair_light_2600mm_ha.fit")...)
	mutated[len(mutated)/2] ^= 0xFF
	h.dev.AddFile(emmc, f.SourcePath, mutated, time.Unix(f.Mtime, 0))

	h.nowVal = h.nowVal.Add(25 * time.Hour)
	h.cycle()

	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a device copy whose content changed since filing must never be deleted")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
}

// noIntents is the empty open-intent set — re-file or supersede — for the cases
// where no frame is mid-move, which is every case that predates the re-file tool.
var noIntents map[string]bool

// The authorization gate itself: every disqualifier must defer, and only a frame
// clear of all of them is authorized.
func TestAbsentDeferReasons(t *testing.T) {
	const root = `/lib`
	rootOK := map[string]bool{root: true}
	now := int64(1_000_000)
	settle := time.Hour
	settled := now - int64(settle.Seconds()) - 1

	ok := model.Frame{DestPath: root + `/a.fit`, MissingSeenAt: settled}
	if r := absentDeferReasons(ok, rootOK, noIntents, noIntents, settle, now); len(r) != 0 {
		t.Fatalf("a settled, in-root, unrequested frame must be authorized; got %v", r)
	}

	// Keyed on the dest_path being vacated, so the same otherwise-authorized frame
	// defers once a re-file announces a move of it (the wiring is proven end to end
	// by TestRefileInFlightDefersDeletion).
	if r := absentDeferReasons(ok, rootOK, map[string]bool{ok.DestPath: true}, noIntents, settle, now); !containsStr(r, "refile_in_flight") {
		t.Fatalf("reasons %v, want to contain refile_in_flight", r)
	}

	cases := []struct {
		name   string
		mutate func(*model.Frame)
		rootOK map[string]bool
		want   string
	}{
		{"not marked", func(f *model.Frame) { f.MissingSeenAt = 0 }, rootOK, "not_marked"},
		{"still settling", func(f *model.Frame) { f.MissingSeenAt = now }, rootOK, "settling"},
		{"restore requested", func(f *model.Frame) { f.RestoreRequestedAt = now }, rootOK, "restore_requested"},
		{"root unconfirmed", func(f *model.Frame) {}, map[string]bool{root: false}, "root_unconfirmed"},
		{"outside roots", func(f *model.Frame) { f.DestPath = `/elsewhere/a.fit` }, rootOK, "outside_roots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ok
			tc.mutate(&f)
			got := absentDeferReasons(f, tc.rootOK, noIntents, noIntents, settle, now)
			if len(got) == 0 {
				t.Fatalf("%s must defer, got authorization", tc.name)
			}
			found := false
			for _, r := range got {
				if r == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasons %v, want to contain %q", got, tc.want)
			}
		})
	}
}

// INV-M2 by construction: a frame whose dest_path carries an open re-file intent is
// mid-move, so its device original is never authorized for deletion — whether the
// move crashed, its commit failed, or its rename-back failed. Driven through the
// whole delete pass rather than the predicate alone, because the store read that
// feeds the predicate is the half that can silently go missing. Clearing the intent
// then deletes the original, which is what proves the deferral was the intent and
// not some other disqualifier.
func TestRefileInFlightDefersDeletion(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // the re-file renamed it away
		t.Fatal(err)
	}
	h.cycle()                               // the scan marks it missing
	h.nowVal = h.nowVal.Add(25 * time.Hour) // and the settle window elapses

	if err := h.store.PutRefileIntent(ctx, store.RefileIntent{
		RigID:     "rig-01",
		FromPath:  f.DestPath,
		ToPath:    filepath.Join(h.cfg.Roots.Light, "Sadr", filepath.Base(f.DestPath)),
		CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	// Name the reason, so a refactor cannot quietly swap it for a different
	// disqualifier that happens to defer.
	openIntents, err := h.store.OpenIntentPaths(ctx, "rig-01")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.getFrame(f.SourcePath)
	reasons := absentDeferReasons(*got, map[string]bool{h.cfg.Roots.Light: true}, openIntents, noIntents, 24*time.Hour, h.nowVal.Unix())
	if !containsStr(reasons, "refile_in_flight") {
		t.Fatalf("reasons %v, want to contain refile_in_flight", reasons)
	}

	h.cycle()
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a frame mid-re-file must never have its device original deleted")
	}

	if err := h.store.ClearRefileIntent(ctx, "rig-01", f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if h.deviceHas(f.SourcePath) {
		t.Fatal("clearing the intent must restore eligibility; nothing else was blocking this delete")
	}
}

// design library-reconciliation I13: SetFrameMissing is idempotent — a
// re-observation never restarts the clock.
func TestMarkIsIdempotent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/i.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		DestPath: filepath.Join(h.cfg.Roots.Light, "i.fit")}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	first := h.nowVal.Unix() - 1000
	_ = h.store.SetFrameMissing(ctx, f.ID, first)
	_ = h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix()) // a later re-observation
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.MissingSeenAt != first {
		t.Fatalf("re-observation restarted the settle clock: got %d want %d", got.MissingSeenAt, first)
	}
}

// design library-reconciliation I8: a name differing only by case is never
// marked missing.
func TestCaseFoldedNearMatchIsNotAbsence(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	dir := filepath.Join(h.cfg.Roots.Light, "M31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// On-disk name is upper-case; the stored path is lower-case.
	if err := os.WriteFile(filepath.Join(dir, "LIGHT.fit"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := store.ScanFrame{ID: 1, DestPath: filepath.Join(dir, "light.fit"), Base: "light.fit"}
	rr := &model.RootScanResult{}
	newly := h.w.reconcileDir(ctx, dir, []store.ScanFrame{f}, h.nowVal.Unix(), rr)
	if newly != 0 {
		t.Fatalf("a case-only near-match must not be concluded absent")
	}
}

// design library-reconciliation I9/I10: an unlinked root is skipped; after
// LinkLibrary it is confirmed; a second link adopts the existing id without
// rewriting.
func TestLinkLibrarySemantics(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	root := h.cfg.Roots.Light
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, reason := h.w.rootConfirmed(ctx, root); ok || reason != "not linked" {
		t.Fatalf("unlinked root: ok=%v reason=%q, want not linked", ok, reason)
	}
	if err := h.w.linkLibrary(ctx, root); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.w.rootConfirmed(ctx, root); !ok {
		t.Fatalf("after link the root must be confirmed")
	}
	id1, _, _ := h.store.GetMeta(ctx, LibraryIDKey(root))
	if err := h.w.linkLibrary(ctx, root); err != nil {
		t.Fatal(err)
	}
	id2, _, _ := h.store.GetMeta(ctx, LibraryIDKey(root))
	if id1 != id2 || id1 == "" {
		t.Fatalf("re-linking must adopt the existing id, not mint a new one: %q -> %q", id1, id2)
	}
}

// design library-reconciliation I14: a file that reappears has its mark cleared
// by the scan.
func TestReappearanceClearsMark(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt == 0 {
		t.Fatalf("scan should have marked the frame missing")
	}
	// The file reappears (restored from backup); the next scan clears the mark.
	if err := os.WriteFile(f.DestPath, fixture(t, "asiair_light_2600mm_ha.fit"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.nowVal = h.nowVal.Add(7 * time.Hour) // past reconcileInterval so the scan runs again
	h.cycle()
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt != 0 {
		t.Fatalf("a reappeared file's mark must be cleared; got missing_seen_at=%d", got.MissingSeenAt)
	}
}

// design library-reconciliation I19: a currently-missing row is never pruned.
func TestPruneDeletedSkipsMissingRows(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	old := h.nowVal.Add(-30 * 24 * time.Hour).Unix()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/p.fit",
		SHA256: "abc", Size: 10, Mtime: old, State: model.StateDeleted, DeletedAt: old,
		DestPath: filepath.Join(h.cfg.Roots.Light, "p.fit"), MissingSeenAt: old}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	_ = h.store.SetFrameMissing(ctx, f.ID, old)
	n, err := h.store.PruneDeleted(ctx, h.nowVal.Unix(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a deleted+missing row must not be pruned; pruned %d", n)
	}
}

// design library-reconciliation I15: UpsertFrame and DismissFrame clear the
// missing columns; a re-file means the file is back so any mark is stale.
func TestMissingColumnsLifecycle(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/l.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		DestPath: filepath.Join(h.cfg.Roots.Light, "l.fit")}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	_ = h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix())
	_, _ = h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix())
	// Re-file (a fresh upsert): both columns clear.
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.MissingSeenAt != 0 || got.RestoreRequestedAt != 0 {
		t.Fatalf("re-file must clear both missing columns; got missing=%d restore=%d", got.MissingSeenAt, got.RestoreRequestedAt)
	}

	// A re-file must also clear missing_acked_at (design library-reconciliation
	// §10.5). The ack cannot coexist with a pending restore, so this is a separate
	// arrangement: mark missing, acknowledge (no restore), then re-file.
	_ = h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix())
	if ok, err := h.store.AcknowledgeMissing(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, _, _ = h.store.GetFrameByID(ctx, f.ID)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 {
		t.Fatalf("re-file must clear missing_acked_at; got missing=%d acked=%d", got.MissingSeenAt, got.MissingAckedAt)
	}
}

// A requested restore of a frame whose NAS copy is gone re-pulls the original from
// the device, then clears the missing mark and the request.
func TestRestoreRepullsAndClears(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	if err := os.Remove(f.DestPath); err != nil { // operator culled the NAS copy
		t.Fatal(err)
	}
	h.setMissing(t, f.ID, 0)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}

	h.cycle() // restorePhase re-pulls from the still-resident device original

	if _, err := os.Stat(f.DestPath); err != nil {
		t.Fatalf("restore should have re-pulled the NAS copy: %v", err)
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.MissingSeenAt != 0 || got.RestoreRequestedAt != 0 {
		t.Fatalf("a completed restore must clear both marks; got missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
}

// A restore whose device content no longer matches what was filed must HOLD the
// frame, not report success: the missing mark and the operator's request must
// survive so the frame is not silently treated as restored.
func TestRestoreHeldOnDeviceMismatch(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, f.ID, 0)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}
	// The device file's content has changed since filing but is the SAME size — so
	// the never-re-pull size gate keeps the pull pass from re-filing it, and the
	// mismatch surfaces where it matters: inside restoreNAS's content check.
	mutated := append([]byte(nil), fixture(t, "asiair_light_2600mm_ha.fit")...)
	mutated[len(mutated)/2] ^= 0xFF // same length, different hash
	h.dev.AddFile(emmc, f.SourcePath, mutated, h.nowVal)

	h.cycle()

	if _, err := os.Stat(f.DestPath); err == nil {
		t.Fatalf("a content-mismatched restore must not write the NAS copy")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.MissingSeenAt == 0 || got.RestoreRequestedAt == 0 {
		t.Fatalf("a held restore must keep the mark and request; got missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
	if got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
}

// Auto-link (design library-reconciliation §8): filing and verifying a frame into
// a root links it automatically, with no operator action — the root is unlinked
// before the first frame and confirmed after.
func TestAutoLinkOnFiledFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	if ok, reason := h.w.rootConfirmed(ctx, h.cfg.Roots.Light); ok || reason != "not linked" {
		t.Fatalf("before any frame the root must be unlinked; ok=%v reason=%q", ok, reason)
	}
	fileAndAge(t, h, false) // files + verifies a light frame into the Light root
	if ok, reason := h.w.rootConfirmed(ctx, h.cfg.Roots.Light); !ok {
		t.Fatalf("a verified filed frame must auto-link its root; reason=%q", reason)
	}
	// A deleted marker with the id still recorded is NOT silently recreated: it is
	// the recovery case, left for the alert + a manual re-link (design
	// library-reconciliation §8.2/§8.3).
	if err := h.lfs.Remove(filepath.Join(h.cfg.Roots.Light, model.LibraryMarkerName)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	h.w.ensuredRoots = map[string]bool{} // force a re-evaluation as if a new session
	fileAndAge(t, h, false)              // another verified file into the same root
	if ok, _ := h.w.rootConfirmed(ctx, h.cfg.Roots.Light); ok {
		t.Fatal("a deleted marker must not be silently recreated by auto-link")
	}
}

// A drained/existing library (filed frames on disk, no fresh frame coming to fire
// the file-path auto-link) is linked by the scan, which re-verifies a filed frame.
func TestAutoLinkFromExistingDataOnScan(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false) // files a verified frame (and auto-links the root)
	root := h.cfg.Roots.Light

	// Simulate a pre-auto-link install: the frame is filed, but the root carries no
	// recorded id and no marker.
	if err := h.store.SetMeta(ctx, LibraryIDKey(root), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.lfs.Remove(filepath.Join(root, model.LibraryMarkerName)); err != nil {
		t.Fatal(err)
	}
	h.w.ensuredRoots = map[string]bool{}
	if ok, _ := h.w.rootConfirmed(ctx, root); ok {
		t.Fatal("precondition: the root should read as unlinked")
	}

	h.nowVal = h.nowVal.Add(7 * time.Hour) // past reconcileInterval
	h.w.scanLibrary(ctx, h.cfg, h.profile)

	if ok, reason := h.w.rootConfirmed(ctx, root); !ok {
		t.Fatalf("a scan must auto-link a root holding existing verified frames; reason=%q", reason)
	}
	_ = f
}

// A rig-unreachable alert auto-clears when the rig comes back online, instead of
// lingering until the operator dismisses it.
func TestUnreachableAlertClearsOnReconnect(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
		RigID: "rig-01", Category: model.AlertUnreachable, Severity: model.SeverityWarn,
		Message: "rig unreachable", CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	h.w.wasOnline = false // it was offline; a cycle now finds it reachable
	h.w.markOnline(ctx)

	alerts, err := h.store.ListAlerts(ctx, false, 100) // unacked only
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.Category == model.AlertUnreachable {
			t.Fatal("the unreachable alert must be auto-acked when the rig reconnects")
		}
	}
}

// design library-reconciliation I22 (scheduling half): a paused rig does not scan
// even under a run-now override.
func TestScanSkippedWhenPaused(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.w.paused = true
	if ran := h.w.scanLibrary(context.Background(), h.cfg, h.profile); ran {
		t.Fatalf("a paused rig must not scan")
	}
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt != 0 {
		t.Fatalf("a paused scan must mark nothing")
	}
}

// design library-reconciliation §2.7's second evaluation: hashing the device copy
// streams ~116 MB and takes tens of seconds, so the authorization must be
// re-established from FRESH state immediately before the delete. These drive
// handleAbsentNAS with an authorized-looking snapshot while the real state has
// moved on underneath — which is exactly what a mid-hash Restore or a reappearing
// copy produces.
func TestCulledFrameDeleteRechecksFreshStateAfterHash(t *testing.T) {
	settleWindow := 24 * time.Hour
	rootOf := func(h *harness) map[string]bool {
		return map[string]bool{h.cfg.Roots.Light: true}
	}

	// A Restore requested while the hash was streaming must stop the delete, even
	// though the snapshot the pass started from was fully authorized.
	t.Run("restore requested during the hash", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f, _ := fileAndAge(t, h, false)
		h.linkRoot(t, h.cfg.Roots.Light)
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.cycle()                               // marks it missing
		h.nowVal = h.nowVal.Add(25 * time.Hour) // and it settles

		got, _ := h.getFrame(f.SourcePath)
		stale := *got // an authorized snapshot: settled, no restore pending
		if ok, err := h.store.RequestRestore(ctx, "rig-01", got.ID, h.nowVal.Unix()); err != nil || !ok {
			t.Fatalf("request restore: ok=%v err=%v", ok, err)
		}

		h.w.handleAbsentNAS(ctx, h.localTarget(), h.cfg, h.mountShare(emmc), stale, rootOf(h), nil, nil, settleWindow, deviceDigests{})

		if !h.deviceHas(f.SourcePath) {
			t.Fatal("a restore requested during the hash must stop the delete — that copy is the operator's last one")
		}
	})

	// The NAS copy coming back during the hash removes the premise of the branch:
	// don't delete, and clear the now-stale mark.
	t.Run("NAS copy reappears during the hash", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f, _ := fileAndAge(t, h, false)
		h.linkRoot(t, h.cfg.Roots.Light)
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.cycle()
		h.nowVal = h.nowVal.Add(25 * time.Hour)

		got, _ := h.getFrame(f.SourcePath)
		stale := *got
		// The operator restored it by hand (or a backup did) mid-hash.
		if err := os.WriteFile(f.DestPath, fixture(t, "asiair_light_2600mm_ha.fit"), 0o644); err != nil {
			t.Fatal(err)
		}

		h.w.handleAbsentNAS(ctx, h.localTarget(), h.cfg, h.mountShare(emmc), stale, rootOf(h), nil, nil, settleWindow, deviceDigests{})

		if !h.deviceHas(f.SourcePath) {
			t.Fatal("a copy that reappeared during the hash must not be treated as culled")
		}
		if after, _ := h.getFrame(f.SourcePath); after.MissingSeenAt != 0 {
			t.Fatal("the stale missing mark must be cleared once the copy is back")
		}
	})
}

// --- culled-original drain for proven-archived unclassified frames -------------

// fileUnclassifiedAndAge files one frame that cannot be classified (a BIAS routed by
// <SENSOR> with the sensor map cleared → quarantine, design routing-and-filing §2),
// advancing the clock past grace so a culled copy is deletion-eligible via
// DeletionEligible's unclassified branch. It mirrors fileAndAge but lands the frame in
// the unclassified quarantine.
func fileUnclassifiedAndAge(t *testing.T, h *harness) *model.Frame {
	t.Helper()
	h.cfg.SensorMap = map[string]string{}
	data := fixture(t, "asiair_bias_6200mm.fit")
	src := "Autorun/Bias/Bias_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
	h.cycle()
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("unclassified frame not filed")
	}
	if f.State != model.StateUnclassified {
		t.Fatalf("state=%s, want unclassified", f.State)
	}
	if f.SHA256 == "" {
		t.Fatalf("an unclassified frame must still carry a recorded sha256 (the device anchor)")
	}
	h.nowVal = h.nowVal.Add(2 * time.Hour) // past the 1h grace
	return f
}

// design library-reconciliation I32/I34 (AC1): a culled unclassified frame with a
// recorded sha256, past grace and settle, whose device re-hash equals f.SHA256 under
// a confirmed root, has its device original deleted — exactly as a verified cull, and
// not one pass before settle.
func TestCulledUnclassifiedFrameDeletedAfterSettle(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileUnclassifiedAndAge(t, h)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // operator culled the quarantine copy
		t.Fatal(err)
	}

	h.cycle() // the scan marks it missing; the settle window has NOT elapsed
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a freshly culled unclassified frame must not be deleted before its settle window elapses")
	}
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt == 0 {
		t.Fatal("the scan should have marked the unclassified frame missing")
	}

	h.nowVal = h.nowVal.Add(25 * time.Hour) // past the 24h settle
	h.cycle()

	if h.deviceHas(f.SourcePath) {
		t.Fatal("once the settle window elapses the device original of a culled unclassified frame is deleted")
	}
	if got, _ := h.getFrame(f.SourcePath); got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", got.State)
	}
}

// design library-reconciliation I33 (AC2): an unclassified frame whose NAS copy
// reappears after being marked missing is NOT deleted; its missing_seen_at is cleared
// and it does not linger in DeletionEligible. Driven through deleteOne directly so the
// delete pass — not the scan — is the actor, exercising the verified-only
// normal-cleanup guard that sits after the mark-clear (delete.go).
func TestReappearedUnclassifiedCopyNotDeletedAndMarkCleared(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileUnclassifiedAndAge(t, h)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.cycle() // the scan marks it missing
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt == 0 {
		t.Fatal("the scan should have marked the unclassified frame missing")
	}
	// The NAS copy reappears (a hand restore / backup) while the mark still stands, so
	// the frame is still in DeletionEligible when the delete pass runs.
	if err := os.WriteFile(f.DestPath, fixture(t, "asiair_bias_6200mm.fit"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootOK := map[string]bool{h.cfg.Roots.Light: true}
	h.w.deleteOne(ctx, h.localTarget(), h.mountedSession(t), f.ID, rootOK, nil, nil, 24*time.Hour)

	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a reappeared unclassified copy must never have its device original deleted")
	}
	after, _ := h.getFrame(f.SourcePath)
	if after.State != model.StateUnclassified {
		t.Fatalf("state=%s, want unclassified (not deleted, not held)", after.State)
	}
	if after.MissingSeenAt != 0 {
		t.Fatal("the stale missing mark must be cleared once the copy is back")
	}
	elig, err := h.store.DeletionEligible(ctx, "rig-01", h.nowVal.Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range elig {
		if e.ID == f.ID {
			t.Fatal("a reappeared unclassified frame must not linger in DeletionEligible")
		}
	}
}

// design library-reconciliation I32 (AC5): a device re-hash mismatch aborts the
// delete for a culled unclassified frame and holds it — a counter-reset run
// overwrote the path, so this is not the frame that was culled. Mirrors
// TestCulledFrameWithChangedDeviceContentIsHeld.
func TestCulledUnclassifiedWithChangedDeviceContentIsHeld(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileUnclassifiedAndAge(t, h)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.cycle() // marks it missing

	// Same size AND mtime (so the pre-delete stat still matches and the never-re-pull
	// gate holds), but different bytes — the mismatch must surface at the device re-hash.
	mutated := append([]byte(nil), fixture(t, "asiair_bias_6200mm.fit")...)
	mutated[len(mutated)/2] ^= 0xFF
	h.dev.AddFile(emmc, f.SourcePath, mutated, time.Unix(f.Mtime, 0))

	h.nowVal = h.nowVal.Add(25 * time.Hour)
	h.cycle()

	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a device copy whose content changed since filing must never be deleted")
	}
	if got, _ := h.getFrame(f.SourcePath); got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
}

// design library-reconciliation I34 (AC4): the absentDeferReasons gates apply to a
// culled unclassified frame exactly as to a verified one — a not-yet-settled frame and
// an unconfirmed-root frame both defer. (absentDeferReasons itself is state-agnostic
// and covered per-reason by TestAbsentDeferReasons; this drives the gates through the
// unclassified drain.)
func TestCulledUnclassifiedRespectsDeferReasons(t *testing.T) {
	t.Run("still settling defers", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileUnclassifiedAndAge(t, h)
		h.linkRoot(t, h.cfg.Roots.Light)
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.cycle() // marks missing; settle not elapsed
		h.cycle() // a second pass, still inside settle
		if !h.deviceHas(f.SourcePath) {
			t.Fatal("a still-settling unclassified cull must defer, not delete")
		}
	})
	t.Run("root unconfirmed defers", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f := fileUnclassifiedAndAge(t, h)
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.cycle()                               // marks missing (the root auto-links on filing)
		h.nowVal = h.nowVal.Add(25 * time.Hour) // past settle
		got, _ := h.getFrame(f.SourcePath)
		// Drive the drain with the root explicitly UNCONFIRMED: the gate must defer even
		// though settle has elapsed, exactly as it does for a verified cull.
		unconfirmed := map[string]bool{h.cfg.Roots.Light: false}
		h.w.handleAbsentNAS(ctx, h.localTarget(), h.cfg, h.mountShare(emmc), *got, unconfirmed, nil, nil, 24*time.Hour, deviceDigests{})
		if !h.deviceHas(f.SourcePath) {
			t.Fatal("an unclassified cull under an unconfirmed root must defer, not delete")
		}
	})
}

// design library-reconciliation I36 (AC7): a culled (missing) unclassified frame no
// longer counts toward the never-deletable backlog alert — it is on its way off the
// device via the cull drain — while a non-culled unclassified frame still counts, as
// does a held frame.
func TestCountNeverDeletableExcludesCulledUnclassified(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	keep := fileUnclassifiedAndAge(t, h)
	if n := h.w.countNeverDeletable(ctx); n != 1 {
		t.Fatalf("a non-culled unclassified frame must count as never-deletable; got %d", n)
	}

	// Cull it (mark missing): now excluded from the count.
	h.setMissing(t, keep.ID, 60)
	if n := h.w.countNeverDeletable(ctx); n != 0 {
		t.Fatalf("a culled unclassified frame must not count as never-deletable; got %d", n)
	}

	// A held frame always counts (device bytes mismatched — never removed), even while
	// the culled unclassified frame does not.
	held := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/h.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateHeld,
		DestPath: filepath.Join(h.cfg.Roots.Light, "h.fit")}
	if err := h.store.UpsertFrame(ctx, held); err != nil {
		t.Fatal(err)
	}
	if n := h.w.countNeverDeletable(ctx); n != 1 {
		t.Fatalf("held must count even when the culled unclassified does not; got %d", n)
	}
}

// The library_reconciled alert names the folder relative to its library root and
// which root it is, never the raw absolute path: the operator knows the library by
// its layout, and the absolute mount point is noise (design library-reconciliation
// §4.8).
func TestLibraryReconciledAlertNamesFolderUnderItsRoot(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // the operator culled the NAS copy
		t.Fatal(err)
	}
	h.cycle()

	rel, err := filepath.Rel(h.cfg.Roots.Light, filepath.Dir(f.DestPath))
	if err != nil {
		t.Fatal(err)
	}
	want := "1 frame removed outside the app from " + filepath.ToSlash(rel) +
		" in the light library (observed " + h.nowVal.Format("2006-01-02") + ")"
	var got []string
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertLibraryReconciled {
			got = append(got, a.Message)
		}
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("library_reconciled messages = %q, want [%q]", got, want)
	}
}

func TestLibraryFolderLabel(t *testing.T) {
	roots := model.Roots{Light: "/srv/astro/light", Cal: "/srv/astro/cal"}
	for _, tc := range []struct{ root, dir, want string }{
		{"/srv/astro/light", "/srv/astro/light/M42/FRA300/NIGHT_2", "M42/FRA300/NIGHT_2 in the light library"},
		{"/srv/astro/cal", "/srv/astro/cal/flat", "flat in the calibration library"},
		{"/srv/astro/light", "/srv/astro/light", "the top of the light library"},
	} {
		if got := libraryFolderLabel(roots, tc.root, tc.dir); got != tc.want {
			t.Errorf("libraryFolderLabel(%q, %q) = %q, want %q", tc.root, tc.dir, got, tc.want)
		}
	}
}
