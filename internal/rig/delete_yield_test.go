package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// deleteAllForTest runs one delete slice with a deadline that never passes, so it
// behaves like the whole delete pass the pre-slicing tests were written against:
// it runs until its eligible list is exhausted, the rig is suspended, or the
// destination is unavailable. Its result is discarded; the slice tests below call
// deletePhase directly with explicit deadlines.
func (w *Worker) deleteAllForTest(ctx context.Context, cfg *model.Config, profile *model.RigProfile, dev device.Device, addr string) {
	_ = w.deletePhase(ctx, cfg, w.newCycle(profile, netsite.LocalEndpoint(addr)), dev, time.Now().Add(100*365*24*time.Hour))
}

// TestRotateAfter pins the round-robin order a slice walks: the rows after the
// cursor in (pulled_at, id) order, then the rows up to and including it, with the
// wrap index marking where the original head row lands.
func TestRotateAfter(t *testing.T) {
	row := func(pulledAt, id int64) model.Frame { return model.Frame{ID: id, PulledAt: pulledAt} }
	// DeletionEligible's own order: pulled_at, then id. Rows 1 and 2 tie on pulled_at.
	full := []model.Frame{row(10, 1), row(10, 2), row(20, 3), row(30, 4)}
	withoutRow3 := []model.Frame{row(10, 1), row(10, 2), row(30, 4)}

	cases := []struct {
		name     string
		eligible []model.Frame
		cursor   deleteCursor
		wantIDs  []int64
		wantWrap int
	}{
		{"zero cursor leaves the list unchanged", full, deleteCursor{}, []int64{1, 2, 3, 4}, 4},
		{"mid-list cursor resumes after it", full, deleteCursor{pulledAt: 20, id: 3}, []int64{4, 1, 2, 3}, 1},
		{"tie on pulled_at is resolved by id", full, deleteCursor{pulledAt: 10, id: 1}, []int64{2, 3, 4, 1}, 3},
		{"cursor on the tie's second row", full, deleteCursor{pulledAt: 10, id: 2}, []int64{3, 4, 1, 2}, 2},
		{"cursor on the last row wraps to the head", full, deleteCursor{pulledAt: 30, id: 4}, []int64{1, 2, 3, 4}, 0},
		{"cursor past the end wraps to the head", full, deleteCursor{pulledAt: 99, id: 1}, []int64{1, 2, 3, 4}, 0},
		{"cursor at a deleted row resolves to its successor", withoutRow3, deleteCursor{pulledAt: 20, id: 3}, []int64{4, 1, 2}, 1},
		{"cursor between pulled_at groups", full, deleteCursor{pulledAt: 15, id: 99}, []int64{3, 4, 1, 2}, 2},
		{"empty list, zero cursor", nil, deleteCursor{}, nil, 0},
		{"empty list, non-zero cursor", nil, deleteCursor{pulledAt: 10, id: 1}, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := idsOf(tc.eligible)
			got, wrap := rotateAfter(tc.eligible, tc.cursor)
			if ids := idsOf(got); !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("order = %v, want %v", ids, tc.wantIDs)
			}
			if wrap != tc.wantWrap {
				t.Fatalf("wrap = %d, want %d", wrap, tc.wantWrap)
			}
			if len(got) > 0 && wrap < len(got) && got[wrap].ID != tc.eligible[0].ID {
				t.Fatalf("wrap index %d holds row %d, want the original head row %d", wrap, got[wrap].ID, tc.eligible[0].ID)
			}
			if after := idsOf(tc.eligible); !slices.Equal(after, before) {
				t.Fatalf("rotateAfter modified its input: %v, was %v", after, before)
			}
		})
	}
}

func idsOf(frames []model.Frame) []int64 {
	var ids []int64
	for _, f := range frames {
		ids = append(ids, f.ID)
	}
	return ids
}

