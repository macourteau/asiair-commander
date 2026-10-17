package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestCommitRestoreAdvancesTheReadRowAndItsSharers is the store side of the two
// restore rows of design frame-lifecycle §11.2. It is asserted separately from
// the supersede's even though the two share one transaction, because the restore's
// read row is the one whose DEVICE copy was streamed and that need not be the row
// whose restore was requested: here the request names the sharer, whose own device
// anchors must come through untouched while the archive side of BOTH rows follows
// the file.
func TestCommitRestoreAdvancesTheReadRowAndItsSharers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const dest = "/lib/M31/Light_0001.fit"

	read := seedSupersedeRow(t, s, "Autorun/a.fit", dest, model.Frame{
		SHA256: "device-a-revised", DataSHA256: "the-exposure", Size: 102880, Mtime: 20,
		ArchiveSHA256: "stale", ArchiveSize: 100,
		RetargetSHA256: "overlay-before", RetargetFrom: "NGC 1",
		State: model.StateHeld, PulledAt: 1000,
	})
	requested := seedSupersedeRow(t, s, "Autorun/b.fit", dest, model.Frame{
		SHA256: "device-b", DataSHA256: "the-exposure", Size: 100, Mtime: 11,
		ArchiveSHA256: "stale", ArchiveSize: 100,
		State: model.StateVerified, PulledAt: 1001,
	})

	ok, sharers, err := s.CommitRestore(ctx, ArchiveWrite{
		RigID: "rig-h13", DestPath: dest, FrameID: read.ID,
		SHA256: "device-a-revised", Size: 102880, Mtime: 20,
		ArchiveSHA256: "archive-restored", ArchiveSize: 102880,
		RetargetSHA256: "archive-restored",
		State:          model.StateVerified, PulledAt: 2000,
	})
	if err != nil || !ok || sharers != 1 {
		t.Fatalf("CommitRestore: read=%v sharers=%d err=%v", ok, sharers, err)
	}

	got := mustFrame(t, s, read.ID)
	if got.ArchiveSHA256 != "archive-restored" || got.ArchiveSize != 102880 {
		t.Fatalf("the read row's archive anchors must describe the restored file: %+v", got)
	}
	if got.RetargetSHA256 != "archive-restored" || got.RetargetFrom != "NGC 1" {
		t.Fatalf("the audit record follows the file it describes; what the frame was called does not: %+v", got)
	}
	if got.State != model.StateVerified || got.PulledAt != 2000 {
		t.Fatalf("the read row's lifecycle advances on a device read that happened: state=%s pulled_at=%d", got.State, got.PulledAt)
	}
	if got.DataSHA256 != "the-exposure" {
		t.Fatalf("the exposure identity is preserved, got %q", got.DataSHA256)
	}

	other := mustFrame(t, s, requested.ID)
	if other.ArchiveSHA256 != "archive-restored" || other.ArchiveSize != 102880 {
		t.Fatalf("every row naming the file moves with it: %+v", other)
	}
	if other.SHA256 != "device-b" || other.Size != 100 || other.Mtime != 11 {
		t.Fatalf("the un-read row's device anchors must not move: %+v", other)
	}
	if other.PulledAt != 1001 || other.State != model.StateVerified {
		t.Fatalf("the un-read row's own lifecycle is not this restore's to advance: pulled_at=%d state=%s", other.PulledAt, other.State)
	}
	if other.RetargetSHA256 != "" {
		t.Fatalf("a row that was never retargeted must not gain an overlay, got %q", other.RetargetSHA256)
	}
}

// mustFrame reads one row back, the test having already proven it exists.
func mustFrame(t *testing.T, s Store, id int64) model.Frame {
	t.Helper()
	f, ok, err := s.GetFrameByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID(%d): ok=%v err=%v", id, ok, err)
	}
	return *f
}
