package rig

import (
	"context"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/mediaid"
	"github.com/macourteau/asiair-commander/internal/model"
)

const udisk = "Udisk Images"

// asiairMediaMarker is the media marker's canonical ASIAIR/ subpath — the fake
// share key a test seeds or inspects for the relocated marker.
const asiairMediaMarker = "ASIAIR/" + model.MediaMarkerName

// seedRootMarker persists media id and seeds a legacy share-root marker carrying
// that id, modelling a medium adopted by an older Commander before relocation.
func (h *harness) seedRootMarker(share string) (id string) {
	h.t.Helper()
	m, err := mediaid.Generate("rig-01", "")
	if err != nil {
		h.t.Fatalf("Generate: %v", err)
	}
	if err := h.store.SetMeta(context.Background(), MediaIDKey("rig-01", share), m.ID); err != nil {
		h.t.Fatalf("seed persisted media id: %v", err)
	}
	h.dev.SetMarker(share, model.MediaMarkerName, m.Encode())
	return m.ID
}

// testRoots mirrors the harness rig profile's configured roots (rig_test.go), so
// removableMediumAbsent probes for the same folder the real worker would.
var testRoots = []string{"Autorun"}

// usable drives mediumUsable for the tests that assert only the cleanup verdict,
// discarding the presence result. Presence is asserted directly by the tests that
// exercise the drained-versus-absent distinction.
func (h *harness) usable(ctx context.Context, share device.Share, hasScience bool) bool {
	h.t.Helper()
	u, _ := h.w.mediumUsable(ctx, share, testRoots, hasScience)
	return u
}

// mountShare mounts a share on the harness's fake device, returning the handle so
// a test can drive mediumUsable directly.
// mountedSession opens a session the way a delete or restore pass does, so a test
// driving deleteOne or restoreNAS directly hands them the same thing the pass does.
func (h *harness) mountedSession(t *testing.T) *mountedSession {
	t.Helper()
	sess, err := h.dev.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ms := newMountedSession(sess)
	t.Cleanup(func() { _ = ms.Close() })
	return ms
}

func (h *harness) mountShare(name string) device.Share {
	h.t.Helper()
	sess, err := h.dev.Open(context.Background())
	if err != nil {
		h.t.Fatalf("open: %v", err)
	}
	sh, err := sess.Mount(name)
	if err != nil {
		h.t.Fatalf("mount %s: %v", name, err)
	}
	return sh
}

// persistedMediaID reads back the media id the worker persisted for a share.
func (h *harness) persistedMediaID(share string) string {
	h.t.Helper()
	v, _, err := h.store.GetMeta(context.Background(), MediaIDKey("rig-01", share))
	if err != nil {
		h.t.Fatalf("GetMeta: %v", err)
	}
	return v
}

// TestMediumUsableSwapDetected is the core swap-detection regression: a medium
// adopted on first cleanup (its id persisted) that is later swapped for a
// different stick carrying a foreign media marker must be judged unusable and
// raise a media-changed alert, rather than passing on marker existence alone.
func TestMediumUsableSwapDetected(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	// First managed cleanup over a populated stick adopts and persists a media id.
	if !h.usable(ctx, share, true) {
		t.Fatal("first cleanup over a populated stick should adopt and be usable")
	}
	adopted := h.persistedMediaID(udisk)
	if adopted == "" {
		t.Fatal("first cleanup should persist the adopted media id")
	}

	// The stick is swapped for a different medium whose ASIAIR/ folder (the
	// canonical marker location, read first) carries a foreign media marker.
	foreign, err := mediaid.Generate("someone-else", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.dev.SetMarker(udisk, asiairMediaMarker, foreign.Encode())

	if h.usable(ctx, share, true) {
		t.Fatal("a swapped medium carrying a foreign marker must be unusable")
	}
	if !h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("a media swap must raise a media-changed alert; got %v", h.raisedCategories())
	}
	// The persisted id must not drift to the foreign one — the adopted identity stands.
	if got := h.persistedMediaID(udisk); got != adopted {
		t.Fatalf("persisted media id drifted to %q, want the adopted %q", got, adopted)
	}
}

