package model

import (
	"encoding/json"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAsInt(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{int64(100), 100, true},
		{float64(300), 300, true},
		{float64(300.0), 300, true},
		{float64(1.5), 0, false},
		{"121", 121, true},
		{" 50 ", 50, true},
		{"300.0", 300, true},
		{"1.5", 0, false},
		{"abc", 0, false},
		{true, 0, false},
	}
	for _, c := range cases {
		got, ok := AsInt(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("AsInt(%#v) = %d,%v; want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAsFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(0.00100000004749745), 0.00100000004749745, true},
		{int64(300), 300, true},
		{"1.675", 1.675, true},
		{"abc", 0, false},
		{true, 0, false},
	}
	for _, c := range cases {
		got, ok := AsFloat(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("AsFloat(%#v) = %g,%v; want %g,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestHeaderAccessors(t *testing.T) {
	h := NewHeader([]Card{
		{Key: "imagetyp", Value: "Bias", Comment: "type"},
		{Key: "SET-TEMP", Value: int64(-10)},
		{Key: "EXPTIME", Value: float64(0.00100000004749745)},
		{Key: "INSTRUME", Value: "ZWO ASI6200MM Pro"},
	})
	if v, ok := h.Str("IMAGETYP"); !ok || v != "Bias" {
		t.Errorf("Str(IMAGETYP) = %q,%v", v, ok)
	}
	if v, ok := h.Str("instrume"); !ok || v != "ZWO ASI6200MM Pro" {
		t.Errorf("case-insensitive key lookup failed: %q,%v", v, ok)
	}
	if v, ok := h.Int("SET-TEMP"); !ok || v != -10 {
		t.Errorf("Int(SET-TEMP) = %d,%v", v, ok)
	}
	if v, ok := h.Float("EXPTIME"); !ok || v != 0.00100000004749745 {
		t.Errorf("Float(EXPTIME) = %g,%v", v, ok)
	}
	if h.Has("FILTER") {
		t.Error("Has(FILTER) should be false")
	}
	if _, ok := h.Int("MISSING"); ok {
		t.Error("Int(MISSING) should be false")
	}
}

func TestCanonicalizeType(t *testing.T) {
	cases := map[string]FrameType{
		"LIGHT": TypeLight, "light": TypeLight, " Dark ": TypeDark,
		"FLAT": TypeFlat, "bias": TypeBias, "junk": TypeUnknown, "": TypeUnknown,
	}
	for in, want := range cases {
		if got := CanonicalizeType(in); got != want {
			t.Errorf("CanonicalizeType(%q) = %q; want %q", in, got, want)
		}
	}
	if !TypeDark.IsCalibration() || TypeLight.IsCalibration() {
		t.Error("IsCalibration wrong")
	}
	if !TypeBias.Known() || TypeUnknown.Known() {
		t.Error("Known wrong")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	type holder struct {
		G Duration `yaml:"g" json:"g"`
	}
	h := holder{G: Duration(90 * time.Minute)}
	yb, err := yaml.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(yb); got != "g: 1h30m0s\n" {
		t.Errorf("yaml marshal = %q", got)
	}
	var back holder
	if err := yaml.Unmarshal([]byte("g: 45s\n"), &back); err != nil {
		t.Fatal(err)
	}
	if back.G.D() != 45*time.Second {
		t.Errorf("yaml unmarshal = %v", back.G.D())
	}
	jb, _ := json.Marshal(h)
	if string(jb) != `{"g":"1h30m0s"}` {
		t.Errorf("json marshal = %s", jb)
	}
	var jback holder
	if err := json.Unmarshal([]byte(`{"g":"120s"}`), &jback); err != nil {
		t.Fatal(err)
	}
	if jback.G.D() != 120*time.Second {
		t.Errorf("json unmarshal = %v", jback.G.D())
	}
}

// TestSourceDisabledDefaultsEnabled (D2) pins that an existing config with no
// `disabled` field loads every share enabled — the reason Disabled (zero value =
// enabled) was chosen over Enabled, so no migration is needed. It loads through the
// real YAML path a config.yaml uses.
func TestSourceDisabledDefaultsEnabled(t *testing.T) {
	const y = `
id: rig-01
name: Test
sources:
  - share: EMMC Images
    roots: [Autorun]
  - share: Udisk Images
    roots: [Autorun]
    disabled: true
`
	var p RigProfile
	if err := yaml.Unmarshal([]byte(y), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Sources) != 2 {
		t.Fatalf("sources=%d, want 2", len(p.Sources))
	}
	if p.Sources[0].Disabled {
		t.Fatalf("a source with no `disabled` field loaded disabled; want enabled by default")
	}
	if !p.Sources[1].Disabled {
		t.Fatalf("a source with `disabled: true` loaded enabled")
	}
	enabled := p.EnabledSources()
	if len(enabled) != 1 || enabled[0].Share != "EMMC Images" {
		t.Fatalf("EnabledSources=%v, want only the enabled EMMC Images", enabled)
	}
	disabled := p.DisabledShares()
	if len(disabled) != 1 || !disabled["Udisk Images"] {
		t.Fatalf("DisabledShares=%v, want only Udisk Images", disabled)
	}
}

func TestEffectiveKnobs(t *testing.T) {
	fifteen := Duration(15 * time.Minute)
	cfg := &Config{
		Defaults: RigDefaults{Grace: Duration(time.Hour), PollInterval: Duration(45 * time.Second)},
		Rigs: []RigProfile{
			{ID: "a"},
			{ID: "b", Grace: &fifteen},
		},
	}
	if got := cfg.EffectiveGrace(cfg.Rig("a")); got != time.Hour {
		t.Errorf("rig a grace = %v; want default 1h", got)
	}
	if got := cfg.EffectiveGrace(cfg.Rig("b")); got != 15*time.Minute {
		t.Errorf("rig b grace = %v; want override 15m", got)
	}
	if got := cfg.EffectivePollInterval(cfg.Rig("a")); got != 45*time.Second {
		t.Errorf("rig a poll = %v", got)
	}
	if cfg.Rig("missing") != nil {
		t.Error("Rig(missing) should be nil")
	}
}

// TestCurrentFileProgressed pins the invariant the live transfer feed depends on:
// each mid-transfer progress sample rebuilds Current as a fresh value, and that
// rebuild must carry the file identity (name, share, source path) through so the
// feed keeps matching the transferring row on its rig+share+path key. Only the
// percent and throughput change; the origin marker is left untouched.
func TestCurrentFileProgressed(t *testing.T) {
	base := CurrentFile{File: "Light_0001.fit", Share: "EMMC Images", Source: "Autorun/Light/o/Light_0001.fit", Pct: 0}
	got := base.Progressed(42, 1024)
	if got.File != base.File || got.Share != base.Share || got.Source != base.Source {
		t.Fatalf("Progressed dropped identity: %+v", got)
	}
	if got.Pct != 42 || got.BytesPerSec != 1024 {
		t.Fatalf("Progressed pct=%d bps=%d, want 42/1024", got.Pct, got.BytesPerSec)
	}
	if base.Pct != 0 || base.BytesPerSec != 0 {
		t.Fatalf("Progressed mutated the receiver: %+v", base)
	}
}
