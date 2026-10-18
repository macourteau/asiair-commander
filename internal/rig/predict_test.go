package rig

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// predBase is a fixed host "now" the pure-predictor cases build their capture
// times around. Its value is irrelevant; only the offsets from it matter.
var predBase = time.Date(2026, 6, 1, 22, 0, 0, 0, time.UTC)

// caps builds an ascending capture slice at the given offsets from predBase.
func caps(offsets ...time.Duration) []time.Time {
	out := make([]time.Time, len(offsets))
	for i, d := range offsets {
		out[i] = predBase.Add(d)
	}
	return out
}

func TestPredictFrames(t *testing.T) {
	const s = time.Second
	cases := []struct {
		name       string
		captures   []time.Time
		exposure   float64
		offset     time.Duration
		wantNil    bool
		wantIntSec float64
		wantNextff time.Duration // NextExpectedTS relative to predBase
		wantSample int
	}{
		{
			// A steady sequence: median of four 60s gaps, newest captured 30s ago.
			name:       "steady five frames",
			captures:   caps(-270*s, -210*s, -150*s, -90*s, -30*s),
			exposure:   55,
			wantIntSec: 60,
			wantNextff: 85 * s, // last (-30s) + 60s interval + 55s exposure
			wantSample: 5,
		},
		{
			// A single dither/autofocus gap (600s) is outvoted by the median.
			name:       "median rejects one outlier gap",
			captures:   caps(-780*s, -720*s, -660*s, -60*s), // gaps 60,60,600 → median 60
			exposure:   55,
			wantIntSec: 60,
			wantNextff: 55 * s, // -60s + 60s + 55s
			wantSample: 4,
		},
		{
			name:       "two frames use the single gap",
			captures:   caps(-120*s, -30*s), // one 90s gap
			exposure:   55,
			wantIntSec: 90,
			wantNextff: 115 * s, // -30s + 90s + 55s
			wantSample: 2,
		},
		{
			// One frame: low-confidence exposure fallback, still Predicting.
			name:       "single frame falls back to exposure",
			captures:   caps(-20 * s),
			exposure:   300,
			wantIntSec: 300,
			wantNextff: 580 * s, // -20s + 300s + 300s
			wantSample: 1,
		},
		{
			name:     "single frame without exposure is unknown",
			captures: caps(-20 * s),
			exposure: 0,
			wantNil:  true,
		},
		{
			name:     "no frames is unknown",
			captures: nil,
			wantNil:  true,
		},
		{
			// Backlog / expiry: the newest capture is 2h old (window max(5×60,1h)=1h).
			name:     "old newest capture expires to unknown",
			captures: caps(-2*time.Hour-60*s, -2*time.Hour),
			exposure: 60,
			wantNil:  true,
		},
		{
			// A long exposure widens the window to 5×800s=4000s, so a 3000s-old newest
			// capture still predicts.
			name:       "long exposure widens liveness window",
			captures:   caps(-3060*s, -3000*s),
			exposure:   800,
			wantIntSec: 60,
			wantNextff: -2140 * s, // -3000s + 60s + 800s, overdue but live
			wantSample: 2,
		},
		{
			name:     "long exposure still expires past its window",
			captures: caps(-4061*s, -4001*s),
			exposure: 800, // window 4000s; newest 4001s old
			wantNil:  true,
		},
		{
			// Overdue is normal, not expiry: ETA is in the past yet still Predicting.
			name:       "overdue keeps predicting",
			captures:   caps(-235*s, -145*s), // 90s gap, newest 145s ago
			exposure:   55,
			wantIntSec: 90,
			wantNextff: 0, // -145s + 90s + 55s = predBase exactly (due now)
			wantSample: 2,
		},
		{
			// Mixed exposures blend: gaps 300,60,300,60 → median of {60,60,300,300}=180.
			name:       "mixed exposures blend to the median",
			captures:   caps(-720*s, -420*s, -360*s, -60*s, 0),
			exposure:   60,
			wantIntSec: 180,
			wantNextff: 240 * s, // newest at predBase + 180s + 60s
			wantSample: 5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := predictFrames(tc.captures, tc.exposure, predBase, tc.offset)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("predictFrames = %+v, want nil (Unknown)", got)
				}
				return
			}
			if got == nil {
				t.Fatal("predictFrames = nil, want a prediction")
			}
			if got.State != predictStatePredicting {
				t.Errorf("State = %q, want %q", got.State, predictStatePredicting)
			}
			if got.IntervalSec != tc.wantIntSec {
				t.Errorf("IntervalSec = %v, want %v", got.IntervalSec, tc.wantIntSec)
			}
			if got.SampleCount != tc.wantSample {
				t.Errorf("SampleCount = %d, want %d", got.SampleCount, tc.wantSample)
			}
			wantTS := predBase.Add(tc.wantNextff).Unix()
			if got.NextExpectedTS != wantTS {
				t.Errorf("NextExpectedTS = %d, want %d (Δ %ds)", got.NextExpectedTS, wantTS, got.NextExpectedTS-wantTS)
			}
			if got.LastExposureSec != tc.exposure {
				t.Errorf("LastExposureSec = %v, want %v", got.LastExposureSec, tc.exposure)
			}
		})
	}
}

