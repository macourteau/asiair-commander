package rig

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The synthetic light every revision test is built from: a conforming primary HDU
// over a real data unit. Every committed ASIAIR fixture is header-only, so none of
// them can prove an exposure identity at all — a zero-length data unit is refused as
// an identity by construction.
const (
	revW, revH = 60, 48
	revTarget  = "C-2025 A6"
)

// revisionPair returns hardware finding H13's pair for one light: the frame as the
// device first writes it, and the same exposure after the device rewrites it in
// place to inject the WCS it has since solved. The data unit is byte-identical
// across the two; the header is one 2880-byte block longer.
func revisionPair(target string) (original, revised []byte) {
	return fitstest.Revision(fitstest.LightCards(revW, revH, target), fitstest.SyntheticImage(revW, revH))
}

// dataDigestOf is the exposure identity of a FITS file held in memory, computed the
// way the pull path computes it off the bytes as they pass.
func dataDigestOf(body []byte) string {
	tee := header.NewDigestTee()
	_, _ = tee.Write(body)
	_, data := tee.Sums()
	return data
}

// pullFile puts one file on the device and runs a cycle, returning the row it filed.
func pullFile(t *testing.T, h *harness, src string, body []byte, age time.Duration) *model.Frame {
	t.Helper()
	h.dev.AddFile(emmc, src, body, h.nowVal.Add(-age))
	h.cycle()
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("%s was not filed", src)
	}
	return f
}

// dirEntries lists the base names of a filed frame's directory, so a test can say
// "one file, not two".
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// TestDeviceRevisionSupersedesTheArchiveCopy (design frame-lifecycle AC1) replays
// the observed sequence: a frame filed, then the same frame re-listed 2880 bytes
// larger with its data unit unchanged. One file survives at the canonical name
// holding the revised bytes, no rot_clash is raised, and both sides of the row
// describe the copy they name.
func TestDeviceRevisionSupersedesTheArchiveCopy(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	if f.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the filing path must record the exposure identity, got %q", f.DataSHA256)
	}
	dest := f.DestPath

	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	if names := dirEntries(t, filepath.Dir(dest)); len(names) != 1 {
		t.Fatalf("a revision must leave ONE file, got %v", names)
	}
	if got := hashOnDisk(t, dest); got != sha256hex(revised) {
		t.Fatalf("the canonical name must hold the revised bytes")
	}
	if h.hasAlert(model.AlertRotClash) {
		t.Fatal("a proven revision is not a content clash; no rot_clash may be raised")
	}

	got := mustGetFrame(t, h, f.ID)
	if got.DestPath != dest {
		t.Fatalf("the frame must stay at its own name: %q", got.DestPath)
	}
	if got.SHA256 != sha256hex(revised) || got.Size != int64(len(revised)) {
		t.Fatalf("the device anchors must advance: sha=%q size=%d", got.SHA256, got.Size)
	}
	if got.Mtime != h.nowVal.Add(-5*time.Minute).Unix() {
		t.Fatalf("mtime = %d, want the revised file's %d", got.Mtime, h.nowVal.Add(-5*time.Minute).Unix())
	}
	if got.ArchiveSHA256 != sha256hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must describe the bytes on the NAS: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the exposure identity is what survives the revision, got %q", got.DataSHA256)
	}
	assertArchiveAnchors(t, h, f.ID, "a superseded frame")
	if _, ok, _ := h.store.GetFrameByID(ctx, f.ID); !ok {
		t.Fatal("the row must survive its own supersede")
	}
}

// TestDeviceRevisionCommitsEveryDestPathSharer (design frame-lifecycle AC1, the H12
// half) covers the two-share case the ledger really carries: one session's frames
// split across the USB stick and the eMMC and reunite in one folder through the
// idempotent keep, so two rows name one archive file. The swap rewrites that file,
// so the sharer's archive anchors have to move with it while its own device copy —
// which the swap never read — keeps the anchors that describe it. Committing the
// revised row alone would hold the sharer with a permanent critical delete_mismatch
// against an archive copy that is perfectly good.
func TestDeviceRevisionCommitsEveryDestPathSharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	original, revised := revisionPair(revTarget)
	// Two device paths, one file name: the second is filed by the idempotent keep
	// onto the first's dest_path, which is what creates the shared archive copy.
	revisedSrc := "Autorun/Light/C-2025 A6/Light_0001.fit"
	sharerSrc := "Autorun/Light/C-2025 A6 (usb)/Light_0001.fit"

	revisedRow := pullFile(t, h, revisedSrc, original, 10*time.Minute)
	sharerRow := pullFile(t, h, sharerSrc, original, 10*time.Minute)
	if sharerRow.DestPath != revisedRow.DestPath {
		t.Fatalf("setup: the two rows must share one archive copy, got %q and %q", sharerRow.DestPath, revisedRow.DestPath)
	}
	sharerBefore := mustGetFrame(t, h, sharerRow.ID)

	// Short of the 1h grace, so the cycle's own delete phase does not drain either
	// original before the swap is asserted.
	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, revisedSrc, revised, 5*time.Minute)

	sharer := mustGetFrame(t, h, sharerRow.ID)
	if sharer.ArchiveSHA256 != sha256hex(revised) || sharer.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the sharer's archive anchors must move with the file: sha=%q size=%d", sharer.ArchiveSHA256, sharer.ArchiveSize)
	}
	if sharer.SHA256 != sharerBefore.SHA256 || sharer.Size != sharerBefore.Size || sharer.Mtime != sharerBefore.Mtime {
		t.Fatalf("the sharer's device copy was never read, so its device anchors must not move: %+v", sharer)
	}
	if sharer.PulledAt != sharerBefore.PulledAt || sharer.State != sharerBefore.State {
		t.Fatalf("the sharer's own lifecycle is not this swap's to advance: pulled_at=%d state=%s", sharer.PulledAt, sharer.State)
	}

	// The proof that the split is right: both rows drain, each copy compared against
	// the anchors recorded for the copy it is.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("neither row may be held after a supersede; got %v", h.raisedCategories())
	}
	for _, src := range []string{revisedSrc, sharerSrc} {
		got, _ := h.getFrame(src)
		if got.State != model.StateDeleted {
			t.Fatalf("%s: state=%s, want deleted", src, got.State)
		}
		if h.deviceHas(src) {
			t.Fatalf("%s: the proven original should have drained", src)
		}
	}
}