// TestMediumUsableAdoptFailureDegradesGracefully: when the media marker cannot be
// written (a read-only removable root), removable cleanup is skipped for the
// cycle, but pulls still happen and the worker does not panic.
func TestMediumUsableAdoptFailureDegradesGracefully(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()

	// The removable root refuses marker writes.
	h.dev.FailWriteMarker(udisk, device.NetworkError("read-only root"))

	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/x.fit"
	h.dev.AddFile(udisk, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle() // must not panic

	// The frame is pulled and verified despite the failed marker write.
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatal("a failed media-marker write must not stop pulls")
	}
	if f.State != model.StateVerified {
		t.Fatalf("frame state=%s, want verified", f.State)
	}

	// Cleanup is refused this cycle (the medium could not be adopted).
	share := h.mountShare(udisk)
	if h.usable(ctx, share, true) {
		t.Fatal("a medium whose marker write fails must be unusable for cleanup")
	}
}

// TestMediumUsableMatch: the adopted medium reading back its own marker is usable.
func TestMediumUsableMatch(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	if !h.usable(ctx, share, true) { // adopt
		t.Fatal("first cleanup should be usable")
	}
	if !h.usable(ctx, share, true) { // same stick, same marker
		t.Fatal("the adopted medium reading back its own marker must be usable")
	}
	if h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("a matching medium must not alert; got %v", h.raisedCategories())
	}
}

// TestMediumUsableAbsentMarkerEmptyStickUnusable: an empty root with no marker is
// an absent/removed stick, not "all transferred" — unusable, no alert.
func TestMediumUsableAbsentMarkerEmptyStickUnusable(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	if h.usable(ctx, share, false) {
		t.Fatal("an empty root with no media marker is an absent stick, not usable")
	}
	if h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("an absent stick must not raise a swap alert; got %v", h.raisedCategories())
	}
	if got := h.persistedMediaID(udisk); got != "" {
		t.Fatalf("an absent stick must not persist a media id, got %q", got)
	}
}

// TestMediumUsableAbsentMarkerPopulatedAdopts: a populated root with no marker is
// first-cleanup adoption — usable, and the freshly minted id is persisted and
// present on the medium.
func TestMediumUsableAbsentMarkerPopulatedAdopts(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	if !h.usable(ctx, share, true) {
		t.Fatal("a populated root with no marker should adopt and be usable")
	}
	persisted := h.persistedMediaID(udisk)
	if persisted == "" {
		t.Fatal("adoption must persist a media id")
	}
	// The marker lands under ASIAIR/ (the canonical, provably guest-writable
	// location), never at the share root.
	if _, err := share.ReadMarker(model.MediaMarkerName); !device.IsNotFound(err) {
		t.Fatalf("adoption must not write the marker at the share root, ReadMarker err = %v", err)
	}
	data, err := share.ReadMarker(asiairMediaMarker)
	if err != nil {
		t.Fatalf("ReadMarker(ASIAIR/) after adoption: %v", err)
	}
	m, err := mediaid.Parse(data)
	if err != nil {
		t.Fatalf("Parse adopted marker: %v", err)
	}
	if m.ID != persisted {
		t.Fatalf("on-medium marker id %q != persisted id %q", m.ID, persisted)
	}
}

