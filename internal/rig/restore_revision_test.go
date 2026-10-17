package rig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// requestRestore puts an operator's standing restore request on a row the caller
// has already marked missing, which is the only state RequestRestore accepts.
func requestRestore(t *testing.T, h *harness, id int64) {
	t.Helper()
	ok, err := h.store.RequestRestore(context.Background(), "rig-01", id, h.nowVal.Unix())
	if err != nil || !ok {
		t.Fatalf("request restore %d: ok=%v err=%v", id, ok, err)
	}
}

// restoreOnly runs the restore pass alone, so a test can meet a device copy the
// pull path has not resolved yet — the row still recording what it was filed with
// when the restore streams the revision.
func restoreOnly(t *testing.T, h *harness) {
	t.Helper()
	h.w.restorePhase(context.Background(), h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint("10.0.0.1")), h.dev)
}

// assertRestoreCompleted is the shape every completed restore leaves behind,
// whichever arm produced the bytes: the marks cleared, the frame not held, and no
// integrity alert raised against it.
func assertRestoreCompleted(t *testing.T, h *harness, id int64) model.Frame {
	t.Helper()
	got := mustGetFrame(t, h, id)
	if got.MissingSeenAt != 0 || got.MissingAckedAt != 0 || got.RestoreRequestedAt != 0 {
		t.Fatalf("a completed restore clears all three marks; got missing=%d acked=%d restore=%d",
			got.MissingSeenAt, got.MissingAckedAt, got.RestoreRequestedAt)
	}
	if got.State == model.StateHeld {
		t.Fatalf("a completed restore must not hold the frame: %q", got.Error)
	}
	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("no HoldRestoreMismatch may be raised; got %v", h.raisedCategories())
	}
	return got
}

// TestRestoreOfARevisedCulledFrameWritesTheRevisedBytes (design frame-lifecycle
// AC20) is the discriminator on the restore path: a request standing on a culled
// frame the device has since revised, at a dest_path no other row shares, completes
// rather than holding. The archive expectation still names the content the frame
// was culled with and no device copy holds that any more, so the bytes that go back
// are the revised ones and BOTH sides of the row advance with them.
//
// The restore pass is run alone deliberately. A cycle's pull phase would meet the
// revision first and advance the row's device anchors, which is the next test's
// case; here the row still records the pre-revision copy when the restore streams
// the revised one, which is the only way to reach the discriminator itself.
func TestRestoreOfARevisedCulledFrameWritesTheRevisedBytes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	confirmDeleteIdentity(h)
	culled, revised := cullAndRevise(t, h)
	requestRestore(t, h, culled.ID)

	restoreOnly(t, h)

	if got := hashOnDisk(t, culled.DestPath); got != sha256hex(revised) {
		t.Fatal("the restore must put back the bytes the device now holds")
	}
	got := assertRestoreCompleted(t, h, culled.ID)
	if got.SHA256 != sha256hex(revised) || got.Size != int64(len(revised)) {
		t.Fatalf("the device anchors must advance: sha=%q size=%d", got.SHA256, got.Size)
	}
	if got.Mtime != h.nowVal.Add(-10*time.Minute).Unix() {
		t.Fatalf("mtime = %d, want the revised file's %d", got.Mtime, h.nowVal.Add(-10*time.Minute).Unix())
	}
	// A row left at the pre-revision length fails every later VerifyRead against a
	// truncated prefix, which is why the archive side is asserted at the length too.
	if got.ArchiveSHA256 != sha256hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must describe the restored file: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.DataSHA256 != culled.DataSHA256 {
		t.Fatalf("the exposure identity is what a revision survives: got %q, want %q", got.DataSHA256, culled.DataSHA256)
	}
	// A restore writes an archive copy from device bytes read once, so the grace
	// window restarts before that original becomes deletable.
	if got.PulledAt != h.nowVal.Unix() {
		t.Fatalf("pulled_at = %d, want the restore instant %d", got.PulledAt, h.nowVal.Unix())
	}
	assertArchiveAnchors(t, h, culled.ID, "a restored revision")
}

