package rig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
	// The night ledger has no whole-table read in production, so the preview tests
	// open state.db themselves; the driver comes with the store's own import.
	_ "modernc.org/sqlite"
)

// upsertFailStore wraps a real store and forces UpsertFrame to fail on demand, so a
// test can drive the commit-failure path (fileFrom→recordFiled swallows the error and
// still returns true). Every other method delegates to the embedded store.
type upsertFailStore struct {
	store.Store
	failUpsert bool
}

func (s *upsertFailStore) UpsertFrame(ctx context.Context, f *model.Frame) error {
	if s.failUpsert {
		return errors.New("injected upsert failure")
	}
	return s.Store.UpsertFrame(ctx, f)
}

// fileLight pulls one light frame from the device and returns its verified row. The
// harness strips target spaces, so 'C-2025 A6' files under a "C-2025A6" folder while the
// OBJECT card (and f.Target) keep the space.
func fileLight(t *testing.T, h *harness, src string) *model.Frame {
	t.Helper()
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
	h.cycle()
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("light frame not filed")
	}
	if f.State != model.StateVerified {
		t.Fatalf("filed state=%s, want verified", f.State)
	}
	return f
}

// The four DATE-OBS values the night tests file frames under. The light fixture
// carries the third one, so a frame pulled unmodified belongs to the observing
// date 2025-10-10: the harness's site clock is UTC, and route.ObservingDate
// buckets a 05:25 capture into the previous day's night.
const (
	dateObsNight1 = "2025-10-09T05:25:41.531614" // observing date 2025-10-08
	dateObsNight2 = "2025-10-10T05:25:41.531614" // observing date 2025-10-09
	dateObsNight3 = "2025-10-11T05:25:41.531614" // observing date 2025-10-10 (the fixture's own)
	dateObsNight4 = "2025-10-12T05:25:41.531614" // observing date 2025-10-11
	dateObsNight5 = "2025-10-13T05:25:41.531614" // observing date 2025-10-12
)

// fileLightAs pulls one light frame carrying the given OBJECT and DATE-OBS cards
// and returns its verified row. One cycle per frame, so the order the nights are
// assigned in is the order the calls are written in.
func fileLightAs(t *testing.T, h *harness, src, target, dateObs string) *model.Frame {
	t.Helper()
	h.dev.AddFile(emmc, src, dateObsFixture(t, target, dateObs), h.nowVal.Add(-10*time.Minute))
	h.cycle()
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("light frame %q not filed", src)
	}
	if f.State != model.StateVerified {
		t.Fatalf("%q filed state=%s, want verified", src, f.State)
	}
	return f
}

// nightDirOf returns the path components a filed frame sits under, relative to
// the light root, so an assertion can name a folder rather than a temp path.
func nightDirOf(t *testing.T, h *harness, f *model.Frame) string {
	t.Helper()
	rel, err := filepath.Rel(filepath.Join(h.dir, "light"), filepath.Dir(f.DestPath))
	if err != nil {
		t.Fatalf("frame %d is filed outside the light root: %v", f.ID, err)
	}
	return rel
}

// wantFiledUnder asserts a frame's directory, relative to the light root.
func wantFiledUnder(t *testing.T, h *harness, f *model.Frame, want string) {
	t.Helper()
	if got := nightDirOf(t, h, f); got != want {
		t.Fatalf("frame %d filed under %q, want %q", f.ID, got, want)
	}
}

// reload re-reads a frame row after an operation moved it.
func reload(t *testing.T, h *harness, id int64) *model.Frame {
	t.Helper()
	f, ok, err := h.store.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("frame %d vanished: %v", id, err)
	}
	return f
}

func hashOnDisk(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256hex(b)
}

// TestRetarget_PreservesOriginalSHA256 (INV-1) is the cardinal safety invariant: a
// retarget rewrites the archive's OBJECT card and re-files it, but the sha256 column —
// the device-correlation anchor the untouched device original still hashes to — is never
// mutated. The edited archive hash lands in retarget_sha256 instead.
func TestRetarget_PreservesOriginalSHA256(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	origSHA := f.SHA256
	oldDest := f.DestPath

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil {
		t.Fatalf("serviceRetarget: %v", err)
	}
	if res.Status != "retargeted" {
		t.Fatalf("status=%q reason=%q, want retargeted", res.Status, res.Reason)
	}

	got, ok, _ := h.store.GetFrameByID(ctx, f.ID)
	if !ok {
		t.Fatal("frame vanished")
	}
	if got.SHA256 != origSHA {
		t.Fatalf("sha256 mutated: %q != original %q (INV-1)", got.SHA256, origSHA)
	}
	if got.RetargetSHA256 == "" || got.RetargetSHA256 == origSHA {
		t.Fatalf("retarget_sha256=%q, want the edited-archive hash (non-empty, != sha256)", got.RetargetSHA256)
	}
	if got.Target != "M31" {
		t.Fatalf("target=%q, want M31", got.Target)
	}
	if got.RetargetFrom != "C-2025 A6" {
		t.Fatalf("retarget_from=%q, want the original target", got.RetargetFrom)
	}
	if got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified (retarget is not a lifecycle change)", got.State)
	}
	if !strings.Contains(got.DestPath, "M31") || got.DestPath == oldDest {
		t.Fatalf("dest=%q, want a new M31 path", got.DestPath)
	}
	// The edited archive on disk hashes to retarget_sha256, and the old file is gone.
	if h := hashOnDisk(t, got.DestPath); h != got.RetargetSHA256 {
		t.Fatalf("archive hash %q != retarget_sha256 %q", h, got.RetargetSHA256)
	}
	if _, err := os.Stat(oldDest); !os.IsNotExist(err) {
		t.Fatalf("old archive not removed: %v", err)
	}
	if res.OldPath != oldDest || res.NewPath != got.DestPath {
		t.Fatalf("result paths old=%q new=%q, want %q/%q", res.OldPath, res.NewPath, oldDest, got.DestPath)
	}
}