// TestMediumUsableMigratesLegacyRootMarker: a medium adopted by an older
// Commander carries its marker at the share root. On a Match the pull phase
// relocates it into ASIAIR/ (write, read-back-verify, delete root) while reusing
// the SAME persisted id — a relocation is not a fresh adoption.
func TestMediumUsableMigratesLegacyRootMarker(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	adopted := h.seedRootMarker(udisk)

	if !h.usable(ctx, share, true) {
		t.Fatal("a legacy root marker matching the persisted id must be usable")
	}
	// The marker moved into ASIAIR/ and the root copy is gone.
	data, err := share.ReadMarker(asiairMediaMarker)
	if err != nil {
		t.Fatalf("migrated marker not readable under ASIAIR/: %v", err)
	}
	if m, perr := mediaid.Parse(data); perr != nil || m.ID != adopted {
		t.Fatalf("migrated ASIAIR/ marker = %+v (err %v), want id %s", m, perr, adopted)
	}
	if _, err := share.ReadMarker(model.MediaMarkerName); !device.IsNotFound(err) {
		t.Fatalf("migration must delete the legacy root copy, ReadMarker err = %v", err)
	}
	// The persisted id is unchanged: migration reuses it, never re-mints.
	if got := h.persistedMediaID(udisk); got != adopted {
		t.Fatalf("persisted id changed to %q on migration, want the reused %q", got, adopted)
	}
	if h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("a migration of our own marker must not alert; got %v", h.raisedCategories())
	}
}

// TestMediumUsableForeignRootMarkerNotMigrated: a share-root marker whose id does
// not match the persisted id is a swapped stick — it must be refused and touched
// nothing (no ASIAIR/ write, no root delete), so the swap guard is not bypassed
// by relocating a foreign marker.
func TestMediumUsableForeignRootMarkerNotMigrated(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	// Persist one id, but the root marker carries a different (foreign) one.
	if err := h.store.SetMeta(ctx, MediaIDKey("rig-01", udisk), "the-adopted-id"); err != nil {
		t.Fatalf("seed persisted id: %v", err)
	}
	foreign, err := mediaid.Generate("someone-else", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.dev.SetMarker(udisk, model.MediaMarkerName, foreign.Encode())

	if h.usable(ctx, share, true) {
		t.Fatal("a foreign root marker must be unusable")
	}
	if !h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("a foreign medium must raise a media-changed alert; got %v", h.raisedCategories())
	}
	// Nothing was migrated: no ASIAIR/ copy written, the root copy left in place.
	if _, err := share.ReadMarker(asiairMediaMarker); !device.IsNotFound(err) {
		t.Fatalf("a foreign marker must NOT be migrated into ASIAIR/, err = %v", err)
	}
	if _, err := share.ReadMarker(model.MediaMarkerName); err != nil {
		t.Fatalf("a foreign root marker must be left untouched, ReadMarker err = %v", err)
	}
}

// TestMediumUsableMigrationRootDeleteFailureProceeds: when the legacy root copy
// cannot be deleted (a read-only root), the ASIAIR/ copy the migration wrote is
// authoritative (read first) and cleanup still proceeds.
func TestMediumUsableMigrationRootDeleteFailureProceeds(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	adopted := h.seedRootMarker(udisk)
	// The root copy cannot be removed, but ASIAIR/ writes still succeed.
	h.dev.FailDelete(udisk, device.NetworkError("read-only root"))

	if !h.usable(ctx, share, true) {
		t.Fatal("a matched medium must be usable even when the root copy can't be deleted")
	}
	// The authoritative ASIAIR/ copy was written and carries the reused id.
	data, err := share.ReadMarker(asiairMediaMarker)
	if err != nil {
		t.Fatalf("ASIAIR/ copy not written: %v", err)
	}
	if m, perr := mediaid.Parse(data); perr != nil || m.ID != adopted {
		t.Fatalf("ASIAIR/ marker = %+v (err %v), want id %s", m, perr, adopted)
	}
	if got := h.persistedMediaID(udisk); got != adopted {
		t.Fatalf("persisted id changed to %q, want the reused %q", got, adopted)
	}
}

