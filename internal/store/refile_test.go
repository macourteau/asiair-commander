package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestFrameColumnsRoundTripEveryColumn is the guard against positional drift
// between frameColumns, scanFrame's Scan argument list and UpsertFrame's INSERT
// list — three hand-maintained orderings with nothing but this test enforcing that
// they agree. It writes a DISTINCT value into every column and asserts the whole
// struct comes back equal, so a swapped pair of same-typed columns fails here
// rather than silently mis-filing frames in production.
func TestFrameColumnsRoundTripEveryColumn(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	in := &model.Frame{
		RigID:            "rig-42",
		SourceShare:      "Udisk Images",
		SourcePath:       "Autorun/Light_001.fit",
		SHA256:           "sha-original",
		Size:             123_456_789,
		Mtime:            1_700_000_001,
		DataSHA256:       "sha-data-unit",
		ArchiveSHA256:    "sha-archive",
		ArchiveSize:      123_456_790,
		DeleteDeferCount: 0, // UpsertFrame resets it; TestUpsertFrameResetsTheDeferralCounter owns that rule
		RetargetSHA256:   "sha-overlay",
		RetargetFrom:     "OldTarget",
		FrameType:        "LIGHT",
		Target:           "Sadr_2-3",
		Filter:           "Ha",
		Exptime:          301.5,
		SetTemp:          -9.5,
		Gain:             101,
		Offset:           31,
		Binning:          "2x2",
		Sensor:           "2600MM",
		DateObs:          "2026-07-15T02:03:04Z",
		Night:            7,
		DestPath:         "/nas/light/Sadr/FRA300/S/Sadr_2-3/NIGHT_7/Light_001.fit",
		Sidecars:         []model.Sidecar{{Path: "Autorun/Light_001_thn.jpg", Size: 4096, Mtime: 1_700_000_002}},
		State:            model.StateVerified,
		Attempts:         3,
		NextRetryAt:      1_700_000_003,
		PulledAt:         1_700_000_004,
		DeletedAt:        1_700_000_005,
		Error:            "a recorded failure reason",
		PanelBase:        "Sadr",
		PanelRow:         2,
		PanelCol:         3,
		Astrometry:       &model.Astrometry{Status: model.AstroStatusHeader, Solution: &model.WCSSolution{CenterRA: 12.5, CenterDec: -3.25}},
	}
	if err := s.UpsertFrame(ctx, in); err != nil {
		t.Fatalf("UpsertFrame: %v", err)
	}
	// Copied AFTER the upsert: it assigns ID and reflects the effective astrometry
	// back onto the struct. The three missing_* columns are deliberately absent from
	// the INSERT (the upsert always clears them), so they stay zero here.
	want := *in

	got, ok, err := s.GetFrameByID(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("frame did not round-trip column-for-column:\n got %+v\nwant %+v", *got, want)
	}

	// A second write on the same natural key must carry the pane columns through the
	// conflict branch too — they follow target, which a retarget rewrites.
	in2 := *in
	in2.Target = "NGC6888"
	in2.PanelBase, in2.PanelRow, in2.PanelCol = "", 0, 0
	if err := s.UpsertFrame(ctx, &in2); err != nil {
		t.Fatalf("UpsertFrame update: %v", err)
	}
	got2, _, err := s.GetFrameByID(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetFrameByID after update: %v", err)
	}
	if got2.PanelBase != "" || got2.PanelRow != 0 || got2.PanelCol != 0 {
		t.Fatalf("pane columns must follow target out of a pane: %q %d %d",
			got2.PanelBase, got2.PanelRow, got2.PanelCol)
	}
}

