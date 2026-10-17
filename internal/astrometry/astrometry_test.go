package astrometry

import (
	"math"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// scaleDeg is 1 arcsec/px expressed in degrees/px — a convenient round plate scale
// for the hand-computed expectations below.
const scaleDeg = 1.0 / 3600.0

func hdr(cards ...model.Card) model.Header {
	return model.NewHeader(cards)
}

func card(key string, v any) model.Card { return model.Card{Key: key, Value: v} }

// normalWCS is a North-up, East-left (unmirrored) tangent-plane WCS centered on
// (RA,Dec) = (180, 0) over a 100×100 image with CRPIX at the image center. Its CD
// determinant is negative (normal sky), position angle 0, scale 1 arcsec/px.
func normalWCS() model.Header {
	return hdr(
		card("CTYPE1", "RA---TAN-SIP"),
		card("CTYPE2", "DEC--TAN-SIP"),
		card("CRVAL1", 180.0),
		card("CRVAL2", 0.0),
		card("CRPIX1", 50.5),
		card("CRPIX2", 50.5),
		card("CD1_1", -scaleDeg),
		card("CD1_2", 0.0),
		card("CD2_1", 0.0),
		card("CD2_2", scaleDeg),
		card("NAXIS1", int64(100)),
		card("NAXIS2", int64(100)),
	)
}

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%v)", name, got, want, tol)
	}
}

func TestFullCDSolution(t *testing.T) {
	a := FromHeader(normalWCS())
	if a.Status != model.AstroStatusHeader || a.Solution == nil {
		t.Fatalf("status=%q solution=%v, want header + non-nil solution", a.Status, a.Solution)
	}
	s := a.Solution
	approx(t, "center_ra", s.CenterRA, 180, 1e-6)
	approx(t, "center_dec", s.CenterDec, 0, 1e-6)
	approx(t, "pos_angle", s.PositionAngle, 0, 1e-6)
	approx(t, "pixel_scale", s.PixelScale, 1.0, 1e-9)
	approx(t, "fov_w", s.FOVWidth, 100.0/3600.0, 1e-9)
	approx(t, "fov_h", s.FOVHeight, 100.0/3600.0, 1e-9)
	if s.Flipped {
		t.Errorf("Flipped = true, want false for a normal-parity (det<0) frame")
	}
}

// A CDELT/CROTA2-synthesized WCS must produce the same position angle as the
// hand-built CD equivalent — guards the 180° CD-vs-CDELT split and a dropped
// off-diagonal sign. CROTA2=30° with CDELT1<0, CDELT2>0 corresponds to PA 330°.
func TestCDELTMatchesCD(t *testing.T) {
	const crota = 30.0
	r := crota * math.Pi / 180
	cosr, sinr := math.Cos(r), math.Sin(r)
	cdelt1, cdelt2 := -scaleDeg, scaleDeg

	cdHeader := hdr(
		card("CTYPE1", "RA---TAN"), card("CTYPE2", "DEC--TAN"),
		card("CRVAL1", 10.0), card("CRVAL2", 41.0),
		card("CRPIX1", 50.5), card("CRPIX2", 50.5),
		card("CD1_1", cdelt1*cosr), card("CD1_2", -cdelt2*sinr),
		card("CD2_1", cdelt1*sinr), card("CD2_2", cdelt2*cosr),
		card("NAXIS1", int64(100)), card("NAXIS2", int64(100)),
	)
	cdeltHeader := hdr(
		card("CTYPE1", "RA---TAN"), card("CTYPE2", "DEC--TAN"),
		card("CRVAL1", 10.0), card("CRVAL2", 41.0),
		card("CRPIX1", 50.5), card("CRPIX2", 50.5),
		card("CDELT1", cdelt1), card("CDELT2", cdelt2), card("CROTA2", crota),
		card("NAXIS1", int64(100)), card("NAXIS2", int64(100)),
	)

	cdSol := FromHeader(cdHeader).Solution
	cdeltSol := FromHeader(cdeltHeader).Solution
	if cdSol == nil || cdeltSol == nil {
		t.Fatalf("expected both to solve; cd=%v cdelt=%v", cdSol, cdeltSol)
	}
	approx(t, "CDELT vs CD pos_angle", cdeltSol.PositionAngle, cdSol.PositionAngle, 1e-9)
	// Raw orientation is 330°; folded into [0,180) that is 150° (330 − 180).
	approx(t, "pos_angle", cdSol.PositionAngle, 150, 1e-6)
	if cdSol.Flipped {
		t.Errorf("Flipped = true, want false (det<0)")
	}
}

