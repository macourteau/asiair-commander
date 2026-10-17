package imgpreview

import "strings"

// Bayer colour indices, matching the planar channel order (R,G,B).
const (
	cRed   = 0
	cGreen = 1
	cBlue  = 2
)

// bayerPattern is a validated 2×2 CFA layout plus its origin offsets. colorAt
// reports the colour (cRed/cGreen/cBlue) of the sensor pixel at (x,y).
type bayerPattern struct {
	// quad[dy][dx] is the colour of the pixel at (x%2==dx, y%2==dy) once the
	// XBAYROFF/YBAYROFF origin shift is folded in.
	quad       [2][2]int
	xoff, yoff int
}

// parseBayer interprets a BAYERPAT string ("RGGB"|"BGGR"|"GRBG"|"GBRG") and the
// optional XBAYROFF/YBAYROFF offsets. ok=false for an unrecognised pattern, which
// the caller treats as mono.
func parseBayer(pat string, xoff, yoff int) (bayerPattern, bool) {
	pat = strings.ToUpper(strings.TrimSpace(pat))
	if len(pat) != 4 {
		return bayerPattern{}, false
	}
	var b bayerPattern
	for i := 0; i < 4; i++ {
		var c int
		switch pat[i] {
		case 'R':
			c = cRed
		case 'G':
			c = cGreen
		case 'B':
			c = cBlue
		default:
			return bayerPattern{}, false
		}
		b.quad[i/2][i%2] = c // i: 0=(row0,col0) 1=(row0,col1) 2=(row1,col0) 3=(row1,col1)
	}
	// Normalise offsets to [0,1]; a shift of one flips the phase on that axis.
	b.xoff = ((xoff % 2) + 2) % 2
	b.yoff = ((yoff % 2) + 2) % 2
	return b, true
}

// colorAt returns the CFA colour of pixel (x,y) in stored order.
func (b bayerPattern) colorAt(x, y int) int {
	return b.quad[(y+b.yoff)&1][(x+b.xoff)&1]
}

// superpixel debayers a mono CFA plane into three half-resolution RGB planes: each
// 2×2 quad becomes one pixel (R from the red site, B from the blue site, G from the
// mean of the two green sites). Fast, artefact-free, and the resolution halving is
// irrelevant at preview sizes. A dangling odd row/column is dropped.
func superpixel(mono []float32, w, h int, b bayerPattern) (planes [3][]float32, ow, oh int) {
	ow, oh = w/2, h/2
	for c := range planes {
		planes[c] = make([]float32, ow*oh)
	}
	for by := 0; by < oh; by++ {
		for bx := 0; bx < ow; bx++ {
			var r, g, bl float32
			var gn int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					x, y := bx*2+dx, by*2+dy
					v := mono[y*w+x]
					switch b.colorAt(x, y) {
					case cRed:
						r = v
					case cBlue:
						bl = v
					default:
						g += v
						gn++
					}
				}
			}
			if gn > 0 {
				g /= float32(gn)
			}
			i := by*ow + bx
			planes[cRed][i] = r
			planes[cGreen][i] = g
			planes[cBlue][i] = bl
		}
	}
	return planes, ow, oh
}

// bilinear debayers a mono CFA plane into three full-resolution RGB planes by
// interpolating each missing colour from its nearest same-colour neighbours (edge
// coordinates clamped). Used only for the native-resolution `full` render, where
// dimensions must be preserved.
func bilinear(mono []float32, w, h int, b bayerPattern) [3][]float32 {
	var out [3][]float32
	for c := range out {
		out[c] = make([]float32, w*h)
	}
	at := func(x, y int) float32 {
		if x < 0 {
			x = 0
		} else if x >= w {
			x = w - 1
		}
		if y < 0 {
			y = 0
		} else if y >= h {
			y = h - 1
		}
		return mono[y*w+x]
	}
	avg := func(vs ...float32) float32 {
		var s float32
		for _, v := range vs {
			s += v
		}
		return s / float32(len(vs))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			self := mono[i]
			switch b.colorAt(x, y) {
			case cGreen:
				// Horizontal neighbours are one chroma colour, vertical the other.
				hVal := avg(at(x-1, y), at(x+1, y))
				vVal := avg(at(x, y-1), at(x, y+1))
				out[cGreen][i] = self
				if b.colorAt(x-1, y) == cRed {
					out[cRed][i], out[cBlue][i] = hVal, vVal
				} else {
					out[cRed][i], out[cBlue][i] = vVal, hVal
				}
			case cRed:
				out[cRed][i] = self
				out[cGreen][i] = avg(at(x-1, y), at(x+1, y), at(x, y-1), at(x, y+1))
				out[cBlue][i] = avg(at(x-1, y-1), at(x+1, y-1), at(x-1, y+1), at(x+1, y+1))
			case cBlue:
				out[cBlue][i] = self
				out[cGreen][i] = avg(at(x-1, y), at(x+1, y), at(x, y-1), at(x, y+1))
				out[cRed][i] = avg(at(x-1, y-1), at(x+1, y-1), at(x-1, y+1), at(x+1, y+1))
			}
		}
	}
	return out
}
