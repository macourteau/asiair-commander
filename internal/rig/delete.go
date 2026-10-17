package rig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/filter"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// deletePhase runs the delete pass in its own short session (design
// frame-lifecycle §9): for every deletion-eligible frame (verified, sha256
// recorded, grace elapsed) it authorizes one frame per Admission grant and runs
// the three-way integrity check before removing the proven original. Because
// this session is separate from the pull session, it re-confirms the device
// identity by a live probe before deleting anything, closing the DHCP-handoff
// window.
//
// One call is a slice (design frame-lifecycle §12.3): it starts after
// the round-robin cursor and yields at the first frame boundary past deadline,
// once it has streamed one device original or spent one more poll interval
// skipping cheap exits, so a large backlog cannot keep the next listing waiting.
func (w *Worker) deletePhase(ctx context.Context, cfg *model.Config, cy cycle, dev device.Device, deadline time.Time) slicePass {
	profile, ep := cy.profile, cy.ep
	// Defensive floor: a non-positive grace makes a frame deletion-eligible the
	// same cycle it is verified, which a re-read microseconds after a pull cannot
	// prove — the design architecture §7/§16 grace guarantee. Config validation
	// forbids this for a deleting rig, but a hand-edited config.yaml bypasses that
	// (startup does not validate), so refuse to delete rather than delete on a
	// zero grace.
	grace := cfg.EffectiveGrace(profile)
	if grace <= 0 {
		w.log.Warn("rig: effective grace is non-positive; skipping delete pass", "grace", grace.String())
		return slicePass{}
	}
	graceCutoff := w.now().Add(-grace).Unix()
	eligible, err := w.store.DeletionEligible(ctx, w.profileID, graceCutoff)
	if err != nil {
		w.log.Error("rig: deletion-eligible query failed", "error", err)
		return slicePass{}
	}
	if len(eligible) == 0 {
		// An empty list is a whole sweep by itself, and leaves nothing to repay.
		return slicePass{sweepDone: true, exhausted: true}
	}

	sess, err := dev.Open(ctx)
	if err != nil {
		w.log.Warn("rig: delete-pass session open failed; deferring", "error", err)
		return slicePass{}
	}
	ms := newMountedSession(sess)
	defer ms.Close()

	// Re-confirm the device identity WHILE this delete session is open (the pull
	// session already closed): a wrong device answering after a DHCP handoff must
	// not be deleted from. foreign → alert; foreign OR unconfirmed → delete nothing
	// this pass. The per-frame three-way content-hash check remains the true delete
	// backstop regardless.
	confirmed, foreign := w.confirmDeviceIdentity(ctx, profile, ep)
	if foreign {
		w.refuseForeignDevice(ctx, foreignAtSession)
	}
	if foreign || !confirmed {
		if !foreign {
			// An unanswered probe drops the whole pass with no alert; counting it keeps
			// the throughput lost to missed probes on a lossy link measurable.
			w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedIdentityUnconfirmed)
		}
		w.log.Warn("rig: delete pass skipped; device identity not confirmed", "foreign", foreign)
		return slicePass{unconfirmed: !foreign}
	}
	// Every pre-delete digest this slice measures goes through the cycle's site,
	// names the device this session reached and logs in as this session did. The
	// target is built only here, past the identity check, and passed down the
	// slice, so it ends with the slice and no later measurement may reuse its
	// address or login.
	target := hashTarget{site: cy.site, ip: ep.IP, profile: profile}

	w.setState(model.RigStateDeleting)
	w.sliceActive = true
	// Scoped restore. The slice ends between frames, and the pill must then show
	// what the last listing published rather than a stale "Cleaning up" through the
	// poll wait. It restores the listing's state, not the one seen here: a Pause and
	// a Resume served before the slice (by restorePhase's or the scan's
	// drainCommands) would otherwise reinstate Paused on an unpaused rig. The
	// per-frame cleanup readout is dropped at the same point, so a finished slice
	// never reads as still running.
	defer func() {
		w.sliceActive = false
		changed := w.st.Cleanup != nil
		w.st.Cleanup = nil
		// Undo our own Deleting, and any Paused left on an unpaused rig; a Pause that
		// still holds, or an Error written mid-slice, must survive.
		if w.st.State == model.RigStateDeleting || (w.st.State == model.RigStatePaused && !w.paused) {
			w.setState(w.restorableListedState())
			changed = true
		}
		if changed {
			w.publish()
		}
	}()
	w.publish()

	// Resolve the per-root marker verdict and the settle window ONCE for the pass
	// (not per frame): rootConfirmed costs a NAS marker read, and every culled frame
	// stays eligible every cycle, so a per-frame call would be hundreds of reads a
	// pass. A share that drops mid-pass is caught by the per-frame Stat/VerifyRead
	// error paths, which conclude nothing.
	rootOK := map[string]bool{}
	for _, root := range scanRoots(cfg) {
		ok, _ := w.rootConfirmed(ctx, root)
		rootOK[root] = ok
	}
	settle := cfg.EffectiveReconcileSettle(profile)
	if settle <= 0 {
		settle = defaultReconcileSettle
	}

	// One store read for the whole pass, like the root verdict above. A re-file
	// commits inside a single worker request, so the set cannot grow mid-pass on
	// this goroutine; it can only shrink, which is the safe direction. Failing to
	// read it means we cannot prove no move is in flight, so the pass deletes
	// nothing rather than risk INV-M2.
	openIntents, err := w.store.OpenIntentPaths(ctx, w.profileID)
	if err != nil {
		w.log.Error("rig: open re-file intent query failed; skipping delete pass", "error", err)
		return slicePass{}
	}
	// The supersede set is read on the same terms, for the same reason: a swap
	// commits inside a single worker request, so the set cannot grow mid-pass on this
	// goroutine, and failing to read it means we cannot prove no swap is in flight.
	openSupersedes, err := w.store.OpenSupersedePaths(ctx, w.profileID)
	if err != nil {
		w.log.Error("rig: open supersede intent query failed; skipping delete pass", "error", err)
		return slicePass{}
	}

	disabled := profile.DisabledShares()
	eligible, wrap := rotateAfter(eligible, w.deleteCursor) // wrap: where the head row landed
	w.log.Debug("rig: delete pass start", "eligible", len(eligible), "cursor", w.deleteCursor, "deadline", deadline)
	visited, streamed, removedHere, hashFailed := 0, 0, 0, 0
	sweepDone := false
	enabled := 0 // rows on disabled shares are never drained, so the readout never counts them
	for _, f := range eligible {
		if !disabled[f.SourceShare] {
			enabled++
		}
	}
	// The skip budget runs one poll past the later of the deadline and the loop's
	// start, so a slice whose pull phase already overran the deadline still gets a
	// full poll of skipping past cheap exits and advances the cursor (design
	// frame-lifecycle I1).
	skipFrom := deadline
	if loopStart := w.now(); loopStart.After(deadline) {
		skipFrom = loopStart
	}
	skipLimit := skipFrom.Add(cfg.EffectivePollInterval(profile))
	// The order of checks is load-bearing: suspend and ctx first so a Pause keeps
	// its meaning; the disabled-share skip before the deadline so a slice holding
	// only disabled-share rows never reports more; the deadline before deleteOne so
	// a slice only ever stops between frames.
	for i, f := range eligible {
		w.drainCommands(ctx)
		if ctx.Err() != nil || w.workSuspended() || w.destUnavailable {
			return slicePass{advanced: visited > 0, sweepDone: sweepDone, hashFailed: hashFailed}
		}
		if disabled[f.SourceShare] {
			// A disabled share is fully ignored: its device originals are not cleaned,
			// and its (possibly unreachable) share is never mounted — no mount-fail spam.
			// Nor does it count toward the progress floor.
			continue
		}
		// Past the deadline, stop once one device original has been streamed (the
		// costly part of a visit), or once the skip budget is spent: cheap exits cost
		// almost no link time, so they alone would let a slice yield having done no
		// delete work.
		if !w.now().Before(deadline) && (streamed > 0 || !w.now().Before(skipLimit)) {
			w.metrics.RecordDeletePassYield(w.profileID)
			w.log.Info("rig: delete pass reached its deadline; yielding to the next cycle",
				"visited", visited, "streamed", streamed, "remaining", len(eligible)-i, "device_tight", w.deviceTight)
			return slicePass{more: true, advanced: visited > 0, sweepDone: sweepDone, hashFailed: hashFailed}
		}
		if i >= wrap {
			// This visit moves the cursor past the tail, closing the sweep it held.
			// Counted here rather than on reaching the wrap index: a slice that stopped
			// there without visiting leaves the cursor on the tail, and the next slice
			// starts at the head with a wrap index of 0 and counts the same boundary.
			sweepDone = true
		}
		w.deleteCursor = deleteCursor{pulledAt: f.PulledAt, id: f.ID}
		visited++
		// Each frame's check re-reads its whole device original, so on a slow link a
		// drain runs for many minutes; publish its position per frame, counted across
		// the whole drain, so the card shows progress rather than a stall.
		w.st.Cleanup = &model.CleanupProgress{File: path.Base(f.SourcePath),
			Index: w.drainDone + 1, Total: w.drainDone + enabled - removedHere}
		w.publish()
		out := w.deleteOne(ctx, target, ms, f.ID, rootOK, openIntents, openSupersedes, settle)
		if out.streamed {
			streamed++
		}
		if out.hashFailed {
			hashFailed++
		}
		if out.endSlice {
			// The digest's own session did not reach this rig's device: delete
			// nothing more this slice, as an unconfirmed delete session would not.
			if out.unconfirmed {
				// Reported as the delete session's own unanswered probe is: not progress,
				// so a run until idle cannot finish idle on it, and counted as a stall. The
				// cursor still moved, and a sweep this slice closed goes uncounted, so the
				// sweep count lags rather than runs ahead.
				return slicePass{unconfirmed: true, hashFailed: hashFailed}
			}
			return slicePass{advanced: visited > 0, sweepDone: sweepDone, hashFailed: hashFailed}
		}
		if out.removed != "" {
			w.ownDeletes[out.removed] = true
			w.drainDone++
			removedHere++
			// The drain's last delete, by sweep: past the wrap it belongs to the sweep
			// after the one runCycle has counted so far.
			w.drainSweep = w.sweepHolding(sweepDone)
		}
	}
	w.deleteCursor = deleteCursor{} // exhausted: the next slice starts at the oldest frame
	// Iterating the whole list crosses the tail exactly once, wherever it started,
	// and the zeroed cursor means the next slice counts no boundary until its end.
	return slicePass{advanced: visited > 0, sweepDone: true, exhausted: true, hashFailed: hashFailed}
}

