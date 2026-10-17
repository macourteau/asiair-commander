package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// recoverSupersedes runs startup supersede recovery with this harness's
// dependencies, the way the manager does before any worker exists.
func recoverSupersedes(t *testing.T, h *harness) {
	t.Helper()
	if err := RecoverSupersedeIntents(context.Background(), h.store, h.lfs, nil); err != nil {
		t.Fatalf("RecoverSupersedeIntents: %v", err)
	}
}

// openSupersedes reads the rig's open supersede intents.
func openSupersedes(t *testing.T, h *harness) map[string]bool {
	t.Helper()
	m, err := h.store.OpenSupersedePaths(context.Background(), "rig-01")
	if err != nil {
		t.Fatalf("OpenSupersedePaths: %v", err)
	}
	return m
}

// readLocal reads a file the test expects to be there.
func readLocal(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// deviceRevision models what H13 does to a finished frame: the device rewrites it
// in place to inject the WCS it has since solved, growing it by one 2880-byte FITS
// header block. The bytes differ; the exposure does not.
//
// The block is injected INTO the header rather than appended, which is both what
// the device does and what keeps the difference visible to a reader that looks at
// only the first archive_size bytes: an appended block leaves that prefix hashing
// exactly as it did, which would mask the very check the present-copy window is
// about.
func deviceRevision(original []byte) []byte {
	const block = 2880
	at := block
	if len(original) < at {
		at = len(original)
	}
	injected := make([]byte, block)
	copy(injected, []byte("COMMENT device-side WCS, injected after the frame was finished"))
	out := make([]byte, 0, len(original)+block)
	out = append(out, original[:at]...)
	out = append(out, injected...)
	return append(out, original[at:]...)
}

// stageRevision writes a revised copy to a staging temp and returns the arguments a
// caller that has already proven those bytes hands the primitive: the three-way
// data-unit agreement and the FITS-validity gate are the caller's, so a test of the
// claim itself supplies their outcome rather than re-deriving it.
func stageRevision(t *testing.T, h *harness, f model.Frame, body []byte) supersedeArgs {
	t.Helper()
	tmp := filepath.Join(h.cfg.Roots.Staging, "asiair-revision-"+strconv.FormatInt(f.ID, 10)+".tmp")
	writeLocal(t, tmp, body)
	a := supersedeArgs{
		frame:         f,
		srcPath:       tmp,
		sha256:        sha256hex(body),
		size:          int64(len(body)),
		mtime:         h.nowVal.Add(-time.Minute).Unix(),
		archiveSHA256: sha256hex(body),
		archiveSize:   int64(len(body)),
	}
	if f.RetargetSHA256 != "" {
		// The overlay a caller with a retargeted row has already re-applied to body, which
		// the primitive refuses to proceed without: the bytes staged here stand for the
		// re-apply's output, and a caller that had re-applied nothing would be installing
		// the device's original OBJECT card under a record claiming the edit.
		a.retargetSHA256 = a.archiveSHA256
	}
	return a
}

// TestSupersedeClaimsTheNameAndCommits walks the four-step claim end to end: the
// archive copy comes to hold the revised bytes under its own canonical name, the
// copy set aside is gone, the intent is cleared, and the row advances exactly the
// columns the matrix of design frame-lifecycle §11.2 gives the revised row —
// with data_sha256 preserved through a write that describes different bytes,
// because a header edit is what the exposure identity exists to survive.
func TestSupersedeClaimsTheNameAndCommits(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)

	// A recorded exposure identity, as the discriminator's leg 2 would have needed.
	seeded := *f
	seeded.DataSHA256 = "the-exposure-identity"
	if err := h.store.UpsertFrame(ctx, &seeded); err != nil {
		t.Fatalf("seed data_sha256: %v", err)
	}
	before := mustGetFrame(t, h, f.ID)

	body := deviceRevision(readLocal(t, before.DestPath))
	a := stageRevision(t, h, before, body)
	h.nowVal = h.nowVal.Add(time.Hour) // so the grace-clock restart is visible

	if err := h.w.supersedeArchive(ctx, a); err != nil {
		t.Fatalf("supersedeArchive: %v", err)
	}

	if got := hashOnDisk(t, before.DestPath); got != sha256hex(body) {
		t.Fatalf("the canonical name must hold the revised bytes, got %s", got)
	}
	if fileExists(a.srcPath) {
		t.Fatal("step 2 renames the staged temp into place; a copy left behind means it did not")
	}
	if aside := supersedeAsidePath(before.DestPath, before.ArchiveSHA256); fileExists(aside) {
		t.Fatalf("step 4 must unlink the copy set aside: %s", aside)
	}
	if len(openSupersedes(t, h)) != 0 {
		t.Fatal("a completed swap clears its intent")
	}

	got := mustGetFrame(t, h, f.ID)
	if got.SHA256 != a.sha256 || got.Size != a.size || got.Mtime != a.mtime {
		t.Fatalf("the device anchors must advance: sha=%q size=%d mtime=%d", got.SHA256, got.Size, got.Mtime)
	}
	if got.ArchiveSHA256 != a.archiveSHA256 || got.ArchiveSize != a.archiveSize {
		t.Fatalf("the archive anchors must advance: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DataSHA256 != "the-exposure-identity" {
		t.Fatalf("data_sha256 is preserved across a supersede, got %q", got.DataSHA256)
	}
	if got.PulledAt != h.nowVal.Unix() {
		t.Fatalf("pulled_at = %d, want the grace clock restarted at %d", got.PulledAt, h.nowVal.Unix())
	}
	if got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified", got.State)
	}
	if got.RetargetSHA256 != "" || got.RetargetFrom != "" {
		t.Fatalf("a plain supersede must not mint an overlay: sha=%q from=%q", got.RetargetSHA256, got.RetargetFrom)
	}
	if got.DestPath != before.DestPath {
		t.Fatalf("the claim is of the row's OWN name: dest=%q, want %q", got.DestPath, before.DestPath)
	}
}