// TestRetarget_RefusesDriftedArchive (INV-3) refuses to edit an archive whose on-disk
// bytes no longer hash to archiveHash(f): editing drifted content would bake corruption
// into retarget_sha256 and authorize deleting the correct device original.
func TestRetarget_RefusesDriftedArchive(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)

	// Make the archive read back a different hash than recorded.
	h.lfs.CorruptAfterWrite = func(_, honest string) string {
		return strings.Repeat("0", len(honest))
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil {
		t.Fatalf("serviceRetarget: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status=%q, want failed on a drifted archive", res.Status)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.SHA256 != f.SHA256 || got.RetargetSHA256 != "" {
		t.Fatalf("drifted-archive refusal must not touch the row: sha=%q retarget=%q", got.SHA256, got.RetargetSHA256)
	}
	if got.Target != "C-2025 A6" || got.DestPath != f.DestPath {
		t.Fatalf("drifted-archive refusal must not move the frame: target=%q dest=%q", got.Target, got.DestPath)
	}
	if stagingEmpty(t, h.dir) == false {
		t.Fatalf("retarget temp leaked after refusal")
	}
}

// TestRetarget_RefusesAtRestCorruption (F1, INV-4) proves the first retarget refuses to
// mint retarget_sha256 from unverified at-rest bytes: when a write fault makes the edited
// temp read back a hash different from the stream-derived expectation (rr.OutputSHA256),
// the retarget fails, the overlay is never set, the old archive is byte-intact, and the
// device original survives — so a corrupted archive can never become the delete anchor.
func TestRetarget_RefusesAtRestCorruption(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	oldDest := f.DestPath
	origArchiveHash := hashOnDisk(t, oldDest)

	// A write fault on ONLY the edited retarget temp: its at-rest read-back reports a
	// hash different from the honest streamed bytes. The archive read-back (step 3)
	// stays honest so the drift guard passes and the step-6 at-rest check is reached.
	h.lfs.CorruptAfterWrite = func(path, honest string) string {
		if strings.Contains(path, "asiair-retarget") {
			return strings.Repeat("0", len(honest))
		}
		return honest
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil {
		t.Fatalf("serviceRetarget: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status=%q reason=%q, want failed on an at-rest corrupted edit", res.Status, res.Reason)
	}
	if !strings.Contains(res.Reason, "at-rest verify mismatch") {
		t.Fatalf("reason=%q, want the at-rest write-fault reason", res.Reason)
	}

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 != "" {
		t.Fatalf("retarget_sha256 must NOT be set on a corrupted edit; got %q", got.RetargetSHA256)
	}
	if got.SHA256 != f.SHA256 {
		t.Fatalf("sha256 mutated: %q != %q", got.SHA256, f.SHA256)
	}
	if got.Target != "C-2025 A6" || got.DestPath != oldDest {
		t.Fatalf("row moved on a refused corrupt edit: target=%q dest=%q", got.Target, got.DestPath)
	}
	// The old archive is byte-intact (step 7 never reached) and the device original,
	// untouched by any retarget, survives.
	if hd := hashOnDisk(t, oldDest); hd != origArchiveHash {
		t.Fatalf("old archive changed: %q != %q", hd, origArchiveHash)
	}
	if !h.deviceHas(src) {
		t.Fatalf("device original must survive a refused retarget")
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatalf("retarget temp leaked after refusal")
	}
}

// TestRetarget_FailsOnCommitFailure (F2) proves a swallowed UpsertFrame failure inside
// the edited-archive filing does not report success: fileFrom→recordFiled logs and still
// returns true, so retargetFrame must re-read the row and refuse when the overlay did not
// persist — leaving the old archive and device original intact and the row un-retargeted.
func TestRetarget_FailsOnCommitFailure(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	oldDest := f.DestPath
	origArchiveHash := hashOnDisk(t, oldDest)

	fs := &upsertFailStore{Store: h.store}
	h.w.store = fs
	fs.failUpsert = true

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil {
		t.Fatalf("serviceRetarget: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status=%q reason=%q, want failed when the commit does not persist", res.Status, res.Reason)
	}

	fs.failUpsert = false // let the verification read-backs below succeed
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 != "" || got.Target != "C-2025 A6" || got.DestPath != oldDest {
		t.Fatalf("row must stay un-retargeted after a failed commit: target=%q dest=%q retarget=%q",
			got.Target, got.DestPath, got.RetargetSHA256)
	}
	if hd := hashOnDisk(t, oldDest); hd != origArchiveHash {
		t.Fatalf("old archive changed after a failed commit: %q != %q", hd, origArchiveHash)
	}
	if !h.deviceHas(src) {
		t.Fatalf("device original must survive a failed-commit retarget")
	}
}

// TestRetarget_Idempotent_RetargetFromStable (INV-7) proves a repeat retarget edits from
// the CURRENT archive, updates retarget_sha256, and keeps retarget_from pinned to the
// FIRST original target across the chain.
func TestRetarget_Idempotent_RetargetFromStable(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)

	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("first retarget: status=%q err=%v", res.Status, err)
	}
	afterFirst, _, _ := h.store.GetFrameByID(ctx, f.ID)

	if res, err := h.w.serviceRetarget(ctx, f.ID, "M42"); err != nil || res.Status != "retargeted" {
		t.Fatalf("second retarget: status=%q err=%v", res.Status, err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)

	if got.Target != "M42" {
		t.Fatalf("target=%q, want M42", got.Target)
	}
	if got.RetargetFrom != "C-2025 A6" {
		t.Fatalf("retarget_from=%q, want the FIRST original across repeats (INV-7)", got.RetargetFrom)
	}
	if got.RetargetSHA256 == afterFirst.RetargetSHA256 {
		t.Fatalf("retarget_sha256 not updated on the second retarget")
	}
	if got.SHA256 != f.SHA256 {
		t.Fatalf("sha256 mutated across repeats: %q != %q", got.SHA256, f.SHA256)
	}
	if h := hashOnDisk(t, got.DestPath); h != got.RetargetSHA256 {
		t.Fatalf("M42 archive hash %q != retarget_sha256 %q", h, got.RetargetSHA256)
	}
}

// TestRetarget_ScopeGuards (INV-9) restricts retargeting to verified/deleted frames with
// an archive path; a held frame and a no-dest frame are refused (skipped), and preview
// marks them ineligible.
func TestRetarget_ScopeGuards(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)

	// Held is out of scope.
	held := *f
	held.State = model.StateHeld
	if err := h.store.UpdateFrame(ctx, &held); err != nil {
		t.Fatal(err)
	}
	res, _ := h.w.serviceRetarget(ctx, f.ID, "M31")
	if res.Status != "skipped" {
		t.Fatalf("held frame: status=%q, want skipped", res.Status)
	}
	pv := h.w.previewRetarget(ctx, []int64{f.ID}, "M31")
	if len(pv) != 1 || pv[0].Eligible {
		t.Fatalf("held frame preview must be ineligible; got %+v", pv)
	}

	// No dest_path is out of scope.
	nodest := held
	nodest.State = model.StateVerified
	nodest.DestPath = ""
	if err := h.store.UpdateFrame(ctx, &nodest); err != nil {
		t.Fatal(err)
	}
	res, _ = h.w.serviceRetarget(ctx, f.ID, "M31")
	if res.Status != "skipped" {
		t.Fatalf("no-dest frame: status=%q, want skipped", res.Status)
	}
}

