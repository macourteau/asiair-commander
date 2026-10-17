package route

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// defaultMosaicTemplate is the shipped default from design routing-and-filing
// §7.2. The golden paths below are anchored to this exact string.
const defaultMosaicTemplate = "<TARGET_BASE>/<OTA>/<SESSION>/<TARGET_BASE>_<PANEL>/NIGHT_<n>"

// mosaicTemplates are the shipped defaults plus the mosaic template, so a case
// that routes through the light template renders a visibly different shape from
// one that routes through the mosaic template.
func mosaicTemplates() model.Templates {
	t := defaultTemplates()
	t.Mosaic = defaultMosaicTemplate
	return t
}

func mosaicOpts(detection, strip bool) Options {
	cls := lightCls()
	return Options{
		Templates:         mosaicTemplates(),
		Night:             1,
		ObservingDate:     ObservingDate(cls.DateObs, time.UTC),
		DateFormat:        "2006-01-02",
		StripTargetSpaces: strip,
		MosaicDetection:   detection,
	}
}

// TestRouteMosaicOnlyForParsedLightPanels proves INV-M4: the mosaic template
// routes a frame only when detection is on AND the frame is a LIGHT whose
// TRANSFORMED target parsed as a panel with row and column in 1-20; with
// detection off every outcome is what it was before the feature existed.
//
// It covers acceptance criteria 1 (detection off is byte-identical, including
// for "Sadr_1-1"), 3 (the default mosaic template's golden path), 4 (an ordinary
// target is unchanged), 5 (catalogue designations), 6 (the date-suffix false
// positives the 1-20 bound rejects) and 7 (the residual false positives that
// bound accepts, pinned so changing it is deliberate).
func TestRouteMosaicOnlyForParsedLightPanels(t *testing.T) {
	cases := []struct {
		name   string
		target string
		strip  bool
		// wantOn is the directory with mosaic detection ON, wantOff the one with
		// it off. They differ only for a target that parses as a panel.
		wantOn  string
		wantOff string
	}{
		// design routing-and-filing AC4: an ordinary target files exactly where it
		// does with detection off.
		{name: "plain target", target: "NGC6888",
			wantOn: "NGC6888/FRA300/Summer2026/NIGHT_1", wantOff: "NGC6888/FRA300/Summer2026/NIGHT_1"},

		// design routing-and-filing AC5: catalogue designations as normally written
		// are not panels — the suffix must be introduced by an underscore.
		{name: "Sh2-101", target: "Sh2-101",
			wantOn: "Sh2-101/FRA300/Summer2026/NIGHT_1", wantOff: "Sh2-101/FRA300/Summer2026/NIGHT_1"},
		{name: "M 1-92", target: "M 1-92",
			wantOn: "M 1-92/FRA300/Summer2026/NIGHT_1", wantOff: "M 1-92/FRA300/Summer2026/NIGHT_1"},
		{name: "K 1-16", target: "K 1-16",
			wantOn: "K 1-16/FRA300/Summer2026/NIGHT_1", wantOff: "K 1-16/FRA300/Summer2026/NIGHT_1"},
		{name: "Abell 2-1", target: "Abell 2-1",
			wantOn: "Abell 2-1/FRA300/Summer2026/NIGHT_1", wantOff: "Abell 2-1/FRA300/Summer2026/NIGHT_1"},
		{name: "PK 164+31.1", target: "PK 164+31.1",
			wantOn: "PK 164+31.1/FRA300/Summer2026/NIGHT_1", wantOff: "PK 164+31.1/FRA300/Summer2026/NIGHT_1"},

		// design routing-and-filing AC6: date suffixes have exactly the shape of a
		// panel name and are rejected only by the 1-20 bound. A false positive here
		// would let re-file offer to move archived data.
		{name: "date suffix year-month", target: "M42_2026-07",
			wantOn: "M42_2026-07/FRA300/Summer2026/NIGHT_1", wantOff: "M42_2026-07/FRA300/Summer2026/NIGHT_1"},
		{name: "date suffix year-month other", target: "NGC7000_2024-11",
			wantOn: "NGC7000_2024-11/FRA300/Summer2026/NIGHT_1", wantOff: "NGC7000_2024-11/FRA300/Summer2026/NIGHT_1"},
		{name: "date suffix month-day", target: "M42_12-25",
			wantOn: "M42_12-25/FRA300/Summer2026/NIGHT_1", wantOff: "M42_12-25/FRA300/Summer2026/NIGHT_1"},
		{name: "zero indices", target: "Sadr_0-0",
			wantOn: "Sadr_0-0/FRA300/Summer2026/NIGHT_1", wantOff: "Sadr_0-0/FRA300/Summer2026/NIGHT_1"},
		{name: "above the bound", target: "Sadr_21-1",
			wantOn: "Sadr_21-1/FRA300/Summer2026/NIGHT_1", wantOff: "Sadr_21-1/FRA300/Summer2026/NIGHT_1"},

		// design routing-and-filing AC1 + AC3: the panel case. With detection off it
		// is an ordinary target with an unusual name and files flat, exactly as
		// before.
		{name: "panel", target: "Sadr_1-1",
			wantOn: "Sadr/FRA300/Summer2026/Sadr_1-1/NIGHT_1", wantOff: "Sadr_1-1/FRA300/Summer2026/NIGHT_1"},
		{name: "panel at the bound", target: "Sadr_20-20",
			wantOn: "Sadr/FRA300/Summer2026/Sadr_20-20/NIGHT_1", wantOff: "Sadr_20-20/FRA300/Summer2026/NIGHT_1"},
		// The panel token is reproduced verbatim, so a zero-padded producer name
		// keeps its padding in the folder it files into.
		{name: "zero-padded panel", target: "Sadr_01-02",
			wantOn: "Sadr/FRA300/Summer2026/Sadr_01-02/NIGHT_1", wantOff: "Sadr_01-02/FRA300/Summer2026/NIGHT_1"},
		// design routing-and-filing AC5, second half: a hyphenated designation still
		// yields the right base.
		{name: "hyphenated base", target: "Sh2-101_1-1",
			wantOn: "Sh2-101/FRA300/Summer2026/Sh2-101_1-1/NIGHT_1", wantOff: "Sh2-101_1-1/FRA300/Summer2026/NIGHT_1"},

		// design routing-and-filing AC7: the accepted residual false positives — a
		// designation spelled with an underscore for its space and carrying small
		// numbers. Pinned so that a future change to the bound is a deliberate
		// decision.
		{name: "false positive Abell_2-1", target: "Abell_2-1",
			wantOn: "Abell/FRA300/Summer2026/Abell_2-1/NIGHT_1", wantOff: "Abell_2-1/FRA300/Summer2026/NIGHT_1"},
		{name: "false positive K_1-16", target: "K_1-16",
			wantOn: "K/FRA300/Summer2026/K_1-16/NIGHT_1", wantOff: "K_1-16/FRA300/Summer2026/NIGHT_1"},

		// The parse runs on the TRANSFORMED target: "Sadr_1 -1" is a panel only
		// after the space strip.
		{name: "panel only after the strip", target: "Sadr_1 -1", strip: true,
			wantOn: "Sadr/FRA300/Summer2026/Sadr_1-1/NIGHT_1", wantOff: "Sadr_1-1/FRA300/Summer2026/NIGHT_1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := lightCls()
			cls.Target = tc.target

			for _, detection := range []bool{false, true} {
				opts := mosaicOpts(detection, tc.strip)
				want := tc.wantOff
				if detection {
					want = tc.wantOn
				}
				got, err := Route(cls, testRig(), opts)
				if err != nil {
					t.Fatalf("detection=%v Route(%q): %v", detection, tc.target, err)
				}
				if got.Kind != RootLight {
					t.Errorf("detection=%v Kind = %v, want RootLight", detection, got.Kind)
				}
				if got.Dir != want {
					t.Errorf("detection=%v Dir = %q\n                want %q", detection, got.Dir, want)
				}
				// The exported predicate must agree with the template Route
				// actually selected — the three consumers that cannot see
				// route.Result depend on it.
				wantPanel := detection && tc.wantOn != tc.wantOff
				if got := IsMosaicPanel(tc.target, model.TypeLight, opts); got != wantPanel {
					t.Errorf("detection=%v IsMosaicPanel(%q) = %v, want %v", detection, tc.target, got, wantPanel)
				}
			}
		})
	}
}