// TestSupersedeHeldRowLandsVerified pins the one state advance the commit rule
// makes: a supersede that completes has proven exactly what the hold was raised to
// doubt, and DeletionEligible admits only verified or unclassified-with-a-mark, so
// a row left held would never re-enter the delete pass at all.
func TestSupersedeHeldRowLandsVerified(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	if err := h.store.SetState(ctx, f.ID, model.StateHeld); err != nil {
		t.Fatalf("hold the row: %v", err)
	}
	held := mustGetFrame(t, h, f.ID)

	body := deviceRevision(readLocal(t, held.DestPath))
	if err := h.w.supersedeArchive(ctx, stageRevision(t, h, held, body)); err != nil {
		t.Fatalf("supersedeArchive: %v", err)
	}

	if got := mustGetFrame(t, h, f.ID); got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified — a held row that supersedes must re-enter the delete pass", got.State)
	}
}

// TestSupersedeCrashAtEveryStepRecovers (design frame-lifecycle AC13) drives a
// modelled process crash at each of the four steps and proves the three things that
// make the window closed by construction: the bytes exist under some name at every
// instant, the open intent defers the delete pass meanwhile, and startup recovery
// restores the canonical file with the row anchored to exactly what that file holds.
//
// The uniform assertion is the one that matters — after recovery the row's archive
// anchor IS the hash of the file at its dest_path — because that is the equality
// whose failure is a permanent critical delete_mismatch no later pass can clear.
func TestSupersedeCrashAtEveryStepRecovers(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run(fmt.Sprintf("crash after step %d", step), func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			f := fileLight(t, h, refileSrc)
			original := readLocal(t, f.DestPath)
			body := deviceRevision(original)
			a := stageRevision(t, h, *f, body)
			aside := supersedeAsidePath(f.DestPath, f.ArchiveSHA256)
			h.nowVal = h.nowVal.Add(time.Hour)

			h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == step }
			if err := h.w.supersedeArchive(ctx, a); !errors.Is(err, errSupersedeCrashed) {
				t.Fatalf("supersedeArchive = %v, want the modelled crash", err)
			}
			h.w.crashAfterSupersedeStep = nil

			// The bytes are under some name in every window.
			switch step {
			case 1:
				if fileExists(f.DestPath) || hashOnDisk(t, aside) != sha256hex(original) {
					t.Fatal("after step 1 the copy is under its reserved name and the canonical name is free")
				}
			case 2, 3:
				if hashOnDisk(t, f.DestPath) != sha256hex(body) || hashOnDisk(t, aside) != sha256hex(original) {
					t.Fatalf("after step %d both copies exist, revised at the canonical name", step)
				}
			case 4:
				if hashOnDisk(t, f.DestPath) != sha256hex(body) || fileExists(aside) {
					t.Fatal("after step 4 only the revised copy remains")
				}
			}

			// The intent is open across every window, and it is what the delete pass
			// defers on — by construction, not by alerting afterwards.
			if !openSupersedes(t, h)[f.DestPath] {
				t.Fatalf("the intent must stay open across the crash; got %v", openSupersedes(t, h))
			}
			row := mustGetFrame(t, h, f.ID)
			row.MissingSeenAt = h.nowVal.Add(-72 * time.Hour).Unix() // long past any settle window
			reasons := absentDeferReasons(row, map[string]bool{h.cfg.Roots.Light: true},
				noIntents, openSupersedes(t, h), 0, h.nowVal.Unix())
			if len(reasons) != 1 || reasons[0] != "supersede_in_flight" {
				t.Fatalf("reasons=%v, want supersede_in_flight alone", reasons)
			}

			recoverSupersedes(t, h)

			got := mustGetFrame(t, h, f.ID)
			if !fileExists(got.DestPath) {
				t.Fatalf("recovery must restore the canonical file: %s", got.DestPath)
			}
			if fileExists(aside) {
				t.Fatalf("recovery must leave no copy under the reserved name: %s", aside)
			}
			if len(openSupersedes(t, h)) != 0 {
				t.Fatal("a resolved intent must be dropped")
			}
			if onDisk := hashOnDisk(t, got.DestPath); onDisk != got.ArchiveSHA256 {
				t.Fatalf("the row's archive anchor (%s) must be what its file holds (%s)", got.ArchiveSHA256, onDisk)
			}
			size, _, err := h.lfs.Stat(got.DestPath)
			if err != nil || size != got.ArchiveSize {
				t.Fatalf("archive_size = %d, want the file's own length %d (err %v)", got.ArchiveSize, size, err)
			}
			// Step 1 is the roll-back arm: the swap never installed anything, so the
			// row and its file are exactly as they were. Every later step completed.
			wantHash, wantState := sha256hex(body), model.StateVerified
			if step == 1 {
				wantHash = sha256hex(original)
			}
			if hashOnDisk(t, got.DestPath) != wantHash {
				t.Fatalf("after a crash at step %d the canonical file holds the wrong bytes", step)
			}
			if got.State != wantState {
				t.Fatalf("state=%s, want %s", got.State, wantState)
			}
			if step > 1 && (got.SHA256 != a.sha256 || got.Mtime != a.mtime || got.PulledAt != h.nowVal.Unix()) {
				t.Fatalf("recovery commits the intent's anchors: sha=%q mtime=%d pulled_at=%d",
					got.SHA256, got.Mtime, got.PulledAt)
			}
			if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
				t.Fatalf("no crash window may cost a critical: %v", h.raisedCategories())
			}
		})
	}
}

