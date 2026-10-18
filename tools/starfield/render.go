package starfield

import (
	"math"
	"math/rand/v2"
)

// The photometric model, cut down to what a static demo frame needs.
const (
	// f0 is the photon flux of a G = 0 star, photons/s/cm². It sits a decade below
	// the physical zero point so the one constant absorbs extinction, quantum
	// efficiency, filter and obstruction losses.
	f0 = 1e6
	// skyMag is the sky brightness, mag/arcsec²: a suburban sky.
	skyMag = 20.5
	// seeingArcsec is the atmospheric FWHM, guiding error folded in.
	seeingArcsec = 3.0
	// minFWHMPx keeps a star at least this wide in pixels. A short refractor binned
	// 2x2 is undersampled, and a real one never focuses to a single pixel.
	minFWHMPx = 1.6
	// pedestalADU is the bias level the camera's OFFSET puts under every pixel.
	pedestalADU = 500
	// edgeMarginPx is how far outside the frame a star may sit and still be drawn,
	// so the halo of one just off the edge reaches in.
	edgeMarginPx = 40
)

// PSF components as fractions of a star's flux: a Gaussian seeing core, and a
// Moffat (power-law) halo of scattered light. The halo is what makes a bright star
// look bigger than a faint one once both saturate: its visible radius grows with
// flux, where a Gaussian's barely does.
const (
	coreFrac, haloFrac = 0.85, 0.15
	haloBeta           = 1.5 // Moffat exponent: low for the heavy tail of real optics
	haloCoreScale      = 1.5 // Moffat core radius, in core sigmas
	haloFloorE         = 0.5 // the halo is drawn out to where it falls below this many electrons
	haloMaxPx          = 300
	spikeFrac          = 0.01
)

// Image is a rendered frame: 16-bit pixels, top row first.
type Image struct {
	Width, Height int
	Pix           []uint16
}

// Flux is the electrons a star of G magnitude mag deposits through an aperture
// of apertureMm over seconds, at unit quantum efficiency.
func Flux(mag, apertureMm, seconds float64) float64 {
	r := apertureMm / 20 // radius, cm
	return f0 * math.Pow(10, -0.4*mag) * math.Pi * r * r * seconds
}

// SkyFlux is the sky background in electrons per pixel over the exposure.
func SkyFlux(o Optics, seconds float64) float64 {
	arcsec := o.ScaleDeg() * 3600
	return Flux(skyMag, o.ApertureMm, seconds) * arcsec * arcsec
}

// SigmaPx is the Gaussian sigma of a star's core, in pixels: seeing and the
// optics' own blur (twice the diffraction limit, 276.8"/D) in quadrature.
func SigmaPx(o Optics) float64 {
	optical := 276.8 / o.ApertureMm
	fwhm := math.Hypot(seeingArcsec, optical) / (o.ScaleDeg() * 3600)
	return math.Max(fwhm, minFWHMPx) / 2.35482
}

// Render draws stars as target frames them, over a sky with a gentle gradient
// and vignetting, with shot and read noise from a generator seeded by seed. The
// same inputs always give the same pixels.
func Render(t Target, stars []Star, seed uint64) Image {
	o := t.Optics
	w, h := o.Width(), o.Height()
	electrons := make([]float64, w*h)
	scale := o.ScaleDeg()
	sigma := SigmaPx(o)

	for _, s := range stars {
		xi, eta, ok := Tangent(s.RA, s.Dec, t.RA, t.Dec)
		if !ok {
			continue
		}
		px, py := Pixel(xi, eta, scale, t.RotationDeg, w, h)
		if px < -edgeMarginPx || py < -edgeMarginPx || px > float64(w+edgeMarginPx) || py > float64(h+edgeMarginPx) {
			continue
		}
		e := Flux(s.G, o.ApertureMm, t.Exposure)
		gaussian(electrons, w, h, px, py, sigma, coreFrac*e)
		moffat(electrons, w, h, px, py, haloCoreScale*sigma, haloFrac*e)
		if o.Spikes {
			spikes(electrons, w, h, px, py, sigma, spikeFrac*e)
		}
	}

	sky := SkyFlux(o, t.Exposure)
	cam := o.Camera
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	img := Image{Width: w, Height: h, Pix: make([]uint16, w*h)}
	cx, cy := float64(w)/2, float64(h)/2
	rmax2 := cx*cx + cy*cy
	for y := range h {
		for x := range w {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			vignette := 1 - 0.15*(dx*dx+dy*dy)/rmax2
			gradient := 1 + 0.06*dx/float64(w) - 0.04*dy/float64(h)
			i := y*w + x
			signal := (electrons[i] + sky*gradient) * vignette
			e := poisson(rng, signal) + rng.NormFloat64()*cam.ReadNoise
			adu := pedestalADU + e/cam.EGain
			img.Pix[i] = uint16(math.Round(math.Min(math.Max(adu, 0), math.MaxUint16)))
		}
	}
	return img
}

