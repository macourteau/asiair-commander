package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// mkMissingVerified upserts a verified frame, marks it missing at `seen`, and
// returns its id. Built through the real production path (UpsertFrame +
// SetFrameMissing), not hand-written rows.
func mkMissingVerified(t *testing.T, s Store, rig, path, dest string, seen int64) int64 {
	t.Helper()
	ctx := context.Background()
	f := &model.Frame{RigID: rig, SourceShare: "Udisk", SourcePath: path,
		Size: 1, Mtime: 1, State: model.StateVerified, DestPath: dest, SHA256: "h"}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert %s: %v", path, err)
	}
	if err := s.SetFrameMissing(ctx, f.ID, seen); err != nil {
		t.Fatalf("set missing %s: %v", path, err)
	}
	return f.ID
}

func getFrame(t *testing.T, s Store, id int64) *model.Frame {
	t.Helper()
	f, ok, err := s.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("get frame %d: ok=%v err=%v", id, ok, err)
	}
	return f
}

// setAckedRaw writes missing_acked_at directly, to construct the design
// library-reconciliation §10.5 intermediate state (acked with no live mark) that
// no production method can reach.
func setAckedRaw(t *testing.T, s Store, id, at int64) {
	t.Helper()
	if _, err := s.(*sqlStore).write.ExecContext(context.Background(),
		`UPDATE frames SET missing_acked_at=? WHERE id=?`, at, id); err != nil {
		t.Fatalf("raw set acked: %v", err)
	}
}

// TestSetFrameMissingClearsStaleAcknowledgement pins the design
// library-reconciliation §10.5 enforcement point: a FRESH mark clears any stale
// acknowledgement, but a re-observation of a standing mark leaves a live
// acknowledgement intact.
func TestSetFrameMissingClearsStaleAcknowledgement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Case 1: a fresh mark clears a stale ack. Construct the intermediate state (the
	// file came back, a clear forgot the ack), then re-mark.
	f := &model.Frame{RigID: "rig", SourceShare: "Udisk", SourcePath: "a.fit",
		Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "/lib/a.fit", SHA256: "h"}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	setAckedRaw(t, s, f.ID, 500) // acked, but missing_seen_at is NULL
	if err := s.SetFrameMissing(ctx, f.ID, 1000); err != nil {
		t.Fatalf("set missing: %v", err)
	}
	got := getFrame(t, s, f.ID)
	if got.MissingSeenAt != 1000 {
		t.Fatalf("fresh mark not set: %d", got.MissingSeenAt)
	}
	if got.MissingAckedAt != 0 {
		t.Fatalf("fresh mark must clear the stale ack, got %d", got.MissingAckedAt)
	}

	// Case 2: a re-observation of a standing mark does NOT clear a live ack.
	id := mkMissingVerified(t, s, "rig", "b.fit", "/lib/b.fit", 2000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 2100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	// Re-observe: SetFrameMissing's WHERE missing_seen_at IS NULL must not fire.
	if err := s.SetFrameMissing(ctx, id, 9999); err != nil {
		t.Fatalf("re-observe: %v", err)
	}
	got = getFrame(t, s, id)
	if got.MissingSeenAt != 2000 {
		t.Fatalf("re-observation restarted the settle clock: %d", got.MissingSeenAt)
	}
	if got.MissingAckedAt != 2100 {
		t.Fatalf("re-observation stomped a live acknowledgement: %d", got.MissingAckedAt)
	}
}

// TestClearMissingUnderRootClearsAcknowledgement pins the relink path — the one the
// first draft's enumeration missed (design library-reconciliation §10.5).
func TestClearMissingUnderRootClearsAcknowledgement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := mkMissingVerified(t, s, "rig", "a.fit", "/lib/M31/a.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 1100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}

	n, err := s.ClearMissingUnderRoot(ctx, "/lib")
	if err != nil {
		t.Fatalf("clear under root: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 cleared, got %d", n)
	}
	got := getFrame(t, s, id)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 {
		t.Fatalf("relink must clear both columns: seen=%d acked=%d", got.MissingSeenAt, got.MissingAckedAt)
	}
}

// TestClearFrameMissingClearsAcknowledgement — the scan-sees-file-back path.
func TestClearFrameMissingClearsAcknowledgement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := mkMissingVerified(t, s, "rig", "a.fit", "/lib/a.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 1100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	if err := s.ClearFrameMissing(ctx, id); err != nil {
		t.Fatalf("clear frame missing: %v", err)
	}
	got := getFrame(t, s, id)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 {
		t.Fatalf("ClearFrameMissing must clear both: seen=%d acked=%d", got.MissingSeenAt, got.MissingAckedAt)
	}
}

