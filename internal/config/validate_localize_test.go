package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// formEditableDurations are the model.Config Duration fields a form can edit,
// keyed by their JSON path (a slice element uses its field tag, unindexed). This
// is the same set the LocalizeDecodeError allowlist walks; TestDurationFieldsCategorized
// asserts reflection over model.Config discovers exactly these plus the denylist.
var formEditableDurations = map[string]bool{
	"missing_display_period":    true,
	"defaults.poll_interval":    true,
	"defaults.retry_interval":   true,
	"defaults.min_file_age":     true,
	"defaults.grace":            true,
	"defaults.reconcile_settle": true,
	"rigs.grace":                true,
	"rigs.poll_interval":        true,
	"rigs.retry_interval":       true,
	"rigs.min_file_age":         true,
	"rigs.reconcile_settle":     true,
}

// notFormEditableDurations are the model.Config Duration fields no form produces —
// so a bad value there is out of per-field surfacing's scope (design
// architecture §8.8) and falls to the generic banner.
var notFormEditableDurations = map[string]bool{
	"defaults.unreachable_alert_after": true,
	"defaults.failed_retry.base":       true,
	"defaults.failed_retry.max":        true,
	"web.session_idle_timeout":         true,
	"web.session_max_age":              true,
	"web.login_backoff_base":           true,
	"web.login_backoff_cap":            true,
}

// TestDurationFieldsCategorized (C3) reflects over every model.Config
// Duration and forces each into exactly one of the form-editable allowlist or the
// not-form-editable denylist. A newly added duration field fails this test until
// it is consciously categorized, so the LocalizeDecodeError allowlist cannot
// silently fall behind the struct.
func TestDurationFieldsCategorized(t *testing.T) {
	found := collectDurationPaths(reflect.TypeOf(model.Config{}), "")
	sort.Strings(found)

	for _, path := range found {
		inAllow := formEditableDurations[path]
		inDeny := notFormEditableDurations[path]
		switch {
		case inAllow && inDeny:
			t.Errorf("duration %q is in BOTH the allow- and deny-list", path)
		case !inAllow && !inDeny:
			t.Errorf("duration %q is categorized in NEITHER list — add it to the form-editable "+
				"allowlist (and defaultsDurationFields/rigDurationFields) or the denylist", path)
		}
	}
	// Every allowlist/denylist entry must correspond to a real struct field, so a
	// removed field can't leave a stale category behind.
	present := make(map[string]bool, len(found))
	for _, p := range found {
		present[p] = true
	}
	for path := range formEditableDurations {
		if !present[path] {
			t.Errorf("allowlist entry %q no longer names a model.Config duration", path)
		}
	}
	for path := range notFormEditableDurations {
		if !present[path] {
			t.Errorf("denylist entry %q no longer names a model.Config duration", path)
		}
	}
}

var durationType = reflect.TypeOf(model.Duration(0))

// collectDurationPaths walks a struct type and returns the JSON path of every
// model.Duration field, descending into nested structs and into the element type
// of a struct slice. A slice contributes its field tag unindexed (e.g. "rigs"),
// mirroring how the validator and localizer normalize per-rig fields.
func collectDurationPaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := jsonKey(f)
		if tag == "" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		switch {
		case ft == durationType:
			out = append(out, path)
		case ft.Kind() == reflect.Struct:
			out = append(out, collectDurationPaths(ft, path)...)
		case ft.Kind() == reflect.Slice:
			out = append(out, collectDurationPaths(ft.Elem(), path)...)
		}
	}
	return out
}