// The ETA is when the next frame's FILE should land, not when its exposure starts.
// DATE-OBS is the exposure start, so the newest capture's file appeared about one
// exposure after it, and the next exposure began one interval after it; its file
// lands one exposure later still. Anchoring at capture + interval reads "due now"
// the moment a file lands and "overdue" for the whole of the next exposure.
func TestPredictFramesETAIsNextFileArrival(t *testing.T) {
	const s = time.Second
	// 300s exposures every 310s. The newest exposure started 310s ago, so its file
	// landed about 10s ago and the next exposure started just now.
	got := predictFrames(caps(-930*s, -620*s, -310*s), 300, predBase, 0)
	if got == nil {
		t.Fatal("predictFrames = nil, want a prediction")
	}
	if want := predBase.Add(300 * s).Unix(); got.NextExpectedTS != want {
		t.Fatalf("NextExpectedTS = %+ds from now, want +300s (next exposure started now, its file lands one exposure later)",
			got.NextExpectedTS-predBase.Unix())
	}
}

// The liveness window never ends a prediction before its ETA: a cadence slower than
// the exposure-based window (here 3700s gaps against max(5×60s, 1h)) keeps predicting
// until capture + interval + exposure, and expires once past both.
func TestPredictFramesLivenessCoversETA(t *testing.T) {
	const s = time.Second
	// Newest capture 3650s ago: past the 1h window, before the 3760s ETA lead.
	got := predictFrames(caps(-7350*s, -3650*s), 60, predBase, 0)
	if got == nil {
		t.Fatal("predictFrames = nil before the ETA, want a prediction")
	}
	if want := predBase.Add(110 * s).Unix(); got.NextExpectedTS != want {
		t.Errorf("NextExpectedTS = %+ds from now, want +110s", got.NextExpectedTS-predBase.Unix())
	}
	// Newest capture 3761s ago: past the window and the ETA.
	if got := predictFrames(caps(-7461*s, -3761*s), 60, predBase, 0); got != nil {
		t.Errorf("predictFrames = %+v past both the window and the ETA, want nil", got)
	}
}

// The gaps are device-domain, so a non-zero device clock offset cancels out of the
// interval; only the ETA anchor is corrected by -offset, once, into host domain.
func TestPredictFramesOffsetCancellation(t *testing.T) {
	const s = time.Second
	offset := 5 * time.Minute // device clock runs 5 min ahead of the host
	// Device-domain captures: newest is predBase+offset in device time, i.e. exactly
	// predBase in host time. 60s gaps.
	dev := func(d time.Duration) time.Time { return predBase.Add(offset).Add(d) }
	captures := []time.Time{dev(-180 * s), dev(-120 * s), dev(-60 * s), dev(0)}

	got := predictFrames(captures, 55, predBase, offset)
	if got == nil {
		t.Fatal("predictFrames = nil, want a prediction (newest capture is now in host domain)")
	}
	if got.IntervalSec != 60 {
		t.Errorf("IntervalSec = %v, want 60 (offset must cancel in the gaps)", got.IntervalSec)
	}
	// Host-domain ETA: newest host capture (predBase) + 60s interval + 55s exposure.
	if want := predBase.Add(115 * s).Unix(); got.NextExpectedTS != want {
		t.Errorf("NextExpectedTS = %d, want %d (anchor corrected by -offset)", got.NextExpectedTS, want)
	}
}

// frameCaptureTime prefers DATE-OBS, falls back to the device mtime, and NEVER
// consults PulledAt — the filed time a backlog burst would collapse.
func TestFrameCaptureTime(t *testing.T) {
	obs := time.Date(2026, 6, 1, 3, 15, 0, 0, time.UTC)
	mt := time.Date(2026, 6, 1, 3, 16, 0, 0, time.UTC)
	filed := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC) // hours later, must be ignored

	t.Run("date-obs preferred", func(t *testing.T) {
		f := model.Frame{DateObs: obs.Format(time.RFC3339Nano), Mtime: mt.Unix(), PulledAt: filed.Unix()}
		if got := frameCaptureTime(f); !got.Equal(obs) {
			t.Fatalf("captureTime = %s, want DATE-OBS %s", got, obs)
		}
	})
	t.Run("mtime fallback when date-obs absent", func(t *testing.T) {
		f := model.Frame{DateObs: "", Mtime: mt.Unix(), PulledAt: filed.Unix()}
		if got := frameCaptureTime(f); got.Unix() != mt.Unix() {
			t.Fatalf("captureTime = %s, want mtime %s", got, mt)
		}
	})
	t.Run("mtime fallback when date-obs unparseable", func(t *testing.T) {
		f := model.Frame{DateObs: "not-a-time", Mtime: mt.Unix(), PulledAt: filed.Unix()}
		if got := frameCaptureTime(f); got.Unix() != mt.Unix() {
			t.Fatalf("captureTime = %s, want mtime %s", got, mt)
		}
	})
}

