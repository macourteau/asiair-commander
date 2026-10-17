package rig

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// restoreRetryInterval bounds how often restorePhase re-attempts a given frame
// whose restore keeps failing (e.g. a read-only or full destination): restoreNAS
// streams the whole device copy before it can fail on the destination, so an
// unbounded retry would re-stream every poll. Tracked in an in-memory per-worker
// map — not the pull-retry columns, which would collide.
const restoreRetryInterval = 1 * time.Hour

// maxRestoreInconclusive bounds how many consecutive passes restoreOne's
// inconclusive arm may decline to conclude anything before it reports the restore
// as failed. The arm itself is right — a present-but-mismatched archive copy or a
// transient read error says nothing, and nothing is concluded from an error — but
// one shape makes the deferral permanent: a restore whose commit fails after the
// bytes are renamed into place leaves the archive copy at the revised length while
// the row still records the pre-revision ArchiveSize, so every later
// VerifyRead(path, size) hashes a prefix of a longer file and lands here again. A
// restore's success path writes frame columns, so "file landed, row did not" is a
// state a restore can produce, and the frame's device original stays pinned by the
// standing request for as long as the arm stays quiet.
//
// Three, against the delete pass's twenty, because the two bounds count different
// waits. That one has to outlast the pull path's min_file_age before it may conclude
// anything; this one waits on nothing — restoreRetryInterval already spaces the
// passes an hour apart, so three is a quarter-day of quiet before the operator is
// told, on a request they made themselves.
//
// The count is per worker and in memory, beside lastRestoreAttempt, which backs off
// the same frame on the same key and resets with it: a restart retries immediately
// and re-counts from zero. Persisting it would be a second frames column, and the
// column each write path answers for is the matrix of design frame-lifecycle
// §11.2 to decide.
const maxRestoreInconclusive = 3

// restorePhase drains the operator's pending restore requests, re-pulling each
// frame's NAS copy from a still-resident device original. It runs BEFORE
// deletePhase so a queued restore's original is never destroyed in the same cycle,
// and (unlike deletePhase) is not gated on KeepOriginals: a keep-originals rig
// still has a library to restore into. It opens its own device session and
// confirms identity, since the pull session has closed.
func (w *Worker) restorePhase(ctx context.Context, cfg *model.Config, cy cycle, dev device.Device) {
	profile, ep := cy.profile, cy.ep
	awaiting, err := w.store.FramesAwaitingRestore(ctx, w.profileID)
	if err != nil {
		w.log.Warn("rig: frames-awaiting-restore query failed", "error", err)
		return
	}
	if len(awaiting) == 0 {
		return
	}

	sess, err := dev.Open(ctx)
	if err != nil {
		w.log.Warn("rig: restore-pass session open failed; deferring", "error", err)
		return
	}
	ms := newMountedSession(sess)
	defer ms.Close()

	confirmed, foreign := w.confirmDeviceIdentity(ctx, profile, ep)
	if foreign || !confirmed {
		w.log.Warn("rig: restore pass skipped; device identity not confirmed", "foreign", foreign)
		return
	}

	disabled := profile.DisabledShares()
	for _, f := range awaiting {
		w.drainCommands(ctx)
		if ctx.Err() != nil || w.workSuspended() || w.destUnavailable {
			return
		}
		if disabled[f.SourceShare] {
			// A disabled share is fully ignored: its frames are not restored and its
			// (possibly unreachable) share is never mounted.
			continue
		}
		// Back off a frame whose restore keeps failing.
		if last, ok := w.lastRestoreAttempt[f.ID]; ok && w.now().Sub(last) < restoreRetryInterval {
			continue
		}
		w.restoreOne(ctx, cy.site, ms, f)
	}
}

