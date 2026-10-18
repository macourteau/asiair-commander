package rig

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/platesolve"
	"github.com/macourteau/asiair-commander/internal/store"
)

// solveHarness wires a real store + hub with a fake solver/gate and a header.Fake
// so a solveOne call exercises the full disposition→claim→solve→terminal path.
type solveHarness struct {
	t      *testing.T
	st     store.Store
	hub    *hub.StatusHub
	solver *platesolve.FakeSolver
	gate   *platesolve.FakeGate
	fake   header.Fake
	runner *solveRunner
	dir    string
}

func newSolveHarness(t *testing.T) *solveHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := hub.New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	fake := header.Fake{Headers: map[string]model.Header{}}
	solver := &platesolve.FakeSolver{Results: map[string]platesolve.Result{}, Errs: map[string]error{}, Panics: map[string]bool{}}
	gate := &platesolve.FakeGate{ReadyVal: true, CatalogOK: true, CatalogDir: "/cat"}

	cfg := &atomic.Pointer[model.Config]{}
	cfg.Store(&model.Config{PlateSolveEnabled: true, PlateSolveRadiusDeg: 10})

	sh := &solveHarness{t: t, st: st, hub: h, solver: solver, gate: gate, fake: fake, dir: t.TempDir()}
	sh.runner = &solveRunner{
		st: st, hdr: fake, solver: solver, prov: gate, hub: h, cfg: cfg,
		log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
		radiusDeg: func() float64 { return 10 },
	}
	return sh
}

// hintHeader is a synthetic light-frame header carrying a full pointing hint.
func hintHeader() model.Header {
	return model.NewHeader([]model.Card{
		{Key: "RA", Value: 313.9333},
		{Key: "DEC", Value: 43.6814},
		{Key: "FOCALLEN", Value: 300.0},
		{Key: "XPIXSZ", Value: 5.94},
		{Key: "NAXIS1", Value: int64(6072)},
		{Key: "NAXIS2", Value: int64(4042)},
	})
}

// enqueue writes a real file on disk, upserts a none LIGHT frame pointing at it, and
// maps the header.Fake for it. Returns the frame.
func (sh *solveHarness) enqueue(name string, hdr model.Header, exists bool) *model.Frame {
	sh.t.Helper()
	dest := filepath.Join(sh.dir, name)
	if exists {
		if err := os.WriteFile(dest, []byte("fits"), 0o644); err != nil {
			sh.t.Fatal(err)
		}
	}
	sh.fake.Headers[dest] = hdr
	f := &model.Frame{
		RigID: "rig", SourceShare: "Udisk", SourcePath: name, Size: 1, Mtime: 1,
		FrameType: "LIGHT", Binning: "Bin1", State: model.StateVerified, DestPath: dest,
		Astrometry: &model.Astrometry{Status: model.AstroStatusNone},
	}
	if err := sh.st.UpsertFrame(context.Background(), f); err != nil {
		sh.t.Fatal(err)
	}
	return f
}

func (sh *solveHarness) status(id int64) *model.Astrometry {
	sh.t.Helper()
	got, ok, err := sh.st.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		sh.t.Fatalf("GetFrameByID(%d): ok=%v err=%v", id, ok, err)
	}
	return got.Astrometry
}

// capture subscribes, runs fn, and returns the frame.done statuses emitted.
func (sh *solveHarness) capture(fn func()) []string {
	_, _, events, cancel := sh.hub.Subscribe()
	defer cancel()
	fn()
	var out []string
	for {
		select {
		case e := <-events:
			if e.Name == "frame.done" {
				if f, ok := e.Data.(model.Frame); ok && f.Astrometry != nil {
					out = append(out, f.Astrometry.Status)
				}
			}
		default:
			return out
		}
	}
}