// TestSupersedeRecoveryReAnchorsARetargetedFrame (design frame-lifecycle AC13) is
// what asserts the intent's payload rule: recovery runs before any worker or
// device session exists, so every column the commit advances has to travel WITH
// the intent. A retargeted frame is the case that proves it — its overlay is an
// audit record of what the archive file holds, and a recovery that could not
// re-anchor it would leave the record describing bytes the file no longer has,
// which is why an audit record never serves as an archive-side fallback.
func TestSupersedeRecoveryReAnchorsARetargetedFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	f := fileLight(t, h, refileSrc)
	if res, err := h.w.serviceRetarget(ctx, f.ID, "M31"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q err=%v", res.Status, err)
	}
	edited := mustGetFrame(t, h, f.ID)
	if edited.RetargetSHA256 == "" {
		t.Fatal("setup: the row must carry an overlay")
	}

	// The re-apply's outcome: the edited archive copy rewritten from the revised
	// device bytes, so the two sides carry different hashes and different lengths.
	archiveBody := deviceRevision(readLocal(t, edited.DestPath))
	// The device copy is a different length from the edited archive copy, so neither
	// side's assertion below can pass by reading the other side's value.
	deviceBody := archiveBody[:len(archiveBody)-1]
	a := stageRevision(t, h, edited, archiveBody)
	a.sha256, a.size = sha256hex(deviceBody), int64(len(deviceBody))

	// Crash before the commit, so the commit is recovery's alone.
	h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == 2 }
	if err := h.w.supersedeArchive(ctx, a); !errors.Is(err, errSupersedeCrashed) {
		t.Fatalf("supersedeArchive = %v, want the modelled crash", err)
	}
	h.w.crashAfterSupersedeStep = nil

	recoverSupersedes(t, h)

	got := mustGetFrame(t, h, f.ID)
	if got.RetargetSHA256 == edited.RetargetSHA256 {
		t.Fatal("the audit record must not survive a rewrite of the file it describes")
	}
	if got.RetargetSHA256 != hashOnDisk(t, got.DestPath) {
		t.Fatalf("retarget_sha256 = %q, want the committed file's hash %q",
			got.RetargetSHA256, hashOnDisk(t, got.DestPath))
	}
	if got.ArchiveSHA256 != sha256hex(archiveBody) || got.ArchiveSize != int64(len(archiveBody)) {
		t.Fatalf("the archive anchors must describe the edited copy: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.SHA256 != sha256hex(deviceBody) || got.Size != int64(len(deviceBody)) {
		t.Fatalf("the device anchors must describe the DEVICE copy: sha=%q size=%d", got.SHA256, got.Size)
	}
	if got.RetargetFrom != edited.RetargetFrom {
		t.Fatalf("retarget_from records what the frame was labelled before, got %q", got.RetargetFrom)
	}
}

