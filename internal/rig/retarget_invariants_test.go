package rig

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// filesUnder returns every regular file whose path contains sub, walking root. Used
// to locate a crash-window orphan whose exact night-ordinal path is not known ahead
// of time (preview never assigns a night, so its projected path differs).
func filesUnder(t *testing.T, root, sub string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(p, sub) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// deviceFileHash streams the device-side file at src and returns its SHA-256, so a
// test can prove the untouched device original still hashes to the recorded sha256.
func deviceFileHash(t *testing.T, h *harness, src string) string {
	t.Helper()
	sess, err := h.dev.Open(context.Background())
	if err != nil {
		t.Fatalf("device open: %v", err)
	}
	defer sess.Close()
	sh, err := sess.Mount(h.share)
	if err != nil {
		t.Fatalf("device mount: %v", err)
	}
	rc, err := sh.OpenReader(src)
	if err != nil {
		t.Fatalf("device open reader %s: %v", src, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("device read %s: %v", src, err)
	}
	return sha256hex(b)
}

// confirmDeleteIdentity wires the resolver/probe so the delete pass's live identity
// re-confirmation passes, matching TestDelete_RetargetedThreeWay's setup.
func confirmDeleteIdentity(h *harness) {
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
}

// TestRetarget_DeviceOriginalUntouched (INV-2) proves a full retarget run edits only
// the NAS archive and never issues a mutating call to the device Share: the device
// original is present and byte-identical afterward, and the fake records zero
// device mutations across the operation.
func TestRetarget_DeviceOriginalUntouched(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)

	beforeMut := h.dev.Mutations()
	beforeHash := deviceFileHash(t, h, src)
	if beforeHash != f.SHA256 {
		t.Fatalf("setup: device hash %q != recorded sha256 %q", beforeHash, f.SHA256)
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}

	if got := h.dev.Mutations(); got != beforeMut {
		t.Fatalf("retarget issued %d device-mutating call(s); INV-2 forbids touching the device original", got-beforeMut)
	}
	if !h.deviceHas(src) {
		t.Fatalf("device original vanished; a retarget must never remove it (INV-2)")
	}
	if after := deviceFileHash(t, h, src); after != beforeHash {
		t.Fatalf("device original bytes changed: %q != %q (INV-2)", after, beforeHash)
	}
	// The NAS archive WAS edited — proof this is a real retarget, not a silent no-op
	// that would make the device-untouched assertion vacuous.
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 == "" || got.RetargetSHA256 == f.SHA256 {
		t.Fatalf("retarget did not edit the NAS archive (retarget_sha256=%q); nothing was proven", got.RetargetSHA256)
	}
}

// TestRetarget_RefusesCorruptingWriter (INV-4, retarget level) substitutes, via the
// injectable rewriter seam, a writer that corrupts a data-unit byte and reports the
// differing complement digest an honest writer would. retargetFrame must REFUSE at
// the faithfulness assertion: a failed result, no edited archive filed, no
// retarget_sha256 set, and the old file untouched — proving a buggy writer can never
// authorize deleting the correct device original.
func TestRetarget_RefusesCorruptingWriter(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	oldDest := f.DestPath

	h.w.rewriter = func(srcPath, dst, newTarget string) (header.RewriteResult, error) {
		// Produce a well-formed edit first (so size and re-classification would pass),
		// then corrupt one data-unit byte outside the OBJECT card.
		rr, err := header.RewriteObject(srcPath, dst, newTarget)
		if err != nil {
			return rr, err
		}
		b, rerr := os.ReadFile(dst)
		if rerr != nil {
			return rr, rerr
		}
		b[len(b)-1] ^= 0xFF // the final byte lives in the data unit, never the header
		if werr := os.WriteFile(dst, b, 0o644); werr != nil {
			return rr, werr
		}
		// An honest complement digest over the corrupted output differs from the input's;
		// report that so the load-bearing complement check trips (INV-4).
		rr.ComplementOut[0] ^= 0xFF
		return rr, nil
	}

	res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
	if err != nil {
		t.Fatalf("serviceRetarget: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("status=%q, want failed on a corrupting writer (INV-4)", res.Status)
	}
	if !strings.Contains(res.Reason, "complement") {
		t.Fatalf("reason=%q, want the complement-digest refusal", res.Reason)
	}

	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 != "" {
		t.Fatalf("a refused corrupting edit must never set retarget_sha256; got %q", got.RetargetSHA256)
	}
	if got.Target != "C-2025 A6" || got.DestPath != oldDest {
		t.Fatalf("refusal must not move/relabel the frame: target=%q dest=%q", got.Target, got.DestPath)
	}
	if got.SHA256 != f.SHA256 {
		t.Fatalf("refusal must not touch sha256: %q != %q", got.SHA256, f.SHA256)
	}
	if _, err := os.Stat(oldDest); err != nil {
		t.Fatalf("the old archive must survive a refused edit: %v", err)
	}
	if hd := hashOnDisk(t, oldDest); hd != f.SHA256 {
		t.Fatalf("old archive content changed on a refused edit: %q != %q", hd, f.SHA256)
	}
	if !h.deviceHas(src) {
		t.Fatalf("device original must survive a refused edit")
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatalf("retarget temp leaked after a refused edit")
	}
}

// TestRetarget_CrashWindows (INV-6) simulates the two design routing-and-filing §9.3
// crash windows that leave persistent state and proves each is recoverable with no
// false device delete and no data loss: (a) a crash AFTER the fileFrom rename but
// BEFORE the DB commit, and (b) a crash AFTER the commit but BEFORE the old file is
// removed. For each, the persistent state matches the design routing-and-filing §9.3
// crash-window table, and a subsequent delete pass authorizes the device-original
// deletion only against an intact archive that hashes to archiveHash(row) — so the
// frame is always recoverable from the NAS copy the row points at.
func TestRetarget_CrashWindows(t *testing.T) {
	// Window (a): crash after the rename, before the DB commit. The row stays
	// un-retargeted at the OLD path (overlay NULL) while an orphan NEW file exists.
	t.Run("after_rename_before_commit", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		confirmDeleteIdentity(h)
		src := "Autorun/Light/C-2025 A6/Light_0001.fit"
		f := fileLight(t, h, src)
		oldDest := f.DestPath

		// Model the crash: fileFrom renames the temp into the M31 folder, then the
		// process dies before recordFiled commits the row.
		h.w.crashAfterRename = func(dest string) bool { return strings.Contains(dest, "M31") }

		res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
		if err != nil {
			t.Fatalf("serviceRetarget: %v", err)
		}
		if res.Status != "failed" {
			t.Fatalf("status=%q, want failed (the commit never ran)", res.Status)
		}
		h.w.crashAfterRename = nil

		// design routing-and-filing §9.3 table row 2: OLD + NEW both exist; DB row at
		// OLD path, overlay NULL.
		got, _, _ := h.store.GetFrameByID(ctx, f.ID)
		if got.DestPath != oldDest || got.RetargetSHA256 != "" || got.Target != "C-2025 A6" {
			t.Fatalf("crash-before-commit must leave the row un-retargeted: dest=%q target=%q overlay=%q",
				got.DestPath, got.Target, got.RetargetSHA256)
		}
		if _, err := os.Stat(oldDest); err != nil {
			t.Fatalf("OLD archive must remain intact: %v", err)
		}
		orphans := filesUnder(t, h.cfg.Roots.Light, filepath.Join("M31", "FRA300"))
		if len(orphans) != 1 {
			t.Fatalf("want exactly one NEW orphan under M31 (the rename happened); got %v", orphans)
		}

		// A subsequent delete pass: the three-way check on the OLD path passes (device
		// == sha256, OLD == archiveHash == sha256), so the device original is deleted on
		// a TRUE premise, never a false one, and the OLD archive is preserved.
		h.nowVal = h.nowVal.Add(2 * time.Hour)
		h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

		if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
			t.Fatalf("a crash-window frame must not raise a false loss/hold; got %v", h.raisedCategories())
		}
		after, _, _ := h.store.GetFrameByID(ctx, f.ID)
		if after.State != model.StateDeleted {
			t.Fatalf("state=%s, want deleted (device removed against the intact OLD archive)", after.State)
		}
		if hd := hashOnDisk(t, oldDest); hd != after.SHA256 {
			t.Fatalf("no data loss: the OLD archive the row points at must survive and verify; %q != %q", hd, after.SHA256)
		}
	})

	// Window (b): crash after the commit, before the OLD file is removed. The row is
	// at the NEW path with the overlay set; the OLD file lingers as an orphan.
	t.Run("after_commit_before_old_removed", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		confirmDeleteIdentity(h)
		src := "Autorun/Light/C-2025 A6/Light_0001.fit"
		f := fileLight(t, h, src)
		oldDest := f.DestPath

		// Model the crash: the commit lands, but the process dies before the step-7
		// removal of the old archive. Scope the failure to the OLD path so the staging
		// temp still cleans up.
		h.lfs.RemoveErr = func(path string) error {
			if path == oldDest {
				return os.ErrPermission
			}
			return nil
		}

		res, err := h.w.serviceRetarget(ctx, f.ID, "M31")
		if err != nil {
			t.Fatalf("serviceRetarget: %v", err)
		}
		if res.Status != "retargeted" {
			t.Fatalf("status=%q reason=%q, want retargeted (the commit landed)", res.Status, res.Reason)
		}
		h.lfs.RemoveErr = nil

		// design routing-and-filing §9.3 table row 3: OLD orphan + NEW; DB row at NEW
		// path, overlay set.
		got, _, _ := h.store.GetFrameByID(ctx, f.ID)
		if got.Target != "M31" || got.RetargetSHA256 == "" || !strings.Contains(got.DestPath, "M31") {
			t.Fatalf("crash-before-old-removal must leave the row retargeted at NEW: target=%q dest=%q overlay=%q",
				got.Target, got.DestPath, got.RetargetSHA256)
		}
		if got.DestPath == oldDest {
			t.Fatalf("the row must point at the NEW path")
		}
		if _, err := os.Stat(oldDest); err != nil {
			t.Fatalf("OLD file must linger as an orphan: %v", err)
		}
		if hd := hashOnDisk(t, got.DestPath); hd != got.RetargetSHA256 {
			t.Fatalf("NEW archive must verify against retarget_sha256: %q != %q", hd, got.RetargetSHA256)
		}

		// The delete pass verifies device == sha256 and NEW == retarget_sha256, deleting
		// the device original against the proven NEW archive — no false premise.
		h.nowVal = h.nowVal.Add(2 * time.Hour)
		h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

		if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
			t.Fatalf("a crash-window frame must not raise a false loss/hold; got %v", h.raisedCategories())
		}
		after, _, _ := h.store.GetFrameByID(ctx, f.ID)
		if after.State != model.StateDeleted {
			t.Fatalf("state=%s, want deleted", after.State)
		}
		if hd := hashOnDisk(t, after.DestPath); hd != after.RetargetSHA256 {
			t.Fatalf("no data loss: the NEW archive the row points at must survive and verify; %q != %q", hd, after.RetargetSHA256)
		}
		if _, err := os.Stat(oldDest); err != nil {
			t.Fatalf("the OLD orphan is cosmetic and should still be present post-delete: %v", err)
		}
	})
}

