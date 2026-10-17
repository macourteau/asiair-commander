package rig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
)

// --- re-file scaffolding -------------------------------------------------------

// refileSrc is the device path every re-file test pulls its frame from.
const refileSrc = "Autorun/Light/C-2025 A6/Light_0001.fit"

// refileTemplate drops <SESSION> from the shipped light layout, so a frame filed
// under the default routes one directory up and re-file has something to do. It
// stands in for the mosaic switch the feature was built for; the engine is
// general and nothing in the move primitive is mosaic-specific.
const refileTemplate = "<TARGET>/<OTA>/NIGHT_<n>"

// moveFailStore forces MoveDestPath to fail on demand, which is the only way to
// reach the commit-failure path in a living process: the file is at its new path,
// the row still at the old one, and re-file must put the file back.
type moveFailStore struct {
	store.Store
	fail error
}

func (s *moveFailStore) MoveDestPath(ctx context.Context, rigID, oldDest, newDest string, policy store.MoveDestPolicy) (int, error) {
	if s.fail != nil {
		return 0, s.fail
	}
	return s.Store.MoveDestPath(ctx, rigID, oldDest, newDest, policy)
}

// previewAll runs the unrestricted preview and returns it whole.
func previewAll(t *testing.T, h *harness) model.RefilePreview {
	t.Helper()
	pv, err := h.w.previewRefile(context.Background(), refileScopeAll)
	if err != nil {
		t.Fatalf("previewRefile: %v", err)
	}
	return pv
}

// previewOneGroup requires the migration to be exactly one old dir → new dir move.
func previewOneGroup(t *testing.T, h *harness) model.RefileGroup {
	t.Helper()
	pv := previewAll(t, h)
	if len(pv.Groups) != 1 {
		t.Fatalf("want 1 group, got %d: groups=%+v refusals=%+v", len(pv.Groups), pv.Groups, pv.Refusals)
	}
	return pv.Groups[0]
}

// applyGroup runs one previewed group on the worker.
func applyGroup(t *testing.T, h *harness, g model.RefileGroup) ([]model.RefileResult, model.RefileSummary) {
	t.Helper()
	res, sum, err := h.w.refileGroup(context.Background(), g.ToDir, g.FrameIDs)
	if err != nil {
		t.Fatalf("refileGroup: %v", err)
	}
	return res, sum
}

// requireMoved requires a single-file group to have moved, and returns the result.
func requireMoved(t *testing.T, res []model.RefileResult) model.RefileResult {
	t.Helper()
	if len(res) != 1 {
		t.Fatalf("want 1 result (one per FILE, not per row), got %d: %+v", len(res), res)
	}
	if res[0].Status != refileMoved {
		t.Fatalf("status=%q reason=%q, want moved", res[0].Status, res[0].Reason)
	}
	return res[0]
}

func mustGetFrame(t *testing.T, h *harness, id int64) model.Frame {
	t.Helper()
	f, ok, err := h.store.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID(%d): ok=%v err=%v", id, ok, err)
	}
	return *f
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// openIntents reads the rig's open re-file intents.
func openIntents(t *testing.T, h *harness) map[string]bool {
	t.Helper()
	m, err := h.store.OpenIntentPaths(context.Background(), "rig-01")
	if err != nil {
		t.Fatalf("OpenIntentPaths: %v", err)
	}
	return m
}

// --- INV-M1: a re-file writes dest_path and nothing else -----------------------

// TestRefileWritesOnlyDestPath is the cardinal re-file invariant (INV-M1, design
// routing-and-filing AC24): the re-filed row differs from its pre-move self in
// dest_path and in no other column — state, deleted_at, sha256, the retarget
// overlay, night and every reconciliation mark are byte-equal across the move. It
// is asserted over the WHOLE row so a future regression of the same shape (a
// resurrected deleted frame, a reset overlay, erased reconciliation marks) is
// caught by construction.
//
// The solve runner is not started by this harness (NewWorker is given a nil
// nudge and nil previewer), so nothing writes wcs_status/astrometry from another
// goroutine and the whole-row comparison is stable.
func TestRefileWritesOnlyDestPath(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	before := mustGetFrame(t, h, f.ID)

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	res := requireMoved(t, mustResults(t, h, g))

	after := mustGetFrame(t, h, f.ID)
	if after.DestPath != res.To || after.DestPath == before.DestPath {
		t.Fatalf("dest_path=%q, want the reported new path %q", after.DestPath, res.To)
	}
	want := before
	want.DestPath = after.DestPath
	if !reflect.DeepEqual(want, after) {
		t.Fatalf("a re-file must write dest_path and NOTHING else (INV-M1)\n before=%+v\n  after=%+v", before, after)
	}
	if v := h.counterValue("asiair_commander_refile_frames_total", map[string]string{"result": "moved"}); v != 1 {
		t.Fatalf("refile_frames_total{result=moved}=%v, want 1", v)
	}
}

// mustResults applies a group and returns its results.
func mustResults(t *testing.T, h *harness, g model.RefileGroup) []model.RefileResult {
	t.Helper()
	res, _ := applyGroup(t, h, g)
	return res
}

// --- INV-M3: no byte of a frame is ever rewritten -------------------------------

// TestRefileContentUnchanged (INV-M3) proves the move rewrites nothing: the file
// at the new path is byte-identical to the one that left the old path, and the
// recorded hash still describes it. No path in re-file copies bytes, which is what
// makes moving without a content precondition sufficient.
func TestRefileContentUnchanged(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	beforeHash := hashOnDisk(t, f.DestPath)

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	res := requireMoved(t, mustResults(t, h, g))

	if got := hashOnDisk(t, res.To); got != beforeHash {
		t.Fatalf("archive hash changed across the move: %q != %q (INV-M3)", got, beforeHash)
	}
	after := mustGetFrame(t, h, f.ID)
	if beforeHash != archiveHash(after) {
		t.Fatalf("the moved file no longer hashes to archiveHash: %q != %q", beforeHash, archiveHash(after))
	}
	if fileExists(f.DestPath) {
		t.Fatalf("the old path still holds a copy: %s", f.DestPath)
	}
}

// --- INV-M9: night ordinals are untouched ---------------------------------------

// TestRefilePreservesNight (INV-M9) proves re-file reuses the stored night
// verbatim: the row's night is unchanged, night_counters is unchanged (asking for
// the same ordinal again returns the same number), and the frame lands in the
// NIGHT_n its siblings share rather than a freshly minted one.
func TestRefilePreservesNight(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	before := mustGetFrame(t, h, f.ID)
	if before.Night == 0 {
		t.Fatal("setup: a filed light must carry a night ordinal")
	}
	key := "2025-12-31" // the observing date of the fixture's DATE-OBS is irrelevant; the counter key is
	n1, err := h.store.AssignNight(ctx, store.NightRequest{Target: before.Target, OTA: h.profile.OTA, ObservingDate: key})
	if err != nil {
		t.Fatal(err)
	}

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	res := requireMoved(t, mustResults(t, h, g))

	after := mustGetFrame(t, h, f.ID)
	if after.Night != before.Night {
		t.Fatalf("night=%d, want %d preserved (INV-M9)", after.Night, before.Night)
	}
	if !strings.Contains(res.To, "NIGHT_"+strconv.FormatInt(before.Night, 10)) {
		t.Fatalf("new path %q does not render the stored night NIGHT_%d", res.To, before.Night)
	}
	n2, err := h.store.AssignNight(ctx, store.NightRequest{Target: before.Target, OTA: h.profile.OTA, ObservingDate: key})
	if err != nil {
		t.Fatal(err)
	}
	if n2 != n1 {
		t.Fatalf("night_counters changed across the move: %d != %d (AssignNight must never be called)", n2, n1)
	}
}

