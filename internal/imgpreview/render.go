package imgpreview

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

// renderToImage renders a decoded frame at output dimensions (dw,dh) — already
// fitted and never upscaled by the caller — applying the auto-stretch derived once
// from a fixed-resolution proxy so every size of a frame shares one stretch.
//
// The stretch statistics come from a canonical base (superpixel for CFA, native
// otherwise) downscaled to the proxy bound; the visible plane comes from a base
// whose debayer method matches the output detail (bilinear only when the output
// exceeds half the native resolution, where superpixel's half-res would fall short).
func renderToImage(d *decoded, dw, dh int) image.Image {
	// Canonical base + per-channel stretch parameters (proxy is order-independent,
	// so the ROWORDER flip is irrelevant here).
	cbase, cw, ch := canonicalBase(d)
	stretches := make([]stretch, len(cbase))
	for c := range cbase {
		pw, ph := fitDims(cw, ch, proxyDim)
		proxy := downscale(cbase[c], cw, ch, pw, ph)
		stretches[c] = computeStretch(proxy, targetBackground, shadowsClip)
	}

	// Visible base: reuse the canonical (superpixel/native) planes unless the output
	// needs full-resolution colour, in which case debayer with bilinear.
	base, bw, bh := cbase, cw, ch
	if d.isCFA && (dw > d.W/2 || dh > d.H/2) {
		base = bilinearPlanes(d)
		bw, bh = d.W, d.H
	}

	channels := len(base)
	out := make([][]float32, channels)
	for c := 0; c < channels; c++ {
		plane := base[c]
		if d.bottomUp {
			plane = flipRows(plane, bw, bh)
		}
		small := downscale(plane, bw, bh, dw, dh)
		out[c] = stretches[c].apply(small)
	}

	if channels >= 3 {
		return toNRGBA(out[0], out[1], out[2], dw, dh)
	}
	return toGray(out[0], dw, dh)
}

// canonicalBase returns the planes the stretch statistics are computed from: the
// three superpixel planes for a CFA frame, or the native planes otherwise. Using
// superpixel (not bilinear) here makes the stretch identical across every size,
// including the bilinear `full` render.
func canonicalBase(d *decoded) (planes [][]float32, w, h int) {
	if d.isCFA {
		sp, ow, oh := superpixel(d.plane(0), d.W, d.H, d.bayer)
		return [][]float32{sp[cRed], sp[cGreen], sp[cBlue]}, ow, oh
	}
	planes = make([][]float32, d.Channels)
	for c := 0; c < d.Channels; c++ {
		planes[c] = d.plane(c)
	}
	return planes, d.W, d.H
}

// bilinearPlanes returns the three full-resolution RGB planes of a CFA frame.
func bilinearPlanes(d *decoded) [][]float32 {
	bp := bilinear(d.plane(0), d.W, d.H, d.bayer)
	return [][]float32{bp[cRed], bp[cGreen], bp[cBlue]}
}

// flipRows returns a top-down copy of a bottom-up plane.
func flipRows(src []float32, w, h int) []float32 {
	out := make([]float32, len(src))
	for y := 0; y < h; y++ {
		copy(out[y*w:(y+1)*w], src[(h-1-y)*w:(h-y)*w])
	}
	return out
}

func toGray(plane []float32, w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range plane {
		img.Pix[i] = uint8(clamp01(float64(v))*255 + 0.5)
	}
	return img
}

func toNRGBA(r, g, b []float32, w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		j := i * 4
		img.Pix[j+0] = uint8(clamp01(float64(r[i]))*255 + 0.5)
		img.Pix[j+1] = uint8(clamp01(float64(g[i]))*255 + 0.5)
		img.Pix[j+2] = uint8(clamp01(float64(b[i]))*255 + 0.5)
		img.Pix[j+3] = 255
	}
	return img
}

// outputDims computes the output size for a named size (longest-edge bound, never
// upscaling): SizeFull → native, otherwise fitDims to the bound.
func (d *decoded) outputDims(size Size) (int, int) {
	return fitDims(d.W, d.H, size.maxDim())
}

// boxDims fits the native image into a (maxW,maxH) box, aspect preserved and never
// upscaled, for an arbitrary resize request.
func (d *decoded) boxDims(maxW, maxH int) (int, int) {
	dw, dh := d.W, d.H
	if maxW > 0 && dw > maxW {
		dh = max1(dh * maxW / dw)
		dw = maxW
	}
	if maxH > 0 && dh > maxH {
		dw = max1(dw * maxH / dh)
		dh = maxH
	}
	return dw, dh
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("preview: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}
