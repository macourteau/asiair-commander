package rig

import (
	"context"
	"errors"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
)

// decide answers the two decide-to-pull questions for one listed frame (design
// frame-lifecycle §4): have we already got this file, and — via the differ branch
// — is the old row safely resolved before new content overwrites it. It returns
// whether to pull and the prior DB row (nil if none), which the filing step uses
// to preserve pulled_at on an idempotent keep.
//
// Quiescence (min_file_age AND settle) is a SEPARATE gate applied by the caller
// only to frames this returns pull=true for.
func (w *Worker) decide(ctx context.Context, cfg *model.Config, profile *model.RigProfile, lf listedFrame) (pull bool, prior *model.Frame) {
	row, ok, err := w.store.GetFrame(ctx, w.profileID, lf.share, lf.path)
	if err != nil {
		w.log.Warn("rig: decide lookup failed; skipping frame this cycle", "path", lf.path, "error", err)
		return false, nil
	}
	if !ok {
		// No row — a fresh frame, or the whole DB was lost. Pull; the idempotent
		// keep at filing reconciles a still-present NAS copy (design frame-lifecycle
		// §4, design architecture §7).
		return true, nil
	}
	prior = row

	// A frame the operator culled on the NAS (marked missing by the library scan)
	// is never re-pulled while the device still holds the same-size content, in any
	// state except failed. This gate is on SIZE, before the mtime compare, so a
	// different card reader or a FAT mtime shift cannot re-pull it — only genuinely
	// new content (a different size) falls through to the differ branch, which pulls
	// it as the new frame it is (design library-reconciliation §4.4).
	if row.MissingSeenAt != 0 && row.State != model.StateFailed && row.Size == lf.size {
		return false, prior
	}

	// Compare against the RAW stored size + mtime with the ±2s tolerance used
	// everywhere (FAT granularity + clock-offset jitter). An exact compare would
	// re-pull an unchanged file every cycle and stall the drain (design
	// frame-lifecycle §4).
	if sameListedContent(row, lf.size, lf.mtime) {
		switch row.State {
		case model.StateVerified, model.StateUnclassified, model.StateHeld, model.StateDismissed:
			// A settled state: already have it (a dismissed row is a tombstone whose
			// unchanged corrupt source is deliberately never re-pulled), skip.
			return false, prior
		case model.StateFailed:
			if w.failedDue(cfg, row) {
				w.log.Info("rig: retrying failed frame", "path", lf.path, "attempts", row.Attempts)
				return true, prior
			}
			return false, prior
		case model.StateDeleted:
			// The source reappeared with the same content as a frame we already
			// pulled and deleted; re-adopt it (the idempotent keep matches the NAS
			// copy if still present).
			return true, prior
		default:
			return false, prior
		}
	}

	// DIFFER: the device reused this path with new content (a counter-reset run
	// overwrote the file on FAT). Resolve the OLD row by proof before pulling the
	// new content, which overwrites the row.
	proceed := w.resolveDiffer(ctx, row)
	return proceed, prior
}

// sameListedContent is decide's "have we already got this file" compare: the
// RAW stored size and mtime against the listed ones, the mtime within mtimeTol.
// ArchivedCopy (harvestview.go) shares it so the tool and the puller agree on
// what "the same device file" means.
func sameListedContent(row *model.Frame, size int64, mtime time.Time) bool {
	dtNew := clock.FromOrigin(mtime)
	dtOld := clock.FromOrigin(time.Unix(row.Mtime, 0))
	return row.Size == size && dtNew.EqualWithin(dtOld, mtimeTol)
}

// failedDue reports whether a failed frame's backoff has elapsed and it has not
// exhausted its attempt budget (design frame-lifecycle §4.3). A frame at
// max_attempts stays failed rather than being hammered every cycle; only an
// operator's Run now brings it back under the budget (grantFailedRetry).
func (w *Worker) failedDue(cfg *model.Config, row *model.Frame) bool {
	fr := cfg.Defaults.FailedRetry
	if fr.MaxAttempts > 0 && row.Attempts >= fr.MaxAttempts {
		return false
	}
	nowUnix := w.now().Unix()
	return row.NextRetryAt == 0 || row.NextRetryAt <= nowUnix
}

// grantFailedRetry gives each failed frame on this rig that has used up its
// attempt budget one more attempt, due at once, when the operator presses Run now
// (design frame-lifecycle §4.3). The grant is persisted rather than held for one
// cycle, so it survives the settle gate and admission deferring the attempt to a
// later cycle; one failure exhausts the budget again, so a frame that keeps
// failing costs one attempt per press and never loops. A store error only logs:
// the press still runs its cycle.
func (w *Worker) grantFailedRetry(ctx context.Context) {
	maxAttempts := w.cfg.Load().Defaults.FailedRetry.MaxAttempts
	if maxAttempts <= 0 {
		return // an unlimited budget never exhausts
	}
	n, err := w.store.GrantFailedRetry(ctx, w.profileID, maxAttempts)
	if err != nil {
		w.log.Warn("rig: run-now could not grant exhausted failed frames a retry", "error", err)
		return
	}
	if n > 0 {
		w.log.Info("rig: run-now grants exhausted failed frames one more attempt", "frames", n)
	}
}