// --- INV-M10: never a directory the operator did not see ------------------------

// TestRefileRefusesUnselectedDestination (INV-M10) proves apply re-routes every
// frame and refuses one whose re-derived directory is not the selected group's,
// rather than filing it somewhere the operator never saw in the diff.
func TestRefileRefusesUnselectedDestination(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	// The operator selected g.ToDir; the templates changed again underneath.
	h.cfg.Templates.Light = "<TARGET>/<OTA>/<SESSION>/somewhere-else/NIGHT_<n>"
	res, _ := applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileSkipped {
		t.Fatalf("want a single skipped result, got %+v", res)
	}
	if !strings.Contains(res[0].Reason, "not the selected destination") {
		t.Fatalf("reason=%q, want it to name the unselected destination", res[0].Reason)
	}
	after := mustGetFrame(t, h, f.ID)
	if after.DestPath != f.DestPath || !fileExists(f.DestPath) {
		t.Fatalf("a refused frame must not move: dest=%q", after.DestPath)
	}
}

// --- INV-M5 / design routing-and-filing AC30 / AC36: rows sharing a dest_path
// ---------------------------

// TestRefileUpdatesAllSharingRows (INV-M5, design routing-and-filing AC30) proves
// one move updates every row of the rig sharing the file's dest_path in one
// transaction — the identity four consumers depend on (CountLiveFramesByDest,
// CountFramesTonight, bytes-tonight, ResidentCount) — and that a dest_path shared
// with another rig is refused rather than moved.
func TestRefileUpdatesAllSharingRows(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	first := fileLight(t, h, refileSrc)

	// A second share holding the same bytes files to the same path (the idempotent
	// keep), leaving two rows on one dest_path.
	h.dev.AddShare(tf)
	h.profile.Sources = append(h.profile.Sources, model.Source{Share: tf, Roots: []string{"Autorun"}})
	h.dev.AddFile(tf, refileSrc, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.cycle()

	rows, err := h.store.FramesByDestPath(ctx, "rig-01", first.DestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("setup: want 2 rows sharing one dest_path, got %d", len(rows))
	}
	tonightBefore, err := h.store.CountFramesTonight(ctx, "rig-01", 0)
	if err != nil {
		t.Fatal(err)
	}

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	if g.Count != 1 {
		t.Fatalf("group Count=%d, want 1 — Count counts distinct dest_paths, not rows", g.Count)
	}
	if len(g.FrameIDs) != 2 {
		t.Fatalf("group FrameIDs=%v, want both rows", g.FrameIDs)
	}
	res := requireMoved(t, mustResults(t, h, g))

	for _, r := range rows {
		got := mustGetFrame(t, h, r.ID)
		if got.DestPath != res.To {
			t.Fatalf("row %d dest_path=%q, want %q — every sharing row moves together (INV-M5)", r.ID, got.DestPath, res.To)
		}
	}
	tonightAfter, err := h.store.CountFramesTonight(ctx, "rig-01", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tonightAfter != tonightBefore {
		t.Fatalf("CountFramesTonight changed across the migration: %d != %d", tonightAfter, tonightBefore)
	}
}

// TestRefileRefusesCrossRigSharer (INV-M5, design routing-and-filing AC30)
// proves a dest_path also referenced by ANOTHER rig's row is refused and
// reported: one goroutine owns a rig's frame mutations, so rig A's worker must
// never rewrite rig B's rows.
func TestRefileRefusesCrossRigSharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)

	other := &model.Frame{
		RigID: "rig-02", SourceShare: emmc, SourcePath: "Autorun/other.fit",
		SHA256: f.SHA256, Size: f.Size, Mtime: f.Mtime, FrameType: f.FrameType,
		Target: f.Target, Night: f.Night, DestPath: f.DestPath,
		State: model.StateVerified, PulledAt: f.PulledAt,
	}
	if err := h.store.UpsertFrame(ctx, other); err != nil {
		t.Fatal(err)
	}

	h.cfg.Templates.Light = refileTemplate
	pv := previewAll(t, h)
	if len(pv.Groups) != 0 {
		t.Fatalf("a cross-rig sharer must not be offered; groups=%+v", pv.Groups)
	}
	if len(pv.Refusals) != 1 || !strings.Contains(pv.Refusals[0].Reason, "another rig") {
		t.Fatalf("want one cross-rig refusal, got %+v", pv.Refusals)
	}
	res, _, err := h.w.refileGroup(ctx, filepath.Dir(f.DestPath), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != refileSkipped {
		t.Fatalf("apply must refuse a cross-rig sharer too: %+v", res)
	}
	if !fileExists(f.DestPath) {
		t.Fatalf("a refused frame's file must stay where it is")
	}
}

// TestRefileRefusesPathWithIneligibleSibling (design routing-and-filing AC36)
// proves eligibility is decided per dest_path, not per row: an ineligible sibling
// of a verified H12 row refuses the whole path, because moving on behalf of the
// eligible row would drag the ineligible one along and rewrite its path.
//
// The two cases are different branches of refileRowRefusal and reach it from
// different columns: a hold is a state, while a missing mark leaves the row
// verified and is exactly the case where the file's whereabouts are already in
// question — so a move keyed on the path it names is the last thing to attempt.
func TestRefileRefusesPathWithIneligibleSibling(t *testing.T) {
	cases := []struct {
		name   string
		spoil  func(t *testing.T, h *harness, sibling model.Frame)
		reason string
	}{
		{"held sibling", func(t *testing.T, h *harness, sibling model.Frame) {
			sibling.State = model.StateHeld
			if err := h.store.UpdateFrame(context.Background(), &sibling); err != nil {
				t.Fatal(err)
			}
		}, "held"},
		{"missing-marked sibling", func(t *testing.T, h *harness, sibling model.Frame) {
			h.setMissing(t, sibling.ID, 0)
		}, "marked missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			first := fileLight(t, h, refileSrc)
			h.dev.AddShare(tf)
			h.profile.Sources = append(h.profile.Sources, model.Source{Share: tf, Roots: []string{"Autorun"}})
			h.dev.AddFile(tf, refileSrc, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
			h.cycle()

			rows, err := h.store.FramesByDestPath(ctx, "rig-01", first.DestPath)
			if err != nil || len(rows) != 2 {
				t.Fatalf("setup: rows=%d err=%v", len(rows), err)
			}
			c.spoil(t, h, rows[1])

			h.cfg.Templates.Light = refileTemplate
			pv := previewAll(t, h)
			if len(pv.Groups) != 0 {
				t.Fatalf("a path with an ineligible sibling must not be offered; groups=%+v", pv.Groups)
			}
			if len(pv.Refusals) != 1 || !strings.Contains(pv.Refusals[0].Reason, c.reason) {
				t.Fatalf("want one refusal naming the sibling as %q, got %+v", c.reason, pv.Refusals)
			}
			res, _, err := h.w.refileGroup(ctx, filepath.Dir(first.DestPath), []int64{rows[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || res[0].Status != refileSkipped || !strings.Contains(res[0].Reason, c.reason) {
				t.Fatalf("apply must refuse the path too, naming %q: %+v", c.reason, res)
			}
			if mustGetFrame(t, h, rows[0].ID).DestPath != first.DestPath {
				t.Fatalf("the eligible row must not have moved")
			}
			if !fileExists(first.DestPath) {
				t.Fatalf("a refused path's file must stay where it is: %s", first.DestPath)
			}
		})
	}
}

// --- INV-M6 / design routing-and-filing AC27: pathless rows
// ----------------------------------------------

// TestRefileIgnoresPathlessRows (INV-M6, design routing-and-filing AC27) proves a
// row with no archived file is never offered and never moved — the authorised
// culled-original drain leaves a deleted row with a NULL dest_path and no missing
// mark, which passes every other refusal — and that the store refuses a move keyed
// on an empty path outright.
func TestRefileIgnoresPathlessRows(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)

	// A ResolveCulledFrame row: state deleted, dest_path NULL, no missing mark.
	del := *f
	del.State = model.StateDeleted
	del.DeletedAt = h.nowVal.Unix()
	if err := h.store.UpdateFrame(ctx, &del); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ResolveCulledFrame(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	// A markFailed row: dest_path is bound raw, so it stores a literal ''.
	h.w.markFailed(ctx, h.cfg, listedFrame{
		share: emmc, path: "Autorun/Light/broken.fit", name: "broken.fit",
		size: 10, mtime: h.nowVal,
	}, nil, model.FailOpenSource, "injected")

	h.cfg.Templates.Light = refileTemplate
	pv := previewAll(t, h)
	if len(pv.Groups) != 0 || len(pv.Refusals) != 0 {
		t.Fatalf("pathless rows must be invisible to the preview: groups=%+v refusals=%+v", pv.Groups, pv.Refusals)
	}
	if _, err := h.store.MoveDestPath(ctx, "rig-01", "", "/somewhere", store.RequireRows); !errors.Is(err, store.ErrEmptyDestPath) {
		t.Fatalf("MoveDestPath with an empty oldDest: err=%v, want ErrEmptyDestPath", err)
	}
	// Applying one anyway is skipped with a stated reason, never moved.
	res, _, err := h.w.refileGroup(ctx, filepath.Join(h.cfg.Roots.Light, "anything"), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != refileSkipped || !strings.Contains(res[0].Reason, "no archived file") {
		t.Fatalf("want a skipped pathless row with a stated reason, got %+v", res)
	}
}

// --- INV-M7 / INV-M8: unmanaged files and the prune -----------------------------

// TestRefileLeavesUnmanagedFiles (INV-M7) proves re-file moves nothing but the
// frames it was asked to move and creates no second copy of one: an operator's
// stacked master beside the frame is untouched, its directory survives, and the
// frame exists at exactly one path afterwards.
func TestRefileLeavesUnmanagedFiles(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	oldDir := filepath.Dir(f.DestPath)
	master := filepath.Join(oldDir, "master_Ha.fit")
	writeLocal(t, master, []byte("the operator's stacked master"))

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	if len(g.StrandRisk) != 1 || !strings.Contains(g.StrandRisk[0].Reason, "master_Ha.fit") {
		t.Fatalf("the preview must warn that %s will strand: %+v", oldDir, g.StrandRisk)
	}
	res, sum := applyGroup(t, h, g)
	moved := requireMoved(t, res)

	if b, err := os.ReadFile(master); err != nil || string(b) != "the operator's stacked master" {
		t.Fatalf("an unmanaged file was touched: %v", err)
	}
	if fileExists(f.DestPath) {
		t.Fatalf("the frame exists at both paths: %s and %s", f.DestPath, moved.To)
	}
	if len(sum.PrunedDirs) != 0 {
		t.Fatalf("a directory holding an operator file must not be pruned: %v", sum.PrunedDirs)
	}
	if len(sum.SkippedDirs) != 1 || sum.SkippedDirs[0].Dir != oldDir {
		t.Fatalf("the prune must report the directory it could not remove: %+v", sum.SkippedDirs)
	}
}

// TestPruneSkipsAndReportsNonEmpty (INV-M8) proves the prune never removes a
// non-empty directory and reports each one it skipped — the .DS_Store case the
// macOS-mounted NAS this feature was requested for produces on every vacated
// folder — while a directory that genuinely empties is removed up to, but never
// including, the configured root.
func TestPruneSkipsAndReportsNonEmpty(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	oldDir := filepath.Dir(f.DestPath)
	writeLocal(t, filepath.Join(oldDir, ".DS_Store"), []byte("finder"))

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	if len(g.StrandRisk) != 1 || !strings.Contains(g.StrandRisk[0].Reason, ".DS_Store") {
		t.Fatalf("the preview must name .DS_Store up front: %+v", g.StrandRisk)
	}
	_, sum := applyGroup(t, h, g)
	if len(sum.SkippedDirs) != 1 || sum.SkippedDirs[0].Dir != oldDir ||
		!strings.Contains(sum.SkippedDirs[0].Reason, ".DS_Store") {
		t.Fatalf("the prune must report the stranded directory and why: %+v", sum.SkippedDirs)
	}
	if !fileExists(oldDir) {
		t.Fatalf("a non-empty directory must never be removed")
	}

	// With the .DS_Store gone the same directory prunes, and the walk stops at the
	// configured light root rather than removing it.
	if err := os.Remove(filepath.Join(oldDir, ".DS_Store")); err != nil {
		t.Fatal(err)
	}
	pruned, skipped := h.w.pruneEmptyDirs(oldDir, scanRoots(h.cfg))
	if len(pruned) == 0 || fileExists(oldDir) {
		t.Fatalf("an emptied directory must be pruned: %v %+v", pruned, skipped)
	}
	if !fileExists(h.cfg.Roots.Light) {
		t.Fatalf("the prune must stop at the configured root, never remove it")
	}
	for _, p := range pruned {
		if p == h.cfg.Roots.Light || p == h.cfg.Roots.Cal {
			t.Fatalf("the prune removed a configured root: %v", pruned)
		}
	}
}

// TestRefilePrunesEmptiedDirectories proves the half of the feature without which
// it does not visibly solve the problem: after the last frame leaves a directory
// it is removed, and the walk continues up through the parents the migration
// emptied — otherwise migrating a mosaic leaves ten empty folders in exactly the
// listing the operator wanted cleaned up.
func TestRefilePrunesEmptiedDirectories(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	oldDir := filepath.Dir(f.DestPath)         // <light>/C-2025A6/FRA300/S/NIGHT_1
	oldSession := filepath.Dir(oldDir)         // <light>/C-2025A6/FRA300/S
	keep := filepath.Dir(filepath.Dir(oldDir)) // <light>/C-2025A6/FRA300, which keeps the new NIGHT_1

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	if len(g.StrandRisk) != 0 {
		t.Fatalf("a directory holding only the moving frames must not be flagged: %+v", g.StrandRisk)
	}
	_, sum := applyGroup(t, h, g)

	if fileExists(oldDir) || fileExists(oldSession) {
		t.Fatalf("the emptied directories must be pruned: %s, %s", oldDir, oldSession)
	}
	if len(sum.PrunedDirs) != 2 {
		t.Fatalf("want both emptied directories reported, got %v", sum.PrunedDirs)
	}
	if len(sum.SkippedDirs) != 0 {
		t.Fatalf("the ancestor that still holds the migrated tree is where the walk always stops, "+
			"and must not be reported as a failure: %+v", sum.SkippedDirs)
	}
	if !fileExists(keep) {
		t.Fatalf("the prune walked past a directory that still holds frames: %s", keep)
	}
}

// TestRefileGroupStopsOnCancel proves cancellation stops the migration between
// frames: a request cancelled before it starts moves nothing, and re-running the
// preview still shows the work (there is no resume state — re-running the diff is
// idempotent by construction).
func TestRefileGroupStopsOnCancel(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, sum, err := h.w.refileGroup(ctx, g.ToDir, g.FrameIDs)
	if err != nil {
		t.Fatalf("a cancelled group is not an error: %v", err)
	}
	if len(res) != 0 || len(sum.PrunedDirs) != 0 {
		t.Fatalf("a cancelled group must move and prune nothing: %+v %+v", res, sum)
	}
	if mustGetFrame(t, h, f.ID).DestPath != f.DestPath || !fileExists(f.DestPath) {
		t.Fatalf("a cancelled group must leave the frame alone")
	}
	if got := previewOneGroup(t, h); got.Count != 1 {
		t.Fatalf("the remaining work must still be offered: %+v", got)
	}
}

// TestPruneNeverRemovesAMarkedDirectory (INV-M8) proves a directory holding the
// library-link marker is reported and left in place whatever else it contains.
func TestPruneNeverRemovesAMarkedDirectory(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	dir := filepath.Join(h.cfg.Roots.Light, "Target", "OTA", "NIGHT_1")
	writeLocal(t, filepath.Join(dir, model.LibraryMarkerName), []byte(`{"id":"lib"}`))

	pruned, skipped := h.w.pruneEmptyDirs(dir, scanRoots(h.cfg))
	if len(pruned) != 0 || !fileExists(dir) {
		t.Fatalf("a marked directory must never be removed: pruned=%v", pruned)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, model.LibraryMarkerName) {
		t.Fatalf("the skip must name the marker: %+v", skipped)
	}
}

// --- Preview shape ---------------------------------------------------------------

// TestPreviewRefileNoTemplateChange (design routing-and-filing AC22) proves a
// library that already sits where the current templates say it belongs yields
// no groups at all — the operator is never offered a migration that would move
// nothing.
func TestPreviewRefileNoTemplateChange(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	fileLight(t, h, refileSrc)
	pv := previewAll(t, h)
	if len(pv.Groups) != 0 || len(pv.Refusals) != 0 {
		t.Fatalf("want an empty diff, got groups=%+v refusals=%+v", pv.Groups, pv.Refusals)
	}
	if pv.Scope != refileScopeAll {
		t.Fatalf("scope=%q", pv.Scope)
	}
}

// TestPreviewRefileMosaicScope proves the default scope filters to frames the
// current config would route through the mosaic template, and that it is empty
// while detection is off — with detection off no frame's template selection can
// have changed, which is why the UI offers that scope disabled rather than
// rendering an empty diff.
func TestPreviewRefileMosaicScope(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	// Relabel the frame to a pane name through the production retarget path, so the
	// stored target is one the mosaic parser accepts.
	if res, err := h.w.serviceRetarget(ctx, f.ID, "Sadr_1-1"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget to a pane name: status=%q err=%v", res.Status, err)
	}
	// A second, non-pane frame that must stay out of the mosaic scope.
	other := fileLight(t, h, "Autorun/Light/C-2025 A6/Light_0002.fit")

	pv, err := h.w.previewRefile(ctx, refileScopeMosaic)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Groups) != 0 {
		t.Fatalf("with mosaic detection off the mosaic scope is empty by construction: %+v", pv.Groups)
	}

	h.cfg.MosaicDetection = true
	pv, err = h.w.previewRefile(ctx, refileScopeMosaic)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Groups) != 1 || pv.Groups[0].Count != 1 {
		t.Fatalf("want exactly the pane frame in the mosaic scope, got %+v", pv.Groups)
	}
	if !strings.Contains(pv.Groups[0].ToDir, filepath.Join("Sadr", "FRA300", "S", "Sadr_1-1")) {
		t.Fatalf("the pane must route through the mosaic template: %q", pv.Groups[0].ToDir)
	}
	for _, id := range pv.Groups[0].FrameIDs {
		if id == other.ID {
			t.Fatalf("a non-pane frame leaked into the mosaic scope")
		}
	}
}

// TestPreviewRefileTreatsStoredScalarsAsPresent (design routing-and-filing AC29)
// proves the two details that must NOT be copied from previewRetarget: a
// legitimate SET-TEMP 0 / GAIN 0 is routed as present rather than reported as an
// unroutable frame, and each frame renders its STORED night rather than NIGHT_0.
func TestPreviewRefileTreatsStoredScalarsAsPresent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	// A cooled camera's cal frame recorded at exactly 0 °C and gain 0: presence is
	// not recoverable from the stored columns (UpsertFrame binds these raw, so a
	// legitimate zero is stored as 0 and never NULL), so the preview assumes it.
	dark := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Dark/Dark_0001.fit",
		SHA256: "d0", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		FrameType: string(model.TypeDark), Sensor: "2600MM",
		Gain: 0, SetTemp: 0, Exptime: 300, Binning: "Bin1",
		DateObs: "2026-01-01T22:00:00Z", DestPath: filepath.Join(h.cfg.Roots.Cal, "old", "d.fit"),
	}
	// A light whose stored night is 7: previewRetarget passes Night 0, and copying
	// that here would render NIGHT_0 for every frame in the library.
	light := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Light/M31/Light_0001.fit",
		SHA256: "l0", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		FrameType: string(model.TypeLight), Target: "M31", Night: 7,
		DateObs: "2026-01-01T22:00:00Z", DestPath: filepath.Join(h.cfg.Roots.Light, "old", "l.fit"),
	}
	for _, f := range []*model.Frame{dark, light} {
		if err := h.store.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("upsert %s: %v", f.SourcePath, err)
		}
	}

	pv := previewAll(t, h)
	if len(pv.Refusals) != 0 {
		t.Fatalf("a legitimate zero must not read as an absent token: %+v", pv.Refusals)
	}
	groups := map[string]model.RefileGroup{}
	for _, g := range pv.Groups {
		groups[g.FromDir] = g
	}
	if len(pv.Groups) != 2 {
		t.Fatalf("want both frames offered, got %+v", pv.Groups)
	}

	dg := groups[filepath.Dir(dark.DestPath)]
	if !strings.HasPrefix(dg.ToDir, h.cfg.Roots.Cal) {
		t.Fatalf("a cal frame must route under the cal root: %q", dg.ToDir)
	}
	if !strings.Contains(dg.ToDir, "gain0") || !strings.Contains(dg.ToDir, "0C") {
		t.Fatalf("the stored zeros must render as values: %q", dg.ToDir)
	}
	if !strings.HasSuffix(groups[filepath.Dir(light.DestPath)].ToDir, "NIGHT_7") {
		t.Fatalf("to=%q, want the stored night rendered (never NIGHT_0)", groups[filepath.Dir(light.DestPath)].ToDir)
	}
}