// sameSizeRevisionPair returns H13's pair for a sensor whose base header has room
// for the injected WCS inside the blocks it already occupies: the revision rewrites
// blank padding rather than adding a block, so the two files differ byte for byte at
// an identical length with their data unit at an identical offset.
func sameSizeRevisionPair(target string) (original, revised []byte) {
	return fitstest.SameSizeRevision(fitstest.LightCards(revW, revH, target), fitstest.SyntheticImage(revW, revH))
}

// TestSameSizeRevisionIsRecognised (design frame-lifecycle AC2) carries the
// portability claim of design frame-lifecycle §11.4 on its own. Every other replay
// criterion uses the +2880-byte growth, which is what an ASI6200MM header shows — whether the
// injected WCS overflows a further header block depends on how many cards the base
// header already carries, so on another sensor the revision leaves the length
// untouched. The discriminator turns on the data unit and never on the size, and this
// is where that is proven: without it the whole design is asserted only for one
// sensor's header layout.
//
// The same-size revision of an already-culled frame is deliberately not here: decide's
// skip gate is keyed on size, so such a frame is never re-listed as changed at all —
// the residual the design records in design frame-lifecycle §11.12.
func TestSameSizeRevisionIsRecognised(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := sameSizeRevisionPair(revTarget)
	if len(original) != len(revised) {
		t.Fatalf("setup: the pair must be the same length, got %d and %d", len(original), len(revised))
	}
	if sha256hex(original) == sha256hex(revised) {
		t.Fatal("setup: the pair must differ byte for byte")
	}
	if dataDigestOf(original) != dataDigestOf(revised) {
		t.Fatal("setup: only the header may differ, so the data unit must be identical")
	}

	f := pullFile(t, h, src, original, 10*time.Minute)
	dest := f.DestPath

	// An hour on, so the revision's mtime is the only listed field that moves: the
	// size is unchanged, which is the whole point of the shape.
	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	if names := dirEntries(t, filepath.Dir(dest)); len(names) != 1 {
		t.Fatalf("a revision must leave ONE file whatever the size did, got %v", names)
	}
	if got := hashOnDisk(t, dest); got != sha256hex(revised) {
		t.Fatal("the canonical name must hold the revised bytes")
	}
	if h.hasAlert(model.AlertRotClash) {
		t.Fatalf("a proven revision is not a content clash whatever the size did; got %v", h.raisedCategories())
	}

	got := mustGetFrame(t, h, f.ID)
	if got.DestPath != dest {
		t.Fatalf("the frame must stay at its own name: %q", got.DestPath)
	}
	if got.SHA256 != sha256hex(revised) || got.Mtime != h.nowVal.Add(-5*time.Minute).Unix() {
		t.Fatalf("the device anchors must advance on bytes and mtime alone: sha=%q mtime=%d", got.SHA256, got.Mtime)
	}
	if got.Size != int64(len(revised)) {
		t.Fatalf("size = %d, want the revision's unchanged %d", got.Size, len(revised))
	}
	if got.ArchiveSHA256 != sha256hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must describe the bytes on the NAS: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the exposure identity is what survives the revision, got %q", got.DataSHA256)
	}
	assertArchiveAnchors(t, h, f.ID, "a frame superseded at an unchanged size")
}

// TestDifferentExposureAtTheSamePathStillClashes (design frame-lifecycle AC4) is the
// discriminator's other half: content that differs from the row while its data unit
// does NOT match is a different exposure at a reused path, and every existing
// behaviour applies to it unchanged. Nothing is overwritten. The second exposure is
// built at the FIRST one's length deliberately — the outcome must not turn on the
// size either way.
func TestDifferentExposureAtTheSamePathStillClashes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	cards := fitstest.LightCards(revW, revH, revTarget)
	first := fitstest.Build(cards, fitstest.SyntheticImage(revW, revH))
	second := fitstest.Build(cards, otherImage(revW, revH))
	if len(first) != len(second) {
		t.Fatalf("setup: the two exposures must be the same length, got %d and %d", len(first), len(second))
	}

	f := pullFile(t, h, src, first, 10*time.Minute)
	dest := f.DestPath
	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, second, 5*time.Minute)

	if got := hashOnDisk(t, dest); got != sha256hex(first) {
		t.Fatal("a different exposure must never overwrite the archived one")
	}
	if names := dirEntries(t, filepath.Dir(dest)); len(names) != 2 {
		t.Fatalf("the second exposure must be filed under a token, got %v", names)
	}
	if !h.hasAlert(model.AlertRotClash) {
		t.Fatalf("a genuine content clash still raises rot_clash; got %v", h.raisedCategories())
	}
}

// otherImage is a second exposure of the same shape whose pixels differ, so its data
// unit hashes differently at an identical length.
func otherImage(w, h int) []byte {
	img := fitstest.SyntheticImage(w, h)
	for i := range img {
		img[i] ^= 0x5a
	}
	return img
}

// TestUnparseableFileNeverReplacesAValidOne (design frame-lifecycle AC5) pins the
// structural gate in both orderings. A valid FITS file is never replaced by one that
// does not parse, and there is no converse rule: Commander does not repair an
// archive copy that does not parse from a device copy that does.
func TestUnparseableFileNeverReplacesAValidOne(t *testing.T) {
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	t.Run("incoming does not parse", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := pullFile(t, h, src, original, 10*time.Minute)
		dest := f.DestPath

		h.nowVal = h.nowVal.Add(time.Hour)
		pullFile(t, h, src, []byte("not a FITS file at all, no END card anywhere"), 5*time.Minute)

		if got := hashOnDisk(t, dest); got != sha256hex(original) {
			t.Fatal("a file that does not parse must never replace one that does")
		}
	})

	t.Run("archive copy does not parse", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f := pullFile(t, h, src, original, 10*time.Minute)
		// The archive copy rots into something that is no longer FITS, and its anchor
		// is re-recorded against those bytes: the whole-file leg therefore agrees and
		// only the parse decides. Commander must not "repair" it from the device.
		rotted := []byte("this archive copy is no longer a FITS file")
		writeLocal(t, f.DestPath, rotted)
		row := mustGetFrame(t, h, f.ID)
		row.ArchiveSHA256, row.ArchiveSize = sha256hex(rotted), int64(len(rotted))
		if err := h.store.UpsertFrame(ctx, &row); err != nil {
			t.Fatalf("re-anchor the rotted copy: %v", err)
		}

		h.nowVal = h.nowVal.Add(time.Hour)
		pullFile(t, h, src, revised, 5*time.Minute)

		if got := hashOnDisk(t, f.DestPath); got != sha256hex(rotted) {
			t.Fatal("an archive copy that does not parse is not repaired from the device copy")
		}
		if !h.hasAlert(model.AlertRotClash) {
			t.Fatalf("the revised frame takes the ordinary token-and-alert path; got %v", h.raisedCategories())
		}
	})
}

