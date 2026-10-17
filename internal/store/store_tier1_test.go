package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// darkFrame builds a calibration DARK frame (astroFrame hardcodes LIGHT), to prove
// the solve queue never lists calibration frames.
func darkFrame(path string, a *model.Astrometry) *model.Frame {
	f := astroFrame(path, a)
	f.FrameType = "DARK"
	return f
}

func mustUpsert(t *testing.T, s Store, f *model.Frame) *model.Frame {
	t.Helper()
	if err := s.UpsertFrame(context.Background(), f); err != nil {
		t.Fatalf("UpsertFrame(%s): %v", f.SourcePath, err)
	}
	return f
}

func statusOf(t *testing.T, s Store, id int64) string {
	t.Helper()
	got, ok, err := s.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID(%d): ok=%v err=%v", id, ok, err)
	}
	if got.Astrometry == nil {
		return "<nil>"
	}
	return got.Astrometry.Status
}

// TestListFramesNeedingSolve pins the queue predicate: only wcs_status='none' AND
// frame_type='LIGHT' eligible rows appear — not calibration, not header/solved, not
// the NULL backfill queue.
func TestListFramesNeedingSolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	wantSolve := mustUpsert(t, s, astroFrame("none_light.fit", &model.Astrometry{Status: model.AstroStatusNone}))
	mustUpsert(t, s, darkFrame("none_dark.fit", &model.Astrometry{Status: model.AstroStatusNone}))                                                  // wrong type
	mustUpsert(t, s, astroFrame("header_light.fit", &model.Astrometry{Status: model.AstroStatusHeader, Solution: &model.WCSSolution{CenterRA: 1}})) // terminal
	mustUpsert(t, s, astroFrame("solved_light.fit", &model.Astrometry{Status: model.AstroStatusSolved, Solution: &model.WCSSolution{CenterRA: 2}})) // terminal
	mustUpsert(t, s, astroFrame("null_light.fit", nil))                                                                                             // backfill queue, not solve queue

	need, err := s.ListFramesNeedingSolve(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 || need[0].ID != wantSolve.ID {
		t.Fatalf("solve queue = %v, want only the none LIGHT (%d)", ids(need), wantSolve.ID)
	}
}

// TestClaimFrameForSolve pins the guarded none→solving claim: it succeeds once, then
// a second claim of the now-solving row fails.
func TestClaimFrameForSolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	f := mustUpsert(t, s, astroFrame("claim.fit", &model.Astrometry{Status: model.AstroStatusNone}))

	claimBlob := &model.Astrometry{Status: model.AstroStatusSolving}
	ok, err := s.ClaimFrameForSolve(ctx, f.ID, claimBlob)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if got := statusOf(t, s, f.ID); got != model.AstroStatusSolving {
		t.Fatalf("after claim status=%q, want solving", got)
	}
	ok, err = s.ClaimFrameForSolve(ctx, f.ID, claimBlob)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second claim of a solving row returned true, want false")
	}
}

// TestSetFrameSolveResultGuarded pins design features §3.5.3: the terminal write
// hits only while the row is solving, and returns false when the row was reset out
// from under the solve (the mid-solve differ case) so the stale solution is
// discarded.
func TestSetFrameSolveResultGuarded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	f := mustUpsert(t, s, astroFrame("cas.fit", &model.Astrometry{Status: model.AstroStatusNone}))

	// Not claimed yet (still none): a terminal write must miss.
	ok, err := s.SetFrameSolveResult(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusSolved,
		Solution: &model.WCSSolution{CenterRA: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("terminal write on a none row returned true; must be guarded on solving")
	}

	// Claim, then the terminal write hits.
	if ok, _ := s.ClaimFrameForSolve(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusSolving}); !ok {
		t.Fatal("claim failed")
	}
	ok, err = s.SetFrameSolveResult(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusSolved,
		Solution: &model.WCSSolution{CenterRA: 5}})
	if err != nil || !ok {
		t.Fatalf("terminal write on a solving row: ok=%v err=%v", ok, err)
	}
	if got := statusOf(t, s, f.ID); got != model.AstroStatusSolved {
		t.Fatalf("after terminal write status=%q, want solved", got)
	}

	// Simulate a mid-solve reset (differ): the row moved back to none. A late terminal
	// write from the racing solve must miss.
	if err := s.ClearFrameAstrometry(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, s, astroFrame("cas.fit", &model.Astrometry{Status: model.AstroStatusNone}))
	ok, err = s.SetFrameSolveResult(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusSolved,
		Solution: &model.WCSSolution{CenterRA: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("late terminal write after a reset returned true; the stale solution must be discarded")
	}
}

