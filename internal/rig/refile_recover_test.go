package rig

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// recover runs startup recovery with this harness's dependencies, the way the
// manager does before any worker exists.
func recoverIntents(t *testing.T, h *harness) {
	t.Helper()
	if err := RecoverRefileIntents(context.Background(), h.store, h.lfs, nil); err != nil {
		t.Fatalf("RecoverRefileIntents: %v", err)
	}
}

// TestRefileNeverStrandsAFrame (INV-M2, design routing-and-filing AC32) is the
// invariant whose failure destroys data: every way a frame can end up marked
// missing leads, one settle window later, to its device original being deleted
// against an archive nothing references.
//
// It drives the crash between the rename and the commit through re-file's own
// hook and proves the three things that together make the invariant true — the
// intent is open, so absentDeferReasons defers the deletion (by construction, not
// by alerting); startup recovery finishes the interrupted commit; and afterwards
// the row names a file that is actually there, so nothing marks it missing.
func TestRefileNeverStrandsAFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, refileSrc)
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)

	var crashedAt string
	h.w.crashAfterRefileRename = func(dest string) bool {
		crashedAt = dest
		return true
	}
	res, sum := applyGroup(t, h, g)
	if len(res) != 1 || res[0].Status != refileFailed {
		t.Fatalf("want the interrupted move reported as failed, got %+v", res)
	}
	if len(sum.PrunedDirs) != 0 {
		t.Fatalf("an interrupted move must prune nothing: %v", sum.PrunedDirs)
	}
	if crashedAt == "" || !fileExists(crashedAt) {
		t.Fatalf("the crash window requires the file at its new path: %q", crashedAt)
	}
	if got := mustGetFrame(t, h, f.ID); got.DestPath != f.DestPath {
		t.Fatalf("the row must still name the old path after a crash: %q", got.DestPath)
	}

	// INV-M2 by construction: the open intent is a deferral reason, so the frame is
	// never deletion-eligible in this window however the move failed.
	intents := openIntents(t, h)
	if !intents[f.DestPath] {
		t.Fatalf("the intent must be open across the crash window; got %v", intents)
	}
	row := mustGetFrame(t, h, f.ID)
	row.MissingSeenAt = h.nowVal.Add(-72 * time.Hour).Unix() // long past any settle window
	if reasons := absentDeferReasons(row, map[string]bool{h.cfg.Roots.Light: true}, intents, noIntents, 0, h.nowVal.Unix()); !containsReason(reasons, "refile_in_flight") {
		t.Fatalf("absentDeferReasons=%v, want refile_in_flight (INV-M2)", reasons)
	}

	// Startup recovery finishes the commit, and the row then names a file that is
	// really there — so the restart marks no frame missing.
	recoverIntents(t, h)
	after := mustGetFrame(t, h, f.ID)
	if after.DestPath != crashedAt {
		t.Fatalf("recovery must finish the interrupted commit: dest=%q, want %q", after.DestPath, crashedAt)
	}
	if !fileExists(after.DestPath) {
		t.Fatalf("the committed path does not exist: %s", after.DestPath)
	}
	if after.MissingSeenAt != 0 {
		t.Fatalf("recovery must leave no frame marked missing")
	}
	if len(openIntents(t, h)) != 0 {
		t.Fatalf("a resolved intent must be dropped")
	}
}