// restorableListedState is the pill state the last listing published, for a
// slice's end or a Resume to put back. No listing has recorded one on a worker
// that has not listed yet (only a direct call can get there); Idle is what a
// fresh worker starts as.
func (w *Worker) restorableListedState() model.RigState {
	if w.listedState == "" {
		return model.RigStateIdle
	}
	return w.listedState
}

// slicePass is what one delete slice reports to runCycle. more: it yielded at its
// deadline with eligible frames left. advanced: it visited a row, moving the
// cursor. sweepDone: it passed the tail of the eligible list, completing a sweep —
// by visiting a row at or past rotateAfter's wrap index, or by iterating the whole
// list. A slice that exhausts a list it started at the head right after the
// previous slice left the cursor on the tail crosses the tail twice and reports it
// once, so the sweep count can lag by one there, never run ahead. exhausted: it
// iterated its whole eligible list, or that list was empty. Every prologue early
// return reports none of them, so a broken session or an unconfirmed identity
// falls back to poll cadence.
//
// hashFailed and unconfirmed feed only the stalled-progress count: the frames
// whose pre-delete measurement failed (a remote site's fails without touching
// the session), and a delete session whose identity probe got no answer.
type slicePass struct {
	more, advanced, sweepDone, exhausted bool
	hashFailed                           int
	unconfirmed                          bool
}

// deleteCursor is the round-robin position of the delete pass: the
// (pulled_at, id) of the last frame a slice attempted, in DeletionEligible's own
// order. The zero value means "start at the oldest frame".
type deleteCursor struct{ pulledAt, id int64 }

// rotateAfter returns eligible rotated to start at the first frame whose
// (PulledAt, ID) is strictly after cursor, so each slice resumes past the last
// frame attempted and a head frame that hashes but never concludes cannot eat
// every slice. eligible must be in DeletionEligible's (pulled_at, id) order. A
// cursor naming a row that has since left the list resolves to its successor;
// one past the last row wraps to the head.
//
// wrap is where the original head row lands in the rotated list: len(eligible)
// for a zero cursor (nothing rotated, so the head is only reached by finishing
// the list), and 0 for a cursor past the end. A slice reaching it has passed the
// tail and completed a sweep.
//
// The input is never modified; a rotated result is a fresh slice.
func rotateAfter(eligible []model.Frame, cursor deleteCursor) (rotated []model.Frame, wrap int) {
	if cursor == (deleteCursor{}) {
		return eligible, len(eligible)
	}
	k := len(eligible)
	for i, f := range eligible {
		if f.PulledAt > cursor.pulledAt || (f.PulledAt == cursor.pulledAt && f.ID > cursor.id) {
			k = i
			break
		}
	}
	rotated = make([]model.Frame, 0, len(eligible))
	rotated = append(rotated, eligible[k:]...)
	rotated = append(rotated, eligible[:k]...)
	return rotated, len(eligible) - k
}

// deleteOutcome is what one deleteOne call did to the device, reported so a
// slice can bound itself without deleteOne changing any decision: whether it
// streamed the device original (the costly part of a visit, whatever the hash
// then showed), and the listingKey of the source it removed. A source that had
// already vanished is not recorded, since Commander removed nothing.
type deleteOutcome struct {
	streamed bool   // hashDevice was called for this frame
	removed  string // listingKey(share, path) of the source deviceDeleteFrame removed; "" otherwise
	endSlice bool   // hashDevice refused the digest on identity; the slice deletes nothing more
	// unconfirmed: endSlice because the measurement's probe got no answer, rather
	// than because it reached a different device.
	unconfirmed bool
	// hashFailed: the measurement failed for another reason than a Pause or the
	// identity, and the frame was deferred. It feeds the stalled-progress count
	// only, since a remote site's digest call fails without touching the session.
	hashFailed bool
}

