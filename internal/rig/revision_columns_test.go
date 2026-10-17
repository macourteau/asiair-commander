package rig

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// assertArchiveAnchors proves a row's archive-side pair describes the file the row
// points at: non-empty, non-zero, and equal to what is actually on disk. Both halves
// matter — an empty anchor turns a perfectly good archive copy into a critical
// delete_mismatch, and a wrong one fails every later archive-side verify.
func assertArchiveAnchors(t *testing.T, h *harness, id int64, what string) {
	t.Helper()
	f, ok, err := h.store.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("%s: GetFrameByID: ok=%v err=%v", what, ok, err)
	}
	if f.ArchiveSHA256 == "" || f.ArchiveSize == 0 {
		t.Fatalf("%s: left the archive anchors unset (sha=%q size=%d)", what, f.ArchiveSHA256, f.ArchiveSize)
	}
	st, serr := os.Stat(f.DestPath)
	if serr != nil {
		t.Fatalf("%s: archive copy missing at %q: %v", what, f.DestPath, serr)
	}
	if f.ArchiveSize != st.Size() {
		t.Fatalf("%s: archive_size %d != the file's %d", what, f.ArchiveSize, st.Size())
	}
	if hd := hashOnDisk(t, f.DestPath); hd != f.ArchiveSHA256 {
		t.Fatalf("%s: archive_sha256 %q does not describe the file (%q)", what, f.ArchiveSHA256, hd)
	}
}

// TestEveryFilingWriterSetsTheArchiveAnchors is the guard the design chose over a
// COALESCE bridge inside archiveHash: every path that files bytes leaves
// archive_sha256 non-empty and archive_size non-zero, so a write site missed when a
// new path is added fails loudly here instead of degrading into a critical
// delete_mismatch against an archive copy that is perfectly good.
func TestEveryFilingWriterSetsTheArchiveAnchors(t *testing.T) {
	ctx := context.Background()

	t.Run("pull", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		assertArchiveAnchors(t, h, f.ID, "a freshly pulled frame")
	})

	t.Run("quarantine", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		// A BIAS routed by <SENSOR> with the sensor map cleared quarantines.
		h.cfg.SensorMap = map[string]string{}
		src := "Autorun/Bias/Bias_0001.fit"
		h.dev.AddFile(emmc, src, fixture(t, "asiair_bias_6200mm.fit"), h.nowVal.Add(-10*time.Minute))
		h.cycle()
		f, ok := h.getFrame(src)
		if !ok || f.State != model.StateUnclassified {
			t.Fatalf("frame should be unclassified first (ok=%v)", ok)
		}
		assertArchiveAnchors(t, h, f.ID, "a quarantined frame")
	})

	t.Run("reclassify", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.cfg.SensorMap = map[string]string{}
		src := "Autorun/Bias/Bias_0001.fit"
		h.dev.AddFile(emmc, src, fixture(t, "asiair_bias_6200mm.fit"), h.nowVal.Add(-10*time.Minute))
		h.cycle()
		f, _ := h.getFrame(src)

		newCfg := *h.cfg
		newCfg.SensorMap = map[string]string{"ZWO ASI6200MM Pro": "6200MM"}
		h.cfgPtr.Store(&newCfg)
		h.w.runReclassify(ctx)

		after, _ := h.getFrame(src)
		if after.State != model.StateVerified {
			t.Fatalf("reclassify should resolve to verified; got %s", after.State)
		}
		// A local rename changes no bytes, so the anchors must be carried through
		// rather than reset by the move.
		assertArchiveAnchors(t, h, f.ID, "a reclassified frame")
	})

	t.Run("retarget", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
			t.Fatalf("retarget: status=%q err=%v", res.Status, err)
		}
		// The edited bytes, not the device original's: the archive side follows the
		// file that actually lands.
		assertArchiveAnchors(t, h, f.ID, "a retargeted frame")
		after, _, _ := h.store.GetFrameByID(ctx, f.ID)
		if after.ArchiveSHA256 == after.SHA256 {
			t.Fatalf("a retargeted frame's archive anchor must be the edited hash, not the device one (%q)", after.SHA256)
		}
	})

	t.Run("restore", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
		f, _ := fileAndAge(t, h, false)
		if err := os.Remove(f.DestPath); err != nil { // operator culled the NAS copy
			t.Fatal(err)
		}
		h.setMissing(t, f.ID, 0)
		if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
			t.Fatalf("request restore: ok=%v err=%v", ok, err)
		}
		h.cycle() // restorePhase re-pulls from the still-resident device original
		// The restore writes bytes the anchors already name, so they must still
		// describe the file it put back.
		assertArchiveAnchors(t, h, f.ID, "a restored frame")
	})
}