// TestDeleteOneReportsOutcome pins the two facts deleteOne reports to its slice:
// whether it streamed the device original, and which source it removed. They are
// what the slice's progress floor and the Safe verdict's own-delete discount read,
// so a cheap exit must report neither, and a streamed visit that concludes nothing
// must not claim a removal.
func TestDeleteOneReportsOutcome(t *testing.T) {
	settle := 24 * time.Hour
	rootOf := func(h *harness) map[string]bool { return map[string]bool{h.cfg.Roots.Light: true} }

	t.Run("verified matching frame is streamed and removed", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)

		out := h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, rootOf(h), noIntents, noIntents, settle)

		if want := (deleteOutcome{streamed: true, removed: listingKey(emmc, f.SourcePath)}); out != want {
			t.Fatalf("outcome = %+v, want %+v", out, want)
		}
		if h.deviceHas(f.SourcePath) {
			t.Fatal("a three-way-proven original should have been deleted")
		}
	})

	t.Run("open supersede intent exits before streaming", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		reads := h.dev.ContentReads()

		out := h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, rootOf(h), noIntents,
			map[string]bool{f.DestPath: true}, settle)

		if out != (deleteOutcome{}) {
			t.Fatalf("outcome = %+v, want neither streamed nor removed", out)
		}
		if got := h.dev.ContentReads() - reads; got != 0 {
			t.Fatalf("a cheap exit read the device original %d times, want 0", got)
		}
		if !h.deviceHas(f.SourcePath) {
			t.Fatal("a frame mid-swap must not be deleted")
		}
	})

	t.Run("transient VerifyRead failure is streamed but not removed", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		// A directory at the archive path passes the pre-hash Stat, then fails the
		// read with an error that is neither absence nor truncation: a transient
		// VerifyRead failure, which defers after the device hash.
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.DestPath, 0o755); err != nil {
			t.Fatal(err)
		}

		out := h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, rootOf(h), noIntents, noIntents, settle)

		if want := (deleteOutcome{streamed: true}); out != want {
			t.Fatalf("outcome = %+v, want %+v", out, want)
		}
		if !h.logContains("NAS re-hash transient error") {
			t.Fatalf("expected the transient VerifyRead deferral; log:\n%s", h.logBuf.String())
		}
		if !h.deviceHas(f.SourcePath) {
			t.Fatal("a frame whose NAS copy could not be read must not be deleted")
		}
		if got, _ := h.getFrame(f.SourcePath); got.State != model.StateVerified {
			t.Fatalf("state = %s, want verified (deferred)", got.State)
		}
	})

	t.Run("culled frame is streamed and removed through handleAbsentNAS", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.setMissing(t, f.ID, int64((settle + time.Hour).Seconds())) // culled and settled

		out := h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, rootOf(h), noIntents, noIntents, settle)

		if want := (deleteOutcome{streamed: true, removed: listingKey(emmc, f.SourcePath)}); out != want {
			t.Fatalf("outcome = %+v, want %+v", out, want)
		}
		if h.deviceHas(f.SourcePath) {
			t.Fatal("a settled cull's device original should have been deleted")
		}
	})

	t.Run("NAS copy culled during the device hash keeps streamed and reports the removal", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		h.setMissing(t, f.ID, int64((settle + time.Hour).Seconds()))
		// The copy is present at the pre-hash Stat and gone by VerifyRead: deleteOne's
		// own hash streams, then hands its digests to handleAbsentNAS, which must not
		// stream again and whose removal must still reach the caller.
		h.dev.OnOpenReader = func(share, path string) {
			if share == emmc && path == f.SourcePath {
				_ = os.Remove(f.DestPath)
			}
		}
		reads := h.dev.ContentReads()

		out := h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, rootOf(h), noIntents, noIntents, settle)
		h.dev.OnOpenReader = nil

		if want := (deleteOutcome{streamed: true, removed: listingKey(emmc, f.SourcePath)}); out != want {
			t.Fatalf("outcome = %+v, want %+v", out, want)
		}
		if got := h.dev.ContentReads() - reads; got != 1 {
			t.Fatalf("device original read %d times, want 1 (handleAbsentNAS reuses the hash)", got)
		}
		if h.deviceHas(f.SourcePath) {
			t.Fatal("the culled frame's device original should have been deleted")
		}
	})

	t.Run("source vanished before the delete is not a removal", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		f, _ := fileAndAge(t, h, false)
		// Remove the source from the device after the hash, as an external deletion
		// would: deviceDeleteFrame reports deleted == false with no error.
		h.dev.OnOpenReader = func(share, path string) {
			if share == emmc && path == f.SourcePath {
				h.dev.OnStat = func(share, path string) {
					if share == emmc && path == f.SourcePath {
						h.dev.OnStat = nil
						h.dev.RemoveFile(emmc, f.SourcePath)
					}
				}
			}
		}

		out := h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, rootOf(h), noIntents, noIntents, settle)
		h.dev.OnOpenReader, h.dev.OnStat = nil, nil

		if want := (deleteOutcome{streamed: true}); out != want {
			t.Fatalf("outcome = %+v, want %+v", out, want)
		}
		if h.counterValue("asiair_commander_frames_deleted_total", map[string]string{"manner": "source_vanished"}) != 1 {
			t.Fatal("expected the frame closed out as source_vanished")
		}
		if got, _ := h.getFrame(f.SourcePath); got.State != model.StateDeleted {
			t.Fatalf("state = %s, want deleted (closed out as source_vanished)", got.State)
		}
	})
}