// TestSupersedeMovesTheArchiveSideOfEverySharer pins the sharer row of the matrix.
// Two rows on one archive file is H12, the USB→eMMC failover, and committing the
// revised row alone would leave the sharer anchored to pre-revision bytes in a file
// that holds revised ones — a permanent critical delete_mismatch no pull-path route
// can clear. Its device side stays untouched for the converse reason: the swap
// never read its device copy.
func TestSupersedeMovesTheArchiveSideOfEverySharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	first := fileLight(t, h, refileSrc)

	// The same frame on a second share: one file, two rows (the idempotent keep).
	h.dev.AddShare(tf)
	h.profile.Sources = append(h.profile.Sources, model.Source{Share: tf, Roots: []string{"Autorun"}})
	h.dev.AddFile(tf, refileSrc, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.cycle()
	rows, err := h.store.FramesByDestPath(ctx, "rig-01", first.DestPath)
	if err != nil || len(rows) != 2 {
		t.Fatalf("setup: rows=%d err=%v", len(rows), err)
	}
	var sharer model.Frame
	for _, r := range rows {
		if r.ID != first.ID {
			sharer = r
		}
	}

	revised := mustGetFrame(t, h, first.ID)
	body := deviceRevision(readLocal(t, revised.DestPath))
	a := stageRevision(t, h, revised, body)
	h.nowVal = h.nowVal.Add(time.Hour)
	if err := h.w.supersedeArchive(ctx, a); err != nil {
		t.Fatalf("supersedeArchive: %v", err)
	}

	got := mustGetFrame(t, h, sharer.ID)
	if got.ArchiveSHA256 != a.archiveSHA256 || got.ArchiveSize != a.archiveSize {
		t.Fatalf("a sharer's archive anchors move with the file: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.SHA256 != sharer.SHA256 || got.Size != sharer.Size || got.Mtime != sharer.Mtime {
		t.Fatalf("a sharer's device anchors describe its OWN untouched copy: sha=%q size=%d mtime=%d",
			got.SHA256, got.Size, got.Mtime)
	}
	if got.State != sharer.State || got.PulledAt != sharer.PulledAt {
		t.Fatalf("a sharer's lifecycle is not the supersede's to advance: state=%s pulled_at=%d", got.State, got.PulledAt)
	}
}

// TestSupersedeRefusesACrossRigSharer pins the guard. FramesByDestPath is rig-scoped
// by construction, so a row on another rig's ledger is out of reach of the commit
// and would be left anchored to pre-revision bytes in a file that holds revised
// ones. The repo already meets that hazard by refusing rather than reaching, and the
// refusal has to come before anything moves: a frame that takes the ordinary path
// must find its archive copy exactly where it left it.
func TestSupersedeRefusesACrossRigSharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)

	other := &model.Frame{
		RigID: "rig-02", SourceShare: emmc, SourcePath: "Autorun/Light/other_0001.fit",
		SHA256: "another-rigs-device-copy", Size: f.Size, Mtime: f.Mtime,
		ArchiveSHA256: f.ArchiveSHA256, ArchiveSize: f.ArchiveSize,
		State: model.StateVerified, DestPath: f.DestPath, PulledAt: f.PulledAt,
	}
	if err := h.store.UpsertFrame(ctx, other); err != nil {
		t.Fatalf("seed the other rig's row: %v", err)
	}
	original := readLocal(t, f.DestPath)
	a := stageRevision(t, h, *f, deviceRevision(original))

	err := h.w.supersedeArchive(ctx, a)
	if !errors.Is(err, errSupersedeRefused) {
		t.Fatalf("supersedeArchive = %v, want a refusal", err)
	}
	if hashOnDisk(t, f.DestPath) != sha256hex(original) {
		t.Fatal("a refused supersede must leave the archive copy exactly as it was")
	}
	if !fileExists(a.srcPath) {
		t.Fatal("a refused supersede consumes nothing; the caller still owns its temp")
	}
	if len(openSupersedes(t, h)) != 0 {
		t.Fatal("a refusal happens before the intent is written")
	}
	if got := mustGetFrame(t, h, f.ID); got.ArchiveSHA256 != f.ArchiveSHA256 || got.SHA256 != f.SHA256 {
		t.Fatalf("nothing may be written on this rig's ledger: %+v", got)
	}
	got, ok, err := h.store.GetFrame(ctx, "rig-02", other.SourceShare, other.SourcePath)
	if err != nil || !ok {
		t.Fatalf("GetFrame(rig-02): ok=%v err=%v", ok, err)
	}
	if got.ArchiveSHA256 != other.ArchiveSHA256 || got.SHA256 != other.SHA256 {
		t.Fatalf("nothing may be written on the other rig's ledger either: %+v", got)
	}
}