func TestSolveOne_Solved(t *testing.T) {
	sh := newSolveHarness(t)
	f := sh.enqueue("solved.fit", hintHeader(), true)
	dest := f.DestPath
	sh.solver.Results[dest] = platesolve.Result{Solution: &model.WCSSolution{CenterRA: 313.9, CenterDec: 43.6}}

	emits := sh.capture(func() { sh.runner.solveOne(context.Background(), f) })

	if got := sh.status(f.ID); got.Status != model.AstroStatusSolved || got.Solution == nil {
		t.Fatalf("status=%+v, want solved with a solution", got)
	}
	// A solvable frame emits solving… then solved.
	if len(emits) != 2 || emits[0] != model.AstroStatusSolving || emits[1] != model.AstroStatusSolved {
		t.Fatalf("emits=%v, want [solving solved]", emits)
	}
}

func TestSolveOne_NoMatchFailed(t *testing.T) {
	sh := newSolveHarness(t)
	f := sh.enqueue("nomatch.fit", hintHeader(), true)
	// No mapping → FakeSolver returns a clean no-match.
	sh.runner.solveOne(context.Background(), f)
	if got := sh.status(f.ID); got.Status != model.AstroStatusFailed {
		t.Fatalf("status=%+v, want failed", got)
	}
}

func TestSolveOne_NoHintUnsolvable(t *testing.T) {
	sh := newSolveHarness(t)
	// Header with optics but no RA/DEC.
	noHint := model.NewHeader([]model.Card{
		{Key: "FOCALLEN", Value: 300.0}, {Key: "XPIXSZ", Value: 5.94},
		{Key: "NAXIS1", Value: int64(6072)}, {Key: "NAXIS2", Value: int64(4042)},
	})
	f := sh.enqueue("nohint.fit", noHint, true)

	emits := sh.capture(func() { sh.runner.solveOne(context.Background(), f) })
	got := sh.status(f.ID)
	if got.Status != model.AstroStatusUnsolvable {
		t.Fatalf("status=%+v, want unsolvable", got)
	}
	// Unsolvable writes directly with no "solving…" flicker: exactly one emit.
	if len(emits) != 1 || emits[0] != model.AstroStatusUnsolvable {
		t.Fatalf("emits=%v, want [unsolvable] (no solving flicker)", emits)
	}
	if sh.solver.Calls != nil {
		t.Fatalf("the solver must not run for a no-hint frame; calls=%v", sh.solver.Calls)
	}
}

func TestSolveOne_MissingFileTransient(t *testing.T) {
	sh := newSolveHarness(t)
	f := sh.enqueue("missing.fit", hintHeader(), false) // no file on disk
	sh.runner.solveOne(context.Background(), f)
	if got := sh.status(f.ID); got.Status != model.AstroStatusNone {
		t.Fatalf("status=%+v, want none (transient, left queued)", got)
	}
}

func TestSolveOne_BelowFloorUnsolvable(t *testing.T) {
	sh := newSolveHarness(t)
	// Force CatalogFor to report the field uncovered whatever its size.
	sh.gate.CatalogOK = false
	f := sh.enqueue("belowfloor.fit", hintHeader(), true)

	sh.runner.solveOne(context.Background(), f)
	got := sh.status(f.ID)
	if got.Status != model.AstroStatusUnsolvable {
		t.Fatalf("status=%+v, want unsolvable (below floor)", got)
	}
	if got.FOVDeg <= 0 {
		t.Fatalf("below-floor unsolvable must record FOVDeg for the re-queue; got %v", got.FOVDeg)
	}
	if sh.solver.Calls != nil {
		t.Fatalf("the solver must not run below floor; calls=%v", sh.solver.Calls)
	}
}