// TestPreviewRefileKeepsUncooledFrames proves a frame recorded without SET-TEMP
// re-files into the uncooled bucket: assuming presence, as the other scalars do,
// would render its stored 0 as 0C and mix it with darks cooled to 0C.
func TestPreviewRefileKeepsUncooledFrames(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	dark := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Dark/Dark_0001.fit",
		SHA256: "d0", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		FrameType: string(model.TypeDark), Sensor: "2600MM", SetTempAbsent: true,
		Gain: 100, Exptime: 300, Binning: "Bin1",
		DateObs: "2026-01-01T22:00:00Z", DestPath: filepath.Join(h.cfg.Roots.Cal, "old", "d.fit"),
	}
	if err := h.store.UpsertFrame(context.Background(), dark); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	pv := previewAll(t, h)
	if len(pv.Refusals) != 0 || len(pv.Groups) != 1 {
		t.Fatalf("want one group and no refusals: %+v", pv)
	}
	if to := pv.Groups[0].ToDir; !strings.Contains(to, route.Uncooled) || strings.Contains(to, "0C") {
		t.Fatalf("to=%q, want the uncooled bucket", to)
	}
}

// TestPreviewRefileReportsQuarantine proves a frame the CURRENT templates would
// quarantine is reported rather than moved to unclassified/: it has no
// destination, so it cannot be a group member either.
func TestPreviewRefileReportsQuarantine(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	// <SENSOR> is unresolvable for a light whose sensor column is empty.
	h.cfg.Templates.Light = "<TARGET>/<SENSOR>/NIGHT_<n>"
	got := mustGetFrame(t, h, f.ID)
	got.Sensor = ""
	if err := h.store.UpdateFrame(context.Background(), &got); err != nil {
		t.Fatal(err)
	}

	pv := previewAll(t, h)
	if len(pv.Groups) != 0 {
		t.Fatalf("a quarantining frame must never be offered: %+v", pv.Groups)
	}
	if len(pv.Refusals) != 1 || !strings.Contains(pv.Refusals[0].Reason, "cannot route") {
		t.Fatalf("want a stated quarantine refusal, got %+v", pv.Refusals)
	}
}