// TestSupersedeInFlightNeverReleasesTheDeviceOriginal reaches the deferral through
// execution rather than argument, as the re-file case does: the intent it defers on
// is the one an interrupted swap left behind, the file really is absent from the
// row's path, and the ordinary machinery — scan marks missing, settle window
// elapses, delete pass runs — is what would otherwise release a device original
// against an archive copy that is mid-swap (INV-M2).
func TestSupersedeInFlightNeverReleasesTheDeviceOriginal(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	h.linkRoot(t, h.cfg.Roots.Light)

	body := deviceRevision(readLocal(t, f.DestPath))
	h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == 1 }
	if err := h.w.supersedeArchive(ctx, stageRevision(t, h, *f, body)); !errors.Is(err, errSupersedeCrashed) {
		t.Fatalf("supersedeArchive = %v, want the modelled crash", err)
	}
	h.w.crashAfterSupersedeStep = nil
	if fileExists(f.DestPath) {
		t.Fatal("setup: the canonical path must be free while the claim is in flight")
	}

	// The scan finds nothing at the row's path and marks it missing, which is what
	// supersede_in_flight is not there to prevent: what it promises is that the mark
	// never authorizes a delete.
	h.nowVal = h.nowVal.Add(7 * time.Hour)
	h.cycle()
	if marked, _ := h.getFrame(f.SourcePath); marked.MissingSeenAt == 0 {
		t.Fatal("setup: the scan marks a frame whose file is mid-swap missing")
	}

	// A settle window later the real delete pass runs.
	h.nowVal = h.nowVal.Add(25 * time.Hour)
	h.cycle()

	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a frame whose archive copy is mid-swap must never have its device original deleted (INV-M2)")
	}
	settled, _ := h.getFrame(f.SourcePath)
	reasons := absentDeferReasons(*settled, map[string]bool{h.cfg.Roots.Light: true},
		noIntents, openSupersedes(t, h), 24*time.Hour, h.nowVal.Unix())
	if len(reasons) != 1 || reasons[0] != "supersede_in_flight" {
		t.Fatalf("reasons=%v, want supersede_in_flight alone — nothing else may be doing this work", reasons)
	}
	if !h.logContains("reasons=supersede_in_flight") {
		t.Fatal("the delete pass must name supersede_in_flight as the reason it deferred")
	}
	if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("an interrupted swap may not cost a critical: %v", h.raisedCategories())
	}

	// And recovery releases it: the deferral was the intent's doing, not some
	// incidental disqualifier's.
	recoverSupersedes(t, h)
	if !fileExists(f.DestPath) {
		t.Fatalf("recovery must put the archive copy back at %s", f.DestPath)
	}
	if len(openSupersedes(t, h)) != 0 {
		t.Fatal("a resolved intent must be dropped")
	}
}