// TestMediumUsablePostResetRootMarkerReadopts: after a reset that cleared the
// persisted id but could not delete a legacy root marker (read-only root), the
// next pass must re-adopt into ASIAIR/ (persistedID=="" + root-only marker), not
// judge the lingering root marker Foreign and re-block cleanup forever.
func TestMediumUsablePostResetRootMarkerReadopts(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	// A lingering legacy root marker, but no persisted identity (post-reset).
	stale, err := mediaid.Generate("rig-01", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.dev.SetMarker(udisk, model.MediaMarkerName, stale.Encode())

	if !h.usable(ctx, share, true) {
		t.Fatal("a post-reset root-only marker with no persisted id must re-adopt, not refuse")
	}
	if h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("re-adoption must not raise a swap alert; got %v", h.raisedCategories())
	}
	// A fresh id was minted and adopted into ASIAIR/.
	fresh := h.persistedMediaID(udisk)
	if fresh == "" || fresh == stale.ID {
		t.Fatalf("re-adoption should mint a fresh id (got %q, stale %q)", fresh, stale.ID)
	}
	data, err := share.ReadMarker(asiairMediaMarker)
	if err != nil {
		t.Fatalf("re-adopted ASIAIR/ marker not readable: %v", err)
	}
	if m, perr := mediaid.Parse(data); perr != nil || m.ID != fresh {
		t.Fatalf("ASIAIR/ marker = %+v (err %v), want fresh id %s", m, perr, fresh)
	}
}

// TestMediaConfirmedForDelete exercises the delete-phase read-only resolver:
// ASIAIR/ first, root only on not-found, transient errors stay a defer, and both
// absent refuses.
func TestMediaConfirmedForDelete(t *testing.T) {
	t.Run("confirms via ASIAIR", func(t *testing.T) {
		h := newHarness(t, udisk, header.New())
		share := h.mountShare(udisk)
		h.dev.SetMarker(udisk, asiairMediaMarker, []byte(`{"id":"x"}`+"\n"))
		if !h.w.mediaConfirmedForDelete(share) {
			t.Fatal("an ASIAIR/ marker must confirm the medium for delete")
		}
	})

	t.Run("falls back to root on not-found", func(t *testing.T) {
		h := newHarness(t, udisk, header.New())
		share := h.mountShare(udisk)
		h.dev.SetMarker(udisk, model.MediaMarkerName, []byte(`{"id":"x"}`+"\n"))
		if !h.w.mediaConfirmedForDelete(share) {
			t.Fatal("a legacy root marker must confirm when ASIAIR/ is absent")
		}
	})

	t.Run("transient ASIAIR error stays a defer", func(t *testing.T) {
		h := newHarness(t, udisk, header.New())
		share := h.mountShare(udisk)
		// A good root marker exists, but the ASIAIR/ read faults transiently: the
		// resolver must propagate the fault, NOT fall through to the root success.
		h.dev.SetMarker(udisk, model.MediaMarkerName, []byte(`{"id":"x"}`+"\n"))
		h.dev.FailReadMarker(udisk, asiairMediaMarker, device.NetworkError("link reset"))
		if h.w.mediaConfirmedForDelete(share) {
			t.Fatal("a transient ASIAIR/ read error must defer, not fall through to root")
		}
	})

	t.Run("refuses when neither present", func(t *testing.T) {
		h := newHarness(t, udisk, header.New())
		share := h.mountShare(udisk)
		if h.w.mediaConfirmedForDelete(share) {
			t.Fatal("no marker at either location must refuse the delete")
		}
	})

	t.Run("fixed medium always confirmed", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		share := h.mountShare(emmc)
		if !h.w.mediaConfirmedForDelete(share) {
			t.Fatal("a fixed medium is always confirmed for delete")
		}
	})
}