// TestZeroLengthDataUnitIsNeverAnIdentity (design frame-lifecycle AC6) covers the
// file that parses and carries no data unit — legal FITS, and the one digest every
// such file shares. Two header-only frames would agree on it deterministically,
// which is the case nothing probabilistic may reach, so the precondition refuses it
// wherever identity is tested: on the discriminator, and on the re-apply
// disjunction.
func TestZeroLengthDataUnitIsNeverAnIdentity(t *testing.T) {
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	first := fitstest.Build(fitstest.LightCards(revW, revH, revTarget), nil)
	second := fitstest.Build(append(fitstest.LightCards(revW, revH, revTarget),
		"COMMENT   a second, unrelated header-only frame"), nil)

	t.Run("no supersede is authorized", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := pullFile(t, h, src, first, 10*time.Minute)
		if f.DataSHA256 != header.EmptyDataDigest {
			t.Fatalf("setup: a header-only frame records the empty-input digest, got %q", f.DataSHA256)
		}
		dest := f.DestPath

		h.nowVal = h.nowVal.Add(time.Hour)
		pullFile(t, h, src, second, 5*time.Minute)

		if got := hashOnDisk(t, dest); got != sha256hex(first) {
			t.Fatal("two header-only frames are not the same exposure; nothing may be overwritten")
		}
		if !h.hasAlert(model.AlertRotClash) {
			t.Fatalf("the second frame takes the ordinary token-and-alert path; got %v", h.raisedCategories())
		}
	})

	t.Run("no overlay is re-applied", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		f := pullFile(t, h, src, first, 10*time.Minute)
		if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
			t.Fatalf("retarget: status=%q err=%v", res.Status, err)
		}

		h.nowVal = h.nowVal.Add(time.Hour)
		pullFile(t, h, src, second, 5*time.Minute)

		// Re-applied to a file it does not describe, the rewrite passes its own
		// faithfulness check and then fails the at-rest comparison, which classifyAndFile
		// turns into a deferral — the frame is never filed and is re-pulled every cycle.
		if h.logContains("re-apply retarget failed; deferring file") {
			t.Fatal("the overlay must not be re-applied to a frame that only shares an empty data unit")
		}
		after := mustGetFrame(t, h, f.ID)
		if after.SHA256 != sha256hex(second) {
			t.Fatalf("the second frame must be filed as the new content it is, got sha=%q", after.SHA256)
		}
		if after.RetargetSHA256 != "" {
			t.Fatalf("genuinely new content resets the overlay, got %q", after.RetargetSHA256)
		}
	})
}

// retargetTo re-labels one filed frame and returns the row the operation left.
func retargetTo(t *testing.T, h *harness, id int64, target string) model.Frame {
	t.Helper()
	res, err := h.w.serviceRetarget(context.Background(), id, target)
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget %d to %q: status=%q reason=%q err=%v", id, target, res.Status, res.Reason, err)
	}
	return mustGetFrame(t, h, id)
}

// objectOf reads the OBJECT card a filed archive copy actually carries, so a test
// can say the re-label survived rather than trusting the row that claims it did.
func objectOf(t *testing.T, h *harness, path string) string {
	t.Helper()
	hdr, err := h.w.hdr.ReadHeader(path)
	if err != nil {
		t.Fatalf("read header %s: %v", path, err)
	}
	obj, _ := hdr.Str("OBJECT")
	return obj
}

// TestRetargetedFrameSurvivesARevision (design frame-lifecycle AC8) is the second
// consequence: a device-side revision is indistinguishable from genuinely new content
// under a rule that re-applies on any content change, so the OBJECT edit would revert
// and the frame would route back to the folder it was moved out of. Under the disjunction it keeps its overlay and
// its folder, is actually filed rather than deferred, and the audit record beside the
// archive anchor describes the bytes the NAS now holds.
func TestRetargetedFrameSurvivesARevision(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	relabelled := retargetTo(t, h, f.ID, "M31")
	dest := relabelled.DestPath

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	got := mustGetFrame(t, h, f.ID)
	if got.DestPath != dest {
		t.Fatalf("the frame must stay in the folder it was re-labelled into: %q, want %q", got.DestPath, dest)
	}
	if got.Target != relabelled.Target || got.RetargetFrom != relabelled.RetargetFrom {
		t.Fatalf("the re-label must survive: target=%q from=%q", got.Target, got.RetargetFrom)
	}
	if names := dirEntries(t, filepath.Dir(dest)); len(names) != 1 {
		t.Fatalf("the revision is filed in place, not beside itself: %v", names)
	}
	onDisk := hashOnDisk(t, dest)
	if got.ArchiveSHA256 != onDisk || got.RetargetSHA256 != onDisk {
		t.Fatalf("both the archive anchor and the audit record must describe the file: archive=%q overlay=%q disk=%q",
			got.ArchiveSHA256, got.RetargetSHA256, onDisk)
	}
	if got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("archive_size = %d, want the edited revision's %d", got.ArchiveSize, len(revised))
	}
	if obj := objectOf(t, h, dest); obj != "M31" {
		t.Fatalf("the archive copy's OBJECT card reads %q, want the re-label", obj)
	}
	if dataDigestOf(readLocal(t, dest)) != dataDigestOf(original) {
		t.Fatal("the edit changed the label; the science data is the same exposure")
	}
}