// TestUpsertFrameConflictClearsAcknowledgement — a re-file (ON CONFLICT) clears both
// reconciliation columns.
func TestUpsertFrameConflictClearsAcknowledgement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := mkMissingVerified(t, s, "rig", "a.fit", "/lib/a.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 1100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	// Re-file the same (rig, share, path): the copy is present and verified again.
	again := &model.Frame{RigID: "rig", SourceShare: "Udisk", SourcePath: "a.fit",
		Size: 1, Mtime: 2, State: model.StateVerified, DestPath: "/lib/a.fit", SHA256: "h2"}
	if err := s.UpsertFrame(ctx, again); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got := getFrame(t, s, id)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 {
		t.Fatalf("re-file must clear both: seen=%d acked=%d", got.MissingSeenAt, got.MissingAckedAt)
	}
}

// TestDismissFrameClearsAcknowledgement — a tombstone clears both columns.
func TestDismissFrameClearsAcknowledgement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := mkMissingVerified(t, s, "rig", "a.fit", "/lib/a.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 1100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	if err := s.DismissFrame(ctx, id); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	got := getFrame(t, s, id)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 {
		t.Fatalf("dismiss must clear both: seen=%d acked=%d", got.MissingSeenAt, got.MissingAckedAt)
	}
}