// TestRetarget_DeletedStaysDeleted (INV-9) retargets an archive whose device original is
// already gone: it is a pure relabel, and the frame's deleted state is preserved (not
// resurrected to verified).
func TestRetarget_DeletedStaysDeleted(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)

	del := *f
	del.State = model.StateDeleted
	// The device original was reclaimed at this instant; the relabel must preserve the
	// reclaim clock so the row still prunes (F3 — a nulled deleted_at never prunes).
	deletedAt := h.nowVal.Add(-time.Hour).Unix()
	del.DeletedAt = deletedAt
	if err := h.store.UpdateFrame(ctx, &del); err != nil {
		t.Fatal(err)
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil {
		t.Fatalf("serviceRetarget: %v", err)
	}
	if res.Status != "retargeted" {
		t.Fatalf("status=%q reason=%q, want retargeted", res.Status, res.Reason)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted preserved (INV-9)", got.State)
	}
	if got.DeletedAt != deletedAt {
		t.Fatalf("deleted_at=%d, want %d preserved across the relabel (F3)", got.DeletedAt, deletedAt)
	}
	if got.Target != "M31" || got.RetargetSHA256 == "" {
		t.Fatalf("deleted relabel must still move/overlay: target=%q retarget=%q", got.Target, got.RetargetSHA256)
	}
}

// TestReapplyRetarget_OnDifferJitter (INV-8) proves a re-pull of a retargeted frame's
// ORIGINAL device bytes — triggered by a device-mtime jitter of identical content — re-
// applies the OBJECT edit at the filing chokepoint, so the archive keeps its corrected
// M31 label instead of reverting to the original folder, and retarget_sha256 is
// preserved.
func TestReapplyRetarget_OnDifferJitter(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	before, _, _ := h.store.GetFrameByID(ctx, f.ID)

	// The device still holds the ORIGINAL bytes (retarget never touches the device); bump
	// its mtime past the ±2s tolerance so decide takes the differ branch on identical
	// content.
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-5*time.Minute))

	h.cycle()

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.Target != "M31" {
		t.Fatalf("target reverted to %q; the re-apply rule must keep M31 (INV-8)", got.Target)
	}
	if !strings.Contains(got.DestPath, "M31") {
		t.Fatalf("archive reverted out of M31: %q", got.DestPath)
	}
	if got.RetargetSHA256 != before.RetargetSHA256 {
		t.Fatalf("retarget_sha256 changed on an identical-content re-pull: %q != %q", got.RetargetSHA256, before.RetargetSHA256)
	}
	if got.SHA256 != before.SHA256 {
		t.Fatalf("sha256 mutated on re-pull: %q != %q", got.SHA256, before.SHA256)
	}
	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("a retargeted differ re-pull must not raise frame_lost; got %v", h.raisedCategories())
	}
}

// TestDiffer_PreservesOverlayOnIdenticalContent (INV-10) is the overlay-preservation half:
// an identical-content re-pull keeps retarget_sha256 (via the re-apply), while
// TestDiffer_ResetsOverlayOnNewContent covers the reset half.
func TestDiffer_PreservesOverlayOnIdenticalContent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	before, _, _ := h.store.GetFrameByID(ctx, f.ID)

	data := fixture(t, "asiair_light_2600mm_ha.fit")
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-5*time.Minute)) // identical bytes, jittered mtime
	h.cycle()

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 != before.RetargetSHA256 || got.RetargetSHA256 == "" {
		t.Fatalf("overlay not preserved on identical-content re-pull: %q != %q (INV-10)", got.RetargetSHA256, before.RetargetSHA256)
	}
}

// TestDiffer_ResetsOverlayOnNewContent (INV-10) proves genuinely-new content at the same
// source path resets the overlay to NULL: the re-apply rule fires only when the streamed
// bytes match prior.SHA256, so different bytes file as the new frame they are (a stale
// retarget_sha256 would otherwise hang the frame held).
func TestDiffer_ResetsOverlayOnNewContent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}

	// Overwrite the device source with DIFFERENT content (one byte past the END card, so
	// the header/OBJECT are unchanged but the bytes — and the hash — differ).
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	newContent := append([]byte(nil), data...)
	newContent[len(newContent)-1] ^= 0xFF
	if sha256hex(newContent) == f.SHA256 {
		t.Fatal("setup: content must differ from the original")
	}
	h.dev.AddFile(emmc, src, newContent, h.nowVal.Add(-5*time.Minute))

	h.cycle()

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 != "" {
		t.Fatalf("overlay must reset to NULL on genuinely-new content; got %q (INV-10)", got.RetargetSHA256)
	}
	if got.RetargetFrom != "" {
		t.Fatalf("retarget_from must reset with the overlay; got %q", got.RetargetFrom)
	}
	// The new bytes carry the ORIGINAL OBJECT, so the frame files back under its own
	// label, not M31.
	if got.Target != "C-2025 A6" {
		t.Fatalf("new content should file under its own OBJECT; target=%q", got.Target)
	}
	if got.SHA256 != sha256hex(newContent) {
		t.Fatalf("sha256 should track the new content: %q != %q", got.SHA256, sha256hex(newContent))
	}
}

// TestReapplyRetarget_OnDeletedReadopt (INV-8) proves the deleted-readopt path re-applies
// the edit: a retargeted frame marked deleted whose device original reappears is re-
// archived under its corrected label with the overlay preserved, not reverted.
func TestReapplyRetarget_OnDeletedReadopt(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	before, _, _ := h.store.GetFrameByID(ctx, f.ID)

	// Simulate the device original having been reclaimed, then reappearing with the same
	// (original) content — the deleted-readopt case. decide re-adopts it.
	del := *before
	del.State = model.StateDeleted
	if err := h.store.UpdateFrame(ctx, &del); err != nil {
		t.Fatal(err)
	}

	h.cycle()

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.Target != "M31" || !strings.Contains(got.DestPath, "M31") {
		t.Fatalf("deleted-readopt reverted the label: target=%q dest=%q (INV-8)", got.Target, got.DestPath)
	}
	if got.RetargetSHA256 != before.RetargetSHA256 {
		t.Fatalf("overlay not preserved on readopt: %q != %q", got.RetargetSHA256, before.RetargetSHA256)
	}
	if got.State != model.StateVerified {
		t.Fatalf("readopt should re-verify the frame; state=%s", got.State)
	}
}

