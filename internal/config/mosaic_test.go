package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
	"gopkg.in/yaml.v3"
)

// mosaicCfg is a config with mosaic detection on, one clean managed rig, and the
// given mosaic template — the shape every rule below is exercised against.
func mosaicCfg(tmpl string) *model.Config {
	c := Defaults()
	c.MosaicDetection = true
	c.Templates.Mosaic = tmpl
	c.Rigs = []model.RigProfile{{
		ID: "r1", Name: "R", Managed: true, GUID: "g1", KeepOriginals: true,
		OTA: "RC8", Session: "S1",
		Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
	}}
	return c
}

// problemsFor returns the messages emitted against one field.
func problemsFor(ps []Problem, field string) []string {
	var out []string
	for _, p := range ps {
		if p.Field == field {
			out = append(out, p.Message)
		}
	}
	return out
}

// TestMosaicDetectionShipsOff is design routing-and-filing AC1's config half:
// recognition is a parse of a producer naming convention, so a target of the
// operator's own shaped like "<name>_1-1" would start filing somewhere else the
// moment it defaulted on. Load never applies Defaults(), so nothing else in the
// tree pins the shipped value — a flipped default would change where an
// unattended install files tonight's frames and fail no other test.
func TestMosaicDetectionShipsOff(t *testing.T) {
	if Defaults().MosaicDetection {
		t.Fatal("mosaic detection ships off: opting in is the operator's decision")
	}
	// And the wizard's first write persists the opt-out rather than leaving the
	// key absent for a later default to fill in.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, Defaults()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MosaicDetection {
		t.Fatal("a config written from the shipped defaults must load with detection off")
	}
}

// TestMosaicTemplateMustDistinguishPanels proves INV-M11: a saved mosaic template
// always tells panels apart in a folder ABOVE its <n> segment.
//
// The rule is positional, not a containment test, and the third case is why:
// ".../NIGHT_<n>/<PANEL>" contains a panel token and would pass containment while
// being worse than the flat layout the rule forbids — the night rebuild stops
// descending at NIGHT_<n> and skips directory entries, so such a folder yields no
// entry at all and every ordinal in the tree is lost, not just nine of ten.
func TestMosaicTemplateMustDistinguishPanels(t *testing.T) {
	cases := []struct {
		name  string
		tmpl  string
		valid bool
	}{
		{"default", defaultMosaicTemplate, true},
		{"explicit TARGET leaf", "<TARGET_BASE>/<OTA>/<SESSION>/<TARGET>/NIGHT_<n>", true},
		{"bare PANEL leaf", "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>/NIGHT_<n>", true},
		{"row and col in separate segments", "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>", true},
		// Strictly BEFORE: a panel token sharing the night segment is not a folder
		// above it, so it does not satisfy the rule even though it would produce
		// distinct directory names.
		{"panel token inside the night segment", "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>_NIGHT_<n>", false},
		{"flat: no panel token at all", "<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>", false},
		{"panel BELOW the night folder", "<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>/<PANEL>", false},
		{"row without col", "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/NIGHT_<n>", false},
		{"col without row", "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_COL>/NIGHT_<n>", false},
		{"first <n> segment is the one that counts", "NIGHT_<n>/<PANEL>/NIGHT_<n>", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := Validate(mosaicCfg(c.tmpl), nil)
			msgs := problemsFor(ps, "templates.mosaic")
			distinguishes := true
			for _, m := range msgs {
				if strings.Contains(m, "tell panels apart") {
					distinguishes = false
				}
			}
			if distinguishes != c.valid {
				t.Fatalf("template %q: distinguishes=%v want %v (problems: %v)", c.tmpl, distinguishes, c.valid, msgs)
			}
			if c.valid && HasErrors(ps) {
				t.Fatalf("template %q must validate clean, got %+v", c.tmpl, ps)
			}
		})
	}
}

// TestMosaicTemplateTokenAndNightRules covers design routing-and-filing AC13's
// positive half: with detection on, an unknown token or a missing <n> is a
// field-level error on templates.mosaic.
func TestMosaicTemplateTokenAndNightRules(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		want string
	}{
		{"unknown token", "<TARGET_BASE>/<PANEL>/<NOPE>/NIGHT_<n>", "<NOPE> is not a valid variable name"},
		{"missing night", "<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>", "mosaic template must contain the night segment token <n>"},
		{"parent escape", "../<PANEL>/NIGHT_<n>", `template must not contain a ".." segment`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := Validate(mosaicCfg(c.tmpl), nil)
			if !HasErrors(ps) {
				t.Fatalf("template %q must be rejected", c.tmpl)
			}
			msgs := problemsFor(ps, "templates.mosaic")
			found := false
			for _, m := range msgs {
				if strings.Contains(m, c.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("template %q: want a templates.mosaic problem containing %q, got %v", c.tmpl, c.want, msgs)
			}
		})
	}
}