// TestRestoreOfARevisedRetargetedFrameAdvancesTheOverlay (design frame-lifecycle
// AC20, the overlay half) is the audit record following the file it describes: the
// restore re-applies the OBJECT edit to the REVISED bytes, so retarget_sha256 has
// to name what the archive copy now holds. Left at its pre-revision value it
// describes bytes the file no longer holds, which is why an audit record never
// serves as an archive-side fallback.
func TestRestoreOfARevisedRetargetedFrameAdvancesTheOverlay(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	confirmDeleteIdentity(h)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	filed := pullFile(t, h, src, original, 10*time.Minute)
	labelled := retargetTo(t, h, filed.ID, "M 42")
	if labelled.RetargetSHA256 == "" {
		t.Fatal("setup: the frame must carry an overlay")
	}
	if err := os.Remove(labelled.DestPath); err != nil { // the operator culls the NAS copy
		t.Fatal(err)
	}
	h.setMissing(t, labelled.ID, 0)
	requestRestore(t, h, labelled.ID)
	h.nowVal = h.nowVal.Add(time.Hour)
	h.dev.AddFile(emmc, src, revised, h.nowVal.Add(-10*time.Minute))

	restoreOnly(t, h)

	got := assertRestoreCompleted(t, h, labelled.ID)
	if obj := objectOf(t, h, got.DestPath); obj != "M 42" {
		t.Fatalf("the restored copy must keep the operator's label, got %q", obj)
	}
	onDisk := hashOnDisk(t, got.DestPath)
	if onDisk == sha256hex(revised) {
		t.Fatal("the restored copy is the EDITED revision, not the device's own bytes")
	}
	if got.ArchiveSHA256 != onDisk || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must describe the edited revision: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got.RetargetSHA256 != onDisk {
		t.Fatalf("retarget_sha256 = %q, want the bytes the file holds %q", got.RetargetSHA256, onDisk)
	}
	if got.SHA256 != sha256hex(revised) || got.Size != int64(len(revised)) {
		t.Fatalf("the device anchors describe the DEVICE copy: sha=%q size=%d", got.SHA256, got.Size)
	}
}

// TestStandingRestoreSurvivesTheCulledRevisionWrite is the interaction between the
// culled targeted write and a request already standing. The write advances the
// row's device anchors, so by the time the same cycle's restore pass runs, the
// device copy matches the row and nothing looks revised at all — while the archive
// expectation still names the content the frame was culled with. Against that
// expectation the restore would fail its at-rest verify every hour: a warn alert,
// a request that never gives up, and a device original pinned and undrainable.
func TestStandingRestoreSurvivesTheCulledRevisionWrite(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	confirmDeleteIdentity(h)
	culled, revised := cullAndRevise(t, h)
	requestRestore(t, h, culled.ID)

	h.cycle() // the pull path advances the row's device anchors, then the restore runs

	if got := hashOnDisk(t, culled.DestPath); got != sha256hex(revised) {
		t.Fatal("the restore must complete in the same cycle that advanced the row")
	}
	got := assertRestoreCompleted(t, h, culled.ID)
	if got.ArchiveSHA256 != sha256hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must follow the restored file: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	if h.hasAlert(model.AlertTransferFailed) {
		t.Fatalf("a completed restore raises no transfer_failed; got %v", h.raisedCategories())
	}

	// The original is no longer pinned: with both copies proven the delete pass runs
	// the three-way check and drains it.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.cycle()
	if h.deviceHas(culled.SourcePath) {
		t.Fatal("a restored frame's device original must drain once its grace elapses")
	}
}

// sharedRevisedArchive stages design frame-lifecycle AC24's ledger: two rows
// reunited on one archive file by the idempotent keep, whose shared copy a
// supersede has since replaced with the device's revision of one of them. It
// returns the revised row, the sharer as it stood before the swap, and the revised
// bytes now under the canonical name.
func sharedRevisedArchive(t *testing.T, h *harness) (revisedRow, sharerBefore model.Frame, revised []byte) {
	t.Helper()
	confirmDeleteIdentity(h)
	original, rev := revisionPair(revTarget)
	revisedSrc := "Autorun/Light/C-2025 A6/Light_0001.fit"
	sharerSrc := "Autorun/Light/C-2025 A6 (usb)/Light_0001.fit"

	a := pullFile(t, h, revisedSrc, original, 10*time.Minute)
	b := pullFile(t, h, sharerSrc, original, 10*time.Minute)
	if a.DestPath != b.DestPath {
		t.Fatalf("setup: the two rows must share one archive copy, got %q and %q", a.DestPath, b.DestPath)
	}
	// Short of the 1h grace, so neither original drains before the restore is driven.
	h.nowVal = h.nowVal.Add(10 * time.Minute)
	pullFile(t, h, revisedSrc, rev, 5*time.Minute)
	if got := hashOnDisk(t, a.DestPath); got != sha256hex(rev) {
		t.Fatal("setup: the supersede must have installed the revised bytes")
	}
	return mustGetFrame(t, h, a.ID), mustGetFrame(t, h, b.ID), rev
}