// design routing-and-filing AC8: a calibration frame can carry a stale OBJECT
// from the light it followed. A suffix match on one must not route it through
// the mosaic template, which would file it under the LIGHT root.
func TestMosaicNeverRoutesCalibration(t *testing.T) {
	for _, ft := range []model.FrameType{model.TypeDark, model.TypeFlat, model.TypeBias} {
		t.Run(string(ft), func(t *testing.T) {
			cls := lightCls()
			cls.Type = ft
			cls.Target = "Sadr_1-1"
			cls.Sensor = "6200MM"
			cls.SensorMapped = true
			cls.Gain, cls.GainPresent = 100, true
			cls.BinX, cls.BinY, cls.BinPresent = 1, 1, true
			cls.SetTempC, cls.SetTempPresent = -10, true
			cls.Exptime, cls.ExptimePresent = 300, true

			opts := mosaicOpts(true, false)
			got, err := Route(cls, testRig(), opts)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if got.Kind != RootCal {
				t.Errorf("Kind = %v, want RootCal", got.Kind)
			}
			if !strings.HasPrefix(got.Dir, "6200MM/"+string(ft)+"/") {
				t.Errorf("Dir = %q, want the %s calibration template", got.Dir, ft)
			}
			if IsMosaicPanel(cls.Target, ft, opts) {
				t.Errorf("IsMosaicPanel(%q, %s) = true, want false", cls.Target, ft)
			}
		})
	}
}

