package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func (h *harness) countAlerts(cat model.Category) int {
	n := 0
	for _, a := range h.notify.Raised() {
		if a.Category == cat {
			n++
		}
	}
	return n
}

// An acknowledged frame waives the settle window and nothing else: a mark one
// minute old is still settling unacked, but authorized once acknowledged.
func TestAcknowledgedFrameSkipsSettleWindow(t *testing.T) {
	const root = `/lib`
	rootOK := map[string]bool{root: true}
	now := int64(1_000_000)
	settle := 24 * time.Hour

	f := model.Frame{DestPath: root + `/a.fit`, MissingSeenAt: now - 60} // 1 min old
	if r := absentDeferReasons(f, rootOK, noIntents, noIntents, settle, now); len(r) != 1 || r[0] != "settling" {
		t.Fatalf("a young unacked mark must defer as settling; got %v", r)
	}
	f.MissingAckedAt = now - 30
	if r := absentDeferReasons(f, rootOK, noIntents, noIntents, settle, now); len(r) != 0 {
		t.Fatalf("an acknowledged frame waives the settle window; got %v", r)
	}
}

// The most important regression: acknowledge waives ONLY the settle window. Every
// other disqualifier survives the acknowledgement.
func TestAcknowledgeDoesNotWaiveOtherReasons(t *testing.T) {
	const root = `/lib`
	now := int64(1_000_000)
	settle := 24 * time.Hour
	acked := model.Frame{DestPath: root + `/a.fit`, MissingSeenAt: now - 60, MissingAckedAt: now - 30}

	if r := absentDeferReasons(acked, map[string]bool{root: false}, noIntents, noIntents, settle, now); !containsStr(r, "root_unconfirmed") {
		t.Fatalf("ack must not waive root_unconfirmed; got %v", r)
	}

	out := acked
	out.DestPath = `/elsewhere/a.fit`
	if r := absentDeferReasons(out, map[string]bool{root: true}, noIntents, noIntents, settle, now); !containsStr(r, "outside_roots") {
		t.Fatalf("ack must not waive outside_roots; got %v", r)
	}

	rp := acked
	rp.RestoreRequestedAt = now
	if r := absentDeferReasons(rp, map[string]bool{root: true}, noIntents, noIntents, settle, now); !containsStr(r, "restore_requested") {
		t.Fatalf("ack must not waive restore_requested; got %v", r)
	}

	inFlight := map[string]bool{acked.DestPath: true}
	if r := absentDeferReasons(acked, map[string]bool{root: true}, inFlight, noIntents, settle, now); !containsStr(r, "refile_in_flight") {
		t.Fatalf("ack must not waive refile_in_flight; got %v", r)
	}
}

// A non-verified frame and a restore-pending frame are both refused with their
// distinct sentinels, never silently succeeding.
func TestAcknowledgeRefusesNonVerifiedAndRestorePending(t *testing.T) {
	ctx := context.Background()

	t.Run("non-verified", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		h.setMissing(t, f.ID, 60)
		got, _ := h.getFrame(f.SourcePath)
		got.State = model.StateHeld
		if err := h.store.UpdateFrame(ctx, got); err != nil {
			t.Fatal(err)
		}
		if err := h.w.serviceAcknowledge(ctx, f.ID); !errors.Is(err, ErrNotAcknowledgeable) {
			t.Fatalf("held frame ack: got %v, want ErrNotAcknowledgeable", err)
		}
	})

	t.Run("restore pending", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		h.setMissing(t, f.ID, 60)
		if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
			t.Fatalf("request restore: ok=%v err=%v", ok, err)
		}
		if err := h.w.serviceAcknowledge(ctx, f.ID); !errors.Is(err, ErrRestorePending) {
			t.Fatalf("restore-pending ack: got %v, want ErrRestorePending", err)
		}
	})
}

