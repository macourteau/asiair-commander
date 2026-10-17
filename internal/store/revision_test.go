package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestUpsertFramePreservesDataSHA256 pins the one column UpsertFrame must NOT
// overwrite from excluded: the exposure identity is fixed for the exposure's life,
// so a re-file whose incoming frame carries none leaves the recorded value intact,
// while a genuinely different exposure at the same path — which arrives WITH its own
// digest — replaces it. Nothing else asserts the preserve, and regressing it nulls
// the identity silently: the discriminator then has nothing to compare a revision
// against and falls back to the ordinary behaviour with no error raised anywhere
// (design frame-lifecycle §11.3).
func TestUpsertFramePreservesDataSHA256(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	f := &model.Frame{
		RigID: "rig-h13", SourceShare: "Udisk", SourcePath: "Autorun/light_001.fit",
		SHA256: "devhash", DataSHA256: "dataunit", Size: 100, Mtime: 1,
		ArchiveSHA256: "devhash", ArchiveSize: 100,
		State: model.StateVerified, DestPath: "/lib/M31/light_001.fit",
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert (record): %v", err)
	}

	// A re-file through a path that computed no data-unit digest: same natural key,
	// DataSHA256 empty — exactly what recordFiled binds for a frame whose producer
	// did not tee one.
	refile := *f
	refile.DataSHA256 = ""
	refile.SHA256 = "devhash2"
	if err := s.UpsertFrame(ctx, &refile); err != nil {
		t.Fatalf("upsert (re-file): %v", err)
	}
	got, ok, err := s.GetFrameByID(ctx, f.ID)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID: ok=%v err=%v", ok, err)
	}
	if got.DataSHA256 != "dataunit" {
		t.Fatalf("a re-file carrying no data_sha256 must preserve the recorded one, got %q", got.DataSHA256)
	}
	if got.SHA256 != "devhash2" {
		t.Fatalf("the device anchor must still follow excluded, got %q", got.SHA256)
	}

	// A different exposure written at the same path carries its own digest, which
	// wins: the preserve is "never nulled", not "never changed".
	replaced := *f
	replaced.DataSHA256 = "otherdataunit"
	if err := s.UpsertFrame(ctx, &replaced); err != nil {
		t.Fatalf("upsert (replace): %v", err)
	}
	got, _, _ = s.GetFrameByID(ctx, f.ID)
	if got.DataSHA256 != "otherdataunit" {
		t.Fatalf("an incoming data_sha256 must win, got %q", got.DataSHA256)
	}
}

// TestUpsertFrameWritesArchiveAnchorsFromExcluded pins the other half of the same
// contract: the archive-side anchors follow the plain excluded= pattern, so a writer
// that files different bytes advances them and one that passes nothing empties them
// loudly rather than inheriting a stale value. A COALESCE bridge here is what the
// design rejected in favour of a test that fails when a write site is missed.
func TestUpsertFrameWritesArchiveAnchorsFromExcluded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	f := &model.Frame{
		RigID: "rig-h13", SourceShare: "Udisk", SourcePath: "Autorun/light_002.fit",
		SHA256: "devhash", Size: 100, Mtime: 1,
		ArchiveSHA256: "archhash", ArchiveSize: 100,
		State: model.StateVerified, DestPath: "/lib/M31/light_002.fit",
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert (set): %v", err)
	}
	got, ok, err := s.GetFrameByID(ctx, f.ID)
	if err != nil || !ok || got.ArchiveSHA256 != "archhash" || got.ArchiveSize != 100 {
		t.Fatalf("archive anchors not round-tripped: sha=%q size=%d (ok=%v err=%v)",
			got.ArchiveSHA256, got.ArchiveSize, ok, err)
	}

	// A write of different archive bytes advances both, leaving the device anchors
	// where they are: the two sides move independently.
	advanced := *f
	advanced.ArchiveSHA256, advanced.ArchiveSize = "archhash2", 102880
	if err := s.UpsertFrame(ctx, &advanced); err != nil {
		t.Fatalf("upsert (advance): %v", err)
	}
	got, _, _ = s.GetFrameByID(ctx, f.ID)
	if got.ArchiveSHA256 != "archhash2" || got.ArchiveSize != 102880 {
		t.Fatalf("archive anchors must follow excluded: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.SHA256 != "devhash" || got.Size != 100 {
		t.Fatalf("the device anchors must be untouched by an archive-side advance: sha=%q size=%d", got.SHA256, got.Size)
	}

	// A writer that passes nothing empties them — the loud failure the design chose
	// over a silent fallback.
	blank := *f
	blank.ArchiveSHA256, blank.ArchiveSize = "", 0
	if err := s.UpsertFrame(ctx, &blank); err != nil {
		t.Fatalf("upsert (blank): %v", err)
	}
	got, _, _ = s.GetFrameByID(ctx, f.ID)
	if got.ArchiveSHA256 != "" || got.ArchiveSize != 0 {
		t.Fatalf("a missed write site must empty the anchors, not inherit: sha=%q size=%d",
			got.ArchiveSHA256, got.ArchiveSize)
	}
}

