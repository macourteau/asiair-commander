package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// fakeVolume is a test VolumeResolver (and optionally HostResolver).
type fakeVolume struct {
	same    bool
	err     error
	addrs   map[string]string // host -> resolved address
	resolve bool
}

func (f fakeVolume) SameVolume(paths ...string) (bool, error) { return f.same, f.err }

func (f fakeVolume) Resolve(host string) (string, error) {
	if !f.resolve {
		return "", errors.New("no resolver")
	}
	if a, ok := f.addrs[host]; ok {
		return a, nil
	}
	return host, nil
}

func TestLoadNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Defaults()
	cfg.PasswordHash = "$2a$12$abcdefghijklmnopqrstuv"
	cfg.SessionSecret = "c2VjcmV0LXNlc3Npb24tYnl0ZXM="
	cfg.Rigs = []model.RigProfile{{
		ID: "rig-01", Name: "Main", Host: "asiair-main", Password: "12345678",
		Sources: []model.Source{{Share: "Udisk Images", Roots: []string{"Autorun"}}},
	}}

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if cfg.SchemaVersion != model.CurrentSchemaVersion {
		t.Errorf("Save did not stamp SchemaVersion, got %d", cfg.SchemaVersion)
	}
	if cfg.UpdatedAt == 0 {
		t.Errorf("Save did not stamp UpdatedAt")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, cfg)
	}
}

func TestSaveAtomicNoTempLeftAndBak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := Save(path, Defaults()); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// Second save produces a .bak of the first file.
	if err := Save(path, Defaults()); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("expected .bak after second save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSecretStoredWrapped(t *testing.T) {
	// On non-Windows the stored form is base64(plaintext); assert the raw file does
	// not carry the plaintext secret verbatim and that Load recovers it.
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Defaults()
	cfg.SessionSecret = "plaintext-secret-value"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "plaintext-secret-value") {
		t.Errorf("plaintext secret found in stored file")
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.SessionSecret != "plaintext-secret-value" {
		t.Errorf("secret not recovered: %q", got.SessionSecret)
	}
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "schema_version: 999\nweb_port: 8686\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("want ErrSchemaTooNew, got %v", err)
	}
}

func TestMigrateV1EnablesPreviews(t *testing.T) {
	// A pre-feature (v1) config has none of the preview_* keys. After load it must
	// migrate to the current schema with previews ENABLED and valid bounds, not the
	// Go zero values (disabled / 0 concurrency / 0 cap) that would fail Validate.
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "schema_version: 1\nweb_port: 8686\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	if cfg.SchemaVersion != model.CurrentSchemaVersion {
		t.Errorf("schema not migrated: %d", cfg.SchemaVersion)
	}
	if !cfg.PreviewsEnabled {
		t.Error("previews should be enabled after migration")
	}
	if cfg.PreviewConcurrency < 1 || cfg.PreviewCacheMaxBytes < 256*1024*1024 {
		t.Errorf("migrated bounds invalid: conc=%d cap=%d", cfg.PreviewConcurrency, cfg.PreviewCacheMaxBytes)
	}
	for _, p := range Validate(cfg, nil) {
		if p.Severity == SeverityError && (p.Field == "preview_concurrency" || p.Field == "preview_cache_max_bytes") {
			t.Errorf("migrated config fails validation: %+v", p)
		}
	}
}

func TestDefaultsValidateClean(t *testing.T) {
	ps := Validate(Defaults(), nil)
	for _, p := range ps {
		if p.Severity == SeverityError {
			t.Errorf("Defaults produced error: %+v", p)
		}
	}
	if HasErrors(ps) {
		t.Errorf("Defaults must have no validation errors, got %+v", ps)
	}
}

