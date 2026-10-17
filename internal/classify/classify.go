// Package classify implements the pure Classifier: it turns a parsed FITS
// header into a model.Classification using an editable alias table and
// sensor map. It performs no I/O and never panics on odd header values —
// a non-numeric where numeric is expected simply leaves the field absent,
// letting the Router decide whether that absence is routing-critical.
package classify

import (
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// dateObsLayouts are tried in order against DATE-OBS. The card carries no
// zone (e.g. "2026-06-19T01:02:22.031444"); a trailing "Z" is tolerated too.
var dateObsLayouts = []string{
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
}

// Classify derives a Classification from h using aliasTable to resolve
// IMAGETYP (matched case-insensitively on both sides) and sensorMap to
// resolve INSTRUME (matched exactly on the trimmed value). It is pure and
// never panics: header coercion failures just leave the affected field's
// presence flag false.
func Classify(h model.Header, aliasTable map[string]string, sensorMap map[string]string) model.Classification {
	var c model.Classification

	c.Type, c.RawImageType = classifyType(h, aliasTable)

	if instrument, ok := h.Str("INSTRUME"); ok {
		c.Instrument = instrument
		if sensor, ok := sensorMap[instrument]; ok {
			c.Sensor = sensor
			c.SensorMapped = true
		}
	}

	if target, ok := h.Str("OBJECT"); ok {
		c.Target = target
		c.TargetPresent = true
	}

	if filter, ok := h.Str("FILTER"); ok {
		c.Filter = filter
		c.FilterPresent = true
	}

	c.Exptime, c.ExptimePresent = h.Float("EXPTIME")
	c.SetTempC, c.SetTempPresent = h.Float("SET-TEMP") // never CCD-TEMP — see design routing-and-filing §1
	c.Gain, c.GainPresent = h.Int("GAIN")
	c.Offset, c.OffsetPresent = h.Int("OFFSET")

	if binX, ok := h.Int("XBINNING"); ok {
		c.BinX = binX
		c.BinPresent = true
		if binY, ok := h.Int("YBINNING"); ok {
			c.BinY = binY
		} else {
			c.BinY = binX
		}
	}

	c.DateObs, c.DateObsPresent = parseDateObs(h)

	return c
}

// classifyType resolves IMAGETYP through aliasTable, matching case-
// insensitively on both the header value and each alias key. It returns the
// canonical type and the normalized (trimmed) IMAGETYP value — the latter is
// what an unknown-type alert names, so it is returned even on a match.
func classifyType(h model.Header, aliasTable map[string]string) (model.FrameType, string) {
	raw, ok := h.Str("IMAGETYP")
	if !ok {
		return model.TypeUnknown, ""
	}

	needle := strings.ToLower(raw)
	for alias, canonical := range aliasTable {
		if strings.ToLower(alias) == needle {
			return model.CanonicalizeType(canonical), raw
		}
	}
	return model.TypeUnknown, raw
}

// parseDateObs parses DATE-OBS as UTC (the card carries no zone). A trailing
// "Z" is stripped before trying each layout so a zoned card still parses.
func parseDateObs(h model.Header) (time.Time, bool) {
	raw, ok := h.Str("DATE-OBS")
	if !ok {
		return time.Time{}, false
	}
	raw = strings.TrimSuffix(raw, "Z")
	for _, layout := range dateObsLayouts {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