// TestDelete_RetargetedThreeWay (INV-5) is the delete-path safety proof for a retargeted
// frame: the device original still hashes to sha256 (never touched), the NAS archive
// hashes to retarget_sha256, and the three-way check authorizes the delete only because
// the archive side compares to archiveHash(f) and the device side to sha256.
func TestDelete_RetargetedThreeWay(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true

	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	// The archive now hashes to retarget_sha256; the device original is untouched.
	if got.RetargetSHA256 == "" || got.SHA256 == got.RetargetSHA256 {
		t.Fatalf("setup: expected a retargeted overlay, got sha=%q retarget=%q", got.SHA256, got.RetargetSHA256)
	}

	h.nowVal = h.nowVal.Add(2 * time.Hour) // past grace → deletion-eligible

	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

	if h.deviceHas(src) {
		t.Fatalf("a retargeted frame's device original must be deleted once the three-way check passes (INV-5)")
	}
	after, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if after.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", after.State)
	}
	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("the three-way check must not hold a correctly-retargeted frame; got %v", h.raisedCategories())
	}
}

// TestRestoreOne_RetargetedPresentRecognized proves restoreOne's "back and good"
// pre-check compares to archiveHash(f): a healthy retargeted NAS copy (hashing to
// retarget_sha256) is recognized and the request cleared, rather than pinning the device
// original behind a standing restore request (restore.go:100, design
// routing-and-filing §9.4).
func TestRestoreOne_RetargetedPresentRecognized(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	h.setMissing(t, f.ID, 0)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}

	h.cycle() // restorePhase sees the M31 archive present and good

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RestoreRequestedAt != 0 || got.MissingSeenAt != 0 {
		t.Fatalf("a present retargeted copy must clear the request (archiveHash pre-check); missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
}

// TestRestore_ReappliesRetarget proves the restore path re-applies the OBJECT edit: a
// retargeted frame whose NAS copy was culled is re-pulled from the (original-carrying)
// device original, re-edited to the corrected label, and its at-rest copy verifies
// against archiveHash(f) — so the restored archive hashes to retarget_sha256, not the
// original (design routing-and-filing §9.4, delete.go:546).
func TestRestore_ReappliesRetarget(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	m31Dest := got.DestPath
	wantHash := got.RetargetSHA256

	if err := os.Remove(m31Dest); err != nil { // operator culled the retargeted NAS copy
		t.Fatal(err)
	}
	h.setMissing(t, f.ID, 0)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}

	h.cycle() // restorePhase re-pulls the original and re-applies the edit

	if hd := hashOnDisk(t, m31Dest); hd != wantHash {
		t.Fatalf("restored archive hash %q != retarget_sha256 %q — the edit was not re-applied", hd, wantHash)
	}
	after, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if after.RestoreRequestedAt != 0 || after.MissingSeenAt != 0 {
		t.Fatalf("a completed restore must clear both marks; missing=%d restore=%d",
			after.MissingSeenAt, after.RestoreRequestedAt)
	}
	if after.SHA256 != f.SHA256 || after.RetargetSHA256 != wantHash {
		t.Fatalf("restore must keep the overlay: sha=%q retarget=%q", after.SHA256, after.RetargetSHA256)
	}
}

// TestParseBinningToken pins the inverse of binningToken used by preview.
func TestParseBinningToken(t *testing.T) {
	cases := []struct {
		tok  string
		x, y int64
		ok   bool
	}{
		{"Bin1", 1, 1, true},
		{"Bin2", 2, 2, true},
		{"Bin1x2", 1, 2, true},
		{"Bin3x1", 3, 1, true},
		{"", 0, 0, false},
		{"Bin", 0, 0, false},
		{"Binx", 0, 0, false},
		{"2x2", 0, 0, false},
	}
	for _, c := range cases {
		x, y, ok := parseBinningToken(c.tok)
		if x != c.x || y != c.y || ok != c.ok {
			t.Errorf("parseBinningToken(%q)=(%d,%d,%v), want (%d,%d,%v)", c.tok, x, y, ok, c.x, c.y, c.ok)
		}
	}
}

// TestPreviewRetarget_ProjectsPathWithoutIO checks the read-only projection: an eligible
// light previews its new M31 path (flagging the night approximation) and never mutates
// night_counters (Night stays approximate, AssignNight is not called).
func TestPreviewRetarget_ProjectsPathWithoutIO(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)

	pv := h.w.previewRetarget(ctx, []int64{f.ID}, "M31")
	if len(pv) != 1 {
		t.Fatalf("want 1 preview, got %d", len(pv))
	}
	p := pv[0]
	if !p.Eligible {
		t.Fatalf("light should be eligible; reason=%q", p.Reason)
	}
	if p.CurrentTarget != "C-2025 A6" {
		t.Fatalf("current_target=%q", p.CurrentTarget)
	}
	if !strings.Contains(p.NewPath, filepath.Join("M31", "FRA300")) {
		t.Fatalf("new_path=%q, want it under M31/FRA300", p.NewPath)
	}
	if !p.NightApprox {
		t.Fatalf("the default light template uses <n>, so night must be flagged approximate")
	}
	if p.OldPath != f.DestPath {
		t.Fatalf("old_path=%q, want %q", p.OldPath, f.DestPath)
	}
}

// TestRetarget_JoinsExistingNightLedger covers a session that started on one target,
// briefly re-slewed to another, and had the subs that kept the wrong OBJECT retargeted
// back. They must join the night that target's own subs from the same night are filed
// under (NIGHT_3 here), not open NIGHT_1 in the right target folder.
//
// The destination is seeded by FILING three real nights of frames through the pull
// path, not by writing night_counters rows: the ordinal a retarget joins is read out
// of the destination folder, so a ledger row with nothing filed under it is not the
// evidence this decision rests on. The third night is the retargeted frame's own
// observing date, which is the ordinal it must join.
//
// The retarget target is spelled the way the operator reads it off disk — space-stripped,
// because strip_target_spaces renders "NGC 7380" as the folder "NGC7380". Both spellings
// route to that one folder, so the night ordinal is the only thing that may differ, and
// it must not.
func TestRetarget_JoinsExistingNightLedger(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")

	// The destination target's three nights; the frame in hand was captured on the third
	// (DATE-OBS 2025-10-11T05:25 → observing date 2025-10-10).
	for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
		src := fmt.Sprintf("Autorun/Light/NGC 7380/Light_%04d.fit", i+1)
		seeded := fileLightAs(t, h, src, "NGC 7380", dateObs)
		wantFiledUnder(t, h, seeded, filepath.Join("NGC7380", "FRA300", "S", fmt.Sprintf("NIGHT_%d", i+1)))
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "NGC7380")
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}

	got, ok, _ := h.store.GetFrameByID(ctx, f.ID)
	if !ok {
		t.Fatal("frame vanished")
	}
	want := filepath.Join("NGC7380", "FRA300", "S", "NIGHT_3")
	if !strings.Contains(got.DestPath, want) {
		t.Fatalf("filed at %q, want it under %q — the retargeted frame must join the\n"+
			"night its destination target already assigned to this observing date", got.DestPath, want)
	}
}

