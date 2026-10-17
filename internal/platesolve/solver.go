// Package platesolve is the Tier-1 plate solver: it produces a model.WCSSolution
// for a light frame the ASIAIR left without an embedded WCS, by shelling out to
// the external astap_cli program with a pointing hint. It adds no astrometry math
// — astap_cli's output is itself a FITS header that the existing internal/header
// reader and internal/astrometry package turn into the same WCSSolution Tier 0
// produces (design features §3.3). The package owns three
// concerns behind small seams: the Solver (run a solve), the Provisioner (make the
// binary + a star catalog available), and a fake for tests.
package platesolve

import (
	"context"
	"errors"

	"github.com/macourteau/asiair-commander/internal/model"
)

// ErrSolveTimeout is returned (wrapped) when a solve exceeds its deadline. Unlike a
// missing-binary/catalog fault (a global transient), a timeout is a property of the
// frame — a genuinely hard field — so the worker budgets it per-frame and
// terminalizes a chronically-timing-out frame instead of retrying it forever.
var ErrSolveTimeout = errors.New("platesolve: solve deadline exceeded")

// Request is one hinted solve. The hint (center + exact scale) collapses the solve
// from an all-sky search to a local refinement — the regime this feature targets.
type Request struct {
	Path       string  // the frame's filed dest_path
	HintRA     float64 // deg (from AstroHints.RA)
	HintDec    float64 // deg (from AstroHints.Dec)
	FOVHeight  float64 // deg, field height (from FOCALLEN/XPIXSZ/binning/NAXIS2)
	RadiusDeg  float64 // hint search radius
	CatalogDir string  // the star-catalog dir CatalogFor(fov) chose
	NAXIS1     int     // source axis lengths, injected into the .wcs so FromHeader can compute FOV (design features §3.3)
	NAXIS2     int
}

// Result is a solve outcome. Solution is non-nil iff the solver reported
// PLTSOLVD=T; otherwise Reason is a human/log string ("no star match").
type Result struct {
	Solution *model.WCSSolution
	Reason   string
}

// Solver runs a single hinted plate solve.
//
// The error/nil contract is load-bearing for the worker's transient-vs-terminal
// decision: a non-nil err is an OPERATIONAL failure (binary/catalog missing,
// unreadable frame) the caller treats as transient; a nil err with a nil
// Result.Solution is a CLEAN "solver ran, found nothing" the caller treats as
// terminal `failed`.
type Solver interface {
	Solve(ctx context.Context, req Request) (Result, error)
}

// SolveGate is the slice of the Provisioner the solve worker depends on, kept to
// three methods so the worker's tests can fake it without constructing a real
// provisioner or seeding disk state (design features §3.6). *Provisioner
// satisfies it.
type SolveGate interface {
	// Ready reports the binary is resolved AND at least one catalog is installed.
	Ready() bool
	// CatalogFor returns the shallowest installed catalog dir whose floor ≤ fovDeg,
	// or ok=false when no installed catalog covers the field.
	CatalogFor(fovDeg float64) (dir string, ok bool)
	// BeginSolve acquires the solve/download gate (ctx-aware), returning a release
	// the worker defers. It blocks while a catalog download holds the gate, and
	// unblocks on ctx cancel so shutdown isn't stuck behind a multi-minute download.
	BeginSolve(ctx context.Context) (release func(), err error)
	// DownloadPending reports that a catalog download is queued or running; the worker
	// yields the backlog so the download preempts after the current solve.
	DownloadPending() bool
}
