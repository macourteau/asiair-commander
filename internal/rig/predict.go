package rig

import (
	"sort"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// predictSampleSize (N) is the count of most-recent light captures whose
// consecutive gaps seed the cadence median. Kept small so a single dither /
// autofocus / meridian-flip gap is a minority the median rejects (N=5 → up to 4
// gaps). Tunable in one place.
const predictSampleSize = 5

// predictStatePredicting is the only FramePrediction.State the backend emits: the
// Unknown state is represented by an absent prediction (a nil pointer), never a
// serialized "unknown".
const predictStatePredicting = "predicting"

// predictFrames derives the next-frame cadence prediction from tonight's recent
// light captures. captures are the DEVICE-domain capture times of the most recent
// lights in ascending order (at most predictSampleSize entries); lastExposureSec is
// the exposure of the newest capture; now is host wall time; offset is the
// per-session device-minus-host clock offset (clock.Clock.Offset()).
//
// It is pure — no device, store, or SMB dependency — so the cadence math and the
// liveness gate are unit-testable without a rig. It returns nil (→ Unknown; the
// caller leaves RigStatus.Prediction unset) when there are no captures, when the
// gaps yield no usable interval, or when the newest capture is already outside the
// liveness window. The window and the ETA anchor are both evaluated in the HOST
// domain by correcting the device capture time with -offset, exactly as
// clock.Clock.YoungerThan corrects a device timestamp.
func predictFrames(captures []time.Time, lastExposureSec float64, now time.Time, offset time.Duration) *model.FramePrediction {
	if len(captures) == 0 {
		return nil
	}
	// The newest capture in the host's clock domain (subtract the offset once, the
	// same transform the min-file-age check uses).
	lastHost := captures[len(captures)-1].Add(-offset)

	// Capture-time liveness gate (design features §5): predict only while the newest
	// capture is recent. This is measured from CAPTURE time, never arrival/filed
	// time, so a backfill of historical frames — whose newest capture is old even
	// though the row was just filed — never predicts, with no explicit backfill
	// detection. A live session that briefly fell behind and is catching up keeps
	// recent captures, so it correctly stays Predicting.
	if now.Sub(lastHost) > livenessWindow(lastExposureSec) {
		return nil
	}

	interval, samples := cadenceInterval(captures, lastExposureSec)
	if interval <= 0 {
		return nil
	}

	// Anchor "next expected" at the device capture time plus the interval, then
	// correct to host domain so the browser counts down against its own clock.
	// Overdue is normal (overhead ran long); only the liveness window above flips
	// the state to Unknown.
	nextHost := lastHost.Add(interval)
	return &model.FramePrediction{
		State:           predictStatePredicting,
		IntervalSec:     interval.Seconds(),
		LastExposureSec: lastExposureSec,
		NextExpectedTS:  nextHost.Unix(),
		SampleCount:     samples,
	}
}

// cadenceInterval returns the predicted capture-to-capture interval and the number
// of captures it rests on. With ≥2 captures it is the MEDIAN of the consecutive
// gaps (device-domain, so the clock offset cancels; robust to a single outlier gap
// from a dither / autofocus / flip). With exactly one capture it falls back to the
// exposure — a low-confidence guess the SampleCount==1 caller renders softly.
func cadenceInterval(captures []time.Time, lastExposureSec float64) (interval time.Duration, samples int) {
	n := len(captures)
	if n == 1 {
		if lastExposureSec <= 0 {
			return 0, 1
		}
		return time.Duration(lastExposureSec * float64(time.Second)), 1
	}
	gaps := make([]time.Duration, 0, n-1)
	for i := 1; i < n; i++ {
		if g := captures[i].Sub(captures[i-1]); g > 0 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0, n
	}
	return medianDuration(gaps), n
}

// medianDuration returns the median of ds without mutating it. On an even count it
// averages the two central values.
func medianDuration(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// livenessWindow is the max(5×lastExposure, 1h) span past the newest capture within
// which the sequence is treated as live (design features §5.1/§5). It is anchored on
// the stable exposure, not the measured interval, because a legitimate long
// flip/autofocus gap would inflate a measured interval to something absurd and keep
// a dead prediction alive for hours. The 1h floor dominates for typical exposures;
// only a very long exposure (>720s) lets 5×lastExposure win.
func livenessWindow(lastExposureSec float64) time.Duration {
	if w := time.Duration(lastExposureSec*5) * time.Second; w > time.Hour {
		return w
	}
	return time.Hour
}

// frameCaptureTime returns a frame's DEVICE-domain capture time: DATE-OBS parsed
// (the true capture start), falling back to the device file mtime when DATE-OBS is
// absent or unparseable. It never uses PulledAt (the host-domain filed time), which
// a backlog burst would collapse into a false fast cadence.
func frameCaptureTime(f model.Frame) time.Time {
	if f.DateObs != "" {
		// DateObs is stored as RFC3339Nano UTC (see recordFiled); RFC3339 also parses
		// the whole-second form.
		if t, err := time.Parse(time.RFC3339Nano, f.DateObs); err == nil {
			return t.UTC()
		}
	}
	return time.Unix(f.Mtime, 0).UTC()
}
