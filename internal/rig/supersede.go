package rig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// errSupersedeRefused reports a supersede this primitive declined to attempt,
// having changed nothing on disk and nothing in the ledger. The caller takes
// the ordinary path for the frame, which is what every unproven case does.
var errSupersedeRefused = errors.New("supersede refused")

// errSupersedeCrashed is the modelled process crash the tests drive through
// crashAfterSupersedeStep. It never escapes a production build: the hook is nil
// there, so nothing returns it.
var errSupersedeCrashed = errors.New("supersede interrupted (modelled crash)")

// supersedeArgs bundles one supersede's inputs: the row whose archive copy is
// being replaced, the file to install under its name, and the anchors the commit
// advances.
//
// Everything that authorizes the swap is the CALLER's: the discriminator that
// recognised a revision, the three-way data-unit agreement and the FITS-validity
// gate. By the time this is called the bytes at srcPath are proven, and what is
// left is the one thing that cannot be done atomically — claiming a name that is
// already taken without ever holding only one copy of the frame.
type supersedeArgs struct {
	// frame is the revised row as last read. Its DestPath names the archive copy
	// being replaced and its ArchiveSHA256 is the content that copy is proven to
	// hold, which the reserved sibling name is derived from.
	frame model.Frame
	// srcPath is the verified file to install at the canonical name — a staging
	// temp. A successful step 2 consumes it; this primitive never removes it, so a
	// failure leaves it for the caller's own cleanup exactly where it was.
	srcPath string
	// The device-side anchors of the revised copy, as just proven off the device:
	// its whole-file hash, its length and its modification time. mtime especially
	// cannot be recovered later from the archive copy, which is why the intent
	// carries it rather than leaving recovery to re-derive it.
	sha256 string
	size   int64
	mtime  int64
	// The archive-side anchors of srcPath: what the file at the canonical name will
	// hash to, and the length every archive-side comparison against it will use.
	archiveSHA256 string
	archiveSize   int64
	// retargetSHA256 is the overlay the caller re-applied to srcPath, empty when it
	// re-applied none. It is not a second spelling of archiveSHA256: the two are the
	// same value on the re-apply branch and only the caller knows whether that branch
	// ran, which is what the overlay refusal below turns on.
	retargetSHA256 string
}

