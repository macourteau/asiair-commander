package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The tests in this file drive a run until idle on a paused rig (design
// frame-lifecycle §12.6 "Run now while paused runs until idle" and
// "Run state"): its cycles, every row of the command and end tables, and what
// the card is published at each.

// pauseRig applies a Pause as Run's select would.
func (h *harness) pauseRig() { h.w.apply(context.Background(), Pause) }

// runNow applies a Run now as Run's select would, lets the delay it returned
// pass, and returns it.
func (h *harness) runNow() time.Duration {
	next, _ := h.w.apply(context.Background(), RunNow)
	h.nowVal = h.nowVal.Add(next)
	return next
}

// serve applies the oldest queued command as Run's select would, letting the
// delay pass when it ran a cycle, and reports whether it did.
func (h *harness) serve(t *testing.T) bool {
	t.Helper()
	select {
	case c := <-h.w.cmds:
		next, sched := h.w.apply(context.Background(), c)
		if sched == schedRan {
			h.nowVal = h.nowVal.Add(next)
		}
		return sched == schedRan
	default:
		t.Fatal("no command queued")
		return false
	}
}

// runToEnd runs the cycles Run's ticker would while the run lasts, at most max,
// and returns how many it ran.
func (h *harness) runToEnd(t *testing.T, max int) int {
	t.Helper()
	for n := 1; n <= max; n++ {
		h.runCycleAndWait()
		if !h.w.runUntilIdle {
			return n
		}
	}
	t.Fatalf("the run was still going after %d cycles", max)
	return 0
}

// keepCaptureDir keeps the capture folder once its last frame is deleted, as on a
// real device; the fake would otherwise drop the emptied root and fail the listing.
func (h *harness) keepCaptureDir() { h.dev.AddDir(emmc, "Autorun/Light", h.nowVal.Add(-3*time.Hour)) }

// runEndedBecause reports that a run ended for why, as its log line gives it.
func (h *harness) runEndedBecause(why string) bool {
	for _, line := range strings.Split(h.logBuf.String(), "\n") {
		if strings.Contains(line, `msg="rig: run until idle ended"`) && strings.Contains(line, " why="+why+" ") {
			return true
		}
	}
	return false
}

// drainingRun pauses a rig holding three proven frames on a slow Wi-Fi
// link and starts a run, whose first cycle deletes one frame and yields.
func drainingRun(t *testing.T) (*harness, []model.Frame) {
	t.Helper()
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 3)
	h.keepCaptureDir()
	h.slowLink(slowLinkRead)
	h.pauseRig()
	if next := h.runNow(); next != time.Second {
		t.Fatalf("precondition: the run's first cycle returned %v, want a yield", next)
	}
	if !h.status().RunUntilIdle || h.onDevice(frames) != 2 {
		t.Fatal("precondition: the run should be going with one frame deleted")
	}
	return h, frames
}

// assertRunEnded checks that the run is over on a rig still paused, with the pill
// and verdict the end table gives, and returns the published status.
func assertRunEnded(t *testing.T, h *harness, state model.RigState, safe model.SafeToPowerOff, reason string) model.RigStatus {
	t.Helper()
	st := h.status()
	if h.w.runUntilIdle || st.RunUntilIdle {
		t.Fatal("the run is still going")
	}
	if !h.w.paused || !st.Paused {
		t.Fatal("the rig is no longer paused after its run")
	}
	if st.State != state {
		t.Fatalf("state %q, want %q", st.State, state)
	}
	if st.SafeToPowerOff != safe || st.PowerOffReason != reason {
		t.Fatalf("verdict %q/%q, want %q/%q", st.SafeToPowerOff, st.PowerOffReason, safe, reason)
	}
	if st.Draining {
		t.Fatal("a paused rig whose run ended still reads draining")
	}
	return st
}

