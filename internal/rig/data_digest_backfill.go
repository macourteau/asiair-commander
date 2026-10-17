package rig

import (
	"context"
	"log/slog"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	// dataDigestPageSize is the query page for the id-cursor walk.
	dataDigestPageSize = 256
	// dataDigestDutyCycle is the share of wall-clock the cursor may spend reading:
	// after each archive pass it sleeps for that read's own duration scaled so the
	// job can never take more than this fraction of the destination volume's read
	// capacity, whatever that volume turns out to be.
	//
	// Pacing by duty cycle rather than by a fixed pause is what makes the budget a
	// promise instead of a guess, and it is why the Tier-0 astrometry backfill's
	// 3ms yield is deliberately not reused: that value is calibrated for 2880-byte
	// header reads, and beside a ~116 MB cache-bypassing whole-file pass it is
	// indistinguishable from no pacing at all.
	dataDigestDutyCycle = 0.1
	// dataDigestMinPause keeps a page of rows whose files are absent or tiny from
	// spinning the cursor: a duty cycle scaled off a read that took no measurable
	// time yields no pause at all.
	dataDigestMinPause = 50 * time.Millisecond
	// dataDigestProgressEvery logs a progress line every N recorded digests.
	dataDigestProgressEvery = 100
)

// BackfillDataDigests records data_sha256 for rows that have none (design
// frame-lifecycle §11.8). Every such row carries no exposure identity, so the
// revision discriminator's second leg cannot be established for it — and a row
// already held by a revision raises a critical frame_lost on its next listing.
// The backfill is what extends revision handling to those rows.
//
// A row is backfilled ONLY from a copy proven to be the archived bytes: here, an
// archive copy that still hashes to archive_sha256. Without that condition a
// rotted file's data unit would be recorded as authoritative and the rot blessed
// permanently — a worse failure than the one the design fixes. The device-side
// proof, which is what lets a culled row be backfilled at all, rides the delete
// pass's own stream (handleAbsentNAS) and is not reachable from here: no cursor
// opens a device session, which is what bounds the device source to the rows the
// culled-original drain passes through.
//
// It runs on a BUDGET rather than on a population argument, because there is no
// population argument to make: "only a row whose device original still exists"
// bounds nothing on a rig with keep_originals set, which skips the delete pass
// entirely so every row it has ever filed still has a live original, and
// PruneDeleted prunes only deleted rows, so the set is not seven-day-bounded
// either. Unbudgeted, this job is the whole library read as uncached O_DIRECT
// passes against the volume live pulls are writing to.
//
// There is deliberately no ceiling on the pause a slow read earns. A pathological
// volume simply makes the walk take proportionally longer, which costs nothing:
// a row needed before the cursor reaches it has its value computed on demand under
// the same proof (provenRevisionOf), so nothing depends on this finishing.
//
// Like the Tier-0 backfill it walks by an advancing id cursor rather than
// looping until a batch empties: a row whose archive copy is unreadable is
// deliberately left without a value, and a batch-empties termination would
// re-fetch those rows forever.
func BackfillDataDigests(ctx context.Context, st store.Store, lfs localfs.LocalFS, logger *slog.Logger) {
	logger.Info("data digest backfill: starting")
	var recorded, unproven, skipped int

	// Two passes, in the order design frame-lifecycle §11.8 gives: rows already
	// held by a revision first — they are the ones that would otherwise raise a
	// critical frame_lost on upgrade, and the archive copy that arm verifies is
	// the proof this cursor needs — then every other row with an archive copy,
	// then nothing else proactively.
	for _, held := range []bool{true, false} {
		var afterID int64
		for {
			if ctx.Err() != nil {
				logger.Info("data digest backfill: cancelled", "recorded", recorded)
				return
			}
			frames, err := st.ListFramesNeedingDataDigest(ctx, afterID, held, dataDigestPageSize)
			if err != nil {
				logger.Error("data digest backfill: list failed; stopping",
					"held", held, "recorded", recorded, "error", err)
				return
			}
			for i := range frames {
				f := frames[i]
				afterID = f.ID // advance past every attempted row, whatever the outcome
				if f.ArchiveSize <= 0 {
					// Nothing to read a digest out of, and VerifyReadTee would report a
					// short file rather than say so.
					skipped++
					continue
				}
				start := time.Now()
				data, ok := provenArchiveDataDigest(lfs, f, logger)
				if !ok {
					unproven++
				} else if recordDataDigest(ctx, st, logger, f, data) {
					recorded++
					if recorded%dataDigestProgressEvery == 0 {
						logger.Info("data digest backfill: progress", "recorded", recorded)
					}
				}
				if !pauseForBudget(ctx, time.Since(start)) {
					logger.Info("data digest backfill: cancelled", "recorded", recorded)
					return
				}
			}
			if len(frames) < dataDigestPageSize {
				break
			}
		}
	}
	logger.Info("data digest backfill: complete",
		"recorded", recorded, "unproven", unproven, "skipped", skipped)
}