// TestMosaicValidationSuppressedWhileDetectionOff is design routing-and-filing
// AC13's and AC15's other half. The mosaic field is hidden while the toggle is
// off, so enforcing it would make it possible to 400 every config write over a
// value the operator cannot see: a hand-edited config.yaml, or one typed before
// the toggle was turned off.
func TestMosaicValidationSuppressedWhileDetectionOff(t *testing.T) {
	for _, tmpl := range []string{
		"<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>", // would fail the panel rule
		"<NOPE>/NIGHT_<n>",                        // would fail the token rule
		"<TARGET_BASE>/<PANEL>",                   // would fail the night rule
		"",                                        // would fail the emptiness rule
	} {
		c := mosaicCfg(tmpl)
		c.MosaicDetection = false
		if ps := Validate(c, nil); HasErrors(ps) {
			t.Fatalf("mosaic template %q must not be validated while detection is off, got %+v", tmpl, ps)
		}
	}
}

// TestMosaicOnlyTokensRejectedElsewhere is design routing-and-filing AC14's save
// half: outside the mosaic template these tokens have nothing to render, so the
// router quarantines the frame — which is why saving one anywhere else is
// refused.
func TestMosaicOnlyTokensRejectedElsewhere(t *testing.T) {
	for _, tok := range []string{"<TARGET_BASE>", "<PANEL>", "<PANEL_ROW>", "<PANEL_COL>"} {
		t.Run(tok+" in light", func(t *testing.T) {
			c := mosaicCfg(defaultMosaicTemplate)
			c.Templates.Light = "<TARGET>/" + tok + "/<OTA>/<SESSION>/NIGHT_<n>"
			msgs := problemsFor(Validate(c, nil), "templates.light")
			if len(msgs) != 1 || !strings.Contains(msgs[0], "only be used in the mosaic template") {
				t.Fatalf("%s in the light template must be refused, got %v", tok, msgs)
			}
		})
		t.Run(tok+" in cal", func(t *testing.T) {
			c := mosaicCfg(defaultMosaicTemplate)
			c.Templates.Cal[model.TypeDark] = "<SENSOR>/DARK/" + tok
			msgs := problemsFor(Validate(c, nil), "templates.cal.DARK")
			if len(msgs) != 1 || !strings.Contains(msgs[0], "only be used in the mosaic template") {
				t.Fatalf("%s in the DARK template must be refused, got %v", tok, msgs)
			}
		})
	}
	// The same tokens are accepted in the field they belong to. <PANEL_ROW> and
	// <PANEL_COL> are design routing-and-filing AC16: they save even though the
	// Settings chips do not offer them.
	c := mosaicCfg("<TARGET_BASE>/<OTA>/<SESSION>/<PANEL_ROW>/<PANEL_COL>/NIGHT_<n>")
	if ps := Validate(c, nil); HasErrors(ps) {
		t.Fatalf("<PANEL_ROW>/<PANEL_COL> must save in the mosaic template, got %+v", ps)
	}
}

// TestMosaicOTACrossCheckNamesMosaicTemplate is design routing-and-filing AC15:
// <OTA> in the mosaic template on a blank-OTA rig is an error while detection
// is on — every panel would quarantine at 3am — is not reported at all while it
// is off, and the message says which template created the need.
func TestMosaicOTACrossCheckNamesMosaicTemplate(t *testing.T) {
	blankOTA := func(detection bool) *model.Config {
		c := mosaicCfg("<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>/NIGHT_<n>")
		c.MosaicDetection = detection
		// Only the mosaic template may name <OTA>, so the light template cannot be
		// the one creating the need.
		c.Templates.Light = "<TARGET>/<SESSION>/NIGHT_<n>"
		delete(c.Templates.Cal, model.TypeFlat) // the only cal default naming <OTA>
		c.Rigs[0].OTA = ""
		return c
	}

	ps := Validate(blankOTA(true), nil)
	msgs := problemsFor(ps, "rigs[r1].ota")
	if len(msgs) != 1 {
		t.Fatalf("want exactly one rigs[r1].ota problem (the SPA routes field errors by field), got %v", msgs)
	}
	if !strings.Contains(msgs[0], "your mosaic template") {
		t.Fatalf("the OTA message must name the mosaic template, got %q", msgs[0])
	}
	// Severity is the half that decides whether the save is blocked: a warning
	// still writes the config, and every panel of the night then quarantines at
	// 3am on a blank <OTA>.
	for _, p := range ps {
		if p.Field == "rigs[r1].ota" && p.Severity != SeverityError {
			t.Fatalf("the mosaic <OTA> cross-check must be an error, got %q", p.Severity)
		}
	}
	if !HasErrors(ps) {
		t.Fatalf("a blank OTA under a mosaic template naming <OTA> must block the save, got %+v", ps)
	}

	if msgs := problemsFor(Validate(blankOTA(false), nil), "rigs[r1].ota"); len(msgs) != 0 {
		t.Fatalf("a blank OTA must not be reported while detection is off, got %v", msgs)
	}
}

