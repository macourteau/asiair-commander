package rig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"runtime/debug"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	// reconcileInterval is how often a rig runs a full automatic library scan.
	reconcileInterval = 6 * time.Hour
	// reconcileRetryInterval is the shorter retry after a pass in which every root
	// failed or the pass was abandoned on pause/cancellation.
	reconcileRetryInterval = 15 * time.Minute
	// rootUnavailableAlertAfter is how long a root must stay unlinked/failing
	// before AlertLibraryUnavailable is raised.
	rootUnavailableAlertAfter = 6 * time.Hour
	// defaultReconcileSettle is the settle-window fallback the delete pass applies
	// when a rig's effective reconcile_settle is a hand-edited non-positive value;
	// the real window is the per-rig config (design library-reconciliation §4.5).
	defaultReconcileSettle = 24 * time.Hour
	// maxScanDirsInResult caps how many directory names a scan result carries back
	// for the UI (newest first).
	maxScanDirsInResult = 50
)

// scanRoots returns the destination roots the scan walks: Light and Cal only (the
// two that hold filed frames). Staging and Logs never do.
func scanRoots(cfg *model.Config) []string {
	var roots []string
	for _, r := range []string{cfg.Roots.Light, cfg.Roots.Cal} {
		if strings.TrimSpace(r) != "" {
			roots = append(roots, r)
		}
	}
	return roots
}

// scanLibrary is the out-of-band-deletion observer: for each configured,
// currently-linked destination root it lists the directories holding this rig's
// live rows and marks absent frames / clears reappeared ones. It authorizes
// nothing (its only writes are the idempotent, reversible missing mark) and never
// runs while the rig is paused. Panics are recovered so a scan fault never takes
// down the worker cycle.
// scanLibrary returns whether the pass completed with at least one root
// successfully reconciled — false if every root failed its gate or the pass was
// abandoned on pause/cancellation, which the scheduler uses to retry sooner.
func (w *Worker) scanLibrary(ctx context.Context, cfg *model.Config, profile *model.RigProfile) (anySuccess bool) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("rig: library scan panic recovered", "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
		}
	}()
	if w.paused {
		return false
	}
	roots := scanRoots(cfg)
	res := model.LibraryScanResult{At: w.now().Unix()}

	// Rows whose dest_path falls under no configured root are invisible to the
	// per-root walk; report them so a repointed root does not silently orphan them.
	if n, err := w.store.CountOutsideRoots(ctx, w.profileID, roots); err != nil {
		w.log.Warn("rig: outside-roots count failed", "error", err)
	} else if n > 0 {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertConfig,
			Message:  fmt.Sprintf("%d frame(s) reference a path under no configured library root; relink or restore the root", n),
		})
	}

	for _, root := range roots {
		if ctx.Err() != nil || w.paused {
			w.publishScanResult(ctx, res)
			return false
		}
		rr := model.RootScanResult{Root: root}
		ok, reason := w.rootConfirmed(ctx, root)
		// An existing/drained library never fires the file-path auto-link (no fresh
		// frame files there), so link it here from data already on disk, confirmed by
		// re-verifying a filed frame. Only a "not linked" root (no recorded id) is a
		// candidate; a "marker missing" root is the recovery case and stays for a
		// re-link.
		if !ok && reason == "not linked" && w.tryLinkFromExistingData(ctx, root, roots) {
			ok, reason = w.rootConfirmed(ctx, root)
		}
		rr.Linked = ok
		rr.Reason = reason
		if !ok {
			w.handleRootUnavailable(ctx, root, reason)
			res.Roots = append(res.Roots, rr)
			continue
		}
		w.clearRootUnavailable(ctx, root)

		abandoned := w.scanRoot(ctx, cfg, root, roots, &rr)
		res.Roots = append(res.Roots, rr)
		if abandoned {
			w.publishScanResult(ctx, res)
			return false
		}
		anySuccess = true
	}

	w.publishScanResult(ctx, res)
	return anySuccess
}

// runScanIfDue runs the library scan when an operator rescan is queued or the
// automatic interval has elapsed. A manual rescan does not advance lastScan; an
// automatic pass advances it by the full interval on success, or by only
// (interval - retry) on failure/abandon so the next attempt is retryInterval away.
func (w *Worker) runScanIfDue(ctx context.Context, cfg *model.Config, profile *model.RigProfile) {
	manual := false
	select {
	case <-w.rescanReqs:
		manual = true
	default:
	}
	due := w.now().Sub(w.lastScan) >= reconcileInterval
	if !manual && !due {
		return
	}
	ok := w.scanLibrary(ctx, cfg, profile)
	if manual {
		return
	}
	if ok {
		w.lastScan = w.now()
	} else {
		w.lastScan = w.now().Add(-(reconcileInterval - reconcileRetryInterval))
	}
}