// jsonKey returns a struct field's JSON key (before any ",omitempty"), or "" if it
// is unexported or explicitly skipped with `json:"-"`.
func jsonKey(f reflect.StructField) string {
	if f.PkgPath != "" { // unexported
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	name := strings.Split(tag, ",")[0]
	if name == "-" {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}

// TestValidatorFieldFixture (C3) runs a battery of deliberately invalid
// Configs, collects every Field the validator emits (normalizing rigs[<id>] to
// rigs[*]), and asserts the set EQUALS the checked-in fixture. Adding a validator
// rule without updating both the battery and the fixture fails here, so no backend
// field can silently regress to the banner unnoticed.
func TestValidatorFieldFixture(t *testing.T) {
	got := map[string]bool{}
	for _, cfg := range invalidConfigBattery() {
		for _, p := range Validate(cfg, sameVolFalse{}) {
			got[normalizeRigField(p.Field)] = true
		}
	}

	want := loadFieldFixture(t, filepath.Join("testdata", "validator_fields.txt"))

	gotSorted := sortedKeys(got)
	if !reflect.DeepEqual(gotSorted, want) {
		t.Errorf("emitted validator Field set != fixture\n emitted: %v\n fixture: %v\n"+
			"(update testdata/validator_fields.txt and the web copy, or the battery)",
			gotSorted, want)
	}
}

// sameVolFalse is a VolumeResolver that reports the roots span multiple volumes,
// so validateRoots emits its "roots" Field.
type sameVolFalse struct{}

func (sameVolFalse) SameVolume(paths ...string) (bool, error) { return false, nil }

// invalidConfigBattery returns Configs each tripping a group of validator rules;
// their union covers every Field the fixture lists.
func invalidConfigBattery() []*model.Config {
	// A: whole-config scalar rules + roots + a managed rig missing its bindings,
	// watched shares, OTA/Session, and with non-positive cadence/grace/settle.
	a := Defaults()
	a.DeleteSidecarExtensions = []string{"fit"}    // science ext, not in exclude → delete_sidecar_extensions
	a.Templates.Light = "<TARGET>/<OTA>/<SESSION>" // missing <n> → templates.light;
	// keeps <OTA>/<SESSION> so a blank rig OTA/Session trips rigs[*].ota/.session
	a.Templates.Cal[model.TypeDark] = ".." // ".." segment → templates.cal.DARK
	a.DateFormat = "not-a-layout"          // → date_format
	a.MaxConcurrentTransfers = 0           // → max_concurrent_transfers
	a.WebPort = 0                          // → web_port
	a.PreviewConcurrency = 0               // → preview_concurrency
	a.PreviewCacheMaxBytes = 0             // → preview_cache_max_bytes
	a.PlateSolveRadiusDeg = 0              // → plate_solve_radius_deg
	a.Defaults.PollInterval = 0            // → defaults.poll_interval
	a.Defaults.RetryInterval = 0           // → defaults.retry_interval
	a.Defaults.Grace = 0                   // effective grace 0 → rigs[*].grace
	a.Defaults.ReconcileSettle = 0         // → rigs[*].reconcile_settle
	zero := model.Duration(0)
	a.Rigs = []model.RigProfile{{
		ID:            "rig-a",
		Managed:       true,
		PollInterval:  &zero, // → rigs[*].poll_interval
		RetryInterval: &zero, // → rigs[*].retry_interval
		SyncClock:     true,  // opted in without a guid → rigs[*].sync_clock
		// no GUID/Host → rigs[*].guid; no sources → rigs[*].sources;
		// blank OTA/Session with <OTA>/<SESSION> templates → rigs[*].ota/.session
	}}

	// B: two managed rigs sharing a host → the duplicate-host warning on rigs[*].host.
	b := Defaults()
	b.Rigs = []model.RigProfile{
		{ID: "rig-b1", Managed: true, GUID: "g1", Host: "nas.local", KeepOriginals: true,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
			OTA:     "RC8", Session: "S1"},
		{ID: "rig-b2", Managed: true, GUID: "g2", Host: "nas.local", KeepOriginals: true,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
			OTA:     "RC8", Session: "S1"},
	}

	// C: mosaic detection on with a template that names no panel token before its
	// <n> segment → templates.mosaic. Detection has to be on: the whole field is
	// unvalidated while it is off.
	c := Defaults()
	c.Templates.Mosaic = "<TARGET_BASE>/<OTA>/<SESSION>/NIGHT_<n>"
	c.MosaicDetection = true
	c.Rigs = []model.RigProfile{
		{ID: "rig-c", Managed: true, GUID: "g3", KeepOriginals: true,
			Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
			OTA:     "RC8", Session: "S1"},
	}

	return []*model.Config{a, b, c}
}

// normalizeRigField collapses rigs[<id>] to rigs[*] so the fixture is id-agnostic.
func normalizeRigField(field string) string {
	return rigIDPattern.ReplaceAllString(field, "rigs[*]")
}

var rigIDPattern = regexp.MustCompile(`rigs\[[^\]]+\]`)

func loadFieldFixture(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var lines []string
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	sort.Strings(lines)
	return lines
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
