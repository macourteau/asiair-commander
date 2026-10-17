package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestCommitCulledRevisionPreservesTheReconciliationMarks is the store side of
// design frame-lifecycle §11.12: a targeted write whose fields UpsertFrame must
// never clobber, following SetFrameAstrometry's precedent. UpsertFrame clears all
// three marks unconditionally on every re-file, which is why an ordinary filing
// write cannot express this outcome at all.
//
// A restore request and an acknowledgement cannot stand together — RequestRestore
// nulls missing_acked_at, Restore beating Acknowledge by the reconciliation
// design's own rule — so the two are asserted as the two shapes a culled row can
// actually take rather than hand-written into one row the store would never hold.
func TestCommitCulledRevisionPreservesTheReconciliationMarks(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		mark func(t *testing.T, s Store, id int64)
	}{
		{"acknowledged", func(t *testing.T, s Store, id int64) {
			if ok, err := s.AcknowledgeMissing(ctx, "rig-h13", id, 1200); err != nil || !ok {
				t.Fatalf("AcknowledgeMissing: ok=%v err=%v", ok, err)
			}
		}},
		{"restore requested", func(t *testing.T, s Store, id int64) {
			if ok, err := s.RequestRestore(ctx, "rig-h13", id, 1200); err != nil || !ok {
				t.Fatalf("RequestRestore: ok=%v err=%v", ok, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			row := seedSupersedeRow(t, s, "Autorun/culled.fit", "/lib/M31/Light_0003.fit", model.Frame{
				SHA256: "device-original", DataSHA256: "the-exposure", Size: 100, Mtime: 10,
				ArchiveSHA256: "device-original", ArchiveSize: 100,
				State: model.StateVerified, PulledAt: 1000,
				Astrometry: &model.Astrometry{Status: model.AstroStatusSolved},
			})
			if err := s.SetFrameMissing(ctx, row.ID, 1100); err != nil {
				t.Fatal(err)
			}
			tc.mark(t, s, row.ID)
			before, _, _ := s.GetFrameByID(ctx, row.ID)
			// A budget the pre-delete gate had spent on this frame's disagreement.
			if _, err := s.(*sqlStore).write.ExecContext(ctx,
				`UPDATE frames SET delete_defer_count = 3 WHERE id = ?`, row.ID); err != nil {
				t.Fatalf("spend the budget: %v", err)
			}

			ok, err := s.CommitCulledRevision(ctx, CulledRevision{
				RigID: "rig-h13", FrameID: row.ID,
				SHA256: "device-revised", Size: 102880, Mtime: 20, DataSHA256: "the-exposure",
			})
			if err != nil || !ok {
				t.Fatalf("CommitCulledRevision: ok=%v err=%v", ok, err)
			}

			got, _, _ := s.GetFrameByID(ctx, row.ID)
			if got.SHA256 != "device-revised" || got.Size != 102880 || got.Mtime != 20 {
				t.Fatalf("the device anchors must advance: sha=%q size=%d mtime=%d",
					got.SHA256, got.Size, got.Mtime)
			}
			if got.DataSHA256 != "the-exposure" {
				t.Fatalf("data_sha256 = %q, want the identity this path proved", got.DataSHA256)
			}
			if got.MissingSeenAt != before.MissingSeenAt ||
				got.MissingAckedAt != before.MissingAckedAt ||
				got.RestoreRequestedAt != before.RestoreRequestedAt {
				t.Fatalf("the reconciliation marks must stand: seen=%d acked=%d restore=%d",
					got.MissingSeenAt, got.MissingAckedAt, got.RestoreRequestedAt)
			}
			// No archive copy changed, so the archive-side expectation must not move.
			if got.ArchiveSHA256 != "device-original" || got.ArchiveSize != 100 {
				t.Fatalf("the archive anchors must be untouched: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
			}
			// Resetting these is ClearFrameAstrometry's job on the supersede path that
			// re-extracts; this path files nothing for a re-extraction to describe.
			if got.Astrometry == nil || got.Astrometry.Status != model.AstroStatusSolved {
				t.Fatalf("the astrometry is not this path's to touch: %+v", got.Astrometry)
			}
			if got.State != model.StateVerified || got.PulledAt != 1000 || got.DestPath != before.DestPath {
				t.Fatalf("the row's lifecycle is not this path's either: state=%s pulled_at=%d dest=%q",
					got.State, got.PulledAt, got.DestPath)
			}
			if got.DeleteDeferCount != 0 {
				t.Fatalf("the frame's source agrees with its row again, so the budget resets; got %d", got.DeleteDeferCount)
			}
		})
	}
}

// TestCommitCulledRevisionRefusesAnUnculledRow pins the guard. Advancing sha256 on
// a row that has stopped being culled would leave its device anchor matching bytes
// no archive copy holds, and the next pull of that frame would read as a content
// clash rather than a revision — a rot_clash alert and a token-suffixed duplicate.
// A refused write leaves the caller on the ordinary path instead.
func TestCommitCulledRevisionRefusesAnUnculledRow(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	row := seedSupersedeRow(t, s, "Autorun/present.fit", "/lib/M31/Light_0004.fit", model.Frame{
		SHA256: "device-original", DataSHA256: "the-exposure", Size: 100, Mtime: 10,
		ArchiveSHA256: "device-original", ArchiveSize: 100, State: model.StateVerified,
	})

	ok, err := s.CommitCulledRevision(ctx, CulledRevision{
		RigID: "rig-h13", FrameID: row.ID,
		SHA256: "device-revised", Size: 102880, Mtime: 20, DataSHA256: "the-exposure",
	})
	if err != nil {
		t.Fatalf("CommitCulledRevision: %v", err)
	}
	if ok {
		t.Fatal("a row carrying no cull mark is not this path's to write")
	}
	if got, _, _ := s.GetFrameByID(ctx, row.ID); got.SHA256 != "device-original" || got.Size != 100 {
		t.Fatalf("a refused write must leave the row alone: sha=%q size=%d", got.SHA256, got.Size)
	}
}

// TestCommitCulledRevisionWritesTheExposureIdentity pins the one cell of design
// frame-lifecycle §11.2 where data_sha256 ADVANCES rather than being preserved. In
// production the two are indistinguishable — the discriminator establishes the
// incoming file's data unit against the row's before this write is ever reached, so
// the value written always equals the value already there — which is exactly why
// the rule is only checkable here, against the method's own contract of writing
// what it is given. Dropping the column from the UPDATE is otherwise invisible.
func TestCommitCulledRevisionWritesTheExposureIdentity(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	row := seedSupersedeRow(t, s, "Autorun/identity.fit", "/lib/M31/Light_0005.fit", model.Frame{
		SHA256: "device-original", DataSHA256: "recorded-exposure", Size: 100, Mtime: 10,
		ArchiveSHA256: "device-original", ArchiveSize: 100, State: model.StateVerified,
	})
	if err := s.SetFrameMissing(ctx, row.ID, 1100); err != nil {
		t.Fatal(err)
	}

	if ok, err := s.CommitCulledRevision(ctx, CulledRevision{
		RigID: "rig-h13", FrameID: row.ID,
		SHA256: "device-revised", Size: 102880, Mtime: 20, DataSHA256: "proven-exposure",
	}); err != nil || !ok {
		t.Fatalf("CommitCulledRevision: ok=%v err=%v", ok, err)
	}

	if got, _, _ := s.GetFrameByID(ctx, row.ID); got.DataSHA256 != "proven-exposure" {
		t.Fatalf("data_sha256 = %q, want the identity this path was given to advance", got.DataSHA256)
	}
}