// TestReclaimSolvingFrames pins the startup self-heal: every solving row resets to
// none, blob and column together.
func TestReclaimSolvingFrames(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a := mustUpsert(t, s, astroFrame("a.fit", &model.Astrometry{Status: model.AstroStatusNone}))
	b := mustUpsert(t, s, astroFrame("b.fit", &model.Astrometry{Status: model.AstroStatusNone}))
	for _, f := range []*model.Frame{a, b} {
		if ok, _ := s.ClaimFrameForSolve(ctx, f.ID, &model.Astrometry{Status: model.AstroStatusSolving,
			Hints: &model.AstroHints{}}); !ok {
			t.Fatalf("claim %d failed", f.ID)
		}
	}
	n, err := s.ReclaimSolvingFrames(ctx)
	if err != nil || n != 2 {
		t.Fatalf("ReclaimSolvingFrames = %d, %v; want 2", n, err)
	}
	for _, f := range []*model.Frame{a, b} {
		if got := statusOf(t, s, f.ID); got != model.AstroStatusNone {
			t.Fatalf("frame %d status=%q after reclaim, want none", f.ID, got)
		}
	}
}

// TestClearFrameAstrometry pins the content-change reset: the row returns to
// NULL/NULL and re-enters the Tier-0 backfill queue.
func TestClearFrameAstrometry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	f := mustUpsert(t, s, astroFrame("clear.fit", &model.Astrometry{Status: model.AstroStatusSolved,
		Solution: &model.WCSSolution{CenterRA: 3}}))
	if err := s.ClearFrameAstrometry(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.GetFrameByID(ctx, f.ID)
	if got.Astrometry != nil {
		t.Fatalf("after clear Astrometry=%+v, want nil", got.Astrometry)
	}
	need, _ := s.ListFramesNeedingAstrometry(ctx, 0, 10)
	if len(need) != 1 || need[0].ID != f.ID {
		t.Fatalf("cleared frame did not re-enter the backfill queue: %v", ids(need))
	}
}

// TestRequeueFloorUnsolvable pins the deeper-catalog re-queue: only below-floor
// rejects whose own field now fits (fov_deg ≥ the installed floor) flip back to
// none; a field too small even for the new catalog stays unsolvable.
func TestRequeueFloorUnsolvable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// 0.25° field: below d05 (0.6) and d20 (0.3), covered by d50 (0.2).
	fits := mustUpsert(t, s, astroFrame("fits_d50.fit", &model.Astrometry{Status: model.AstroStatusUnsolvable, FOVDeg: 0.25}))
	// 0.15° field: below even d50 (0.2), stays unsolvable.
	tooSmall := mustUpsert(t, s, astroFrame("too_small.fit", &model.Astrometry{Status: model.AstroStatusUnsolvable, FOVDeg: 0.15}))
	// A no-hint unsolvable (fov_deg 0) must never be re-queued by a floor.
	noHint := mustUpsert(t, s, astroFrame("no_hint.fit", &model.Astrometry{Status: model.AstroStatusUnsolvable}))

	n, err := s.RequeueFloorUnsolvable(ctx, 0.2) // d50 installed
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RequeueFloorUnsolvable(0.2) = %d, want 1", n)
	}
	if got := statusOf(t, s, fits.ID); got != model.AstroStatusNone {
		t.Errorf("0.25° frame status=%q, want none (re-queued)", got)
	}
	if got := statusOf(t, s, tooSmall.ID); got != model.AstroStatusUnsolvable {
		t.Errorf("0.15° frame status=%q, want unsolvable (still too small)", got)
	}
	if got := statusOf(t, s, noHint.ID); got != model.AstroStatusUnsolvable {
		t.Errorf("no-hint frame status=%q, want unsolvable (no fov_deg)", got)
	}
}

