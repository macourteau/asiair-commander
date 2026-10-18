package starfield

import "math"

const deg = math.Pi / 180

// Tangent projects (ra, dec) onto the plane tangent to the sky at (ra0, dec0),
// the gnomonic (TAN) projection a FITS WCS describes and a plate solver inverts.
// Inputs are degrees; xi (positive east) and eta (positive north) are degrees on
// the tangent plane. ok is false for a point 90° or more from the tangent point,
// which has no projection.
func Tangent(ra, dec, ra0, dec0 float64) (xi, eta float64, ok bool) {
	sinD, cosD := math.Sincos(dec * deg)
	sinD0, cosD0 := math.Sincos(dec0 * deg)
	sinDA, cosDA := math.Sincos((ra - ra0) * deg)
	d := sinD0*sinD + cosD0*cosD*cosDA
	if d <= 0 {
		return 0, 0, false
	}
	xi = cosD * sinDA / d / deg
	eta = (cosD0*sinD - sinD0*cosD*cosDA) / d / deg
	return xi, eta, true
}

// Pixel maps tangent-plane coordinates to a pixel position in a width x height
// frame stored top row first, the order an ASIAIR writes: the field centre lands
// on the frame centre and, at rotation 0, north is up and east is left, the sky
// as seen looking up. rotDeg turns the camera counter-clockwise on the sky.
// Pixel x covers [x, x+1), so the centre of pixel (0, 0) is (0.5, 0.5).
func Pixel(xi, eta, scaleDeg, rotDeg float64, width, height int) (px, py float64) {
	// Right on the image is west (-xi); up the image is north (+eta).
	u, v := -xi/scaleDeg, eta/scaleDeg
	s, c := math.Sincos(rotDeg * deg)
	ur := u*c - v*s
	vr := u*s + v*c
	return float64(width)/2 + ur, float64(height)/2 - vr
}