// --- Apply-time refusals ---------------------------------------------------------

// TestRefileRefusesIneligibleStates (design routing-and-filing AC28) proves every
// ineligible condition is skipped with a stated reason and never moved: a frame
// that is unclassified, dismissed, failed or held, or that carries a missing mark
// or a standing restore request, keeps its file where it is.
func TestRefileRefusesIneligibleStates(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	cases := []struct {
		name   string
		mutate func(f *model.Frame)
		want   string
	}{
		{"unclassified", func(f *model.Frame) { f.State = model.StateUnclassified }, "unclassified"},
		{"dismissed", func(f *model.Frame) { f.State = model.StateDismissed }, "dismissed"},
		{"failed", func(f *model.Frame) { f.State = model.StateFailed }, "failed"},
		{"held", func(f *model.Frame) { f.State = model.StateHeld }, "held"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := mustGetFrame(t, h, f.ID)
			row.State = model.StateVerified
			c.mutate(&row)
			if err := h.store.UpdateFrame(ctx, &row); err != nil {
				t.Fatal(err)
			}
			pv := previewAll(t, h)
			if len(pv.Groups) != 0 {
				t.Fatalf("an ineligible frame must not be offered: %+v", pv.Groups)
			}
			res, _ := applyGroup(t, h, g)
			if len(res) != 1 || res[0].Status != refileSkipped || !strings.Contains(res[0].Reason, c.want) {
				t.Fatalf("want a skipped result naming %q, got %+v", c.want, res)
			}
			if mustGetFrame(t, h, f.ID).DestPath != f.DestPath {
				t.Fatalf("an ineligible frame must not move")
			}
		})
	}

	// The two marks are refused on their own, with the row back in a live state.
	row := mustGetFrame(t, h, f.ID)
	row.State = model.StateVerified
	if err := h.store.UpdateFrame(ctx, &row); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, f.ID, 0)
	res, _ := applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileSkipped || !strings.Contains(res[0].Reason, "missing") {
		t.Fatalf("a missing-marked frame must be skipped with a reason, got %+v", res)
	}
	if pv := previewAll(t, h); len(pv.Groups) != 0 {
		t.Fatalf("a missing-marked frame must not be offered: %+v", pv.Groups)
	}

	// A standing restore request on a PRESENT frame: MarkRestoreInProgress sets it
	// during the truncated-copy replace, so a crash there leaves a known-short
	// archive with no missing mark to refuse the move on its behalf.
	if err := h.store.ClearFrameMissing(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.MarkRestoreInProgress(ctx, f.ID, h.nowVal.Unix()); err != nil {
		t.Fatal(err)
	}
	res, _ = applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileSkipped || !strings.Contains(res[0].Reason, "restore is pending") {
		t.Fatalf("a frame with a standing restore request must be skipped with a reason, got %+v", res)
	}
	if pv := previewAll(t, h); len(pv.Groups) != 0 {
		t.Fatalf("a frame with a standing restore request must not be offered: %+v", pv.Groups)
	}
	if mustGetFrame(t, h, f.ID).DestPath != f.DestPath || !fileExists(f.DestPath) {
		t.Fatalf("a refused frame must not move")
	}
}

