package rig

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/platesolve"
	"github.com/macourteau/asiair-commander/internal/store"
)

const (
	// solvePageSize is the query page for the id-cursor drain of the solve queue.
	solvePageSize = 256
	// solveYield paces the single solve worker between solves so its full-frame NAS
	// reads can't starve live pulls (mirrors the backfill's inter-read yield).
	solveYield = 3 * time.Millisecond
	// solveReadyPoll is how often RunSolver re-checks prov.Ready() while the catalog
	// is still downloading.
	solveReadyPoll = 2 * time.Second
	// solveRedrainInterval is the periodic from-zero re-sweep that closes the ordering
	// gap with the Tier-0 backfill (a legacy frame flipped NULL→none after the drain
	// cursor passed it is re-seen next sweep) and recovers any dropped nudge.
	solveRedrainInterval = 5 * time.Minute
	// maxSolveAttempts terminalizes a poison frame that panics deterministically:
	// after this many panics it is written failed rather than flapping none↔solving.
	maxSolveAttempts = 3
)

// solveRunner bundles the collaborators a solve pass needs. It is the private form
// of RunSolver's arguments so the per-frame helpers read cleanly.
type solveRunner struct {
	st        store.Store
	hdr       header.Reader
	solver    platesolve.Solver
	prov      platesolve.SolveGate
	hub       hub.Hub
	cfg       *atomic.Pointer[model.Config]
	log       *slog.Logger
	now       func() time.Time
	radiusDeg func() float64 // live, read per solve from the config pointer
}

// RunSolver is the long-lived Tier-1 solve worker: a single goroutine that, once the
// provisioner is ready, drains the DB-backed queue of solvable light frames and
// re-solves on a wakeup nudge for low latency. The DB is the queue of record — a
// lost nudge is recovered by the periodic from-zero re-sweep, so nothing is lost.
func RunSolver(ctx context.Context, st store.Store, hdr header.Reader,
	solver platesolve.Solver, prov platesolve.SolveGate, h hub.Hub,
	nudge <-chan int64, cfgPtr *atomic.Pointer[model.Config], logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "solve-worker")

	cfg := cfgPtr.Load()
	if cfg == nil || !cfg.PlateSolveEnabled {
		logger.Info("plate solving disabled; solve worker not running")
		return
	}

	r := &solveRunner{
		st: st, hdr: hdr, solver: solver, prov: prov, hub: h, cfg: cfgPtr,
		log: logger, now: time.Now,
		radiusDeg: func() float64 {
			c := cfgPtr.Load()
			if c == nil || c.PlateSolveRadiusDeg <= 0 {
				return 10
			}
			return float64(c.PlateSolveRadiusDeg)
		},
	}

	// Hold until the binary + a catalog are present; until then every `none` light
	// simply stays `none` (renders blank, same as Tier 0). Drain nudges meanwhile.
	if !r.waitReady(ctx, nudge) {
		return
	}
	logger.Info("solve worker: ready; draining backlog")

	tick := time.NewTicker(solveRedrainInterval)
	defer tick.Stop()
	for {
		r.drainBacklog(ctx)
		select {
		case <-ctx.Done():
			return
		case <-nudge:
			// Any nudge (a freshly-filed hinted light, a Tier-0 backfill write, or a
			// post-download wake) means "sweep now"; the from-zero re-drain above picks
			// up the specific frame. Coalesced: a burst of nudges collapses into a few
			// near-empty indexed re-scans.
		case <-tick.C:
		}
	}
}

// waitReady blocks until the provisioner reports ready or ctx is cancelled,
// draining (and ignoring) nudges meanwhile so the channel never backs up.
func (r *solveRunner) waitReady(ctx context.Context, nudge <-chan int64) bool {
	for !r.prov.Ready() {
		select {
		case <-ctx.Done():
			return false
		case <-nudge:
		case <-time.After(solveReadyPoll):
		}
	}
	return ctx.Err() == nil
}

// drainBacklog walks the solve queue by an advancing id cursor, from zero, solving
// each eligible frame. Advancing past every attempted row (whatever the outcome)
// guarantees a bounded pass, exactly like the Tier-0 backfill.
func (r *solveRunner) drainBacklog(ctx context.Context) {
	var afterID int64
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := r.st.ListFramesNeedingSolve(ctx, afterID, solvePageSize)
		if err != nil {
			r.log.Error("solve worker: list failed; stopping this sweep", "error", err)
			return
		}
		for i := range frames {
			// Yield the gate to a queued/running catalog download: stop draining so the
			// download preempts after the current solve rather than waiting out the whole
			// backlog. The download wakes the worker on completion to resume this sweep.
			if r.prov.DownloadPending() {
				return
			}
			f := &frames[i]
			afterID = f.ID
			r.solveOne(ctx, f)
			select {
			case <-ctx.Done():
				return
			case <-time.After(solveYield):
			}
		}
		if len(frames) < solvePageSize {
			return
		}
	}
}

