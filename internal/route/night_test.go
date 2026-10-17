package route

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

func TestSplitAtNight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tmpl   string
		before []string
		night  string
		ok     bool
	}{
		{"shipped light", "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>",
			[]string{"<TARGET>", "<OTA>", "<SESSION>"}, "NIGHT_<n>", true},
		{"shipped mosaic", defaultMosaicTemplate,
			[]string{"<TARGET_BASE>", "<OTA>", "<SESSION>", "<TARGET_BASE>_<PANEL>"}, "NIGHT_<n>", true},
		{"night is not the last segment", "<TARGET>/NIGHT_<n>/<FILTER>",
			[]string{"<TARGET>"}, "NIGHT_<n>", true},
		{"the first <n> wins", "<TARGET>/NIGHT_<n>/N<n>",
			[]string{"<TARGET>"}, "NIGHT_<n>", true},
		{"night is the first segment", "NIGHT_<n>/<TARGET>", nil, "NIGHT_<n>", true},
		{"the whole template is the night", "<TARGET>_NIGHT_<n>", nil, "<TARGET>_NIGHT_<n>", true},
		{"backslash-authored", `<TARGET>\<OTA>\NIGHT_<n>`,
			[]string{"<TARGET>", "<OTA>"}, "NIGHT_<n>", true},
		{"no night segment", "<TARGET>/<OTA>/<SESSION>", nil, "", false},
		// FieldsFunc would drop the empty segment and mis-align the prefix with the
		// segment list Route renders; Split keeps it.
		{"empty segments are kept", "<TARGET>//NIGHT_<n>",
			[]string{"<TARGET>", ""}, "NIGHT_<n>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, night, ok := SplitAtNight(tc.tmpl)
			if ok != tc.ok || night != tc.night {
				t.Fatalf("SplitAtNight = (_, %q, %v); want (_, %q, %v)", night, ok, tc.night, tc.ok)
			}
			if strings.Join(before, "|") != strings.Join(tc.before, "|") {
				t.Fatalf("before = %v, want %v", before, tc.before)
			}
		})
	}
}

// TestNightFolderMatchesRoute is the property the folder consultation rests on:
// the prefix plus the rendered night segment is exactly what Route produces for
// the same frame, so the folder consulted for an ordinal is the folder the frame
// is filed into. It is asserted over the shipped light and mosaic templates, a
// night segment that is not the last one, and a segment carrying a second token.
func TestNightFolderMatchesRoute(t *testing.T) {
	panelCls := lightCls()
	panelCls.Target = "Sadr_1-1"

	for _, tc := range []struct {
		name       string
		tmpl       string
		cls        model.Classification
		detection  bool
		night      int
		wantPrefix string
		wantName   string
	}{
		{"shipped light", "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>", lightCls(), false, 3,
			"NGC7000/FRA300/Summer2026", "NIGHT_3"},
		{"shipped mosaic", defaultMosaicTemplate, panelCls, true, 2,
			"Sadr/FRA300/Summer2026/Sadr_1-1", "NIGHT_2"},
		{"night is not the last segment", "<TARGET>/NIGHT_<n>/<FILTER>", lightCls(), false, 1,
			"NGC7000", "NIGHT_1"},
		{"the night segment names a second token", "<TARGET>/<DATE>_NIGHT_<n>", lightCls(), false, 4,
			"NGC7000", "2026-06-18_NIGHT_4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{
				Templates:       model.Templates{Light: tc.tmpl, Mosaic: tc.tmpl},
				ObservingDate:   ObservingDate(tc.cls.DateObs, time.UTC),
				DateFormat:      "2006-01-02",
				MosaicDetection: tc.detection,
			}
			prefix, nightName, ok, err := NightFolder(tc.cls, testRig(), opts)
			if err != nil || !ok {
				t.Fatalf("NightFolder = (%q, _, %v, %v)", prefix, ok, err)
			}
			if prefix != tc.wantPrefix {
				t.Fatalf("prefix = %q, want %q", prefix, tc.wantPrefix)
			}
			name, nerr := nightName(tc.night)
			if nerr != nil {
				t.Fatalf("nightName(%d): %v", tc.night, nerr)
			}
			if name != tc.wantName {
				t.Fatalf("nightName(%d) = %q, want %q", tc.night, name, tc.wantName)
			}

			// The same inputs through Route must land in that exact folder.
			opts.Night = tc.night
			res, rerr := Route(tc.cls, testRig(), opts)
			if rerr != nil {
				t.Fatalf("Route: %v", rerr)
			}
			if !strings.HasPrefix(res.Dir, prefix+"/"+name) {
				t.Fatalf("Route Dir %q does not start at the consulted folder %q", res.Dir, prefix+"/"+name)
			}
		})
	}
}

// TestNightFolderDisabledCases are the shapes that switch the consultation off
// rather than failing a frame: a template naming no <n>, and one whose prefix
// renders empty — where the range would be the whole light root and every
// target's NIGHT_k a candidate. Filing then behaves as it does with no
// consultation at all.
func TestNightFolderDisabledCases(t *testing.T) {
	for _, tc := range []struct{ name, tmpl string }{
		{"no night segment", "<TARGET>/<OTA>/<SESSION>"},
		{"night is the first segment", "NIGHT_<n>/<TARGET>"},
		{"the whole template is the night segment", "<TARGET>_NIGHT_<n>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cls := lightCls()
			opts := Options{
				Templates:     model.Templates{Light: tc.tmpl},
				ObservingDate: ObservingDate(cls.DateObs, time.UTC),
				DateFormat:    "2006-01-02",
			}
			prefix, _, ok, err := NightFolder(cls, testRig(), opts)
			if err != nil || ok || prefix != "" {
				t.Fatalf("NightFolder = (%q, _, %v, %v); want it disabled with no error", prefix, ok, err)
			}
		})
	}

	// A frame with no applicable template is not a folder question either: Route
	// reports it moments later and the caller quarantines there.
	cls := lightCls()
	if _, _, ok, err := NightFolder(cls, testRig(), Options{}); ok || err != nil {
		t.Fatalf("an unconfigured template = (%v, %v); want it disabled with no error", ok, err)
	}
}

// TestNightFolderQuarantinesOnPrefixToken proves the other arm: a
// routing-critical token in a segment ABOVE <n> returns the same
// *QuarantineError Route derives from the same segment, so a caller can tell it
// apart from a store failure and let routing quarantine the frame.
func TestNightFolderQuarantinesOnPrefixToken(t *testing.T) {
	cls := lightCls()
	cls.SensorMapped = false
	opts := Options{
		Templates:     model.Templates{Light: "<SENSOR>/<TARGET>/NIGHT_<n>"},
		ObservingDate: ObservingDate(cls.DateObs, time.UTC),
		DateFormat:    "2006-01-02",
	}
	_, _, ok, err := NightFolder(cls, testRig(), opts)
	if ok {
		t.Fatal("a quarantining prefix must not report a folder")
	}
	var qe *QuarantineError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v (%T), want a *QuarantineError", err, err)
	}
	if _, rerr := Route(cls, testRig(), opts); !errors.As(rerr, &qe) {
		t.Fatalf("Route must derive the identical quarantine; got %v", rerr)
	}
}
