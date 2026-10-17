package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// seedSupersedeRow files one row directly, standing in for a filing path: every
// column this test set reads is one a filing site writes.
func seedSupersedeRow(t *testing.T, s Store, path, dest string, f model.Frame) model.Frame {
	t.Helper()
	ctx := context.Background()
	f.RigID, f.SourceShare, f.SourcePath, f.DestPath = orDefault(f.RigID, "rig-h13"), "Udisk", path, dest
	if err := s.UpsertFrame(ctx, &f); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	got, ok, err := s.GetFrameByID(ctx, f.ID)
	if err != nil || !ok {
		t.Fatalf("read back %s: ok=%v err=%v", path, ok, err)
	}
	return *got
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// TestCommitSupersedeAdvancesTheRevisedRowAndItsSharers is the store side of
// design frame-lifecycle §11.2: one call, two row sets, and the difference between
// them is which copy each column describes. The revised row's device anchors move
// because its device copy was read; a sharer's do not, because the swap never read
// the sharer's — nor its state, nor the grace clock its pulled_at anchors, the
// supersede having neither caused any hold it carries nor discharged one.
func TestCommitSupersedeAdvancesTheRevisedRowAndItsSharers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const dest = "/lib/M31/Light_0001.fit"

	revised := seedSupersedeRow(t, s, "Autorun/a.fit", dest, model.Frame{
		SHA256: "device-a", DataSHA256: "the-exposure", Size: 100, Mtime: 10,
		ArchiveSHA256: "device-a", ArchiveSize: 100,
		State: model.StateHeld, PulledAt: 1000,
	})
	sharer := seedSupersedeRow(t, s, "Autorun/b.fit", dest, model.Frame{
		SHA256: "device-b", DataSHA256: "the-exposure", Size: 100, Mtime: 11,
		ArchiveSHA256: "device-a", ArchiveSize: 100,
		State: model.StateVerified, PulledAt: 1001,
	})
	// A budget the pre-delete gate had spent on this frame's disagreement.
	if _, err := s.(*sqlStore).write.ExecContext(ctx,
		`UPDATE frames SET delete_defer_count = 2 WHERE id = ?`, revised.ID); err != nil {
		t.Fatalf("spend the budget: %v", err)
	}

	in := SupersedeIntent{
		RigID: "rig-h13", DestPath: dest, AsidePath: dest + ".superseding-device-a",
		FrameID: revised.ID, SHA256: "device-a-revised", Size: 102880, Mtime: 20,
		ArchiveSHA256: "archive-revised", ArchiveSize: 102880,
		State: model.StateVerified, PulledAt: 2000,
	}
	ok, sharers, err := s.CommitSupersede(ctx, in)
	if err != nil || !ok || sharers != 1 {
		t.Fatalf("CommitSupersede: revised=%v sharers=%d err=%v", ok, sharers, err)
	}

	got, _, _ := s.GetFrameByID(ctx, revised.ID)
	if got.SHA256 != "device-a-revised" || got.Size != 102880 || got.Mtime != 20 {
		t.Fatalf("the revised row's device anchors must advance: sha=%q size=%d mtime=%d",
			got.SHA256, got.Size, got.Mtime)
	}
	if got.ArchiveSHA256 != "archive-revised" || got.ArchiveSize != 102880 {
		t.Fatalf("its archive anchors must advance: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DataSHA256 != "the-exposure" {
		t.Fatalf("data_sha256 is what a header edit survives; got %q", got.DataSHA256)
	}
	if got.State != model.StateVerified || got.PulledAt != 2000 {
		t.Fatalf("state=%s pulled_at=%d, want verified and the grace clock restarted", got.State, got.PulledAt)
	}
	if got.DeleteDeferCount != 0 {
		t.Fatalf("the frame's source agrees with its row again, so the budget resets; got %d", got.DeleteDeferCount)
	}

	side, _, _ := s.GetFrameByID(ctx, sharer.ID)
	if side.ArchiveSHA256 != "archive-revised" || side.ArchiveSize != 102880 {
		t.Fatalf("a sharer's archive anchors move with the file: sha=%q size=%d", side.ArchiveSHA256, side.ArchiveSize)
	}
	if side.SHA256 != "device-b" || side.Size != 100 || side.Mtime != 11 {
		t.Fatalf("a sharer's device anchors describe its own untouched copy: sha=%q size=%d mtime=%d",
			side.SHA256, side.Size, side.Mtime)
	}
	if side.State != model.StateVerified || side.PulledAt != 1001 {
		t.Fatalf("a sharer's lifecycle is not the supersede's to advance: state=%s pulled_at=%d", side.State, side.PulledAt)
	}
}

// TestCommitSupersedeAdvancesOnlyAnExistingOverlay pins the overlay rule from both
// sides. The audit record must keep matching what the archive file holds, so a row
// carrying one has it advanced wherever that file is rewritten; a row carrying none
// must not GAIN one, a non-empty retarget_sha256 being what "this frame was
// retargeted" means.
func TestCommitSupersedeAdvancesOnlyAnExistingOverlay(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const dest = "/lib/M31/Light_0002.fit"

	plain := seedSupersedeRow(t, s, "Autorun/plain.fit", dest, model.Frame{
		SHA256: "device-plain", Size: 100, Mtime: 10,
		ArchiveSHA256: "device-plain", ArchiveSize: 100, State: model.StateVerified,
	})
	overlaid := seedSupersedeRow(t, s, "Autorun/overlaid.fit", dest, model.Frame{
		SHA256: "device-overlaid", Size: 100, Mtime: 11,
		ArchiveSHA256: "edited-old", ArchiveSize: 100, State: model.StateVerified,
		RetargetSHA256: "edited-old", RetargetFrom: "C-2025 A6",
	})

	if _, _, err := s.CommitSupersede(ctx, SupersedeIntent{
		RigID: "rig-h13", DestPath: dest, AsidePath: dest + ".superseding-x", FrameID: plain.ID,
		SHA256: "device-plain-revised", Size: 102880, Mtime: 20,
		ArchiveSHA256: "edited-new", ArchiveSize: 102880, RetargetSHA256: "edited-new",
		State: model.StateVerified, PulledAt: 2000,
	}); err != nil {
		t.Fatalf("CommitSupersede: %v", err)
	}

	if got, _, _ := s.GetFrameByID(ctx, plain.ID); got.RetargetSHA256 != "" || got.RetargetFrom != "" {
		t.Fatalf("a row that was never retargeted must not gain an overlay: sha=%q from=%q",
			got.RetargetSHA256, got.RetargetFrom)
	}
	got, _, _ := s.GetFrameByID(ctx, overlaid.ID)
	if got.RetargetSHA256 != "edited-new" {
		t.Fatalf("the audit record must follow the file it describes, got %q", got.RetargetSHA256)
	}
	if got.RetargetFrom != "C-2025 A6" {
		t.Fatalf("what the frame was labelled before does not change when its bytes do, got %q", got.RetargetFrom)
	}

	// An intent carrying no overlay value leaves every row's record alone, rather
	// than blanking the ones that have it.
	if _, _, err := s.CommitSupersede(ctx, SupersedeIntent{
		RigID: "rig-h13", DestPath: dest, AsidePath: dest + ".superseding-y", FrameID: plain.ID,
		SHA256: "device-plain-revised", Size: 102880, Mtime: 21,
		ArchiveSHA256: "edited-newer", ArchiveSize: 102880,
		State: model.StateVerified, PulledAt: 2001,
	}); err != nil {
		t.Fatalf("CommitSupersede (no overlay): %v", err)
	}
	if got, _, _ := s.GetFrameByID(ctx, overlaid.ID); got.RetargetSHA256 != "edited-new" {
		t.Fatalf("an empty overlay value writes nothing, got %q", got.RetargetSHA256)
	}
}

// TestCommitSupersedeReportsAVanishedRevisedRow: PruneDeleted runs hourly and can
// legitimately remove the revised row mid-swap, and recovery re-runs a commit that
// may already have landed. Neither is an error — but the caller is told, because a
// swap whose revised row is gone is a warning to log, and the sharers still describe
// the file that is there now.
func TestCommitSupersedeReportsAVanishedRevisedRow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const dest = "/lib/M31/Light_0003.fit"
	sharer := seedSupersedeRow(t, s, "Autorun/left.fit", dest, model.Frame{
		SHA256: "device-left", Size: 100, Mtime: 10,
		ArchiveSHA256: "old", ArchiveSize: 100, State: model.StateVerified,
	})

	ok, sharers, err := s.CommitSupersede(ctx, SupersedeIntent{
		RigID: "rig-h13", DestPath: dest, AsidePath: dest + ".superseding-z", FrameID: 9999,
		SHA256: "gone", Size: 1, Mtime: 1, ArchiveSHA256: "new", ArchiveSize: 102880,
		State: model.StateVerified, PulledAt: 2000,
	})
	if err != nil {
		t.Fatalf("a vanished revised row is not an error: %v", err)
	}
	if ok {
		t.Fatal("no revised row was updated; the caller must be told so it can warn")
	}
	if sharers != 1 {
		t.Fatalf("sharers=%d, want the row that still describes the file committed", sharers)
	}
	if got, _, _ := s.GetFrameByID(ctx, sharer.ID); got.ArchiveSHA256 != "new" {
		t.Fatalf("the surviving row must describe the bytes the file now holds, got %q", got.ArchiveSHA256)
	}
}