// TestRefileSkipsFrameIneligibleSincePreview (design routing-and-filing AC34)
// proves each row is re-read immediately before its move: a frame that stopped
// being eligible between preview and apply is skipped and reported rather than
// moved from a stale snapshot.
func TestRefileSkipsFrameIneligibleSincePreview(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	// The operator culls the NAS copy after previewing; the scan marks it missing.
	h.setMissing(t, f.ID, 0)

	res, sum := applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileSkipped {
		t.Fatalf("want the stale frame skipped, got %+v", res)
	}
	if len(sum.PrunedDirs) != 0 {
		t.Fatalf("nothing moved, so nothing may be pruned: %v", sum.PrunedDirs)
	}
	if mustGetFrame(t, h, f.ID).DestPath != f.DestPath {
		t.Fatalf("a skipped frame's row must not change")
	}
}

// --- Already-there destinations ---------------------------------------------------

// TestRefileAdoptsIdenticalDestination (design routing-and-filing AC35) proves a
// destination already holding a byte-identical copy of the frame is treated as
// already moved rather than disambiguated into a second copy — the state an
// interrupted earlier run leaves — and that the copy left at the old path is
// reported rather than deleted.
func TestRefileAdoptsIdenticalDestination(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	// An earlier interrupted run already placed the file at its destination.
	want := filepath.Join(g.ToDir, filepath.Base(f.DestPath))
	body, err := os.ReadFile(f.DestPath)
	if err != nil {
		t.Fatal(err)
	}
	writeLocal(t, want, body)

	res := requireMoved(t, mustResults(t, h, g))
	if res.To != want {
		t.Fatalf("to=%q, want the identical copy adopted at %q (never a second copy)", res.To, want)
	}
	if !strings.Contains(res.Reason, "byte-identical") {
		t.Fatalf("the adoption must be reported: %q", res.Reason)
	}
	if mustGetFrame(t, h, f.ID).DestPath != want {
		t.Fatalf("the row must point at the adopted copy")
	}
	if !fileExists(f.DestPath) {
		t.Fatalf("re-file must not delete archived data: the old copy should be left and reported")
	}
	entries, err := os.ReadDir(g.ToDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the destination must hold exactly one copy, got %d", len(entries))
	}
}