func TestValidateErrors(t *testing.T) {
	base := func() *model.Config { return Defaults() }

	tests := []struct {
		name   string
		mutate func(*model.Config)
		field  string
	}{
		{"light template missing night token", func(c *model.Config) {
			c.Templates.Light = "<TARGET>/<OTA>/<SESSION>"
		}, "templates.light"},
		{"template with dotdot", func(c *model.Config) {
			c.Templates.Light = "<TARGET>/../NIGHT_<n>"
		}, "templates.light"},
		{"absolute template", func(c *model.Config) {
			c.Templates.Light = "/abs/<TARGET>/NIGHT_<n>"
		}, "templates.light"},
		{"drive letter template", func(c *model.Config) {
			c.Templates.Light = "C:/<TARGET>/NIGHT_<n>"
		}, "templates.light"},
		{"bad date format", func(c *model.Config) {
			c.DateFormat = "not-a-layout"
		}, "date_format"},
		{"sidecar not subset of exclude", func(c *model.Config) {
			c.DeleteSidecarExtensions = []string{"png"}
		}, "delete_sidecar_extensions"},
		{"sidecar overlaps science", func(c *model.Config) {
			c.ExcludeExtensions = []string{"jpg", "jpeg", "fit"}
			c.DeleteSidecarExtensions = []string{"fit"}
		}, "delete_sidecar_extensions"},
		{"max concurrent zero", func(c *model.Config) {
			c.MaxConcurrentTransfers = 0
		}, "max_concurrent_transfers"},
		{"web port out of range", func(c *model.Config) {
			c.WebPort = 70000
		}, "web_port"},
		{"grace zero on rig", func(c *model.Config) {
			zero := model.Duration(0)
			c.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h", Grace: &zero}}
		}, "rigs[rig-01].grace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			ps := Validate(c, nil)
			if !HasErrors(ps) {
				t.Fatalf("expected an error, got %+v", ps)
			}
			found := false
			for _, p := range ps {
				if p.Severity == SeverityError && p.Field == tt.field {
					found = true
				}
			}
			if !found {
				t.Errorf("expected error on field %q, got %+v", tt.field, ps)
			}
		})
	}
}

func TestValidateGraceKeepOriginals(t *testing.T) {
	zero := model.Duration(0)

	// A keep-originals rig never deletes, so grace=0 must be tolerated.
	keep := Defaults()
	keep.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h", Grace: &zero, KeepOriginals: true}}
	if ps := Validate(keep, nil); HasErrors(ps) {
		t.Fatalf("grace=0 with keep_originals must not error, got %+v", ps)
	}

	// The same rig that deletes still requires grace > 0.
	del := Defaults()
	del.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h", Grace: &zero, KeepOriginals: false}}
	ps := Validate(del, nil)
	found := false
	for _, p := range ps {
		if p.Severity == SeverityError && p.Field == "rigs[rig-01].grace" {
			found = true
		}
	}
	if !found {
		t.Errorf("grace=0 while deleting must error, got %+v", ps)
	}
}