// TestDeletePassCountsUnconfirmedIdentity: a delete session whose identity probe
// gets no reply deletes nothing and is counted as identity_unconfirmed, so the
// throughput lost to missed probes is measurable; a foreign answer is an alert,
// not this quiet counter.
func TestDeletePassCountsUnconfirmedIdentity(t *testing.T) {
	const blocked = "asiair_commander_delete_blocked_total"
	reason := map[string]string{"reason": "identity_unconfirmed"}
	src := "Autorun/Light/Light_0001.fit"

	t.Run("no reply", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "dev-guid"
		h.resolver.probeOK = false
		h.deleteEligibleFrame(t, src)

		h.w.deleteAllForTest(context.Background(), h.cfg, h.profile, h.dev, "10.0.0.1")

		if got := h.counterValue(blocked, reason); got != 1 {
			t.Fatalf("delete_blocked_total{reason=identity_unconfirmed} = %v, want 1", got)
		}
		if !h.deviceHas(src) {
			t.Fatal("an unconfirmed delete session must delete nothing")
		}
	})

	t.Run("foreign", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID, h.resolver.probeOK = "someone-else", true
		h.deleteEligibleFrame(t, src)

		h.w.deleteAllForTest(context.Background(), h.cfg, h.profile, h.dev, "10.0.0.1")

		if got := h.counterValue(blocked, reason); got != 0 {
			t.Fatalf("delete_blocked_total{reason=identity_unconfirmed} = %v on a foreign answer, want 0", got)
		}
		if !h.deviceHas(src) {
			t.Fatal("a foreign delete session must delete nothing")
		}
	})
}

// --- delete slices ------------------------------------------------------------

const sliceAddr = "10.0.0.1"

// slowLinkRead is the link time one device read of a large frame costs over a
// slow Wi-Fi link (89 MB at about 1.3 MB/s).
const slowLinkRead = 70 * time.Second

// seedEligible seeds n three-way provable, grace-elapsed frames on the device and
// the NAS, oldest first, and returns them in DeletionEligible's (pulled_at, id)
// order. Each frame has its own content and paths so a delete is attributable.
func (h *harness) seedEligible(t *testing.T, n int) []model.Frame {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		h.seedEligibleFrame(t, i)
	}
	elig, err := h.store.DeletionEligible(ctx, "rig-01", h.nowVal.Add(-time.Hour).Unix())
	if err != nil || len(elig) != n {
		t.Fatalf("expected %d eligible frames, got %d (err=%v)", n, len(elig), err)
	}
	return elig
}

// seedEligibleFrame seeds the i-th frame seedEligible would, alone, so a test can
// make one more frame eligible mid-drain. Its pulled_at orders it after frames
// 0..i-1, and its mtime is two hours before the harness clock, so grace has
// elapsed.
func (h *harness) seedEligibleFrame(t *testing.T, i int) model.Frame {
	t.Helper()
	mt := h.nowVal.Add(-2 * time.Hour) // pulled long ago → grace elapsed
	content := []byte(fmt.Sprintf("a-proven-original-frame-%02d", i))
	src := fmt.Sprintf("Autorun/Light/Light_%04d.fit", i+1)
	h.dev.AddFile(emmc, src, content, mt)
	nasPath := filepath.Join(h.dir, "light", fmt.Sprintf("Light_%04d.fit", i+1))
	writeLocal(t, nasPath, content)
	f := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: src,
		SHA256: sha256hex(content), Size: int64(len(content)), Mtime: mt.Unix(),
		ArchiveSHA256: sha256hex(content), ArchiveSize: int64(len(content)),
		DestPath: nasPath, State: model.StateVerified, PulledAt: mt.Unix() + int64(i),
	}
	if err := h.store.UpsertFrame(context.Background(), f); err != nil {
		t.Fatalf("upsert frame: %v", err)
	}
	return *f
}

// slowLink makes every device read advance the harness clock by d, as reading a
// whole frame over a slow Wi-Fi link would.
func (h *harness) slowLink(d time.Duration) {
	h.dev.OnOpenReader = func(string, string) { h.nowVal = h.nowVal.Add(d) }
}

// changeSource makes a frame's device original differ from its row, so deleteOne
// takes the cheap source-changed deferral without reading it.
func (h *harness) changeSource(f model.Frame) {
	h.dev.SetMtime(emmc, f.SourcePath, time.Unix(f.Mtime, 0).Add(time.Hour))
}

// failVerifyRead replaces a frame's NAS copy with a directory: it passes the
// pre-hash Stat and then fails VerifyRead transiently, so the visit streams the
// device original and defers.
func failVerifyRead(t *testing.T, f model.Frame) {
	t.Helper()
	if err := os.Remove(f.DestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.DestPath, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) slice(deadline time.Time) slicePass {
	return h.w.deletePhase(context.Background(), h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint(sliceAddr)), h.dev, deadline)
}

func (h *harness) yields() float64 {
	return h.counterValue("asiair_commander_delete_pass_yield_total", map[string]string{"rig": "rig-01"})
}

func (h *harness) deferCount(t *testing.T, f model.Frame) int {
	t.Helper()
	got, ok := h.getFrame(f.SourcePath)
	if !ok {
		t.Fatalf("frame %s not found", f.SourcePath)
	}
	return got.DeleteDeferCount
}

func cursorOf(f model.Frame) deleteCursor { return deleteCursor{pulledAt: f.PulledAt, id: f.ID} }

// statusRecordingHub records the pill, the readout, the run flag and the verdict
// of every status the worker publishes, so a test can see them mid-slice.
type statusRecordingHub struct {
	*hub.StatusHub
	mu   sync.Mutex
	pubs []model.RigStatus
}