// design library-reconciliation I35 (AC6): a culled unclassified frame is per-frame
// acknowledgeable at the authoritative worker layer (the widened proven-archived
// guard), while a held frame stays refused with its sentinel.
func TestServiceAcknowledgeAcceptsUnclassifiedRefusesHeld(t *testing.T) {
	ctx := context.Background()

	t.Run("culled unclassified accepted", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileUnclassifiedAndAge(t, h)
		h.setMissing(t, f.ID, 60) // a young mark: settle would defer a drain, but ack waives it
		if err := h.w.serviceAcknowledge(ctx, f.ID); err != nil {
			t.Fatalf("culled unclassified ack: got %v, want nil", err)
		}
		if got, _ := h.getFrame(f.SourcePath); got.MissingAckedAt == 0 {
			t.Fatal("acknowledge must stamp missing_acked_at on an unclassified frame")
		}
	})

	t.Run("held refused", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f := fileUnclassifiedAndAge(t, h)
		h.setMissing(t, f.ID, 60)
		got, _ := h.getFrame(f.SourcePath)
		got.State = model.StateHeld
		if err := h.store.UpdateFrame(ctx, got); err != nil {
			t.Fatal(err)
		}
		if err := h.w.serviceAcknowledge(ctx, f.ID); !errors.Is(err, ErrNotAcknowledgeable) {
			t.Fatalf("held frame ack: got %v, want ErrNotAcknowledgeable", err)
		}
	})
}

// ackRaceStore models a row changing between the worker's read and the store's
// write: the acknowledge write affects no rows, and a re-read now sees a restore.
type ackRaceStore struct {
	store.Store
}

func (s *ackRaceStore) AcknowledgeMissing(ctx context.Context, rigID string, frameID, at int64) (bool, error) {
	if _, err := s.Store.RequestRestore(ctx, rigID, frameID, at); err != nil {
		return false, err
	}
	return false, nil
}

// A store write reporting changed==false is a race, never reported as success:
// classifyAckRefusal re-reads and surfaces the sentinel that now applies.
func TestAcknowledgeRaceReturnsRefusal(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	h.setMissing(t, f.ID, 60)
	h.w.store = &ackRaceStore{Store: h.store}

	err := h.w.serviceAcknowledge(ctx, f.ID)
	if err == nil {
		t.Fatal("a raced write (changed=false) must not report success")
	}
	if !errors.Is(err, ErrRestorePending) {
		t.Fatalf("classifyAckRefusal should surface the racing restore; got %v", err)
	}
}

// End-to-end: an acknowledged frame is deleted the same day (settle waived), the
// row is resolved out of the scan (manner=culled_acked, dest_path nulled), and a
// LATER library scan does not resurrect it, re-mark it, or raise a false alert.
func TestAcknowledgedCulledFrameDeletedAndDoesNotReappear(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // operator culled the NAS copy
		t.Fatal(err)
	}

	h.cycle() // scan marks it missing; the settle window has NOT elapsed
	if got, _ := h.getFrame(f.SourcePath); got.MissingSeenAt == 0 {
		t.Fatal("the scan should have marked the frame missing")
	}
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a freshly culled, unacknowledged frame must not be deleted yet")
	}

	if err := h.w.serviceAcknowledge(ctx, f.ID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	h.cycle() // the ack waives the settle window, so the delete pass authorizes now

	if h.deviceHas(f.SourcePath) {
		t.Fatal("an acknowledged culled frame's device original must be deleted")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", got.State)
	}
	if got.DestPath != "" {
		t.Fatalf("a resolved culled row must null dest_path; got %q", got.DestPath)
	}
	if got.MissingSeenAt != 0 {
		t.Fatalf("a resolved culled row must clear the missing mark; got %d", got.MissingSeenAt)
	}
	if v := h.counterValue("asiair_commander_frames_deleted_total",
		map[string]string{"rig": "rig-01", "manner": "culled_acked"}); v != 1 {
		t.Fatalf("culled_acked deletes = %v, want 1", v)
	}

	// A later scan must NOT re-derive the mark or re-alert — the whole point of
	// resolving the row out of ScanTargets.
	before := h.countAlerts(model.AlertLibraryReconciled)
	h.nowVal = h.nowVal.Add(7 * time.Hour) // past reconcileInterval
	h.w.scanLibrary(ctx, h.cfg, h.profile)
	if after := h.countAlerts(model.AlertLibraryReconciled); after != before {
		t.Fatalf("a resolved culled row must raise no new library_reconciled alert; before=%d after=%d", before, after)
	}
	if again, _ := h.getFrame(f.SourcePath); again.MissingSeenAt != 0 {
		t.Fatalf("a resolved culled row must not be re-marked missing by a later scan; got %d", again.MissingSeenAt)
	}
}