// TestRestoreOfASharerStreamsTheRowThatAnswersTheArchiveExpectation (design
// frame-lifecycle AC24): the restore was requested on the SHARER, whose own device
// copy is the pre-revision one. A restore puts back the bytes the archive
// expectation names, so the copy it streams is the revised row's — the requested
// row's would reinstate content no row is anchored to and leave the other naming a
// file that is not its frame.
func TestRestoreOfASharerStreamsTheRowThatAnswersTheArchiveExpectation(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	revisedRow, sharerBefore, revised := sharedRevisedArchive(t, h)
	dest := sharerBefore.DestPath

	if err := os.Remove(dest); err != nil { // the operator culls the shared copy
		t.Fatal(err)
	}
	// The row whose copy the restore will read is held, so that the read row's
	// lifecycle columns are observably stale going in: the archive anchors of a
	// shared file cannot be, the bytes a restore writes being by construction the
	// ones every sharer's expectation already names.
	held := revisedRow
	held.State = model.StateHeld
	held.Error = "an earlier pass doubted this copy"
	if err := h.store.UpdateFrame(context.Background(), &held); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, sharerBefore.ID, 0)
	requestRestore(t, h, sharerBefore.ID)
	// Past the supersede that set the read row's grace anchor, so "the restore
	// restarted it" is distinguishable from "the swap already had".
	h.nowVal = h.nowVal.Add(5 * time.Minute)

	restoreOnly(t, h)

	if got := hashOnDisk(t, dest); got != sha256hex(revised) {
		t.Fatal("the restore must put back the bytes archive_sha256 names, not the requested row's copy")
	}
	sharer := assertRestoreCompleted(t, h, sharerBefore.ID)
	if sharer.ArchiveSHA256 != sha256hex(revised) || sharer.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the sharer's archive anchors must match the file: sha=%q size=%d", sharer.ArchiveSHA256, sharer.ArchiveSize)
	}
	if sharer.SHA256 != sharerBefore.SHA256 || sharer.Size != sharerBefore.Size || sharer.Mtime != sharerBefore.Mtime {
		t.Fatalf("the un-read row's device anchors must not move: %+v", sharer)
	}
	if sharer.PulledAt != sharerBefore.PulledAt {
		t.Fatalf("the un-read row's grace clock is not this restore's to restart: %d", sharer.PulledAt)
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatal("a restore that streamed two device copies must leave no staging temp behind")
	}
	read := mustGetFrame(t, h, revisedRow.ID)
	if read.ArchiveSHA256 != sha256hex(revised) || read.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the row whose copy was read must also match the file: sha=%q size=%d", read.ArchiveSHA256, read.ArchiveSize)
	}
	if read.PulledAt != h.nowVal.Unix() {
		t.Fatalf("the read row's grace clock restarts on the device read: %d", read.PulledAt)
	}
	// The restore proved exactly what the hold doubted — the device copy read, the
	// archive copy written and verified — and a row left held drains never.
	if read.State != model.StateVerified {
		t.Fatalf("the read row's state = %s, want verified", read.State)
	}
	if sharer.State != sharerBefore.State {
		t.Fatalf("the un-read row's state is not this restore's to advance: %s", sharer.State)
	}

	// The proof that the split is right: both rows drain, each copy compared against
	// the anchors recorded for the copy it is.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.w.deleteAllForTest(context.Background(), h.cfg, h.profile, h.dev, "10.0.0.1")
	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("neither row may be held after a restore; got %v", h.raisedCategories())
	}
	for _, s := range []string{revisedRow.SourcePath, sharerBefore.SourcePath} {
		if h.deviceHas(s) {
			t.Fatalf("%s: the proven original should have drained", s)
		}
	}
}