// TestRetargetedFrameWithNoRecordedIdentityIsNotSuperseded guards the one shape in
// which a supersede would install the device's UNEDITED bytes under a record
// claiming an OBJECT edit. The row is retargeted and was filed before data_sha256
// existed, and the backfill cursor has not reached it: the re-apply's disjunction
// holds on neither arm — there is no recorded identity, and a revision changes the
// stream hash — so no edit is re-applied, while provenRevisionOf establishes leg 2
// from the archive copy's own read and proves the revision regardless.
//
// The supersede must refuse there. Left to proceed it is consequence 2 of the
// design's Problem section, reintroduced and then permanently undetectable:
// archive_sha256, retarget_sha256 and the on-disk hash would all agree, so every
// later VerifyRead, three-way check and sameArchiveAt passes while the row's target
// and the file's OBJECT disagree forever.
func TestRetargetedFrameWithNoRecordedIdentityIsNotSuperseded(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	relabelled := retargetTo(t, h, f.ID, "M31")
	dest := relabelled.DestPath
	// A row with no recorded data digest, which the backfill has not reached yet.
	forgetDataDigest(t, h, f.ID)

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	if !h.logContains("carries an overlay that was not re-applied") {
		t.Fatal("the supersede must refuse a row whose overlay the caller could not re-apply")
	}
	if obj := objectOf(t, h, dest); obj != "M31" {
		t.Fatalf("the re-labelled archive copy's OBJECT card reads %q, want the edit it was left with", obj)
	}
	if got := hashOnDisk(t, dest); got != relabelled.RetargetSHA256 {
		t.Fatalf("a refused supersede leaves the archive copy untouched: %q, want %q", got, relabelled.RetargetSHA256)
	}
	got := mustGetFrame(t, h, f.ID)
	if got.RetargetSHA256 == sha256hex(revised) {
		t.Fatal("the audit record must never describe bytes carrying the device's own OBJECT card")
	}
	// The ordinary path for content the frame cannot prove itself the revision of:
	// filed under its own name with the overlay reset, which is the reverted
	// re-label the backfill then fixes for good rather than a copy silently
	// mislabelled forever.
	if got.DestPath == dest {
		t.Fatalf("a refused supersede must not file over the re-labelled copy: %q", got.DestPath)
	}
	if got.RetargetSHA256 != "" {
		t.Fatalf("genuinely-new content resets the overlay as it does for any frame, got %q", got.RetargetSHA256)
	}
}

// TestBackfilledRetargetedFrameThenSupersedes is the other half of that refusal:
// it is self-resolving. Once the backfill records the row's data_sha256 from an
// archive copy proven to be the archived bytes, the re-apply's first arm holds, the
// edit is re-applied to the revision and the supersede proceeds with the re-label
// intact — so the refusal costs a cycle, not the frame.
func TestBackfilledRetargetedFrameThenSupersedes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	relabelled := retargetTo(t, h, f.ID, "M31")
	dest := relabelled.DestPath
	forgetDataDigest(t, h, f.ID)

	BackfillDataDigests(context.Background(), h.store, h.lfs, discardLogger())
	if got := mustGetFrame(t, h, f.ID); got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("setup: the backfill must record the exposure identity, got %q", got.DataSHA256)
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	if h.logContains("carries an overlay that was not re-applied") {
		t.Fatal("a row carrying its exposure identity re-applies its overlay and supersedes")
	}
	got := mustGetFrame(t, h, f.ID)
	if got.DestPath != dest {
		t.Fatalf("the frame must stay in the folder it was re-labelled into: %q, want %q", got.DestPath, dest)
	}
	if names := dirEntries(t, filepath.Dir(dest)); len(names) != 1 {
		t.Fatalf("the revision is filed in place, not beside itself: %v", names)
	}
	if obj := objectOf(t, h, dest); obj != "M31" {
		t.Fatalf("the archive copy's OBJECT card reads %q, want the re-label", obj)
	}
	onDisk := hashOnDisk(t, dest)
	if got.ArchiveSHA256 != onDisk || got.RetargetSHA256 != onDisk {
		t.Fatalf("both the archive anchor and the audit record must describe the file: archive=%q overlay=%q disk=%q",
			got.ArchiveSHA256, got.RetargetSHA256, onDisk)
	}
	if got.SHA256 != sha256hex(revised) {
		t.Fatalf("the device anchor must describe the revised device copy, got %q", got.SHA256)
	}
}