// TestSupersedeAsideNameIsDerivedFromTheCopySetAside pins what makes recovery able
// to read a swap's progress off the disk: the reserved sibling is named from the
// content hash of the copy it holds, so nothing else in the library can be sitting
// under it and two swaps in one directory can never collide. It carries no FITS
// extension, a representative-frame scan keying on those.
func TestSupersedeAsideNameIsDerivedFromTheCopySetAside(t *testing.T) {
	const dest = "/lib/M31/FRA300/NIGHT_1/Light_0001.fit"
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	aside := supersedeAsidePath(dest, hash)

	if filepath.Dir(aside) != filepath.Dir(dest) {
		t.Fatalf("the reserved name must be a SIBLING (a rename cannot span volumes): %s", aside)
	}
	if got := filepath.Ext(aside); got == ".fit" || got == ".fits" || got == ".fts" {
		t.Fatalf("the reserved name must carry no frame extension, got %q", got)
	}
	if supersedeAsidePath(dest, "ffffffffffffffffffffffffffffffff") == aside {
		t.Fatal("two different copies must reserve two different names")
	}
	if supersedeAsidePath(dest, hash) != aside {
		t.Fatal("the reserved name must be derivable again from the row alone")
	}
}

// h12Sharers files one frame twice — once per share, one archive file, two rows —
// and returns the row whose device copy the swap will read and the row sharing its
// archive copy. That is H12, the USB→eMMC failover, reunited by the idempotent keep.
func h12Sharers(t *testing.T, h *harness) (revised, sharer model.Frame) {
	t.Helper()
	first := fileLight(t, h, refileSrc)
	h.dev.AddShare(tf)
	h.profile.Sources = append(h.profile.Sources, model.Source{Share: tf, Roots: []string{"Autorun"}})
	h.dev.AddFile(tf, refileSrc, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.cycle()
	rows, err := h.store.FramesByDestPath(context.Background(), "rig-01", first.DestPath)
	if err != nil || len(rows) != 2 {
		t.Fatalf("setup: rows=%d err=%v", len(rows), err)
	}
	for _, r := range rows {
		if r.ID == first.ID {
			revised = r
		} else {
			sharer = r
		}
	}
	return revised, sharer
}

// TestSupersedeInFlightDefersASharerWhoseCopyIsPresent (design frame-lifecycle AC13)
// is the second of the two windows, and the one absentDeferReasons cannot reach.
// Between steps 2 and 3 the canonical name is PRESENT and holds bytes no row is
// anchored to yet — a shape a re-file never produces, a re-file always vacating the
// path it moves from. A row sharing that dest_path has its own untouched device copy,
// so the pre-delete gate passes it, and the delete pass reaches it through the
// ordinary present-copy branch: without a guard there it fails the three-way check
// and is held with a critical delete_mismatch, on an archive copy that is about to be
// perfectly good.
func TestSupersedeInFlightDefersASharerWhoseCopyIsPresent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	revised, sharer := h12Sharers(t, h)

	body := deviceRevision(readLocal(t, revised.DestPath))
	h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == 2 }
	if err := h.w.supersedeArchive(ctx, stageRevision(t, h, revised, body)); !errors.Is(err, errSupersedeCrashed) {
		t.Fatalf("supersedeArchive = %v, want the modelled crash", err)
	}
	h.w.crashAfterSupersedeStep = nil
	if hashOnDisk(t, revised.DestPath) != sha256hex(body) {
		t.Fatal("setup: the canonical name must hold the post-swap bytes")
	}
	if got := mustGetFrame(t, h, sharer.ID); got.ArchiveSHA256 != sharer.ArchiveSHA256 {
		t.Fatal("setup: no row may be anchored to those bytes yet")
	}

	// A real delete pass, with both originals past grace.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.cycle()

	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("a sharer whose archive copy is mid-swap must be deferred, not held: %v", h.raisedCategories())
	}
	if got := mustGetFrame(t, h, sharer.ID); got.State != model.StateVerified {
		t.Fatalf("the sharer's state=%s, want the row left alone by the deferral", got.State)
	}
	if got := mustGetFrame(t, h, revised.ID); got.State != model.StateVerified {
		t.Fatalf("the revised row's state=%s, want the row left alone by the deferral", got.State)
	}
	if !h.deviceHas(revised.SourcePath) {
		t.Fatal("a frame whose archive copy is mid-swap must never have its device original deleted (INV-M2)")
	}
	if !h.logContains("archive copy is mid-swap") {
		t.Fatal("the delete pass must say why it deferred")
	}

	// And the guard releases once recovery resolves the swap: the deferral was the
	// intent's doing, not some incidental disqualifier's.
	recoverSupersedes(t, h)
	after := mustGetFrame(t, h, sharer.ID)
	if after.ArchiveSHA256 != sha256hex(body) {
		t.Fatalf("the sharer must end anchored to the bytes its file holds, got %q", after.ArchiveSHA256)
	}
}