// TestRetargetPreservesRecordedDataSHA256 drives the preserve through a real filing
// path rather than a bare UpsertFrame: the retarget re-files the row and computes no
// data-unit digest, so the recorded exposure identity must survive it. Losing it
// here is silent — no error, no alert — and it is what the discriminator compares a
// device-side revision against.
func TestRetargetPreservesRecordedDataSHA256(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")

	// Record the identity on the row the way the backfill will, then re-file through
	// a path that carries none of its own.
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	got.DataSHA256 = "the-exposure-identity"
	if err := h.store.UpsertFrame(ctx, got); err != nil {
		t.Fatalf("record data_sha256: %v", err)
	}
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}

	after, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if after.DataSHA256 != "the-exposure-identity" {
		t.Fatalf("a re-file carrying no data_sha256 must leave the recorded one intact, got %q", after.DataSHA256)
	}
}

// downgradeToSchema8 strips the copy-keyed columns and the supersede intent table
// and rewinds the recorded schema version, turning a closed database back into
// exactly the v8 one migrate9 meets in the field. It is how the day-zero test gets
// pre-migration rows that were written by the real filing path: this binary can only
// produce current rows, and hand-INSERTing v8 ones would test a shape no release
// ever wrote. Every object a later migration creates has to go, or that migration
// meets its own leftovers on the way back up.
func downgradeToSchema8(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	const ddl = `
ALTER TABLE frames DROP COLUMN data_sha256;
ALTER TABLE frames DROP COLUMN archive_sha256;
ALTER TABLE frames DROP COLUMN archive_size;
ALTER TABLE frames DROP COLUMN delete_defer_count;
DROP TABLE supersede_intents;
UPDATE meta SET value='8' WHERE key='schema_version';
`
	if _, err := raw.ExecContext(context.Background(), ddl); err != nil {
		t.Fatalf("downgrade to v8: %v", err)
	}
}

// TestDayZeroMigrationSeedsTheArchiveAnchors is the no-op claim the whole column
// split rests on: after migrate9 every pre-existing row's archive_sha256 is the
// answer the schema-8 derivation gave — the overlay hash where one exists, sha256
// otherwise — and its archive_size is size, so a delete pass over rows of both
// shapes holds none of them. If the seeding is wrong it is wrong for the whole
// library at once, and the symptom is a critical delete_mismatch raised against
// archive copies that are perfectly good.
func TestDayZeroMigrationSeedsTheArchiveAnchors(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)

	plainSrc := "Autorun/Light/C-2025 A6/Light_0001.fit"
	overSrc := "Autorun/Light/C-2025 A6/Light_0002.fit"
	plain := fileLight(t, h, plainSrc)
	over := fileLight(t, h, overSrc)
	if res, err := h.w.serviceRetarget(ctx, over.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}

	// The answer the code gave before the split, captured per row.
	plainRow, _, _ := h.store.GetFrameByID(ctx, plain.ID)
	overRow, _, _ := h.store.GetFrameByID(ctx, over.ID)
	wantPlain, wantPlainSize := plainRow.SHA256, plainRow.Size
	wantOver, wantOverSize := overRow.RetargetSHA256, overRow.Size
	if wantOver == "" {
		t.Fatal("the retargeted row must carry an overlay for this test to cover the overlay arm")
	}

	dbPath := filepath.Join(h.dir, "state.db")
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	downgradeToSchema8(t, dbPath)
	migrated, err := store.Open(dbPath) // runs migrate9 and migrate10
	if err != nil {
		t.Fatalf("re-open (migrate): %v", err)
	}
	t.Cleanup(func() { migrated.Close() })
	h.store, h.w.store = migrated, migrated

	for _, tc := range []struct {
		what      string
		id        int64
		wantHash  string
		wantBytes int64
	}{
		{"a plain row", plain.ID, wantPlain, wantPlainSize},
		{"a retargeted row", over.ID, wantOver, wantOverSize},
	} {
		got, ok, err := h.store.GetFrameByID(ctx, tc.id)
		if err != nil || !ok {
			t.Fatalf("%s: GetFrameByID: ok=%v err=%v", tc.what, ok, err)
		}
		if got.ArchiveSHA256 != tc.wantHash {
			t.Fatalf("%s: archive_sha256 = %q, want the pre-split answer %q", tc.what, got.ArchiveSHA256, tc.wantHash)
		}
		if got.ArchiveSize != tc.wantBytes {
			t.Fatalf("%s: archive_size = %d, want size %d", tc.what, got.ArchiveSize, tc.wantBytes)
		}
		if got.DataSHA256 != "" || got.DeleteDeferCount != 0 {
			t.Fatalf("%s: migrated with data=%q deferrals=%d, want unrecorded and zero",
				tc.what, got.DataSHA256, got.DeleteDeferCount)
		}
	}

	// The seeding is a no-op only if the delete pass agrees: both shapes must pass
	// the three-way check and drain, with no hold and no alert.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("a migrated row must not be held or alerted; got %v", h.raisedCategories())
	}
	for _, src := range []string{plainSrc, overSrc} {
		got, _ := h.getFrame(src)
		if got.State != model.StateDeleted {
			t.Fatalf("%s: state=%s, want deleted (the seeded anchors verified)", src, got.State)
		}
		if h.deviceHas(src) {
			t.Fatalf("%s: the proven original should have drained", src)
		}
	}
}