// TestRetarget_JoinsExistingNightLedger_ExactSpelling is the control for the test above:
// the identical scenario, with the new target typed exactly as the destination frames'
// OBJECT card reads. It isolates the failure to the spelling of the target rather than to
// retargeting itself.
func TestRetarget_JoinsExistingNightLedger_ExactSpelling(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")

	for _, date := range []string{"2025-10-08", "2025-10-09", "2025-10-10"} {
		if _, err := h.store.AssignNight(ctx, store.NightRequest{Target: "NGC 7380", OTA: "FRA300", ObservingDate: date}); err != nil {
			t.Fatalf("seed night %s: %v", date, err)
		}
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "NGC 7380")
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	want := filepath.Join("NGC7380", "FRA300", "S", "NIGHT_3")
	if !strings.Contains(got.DestPath, want) {
		t.Fatalf("filed at %q, want it under %q", got.DestPath, want)
	}
}

// TestRetarget_FoldsTargetCase covers the operator half of the case fold: a target
// typed in a different case from the one the archive already holds is not a new
// target, because the folder it renders is the same folder. The frame joins that
// target's ledger and folder, and the OBJECT card written into the archive carries
// the canonical spelling — a card disagreeing with its own row would fail the
// re-apply hash check on every later re-pull of the original.
func TestRetarget_FoldsTargetCase(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
	for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
		src := fmt.Sprintf("Autorun/Light/NGC 7380/Light_%04d.fit", i+1)
		fileLightAs(t, h, src, "NGC 7380", dateObs)
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "ngc 7380")
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}

	got, ok, _ := h.store.GetFrameByID(ctx, f.ID)
	if !ok {
		t.Fatal("frame vanished")
	}
	want := filepath.Join("NGC7380", "FRA300", "S", "NIGHT_3")
	if !strings.Contains(got.DestPath, want) {
		t.Fatalf("filed at %q, want it under %q", got.DestPath, want)
	}
	if got.Target != "NGC 7380" {
		t.Fatalf("stored target %q; want the canonical spelling \"NGC 7380\"", got.Target)
	}
	hdr, herr := h.w.hdr.ReadHeader(got.DestPath)
	if herr != nil {
		t.Fatalf("re-read archive header: %v", herr)
	}
	if obj, _ := hdr.Str("OBJECT"); obj != "NGC 7380" {
		t.Fatalf("OBJECT card = %q; want the canonical spelling the row carries", obj)
	}
}

// TestAdoptsNightHeldByDestinationFolder (INV-N2) is the general statement the field
// report is one instance of: a frame whose destination night folder already holds its
// observing date joins that night, whatever its OBJECT spelling. Both spellings render
// the same folder, so both must land in the same night.
func TestAdoptsNightHeldByDestinationFolder(t *testing.T) {
	for _, spelling := range []string{"NGC7380", "NGC 7380"} {
		t.Run(spelling, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
			for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
				fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/Light_%04d.fit", i+1), "NGC 7380", dateObs)
			}

			res, err := h.w.serviceRetarget(ctx, f.ID, spelling)
			if err != nil || res.Status != "retargeted" {
				t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
			}
			wantFiledUnder(t, h, reload(t, h, f.ID), filepath.Join("NGC7380", "FRA300", "S", "NIGHT_3"))
		})
	}
}

// TestMintsAboveFolderHighWater (INV-N3) covers the other half of the repair: an
// observing date the destination target never shot has no ordinal to adopt, and
// MAX(night)+1 over its own fresh scope would be 1 — landing a genuinely new night on
// top of the siblings' occupied NIGHT_1. It must file above every ordinal the folder
// holds instead.
func TestMintsAboveFolderHighWater(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	// The subject was captured on a fourth night, one the destination never shot.
	f := fileLightAs(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit", "C-2025 A6", dateObsNight4)
	for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
		fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/Light_%04d.fit", i+1), "NGC 7380", dateObs)
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "NGC7380")
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}
	wantFiledUnder(t, h, reload(t, h, f.ID), filepath.Join("NGC7380", "FRA300", "S", "NIGHT_4"))
}

// TestNoAdoptionAcrossFolders (INV-N5) pins the rule that only the frame's own night
// folder is consulted. Under the shipped mosaic template a plain target's prefix is an
// ANCESTOR of every panel tree, so the panels' ordinals are visible to the first,
// subtree-wide step — and must be dropped by the second, exact-folder one. Both sort
// directions are covered because a sampled-path implementation fails differently on
// each: "M31_1-1" sorts before "NIGHT_", "Sadr_1-1" after.
//
// The panels are filed on the same three nights as the plain light's own date, so an
// implementation that adopted from the subtree would answer 3 and one that only took
// the subtree's high-water mark would answer 4. The correct answer is 1.
func TestNoAdoptionAcrossFolders(t *testing.T) {
	for _, target := range []string{"M31", "Sadr"} {
		t.Run(target, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			h.cfg.MosaicDetection = true
			for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
				panelFrame := fileLightAs(t, h, fmt.Sprintf("Autorun/Light/panel/Light_%04d.fit", i+1),
					target+"_1-1", dateObs)
				wantFiledUnder(t, h, panelFrame, filepath.Join(target, "FRA300", "S", target+"_1-1",
					fmt.Sprintf("NIGHT_%d", i+1)))
			}

			plain := fileLightAs(t, h, "Autorun/Light/plain/Light_0001.fit", target, dateObsNight3)
			wantFiledUnder(t, h, plain, filepath.Join(target, "FRA300", "S", "NIGHT_1"))
		})
	}
}