// supersedeArchive replaces the bytes under one archive name with srcPath's,
// through the four-step claim of the design frame-lifecycle §11.9:
//
//  1. rename the existing archive copy aside, to a reserved sibling name derived
//     from its own content hash;
//  2. RenameNoReplace the verified file into the name so freed;
//  3. commit the rows;
//  4. unlink the file set aside.
//
// No atomic replace is available here — RenameNoReplace cannot replace by
// construction, localfs has no replace primitive and SMB replace is not atomic —
// so the sequence is four steps each of which is a no-replace rename or an unlink,
// arranged so that at every instant the bytes exist under some name.
//
// The file renamed in step 1 is the one the ROW describes, f.DestPath, never the
// candidate name a fresh frame's filing path would claim: the two diverge in
// exactly the state a held revision leaves behind — a row's dest_path can be a
// token-suffixed name while candidate 0 is a different frame's file — and keying
// off the candidate would move dest_path and orphan the file the row (and, for a
// retargeted frame, retarget_sha256) actually describes.
//
// The intent is written BEFORE step 1 and cleared after step 4, and a failure to
// persist it aborts before step 1: the crash-safety guarantee is void without it,
// and the conditions that break that write are the conditions that break the
// commit.
func (w *Worker) supersedeArchive(ctx context.Context, a supersedeArgs) error {
	f := a.frame
	switch {
	case f.DestPath == "":
		return fmt.Errorf("%w: the row names no archived file", errSupersedeRefused)
	case f.ArchiveSHA256 == "":
		// Without the hash of what is there, there is no reserved name to derive and
		// no way for recovery to tell the copy set aside from anything else.
		return fmt.Errorf("%w: the row records no archive hash for %s", errSupersedeRefused, f.DestPath)
	case a.archiveSHA256 == "" || a.archiveSize <= 0:
		// An empty archive anchor turns a perfectly good archive copy into a critical
		// delete_mismatch on the next pass, so it is refused rather than written.
		return fmt.Errorf("%w: no archive anchors were proven for %s", errSupersedeRefused, a.srcPath)
	case f.RetargetSHA256 != "" && a.retargetSHA256 == "":
		// The row carries an OBJECT edit and the caller re-applied none, so srcPath holds
		// the device's unedited bytes. Installing them and advancing the overlay with the
		// file would put the device's original OBJECT card on the archive while
		// retarget_sha256 claimed the edit — consequence 2 of the problem
		// design frame-lifecycle §11 states, reintroduced, and this time permanently
		// invisible: archive_sha256, retarget_sha256 and the on-disk hash would all agree,
		// so every later VerifyRead, three-way check and sameArchiveAt passes while the
		// row's target and the file's OBJECT disagree forever.
		//
		// Reachable for a retargeted row filed before data_sha256 existed and not yet
		// reached by the backfill: the re-apply's disjunction holds on neither arm there
		// (no recorded identity, and a revision changes the stream hash), so it declines,
		// while provenRevisionOf establishes leg 2 from the archive copy's own read and
		// proves the revision anyway. Refusing returns the frame to the ordinary
		// token-and-alert path — loud, non-destructive, and self-resolving, since the
		// backfill's record of data_sha256 makes the re-apply's first arm hold on a later
		// cycle. Proving identity from the archive copy inside the re-apply instead would
		// put a whole-file archive read on the ordinary filing path for every pending
		// retargeted row, where the standing rule of design frame-lifecycle §11.4 is
		// that a leg which cannot be established leaves the frame on the existing path
		// rather than reaching for a substitute proof.
		return fmt.Errorf("%w: %s carries an overlay that was not re-applied to %s", errSupersedeRefused, f.DestPath, a.srcPath)
	}
	if refusal, err := w.supersedeRefusal(ctx, f); err != nil {
		return err
	} else if refusal != "" {
		return fmt.Errorf("%w: %s", errSupersedeRefused, refusal)
	}

	aside := supersedeAsidePath(f.DestPath, f.ArchiveSHA256)
	in := store.SupersedeIntent{
		RigID:         w.profileID,
		DestPath:      f.DestPath,
		AsidePath:     aside,
		FrameID:       f.ID,
		SHA256:        a.sha256,
		Size:          a.size,
		Mtime:         a.mtime,
		ArchiveSHA256: a.archiveSHA256,
		ArchiveSize:   a.archiveSize,
		// The overlay is an audit record of what the archive file holds, and this swap
		// rewrites that file, so the record follows it. The store applies it only to a
		// row that already carries an overlay, so a frame that was never retargeted
		// never gains one here.
		//
		// It is the archive anchor rather than a.retargetSHA256 because a dest_path
		// SHARER can carry an overlay the revised row does not, and its record has to
		// follow the file as well; on the revised row the two are the same value, the
		// refusal above being what guarantees an edit was actually re-applied.
		RetargetSHA256: a.archiveSHA256,
		State:          provenCommitState(f.State),
		// The grace clock restarts because the device copy has just been read afresh:
		// the window exists so a second device read is genuinely independent of the
		// first, and a superseded archive copy has just been read once. The
		// rule of design frame-lifecycle §8.4 that a re-pull after a crash must not
		// restart the clock governs a re-pull of IDENTICAL bytes, which this is not.
		PulledAt:  w.now().Unix(),
		CreatedAt: w.now().Unix(),
	}
	if err := w.store.PutSupersedeIntent(ctx, in); err != nil {
		return fmt.Errorf("could not record the supersede intent for %s: %w", f.DestPath, err)
	}

	// Step 1: the proven archive copy goes aside under its reserved name.
	if err := w.lfs.RenameNoReplace(f.DestPath, aside); err != nil {
		w.clearSupersedeIntent(ctx, f.DestPath)
		return fmt.Errorf("could not set the archive copy aside (%s): %w", f.DestPath, err)
	}
	if w.crashAfterSupersedeStep != nil && w.crashAfterSupersedeStep(1, f.DestPath) {
		return errSupersedeCrashed
	}

	// Step 2: the verified file claims the name step 1 freed.
	if err := w.lfs.RenameNoReplace(a.srcPath, f.DestPath); err != nil {
		w.supersedeRollback(ctx, in, false, "the revised file could not claim the name: "+err.Error())
		return fmt.Errorf("could not install the revised copy at %s: %w", f.DestPath, err)
	}
	if w.crashAfterSupersedeStep != nil && w.crashAfterSupersedeStep(2, f.DestPath) {
		return errSupersedeCrashed
	}

	// Step 3: the rows. Uncancellable, as the re-file commit is: a cancel arriving
	// here would fail the write and drive a routine cancellation through the
	// rollback route, undoing a swap whose bytes are already in place.
	commitCtx := context.WithoutCancel(ctx)
	revised, sharers, err := w.store.CommitSupersede(commitCtx, in)
	if err != nil {
		w.supersedeRollback(commitCtx, in, true, "the commit failed: "+err.Error())
		return fmt.Errorf("could not commit the supersede of %s: %w", f.DestPath, err)
	}
	if !revised {
		// PruneDeleted runs hourly and the row can legitimately vanish mid-swap. Unlike
		// a genuine commit failure this does NOT roll back: putting the old bytes back
		// for a row nothing references would re-create something no row names, and any
		// sharer committed above describes the file that is there now.
		w.log.Warn("rig: supersede found no row to commit; the revised file is in place",
			"frame", f.ID, "dest", f.DestPath, "sharers", sharers)
	}
	if w.crashAfterSupersedeStep != nil && w.crashAfterSupersedeStep(3, f.DestPath) {
		return errSupersedeCrashed
	}

	// Step 4: the copy set aside is no longer anyone's archive copy.
	if err := w.lfs.Remove(aside); err != nil && !isNotExist(err) {
		// Every row now names the file at the canonical path, which holds the bytes
		// they were just committed against, so the leftover is an orphan rather than a
		// hazard. The intent stays open: recovery's "both present" arm re-commits and
		// removes it, and meanwhile supersede_in_flight keeps the frame out of the
		// delete pass.
		w.log.Error("rig: could not remove the superseded archive copy; leaving the intent open",
			"frame", f.ID, "aside", aside, "error", err)
		return nil
	}
	if w.crashAfterSupersedeStep != nil && w.crashAfterSupersedeStep(4, f.DestPath) {
		return errSupersedeCrashed
	}

	w.clearSupersedeIntent(commitCtx, f.DestPath)
	w.log.Info("rig: superseded the archive copy with the device's revision",
		"frame", f.ID, "dest", f.DestPath, "sharers", sharers)
	return nil
}

