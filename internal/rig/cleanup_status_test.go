package rig

import (
	"path"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// Each delete re-reads the whole device original, so on a slow link a pass over a
// large backlog runs for many minutes. It must publish where it is, per frame, or
// the card reads as stalled for the whole pass — and drop that position once the
// pass ends so a finished pass never shows as still running.
func TestDeletePassPublishesProgressThenClears(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	rec := &recordingHub{StatusHub: h.hub}
	h.w.hub = rec

	h.cycle() // runs the delete pass; the frame is eligible

	if h.deviceHas(f.SourcePath) {
		t.Fatal("precondition: the delete pass should have removed the proven original")
	}
	rec.mu.Lock()
	var saw *model.CleanupProgress
	for _, c := range rec.cleanups {
		if c != nil {
			saw = c
		}
	}
	rec.mu.Unlock()
	want := model.CleanupProgress{File: path.Base(f.SourcePath), Index: 1, Total: 1}
	if saw == nil || *saw != want {
		t.Fatalf("delete pass published cleanup %+v, want %+v", saw, want)
	}
	if got := h.status().Cleanup; got != nil {
		t.Fatalf("cleanup %+v still published after the pass ended", got)
	}
}

// A pause is applied only between files, which on a slow link can be a minute or
// more away. Until then the card must say the pause is on its way, or a second
// click just queues a second pause and the operator concludes nothing happened:
// the requested state reads paused while the published state is not yet.
func TestPauseIsPendingUntilApplied(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cycle()
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit",
		fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))

	h.w.Send(Pause) // queued, not yet applied: the worker is between cycles

	st := h.status()
	if st.Requested != model.RequestPaused || st.Paused {
		t.Fatalf("queued pause: requested=%q paused=%v, want paused requested and not yet applied",
			st.Requested, st.Paused)
	}

	h.cycle() // drains the pause between files, before the frame is pulled

	st = h.status()
	if st.Requested != model.RequestPaused || !st.Paused {
		t.Fatalf("applied pause: requested=%q paused=%v, want requested and current to agree",
			st.Requested, st.Paused)
	}
}

// Resuming while the pause is still queued is how the operator cancels it; the
// card must stop saying "pausing" at once, not after the worker catches up.
func TestResumeWithdrawsPendingPause(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cycle()

	h.w.Send(Pause)
	h.w.Send(Resume)

	if got := h.status().Requested; got != model.RequestRunning {
		t.Fatalf("a resume sent behind a queued pause left the request at %q", got)
	}

	h.cycle() // drains both
	if st := h.status(); st.Paused || st.Requested != model.RequestRunning {
		t.Fatalf("after draining pause+resume: paused=%v requested=%q, want running and not paused",
			st.Paused, st.Requested)
	}
}

// A command the full channel drops is never applied, so it must record nothing: a
// dropped pause recorded as requested would read "pausing" for good.
func TestDroppedCommandRecordsNothing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cycle()
	for range cap(h.w.cmds) {
		h.w.Send(ReloadConfig)
	}

	h.w.Send(Pause) // the channel is full: dropped

	if got := h.status().Requested; got != model.RequestRunning {
		t.Fatalf("a dropped pause recorded the request %q", got)
	}
}