func (r *statusRecordingHub) PublishRig(rs model.RigStatus) {
	r.mu.Lock()
	r.pubs = append(r.pubs, model.RigStatus{State: rs.State, Cleanup: rs.Cleanup, Paused: rs.Paused, Draining: rs.Draining,
		RunUntilIdle: rs.RunUntilIdle, SafeToPowerOff: rs.SafeToPowerOff, PowerOffReason: rs.PowerOffReason, SafeAsOf: rs.SafeAsOf})
	r.mu.Unlock()
	r.StatusHub.PublishRig(rs)
}

// take returns what was published since the last take.
func (r *statusRecordingHub) take() []model.RigStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.pubs
	r.pubs = nil
	return out
}

func (h *harness) recordStatuses() *statusRecordingHub {
	rec := &statusRecordingHub{StatusHub: h.hub}
	h.w.hub = rec
	return rec
}

// cleanupsOf returns the non-nil cleanup readouts in publish order.
func cleanupsOf(pubs []model.RigStatus) []model.CleanupProgress {
	var out []model.CleanupProgress
	for _, p := range pubs {
		if p.Cleanup != nil {
			out = append(out, *p.Cleanup)
		}
	}
	return out
}

// TestDeletePassSliceYieldsAtDeadline: on a slow link, a slice whose deadline has
// passed deletes one frame and yields, handing the pill back to the listing's
// state; repeated slices drain the backlog, deleting only proven frames, with a
// readout that counts across the whole drain.
func TestDeletePassSliceYieldsAtDeadline(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	frames := h.seedEligible(t, 5)
	h.slowLink(slowLinkRead)
	rec := h.recordStatuses()
	h.w.listedState = model.RigStatePulling // what the cycle's listing published

	for n := 1; n <= 5; n++ {
		pass := h.slice(h.nowVal.Add(-time.Second)) // the pull phase already overran

		want := slicePass{more: true, advanced: true}
		if n == 5 {
			// The last frame is the whole list: the slice exhausts it rather than yield.
			want = slicePass{advanced: true, sweepDone: true, exhausted: true}
		}
		if pass != want {
			t.Fatalf("slice %d: result %+v, want %+v", n, pass, want)
		}
		for i, f := range frames {
			if gone := !h.deviceHas(f.SourcePath); gone != (i < n) {
				t.Fatalf("slice %d: frame %d deleted=%v, want %v", n, i, gone, i < n)
			}
		}
		wantReadout := []model.CleanupProgress{{File: filepath.Base(frames[n-1].SourcePath), Index: n, Total: 5}}
		if got := cleanupsOf(rec.take()); !slices.Equal(got, wantReadout) {
			t.Fatalf("slice %d: cleanup readout %+v, want %+v", n, got, wantReadout)
		}
		st := h.status()
		if st.State != model.RigStatePulling {
			t.Fatalf("slice %d: state %q after the slice, want the listing's %q", n, st.State, model.RigStatePulling)
		}
		if st.Cleanup != nil {
			t.Fatalf("slice %d: cleanup %+v still published after the slice", n, st.Cleanup)
		}
		if wantYields := float64(min(n, 4)); h.yields() != wantYields {
			t.Fatalf("slice %d: yield counter %v, want %v", n, h.yields(), wantYields)
		}
	}
	if got := h.dev.Mutations(); got != 5 {
		t.Fatalf("device mutations = %d, want exactly the 5 proven originals", got)
	}
	for _, f := range frames {
		if got, _ := h.getFrame(f.SourcePath); got.State != model.StateDeleted {
			t.Fatalf("frame %s state %s, want deleted", f.SourcePath, got.State)
		}
		if !h.w.ownDeletes[listingKey(emmc, f.SourcePath)] {
			t.Fatalf("own deletes miss %s", f.SourcePath)
		}
	}
	if h.w.deleteCursor != (deleteCursor{}) {
		t.Fatalf("cursor %+v after the list was exhausted, want zero", h.w.deleteCursor)
	}
}