// TestSanitizedSiblingFoldersMintSeparately is the no-false-adoption case: under the
// shipped mosaic template "Sadr._1-1" and "Sadr_1-1" render two genuinely different
// leaf folders — the leaf sanitizes as one piece, so the trailing dot survives there —
// and each must mint its own NIGHT_1 for the same observing date rather than adopting
// the other's.
func TestSanitizedSiblingFoldersMintSeparately(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.MosaicDetection = true

	dotted := fileLightAs(t, h, "Autorun/Light/a/Light_0001.fit", "Sadr._1-1", dateObsNight1)
	wantFiledUnder(t, h, dotted, filepath.Join("Sadr", "FRA300", "S", "Sadr._1-1", "NIGHT_1"))

	plain := fileLightAs(t, h, "Autorun/Light/b/Light_0001.fit", "Sadr_1-1", dateObsNight1)
	wantFiledUnder(t, h, plain, filepath.Join("Sadr", "FRA300", "S", "Sadr_1-1", "NIGHT_1"))
}

// TestNestedRowsCountTowardTheNightFolder covers a template whose night folder is not
// the last segment: with "<TARGET>/NIGHT_<n>/<FILTER>" every filed row sits two
// components below the folder whose ordinal is being decided. A depth rule would
// collapse the folder's ordinals to nothing here and make the whole consultation a
// no-op; the exact-folder range needs none, so both arms still work.
func TestNestedRowsCountTowardTheNightFolder(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Templates.Light = "<TARGET>/NIGHT_<n>/<FILTER>"

	first := fileLightAs(t, h, "Autorun/Light/a/Light_0001.fit", "M31", dateObsNight1)
	wantFiledUnder(t, h, first, filepath.Join("M31", "NIGHT_1", "HA"))

	// A second spelling of the same target on the SAME night: a fresh ledger scope
	// whose folder already holds this observing date two components down → adopt 1.
	sameNight := fileLightAs(t, h, "Autorun/Light/b/Light_0001.fit", "M  31", dateObsNight1)
	wantFiledUnder(t, h, sameNight, filepath.Join("M31", "NIGHT_1", "HA"))

	// A third spelling on a night the folder does not hold: nothing to adopt, but the
	// nested rows still set the high-water mark → mint 2, never back onto NIGHT_1.
	newNight := fileLightAs(t, h, "Autorun/Light/c/Light_0001.fit", "M   31", dateObsNight2)
	wantFiledUnder(t, h, newNight, filepath.Join("M31", "NIGHT_2", "HA"))
}

// TestAdoptedNightIsDecidedByFrameCount drives the reported archive's own ambiguity:
// one folder holding one observing date at two ordinals. Neither the lowest nor the
// highest is right in general — the folder that predominantly holds the date is the one
// the operator means — so the same library shape must adopt 3 or 1 depending only on
// where the frames are.
//
// The minority side is filed through the real pull path by pre-seeding a
// differently-spelled scope's ledger, which is exactly how the archive got that way.
func TestAdoptedNightIsDecidedByFrameCount(t *testing.T) {
	const misfiledScope = "NGC  7380" // two spaces: same folder after the strip, its own ledger scope
	for _, tc := range []struct {
		name               string
		inNight1, inNight3 int
		want               string
	}{
		{"most frames in NIGHT_3", 1, 2, "NIGHT_3"},
		{"most frames in NIGHT_1", 2, 1, "NIGHT_1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			subject := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")

			// Three real nights of the destination target; the third is the subject's.
			for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
				fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/Light_%04d.fit", i+1), "NGC 7380", dateObs)
			}
			for i := 1; i < tc.inNight3; i++ {
				fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/extra_%04d.fit", i), "NGC 7380", dateObsNight3)
			}

			// The misfiled side: a scope whose ledger already sends this date to 1.
			if n, err := h.store.AssignNight(ctx, store.NightRequest{
				Target: misfiledScope, OTA: "FRA300", ObservingDate: "2025-10-10",
			}); err != nil || n != 1 {
				t.Fatalf("seed misfiled scope: night=%d err=%v", n, err)
			}
			for i := 0; i < tc.inNight1; i++ {
				misfiled := fileLightAs(t, h, fmt.Sprintf("Autorun/Light/bad/Light_%04d.fit", i), misfiledScope, dateObsNight3)
				wantFiledUnder(t, h, misfiled, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_1"))
			}

			res, err := h.w.serviceRetarget(ctx, subject.ID, "NGC7380")
			if err != nil || res.Status != "retargeted" {
				t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
			}
			wantFiledUnder(t, h, reload(t, h, subject.ID), filepath.Join("NGC7380", "FRA300", "S", tc.want))
		})
	}
}

// TestOneObservingDatePerNightFolder (INV-N1) asserts the harm directly, over
// dest_paths rather than over night_counters: two observing dates sharing one night
// directory is what the operator sees, and it is what must not happen. The library it
// asserts over exercises both arms — a spelling change that mints above the folder's
// high-water mark, and a retarget that adopts an ordinal the folder already holds.
func TestOneObservingDatePerNightFolder(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	subject := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")

	for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
		fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/Light_%04d.fit", i+1), "NGC 7380", dateObs)
	}
	// The ASIAIR's own target rename mid-run: a fresh scope, a fourth night, one folder.
	renamed := fileLightAs(t, h, "Autorun/Light/renamed/Light_0001.fit", "NGC7380", dateObsNight4)
	wantFiledUnder(t, h, renamed, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_4"))

	res, err := h.w.serviceRetarget(ctx, subject.ID, "NGC7380")
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("serviceRetarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}
	wantFiledUnder(t, h, reload(t, h, subject.ID), filepath.Join("NGC7380", "FRA300", "S", "NIGHT_3"))

	frames, _, err := h.store.ListFrames(ctx, "rig-01", "", "", 0, 500)
	if err != nil {
		t.Fatalf("ListFrames: %v", err)
	}
	dates := map[string]map[string]bool{}
	for _, f := range frames {
		if f.DestPath == "" || f.Night < 1 || f.DateObs == "" {
			continue
		}
		captured, perr := time.Parse(time.RFC3339, f.DateObs)
		if perr != nil {
			t.Fatalf("frame %d has an unparseable date_obs %q: %v", f.ID, f.DateObs, perr)
		}
		dir := filepath.Dir(f.DestPath)
		if dates[dir] == nil {
			dates[dir] = map[string]bool{}
		}
		dates[dir][route.ObservingDateKey(route.ObservingDate(captured, time.UTC))] = true
	}
	if len(dates) == 0 {
		t.Fatal("no filed lights to check")
	}
	for dir, seen := range dates {
		if len(seen) > 1 {
			t.Errorf("night folder %q holds %d observing dates: %v", dir, len(seen), seen)
		}
	}
}

// TestRetargetKeepsSanitizedTargetsEligible: RewriteObject sanitizes before writing
// the OBJECT card, so a reserved Windows name, a trailing dot and an illegal
// character all retarget successfully — landing as "_CON", "M31" and "NGC_7380" —
// and the preview must call them eligible rather than refuse them.
func TestRetargetKeepsSanitizedTargetsEligible(t *testing.T) {
	for _, tc := range []struct{ typed, folder string }{
		{"CON", "_CON"},
		{"M31.", "M31"},
		{"NGC:7380", "NGC_7380"},
	} {
		t.Run(tc.typed, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			f := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")

			pv := h.w.previewRetarget(ctx, []int64{f.ID}, tc.typed)
			if len(pv) != 1 || !pv[0].Eligible {
				t.Fatalf("preview of %q must stay eligible: %+v", tc.typed, pv)
			}
			res, err := h.w.serviceRetarget(ctx, f.ID, tc.typed)
			if err != nil || res.Status != "retargeted" {
				t.Fatalf("serviceRetarget(%q): status=%q reason=%q err=%v", tc.typed, res.Status, res.Reason, err)
			}
			wantFiledUnder(t, h, reload(t, h, f.ID), filepath.Join(tc.folder, "FRA300", "S", "NIGHT_1"))
		})
	}
}