// supersedeRefusal states why this rig's worker must not swap f's archive copy, or
// "" when it may. One condition refuses: a row on ANOTHER rig's ledger sharing the
// same dest_path.
//
// FramesByDestPath is rig-scoped by construction, so such a row is out of reach of
// the commit and would be left anchored to pre-revision bytes in a file that holds
// revised ones — a permanent critical delete_mismatch on a ledger this write never
// touched. CountFramesByDestAnyRig is how the repo detects that, and refusing is
// how it already answers it (refileSharers consumes it exactly this way); this is
// that one policy, not a second one for the same hazard.
//
// A `deleted` row is not a sharer for this question. A tombstone has no live claim
// on the file's content: its device original is gone and its row is a terminal
// record that DeletionEligible never readmits, so it cannot earn the delete_mismatch
// the refusal exists to prevent. Counted as a sharer it refuses every supersede at a
// dest_path whose H12 USB/eMMC pair has already had half of it drained.
//
// Both queries are always made. The unfiltered count could short-circuit on "one row
// in the whole fleet, and it is this one"; the filtered count cannot, because the row
// it counts may be another rig's while this one is the tombstone.
func (w *Worker) supersedeRefusal(ctx context.Context, f model.Frame) (string, error) {
	total, err := w.store.CountUndeletedFramesByDestAnyRig(ctx, f.DestPath)
	if err != nil {
		return "", fmt.Errorf("could not count the rows sharing %s: %w", f.DestPath, err)
	}
	rows, err := w.store.FramesByDestPath(ctx, w.profileID, f.DestPath)
	if err != nil {
		return "", fmt.Errorf("could not read the rows sharing %s: %w", f.DestPath, err)
	}
	if total > len(undeletedRows(rows)) {
		return "the archive file is also referenced by another rig's frame", nil
	}
	return "", nil
}

// undeletedRows drops the tombstones from a set of rows sharing one dest_path, so
// the two consumers of that set — the cross-rig refusal above and the
// restore's candidate list — agree on what counts as a sharer. A `deleted` row has
// no live claim on the archive file's content, its device original being gone and
// its row terminal.
//
// The filtering is done here rather than in FramesByDestPath because refileSharers
// consumes that accessor to answer a different question — which rows NAME the file,
// tombstones included, since a re-file moves them all — and belongs to another
// design.
func undeletedRows(rows []model.Frame) []model.Frame {
	out := rows[:0:0]
	for _, r := range rows {
		if r.State != model.StateDeleted {
			out = append(out, r)
		}
	}
	return out
}