// TestDeleteFloorWhenPullOverruns: a slice that starts past its deadline still
// makes progress — one streamed visit, past any cheap exits — and when cheap exits
// alone fill a poll of skipping it yields there with the cursor advanced past them.
func TestDeleteFloorWhenPullOverruns(t *testing.T) {
	t.Run("streams exactly one frame", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)

		pass := h.slice(h.nowVal.Add(-time.Second))

		if pass != (slicePass{more: true, advanced: true}) {
			t.Fatalf("result %+v, want a yield after one visit", pass)
		}
		if got := h.dev.ContentReads(); got != 1 {
			t.Fatalf("device reads = %d, want exactly one streamed visit", got)
		}
		if h.deviceHas(frames[0].SourcePath) || !h.deviceHas(frames[1].SourcePath) || !h.deviceHas(frames[2].SourcePath) {
			t.Fatal("want only the first frame deleted")
		}
	})

	t.Run("visits cheap exits, then streams and deletes the fourth", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		frames := h.seedEligible(t, 5)
		h.slowLink(slowLinkRead)
		// Row 0 is mid-supersede; rows 1 and 2 changed on the device since filing.
		if err := h.store.PutSupersedeIntent(context.Background(), store.SupersedeIntent{
			RigID: "rig-01", DestPath: frames[0].DestPath, AsidePath: frames[0].DestPath + ".aside",
			FrameID: frames[0].ID, CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			t.Fatal(err)
		}
		h.changeSource(frames[1])
		h.changeSource(frames[2])

		pass := h.slice(h.nowVal.Add(-time.Second))

		if pass != (slicePass{more: true, advanced: true}) {
			t.Fatalf("result %+v, want a yield", pass)
		}
		if got := h.dev.ContentReads(); got != 1 {
			t.Fatalf("device reads = %d, want one streamed visit", got)
		}
		if h.deviceHas(frames[3].SourcePath) {
			t.Fatal("the fourth row should have been streamed and deleted")
		}
		for _, i := range []int{0, 1, 2, 4} {
			if !h.deviceHas(frames[i].SourcePath) {
				t.Fatalf("row %d deleted, want kept", i)
			}
		}
		if h.deferCount(t, frames[1]) != 1 || h.deferCount(t, frames[2]) != 1 {
			t.Fatal("the changed-source rows should each have been visited once")
		}
		if h.w.deleteCursor != cursorOf(frames[3]) {
			t.Fatalf("cursor %+v, want the fourth row", h.w.deleteCursor)
		}
	})

	// Each cheap exit is made to cost 20 s of the link (its source Stat), so a poll
	// of skipping covers three or four of them, depending on where the budget starts.
	cheapRows := func(t *testing.T) (*harness, []model.Frame) {
		h := newHarness(t, emmc, header.New())
		if poll := h.cfg.EffectivePollInterval(h.profile); poll != 45*time.Second {
			t.Fatalf("test assumes a 45s poll, got %v", poll)
		}
		frames := h.seedEligible(t, 6)
		for _, f := range frames {
			h.changeSource(f)
		}
		h.dev.OnStat = func(string, string) { h.nowVal = h.nowVal.Add(20 * time.Second) }
		return h, frames
	}
	// assertSkipYield checks a budget yield that visited the first `visited` rows
	// (each once, as a source-changed deferral) and nothing past them, at or after
	// budgetEnd.
	assertSkipYield := func(t *testing.T, h *harness, frames []model.Frame, visited int, budgetEnd time.Time, pass slicePass) {
		t.Helper()
		if pass != (slicePass{more: true, advanced: true}) {
			t.Fatalf("result %+v, want a budget yield that advanced the cursor", pass)
		}
		if got := h.dev.ContentReads(); got != 0 {
			t.Fatalf("device reads = %d, want none (streamed == 0)", got)
		}
		if h.nowVal.Before(budgetEnd) {
			t.Fatalf("yielded at %v, before the skip budget ended at %v", h.nowVal, budgetEnd)
		}
		for i, f := range frames {
			want := 0
			if i < visited {
				want = 1
			}
			if got := h.deferCount(t, f); got != want {
				t.Fatalf("row %d visited %d times, want %d", i, got, want)
			}
		}
		if h.w.deleteCursor != cursorOf(frames[visited-1]) {
			t.Fatalf("cursor %+v, want the last visited row", h.w.deleteCursor)
		}
		if !h.logContains("streamed=0") {
			t.Fatal("a budget yield should log streamed=0")
		}
	}

	t.Run("more cheap exits than one poll can visit", func(t *testing.T) {
		h, frames := cheapRows(t)
		poll := h.cfg.EffectivePollInterval(h.profile)
		loopStart := h.nowVal
		pass := h.slice(loopStart.Add(-time.Second))
		// The budget runs from the loop's start: rows 0-2 are visited at 0 s, 20 s and
		// 40 s, and row 3's boundary at 60 s is past loopStart + 45 s.
		assertSkipYield(t, h, frames, 3, loopStart.Add(poll), pass)
	})

	t.Run("budget runs from a deadline that falls after the loop starts", func(t *testing.T) {
		h, frames := cheapRows(t)
		poll := h.cfg.EffectivePollInterval(h.profile)
		loopStart := h.nowVal
		deadline := loopStart.Add(20 * time.Second)
		pass := h.slice(deadline)
		// Row 0 is visited before the deadline, rows 1-3 at 20 s, 40 s and 60 s, all
		// inside deadline + 45 s = 65 s; row 4's boundary at 80 s is past it. A budget
		// counted from the loop's start would have stopped at row 3's boundary.
		assertSkipYield(t, h, frames, 4, deadline.Add(poll), pass)
	})

	t.Run("loop starting past deadline plus poll still skips for one poll (design frame-lifecycle I1)", func(t *testing.T) {
		h, frames := cheapRows(t)
		poll := h.cfg.EffectivePollInterval(h.profile)
		loopStart := h.nowVal
		pass := h.slice(loopStart.Add(-3 * poll)) // the pull ran past 2 × poll
		assertSkipYield(t, h, frames, 3, loopStart.Add(poll), pass)
	})
}