// addCaptureAt drops a second settled capture beside newCapture, for a pull walk
// that has more than one frame to copy.
func (h *harness) addCaptureAt(t *testing.T, p string) {
	t.Helper()
	h.dev.AddFile(emmc, p, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
}

// TestRunNowWhilePausedRunsUntilIdle: Run now on a paused rig runs the ordinary
// sliced cycles until nothing more needs doing, then leaves the rig paused; every
// command and every end of the run behaves as the tables of design
// frame-lifecycle §12.6 give.
func TestRunNowWhilePausedRunsUntilIdle(t *testing.T) {
	t.Run("drains a multi-slice backlog, then stops still paused", func(t *testing.T) {
		h, frames := drainingRun(t)
		st := h.status()
		if st.State == model.RigStatePaused || !st.Paused {
			t.Fatalf("during the run state=%q paused=%v, want the cycle's state on a paused rig", st.State, st.Paused)
		}
		if st.PowerOffReason == "paused" || st.SafeAsOf != st.ListingTS {
			t.Fatalf("during the run the verdict %q/%q (as of %d, listed %d) was not computed from the listing",
				st.SafeToPowerOff, st.PowerOffReason, st.SafeAsOf, st.ListingTS)
		}

		n := h.runToEnd(t, 10)

		if h.onDevice(frames) != 0 {
			t.Fatalf("%d frames left on the device; the run should have drained them", h.onDevice(frames))
		}
		if n < 3 {
			t.Fatalf("the run ended after %d more cycles; two frames each need a slice", n)
		}
		st = assertRunEnded(t, h, model.RigStatePaused, model.SafeYes, "")
		if st.SafeAsOf == 0 || st.SafeAsOf != st.ListingTS {
			t.Fatalf("final verdict as of %d, want the final listing's time %d", st.SafeAsOf, st.ListingTS)
		}
		if !h.runEndedBecause("idle") {
			t.Fatal("the run should have ended at idle")
		}
	})

	t.Run("the exhausted end keeps the final verdict until Run now or Resume", func(t *testing.T) {
		h, _ := drainingRun(t)
		h.runToEnd(t, 10)
		final := h.status()

		// Paused, the rig runs no cycle, so the verdict and its time stand.
		h.nowVal = h.nowVal.Add(10 * time.Minute)
		if !h.w.workSuspended() {
			t.Fatal("precondition: a paused rig whose run ended is suspended")
		}
		if st := h.status(); st.SafeToPowerOff != final.SafeToPowerOff || st.SafeAsOf != final.SafeAsOf {
			t.Fatalf("verdict moved to %q as of %d without a cycle", st.SafeToPowerOff, st.SafeAsOf)
		}

		h.runNow() // a new run's first listing replaces it
		if st := h.status(); st.SafeAsOf <= final.SafeAsOf {
			t.Fatalf("Run now left the verdict as of %d, want a fresh listing's time", st.SafeAsOf)
		}
		h.runToEnd(t, 5)
		final = h.status()

		h.nowVal = h.nowVal.Add(10 * time.Minute)
		h.w.apply(context.Background(), Resume)
		if st := h.status(); st.Paused || st.SafeAsOf <= final.SafeAsOf {
			t.Fatalf("Resume left paused=%v and the verdict as of %d, want a fresh listing", st.Paused, st.SafeAsOf)
		}
	})

	t.Run("held and always-deferring frames do not keep it running", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 2)
		failVerifyRead(t, frames[0]) // eligible, and defers on every visit
		held := seedHeldFrame(t, h)
		h.pauseRig()

		h.runNow()
		h.runToEnd(t, 5)

		if h.deviceHas(frames[1].SourcePath) {
			t.Fatal("the provable frame should have been deleted")
		}
		if !h.deviceHas(frames[0].SourcePath) || !h.deviceHas(held) {
			t.Fatal("the deferring and held frames must stay on the device")
		}
		assertRunEnded(t, h, model.RigStatePaused, model.SafeWorking, "")
		if !h.runEndedBecause("idle") {
			t.Fatal("the run should have ended at idle, the rest deferring")
		}
	})

	t.Run("slow link: rows that stream then defer end at idle after a whole sweep", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 2)
		for _, f := range frames {
			failVerifyRead(t, f) // streams the device original, then defers
		}
		visits := map[string]int{}
		h.dev.OnOpenReader = func(_, p string) {
			h.nowVal = h.nowVal.Add(slowLinkRead) // one streamed visit outlasts poll
			visits[p]++
		}
		h.pauseRig()

		h.runNow()
		h.runToEnd(t, 8)

		for _, f := range frames {
			if visits[f.SourcePath] == 0 {
				t.Fatalf("%s was never tried before the run ended", f.SourcePath)
			}
			if !h.deviceHas(f.SourcePath) {
				t.Fatalf("%s deleted; it never proved out", f.SourcePath)
			}
		}
		if !h.runEndedBecause("idle") {
			t.Fatal("the run should have ended at idle once a whole sweep deleted nothing")
		}
	})

	t.Run("run now during a run runs no cycle", func(t *testing.T) {
		h, _ := drainingRun(t)
		reads, listings := h.dev.ContentReads(), h.w.listings

		if _, sched := h.w.apply(context.Background(), RunNow); sched != schedKeep {
			t.Fatalf("Run now during a run scheduled %v, want the ticker's deadline left in place", sched)
		}
		if h.dev.ContentReads() != reads || h.w.listings != listings || !h.w.runUntilIdle {
			t.Fatal("Run now during a run must change nothing")
		}
	})

	t.Run("stop run on a suspended rig has no effect", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.cycle()
		h.pauseRig()
		before := h.status()

		if _, sched := h.w.apply(context.Background(), StopRun); sched == schedRan {
			t.Fatal("Stop run ran a cycle")
		}
		if st := h.status(); !reflect.DeepEqual(st, before) {
			t.Fatalf("Stop run on a suspended rig changed its status:\n%+v\nwant\n%+v", st, before)
		}
	})

	t.Run("stop run between listings keeps the final verdict", func(t *testing.T) {
		h, frames := drainingRun(t)
		last := h.status()

		if _, sched := h.w.apply(context.Background(), StopRun); sched == schedRan {
			t.Fatal("Stop run ran a cycle")
		}

		st := assertRunEnded(t, h, model.RigStatePaused, last.SafeToPowerOff, last.PowerOffReason)
		if st.SafeAsOf != last.SafeAsOf || st.SafeAsOf == 0 {
			t.Fatalf("verdict as of %d, want the last listing's %d", st.SafeAsOf, last.SafeAsOf)
		}
		if h.onDevice(frames) != 2 {
			t.Fatal("a stopped run must not go on deleting")
		}
	})

	t.Run("stop run mid-slice finishes the frame, then ends", func(t *testing.T) {
		h, frames := drainingRun(t)
		h.dev.OnOpenReader = func(_, p string) {
			if p == frames[1].SourcePath {
				h.w.Send(StopRun)
			}
		}
		listed := h.status()

		h.runCycleAndWait()

		if h.deviceHas(frames[1].SourcePath) {
			t.Fatal("the in-flight frame should have finished")
		}
		if !h.deviceHas(frames[2].SourcePath) {
			t.Fatal("the stop should have ended the slice before the next frame")
		}
		st := h.status()
		assertRunEnded(t, h, model.RigStatePaused, st.SafeToPowerOff, st.PowerOffReason)
		if st.PowerOffReason == "paused" || st.SafeAsOf <= listed.SafeAsOf {
			t.Fatalf("verdict %q/%q as of %d, want this cycle's listing verdict", st.SafeToPowerOff, st.PowerOffReason, st.SafeAsOf)
		}
	})

	t.Run("stop run mid-pull ends with that listing's paused verdict", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		h.addCaptureAt(t, "Autorun/Light/C-2025 A6/Light_NGC_0002.fit")
		h.dev.OnOpenReader = func(string, string) { h.w.Send(StopRun) }
		h.pauseRig()

		h.runNow()

		if acquired, _ := h.adm.counts(); acquired != 1 {
			t.Fatalf("%d copies started; the stop should have suspended the rest of the listing", acquired)
		}
		if st := assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "paused"); st.SafeAsOf != 0 {
			t.Fatalf("the paused verdict came from the stop, not the listing, yet reads as of %d", st.SafeAsOf)
		}
	})

	t.Run("a listing that fails does not end it at idle", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t) // ready to copy, behind a listing that fails
		h.dev.FailListDir(emmc, errors.New("listing failed"))
		h.pauseRig()

		h.runNow() // nothing to delete: the sweep is whole at once
		for n := 1; n < maxNoProgressCycles; n++ {
			if !h.w.runUntilIdle {
				t.Fatalf("the run ended after %d cycles whose listing failed", n)
			}
			h.runCycleAndWait()
		}
		assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "listing_failed")
		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("the run should have ended on its no-progress count, never at idle")
		}
	})

	t.Run("a cursor advance behind a listing that fails is not progress", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 1)
		failVerifyRead(t, frames[0]) // streams and defers on every visit
		visits := 0
		h.dev.OnOpenReader = func(string, string) { visits++ }
		h.dev.FailListDir(emmc, errors.New("listing failed"))
		h.pauseRig()

		h.runNow()
		for n := 1; n < maxNoProgressCycles; n++ {
			if !h.w.runUntilIdle || h.w.noProgress != n {
				t.Fatalf("after %d cycles no-progress=%d running=%v, want %d and running", n, h.w.noProgress, h.w.runUntilIdle, n)
			}
			h.runCycleAndWait()
		}
		if visits != maxNoProgressCycles {
			t.Fatalf("the deferring frame was streamed %d times, want once per cycle (%d)", visits, maxNoProgressCycles)
		}
		assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "listing_failed")
		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("the run should have ended on its no-progress count")
		}
		if !h.deviceHas(frames[0].SourcePath) {
			t.Fatal("the deferring frame must stay on the device")
		}
	})

	t.Run("a delete behind a listing that fails resets the count", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 2)
		for _, f := range frames {
			failVerifyRead(t, f)
		}
		h.keepCaptureDir()
		h.dev.FailListDir(emmc, errors.New("listing failed"))
		h.pauseRig()

		h.runNow()
		h.runCycleAndWait()
		if h.w.noProgress != 2 || !h.w.runUntilIdle {
			t.Fatalf("precondition: after two cycles no-progress=%d running=%v", h.w.noProgress, h.w.runUntilIdle)
		}
		// The second frame's NAS copy reads again: the next slice proves and deletes it.
		if err := os.Remove(frames[1].DestPath); err != nil {
			t.Fatal(err)
		}
		writeLocal(t, frames[1].DestPath, []byte(fmt.Sprintf("a-proven-original-frame-%02d", 1)))
		h.runCycleAndWait()
		if h.deviceHas(frames[1].SourcePath) || h.w.noProgress != 0 || !h.w.runUntilIdle {
			t.Fatalf("a delete should reset the count and keep the run going: deleted=%v no-progress=%d running=%v",
				!h.deviceHas(frames[1].SourcePath), h.w.noProgress, h.w.runUntilIdle)
		}

		for n := 1; n < maxNoProgressCycles; n++ {
			h.runCycleAndWait()
			if !h.w.runUntilIdle {
				t.Fatalf("the run ended %d cycles after the delete, before its count", n)
			}
		}
		h.runCycleAndWait()
		assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "listing_failed")
		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("the run should have ended on its no-progress count")
		}
	})

	t.Run("pause during a run ends it with the paused verdict", func(t *testing.T) {
		h, _ := drainingRun(t)
		h.pauseRig()
		st := assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "paused")
		if st.SafeAsOf != 0 {
			t.Fatalf("a Pause's verdict came from no listing, yet reads as of %d", st.SafeAsOf)
		}
	})

	t.Run("resume during a run ends it, unpauses and checks at once", func(t *testing.T) {
		h, frames := drainingRun(t)
		h.nowVal = h.nowVal.Add(time.Minute)

		if _, sched := h.w.apply(context.Background(), Resume); sched != schedRan {
			t.Fatal("Resume should run a cycle at once")
		}

		st := h.status()
		if h.w.runUntilIdle || st.RunUntilIdle || st.Paused || h.w.paused {
			t.Fatalf("after Resume run=%v paused=%v, want an unpaused rig with no run", st.RunUntilIdle, st.Paused)
		}
		if st.State == model.RigStatePaused || st.PowerOffReason == "paused" {
			t.Fatalf("after Resume state %q verdict reason %q, want the listing's", st.State, st.PowerOffReason)
		}
		if h.onDevice(frames) != 1 {
			t.Fatal("the resumed cycle should have deleted the next frame")
		}
	})

	t.Run("a foreign identity ends it in Error", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID = "dev-guid"
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		h.pauseRig()
		h.runNow()

		h.resolver.mu.Lock()
		h.resolver.probeGUID = "someone-else"
		h.resolver.mu.Unlock()
		h.runCycleAndWait()

		assertRunEnded(t, h, model.RigStateError, model.SafeUnknown, "identity")
		if h.onDevice(frames) != 2 {
			t.Fatal("nothing may be deleted from a foreign device")
		}
	})

	t.Run("a foreign identity at the delete probe ends it in Error", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID = "dev-guid"
		// Probes 1 and 2 are the run's first pull and slice; 3 is the next pull, 4 its slice.
		h.w.resolver = &scriptedProbe{fakeResolver: h.resolver, from: 4, foreign: true}
		h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		h.pauseRig()
		h.runNow()

		h.runCycleAndWait()

		assertRunEnded(t, h, model.RigStateError, model.SafeUnknown, "identity")
	})

	t.Run("an offline cycle ends it back at Paused", func(t *testing.T) {
		h, _ := drainingRun(t)
		h.dev.SetReachable(false)

		h.runCycleAndWait()

		st := assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, reasonOfflineDuringRun)
		if st.Online || st.SafeAsOf != 0 {
			t.Fatalf("online=%v as of %d, want an offline rig whose verdict came from no listing", st.Online, st.SafeAsOf)
		}
	})

	t.Run("an unconfirmed identity ends it only through the no-progress count", func(t *testing.T) {
		h, _ := drainingRun(t)
		h.resolver.mu.Lock()
		h.resolver.probeOK = false
		h.resolver.mu.Unlock()

		for n := 1; n < maxNoProgressCycles; n++ {
			h.runCycleAndWait()
			if !h.w.runUntilIdle {
				t.Fatalf("an unconfirmed identity ended the run after %d cycles", n)
			}
		}
		h.runCycleAndWait()

		// The refused cycles published Unknown, which the end keeps.
		assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "identity")
		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("the run should have ended on its no-progress count")
		}
	})

	t.Run("an unreadable destination ends it on the third cycle; a copy resets the count", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		unreadable := fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)
		h.adm.acqErr = unreadable
		h.pauseRig()

		h.runNow()
		h.runCycleAndWait()
		if h.w.noProgress != 2 || !h.w.runUntilIdle {
			t.Fatalf("after two blocked cycles no-progress=%d running=%v", h.w.noProgress, h.w.runUntilIdle)
		}
		h.adm.acqErr = nil
		h.runCycleAndWait() // the capture copies
		if _, ok := h.getFrame(newCapture); !ok || h.w.noProgress != 0 || !h.w.runUntilIdle {
			t.Fatalf("a copy should reset the count and keep the run going: no-progress=%d running=%v", h.w.noProgress, h.w.runUntilIdle)
		}

		h.addCaptureAt(t, "Autorun/Light/C-2025 A6/Light_NGC_0002.fit")
		h.adm.acqErr = unreadable
		for n := 1; n < maxNoProgressCycles; n++ {
			h.runCycleAndWait()
			if !h.w.runUntilIdle {
				t.Fatalf("the run ended after %d blocked cycles, before its count", n)
			}
		}
		h.runCycleAndWait()
		assertRunEnded(t, h, model.RigStatePaused, model.SafeWorking, "")
		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("the run should have ended on its no-progress count")
		}
	})

	t.Run("a cursor advance resets the no-progress count", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		for _, f := range frames {
			failVerifyRead(t, f)
		}
		h.slowLink(slowLinkRead)
		h.addCapture(t)
		h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)
		h.pauseRig()

		h.runNow()
		h.runCycleAndWait()
		if h.w.noProgress != 2 {
			t.Fatalf("precondition: no-progress=%d after two blocked cycles, want 2", h.w.noProgress)
		}
		h.dev.RemoveFile(emmc, newCapture) // nothing to copy: the slice runs
		h.runCycleAndWait()

		if h.w.deleteCursor != cursorOf(frames[0]) || h.w.noProgress != 0 || !h.w.runUntilIdle {
			t.Fatalf("cursor %+v no-progress=%d running=%v, want an advance that resets the count",
				h.w.deleteCursor, h.w.noProgress, h.w.runUntilIdle)
		}
	})

	t.Run("skip-budget yields are progress", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 20)
		// Every row is mid-supersede, a cheap exit reached after the source Stat,
		// which costs 20 s of the link: each slice skips a few rows and yields on its
		// budget, streaming nothing.
		for _, f := range frames {
			if err := h.store.PutSupersedeIntent(context.Background(), store.SupersedeIntent{
				RigID: "rig-01", DestPath: f.DestPath, AsidePath: f.DestPath + ".aside",
				FrameID: f.ID, CreatedAt: h.nowVal.Unix(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		h.dev.OnStat = func(string, string) { h.nowVal = h.nowVal.Add(20 * time.Second) }
		h.pauseRig()

		h.runNow()
		cycles := 1
		for h.w.runUntilIdle {
			if h.w.noProgress != 0 {
				t.Fatalf("cycle %d: a skip-budget yield counted as no progress (%d)", cycles, h.w.noProgress)
			}
			if cycles > 20 {
				t.Fatal("the run never ended")
			}
			h.runCycleAndWait()
			cycles++
		}
		if cycles <= maxNoProgressCycles {
			t.Fatalf("the run ended after %d cycles; too few to tell a budget yield from no progress", cycles)
		}
		if !h.logContains("streamed=0") || !h.runEndedBecause("idle") {
			t.Fatal("the run should have yielded on its skip budget, then ended at idle")
		}
		if h.onDevice(frames) != len(frames) {
			t.Fatal("no mid-supersede row may be deleted")
		}
	})

	t.Run("keep originals ends once nothing is ready to copy", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.profile.KeepOriginals = true
		h.addCapture(t)
		h.pauseRig()

		h.runNow()
		if _, ok := h.getFrame(newCapture); !ok || !h.w.runUntilIdle {
			t.Fatal("the run's first cycle should copy the capture and go on")
		}
		if n := h.runToEnd(t, 3); n != 1 {
			t.Fatalf("the run took %d more cycles; one listing with nothing to copy is idle", n)
		}
		if !h.deviceHas(newCapture) {
			t.Fatal("a keep-originals rig never deletes")
		}
		if !h.runEndedBecause("idle") {
			t.Fatal("the run should have ended at idle")
		}
	})

	t.Run("run now queued behind a pause, served mid-slice, starts the run", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		h.dev.OnOpenReader = func(_, p string) {
			if p == frames[0].SourcePath {
				h.w.Send(Pause)
				h.w.Send(RunNow)
			}
		}

		h.runCycleAndWait() // unpaused: the slice's drain serves both

		// The Pause interrupts frames[0]'s re-hash (design frame-lifecycle §12.8), so
		// that frame defers; the drain then serves both commands and the run carries
		// the slice on through the rest.
		if !h.deviceHas(frames[0].SourcePath) || h.onDevice(frames) != 1 {
			t.Fatalf("%d frames left; want only the interrupted one, the run carrying the slice on", h.onDevice(frames))
		}
		st := h.status()
		if !h.w.paused || !h.w.runUntilIdle || !st.RunUntilIdle {
			t.Fatalf("paused=%v run=%v, want a paused rig running until idle", h.w.paused, st.RunUntilIdle)
		}
		if st.State == model.RigStatePaused {
			t.Fatal("the pill should show the cycle, not Paused, while the run goes on")
		}
	})

	t.Run("stop run then run now in one drain leaves the run going", func(t *testing.T) {
		h, frames := drainingRun(t)
		rec := h.recordStatuses()
		h.dev.OnOpenReader = func(_, p string) {
			h.nowVal = h.nowVal.Add(slowLinkRead)
			if p == frames[1].SourcePath {
				h.w.Send(StopRun)
				h.w.Send(RunNow)
			}
		}

		h.runCycleAndWait()

		if !h.w.runUntilIdle {
			t.Fatal("the later Run now should have kept the run going")
		}
		for _, p := range rec.take() {
			if !p.RunUntilIdle {
				t.Fatalf("published %+v with the run ended; the queue said to go on", p)
			}
		}
		h.runToEnd(t, 5)
		if h.onDevice(frames) != 0 {
			t.Fatal("the run should have gone on to drain the backlog")
		}
	})

	t.Run("a run re-bases its sweep and no-progress records", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		for _, f := range frames {
			failVerifyRead(t, f)
		}
		visits := map[string]int{}
		h.dev.OnOpenReader = func(_, p string) {
			h.nowVal = h.nowVal.Add(slowLinkRead)
			visits[p]++
		}
		// Unpaused, the sweep count advances well past zero, the cursor mid-list.
		for h.w.sweeps < 2 || h.w.deleteCursor == (deleteCursor{}) || h.w.deleteCursor == cursorOf(frames[2]) {
			h.runCycleAndWait()
		}
		h.pauseRig()
		clear(visits)
		start := h.w.sweeps

		h.runNow()
		if !h.w.runUntilIdle {
			t.Fatal("the run ended in its first cycle on sweeps counted before it began")
		}
		h.runToEnd(t, 8)
		if h.w.sweeps < start+2 {
			t.Fatalf("the run ended at sweep %d, before a whole sweep within it (from %d)", h.w.sweeps, start)
		}
		for _, f := range frames {
			if visits[f.SourcePath] == 0 {
				t.Fatalf("%s was not tried within the run", f.SourcePath)
			}
		}
	})

	t.Run("a run does not end on an earlier run's no-progress count", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)
		h.pauseRig()
		h.runNow()
		h.runToEnd(t, maxNoProgressCycles)
		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("precondition: the first run should have ended on its no-progress count")
		}

		h.runNow()
		if !h.w.runUntilIdle || h.w.noProgress != 1 {
			t.Fatalf("the second run's first cycle: running=%v no-progress=%d, want a fresh count",
				h.w.runUntilIdle, h.w.noProgress)
		}
	})

	t.Run("on an unpaused rig run now checks at once and stop run changes nothing", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)

		if _, sched := h.w.apply(context.Background(), RunNow); sched != schedRan {
			t.Fatal("Run now on an unpaused rig should run a cycle at once")
		}
		if _, ok := h.getFrame(newCapture); !ok {
			t.Fatal("the cycle should have copied the capture")
		}
		before := h.status()
		if before.RunUntilIdle || before.Paused {
			t.Fatal("Run now on an unpaused rig must not start a run")
		}

		if _, sched := h.w.apply(context.Background(), StopRun); sched == schedRan {
			t.Fatal("Stop run ran a cycle")
		}
		if st := h.status(); !reflect.DeepEqual(st, before) {
			t.Fatalf("Stop run on an unpaused rig changed its status:\n%+v\nwant\n%+v", st, before)
		}
	})
}