// restoreOne re-pulls one frame's NAS copy, deciding by VERIFICATION not mere
// existence: a copy that is back and good just clears the request; a truncated one
// is replaced; an absent one is re-pulled. On success it clears the missing mark
// and the request.
func (w *Worker) restoreOne(ctx context.Context, site netsite.Site, ms *mountedSession, f model.Frame) {
	release, err := w.adm.Acquire(ctx, w.requester(site), 0)
	if err != nil {
		if errors.Is(err, ErrDestinationUnavailable) {
			w.destUnavailable = true
			return
		}
		w.log.Warn("rig: restore admission aborted", "path", f.SourcePath, "error", err)
		return
	}
	defer release()

	share, err := ms.share(f.SourceShare)
	if err != nil {
		w.log.Warn("rig: restore mount failed", "share", f.SourceShare, "error", err)
		return
	}
	// A swapped/absent removable medium mounts with an empty root; without this a
	// restore reads the wrong content and holds the frame with a critical mismatch.
	if !w.mediaConfirmedForDelete(share) {
		return
	}

	w.lastRestoreAttempt[f.ID] = w.now()

	hash, verr := w.lfs.VerifyRead(f.DestPath, f.ArchiveSize)
	switch {
	case verr == nil && hash == archiveHash(f):
		// Back and good already: nothing to re-pull. Archive side, so archiveHash — a
		// healthy retargeted copy is recognized rather than pinned by a standing request
		// (design routing-and-filing §9.4).
		w.clearRestored(ctx, f)
		return
	case verr != nil && errors.Is(verr, localfs.ErrShortFile):
		if rerr := w.restoreNAS(ctx, ms, f, true); rerr != nil {
			// A held frame (device content changed) already alerted via holdFrame and
			// must keep its mark/request; only a genuine failure raises restoreFailed.
			if !errors.Is(rerr, errRestoreHeld) {
				w.restoreFailed(ctx, f, rerr)
			}
			return
		}
	case verr != nil && isNotExist(verr):
		if rerr := w.restoreNAS(ctx, ms, f, false); rerr != nil {
			if !errors.Is(rerr, errRestoreHeld) {
				w.restoreFailed(ctx, f, rerr)
			}
			return
		}
	default:
		// Present but a non-matching hash, or a transient read error: nothing is
		// concluded from either, so the pass defers — but not forever, because a
		// half-committed restore lands here on every pass and would otherwise re-verify
		// in silence while the device original stays pinned by the standing request.
		w.restoreInconclusive[f.ID]++
		n := w.restoreInconclusive[f.ID]
		if n >= maxRestoreInconclusive {
			w.restoreFailed(ctx, f, inconclusiveRestore{dest: f.DestPath, err: verr})
			return
		}
		w.log.Warn("rig: restore verify inconclusive; deferring",
			"dest", f.DestPath, "deferred", n, "bound", maxRestoreInconclusive, "error", verr)
		return
	}
	w.clearRestored(ctx, f)
	w.log.Info("rig: restored missing frame on operator request", "path", f.SourcePath, "dest", f.DestPath)
}

// clearRestored resolves a successfully-restored frame: it clears the missing mark
// and the operator's request, and forgets the frame's two per-episode counters — the
// retry backoff, so a frame culled again soon after is not skipped by
// restoreRetryInterval, and the inconclusive count, so the next episode gets the
// whole bound rather than inheriting a spent one.
func (w *Worker) clearRestored(ctx context.Context, f model.Frame) {
	_ = w.store.ClearFrameMissing(ctx, f.ID)
	_ = w.store.ClearRestoreRequest(ctx, w.profileID, f.ID)
	delete(w.lastRestoreAttempt, f.ID)
	delete(w.restoreInconclusive, f.ID)
}

// inconclusiveRestore is what the bound above reports: the archive copy is there and
// answers neither the row's expectation nor the short/absent shapes the two arms
// above handle, so no pass can say what is wrong with it. It names the file rather
// than the device, because on this arm the device was never read.
//
// The text carries the bound rather than the pass number, which keeps it identical
// on every pass past the bound. That matters because of sqlStore.RaiseAlert, which
// returns the existing id for an identical unacked (rig_id, category, message)
// tuple and inserts nothing: interpolating the running count — the obvious thing to
// want here — would make every pass a distinct message and mint an hourly alert
// instead of one standing one. The dependency is named rather than described so a
// future edit can grep for it.
type inconclusiveRestore struct {
	dest string
	err  error
}

func (e inconclusiveRestore) Error() string {
	msg := "its archive copy at " + e.dest + " neither verifies nor reads as missing, after " +
		strconv.Itoa(maxRestoreInconclusive) + " passes"
	if e.err != nil {
		msg += ": " + e.err.Error()
	}
	return msg
}

// restoreFailed raises one warn alert naming the frame; the request flag stays set
// (there is no give-up), holding the device original until the operator cancels.
func (w *Worker) restoreFailed(ctx context.Context, f model.Frame, err error) {
	w.log.Warn("rig: restore failed; request stands", "path", f.SourcePath, "error", err)
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityWarn,
		Category: model.AlertTransferFailed,
		Message:  "could not restore " + f.SourcePath + " from the device: " + err.Error(),
	})
}