// deleteOne authorizes and runs the three-way integrity check for one frame,
// deleting it (and its recorded sidecars) from the device only when
// deviceHash == nasHash == recorded sha256 and the source re-stats within ±2s
// (design frame-lifecycle §9). One frame per Admission grant.
//
// It re-reads the row fresh (a restore request or a clear may have arrived since
// deletePhase loaded the eligible set) and takes the per-pass root verdict, open
// re-file intents and settle window rather than re-computing them.
//
// For an ABSENT NAS copy — the operator culled it — it hands off to
// handleAbsentNAS, which authorizes the device-original deletion only once every
// disqualifier is clear and the device copy still hashes to what was archived
// (design library-reconciliation §9.1).
//
// The returned deleteOutcome reports only what the call already did — whether it
// streamed the device original, and which source it removed — so the caller can
// bound a slice without changing any decision made here.
func (w *Worker) deleteOne(ctx context.Context, t hashTarget, ms *mountedSession, frameID int64, rootOK, openIntents, openSupersedes map[string]bool, settle time.Duration) (out deleteOutcome) {
	cfg := w.cfg.Load()
	fp, ok, err := w.store.GetFrameByID(ctx, frameID)
	if err != nil || !ok {
		return // vanished on re-read (pruned/dismissed): nothing to authorize
	}
	f := *fp

	// Mounted from the row read fresh above, not from the eligible-set snapshot, and
	// through the session rather than a share the caller resolved: restoreNAS may
	// have to reach a sharing row's copy on a different share.
	share, err := ms.share(f.SourceShare)
	if err != nil {
		w.log.Warn("rig: delete-pass mount failed", "share", f.SourceShare, "error", err)
		return
	}

	release, err := w.adm.Acquire(ctx, w.requester(t.site), 0)
	if err != nil {
		if errors.Is(err, ErrDestinationUnavailable) {
			// Cycle-level, as in the pull phase: the same free-space probe runs for
			// every frame regardless of size, so without the latch an unreadable
			// destination logs one WARN per eligible frame.
			w.destUnavailable = true
			w.log.Debug("rig: delete admission aborted; destination unreadable", "path", f.SourcePath, "error", err)
			return
		}
		w.log.Warn("rig: delete admission aborted", "path", f.SourcePath, "error", err)
		return
	}
	defer release()

	// A removable medium must present a readable media marker before any cleanup,
	// so an absent or unmarked stick's empty listing is never read as mass
	// deletions (H5/H10). This is a presence check, not an identity check — the
	// swap backstop is the three-way hash and re-stat below.
	if !w.mediaConfirmedForDelete(share) {
		// A quiet path that suppresses ALL deletes for the share with no alert — one of
		// the two reasons this metric exists.
		w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedMediaUnconfirmed)
		w.log.Debug("rig: removable medium not confirmed; deferring delete", "path", f.SourcePath)
		return
	}

	stat, serr := share.Stat(f.SourcePath)
	if serr != nil {
		if device.IsNotFound(serr) {
			// A successful listing showing the file absent is "vanished".
			w.handleVanishedSource(ctx, f)
			return
		}
		// A read/list error is NEVER vanished — transient, retry next pass.
		w.log.Warn("rig: pre-delete stat transient error; deferring", "path", f.SourcePath, "error", serr)
		return
	}

	// Source present but changed since filing → defer, and only hold once the
	// deferral budget is spent.
	if stat.Size != f.Size || !clock.FromOrigin(stat.Mtime).EqualWithin(clock.FromOrigin(time.Unix(f.Mtime, 0)), mtimeTol) {
		w.deferChangedSource(ctx, f)
		return
	}

	// Cheap NAS-side Stat BEFORE the ~116 MB device hash: if the copy is gone (the
	// operator culled it), the absent branch decides without this pass having hashed
	// anything yet, so a still-settling frame never re-hashes its full device copy
	// every pass.
	if _, _, nerr := w.lfs.Stat(f.DestPath); nerr != nil {
		if isNotExist(nerr) {
			out = w.handleAbsentNAS(ctx, t, cfg, share, f, rootOK, openIntents, openSupersedes, settle, deviceDigests{})
			return
		}
		w.log.Warn("rig: pre-hash NAS stat transient error; deferring delete", "dest", f.DestPath, "error", nerr)
		return
	}

	// The present-copy branch needs its own guard: absentDeferReasons is the whole
	// story for a re-file, which always vacates the path it moves from, but a
	// supersede replaces a path IN PLACE. Between its steps 2 and 3 the canonical
	// name is present and holds bytes no row is anchored to yet, a shape a re-file
	// never produces — so a row sharing that dest_path, whose own device copy the
	// swap did not touch and which therefore passes the gate above, would reach the
	// three-way check below and earn a critical delete_mismatch on a perfectly good
	// archive copy. One map lookup, spent before the ~116 MB device hash (design
	// frame-lifecycle §11.9).
	if openSupersedes[f.DestPath] {
		w.log.Info("rig: archive copy is mid-swap; deferring delete",
			"path", f.SourcePath, "dest", f.DestPath, "reasons", "supersede_in_flight")
		return
	}

	out.streamed = true
	digests, err := w.hashDevice(ctx, t, share, f.SourcePath)
	if errors.Is(err, errHashIdentity) {
		out.endSlice, out.unconfirmed = true, errors.Is(err, errHashUnconfirmed)
		return
	}
	if errors.Is(err, errHashInterrupted) {
		w.log.Info(hashInterruptedMsg(ctx), "path", f.SourcePath)
		w.metrics.RecordDeleteHashInterrupted(w.profileID)
		return
	}
	if err != nil {
		w.log.Warn("rig: device re-hash transient error; deferring delete", "path", f.SourcePath, "error", err)
		out.hashFailed = true
		return
	}
	deviceHash := digests.whole

	nasHash, verr := w.lfs.VerifyRead(f.DestPath, f.ArchiveSize)
	if verr != nil {
		if errors.Is(verr, localfs.ErrShortFile) {
			// A truncated-but-present NAS copy is corruption, not a cull → re-pull to
			// recover it (replacing the short file). This is the ONE remaining restore
			// path; an ABSENT copy is never restored.
			if rerr := w.restoreNAS(ctx, ms, f, true); rerr != nil && !errors.Is(rerr, errRestoreHeld) {
				// errRestoreHeld already held the frame and alerted; only a genuine
				// failure needs this defer log.
				w.log.Warn("rig: restore of truncated NAS copy failed; deferring", "dest", f.DestPath, "error", rerr)
			}
			return
		}
		if isNotExist(verr) {
			// Raced: the copy vanished between the pre-hash Stat and here. Treat as
			// absent — never restore. The device copy was streamed just above, so hand
			// that hash over rather than reading ~116 MB again.
			// streamed is already set by the hash above; only the removal, and an
			// identity refusal ending the slice, are news.
			ao := w.handleAbsentNAS(ctx, t, cfg, share, f, rootOK, openIntents, openSupersedes, settle, digests)
			out.removed, out.endSlice, out.unconfirmed = ao.removed, ao.endSlice, ao.unconfirmed
			return
		}
		w.log.Warn("rig: NAS re-hash transient error; deferring delete", "dest", f.DestPath, "error", verr)
		return
	}

	// The NAS copy is present and readable: any stale missing mark / restore request
	// is moot (the file is demonstrably back).
	if f.MissingSeenAt != 0 {
		_ = w.store.ClearFrameMissing(ctx, f.ID)
	}
	if f.RestoreRequestedAt != 0 {
		_ = w.store.ClearRestoreRequest(ctx, w.profileID, f.ID)
	}

	// Normal cleanup is verified-only: an unclassified frame is not filed to its real
	// home, so its device original is kept as the authoritative copy until the operator
	// resolves it. This guard sits AFTER the mark-clear above so a reappeared (or raced-
	// back) unclassified NAS copy has its stale missing/restore marks cleared and is then
	// skipped here — never deleted and never held (design library-reconciliation §11.2,
	// AC2). Only the absent-NAS cull-drain branch (handleAbsentNAS) authorizes deleting an
	// unclassified original.
	if f.State != model.StateVerified {
		return
	}

	if deviceHash != f.SHA256 || nasHash != archiveHash(f) {
		// Device side stays f.SHA256 (the untouched original); the archive side uses
		// archiveHash so a retargeted NAS copy verifies against retarget_sha256
		// (design routing-and-filing §9.5).
		w.holdFrame(ctx, f, model.HoldThreeWayMismatch, "delete-pass three-way check failed (device/NAS/recorded mismatch)")
		return
	}

	deleted, err := w.deviceDeleteFrame(ctx, cfg, share, f)
	if err != nil {
		if errors.Is(err, errSourceChangedBeforeDelete) {
			// Benign: a new frame overwrote the path after our verify. Do NOT delete
			// and do NOT alert — the frame stays verified (safe on NAS) and the pull
			// path's differ branch reconciles it next cycle. The second quiet
			// stop-deleting path this metric exists for.
			w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedSourceChangedPreDelete)
			w.log.Info("rig: source changed just before delete; skipping, pull path will reconcile", "path", f.SourcePath)
			return
		}
		// A persistent sharing violation through the retry budget does not escalate
		// or force: the frame stays verified (already safe on the NAS), alerted, and
		// retried on a later pass.
		w.log.Warn("rig: device delete failed; frame stays verified (safe on NAS)", "path", f.SourcePath, "error", err)
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertTransferFailed,
			Message:  "could not delete source (still safe on NAS): " + f.SourcePath + ": " + err.Error(),
		})
		return
	}
	if deleted {
		out.removed = listingKey(f.SourceShare, f.SourcePath)
	}
	manner := metrics.MannerDeviceDeleted
	if !deleted {
		// deviceDeleteFrame proved the source had already vanished after verify — the
		// row is closed out but nothing was removed, so it is not a real device delete.
		manner = metrics.MannerSourceVanished
	}
	w.markDeleted(ctx, f, manner)
	w.log.Info("rig: deleted proven original from device", "path", f.SourcePath, "dest", f.DestPath)
	return
}