// TestRefileCaseOnlyRenameAdoptsTheSameFile (design routing-and-filing AC35)
// covers the case-only rename on a case-insensitive volume — the deployment this
// feature was requested for — where RenameNoReplace returns EEXIST against the
// file ITSELF. Advancing blindly there would permanently append a date token to a
// frame that was already correct. The volume's behaviour is injected so the test
// is deterministic on a case-sensitive filesystem too.
func TestRefileCaseOnlyRenameAdoptsTheSameFile(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	// A template edit that changes only the case of the leaf segment.
	h.cfg.Templates.Light = "<TARGET>/<OTA>/<SESSION>/night_<n>"
	h.lfs.RenameErr = func(oldpath, newpath string) error {
		if oldpath != newpath && strings.EqualFold(oldpath, newpath) {
			return localfs.ErrExist // one file, two spellings
		}
		return nil
	}
	g := previewOneGroup(t, h)
	res := requireMoved(t, mustResults(t, h, g))

	want := filepath.Join(g.ToDir, filepath.Base(f.DestPath))
	if res.To != want {
		t.Fatalf("to=%q, want %q — a case-only rename must not append a disambiguation token", res.To, want)
	}
	if mustGetFrame(t, h, f.ID).DestPath != want {
		t.Fatalf("the row must be repointed at the re-cased path")
	}
}

// TestRefileRefusesCrossVolumeRename proves EXDEV refuses with a per-frame reason
// rather than falling back to a copy: carrying an untested copy-verify-delete
// branch is exactly what would make re-file rewrite bytes, and it is the reason a
// move needs no content precondition at all.
func TestRefileRefusesCrossVolumeRename(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	h.lfs.RenameErr = func(string, string) error { return localfs.ErrCrossDevice }

	res, sum := applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileFailed || !strings.Contains(res[0].Reason, "another volume") {
		t.Fatalf("want a failed result naming the volume boundary, got %+v", res)
	}
	if !fileExists(f.DestPath) || mustGetFrame(t, h, f.ID).DestPath != f.DestPath {
		t.Fatalf("a refused cross-volume move must leave the frame untouched")
	}
	if len(openIntents(t, h)) != 0 {
		t.Fatalf("nothing is in flight after a refused rename; the intent must be cleared")
	}
	if len(sum.PrunedDirs) != 0 {
		t.Fatalf("nothing moved, so nothing may be pruned: %v", sum.PrunedDirs)
	}
}

// --- Commit failure ---------------------------------------------------------------

// TestRefileCommitFailureRenamesBack (design routing-and-filing AC37) proves a
// database write that fails without a crash is handled in-process: the file is
// moved back and the intent dropped, so the ordinary machinery never sees a row
// pointing at a vanished path.
func TestRefileCommitFailureRenamesBack(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	fs := &moveFailStore{Store: h.store, fail: errors.New("injected: database is locked")}
	h.w.store = fs

	res, _ := applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileFailed || !strings.Contains(res[0].Reason, "moved back") {
		t.Fatalf("want a failed result reporting the rename back, got %+v", res)
	}
	fs.fail = nil
	if !fileExists(f.DestPath) {
		t.Fatalf("a failed commit must put the file back at %s", f.DestPath)
	}
	if fileExists(res[0].To) {
		t.Fatalf("the file must not be left at the new path: %s", res[0].To)
	}
	if got := mustGetFrame(t, h, f.ID); got.DestPath != f.DestPath {
		t.Fatalf("the row must be unchanged: %q", got.DestPath)
	}
	if len(openIntents(t, h)) != 0 {
		t.Fatalf("a resolved commit failure must drop its intent")
	}
	if h.hasAlert(model.AlertRefile) {
		t.Fatalf("a recovered commit failure must not raise the critical alert")
	}
}

// TestRefileCommitFailureAlertsWhenRenameBackFails (design routing-and-filing
// AC37, INV-M2) proves the emergency route: when the file cannot be put back, the
// intent SURVIVES — which is what keeps the frame out of DeletionEligible however
// the move failed — and a critical alert is raised. Visibility alone would not be
// prevention: the settle window would still elapse.
func TestRefileCommitFailureAlertsWhenRenameBackFails(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	fs := &moveFailStore{Store: h.store, fail: errors.New("injected: disk full")}
	h.w.store = fs
	h.lfs.RenameErr = func(oldpath, _ string) error {
		if oldpath != f.DestPath { // the rename BACK, whose source is the new path
			return errors.New("injected: rename back failed")
		}
		return nil
	}

	res, _ := applyGroup(t, h, g)
	fs.fail = nil
	if len(res) != 1 || res[0].Status != refileFailed || !strings.Contains(res[0].Reason, "could not be moved back") {
		t.Fatalf("want a failed result reporting the failed rename back, got %+v", res)
	}
	if !h.hasAlert(model.AlertRefile) {
		t.Fatalf("want a %s alert; got %v", model.AlertRefile, h.raisedCategories())
	}
	if h.alertSeverity(model.AlertRefile) != model.SeverityCritical {
		t.Fatalf("the failed rename back must be critical, got %q", h.alertSeverity(model.AlertRefile))
	}
	intents := openIntents(t, h)
	if !intents[f.DestPath] {
		t.Fatalf("the intent must survive so refile_in_flight defers deletion; got %v", intents)
	}
	// INV-M2: the frame is not deletion-eligible while the intent is open, even
	// though the ordinary machinery will mark it missing.
	row := mustGetFrame(t, h, f.ID)
	reasons := absentDeferReasons(row, map[string]bool{h.cfg.Roots.Light: true}, intents, noIntents, 0, h.nowVal.Unix())
	if !containsReason(reasons, "refile_in_flight") {
		t.Fatalf("absentDeferReasons=%v, want refile_in_flight (INV-M2)", reasons)
	}
}