// TestUpgradeV7ToV8BackfillsPanes pins design routing-and-filing AC21: the
// migration adds the pane columns and backfills every existing row by parsing its
// stored target — including rows whose target carries no pane suffix, which read
// back as empty base and zero row/col.
//
// The "NGC 7000_1-1" row is the load-bearing case, and its expected values are the
// ones internal/rig's TestFiledFramePanesComeFromRawTarget asserts for a frame
// filed live: the backfill parses the RAW target, so a base containing a space must
// survive here exactly as the filing path records it, whatever strip_target_spaces
// is set to.
func TestUpgradeV7ToV8BackfillsPanes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []func(context.Context, *sql.Tx) error{
		migrate1, migrate2, migrate3, migrate4, migrate5, migrate6, migrate7,
	} {
		if err := m(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version','7')`); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		path, target string
	}{
		{"pane.fit", "Sadr_1-1"},
		{"pane-hi.fit", "Sadr_2-10"},
		{"spaced.fit", "NGC 7000_1-1"},
		{"plain.fit", "NGC6888"},
		{"dated.fit", "M42_2026-07"}, // out of the plausibility bound: not a pane
		{"padded.fit", "Sadr_01-02"}, // zero-padded indices still parse
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO frames(rig_id,source_share,source_path,size,mtime,state,dest_path,target)
			 VALUES('rig','Udisk',?,1,1,'verified','/nas/'||?,?)`, r.path, r.path, r.target); err != nil {
			t.Fatal(err)
		}
	}
	// A row with no target at all — a failed transfer never got as far as a header.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO frames(rig_id,source_share,source_path,size,mtime,state)
		 VALUES('rig','Udisk','notarget.fit',1,1,'failed')`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer s.Close()

	// Open runs the whole registry, so the upgrade lands at the current version
	// rather than stopping at the migration under test.
	if v, err := s.(*sqlStore).schemaVersion(ctx); err != nil || v != model.CurrentStoreSchemaVersion {
		t.Fatalf("schema version after upgrade = %d (err %v), want %d", v, err, model.CurrentStoreSchemaVersion)
	}

	want := map[string]struct {
		base     string
		row, col int
	}{
		"pane.fit":     {"Sadr", 1, 1},
		"pane-hi.fit":  {"Sadr", 2, 10},
		"spaced.fit":   {"NGC 7000", 1, 1},
		"plain.fit":    {"", 0, 0},
		"dated.fit":    {"", 0, 0},
		"padded.fit":   {"Sadr", 1, 2},
		"notarget.fit": {"", 0, 0},
	}
	for p, w := range want {
		f, ok, err := s.GetFrame(ctx, "rig", "Udisk", p)
		if err != nil || !ok {
			t.Fatalf("GetFrame %s: ok=%v err=%v", p, ok, err)
		}
		if f.PanelBase != w.base || f.PanelRow != w.row || f.PanelCol != w.col {
			t.Errorf("%s: panes = %q %d %d, want %q %d %d",
				p, f.PanelBase, f.PanelRow, f.PanelCol, w.base, w.row, w.col)
		}
	}

	// The intent table and the dest_path index are part of the same migration.
	if _, err := s.(*sqlStore).read.ExecContext(ctx, `SELECT 1 FROM refile_intents LIMIT 1`); err != nil {
		t.Fatalf("refile_intents must exist after migrate8: %v", err)
	}
	var idx int
	if err := s.(*sqlStore).read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_frames_dest_path'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx_frames_dest_path missing after migrate8")
	}
}

// filedFrame upserts a minimal verified frame and returns its id.
func filedFrame(t *testing.T, s Store, rigID, share, path, dest string) int64 {
	t.Helper()
	f := &model.Frame{
		RigID: rigID, SourceShare: share, SourcePath: path,
		SHA256: "h-" + path, Size: 10, Mtime: 1, FrameType: "LIGHT", Target: "Sadr_1-1",
		DestPath: dest, State: model.StateVerified, PulledAt: 1_700_000_000,
		PanelBase: "Sadr", PanelRow: 1, PanelCol: 1,
	}
	if err := s.UpsertFrame(context.Background(), f); err != nil {
		t.Fatalf("UpsertFrame %s: %v", path, err)
	}
	return f.ID
}