// TestSupersedeIntentRoundTripsEveryAnchor is what the payload rule rests on:
// recovery runs before any worker or device session exists, so an anchor that does
// not survive the table cannot be committed at all — mtime especially, which is the
// DEVICE file's and cannot be re-derived from the archive copy left on disk.
func TestSupersedeIntentRoundTripsEveryAnchor(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	in := SupersedeIntent{
		RigID: "rig-h13", DestPath: "/lib/M31/Light_0004.fit",
		AsidePath: "/lib/M31/Light_0004.fit.superseding-abcdef", FrameID: 42,
		SHA256: "device-revised", Size: 102880, Mtime: 1_700_000_000,
		ArchiveSHA256: "archive-revised", ArchiveSize: 102880, RetargetSHA256: "edited",
		State: model.StateVerified, PulledAt: 1_700_000_100, CreatedAt: 1_700_000_200,
	}
	if err := s.PutSupersedeIntent(ctx, in); err != nil {
		t.Fatalf("PutSupersedeIntent: %v", err)
	}
	list, err := s.ListSupersedeIntents(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSupersedeIntents: n=%d err=%v", len(list), err)
	}
	if list[0] != in {
		t.Fatalf("intent round-trip lost an anchor:\n got %+v\nwant %+v", list[0], in)
	}

	// The open set is rig-scoped and keyed on the path being claimed, which is what
	// absentDeferReasons matches a frame's dest_path against.
	open, err := s.OpenSupersedePaths(ctx, "rig-h13")
	if err != nil || !open[in.DestPath] || len(open) != 1 {
		t.Fatalf("OpenSupersedePaths(rig-h13) = %v (err %v)", open, err)
	}
	if other, err := s.OpenSupersedePaths(ctx, "rig-other"); err != nil || len(other) != 0 {
		t.Fatalf("OpenSupersedePaths(rig-other) = %v (err %v), want another rig's swap invisible", other, err)
	}

	// Re-announcing the same claim rewrites it rather than duplicating it: an intent
	// must always name the anchors the commit about to be attempted would write.
	in.ArchiveSHA256, in.CreatedAt = "archive-revised-again", 1_700_000_300
	if err := s.PutSupersedeIntent(ctx, in); err != nil {
		t.Fatalf("PutSupersedeIntent (rewrite): %v", err)
	}
	if list, err = s.ListSupersedeIntents(ctx); err != nil || len(list) != 1 || list[0] != in {
		t.Fatalf("rewrite: n=%d err=%v got %+v", len(list), err, list)
	}

	if err := s.ClearSupersedeIntent(ctx, in.RigID, in.DestPath); err != nil {
		t.Fatalf("ClearSupersedeIntent: %v", err)
	}
	if list, err = s.ListSupersedeIntents(ctx); err != nil || len(list) != 0 {
		t.Fatalf("after clear: n=%d err=%v", len(list), err)
	}
	// Clearing an absent intent is not an error: every failure path clears
	// unconditionally.
	if err := s.ClearSupersedeIntent(ctx, in.RigID, in.DestPath); err != nil {
		t.Fatalf("clearing an absent intent must be a no-op, got %v", err)
	}
}