// A both-sides cull (deleteOne's device Stat NotFound at delete time) records
// source_vanished — NOT culled_acked, even on an acknowledged frame — and keeps the
// row's missing mark and dest_path so the reinserted-card Restore path still applies.
func TestBothSidesCullKeepsMissingMark(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // NAS copy culled
		t.Fatal(err)
	}
	h.setMissing(t, f.ID, 60)
	if err := h.w.serviceAcknowledge(ctx, f.ID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	snap, _ := h.getFrame(f.SourcePath) // an acknowledged, authorized snapshot
	if snap.MissingAckedAt == 0 {
		t.Fatal("precondition: the frame should be acknowledged")
	}

	// The device source vanishes before the delete (a Udisk pulled mid-pass): the
	// final Stat in deviceDeleteFrame returns NotFound → deleted=false.
	h.dev.RemoveFile(emmc, f.SourcePath)
	share := h.mountShare(emmc)
	rootOK := map[string]bool{h.cfg.Roots.Light: true}

	// Pass the recorded hash so handleAbsentNAS skips the (now-impossible) device
	// re-hash and reaches the delete, where the vanished source produces !deleted.
	h.w.handleAbsentNAS(ctx, h.localTarget(), h.cfg, share, *snap, rootOK, nil, nil, 24*time.Hour, deviceDigests{whole: snap.SHA256})

	if v := h.counterValue("asiair_commander_frames_deleted_total",
		map[string]string{"rig": "rig-01", "manner": "source_vanished"}); v != 1 {
		t.Fatalf("source_vanished deletes = %v, want 1", v)
	}
	if v := h.counterValue("asiair_commander_frames_deleted_total",
		map[string]string{"rig": "rig-01", "manner": "culled_acked"}); v != 0 {
		t.Fatalf("a both-sides cull must NOT record culled_acked; got %v", v)
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", got.State)
	}
	if got.DestPath == "" || got.MissingSeenAt == 0 {
		t.Fatalf("a both-sides cull must NOT resolve the row; dest=%q missing=%d", got.DestPath, got.MissingSeenAt)
	}
	// The kept mark means RequestRestore still applies (a reinserted card can restore).
	if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("a kept missing mark must still admit RequestRestore; ok=%v err=%v", ok, err)
	}
}

// failMarkDeletedStore fails the mark-deleted write (the StateDeleted UpdateFrame)
// and counts ResolveCulledFrame calls, to prove the design
// library-reconciliation §10.6 resolve is gated on markDeleted actually
// persisting: nulling dest_path on a row still reading 'verified' would strand
// it deletion-eligible with no destination.
type failMarkDeletedStore struct {
	store.Store
	resolveCalls int
}

func (s *failMarkDeletedStore) UpdateFrame(ctx context.Context, f *model.Frame) error {
	if f.State == model.StateDeleted {
		return fmt.Errorf("injected mark-deleted failure")
	}
	return s.Store.UpdateFrame(ctx, f)
}

func (s *failMarkDeletedStore) ResolveCulledFrame(ctx context.Context, id int64) error {
	s.resolveCalls++
	return s.Store.ResolveCulledFrame(ctx, id)
}

func TestCulledResolveSkippedWhenMarkDeletedFails(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f, _ := fileAndAge(t, h, false)
	h.linkRoot(t, h.cfg.Roots.Light)
	if err := os.Remove(f.DestPath); err != nil { // operator culled the NAS copy
		t.Fatal(err)
	}
	h.cycle() // scan marks it missing
	if err := h.w.serviceAcknowledge(ctx, f.ID); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	// Swap in a store whose mark-deleted write fails, then run the delete pass: the
	// device original is removed, but persisting state='deleted' fails.
	fs := &failMarkDeletedStore{Store: h.store}
	h.w.store = fs
	h.cycle()

	if h.deviceHas(f.SourcePath) {
		t.Fatal("the device original is deleted before the (failing) mark-deleted write")
	}
	if fs.resolveCalls != 0 {
		t.Fatalf("ResolveCulledFrame must be skipped when markDeleted fails; calls=%d", fs.resolveCalls)
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State == model.StateDeleted {
		t.Fatal("the mark-deleted write was injected to fail; state must not be deleted")
	}
	if got.DestPath == "" {
		t.Fatal("resolve was skipped, so dest_path must be intact — never nulled on a 'verified' row")
	}
	if got.MissingSeenAt == 0 {
		t.Fatal("the missing mark must survive a failed markDeleted")
	}
}
