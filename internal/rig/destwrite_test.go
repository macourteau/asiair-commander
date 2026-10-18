package rig

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// errNoSuchDevice is what Windows reported when the disk under the destination's
// drive letter dropped out: free-space queries still answered, every flush of a
// staged frame failed.
var errNoSuchDevice = errors.New("A device which does not exist was specified.")

// TestStagingFlushFaultPausesPullsAndAlerts reproduces a destination that answers
// its free-space query but refuses to flush: the fault must stop the cycle after
// the first frame, raise a rig-scoped alert, put the reason on the card, and
// never let the drain line read "drained" over frames that were never copied.
func TestStagingFlushFaultPausesPullsAndAlerts(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	for _, n := range []string{"1", "2", "3"} {
		h.dev.AddFile(emmc, "Autorun/Light/o/Light_000"+n+".fit", data, h.nowVal.Add(-10*time.Minute))
	}
	syncs := 0
	h.w.syncTemp = func(*os.File) error { syncs++; return errNoSuchDevice }

	h.cycle()

	if syncs != 1 {
		t.Fatalf("a flush fault must stop the cycle after the first frame; got %d flush attempts", syncs)
	}
	var found int
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertDestinationUnavailable {
			found++
			if a.RigID != "rig-01" {
				t.Fatalf("write-fault alert RigID=%q, want rig-scoped so other rigs' free-space checks cannot ack it", a.RigID)
			}
		}
	}
	if found != 1 {
		t.Fatalf("want exactly 1 destination alert, got %d (raised: %v)", found, h.raisedCategories())
	}
	st := h.status()
	if !strings.Contains(st.LastError, errNoSuchDevice.Error()) {
		t.Fatalf("LastError=%q, want the write fault surfaced on the card", st.LastError)
	}
	if st.SafeToPowerOff == model.SafeYes {
		t.Fatal("a rig with uncopied frames must not read safe")
	}
	if st.Drain == "drained" {
		t.Fatal(`drain reads "drained" while three uncopied frames sit on the device`)
	}
	// The loop still walks every frame (it only skips the copies), so the pending
	// set is complete and holds all three.
	if len(st.Pending) != 3 || !st.PendingListed {
		t.Fatalf("pending=%v listed=%v, want all three uncopied frames in a complete set", st.Pending, st.PendingListed)
	}
	if !h.logContains("staging area not writable; pulls paused") {
		t.Fatalf("want the cycle-level write-fault WARN; log:\n%s", h.logBuf.String())
	}
	for _, n := range []string{"1", "2", "3"} {
		if !h.deviceHas("Autorun/Light/o/Light_000" + n + ".fit") {
			t.Fatalf("frame %s left the device though nothing was copied", n)
		}
	}
}

// TestStagingWriteFaultProbesInsteadOfStreaming pins the recovery loop: while the
// fault stands, a failing write probe skips the pull without streaming a frame;
// once the probe passes, frames file again and the rig's alert clears.
func TestStagingWriteFaultProbesInsteadOfStreaming(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	syncs := 0
	h.w.syncTemp = func(*os.File) error { syncs++; return errNoSuchDevice }
	h.cycle()
	if syncs != 1 {
		t.Fatalf("setup: want the first cycle to hit the flush fault once, got %d", syncs)
	}
	// Persist the alert the fake notifier only recorded, so recovery has a row to ack.
	if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
		RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertDestinationUnavailable,
		Message: "standing write fault", CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}

	// Still broken: the probe fails, so no frame is streamed at all.
	probes := 0
	h.lfs.ProbeErr = func(string) error { probes++; return errNoSuchDevice }
	h.nowVal = h.nowVal.Add(time.Minute)
	h.cycle()
	if probes != 1 {
		t.Fatalf("want one write probe per faulted cycle, got %d", probes)
	}
	if syncs != 1 {
		t.Fatalf("a failing probe must skip the pull; a frame was streamed and flushed (%d flushes)", syncs)
	}
	if f, ok := h.getFrame(src); ok && safelyOnNAS(f.State) {
		t.Fatalf("frame state=%s while the destination refuses writes", f.State)
	}
	// A second faulted cycle over an unchanged listing is idle, which is exactly
	// when an uncounted frame would let the rig read safe to power off.
	h.nowVal = h.nowVal.Add(time.Minute)
	h.cycle()
	st := h.status()
	if st.SafeToPowerOff == model.SafeYes {
		t.Fatal("reads safe to power off while an uncopied frame sits on the device behind a write fault")
	}
	if st.Drain == "drained" {
		t.Fatal(`drain reads "drained" while an uncopied frame sits on the device behind a write fault`)
	}
	if len(st.Pending) != 1 || !st.PendingListed {
		t.Fatalf("pending=%v listed=%v, want the uncopied frame in a complete pending set", st.Pending, st.PendingListed)
	}

	// The volume comes back: the probe passes, the frame files, the alert clears.
	h.lfs.ProbeErr = nil
	h.w.syncTemp = (*os.File).Sync
	h.nowVal = h.nowVal.Add(time.Minute)
	h.cycle()
	if f, ok := h.getFrame(src); !ok || f.State != model.StateVerified {
		t.Fatalf("frame not filed after recovery: ok=%v frame=%+v", ok, f)
	}
	if st := h.status(); st.LastError != "" {
		t.Fatalf("LastError=%q after recovery, want cleared", st.LastError)
	}
	if !h.logContains("staging area writable again") {
		t.Fatal("want the recovery logged")
	}
	// The alert clears when the probe passes, not after the backlog has copied.
	logs := h.logBuf.String()
	acked, filed := strings.Index(logs, "resolved cleared alerts"), strings.Index(logs, "rig: filed frame")
	if acked < 0 || filed < 0 || acked > filed {
		t.Fatalf("want the write-fault alert acked before the backlog is copied (ack at %d, first filing at %d)", acked, filed)
	}
	alerts, err := h.store.ListAlerts(ctx, false, 100)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	for _, a := range alerts {
		if a.Category == model.AlertDestinationUnavailable {
			t.Fatalf("rig-scoped write-fault alert still unacked after recovery: %+v", a)
		}
	}
}

func TestDrainLabel(t *testing.T) {
	for _, tc := range []struct {
		resident, pending int
		keep              bool
		want              string
	}{
		{0, 0, false, "drained"},
		{0, 3, false, ""}, // nothing copied yet: not drained
		{2, 3, false, "2 resident"},
		{2, 0, false, "2 resident"},
		{0, 3, true, "0 kept"},
		{4, 0, true, "4 kept"},
	} {
		if got := drainLabel(tc.resident, tc.pending, tc.keep); got != tc.want {
			t.Errorf("drainLabel(%d, %d, %v)=%q, want %q", tc.resident, tc.pending, tc.keep, got, tc.want)
		}
	}
}