// handleAbsentNAS decides what to do when a deletion-eligible frame's NAS copy is
// confirmed absent (the operator culled it). It NEVER restores. It authorizes the
// device-original deletion only when the mark has settled, no restore is pending,
// the root is confirmed, and the path is under a configured root — the row having
// been re-read fresh by deleteOne, and re-checked here immediately before acting
// (design library-reconciliation §9.1). Like deleteOne, it reports whether it
// streamed the device original and which source it removed, so a culled frame's hash
// and delete count like any other.
func (w *Worker) handleAbsentNAS(ctx context.Context, t hashTarget, cfg *model.Config, share device.Share, f model.Frame, rootOK, openIntents, openSupersedes map[string]bool, settle time.Duration, known deviceDigests) (out deleteOutcome) {
	if reasons := absentDeferReasons(f, rootOK, openIntents, openSupersedes, settle, w.now().Unix()); len(reasons) > 0 {
		w.log.Info("rig: culled frame not yet authorized for deletion; deferring",
			"path", f.SourcePath, "reasons", strings.Join(reasons, ","))
		return
	}

	// Authorized. With no NAS copy left there is no three-way check to run, so the
	// recorded sha256 is the only evidence that the original still on the device is
	// the frame that was archived and then culled. Prove that before removing it.
	// The caller may already have streamed the device copy this pass (the branch
	// where the NAS copy vanished between the pre-hash Stat and VerifyRead); reuse
	// that rather than paying a second ~116 MB read.
	digests := known
	if digests.whole == "" {
		var err error
		out.streamed = true
		if digests, err = w.hashDevice(ctx, t, share, f.SourcePath); err != nil {
			if errors.Is(err, errHashIdentity) {
				out.endSlice, out.unconfirmed = true, errors.Is(err, errHashUnconfirmed)
				return
			}
			if errors.Is(err, errHashInterrupted) {
				w.log.Info(hashInterruptedMsg(ctx), "path", f.SourcePath)
				w.metrics.RecordDeleteHashInterrupted(w.profileID)
				return
			}
			w.log.Warn("rig: device re-hash transient error; deferring culled-frame delete", "path", f.SourcePath, "error", err)
			out.hashFailed = true
			return
		}
	}
	if digests.whole != f.SHA256 {
		// Different content at the same path — a counter-reset run overwrote it — so
		// this is NOT the frame the operator culled, and deleting it would destroy a
		// frame that was never archived. Refuse, and hold: that surfaces it and takes
		// the row out of DeletionEligible (which selects verified and culled-unclassified
		// frames — held is excluded), so the pass stops re-hashing a full device copy
		// every cycle. The pull path's differ
		// branch reconciles the new content as its own frame.
		w.holdFrame(ctx, f, model.HoldSourceChanged,
			"device content changed since filing; refusing to delete the original of a culled frame")
		return
	}

	// The device copy has just proven itself byte-identical to what was archived, so
	// the data unit teed off that same stream is exactly as authoritative an exposure
	// identity as one read from the archive copy — and it is the only one available to
	// a culled row, which has no archive copy by definition. It rides the read this
	// pass was already making and opens nothing of its own, which is what bounds the
	// device-side proof to the rows this drain passes through (design
	// frame-lifecycle §11.8).
	w.recordProvenDataDigest(ctx, f, digests.data)

	// design library-reconciliation §2.7's second evaluation, and it is NOT
	// optional: hashing streamed the whole device copy and took tens of seconds,
	// during which drainCommands has not run for this frame. Without this, a Restore
	// the operator clicks mid-hash sits unserviced on the worker channel and the
	// delete proceeds on a stale snapshot — destroying the last remaining copy of a
	// frame they are actively recovering. Service what queued, then re-read the row
	// and re-run the NAS-side checks.
	w.drainCommands(ctx)
	if ctx.Err() != nil || w.workSuspended() || w.destUnavailable {
		return
	}
	fresh, ok, ferr := w.store.GetFrameByID(ctx, f.ID)
	if ferr != nil || !ok {
		return // pruned or dismissed while hashing: nothing left to authorize
	}
	f = *fresh
	cfg = w.cfg.Load()

	if _, _, nerr := w.lfs.Stat(f.DestPath); nerr == nil {
		// The copy came back while we hashed, so the premise of this branch is gone.
		// Clear the now-stale marks and let the ordinary present-copy path (with its
		// full three-way check) own it on the next pass rather than deleting on a
		// premise that just changed.
		_ = w.store.ClearFrameMissing(ctx, f.ID)
		if f.RestoreRequestedAt != 0 {
			_ = w.store.ClearRestoreRequest(ctx, w.profileID, f.ID)
		}
		w.log.Info("rig: NAS copy reappeared while hashing; no longer treating it as culled", "dest", f.DestPath)
		return
	} else if !isNotExist(nerr) {
		w.log.Warn("rig: post-hash NAS stat transient error; deferring culled-frame delete", "dest", f.DestPath, "error", nerr)
		return
	}
	if reasons := absentDeferReasons(f, rootOK, openIntents, openSupersedes, settle, w.now().Unix()); len(reasons) > 0 {
		w.log.Info("rig: culled frame disqualified while hashing; deferring",
			"path", f.SourcePath, "reasons", strings.Join(reasons, ","))
		return
	}

	deleted, err := w.deviceDeleteFrame(ctx, cfg, share, f)
	if err != nil {
		if errors.Is(err, errSourceChangedBeforeDelete) {
			// Benign: the source changed between the hash and the delete. Do not delete
			// and do not alert — the pull path reconciles it next cycle.
			w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedSourceChangedPreDelete)
			w.log.Info("rig: source changed just before a culled-frame delete; skipping", "path", f.SourcePath)
			return
		}
		w.log.Warn("rig: culled-frame device delete failed; frame stays eligible", "path", f.SourcePath, "error", err)
		return
	}
	if deleted {
		out.removed = listingKey(f.SourceShare, f.SourcePath)
	}
	// Labelled distinctly from an ordinary grace delete: this is the newly-armed,
	// riskiest path, and an operator wants to alert on a spike of it on its own.
	manner := metrics.MannerCulled
	if f.MissingAckedAt != 0 {
		manner = metrics.MannerCulledAcked
	}
	if !deleted {
		// Unchanged and must stay LAST: nothing was actually removed, so neither
		// culled label may claim a deletion.
		manner = metrics.MannerSourceVanished
	}
	persisted := w.markDeleted(ctx, f, manner)
	if deleted && persisted {
		// design library-reconciliation §10.6: we removed the original because the
		// frame was culled, so the row is fully resolved — nulling dest_path is what
		// stops the next scan re-deriving the missing mark and raising a false
		// library_reconciled alert. NOT in the !deleted branch: that is a both-sides
		// cull whose mark keeps the reinserted-card restore path alive. Gated on
		// persisted: if markDeleted's UpdateFrame failed the row is still 'verified',
		// and nulling its dest_path would strand it deletion-eligible with no
		// destination, tripping a false doubly-vanished alert next pass — worse than
		// the accepted crash-window.
		if err := w.store.ResolveCulledFrame(ctx, f.ID); err != nil {
			w.log.Warn("rig: could not resolve a culled frame out of the scan; it may reappear in the missing panel",
				"frame", f.ID, "path", f.SourcePath, "error", err)
		}
	}
	if deleted {
		w.log.Warn("rig: deleted the device original of a frame culled from the NAS",
			"path", f.SourcePath, "dest", f.DestPath, "acknowledged", f.MissingAckedAt != 0)
	} else {
		// Pre-existing inaccuracy, fixed here because these are the lines being edited:
		// the WARN above was emitted unconditionally and claimed a deletion that never
		// happened. This branch is the ONLY operator-visible signal for a source that
		// vanished between the hash and the delete, so it must say what actually occurred.
		w.log.Warn("rig: culled frame's source had already vanished before the delete; nothing removed",
			"path", f.SourcePath, "dest", f.DestPath)
	}
	return
}

// absentDeferReasons returns the release-two disqualifiers that apply to an absent
// frame, evaluated purely from stored state (no I/O) so it is also the basis for
// the validation report's counterfactual upper bound.
//
// openIntents is the rig's set of dest_paths with an open re-file intent, read once
// per delete pass. A frame mid-move is never deletion-eligible however the move
// failed — crash, commit failure, or a failed rename-back — which is what makes
// INV-M2 true by construction rather than by alerting: without it the file sits at
// the new path, the row at the old one, and one settle window later this pass would
// delete a device original against an archive nothing references.
//
// openSupersedes is the same set for the four-step name claim, read the same way. A
// frame whose archive copy is mid-swap has its bytes under a reserved sibling name
// for part of that sequence, so its canonical path reads as absent to this pass;
// deferring on the open intent is what closes that window BY CONSTRUCTION rather
// than by scanning for the damage afterwards (design frame-lifecycle §11.9).
func absentDeferReasons(f model.Frame, rootOK, openIntents, openSupersedes map[string]bool, settle time.Duration, now int64) []string {
	var reasons []string
	if f.MissingSeenAt == 0 {
		reasons = append(reasons, "not_marked")
	} else if f.MissingAckedAt == 0 && now-f.MissingSeenAt < int64(settle.Seconds()) {
		// The operator vouched for this cull, so the settle window — which exists to
		// catch an ACCIDENTAL absence — has nothing left to protect. Every other
		// reason below still applies; see design library-reconciliation §10.2 on what
		// this costs.
		reasons = append(reasons, "settling")
	}
	if f.RestoreRequestedAt != 0 {
		reasons = append(reasons, "restore_requested")
	}
	if openIntents[f.DestPath] {
		reasons = append(reasons, "refile_in_flight")
	}
	if openSupersedes[f.DestPath] {
		reasons = append(reasons, "supersede_in_flight")
	}
	// Root membership: the longest configured root that contains the path must be
	// present and currently confirmed.
	var best string
	for root := range rootOK {
		if store.UnderRoot(f.DestPath, root) && len(root) > len(best) {
			best = root
		}
	}
	if best == "" {
		reasons = append(reasons, "outside_roots")
	} else if !rootOK[best] {
		reasons = append(reasons, "root_unconfirmed")
	}
	return reasons
}

// handleVanishedSource records a benign deletion only after re-proving the NAS
// copy is present and matches. A doubly-vanished frame (source gone AND NAS copy
// gone/corrupt) is the routine both-sides cull design library-reconciliation §1
// describes: it is marked deleted (a terminal state, so it leaves DeletionEligible
// and stops being re-probed every poll cycle) and alerted at WARN, not the
// critical frame_lost a genuinely unexplained loss would raise. Critical is
// reserved for a present-but-corrupt NAS copy. Marking deleted destroys nothing —
// the source is already gone by construction (the caller's Stat returned
// not-found) and the NAS copy is confirmed absent — it only records what already
// happened.
func (w *Worker) handleVanishedSource(ctx context.Context, f model.Frame) {
	nasHash, verr := w.lfs.VerifyRead(f.DestPath, f.ArchiveSize)
	if verr != nil {
		if isNotExist(verr) {
			// A both-sides cull is routine cleanup, not a loss: warn (not critical
			// frame_lost) and mark deleted (a terminal state, so it stops being
			// re-probed every pass). Recorded as a source_vanished delete, not a
			// frame-lost — the corrupt branch below is the one genuine-loss case.
			w.alertFrameLost(ctx, &f, "source vanished and NAS copy is missing (doubly-vanished)", model.SeverityWarn)
			w.markDeleted(ctx, f, metrics.MannerSourceVanished)
			return
		}
		if errors.Is(verr, localfs.ErrShortFile) {
			w.alertFrameLost(ctx, &f, "source vanished and NAS copy is truncated (doubly-vanished)", model.SeverityWarn)
			w.markDeleted(ctx, f, metrics.MannerSourceVanished)
			return
		}
		w.log.Warn("rig: NAS re-hash transient error on vanished source; deferring", "dest", f.DestPath, "error", verr)
		return
	}
	if nasHash != archiveHash(f) {
		// Present-but-corrupt: a genuine integrity problem, not routine cleanup. Archive
		// side, so archiveHash (retarget_sha256 for a retargeted copy) (design
		// routing-and-filing §9.5).
		w.metrics.RecordFrameLost(w.profileID, metrics.LostNASCorrupt)
		w.alertFrameLost(ctx, &f, "source vanished and NAS copy is corrupt (doubly-vanished)", model.SeverityCritical)
		return
	}
	w.markDeleted(ctx, f, metrics.MannerSourceVanished)
	w.log.Info("rig: source vanished; NAS copy verified good, marking deleted", "path", f.SourcePath)
}

// errRestoreHeld signals that a restore streamed the device copy but its content
// no longer matches what was filed, so the frame was held rather than restored.
// The caller must not clear the missing mark or the operator's restore request.
var errRestoreHeld = errors.New("restore held: device content changed since filing")