// TestDelete_RetargetedVanishedSource (INV-5) proves handleVanishedSource compares
// the present NAS copy against archiveHash(f) (= retarget_sha256): a retargeted
// frame whose device source has vanished but whose healthy archive is present is a
// benign source-vanish, marked deleted — NOT a false frame_lost critical (which a
// sha256 comparison against the edited archive would wrongly raise).
func TestDelete_RetargetedVanishedSource(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if got.RetargetSHA256 == "" {
		t.Fatalf("setup: expected a retargeted overlay")
	}

	h.dev.RemoveFile(emmc, src) // device source vanished; the M31 archive is intact
	h.nowVal = h.nowVal.Add(2 * time.Hour)

	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("a healthy retargeted archive must not raise frame_lost on a vanished source (archiveHash, design routing-and-filing §9.5); got %v", h.raisedCategories())
	}
	after, _, _ := h.store.GetFrameByID(ctx, f.ID)
	if after.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted (benign source-vanish)", after.State)
	}
	if hd := hashOnDisk(t, after.DestPath); hd != after.RetargetSHA256 {
		t.Fatalf("the retargeted archive must remain intact: %q != %q", hd, after.RetargetSHA256)
	}
}

// TestDiffer_RetargetedNoFalseLost (INV-5) exercises the differ branch (decide.go:129)
// on a retargeted verified frame whose archive is intact: the device reused the source
// path with genuinely-new content (a size change). resolveDiffer must read the archive
// via archiveHash(row) (= retarget_sha256), see a benign overwrite, and pull — never
// raise a false CRITICAL frame_lost (which a sha256 comparison against the edited
// archive would).
func TestDiffer_RetargetedNoFalseLost(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	f := fileLight(t, h, src)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	got, _, _ := h.store.GetFrameByID(ctx, f.ID)

	// The device wrote new content (a different size) at the same path → differ branch.
	lf := listedFrame{share: emmc, path: src, name: "Light_0001.fit", size: got.Size + 4096, mtime: h.nowVal}
	pull, prior := h.w.decide(ctx, h.cfg, h.profile, lf)
	if !pull {
		t.Fatalf("the differ branch must pull the new content over a benign overwrite")
	}
	if prior == nil || prior.ID != f.ID {
		t.Fatalf("decide must return the prior retargeted row")
	}
	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("an intact retargeted archive must not raise frame_lost on the differ branch (decide.go:129 archiveHash); got %v", h.raisedCategories())
	}
}
