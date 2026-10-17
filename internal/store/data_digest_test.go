package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestSetFrameDataDigestIfPendingGuardsOnTheProvenAnchors is the store side of the
// backfill's one write. The digest is only as good as the copy it was read from,
// and the row that copy answers for is named by sha256 and archive_sha256 — so a
// row whose anchors moved between the read and the write refuses it rather than
// recording one exposure's identity on another exposure's row, and a row that has
// since acquired a value keeps the one a real proof path wrote.
func TestSetFrameDataDigestIfPendingGuardsOnTheProvenAnchors(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	row := seedSupersedeRow(t, s, "Autorun/a.fit", "/lib/M31/Light_0001.fit", model.Frame{
		SHA256: "device-original", Size: 100, Mtime: 10,
		ArchiveSHA256: "device-original", ArchiveSize: 100,
		State: model.StateVerified, PulledAt: 1000,
	})
	if row.DataSHA256 != "" {
		t.Fatalf("setup: a row filed before the column existed carries no identity, got %q", row.DataSHA256)
	}

	if ok, err := s.SetFrameDataDigestIfPending(ctx, row.ID, "moved-since", row.ArchiveSHA256, "the-exposure"); err != nil || ok {
		t.Fatalf("a row whose device anchor moved must refuse the write: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetFrameDataDigestIfPending(ctx, row.ID, row.SHA256, "moved-since", "the-exposure"); err != nil || ok {
		t.Fatalf("a row whose archive anchor moved must refuse the write: ok=%v err=%v", ok, err)
	}
	if got, _, _ := s.GetFrameByID(ctx, row.ID); got.DataSHA256 != "" {
		t.Fatalf("a refused write must leave the row unset, got %q", got.DataSHA256)
	}

	if ok, err := s.SetFrameDataDigestIfPending(ctx, row.ID, row.SHA256, row.ArchiveSHA256, "the-exposure"); err != nil || !ok {
		t.Fatalf("SetFrameDataDigestIfPending: ok=%v err=%v", ok, err)
	}
	got, _, _ := s.GetFrameByID(ctx, row.ID)
	if got.DataSHA256 != "the-exposure" {
		t.Fatalf("data_sha256 = %q, want the proven identity", got.DataSHA256)
	}
	// And nothing else: the backfill advances that one column (design
	// frame-lifecycle §11.2).
	if got.SHA256 != row.SHA256 || got.Size != row.Size || got.Mtime != row.Mtime ||
		got.ArchiveSHA256 != row.ArchiveSHA256 || got.ArchiveSize != row.ArchiveSize ||
		got.State != row.State || got.PulledAt != row.PulledAt || got.DeleteDeferCount != row.DeleteDeferCount {
		t.Fatalf("the backfill must advance data_sha256 alone, got %+v", *got)
	}

	if ok, err := s.SetFrameDataDigestIfPending(ctx, row.ID, row.SHA256, row.ArchiveSHA256, "a-different-exposure"); err != nil || ok {
		t.Fatalf("a recorded identity is never clobbered by a cursor: ok=%v err=%v", ok, err)
	}
	if got, _, _ := s.GetFrameByID(ctx, row.ID); got.DataSHA256 != "the-exposure" {
		t.Fatalf("data_sha256 = %q, want the value the first proof wrote", got.DataSHA256)
	}
}

// TestListFramesNeedingDataDigestPagesTheTwoPasses asserts the queue's shape: held
// rows are a pass of their own so they can be backfilled first, rows with no
// archive copy to prove a digest against are never offered, and a row that
// acquires a value leaves the queue.
func TestListFramesNeedingDataDigestPagesTheTwoPasses(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := model.Frame{
		SHA256: "device", Size: 100, Mtime: 10,
		ArchiveSHA256: "archive", ArchiveSize: 100, State: model.StateVerified, PulledAt: 1000,
	}
	verified := seedSupersedeRow(t, s, "Autorun/v.fit", "/lib/v.fit", base)

	heldRow := base
	heldRow.State = model.StateHeld
	held := seedSupersedeRow(t, s, "Autorun/h.fit", "/lib/h.fit", heldRow)

	noArchive := base
	noArchive.ArchiveSHA256, noArchive.ArchiveSize = "", 0
	noAnchor := seedSupersedeRow(t, s, "Autorun/n.fit", "/lib/n.fit", noArchive)

	recorded := base
	recorded.DataSHA256 = "already-known"
	known := seedSupersedeRow(t, s, "Autorun/k.fit", "/lib/k.fit", recorded)

	ids := func(held bool) []int64 {
		t.Helper()
		frames, err := s.ListFramesNeedingDataDigest(ctx, 0, held, 10)
		if err != nil {
			t.Fatalf("ListFramesNeedingDataDigest(held=%v): %v", held, err)
		}
		var out []int64
		for _, f := range frames {
			out = append(out, f.ID)
		}
		return out
	}

	if got := ids(true); len(got) != 1 || got[0] != held.ID {
		t.Fatalf("held pass = %v, want only the held row %d", got, held.ID)
	}
	if got := ids(false); len(got) != 1 || got[0] != verified.ID {
		t.Fatalf("remaining pass = %v, want only the verified row %d (not %d, which has no archive anchor, nor %d, which already has a value)",
			got, verified.ID, noAnchor.ID, known.ID)
	}

	if ok, err := s.SetFrameDataDigestIfPending(ctx, verified.ID, verified.SHA256, verified.ArchiveSHA256, "the-exposure"); err != nil || !ok {
		t.Fatalf("SetFrameDataDigestIfPending: ok=%v err=%v", ok, err)
	}
	if got := ids(false); len(got) != 0 {
		t.Fatalf("a backfilled row must leave the queue, got %v", got)
	}
}