// A frame and its 180°-rotated self (all CD elements negated) must report the SAME
// position angle: the framing footprint is identical, which is the whole point of
// folding PA into [0,180). This is the real-data case (a meridian flip / camera
// reinserted end-for-end between sessions).
func TestPositionAngle180Fold(t *testing.T) {
	base := hdr(
		card("CTYPE1", "RA---TAN"), card("CTYPE2", "DEC--TAN"),
		card("CRVAL1", 300.0), card("CRVAL2", 36.0),
		card("CRPIX1", 50.5), card("CRPIX2", 50.5),
		card("CD1_1", 0.000205), card("CD1_2", 0.000172),
		card("CD2_1", -0.000172), card("CD2_2", 0.000205),
		card("NAXIS1", int64(100)), card("NAXIS2", int64(100)),
	)
	rotated := hdr(
		card("CTYPE1", "RA---TAN"), card("CTYPE2", "DEC--TAN"),
		card("CRVAL1", 300.0), card("CRVAL2", 36.0),
		card("CRPIX1", 50.5), card("CRPIX2", 50.5),
		card("CD1_1", -0.000205), card("CD1_2", -0.000172),
		card("CD2_1", 0.000172), card("CD2_2", -0.000205),
		card("NAXIS1", int64(100)), card("NAXIS2", int64(100)),
	)
	a := FromHeader(base).Solution
	rb := FromHeader(rotated).Solution
	if a == nil || rb == nil {
		t.Fatal("expected both to solve")
	}
	if a.PositionAngle < 0 || a.PositionAngle >= 180 {
		t.Errorf("pos_angle %v not in [0,180)", a.PositionAngle)
	}
	approx(t, "180°-rotated pos_angle matches", rb.PositionAngle, a.PositionAngle, 1e-6)
}

func TestMirrorFlipped(t *testing.T) {
	h := normalWCS()
	// Flip parity: make CD1_1 positive so det(CD) > 0 (mirrored sky).
	h = hdr(
		card("CTYPE1", "RA---TAN-SIP"), card("CTYPE2", "DEC--TAN-SIP"),
		card("CRVAL1", 180.0), card("CRVAL2", 0.0),
		card("CRPIX1", 50.5), card("CRPIX2", 50.5),
		card("CD1_1", scaleDeg), card("CD1_2", 0.0),
		card("CD2_1", 0.0), card("CD2_2", scaleDeg),
		card("NAXIS1", int64(100)), card("NAXIS2", int64(100)),
	)
	s := FromHeader(h).Solution
	if s == nil {
		t.Fatal("expected a solution")
	}
	if !s.Flipped {
		t.Errorf("Flipped = false, want true for a mirrored (det>0) frame")
	}
	approx(t, "pixel_scale", s.PixelScale, 1.0, 1e-9)
}

// An off-center CRPIX (reference at the image corner) must yield the true image
// center, not CRVAL — guards that the gnomonic deprojection is actually applied.
func TestOffCenterCRPIX(t *testing.T) {
	h := hdr(
		card("CTYPE1", "RA---TAN"), card("CTYPE2", "DEC--TAN"),
		card("CRVAL1", 180.0), card("CRVAL2", 0.0),
		card("CRPIX1", 1.0), card("CRPIX2", 1.0),
		card("CD1_1", -scaleDeg), card("CD1_2", 0.0),
		card("CD2_1", 0.0), card("CD2_2", scaleDeg),
		card("NAXIS1", int64(100)), card("NAXIS2", int64(100)),
	)
	s := FromHeader(h).Solution
	if s == nil {
		t.Fatal("expected a solution")
	}
	// center pixel (50.5,50.5) is 49.5px from CRPIX → 49.5 arcsec ≈ 0.01375° each.
	approx(t, "center_ra", s.CenterRA, 179.98625, 1e-4)
	approx(t, "center_dec", s.CenterDec, 0.01375, 1e-4)
	if math.Abs(s.CenterRA-180) < 1e-6 {
		t.Errorf("center_ra collapsed to CRVAL; deprojection not applied")
	}
}

