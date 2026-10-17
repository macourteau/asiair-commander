package rig

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
)

// forgetDataDigest turns a row the real filing path wrote into the shape the
// backfill exists for: an archive copy and device anchors, and no recorded
// exposure identity. Nothing in production nulls that column, so there is no store
// method for it — the test opens the same file on its own handle, as the day-zero
// migration test does for the columns it has to strip.
func forgetDataDigest(t *testing.T, h *harness, ids ...int64) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "state.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open state.db: %v", err)
	}
	defer db.Close()
	for _, id := range ids {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE frames SET data_sha256 = NULL WHERE id = ?`, id); err != nil {
			t.Fatalf("forget the exposure identity of frame %d: %v", id, err)
		}
	}
}

// readOrderLFS records the ORDER of the whole-file passes made through the
// filesystem seam. countingLFS counts them by path, which cannot say "before".
type readOrderLFS struct {
	localfs.LocalFS
	mu   sync.Mutex
	seen []string
}

func newReadOrderLFS(inner localfs.LocalFS) *readOrderLFS {
	return &readOrderLFS{LocalFS: inner}
}

func (r *readOrderLFS) record(path string) {
	r.mu.Lock()
	r.seen = append(r.seen, path)
	r.mu.Unlock()
}

func (r *readOrderLFS) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *readOrderLFS) VerifyRead(path string, size int64) (string, error) {
	r.record(path)
	return r.LocalFS.VerifyRead(path, size)
}

func (r *readOrderLFS) VerifyReadTee(path string, size, dataOffset int64) (string, string, error) {
	r.record(path)
	return r.LocalFS.VerifyReadTee(path, size, dataOffset)
}

// cullAndForget stages the row the device-side proof exists for: a frame filed and
// then deleted from the NAS by the operator, whose device original is untouched and
// whose exposure identity was never recorded — the state every culled row is in on
// upgrade. It returns the row as it stands once the cull is confirmed, and the bytes
// the device still holds.
func cullAndForget(t *testing.T, h *harness) (model.Frame, []byte) {
	t.Helper()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, _ := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // the operator culls the NAS copy
		t.Fatal(err)
	}
	// Past the library scan's interval, so the next cycle's pass actually runs.
	h.nowVal = h.nowVal.Add(7 * time.Hour)
	h.cycle()
	culled := mustGetFrame(t, h, f.ID)
	if culled.MissingSeenAt == 0 {
		t.Fatal("setup: the scan must mark the culled frame missing")
	}
	forgetDataDigest(t, h, f.ID)
	return mustGetFrame(t, h, f.ID), original
}

// TestBackfillRefusesARottedArchiveCopy (design frame-lifecycle AC11) is the
// condition the whole job hangs on. A copy that no longer hashes to its own
// archive-side anchor proves nothing about the exposure, and recording its data
// unit as the row's identity would bless the rot permanently — a worse failure than
// a revision the row cannot recognise.
func TestBackfillRefusesARottedArchiveCopy(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, _ := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	forgetDataDigest(t, h, f.ID)

	// Rot in place: the same length, so the row's archive_size still describes the
	// file and the read reaches the hash comparison rather than short-circuiting.
	rotted := append([]byte(nil), original...)
	rotted[len(rotted)-1] ^= 0xff
	if err := os.WriteFile(f.DestPath, rotted, 0o644); err != nil {
		t.Fatal(err)
	}

	BackfillDataDigests(context.Background(), h.store, h.lfs, discardLogger())

	if got := mustGetFrame(t, h, f.ID); got.DataSHA256 != "" {
		t.Fatalf("a rotted copy must leave the row without an identity, got %q", got.DataSHA256)
	}
}

// TestBackfillRecordsAProvenArchiveCopy is the other side of that condition: a copy
// that still hashes to its anchor IS the archived bytes, so its data unit is the
// row's exposure identity.
func TestBackfillRecordsAProvenArchiveCopy(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, _ := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	forgetDataDigest(t, h, f.ID)

	BackfillDataDigests(context.Background(), h.store, h.lfs, discardLogger())

	if got := mustGetFrame(t, h, f.ID); got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("data_sha256 = %q, want the archived copy's data unit", got.DataSHA256)
	}
}

// TestBackfillTakesHeldRowsFirst (design frame-lifecycle AC11) is the ordering half.
// A row already held by a revision is the one whose next listing would otherwise raise
// a critical frame_lost, so it is backfilled ahead of rows that merely lack the
// value — and the archive copy that arm verifies is the proof the cursor needs,
// which is why the two can be served by one pass in that order.
func TestBackfillTakesHeldRowsFirst(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	original, _ := revisionPair(revTarget)

	// The merely-unset row is filed FIRST, so it carries the lower id: an ordering
	// that fell out of the id cursor alone would put it first.
	plain := pullFile(t, h, "Autorun/Light/C-2025 A6/Light_0001.fit", original, 10*time.Minute)
	heldRow := pullFile(t, h, "Autorun/Light/C-2025 A6/Light_0002.fit", original, 10*time.Minute)
	h.w.holdFrame(ctx, *heldRow, model.HoldSourceChanged, "source present but size/mtime changed since filing")
	if got := mustGetFrame(t, h, heldRow.ID); got.State != model.StateHeld {
		t.Fatalf("setup: state = %s, want held", got.State)
	}
	forgetDataDigest(t, h, plain.ID, heldRow.ID)

	order := newReadOrderLFS(h.lfs)
	BackfillDataDigests(ctx, h.store, order, discardLogger())

	seen := order.order()
	if len(seen) != 2 || seen[0] != heldRow.DestPath || seen[1] != plain.DestPath {
		t.Fatalf("the cursor read %v, want the held row %q before the merely-unset %q",
			seen, heldRow.DestPath, plain.DestPath)
	}
	for _, f := range []*model.Frame{plain, heldRow} {
		if got := mustGetFrame(t, h, f.ID); got.DataSHA256 != dataDigestOf(original) {
			t.Fatalf("frame %d: data_sha256 = %q, want the archived copy's data unit", f.ID, got.DataSHA256)
		}
	}
}

// TestCulledDrainSuppliesTheExposureIdentity (design frame-lifecycle AC11) is the
// device-side proof, and the read count is the criterion. A culled row has no
// archive copy by definition, so the only copy that can prove itself is the device
// original the drain is about to delete — and it proves itself by still hashing to
// the row's sha256, which the drain already establishes. The digest rides that
// stream and costs nothing.
func TestCulledDrainSuppliesTheExposureIdentity(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	culled, original := cullAndForget(t, h)

	h.nowVal = h.nowVal.Add(25 * time.Hour) // past the 24h reconcile settle
	if reads := deleteOnly(t, h); reads != 1 {
		t.Fatalf("the drain made %d device content reads, want the one it was already making", reads)
	}

	got := mustGetFrame(t, h, culled.ID)
	if got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("data_sha256 = %q, want the device copy's data unit", got.DataSHA256)
	}
	if h.deviceHas(culled.SourcePath) {
		t.Fatal("the drain must still delete the device original it proved")
	}
}

// TestCulledRowTheSamePassDefersGetsNoExposureIdentity (design frame-lifecycle
// AC11) is the residual the design records, asserted rather than assumed. The
// device-side proof rides handleAbsentNAS's stream and NOTHING else:
// absentDeferReasons runs before that hash, so a culled row the pass does not
// delete is never reached — and no cursor opens a device session on its behalf.
// Each case is a row that stays culled with its original resident, which is
// exactly where consequence 3 stays unfixed.
func TestCulledRowTheSamePassDefersGetsNoExposureIdentity(t *testing.T) {
	t.Run("inside reconcile_settle", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		culled, _ := cullAndForget(t, h)

		// Still inside the 24h settle: the gate defers before any hash.
		if reads := deleteOnly(t, h); reads != 0 {
			t.Fatalf("a deferred culled frame cost %d device content reads, want none", reads)
		}
		if got := mustGetFrame(t, h, culled.ID); got.DataSHA256 != "" {
			t.Fatalf("data_sha256 = %q, want none: no read proved it", got.DataSHA256)
		}
	})

	t.Run("keep_originals rig", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		culled, _ := cullAndForget(t, h)
		h.cfg.Rigs[0].KeepOriginals = true

		// Past the settle, so only keep_originals stands between the row and a drain.
		// The whole cycle runs, and the delete pass it skips is the only place the
		// device-side proof lives.
		h.nowVal = h.nowVal.Add(25 * time.Hour)
		before := h.dev.ContentReads()
		h.cycle()
		if reads := h.dev.ContentReads() - before; reads != 0 {
			t.Fatalf("a keep-originals cycle cost %d device content reads, want none", reads)
		}
		if got := mustGetFrame(t, h, culled.ID); got.DataSHA256 != "" {
			t.Fatalf("data_sha256 = %q, want none: no cursor opens a device session", got.DataSHA256)
		}
		if !h.deviceHas(culled.SourcePath) {
			t.Fatal("setup: a keep-originals rig deletes nothing")
		}
	})
}

// TestBackfilledHeldRowSupersedesInsteadOfAlerting (design frame-lifecycle AC11) is
// the end-to-end claim: a row held by a revision, once backfilled, supersedes on its
// next cycle instead of raising a critical frame_lost. Without the backfill the
// row's leg 2 cannot be established at all, so the frame is filed as the different
// exposure it has come to look like, and revision handling never reaches the rows
// that need it most.
func TestBackfilledHeldRowSupersedesInsteadOfAlerting(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	roomyDevice(h)
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	h.w.holdFrame(ctx, *f, model.HoldSourceChanged, "source present but size/mtime changed since filing")
	forgetDataDigest(t, h, f.ID)
	mark := len(h.notify.Raised()) // the hold's own delete_mismatch stops here

	BackfillDataDigests(ctx, h.store, h.lfs, discardLogger())
	if got := mustGetFrame(t, h, f.ID); got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the held row must be backfilled first of all: data_sha256 = %q", got.DataSHA256)
	}

	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	if cats := raisedSince(h, mark); len(cats) != 0 {
		t.Fatalf("a backfilled row supersedes rather than alerting; got %v", cats)
	}
	got := mustGetFrame(t, h, f.ID)
	if got.State != model.StateVerified {
		t.Fatalf("state = %s, want verified: a row left held drains never", got.State)
	}
	if got.SHA256 != sha256hex(revised) || got.ArchiveSHA256 != sha256hex(revised) {
		t.Fatalf("both sides must describe the revised bytes: sha=%q archive=%q", got.SHA256, got.ArchiveSHA256)
	}
	if got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the exposure identity is what the revision survives, got %q", got.DataSHA256)
	}
	if names := dirEntries(t, filepath.Dir(got.DestPath)); len(names) != 1 {
		t.Fatalf("a revision must leave ONE file, got %v", names)
	}
}

// TestUnreachedRowIsBackfilledOnDemand is the rule that keeps anything from
// depending on the cursor having finished: a row needed before the cursor reaches it
// has its value computed on demand, under the same proof — leg 3's own read of a
// copy that still hashes to archive_sha256 — so the revision is recognised on the
// spot and the row keeps the identity that read proved.
func TestUnreachedRowIsBackfilledOnDemand(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	forgetDataDigest(t, h, f.ID)

	// No cursor runs. The pull path meets the revision on its own.
	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	got := mustGetFrame(t, h, f.ID)
	if got.SHA256 != sha256hex(revised) {
		t.Fatalf("the revision must be recognised without the cursor: sha256 = %q", got.SHA256)
	}
	if got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the on-demand proof must be recorded, got %q", got.DataSHA256)
	}
	if h.hasAlert(model.AlertRotClash) {
		t.Fatal("a proven revision is not a content clash; no rot_clash may be raised")
	}
	if names := dirEntries(t, filepath.Dir(got.DestPath)); len(names) != 1 {
		t.Fatalf("a revision must leave ONE file, got %v", names)
	}
}

// TestPendingRowRefusesAZeroLengthDataUnit holds the on-demand arm to the
// precondition of design frame-lifecycle §11.4. A row with no recorded identity
// skips the recorded-value leg entirely, so the precondition is carried there by
// revisionCandidate alone — and without it two unrelated header-only frames agree
// deterministically on the one digest every zero-length data unit shares, and the
// archive copy of the first is overwritten by the second.
// TestZeroLengthDataUnitIsNeverAnIdentity asserts this of the RECORDED path; this
// is the second class of rows that path no longer speaks for.
func TestPendingRowRefusesAZeroLengthDataUnit(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	first := fitstest.Build(fitstest.LightCards(revW, revH, revTarget), nil)
	second := fitstest.Build(append(fitstest.LightCards(revW, revH, revTarget),
		"COMMENT   a second, unrelated header-only frame"), nil)

	f := pullFile(t, h, src, first, 10*time.Minute)
	forgetDataDigest(t, h, f.ID)

	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, second, 5*time.Minute)

	if got := hashOnDisk(t, f.DestPath); got != sha256hex(first) {
		t.Fatal("two header-only frames are not the same exposure; nothing may be overwritten")
	}
	if !h.hasAlert(model.AlertRotClash) {
		t.Fatalf("the second frame takes the ordinary token-and-alert path; got %v", h.raisedCategories())
	}
}

// TestPendingRowIsNotRevisedByIdenticalBytes is leg 1 on that same arm, and it is
// the conjunct's own test: the precondition holds throughout, so only leg 1 stands
// between this re-pull and a supersede. A spurious FAT mtime shift sends
// byte-identical content back through the differ branch, which the idempotent keep
// owns. Proving it a "revision" of itself would not merely swap a file for its own
// copy — it would restart the deletion grace clock the row's pulled_at anchors and
// clear a terminal Tier-1 verdict that every ordinary re-file preserves, so those
// two are asserted beside the bytes.
//
// The read count is the other half: leg 1 is spent BEFORE the archive read
// precisely so a row the cursor has not reached pays nothing extra on an ordinary
// re-pull. The passes over the archive copy are the idempotent keep's own content
// compare and the differ branch's own proof of it; a supersede adds leg 3 and the
// swap's verifies on top of them.
func TestPendingRowIsNotRevisedByIdenticalBytes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, _ := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	// Tier 1 ran and found no match. That verdict is terminal and outlives every
	// ordinary re-file; only a genuine content change clears it.
	if err := h.store.SetFrameAstrometry(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusFailed}); err != nil {
		t.Fatalf("seed the Tier-1 verdict: %v", err)
	}
	forgetDataDigest(t, h, f.ID)
	before := mustGetFrame(t, h, f.ID)

	counting := newCountingLFS(h.lfs)
	h.w.lfs = counting

	// The same bytes, listed with an mtime far outside the ±2s tolerance: a card
	// reader or a FAT shift, which the differ branch resolves and then re-pulls. The
	// clock does not advance, so the deletion grace has not elapsed and the delete
	// pass reads nothing — the two passes counted below are the pull path's own.
	h.dev.AddFile(emmc, src, original, h.nowVal.Add(-48*time.Hour))
	h.cycle()

	got := mustGetFrame(t, h, f.ID)
	if got.SHA256 != sha256hex(original) || got.DestPath != before.DestPath {
		t.Fatalf("identical device bytes are the same file: sha=%q dest=%q", got.SHA256, got.DestPath)
	}
	if names := dirEntries(t, filepath.Dir(before.DestPath)); len(names) != 1 {
		t.Fatalf("an idempotent keep files nothing beside the copy it adopts; got %v", names)
	}
	if hashOnDisk(t, before.DestPath) != sha256hex(original) {
		t.Fatal("no swap may happen: the archive copy already holds these bytes")
	}
	if got.PulledAt != before.PulledAt {
		t.Fatalf("pulled_at = %d, want the original pull's %d: a no-op re-pull must not restart the grace clock",
			got.PulledAt, before.PulledAt)
	}
	if got.Astrometry == nil || got.Astrometry.Status != model.AstroStatusFailed {
		t.Fatalf("a terminal Tier-1 verdict outlives an ordinary re-file, got %+v", got.Astrometry)
	}
	// Two: the differ branch proving the old row's copy is intact before anything
	// overwrites it, and the idempotent keep's content compare. A supersede would
	// add leg 3 and the swap's verifies on top of them.
	if n := counting.count(before.DestPath); n != 2 {
		t.Fatalf("the re-pull made %d whole-file archive passes, want the differ verify and the idempotent keep's compare", n)
	}
}

// TestOnDemandArmCostsOneWholeFilePass is design frame-lifecycle AC10's
// pending-row leg. TestDataUnitDigestsCostNoSecondWholeFilePass pins the read
// count of the RECORDED path; the on-demand arm is placed where it is — after leg
// 3's verify, not before it — exactly so it folds into that one read, and nothing
// else would fail if a later edit moved it earlier and bought a second ~116 MB
// pass.
func TestOnDemandArmCostsOneWholeFilePass(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	forgetDataDigest(t, h, f.ID)
	row := mustGetFrame(t, h, f.ID)

	counting := newCountingLFS(h.lfs)
	h.w.lfs = counting
	proven := h.w.provenRevisionOf(context.Background(), fileArgs{
		prior:      &row,
		lf:         listedFrame{share: emmc, path: src, size: int64(len(revised))},
		sha256:     sha256hex(revised),
		dataSHA256: dataDigestOf(revised),
	})
	if !proven {
		t.Fatal("setup: the pending row must prove itself on demand, or this counts the wrong path")
	}
	if n := counting.count(row.DestPath); n != 1 {
		t.Fatalf("the on-demand arm made %d whole-file archive passes, want exactly one", n)
	}
	if got := mustGetFrame(t, h, f.ID); got.DataSHA256 != dataDigestOf(original) {
		t.Fatalf("the read that proved the copy must also record it, got %q", got.DataSHA256)
	}
}

// TestOnDemandProofIsRefusedByARottedArchiveCopy holds the on-demand arm to the same
// condition as the cursor: a copy that has drifted proves nothing, so the row keeps
// no identity and the frame takes the ordinary token-and-alert path rather than
// overwriting an archive copy on an unproven premise.
func TestOnDemandProofIsRefusedByARottedArchiveCopy(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	forgetDataDigest(t, h, f.ID)
	rotted := append([]byte(nil), original...)
	rotted[len(rotted)-1] ^= 0xff
	if err := os.WriteFile(f.DestPath, rotted, 0o644); err != nil {
		t.Fatal(err)
	}

	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	if hashOnDisk(t, f.DestPath) != sha256hex(rotted) {
		t.Fatal("an unproven revision must not overwrite the archive copy")
	}
	if !h.hasAlert(model.AlertRotClash) {
		t.Fatalf("an unproven clash still earns rot_clash; got %v", h.raisedCategories())
	}
	if names := dirEntries(t, filepath.Dir(f.DestPath)); len(names) != 2 {
		t.Fatalf("an unproven clash takes the ordinary token-and-alert path, leaving two files; got %v", names)
	}
}
