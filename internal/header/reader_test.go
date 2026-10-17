package header

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// fixture reads and parses one testdata file, failing the test on any error.
func fixture(t *testing.T, name string) model.Header {
	t.Helper()
	h, err := New().ReadHeader(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("ReadHeader(%s): %v", name, err)
	}
	return h
}

func mustStr(t *testing.T, h model.Header, key string) string {
	t.Helper()
	v, ok := h.Str(key)
	if !ok {
		t.Fatalf("missing string key %q", key)
	}
	return v
}

func mustInt(t *testing.T, h model.Header, key string) int64 {
	t.Helper()
	v, ok := h.Int(key)
	if !ok {
		t.Fatalf("key %q not coercible to int", key)
	}
	return v
}

func mustFloat(t *testing.T, h model.Header, key string) float64 {
	t.Helper()
	v, ok := h.Float(key)
	if !ok {
		t.Fatalf("key %q not coercible to float", key)
	}
	return v
}

func TestParseBias(t *testing.T) {
	h := fixture(t, "asiair_bias_6200mm.fit")

	if got := mustStr(t, h, "IMAGETYP"); got != "Bias" {
		t.Errorf("IMAGETYP = %q, want %q (FITS pad must be trimmed)", got, "Bias")
	}
	if got := mustFloat(t, h, "EXPTIME"); got != 0.00100000004749745 {
		t.Errorf("EXPTIME = %v, want 0.00100000004749745", got)
	}
	if got := mustInt(t, h, "SET-TEMP"); got != -10 {
		t.Errorf("SET-TEMP int = %d, want -10", got)
	}
	if got := mustFloat(t, h, "SET-TEMP"); got != -10 {
		t.Errorf("SET-TEMP float = %v, want -10", got)
	}
	// CCD-TEMP is a distinct float, not the setpoint.
	if got := mustFloat(t, h, "CCD-TEMP"); got != -2.09999990463257 {
		t.Errorf("CCD-TEMP = %v, want -2.09999990463257", got)
	}
	if got := mustStr(t, h, "INSTRUME"); got != "ZWO ASI6200MM Pro" {
		t.Errorf("INSTRUME = %q, want %q", got, "ZWO ASI6200MM Pro")
	}
}

func TestParseDark(t *testing.T) {
	h := fixture(t, "asiair_dark_6200mm.fit")

	if got := mustStr(t, h, "IMAGETYP"); got != "Dark" {
		t.Errorf("IMAGETYP = %q, want %q", got, "Dark")
	}
	if got := mustFloat(t, h, "EXPTIME"); got != 300 {
		t.Errorf("EXPTIME = %v, want 300", got)
	}
	if got := mustStr(t, h, "INSTRUME"); got != "ZWO ASI6200MM Pro" {
		t.Errorf("INSTRUME = %q, want %q", got, "ZWO ASI6200MM Pro")
	}
}

func TestParseFlat(t *testing.T) {
	h := fixture(t, "asiair_flat_6200mm_ha.fit")

	if got := mustStr(t, h, "IMAGETYP"); got != "Flat" {
		t.Errorf("IMAGETYP = %q, want %q", got, "Flat")
	}
	if got := mustStr(t, h, "FILTER"); got != "HA" {
		t.Errorf("FILTER = %q, want %q", got, "HA")
	}
	if got := mustFloat(t, h, "EXPTIME"); got != 1.67499804496765 {
		t.Errorf("EXPTIME = %v, want 1.67499804496765", got)
	}
}

func TestParseLight2600mm(t *testing.T) {
	h := fixture(t, "asiair_light_2600mm_ha.fit")

	if got := mustStr(t, h, "IMAGETYP"); got != "Light" {
		t.Errorf("IMAGETYP = %q, want %q", got, "Light")
	}
	if got := mustStr(t, h, "OBJECT"); got != "C-2025 A6" {
		t.Errorf("OBJECT = %q, want %q (internal space preserved)", got, "C-2025 A6")
	}
	if got := mustStr(t, h, "FILTER"); got != "HA" {
		t.Errorf("FILTER = %q, want %q", got, "HA")
	}
	if got := mustStr(t, h, "INSTRUME"); got != "ZWO ASI2600MM Pro" {
		t.Errorf("INSTRUME = %q, want %q", got, "ZWO ASI2600MM Pro")
	}
}