// TestRestoreIsRefusedWhenNoSharingRowAnswers (design frame-lifecycle AC24, the
// other half): a restore that no sharing row's device copy satisfies is refused
// rather than writing bytes that satisfy no row. The requested row's own copy is
// intact and matches its own anchors — which is exactly the temptation — but the
// file it would write is not the one the other row is anchored to.
func TestRestoreIsRefusedWhenNoSharingRowAnswers(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	revisedRow, sharerBefore, _ := sharedRevisedArchive(t, h)
	dest := sharerBefore.DestPath

	// A genuinely different exposure replaces the revised row's device copy, so no
	// copy on the device answers the archive expectation any more.
	other := fitstest.Build(fitstest.LightCards(revW, revH, revTarget), otherImage(revW, revH))
	h.dev.AddFile(emmc, revisedRow.SourcePath, other, h.nowVal)
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, sharerBefore.ID, 0)
	requestRestore(t, h, sharerBefore.ID)

	restoreOnly(t, h)

	if _, err := os.Stat(dest); err == nil {
		t.Fatalf("an unsatisfiable restore must write nothing at %s", dest)
	}
	if names := dirEntries(t, filepath.Dir(dest)); len(names) != 0 {
		t.Fatalf("nothing may be written for an unsatisfiable restore, found %v", names)
	}
	got := mustGetFrame(t, h, sharerBefore.ID)
	if got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
	if got.MissingSeenAt == 0 || got.RestoreRequestedAt == 0 {
		t.Fatalf("a held restore keeps the mark and the request; got missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
	if !h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("a refused restore holds the frame and alerts; got %v", h.raisedCategories())
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatal("a refused restore must leave no staging temp behind")
	}
	// The sharer's own device anchors describe its own untouched copy throughout: a
	// refusal writes nothing, on either side of either row.
	if got.SHA256 != sharerBefore.SHA256 || got.ArchiveSHA256 != sharerBefore.ArchiveSHA256 {
		t.Fatalf("a refused restore must commit nothing: %+v", got)
	}
}