// TestRefileCandidatesNeverOffersAPathlessRow is the store half of design
// routing-and-filing AC27: a row with a NULL or empty dest_path names no archived
// file, so re-file must never offer it. Neither case is implied by the state
// filter — both are built here the way production builds them.
func TestRefileCandidatesNeverOffersAPathlessRow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	keep := filedFrame(t, s, "rig", "Udisk", "keep.fit", "/nas/keep.fit")

	// A ResolveCulledFrame row: markDeleted leaves state='deleted', then the resolve
	// nulls dest_path and every reconciliation column — no missing mark survives, so
	// only the dest_path predicate can refuse it.
	culled := filedFrame(t, s, "rig", "Udisk", "culled.fit", "/nas/culled.fit")
	if err := s.SetState(ctx, culled, model.StateDeleted); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveCulledFrame(ctx, culled); err != nil {
		t.Fatal(err)
	}

	// A markFailed row: UpsertFrame binds dest_path raw rather than through nullStr,
	// so a frame filed with no destination stores a literal '' — not NULL.
	failed := &model.Frame{
		RigID: "rig", SourceShare: "Udisk", SourcePath: "failed.fit",
		Size: 10, Mtime: 1, State: model.StateFailed, Attempts: 1,
	}
	if err := s.UpsertFrame(ctx, failed); err != nil {
		t.Fatal(err)
	}
	var destIsEmptyString bool
	if err := s.(*sqlStore).read.QueryRowContext(ctx,
		`SELECT dest_path = '' FROM frames WHERE id=?`, failed.ID).Scan(&destIsEmptyString); err != nil {
		t.Fatal(err)
	}
	if !destIsEmptyString {
		t.Fatalf("a failed row is expected to store a literal empty dest_path, not NULL")
	}
	// The same empty dest_path in an ELIGIBLE state, so the '' predicate is proven
	// on its own rather than incidentally by the state filter.
	blank := &model.Frame{
		RigID: "rig", SourceShare: "Udisk", SourcePath: "blank.fit",
		SHA256: "h", Size: 10, Mtime: 1, State: model.StateVerified, PulledAt: 1,
	}
	if err := s.UpsertFrame(ctx, blank); err != nil {
		t.Fatal(err)
	}

	// The other refusals the query owns.
	marked := filedFrame(t, s, "rig", "Udisk", "marked.fit", "/nas/marked.fit")
	if err := s.SetFrameMissing(ctx, marked, 1_700_000_500); err != nil {
		t.Fatal(err)
	}
	restoring := filedFrame(t, s, "rig", "Udisk", "restoring.fit", "/nas/restoring.fit")
	if err := s.MarkRestoreInProgress(ctx, restoring, 1_700_000_500); err != nil {
		t.Fatal(err)
	}
	held := filedFrame(t, s, "rig", "Udisk", "held.fit", "/nas/held.fit")
	if err := s.SetState(ctx, held, model.StateHeld); err != nil {
		t.Fatal(err)
	}
	otherRig := filedFrame(t, s, "rig-2", "Udisk", "other.fit", "/nas/other.fit")

	got, err := s.RefileCandidates(ctx, "rig")
	if err != nil {
		t.Fatalf("RefileCandidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != keep {
		var ids []int64
		for _, f := range got {
			ids = append(ids, f.ID)
		}
		t.Fatalf("RefileCandidates returned %v, want only the eligible row %d (culled=%d failed=%d blank=%d marked=%d restoring=%d held=%d other-rig=%d)",
			ids, keep, culled, failed.ID, blank.ID, marked, restoring, held, otherRig)
	}
	for _, f := range got {
		if f.DestPath == "" {
			t.Fatalf("a candidate must never carry an empty dest_path: %+v", f)
		}
	}
}