// restoreNAS re-pulls and re-verifies a frame's NAS copy from a still-resident
// device original, recovering it before any delete is considered (design
// frame-lifecycle §9). It reads config through w.cfg.Load() (deleteOne carries no
// *Config). replaceExisting removes a present-but-corrupt destination
// immediately before the rename: the exclusive RenameNoReplace fails on an
// existing (truncated) file, so without the removal the truncated copy is never
// replaced. It returns an error so the caller can report failure (errRestoreHeld
// when no device copy answered and the frame was held). It marks the restore in
// progress before the destructive Remove so a crash in that window leaves a
// standing restore request rather than a self-inflicted cull. A restore-result
// metric is recorded once on exit, except on a panic (which the outer runCycle
// recover owns, hazard P7).
//
// "A still-resident device original" is the frame's own in every case but one. A
// restore puts back the bytes the ARCHIVE expectation names, so where a dest_path
// is shared the copy it streams is whichever sharing row's answers that expectation
// — after a supersede the revised row's, not the row whose restore was requested —
// and a restore no sharing row can satisfy is refused and the frame held, which is
// what an unsatisfiable restore already does. It takes a mounted session rather
// than one share for exactly that: a sharing row's copy can live on another share.
//
// The discriminator of design frame-lifecycle §11.4 applies to the streamed bytes
// here as it does on the pull path, so the requested row's own copy the device has
// since revised restores from the REVISED bytes instead of holding. Both copies
// then hold what was written, so the row commits — see commitRestore, where its
// success path wrote no frame column at all (design frame-lifecycle §11.6).
func (w *Worker) restoreNAS(ctx context.Context, ms *mountedSession, f model.Frame, replaceExisting bool) error {
	result := metrics.RestoreLocalError
	defer func() {
		if r := recover(); r != nil {
			panic(r)
		}
		w.metrics.RecordNASRestore(w.profileID, result)
	}()

	w.log.Warn("rig: recovering NAS copy from device", "dest", f.DestPath, "path", f.SourcePath, "replace", replaceExisting)
	cfg := w.cfg.Load()
	if err := w.lfs.MkdirAll(cfg.Roots.Staging); err != nil {
		return fmt.Errorf("restore staging mkdir: %w", err)
	}
	profile := cfg.Rig(w.profileID)
	if profile == nil {
		// A reload dropped this rig between the phase that opened the session and here,
		// which is a teardown: nothing can be said about its shares, so nothing is read.
		return fmt.Errorf("restore: rig %s is no longer configured", w.profileID)
	}
	candidates, err := w.restoreCandidates(ctx, f)
	if err != nil {
		return err
	}

	// The backstop that removes whatever staging file survives the loop: the copy held
	// back for the diverged arm, which has to outlive its own iteration because it is
	// only installed once no sharing row has answered, and the chosen one when
	// something after the rename fails. A rejected candidate's file goes in its own
	// iteration instead (dropTemps), so the loop holds one frame's worth at a time.
	var temps []string
	defer func() {
		for _, p := range temps {
			_ = w.lfs.Remove(p)
		}
	}()

	// Whether this rig's ledger names the archive file twice is what decides between
	// the two rules below, and it is read once, before any copy is streamed.
	shared := len(candidates) > 1
	var chosen, own *restoreSource
	var firstErr error
	disabled := profile.DisabledShares()
	for _, c := range candidates {
		if c.ID != f.ID {
			if disabled[c.SourceShare] {
				// A disabled share is fully ignored, here as everywhere: its device copies
				// are not read and its (possibly unreachable) share is never mounted.
				continue
			}
			// The requested row's medium was confirmed by the caller; a sharer's is its
			// own share, and a swapped removable medium mounts with an empty root.
			if sh, serr := ms.share(c.SourceShare); serr != nil || !w.mediaConfirmedForDelete(sh) {
				w.log.Warn("rig: a sharing row's device copy is out of reach; skipping it",
					"share", c.SourceShare, "path", c.SourcePath, "error", serr)
				continue
			}
		}
		staged := len(temps)
		src, serr := w.readRestoreSource(cfg, ms, c, &temps)
		if serr != nil {
			temps = w.dropTemps(temps, staged)
			if firstErr == nil {
				firstErr = serr
			}
			w.log.Warn("rig: a candidate device copy could not be read for a restore",
				"dest", f.DestPath, "path", c.SourcePath, "error", serr)
			continue
		}
		if src.archiveSHA256 == f.ArchiveSHA256 && src.archiveSize == f.ArchiveSize {
			// This copy answers the archive expectation, which is what a restore puts
			// back. Where the file is shared that is the whole rule, and where it is not
			// it is the ordinary case, so it ends the search either way.
			chosen = src
			break
		}
		if !shared && ((src.deviceSHA256 == c.SHA256 && src.deviceSize == c.Size) || src.revised) {
			// One row names this archive file, so its own device copy is the only content
			// there is for it: the bytes the row records, or the device's own revision of
			// them. Either way the archive anchors advance to what gets written — which is
			// how a culled row whose device anchors a revision already advanced is restored
			// at all, its archive expectation still naming the content it was culled with.
			//
			// Deliberately not offered for a SHARED file: two rows sharing one archive copy
			// have one correct content for it, so a row writing its own divergent bytes
			// there would leave the other naming a file that is not its frame. A restore no
			// sharing row can satisfy is refused, exactly as an unsatisfiable one already is.
			own = src
			continue
		}
		// This copy answers nothing, and its staging file is the size of a frame, so it
		// goes now rather than at return: only a copy held back for the arm above
		// outlives its own iteration.
		temps = w.dropTemps(temps, staged)
	}
	if chosen == nil && own != nil {
		adopted, aerr := w.adoptDivergedRestore(ctx, f, own)
		if aerr != nil {
			return aerr
		}
		chosen = adopted
	}
	if chosen == nil {
		if firstErr != nil {
			// Nothing is concluded from an error: a device copy nobody could read proves
			// nothing about the frame, so the request stands and a later pass retries.
			if errors.Is(firstErr, errRestoreDeviceRead) {
				result = metrics.RestoreDeviceError
			}
			return firstErr
		}
		result = metrics.RestoreHeld
		// holdFrame records the block metric and raises the alert. Signal the caller so
		// it does NOT treat the hold as a completed restore and clear the mark/request.
		w.holdFrame(ctx, f, model.HoldRestoreMismatch, "device content changed since filing (mismatch during NAS restore)")
		return errRestoreHeld
	}

	if err := w.lfs.MkdirAll(dirOf(f.DestPath)); err != nil {
		return fmt.Errorf("restore mkdir dest: %w", err)
	}
	if replaceExisting {
		// Mark intent BEFORE the destructive Remove, so a crash between Remove and
		// rename leaves a standing restore request rather than an armed cull. If the
		// mark can't be persisted, abort rather than remove the copy unprotected.
		if err := w.store.MarkRestoreInProgress(ctx, f.ID, w.now().Unix()); err != nil {
			return fmt.Errorf("restore mark intent: %w", err)
		}
		_ = w.lfs.Remove(f.DestPath)
	}
	if err := w.lfs.RenameNoReplace(chosen.path, f.DestPath); err != nil {
		if localfs.IsExist(err) {
			// Measured against the bytes this restore was about to install, not against
			// the row's standing expectation: on the revision arm the two differ, and a
			// concurrent restore of the same revision is still a completed restore.
			if eh, e := w.lfs.VerifyRead(f.DestPath, chosen.archiveSize); e == nil && eh == chosen.archiveSHA256 {
				result = metrics.RestoreAlreadyPresent
				w.log.Info("rig: NAS copy already restored concurrently", "dest", f.DestPath)
				return w.commitRestore(ctx, f, chosen)
			}
		}
		return fmt.Errorf("restore rename: %w", err)
	}
	result = metrics.RestoreRestored
	if err := w.commitRestore(ctx, f, chosen); err != nil {
		return err
	}
	w.log.Info("rig: restored NAS copy", "dest", f.DestPath, "from", chosen.row.SourcePath, "revised", chosen.revised)
	return nil
}

// dropTemps removes the staging files one candidate produced and returns the list
// without them, so a restore that streams several sharing copies holds one frame's
// worth of staging at a time rather than every candidate's at once.
func (w *Worker) dropTemps(temps []string, from int) []string {
	for _, p := range temps[from:] {
		_ = w.lfs.Remove(p)
	}
	return temps[:from]
}

// restoreSource is one candidate for the bytes a restore puts back: a row sharing
// the archive file, the file its device copy was streamed (and, where the row
// carries an overlay, re-edited) into, and what that stream proved about each copy.
type restoreSource struct {
	// row is the sharing row whose DEVICE copy was read, which after a supersede is
	// not necessarily the row whose restore was requested.
	row model.Frame
	// path is the file that would be renamed into DestPath.
	path string
	// The device-side anchors of row's copy, as just proven off the device.
	deviceSHA256 string
	deviceSize   int64
	deviceMtime  int64
	// The archive-side anchors of path. They part from the device ones exactly where
	// an overlay was re-applied, which is what the two column groups exist for.
	archiveSHA256 string
	archiveSize   int64
	// revised says the streamed bytes are a proven device-side revision of row:
	// different bytes, the same exposure.
	revised bool
}

// errRestoreDeviceRead marks a candidate whose device copy could not be read at
// all, so that a restore left with no source can tell "the device would not answer"
// — report it, the request stands — from "no copy answers the expectation", which
// holds the frame.
var errRestoreDeviceRead = errors.New("restore: device copy unreadable")