// TestDeleteSliceCrossesDeadlineMidSlice: the deadline is checked at every frame
// boundary against the advancing clock, so a slice that starts before its
// deadline keeps deleting until a boundary past it, then yields.
func TestDeleteSliceCrossesDeadlineMidSlice(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	frames := h.seedEligible(t, 5)
	h.slowLink(slowLinkRead)

	// Frames 0 and 1 start at 0 s and 70 s, before the deadline; frame 2's boundary
	// at 140 s is past it.
	pass := h.slice(h.nowVal.Add(100 * time.Second))

	if pass != (slicePass{more: true, advanced: true}) {
		t.Fatalf("result %+v, want a yield", pass)
	}
	for i, f := range frames {
		if gone := !h.deviceHas(f.SourcePath); gone != (i < 2) {
			t.Fatalf("frame %d deleted=%v, want %v", i, gone, i < 2)
		}
	}
	if h.yields() != 1 {
		t.Fatalf("yield counter %v, want 1", h.yields())
	}
	if h.w.deleteCursor != cursorOf(frames[1]) {
		t.Fatalf("cursor %+v, want frame 1", h.w.deleteCursor)
	}
}

// TestDeleteCursorNoHeadStarvation: a head frame that streams and defers on every
// visit gets one visit per rotation, and every frame behind it is still deleted.
func TestDeleteCursorNoHeadStarvation(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	frames := h.seedEligible(t, 4)
	failVerifyRead(t, frames[0])
	headVisits := 0
	h.dev.OnOpenReader = func(_, p string) {
		h.nowVal = h.nowVal.Add(slowLinkRead)
		if p == frames[0].SourcePath {
			headVisits++
		}
	}

	// Slice 1 streams the head and yields; slices 2-4 each delete one frame behind it.
	for n := 1; n <= 4; n++ {
		if pass := h.slice(h.nowVal.Add(-time.Second)); !pass.more {
			t.Fatalf("slice %d: result %+v, want a yield", n, pass)
		}
	}
	for _, f := range frames[1:] {
		if h.deviceHas(f.SourcePath) {
			t.Fatalf("%s not deleted within 4 slices; the head starved it", f.SourcePath)
		}
	}
	if headVisits != 1 {
		t.Fatalf("head visited %d times in its first rotation, want 1", headVisits)
	}

	// Only the head is left: each later slice is a whole rotation, one visit each.
	for n := 1; n <= 2; n++ {
		pass := h.slice(h.nowVal.Add(-time.Second))
		if !pass.exhausted || pass.more {
			t.Fatalf("head-only slice %d: result %+v, want exhausted", n, pass)
		}
		if headVisits != 1+n {
			t.Fatalf("head visited %d times after %d more rotations, want %d", headVisits, n, 1+n)
		}
	}
	if !h.deviceHas(frames[0].SourcePath) {
		t.Fatal("a head frame that never verifies must never be deleted")
	}
	if got, _ := h.getFrame(frames[0].SourcePath); got.State != model.StateVerified {
		t.Fatalf("head state %s, want verified (deferred)", got.State)
	}
}