// seedHeldFrame puts a frame on the device whose row is held: never copied again,
// never deleted, and so never work a run can finish.
func seedHeldFrame(t *testing.T, h *harness) string {
	t.Helper()
	const src = "Autorun/Light/Held_0001.fit"
	mt := h.nowVal.Add(-2 * time.Hour)
	content := []byte("a-held-frame")
	h.dev.AddFile(emmc, src, content, mt)
	if err := h.store.UpsertFrame(context.Background(), &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: src,
		SHA256: sha256hex(content), Size: int64(len(content)), Mtime: mt.Unix(),
		State: model.StateHeld, PulledAt: mt.Unix(),
	}); err != nil {
		t.Fatalf("upsert held frame: %v", err)
	}
	return src
}

// TestRunUntilIdlePublished: the card sees the run for exactly as long as it
// lasts, the hub's requested state reads what the operator asked from the moment
// the command is queued, and each end publishes the pill and verdict the end table
// of design frame-lifecycle §12.6 gives.
func TestRunUntilIdlePublished(t *testing.T) {
	// pausedViaSend pauses the rig through Send, as the manager does, so the hub
	// records the request.
	pausedViaSend := func(t *testing.T, h *harness) {
		t.Helper()
		h.w.Send(Pause)
		h.serve(t)
	}
	requested := func(h *harness) model.RequestedState { return h.status().Requested }

	t.Run("the flag stands for the whole run and a run ending by itself settles the request", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		h.keepCaptureDir()
		h.slowLink(slowLinkRead)
		pausedViaSend(t, h)
		rec := h.recordStatuses()

		h.w.Send(RunNow)
		if got := requested(h); got != model.RequestRunUntilIdle || h.status().RunUntilIdle {
			t.Fatalf("queued Run now: requested %q run=%v, want the run requested, not yet running", got, h.status().RunUntilIdle)
		}
		h.serve(t)
		h.runToEnd(t, 10)

		pubs := rec.take()
		first := slices.IndexFunc(pubs, func(p model.RigStatus) bool { return p.RunUntilIdle })
		if first < 0 {
			t.Fatal("the run was never published")
		}
		last := first
		for i, p := range pubs {
			if p.RunUntilIdle {
				last = i
			}
		}
		for i := first; i <= last; i++ {
			if !pubs[i].RunUntilIdle {
				t.Fatalf("publish %d mid-run dropped the flag: %+v", i, pubs[i])
			}
		}
		if end := pubs[last+1]; end.RunUntilIdle || end.State != model.RigStatePaused || end.SafeToPowerOff != model.SafeYes || end.SafeAsOf == 0 {
			t.Fatalf("the end published %+v, want Paused with the final listing's verdict and time", end)
		}
		if h.onDevice(frames) != 0 {
			t.Fatal("precondition: the run should have drained the backlog")
		}
		if got := requested(h); got != model.RequestPaused {
			t.Fatalf("after the run ended by itself the request reads %q, want paused", got)
		}
	})

	t.Run("a run ending in its first cycle settles the request too", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.keepCaptureDir() // an empty device that lists cleanly
		h.cycle()
		pausedViaSend(t, h)
		rec := h.recordStatuses()

		h.w.Send(RunNow)
		h.serve(t)

		if !slices.ContainsFunc(rec.take(), func(p model.RigStatus) bool { return p.RunUntilIdle }) {
			t.Fatal("the run was never published, so the hub could not see it end")
		}
		if h.w.runUntilIdle {
			t.Fatal("precondition: nothing to do, so the run should end in its first cycle")
		}
		if got := requested(h); got != model.RequestPaused {
			t.Fatalf("request %q after a one-cycle run, want paused", got)
		}
	})

	t.Run("stop run reads stopping until it lands, then ends with the final verdict", func(t *testing.T) {
		h, frames := drainingRun(t)
		h.hub.SetRequested("rig-01", model.RequestRunUntilIdle) // as a Run now through Send records
		last := h.status()

		h.w.Send(StopRun)
		if st := h.status(); st.Requested != model.RequestPaused || !st.RunUntilIdle {
			t.Fatalf("queued Stop run: requested %q run=%v, want stopping", st.Requested, st.RunUntilIdle)
		}
		h.serve(t)

		st := assertRunEnded(t, h, model.RigStatePaused, last.SafeToPowerOff, last.PowerOffReason)
		if st.SafeAsOf != last.SafeAsOf || st.Requested != model.RequestPaused {
			t.Fatalf("as of %d requested %q, want the last listing's %d and paused", st.SafeAsOf, st.Requested, last.SafeAsOf)
		}
		if h.onDevice(frames) != 2 {
			t.Fatal("a stopped run must not go on deleting")
		}
	})

	t.Run("pause during a run reads stopping, then ends at the paused verdict", func(t *testing.T) {
		h, _ := drainingRun(t)
		h.hub.SetRequested("rig-01", model.RequestRunUntilIdle)

		h.w.Send(Pause)
		if st := h.status(); st.Requested != model.RequestPaused || !st.RunUntilIdle {
			t.Fatalf("queued Pause: requested %q run=%v, want stopping", st.Requested, st.RunUntilIdle)
		}
		h.serve(t)

		st := assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "paused")
		if st.SafeAsOf != 0 || st.Requested != model.RequestPaused {
			t.Fatalf("as of %d requested %q, want no listing time and paused", st.SafeAsOf, st.Requested)
		}
	})

	t.Run("stop run mid-pull publishes the paused verdict", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		h.addCaptureAt(t, "Autorun/Light/C-2025 A6/Light_NGC_0002.fit")
		pausedViaSend(t, h)
		h.dev.OnOpenReader = func(string, string) { h.w.Send(StopRun) }

		h.w.Send(RunNow)
		h.serve(t)

		st := assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, "paused")
		if st.Requested != model.RequestPaused || st.SafeAsOf != 0 {
			t.Fatalf("requested %q as of %d, want paused with no listing time", st.Requested, st.SafeAsOf)
		}
	})

	t.Run("an offline end publishes Paused with offline during the last run", func(t *testing.T) {
		h, _ := drainingRun(t)
		h.hub.SetRequested("rig-01", model.RequestRunUntilIdle)
		h.dev.SetReachable(false)

		h.runCycleAndWait()

		st := assertRunEnded(t, h, model.RigStatePaused, model.SafeUnknown, reasonOfflineDuringRun)
		if st.SafeAsOf != 0 || st.Requested != model.RequestPaused {
			t.Fatalf("as of %d requested %q, want no listing time and the request settled", st.SafeAsOf, st.Requested)
		}
	})

	t.Run("an identity end publishes Error with the identity verdict", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID = "dev-guid"
		h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		pausedViaSend(t, h)
		h.w.Send(RunNow)
		h.serve(t)
		h.resolver.mu.Lock()
		h.resolver.probeGUID = "someone-else"
		h.resolver.mu.Unlock()

		h.runCycleAndWait()

		st := assertRunEnded(t, h, model.RigStateError, model.SafeUnknown, "identity")
		if st.SafeAsOf != 0 || st.Requested != model.RequestPaused {
			t.Fatalf("as of %d requested %q, want no listing time and the request settled", st.SafeAsOf, st.Requested)
		}
	})

	t.Run("run now and stop run on an unpaused rig leave it running", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.cycle()

		h.w.Send(RunNow)
		if got := requested(h); got != model.RequestRunning {
			t.Fatalf("queued Run now on an unpaused rig requested %q", got)
		}
		h.serve(t)
		h.w.Send(StopRun)
		if got := requested(h); got != model.RequestRunning {
			t.Fatalf("queued Stop run on an unpaused rig requested %q", got)
		}
		h.serve(t)
		if st := h.status(); st.Requested != model.RequestRunning || st.RunUntilIdle || st.Paused {
			t.Fatalf("requested %q run=%v paused=%v, want a running rig with nothing pending", st.Requested, st.RunUntilIdle, st.Paused)
		}
	})

	t.Run("a run ending on its no-progress count settles the request", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)
		pausedViaSend(t, h)

		h.w.Send(RunNow)
		h.serve(t)
		h.runToEnd(t, maxNoProgressCycles)

		if !h.runEndedBecause(`"no progress"`) {
			t.Fatal("precondition: the run should have ended on its no-progress count")
		}
		if got := requested(h); got != model.RequestPaused {
			t.Fatalf("request %q after the run ended on its no-progress count, want paused", got)
		}
	})

	t.Run("pause then run now in one drain: the new run reads run until idle", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		pausedViaSend(t, h)
		h.w.Send(RunNow)
		h.serve(t)
		h.dev.OnOpenReader = func(_, p string) {
			h.nowVal = h.nowVal.Add(slowLinkRead)
			if p == frames[1].SourcePath {
				h.w.Send(Pause)
				h.w.Send(RunNow)
			}
		}

		h.runCycleAndWait()

		if !strings.Contains(h.logBuf.String(), "why=pause") {
			t.Fatal("precondition: the Pause should have ended the first run")
		}
		if st := h.status(); st.Requested != model.RequestRunUntilIdle || !st.RunUntilIdle {
			t.Fatalf("requested %q run=%v, want the new run going and requested, not stopping", st.Requested, st.RunUntilIdle)
		}
	})

	t.Run("stop run then run now across Run's select: the new run reads run until idle", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.seedEligible(t, 3)
		h.keepCaptureDir()
		h.slowLink(slowLinkRead)
		pausedViaSend(t, h)
		h.w.Send(RunNow)
		h.serve(t)
		if !h.w.runUntilIdle {
			t.Fatal("precondition: the first run should still be going")
		}

		h.w.Send(StopRun)
		h.w.Send(RunNow)
		if h.serve(t) {
			t.Fatal("precondition: Stop run runs no cycle")
		}
		if h.w.runUntilIdle {
			t.Fatal("precondition: Stop run, served alone, should have ended the run")
		}
		h.serve(t)

		if st := h.status(); st.Requested != model.RequestRunUntilIdle || !st.RunUntilIdle {
			t.Fatalf("requested %q run=%v, want the new run going and requested, not stopping", st.Requested, st.RunUntilIdle)
		}
	})

	t.Run("run now behind a stop run in one drain requests the run again", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		pausedViaSend(t, h)
		h.w.Send(RunNow)
		h.serve(t)
		h.dev.OnOpenReader = func(_, p string) {
			h.nowVal = h.nowVal.Add(slowLinkRead)
			if p == frames[1].SourcePath {
				h.w.Send(StopRun)
				if got := requested(h); got != model.RequestPaused {
					t.Errorf("queued Stop run requested %q, want paused", got)
				}
				h.w.Send(RunNow)
			}
		}

		h.runCycleAndWait()

		if st := h.status(); st.Requested != model.RequestRunUntilIdle || !st.RunUntilIdle {
			t.Fatalf("requested %q run=%v, want the run going and requested", st.Requested, st.RunUntilIdle)
		}
	})
}