// TestResetIdentityClearsMediaIDAndReadopts: an operator reset clears the
// persisted media id and wipes the on-medium marker, so a deliberately swapped
// stick (carrying a foreign marker at reset time) re-adopts cleanly on the next
// pass rather than tripping the swap guard forever.
func TestResetIdentityClearsMediaIDAndReadopts(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	if !h.usable(ctx, share, true) { // adopt id X
		t.Fatal("first cleanup should adopt")
	}
	adopted := h.persistedMediaID(udisk)

	// Operator physically swaps the stick; the new one carries a foreign marker in
	// its ASIAIR/ folder (the canonical location).
	foreign, err := mediaid.Generate("someone-else", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.dev.SetMarker(udisk, asiairMediaMarker, foreign.Encode())

	// Reset only proceeds for a guid-bound rig once a live probe confirms the
	// connected device reports that guid (design device-identity-and-media §4).
	// Bind the rig and script a matching probe so the clear-and-readopt path runs.
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true

	// Reset clears the persisted id and deletes the on-medium marker.
	h.w.resetIdentity(ctx)
	if got := h.persistedMediaID(udisk); got != "" {
		t.Fatalf("reset must clear the persisted media id, got %q", got)
	}
	if _, err := share.ReadMarker(asiairMediaMarker); !device.IsNotFound(err) {
		t.Fatalf("reset must delete the ASIAIR/ media marker, ReadMarker err = %v", err)
	}

	// Next pass over the populated (now marker-less) stick re-adopts a fresh id.
	if !h.usable(ctx, share, true) {
		t.Fatal("after reset the swapped stick should re-adopt and be usable")
	}
	if got := h.persistedMediaID(udisk); got == "" || got == adopted {
		t.Fatalf("re-adoption should mint a fresh id (got %q, old %q)", got, adopted)
	}
}

// TestMediumPresenceDrainedStickIsPresent is the core regression for the
// drained-stick trap: a removable medium that is inserted but holds no frames is
// NOT usable (nothing is adopted, so nothing may be cleaned) yet IS present — and
// only absence may suppress the safe-to-power-off verdict. Reporting a drained
// stick absent pinned the rig at "unknown" forever, with no user action able to
// clear it.
func TestMediumPresenceDrainedStickIsPresent(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	// The stick's data structure survives a drain even though every frame is gone.
	h.dev.AddDir(udisk, "ASIAIR", h.nowVal)
	share := h.mountShare(udisk)

	usable, present := h.w.mediumUsable(ctx, share, testRoots, false)
	if usable {
		t.Fatal("a drained stick has nothing adopted, so it must not be usable for cleanup")
	}
	if !present {
		t.Fatal("a drained stick is still inserted; reporting it absent pins the rig at unknown forever")
	}
	if h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("a drained stick must not raise a swap alert; got %v", h.raisedCategories())
	}
}

// TestMediumPresenceAbsentStickIsAbsent pins the other side of the H5 distinction:
// with no data structure at the share root the slot really is empty, so the medium
// reads absent and the verdict stays unknown. Paired with the drained test above,
// this stops a future change from fixing one case by breaking the other.
func TestMediumPresenceAbsentStickIsAbsent(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	// newHarness registers an empty udisk share: mounts, lists, holds nothing.
	share := h.mountShare(udisk)

	usable, present := h.w.mediumUsable(ctx, share, testRoots, false)
	if usable {
		t.Fatal("an absent stick must not be usable")
	}
	if present {
		t.Fatal("an empty slot with no data structure must read absent")
	}
}

// TestMediumPresenceMatchIsPresent guards the load-bearing row of the presence
// table: the ordinary healthy case — an adopted, populated stick reading back its
// own marker — must report present. Getting this wrong sets mediaAbsent on every
// healthy rig and inverts the drained-stick bug onto the normal path.
func TestMediumPresenceMatchIsPresent(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	if !h.usable(ctx, share, true) { // adopt
		t.Fatal("first cleanup over a populated stick should adopt")
	}
	usable, present := h.w.mediumUsable(ctx, share, testRoots, true)
	if !usable || !present {
		t.Fatalf("the adopted medium must be usable and present; got usable=%v present=%v", usable, present)
	}
}