func TestDegenerateCD(t *testing.T) {
	h := hdr(
		card("CTYPE1", "RA---TAN"), card("CTYPE2", "DEC--TAN"),
		card("CRVAL1", 10.0), card("CRVAL2", 20.0),
		card("CD1_1", 1e-16), card("CD1_2", 1e-16),
		card("CD2_1", 1e-16), card("CD2_2", 1e-16),
	)
	a := FromHeader(h)
	if a.Solution != nil {
		t.Errorf("expected no solution for a degenerate CD, got %+v", a.Solution)
	}
	if a.Status != model.AstroStatusNone {
		t.Errorf("status = %q, want none", a.Status)
	}
}

func TestSwappedAxes(t *testing.T) {
	h := hdr(
		card("CTYPE1", "DEC--TAN"), card("CTYPE2", "RA---TAN"),
		card("CRVAL1", 20.0), card("CRVAL2", 10.0),
		card("CD1_1", -scaleDeg), card("CD2_2", scaleDeg),
	)
	if s := FromHeader(h).Solution; s != nil {
		t.Errorf("expected no solution for swapped DEC/RA axes, got %+v", s)
	}
}

func TestHintsSexagesimal(t *testing.T) {
	// OBJCTRA in hours ("10 45 03.6" = 10.751h → 161.265°), OBJCTDEC in degrees.
	h := hdr(
		card("IMAGETYP", "Light Frame"),
		card("OBJCTRA", "10 45 03.6"),
		card("OBJCTDEC", "-59 41 04"),
	)
	a := FromHeader(h)
	if a.Solution != nil {
		t.Errorf("expected no solution (no CTYPE), got %+v", a.Solution)
	}
	if a.Hints == nil || a.Hints.RA == nil || a.Hints.Dec == nil {
		t.Fatalf("expected RA/Dec hints, got %+v", a.Hints)
	}
	approx(t, "hint RA", *a.Hints.RA, 161.265, 1e-4)
	approx(t, "hint Dec", *a.Hints.Dec, -59.684444, 1e-4)
}

func TestHintsNumeric(t *testing.T) {
	h := hdr(
		card("IMAGETYP", "Light Frame"),
		card("RA", 161.265),
		card("DEC", -59.6844),
		card("FOCALLEN", 540.0),
		card("XPIXSZ", 3.76),
	)
	a := FromHeader(h)
	if a.Hints == nil {
		t.Fatal("expected hints")
	}
	if a.Hints.RA == nil || *a.Hints.RA != 161.265 {
		t.Errorf("hint RA = %v, want 161.265", a.Hints.RA)
	}
	if a.Hints.FocalLenMM == nil || *a.Hints.FocalLenMM != 540 {
		t.Errorf("hint FOCALLEN = %v, want 540", a.Hints.FocalLenMM)
	}
	if a.Hints.PixelSizeUm == nil || *a.Hints.PixelSizeUm != 3.76 {
		t.Errorf("hint XPIXSZ = %v, want 3.76", a.Hints.PixelSizeUm)
	}
}

func TestCalibrationNoWCSWithHints(t *testing.T) {
	// A flat with optics cards but no CTYPE: status none, no solution, hints present.
	h := hdr(
		card("IMAGETYP", "Flat Frame"),
		card("FOCALLEN", 540.0),
		card("XPIXSZ", 3.76),
	)
	a := FromHeader(h)
	if a.Status != model.AstroStatusNone || a.Solution != nil {
		t.Errorf("status=%q solution=%v, want none + nil", a.Status, a.Solution)
	}
	if a.Hints == nil {
		t.Error("expected hints from FOCALLEN/XPIXSZ")
	}
}

func TestBiasNeither(t *testing.T) {
	h := hdr(card("IMAGETYP", "Bias Frame"))
	a := FromHeader(h)
	if a.Status != model.AstroStatusNone || a.Solution != nil || a.Hints != nil {
		t.Errorf("got status=%q solution=%v hints=%v, want none/nil/nil", a.Status, a.Solution, a.Hints)
	}
}
