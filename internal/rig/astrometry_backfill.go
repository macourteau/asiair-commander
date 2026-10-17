package rig

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	// backfillPageSize is the query page for the id-cursor walk.
	backfillPageSize = 256
	// backfillYield is the inter-read pause so the stream of dest_path reads — which
	// land on the same NAS storage live pulls write to — cannot starve them.
	backfillYield = 3 * time.Millisecond
	// backfillProgressEvery logs a progress line every N processed frames.
	backfillProgressEvery = 500
)

// BackfillAstrometry populates astrometry for already-filed frames that predate the
// feature (wcs_status IS NULL), once, in the background. It is display-only, so its
// ordering relative to the workers does not matter; the whole pass is idempotent and
// resumable across restarts because the NULL guard makes each frame processed at
// most once.
//
// It walks by an advancing id cursor rather than "loop until a batch is empty": a
// frame whose file is currently unreachable is deliberately left NULL, and a
// batch-empties termination would re-fetch those unresolved rows forever, spinning a
// hot os.Stat loop. Advancing past every attempted row guarantees a bounded single
// pass; the still-NULL transient rows are retried on the next startup (a fresh cursor
// from 0).
func BackfillAstrometry(ctx context.Context, st store.Store, hdr header.Reader, nudge chan<- int64, logger *slog.Logger) {
	logger.Info("astrometry backfill: starting")
	var afterID int64
	var processed, solutions, none, skipped int

	for {
		if ctx.Err() != nil {
			logger.Info("astrometry backfill: cancelled", "processed", processed)
			return
		}
		frames, err := st.ListFramesNeedingAstrometry(ctx, afterID, backfillPageSize)
		if err != nil {
			logger.Error("astrometry backfill: list failed; stopping", "processed", processed, "error", err)
			return
		}
		for i := range frames {
			f := &frames[i]
			afterID = f.ID // advance past every attempted row, whatever the outcome

			// Classify failure by reachability, not error string: a stat failure of any
			// kind (missing file, not-ready share, stale mount) is transient — skip
			// without writing so it retries next startup. Only a header read that fails
			// on a file that DID stat is a deterministic parse error → wcs_status='none'.
			if _, statErr := os.Stat(f.DestPath); statErr != nil {
				skipped++
				continue
			}

			a := computeAstrometry(hdr, f.DestPath)
			if err := st.SetFrameAstrometryIfPending(ctx, f.ID, a); err != nil {
				logger.Warn("astrometry backfill: write failed", "id", f.ID, "error", err)
				continue
			}
			processed++
			if a.Solution != nil {
				solutions++
			} else {
				none++
				// A light this backfill just marked `none` is a Tier-1 candidate — nudge
				// the solver. Nudging even a no-hint light matters: the wakeup re-drain
				// marks it `unsolvable` ("cannot solve") promptly without invoking astap,
				// rather than leaving it blank until the periodic re-sweep. On first run
				// these usually land while the solve worker is still blocked on the catalog
				// download and are dropped, which is harmless — the from-zero re-sweep picks
				// them up once the catalog lands.
				nudgeSolveCandidate(nudge, f)
			}
			if processed%backfillProgressEvery == 0 {
				logger.Info("astrometry backfill: progress", "processed", processed, "solutions", solutions)
			}
			time.Sleep(backfillYield)
		}
		if len(frames) < backfillPageSize {
			break
		}
	}
	logger.Info("astrometry backfill: complete",
		"processed", processed, "solutions", solutions, "none", none, "transient_skipped", skipped)
}

// nudgeSolveCandidate sends a non-blocking solve nudge for a light frame the
// backfill just marked `none`. It mirrors the ingest hook (maybeEnqueueSolve),
// which also nudges every none light: a no-hint frame is dispositioned unsolvable
// cheaply (no astap call), so nudging it makes "cannot solve" appear promptly.
func nudgeSolveCandidate(nudge chan<- int64, f *model.Frame) {
	if nudge == nil || f.FrameType != string(model.TypeLight) {
		return
	}
	select {
	case nudge <- f.ID:
	default:
	}
}

// computeAstrometry reads the header (the file has already been confirmed to stat)
// and extracts its astrometry. A read failure here is a genuine parse error
// (truncated, non-FITS, no END card) — deterministic — so it records "none" rather
// than leaving the row NULL to be retried forever.
func computeAstrometry(hdr header.Reader, path string) *model.Astrometry {
	h, err := hdr.ReadHeader(path)
	if err != nil {
		return &model.Astrometry{Status: model.AstroStatusNone}
	}
	return astrometry.FromHeader(h)
}
