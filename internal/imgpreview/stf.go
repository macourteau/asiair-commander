package imgpreview

import "sort"

// degenerateMADN is the normalized-MAD floor below which a channel is treated as
// flat and passed through unstretched. On the [0,1] normalized scale a real frame
// (even a dark or bias, which carry read noise) sits well above this; only a
// genuinely signal-free channel falls under it, where the MTF would otherwise
// binarize it to black/white.
const degenerateMADN = 1e-6

// stretch holds the per-channel STF parameters derived from the proxy plane and
// applied to every size of that channel. passthrough marks a degenerate (flat)
// channel that must be rendered as-is rather than crushed.
type stretch struct {
	c0          float64
	denom       float64
	beta        float64
	passthrough bool
}

// computeStretch derives the STF parameters from a plane (the screen-sized proxy):
// shadows clipped at median + shadowsClip·MADN, then a midtones balance that pins
// the clipped median to target. A degenerate channel (clip point at/above 1, or a
// near-zero MADN) yields a pass-through stretch.
func computeStretch(pix []float32, target, shadows float64) stretch {
	med, madn := medianMADN(pix)
	c0 := clamp01(med + shadows*madn)
	denom := 1 - c0
	if denom <= 0 || madn < degenerateMADN {
		return stretch{passthrough: true}
	}
	m0 := (med - c0) / denom
	return stretch{c0: c0, denom: denom, beta: midtones(target, m0)}
}

// apply maps a plane's [0,1] values through this stretch, returning a new plane.
func (s stretch) apply(pix []float32) []float32 {
	out := make([]float32, len(pix))
	if s.passthrough {
		copy(out, pix)
		return out
	}
	for i, v := range pix {
		x := (float64(v) - s.c0) / s.denom
		out[i] = float32(clamp01(mtf(s.beta, clamp01(x))))
	}
	return out
}

// midtones solves mtf(beta, m0) = target for the midtones balance beta. For
// target 0.25 this reduces to 3·m0/(2·m0+1).
func midtones(target, m0 float64) float64 {
	den := 2*target*m0 - target - m0
	if den == 0 {
		return 0.5
	}
	return clamp01(m0 * (target - 1) / den)
}

// mtf is the midtones transfer function with midtones balance m.
func mtf(m, x float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	case m <= 0:
		return 1
	case m >= 1:
		return 0
	case x == m:
		return 0.5
	}
	return (m - 1) * x / ((2*m-1)*x - m)
}

// medianMADN returns the median and normalized MAD (1.4826·MAD) of pix.
func medianMADN(pix []float32) (float64, float64) {
	if len(pix) == 0 {
		return 0, 0
	}
	buf := make([]float64, len(pix))
	for i, v := range pix {
		buf[i] = float64(v)
	}
	med := medianOf(buf)
	for i := range buf {
		buf[i] = absf(buf[i] - med)
	}
	return med, 1.4826 * medianOf(buf)
}

func medianOf(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// fitDims scales (w,h) so the longer side is at most maxDim, preserving aspect.
// A maxDim of 0 (SizeFull) or dimensions already within bound return (w,h) — never
// upscaling.
func fitDims(w, h, maxDim int) (int, int) {
	if maxDim <= 0 || (w <= maxDim && h <= maxDim) {
		return w, h
	}
	if w >= h {
		return maxDim, max1(h * maxDim / w)
	}
	return max1(w * maxDim / h), maxDim
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// downscale area-averages src (sw×sh) into a dw×dh buffer. Averaging (not nearest)
// keeps preview stars from aliasing away. Returns src unchanged when dims match.
func downscale(src []float32, sw, sh, dw, dh int) []float32 {
	if dw == sw && dh == sh {
		return src
	}
	out := make([]float32, dw*dh)
	for oy := 0; oy < dh; oy++ {
		y0 := oy * sh / dh
		y1 := (oy + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for ox := 0; ox < dw; ox++ {
			x0 := ox * sw / dw
			x1 := (ox + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sum float32
			var n int
			for y := y0; y < y1 && y < sh; y++ {
				row := y * sw
				for x := x0; x < x1 && x < sw; x++ {
					sum += src[row+x]
					n++
				}
			}
			if n > 0 {
				out[oy*dw+ox] = sum / float32(n)
			}
		}
	}
	return out
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
