package config

import (
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

func managedRig(sources ...model.Source) model.Config {
	c := *Defaults()
	c.Rigs = []model.RigProfile{{ID: "r1", Name: "R", Host: "h", Managed: true, OTA: "O", Session: "S", Sources: sources}}
	return c
}

func TestValidateSources_ManagedRigNeedsWatchedFolders(t *testing.T) {
	// A managed rig watching a share with folders is fine.
	if ps := Validate(ptr(managedRig(model.Source{Share: "EMMC Images", Roots: []string{"Autorun"}})), nil); HasErrors(ps) {
		t.Fatalf("valid source should not error: %+v", ps)
	}
	// A share with an empty folder list watches NOTHING → hard error.
	if ps := Validate(ptr(managedRig(model.Source{Share: "EMMC Images", Roots: nil})), nil); !HasErrors(ps) {
		t.Fatalf("empty roots on a managed rig must be an error")
	}
	// Blank folder strings collapse to nothing → error.
	if ps := Validate(ptr(managedRig(model.Source{Share: "EMMC Images", Roots: []string{"  "}})), nil); !HasErrors(ps) {
		t.Fatalf("all-blank roots must be an error")
	}
	// No sources at all on a managed rig → error.
	if ps := Validate(ptr(managedRig()), nil); !HasErrors(ps) {
		t.Fatalf("a managed rig with no sources must be an error")
	}
	// An UNMANAGED rig with empty roots is not actively watched → no error from this rule.
	c := managedRig(model.Source{Share: "EMMC Images", Roots: nil})
	c.Rigs[0].Managed = false
	for _, p := range Validate(ptr(c), nil) {
		if p.Severity == SeverityError {
			t.Fatalf("unmanaged rig should not error on empty roots: %+v", p)
		}
	}
}

// With no rigs configured, the folder templates may freely reference profile
// tokens like <OTA>/<SESSION> — there is no rig whose blank field could send
// frames to unclassified, so template editing must not be blocked. This is the
// escape hatch from the OTA/Session chicken-and-egg: edit templates first, add
// devices after.
func TestValidateSources_NoRigsAllowsProfileTokens(t *testing.T) {
	c := *Defaults() // default light template is "<TARGET>/<OTA>/<SESSION>/NIGHT_<n>"
	c.Rigs = nil
	if !strings.Contains(c.Templates.Light, "<OTA>") || !strings.Contains(c.Templates.Light, "<SESSION>") {
		t.Fatalf("test premise broken: default light template lacks <OTA>/<SESSION>: %q", c.Templates.Light)
	}
	if ps := Validate(&c, nil); HasErrors(ps) {
		t.Fatalf("templates with <OTA>/<SESSION> and no rigs must validate clean: %+v", ps)
	}
}

// A managed rig with a blank OTA is an error only while a template references
// <OTA>; dropping the token from every template clears the error (the other
// escape hatch). Same contract for <SESSION>.
func TestValidateSources_BlankProfileTokenGatedOnTemplateUse(t *testing.T) {
	rig := model.Source{Share: "EMMC Images", Roots: []string{"Autorun"}}

	// Blank OTA + template using <OTA> → hard error on the ota field.
	c := managedRig(rig)
	c.Rigs[0].OTA = ""
	c.Rigs[0].Session = "S"
	if !hasFieldError(Validate(&c, nil), "rigs[r1].ota") {
		t.Fatalf("blank OTA with <OTA> in template must error on the ota field")
	}

	// Same rig, but no template references <OTA> or <SESSION> → no token errors.
	c2 := managedRig(rig)
	c2.Rigs[0].OTA = ""
	c2.Rigs[0].Session = ""
	c2.Templates.Light = "<TARGET>/NIGHT_<n>"
	c2.Templates.Cal = map[model.FrameType]string{
		model.TypeDark: "<SENSOR>/DARK/<EXPOSURE>",
		model.TypeFlat: "<SENSOR>/FLAT/<FILTER>",
		model.TypeBias: "<SENSOR>/BIAS/<GAIN>",
	}
	if ps := Validate(&c2, nil); hasFieldError(ps, "rigs[r1].ota") || hasFieldError(ps, "rigs[r1].session") {
		t.Fatalf("blank OTA/Session must not error when no template uses the token: %+v", ps)
	}
}

// A non-positive poll interval must be rejected at validation, since it would
// otherwise panic the worker ticker at runtime.
func TestValidateCadenceRejectsNonPositive(t *testing.T) {
	c := *Defaults()
	c.Defaults.PollInterval = model.Duration(0)
	if !hasFieldError(Validate(&c, nil), "defaults.poll_interval") {
		t.Fatal("zero poll interval must be an error")
	}
}

// A misspelled template token must be rejected rather than rendered as a literal
// folder name.
func TestValidateTemplateRejectsUnknownToken(t *testing.T) {
	c := *Defaults()
	c.Templates.Light = "<TARGET>/<SERSSION>/NIGHT_<n>"
	if !hasFieldError(Validate(&c, nil), "templates.light") {
		t.Fatal("unknown template token must be an error")
	}
}

// The date-part tokens <YEAR>/<MONTH>/<DAY> are canonical and must pass
// validation alongside <n>, while a misspelling of one still fails.
func TestValidateTemplateAcceptsDatePartTokens(t *testing.T) {
	c := *Defaults()
	c.Templates.Light = "<TARGET>/<YEAR>/<MONTH>/<DAY>/NIGHT_<n>"
	if hasFieldError(Validate(&c, nil), "templates.light") {
		t.Fatal("<YEAR>/<MONTH>/<DAY> must be accepted")
	}

	c.Templates.Light = "<TARGET>/<YEARR>/NIGHT_<n>"
	if !hasFieldError(Validate(&c, nil), "templates.light") {
		t.Fatal("a misspelled date-part token must be an error")
	}
}

func hasFieldError(ps []Problem, field string) bool {
	for _, p := range ps {
		if p.Severity == SeverityError && p.Field == field {
			return true
		}
	}
	return false
}

func ptr(c model.Config) *model.Config { return &c }