// TestRevisionReapplyRefusesACorruptingWriter (design frame-lifecycle AC8) keeps the
// faithfulness proof load-bearing across the re-anchoring: the three checks are the
// INV-4 assertions that authorize deleting a device original, so re-anchoring them to
// the file in hand must not relax any of them. A writer that touches one byte outside
// the OBJECT card is still refused, and the frame is deferred rather than filed
// against a corrupted edit.
func TestRevisionReapplyRefusesACorruptingWriter(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	relabelled := retargetTo(t, h, f.ID, "M31")
	before := hashOnDisk(t, relabelled.DestPath)

	h.w.rewriter = func(srcPath, dst, newTarget string) (header.RewriteResult, error) {
		rr, err := header.RewriteObject(srcPath, dst, newTarget)
		if err != nil {
			return rr, err
		}
		b, rerr := os.ReadFile(dst)
		if rerr != nil {
			return rr, rerr
		}
		b[len(b)-1] ^= 0xFF // the final byte lives in the data unit, never the header
		if werr := os.WriteFile(dst, b, 0o644); werr != nil {
			return rr, werr
		}
		rr.ComplementOut[0] ^= 0xFF // an honest digest over the corrupted output
		return rr, nil
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	h.dev.AddFile(emmc, src, revised, h.nowVal.Add(-5*time.Minute))
	h.cycle()

	if !h.logContains("re-apply retarget failed; deferring file") {
		t.Fatal("a corrupting edit must be refused before anything is filed")
	}
	if got := hashOnDisk(t, relabelled.DestPath); got != before {
		t.Fatal("the archive copy must be untouched when the edit is refused")
	}
	if got := mustGetFrame(t, h, f.ID); got.SHA256 != relabelled.SHA256 {
		t.Fatalf("a refused re-apply files nothing, so the row's device anchor stands: %q", got.SHA256)
	}
}

// TestRevisedRetargetedFrameAnchorsToTheRevisedDeviceBytes (design frame-lifecycle
// AC16) is asserted apart from design frame-lifecycle AC8 because every assertion
// there is satisfiable while this is broken. A filing path that committed
// prior.SHA256 on the re-apply branch would keep the row pointing at an untouched
// device original; after a revision there is none, and the pre-revision hash leaves
// deviceHash != f.SHA256 on every delete pass — a permanent critical
// delete_mismatch on the path revision handling exists for.
func TestRevisedRetargetedFrameAnchorsToTheRevisedDeviceBytes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	retargetTo(t, h, f.ID, "M31")

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	got := mustGetFrame(t, h, f.ID)
	if got.SHA256 != sha256hex(revised) {
		t.Fatalf("sha256 = %q, want the REVISED device bytes %q", got.SHA256, sha256hex(revised))
	}
	if got.Size != int64(len(revised)) {
		t.Fatalf("size = %d, want the revised device file's %d", got.Size, len(revised))
	}

	// The proof that matters: the next delete pass reaches the three-way check and
	// passes it, the device leg against sha256 and the archive leg against
	// archive_sha256 — two values rather than one.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("the revised original must verify against its own row; got %v", h.raisedCategories())
	}
	if after := mustGetFrame(t, h, f.ID); after.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted (the three-way check passed)", after.State)
	}
	if h.deviceHas(src) {
		t.Fatal("the proven revised original should have drained")
	}
}

// TestSupersedeAdvancesASharersOverlay (design frame-lifecycle AC9) is the one place
// the sharer's overlay advance is asserted: design frame-lifecycle AC1's sharer
// carries none. Two retargeted rows come to share one archive file through the
// idempotent keep — the second row's edit is byte-identical to the first's, so it
// adopts that file rather than writing another — and a swap of those bytes has to
// move both rows' audit records with them. An audit record that silently stops
// matching the file it describes is why an audit record never serves as an
// archive-side fallback.
func TestSupersedeAdvancesASharersOverlay(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	original, revised := revisionPair(revTarget)
	revisedSrc := "Autorun/Light/C-2025 A6/Light_0001.fit"
	sharerSrc := "Autorun/Light/C-2025 A6 (usb)/Light_0001.fit"

	a := pullFile(t, h, revisedSrc, original, 10*time.Minute)
	retargetTo(t, h, a.ID, "M31")
	b := pullFile(t, h, sharerSrc, original, 10*time.Minute)
	sharerBefore := retargetTo(t, h, b.ID, "M31")
	revisedBefore := mustGetFrame(t, h, a.ID)
	if sharerBefore.DestPath != revisedBefore.DestPath {
		t.Fatalf("setup: the two re-labelled rows must share one archive copy, got %q and %q",
			sharerBefore.DestPath, revisedBefore.DestPath)
	}
	if sharerBefore.RetargetSHA256 == "" {
		t.Fatal("setup: the sharer must carry an overlay for this test to cover the advance")
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, revisedSrc, revised, 5*time.Minute)

	onDisk := hashOnDisk(t, revisedBefore.DestPath)
	for _, tc := range []struct {
		what string
		id   int64
	}{{"the revised row", a.ID}, {"the sharer", b.ID}} {
		got := mustGetFrame(t, h, tc.id)
		if got.ArchiveSHA256 != onDisk || got.RetargetSHA256 != onDisk {
			t.Fatalf("%s: archive=%q overlay=%q must both describe the file (%q)",
				tc.what, got.ArchiveSHA256, got.RetargetSHA256, onDisk)
		}
		if got.ArchiveSize != int64(len(revised)) {
			t.Fatalf("%s: archive_size = %d, want %d", tc.what, got.ArchiveSize, len(revised))
		}
	}
	sharer := mustGetFrame(t, h, b.ID)
	if sharer.SHA256 != sharerBefore.SHA256 || sharer.Size != sharerBefore.Size {
		t.Fatalf("the sharer's device copy was not read, so its device anchors stand: sha=%q size=%d", sharer.SHA256, sharer.Size)
	}

	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("neither row may be held on its next delete pass; got %v", h.raisedCategories())
	}
	for _, src := range []string{revisedSrc, sharerSrc} {
		if got, _ := h.getFrame(src); got.State != model.StateDeleted {
			t.Fatalf("%s: state=%s, want deleted", src, got.State)
		}
	}
}

// TestSupersedeReExtractsTheAstrometry (design frame-lifecycle AC17) pins the one
// outcome design frame-lifecycle AC1 passes right through: a Tier-1 verdict is
// terminal and never retried, so without a re-extraction it shadows the WCS the device
// has since written into the header. What is asserted is that outcome and deliberately
// NOT the ClearFrameAstrometry call — an implementation that commits outside
// UpsertFrame overwrites the stale verdict either way, so demanding the clear would be
// asserting the call rather than anything an operator can observe.
func TestSupersedeReExtractsTheAstrometry(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	// Tier 1 ran on the pre-revision frame and found no match. That verdict is
	// terminal and never retried, so it outlives every ordinary re-file.
	if err := h.store.SetFrameAstrometry(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusFailed}); err != nil {
		t.Fatalf("seed the Tier-1 verdict: %v", err)
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	got := mustGetFrame(t, h, f.ID)
	if got.Astrometry == nil || got.Astrometry.Status != model.AstroStatusHeader {
		t.Fatalf("the header the device wrote must win over the stale verdict, got %+v", got.Astrometry)
	}
	if got.Astrometry.Solution == nil {
		t.Fatal("the WCS the device injected must be extracted, not just the status")
	}
}