// TestRestoreOfDivergedBytesIsRefusedForACrossRigSharer is the cross-rig half of
// the invariant on the restore path. Putting back bytes the archive expectation
// does not name rewrites the shared file, so it commits every row naming it — and
// FramesByDestPath is rig-scoped by construction, so a row on a second ledger is
// out of reach of that commit and would be left anchored to bytes the file no
// longer holds: a permanent critical delete_mismatch on a ledger this write never
// touched. The repo already meets that hazard by refusing, and a refused restore is
// an unsatisfiable one, which holds.
//
// The two-ledger state is built through the real filing path, the second rig's
// cycle adopting the file the first already filed at the name it would claim.
func TestRestoreOfDivergedBytesIsRefusedForACrossRigSharer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	w2, dev2 := addSecondRig(t, h)
	original, revised := revisionPair(revTarget)
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"

	a := pullFile(t, h, src, original, 10*time.Minute)
	dev2.AddFile(emmc, src, original, h.nowVal.Add(-10*time.Minute))
	w2.runCycle(ctx)
	b, ok, err := h.store.GetFrame(ctx, "rig-02", emmc, src)
	if err != nil || !ok {
		t.Fatalf("the second rig's frame was not filed: ok=%v err=%v", ok, err)
	}
	if b.DestPath != a.DestPath {
		t.Fatalf("setup: both ledgers must name one archive file, got %q and %q", b.DestPath, a.DestPath)
	}

	// The operator culls the shared copy and asks for it back, and the device has
	// since revised the first rig's original — so the only way to satisfy the request
	// is to write bytes neither ledger is anchored to.
	if err := os.Remove(a.DestPath); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, a.ID, 0)
	requestRestore(t, h, a.ID)
	h.nowVal = h.nowVal.Add(time.Hour)
	h.dev.AddFile(emmc, src, revised, h.nowVal.Add(-10*time.Minute))

	restoreOnly(t, h)

	if names := dirEntries(t, filepath.Dir(a.DestPath)); len(names) != 0 {
		t.Fatalf("a refused restore must write nothing, found %v", names)
	}
	got := mustGetFrame(t, h, a.ID)
	if got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
	if got.MissingSeenAt == 0 || got.RestoreRequestedAt == 0 {
		t.Fatalf("a held restore keeps the mark and the request; got missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
	if got.SHA256 != a.SHA256 || got.ArchiveSHA256 != a.ArchiveSHA256 || got.ArchiveSize != a.ArchiveSize {
		t.Fatalf("a refused restore commits nothing on its own ledger: %+v", got)
	}
	other := mustGetFrame(t, h, b.ID)
	if other.ArchiveSHA256 != b.ArchiveSHA256 || other.ArchiveSize != b.ArchiveSize {
		t.Fatalf("the other rig's row must be left exactly as it was: %+v", other)
	}
}

// TestRestoreIgnoresADeletedSharerAtTheSameDestPath is the H12 pair whose halves
// part company: one session's frames split across the USB stick and the eMMC and
// reunite in one archive file, and the delete pass drains one half's device original
// before the operator asks for the other back. Counted as a sharer, the tombstone
// closes the diverged-adopt arm for the live row and then supplies the absent-copy
// read error that is returned in preference to the hold — a WARN transfer_failed and
// the whole device copy re-streamed every hour, forever, where without it the
// identical restore completes.
//
// A tombstone has no live claim on the file's content, so it is not a sharer here.
func TestRestoreIgnoresADeletedSharerAtTheSameDestPath(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	confirmDeleteIdentity(h)
	culled, revised := cullAndRevise(t, h)
	tombstoneAt(t, h, culled, "Autorun/Light/C-2025 A6 (usb)/Light_0001.fit")
	requestRestore(t, h, culled.ID)

	restoreOnly(t, h)

	if h.hasAlert(model.AlertTransferFailed) {
		t.Fatalf("a tombstone must not turn a satisfiable restore into a failure; got %v", h.raisedCategories())
	}
	if got := hashOnDisk(t, culled.DestPath); got != sha256hex(revised) {
		t.Fatal("the restore must put back the bytes the device now holds")
	}
	got := assertRestoreCompleted(t, h, culled.ID)
	if got.ArchiveSHA256 != sha256hex(revised) || got.ArchiveSize != int64(len(revised)) {
		t.Fatalf("the archive anchors must describe the restored file: sha=%q size=%d", got.ArchiveSHA256, got.ArchiveSize)
	}
	// The tombstone still names the file, so the write that changed it moved its
	// archive-side anchors too — a row excluded from the candidate set is not a row
	// excluded from the commit.
	tomb, ok, err := h.store.GetFrame(ctx, "rig-01", emmc, "Autorun/Light/C-2025 A6 (usb)/Light_0001.fit")
	if err != nil || !ok {
		t.Fatalf("the tombstone must survive: ok=%v err=%v", ok, err)
	}
	if tomb.ArchiveSHA256 != sha256hex(revised) {
		t.Fatalf("the tombstone's archive anchor must follow the file it names, got %q", tomb.ArchiveSHA256)
	}
}

// tombstoneAt adds the drained half of an H12 USB/eMMC pair: a second row of the
// same rig naming one archive file, whose device original the delete pass has
// already removed.
func tombstoneAt(t *testing.T, h *harness, f model.Frame, srcPath string) model.Frame {
	t.Helper()
	row := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: srcPath,
		SHA256: f.SHA256, Size: f.Size, Mtime: f.Mtime, DataSHA256: f.DataSHA256,
		FrameType: f.FrameType, Target: f.Target, DestPath: f.DestPath,
		ArchiveSHA256: f.ArchiveSHA256, ArchiveSize: f.ArchiveSize,
		State: model.StateDeleted, DeletedAt: h.nowVal.Unix(), PulledAt: f.PulledAt,
	}
	if err := h.store.UpsertFrame(context.Background(), row); err != nil {
		t.Fatalf("seed the drained half of the pair: %v", err)
	}
	return *row
}

// commitRestoreFailStore fails the restore's row commit, which is the only way from
// a living process to reach the state the bound below exists for: the bytes renamed
// into place and no row describing them.
type commitRestoreFailStore struct {
	store.Store
	fail error
}

func (s *commitRestoreFailStore) CommitRestore(ctx context.Context, w store.ArchiveWrite) (bool, int, error) {
	if s.fail != nil {
		return false, 0, s.fail
	}
	return s.Store.CommitRestore(ctx, w)
}

// inconclusiveAlerts counts the alerts the restore's inconclusive arm has escalated,
// which the setup's own commit failure must not be mistaken for.
func inconclusiveAlerts(h *harness) int {
	n := 0
	for _, a := range h.notify.Raised() {
		if strings.Contains(a.Message, "neither verifies nor reads as missing") {
			n++
		}
	}
	return n
}