// restoreCandidates is the set of device copies that may answer f's archive
// expectation, the requested row first.
//
// A restore puts back the bytes the archive expectation names, and two rows sharing
// one archive file have ONE correct content for it — so the copy to stream is
// whichever sharing row's hashes to archive_sha256, which after a supersede is the
// revised row's rather than the row whose restore was requested (design
// frame-lifecycle §11.6). The requested row leads because it is the answer in every
// case but that one, and a candidate that answers ends the search before any
// further copy is streamed.
//
// A `deleted` row is not one of those copies. A tombstone has no live claim on the
// archive file's content — its device original is gone and its row is a terminal
// record — so it can neither answer the expectation nor be stranded by bytes that do
// not. Counted as a sharer it closes the diverged-adopt arm for the live row and
// then contributes the absent-copy read error the caller returns in preference to
// the hold, which turns a satisfiable restore into an hourly re-stream of the whole
// device copy behind a standing WARN. The requested row itself always leads,
// whatever its state: it is the request's own subject, and dropping it would leave
// the restore with nothing to read.
func (w *Worker) restoreCandidates(ctx context.Context, f model.Frame) ([]model.Frame, error) {
	rows, err := w.store.FramesByDestPath(ctx, w.profileID, f.DestPath)
	if err != nil {
		return nil, fmt.Errorf("restore: could not read the rows sharing %s: %w", f.DestPath, err)
	}
	out := []model.Frame{f}
	for _, r := range undeletedRows(rows) {
		if r.ID != f.ID {
			out = append(out, r)
		}
	}
	return out, nil
}

// readRestoreSource streams one candidate's device copy into staging, re-applies
// that row's overlay where it carries one, and reports what the bytes in hand prove
// about both copies. It writes nothing outside staging and decides nothing: which
// candidate the restore installs is the caller's.
//
// The data unit is teed off the stream the restore is already making, so the
// discriminator costs no extra open — the same shape the pull path's device-side
// consumer has (design frame-lifecycle §11.3).
func (w *Worker) readRestoreSource(cfg *model.Config, ms *mountedSession, c model.Frame, temps *[]string) (*restoreSource, error) {
	share, err := ms.share(c.SourceShare)
	if err != nil {
		return nil, fmt.Errorf("%w: mount %s: %v", errRestoreDeviceRead, c.SourceShare, err)
	}
	tmp, err := os.CreateTemp(cfg.Roots.Staging, "asiair-restore-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("restore temp create: %w", err)
	}
	tmpPath := tmp.Name()
	*temps = append(*temps, tmpPath)

	rc, err := share.OpenReader(c.SourcePath)
	if err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("%w: open %s: %v", errRestoreDeviceRead, c.SourcePath, err)
	}
	tee := header.NewDigestTee()
	buf := make([]byte, header.CopyBufSize)
	n, cerr := io.CopyBuffer(io.MultiWriter(tmp, tee), rc, buf)
	_ = rc.Close()
	if cerr != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("%w: stream %s: %v", errRestoreDeviceRead, c.SourcePath, cerr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("restore fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("restore close: %w", err)
	}
	streamHash, streamData := tee.Sums()

	// The row is about to record these bytes as its device copy, so its mtime is read
	// back AFTER the stream and cross-checked against the length just read: an mtime
	// taken before a ~116 MB read can describe a copy the device has since rewritten,
	// which is the very thing this path exists to notice.
	st, serr := share.Stat(c.SourcePath)
	if serr != nil {
		return nil, fmt.Errorf("%w: re-stat %s: %v", errRestoreDeviceRead, c.SourcePath, serr)
	}
	if st.Size != n {
		return nil, fmt.Errorf("%w: %s changed while it was being read", errRestoreDeviceRead, c.SourcePath)
	}

	src := &restoreSource{
		row:           c,
		path:          tmpPath,
		deviceSHA256:  streamHash,
		deviceSize:    n,
		deviceMtime:   st.Mtime.Unix(),
		archiveSHA256: streamHash,
		archiveSize:   n,
		revised:       revisedExposure(streamHash, streamData, c),
	}

	// The device stream carries the ORIGINAL OBJECT (the device is never touched), so
	// a retargeted frame's restored NAS copy must re-apply the edit before it lands,
	// else it reverts the label AND answers no archive expectation. The re-apply owns
	// its own faithfulness proof and its own at-rest read-back, both anchored to the
	// file in hand — which is what lets it prove a revision the device has grown by a
	// header block, where the row's recorded archive length would refuse the edit with
	// "edited size != original".
	editPath, editHash, rerr := w.reapplyRetargetIfNeeded(cfg, tmpPath, streamHash, streamData, n, &c)
	if rerr != nil {
		return nil, fmt.Errorf("restore re-apply: %w", rerr)
	}
	if editPath != tmpPath {
		*temps = append(*temps, editPath)
		src.path = editPath
		src.archiveSHA256 = editHash
		return src, nil
	}

	// No overlay: this is the one at-rest read-back on the path, and it measures the
	// temp about to be renamed at ITS OWN length against the digest proven of ITS OWN
	// bytes. Measuring it at the row's recorded archive length would hash a prefix of
	// a revised copy and compare it to a whole-file digest, which can never match.
	diskHash, verr := w.lfs.VerifyRead(tmpPath, n)
	if verr != nil || diskHash != streamHash {
		return nil, fmt.Errorf("restore at-rest verify failed: %v", verr)
	}
	return src, nil
}

// adoptDivergedRestore decides whether a restore may put back a device copy whose
// bytes are not the ones the archive expectation names, and returns the source to
// install, or nil to leave the restore unsatisfied and hold.
//
// Installing them rewrites the archive file with content no row is anchored to yet,
// so it commits every row sharing that dest_path — and a row on ANOTHER rig's
// ledger is out of reach of that commit: it would be left anchored to bytes the
// file no longer holds, a permanent critical delete_mismatch on a ledger this write
// never touched. That is the one refusal the invariant of design frame-lifecycle
// §11.1 already makes for the supersede, asked here through the same predicate
// rather than as a second policy for one hazard. A refused restore is simply an
// unsatisfiable one, which holds.
//
// A store read that FAILS is returned rather than turned into a refusal: it says
// nothing about the frame, and concluding from it would hold a perfectly good one
// with a critical alert claiming its device content had changed. The sibling caller
// of this predicate returns it unchanged for the same reason.
func (w *Worker) adoptDivergedRestore(ctx context.Context, f model.Frame, src *restoreSource) (*restoreSource, error) {
	refusal, err := w.supersedeRefusal(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("restore: could not tell whether %s is named by another rig: %w", f.DestPath, err)
	}
	if refusal != "" {
		w.log.Warn("rig: not restoring diverged device bytes over an archive file another rig names",
			"dest", f.DestPath, "path", src.row.SourcePath, "reason", refusal)
		return nil, nil
	}
	w.log.Info("rig: restoring device bytes the archive expectation does not name; its anchors advance with the file",
		"dest", f.DestPath, "path", src.row.SourcePath, "revised", src.revised)
	return src, nil
}

// commitRestore writes what a completed restore proved, which its success path did
// not write at all: both copies now hold the restored bytes, and a row left at the
// pre-revision length would fail every later VerifyRead against a truncated prefix
// while an overlay left at its pre-revision hash would describe bytes the file no
// longer holds (design frame-lifecycle §11.6, §11.2).
//
// The row it advances is the one whose DEVICE copy was read — the requested row in
// every case but a superseded sharer's — and every other row naming the file moves
// on the archive side alone, its own device copy never having been read.
//
// A failed commit is returned rather than swallowed: the caller must not clear the
// missing mark and the operator's request over rows that do not describe the file.
func (w *Worker) commitRestore(ctx context.Context, f model.Frame, src *restoreSource) error {
	// Uncancellable for the reason the supersede's commit is: the bytes are already
	// at the canonical name, and a cancel arriving here would leave every row naming
	// a file that no longer holds what they say it does.
	commitCtx := context.WithoutCancel(ctx)
	read, sharers, err := w.store.CommitRestore(commitCtx, store.ArchiveWrite{
		RigID:         w.profileID,
		DestPath:      f.DestPath,
		FrameID:       src.row.ID,
		SHA256:        src.deviceSHA256,
		Size:          src.deviceSize,
		Mtime:         src.deviceMtime,
		ArchiveSHA256: src.archiveSHA256,
		ArchiveSize:   src.archiveSize,
		// The overlay is an audit record of what the archive file holds and this write
		// rewrote that file, so the record follows it. The store applies it only to a
		// row that already carries one.
		RetargetSHA256: src.archiveSHA256,
		State:          provenCommitState(src.row.State),
		// The grace clock restarts because the device copy has just been read afresh:
		// a restore writes an archive copy from device bytes read once, so the window
		// restarts before that original becomes deletable.
		PulledAt: w.now().Unix(),
	})
	if err != nil {
		w.log.Error("rig: could not commit a restored frame's anchors", "dest", f.DestPath, "frame", src.row.ID, "error", err)
		return fmt.Errorf("restore commit: %w", err)
	}
	if !read {
		// PruneDeleted runs hourly and a re-file can move a row out from under a
		// restore, so a row that has stopped naming this path is not an error here.
		w.log.Warn("rig: restore found no row to commit; the restored file is in place",
			"dest", f.DestPath, "frame", src.row.ID, "sharers", sharers)
	}
	return nil
}

// errSourceChangedBeforeDelete signals that the source file changed between the
// three-way integrity hashes and the delete itself — a counter-reset run wrote
// new, never-verified content at the same path. We must NOT delete it; the pull
// path's differ branch reconciles the old (NAS-proven) row and pulls the new
// content next cycle.
var errSourceChangedBeforeDelete = errors.New("source changed between verify and delete")