// TestPauseMidDrain: a Pause applied between frames ends the slice before the
// next frame and leaves the pill Paused, the frame it interrupted deferring;
// after Resume the drain continues from the cursor. A Pause and Resume served
// together let the slice carry on, with the pill back on Deleting at once and on
// the listing's state after the slice.
func TestPauseMidDrain(t *testing.T) {
	t.Run("pause stops the slice; resume continues from the cursor", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		frames := h.seedEligible(t, 4)
		// The head streams and defers on every visit, so a slice that restarted from
		// the head rather than the cursor would spend its one streamed visit there.
		failVerifyRead(t, frames[0])
		h.w.listedState = model.RigStatePulling
		h.dev.OnOpenReader = func(_, p string) {
			h.nowVal = h.nowVal.Add(slowLinkRead)
			if p == frames[1].SourcePath {
				h.w.Send(Pause) // the operator pauses while frame 1 is being hashed
			}
		}

		pass := h.w.deletePhase(context.Background(), h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint(sliceAddr)), h.dev, h.nowVal.Add(time.Hour))

		if pass != (slicePass{advanced: true}) {
			t.Fatalf("result %+v, want more == false after a pause", pass)
		}
		// The Pause interrupts the in-flight re-hash (design frame-lifecycle §12.8),
		// so that frame defers rather than finishing; the slice still stops before
		// the next frame.
		if !h.deviceHas(frames[1].SourcePath) {
			t.Fatal("the frame whose re-hash the pause interrupted must defer, not delete")
		}
		if got, _ := h.getFrame(frames[1].SourcePath); got.State != model.StateVerified {
			t.Fatalf("interrupted frame state %s, want verified (deferred)", got.State)
		}
		if !h.deviceHas(frames[2].SourcePath) || !h.deviceHas(frames[3].SourcePath) {
			t.Fatal("the pause should have stopped the slice before the next frame")
		}
		if st := h.status(); st.State != model.RigStatePaused || !st.Paused {
			t.Fatalf("state %q paused=%v, want Paused", st.State, st.Paused)
		}
		if h.w.deleteCursor != cursorOf(frames[1]) {
			t.Fatalf("cursor %+v, want frame 1", h.w.deleteCursor)
		}

		h.w.setPaused(false)
		h.dev.OnOpenReader = func(string, string) { h.nowVal = h.nowVal.Add(slowLinkRead) }
		pass = h.slice(h.nowVal.Add(-time.Second))

		if !pass.more {
			t.Fatalf("result %+v, want a yield after one streamed visit", pass)
		}
		if h.deviceHas(frames[2].SourcePath) {
			t.Fatal("the resumed slice should have continued at frame 2, after the cursor")
		}
		if !h.deviceHas(frames[3].SourcePath) {
			t.Fatal("the resumed slice should have yielded after frame 2")
		}
	})

	t.Run("pause and resume in one drain", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		frames := h.seedEligible(t, 3)
		h.w.listedState = model.RigStatePulling
		rec := h.recordStatuses()
		h.dev.OnOpenReader = func(_, p string) {
			if p == frames[0].SourcePath {
				h.w.Send(Pause)
				h.w.Send(Resume)
			}
		}

		pass := h.w.deletePhase(context.Background(), h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint(sliceAddr)), h.dev, h.nowVal.Add(time.Hour))

		if pass != (slicePass{advanced: true, sweepDone: true, exhausted: true}) {
			t.Fatalf("result %+v, want the slice to run to the end", pass)
		}
		for _, f := range frames {
			if h.deviceHas(f.SourcePath) {
				t.Fatalf("%s not deleted; the resumed slice should have gone on", f.SourcePath)
			}
		}
		// The published pills after the Pause lands: Paused, then Deleting as soon
		// as the Resume applies, before the next frame's readout.
		pubs := rec.take()
		paused := slices.IndexFunc(pubs, func(p model.RigStatus) bool { return p.State == model.RigStatePaused })
		if paused < 0 || paused+1 >= len(pubs) {
			t.Fatalf("no Paused publish followed by another: %+v", pubs)
		}
		if next := pubs[paused+1]; next.State != model.RigStateDeleting || next.Paused {
			t.Fatalf("publish after the Pause = %+v, want Deleting on an unpaused rig", next)
		}
		if st := h.status(); st.State != model.RigStatePulling || st.Paused {
			t.Fatalf("state %q paused=%v after the slice, want the listing's %q", st.State, st.Paused, model.RigStatePulling)
		}
	})
}

// TestPillRestoreAfterRestorePhasePause: a Pause and a Resume served by the
// restore pass's drainCommands leave Paused on the pill of an unpaused rig; the
// slice that follows must end on the listing's state, not reinstate that Paused.
func TestPillRestoreAfterRestorePhasePause(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.resolver.resolveAddr, h.resolver.resolveOnline = sliceAddr, true
	frames := h.seedEligible(t, 4)
	// Frames 0 and 1 were culled and asked back: the restore pass walks them,
	// draining commands before each.
	for _, f := range frames[:2] {
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.setMissing(t, f.ID, 60)
		if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
			t.Fatalf("request restore: ok=%v err=%v", ok, err)
		}
	}
	// Queue the Pause and Resume while the first restore re-reads its original, so
	// the restore pass's drain before the second one serves them — after the
	// listing, before the slice.
	queued := false
	h.dev.OnOpenReader = func(_, p string) {
		if !queued && p == frames[0].SourcePath {
			queued = true
			h.w.Send(Pause)
			h.w.Send(Resume)
		}
	}
	rec := h.recordStatuses()

	h.cycle()

	if !queued {
		t.Fatal("precondition: the restore pass never read the first restored frame")
	}
	pubs := rec.take()
	paused := slices.IndexFunc(pubs, func(p model.RigStatus) bool { return p.State == model.RigStatePaused })
	deleting := slices.IndexFunc(pubs, func(p model.RigStatus) bool { return p.State == model.RigStateDeleting })
	if paused < 0 || deleting < paused {
		t.Fatalf("precondition: want Paused published before the slice's Deleting; got %+v", pubs)
	}
	if h.deviceHas(frames[2].SourcePath) || h.deviceHas(frames[3].SourcePath) {
		t.Fatal("precondition: the slice should have run and deleted the proven frames")
	}
	st := h.status()
	if st.Paused {
		t.Fatal("the rig should be unpaused")
	}
	if st.State == model.RigStatePaused || st.State != h.w.listedState {
		t.Fatalf("state %q after the slice, want the listing's %q", st.State, h.w.listedState)
	}
}

// TestOnlyDisabledSharesRemain: a slice left holding only rows on disabled shares
// neither visits them nor reports more work, so it never drives a continuation.
func TestOnlyDisabledSharesRemain(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	frames := h.seedEligible(t, 2)
	h.profile.Sources[0].Disabled = true

	pass := h.slice(h.nowVal.Add(-time.Hour))

	if pass != (slicePass{sweepDone: true, exhausted: true}) {
		t.Fatalf("result %+v, want more == false and exhausted", pass)
	}
	for _, f := range frames {
		if !h.deviceHas(f.SourcePath) {
			t.Fatalf("%s on a disabled share was deleted", f.SourcePath)
		}
	}
	if h.yields() != 0 {
		t.Fatalf("yield counter %v, want 0", h.yields())
	}
}