// solveOne runs the full disposition→claim→solve→terminal-write sequence for one
// frame under a per-frame recover so a single bad frame can't kill solving. The
// recover budgets only a *panic* (a per-frame fault): after maxSolveAttempts the
// frame is written terminal `failed` instead of looping. Global faults (missing
// binary/catalog, unreadable .ini) are handled inline as unbudgeted transients.
func (r *solveRunner) solveOne(ctx context.Context, f *model.Frame) {
	release, err := r.prov.BeginSolve(ctx)
	if err != nil {
		return // ctx cancelled while acquiring the gate
	}
	defer release()

	var (
		claimed   bool
		claimBlob *model.Astrometry
	)
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("solve worker: panic on frame; budgeting", "id", f.ID, "panic", rec)
			if claimed {
				r.budgetFault(ctx, f, claimBlob, "repeated solver error")
			}
		}
	}()

	// Reachability: a stat failure of any kind is transient — leave `none`.
	if _, statErr := os.Stat(f.DestPath); statErr != nil {
		return
	}
	// Re-read the source header for NAXIS1/2 and a fresh hint (hardens the list→claim
	// TOCTOU). A read failure after a good stat is transient.
	h, err := r.hdr.ReadHeader(f.DestPath)
	if err != nil {
		return
	}
	astro := astrometry.FromHeader(h)
	hints := astro.Hints
	priorAttempts := 0
	if f.Astrometry != nil {
		priorAttempts = f.Astrometry.Attempts
	}

	n1, _ := h.Float("NAXIS1")
	n2, _ := h.Float("NAXIS2")

	// Disposition (before claiming): a frame with no pointing hint is unsolvable; a
	// frame whose field is below every installed catalog's floor is unsolvable and
	// records its FOV so a deeper catalog re-queues it.
	disp := dispositionOf(hints, n2, r.prov)

	// Claim none→solving. All terminal writes go through the guarded CAS after this,
	// so a mid-solve re-pull is never clobbered.
	claimBlob = &model.Astrometry{Status: model.AstroStatusSolving, Hints: hints, Attempts: priorAttempts, FOVDeg: disp.fov}
	claimed, err = r.st.ClaimFrameForSolve(ctx, f.ID, claimBlob)
	if err != nil {
		r.log.Warn("solve worker: claim failed", "id", f.ID, "error", err)
		return
	}
	if !claimed {
		return // re-pulled / solved / already claimed — nothing owed
	}

	if disp.unsolvable {
		// Write unsolvable directly (no "solving…" emit → no flicker), then emit + log.
		blob := &model.Astrometry{Status: model.AstroStatusUnsolvable, Hints: hints, Reason: disp.reason, FOVDeg: disp.fov}
		if r.writeTerminal(ctx, f, blob) {
			r.log.Info("solve worker: cannot solve", "id", f.ID, "path", f.DestPath, "reason", disp.reason)
		}
		return
	}

	// Solvable: show "solving…", then run the solve.
	f.Astrometry = claimBlob
	r.emit(f)

	req := platesolve.Request{
		Path:       f.DestPath,
		HintRA:     *hints.RA,
		HintDec:    *hints.Dec,
		FOVHeight:  disp.fov,
		RadiusDeg:  r.radiusDeg(),
		CatalogDir: disp.catalogDir,
		NAXIS1:     int(n1),
		NAXIS2:     int(n2),
	}
	res, serr := r.solver.Solve(ctx, req)
	if serr != nil {
		if errors.Is(serr, platesolve.ErrSolveTimeout) {
			// A timeout is a property of this frame (a genuinely hard field), not a
			// global fault — budget it per-frame so a frame that always times out
			// terminalizes instead of burning the full deadline on every re-sweep and
			// starving the gate. (Normal solves are sub-second; a 120 s solve is the
			// hard case, not transient slowness.)
			r.log.Warn("solve worker: solve timed out; budgeting", "id", f.ID)
			r.budgetFault(ctx, f, claimBlob, "repeated solve timeout")
			return
		}
		// Global/operational error (binary/catalog/path fault): unbudgeted transient —
		// reclaim to `none`, no attempt counted, and clear the "solving…" spinner.
		r.log.Warn("solve worker: solver error (transient); leaving queued", "id", f.ID, "error", serr)
		r.reclaimToNone(ctx, f, priorAttempts, hints)
		return
	}
	if res.Solution != nil {
		blob := &model.Astrometry{
			Status:   model.AstroStatusSolved,
			Solution: res.Solution,
			Hints:    hints,
			SolvedAt: r.now().UnixNano(),
			FOVDeg:   disp.fov,
		}
		if r.writeTerminal(ctx, f, blob) {
			r.log.Info("solve worker: solved", "id", f.ID, "ra", res.Solution.CenterRA, "dec", res.Solution.CenterDec)
		}
		return
	}
	// Clean no-match → failed (terminal, never retried).
	blob := &model.Astrometry{Status: model.AstroStatusFailed, Hints: hints, Reason: res.Reason, FOVDeg: disp.fov}
	if r.writeTerminal(ctx, f, blob) {
		r.log.Info("solve worker: solve failed", "id", f.ID, "reason", res.Reason)
	}
}

