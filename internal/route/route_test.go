package route

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// defaultTemplates are the shipped defaults (design
// routing-and-filing §3). The golden end-to-end paths are anchored to these
// exact strings.
func defaultTemplates() model.Templates {
	return model.Templates{
		Light: "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>",
		Cal: map[model.FrameType]string{
			model.TypeDark: "<SENSOR>/DARK/<GAIN>/<BINNING>/<SET-TEMP>/<EXPOSURE>/<DATE>",
			model.TypeFlat: "<SENSOR>/FLAT/<OTA>/<GAIN>/<BINNING>/<DATE>",
			model.TypeBias: "<SENSOR>/BIAS/<GAIN>/<BINNING>/<SET-TEMP>/<DATE>",
		},
	}
}

func testRig() model.RigProfile {
	return model.RigProfile{ID: "rig-01", OTA: "FRA300", Session: "Summer2026"}
}

var aliasTable = map[string]string{
	"light": "LIGHT", "dark": "DARK", "flat": "FLAT", "bias": "BIAS",
	"Dark Frame": "DARK", "Bias Frame": "BIAS",
}

var sensorMap = map[string]string{
	"ZWO ASI6200MM Pro": "6200MM",
	"ZWO ASI2600MM Pro": "2600MM",
	"ZWO ASI294MC Pro":  "294MC",
}

// TestRouteFixtures runs each committed FITS fixture through the real pipeline
// header → classify → Route and asserts the exact destination directory.
func TestRouteFixtures(t *testing.T) {
	cases := []struct {
		name              string
		fixture           string
		night             int
		stripTargetSpaces bool
		wantKind          RootKind
		wantDir           string
	}{
		{
			name:              "light_2600mm",
			fixture:           "asiair_light_2600mm_ha.fit",
			night:             2,
			stripTargetSpaces: true,
			wantKind:          RootLight,
			wantDir:           "C-2025A6/FRA300/Summer2026/NIGHT_2",
		},
		{
			name:     "dark_6200mm",
			fixture:  "asiair_dark_6200mm.fit",
			wantKind: RootCal,
			// SET-TEMP 0, EXPTIME 300, gain 100, bin1, DATE-OBS 2026-06-19T02:07:50Z → 2026-06-18
			wantDir: "6200MM/DARK/gain100/Bin1/0C/300s/2026-06-18",
		},
		{
			name:     "bias_6200mm",
			fixture:  "asiair_bias_6200mm.fit",
			wantKind: RootCal,
			// SET-TEMP -10, gain 100, bin1, DATE-OBS 2026-06-19T01:02Z → 2026-06-18
			wantDir: "6200MM/BIAS/gain100/Bin1/-10C/2026-06-18",
		},
		{
			name:     "flat_6200mm_ha",
			fixture:  "asiair_flat_6200mm_ha.fit",
			wantKind: RootCal,
			// gain 100, bin1, OTA FRA300, DATE-OBS 2026-07-15T04:02Z → 2026-07-14
			wantDir: "6200MM/FLAT/FRA300/gain100/Bin1/2026-07-14",
		},
	}

	r := header.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := r.ReadHeader("../header/testdata/" + tc.fixture)
			if err != nil {
				t.Fatalf("ReadHeader: %v", err)
			}
			cls := classify.Classify(h, aliasTable, sensorMap)
			opts := Options{
				Templates:         defaultTemplates(),
				Night:             tc.night,
				ObservingDate:     ObservingDate(cls.DateObs, time.UTC),
				DateFormat:        "2006-01-02",
				StripTargetSpaces: tc.stripTargetSpaces,
			}
			got, err := Route(cls, testRig(), opts)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.Dir != tc.wantDir {
				t.Errorf("Dir  = %q\nwant   %q", got.Dir, tc.wantDir)
			}
		})
	}
}