// TestSolveOne_MidSolveResetDiscards pins the design features §3.5.3 CAS miss: if
// the row is reset out from under the solve, the stale solution is discarded and no
// `solved` is emitted.
func TestSolveOne_MidSolveResetDiscards(t *testing.T) {
	sh := newSolveHarness(t)
	f := sh.enqueue("race.fit", hintHeader(), true)
	dest := f.DestPath

	// A solver that, mid-solve, simulates a differ re-pull resetting the row, then
	// returns a solution the guarded terminal write must discard.
	sh.runner.solver = &resettingSolver{st: sh.st, id: f.ID, sol: &model.WCSSolution{CenterRA: 1}}
	_ = dest

	emits := sh.capture(func() { sh.runner.solveOne(context.Background(), f) })
	got := sh.status(f.ID)
	if got.Status == model.AstroStatusSolved {
		t.Fatalf("a stale solution was written despite the mid-solve reset: %+v", got)
	}
	// The solving… emit is fine; a solved emit must NOT appear.
	for _, s := range emits {
		if s == model.AstroStatusSolved {
			t.Fatalf("a solved event was emitted after a discarded CAS: %v", emits)
		}
	}
}

// TestSolveOne_PoisonFrameBudget pins that a deterministically-panicking frame is
// terminalized to failed after maxSolveAttempts rather than flapping forever.
func TestSolveOne_PoisonFrameBudget(t *testing.T) {
	sh := newSolveHarness(t)
	f := sh.enqueue("poison.fit", hintHeader(), true)
	sh.solver.Panics[f.DestPath] = true

	for i := 0; i < maxSolveAttempts; i++ {
		cur, _, _ := sh.st.GetFrameByID(context.Background(), f.ID)
		sh.runner.solveOne(context.Background(), cur)
	}
	got := sh.status(f.ID)
	if got.Status != model.AstroStatusFailed {
		t.Fatalf("after %d panics status=%+v, want failed (budget exhausted)", maxSolveAttempts, got)
	}
}

// TestSolveOne_TimeoutBudget pins that a frame that always times out is budgeted
// per-frame (like a panic) and terminalizes to failed after maxSolveAttempts, rather
// than being an unbudgeted transient that retries — and holds the gate — forever.
func TestSolveOne_TimeoutBudget(t *testing.T) {
	sh := newSolveHarness(t)
	f := sh.enqueue("timeout.fit", hintHeader(), true)
	sh.solver.Errs[f.DestPath] = platesolve.ErrSolveTimeout

	for i := 0; i < maxSolveAttempts; i++ {
		cur, _, _ := sh.st.GetFrameByID(context.Background(), f.ID)
		sh.runner.solveOne(context.Background(), cur)
	}
	got := sh.status(f.ID)
	if got.Status != model.AstroStatusFailed {
		t.Fatalf("after %d timeouts status=%+v, want failed", maxSolveAttempts, got)
	}
	if got.Reason != "repeated solve timeout" {
		t.Errorf("reason=%q, want 'repeated solve timeout'", got.Reason)
	}
}

// TestDrainYieldsToDownload pins that the solve worker stops draining the backlog
// while a catalog download is pending, so the download preempts the gate.
func TestDrainYieldsToDownload(t *testing.T) {
	sh := newSolveHarness(t)
	sh.gate.Pending = true // a download is queued/running
	f := sh.enqueue("waits.fit", hintHeader(), true)

	sh.runner.drainBacklog(context.Background())

	if sh.solver.Calls != nil {
		t.Fatalf("the worker must yield to a pending download; solver calls=%v", sh.solver.Calls)
	}
	if got := sh.status(f.ID); got.Status != model.AstroStatusNone {
		t.Fatalf("frame status=%+v, want none (left for after the download)", got)
	}
}

// resettingSolver simulates a mid-solve differ re-pull by resetting the frame out of
// `solving` before returning a solution.
type resettingSolver struct {
	st  store.Store
	id  int64
	sol *model.WCSSolution
}

func (r *resettingSolver) Solve(ctx context.Context, _ platesolve.Request) (platesolve.Result, error) {
	// The row is `solving`; clear it (as a content-change differ would) so the
	// terminal CAS misses.
	_ = r.st.ClearFrameAstrometry(ctx, r.id)
	_ = r.st.UpsertFrame(ctx, &model.Frame{
		RigID: "rig", SourceShare: "Udisk", SourcePath: "race.fit", Size: 2, Mtime: 2,
		FrameType: "LIGHT", State: model.StateVerified,
		Astrometry: &model.Astrometry{Status: model.AstroStatusNone},
	})
	return platesolve.Result{Solution: r.sol}, nil
}