func TestParseLight294mc(t *testing.T) {
	h := fixture(t, "asiair_light_294mc_osc.fit")

	if h.Has("FILTER") {
		t.Errorf("Has(FILTER) = true, want false (OSC frame has no filter card)")
	}
	if got := mustInt(t, h, "GAIN"); got != 121 {
		t.Errorf("GAIN = %d, want 121", got)
	}
	if got := mustInt(t, h, "OFFSET"); got != 30 {
		t.Errorf("OFFSET = %d, want 30", got)
	}
	if got := mustStr(t, h, "INSTRUME"); got != "ZWO ASI294MC Pro" {
		t.Errorf("INSTRUME = %q, want %q", got, "ZWO ASI294MC Pro")
	}
}

// TestExptimeCoercion proves EXPTIME written int-like as "300." (a float token)
// coerces cleanly to both Float and Int through the model accessors.
func TestExptimeCoercion(t *testing.T) {
	h := fixture(t, "asiair_dark_6200mm.fit")

	f, ok := h.Float("EXPTIME")
	if !ok || f != 300 {
		t.Errorf("Float(EXPTIME) = %v, %v; want 300, true", f, ok)
	}
	i, ok := h.Int("EXPTIME")
	if !ok || i != 300 {
		t.Errorf("Int(EXPTIME) = %v, %v; want 300, true", i, ok)
	}
	// Confirm the card really is a float64, not an int64, so this exercises the
	// float-to-int coercion path rather than a plain int read.
	c, _ := h.Card("EXPTIME")
	if _, isFloat := c.Value.(float64); !isFloat {
		t.Errorf("EXPTIME value type = %T, want float64 (from token \"300.\")", c.Value)
	}
}

// TestFixturesCarryNoPII is the design architecture §14.2 privacy guard: every
// committed header fixture must be free of site/observer keywords.
func TestFixturesCarryNoPII(t *testing.T) {
	err := filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".fit", ".fits", ".hdr":
		default:
			return nil
		}

		h, err := New().ReadHeader(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return nil
		}
		for _, key := range h.Keys() {
			if isPIIKey(key) {
				t.Errorf("%s: PII keyword %q present in a committed fixture", path, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk testdata: %v", err)
	}
}

// isPIIKey reports whether a normalized keyword is on the privacy denylist or is
// a GPS* keyword.
func isPIIKey(key string) bool {
	if strings.HasPrefix(key, "GPS") {
		return true
	}
	for _, banned := range PIIDenylist {
		if strings.EqualFold(key, banned) {
			return true
		}
	}
	return false
}

func TestMalformedInputErrors(t *testing.T) {
	dir := t.TempDir()

	cases := map[string][]byte{
		"garbage_short.fit": []byte("this is not a FITS file"),
		"garbage_block.fit": func() []byte {
			b := make([]byte, blockSize)
			for i := range b {
				b[i] = 'x' // a full block of garbage, no END card
			}
			return b
		}(),
		"empty.fit": {},
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, data, 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			// Must return an error and must not panic.
			if _, err := New().ReadHeader(p); err == nil {
				t.Errorf("ReadHeader(%s) = nil error, want error", name)
			}
		})
	}
}

func TestFakeReader(t *testing.T) {
	want := fixture(t, "asiair_bias_6200mm.fit")
	f := Fake{Headers: map[string]model.Header{"bias": want}}

	got, err := f.ReadHeader("bias")
	if err != nil {
		t.Fatalf("Fake.ReadHeader(bias): %v", err)
	}
	if v, _ := got.Str("IMAGETYP"); v != "Bias" {
		t.Errorf("fake IMAGETYP = %q, want Bias", v)
	}
	if _, err := f.ReadHeader("missing"); err == nil {
		t.Errorf("Fake.ReadHeader(missing) = nil error, want error")
	}
}