// TestSupersedeRestartsTheGraceClock (design frame-lifecycle AC21) holds the
// deletion grace to its reason: the window exists so a second device read is
// genuinely independent of the first, and a superseded archive copy has just been
// read once. The rule of design frame-lifecycle §8.4 that a re-pull must not restart
// the clock governs a crash re-pull of IDENTICAL bytes; a revision is different
// bytes, freshly written and freshly verified.
func TestSupersedeRestartsTheGraceClock(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	// Past the 1h grace of the ORIGINAL pull, so the only clock that can keep the
	// original alive below is the one the supersede restarts.
	h.nowVal = h.nowVal.Add(65 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	superseded := mustGetFrame(t, h, f.ID)
	if superseded.PulledAt != h.nowVal.Unix() {
		t.Fatalf("pulled_at = %d, want the supersede instant %d", superseded.PulledAt, h.nowVal.Unix())
	}

	h.nowVal = h.nowVal.Add(25 * time.Minute)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	if got := mustGetFrame(t, h, f.ID); got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified: the grace clock restarted at the supersede", got.State)
	}
	if !h.deviceHas(src) {
		t.Fatal("the device original must not be deletable before grace elapses from the supersede")
	}

	h.nowVal = h.nowVal.Add(time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	if got := mustGetFrame(t, h, f.ID); got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted once grace has elapsed from the supersede", got.State)
	}
}

// addSecondRig gives the harness a second rig filing into the same library through
// the same filesystem seam and the same store, which is how two ledgers come to name
// one archive file. It re-resolves the harness's own profile pointer, since appending
// to the config's rig slice can move it.
func addSecondRig(t *testing.T, h *harness) (*Worker, *device.Fake) {
	t.Helper()
	h.cfg.Rigs = append(h.cfg.Rigs, model.RigProfile{
		ID: "rig-02", Name: "Second", Host: "h2", OTA: "FRA300", Session: "S",
		GUID: "dev-guid", Managed: true, CapacityConstrained: true,
		Sources: []model.Source{{Share: emmc, Roots: []string{"Autorun"}}},
	})
	h.cfgPtr.Store(h.cfg)
	h.profile = h.cfg.Rig("rig-01")

	dev := device.NewFake()
	dev.AddShare(emmc)
	logger := slog.New(slog.NewTextHandler(h.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := NewWorker("rig-02", h.cfgPtr, func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev },
		h.resolver, netsite.NewRegistry(logger), h.lfs, header.New(), h.store, h.hub, h.notify, h.adm, nil, nil,
		time.UTC, func() time.Time { return h.nowVal }, h.metrics, logger)
	w.lookupHost = noLookup
	return w, dev
}

// TestFilingRefusesToSupersedeACrossRigSharer (design frame-lifecycle AC23) drives the guard end to
// end through the filing path, where the primitive's own test drives it directly. It is the guard
// the invariant states rather than a mechanism: FramesByDestPath is rig-scoped by construction, so
// a row on another rig's ledger is out of reach of the commit and would be left anchored to
// pre-revision bytes in a file that holds revised ones — a permanent critical delete_mismatch on a
// ledger the write never touched. The repo already meets that hazard by refusing, and a refused
// frame takes the ordinary token-and-alert path.
func TestFilingRefusesToSupersedeACrossRigSharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	w2, dev2 := addSecondRig(t, h)
	profile2 := h.cfg.Rig("rig-02")
	original, revised := revisionPair(revTarget)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"

	// The same exposure on two devices: the second rig's cycle finds the first's file
	// already at the name it would claim and adopts it, so one archive file is named by
	// a row on each ledger.
	a := pullFile(t, h, src, original, 10*time.Minute)
	dev2.AddFile(emmc, src, original, h.nowVal.Add(-10*time.Minute))
	w2.runCycle(ctx)
	b, ok, err := h.store.GetFrame(ctx, "rig-02", emmc, src)
	if err != nil || !ok {
		t.Fatalf("the second rig's frame was not filed: ok=%v err=%v", ok, err)
	}
	if b.DestPath != a.DestPath {
		t.Fatalf("setup: both ledgers must name one archive file, got %q and %q", b.DestPath, a.DestPath)
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	if got := hashOnDisk(t, a.DestPath); got != sha256hex(original) {
		t.Fatal("a refused supersede must leave the shared archive copy exactly as it was")
	}
	if !h.hasAlert(model.AlertRotClash) {
		t.Fatalf("the refused frame takes the ordinary token-and-alert path; got %v", h.raisedCategories())
	}
	if after := mustGetFrame(t, h, b.ID); after.ArchiveSHA256 != sha256hex(original) {
		t.Fatalf("the other rig's row must be left anchored to the bytes its file holds, got %q", after.ArchiveSHA256)
	}
	revisedRow := mustGetFrame(t, h, a.ID)
	if revisedRow.DestPath == a.DestPath {
		t.Fatal("the revised frame is filed under a token of its own, not over the shared copy")
	}

	// Both ledgers stay consistent: each row's two copies answer for it, so neither
	// delete pass holds anything.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	w2.deleteAllForTest(ctx, h.cfg, profile2, dev2, "10.0.0.2")
	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("no ledger may be held after a refused supersede; got %v", h.raisedCategories())
	}
}