// deviceDeleteFrame removes the frame and only its recorded sidecars whose
// extension is a delete_sidecar_extension, each re-stat'd within ±2s immediately
// before removal so a replaced sidecar is left alone (design frame-lifecycle §9).
// deviceDeleteFrame returns deleted=true only when it actually removed the source;
// deleted=false with a nil error means the source had already vanished post-verify
// (nothing to remove), which the caller records as source_vanished rather than a
// real device delete so the metric does not overcount.
func (w *Worker) deviceDeleteFrame(ctx context.Context, cfg *model.Config, share device.Share, f model.Frame) (deleted bool, err error) {
	// Final re-stat immediately before the delete — the hashes above took seconds
	// (a full ~116 MB device re-read plus the NAS re-hash), during which a
	// counter-reset capture run could have overwritten this exact path with a new,
	// never-verified frame. Deleting that would lose it. Mirror the pull path's
	// post-stream re-stat and the sidecar re-stat discipline: if the source no
	// longer matches what we verified, refuse to delete and let the pull path
	// reconcile. A NotFound here means it vanished after we proved the NAS copy —
	// safe to record deleted (nothing to remove).
	st, serr := share.Stat(f.SourcePath)
	if serr != nil {
		if device.IsNotFound(serr) {
			return false, nil // vanished post-verify; NAS copy proven → record deleted, but nothing was removed
		}
		return false, serr
	}
	if st.Size != f.Size || !clock.FromOrigin(st.Mtime).EqualWithin(clock.FromOrigin(time.Unix(f.Mtime, 0)), mtimeTol) {
		return false, errSourceChangedBeforeDelete
	}

	if err := share.Delete(f.SourcePath); err != nil {
		return false, err
	}
	w.sweepAppleDoubleTwin(share, f.SourcePath)
	for _, sc := range f.Sidecars {
		if !filter.ExtMatches(sc.Path, cfg.DeleteSidecarExtensions) {
			continue
		}
		st, err := share.Stat(sc.Path)
		if err != nil {
			if !device.IsNotFound(err) {
				w.log.Debug("rig: sidecar stat failed; leaving alone", "path", sc.Path, "error", err)
			}
			continue
		}
		if st.Size != sc.Size || !clock.FromOrigin(st.Mtime).EqualWithin(clock.FromOrigin(time.Unix(sc.Mtime, 0)), mtimeTol) {
			w.log.Info("rig: sidecar replaced since filing; leaving alone", "path", sc.Path)
			continue
		}
		if err := share.Delete(sc.Path); err != nil {
			w.log.Debug("rig: sidecar delete failed", "path", sc.Path, "error", err)
			continue
		}
		w.sweepAppleDoubleTwin(share, sc.Path)
	}
	return true, nil
}

// sweepAppleDoubleTwin removes the AppleDouble companion ("._<name>") a macOS
// finder leaves beside a file it has browsed, for a file this pass just deleted.
// Those twins are already junk to every read path (filter.IsJunkFile), so they
// are never pulled, never paired, and never recorded as sidecars — which means
// nothing else would ever remove them and they accumulate on the device forever.
//
// Scope is deliberately narrow: only the twin of a file whose parent frame was
// verified byte-for-byte and deleted on this same pass. An AppleDouble file
// carries Finder metadata, never image data, so removing one alongside its
// already-proven parent risks nothing — but the delete pass does not go hunting
// for junk it cannot tie to a frame it owns. Orphan twins whose parent is long
// gone are left alone here.
//
// Best-effort throughout: a missing twin is the normal case, and a read-only
// root or a failed delete must never fail the frame's deletion, which has
// already succeeded.
func (w *Worker) sweepAppleDoubleTwin(share device.Share, sourcePath string) {
	dir, base := path.Split(sourcePath)
	twin := dir + "._" + base
	if !filter.IsJunkFile(path.Base(twin)) {
		// Defensive: the name we built must classify as junk, or we are about to
		// delete something that is not an AppleDouble twin.
		return
	}
	if _, err := share.Stat(twin); err != nil {
		return // absent (the common case) or unreadable — either way, leave it
	}
	if err := share.Delete(twin); err != nil {
		w.log.Debug("rig: AppleDouble twin delete failed; leaving alone", "path", twin, "error", err)
		return
	}
	w.log.Debug("rig: swept AppleDouble twin alongside its deleted parent", "path", twin)
}

// deviceDigests is what one streamed pass over a device copy yields: the
// whole-file hash the delete pass authorizes on, and the exposure identity teed
// off the very same bytes. They travel together so that a caller holding one
// cannot be tempted to open the file again for the other — no consumer of a data
// unit makes a second whole-file pass (design frame-lifecycle §11.3).
type deviceDigests struct {
	whole string
	data  string
}

// hashTarget is where a delete slice's pre-delete digests are measured: the
// cycle's site, which also admits the slice's deletes, and the device address
// and login its requests name. deletePhase builds it once the slice's session
// has confirmed identity and passes it down the slice; hashDevice cannot
// measure without one.
type hashTarget struct {
	site    netsite.Site
	ip      string
	profile *model.RigProfile
}

// hashDevice measures the device copy's digests once, through t's site, and
// returns both — the independent device read of the delete-pass tie-breaker
// (design frame-lifecycle §9), with the data unit teed off it for the
// backfill's device-side proof (design frame-lifecycle §11.8). The site either
// runs streamDeviceDigests, this session's own read, or measures the file in a
// session of its own; it never compares.
//
// What comes back is judged only by its fields. A measurement that probed
// identity in its own session must have reached this rig's device: a different
// device is refused as the delete session's foreign probe is, and no answer as
// its unconfirmed one is, each with errHashIdentity (errHashUnconfirmed for no
// answer), which ends the slice. A
// data-unit digest this build's algorithm did not compute is dropped, so no
// consumer records it.
//
// Pause and ctx ending stop the measurement with errHashInterrupted (design
// frame-lifecycle §12.8). Both callers defer on any error before
// comparing anything, so an abandoned measurement is only ever a deferral; the
// delete itself is never interrupted.
func (w *Worker) hashDevice(ctx context.Context, t hashTarget, share device.Share, path string) (deviceDigests, error) {
	stop := func() bool { return ctx.Err() != nil || w.pauseRequested.Load() }
	req := netsite.DigestRequest{
		IP: t.ip, Share: share.Name(), Path: path, Stop: stop,
		Local: func() (netsite.Digests, error) { return streamDeviceDigests(share, path, stop) },
	}
	if t.profile != nil {
		req.User, req.Password = netsite.Credential(t.profile.User), netsite.Credential(t.profile.Password)
	}
	res, err := t.site.MeasureDigests(ctx, req)
	if errors.Is(err, netsite.ErrStopped) {
		return deviceDigests{}, errHashInterrupted
	}
	if err != nil {
		return deviceDigests{}, err
	}
	if res.Probed {
		switch confirmed, foreign := hashIdentity(t.profile, res.GUID, res.CPUID); {
		case foreign:
			w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedHashIdentityMismatch)
			w.log.Warn("rig: the device measured for a delete reported a different identity; ending the delete slice",
				"path", path, "guid", res.GUID)
			w.refuseForeignDevice(ctx, foreignAtHash)
			return deviceDigests{}, errHashIdentity
		case !confirmed:
			w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedHashIdentityUnconfirmed)
			w.log.Warn("rig: the device measured for a delete did not confirm its identity; ending the delete slice",
				"path", path)
			return deviceDigests{}, errHashUnconfirmed
		}
	}
	data := res.Data
	if !res.DataRecordable {
		// A data unit from another algorithm would corrupt the exposure identity
		// it was recorded as; the whole-file digest the compare uses is unaffected.
		w.log.Info("rig: data-unit digest measured by a different algorithm; not recording the frame's exposure identity",
			"path", path)
		data = ""
	}
	return deviceDigests{whole: res.Whole, data: data}, nil
}

// streamDeviceDigests streams the device copy once in this session and returns
// both digests through header.StreamDigests, the read a remote's measurement
// runs too. The tee costs no extra read and no extra open: it is fed the bytes
// already passing. The stream stops between buffers with errHashInterrupted once
// stop reports true, so a Pause waits out one buffer rather than the whole file.
func streamDeviceDigests(share device.Share, path string, stop func() bool) (netsite.Digests, error) {
	rc, err := share.OpenReader(path)
	if err != nil {
		return netsite.Digests{}, err
	}
	defer rc.Close()
	whole, data, err := header.StreamDigests(rc, stop, nil)
	if errors.Is(err, header.ErrStreamStopped) {
		return netsite.Digests{}, errHashInterrupted
	}
	if err != nil {
		return netsite.Digests{}, err
	}
	return netsite.Digests{Whole: whole, Data: data}, nil
}

// hashIdentity judges the identity a digest measurement probed in its own
// session as confirmDeviceIdentity judges the delete session's probe: no
// answer is unconfirmed, a host-bound rig (no guid yet) trusts the answer as
// its delete session already did, and a bound rig needs identityMatches. With
// no profile there is nothing to compare against, so it fails closed.
func hashIdentity(profile *model.RigProfile, guid, cpuID string) (confirmed, foreign bool) {
	if profile == nil || (guid == "" && cpuID == "") {
		return false, false
	}
	if profile.GUID == "" || identityMatches(profile, guid, cpuID) {
		return true, false
	}
	return false, true
}