func TestValidateClockSyncNeedsGUID(t *testing.T) {
	cases := []struct {
		name string
		rig  model.RigProfile
		want bool
	}{
		{"guid-less opt-in warns", model.RigProfile{ID: "rig-01", Host: "h", SyncClock: true}, true},
		{"guid-bound opt-in is fine", model.RigProfile{ID: "rig-01", GUID: "g1", SyncClock: true}, false},
		{"opt-out is fine", model.RigProfile{ID: "rig-01", Host: "h", SyncClock: false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			tc.rig.Grace = gracePtr(time.Hour)
			c.Rigs = []model.RigProfile{tc.rig}
			ps := Validate(c, nil)
			if HasErrors(ps) {
				t.Fatalf("clock sync must never error, got %+v", ps)
			}
			found := false
			for _, p := range ps {
				if p.Severity == SeverityWarning && p.Field == "rigs[rig-01].sync_clock" {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("sync_clock warning = %v, want %v (problems %+v)", found, tc.want, ps)
			}
		})
	}
}

func TestValidateOneVolumeError(t *testing.T) {
	ps := Validate(Defaults(), fakeVolume{same: false})
	if !HasErrors(ps) {
		t.Fatalf("expected one-volume error, got %+v", ps)
	}
	ps = Validate(Defaults(), fakeVolume{same: true})
	if HasErrors(ps) {
		t.Fatalf("same-volume should pass, got %+v", ps)
	}
}

func TestValidateUdiskWithoutEMMCWarning(t *testing.T) {
	c := Defaults()
	c.Rigs = []model.RigProfile{{
		ID: "rig-01", Host: "h",
		Sources: []model.Source{{Share: "Udisk Images", Roots: []string{"Autorun"}}},
	}}
	ps := Validate(c, nil)
	if HasErrors(ps) {
		t.Fatalf("unexpected error: %+v", ps)
	}
	found := false
	for _, p := range ps {
		if p.Severity == SeverityWarning && strings.Contains(p.Field, "sources") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Udisk-without-EMMC warning, got %+v", ps)
	}
}

func TestValidateDuplicateHostWarning(t *testing.T) {
	c := Defaults()
	c.Rigs = []model.RigProfile{
		{ID: "rig-01", Host: "asiair-main", Grace: gracePtr(time.Hour)},
		{ID: "rig-02", Host: "ASIAIR-MAIN", Grace: gracePtr(time.Hour)},
	}
	ps := Validate(c, nil)
	found := false
	for _, p := range ps {
		if p.Severity == SeverityWarning && p.Field == "rigs[rig-02].host" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected duplicate-host warning, got %+v", ps)
	}
}

func TestValidateLongPathWarning(t *testing.T) {
	c := Defaults()
	c.Roots.Cal = strings.Repeat("D", 200)
	ps := Validate(c, nil)
	found := false
	for _, p := range ps {
		if p.Severity == SeverityWarning && strings.HasPrefix(p.Field, "templates.cal") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected long-path warning, got %+v", ps)
	}
}

func TestNormalizeTemplates(t *testing.T) {
	c := Defaults()
	c.Templates.Light = `<TARGET>\<OTA>\NIGHT_<n>`
	c.Templates.Cal[model.TypeDark] = `<SENSOR>\DARK\<DATE>`
	c.Templates.Cal[model.TypeFlat] = "<SENSOR>/FLAT/<DATE>" // already canonical

	NormalizeTemplates(c)

	if c.Templates.Light != "<TARGET>/<OTA>/NIGHT_<n>" {
		t.Errorf("light = %q, want canonical forward slashes", c.Templates.Light)
	}
	if c.Templates.Cal[model.TypeDark] != "<SENSOR>/DARK/<DATE>" {
		t.Errorf("dark = %q, want canonical forward slashes", c.Templates.Cal[model.TypeDark])
	}
	if c.Templates.Cal[model.TypeFlat] != "<SENSOR>/FLAT/<DATE>" {
		t.Errorf("flat = %q, must be left untouched", c.Templates.Cal[model.TypeFlat])
	}
}

func TestRedact(t *testing.T) {
	c := Defaults()
	c.PasswordHash = "hash"
	c.SessionSecret = "secret"
	c.Rigs = []model.RigProfile{{ID: "rig-01", Password: "pw"}}

	r := Redact(c)
	if r.PasswordHash != model.RedactedSentinel {
		t.Errorf("password_hash not redacted: %q", r.PasswordHash)
	}
	if r.SessionSecret != model.RedactedSentinel {
		t.Errorf("session_secret not redacted: %q", r.SessionSecret)
	}
	if r.Rigs[0].Password != model.RedactedSentinel {
		t.Errorf("rig password not redacted: %q", r.Rigs[0].Password)
	}
	// Original untouched (deep copy).
	if c.PasswordHash != "hash" || c.Rigs[0].Password != "pw" {
		t.Errorf("Redact mutated original")
	}
	// Empty secret stays empty (first-run signal).
	empty := Redact(Defaults())
	if empty.PasswordHash != "" || empty.SessionSecret != "" {
		t.Errorf("empty secrets should not be redacted to sentinel")
	}
}

func TestApplyRedactedWrite(t *testing.T) {
	stored := Defaults()
	stored.PasswordHash = "stored-hash"
	stored.SessionSecret = "stored-secret"
	stored.Rigs = []model.RigProfile{{ID: "rig-01", Password: "stored-pw"}}

	incoming := Redact(stored) // all secrets are the sentinel
	incoming.PasswordHash = "new-hash"

	out := ApplyRedactedWrite(stored, incoming)
	if out.PasswordHash != "new-hash" {
		t.Errorf("non-sentinel should replace: %q", out.PasswordHash)
	}
	if out.SessionSecret != "stored-secret" {
		t.Errorf("sentinel should preserve stored secret: %q", out.SessionSecret)
	}
	if out.Rigs[0].Password != "stored-pw" {
		t.Errorf("sentinel rig password should preserve: %q", out.Rigs[0].Password)
	}
}

// TestApplyRedactedWritePreservesSourceDisabled guards the per-share disable flag
// across the redact/clone round-trip every API save funnels through. cloneRig
// rebuilds each Source with a struct literal, so a field omitted there is silently
// dropped on save -- which would make disabling a share a no-op and leave the
// disable guard unreachable.
func TestApplyRedactedWritePreservesSourceDisabled(t *testing.T) {
	stored := Defaults()
	stored.Rigs = []model.RigProfile{{ID: "rig-01", Sources: []model.Source{
		{Share: "EMMC Images", Roots: []string{"Autorun"}},
		{Share: "TF Images", Roots: []string{"Autorun"}, Disabled: true},
	}}}
	incoming := Redact(stored)

	out := ApplyRedactedWrite(stored, incoming)
	got := out.Rig("rig-01").Sources
	if len(got) != 2 {
		t.Fatalf("sources = %d, want 2", len(got))
	}
	if got[0].Disabled {
		t.Errorf("EMMC Images should stay enabled")
	}
	if !got[1].Disabled {
		t.Errorf("TF Images Disabled was dropped by the redact/clone round-trip")
	}
}

func TestCheckVersion(t *testing.T) {
	stored := &model.Config{UpdatedAt: 100}
	if err := CheckVersion(stored, &model.Config{UpdatedAt: 100}); err != nil {
		t.Errorf("matching versions should pass: %v", err)
	}
	if err := CheckVersion(stored, &model.Config{UpdatedAt: 99}); !errors.Is(err, ErrStaleWrite) {
		t.Errorf("stale write should be rejected, got %v", err)
	}
}

func gracePtr(d time.Duration) *model.Duration {
	v := model.Duration(d)
	return &v
}

// TestWriteFileRestrictedReplacesUnwritableTarget: on a config migration, Load
// refreshes config.yaml.bak, and an existing .bak written under a different
// principal cannot be overwritten in place by the least-privilege Windows service
// account ("Access is denied"), so the write must replace it rather than truncate it. A read-only existing target models that ACL cross-platform: an in-place
// O_TRUNC open fails, an atomic temp+rename must succeed.
func TestWriteFileRestrictedReplacesUnwritableTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml.bak")
	if err := os.WriteFile(path, []byte("old backup"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := writeFileRestricted(path, []byte("new backup")); err != nil {
		t.Fatalf("writeFileRestricted over a read-only target: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new backup" {
		t.Errorf("content = %q, want %q", got, "new backup")
	}
}

// TestLoadMigrationSurvivesUnwritableBak asserts a config that needs migration still
// loads (migrated in memory) even when the .bak cannot be refreshed — a backup
// failure must never block startup. The config dir is made read-only so the .bak
// write fails; Load must still return the forward-migrated config.
func TestLoadMigrationSurvivesUnwritableBak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// A minimal older-schema config (schema_version 1 → forces a forward migration).
	if err := os.WriteFile(path, []byte("schema_version: 1\nweb_port: 8686\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the directory read-only so any .bak write (in-place or temp+rename) fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load must survive a failed .bak refresh, got: %v", err)
	}
	if cfg.SchemaVersion != model.CurrentSchemaVersion {
		t.Errorf("config not migrated: schema_version = %d, want %d", cfg.SchemaVersion, model.CurrentSchemaVersion)
	}
}

// TestLoadIgnoresUndefinedTLSKey covers design architecture §8.5 invariant 6: a
// config.yaml carrying a tls block, a key the schema does not define, must still
// load. The loader is non-strict, so the key is ignored rather than rejected; it
// disappears on the next Save.
func TestLoadIgnoresUndefinedTLSKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := fmt.Sprintf("schema_version: %d\nweb_port: 8686\ntls:\n  enabled: true\n",
		model.CurrentSchemaVersion)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with an undefined tls key: %v", err)
	}
	if cfg.WebPort != 8686 {
		t.Errorf("web_port = %d, want 8686", cfg.WebPort)
	}
}

// TestUpdateCheckDefaultsOnWithoutTheKey: a config written before the update
// check existed has no update_check_disabled key and must load with checking
// on, at the same schema version and with no migration; an explicit opt-out
// round-trips (design features §7).
func TestUpdateCheckDefaultsOnWithoutTheKey(t *testing.T) {
	if Defaults().UpdateCheckDisabled {
		t.Fatal("Defaults() ships with the update check off")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := fmt.Sprintf("schema_version: %d\nweb_port: 8686\n", model.CurrentSchemaVersion)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UpdateCheckDisabled {
		t.Error("a config without update_check_disabled loaded with the check off")
	}

	cfg.UpdateCheckDisabled = true
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.UpdateCheckDisabled {
		t.Error("update_check_disabled: true did not survive a save and reload")
	}
}