// TestHalfCommittedRestoreEscalatesAfterItsBound drives the one shape that makes
// restoreOne's inconclusive arm permanent. restoreNAS renames the revised bytes into
// place and only then commits, so a failed commit leaves the archive copy at the
// revised length while the row still records the pre-revision ArchiveSize: the next
// pass's VerifyRead(path, size) hashes a prefix of a longer file — not ErrShortFile,
// not a missing file, just a mismatch — and lands on the arm that concludes nothing.
// Quiet forever there, with restore_requested_at keeping the device original out of
// DeletionEligible, is a frame that never drains and an operator never told.
func TestHalfCommittedRestoreEscalatesAfterItsBound(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	confirmDeleteIdentity(h)
	culled, revised := cullAndRevise(t, h)
	requestRestore(t, h, culled.ID)

	// The half-commit: the file lands, the row does not.
	real := h.w.store
	h.w.store = &commitRestoreFailStore{Store: h.store, fail: errors.New("database is locked")}
	restoreOnly(t, h)
	h.w.store = real
	if got := hashOnDisk(t, culled.DestPath); got != sha256hex(revised) {
		t.Fatal("setup: the restored bytes must be in place before the commit failed")
	}
	if got := mustGetFrame(t, h, culled.ID); got.ArchiveSize == int64(len(revised)) {
		t.Fatal("setup: the row must still record the pre-revision length")
	}
	if inconclusiveAlerts(h) != 0 {
		t.Fatal("setup: the commit failure is not the arm under test")
	}

	for pass := 1; pass < maxRestoreInconclusive; pass++ {
		h.nowVal = h.nowVal.Add(restoreRetryInterval + time.Minute)
		restoreOnly(t, h)
		if n := inconclusiveAlerts(h); n != 0 {
			t.Fatalf("pass %d of %d must defer quietly; got %d alerts", pass, maxRestoreInconclusive, n)
		}
		if !h.logContains("restore verify inconclusive; deferring") {
			t.Fatalf("pass %d must record the deferral it made", pass)
		}
	}

	h.nowVal = h.nowVal.Add(restoreRetryInterval + time.Minute)
	restoreOnly(t, h)
	if n := inconclusiveAlerts(h); n != 1 {
		t.Fatalf("the bound must escalate exactly once; got %d alerts from %v", n, h.raisedCategories())
	}
	// The escalation reports, it does not resolve: the operator's request stands and
	// the frame is not held on a read that concluded nothing.
	got := mustGetFrame(t, h, culled.ID)
	if got.RestoreRequestedAt == 0 || got.MissingSeenAt == 0 {
		t.Fatalf("the request stands until a pass can satisfy it; got missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
	if got.State == model.StateHeld {
		t.Fatalf("nothing is concluded from an inconclusive read: %q", got.Error)
	}
}

// crossRigCountFailStore forces the cross-rig sharer count to fail, which is the
// only way to reach the store-read failure inside the diverged arm's refusal from a
// living process.
type crossRigCountFailStore struct {
	store.Store
	fail error
}

func (s *crossRigCountFailStore) CountUndeletedFramesByDestAnyRig(ctx context.Context, destPath string) (int, error) {
	if s.fail != nil {
		return 0, s.fail
	}
	return s.Store.CountUndeletedFramesByDestAnyRig(ctx, destPath)
}

// TestRestoreConcludesNothingFromAFailedCrossRigCount: the read that tells the
// diverged arm whether another rig names this archive file says nothing about the
// frame when it FAILS. Concluding from it would hold a perfectly good frame with a
// critical delete_mismatch whose text claims its device content changed — and
// nothing is concluded from an error (design library-reconciliation §2.1), a rule
// the device-read half of this same path already honours. The restore fails instead:
// one warn, the request stands, and a later pass retries.
func TestRestoreConcludesNothingFromAFailedCrossRigCount(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	confirmDeleteIdentity(h)
	culled, _ := cullAndRevise(t, h)
	requestRestore(t, h, culled.ID)
	h.w.store = &crossRigCountFailStore{Store: h.store, fail: errors.New("database is locked")}

	restoreOnly(t, h)

	got := mustGetFrame(t, h, culled.ID)
	if got.State == model.StateHeld {
		t.Fatalf("a failed store read must not hold the frame: %q", got.Error)
	}
	if h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("no critical integrity alert may be raised from a store read failure; got %v", h.raisedCategories())
	}
	if !h.hasAlert(model.AlertTransferFailed) {
		t.Fatalf("the restore failed, so the operator gets the warn the failure path raises; got %v", h.raisedCategories())
	}
	if got.MissingSeenAt == 0 || got.RestoreRequestedAt == 0 {
		t.Fatalf("the request stands until a pass can satisfy it; got missing=%d restore=%d",
			got.MissingSeenAt, got.RestoreRequestedAt)
	}
	if got.SHA256 != culled.SHA256 || got.ArchiveSHA256 != culled.ArchiveSHA256 {
		t.Fatalf("a restore that decided nothing commits nothing: %+v", got)
	}
}