// TestMoveDestPathRefusesEmptyOldDest is the other half of design
// routing-and-filing AC27: an empty match key is never a legitimate move, and
// would rewrite every empty-dest_path row of the rig in one transaction.
func TestMoveDestPathRefusesEmptyOldDest(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, p := range []string{"a.fit", "b.fit"} {
		f := &model.Frame{
			RigID: "rig", SourceShare: "Udisk", SourcePath: p,
			Size: 10, Mtime: 1, State: model.StateVerified, PulledAt: 1,
		}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.MoveDestPath(ctx, "rig", "", "/nas/new.fit", RequireRows)
	if !errors.Is(err, ErrEmptyDestPath) {
		t.Fatalf("MoveDestPath with an empty oldDest = (%d, %v), want ErrEmptyDestPath", n, err)
	}
	// Tolerating a zero-row update must not weaken the refusal.
	if _, err := s.MoveDestPath(ctx, "rig", "", "/nas/new.fit", TolerateNoRows); !errors.Is(err, ErrEmptyDestPath) {
		t.Fatalf("the empty-key refusal must hold under TolerateNoRows too, got %v", err)
	}
	var rewritten int
	if err := s.(*sqlStore).read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frames WHERE dest_path = '/nas/new.fit'`).Scan(&rewritten); err != nil {
		t.Fatal(err)
	}
	if rewritten != 0 {
		t.Fatalf("%d pathless rows were rewritten by a refused move", rewritten)
	}
}

// TestMoveDestPathMovesSharingRowsTogether is the store half of design
// routing-and-filing AC30: a frame present on both the USB and eMMC shares is two
// rows and one file, so one move updates both — and the counts that dedupe on
// dest_path stay put across it. A sharer on another rig is left alone: one
// goroutine owns a rig's frame mutations.
func TestMoveDestPathMovesSharingRowsTogether(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const from = "/nas/light/Sadr_1-1/NIGHT_1/Light_001.fit"
	const to = "/nas/light/Sadr/FRA300/S/Sadr_1-1/NIGHT_1/Light_001.fit"
	usb := filedFrame(t, s, "rig", "Udisk", "Autorun/Light_001.fit", from)
	emmc := filedFrame(t, s, "rig", "EMMC", "Autorun/Light_001.fit", from)
	foreign := filedFrame(t, s, "rig-2", "Udisk", "Autorun/Light_001.fit", from)

	before, err := s.CountFramesTonight(ctx, "rig", 0)
	if err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatalf("two rows sharing one dest_path must count once, got %d", before)
	}
	// The un-rig-scoped count is what makes a cross-rig sharer detectable at all.
	if n, err := s.CountFramesByDestAnyRig(ctx, from); err != nil || n != 3 {
		t.Fatalf("CountFramesByDestAnyRig = %d (err %v), want 3", n, err)
	}

	n, err := s.MoveDestPath(ctx, "rig", from, to, RequireRows)
	if err != nil {
		t.Fatalf("MoveDestPath: %v", err)
	}
	if n != 2 {
		t.Fatalf("MoveDestPath updated %d rows, want both sharing rows", n)
	}

	for _, id := range []int64{usb, emmc} {
		f, _, err := s.GetFrameByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if f.DestPath != to {
			t.Errorf("frame %d dest_path = %q, want %q", id, f.DestPath, to)
		}
	}
	f, _, err := s.GetFrameByID(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if f.DestPath != from {
		t.Errorf("another rig's row must not move: dest_path = %q", f.DestPath)
	}

	after, err := s.CountFramesTonight(ctx, "rig", 0)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("CountFramesTonight changed across the move: %d -> %d", before, after)
	}
}

// TestMoveDestPathZeroRowPolicy pins the asymmetry the crash-safety design rests
// on: on the live apply path a vanished row is a failure the caller must report
// (PruneDeleted can delete one under it), while recovery re-runs a commit that may
// already have landed and must tolerate matching nothing.
func TestMoveDestPathZeroRowPolicy(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.MoveDestPath(ctx, "rig", "/nas/gone.fit", "/nas/new.fit", RequireRows); !errors.Is(err, ErrNoRowsMoved) {
		t.Fatalf("a zero-row move must fail by default, got %v", err)
	}
	n, err := s.MoveDestPath(ctx, "rig", "/nas/gone.fit", "/nas/new.fit", TolerateNoRows)
	if err != nil {
		t.Fatalf("recovery must tolerate a zero-row move: %v", err)
	}
	if n != 0 {
		t.Fatalf("row count = %d, want 0", n)
	}
}

// TestFramesByDestPath covers what recovery needs from it: the rows at a path, all
// of them, rig-scoped — and nothing at all for a path no row names, which is how
// "the row was pruned while the intent was open" is told apart from "the row is
// still at from".
func TestFramesByDestPath(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const dest = "/nas/light/Sadr_1-1/NIGHT_1/Light_001.fit"
	filedFrame(t, s, "rig", "Udisk", "Autorun/Light_001.fit", dest)
	filedFrame(t, s, "rig", "EMMC", "Autorun/Light_001.fit", dest)
	filedFrame(t, s, "rig-2", "Udisk", "Autorun/Light_001.fit", dest)

	got, err := s.FramesByDestPath(ctx, "rig", dest)
	if err != nil {
		t.Fatalf("FramesByDestPath: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want both of the rig's sharing rows", len(got))
	}
	none, err := s.FramesByDestPath(ctx, "rig", "/nas/nowhere.fit")
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown path = %v (err %v), want no rows", none, err)
	}
	if _, err := s.FramesByDestPath(ctx, "rig", ""); !errors.Is(err, ErrEmptyDestPath) {
		t.Fatalf("an empty match key must be refused here too, got %v", err)
	}
}

// TestRefileIntentsRoundTrip covers the intent record's contract: it upserts on
// (rig_id, from_path) so the EEXIST retry can rewrite the destination, it is
// listed fleet-wide for startup recovery, it is exposed rig-scoped by vacated path
// for absentDeferReasons, and clearing one that is not there is not an error.
func TestRefileIntentsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	in := RefileIntent{RigID: "rig", FromPath: "/nas/old.fit", ToPath: "/nas/new.fit", CreatedAt: 1_700_000_000}
	if err := s.PutRefileIntent(ctx, in); err != nil {
		t.Fatalf("PutRefileIntent: %v", err)
	}
	// The retry ladder picked another candidate name for the same source.
	retry := in
	retry.ToPath = "/nas/new_ab12cd34.fit"
	retry.CreatedAt = 1_700_000_010
	if err := s.PutRefileIntent(ctx, retry); err != nil {
		t.Fatalf("PutRefileIntent retry: %v", err)
	}
	other := RefileIntent{RigID: "rig-2", FromPath: "/nas/other.fit", ToPath: "/nas/other-new.fit", CreatedAt: 1_700_000_020}
	if err := s.PutRefileIntent(ctx, other); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListRefileIntents(ctx)
	if err != nil {
		t.Fatalf("ListRefileIntents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d intents, want one per (rig, from_path): %+v", len(all), all)
	}
	if all[0] != retry {
		t.Fatalf("the retry must have rewritten the intent in place: %+v", all[0])
	}

	paths, err := s.OpenIntentPaths(ctx, "rig")
	if err != nil {
		t.Fatalf("OpenIntentPaths: %v", err)
	}
	if len(paths) != 1 || !paths["/nas/old.fit"] {
		t.Fatalf("OpenIntentPaths = %v, want the rig's vacated path only", paths)
	}

	if err := s.ClearRefileIntent(ctx, "rig", "/nas/old.fit"); err != nil {
		t.Fatalf("ClearRefileIntent: %v", err)
	}
	// Every failure path clears unconditionally, so a second clear must be a no-op.
	if err := s.ClearRefileIntent(ctx, "rig", "/nas/old.fit"); err != nil {
		t.Fatalf("clearing an absent intent must not error: %v", err)
	}
	left, err := s.ListRefileIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].RigID != "rig-2" {
		t.Fatalf("remaining intents = %+v, want only the other rig's", left)
	}
}