// lightCls is a minimal valid LIGHT classification for token-level tests.
func lightCls() model.Classification {
	return model.Classification{
		Type:           model.TypeLight,
		Target:         "NGC7000",
		TargetPresent:  true,
		DateObs:        time.Date(2026, 6, 18, 22, 0, 0, 0, time.UTC),
		DateObsPresent: true,
	}
}

// A "\"-authored template (an operator on Windows) must split into the same
// segments — and route to the identical Dir — as its "/"-authored twin. The
// router always emits a "/"-joined Dir; joinLocal converts it to the host
// separator later.
func TestRouteAcceptsBackslashTemplate(t *testing.T) {
	cls := lightCls()
	base := Options{
		Templates:     model.Templates{Light: "<TARGET>/<OTA>/NIGHT_<n>"},
		Night:         3,
		ObservingDate: ObservingDate(cls.DateObs, time.UTC),
		DateFormat:    "2006-01-02",
	}
	fwd, err := Route(cls, testRig(), base)
	if err != nil {
		t.Fatalf("forward-slash Route: %v", err)
	}

	back := base
	back.Templates = model.Templates{Light: `<TARGET>\<OTA>\NIGHT_<n>`}
	got, err := Route(cls, testRig(), back)
	if err != nil {
		t.Fatalf("backslash Route: %v", err)
	}
	if got.Dir != fwd.Dir {
		t.Errorf("backslash Dir = %q, want %q (must route identically)", got.Dir, fwd.Dir)
	}
	if want := "NGC7000/FRA300/NIGHT_3"; got.Dir != want {
		t.Errorf("Dir = %q, want %q", got.Dir, want)
	}
}