// enqueueSynthLight writes a synthetic light FITS of the given geometry and binning
// with a full solve hint, reads its header back with the real reader, files its
// binning column the way the filer does (classify → binningToken), and enqueues it.
func (sh *solveHarness) enqueueSynthLight(name string, width, height, bin int, pixelUm float64) *model.Frame {
	sh.t.Helper()
	var cards []string
	for _, c := range fitstest.LightCards(width, height, "Synthetic Field") {
		switch {
		case strings.HasPrefix(c, "XBINNING="):
			c = fmt.Sprintf("XBINNING= %20d / Binning factor in width", bin)
		case strings.HasPrefix(c, "YBINNING="):
			c = fmt.Sprintf("YBINNING= %20d / Binning factor in height", bin)
		}
		cards = append(cards, c)
	}
	cards = append(cards,
		"RA      =             313.9333 / RA of center (deg)",
		"DEC     =              43.6814 / DEC of center (deg)",
		"FOCALLEN=                  300 / focal length in mm",
		fmt.Sprintf("XPIXSZ  = %20g / pixel size in microns (with binning)", pixelUm),
		fmt.Sprintf("YPIXSZ  = %20g / pixel size in microns (with binning)", pixelUm),
	)
	dest := filepath.Join(sh.dir, name)
	if err := os.WriteFile(dest, fitstest.Build(cards, fitstest.SyntheticImage(width, height)), 0o644); err != nil {
		sh.t.Fatal(err)
	}
	h, err := header.New().ReadHeader(dest)
	if err != nil {
		sh.t.Fatal(err)
	}
	cls := classify.Classify(h, nil, nil)
	if !cls.BinPresent || cls.BinX != int64(bin) {
		sh.t.Fatalf("classify binning = %d (present %v), want %d", cls.BinX, cls.BinPresent, bin)
	}
	sh.fake.Headers[dest] = h
	f := &model.Frame{
		RigID: "rig", SourceShare: "Udisk", SourcePath: name, Size: 1, Mtime: 1,
		FrameType: "LIGHT", Binning: binningToken(cls.BinX, cls.BinY), State: model.StateVerified, DestPath: dest,
		Astrometry: &model.Astrometry{Status: model.AstroStatusNone},
	}
	if err := sh.st.UpsertFrame(context.Background(), f); err != nil {
		sh.t.Fatal(err)
	}
	return f
}

// The ASIAIR's XPIXSZ already includes binning (design asiair-hardware-notes §8.1:
// "pixel size in microns (with binning)"), so a bin-2 frame of the same sensor and
// optics (double XPIXSZ, half NAXIS) covers the same field as the bin-1 frame, and
// the -fov hint must match. Multiplying XPIXSZ by binning again doubles it.
func TestSolveOne_BinnedFrameFOVMatchesUnbinned(t *testing.T) {
	sh := newSolveHarness(t)
	bin1 := sh.enqueueSynthLight("bin1.fit", 600, 400, 1, 3.76)
	bin2 := sh.enqueueSynthLight("bin2.fit", 300, 200, 2, 7.52)
	sh.runner.solveOne(context.Background(), bin1)
	sh.runner.solveOne(context.Background(), bin2)

	// 206.265 × 3.76 µm / 300 mm = 2.585 arcsec/px over 400 rows.
	want := 206.265 * 3.76 / 300 * 400 / 3600
	for _, f := range []*model.Frame{bin1, bin2} {
		got := sh.status(f.ID)
		if math.Abs(got.FOVDeg-want) > 1e-9 {
			t.Errorf("%s (binning %s): FOVDeg = %.4f°, want %.4f°", filepath.Base(f.DestPath), f.Binning, got.FOVDeg, want)
		}
	}
}
