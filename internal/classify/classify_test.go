package classify

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// seededAliasTable and seededSensorMap mirror the shipped config defaults
// (design architecture §7.2, internal/config/defaults.go).
func seededAliasTable() map[string]string {
	return map[string]string{
		"light":      "LIGHT",
		"dark":       "DARK",
		"flat":       "FLAT",
		"bias":       "BIAS",
		"dark frame": "DARK",
		"bias frame": "BIAS",
	}
}

func seededSensorMap() map[string]string {
	return map[string]string{
		"ZWO ASI6200MM Pro": "6200MM",
		"ZWO ASI2600MM Pro": "2600MM",
		"ZWO ASI294MC Pro":  "294MC",
	}
}

func loadFixture(t *testing.T, name string) model.Header {
	t.Helper()
	h, err := header.New().ReadHeader(filepath.Join("..", "header", "testdata", name))
	if err != nil {
		t.Fatalf("ReadHeader(%s): %v", name, err)
	}
	return h
}

func TestClassify_Fixture_Bias6200mm(t *testing.T) {
	h := loadFixture(t, "asiair_bias_6200mm.fit")
	c := Classify(h, seededAliasTable(), seededSensorMap())

	if c.Type != model.TypeBias {
		t.Errorf("Type = %q, want %q", c.Type, model.TypeBias)
	}
	if !c.SetTempPresent || c.SetTempC != -10 {
		t.Errorf("SetTempC = %v (present=%v), want -10 (present=true) — must read SET-TEMP, not CCD-TEMP", c.SetTempC, c.SetTempPresent)
	}
	if !c.SensorMapped || c.Sensor != "6200MM" {
		t.Errorf("Sensor = %q (mapped=%v), want 6200MM (mapped=true)", c.Sensor, c.SensorMapped)
	}
	if !c.DateObsPresent {
		t.Fatal("DateObsPresent = false, want true")
	}
	want := time.Date(2026, 6, 19, 1, 2, 22, 31444000, time.UTC)
	if !c.DateObs.Equal(want) {
		t.Errorf("DateObs = %v, want %v", c.DateObs, want)
	}
	if c.DateObs.Location() != time.UTC {
		t.Errorf("DateObs location = %v, want UTC", c.DateObs.Location())
	}
}

func TestClassify_Fixture_Dark6200mm(t *testing.T) {
	h := loadFixture(t, "asiair_dark_6200mm.fit")
	c := Classify(h, seededAliasTable(), seededSensorMap())

	if c.Type != model.TypeDark {
		t.Errorf("Type = %q, want %q", c.Type, model.TypeDark)
	}
	if !c.ExptimePresent || c.Exptime != 300 {
		t.Errorf("Exptime = %v (present=%v), want 300 (present=true)", c.Exptime, c.ExptimePresent)
	}
}

func TestClassify_Fixture_Flat6200mmHa(t *testing.T) {
	h := loadFixture(t, "asiair_flat_6200mm_ha.fit")
	c := Classify(h, seededAliasTable(), seededSensorMap())

	if c.Type != model.TypeFlat {
		t.Errorf("Type = %q, want %q", c.Type, model.TypeFlat)
	}
	if !c.FilterPresent || c.Filter != "HA" {
		t.Errorf("Filter = %q (present=%v), want HA (present=true)", c.Filter, c.FilterPresent)
	}
}

func TestClassify_Fixture_Light2600mmHa(t *testing.T) {
	h := loadFixture(t, "asiair_light_2600mm_ha.fit")
	c := Classify(h, seededAliasTable(), seededSensorMap())

	if c.Type != model.TypeLight {
		t.Errorf("Type = %q, want %q", c.Type, model.TypeLight)
	}
	if !c.TargetPresent || c.Target != "C-2025 A6" {
		t.Errorf("Target = %q (present=%v), want %q (present=true)", c.Target, c.TargetPresent, "C-2025 A6")
	}
	if !c.SensorMapped || c.Sensor != "2600MM" {
		t.Errorf("Sensor = %q (mapped=%v), want 2600MM (mapped=true)", c.Sensor, c.SensorMapped)
	}
	if !c.FilterPresent || c.Filter != "HA" {
		t.Errorf("Filter = %q (present=%v), want HA (present=true)", c.Filter, c.FilterPresent)
	}
}