// TestMediumPresenceForeignIsPresent: a foreign marker proves a stick is inserted
// — it is simply not ours. Cleanup stays refused and the swap alert stays raised,
// but the medium must not read absent.
func TestMediumPresenceForeignIsPresent(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	if !h.usable(ctx, share, true) { // adopt, persisting our id
		t.Fatal("first cleanup should adopt")
	}
	foreign, err := mediaid.Generate("someone-else", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.dev.SetMarker(udisk, asiairMediaMarker, foreign.Encode())

	usable, present := h.w.mediumUsable(ctx, share, testRoots, true)
	if usable {
		t.Fatal("a swapped medium must stay unusable for cleanup")
	}
	if !present {
		t.Fatal("a foreign marker proves a medium is inserted; it must not read absent")
	}
	if !h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("a media swap must still alert; got %v", h.raisedCategories())
	}
}

// TestMediumUsableNoPersistedIDReadopts covers the reinstall/host-move case: a
// Commander with no persisted media id meeting a stick that already carries a
// marker must re-adopt, not judge it Foreign. With no prior adoption there is
// nothing for the swap guard to protect, so refusing would block cleanup behind a
// manual reset for a medium that is very likely our own.
func TestMediumUsableNoPersistedIDReadopts(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	// A marker from a previous install sits at the canonical location; this
	// install's store knows nothing about it.
	previous, err := mediaid.Generate("previous-install", "")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	h.dev.SetMarker(udisk, asiairMediaMarker, previous.Encode())
	if got := h.persistedMediaID(udisk); got != "" {
		t.Fatalf("precondition: no persisted id expected, got %q", got)
	}

	if !h.usable(ctx, share, true) {
		t.Fatal("with no persisted id the medium must re-adopt, not be judged foreign")
	}
	if h.hasAlert(model.AlertMediaChanged) {
		t.Fatalf("re-adoption is not a swap and must not alert; got %v", h.raisedCategories())
	}
	// The medium's EXISTING identity is adopted in place, not overwritten with a
	// fresh one. Re-branding would make any other install sharing this stick see a
	// foreign marker next cycle, and the two would take turns claiming it forever.
	if got := h.persistedMediaID(udisk); got != previous.ID {
		t.Fatalf("persisted media id = %q, want the medium's existing %q — the marker must be adopted, not re-minted", got, previous.ID)
	}
	data, err := share.ReadMarker(asiairMediaMarker)
	if err != nil {
		t.Fatalf("ReadMarker after re-adoption: %v", err)
	}
	m, err := mediaid.Parse(data)
	if err != nil {
		t.Fatalf("Parse marker after re-adoption: %v", err)
	}
	if m.ID != previous.ID {
		t.Fatalf("on-medium marker id = %q, want the untouched %q", m.ID, previous.ID)
	}
}

// TestMediumPresenceUnreadableRootIsNotPresent closes the one fail-open hole in
// the presence probe: a share root that will not list at all proves nothing about
// whether a stick is inserted. It must read not-present, like every other read
// failure on this path — otherwise a stick pulled between the frame walk and the
// presence check could leave the rig reading "safe to power off".
func TestMediumPresenceUnreadableRootIsNotPresent(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()
	share := h.mountShare(udisk)

	h.dev.FailListDir(udisk, device.NetworkError("share root unreadable"))

	usable, present := h.w.mediumUsable(ctx, share, testRoots, false)
	if usable {
		t.Fatal("an unreadable root must not be usable")
	}
	if present {
		t.Fatal("an unreadable root proves nothing about presence; it must not read present")
	}
}

// TestUnadoptedMediumNeverAuthorizesDelete pins that none of the presence work
// widens delete permission: the delete pass has its own gate, and a medium with no
// readable marker never authorizes deletions regardless of what the pull phase
// concluded about presence.
func TestUnadoptedMediumNeverAuthorizesDelete(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	// Present and drained — the case that reads "safe" — but never adopted.
	h.dev.AddDir(udisk, "ASIAIR", h.nowVal)
	share := h.mountShare(udisk)

	if h.w.mediaConfirmedForDelete(share) {
		t.Fatal("a medium with no marker must never authorize deletes")
	}
}