// TestRunNowDuringRunKeepsDeadline drives Run: Run now pressed again and again
// during a run with no continuation pending leaves the run's next cycle at its own
// scheduled time rather than pushing it out a full poll with each press.
func TestRunNowDuringRunKeepsDeadline(t *testing.T) {
	const poll = 2 * time.Second
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.addCapture(t)
	// An unreadable destination keeps the run going for maxNoProgressCycles cycles,
	// each returning a full poll: no continuation is ever pending.
	h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)
	h.cfg.Defaults.PollInterval = model.Duration(poll)
	h.pauseRig()
	cycles := func() int { return strings.Count(h.logBuf.String(), "rig: run until idle progress") }
	// The run starts before Run does, so Run's own first cycle is the run's second
	// and every press is served by Run's select; one sent before Run's startup drain
	// had finished would be served there instead.
	h.runNow()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(5 * time.Second)
	for cycles() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("Run never ran the run's second cycle")
		}
		time.Sleep(time.Millisecond)
	}
	if st := h.status(); !st.RunUntilIdle || st.Draining {
		t.Fatalf("precondition: run=%v draining=%v, want a run with no continuation pending", st.RunUntilIdle, st.Draining)
	}

	// Each press would reset a ticker to a full poll, so presses spaced well inside
	// one poll would hold the next cycle off for as long as they kept coming.
	pressedUntil := time.Now().Add(3 * poll)
	for cycles() < 3 {
		if time.Now().After(pressedUntil) {
			t.Fatalf("no third cycle within %v of pressing Run now every 100 ms; the presses postponed the run", 3*poll)
		}
		h.w.Send(RunNow)
		time.Sleep(100 * time.Millisecond)
	}
	if !h.logContains("run-now during a run until idle") {
		t.Fatal("precondition: the presses should have been served during the run")
	}
}

// TestRunNowQueuedBeforeRunListsOnce drives Run: a Run now queued before Run
// starts is served by the startup drain, which starts the run and runs its first
// cycle; Run's own initial cycle must not follow at once, or two listings would
// land closer together than one poll.
func TestRunNowQueuedBeforeRunListsOnce(t *testing.T) {
	const poll = 2 * time.Second
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.addCapture(t)
	h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)
	h.cfg.Defaults.PollInterval = model.Duration(poll)
	h.pauseRig()
	cycles := func() int { return strings.Count(h.logBuf.String(), "rig: run until idle progress") }
	h.w.Send(RunNow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(5 * time.Second)
	for cycles() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the queued Run now never ran a cycle")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(poll / 2)
	if n := cycles(); n != 1 {
		t.Fatalf("%d cycles within half a poll of the queued Run now, want 1: the initial cycle listed again at once", n)
	}
}