// TestDeleteSliceResultFlags pins the slice result on every kind of return, since
// runCycle reads them for its continuation, its debt and a run's end.
func TestDeleteSliceResultFlags(t *testing.T) {
	past := func(h *harness) time.Time { return h.nowVal.Add(-time.Second) }
	future := func(h *harness) time.Time { return h.nowVal.Add(time.Hour) }

	cases := []struct {
		name  string
		setup func(t *testing.T, h *harness)
		dl    func(h *harness) time.Time
		want  slicePass
	}{
		{"empty eligible list", func(*testing.T, *harness) {}, past,
			slicePass{sweepDone: true, exhausted: true}},
		{"full pass from a zero cursor", func(t *testing.T, h *harness) { h.seedEligible(t, 3) }, future,
			slicePass{advanced: true, sweepDone: true, exhausted: true}},
		{"yield before the wrap index", func(t *testing.T, h *harness) { h.seedEligible(t, 3) }, past,
			slicePass{more: true, advanced: true}},
		{"yield after the wrap index", func(t *testing.T, h *harness) {
			frames := h.seedEligible(t, 3)
			h.w.deleteCursor = cursorOf(frames[2]) // on the tail: the head is the wrap
		}, past, slicePass{more: true, advanced: true, sweepDone: true}},
		{"yield at the wrap index without visiting it", func(t *testing.T, h *harness) {
			frames := h.seedEligible(t, 3)
			// Rotated [2, 0, 1]: the slice visits the tail, then stops before the head.
			// It has not moved the cursor past the tail; the slice that visits the head
			// counts that boundary, so this one must not.
			h.w.deleteCursor = cursorOf(frames[1])
		}, past, slicePass{more: true, advanced: true}},
		{"suspend after the wrap index", func(t *testing.T, h *harness) {
			frames := h.seedEligible(t, 3)
			h.w.deleteCursor = cursorOf(frames[2])
			h.dev.OnOpenReader = func(string, string) { h.w.Send(Pause) }
		}, future, slicePass{advanced: true, sweepDone: true}},
		{"suspended before the first frame", func(t *testing.T, h *harness) {
			h.seedEligible(t, 3)
			h.w.Send(Pause)
		}, future, slicePass{}},
		{"non-positive grace", func(t *testing.T, h *harness) {
			h.seedEligible(t, 1)
			zero := model.Duration(0)
			h.profile.Grace = &zero
		}, future, slicePass{}},
		{"session open failure", func(t *testing.T, h *harness) {
			h.seedEligible(t, 1)
			h.dev.FailSessionOpen(errors.New("connection refused"))
		}, future, slicePass{}},
		{"identity unconfirmed", func(t *testing.T, h *harness) {
			h.seedEligible(t, 1)
			h.profile.GUID = "dev-guid"
			h.resolver.probeOK = false
		}, future, slicePass{unconfirmed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			h.slowLink(slowLinkRead)
			tc.setup(t, h)
			if got := h.slice(tc.dl(h)); got != tc.want {
				t.Fatalf("result %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDeleteSliceForeignIdentityKeepsError: a foreign device answering a later
// slice's probe raises the error before the pill restore is registered, so the
// slice's end must not paper over it with the listing's state.
func TestDeleteSliceForeignIdentityKeepsError(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	frames := h.seedEligible(t, 3)
	h.profile.GUID = "dev-guid"
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
	h.slowLink(slowLinkRead)
	h.w.listedState = model.RigStateIdle

	if pass := h.slice(h.nowVal.Add(-time.Second)); !pass.more {
		t.Fatalf("first slice %+v, want a yield", pass)
	}
	h.resolver.probeGUID = "someone-else" // the device changed between slices

	pass := h.slice(h.nowVal.Add(-time.Second))

	if pass != (slicePass{}) {
		t.Fatalf("result %+v, want a prologue return", pass)
	}
	if st := h.status(); st.State != model.RigStateError {
		t.Fatalf("state %q, want Error to survive the slice", st.State)
	}
	// Commander's own probe saw the other device, so pulls are refused too.
	const want = "identity mismatch: a different device answered at this address; refusing pulls and deletes"
	if st := h.status(); st.LastError != want {
		t.Fatalf("LastError %q, want %q", st.LastError, want)
	}
	if !h.deviceHas(frames[1].SourcePath) || !h.deviceHas(frames[2].SourcePath) {
		t.Fatal("a foreign delete session must delete nothing")
	}
}

// TestDeleteSliceRestoreWithoutListing: a slice run on a worker that has not yet
// listed restores Idle, the state a fresh worker starts in, never an empty state.
func TestDeleteSliceRestoreWithoutListing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.seedEligible(t, 1)

	h.w.deleteAllForTest(context.Background(), h.cfg, h.profile, h.dev, sliceAddr)

	if st := h.status(); st.State != model.RigStateIdle {
		t.Fatalf("state %q, want Idle", st.State)
	}
}
