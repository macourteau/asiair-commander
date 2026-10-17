package rig

import (
	"context"
	"errors"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// resolveRevision resolves one filing that turns out to be a device-side revision
// of a frame already filed: the ASIAIR rewrites a finished light in place to inject
// the WCS it has since solved, so the bytes at a path Commander already pulled
// differ while the exposure does not (hardware finding H13,
// design frame-lifecycle §11).
//
// done says the filing is this path's, and out is then the terminal outcome to
// report for it. done=false is the ordinary answer and leaves the filing to the
// caller, which goes on to claim a name, disambiguating a content clash and
// raising rot_clash as it does for any frame.
//
// It implements the outcome table of design frame-lifecycle §11.5 keyed on
// what the row owes rather than on lifecycle state: a culled row, which has no
// archive copy to protect, takes the targeted write below; an archive copy that
// verifies and answers for the same exposure is superseded; and every other shape —
// a copy that has drifted, an unproven identity, a read that failed — falls back to
// the ordinary path. The fallback is never a weaker check.
func (w *Worker) resolveRevision(ctx context.Context, a fileArgs) (out fileOutcome, done bool) {
	prior := a.prior
	if prior == nil || prior.DestPath == "" {
		return fileDeferred, false
	}
	// The culled row is decided before the supersede arm because its whole proof is
	// the discriminator: there is no archive copy to establish leg 3 against, so
	// provenRevisionOf below would route it to the ordinary path — and the ordinary
	// path files the exposure the operator deleted straight back onto the NAS.
	if culled, isCulled := w.culledRevision(ctx, a); isCulled {
		return culled, true
	}
	if !w.provenRevisionOf(ctx, a) {
		return fileDeferred, false
	}
	if err := w.supersedeArchive(ctx, supersedeArgs{
		frame:   *prior,
		srcPath: a.srcPath,
		// The device side is the file as the device now holds it — the streamed bytes,
		// their listed length and their listed mtime — never the edited copy that lands
		// on the archive.
		sha256: a.sha256,
		size:   a.lf.size,
		mtime:  a.lf.mtime.Unix(),
		// The archive side describes a.srcPath: the edited bytes where an overlay was
		// re-applied over the revision, the incoming bytes otherwise.
		archiveSHA256: a.archiveSHA256,
		archiveSize:   a.archiveSize,
		// Empty exactly when the re-apply declined, which is the one thing the primitive
		// cannot see for itself and the one thing that decides whether a retargeted row
		// may be superseded at all.
		retargetSHA256: a.retargetSHA256,
	}); err != nil {
		if errors.Is(err, errSupersedeRefused) {
			// Nothing was touched — a cross-rig sharer, most of all — so the frame takes
			// the ordinary token-and-alert path and both ledgers stay consistent.
			w.log.Warn("rig: not superseding the archive copy; filing as a clash instead",
				"path", a.lf.path, "dest", prior.DestPath, "reason", err)
			return fileDeferred, false
		}
		// A swap that began and could not finish: its own recovery owns what is on
		// disk, so the frame is left for the next cycle rather than filed beside it.
		w.log.Error("rig: superseding the archive copy failed; leaving the frame for the next cycle",
			"path", a.lf.path, "dest", prior.DestPath, "error", err)
		return fileDeferred, true
	}
	w.recordSuperseded(ctx, a)
	return fileFiled, true
}

// provenRevisionOf answers the one question that authorizes replacing an archived
// exposure with the device's own revision of it. It is three legs on the data unit,
// all established in the same cycle, plus a structural gate:
//
//  1. the incoming file's data unit, teed off the staging temp's at-rest verify;
//  2. the data unit the row records for the exposure;
//  3. a cache-bypassing re-read of the data unit the archive copy actually holds.
//
// The structural gate is that the incoming file parses as FITS: a valid FITS file is
// never replaced by one that does not parse. It needs no separate test, because the
// digest seam reports an empty data digest for a header that does not parse and
// IsUsableDataDigest refuses that value — the same predicate refusing the digest
// every zero-length data unit shares, which would otherwise make two unrelated
// header-only frames agree deterministically on all three legs. There is no converse
// rule: Commander does not repair a corrupt archive copy from a device copy, and an
// archive copy that does not parse fails leg 3 for that reason.
//
// Nothing here turns on the file's SIZE. The +2880-byte growth is one camera's
// behaviour — whether the injected WCS overflows a further header block depends on
// how many cards the base header already carries — so a revision may leave the size
// untouched on another camera and must still be recognised.
//
// A row filed before data_sha256 existed cannot answer leg 2 from its record at
// all. Refusing there would leave every such frame on the ordinary path until the
// backfill cursor reached it, so leg 2 is established for those rows from leg 3's
// own read — the archive copy that has just proven itself the archived bytes,
// which is the same proof the cursor uses and the one the on-demand arm of
// design frame-lifecycle §11.8 names. Leg 1 and
// the precondition are still spent first, so an ordinary re-pull of a row the
// cursor has not reached never pays that read.
//
// That arm makes this more than the predicate its name promises: on a row with no
// recorded identity it WRITES the value it has just proven, before answering. The
// alternative — handing the digest back for resolveRevision to write — is more
// plumbing than the one call is worth, so the write stays here and is stated here
// rather than left for a reader to discover.
func (w *Worker) provenRevisionOf(ctx context.Context, a fileArgs) bool {
	if a.prior == nil {
		return false
	}
	row := *a.prior
	pendingDigest := row.DataSHA256 == ""
	if pendingDigest {
		if !revisionCandidate(a.sha256, a.dataSHA256, row) {
			return false
		}
	} else if !sameExposureRevised(a, row) {
		return false
	}
	if row.ArchiveSHA256 == "" {
		// No archive-side expectation to establish leg 3 against, so the copy on disk
		// cannot be proven to be the one this row describes. Unreachable for a filed
		// row — the day-zero migration seeds the column and every filing site writes it
		// — and refused rather than bridged, for the reason nothing bridges a missing
		// one anywhere else (design frame-lifecycle §11.1).
		w.log.Warn("rig: the row records no archive-side hash; not superseding its copy",
			"dest", row.DestPath)
		return false
	}
	// Leg 3. The offset comes from a header read and the digests from the one
	// cache-bypassing pass VerifyRead already makes, so the archive copy is read whole
	// exactly once; a buffered read of a file Commander itself wrote is served from
	// resident pages and would prove nothing about what the disk holds.
	off, oerr := header.DataOffset(row.DestPath)
	if oerr != nil {
		if isNotExist(oerr) {
			// There is no archive copy to replace, which is where a frame the operator
			// culled on the NAS lands — an expected state the design routes to the
			// ordinary path deliberately, so it is not a warning about an unreadable
			// header.
			w.log.Info("rig: no archive copy to supersede; filing as a clash instead",
				"dest", row.DestPath)
			return false
		}
		w.log.Warn("rig: could not read the archived copy's header; not superseding it",
			"dest", row.DestPath, "error", oerr)
		return false
	}
	onDisk, archiveData, verr := w.lfs.VerifyReadTee(row.DestPath, row.ArchiveSize, off)
	if verr != nil {
		// Truncated, unreadable, or vanished between the two reads: no archive copy is
		// proven, so there is nothing this path may replace and the frame takes the
		// ordinary path like every other unproven shape.
		w.log.Warn("rig: no verified archive copy to supersede; filing as a clash instead",
			"dest", row.DestPath, "error", verr)
		return false
	}
	if onDisk != archiveHash(row) {
		// A copy that has genuinely rotted: leg 3 cannot be established against drifted
		// bytes, so identity goes unproven and the frame earns the rot_clash alert the
		// disambiguating claim raises.
		w.log.Warn("rig: the archived copy has drifted from its recorded hash; not superseding it",
			"dest", row.DestPath, "recorded", archiveHash(row), "onDisk", onDisk)
		return false
	}
	// The copy on disk is now proven to be the archived bytes, which is the whole of
	// the backfill's condition, so a row that recorded no exposure identity acquires
	// one here whether or not this particular filing turns out to be a revision. Its
	// leg 2 is then that same value, which is what the comparison immediately below
	// tests it against — the precondition having been spent above.
	if pendingDigest {
		w.recordProvenDataDigest(ctx, row, archiveData)
	}
	if archiveData != a.dataSHA256 {
		w.log.Warn("rig: the archived copy holds a different exposure; not superseding it",
			"dest", row.DestPath, "path", a.lf.path)
		return false
	}
	return true
}

// sameExposureRevised is the discriminator of design frame-lifecycle §11.4: legs 1
// and 2, and the precondition that stands in front of every identity test in the
// design. The bytes differ from what the row records for the device copy, and the
// data unit — the part the device's header edit leaves alone — is still the
// exposure the row records. An overlay re-applied above rewrites one header card of
// the incoming file and so leaves leg 1 describing the bytes that are about to land
// either way.
//
// IsUsableDataDigest is what refuses both a header that does not parse, for which
// the digest seam reports an empty data digest, and a file that parses but carries
// a zero-length data unit: every such file's data unit hashes to the same value, so
// without it two genuinely different frames would agree deterministically.
func sameExposureRevised(a fileArgs, row model.Frame) bool {
	return revisedExposure(a.sha256, a.dataSHA256, row)
}

// revisedExposure is that same discriminator over the two digests of a file in
// hand, rather than over a filing's arguments. The restore path reads its device
// copy through a stream of its own and has no fileArgs to offer, and the design
// applies the discriminator "identically" there (the restoreNAS row of design
// frame-lifecycle §11.6) — so both consumers ask one function rather than each
// spelling the three legs out and drifting on the precondition.
func revisedExposure(wholeHash, dataHash string, row model.Frame) bool {
	return wholeHash != row.SHA256 &&
		header.IsUsableDataDigest(dataHash) &&
		dataHash == row.DataSHA256
}

// revisionCandidate is the part of that discriminator a row filed before
// data_sha256 existed can still answer: leg 1 — the bytes differing from what the
// row records for the device copy — and the precondition that stands in front of
// every identity test in the design. Leg 2 is established for such a row from the
// archive copy that proves itself in provenRevisionOf, so it is spent where that
// read happens rather than here.
//
// It sits beside revisedExposure so the two cannot drift on the precondition, and
// it is never a weaker test: every caller that skips leg 2 here establishes it
// against a copy proven to be the archived bytes before concluding anything.
func revisionCandidate(wholeHash, dataHash string, row model.Frame) bool {
	return wholeHash != row.SHA256 && header.IsUsableDataDigest(dataHash)
}

// culledRevision resolves the "Absent / either / culled" row of the outcome table:
// the operator deleted this frame's NAS copy on purpose and the device then revised
// its header. There is no archive copy to protect, so the discriminator is the
// whole proof, and what follows is a targeted write rather than a filing — nothing
// is filed to the archive, because the operator deleted that exposure and the
// device rewriting its header does not un-delete it.
//
// Both halves of the cell are load-bearing. Filing the frame by the ordinary path
// would bring back a frame the operator culled, UpsertFrame clearing all three
// reconciliation marks on the way. Leaving the row alone is no better: decide's
// skip gate is keyed on size, so an untouched row never matches the revised device
// file again — the frame is pulled in full every poll interval forever, pins
// pendingWork so the rig never reads safe to power off, and the pre-delete gate
// holds it with a critical alert rather than draining it. With the row's anchors
// advanced the size gate matches again on the next cycle and the frame is skipped,
// and the culled-original drain can authorize deleting it because deviceHash ==
// f.SHA256 once more.
//
// done=false means this is not that cell and the caller goes on to the supersede
// arm: a row carrying no cull mark, a frame that is not a proven revision of the
// exposure the row records (the residual the design records for a pre-existing
// culled row whose data_sha256 the backfill can never reach), or a culled row whose
// copy has REAPPEARED — that one verifies, so it is the supersede arm's and its
// marks clear as they do for any supersede.
func (w *Worker) culledRevision(ctx context.Context, a fileArgs) (fileOutcome, bool) {
	row := *a.prior
	if row.MissingSeenAt == 0 || !sameExposureRevised(a, row) {
		return fileDeferred, false
	}
	if _, _, err := w.lfs.Stat(row.DestPath); err == nil {
		// The copy is back. It is no longer this cell: leg 3 has something to read, so
		// the supersede arm proves it against the bytes on disk.
		return fileDeferred, false
	} else if !isNotExist(err) {
		// Nothing is concluded from an error: with the archive side unreadable this
		// frame's cell is unknown, and falling through would file the culled exposure
		// back onto a NAS that may well still hold it. Defer and re-pull next cycle.
		w.log.Warn("rig: cannot tell whether a culled frame's copy is back; deferring its revision",
			"dest", row.DestPath, "error", err)
		return fileDeferred, true
	}
	ok, err := w.store.CommitCulledRevision(ctx, store.CulledRevision{
		RigID:   w.profileID,
		FrameID: row.ID,
		// The device side is the file as the device now holds it: the streamed bytes
		// and their listed length and mtime. There is no edited copy here — nothing is
		// filed — so a re-applied overlay above changes none of these.
		SHA256:     a.sha256,
		Size:       a.lf.size,
		Mtime:      a.lf.mtime.Unix(),
		DataSHA256: a.dataSHA256,
	})
	if err != nil {
		w.log.Error("rig: could not advance a culled frame's device anchors; leaving it for the next cycle",
			"frame", row.ID, "path", a.lf.path, "error", err)
		return fileDeferred, true
	}
	if !ok {
		// The cull mark went out from under the write — the copy came back and the
		// delete pass cleared it, or the row was pruned. The ordinary path owns a row
		// that is no longer culled.
		w.log.Info("rig: a culled frame stopped being culled while its revision was pulled; filing it",
			"frame", row.ID, "path", a.lf.path)
		return fileDeferred, false
	}
	w.log.Info("rig: recognised a device-side revision of a culled frame; advanced its device anchors and filed nothing",
		"frame", row.ID, "path", a.lf.path, "dest", row.DestPath)
	// A terminal outcome that files nothing: the key leaves the live feed's pending
	// set exactly as a filed frame's does, and no frame.done is emitted because no
	// frame was filed and the row's own lifecycle has not moved.
	w.terminalThisCycle[settleKey(a.lf)] = true
	return fileNothingFiled, true
}

// recordSuperseded closes out a completed swap: the astrometry the revised file
// carries, and the bookkeeping recordFiled does for a frame that reached a terminal
// filed outcome. The ROW is already written — CommitSupersede wrote the revised row
// and every dest_path sharer inside the claim — so nothing here upserts, which is
// what keeps a blanket re-file from clobbering a two-sided commit.
func (w *Worker) recordSuperseded(ctx context.Context, a fileArgs) {
	id := a.prior.ID
	// A supersede is a genuine content change, and the revised file carries the WCS
	// the device embedded, so the Tier-0 extraction is re-recorded from it.
	// UpsertFrame cannot deliver that: its wcs_status write is a sticky CASE, so a
	// Tier-1 solving/solved/failed/unsolvable verdict survives a re-file and would
	// shadow the header the device has since written. ClearFrameAstrometry is the store
	// method whose own comment names this the sanctioned reset for a genuine content
	// change; every other re-file path's stickiness is left exactly as it is
	// (design frame-lifecycle §11.10).
	//
	// The write below is unconditional, so on this path the clear is not what makes the
	// header verdict win. It is made for the two cases that one does not cover: a commit
	// routed back through UpsertFrame by a later change, and a re-extraction whose own
	// write fails — where a cleared row is picked up and re-solved, and an uncleared one
	// keeps a verdict describing bytes that are gone.
	if err := w.store.ClearFrameAstrometry(ctx, id); err != nil {
		w.log.Error("rig: could not reset the astrometry of a superseded frame", "frame", id, "error", err)
	} else if a.astro != nil {
		if err := w.store.SetFrameAstrometry(ctx, id, a.astro); err != nil {
			w.log.Error("rig: could not record the astrometry of a superseded frame", "frame", id, "error", err)
		}
	}

	w.metrics.RecordFrameFiled(w.profileID, metrics.FrameClassOf(string(a.cls.Type)), metrics.DispositionFresh)
	// The frame reached a terminal outcome this cycle, so its key leaves the live
	// feed's Pending set exactly as a fresh filing's does (design features §4.3).
	w.terminalThisCycle[settleKey(a.lf)] = true

	f, ok, err := w.store.GetFrameByID(ctx, id)
	if err != nil || !ok {
		w.log.Warn("rig: superseded frame could not be re-read for the live feed", "frame", id, "error", err)
		return
	}
	w.hub.Emit(hub.Event{Name: "frame.done", Data: *f})
	// The revised light may still carry no embedded WCS — the device revises a header
	// for other reasons than a solve — so it is a Tier-1 candidate like any other
	// freshly-filed frame.
	w.maybeEnqueueSolve(f)
	if w.previewer != nil {
		// The thumbnail cache is keyed on the content hash, so the warm has to name the
		// revised bytes rather than the ones the preview was built from.
		w.previewer.Warm(f.SHA256, f.DestPath)
	}
	// The same auto-link every filing site makes: a worker that restarts and meets a
	// supersede before any fresh filing would otherwise leave its destination root
	// unlinked, which fails silently (design architecture §8).
	w.ensureLibraryLinked(ctx, f.DestPath)
}
