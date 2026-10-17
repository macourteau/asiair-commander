// Package astrometry turns a parsed FITS header into a model.Astrometry: the
// pointing/orientation the ASIAIR already embeds in a light frame's WCS. It mirrors
// the classify boundary — a pure function, no I/O, never panics on odd values — and
// is the Tier-0 producer of the shared model.WCSSolution; a future Tier-1 solver
// becomes a second producer of the same value.
//
// The WCS math (CD matrix → scale, rotation, parity; gnomonic deprojection of the
// image center) follows FITS WCS Paper II (Calabretta & Greisen 2002) and
// astrometry.net's orientation convention (tan_get_orientation), so the reported
// position angle is comparable to what capture software reports rather than ~90°
// off. SIP distortion terms are ignored — they do not affect the center, scale, or
// rotation displayed here.
package astrometry

import (
	"math"
	"strconv"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
)

// degenerateDet is the |det(CD)| floor below which the linear transform carries no
// usable scale/orientation; also guards atan2(0,0) from reporting a bogus 0°.
const degenerateDet = 1e-30

// FromHeader extracts the astrometry from a parsed header. It always returns a
// non-nil *Astrometry: Status is "header" with a Solution when the header carries a
// usable tangent-plane WCS, else "none"; Hints are populated independently whenever
// pointing/optics cards are present.
func FromHeader(h model.Header) *model.Astrometry {
	a := &model.Astrometry{Status: model.AstroStatusNone, Hints: readHints(h)}
	if sol := readSolution(h); sol != nil {
		a.Status = model.AstroStatusHeader
		a.Solution = sol
	}
	return a
}

// cdMatrix is the linear transform from pixel offsets to intermediate world
// coordinates (degrees), CDi_j indexed as [row][col].
type cdMatrix struct {
	c11, c12, c21, c22 float64
}

// readSolution derives the displayable WCS summary, or nil when the header lacks a
// usable tangent-plane solution (wrong/absent CTYPE, no tangent point, or a
// degenerate transform).
func readSolution(h model.Header) *model.WCSSolution {
	ct1, ok1 := h.Str("CTYPE1")
	ct2, ok2 := h.Str("CTYPE2")
	if !ok1 || !ok2 {
		return nil
	}
	// ASIAIR writes RA-first axis order (CTYPE1=RA---TAN-SIP, CTYPE2=DEC--TAN-SIP).
	// A swapped DEC/RA order is treated as no solution rather than silently
	// transposed — Tier 0 does not guess at foreign axis conventions.
	if !strings.HasPrefix(strings.ToUpper(ct1), "RA---") || !strings.HasPrefix(strings.ToUpper(ct2), "DEC--") {
		return nil
	}

	// A tangent point is mandatory to place the center on the sky.
	crval1, okv1 := h.Float("CRVAL1")
	crval2, okv2 := h.Float("CRVAL2")
	if !okv1 || !okv2 {
		return nil
	}

	cd, ok := readCD(h)
	if !ok {
		return nil
	}
	det := cd.c11*cd.c22 - cd.c12*cd.c21
	if math.Abs(det) < degenerateDet {
		return nil
	}

	scale := 3600 * math.Sqrt(math.Abs(det)) // arcsec/px (rotation-invariant plate scale)

	// Position angle, parity-aware (astrometry.net tan_get_orientation), then folded
	// into [0,180). A rectangular sensor's framing is invariant under a 180° rotation
	// (the field footprint is identical and stacking software derotates), so a PA and
	// PA+180 describe the *same* framing — folding them together is what makes the
	// value a true drift signal: a meridian flip reads as ~0° change rather than a
	// spurious ~180°. parity uses det >= 0; the exact-0 case is excluded above.
	parity := 1.0
	if det < 0 {
		parity = -1.0
	}
	t := parity*cd.c11 + cd.c22
	aa := parity*cd.c21 - cd.c12
	pa := normalize180(-math.Atan2(aa, t) * 180 / math.Pi)

	// Image center in pixels and the field of view need NAXIS; without it the center
	// falls back to the tangent point (offset 0) and the FOV is 0.
	crpix1, _ := h.Float("CRPIX1") // absent → 0 per FITS
	crpix2, _ := h.Float("CRPIX2")
	var dx, dy, fovW, fovH float64
	if n1, okn1 := h.Float("NAXIS1"); okn1 {
		if n2, okn2 := h.Float("NAXIS2"); okn2 {
			dx = (n1+1)/2 - crpix1
			dy = (n2+1)/2 - crpix2
			fovW = n1 * scale / 3600
			fovH = n2 * scale / 3600
		}
	}

	centerRA, centerDec := deproject(cd, dx, dy, crval1, crval2)

	return &model.WCSSolution{
		CenterRA:      centerRA,
		CenterDec:     centerDec,
		PositionAngle: pa,
		PixelScale:    scale,
		FOVWidth:      fovW,
		FOVHeight:     fovH,
		Flipped:       det > 0, // normal N-up/E-left sky has det < 0
	}
}