// TestRefileCommitFailureNeverReleasesTheDeviceOriginal (design
// routing-and-filing AC37, INV-M2) is the last mile the two tests above stop
// short of, joined in one run and reached through execution rather than
// argument: the intent it defers on is the one a failed commit left behind, not
// one written by hand.
//
// A commit failure whose rename back also fails leaves the file at the new path
// and the row at the old one, so the ordinary machinery takes over — the scan
// marks the frame missing, which design routing-and-filing AC37 says it DOES,
// because what refile_in_flight guarantees is not that the mark never appears but
// that it never makes the frame deletion-eligible. A settle window later the real
// delete pass runs against a device original that is still the operator's only
// other copy. Clearing the intent, as startup recovery would, releases it — which
// is what proves the deferral did the work rather than some incidental
// disqualifier.
func TestRefileCommitFailureNeverReleasesTheDeviceOriginal(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	h.linkRoot(t, h.cfg.Roots.Light)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	fs := &moveFailStore{Store: h.store, fail: errors.New("injected: disk full")}
	h.w.store = fs
	h.lfs.RenameErr = func(oldpath, _ string) error {
		if oldpath != f.DestPath { // the rename BACK, whose source is the new path
			return errors.New("injected: rename back failed")
		}
		return nil
	}
	res, _ := applyGroup(t, h, g)
	fs.fail, h.w.store, h.lfs.RenameErr = nil, h.store, nil

	if len(res) != 1 || res[0].Status != refileFailed || !strings.Contains(res[0].Reason, "could not be moved back") {
		t.Fatalf("want a failed result reporting the failed rename back, got %+v", res)
	}
	if !fileExists(res[0].To) || fileExists(f.DestPath) {
		t.Fatalf("setup: the file must be stranded at %s with nothing at %s", res[0].To, f.DestPath)
	}
	if got := mustGetFrame(t, h, f.ID); got.DestPath != f.DestPath {
		t.Fatalf("setup: the row must still name the old path, got %q", got.DestPath)
	}
	if !openIntents(t, h)[f.DestPath] {
		t.Fatalf("setup: the intent must survive the failed rename back")
	}
	if h.alertSeverity(model.AlertRefile) != model.SeverityCritical {
		t.Fatalf("want a critical %s alert, got %q", model.AlertRefile, h.alertSeverity(model.AlertRefile))
	}

	// The ordinary machinery now runs: the scan finds nothing at the row's path.
	h.nowVal = h.nowVal.Add(7 * time.Hour) // past grace, and the scan is due
	h.cycle()
	marked, _ := h.getFrame(f.SourcePath)
	if marked.MissingSeenAt == 0 {
		t.Fatal("the scan marks a re-file-failed frame missing; refile_in_flight promises only that the mark never authorizes a delete")
	}

	// A settle window later the real delete pass runs — and finds the frame
	// disqualified for exactly one reason.
	h.nowVal = h.nowVal.Add(25 * time.Hour)
	h.cycle()

	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a frame whose re-file failed must never have its device original deleted (INV-M2)")
	}
	settled, _ := h.getFrame(f.SourcePath)
	reasons := absentDeferReasons(*settled, map[string]bool{h.cfg.Roots.Light: true},
		openIntents(t, h), noIntents, 24*time.Hour, h.nowVal.Unix())
	if len(reasons) != 1 || reasons[0] != "refile_in_flight" {
		t.Fatalf("reasons=%v, want refile_in_flight alone — nothing else may be doing this work", reasons)
	}
	if !h.logContains("reasons=refile_in_flight") {
		t.Fatal("the delete pass must name refile_in_flight as the reason it deferred")
	}
	if settled.State != model.StateVerified {
		t.Fatalf("state=%s, want the row left alone by the deferral", settled.State)
	}

	// Recovery clears the intent; the same frame is then released as any cull is,
	// so the deferral — not an incidental disqualifier — was what held it.
	if err := h.store.ClearRefileIntent(ctx, "rig-01", f.DestPath); err != nil {
		t.Fatal(err)
	}
	h.cycle()
	if h.deviceHas(f.SourcePath) {
		t.Fatal("clearing the intent must restore eligibility; nothing else was blocking this delete")
	}
}

// TestRefileVanishedRowIsNotRestored proves the one commit failure that must NOT
// rename back: a zero-row update means the row was legitimately pruned mid-move,
// so restoring the file would re-create something nothing references.
func TestRefileVanishedRowIsNotRestored(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	fs := &moveFailStore{Store: h.store, fail: store.ErrNoRowsMoved}
	h.w.store = fs

	res, _ := applyGroup(t, h, g)
	fs.fail = nil
	if len(res) != 1 || res[0].Status != refileFailed || !strings.Contains(res[0].Reason, "row was removed") {
		t.Fatalf("want a failed result reporting the vanished row, got %+v", res)
	}
	if fileExists(f.DestPath) || !fileExists(res[0].To) {
		t.Fatalf("the file must be left at its new path, not restored")
	}
	if len(openIntents(t, h)) != 0 {
		t.Fatalf("the intent must be cleared: the row is gone, not mid-move")
	}
}

// --- Interaction with the rest of the lifecycle ------------------------------------

// TestRefileRetargetedFrameSurvives (design routing-and-filing AC25) proves a
// retargeted frame survives a re-file: archiveHash still resolves to the
// overlay, the archive on disk still hashes to it, and the delete pass's
// three-way check raises no delete_mismatch. Reusing the filing path would have
// reset retarget_sha256 and held the frame.
func TestRefileRetargetedFrameSurvives(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true

	f := fileLight(t, h, refileSrc)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	before := mustGetFrame(t, h, f.ID)
	if before.RetargetSHA256 == "" {
		t.Fatal("setup: expected a retarget overlay")
	}

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	res := requireMoved(t, mustResults(t, h, g))

	after := mustGetFrame(t, h, f.ID)
	if after.RetargetSHA256 != before.RetargetSHA256 || after.SHA256 != before.SHA256 {
		t.Fatalf("the overlay must survive the move: sha=%q retarget=%q", after.SHA256, after.RetargetSHA256)
	}
	if hd := hashOnDisk(t, res.To); hd != archiveHash(after) {
		t.Fatalf("the moved archive %q does not hash to archiveHash %q", hd, archiveHash(after))
	}
	h.nowVal = h.nowVal.Add(2 * time.Hour) // past grace
	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")
	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("a re-filed retargeted frame must not trip the three-way check; got %v", h.raisedCategories())
	}
	if got := mustGetFrame(t, h, f.ID); got.State != model.StateDeleted {
		t.Fatalf("state=%s, want the device original released as usual", got.State)
	}
}

// TestRefileDeletedFrameStaysDeleted (design routing-and-filing AC26) proves a
// frame whose device original is already reclaimed is moved without being
// resurrected: it stays deleted, keeps its reclaim clock, never re-enters
// DeletionEligible against an original that no longer exists, and does not inflate
// ResidentCount.
func TestRefileDeletedFrameStaysDeleted(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)

	del := mustGetFrame(t, h, f.ID)
	del.State = model.StateDeleted
	del.DeletedAt = h.nowVal.Add(-time.Hour).Unix()
	if err := h.store.UpdateFrame(ctx, &del); err != nil {
		t.Fatal(err)
	}
	residentBefore, err := h.store.ResidentCount(ctx, "rig-01")
	if err != nil {
		t.Fatal(err)
	}

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	requireMoved(t, mustResults(t, h, g))

	after := mustGetFrame(t, h, f.ID)
	if after.State != model.StateDeleted || after.DeletedAt != del.DeletedAt {
		t.Fatalf("a deleted frame must stay deleted with its reclaim clock: state=%s deleted_at=%d", after.State, after.DeletedAt)
	}
	elig, err := h.store.DeletionEligible(ctx, "rig-01", h.nowVal.Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range elig {
		if e.ID == f.ID {
			t.Fatalf("a re-filed deleted frame must not re-enter DeletionEligible")
		}
	}
	residentAfter, err := h.store.ResidentCount(ctx, "rig-01")
	if err != nil {
		t.Fatal(err)
	}
	if residentAfter != residentBefore {
		t.Fatalf("ResidentCount changed across the move: %d != %d", residentAfter, residentBefore)
	}
}

