package rig

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
)

// The live transfer feed (design features §4) surfaces two
// in-memory signals the backend otherwise keeps to itself: the per-cycle set of
// pull-candidates that reached no terminal outcome (RigStatus.Pending, gated by
// PendingListed), and the full frame carried on the frame.done/frame.failed/
// frame.deleted events. These tests pin the "no terminal outcome this cycle" rule
// that keeps a real in-flight frame from being wrongly dropped, the authority of
// PendingListed, and the enriched event payloads.

// recordingHub wraps the real StatusHub to capture every published Current so a
// test can assert the transferring file's identity (share + source path) survives
// each mid-copy reconstruction rather than being blanked after the first sample.
type recordingHub struct {
	*hub.StatusHub
	mu       sync.Mutex
	currents []*model.CurrentFile
	cleanups []*model.CleanupProgress
}

func (r *recordingHub) PublishRig(rs model.RigStatus) {
	r.mu.Lock()
	r.currents = append(r.currents, rs.Current)
	r.cleanups = append(r.cleanups, rs.Cleanup)
	r.mu.Unlock()
	r.StatusHub.PublishRig(rs)
}

func (r *recordingHub) captured() []*model.CurrentFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.CurrentFile, len(r.currents))
	copy(out, r.currents)
	return out
}

// captureEvents subscribes to the hub, runs fn, and returns every discrete event
// emitted during it. Emit pushes synchronously onto the subscriber's buffered
// queue, so after fn returns the events are already drainable non-blocking.
func (h *harness) captureEvents(fn func()) []hub.Event {
	_, _, events, cancel := h.hub.Subscribe()
	defer cancel()
	fn()
	var out []hub.Event
	for {
		select {
		case e := <-events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func findEvent(evts []hub.Event, name string) *hub.Event {
	for i := range evts {
		if evts[i].Name == name {
			return &evts[i]
		}
	}
	return nil
}

// --- pending set: the "no terminal outcome this cycle" rule --------------------

// A frame still within its settle window is a pull-candidate that never reaches a
// terminal outcome, so it belongs in Pending (and is never persisted).
func TestPendingListsSettlingFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.MinFileAge = model.Duration(time.Hour)
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal) // mtime == now → fails min_file_age

	h.cycle()

	st := h.status()
	if !st.PendingListed {
		t.Fatalf("a complete listing must publish PendingListed=true")
	}
	if len(st.Pending) != 1 {
		t.Fatalf("settling frame should be in Pending; got %d entries", len(st.Pending))
	}
	p := st.Pending[0]
	if p.Share != emmc || p.Source != src || p.Size != int64(len(data)) {
		t.Fatalf("pending entry = %+v, want share=%q source=%q size=%d", p, emmc, src, len(data))
	}
	if st.WaitingToSettle != 1 {
		t.Fatalf("waiting_to_settle=%d, want 1", st.WaitingToSettle)
	}
	if _, ok := h.getFrame(src); ok {
		t.Fatalf("a pending frame must never be persisted to the store")
	}
}

// A transfer deferred at the admission/disk-reserve gate returns before any
// terminal event, so its frame stays in Pending across the retry backoff and is
// never d1-removed as a ghost.
func TestPendingListsAdmissionDeferredFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.adm.acqErr = errors.New("nas reserve unavailable")
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute)) // quiescent

	h.cycle()

	st := h.status()
	if len(st.Pending) != 1 || st.Pending[0].Source != src {
		t.Fatalf("admission-deferred frame should stay in Pending; got %+v", st.Pending)
	}
	if _, ok := h.getFrame(src); ok {
		t.Fatalf("a deferred (never-transferred) frame must not be persisted")
	}
}

// A successfully filed frame reached a terminal outcome, so it is subtracted from
// Pending — the row shows its persisted state, not "pending".
func TestPendingExcludesFiledFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	st := h.status()
	if !st.PendingListed {
		t.Fatalf("a complete listing must publish PendingListed=true")
	}
	if len(st.Pending) != 0 {
		t.Fatalf("a filed frame must be excluded from Pending; got %+v", st.Pending)
	}
	if f, ok := h.getFrame(src); !ok || f.State != model.StateVerified {
		t.Fatalf("frame should be filed verified")
	}
}

// A hard failure is terminal (frame.failed), so it too is subtracted from Pending
// — this is the sole guard against d1 later wrongly removing a real frame, so the
// exclusion is asserted directly.
func TestPendingExcludesHardFailedFrame(t *testing.T) {
	h := newHarness(t, emmc, panicReader{}) // header read panics → per-file failure
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, []byte("not-a-fits"), h.nowVal.Add(-10*time.Minute))

	h.cycle()

	f, ok := h.getFrame(src)
	if !ok || f.State != model.StateFailed {
		t.Fatalf("frame should be hard-failed; ok=%v state=%v", ok, f)
	}
	st := h.status()
	if len(st.Pending) != 0 {
		t.Fatalf("a hard-failed frame must be excluded from Pending; got %+v", st.Pending)
	}
}

// --- PendingListed authority ---------------------------------------------------

// A per-share listing failure yields an incomplete set that must not be treated as
// authoritative, or the client would d1-remove rows for the unlisted share.
func TestPendingListedFalseOnPartialListing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.dev.FailListDir(emmc, device.NetworkError("list boom"))

	h.cycle()

	if st := h.status(); st.PendingListed {
		t.Fatalf("a partial (failed) listing must publish PendingListed=false")
	}
}

