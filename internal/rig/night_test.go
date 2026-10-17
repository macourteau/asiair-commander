package rig

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/route"
)

// TestFolderRangeExcludesSiblings pins the half-open dest_path range: everything
// filed at or below the folder is inside it, and a sibling whose name merely
// starts with the same characters is outside. The upper bound is the successor
// of the trailing separator, which is what makes "M31_old" — "_" (0x5F) sorting
// above "0" (0x30) — fall out.
func TestFolderRangeExcludesSiblings(t *testing.T) {
	sep := string(filepath.Separator)
	base := filepath.Join("nas", "light", "M31")
	lo, hi := folderRange(base)

	if lo != base+sep {
		t.Fatalf("lo=%q, want the folder plus a separator", lo)
	}
	for _, in := range []string{
		filepath.Join(base, "Light_0001.fit"),
		filepath.Join(base, "NIGHT_1", "Light_0001.fit"),
		filepath.Join(base, "NIGHT_1", "HA", "Light_0001.fit"),
	} {
		if !(in >= lo && in < hi) {
			t.Errorf("%q is filed under %q but falls outside [%q,%q)", in, base, lo, hi)
		}
	}
	for _, out := range []string{
		filepath.Join("nas", "light", "M31_old", "Light_0001.fit"),
		filepath.Join("nas", "light", "M310", "Light_0001.fit"),
		filepath.Join("nas", "light", "M3", "Light_0001.fit"),
		filepath.Join("nas", "light", "NIGHT_1", "Light_0001.fit"),
	} {
		if out >= lo && out < hi {
			t.Errorf("%q is not under %q but falls inside [%q,%q)", out, base, lo, hi)
		}
	}

	// A root that already ends in a separator (a drive or UNC root) must not gain
	// a second one, which would match nothing at all.
	if lo, _ := folderRange("C:" + sep); lo != "C:"+sep {
		t.Errorf("folderRange doubled the separator on a drive root: %q", lo)
	}
}

// TestObservingWindowInvertsObservingDate is the property the folder read rests
// on: a frame's stored date_obs falls inside the window of observing date D if
// and only if route.ObservingDate would bucket that frame into D. It is asserted
// across a spring-forward day (23 hours), a fall-back day (25 hours) and an
// ordinary one, because a window built as an absolute 24 hours from its lower
// bound is wrong at one end of each transition — pulling the next date's first
// hour in, or dropping an hour of this date's own frames, either of which skews
// the frame counts an adoption is decided by.
//
// Fractional and whole-second timestamps are both walked: RFC3339Nano drops the
// fractional part at zero nanoseconds, so date_obs holds both spellings and the
// bounds have to bracket them alike.
func TestObservingWindowInvertsObservingDate(t *testing.T) {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Skipf("America/Toronto unavailable: %v", err)
	}
	for _, day := range []struct {
		name string
		y    int
		m    time.Month
		d    int
	}{
		{"spring forward", 2026, time.March, 8},
		{"fall back", 2026, time.November, 1},
		{"ordinary day", 2026, time.June, 15},
	} {
		t.Run(day.name, func(t *testing.T) {
			date := time.Date(day.y, day.m, day.d, 0, 0, 0, 0, loc)
			lo, hi := observingWindow(date)
			if strings.HasSuffix(lo, "Z") || strings.HasSuffix(hi, "Z") {
				t.Fatalf("bounds must be truncated, not zone-suffixed: %q..%q", lo, hi)
			}
			// From six hours before the window opens to six after it closes.
			for step := 0; step <= 4*36; step++ {
				inst := date.Add(6*time.Hour + time.Duration(step)*15*time.Minute)
				for _, frac := range []time.Duration{0, 31444 * time.Microsecond} {
					captured := inst.Add(frac)
					stored := captured.UTC().Format(time.RFC3339Nano)
					inWindow := stored >= lo && stored < hi
					want := route.ObservingDate(captured, loc).Equal(date)
					if inWindow != want {
						t.Fatalf("date_obs %q: in window %v, but ObservingDate says %v (window %q..%q)",
							stored, inWindow, want, lo, hi)
					}
				}
			}
		})
	}
}