// TestCommitSupersedeSkipsARowThatMovedAway pins the revised row's dest_path
// predicate. An intent names a path; a row can be re-filed out from under it while
// it is open. Committing on the id alone would hand that row archive anchors
// describing a file at a path it no longer names — a bad anchor, and a critical
// delete_mismatch on its next pass, rather than merely an orphan.
func TestCommitSupersedeSkipsARowThatMovedAway(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const was = "/lib/M31/Light_0005.fit"
	const now = "/lib/M31/FRA300/NIGHT_1/Light_0005.fit"

	row := seedSupersedeRow(t, s, "Autorun/moved.fit", was, model.Frame{
		SHA256: "device-moved", Size: 100, Mtime: 10,
		ArchiveSHA256: "device-moved", ArchiveSize: 100, State: model.StateVerified, PulledAt: 1000,
	})
	if _, err := s.MoveDestPath(ctx, "rig-h13", was, now, RequireRows); err != nil {
		t.Fatalf("re-file the row out from under the intent: %v", err)
	}

	revised, sharers, err := s.CommitSupersede(ctx, SupersedeIntent{
		RigID: "rig-h13", DestPath: was, AsidePath: was + ".superseding-device-moved",
		FrameID: row.ID, SHA256: "device-revised", Size: 102880, Mtime: 20,
		ArchiveSHA256: "archive-revised", ArchiveSize: 102880,
		State: model.StateVerified, PulledAt: 2000,
	})
	if err != nil {
		t.Fatalf("a row that moved away is not an error: %v", err)
	}
	if revised || sharers != 0 {
		t.Fatalf("nothing names the intent's path any more: revised=%v sharers=%d", revised, sharers)
	}
	got, _, _ := s.GetFrameByID(ctx, row.ID)
	if got.ArchiveSHA256 != "device-moved" || got.ArchiveSize != 100 || got.SHA256 != "device-moved" {
		t.Fatalf("a row that moved away must acquire no anchor: sha=%q archive=%q size=%d",
			got.SHA256, got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DestPath != now {
		t.Fatalf("dest_path = %q, want the re-file's %q", got.DestPath, now)
	}
}