// supersedeFailStore forces CommitSupersede to fail on demand, which is the only way
// to reach the commit-failure rollback in a living process: the revised copy is at
// the canonical name, no row is anchored to it, and the swap must put the old copy
// back.
type supersedeFailStore struct {
	store.Store
	fail error
}

func (s *supersedeFailStore) CommitSupersede(ctx context.Context, in store.SupersedeIntent) (bool, int, error) {
	if s.fail != nil {
		return false, 0, s.fail
	}
	return s.Store.CommitSupersede(ctx, in)
}

// TestSupersedeRollsBackAFailedSwap covers both entries into the roll-back: a step 2
// that cannot claim the freed name, and a commit that fails without a crash — the
// one a restart would not resolve for hours, the file holding bytes no row names.
// Either way the archive copy the rows describe ends back under its own name, the
// row is untouched and the frame is simply not superseded this cycle.
func TestSupersedeRollsBackAFailedSwap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken func(h *harness, a supersedeArgs) func()
	}{
		{
			// Step 2's rename fails: the old copy is aside and nothing has claimed the
			// canonical name.
			name: "the revised file cannot claim the name",
			broken: func(h *harness, a supersedeArgs) func() {
				// Only the rename that INSTALLS the revised copy fails. The roll-back's
				// own rename shares its destination and must be let through, or the test
				// would be pinning a double failure rather than the roll-back.
				h.lfs.RenameErr = func(oldpath, _ string) error {
					if oldpath == a.srcPath {
						return errors.New("injected: the destination volume went away")
					}
					return nil
				}
				return func() { h.lfs.RenameErr = nil }
			},
		},
		{
			// The commit fails after step 2: the canonical name holds the revised bytes,
			// which are a copy of a device original still on the device, so removing them
			// costs nothing and the aside copy is the archive's content throughout.
			name: "the commit fails",
			broken: func(h *harness, _ supersedeArgs) func() {
				fs := &supersedeFailStore{Store: h.store, fail: errors.New("injected: database is locked")}
				h.w.store = fs
				return func() { h.w.store = h.store }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			f := fileLight(t, h, refileSrc)
			original := readLocal(t, f.DestPath)
			aside := supersedeAsidePath(f.DestPath, f.ArchiveSHA256)
			a := stageRevision(t, h, *f, deviceRevision(original))

			restore := tc.broken(h, a)
			err := h.w.supersedeArchive(ctx, a)
			restore()
			if err == nil {
				t.Fatal("supersedeArchive must report a swap it could not complete")
			}

			if hashOnDisk(t, f.DestPath) != sha256hex(original) {
				t.Fatal("the archive copy the rows describe must be back under its own name")
			}
			if fileExists(aside) {
				t.Fatalf("the reserved name must be free again: %s", aside)
			}
			if len(openSupersedes(t, h)) != 0 {
				t.Fatal("a completed roll-back clears its intent; nothing is left to defer")
			}
			got := mustGetFrame(t, h, f.ID)
			if got.ArchiveSHA256 != f.ArchiveSHA256 || got.ArchiveSize != f.ArchiveSize ||
				got.SHA256 != f.SHA256 || got.Size != f.Size || got.Mtime != f.Mtime || got.PulledAt != f.PulledAt {
				t.Fatalf("a rolled-back swap writes nothing: %+v", got)
			}
			if h.hasAlert(model.AlertFrameLost) || h.hasAlert(model.AlertDeleteMismatch) {
				t.Fatalf("a swap that put everything back costs no critical: %v", h.raisedCategories())
			}
		})
	}
}

