package rig

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/macourteau/asiair-commander/internal/model"
)

// ErrFrameNotFound is returned when a dismiss targets a frame id that does not
// exist on this rig. ErrNotDismissable is returned when the frame exists but is
// not in a state that may be dismissed. Both are exported so the web layer can map
// them to HTTP status codes via errors.Is (design frame-lifecycle §13).
var (
	ErrFrameNotFound  = errors.New("rig: frame not found")
	ErrNotDismissable = errors.New("rig: only unclassified frames can be dismissed")
)

// discardOneGuarded wraps discardOne in a recover that turns a panic into a
// returned error, matching the "one bad frame never kills the worker/service"
// invariant every other frame-mutating path honours: the dismissReqs case (and its
// drainCommands twin) is a fresh worker entry point not under runCycle's recover.
func (w *Worker) discardOneGuarded(ctx context.Context, frameID int64) (err error) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("rig: dismiss panic recovered", "frame", frameID, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			err = fmt.Errorf("rig: dismiss panic: %v", r)
		}
	}()
	return w.discardOne(ctx, frameID)
}

// discardOne dismisses one unclassified frame: it purges the NAS quarantine copy
// (unless another live row shares it) and tombstones the row so the corrupt file
// stops driving the backlog alert and is not re-pulled while unchanged (design
// frame-lifecycle §13). Runs on the worker goroutine so it never races the
// pull/delete passes.
func (w *Worker) discardOne(ctx context.Context, frameID int64) error {
	f, ok, err := w.store.GetFrameByID(ctx, frameID)
	if err != nil {
		return err
	}
	if !ok || f.RigID != w.profileID {
		return ErrFrameNotFound
	}
	if f.State == model.StateDismissed {
		return nil // idempotent: already done
	}
	if f.State != model.StateUnclassified {
		return ErrNotDismissable
	}

	// Purge the NAS copy only if no OTHER live row references the same dest_path.
	// Identical content pulled from two shares files to one NAS copy shared by two
	// rows; removing it while a sibling still points at it would orphan the sibling
	// and later raise a spurious frame_lost.
	if f.DestPath != "" {
		shared, err := w.store.CountLiveFramesByDest(ctx, w.profileID, f.DestPath, f.ID)
		if err != nil {
			return err
		}
		if shared == 0 {
			if err := w.lfs.Remove(f.DestPath); err != nil && !isNotExist(err) {
				w.log.Warn("rig: dismiss purge failed; leaving frame unclassified",
					"path", f.SourcePath, "dest", f.DestPath, "error", err)
				return err
			}
			// The NAS bytes are gone, so the derived previews must not outlive them.
			if w.previewer != nil {
				w.previewer.EvictDigest(f.SHA256)
			}
		} else {
			w.log.Info("rig: dismiss keeping shared NAS copy", "dest", f.DestPath, "otherRows", shared)
		}
	}

	// DismissFrame sets state=dismissed AND clears pulled_at + dest_path in one
	// UPDATE, so the frame also leaves the pulled_at-keyed transfer-history /
	// "transferred tonight" queries. size/mtime are retained for decide()'s
	// change-detection (the tombstone's stickiness).
	if err := w.store.DismissFrame(ctx, frameID); err != nil {
		// The NAS copy may already be purged above; the row is still unclassified.
		// This half-state self-heals on the operator's retry (the purge is
		// idempotent). Log it distinctly so the window is visible if it ever bites.
		w.log.Warn("rig: dismiss tombstone write failed after purge; retry to reconcile",
			"path", f.SourcePath, "error", err)
		return err
	}
	w.log.Info("rig: dismissed unclassified frame", "path", f.SourcePath)

	w.ackResolvedUnclassified(ctx) // sweeps this rig's resolved per-frame unclassified alerts
	w.maybeAckBacklog(ctx)         // auto-acks the aggregate alert if it's resolved
	w.recomputeCounters(ctx)       // recompute resident/"kept" + tonight, then publish
	return nil
}