// TestRefileRepullFiresNoDifferOrReadopt (design routing-and-filing AC33) proves
// what the design insisted be demonstrated rather than reasoned about: re-file
// changes only dest_path, leaving sha256 and the (rig, share, path) key intact,
// so a later cycle over the same device source takes neither the differ nor the
// readopt branch of decide() and raises no frame_lost alert.
func TestRefileRepullFiresNoDifferOrReadopt(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	res := requireMoved(t, mustResults(t, h, g))

	h.cycle() // the device original is still there, unchanged

	after := mustGetFrame(t, h, f.ID)
	if after.DestPath != res.To {
		t.Fatalf("a re-pull re-filed the frame: dest=%q, want %q", after.DestPath, res.To)
	}
	if after.SHA256 != f.SHA256 || after.State != model.StateVerified {
		t.Fatalf("row disturbed by the cycle: sha=%q state=%s", after.SHA256, after.State)
	}
	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("a re-filed frame must not raise frame_lost; got %v", h.raisedCategories())
	}
	if h.logContains("differ-branch") {
		t.Fatalf("decide() took the differ branch after a re-file")
	}
}

// --- Worker wiring and cancellation ------------------------------------------------

// TestRefileGroupThroughWorkerLoop exercises the concurrency wiring end to end —
// RefileGroup → the refileReqs channel → the Run select → refileGroup — rather than
// the move primitive in isolation. A re-file request is serviced ONLY in the Run
// select, never in drainCommands, so nothing else in the suite proves it is
// reachable at all. The per-request timeout makes broken wiring fail fast instead
// of hanging the suite.
func TestRefileGroupThroughWorkerLoop(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.w.Run(ctx); close(done) }()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	res, _, err := h.w.RefileGroup(reqCtx, g.ToDir, g.FrameIDs)
	reqCancel()
	cancel()
	<-done

	if err != nil {
		t.Fatalf("RefileGroup through the worker loop: %v", err)
	}
	moved := requireMoved(t, res)
	if got := mustGetFrame(t, h, f.ID); got.DestPath != moved.To {
		t.Fatalf("dest_path=%q, want %q", got.DestPath, moved.To)
	}
	if !fileExists(moved.To) {
		t.Fatalf("the file must be at its new path: %s", moved.To)
	}
}

// cancelAfterMoveStore cancels the migration's context the moment the first commit
// lands, standing in for the operator cancelling the apply — or the HTTP client
// going away — part-way through a group.
type cancelAfterMoveStore struct {
	store.Store
	cancel context.CancelFunc
	moves  int
}

func (s *cancelAfterMoveStore) MoveDestPath(ctx context.Context, rigID, oldDest, newDest string, policy store.MoveDestPolicy) (int, error) {
	n, err := s.Store.MoveDestPath(ctx, rigID, oldDest, newDest, policy)
	s.moves++
	if s.moves == 1 {
		s.cancel()
	}
	return n, err
}

// secondArchivedLight puts a second archived file beside f, with its own row, so
// one group carries two files and a cancellation has somewhere to stop between
// them.
func secondArchivedLight(t *testing.T, h *harness, f *model.Frame) model.Frame {
	t.Helper()
	ctx := context.Background()
	data, err := os.ReadFile(f.DestPath)
	if err != nil {
		t.Fatalf("read %s: %v", f.DestPath, err)
	}
	path := filepath.Join(filepath.Dir(f.DestPath), "Light_0002.fit")
	writeLocal(t, path, data)
	// A faithful sibling: the routing scalars are copied verbatim, so the two frames
	// route identically and land in one group.
	sibling := *f
	sibling.ID = 0
	sibling.SourcePath = "Autorun/Light/C-2025 A6/Light_0002.fit"
	sibling.DestPath = path
	if err := h.store.UpsertFrame(ctx, &sibling); err != nil {
		t.Fatalf("upsert the second archived frame: %v", err)
	}
	rows, err := h.store.FramesByDestPath(ctx, "rig-01", path)
	if err != nil || len(rows) != 1 {
		t.Fatalf("FramesByDestPath(%s): rows=%d err=%v", path, len(rows), err)
	}
	return rows[0]
}

// TestRefileCancelledMidGroupResumesOnRerun (design routing-and-filing AC38)
// proves cancellation stops the migration between frames and that re-running
// finishes it: the context is cancelled as the first commit lands, the second
// frame is left exactly where it was, a fresh preview offers precisely the
// remaining work, and a second apply completes it. There is no resume state
// because re-running the diff is idempotent by construction.
func TestRefileCancelledMidGroupResumesOnRerun(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	first := fileLight(t, h, refileSrc)
	second := secondArchivedLight(t, h, first)

	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	if g.Count != 2 || len(g.FrameIDs) != 2 {
		t.Fatalf("setup: want one group of two files, got %+v", g)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.w.store = &cancelAfterMoveStore{Store: h.store, cancel: cancel}
	res, _, err := h.w.refileGroup(ctx, g.ToDir, g.FrameIDs)
	h.w.store = h.store
	if err != nil {
		t.Fatalf("a cancelled group is not an error: %v", err)
	}
	if len(res) != 1 || res[0].Status != refileMoved {
		t.Fatalf("want exactly one frame moved before the cancel took effect, got %+v", res)
	}

	// One of the two moved; the other is untouched, file and row alike.
	stalled := second
	if res[0].FrameID == second.ID {
		stalled = *first
	}
	if got := mustGetFrame(t, h, stalled.ID); got.DestPath != stalled.DestPath {
		t.Fatalf("the cancelled frame's row moved: %q, want %q", got.DestPath, stalled.DestPath)
	}
	if !fileExists(stalled.DestPath) {
		t.Fatalf("the cancelled frame's file moved: %s", stalled.DestPath)
	}

	// The remaining work is simply offered again, and a second apply completes it.
	rest := previewOneGroup(t, h)
	if rest.Count != 1 || len(rest.FrameIDs) != 1 || rest.FrameIDs[0] != stalled.ID {
		t.Fatalf("the preview must offer exactly the frame the cancel left behind: %+v", rest)
	}
	finished := requireMoved(t, mustResults(t, h, rest))
	if got := mustGetFrame(t, h, stalled.ID); got.DestPath != finished.To {
		t.Fatalf("dest_path=%q, want %q", got.DestPath, finished.To)
	}
	if !fileExists(finished.To) || fileExists(stalled.DestPath) {
		t.Fatalf("the second apply must leave the file only at %s", finished.To)
	}
	if len(previewAll(t, h).Groups) != 0 {
		t.Fatalf("the migration must be complete after the second apply")
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