// TestRequestRestoreClearsAcknowledgement — Restore beats Acknowledge (design
// library-reconciliation §10.4).
func TestRequestRestoreClearsAcknowledgement(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := mkMissingVerified(t, s, "rig", "a.fit", "/lib/a.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 1100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RequestRestore(ctx, "rig", id, 1200); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}
	got := getFrame(t, s, id)
	if got.RestoreRequestedAt != 1200 {
		t.Fatalf("restore not recorded: %d", got.RestoreRequestedAt)
	}
	if got.MissingAckedAt != 0 {
		t.Fatalf("restore must clear the acknowledgement, got %d", got.MissingAckedAt)
	}
	if got.MissingSeenAt != 1000 {
		t.Fatalf("restore must keep the missing mark: %d", got.MissingSeenAt)
	}
}

// TestAcknowledgeMissingRefusesUnmarkedRestorePendingAndNonVerified pins the SQL
// guard alone: acknowledgement applies only to a verified, marked-missing,
// no-restore, not-already-acked row.
func TestAcknowledgeMissingRefusesUnmarkedRestorePendingAndNonVerified(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Unmarked verified frame: no missing_seen_at → refused.
	unmarked := &model.Frame{RigID: "rig", SourceShare: "Udisk", SourcePath: "u.fit",
		Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "/lib/u.fit", SHA256: "h"}
	if err := s.UpsertFrame(ctx, unmarked); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if ok, err := s.AcknowledgeMissing(ctx, "rig", unmarked.ID, 10); err != nil || ok {
		t.Fatalf("unmarked frame must be refused: ok=%v err=%v", ok, err)
	}

	// Restore-pending frame → refused.
	rp := mkMissingVerified(t, s, "rig", "r.fit", "/lib/r.fit", 1000)
	if ok, err := s.RequestRestore(ctx, "rig", rp, 1100); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AcknowledgeMissing(ctx, "rig", rp, 1200); err != nil || ok {
		t.Fatalf("restore-pending frame must be refused: ok=%v err=%v", ok, err)
	}

	// Non-verified (held) frame that is marked missing → refused.
	held := &model.Frame{RigID: "rig", SourceShare: "Udisk", SourcePath: "h.fit",
		Size: 1, Mtime: 1, State: model.StateHeld, DestPath: "/lib/h.fit", SHA256: "h"}
	if err := s.UpsertFrame(ctx, held); err != nil {
		t.Fatalf("upsert held: %v", err)
	}
	if err := s.SetFrameMissing(ctx, held.ID, 1000); err != nil {
		t.Fatalf("mark held missing: %v", err)
	}
	if ok, err := s.AcknowledgeMissing(ctx, "rig", held.ID, 1200); err != nil || ok {
		t.Fatalf("held frame must be refused: ok=%v err=%v", ok, err)
	}

	// Already acknowledged → refused (missing_acked_at IS NULL guard).
	acked := mkMissingVerified(t, s, "rig", "k.fit", "/lib/k.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", acked, 1100); err != nil || !ok {
		t.Fatalf("first ack: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AcknowledgeMissing(ctx, "rig", acked, 1200); err != nil || ok {
		t.Fatalf("re-ack must report no change: ok=%v err=%v", ok, err)
	}
	if got := getFrame(t, s, acked); got.MissingAckedAt != 1100 {
		t.Fatalf("re-ack must not overwrite the first vouch time: %d", got.MissingAckedAt)
	}

	// Wrong rig → refused.
	wr := mkMissingVerified(t, s, "rig", "w.fit", "/lib/w.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "other", wr, 1100); err != nil || ok {
		t.Fatalf("cross-rig ack must be refused: ok=%v err=%v", ok, err)
	}
}

// design library-reconciliation I35 (AC6, SQL): the per-frame AcknowledgeMissing
// accepts a culled unclassified frame (proven-archived), while the GROUP
// AcknowledgeMissingByIDs stays verified-only and skips it — the guard that keeps a
// group-ack off the flat quarantine directory.
func TestAcknowledgeMissingUnclassifiedPerFrameVsGroup(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	mkMissingUnclassified := func(path string) int64 {
		f := &model.Frame{RigID: "rig", SourceShare: "Udisk", SourcePath: path,
			Size: 1, Mtime: 1, State: model.StateUnclassified, DestPath: "/lib/" + path, SHA256: "h"}
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
		if err := s.SetFrameMissing(ctx, f.ID, 1000); err != nil {
			t.Fatalf("set missing %s: %v", path, err)
		}
		return f.ID
	}

	// Per-frame: a culled unclassified frame IS acknowledgeable.
	perFrame := mkMissingUnclassified("pf.fit")
	if ok, err := s.AcknowledgeMissing(ctx, "rig", perFrame, 1100); err != nil || !ok {
		t.Fatalf("per-frame ack of a culled unclassified frame must apply: ok=%v err=%v", ok, err)
	}

	// Group: the SAME class is skipped (verified-only), so no rows are acknowledged.
	grp := mkMissingUnclassified("grp.fit")
	n, err := s.AcknowledgeMissingByIDs(ctx, "rig", []int64{grp}, 1100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("group ack must skip an unclassified frame (stays verified-only); acknowledged %d", n)
	}
	if got := getFrame(t, s, grp); got.MissingAckedAt != 0 {
		t.Fatalf("group ack must not stamp an unclassified frame; got %d", got.MissingAckedAt)
	}
}

// TestAcknowledgeMissingByIDsScopesToRigAndGuards pins that another rig's id in the
// slice is untouched, and the per-row guards apply.
func TestAcknowledgeMissingByIDsScopesToRigAndGuards(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	a := mkMissingVerified(t, s, "rig", "a.fit", "/lib/d/a.fit", 1000)
	b := mkMissingVerified(t, s, "rig", "b.fit", "/lib/d/b.fit", 1000)
	other := mkMissingVerified(t, s, "other", "o.fit", "/lib/d/o.fit", 1000)
	// A restore-pending row on the target rig must be skipped by the guard.
	rp := mkMissingVerified(t, s, "rig", "r.fit", "/lib/d/r.fit", 1000)
	if ok, err := s.RequestRestore(ctx, "rig", rp, 1100); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}

	n, err := s.AcknowledgeMissingByIDs(ctx, "rig", []int64{a, b, other, rp}, 2000)
	if err != nil {
		t.Fatalf("ack by ids: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 acknowledged (a,b), got %d", n)
	}
	if getFrame(t, s, a).MissingAckedAt != 2000 || getFrame(t, s, b).MissingAckedAt != 2000 {
		t.Fatal("a and b must be acknowledged")
	}
	if got := getFrame(t, s, other); got.MissingAckedAt != 0 {
		t.Fatalf("another rig's id must be untouched, got %d", got.MissingAckedAt)
	}
	if got := getFrame(t, s, rp); got.MissingAckedAt != 0 {
		t.Fatalf("a restore-pending row must be skipped, got %d", got.MissingAckedAt)
	}
}

// TestMissingUnderDirIsExactAndAgeIndependent pins byte-exact directory matching (a
// sibling dir, a nested subdirectory, and a case-differing dir are all excluded) and
// that a same-dir row is returned regardless of how old its missing mark is — the
// acknowledge set must match the unfiltered panel.
func TestMissingUnderDirIsExactAndAgeIndependent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	inDir := mkMissingVerified(t, s, "rig", "m.fit", "/a/b/m.fit", 1000)
	_ = mkMissingVerified(t, s, "rig", "sib.fit", "/a/c/sib.fit", 1000)   // sibling dir
	_ = mkMissingVerified(t, s, "rig", "nest.fit", "/a/b/n/q.fit", 1000)  // nested subdir
	_ = mkMissingVerified(t, s, "rig", "case.fit", "/a/B/case.fit", 1000) // case-differing dir
	// Same dir, marked missing far in the past: must still be returned (no age cutoff).
	old := mkMissingVerified(t, s, "rig", "old.fit", "/a/b/old.fit", 1)

	frames, err := s.MissingUnderDir(ctx, "rig", "/a/b")
	if err != nil {
		t.Fatalf("missing under dir: %v", err)
	}
	got := map[int64]bool{}
	for _, f := range frames {
		got[f.ID] = true
	}
	if len(frames) != 2 || !got[inDir] || !got[old] {
		var ids []int64
		for _, f := range frames {
			ids = append(ids, f.ID)
		}
		t.Fatalf("expected exactly the two /a/b rows [%d %d], got %v", inDir, old, ids)
	}
}