// gaussian adds flux electrons as a 2-D Gaussian of width sigma centred at
// (cx, cy), integrated exactly over each pixel through the error function so a
// star's centroid is where the projection put it.
func gaussian(buf []float64, w, h int, cx, cy, sigma, flux float64) {
	r := int(math.Ceil(4 * sigma))
	x0, x1 := max(int(cx)-r, 0), min(int(cx)+r, w-1)
	y0, y1 := max(int(cy)-r, 0), min(int(cy)+r, h-1)
	if x0 > x1 || y0 > y1 {
		return
	}
	inv := 1 / (sigma * math.Sqrt2)
	fx := make([]float64, x1-x0+1)
	for x := x0; x <= x1; x++ {
		fx[x-x0] = 0.5 * (math.Erf((float64(x)+1-cx)*inv) - math.Erf((float64(x)-cx)*inv))
	}
	for y := y0; y <= y1; y++ {
		fy := flux * 0.5 * (math.Erf((float64(y)+1-cy)*inv) - math.Erf((float64(y)-cy)*inv))
		row := buf[y*w:]
		for x := x0; x <= x1; x++ {
			row[x] += fy * fx[x-x0]
		}
	}
}

// moffat adds flux electrons as a circular Moffat profile of core radius r0,
// I(r) = I0 (1 + (r/r0)²)^-β, sampled at pixel centres out to where it fades
// below haloFloorE. The flux beyond that radius is dropped, which is invisible.
func moffat(buf []float64, w, h int, cx, cy, r0, flux float64) {
	i0 := flux * (haloBeta - 1) / (math.Pi * r0 * r0)
	if i0 < haloFloorE {
		return
	}
	// Solve I(r) = haloFloorE for r.
	x2 := math.Pow(i0/haloFloorE, 1/haloBeta) - 1
	r := min(int(math.Ceil(r0*math.Sqrt(x2))), haloMaxPx)
	x0, x1 := max(int(cx)-r, 0), min(int(cx)+r, w-1)
	y0, y1 := max(int(cy)-r, 0), min(int(cy)+r, h-1)
	inv := 1 / (r0 * r0)
	for y := y0; y <= y1; y++ {
		dy := float64(y) + 0.5 - cy
		row := buf[y*w:]
		for x := x0; x <= x1; x++ {
			dx := float64(x) + 0.5 - cx
			row[x] += i0 * math.Pow(1+(dx*dx+dy*dy)*inv, -haloBeta)
		}
	}
}

// spikes adds a Newtonian spider's four diffraction spikes along the frame axes:
// flux electrons shared over four rays whose length grows with the star's
// brightness and whose intensity falls off with distance.
func spikes(buf []float64, w, h int, cx, cy, sigma, flux float64) {
	length := math.Min(0.02*math.Sqrt(flux), 400)
	start := 2 * sigma
	if length <= start+1 {
		return
	}
	var norm float64
	for d := start; d < length; d++ {
		norm += 1 / d
	}
	per := flux / 4 / norm
	ix, iy := int(cx), int(cy)
	for d := start; d < length; d++ {
		v := per / d
		for _, p := range [4][2]int{{ix + int(d), iy}, {ix - int(d), iy}, {ix, iy + int(d)}, {ix, iy - int(d)}} {
			if p[0] >= 0 && p[0] < w && p[1] >= 0 && p[1] < h {
				buf[p[1]*w+p[0]] += v
			}
		}
	}
}

// poisson draws from a Poisson distribution of mean lambda: exactly for a small
// mean, through the normal approximation once it is indistinguishable.
func poisson(rng *rand.Rand, lambda float64) float64 {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		return math.Max(lambda+math.Sqrt(lambda)*rng.NormFloat64(), 0)
	}
	l, k, p := math.Exp(-lambda), 0.0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}