// nightCounterRows dumps the night ledger exactly as it stands, so a test can prove a
// preview left it untouched. Nothing in production reads the whole table, so there is
// no store method for it: the test opens the same file on its own handle.
func nightCounterRows(t *testing.T, h *harness) []string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open state.db: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(),
		`SELECT target, ota, observing_date, night FROM night_counters
		  ORDER BY target, ota, observing_date`)
	if err != nil {
		t.Fatalf("read night_counters: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var target, ota, date string
		var night int
		if err := rows.Scan(&target, &ota, &date, &night); err != nil {
			t.Fatalf("scan night_counters: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%d", target, ota, date, night))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read night_counters: %v", err)
	}
	return out
}

// nightOf returns the NIGHT_n component of a path, so an assertion can name the
// ordinal a projection or a filing chose rather than a whole temp path.
func nightOf(t *testing.T, path string) string {
	t.Helper()
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.HasPrefix(seg, "NIGHT_") {
			return seg
		}
	}
	t.Fatalf("path %q has no night folder", path)
	return ""
}

// wantPreviewMatchesApply is INV-N6 made executable: preview the ids, prove the
// preview wrote nothing, then apply them in the same order and require the projected
// directory to equal the one apply actually filed each frame into. The equality is
// asserted over frames apply files — one that fails earlier (hash drift, at-rest
// verify) consumes no ordinal, while the preview already advanced its running maximum.
func wantPreviewMatchesApply(t *testing.T, h *harness, ids []int64, target string) []model.RetargetPreview {
	t.Helper()
	ctx := context.Background()

	before := nightCounterRows(t, h)
	pv := h.w.previewRetarget(ctx, ids, target)
	if after := nightCounterRows(t, h); !slices.Equal(before, after) {
		t.Fatalf("preview mutated night_counters:\n before %v\n after  %v", before, after)
	}
	if len(pv) != len(ids) {
		t.Fatalf("preview returned %d rows for %d ids", len(pv), len(ids))
	}

	for i, id := range ids {
		res, err := h.w.serviceRetarget(ctx, id, target)
		if err != nil {
			t.Fatalf("serviceRetarget(%d): %v", id, err)
		}
		if res.Status != "retargeted" {
			continue
		}
		if got, want := filepath.Dir(reload(t, h, id).DestPath), filepath.Dir(pv[i].NewPath); got != want {
			t.Fatalf("id %d at position %d: apply filed into %q, the preview projected %q",
				id, i, got, want)
		}
	}
	return pv
}

// TestPreviewMatchesApply (INV-N6) is the whole point of resolving the night in the
// preview: the operator is shown the folder the frames will actually land in, and the
// projection has to survive being compared against what apply then does. Each subtest
// drives one arm of the projection, and every one of them also asserts the equality and
// that night_counters is byte-identical before and after the preview — a
// previewed-then-abandoned retarget must never spend an ordinal.
func TestPreviewMatchesApply(t *testing.T) {
	// dest files the destination folder's nights under a spelling of its own, so the
	// scope the subject retargets INTO is whatever the test names.
	dest := func(t *testing.T, h *harness, target string, dates ...string) {
		t.Helper()
		for i, d := range dates {
			fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/Light_%s_%04d.fit", target, i+1), target, d)
		}
	}

	t.Run("an assigned date is exact", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		subject := fileLightAs(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit", "C-2025 A6", dateObsNight2)
		dest(t, h, "NGC 7380", dateObsNight1, dateObsNight2, dateObsNight3)

		pv := wantPreviewMatchesApply(t, h, []int64{subject.ID}, "NGC 7380")
		if got := nightOf(t, pv[0].NewPath); got != "NIGHT_2" {
			t.Errorf("projected %s, want NIGHT_2 — the ledger already assigned this date", got)
		}
		if pv[0].NightApprox {
			t.Error("an ordinal read off an existing ledger row cannot move; it must carry no caveat")
		}
	})

	t.Run("the folder's own ordinal is adopted", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		subject := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		// A differently-spelled sibling built the folder, so the retarget target is a
		// fresh scope with nothing to read but the folder itself.
		dest(t, h, "NGC7380", dateObsNight1, dateObsNight2, dateObsNight3)

		pv := wantPreviewMatchesApply(t, h, []int64{subject.ID}, "NGC 7380")
		if got := nightOf(t, pv[0].NewPath); got != "NIGHT_3" {
			t.Errorf("projected %s, want NIGHT_3 — the folder already holds this observing date", got)
		}
		if !pv[0].NightApprox {
			t.Error("an adoption is only true until the next frame is filed; it needs the caveat")
		}
	})

	t.Run("a date no folder holds mints above it", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		subject := fileLightAs(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit", "C-2025 A6", dateObsNight4)
		dest(t, h, "NGC7380", dateObsNight1, dateObsNight2, dateObsNight3)

		pv := wantPreviewMatchesApply(t, h, []int64{subject.ID}, "NGC 7380")
		if got := nightOf(t, pv[0].NewPath); got != "NIGHT_4" {
			t.Errorf("projected %s, want NIGHT_4 — above every ordinal the folder holds", got)
		}
	})

	// The reported archive's own shape, and the one case a projection that ignored
	// AssignNight's rule 2 would get wrong: the folder's NIGHT_2 holds this observing
	// date, but the destination scope already spent 2 on another date, so apply discards
	// the hint and mints 3. A preview that adopted 2 would state a folder apply refuses
	// to use.
	t.Run("an ordinal another date holds is not adopted", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		subject := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		dest(t, h, "NGC 7380", dateObsNight1, dateObsNight2)

		// The misfiled side: a sibling spelling whose ledger sends the subject's own
		// observing date into the folder's NIGHT_2.
		if n, err := h.store.AssignNight(ctx, store.NightRequest{
			Target: "NGC  7380", OTA: "FRA300", ObservingDate: "2025-10-10", Preferred: 2,
		}); err != nil || n != 2 {
			t.Fatalf("seed the misfiled scope: night=%d err=%v", n, err)
		}
		misfiled := fileLightAs(t, h, "Autorun/Light/bad/Light_0001.fit", "NGC  7380", dateObsNight3)
		wantFiledUnder(t, h, misfiled, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_2"))

		pv := wantPreviewMatchesApply(t, h, []int64{subject.ID}, "NGC 7380")
		if got := nightOf(t, pv[0].NewPath); got != "NIGHT_3" {
			t.Errorf("projected %s, want NIGHT_3 — NIGHT_2 is spoken for by another observing date", got)
		}
	})

	// Apply iterates the caller's ids in order and does not dedupe, so the projection
	// has to model exactly that: frames of one observing date share an ordinal, a
	// repeated id costs nothing, and only a genuinely new date advances the batch.
	t.Run("a batch advances once per observing date", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		dest(t, h, "NGC7380", dateObsNight1, dateObsNight2, dateObsNight3)
		a := fileLightAs(t, h, "Autorun/Light/a/Light_0001.fit", "C-2025 A6", dateObsNight4)
		b := fileLightAs(t, h, "Autorun/Light/b/Light_0001.fit", "C-2025 A6", dateObsNight4)
		c := fileLightAs(t, h, "Autorun/Light/c/Light_0001.fit", "C-2025 A6", dateObsNight5)

		ids := []int64{a.ID, a.ID, b.ID, c.ID}
		pv := wantPreviewMatchesApply(t, h, ids, "NGC 7380")
		want := []string{"NIGHT_4", "NIGHT_4", "NIGHT_4", "NIGHT_5"}
		for i, w := range want {
			if got := nightOf(t, pv[i].NewPath); got != w {
				t.Errorf("row %d projected %s, want %s (%v)", i, got, w, want)
			}
		}
	})
}