// resolveDiffer proves the fate of the old DB row whose source path is being
// overwritten by new content (design frame-lifecycle §4 differ branch, mirroring
// design frame-lifecycle §9's doubly-vanished rule). It returns whether the
// caller should proceed to pull the new content this cycle; a transient
// VerifyRead error defers the pull so the old row is not overwritten while its
// fate is unproven.
func (w *Worker) resolveDiffer(ctx context.Context, row *model.Frame) bool {
	// A row marked missing reaches the differ branch only with a size change, i.e.
	// the device wrote genuinely new content at a path whose NAS copy the operator
	// deleted. That is not a lost frame — pull the new content and let the file step
	// re-record the row; UpsertFrame clears the stale mark. No frame_lost alert.
	if row.MissingSeenAt != 0 {
		return true
	}
	switch row.State {
	// held belongs in the arm that LOOKS rather than one that concludes from the
	// state flag: a held row with missing_seen_at == 0 is exactly what holdFrame
	// leaves behind, so a frame the device merely revised reaches this branch on
	// the next listing, and alerting from the flag alone would raise a critical
	// frame_lost over an archive copy that is perfectly intact. A held row's copy
	// is the suspect one, which is a reason to verify it rather than a proof about
	// it — nothing is concluded from an error (design library-reconciliation §2.1,
	// design frame-lifecycle §11.6).
	case model.StateVerified, model.StateUnclassified, model.StateHeld:
		if row.SHA256 == "" || row.DestPath == "" {
			// A row that never recorded a good copy: genuinely lost.
			w.alertFrameLost(ctx, row, "overwritten source with no recorded NAS copy", model.SeverityCritical)
			return true
		}
		hash, err := w.lfs.VerifyRead(row.DestPath, row.ArchiveSize)
		if err != nil {
			if isNotExist(err) {
				// Confirmed missing NAS copy: the frame is genuinely lost.
				w.alertFrameLost(ctx, row, "overwritten source and NAS copy is missing", model.SeverityCritical)
				return true
			}
			if errors.Is(err, localfs.ErrShortFile) {
				// Truncated NAS copy with the source already overwritten: genuinely
				// lost, not a transient error to retry forever.
				w.alertFrameLost(ctx, row, "overwritten source and NAS copy is truncated", model.SeverityCritical)
				return true
			}
			// Transient read error (error != vanished): retry next cycle, do not
			// pull over the row yet, and do not alert-as-lost.
			w.log.Warn("rig: differ-branch NAS verify transient error; deferring", "dest", row.DestPath, "error", err)
			return false
		}
		if hash == archiveHash(*row) {
			// The old copy is safely on the NAS; the source merely got overwritten. The
			// archive-side compare uses archiveHash so a retargeted copy (whose bytes hash
			// to retarget_sha256, not sha256) is not mistaken for a lost frame (design
			// routing-and-filing §9.5).
			w.log.Info("rig: differ-branch old copy intact on NAS (benign overwrite)", "dest", row.DestPath)
			return true
		}
		// Confirmed corrupt NAS copy: genuinely lost.
		w.alertFrameLost(ctx, row, "overwritten source and NAS copy is corrupt", model.SeverityCritical)
		return true
	case model.StateFailed:
		// A failed row never had a good copy — a different claim from a held one,
		// and one no read can settle: there is no archive copy to verify.
		w.alertFrameLost(ctx, row, "overwritten source of a frame that never had a proven NAS copy", model.SeverityCritical)
		return true
	case model.StateDismissed:
		// A changed source at a dismissed path is re-pulled; the NAS copy was
		// deliberately purged, so there is nothing to prove and no frame_lost.
		return true
	default: // deleted or unknown
		return true
	}
}

// quiescent applies the two-part quiescence gate to a pull candidate (design
// frame-lifecycle §4): the offset-corrected mtime must be at least min_file_age
// old AND the size+mtime must have held across settle_cycles consecutive
// observations. A still-writing frame fails at least one. It observes the file
// every call so the settle counter accumulates across cycles even while the file
// is still young.
func (w *Worker) quiescent(cfg *model.Config, profile *model.RigProfile, lf listedFrame, cl *clock.Clock) bool {
	dt := clock.FromOrigin(lf.mtime)
	stable := w.settle.observe(settleKey(lf), lf.size, dt)
	if cl.YoungerThan(dt, cfg.EffectiveMinFileAge(profile)) {
		return false
	}
	return stable >= cfg.EffectiveSettleCycles(profile)
}

// settleKey is the per-file settle-tracker key.
func settleKey(lf listedFrame) string { return lf.share + "\x00" + lf.path }

// alertFrameLost raises the loud "frame genuinely lost" alert (design
// frame-lifecycle §4, design frame-lifecycle §9, design architecture §12) — the
// one path that actually loses data must never hide behind a benign outcome.
func (w *Worker) alertFrameLost(ctx context.Context, row *model.Frame, reason string, sev model.Severity) {
	// The log level follows the severity: a both-sides cull (warn) is routine
	// cleanup, not the loud data-loss event a critical frame_lost is.
	if sev == model.SeverityCritical {
		w.log.Error("rig: frame lost", "path", row.SourcePath, "dest", row.DestPath, "reason", reason)
	} else {
		w.log.Warn("rig: frame gone from both sides", "path", row.SourcePath, "dest", row.DestPath, "reason", reason)
	}
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: sev,
		Category: model.AlertFrameLost,
		Message:  "frame lost (" + reason + "): " + row.SourcePath,
	})
}

// safelyOnNAS reports whether a frame in this state means powering the device off
// would lose nothing worth preserving: verified is proven, an unclassified frame
// was verified at rest before being filed to the quarantine area, and a dismissed
// frame was deliberately abandoned (its NAS copy purged) so nothing remains to
// keep. held (a delete-time mismatch left the NAS copy suspect) and failed (the
// pull never landed) do not qualify. Returning true for dismissed keeps a
// still-on-device dismissed frame — skipped by decide() every cycle — from pinning
// pendingWork > 0 forever and freezing the rig at Pulling/Working.
func safelyOnNAS(s model.FrameState) bool {
	return s == model.StateVerified || s == model.StateUnclassified || s == model.StateDismissed
}