// TestMissingGroupCountsAggregatesNonAdjacentDirs pins the grouping fix: rows in the
// same dir that are non-adjacent under ORDER BY dest_path must yield one group.
func TestMissingGroupCountsAggregatesNonAdjacentDirs(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Under ORDER BY dest_path these interleave: /a/b/m, /a/b/n/q, /a/b/z — the
	// /a/b/n/q row (dir /a/b/n) sits between the two /a/b rows.
	mkMissingVerified(t, s, "rig", "m.fit", "/a/b/m.fit", 1000)
	mkMissingVerified(t, s, "rig", "q.fit", "/a/b/n/q.fit", 1000)
	mkMissingVerified(t, s, "rig", "z.fit", "/a/b/z.fit", 1000)

	groups, err := s.MissingGroupCounts(ctx, "rig")
	if err != nil {
		t.Fatalf("group counts: %v", err)
	}
	got := map[string]int{}
	for _, g := range groups {
		if _, dup := got[g.Dir]; dup {
			t.Fatalf("dir %q emitted more than once: %+v", g.Dir, groups)
		}
		got[g.Dir] = g.Count
	}
	if got["/a/b"] != 2 {
		t.Fatalf("expected one /a/b group with count 2, got %+v", groups)
	}
	if got["/a/b/n"] != 1 {
		t.Fatalf("expected one /a/b/n group with count 1, got %+v", groups)
	}
	// Result must be sorted by dir.
	for i := 1; i < len(groups); i++ {
		if groups[i-1].Dir > groups[i].Dir {
			t.Fatalf("groups not sorted by dir: %+v", groups)
		}
	}
}

// TestResolveCulledFrameRetiresRow pins design library-reconciliation §10.6:
// nulling dest_path alongside the reconciliation columns takes the row out of
// the scan's dest_path predicate.
func TestResolveCulledFrameRetiresRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id := mkMissingVerified(t, s, "rig", "a.fit", "/lib/a.fit", 1000)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", id, 1100); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	if err := s.ResolveCulledFrame(ctx, id); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := getFrame(t, s, id)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 || got.RestoreRequestedAt != 0 || got.DestPath != "" {
		t.Fatalf("resolve must null all four: seen=%d acked=%d restore=%d dest=%q",
			got.MissingSeenAt, got.MissingAckedAt, got.RestoreRequestedAt, got.DestPath)
	}
}

// TestUpgradeV5ToV6AddsAckColumn pins the migration: a v5 DB migrates to v6 with
// missing_acked_at present and NULL for existing rows.
func TestUpgradeV5ToV6AddsAckColumn(t *testing.T) {
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
	for _, m := range []func(context.Context, *sql.Tx) error{migrate1, migrate2, migrate3, migrate4, migrate5} {
		if err := m(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version','5')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO frames(rig_id,source_share,source_path,size,mtime,state,dest_path,sha256,missing_seen_at)
		 VALUES('rig','Udisk','x.fit',1,1,'verified','/nas/x.fit','h',1000)`); err != nil {
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

	if v, err := s.(*sqlStore).schemaVersion(ctx); err != nil || v != model.CurrentStoreSchemaVersion {
		t.Fatalf("schema version after upgrade = %d (err %v), want %d", v, err, model.CurrentStoreSchemaVersion)
	}
	var acked sql.NullInt64
	if err := s.(*sqlStore).read.QueryRowContext(ctx,
		`SELECT missing_acked_at FROM frames WHERE source_path='x.fit'`).Scan(&acked); err != nil {
		t.Fatalf("column must exist after migrate6: %v", err)
	}
	if acked.Valid {
		t.Fatalf("existing row's missing_acked_at must be NULL, got %v", acked)
	}
}