// TestRecoverRefileIntentChecksSourceFirst (design routing-and-filing AC32) proves
// the check order is not cosmetic: in a flat panel layout the ASIAIR's filenames
// collide by design and all lights from one camera and binning are identical in
// size, so a recovery that checked `to` first would bind the row to a SIBLING's
// file. Establishing that `from` is empty is what makes a file at `to` mean
// anything.
func TestRecoverRefileIntentChecksSourceFirst(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)

	// An intent whose rename never happened, with a same-named, same-sized decoy
	// already sitting at the destination.
	to := filepath.Join(h.cfg.Roots.Light, "elsewhere", filepath.Base(f.DestPath))
	body, err := os.ReadFile(f.DestPath)
	if err != nil {
		t.Fatal(err)
	}
	decoy := append([]byte(nil), body...)
	decoy[len(decoy)-1] ^= 0xFF // a sibling: same name, same size, different frame
	writeLocal(t, to, decoy)
	if err := h.store.PutRefileIntent(ctx, store.RefileIntent{
		RigID: "rig-01", FromPath: f.DestPath, ToPath: to, CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	recoverIntents(t, h)

	got := mustGetFrame(t, h, f.ID)
	if got.DestPath != f.DestPath {
		t.Fatalf("recovery bound the row to a sibling's file: dest=%q, want %q", got.DestPath, f.DestPath)
	}
	if len(openIntents(t, h)) != 0 {
		t.Fatalf("an intent whose rename never happened must be dropped")
	}
	if hashOnDisk(t, to) == archiveHash(got) {
		t.Fatal("setup: the decoy must not be this frame")
	}
}

// TestRecoverRefileIntentCases walks the remaining rows of the recovery table:
// the commit that already landed, the file that is genuinely gone, and the intent
// whose row was pruned or dismissed while it was open. All three drop the intent;
// none of them may move a file or invent a row.
func TestRecoverRefileIntentCases(t *testing.T) {
	ctx := context.Background()

	t.Run("commit already landed", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileLight(t, h, refileSrc)
		to := filepath.Join(filepath.Dir(f.DestPath), "moved.fit")
		if err := os.Rename(f.DestPath, to); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.MoveDestPath(ctx, "rig-01", f.DestPath, to, store.RequireRows); err != nil {
			t.Fatal(err)
		}
		if err := h.store.PutRefileIntent(ctx, store.RefileIntent{
			RigID: "rig-01", FromPath: f.DestPath, ToPath: to, CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			t.Fatal(err)
		}

		recoverIntents(t, h)

		if got := mustGetFrame(t, h, f.ID); got.DestPath != to {
			t.Fatalf("recovery must leave a committed row alone: %q", got.DestPath)
		}
		if len(openIntents(t, h)) != 0 {
			t.Fatalf("the leftover intent must be dropped")
		}
	})

	t.Run("file at neither path", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileLight(t, h, refileSrc)
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		to := filepath.Join(h.cfg.Roots.Light, "elsewhere", "x.fit")
		if err := h.store.PutRefileIntent(ctx, store.RefileIntent{
			RigID: "rig-01", FromPath: f.DestPath, ToPath: to, CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			t.Fatal(err)
		}

		recoverIntents(t, h)

		// Normal missing-frame handling applies: the row is untouched, and the intent
		// no longer defers anything.
		if got := mustGetFrame(t, h, f.ID); got.DestPath != f.DestPath {
			t.Fatalf("recovery must not repoint a row at a file that is not there: %q", got.DestPath)
		}
		if len(openIntents(t, h)) != 0 {
			t.Fatalf("the intent must be dropped")
		}
	})

	t.Run("no row matches from_path", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		from := filepath.Join(h.cfg.Roots.Light, "pruned", "gone.fit")
		to := filepath.Join(h.cfg.Roots.Light, "elsewhere", "gone.fit")
		writeLocal(t, to, []byte("a file whose row was pruned mid-move"))
		if err := h.store.PutRefileIntent(ctx, store.RefileIntent{
			RigID: "rig-01", FromPath: from, ToPath: to, CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			t.Fatal(err)
		}

		recoverIntents(t, h)

		if len(openIntents(t, h)) != 0 {
			t.Fatalf("an intent with nothing to commit to must be dropped")
		}
		if !fileExists(to) {
			t.Fatalf("recovery must never remove a file")
		}
	})
}

// TestRecoverRefileIntentCommitsEverySharingRow proves an intent names one
// dest_path, not one row: when several rows share it (the H12 two-share case)
// recovery commits every one of them, never a subset.
func TestRecoverRefileIntentCommitsEverySharingRow(t *testing.T) {
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

	// Crash the move between the rename and the commit.
	h.cfg.Templates.Light = refileTemplate
	g := previewOneGroup(t, h)
	h.w.crashAfterRefileRename = func(string) bool { return true }
	applyGroup(t, h, g)

	recoverIntents(t, h)

	to := ""
	for _, r := range rows {
		got := mustGetFrame(t, h, r.ID)
		if to == "" {
			to = got.DestPath
		}
		if got.DestPath == first.DestPath || got.DestPath != to {
			t.Fatalf("recovery committed a subset: row %d dest=%q, want %q", r.ID, got.DestPath, to)
		}
	}
}
