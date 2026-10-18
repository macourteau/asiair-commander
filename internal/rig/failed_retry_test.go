package rig

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// A failed frame that used up its attempt budget is left alone by scheduled
// cycles, gets exactly one more attempt per Run now, and files normally once an
// attempt succeeds (design frame-lifecycle §4.3).
func TestRunNowRetriesExhaustedFailedFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.cfg.Defaults.FailedRetry.MaxAttempts = 2
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(emmc, src, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.dev.FailOpen(emmc, errors.New("access denied (test)"))

	// attemptsIn runs fn and counts the copy attempts it made: each one that fails
	// emits exactly one frame.failed.
	attemptsIn := func(fn func()) int {
		n := 0
		for _, e := range h.captureEvents(fn) {
			if e.Name == "frame.failed" {
				n++
			}
		}
		return n
	}
	// scheduled advances the clock past any backoff and runs a scheduled cycle.
	scheduled := func() {
		h.nowVal = h.nowVal.Add(2 * time.Hour)
		h.cycle()
	}
	runNow := func() {
		h.nowVal = h.nowVal.Add(time.Minute)
		h.w.apply(ctx, RunNow)
	}

	if got := attemptsIn(func() { scheduled(); scheduled() }); got != 2 {
		t.Fatalf("scheduled cycles made %d attempts, want 2 (the budget)", got)
	}
	f, ok := h.getFrame(src)
	if !ok || f.State != model.StateFailed || f.Attempts != 2 {
		t.Fatalf("frame after the budget = %+v, want failed with 2 attempts", f)
	}
	if f.NextRetryAt != 0 {
		t.Fatalf("exhausted frame records next_retry_at %d, want none: only Run now retries it", f.NextRetryAt)
	}
	if got := attemptsIn(func() { scheduled(); scheduled(); scheduled() }); got != 0 {
		t.Fatalf("scheduled cycles retried an exhausted frame %d times, want 0", got)
	}

	// Each Run now grants one attempt; a frame that keeps failing does not loop.
	for press := 1; press <= 2; press++ {
		if got := attemptsIn(runNow); got != 1 {
			t.Fatalf("Run now #%d made %d attempts, want 1", press, got)
		}
		if got := attemptsIn(func() { scheduled(); scheduled() }); got != 0 {
			t.Fatalf("scheduled cycles after Run now #%d made %d attempts, want 0", press, got)
		}
		if f, _ := h.getFrame(src); f.State != model.StateFailed || f.Attempts != 2 {
			t.Fatalf("frame after Run now #%d = %+v, want failed with 2 attempts", press, f)
		}
	}

	// The cause is fixed; the next Run now files the frame normally.
	h.dev.FailOpen(emmc, nil)
	runNow()
	if f, _ := h.getFrame(src); f.State != model.StateVerified {
		t.Fatalf("frame after a successful Run now retry = %+v, want verified", f)
	}
}

// A frame still inside its budget keeps its backoff on Run now: only an
// exhausted frame is granted an attempt, so Run now never shortens the retry
// schedule of a frame that is still retrying on its own.
func TestRunNowLeavesFailedFrameWithAttemptsLeftAlone(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		Size: 10, Mtime: h.nowVal.Unix(), State: model.StateFailed, Attempts: 1,
		NextRetryAt: h.nowVal.Add(time.Hour).Unix()}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	h.w.grantFailedRetry(ctx)
	got, _ := h.getFrame("Autorun/x.fit")
	if got.Attempts != 1 || got.NextRetryAt != f.NextRetryAt {
		t.Fatalf("frame with attempts left = %+v, want attempts 1 and its backoff kept", got)
	}
}

// The grant outlives the Run now cycle: with settle_cycles = 1 an exhausted
// frame (not a pull candidate, so not observed by the settle tracker) is first
// sighted on the Run now cycle and only copied on the next one, which must still
// make the attempt.
func TestRunNowGrantSurvivesTheSettleGate(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.cfg.Defaults.SettleCycles = 1
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(emmc, src, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	if err := h.store.UpsertFrame(ctx, &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: src,
		Size: int64(len(fixture(t, "asiair_light_2600mm_ha.fit"))), Mtime: h.nowVal.Add(-10 * time.Minute).Unix(),
		State: model.StateFailed, Attempts: h.cfg.Defaults.FailedRetry.MaxAttempts}); err != nil {
		t.Fatal(err)
	}

	h.w.apply(ctx, RunNow)
	if f, _ := h.getFrame(src); f.State != model.StateFailed {
		t.Fatalf("frame after the Run now cycle = %+v, want still failed (settling)", f)
	}
	h.cycle()
	if f, _ := h.getFrame(src); f.State != model.StateVerified {
		t.Fatalf("frame after the next cycle = %+v, want verified", f)
	}
}