// design routing-and-filing AC9: "_1-1" parses as a panel with an empty base. A
// frame whose name is unknowable has no correct home, so <TARGET_BASE>
// quarantines it rather than filing it under a folder named for its panel
// alone.
func TestMosaicDegenerateBaseQuarantines(t *testing.T) {
	cls := lightCls()
	cls.Target = "_1-1"

	_, err := Route(cls, testRig(), mosaicOpts(true, false))
	var qe *QuarantineError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v, want *QuarantineError", err)
	}
	if qe.MissingToken != "OBJECT" {
		t.Errorf("MissingToken = %q, want %q", qe.MissingToken, "OBJECT")
	}

	// With detection off it is an ordinary target and files flat, unchanged.
	got, err := Route(cls, testRig(), mosaicOpts(false, false))
	if err != nil {
		t.Fatalf("detection off Route: %v", err)
	}
	if want := "_1-1/FRA300/Summer2026/NIGHT_1"; got.Dir != want {
		t.Errorf("detection off Dir = %q, want %q", got.Dir, want)
	}
}

// design routing-and-filing AC10: the three layout forms in design
// routing-and-filing §7.2 render the paths shown there, plus the
// <PANEL_ROW>/<PANEL_COL> form the panel-distinguishing rule accepts. The rig's
// SESSION is set to the doc's placeholder so the expected strings read verbatim
// as the design writes them.
func TestMosaicTemplateLayouts(t *testing.T) {
	rig := testRig()
	rig.Session = "SESSION"

	cases := []struct {
		template string
		target   string
		want     string
	}{
		{defaultMosaicTemplate, "Sadr_1-1", "Sadr/FRA300/SESSION/Sadr_1-1/NIGHT_1"},
		{"<TARGET_BASE>/<OTA>/<SESSION>/<TARGET>/NIGHT_<n>", "Sadr_1-1", "Sadr/FRA300/SESSION/Sadr_1-1/NIGHT_1"},
		{"<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>/NIGHT_<n>", "Sadr_1-1", "Sadr/FRA300/SESSION/1-1/NIGHT_1"},
		// <PANEL_ROW>/<PANEL_COL> render the parsed integers, which is why the
		// equivalence claim is stated in terms of <PANEL>: a zero-padded producer
		// name reassembles here without its padding.
		{"<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>", "Sadr_1-1", "Sadr/FRA300/SESSION/1/1/NIGHT_1"},
		{"<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>", "Sadr_01-02", "Sadr/FRA300/SESSION/1/2/NIGHT_1"},
		{"<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>/NIGHT_<n>", "Sadr_01-02", "Sadr/FRA300/SESSION/01-02/NIGHT_1"},
	}

	for _, tc := range cases {
		t.Run(tc.template+" "+tc.target, func(t *testing.T) {
			cls := lightCls()
			cls.Target = tc.target
			opts := mosaicOpts(true, false)
			opts.Templates.Mosaic = tc.template
			got, err := Route(cls, rig, opts)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if got.Dir != tc.want {
				t.Errorf("Dir = %q, want %q", got.Dir, tc.want)
			}
		})
	}
}