func TestClassify_Fixture_Light294mcOSC(t *testing.T) {
	h := loadFixture(t, "asiair_light_294mc_osc.fit")
	c := Classify(h, seededAliasTable(), seededSensorMap())

	if c.Type != model.TypeLight {
		t.Errorf("Type = %q, want %q", c.Type, model.TypeLight)
	}
	if !c.SensorMapped || c.Sensor != "294MC" {
		t.Errorf("Sensor = %q (mapped=%v), want 294MC (mapped=true)", c.Sensor, c.SensorMapped)
	}
	if c.FilterPresent {
		t.Errorf("FilterPresent = true, want false (OSC camera, no FILTER card)")
	}
	if !c.GainPresent || c.Gain != 121 {
		t.Errorf("Gain = %v (present=%v), want 121 (present=true)", c.Gain, c.GainPresent)
	}
	if !c.OffsetPresent || c.Offset != 30 {
		t.Errorf("Offset = %v (present=%v), want 30 (present=true)", c.Offset, c.OffsetPresent)
	}
}

func TestClassify_Synthetic(t *testing.T) {
	aliases := seededAliasTable()
	sensors := seededSensorMap()

	t.Run("NINA uppercase LIGHT", func(t *testing.T) {
		h := model.NewHeader([]model.Card{{Key: "IMAGETYP", Value: "LIGHT"}})
		c := Classify(h, aliases, sensors)
		if c.Type != model.TypeLight {
			t.Errorf("Type = %q, want %q", c.Type, model.TypeLight)
		}
	})

	t.Run("NINA uppercase FLAT", func(t *testing.T) {
		h := model.NewHeader([]model.Card{{Key: "IMAGETYP", Value: "FLAT"}})
		c := Classify(h, aliases, sensors)
		if c.Type != model.TypeFlat {
			t.Errorf("Type = %q, want %q", c.Type, model.TypeFlat)
		}
	})

	t.Run("Dark Frame alias", func(t *testing.T) {
		h := model.NewHeader([]model.Card{{Key: "IMAGETYP", Value: "Dark Frame"}})
		c := Classify(h, aliases, sensors)
		if c.Type != model.TypeDark {
			t.Errorf("Type = %q, want %q", c.Type, model.TypeDark)
		}
	})

	t.Run("unknown IMAGETYP", func(t *testing.T) {
		h := model.NewHeader([]model.Card{{Key: "IMAGETYP", Value: "Foobar"}})
		c := Classify(h, aliases, sensors)
		if c.Type != model.TypeUnknown {
			t.Errorf("Type = %q, want %q", c.Type, model.TypeUnknown)
		}
		if c.RawImageType != "Foobar" {
			t.Errorf("RawImageType = %q, want %q", c.RawImageType, "Foobar")
		}
	})

	t.Run("absent IMAGETYP", func(t *testing.T) {
		h := model.NewHeader(nil)
		c := Classify(h, aliases, sensors)
		if c.Type != model.TypeUnknown {
			t.Errorf("Type = %q, want %q", c.Type, model.TypeUnknown)
		}
		if c.RawImageType != "" {
			t.Errorf("RawImageType = %q, want empty", c.RawImageType)
		}
	})

	t.Run("INSTRUME not in sensor map", func(t *testing.T) {
		h := model.NewHeader([]model.Card{{Key: "INSTRUME", Value: "Some Unknown Camera"}})
		c := Classify(h, aliases, sensors)
		if c.SensorMapped {
			t.Errorf("SensorMapped = true, want false")
		}
		if c.Instrument != "Some Unknown Camera" {
			t.Errorf("Instrument = %q, want %q", c.Instrument, "Some Unknown Camera")
		}
	})

	t.Run("BinX != BinY", func(t *testing.T) {
		h := model.NewHeader([]model.Card{
			{Key: "XBINNING", Value: int64(1)},
			{Key: "YBINNING", Value: int64(2)},
		})
		c := Classify(h, aliases, sensors)
		if !c.BinPresent || c.BinX != 1 || c.BinY != 2 {
			t.Errorf("BinX=%v BinY=%v present=%v, want 1,2,true", c.BinX, c.BinY, c.BinPresent)
		}
	})

	t.Run("XBINNING present, YBINNING absent defaults BinY to BinX", func(t *testing.T) {
		h := model.NewHeader([]model.Card{
			{Key: "XBINNING", Value: int64(2)},
		})
		c := Classify(h, aliases, sensors)
		if !c.BinPresent || c.BinX != 2 || c.BinY != 2 {
			t.Errorf("BinX=%v BinY=%v present=%v, want 2,2,true", c.BinX, c.BinY, c.BinPresent)
		}
	})
}