// Going offline must clear PendingListed: handleOffline runs before the pull phase
// (so the top-of-cycle reset never fires), and republishing the prior cycle's
// authoritative flag would let the client reconcile against a now-stale set.
func TestPendingListedFalseWhenOffline(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.MinFileAge = model.Duration(time.Hour)
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit",
		fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal)

	h.cycle() // a complete listing → PendingListed true, a settling row pending
	if st := h.status(); !st.PendingListed {
		t.Fatalf("precondition: first cycle should be authoritative")
	}

	h.dev.SetReachable(false) // rig drops off the network
	h.cycle()                 // probe fails → handleOffline

	st := h.status()
	if st.State != model.RigStateOffline {
		t.Fatalf("state=%s, want offline", st.State)
	}
	if st.PendingListed {
		t.Fatalf("an offline rig must publish PendingListed=false")
	}
}

// A cycle that lists while the rig is suspended (paused, no run until idle — an
// identity reset runs one) still reports as Paused, and its snapshot must not be
// authoritative for reconciliation — the paused branch of publishListingStatus
// resets the flag set at end-of-listing.
func TestPendingListedFalseWhenPaused(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.MinFileAge = model.Duration(time.Hour)
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit",
		fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal)
	h.w.paused = true

	h.cycle()

	if st := h.status(); st.PendingListed {
		t.Fatalf("a paused rig's snapshot must publish PendingListed=false")
	}
}

// --- Current identity survives every reconstruction ----------------------------

// Every Current published during a transfer must carry the file identity
// (share + source path) so the live feed keeps matching the transferring row on
// its key. Both the start publish (built at transfer open) and the 100% publish
// (rebuilt via CurrentFile.Progressed, the same helper each mid-copy sample uses)
// are exercised here; Progressed itself is unit-tested in the model package.
func TestCurrentCarriesIdentityThroughTransfer(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	rec := &recordingHub{StatusHub: h.hub}
	h.w.hub = rec

	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	var sawStart, sawDone bool
	for _, c := range rec.captured() {
		if c == nil {
			continue
		}
		if c.Source != src || c.Share != emmc {
			t.Fatalf("Current dropped its identity during transfer: %+v", c)
		}
		switch c.Pct {
		case 0:
			sawStart = true
		case 100:
			sawDone = true
		}
	}
	if !sawStart {
		t.Fatalf("no transfer-start Current (pct 0) was published")
	}
	if !sawDone {
		t.Fatalf("no copy-complete Current (pct 100, via Progressed) was published")
	}
}

// --- enriched events -----------------------------------------------------------

func TestFrameDoneEventCarriesFullFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	evts := h.captureEvents(func() { h.cycle() })

	e := findEvent(evts, "frame.done")
	if e == nil {
		t.Fatalf("no frame.done emitted")
	}
	f, ok := e.Data.(model.Frame)
	if !ok {
		t.Fatalf("frame.done Data is %T, want model.Frame", e.Data)
	}
	if f.SourcePath != src || f.State != model.StateVerified || f.SHA256 != sha256hex(data) || f.DestPath == "" {
		t.Fatalf("frame.done carried an incomplete frame: %+v", f)
	}
}

func TestFrameFailedEventCarriesFullFrame(t *testing.T) {
	h := newHarness(t, emmc, panicReader{})
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, []byte("not-a-fits"), h.nowVal.Add(-10*time.Minute))

	evts := h.captureEvents(func() { h.cycle() })

	e := findEvent(evts, "frame.failed")
	if e == nil {
		t.Fatalf("no frame.failed emitted")
	}
	f, ok := e.Data.(model.Frame)
	if !ok {
		t.Fatalf("frame.failed Data is %T, want model.Frame", e.Data)
	}
	if f.SourcePath != src || f.State != model.StateFailed || f.Error == "" {
		t.Fatalf("frame.failed carried an incomplete frame: %+v", f)
	}
}

func TestFrameDeletedEmittedOnMarkDeleted(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false) // files the frame, then ages past the grace period

	evts := h.captureEvents(func() { h.cycle() }) // the delete pass now runs

	if h.deviceHas(f.SourcePath) {
		t.Fatalf("frame should have been deleted from the device")
	}
	e := findEvent(evts, "frame.deleted")
	if e == nil {
		t.Fatalf("markDeleted must emit frame.deleted")
	}
	fr, ok := e.Data.(model.Frame)
	if !ok || fr.State != model.StateDeleted || fr.SourcePath != f.SourcePath {
		t.Fatalf("frame.deleted carried the wrong payload: %T %+v", e.Data, e.Data)
	}
}

// holdFrame keeps a delete-time integrity mismatch (StateHeld); it is not a
// deletion, so it must emit no frame.deleted.
func TestHoldFrameEmitsNoDeletedEvent(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.lfs.CorruptAfterWrite = func(_, honest string) string { return honestZeros(honest) }

	evts := h.captureEvents(func() { h.cycle() })

	if e := findEvent(evts, "frame.deleted"); e != nil {
		t.Fatalf("holdFrame must not emit frame.deleted")
	}
	if got, _ := h.getFrame(f.SourcePath); got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
}

// honestZeros returns a same-length all-zero string so the delete-pass NAS re-hash
// mismatches the recorded hash, forcing the three-way check to hold the frame.
func honestZeros(honest string) string {
	b := make([]byte, len(honest))
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