// TestSupersedeIgnoresACrossRigTombstone is the cross-rig twin of the sharer
// refusal: the row on the other ledger has already drained its device original, so
// it is a terminal record rather than a sharer. Refusing on its account would put a
// revised frame that should file cleanly back on the token-and-alert path for
// good — the refusal's own reason, a permanent critical delete_mismatch on a ledger
// the write never touched, cannot be earned by a row DeletionEligible never readmits.
func TestSupersedeIgnoresACrossRigTombstone(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	w2, dev2 := addSecondRig(t, h)
	profile2 := h.cfg.Rig("rig-02")
	original, revised := revisionPair(revTarget)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"

	a := pullFile(t, h, src, original, 10*time.Minute)
	dev2.AddFile(emmc, src, original, h.nowVal.Add(-10*time.Minute))
	w2.runCycle(ctx)
	b, ok, err := h.store.GetFrame(ctx, "rig-02", emmc, src)
	if err != nil || !ok {
		t.Fatalf("the second rig's frame was not filed: ok=%v err=%v", ok, err)
	}
	if b.DestPath != a.DestPath {
		t.Fatalf("setup: both ledgers must name one archive file, got %q and %q", b.DestPath, a.DestPath)
	}
	// The second rig's delete pass drains its own original, which is the tombstone
	// markDeleted leaves behind: the same row, with no device copy left to answer for.
	b.State, b.DeletedAt = model.StateDeleted, h.nowVal.Unix()
	if err := h.store.UpdateFrame(ctx, b); err != nil {
		t.Fatalf("drain the second rig's original: %v", err)
	}

	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, src, revised, 5*time.Minute)

	if names := dirEntries(t, filepath.Dir(a.DestPath)); len(names) != 1 {
		t.Fatalf("the supersede must proceed, leaving ONE file: %v", names)
	}
	if got := hashOnDisk(t, a.DestPath); got != sha256hex(revised) {
		t.Fatal("the shared archive copy must hold the revised bytes")
	}
	if h.hasAlert(model.AlertRotClash) {
		t.Fatalf("a proven revision is not a content clash; got %v", h.raisedCategories())
	}
	if got := mustGetFrame(t, h, a.ID); got.DestPath != a.DestPath || got.ArchiveSHA256 != sha256hex(revised) {
		t.Fatalf("the revised row must be committed at its own name: dest=%q archive=%q", got.DestPath, got.ArchiveSHA256)
	}
	// The tombstone is out of reach of a rig-scoped commit, as every cross-rig row is.
	// It costs nothing: a terminal record never re-enters the delete pass, so neither
	// ledger can be held over it.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	w2.deleteAllForTest(ctx, h.cfg, profile2, dev2, "10.0.0.2")
	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("no ledger may be held after a supersede past a tombstone; got %v", h.raisedCategories())
	}
	if got := mustGetFrame(t, h, b.ID); got.State != model.StateDeleted {
		t.Fatalf("the tombstone stays a tombstone, got %s", got.State)
	}
}

// countingLFS counts the WHOLE-FILE passes made through the filesystem seam, by
// path. Both VerifyRead and its tee read the file whole with the page cache
// bypassed; a header read does not go through this seam at all, which is exactly the
// distinction the one-pass rule draws.
type countingLFS struct {
	localfs.LocalFS
	mu     sync.Mutex
	passes map[string]int
}

func newCountingLFS(inner localfs.LocalFS) *countingLFS {
	return &countingLFS{LocalFS: inner, passes: map[string]int{}}
}

func (c *countingLFS) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.passes[path]
}

func (c *countingLFS) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.passes {
		n += v
	}
	return n
}

func (c *countingLFS) bump(path string) {
	c.mu.Lock()
	c.passes[path]++
	c.mu.Unlock()
}

func (c *countingLFS) VerifyRead(path string, size int64) (string, error) {
	c.bump(path)
	return c.LocalFS.VerifyRead(path, size)
}

func (c *countingLFS) VerifyReadTee(path string, size, dataOffset int64) (string, string, error) {
	c.bump(path)
	return c.LocalFS.VerifyReadTee(path, size, dataOffset)
}

// TestDataUnitDigestsCostNoSecondWholeFilePass (design frame-lifecycle AC10, the
// consumer half) asserts the read COUNT rather than the digest value: every consumer
// of a data unit tees it off bytes it is already reading. Leg 1 rides the at-rest
// verify the pull already makes, and leg 3 is one cache-bypassing pass over the
// archive copy — the header read that hands it the offset does not go through the
// filesystem seam at all and is not a whole-file pass.
func TestDataUnitDigestsCostNoSecondWholeFilePass(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	counting := newCountingLFS(h.lfs)
	h.w.lfs = counting
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	// Leg 1: a fresh pull reads the staging temp whole exactly once, which is the
	// at-rest verify it already made before any exposure identity existed.
	f := pullFile(t, h, src, original, 10*time.Minute)
	if got := counting.total(); got != 1 {
		t.Fatalf("a fresh pull makes %d whole-file passes, want the at-rest verify alone", got)
	}
	if got := counting.count(f.DestPath); got != 0 {
		t.Fatalf("filing a fresh frame reads the archive copy %d times, want none", got)
	}

	// Leg 3: one pass over the archive copy, no matter that its offset came from a
	// header read of the same file a moment earlier.
	before := counting.count(f.DestPath)
	row := mustGetFrame(t, h, f.ID)
	proven := h.w.provenRevisionOf(context.Background(), fileArgs{
		prior:      &row,
		lf:         listedFrame{share: emmc, path: src, size: int64(len(revised))},
		sha256:     sha256hex(revised),
		dataSHA256: dataDigestOf(revised),
	})
	if !proven {
		t.Fatal("setup: the revision must prove itself, or this counts the wrong path")
	}
	if got := counting.count(f.DestPath) - before; got != 1 {
		t.Fatalf("leg 3 makes %d whole-file passes over the archive copy, want exactly one", got)
	}
}

// cullAndRevise stages the "Absent / either / culled" row of the outcome table: a
// frame filed and then deleted from the NAS by the operator, whose device original
// the ASIAIR afterwards rewrites in place. It returns the row as it stood when the
// cull was confirmed, and the revised bytes now on the device — the caller runs the
// cycle that meets them, so a test can measure what that one cycle costs.
func cullAndRevise(t *testing.T, h *harness) (model.Frame, []byte) {
	t.Helper()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // the operator culls the NAS copy
		t.Fatal(err)
	}
	// Past the library scan's interval, so the next cycle's pass actually runs.
	h.nowVal = h.nowVal.Add(7 * time.Hour)
	h.cycle() // the library scan confirms the cull and marks the row
	culled := mustGetFrame(t, h, f.ID)
	if culled.MissingSeenAt == 0 {
		t.Fatal("setup: the scan must mark the culled frame missing")
	}

	// The device rewrites the finished frame in place to inject the WCS it has since
	// solved: same exposure, one header block longer.
	h.nowVal = h.nowVal.Add(time.Hour)
	h.dev.AddFile(emmc, src, revised, h.nowVal.Add(-10*time.Minute))
	return culled, revised
}