// TestOTACrossCheckListsEveryNeedingTemplate pins that splitting the scan
// per-template still emits ONE problem per rig per field, listing all of them —
// two problems on one Field would render as duplicates in the SPA.
func TestOTACrossCheckListsEveryNeedingTemplate(t *testing.T) {
	c := mosaicCfg("<TARGET_BASE>/<OTA>/<SESSION>/<PANEL>/NIGHT_<n>")
	c.Rigs[0].OTA = "" // light, flat and mosaic all name <OTA> by default

	msgs := problemsFor(Validate(c, nil), "rigs[r1].ota")
	if len(msgs) != 1 {
		t.Fatalf("want exactly one rigs[r1].ota problem, got %v", msgs)
	}
	for _, name := range []string{"light", "flat", "mosaic"} {
		if !strings.Contains(msgs[0], name) {
			t.Fatalf("OTA message must name the %s template: %q", name, msgs[0])
		}
	}
}

// TestNormalizeTemplatesFillsEmptyMosaic is design routing-and-filing AC13's
// last clause: a blank mosaic template normalises to the default rather than
// being rejected. Filling it here rather than in a migration is what keeps an
// install whose rig leaves OTA blank valid, since the default names <OTA>.
func TestNormalizeTemplatesFillsEmptyMosaic(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		c := Defaults()
		c.Templates.Mosaic = blank
		NormalizeTemplates(c)
		if c.Templates.Mosaic != defaultMosaicTemplate {
			t.Fatalf("blank %q must normalise to the default, got %q", blank, c.Templates.Mosaic)
		}
	}
	// A customised value is left alone, apart from the separator rewrite every
	// template gets.
	c := Defaults()
	c.Templates.Mosaic = `<TARGET_BASE>\<PANEL>\NIGHT_<n>`
	NormalizeTemplates(c)
	if c.Templates.Mosaic != "<TARGET_BASE>/<PANEL>/NIGHT_<n>" {
		t.Fatalf("mosaic template not canonicalised to '/': %q", c.Templates.Mosaic)
	}
	// And validation never sees "": the emptiness rule exists for the paths that
	// reach Validate without normalising.
	empty := mosaicCfg("")
	msgs := problemsFor(Validate(empty, nil), "templates.mosaic")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "must not be empty") {
		t.Fatalf("an unnormalised empty mosaic template must be refused alone, got %v", msgs)
	}
}

// TestLoadFillsMissingMosaicTemplate is design routing-and-filing AC18's load
// half: a config.yaml written before templates.mosaic existed loads with the
// default applied, so the GUID-binding applier — which validates the loaded
// config without normalising — does not fail on every existing install.
func TestLoadFillsMissingMosaicTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	prior := `schema_version: 4
web_port: 8686
templates:
  light: <TARGET>/<OTA>/<SESSION>/NIGHT_<n>
  cal:
    DARK: <SENSOR>/DARK/<DATE>
`
	if err := os.WriteFile(path, []byte(prior), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Templates.Mosaic != defaultMosaicTemplate {
		t.Fatalf("mosaic template not filled at load: %q", cfg.Templates.Mosaic)
	}
	if cfg.MosaicDetection {
		t.Fatal("a config predating the field must load with detection off")
	}
	// The whole point: validation of the loaded config passes without a save.
	for _, p := range Validate(cfg, nil) {
		if p.Severity == SeverityError && strings.HasPrefix(p.Field, "templates.") {
			t.Fatalf("a pre-existing config must validate clean after load: %+v", p)
		}
	}
}

// TestOlderReleaseStillLoadsThisConfig is design routing-and-filing AC19: the
// previous release reads a config this one wrote. It will not start — the
// store-schema bump refuses an older binary — but the config must not be what
// stops it. priorConfig mirrors the fields that release knew; yaml.Unmarshal is
// non-strict, so the two added keys are ignored.
func TestOlderReleaseStillLoadsThisConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Defaults()
	cfg.MosaicDetection = true
	cfg.Templates.Mosaic = "<TARGET_BASE>/<PANEL>/NIGHT_<n>"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "mosaic_detection: true") {
		t.Fatalf("saved config does not carry mosaic_detection:\n%s", raw)
	}

	type priorTemplates struct {
		Light string                     `yaml:"light"`
		Cal   map[model.FrameType]string `yaml:"cal"`
	}
	type priorConfig struct {
		SchemaVersion int            `yaml:"schema_version"`
		WebPort       int            `yaml:"web_port"`
		Templates     priorTemplates `yaml:"templates"`
	}
	var prior priorConfig
	if err := yaml.Unmarshal(raw, &prior); err != nil {
		t.Fatalf("an older release must still parse this config: %v", err)
	}
	if prior.Templates.Light != cfg.Templates.Light || prior.WebPort != cfg.WebPort {
		t.Fatalf("older-release round trip lost values: %+v", prior)
	}
}