// pauseForBudget spends the cursor's budget for one archive read: it sleeps long
// enough that the read it just made accounts for at most dataDigestDutyCycle of
// the wall-clock since that read began. It reports whether the pause completed —
// false means the context was cancelled and the walk must stop.
func pauseForBudget(ctx context.Context, elapsed time.Duration) bool {
	pause := time.Duration(float64(elapsed) * (1 - dataDigestDutyCycle) / dataDigestDutyCycle)
	if pause < dataDigestMinPause {
		pause = dataDigestMinPause
	}
	t := time.NewTimer(pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// provenArchiveDataDigest reads f's archive copy once and returns its data unit's
// digest, but only when that copy still hashes to the row's archive-side
// expectation. That condition is the whole point of the backfill: a copy that no
// longer answers for its own anchor proves nothing about the exposure, and
// recording its data unit anyway would bless the rot permanently.
//
// The offset comes from a header read and the digests from the one
// cache-bypassing pass VerifyRead already makes, so the copy is read whole exactly
// once — a buffered read would be served from resident pages and prove nothing
// about what the disk holds.
//
// A false answer is never an error to retry in a tight loop: the row is left
// without a value and the next start's fresh cursor tries again.
func provenArchiveDataDigest(lfs localfs.LocalFS, f model.Frame, logger *slog.Logger) (string, bool) {
	off, err := header.DataOffset(f.DestPath)
	if err != nil {
		if !isNotExist(err) {
			logger.Debug("data digest backfill: header unreadable; leaving the row unset",
				"dest", f.DestPath, "error", err)
		}
		return "", false
	}
	onDisk, data, err := lfs.VerifyReadTee(f.DestPath, f.ArchiveSize, off)
	if err != nil {
		logger.Debug("data digest backfill: archive copy unreadable; leaving the row unset",
			"dest", f.DestPath, "error", err)
		return "", false
	}
	if onDisk != archiveHash(f) {
		// Genuine rot, or a file this row has stopped describing. Either way the bytes
		// are not proven to be the archived ones, so nothing about them is recorded —
		// the rot_clash alert exists to surface this, not a silent blessing of it.
		logger.Warn("data digest backfill: archive copy has drifted from its recorded hash; leaving the row unset",
			"dest", f.DestPath, "recorded", archiveHash(f), "onDisk", onDisk)
		return "", false
	}
	if data == "" {
		// The header did not parse, which the digest seam reports as an empty data
		// digest rather than an error. There is no identity to record.
		return "", false
	}
	return data, true
}

// recordDataDigest writes one proven exposure identity, and is the single place
// that decides a row is worth writing at all: a row already carrying a value is
// left alone, and so is an unproven one. It reports whether the write landed.
//
// The store write is a compare-and-set on the anchors the proof was made against,
// so a re-file that lands between the read and the write refuses it rather than
// recording one exposure's identity on another's row.
func recordDataDigest(ctx context.Context, st store.Store, logger *slog.Logger, f model.Frame, data string) bool {
	if f.DataSHA256 != "" || data == "" {
		return false
	}
	ok, err := st.SetFrameDataDigestIfPending(ctx, f.ID, f.SHA256, f.ArchiveSHA256, data)
	if err != nil {
		logger.Warn("could not record a frame's exposure identity", "frame", f.ID, "error", err)
		return false
	}
	if !ok {
		logger.Debug("a frame's anchors moved while its exposure identity was proven; leaving it for the next pass",
			"frame", f.ID)
		return false
	}
	return true
}

// recordProvenDataDigest is the worker's door onto the same write, for the two
// proofs that arise inside a cycle rather than in the cursor: the device copy the
// culled-original drain has just proven byte-identical to what was archived, and
// the archive copy the revision path verifies before superseding it.
func (w *Worker) recordProvenDataDigest(ctx context.Context, f model.Frame, data string) {
	if recordDataDigest(ctx, w.store, w.log, f, data) {
		w.log.Info("rig: recorded the exposure identity of a frame filed before it was tracked",
			"frame", f.ID, "dest", f.DestPath)
	}
}
