package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestMigrateHeaderEdits is HE-12 (design header-edits §5.2): the migration carries
// every retargeted row's overlay into edit_sha256 and an OBJECT entry of
// header_edits whose from is the old retarget_from, leaves an unretargeted row with
// no overlay, and an intent open across the upgrade recovers as the revision it is.
func TestMigrateHeaderEdits(t *testing.T) {
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
	for _, m := range migrations {
		if m.version > 11 {
			break
		}
		if err := m.up(ctx, tx); err != nil {
			t.Fatalf("migrate%d: %v", m.version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version','11')`); err != nil {
		t.Fatal(err)
	}
	const dest = "/lib/M31/Light_0001.fit"
	for _, q := range []string{
		// Retargeted from "C-2025 A6" to M31.
		`INSERT INTO frames(rig_id,source_share,source_path,sha256,size,mtime,state,dest_path,target,
			retarget_sha256,retarget_from,archive_sha256,archive_size,pulled_at)
		 VALUES('rig','Udisk','rt.fit','device-rt',100,10,'verified','` + dest + `','M31',
			'edited-rt','C-2025 A6','edited-rt',100,1000)`,
		// Retargeted with no original recorded: from must be null, not "".
		`INSERT INTO frames(rig_id,source_share,source_path,sha256,size,mtime,state,dest_path,target,
			retarget_sha256,retarget_from,archive_sha256,archive_size)
		 VALUES('rig','Udisk','rt-nofrom.fit','device-nf',100,10,'deleted','/lib/M42/Light_0002.fit','M42',
			'edited-nf','','edited-nf',100)`,
		// Never retargeted; an empty retarget_sha256 means the same.
		`INSERT INTO frames(rig_id,source_share,source_path,sha256,size,mtime,state,dest_path,target,
			archive_sha256,archive_size)
		 VALUES('rig','Udisk','plain.fit','device-plain',100,10,'verified','/lib/NGC6888/Light_0003.fit','NGC6888',
			'device-plain',100)`,
		`INSERT INTO frames(rig_id,source_share,source_path,sha256,size,mtime,state,dest_path,target,
			retarget_sha256,archive_sha256,archive_size)
		 VALUES('rig','Udisk','empty.fit','device-empty',100,10,'verified','/lib/NGC7000/Light_0004.fit','NGC7000',
			'','device-empty',100)`,
		// A device revision of the retargeted row, left open by a crash.
		`INSERT INTO supersede_intents(rig_id,dest_path,aside_path,frame_id,sha256,size,mtime,
			archive_sha256,archive_size,retarget_sha256,state,pulled_at,created_at)
		 VALUES('rig','` + dest + `','` + dest + `.superseding-abc',
			(SELECT id FROM frames WHERE source_path='rt.fit'),
			'device-rt-revised',102980,20,'edited-rt-revised',102980,'edited-rt-revised','verified',2000,3000)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed v11: %v\n%s", err, q)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer st.Close()
	s := st.(*sqlStore)
	if v, err := s.schemaVersion(ctx); err != nil || v != model.CurrentStoreSchemaVersion {
		t.Fatalf("schema version after upgrade = %d (err %v), want %d", v, err, model.CurrentStoreSchemaVersion)
	}

	byPath := map[string]model.Frame{}
	for _, p := range []string{"rt.fit", "rt-nofrom.fit", "plain.fit", "empty.fit"} {
		f, ok, err := s.GetFrame(ctx, "rig", "Udisk", p)
		if err != nil {
			t.Fatalf("%s: GetFrame: %v", p, err)
		}
		if ok {
			byPath[p] = *f
		}
	}
	from := "C-2025 A6"
	for _, tc := range []struct {
		path      string
		wantSHA   string
		wantEdits model.HeaderEdits
	}{
		{"rt.fit", "edited-rt", model.HeaderEdits{{Key: "OBJECT", Value: "M31", From: &from}}},
		{"rt-nofrom.fit", "edited-nf", model.HeaderEdits{{Key: "OBJECT", Value: "M42", From: nil}}},
		{"plain.fit", "", nil},
		{"empty.fit", "", nil},
	} {
		f, ok := byPath[tc.path]
		if !ok {
			t.Fatalf("%s: row lost in the migration", tc.path)
		}
		if f.EditSHA256 != tc.wantSHA || !reflect.DeepEqual(f.HeaderEdits, tc.wantEdits) {
			t.Fatalf("%s: edit_sha256=%q header_edits=%+v, want %q %+v", tc.path, f.EditSHA256, f.HeaderEdits, tc.wantSHA, tc.wantEdits)
		}
	}
	// The JSON is exactly the §5.2 shape, from an explicit null when none was recorded.
	var nf string
	if err := s.read.QueryRowContext(ctx, `SELECT header_edits FROM frames WHERE source_path='rt-nofrom.fit'`).Scan(&nf); err != nil {
		t.Fatal(err)
	}
	if want := `[{"key":"OBJECT","value":"M42","from":null}]`; nf != want {
		t.Fatalf("header_edits = %s, want %s", nf, want)
	}
	var plainNull bool
	if err := s.read.QueryRowContext(ctx, `SELECT header_edits IS NULL FROM frames WHERE source_path='plain.fit'`).Scan(&plainNull); err != nil || !plainNull {
		t.Fatalf("an unretargeted row's header_edits must be NULL: null=%v err=%v", plainNull, err)
	}

	// The dest_path index survives the rename and the drop.
	var idx int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_frames_dest_path'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("idx_frames_dest_path after upgrade: count=%d err=%v", idx, err)
	}

	// The open intent reads back as a revision and commits as one: the device anchors
	// and lifecycle columns advance, the overlay hash advances on the row carrying
	// one, and the OBJECT entry is untouched.
	intents, err := s.ListSupersedeIntents(ctx)
	if err != nil || len(intents) != 1 {
		t.Fatalf("ListSupersedeIntents: n=%d err=%v", len(intents), err)
	}
	in := intents[0]
	if in.Kind != SupersedeRevision || in.EditSHA256 != "edited-rt-revised" || in.HeaderEdits != nil || in.Filter != "" {
		t.Fatalf("migrated intent = %+v, want a revision carrying its overlay hash", in)
	}
	found, _, err := s.CommitSupersede(ctx, in)
	if err != nil || !found {
		t.Fatalf("CommitSupersede(migrated): found=%v err=%v", found, err)
	}
	got := mustFrame(t, s, byPath["rt.fit"].ID)
	if got.SHA256 != "device-rt-revised" || got.Size != 102980 || got.Mtime != 20 || got.PulledAt != 2000 {
		t.Fatalf("a revision advances the device anchors and the grace clock: %+v", got)
	}
	if got.EditSHA256 != "edited-rt-revised" || got.ArchiveSHA256 != "edited-rt-revised" || got.ArchiveSize != 102980 {
		t.Fatalf("a revision advances the archive anchors and the existing overlay: %+v", got)
	}
	if !reflect.DeepEqual(got.HeaderEdits, byPath["rt.fit"].HeaderEdits) {
		t.Fatalf("a revision must not touch header_edits: %+v", got.HeaderEdits)
	}
}

// TestCommitSupersedeEditKind pins the edit column of the design header-edits §8
// table: every sharer — the edited row included — takes the archive anchors, the
// overlay and the filter, and edit_sha256 is set even on a sharer that carried no
// overlay; the device anchors, state, pulled_at and delete_defer_count of every
// row are untouched, so a deleted sharer stays deleted.
func TestCommitSupersedeEditKind(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const dest = "/lib/M31/Light_0005.fit"

	edited := seedSupersedeRow(t, s, "Autorun/edited.fit", dest, model.Frame{
		SHA256: "device-a", Size: 100, Mtime: 10, Filter: "",
		ArchiveSHA256: "archive-old", ArchiveSize: 100,
		State: model.StateVerified, PulledAt: 1000,
	})
	deletedSharer := seedSupersedeRow(t, s, "Autorun/deleted.fit", dest, model.Frame{
		SHA256: "device-b", Size: 100, Mtime: 11,
		ArchiveSHA256: "archive-old", ArchiveSize: 100,
		State: model.StateDeleted, PulledAt: 1001, DeletedAt: 1500,
	})
	other := seedSupersedeRow(t, s, "Autorun/other.fit", "/lib/M31/Light_0006.fit", model.Frame{
		SHA256: "device-c", Size: 100, Mtime: 12,
		ArchiveSHA256: "archive-c", ArchiveSize: 100,
		State: model.StateVerified, PulledAt: 1002,
	})
	for _, id := range []int64{edited.ID, edited.ID, deletedSharer.ID} {
		if err := s.DeferDelete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	before := map[int64]model.Frame{}
	for _, id := range []int64{edited.ID, deletedSharer.ID, other.ID} {
		before[id] = mustFrame(t, s, id)
	}

	edits := model.HeaderEdits{{Key: "FILTER", Value: "Ha"}}
	in := SupersedeIntent{
		RigID: "rig-h13", DestPath: dest, AsidePath: dest + ".superseding-x", FrameID: edited.ID,
		Kind:          SupersedeEdit,
		ArchiveSHA256: "archive-edited", ArchiveSize: 102980,
		EditSHA256: "archive-edited", HeaderEdits: edits, Filter: "Ha",
		// Stale values an edit commit must never write back, however a recovery
		// replays the intent.
		SHA256: "stale-device", Size: 1, Mtime: 1, State: model.StateHeld, PulledAt: 9,
		CreatedAt: 5000,
	}
	// Committed through the table, as recovery would: the payload must survive it.
	if err := s.PutSupersedeIntent(ctx, in); err != nil {
		t.Fatalf("PutSupersedeIntent: %v", err)
	}
	list, err := s.ListSupersedeIntents(ctx)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], in) {
		t.Fatalf("edit intent round-trip: err=%v got %+v want %+v", err, list, in)
	}
	found, sharers, err := s.CommitSupersede(ctx, list[0])
	if err != nil || !found || sharers != 1 {
		t.Fatalf("CommitSupersede(edit): found=%v sharers=%d err=%v", found, sharers, err)
	}

	for _, id := range []int64{edited.ID, deletedSharer.ID} {
		got, was := mustFrame(t, s, id), before[id]
		if got.ArchiveSHA256 != "archive-edited" || got.ArchiveSize != 102980 {
			t.Fatalf("row %d: archive anchors must describe the edited file: %+v", id, got)
		}
		if got.EditSHA256 != "archive-edited" || !reflect.DeepEqual(got.HeaderEdits, edits) || got.Filter != "Ha" {
			t.Fatalf("row %d: overlay and filter must be the intent's: sha=%q edits=%+v filter=%q", id, got.EditSHA256, got.HeaderEdits, got.Filter)
		}
		if got.SHA256 != was.SHA256 || got.Size != was.Size || got.Mtime != was.Mtime {
			t.Fatalf("row %d: device anchor moved (HE-1): got %q/%d/%d want %q/%d/%d", id, got.SHA256, got.Size, got.Mtime, was.SHA256, was.Size, was.Mtime)
		}
		if got.State != was.State || got.PulledAt != was.PulledAt || got.DeletedAt != was.DeletedAt || got.DeleteDeferCount != was.DeleteDeferCount {
			t.Fatalf("row %d: lifecycle moved: state=%s pulled_at=%d deleted_at=%d defers=%d, want %s %d %d %d",
				id, got.State, got.PulledAt, got.DeletedAt, got.DeleteDeferCount, was.State, was.PulledAt, was.DeletedAt, was.DeleteDeferCount)
		}
	}
	if got := mustFrame(t, s, edited.ID); got.DeleteDeferCount != 2 {
		t.Fatalf("the edited row's deferral count must survive: %d", got.DeleteDeferCount)
	}
	if got := mustFrame(t, s, other.ID); !reflect.DeepEqual(got, before[other.ID]) {
		t.Fatalf("a row at another path must not move:\n got %+v\nwant %+v", got, before[other.ID])
	}

	// A row re-filed elsewhere since the intent was written commits nothing.
	found, sharers, err = s.CommitSupersede(ctx, SupersedeIntent{
		RigID: "rig-h13", DestPath: "/lib/elsewhere.fit", AsidePath: "/lib/elsewhere.fit.superseding-y",
		FrameID: edited.ID, Kind: SupersedeEdit, ArchiveSHA256: "z", ArchiveSize: 1,
		EditSHA256: "z", HeaderEdits: edits, Filter: "OIII",
	})
	if err != nil || found || sharers != 0 {
		t.Fatalf("commit at a path the row no longer names: found=%v sharers=%d err=%v", found, sharers, err)
	}
}

// TestSupersedeIntentRefusesMalformedKinds: an edit commit sets the overlay on
// every sharer, so an edit carrying none would mark rows edited with nothing to
// re-apply; an unknown kind has no commit at all.
func TestSupersedeIntentRefusesMalformedKinds(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := SupersedeIntent{RigID: "rig", DestPath: "/lib/a.fit", AsidePath: "/lib/a.fit.superseding-1", FrameID: 1}
	for name, in := range map[string]SupersedeIntent{
		"edit without hash": func() SupersedeIntent {
			i := base
			i.Kind, i.HeaderEdits = SupersedeEdit, model.HeaderEdits{{Key: "FILTER", Value: "Ha"}}
			return i
		}(),
		"edit without entries": func() SupersedeIntent { i := base; i.Kind, i.EditSHA256 = SupersedeEdit, "h"; return i }(),
		"unknown kind":         func() SupersedeIntent { i := base; i.Kind = "restore"; return i }(),
	} {
		if err := s.PutSupersedeIntent(ctx, in); err == nil {
			t.Errorf("%s: PutSupersedeIntent accepted it", name)
		}
		if _, _, err := s.CommitSupersede(ctx, in); err == nil {
			t.Errorf("%s: CommitSupersede accepted it", name)
		}
	}
}

// TestHeaderEditsWrittenOnlyByUpsert pins the overlay's write rules (design
// header-edits §5.1): UpsertFrame writes header_edits straight from its input, nil
// binding NULL, and UpdateFrame — whose callers hold stale by-value copies — never
// writes it.
func TestHeaderEditsWrittenOnlyByUpsert(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	from := "M 31"
	edits := model.HeaderEdits{{Key: "OBJECT", Value: "M31", From: &from}, {Key: "FILTER", Value: "Ha"}}
	f := &model.Frame{
		RigID: "rig-he", SourceShare: "Udisk", SourcePath: "a.fit", SHA256: "d", Size: 1, Mtime: 1,
		State: model.StateVerified, DestPath: "/lib/a.fit", EditSHA256: "e", HeaderEdits: edits,
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got := mustFrame(t, s, f.ID); !reflect.DeepEqual(got.HeaderEdits, edits) {
		t.Fatalf("round-trip: %+v, want %+v", got.HeaderEdits, edits)
	}

	stale := mustFrame(t, s, f.ID)
	stale.HeaderEdits, stale.EditSHA256 = nil, ""
	stale.State = model.StateHeld
	if err := s.UpdateFrame(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if got := mustFrame(t, s, f.ID); !reflect.DeepEqual(got.HeaderEdits, edits) || got.EditSHA256 != "e" || got.State != model.StateHeld {
		t.Fatalf("UpdateFrame must leave the overlay alone: state=%s sha=%q edits=%+v", got.State, got.EditSHA256, got.HeaderEdits)
	}

	reset := *f
	reset.ID, reset.HeaderEdits, reset.EditSHA256 = 0, model.HeaderEdits{}, ""
	if err := s.UpsertFrame(ctx, &reset); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := s.(*sqlStore).read.QueryRowContext(ctx, `SELECT header_edits IS NULL FROM frames WHERE id=?`, f.ID).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("an empty overlay must store NULL: null=%v err=%v", isNull, err)
	}
}