// scanRoot walks one linked root's directory groups, marking/clearing frames. It
// returns true if the pass was abandoned (ctx cancelled or paused mid-walk).
func (w *Worker) scanRoot(ctx context.Context, cfg *model.Config, root string, allRoots []string, rr *model.RootScanResult) (abandoned bool) {
	now := w.now().Unix()
	err := w.store.ScanTargets(ctx, w.profileID, root, allRoots, func(dir string, frames []store.ScanFrame) error {
		if ctx.Err() != nil || w.paused {
			return errScanAbandoned
		}
		rr.DirsScanned++
		newlyMissing := w.reconcileDir(ctx, dir, frames, now, rr)
		if newlyMissing > 0 {
			if len(rr.Directories) < maxScanDirsInResult {
				rr.Directories = append(rr.Directories, dir)
			}
			w.notify.Raise(ctx, model.Alert{
				RigID:    w.profileID,
				Severity: model.SeverityWarn,
				Category: model.AlertLibraryReconciled,
				Message: fmt.Sprintf("%d frame(s) removed outside the app from %s (observed %s)",
					newlyMissing, dir, w.now().Format("2006-01-02")),
			})
		}
		// Between groups: yield, service commands, honour a pause.
		time.Sleep(backfillYield)
		w.drainCommands(ctx)
		if ctx.Err() != nil || w.paused {
			return errScanAbandoned
		}
		return nil
	})
	if errors.Is(err, errScanAbandoned) {
		return true
	}
	if err != nil {
		w.log.Warn("rig: library scan root failed", "root", root, "error", err)
	}
	return false
}

var errScanAbandoned = errors.New("library scan abandoned")

// reconcileDir applies design library-reconciliation §2.5's three-step
// present/absent decision to one directory group and returns how many frames it
// newly marked missing this call.
func (w *Worker) reconcileDir(ctx context.Context, dir string, frames []store.ScanFrame, now int64, rr *model.RootScanResult) (newlyMissing int) {
	exact := map[string]bool{}
	folded := map[string]bool{}
	dirGone := false
	ents, err := w.lfs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			dirGone = true // confirm each frame individually below
		} else {
			rr.UnscannedDirs++ // transient: conclude nothing about this directory
			return 0
		}
	}
	for _, e := range ents {
		exact[e.Name] = true
		folded[strings.ToLower(e.Name)] = true
	}

	clear := func(f store.ScanFrame) {
		if f.MissingSeenAt == 0 {
			return
		}
		if err := w.store.ClearFrameMissing(ctx, f.ID); err != nil {
			w.log.Warn("rig: clear missing failed", "id", f.ID, "error", err)
			return
		}
		rr.Cleared++
	}
	markAbsent := func(f store.ScanFrame) {
		if f.MissingSeenAt != 0 {
			return // already marked; the mark is idempotent
		}
		if err := w.store.SetFrameMissing(ctx, f.ID, now); err != nil {
			w.log.Warn("rig: set missing failed", "id", f.ID, "error", err)
			return
		}
		newlyMissing++
	}

	for _, f := range frames {
		if !dirGone {
			if exact[f.Base] {
				clear(f)
				continue
			}
			if folded[strings.ToLower(f.Base)] {
				// A case-only divergence means a third party renamed the file, not
				// that it is gone. Never conclude absent; warn and leave the mark.
				w.log.Warn("rig: library entry differs only by case; not concluding absence", "path", f.DestPath)
				continue
			}
		}
		// Candidate absent (name missing from a good listing, or the directory
		// itself is gone): the confirming Stat decides. Only not-exist counts; a
		// Stat error concludes nothing.
		if _, _, serr := w.lfs.Stat(f.DestPath); serr == nil {
			clear(f) // present after all (race with a concurrent write)
		} else if errors.Is(serr, fs.ErrNotExist) {
			markAbsent(f)
		}
	}
	return newlyMissing
}

// rootUnavailableKey is the per-root meta key tracking a first-failure time.
func rootUnavailableKey(root string) string { return "rootunavail:" + RootKey(root) }

// handleRootUnavailable tracks how long a root has been unlinked/failing and
// raises AlertLibraryUnavailable once past the threshold. A "different library"
// reason raises the critical mismatch alert immediately.
func (w *Worker) handleRootUnavailable(ctx context.Context, root, reason string) {
	if reason == "not linked" {
		// A root with no recorded id yet is simply awaiting its first verified frame,
		// which auto-links it (design library-reconciliation §8). It is not a fault
		// and offers no re-link action, so it must not raise the "marker removed"
		// warning.
		return
	}
	if reason == "different library" {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityCritical,
			Category: model.AlertLibraryMismatch,
			Message:  "a different library is mounted at destination root " + root + "; reconciliation is paused. If this is your library, re-link it under Settings › Library.",
		})
		return
	}
	key := rootUnavailableKey(root)
	sinceStr, ok, err := w.store.GetMeta(ctx, key)
	if err != nil {
		return
	}
	now := w.now().Unix()
	if !ok || sinceStr == "" {
		_ = w.store.SetMeta(ctx, key, fmt.Sprintf("%d", now))
		return
	}
	var since int64
	fmt.Sscan(sinceStr, &since)
	if since > 0 && time.Duration(now-since)*time.Second >= rootUnavailableAlertAfter {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertLibraryUnavailable,
			Message:  fmt.Sprintf("destination root %s is %s and has not reconciled for 6 h. If the marker file was removed, re-link it under Settings › Library; if the NAS is unmounted, remount it.", root, reason),
		})
	}
}

// clearRootUnavailable clears a root's first-failure marker on a successful
// confirmation (any rig's success clears it, since the key is per root).
func (w *Worker) clearRootUnavailable(ctx context.Context, root string) {
	if v, ok, _ := w.store.GetMeta(ctx, rootUnavailableKey(root)); ok && v != "" {
		_ = w.store.SetMeta(ctx, rootUnavailableKey(root), "")
	}
}

// publishScanResult stores the scan result on the live status, which carries it to
// the UI as last_library_scan.
func (w *Worker) publishScanResult(_ context.Context, res model.LibraryScanResult) {
	r := res
	w.st.LastLibraryScan = &r
	w.publish()
}