// readCD returns the CD matrix. A present CDi_j card set wins (absent elements
// default to 0, per FITS); otherwise a CDELT1/2 (+ optional CROTA2) pair is
// synthesized into an equivalent CD. Returns false when neither is present.
func readCD(h model.Header) (cdMatrix, bool) {
	if h.Has("CD1_1") || h.Has("CD1_2") || h.Has("CD2_1") || h.Has("CD2_2") {
		var cd cdMatrix
		cd.c11, _ = h.Float("CD1_1")
		cd.c12, _ = h.Float("CD1_2")
		cd.c21, _ = h.Float("CD2_1")
		cd.c22, _ = h.Float("CD2_2")
		return cd, true
	}
	cdelt1, ok1 := h.Float("CDELT1")
	cdelt2, ok2 := h.Float("CDELT2")
	if !ok1 || !ok2 {
		return cdMatrix{}, false
	}
	crota2, _ := h.Float("CROTA2") // absent → 0
	r := crota2 * math.Pi / 180
	cosr, sinr := math.Cos(r), math.Sin(r)
	// Standard AIPS CDELT/CROTA → CD synthesis.
	return cdMatrix{
		c11: cdelt1 * cosr,
		c12: -cdelt2 * sinr,
		c21: cdelt1 * sinr,
		c22: cdelt2 * cosr,
	}, true
}

// deproject maps a pixel offset (dx,dy) from CRPIX through the CD matrix and the
// division-free AIPS TAN (gnomonic) inverse about the tangent point (crval1,crval2,
// degrees), returning the sky coordinate in degrees. At dx=dy=0 it returns the
// tangent point exactly (no 0/0), which is the common centered-CRPIX case.
func deproject(cd cdMatrix, dx, dy, crval1, crval2 float64) (ra, dec float64) {
	// Intermediate world coordinates (degrees) → radians.
	x := (cd.c11*dx + cd.c12*dy) * math.Pi / 180
	y := (cd.c21*dx + cd.c22*dy) * math.Pi / 180
	a0 := crval1 * math.Pi / 180
	d0 := crval2 * math.Pi / 180
	cosd0, sind0 := math.Cos(d0), math.Sin(d0)
	denom := cosd0 - y*sind0
	raRad := a0 + math.Atan2(x, denom)
	decRad := math.Atan2(sind0+y*cosd0, math.Sqrt(denom*denom+x*x))
	return normalizeDeg(raRad * 180 / math.Pi), decRad * 180 / math.Pi
}

// readHints reads the raw solve seeds independently of any solution. Returns nil
// when no pointing/optics card is present.
func readHints(h model.Header) *model.AstroHints {
	var hints model.AstroHints
	present := false
	if ra, ok := objectRA(h); ok {
		hints.RA = &ra
		present = true
	}
	if dec, ok := objectDec(h); ok {
		hints.Dec = &dec
		present = true
	}
	if fl, ok := h.Float("FOCALLEN"); ok {
		hints.FocalLenMM = &fl
		present = true
	}
	if px, ok := h.Float("XPIXSZ"); ok {
		hints.PixelSizeUm = &px
		present = true
	}
	if !present {
		return nil
	}
	return &hints
}

// objectRA returns the object right ascension in degrees: the numeric RA card
// (ASIAIR's degrees convention) if present, else the OBJCTRA sexagesimal string in
// hours converted to degrees.
func objectRA(h model.Header) (float64, bool) {
	if v, ok := h.Float("RA"); ok {
		return v, true
	}
	if s, ok := h.Str("OBJCTRA"); ok {
		if v, ok := parseSexagesimal(s); ok {
			return v * 15, true // hours → degrees
		}
	}
	return 0, false
}

// objectDec returns the object declination in degrees: the numeric DEC card if
// present, else the OBJCTDEC sexagesimal string (already in degrees).
func objectDec(h model.Header) (float64, bool) {
	if v, ok := h.Float("DEC"); ok {
		return v, true
	}
	if s, ok := h.Str("OBJCTDEC"); ok {
		if v, ok := parseSexagesimal(s); ok {
			return v, true
		}
	}
	return 0, false
}

// parseSexagesimal parses "DD MM SS", "DD:MM:SS", or fewer fields (whitespace
// and/or colon separated) into a signed decimal value in the leading unit. The sign
// is taken from a leading '-' on the whole string so a "-00 30 00" keeps its sign
// even though the degrees field is zero.
func parseSexagesimal(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == ':' || r == '\t'
	})
	if len(fields) == 0 {
		return 0, false
	}
	var parts [3]float64
	for i := 0; i < len(fields) && i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return 0, false
		}
		parts[i] = v
	}
	val := parts[0] + parts[1]/60 + parts[2]/3600
	if neg {
		val = -val
	}
	return val, true
}

// normalizeDeg wraps an angle into [0, 360).
func normalizeDeg(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

// normalize180 folds an angle into [0, 180) — the sensor-axis orientation, which is
// what matters for framing (a 180° rotation leaves the footprint unchanged).
func normalize180(d float64) float64 {
	d = math.Mod(d, 180)
	if d < 0 {
		d += 180
	}
	return d
}