func TestRenderExposure(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.00100000004749745, "0.001s"},
		{1.67499804496765, "1.675s"},
		{300.0, "300s"},
		{60.0, "60s"},
	}
	for _, tc := range cases {
		if got := renderExposure(tc.in); got != tc.want {
			t.Errorf("renderExposure(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderSetTemp(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{-30, "-30C"},
		{0, "0C"},
		{-10, "-10C"},
		{-9.6, "-10C"}, // rounds to nearest int
	}
	for _, tc := range cases {
		got, _, err := resolveToken("<SET-TEMP>", model.Classification{SetTempC: tc.in, SetTempPresent: true}, testRig(), routeOpts{})
		if err != nil {
			t.Fatalf("resolveToken: %v", err)
		}
		if got != tc.want {
			t.Errorf("<SET-TEMP> %v = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An uncooled capture carries no SET-TEMP card. It must file into its own
// bucket rather than quarantine, and never borrow CCD-TEMP.
func TestSetTempAbsentRendersUncooled(t *testing.T) {
	cls := model.Classification{
		Type: model.TypeDark, Sensor: "6200MM", SensorMapped: true,
		Gain: 100, GainPresent: true, BinX: 1, BinY: 1, BinPresent: true,
		Exptime: 300, ExptimePresent: true,
		DateObs: time.Date(2026, 6, 19, 2, 0, 0, 0, time.UTC), DateObsPresent: true,
	}
	res, err := Route(cls, testRig(), Options{
		Templates:     defaultTemplates(),
		DateFormat:    "2006-01-02",
		ObservingDate: time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if want := "6200MM/DARK/gain100/Bin1/uncooled/300s/2026-06-18"; res.Dir != want {
		t.Fatalf("Dir = %q, want %q", res.Dir, want)
	}
}

func TestBinningDiffer(t *testing.T) {
	cls := model.Classification{
		Type: model.TypeDark, Sensor: "6200MM", SensorMapped: true,
		BinX: 2, BinY: 3, BinPresent: true,
	}
	opts := Options{Templates: model.Templates{Cal: map[model.FrameType]string{
		model.TypeDark: "<BINNING>",
	}}}
	got, err := Route(cls, testRig(), opts)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got.Dir != "Bin2x3" {
		t.Errorf("Dir = %q, want %q", got.Dir, "Bin2x3")
	}
}

// A calibration frame with a valid DATE-OBS must render <DATE> from its own
// observing date, not collapse every night into the zero date 0001-01-01.
func TestCalFrameUsesObservingDate(t *testing.T) {
	obs := time.Date(2026, 2, 8, 22, 0, 0, 0, time.UTC)
	cls := model.Classification{
		Type: model.TypeDark, Sensor: "6200MM", SensorMapped: true,
		DateObs: obs, DateObsPresent: true,
	}
	opts := Options{
		Templates:     model.Templates{Cal: map[model.FrameType]string{model.TypeDark: "DARK/<DATE>"}},
		DateFormat:    "2006-01-02",
		ObservingDate: ObservingDate(obs, time.UTC),
	}
	got, err := Route(cls, testRig(), opts)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	want := "DARK/" + ObservingDate(obs, time.UTC).Format("2006-01-02")
	if got.Dir != want {
		t.Errorf("Dir = %q, want %q (cal frame must not fold into the zero date)", got.Dir, want)
	}
}

// <YEAR>/<MONTH>/<DAY> break the observing date into date-based folders. They
// must read the same instant <DATE> does, zero-pad month and day, and stay
// consistent with <DATE> for the same frame.
func TestYearMonthDayTokens(t *testing.T) {
	cases := []struct {
		name string
		obs  time.Time
		want string // <YEAR>/<MONTH>/<DAY>
	}{
		// 22:00 UTC is still the same observing date (before the noon rollover).
		{"mid-year", time.Date(2026, 7, 17, 22, 0, 0, 0, time.UTC), "2026/07/17"},
		{"january zero-pad", time.Date(2026, 1, 5, 22, 0, 0, 0, time.UTC), "2026/01/05"},
		{"december", time.Date(2026, 12, 25, 22, 0, 0, 0, time.UTC), "2026/12/25"},
		// 02:00 UTC falls before local noon, so the observing date rolls back to
		// the previous calendar day — here across a year boundary.
		{"night rollover across new year", time.Date(2027, 1, 1, 2, 0, 0, 0, time.UTC), "2026/12/31"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := model.Classification{
				Type: model.TypeDark, Sensor: "6200MM", SensorMapped: true,
				DateObs: tc.obs, DateObsPresent: true,
			}
			opts := Options{
				Templates:     model.Templates{Cal: map[model.FrameType]string{model.TypeDark: "<YEAR>/<MONTH>/<DAY>/<TARGET>"}},
				DateFormat:    "2006-01-02",
				ObservingDate: ObservingDate(tc.obs, time.UTC),
			}
			cls.Target = "M31"
			cls.TargetPresent = true
			got, err := Route(cls, testRig(), opts)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			want := tc.want + "/M31"
			if got.Dir != want {
				t.Errorf("Dir = %q, want %q", got.Dir, want)
			}
			// The date-part tokens must agree with <DATE> for the same frame:
			// rendering <DATE> as YYYY-MM-DD must equal <YEAR>-<MONTH>-<DAY>.
			dateOpts := opts
			dateOpts.Templates = model.Templates{Cal: map[model.FrameType]string{model.TypeDark: "<DATE>"}}
			dateGot, err := Route(cls, testRig(), dateOpts)
			if err != nil {
				t.Fatalf("Route(<DATE>): %v", err)
			}
			wantDate := strings.ReplaceAll(tc.want, "/", "-")
			if dateGot.Dir != wantDate {
				t.Errorf("<DATE> = %q, want %q (must match <YEAR>/<MONTH>/<DAY>)", dateGot.Dir, wantDate)
			}
		})
	}
}

// <YEAR>/<MONTH>/<DAY> share <DATE>'s missing-value handling: a template that
// references any of them must quarantine a frame with no DATE-OBS.
func TestYearMonthDayMissingDateObsQuarantines(t *testing.T) {
	for _, tok := range []string{"<YEAR>", "<MONTH>", "<DAY>"} {
		cls := model.Classification{Type: model.TypeDark, Sensor: "6200MM", SensorMapped: true}
		opts := Options{
			Templates:  model.Templates{Cal: map[model.FrameType]string{model.TypeDark: "DARK/" + tok}},
			DateFormat: "2006-01-02",
		}
		if _, err := Route(cls, testRig(), opts); !errors.As(err, new(*QuarantineError)) {
			t.Fatalf("%s: want QuarantineError for missing DATE-OBS, got %v", tok, err)
		}
	}
}

// A template that references <DATE> must quarantine a frame with no DATE-OBS
// rather than silently render the zero date.
func TestDateTokenMissingDateObsQuarantines(t *testing.T) {
	cls := model.Classification{Type: model.TypeDark, Sensor: "6200MM", SensorMapped: true}
	opts := Options{
		Templates:  model.Templates{Cal: map[model.FrameType]string{model.TypeDark: "DARK/<DATE>"}},
		DateFormat: "2006-01-02",
	}
	if _, err := Route(cls, testRig(), opts); !errors.As(err, new(*QuarantineError)) {
		t.Fatalf("want QuarantineError for missing DATE-OBS, got %v", err)
	}
}

func TestStripTargetSpaces(t *testing.T) {
	opts := func(strip bool) Options {
		return Options{
			Templates:         model.Templates{Light: "<TARGET>"},
			StripTargetSpaces: strip,
		}
	}
	cls := lightCls()
	cls.Target = "C-2025 A6"

	got, err := Route(cls, testRig(), opts(true))
	if err != nil {
		t.Fatalf("Route(strip): %v", err)
	}
	if got.Dir != "C-2025A6" {
		t.Errorf("strip=true Dir = %q, want %q", got.Dir, "C-2025A6")
	}

	got, err = Route(cls, testRig(), opts(false))
	if err != nil {
		t.Fatalf("Route(no strip): %v", err)
	}
	if got.Dir != "C-2025 A6" {
		t.Errorf("strip=false Dir = %q, want %q", got.Dir, "C-2025 A6")
	}
}

// TestFilterCollapse verifies the one optional token: a lone <FILTER> segment is
// dropped on an OSC frame (no FILTER card) but present on a mono frame.
func TestFilterCollapse(t *testing.T) {
	r := header.New()
	opts := Options{
		Templates:         model.Templates{Light: "<TARGET>/<FILTER>/<OTA>"},
		StripTargetSpaces: true,
	}

	osc, err := r.ReadHeader("../header/testdata/asiair_light_294mc_osc.fit")
	if err != nil {
		t.Fatalf("ReadHeader osc: %v", err)
	}
	oscCls := classify.Classify(osc, aliasTable, sensorMap)
	got, err := Route(oscCls, testRig(), opts)
	if err != nil {
		t.Fatalf("Route osc: %v", err)
	}
	if got.Dir != "C-2025A6/FRA300" {
		t.Errorf("osc Dir = %q, want %q (FILTER segment should be dropped)", got.Dir, "C-2025A6/FRA300")
	}

	mono, err := r.ReadHeader("../header/testdata/asiair_light_2600mm_ha.fit")
	if err != nil {
		t.Fatalf("ReadHeader mono: %v", err)
	}
	monoCls := classify.Classify(mono, aliasTable, sensorMap)
	got, err = Route(monoCls, testRig(), opts)
	if err != nil {
		t.Fatalf("Route mono: %v", err)
	}
	if got.Dir != "C-2025A6/HA/FRA300" {
		t.Errorf("mono Dir = %q, want %q (FILTER segment should be present)", got.Dir, "C-2025A6/HA/FRA300")
	}
}

// TestFilterCollapseAbsorbsSeparators verifies that when the optional <FILTER>
// token collapses on an OSC frame inside a segment with other content, one
// adjacent separator is absorbed — leaving no dangling, leading, or doubled
// punctuation — while a present <FILTER> (mono) keeps every separator intact.
func TestFilterCollapseAbsorbsSeparators(t *testing.T) {
	// A LIGHT frame with the tokens the templates below reference resolvable:
	// TARGET/DATE-OBS (required for lights), SENSOR, and EXPTIME. filterPresent
	// selects OSC (absent) versus mono (present, "HA").
	baseCls := func(filterPresent bool) model.Classification {
		cls := lightCls()
		cls.Sensor = "2600MM"
		cls.SensorMapped = true
		cls.Exptime = 1.7
		cls.ExptimePresent = true
		cls.FilterPresent = filterPresent
		if filterPresent {
			cls.Filter = "HA"
		}
		return cls
	}

	cases := []struct {
		name          string
		template      string
		filterPresent bool
		want          string
	}{
		// OSC: FILTER absent — the adjacent separator is absorbed.
		{"osc trailing", "<TARGET>_<FILTER>/<OTA>", false, "NGC7000/FRA300"},
		{"osc leading", "<FILTER>-<TARGET>/<OTA>", false, "NGC7000/FRA300"},
		{"osc doubled", "<SENSOR>_<FILTER>_<EXPOSURE>", false, "2600MM_1.7s"},
		// Regression guard: a lone <FILTER> segment is still dropped whole.
		{"osc lone segment dropped", "<TARGET>/<FILTER>/<OTA>", false, "NGC7000/FRA300"},
		// Mono: FILTER present — every separator is preserved.
		{"mono trailing preserved", "<TARGET>_<FILTER>/<OTA>", true, "NGC7000_HA/FRA300"},
		{"mono doubled preserved", "<SENSOR>_<FILTER>_<EXPOSURE>", true, "2600MM_HA_1.7s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := baseCls(tc.filterPresent)
			opts := Options{Templates: model.Templates{Light: tc.template}}
			got, err := Route(cls, testRig(), opts)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if got.Dir != tc.want {
				t.Errorf("Dir = %q, want %q", got.Dir, tc.want)
			}
		})
	}
}

// TestAdversarialTargets confirms hostile OBJECT values are neutralized into a
// single safe component that cannot escape the root, rather than quarantined.
func TestAdversarialTargets(t *testing.T) {
	cases := []struct {
		object string
		want   string
	}{
		{"../../Windows", ".._.._Windows"},
		{"CON", "_CON"},
		{`a\b:c`, "a_b_c"},
	}
	opts := Options{Templates: model.Templates{Light: "<TARGET>"}}
	for _, tc := range cases {
		cls := lightCls()
		cls.Target = tc.object
		got, err := Route(cls, testRig(), opts)
		if err != nil {
			t.Fatalf("Route(%q): %v", tc.object, err)
		}
		if got.Dir != tc.want {
			t.Errorf("OBJECT %q → Dir %q, want %q", tc.object, got.Dir, tc.want)
		}
	}
}

func TestQuarantine(t *testing.T) {
	cases := []struct {
		name string
		cls  model.Classification
		tmpl model.Templates
		want string // expected MissingToken (empty = don't check)
	}{
		{
			name: "light missing OBJECT",
			cls:  model.Classification{Type: model.TypeLight, DateObs: time.Now(), DateObsPresent: true},
			tmpl: defaultTemplates(),
			want: "OBJECT",
		},
		{
			name: "light missing DATE-OBS",
			cls:  model.Classification{Type: model.TypeLight, Target: "M31", TargetPresent: true},
			tmpl: defaultTemplates(),
			want: "DATE-OBS",
		},
		{
			name: "SENSOR unmapped but referenced",
			cls:  model.Classification{Type: model.TypeDark, SensorMapped: false},
			tmpl: model.Templates{Cal: map[model.FrameType]string{model.TypeDark: "<SENSOR>/DARK"}},
			want: "SENSOR",
		},
		{
			name: "TARGET sanitizes to empty",
			cls:  model.Classification{Type: model.TypeLight, Target: "...", TargetPresent: true, DateObs: time.Now(), DateObsPresent: true},
			tmpl: model.Templates{Light: "<TARGET>-suffix"},
			want: "OBJECT",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Route(tc.cls, testRig(), Options{Templates: tc.tmpl, DateFormat: "2006-01-02"})
			var qe *QuarantineError
			if !errors.As(err, &qe) {
				t.Fatalf("err = %v, want *QuarantineError", err)
			}
			if tc.want != "" && qe.MissingToken != tc.want {
				t.Errorf("MissingToken = %q, want %q", qe.MissingToken, tc.want)
			}
		})
	}
}

// TestRequiredEmptySegmentQuarantines confirms a required token that renders
// empty is a quarantine, not a degenerate "6200MM//..." path.
func TestRequiredEmptySegmentQuarantines(t *testing.T) {
	cls := model.Classification{Type: model.TypeFlat, Sensor: "6200MM", SensorMapped: true}
	rig := testRig()
	rig.OTA = "" // <OTA> segment renders empty
	opts := Options{Templates: model.Templates{Cal: map[model.FrameType]string{
		model.TypeFlat: "<SENSOR>/<OTA>",
	}}}
	_, err := Route(cls, rig, opts)
	var qe *QuarantineError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want *QuarantineError", err)
	}
}

// TestBadTypeAndMissingTemplate covers the defensive plain-error guards.
func TestBadTypeAndMissingTemplate(t *testing.T) {
	_, err := Route(model.Classification{Type: model.TypeUnknown}, testRig(), Options{Templates: defaultTemplates()})
	if err == nil {
		t.Fatal("unknown type: want error")
	}
	var qe *QuarantineError
	if errors.As(err, &qe) {
		t.Errorf("unknown type should be a plain error, got *QuarantineError")
	}

	_, err = Route(model.Classification{Type: model.TypeDark, Sensor: "x", SensorMapped: true}, testRig(),
		Options{Templates: model.Templates{Cal: map[model.FrameType]string{}}})
	if err == nil {
		t.Fatal("missing cal template: want error")
	}
}

func TestObservingDate(t *testing.T) {
	utc := time.UTC
	east5 := time.FixedZone("UTC+5", 5*3600)

	cases := []struct {
		name    string
		instant time.Time
		loc     *time.Location
		wantY   int
		wantM   time.Month
		wantD   int
	}{
		{
			name:    "02:00 UTC carries previous day",
			instant: time.Date(2026, 6, 19, 2, 0, 0, 0, utc),
			loc:     utc,
			wantY:   2026, wantM: 6, wantD: 18,
		},
		{
			name:    "13:00 UTC carries same day",
			instant: time.Date(2026, 6, 19, 13, 0, 0, 0, utc),
			loc:     utc,
			wantY:   2026, wantM: 6, wantD: 19,
		},
		{
			name:    "noon exactly carries same day",
			instant: time.Date(2026, 6, 19, 12, 0, 0, 0, utc),
			loc:     utc,
			wantY:   2026, wantM: 6, wantD: 19,
		},
		{
			// 21:00Z is 02:00 local in UTC+5 on the 19th → previous day 18th.
			name:    "02:00 local in UTC+5 carries previous day",
			instant: time.Date(2026, 6, 18, 21, 0, 0, 0, utc),
			loc:     east5,
			wantY:   2026, wantM: 6, wantD: 18,
		},
		{
			// 08:00Z is 13:00 local in UTC+5 on the 19th → same day.
			name:    "13:00 local in UTC+5 carries same day",
			instant: time.Date(2026, 6, 19, 8, 0, 0, 0, utc),
			loc:     east5,
			wantY:   2026, wantM: 6, wantD: 19,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ObservingDate(tc.instant, tc.loc)
			if got.Year() != tc.wantY || got.Month() != tc.wantM || got.Day() != tc.wantD {
				t.Errorf("ObservingDate = %v, want %04d-%02d-%02d", got, tc.wantY, tc.wantM, tc.wantD)
			}
			if got.Location() != tc.loc {
				t.Errorf("Location = %v, want %v", got.Location(), tc.loc)
			}
			if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
				t.Errorf("not midnight: %v", got)
			}
		})
	}
}