// TestAutoLinkFromExistingDataVerifiesTheArchiveAnchors covers the one site whose
// failure is completely silent: a destination root already holding this
// installation's frames must auto-link even when the sampled frame has been
// superseded, its archive anchors describing bytes the device copy no longer
// matches. Against a re-derived expectation — the overlay hash,
// else sha256 — the sample fails to verify and the root is simply never linked, with
// no alert, no error and nothing in a log to lead an operator to the cause.
func TestAutoLinkFromExistingDataVerifiesTheArchiveAnchors(t *testing.T) {
	ctx := context.Background()
	root := func(h *harness) string { return h.cfg.Roots.Light }

	// supersede advances the row's DEVICE anchors past the archive copy on disk,
	// exactly as the supersede path will: the archive file is untouched and its
	// anchors keep describing it.
	supersede := func(t *testing.T, h *harness, f *model.Frame) {
		t.Helper()
		got, _, _ := h.store.GetFrameByID(ctx, f.ID)
		got.SHA256 = sha256hex([]byte("the-revised-device-bytes"))
		got.Size += 2880
		if err := h.store.UpsertFrame(ctx, got); err != nil {
			t.Fatalf("supersede the row: %v", err)
		}
	}
	// unlink drops the recorded id the filing path's own auto-link wrote, so the
	// scan's existing-data path is the one under test.
	unlink := func(t *testing.T, h *harness, r string) {
		t.Helper()
		if err := h.store.SetMeta(ctx, LibraryIDKey(r), ""); err != nil {
			t.Fatalf("unlink root: %v", err)
		}
		h.w.ensuredRoots = map[string]bool{}
	}

	t.Run("links a superseded sample", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		supersede(t, h, f)
		r := root(h)
		unlink(t, h, r)

		if !h.w.tryLinkFromExistingData(ctx, r, []string{r}) {
			t.Fatal("a root holding a superseded frame must still auto-link: its archive copy verifies against archive_sha256")
		}
		if id, _, _ := h.store.GetMeta(ctx, LibraryIDKey(r)); id == "" {
			t.Fatal("the link must be recorded")
		}
	})

	t.Run("refuses a sample whose archive copy does not verify", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		// The expectation re-derived from a superseded row's device anchors: the
		// archive copy on disk does not hash to them, so the verify fails and nothing
		// links — the silent failure the separate archive columns exist to prevent.
		got, _, _ := h.store.GetFrameByID(ctx, f.ID)
		got.ArchiveSHA256 = sha256hex([]byte("the-revised-device-bytes"))
		if err := h.store.UpsertFrame(ctx, got); err != nil {
			t.Fatalf("mis-anchor the row: %v", err)
		}
		r := root(h)
		unlink(t, h, r)

		if h.w.tryLinkFromExistingData(ctx, r, []string{r}) {
			t.Fatal("a sample that does not verify against its archive anchor must not link the root")
		}
	})
}