// disposition is the pre-claim decision: solvable (with a catalog dir), or
// unsolvable (with a reason). fov is the frame's field height, carried into the
// blob so a below-floor reject can be re-queued when a deeper catalog lands.
type disposition struct {
	unsolvable bool
	reason     string
	fov        float64
	catalogDir string
}

func dispositionOf(hints *model.AstroHints, naxis2 float64, prov platesolve.SolveGate) disposition {
	if hints == nil || hints.RA == nil || hints.Dec == nil {
		return disposition{unsolvable: true, reason: "no pointing hint; blind solving disabled"}
	}
	var focal, pix float64
	if hints.FocalLenMM != nil {
		focal = *hints.FocalLenMM
	}
	if hints.PixelSizeUm != nil {
		pix = *hints.PixelSizeUm
	}
	fov, ok := fovHeightDeg(focal, pix, naxis2)
	if !ok {
		return disposition{unsolvable: true, reason: "cannot determine field of view (missing FOCALLEN/XPIXSZ/NAXIS)"}
	}
	dir, ok := prov.CatalogFor(fov)
	if !ok {
		reason := "field too small for the installed star catalog"
		if name, ok := platesolve.SmallestCovering(fov); ok {
			reason = "field too small for the installed star catalog — download the " + name + " catalog to solve it"
		}
		return disposition{unsolvable: true, reason: reason, fov: fov}
	}
	return disposition{fov: fov, catalogDir: dir}
}

// writeTerminal writes a terminal result through the guarded CAS and, on a hit,
// updates the in-memory frame and emits frame.done. A miss (the row left `solving`
// under a mid-solve re-pull) discards the result and emits nothing — the racing
// re-file's effective-state emit already stands.
func (r *solveRunner) writeTerminal(ctx context.Context, f *model.Frame, blob *model.Astrometry) bool {
	ok, err := r.st.SetFrameSolveResult(ctx, f.ID, blob)
	if err != nil {
		r.log.Warn("solve worker: terminal write failed", "id", f.ID, "error", err)
		return false
	}
	if !ok {
		return false
	}
	f.Astrometry = blob
	r.emit(f)
	return true
}

// reclaimToNone returns a claimed frame to the queue after an unbudgeted transient
// solver error, preserving the attempt count and hints, and clears the spinner.
func (r *solveRunner) reclaimToNone(ctx context.Context, f *model.Frame, attempts int, hints *model.AstroHints) {
	blob := &model.Astrometry{Status: model.AstroStatusNone, Hints: hints, Attempts: attempts}
	r.writeTerminal(ctx, f, blob)
}

// budgetFault increments the per-frame attempt counter after a per-frame fault (a
// panic or a repeated timeout) and either re-queues the frame (under budget) or
// terminalizes it to `failed` (at budget), so a deterministically-bad frame can't
// livelock the from-zero re-sweep. reason is the terminal Reason recorded at budget.
func (r *solveRunner) budgetFault(ctx context.Context, f *model.Frame, claimBlob *model.Astrometry, reason string) {
	attempts := 1
	var hints *model.AstroHints
	var fov float64
	if claimBlob != nil {
		attempts = claimBlob.Attempts + 1
		hints = claimBlob.Hints
		fov = claimBlob.FOVDeg
	}
	if attempts >= maxSolveAttempts {
		r.writeTerminal(ctx, f, &model.Astrometry{
			Status: model.AstroStatusFailed, Hints: hints, Reason: reason, Attempts: attempts, FOVDeg: fov,
		})
		return
	}
	r.writeTerminal(ctx, f, &model.Astrometry{
		Status: model.AstroStatusNone, Hints: hints, Attempts: attempts, FOVDeg: fov,
	})
}

// emit publishes the frame's current state to the live UI (merged by id).
func (r *solveRunner) emit(f *model.Frame) {
	r.hub.Emit(hub.Event{Name: "frame.done", Data: *f})
}

// fovHeightDeg computes a frame's field height in degrees from its optics and
// sensor geometry: the exact plate scale (206.265·XPIXSZ/FOCALLEN arcsec/px) times
// NAXIS2 rows, in degrees. XPIXSZ is the binned pixel pitch, the ASIAIR writing it
// "with binning" (design asiair-hardware-notes §8.1), and NAXIS2 counts binned
// rows, so binning never enters the product. Returns ok=false when an input is
// missing or non-physical, so the caller marks the frame unsolvable rather than
// passing a bogus -fov. Validated against a real frame: 300 mm / 5.94 µm / Bin1 /
// 4042 rows → 4.585°, matching astap's own FOV estimate of 4.59°.
func fovHeightDeg(focalLenMM, pixelSizeUm, naxis2 float64) (float64, bool) {
	if focalLenMM <= 0 || pixelSizeUm <= 0 || naxis2 <= 0 {
		return 0, false
	}
	scaleArcsecPerPx := 206.265 * pixelSizeUm / focalLenMM
	return scaleArcsecPerPx * naxis2 / 3600.0, true
}