// nightReadFailStore fails the ledger read every night projection begins with, so a
// test can drive the preview's error policy without a real database fault.
type nightReadFailStore struct {
	store.Store
}

func (s *nightReadFailStore) LookupNight(ctx context.Context, target, ota, observingDate string) (int, bool, error) {
	return 0, false, errors.New("injected ledger read failure")
}

// TestPreviewSurvivesAFailedNightRead pins the error policy: a read the preview cannot
// make says nothing about the frames, so the rows stay eligible and explain themselves
// — and they render at the next ordinal the batch would take rather than at NIGHT_0,
// the folder that cannot exist. Two failed frames must not project one ordinal.
func TestPreviewSurvivesAFailedNightRead(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	a := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
	b := fileLightAs(t, h, "Autorun/Light/C-2025 A6/Light_0002.fit", "C-2025 A6", dateObsNight4)
	h.w.store = &nightReadFailStore{Store: h.store}

	pv := h.w.previewRetarget(ctx, []int64{a.ID, b.ID}, "NGC 7380")
	if len(pv) != 2 {
		t.Fatalf("want 2 preview rows, got %d", len(pv))
	}
	for i, want := range []string{"NIGHT_1", "NIGHT_2"} {
		if !pv[i].Eligible {
			t.Errorf("row %d: a ledger read failure says nothing about the frame; it must stay eligible", i)
		}
		if pv[i].Reason == "" {
			t.Errorf("row %d: an eligible row that could not resolve its night must say so", i)
		}
		if !pv[i].NightApprox {
			t.Errorf("row %d: an unresolved ordinal is not exact", i)
		}
		if got := nightOf(t, pv[i].NewPath); got != want {
			t.Errorf("row %d rendered %s, want %s", i, got, want)
		}
	}
}

// TestPreviewReportsExistingSpellings covers design routing-and-filing §6.7: unifying
// the night does not unify the archive. The retargeted subs carry the typed OBJECT
// while their siblings keep theirs, and anything grouping by OBJECT — re-file included
// — still sees two targets, so the preview names the spellings already in the folder
// it resolved to. Only that folder: a spelling in another night of the same prefix is
// not the operator's problem here.
func TestPreviewReportsExistingSpellings(t *testing.T) {
	// seed builds a destination folder whose three nights were filed under "NGC  7380",
	// plus one frame of a third spelling in NIGHT_1, and returns a subject captured on
	// the folder's third night.
	seed := func(t *testing.T, h *harness) *model.Frame {
		t.Helper()
		subject := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit")
		for i, d := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
			fileLightAs(t, h, fmt.Sprintf("Autorun/Light/dest/Light_%04d.fit", i+1), "NGC  7380", d)
		}
		sibling := fileLightAs(t, h, "Autorun/Light/other/Light_0001.fit", "NGC   7380", dateObsNight1)
		wantFiledUnder(t, h, sibling, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_1"))
		return subject
	}

	t.Run("a divergent spelling in the resolved folder", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		subject := seed(t, h)

		pv := h.w.previewRetarget(context.Background(), []int64{subject.ID}, "NGC 7380")
		if got := nightOf(t, pv[0].NewPath); got != "NIGHT_3" {
			t.Fatalf("projected %s, want NIGHT_3", got)
		}
		if !slices.Equal(pv[0].ExistingSpellings, []string{"NGC  7380"}) {
			t.Errorf("existing_spellings=%q, want just the resolved folder's own spelling —\n"+
				"the third spelling sits in NIGHT_1, a folder this retarget does not touch",
				pv[0].ExistingSpellings)
		}
	})

	t.Run("no divergence when the spellings agree", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		subject := seed(t, h)

		pv := h.w.previewRetarget(context.Background(), []int64{subject.ID}, "NGC  7380")
		if len(pv[0].ExistingSpellings) != 0 {
			t.Errorf("existing_spellings=%q, want none: the folder holds this exact spelling",
				pv[0].ExistingSpellings)
		}
	})
}