// TestRequeueSolveFailures pins the manual re-solve action: only `failed` frames are
// re-queued (attempt budget reset), while `solved` and `unsolvable` are left alone.
func TestRequeueSolveFailures(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	failed := mustUpsert(t, s, astroFrame("failed.fit", &model.Astrometry{
		Status: model.AstroStatusFailed, Reason: "no star match", Attempts: 3}))
	solved := mustUpsert(t, s, astroFrame("solved.fit", &model.Astrometry{
		Status: model.AstroStatusSolved, Solution: &model.WCSSolution{CenterRA: 1}}))
	unsolvable := mustUpsert(t, s, astroFrame("uns.fit", &model.Astrometry{Status: model.AstroStatusUnsolvable}))

	n, err := s.RequeueSolveFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RequeueSolveFailures = %d, want 1 (only the failed frame)", n)
	}
	if got := statusOf(t, s, failed.ID); got != model.AstroStatusNone {
		t.Errorf("failed frame status=%q, want none (re-queued)", got)
	}
	if got := statusOf(t, s, solved.ID); got != model.AstroStatusSolved {
		t.Errorf("solved frame status=%q, want solved (untouched)", got)
	}
	if got := statusOf(t, s, unsolvable.ID); got != model.AstroStatusUnsolvable {
		t.Errorf("unsolvable frame status=%q, want unsolvable (untouched)", got)
	}
	// The attempt budget is reset so a budget-exhausted failure gets a fresh retry.
	got, _, _ := s.GetFrameByID(ctx, failed.ID)
	if got.Astrometry.Attempts != 0 {
		t.Errorf("re-queued frame attempts=%d, want 0 (budget reset)", got.Astrometry.Attempts)
	}
}

// TestUpsertStickyGuard pins design features §3.5.1: a Tier-0 re-extraction (a
// re-file carrying a non-nil `none` astrometry) must NOT clobber a Tier-1
// solution; only an explicit ClearFrameAstrometry (a genuine content change)
// resets it.
func TestUpsertStickyGuard(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	solved := mustUpsert(t, s, astroFrame("sticky.fit", &model.Astrometry{Status: model.AstroStatusSolved,
		Solution: &model.WCSSolution{CenterRA: 314, CenterDec: 44}}))

	// A re-file of the same key carrying a non-nil `none` (the recordFiled path for a
	// light with no embedded WCS). The solution must survive, and the returned struct
	// must report the EFFECTIVE solved state (design features §3.5.2) so the nudge
	// does not re-enqueue.
	repull := astroFrame("sticky.fit", &model.Astrometry{Status: model.AstroStatusNone})
	if err := s.UpsertFrame(ctx, repull); err != nil {
		t.Fatal(err)
	}
	if repull.Astrometry == nil || repull.Astrometry.Status != model.AstroStatusSolved ||
		repull.Astrometry.Solution == nil || repull.Astrometry.Solution.CenterRA != 314 {
		t.Fatalf("design features §3.5.2 effective state wrong: %+v", repull.Astrometry)
	}
	if got := statusOf(t, s, solved.ID); got != model.AstroStatusSolved {
		t.Fatalf("sticky guard failed: status=%q, want solved", got)
	}

	// A genuine content change clears explicitly, then a re-file re-queues to none.
	if err := s.ClearFrameAstrometry(ctx, solved.ID); err != nil {
		t.Fatal(err)
	}
	after := astroFrame("sticky.fit", &model.Astrometry{Status: model.AstroStatusNone})
	if err := s.UpsertFrame(ctx, after); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s, solved.ID); got != model.AstroStatusNone {
		t.Fatalf("after clear+re-file status=%q, want none (re-queued)", got)
	}
}

// TestUpsertReturnsInsertID pins that the RETURNING switch still propagates the
// assigned id on a fresh insert (a regression would leave f.ID==0 and break the
// solve nudge).
func TestUpsertReturnsInsertID(t *testing.T) {
	s := newStore(t)
	f := astroFrame("fresh.fit", &model.Astrometry{Status: model.AstroStatusNone})
	if err := s.UpsertFrame(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.ID == 0 {
		t.Fatal("UpsertFrame left f.ID==0 on a fresh insert")
	}
}