// errHashIdentity is hashDevice's refusal of a measurement whose own identity
// probe did not confirm this rig's device; it has already been counted, and a
// different device alerted.
var errHashIdentity = errors.New("device measured for a delete is not confirmed as this rig's")

// errHashUnconfirmed is errHashIdentity for a measurement whose probe got no
// answer, which the slice reports as unconfirmed rather than as progress.
var errHashUnconfirmed = fmt.Errorf("%w: its identity probe got no answer", errHashIdentity)

// foreignSource is where a different device's identity was seen, which decides
// what the refusal covers.
type foreignSource int

const (
	// foreignAtSession: the delete session's own probe, which is Commander's;
	// pulls are refused on the same evidence.
	foreignAtSession foreignSource = iota
	// foreignAtHash: the probe a site's digest measurement ran in its own
	// session. Only deletes are refused: pulls resume the next cycle once
	// Commander's own probe confirms the device.
	foreignAtHash
)

// refuseForeignDevice is the foreign-identity block: the alert, and the error
// state with an "identity"-bearing LastError, published at once so the
// /identity banner reflects the mismatch this same cycle instead of lagging one
// pull behind, as the pull phase does for its own probe.
func (w *Worker) refuseForeignDevice(ctx context.Context, src foreignSource) {
	w.raiseIdentityMismatch(ctx)
	w.setState(model.RigStateError)
	w.st.LastError = "identity mismatch: a different device answered at this address; refusing pulls and deletes"
	if src == foreignAtHash {
		w.st.LastError = "identity mismatch: the device measured for a delete reported a different identity; refusing its deletes"
	}
	w.setSafeVerdict(model.SafeUnknown, "identity")
	w.endRunForIdentity()
	w.publish()
}

// errHashInterrupted is hashDevice's error for a stream abandoned for a Pause or
// for ctx ending; callers tell it apart from a transient read error only to log
// and count it.
var errHashInterrupted = errors.New("device re-hash interrupted")

// hashInterruptedMsg names what interrupted a device re-hash: ctx ending is a
// shutdown, anything else is the Pause that set pauseRequested.
func hashInterruptedMsg(ctx context.Context) string {
	if ctx.Err() != nil {
		return "rig: device re-hash interrupted by shutdown; deferring"
	}
	return "rig: device re-hash interrupted by pause; deferring"
}

// maxDeleteDefers bounds how many consecutive delete passes may decline to
// conclude anything about a frame whose source no longer agrees with its row
// (design frame-lifecycle §11.13). The pass after the budget
// is spent falls back to the ordinary outcome.
//
// The value has to clear the wait the pull path owes that same frame, because a
// revision is resolved by the PULL: deletePhase runs every cycle while the pull
// waits out min_file_age plus settle_cycles, which at the shipped 45s poll and
// 300s age is nine or so passes before the supersede can even be attempted. 20
// doubles that, so a rig whose min_file_age is raised well past the default still
// resolves inside the budget, while a source that is genuinely changed rather
// than mid-revision is reported within a quarter of an hour of device time.
const maxDeleteDefers = 20

// deletePressureFactor sets the delete-pressure threshold as a multiple of
// disk_reserve_bytes: below it the device is tight and delete slices get a fair
// share of link time with carried debt (design frame-lifecycle §12.1,
// design frame-lifecycle §12.2). The reserve is where DeviceDiskLow fires, so
// deletes should already have their fair share before that point, not only from
// it. It is a scheduling bias only; no safety decision reads it.
const deletePressureFactor = 2

// deferChangedSource is the pre-delete gate's outcome for a source that no longer
// matches the row (design frame-lifecycle §11.13). It hashes
// nothing: the gate sits before the ~116 MB device hash precisely so a frame the
// device is still revising never re-streams its copy every pass, and the proving
// belongs to the pull path's differ branch, the only place holding both copies.
//
// Deferring silently forever is not on offer either — the device original would
// never drain and the power-off verdict would never clear — so the budget is
// bounded, and a source still changed once it is spent has stopped looking like
// one mid-revision and earns the hold a first sighting is too early for.
func (w *Worker) deferChangedSource(ctx context.Context, f model.Frame) {
	if f.DeleteDeferCount >= maxDeleteDefers {
		w.holdFrame(ctx, f, model.HoldSourceChanged, "source present but size/mtime changed since filing")
		return
	}
	// The count is read off the row rather than tracked in memory so a rig that
	// restarts more often than the budget still escalates, and advanced in SQL so a
	// reset committed by the pull path in the meantime is never written back.
	if err := w.store.DeferDelete(ctx, f.ID); err != nil {
		w.log.Warn("rig: deferral counter write failed; deferring delete", "path", f.SourcePath, "error", err)
		return
	}
	// A quiet label, not the held one: source_changed is a held frame plus a
	// critical alert, and a deferral is neither. Its own label rather than the
	// pre-delete race's, which fires at most once per frame where this one fires
	// on every pass within the bound: sharing a series would make one deferring
	// frame read as a score of benign races.
	w.metrics.RecordDeleteBlocked(w.profileID, metrics.BlockedSourceChangedDeferred)
	w.log.Warn("rig: source present but changed since filing; deferring delete",
		"path", f.SourcePath, "deferred", f.DeleteDeferCount+1, "bound", maxDeleteDefers)
}

// holdFrame moves a frame to held with an alert — a delete-time integrity
// mismatch is kept, never deleted (design frame-lifecycle §9).
func (w *Worker) holdFrame(ctx context.Context, f model.Frame, reason model.HoldReason, msg string) {
	f.State = model.StateHeld
	f.Error = msg
	if err := w.store.UpdateFrame(ctx, &f); err != nil {
		w.log.Error("rig: hold frame update failed", "path", f.SourcePath, "error", err)
	}
	w.metrics.RecordDeleteBlocked(w.profileID, holdReasonToBlocked(reason))
	w.log.Warn("rig: holding frame (delete-pass mismatch)", "path", f.SourcePath, "reason", string(reason), "detail", msg)
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityCritical,
		Category: model.AlertDeleteMismatch,
		Message:  "delete-pass integrity check failed for " + f.SourcePath + ": " + msg,
	})
}

// holdReasonToBlocked maps the held-frame reason to its bounded metric label.
func holdReasonToBlocked(r model.HoldReason) metrics.DeleteBlockedReason {
	switch r {
	case model.HoldSourceChanged:
		return metrics.BlockedSourceChanged
	case model.HoldThreeWayMismatch:
		return metrics.BlockedThreeWayMismatch
	case model.HoldRestoreMismatch:
		return metrics.BlockedRestoreMismatch
	default:
		return metrics.BlockedSourceChanged
	}
}

// markDeleted records a completed deletion. manner distinguishes a real device
// delete from a source that had already vanished (no delete was issued). It reports
// whether the store actually persisted state='deleted', so a caller that follows up
// with dependent writes (the culled-frame resolve, design library-reconciliation
// §10.6) can skip them on failure.
func (w *Worker) markDeleted(ctx context.Context, f model.Frame, manner metrics.DeleteManner) bool {
	f.State = model.StateDeleted
	f.DeletedAt = w.now().Unix()
	if err := w.store.UpdateFrame(ctx, &f); err != nil {
		w.log.Error("rig: mark-deleted update failed", "path", f.SourcePath, "error", err)
		return false
	}
	w.metrics.RecordFrameDeleted(w.profileID, manner)
	// Emit only after the DB genuinely records StateDeleted, so the live feed's row
	// flips to "Deleted" in lockstep with the store (design features §4.4). Emitting
	// before the update succeeded would show "Deleted" while the row still reads
	// verified, reverting on reload. holdFrame (StateHeld) deliberately emits
	// nothing.
	w.hub.Emit(hub.Event{Name: "frame.deleted", Data: f})
	return true
}

// mediaConfirmedForDelete reports whether a share's medium is confirmed PRESENT
// for cleanup: a fixed medium always is; a removable Udisk requires its media
// marker to be readable (adoption and any relocation happen in the pull phase,
// not here), so an absent stick — or a swapped-in one carrying no marker — never
// authorizes deletions (design frame-lifecycle §3, H5/H10). It reads through the
// shared READ-ONLY resolver — ASIAIR/ first, then the legacy root only on
// not-found — so the delete pass never writes a marker. A legacy root marker is
// honored here but relocated only by the pull phase.
//
// Deliberately readable-only, NOT an identity check: it does not compare the
// marker against the persisted media id, so a swapped stick carrying any readable
// marker passes this gate (design device-identity-and-media §6.4:
// the delete pass needs the marker only to be readable). That is sound because
// the real swap backstop is the three-way content hash plus the ±2s source
// re-stat in deleteOne, which the identity docs call the true delete backstop
// with the marker as defense-in-depth (design delete-safety §4). Do not read the
// pull phase's verdict as covering this: a cycle whose pull judged the medium
// Foreign still runs its delete pass, because `refused` (worker.go) is set only
// by device-identity and session failures.
func (w *Worker) mediaConfirmedForDelete(share device.Share) bool {
	if !isRemovableShare(share.Name()) {
		return true
	}
	_, _, err := readMediaMarker(share)
	return err == nil
}

// dirOf returns the parent directory of a host-native local path (dest_path is
// filed with the OS separator, so filepath.Dir splits it correctly on every OS).
func dirOf(p string) string {
	return filepath.Dir(p)
}