// TestUpsertFrameResetsTheDeferralCounter pins the counter's reset condition: every
// filing write is the frame's source coming to agree with its row again, so the
// pre-delete gate's bound is spent per episode of disagreement rather than per row
// lifetime. Without the reset a row that spent its budget on one revision would hold
// on the first sighting of the next one.
func TestUpsertFrameResetsTheDeferralCounter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	f := &model.Frame{
		RigID: "rig-h13", SourceShare: "Udisk", SourcePath: "Autorun/light_003.fit",
		SHA256: "devhash", Size: 100, Mtime: 1,
		ArchiveSHA256: "devhash", ArchiveSize: 100,
		State: model.StateVerified, DestPath: "/lib/M31/light_003.fit",
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert (insert): %v", err)
	}
	if got, _, _ := s.GetFrameByID(ctx, f.ID); got.DeleteDeferCount != 0 {
		t.Fatalf("a fresh row has nothing deferred, got %d", got.DeleteDeferCount)
	}
	if _, err := s.(*sqlStore).write.ExecContext(ctx,
		`UPDATE frames SET delete_defer_count = 3 WHERE id = ?`, f.ID); err != nil {
		t.Fatalf("spend the budget: %v", err)
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert (re-file): %v", err)
	}
	if got, _, _ := s.GetFrameByID(ctx, f.ID); got.DeleteDeferCount != 0 {
		t.Fatalf("a filing write must reset the deferral counter, got %d", got.DeleteDeferCount)
	}
}

// TestDeferDeleteAdvancesOnlyTheCounter is the pre-delete gate's write: a pass that
// declines to conclude anything about a frame records exactly that and nothing more.
// The increment is read-modify-write in SQL rather than a value the caller carries,
// so a reset committed by the pull path between the caller's read and this write can
// never be undone.
func TestDeferDeleteAdvancesOnlyTheCounter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	f := &model.Frame{
		RigID: "rig-h13", SourceShare: "Udisk", SourcePath: "Autorun/light_004.fit",
		SHA256: "devhash", Size: 100, Mtime: 1,
		ArchiveSHA256: "archhash", ArchiveSize: 100,
		State: model.StateVerified, DestPath: "/lib/M31/light_004.fit",
	}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for want := 1; want <= 3; want++ {
		if err := s.DeferDelete(ctx, f.ID); err != nil {
			t.Fatalf("defer %d: %v", want, err)
		}
		got, _, _ := s.GetFrameByID(ctx, f.ID)
		if got.DeleteDeferCount != want {
			t.Fatalf("delete_defer_count = %d, want %d", got.DeleteDeferCount, want)
		}
		if got.SHA256 != f.SHA256 || got.Size != f.Size || got.Mtime != f.Mtime ||
			got.ArchiveSHA256 != f.ArchiveSHA256 || got.ArchiveSize != f.ArchiveSize ||
			got.State != f.State || got.DestPath != f.DestPath {
			t.Fatalf("a deferral proves nothing about either copy and must write nothing else: %+v", got)
		}
	}
}