// light builds a stored light-frame row with a given capture time (DATE-OBS) and
// filed time, for the sourcing-level tests below.
func light(dest string, capture time.Time, filedAt int64, exp float64) *model.Frame {
	return &model.Frame{
		RigID: "rig-01",
		// The row's uniqueness key is (rig_id, source_share, source_path); default it
		// off the dest so each distinct capture is a distinct row. The H12 test
		// overrides these to file one dest_path under two (share, path) keys.
		SourceShare: emmc,
		SourcePath:  dest,
		FrameType:   string(model.TypeLight),
		Exptime:     exp,
		DateObs:     capture.UTC().Format(time.RFC3339Nano),
		Mtime:       capture.Unix(),
		DestPath:    dest,
		State:       model.StateVerified,
		PulledAt:    filedAt,
	}
}

// framePrediction sources tonight's lights and predicts. A live session with
// recent captures predicts; the count reflects distinct captures after H12 dedup.
func TestFramePredictionLiveSession(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	now := h.nowVal
	since := now.Add(-6 * time.Hour).Unix()
	for i, off := range []time.Duration{-180, -120, -60, -1} { // seconds ago
		f := light(filepath.Join("Light", "m31", "f"+string(rune('a'+i))+".fit"), now.Add(off*time.Second), now.Unix(), 55)
		if err := h.store.UpsertFrame(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	got := h.w.framePrediction(context.Background(), since, 0)
	if got == nil {
		t.Fatal("framePrediction = nil, want a live prediction")
	}
	if got.SampleCount != 4 {
		t.Errorf("SampleCount = %d, want 4", got.SampleCount)
	}
	if got.State != predictStatePredicting {
		t.Errorf("State = %q, want predicting", got.State)
	}
}

// An H12 frame lands on two shares as two rows with one dest_path; the predictor
// dedupes by dest_path so it counts one capture, exactly as CountFramesTonight.
func TestFramePredictionDedupesH12(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	now := h.nowVal
	since := now.Add(-6 * time.Hour).Unix()
	dest := filepath.Join("Light", "m31", "dup.fit")
	// Same dest_path filed twice (USB + eMMC), plus one distinct earlier capture.
	if err := h.store.UpsertFrame(context.Background(), light(filepath.Join("Light", "m31", "first.fit"), now.Add(-90*time.Second), now.Unix(), 55)); err != nil {
		t.Fatal(err)
	}
	dupA := light(dest, now.Add(-30*time.Second), now.Unix(), 55)
	dupA.SourceShare = "USB"
	if err := h.store.UpsertFrame(context.Background(), dupA); err != nil {
		t.Fatal(err)
	}
	dupB := light(dest, now.Add(-30*time.Second), now.Unix(), 55)
	dupB.SourceShare = "EMMC"
	dupB.SourcePath = "other/path.fit" // distinct source row, same dest
	if err := h.store.UpsertFrame(context.Background(), dupB); err != nil {
		t.Fatal(err)
	}
	got := h.w.framePrediction(context.Background(), since, 0)
	if got == nil {
		t.Fatal("framePrediction = nil, want a prediction")
	}
	if got.SampleCount != 2 {
		t.Errorf("SampleCount = %d, want 2 (H12 dest_path deduped to one capture)", got.SampleCount)
	}
}

// A backlog of historical frames filed just now (recent PulledAt, OLD capture time)
// must NOT predict: the newest capture is outside the liveness window even though
// the rows were filed moments ago. This is the capture-vs-filed distinction.
func TestFramePredictionBacklogBurstStaysUnknown(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	now := h.nowVal
	since := now.Add(-24 * time.Hour).Unix() // wide enough that the session filter passes
	// Captured across last night (spread by real 5-min gaps), all filed NOW.
	base := now.Add(-8 * time.Hour)
	for i := 0; i < 5; i++ {
		f := light(filepath.Join("Light", "m31", "b"+string(rune('a'+i))+".fit"), base.Add(time.Duration(i)*5*time.Minute), now.Unix(), 60)
		if err := h.store.UpsertFrame(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.w.framePrediction(context.Background(), since, 0); got != nil {
		t.Fatalf("framePrediction = %+v, want nil — a backlog burst (old captures, just filed) must not predict", got)
	}
}

// Session rollover: lights captured before the local-noon boundary are excluded,
// so last night's frames never seed tonight's prediction.
func TestFramePredictionSessionRollover(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	now := h.nowVal
	since := now.Add(-2 * time.Hour).Unix() // "tonight" opened 2h ago
	// Two lights from before the boundary (last night) — recent-ish but pre-since.
	if err := h.store.UpsertFrame(context.Background(), light(filepath.Join("Light", "old", "a.fit"), now.Add(-3*time.Hour), now.Unix(), 55)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertFrame(context.Background(), light(filepath.Join("Light", "old", "b.fit"), now.Add(-3*time.Hour).Add(60*time.Second), now.Unix(), 55)); err != nil {
		t.Fatal(err)
	}
	if got := h.w.framePrediction(context.Background(), since, 0); got != nil {
		t.Fatalf("framePrediction = %+v, want nil — pre-noon frames must not seed tonight", got)
	}
}