// TestSupersedeRecoveryRefusesAnUnexpectedCanonicalFile pins the one place recovery
// could write anchors against bytes that are not there. Nothing of ours writes that
// path while recovery runs, so a canonical file whose length is not the one the
// intent recorded was put there by something outside the application — and
// committing against it would anchor every sharing row to the wrong bytes. Recovery
// concludes nothing and leaves the intent open, which keeps the frame out of the
// delete pass until someone resolves it.
func TestSupersedeRecoveryRefusesAnUnexpectedCanonicalFile(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := fileLight(t, h, refileSrc)
	aside := supersedeAsidePath(f.DestPath, f.ArchiveSHA256)

	h.w.crashAfterSupersedeStep = func(s int, _ string) bool { return s == 2 }
	if err := h.w.supersedeArchive(ctx, stageRevision(t, h, *f, deviceRevision(readLocal(t, f.DestPath)))); !errors.Is(err, errSupersedeCrashed) {
		t.Fatalf("supersedeArchive = %v, want the modelled crash", err)
	}
	h.w.crashAfterSupersedeStep = nil

	// Something outside the application replaces the canonical file while the process
	// is down: a different length is what recovery can see without spending a read.
	writeLocal(t, f.DestPath, []byte("not the file step 2 installed"))

	recoverSupersedes(t, h)

	if !openSupersedes(t, h)[f.DestPath] {
		t.Fatal("recovery must leave an intent it could not resolve OPEN, so the frame stays deferred")
	}
	if !fileExists(aside) {
		t.Fatal("recovery must not unlink the copy set aside on the strength of a file it cannot vouch for")
	}
	got := mustGetFrame(t, h, f.ID)
	if got.ArchiveSHA256 != f.ArchiveSHA256 || got.ArchiveSize != f.ArchiveSize {
		t.Fatalf("no anchor may be committed against bytes recovery cannot vouch for: sha=%q size=%d",
			got.ArchiveSHA256, got.ArchiveSize)
	}
}