// TestCulledFrameRevisionAdvancesAnchorsAndFilesNothing (design frame-lifecycle
// AC7) is the headline of design frame-lifecycle §11.12: the operator deleted
// this exposure on purpose and the device rewriting its header does not un-delete
// it, so the row's device anchors advance, the cull mark stands, and nothing
// reaches the archive.
func TestCulledFrameRevisionAdvancesAnchorsAndFilesNothing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	culled, revised := cullAndRevise(t, h)

	h.cycle()

	got := mustGetFrame(t, h, culled.ID)
	if got.MissingSeenAt != culled.MissingSeenAt {
		t.Fatalf("the cull mark must stand through the revision: got %d, want %d", got.MissingSeenAt, culled.MissingSeenAt)
	}
	if got.SHA256 != sha256hex(revised) || got.Size != int64(len(revised)) {
		t.Fatalf("the device anchors must advance: sha=%q size=%d", got.SHA256, got.Size)
	}
	if got.Mtime != h.nowVal.Add(-10*time.Minute).Unix() {
		t.Fatalf("mtime = %d, want the revised file's %d", got.Mtime, h.nowVal.Add(-10*time.Minute).Unix())
	}
	if got.DataSHA256 != culled.DataSHA256 {
		t.Fatalf("the exposure identity is what the revision survives: got %q, want %q", got.DataSHA256, culled.DataSHA256)
	}
	// No archive copy changed, so the archive-side expectation must not move: that is
	// what keeps a file the operator restores by hand later from raising a critical
	// delete_mismatch against a perfectly good copy.
	if got.ArchiveSHA256 != culled.ArchiveSHA256 || got.ArchiveSize != culled.ArchiveSize {
		t.Fatalf("the archive anchors must not move: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if names := dirEntries(t, filepath.Dir(culled.DestPath)); len(names) != 0 {
		t.Fatalf("nothing may be filed for a culled frame's revision, found %v", names)
	}
	if h.hasAlert(model.AlertRotClash) {
		t.Fatal("a proven revision is not a content clash; no rot_clash may be raised")
	}
}

// TestCulledFrameRevisionStopsRepullingAndDrains (design frame-lifecycle AC7) is the
// failure the criterion exists to catch. Left alone, the row never matches the
// revised device file again: decide's skip gate is keyed on size, so the frame is
// pulled in full every poll interval forever, pins pendingWork, and is held by the
// pre-delete gate rather than drained. It also asserts the third terminal outcome —
// the frame leaves the live feed's pending set with no file_failed recorded against
// it.
func TestCulledFrameRevisionStopsRepullingAndDrains(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	culled, revised := cullAndRevise(t, h)
	bytesBefore := h.counterValue("asiair_commander_transfer_bytes_total", map[string]string{"rig": "rig-01"})

	h.cycle() // the cycle that meets the revision: one pull, and nothing filed

	pulled := h.counterValue("asiair_commander_transfer_bytes_total", map[string]string{"rig": "rig-01"}) - bytesBefore
	if pulled != float64(len(revised)) {
		t.Fatalf("the revision cycle streamed %v bytes, want the revised file's %d", pulled, len(revised))
	}
	if got := h.counterValue("asiair_commander_transfer_outcomes_total",
		map[string]string{"rig": "rig-01", "outcome": "file_failed"}); got != 0 {
		t.Fatalf("a terminal outcome that files nothing is not a filing failure; file_failed = %v", got)
	}
	if got := h.counterValue("asiair_commander_transfer_outcomes_total",
		map[string]string{"rig": "rig-01", "outcome": "culled_revision"}); got != 1 {
		t.Fatalf("culled_revision outcome = %v, want 1", got)
	}
	if p := h.status().Pending; len(p) != 0 {
		t.Fatalf("the frame must leave the live feed's pending set: %+v", p)
	}

	// Two further cycles: with the anchors advanced the size gate matches again, so
	// the frame is skipped and not one byte more is streamed.
	after := h.counterValue("asiair_commander_transfer_bytes_total", map[string]string{"rig": "rig-01"})
	for i := 0; i < 2; i++ {
		h.cycle()
		if got := h.counterValue("asiair_commander_transfer_bytes_total", map[string]string{"rig": "rig-01"}); got != after {
			t.Fatalf("cycle %d re-pulled the revised frame (%v bytes streamed, want none)", i+1, got-after)
		}
	}
	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatal("a recognised revision must not leave the pre-delete gate holding the frame")
	}
	if !h.deviceHas(culled.SourcePath) {
		t.Fatal("the device original must survive until the cull's settle window elapses")
	}

	// The drain can now authorize the delete, because deviceHash == f.SHA256 once more.
	h.nowVal = h.nowVal.Add(25 * time.Hour)
	h.cycle()
	if h.deviceHas(culled.SourcePath) {
		t.Fatal("once the settle window elapses the culled-original drain deletes the revised device copy")
	}
}

// TestCulledFrameWhoseCopyIsBackIsSuperseded pins the clause that keeps the culled
// arm out of the supersede arm's way: "a culled row whose copy has REAPPEARED is
// not [this cell] — it verifies". The cull mark still stands on the row, so only
// the copy's presence separates the two arms, and the culled arm runs first.
// Without that separation the archive keeps the pre-revision bytes forever while
// the row's device anchors move on, and no supersede ever happens.
func TestCulledFrameWhoseCopyIsBackIsSuperseded(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	culled, revised := cullAndRevise(t, h)
	original, _ := revisionPair(revTarget)
	// The operator puts the copy back by hand, before Commander meets the revision.
	// Its bytes are the ones archive_sha256 names, so leg 3 has something to verify.
	writeLocal(t, culled.DestPath, original)

	h.cycle()

	if got := hashOnDisk(t, culled.DestPath); got != sha256hex(revised) {
		t.Fatal("a culled frame whose copy is back is superseded like any other revision")
	}
	got := mustGetFrame(t, h, culled.ID)
	if got.ArchiveSHA256 != sha256hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must follow the bytes the supersede installed: sha=%q size=%d",
			got.ArchiveSHA256, got.ArchiveSize)
	}
	if c := h.counterValue("asiair_commander_transfer_outcomes_total",
		map[string]string{"rig": "rig-01", "outcome": "culled_revision"}); c != 0 {
		t.Fatalf("the targeted write must not run for a frame whose copy is back; culled_revision = %v", c)
	}
}