// supersedeRollback undoes a swap that could not be completed, leaving the archive
// copy the rows describe back under its own name.
//
// installed says whether step 2 landed. When it did, the canonical name holds the
// revised bytes and they are removed first: they are a copy of a device original
// that is still on the device (a supersede deletes nothing there), so the archive's
// own content — the bytes every row is anchored to — is the aside copy throughout,
// and the invariant that the bytes exist under some name holds at every instant.
//
// Any failure here leaves the intent OPEN deliberately, exactly as a failed re-file
// rename-back does: supersede_in_flight is what keeps the frame out of
// DeletionEligible however the swap failed, and the state it leaves behind is one
// of the states startup recovery resolves.
func (w *Worker) supersedeRollback(ctx context.Context, in store.SupersedeIntent, installed bool, why string) {
	if installed {
		if err := w.lfs.Remove(in.DestPath); err != nil && !isNotExist(err) {
			w.log.Error("rig: could not roll a supersede back; both copies remain and the intent is left open",
				"frame", in.FrameID, "dest", in.DestPath, "aside", in.AsidePath, "why", why, "error", err)
			return
		}
	}
	if err := w.lfs.RenameNoReplace(in.AsidePath, in.DestPath); err != nil {
		w.log.Error("rig: could not put the superseded archive copy back; the intent is left open",
			"frame", in.FrameID, "dest", in.DestPath, "aside", in.AsidePath, "why", why, "error", err)
		return
	}
	w.clearSupersedeIntent(ctx, in.DestPath)
	w.log.Warn("rig: supersede rolled back; the archive copy is unchanged",
		"frame", in.FrameID, "dest", in.DestPath, "why", why)
}

// clearSupersedeIntent drops an intent best-effort. A failed clear is benign: it
// leaves an intent whose recovery arm is "the sibling is absent → the swap either
// never began or finished; drop it".
func (w *Worker) clearSupersedeIntent(ctx context.Context, destPath string) {
	if err := w.store.ClearSupersedeIntent(ctx, w.profileID, destPath); err != nil {
		w.log.Warn("rig: could not clear a supersede intent; startup recovery will resolve it",
			"dest", destPath, "error", err)
	}
}

// supersedeAsideSuffix marks the reserved sibling a supersede sets its archive copy
// aside under. It ends the name so the file carries no FITS extension: a
// representative-frame scan keys on those extensions, and a copy that exists only
// inside a swap window is nobody's frame.
const supersedeAsideSuffix = ".superseding-"

// supersedeAsidePath is the reserved sibling name step 1 renames the existing
// archive copy to, derived from THAT copy's own content hash. Deriving it from the
// content rather than from a counter is what lets recovery read a swap's progress
// off the name alone: the file under it is the copy whose hash the row recorded,
// and two swaps in one directory can never claim the same name.
//
// It is a sibling because a rename cannot span volumes and the whole sequence rests
// on renames that either happen entirely or not at all.
func supersedeAsidePath(destPath, archiveSHA256 string) string {
	hash := archiveSHA256
	if len(hash) > 16 {
		hash = hash[:16]
	}
	return filepath.Join(filepath.Dir(destPath), filepath.Base(destPath)+supersedeAsideSuffix+hash)
}

// provenCommitState is the state a write of the archive file leaves on the row
// whose device copy it read — the supersede's revised row, and the restore's read
// row, which prove the same thing.
//
// A held row lands verified: the write has proven exactly what the hold was raised
// to doubt — the device copy read, the archive copy replaced and verified — and
// DeletionEligible admits only verified or unclassified-with-a-mark, so a row left
// held would never re-enter the delete pass at all. That is the never-drains end
// state the design rejected "suppress with no other change" for.
//
// Every other state is carried through unchanged. fileEditedArchive's "preserve
// deleted; do not resurrect to verified" is the same instinct on a different case
// and stays as it is: a proof discharges a suspicion without un-deleting a frame,
// and it equally does not promote a quarantined frame out of quarantine — an
// unclassified row is not filed to its real home, which a supersede does not change.
func provenCommitState(prior model.FrameState) model.FrameState {
	if prior == model.StateHeld {
		return model.StateVerified
	}
	return prior
}