// design routing-and-filing AC11: the two default-equivalent forms —
// "<TARGET_BASE>_<PANEL>" and "<TARGET>" — must render byte-identical
// directories on adversarial targets. That equivalence is what lets an operator
// pick either form; it holds only because the split happens AFTER the
// transforms, never before.
func TestMosaicTokenFormsAgree(t *testing.T) {
	cases := []struct {
		name   string
		target string
		strip  bool
		want   string // the leaf both forms must render
	}{
		// sanitize.Component trims only TRAILING spaces, so a base trimmed
		// separately would drop the internal one and diverge from <TARGET>.
		{name: "internal space, no strip", target: "NGC 7000 _1-1", want: "NGC 7000 _1-1"},
		{name: "internal space, strip", target: "NGC 7000 _1-1", strip: true, want: "NGC7000_1-1"},
		// A panel only after the strip: splitting the raw value first would miss
		// it entirely and file the frame flat.
		{name: "panel after strip", target: "Sadr_1 -1", strip: true, want: "Sadr_1-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := lightCls()
			cls.Target = tc.target

			explicit := mosaicOpts(true, tc.strip)
			explicit.Templates.Mosaic = "<TARGET_BASE>_<PANEL>"
			gotExplicit, err := Route(cls, testRig(), explicit)
			if err != nil {
				t.Fatalf("Route(<TARGET_BASE>_<PANEL>): %v", err)
			}

			whole := mosaicOpts(true, tc.strip)
			whole.Templates.Mosaic = "<TARGET>"
			gotWhole, err := Route(cls, testRig(), whole)
			if err != nil {
				t.Fatalf("Route(<TARGET>): %v", err)
			}

			if gotExplicit.Dir != gotWhole.Dir {
				t.Errorf("<TARGET_BASE>_<PANEL> = %q but <TARGET> = %q (must be identical)", gotExplicit.Dir, gotWhole.Dir)
			}
			if gotExplicit.Dir != tc.want {
				t.Errorf("Dir = %q, want %q", gotExplicit.Dir, tc.want)
			}
		})
	}
}

// design routing-and-filing AC14 (routing half): config validation rejects a
// mosaic-only token in the light or a cal template, but Load does not validate,
// so a hand-edited config.yaml reaches routing unchecked. Such a frame must
// quarantine rather than file into a directory literally named "_PANEL_".
//
// It is exercised with detection ON and a NON-panel light: a panel light would
// route through the mosaic template and never reach the light one.
func TestMosaicTokenOutsideMosaicTemplateQuarantines(t *testing.T) {
	for _, tok := range []string{"<TARGET_BASE>", "<PANEL>", "<PANEL_ROW>", "<PANEL_COL>"} {
		t.Run(tok, func(t *testing.T) {
			cls := lightCls()
			cls.Target = "NGC6888" // not a panel: this frame reaches the light template

			opts := mosaicOpts(true, false)
			opts.Templates.Light = "<TARGET>/" + tok + "/NIGHT_<n>"
			_, err := Route(cls, testRig(), opts)
			var qe *QuarantineError
			if !errors.As(err, &qe) {
				t.Fatalf("err = %v, want *QuarantineError", err)
			}
			if !strings.Contains(qe.Reason, tok) {
				t.Errorf("Reason = %q, want it to name %s", qe.Reason, tok)
			}
		})
	}

	// The same token in a calibration template, where no panel is ever parsed.
	cls := lightCls()
	cls.Type = model.TypeDark
	cls.Target = "Sadr_1-1"
	cls.Sensor = "6200MM"
	cls.SensorMapped = true
	opts := mosaicOpts(true, false)
	opts.Templates.Cal = map[model.FrameType]string{model.TypeDark: "<SENSOR>/<PANEL>"}
	if _, err := Route(cls, testRig(), opts); !errors.As(err, new(*QuarantineError)) {
		t.Fatalf("cal template with <PANEL>: err = %v, want *QuarantineError", err)
	}
}

// IsMosaicPanel is the one predicate three consumers share; it must apply the
// same gating and the same transforms Route does, from the RAW target.
func TestIsMosaicPanelGating(t *testing.T) {
	cases := []struct {
		name      string
		target    string
		frameType model.FrameType
		detection bool
		strip     bool
		want      bool
	}{
		{name: "panel light with detection", target: "Sadr_1-1", frameType: model.TypeLight, detection: true, want: true},
		{name: "detection off", target: "Sadr_1-1", frameType: model.TypeLight, detection: false, want: false},
		{name: "dark frame", target: "Sadr_1-1", frameType: model.TypeDark, detection: true, want: false},
		{name: "unknown type", target: "Sadr_1-1", frameType: model.TypeUnknown, detection: true, want: false},
		{name: "not a panel", target: "NGC6888", frameType: model.TypeLight, detection: true, want: false},
		{name: "out of bounds", target: "M42_2026-07", frameType: model.TypeLight, detection: true, want: false},
		// The transforms are applied by the predicate itself, so a caller holding
		// a raw stored target gets the same answer routing will.
		{name: "surrounding whitespace", target: "  Sadr_1-1  ", frameType: model.TypeLight, detection: true, want: true},
		{name: "panel after strip", target: "Sadr_1 -1", frameType: model.TypeLight, detection: true, strip: true, want: true},
		{name: "panel only with strip off", target: "Sadr_1 -1", frameType: model.TypeLight, detection: true, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := mosaicOpts(tc.detection, tc.strip)
			if got := IsMosaicPanel(tc.target, tc.frameType, opts); got != tc.want {
				t.Errorf("IsMosaicPanel(%q, %s) = %v, want %v", tc.target, tc.frameType, got, tc.want)
			}
		})
	}
}
