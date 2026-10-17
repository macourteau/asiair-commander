package imgpreview

import (
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/astrogo/fitsio"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// maxPixels bounds W×H before any allocation. 150 MP sits well above any real
// astro sensor (~61 MP for the largest in the default map) yet caps the transient
// decode/render memory. Enforced by division so the check itself cannot overflow.
const maxPixels = 150_000_000

// decodeError carries whether a decode failure is deterministic (bad content: will
// always fail, so the on-demand path may record a negative-cache marker) or transient
// (an I/O error that must stay retryable). isDeterministic classifies it.
type decodeError struct {
	deterministic bool
	err           error
}

func (e *decodeError) Error() string { return e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }

func detErr(format string, a ...any) error {
	return &decodeError{deterministic: true, err: fmt.Errorf(format, a...)}
}

func transErr(err error) error { return &decodeError{deterministic: false, err: err} }

func isDeterministic(err error) bool {
	var de *decodeError
	return errors.As(err, &de) && de.deterministic
}

// decoded is a decoded frame in planar [0,1] float form. For a CFA (one-shot-colour)
// frame it is a single mono plane plus the Bayer pattern; debayering happens at render
// time so the method can vary by target size.
type decoded struct {
	W, H, Channels int
	Pix            []float32 // planar, channel c at Pix[c*W*H:(c+1)*W*H]
	isCFA          bool
	bayer          bayerPattern
	bottomUp       bool
}

func (d *decoded) plane(c int) []float32 { return d.Pix[c*d.W*d.H : (c+1)*d.W*d.H] }

// decode reads and validates the FITS header via hdr (the cheap, data-unit-free
// block parser — the pre-allocation gate), then decodes the primary-HDU pixels with
// astrogo/fitsio and normalizes them to [0,1]. fitsio.Read returns raw samples (its
// BZERO/BSCALE rescale lives only in a separate Image() method), so the manual
// BSCALE·raw+BZERO below scales exactly once.
func decode(hdr header.Reader, path string) (*decoded, error) {
	h, err := hdr.ReadHeader(path)
	if err != nil {
		// A header read failure is most often the file being briefly unreachable;
		// keep it retryable.
		return nil, transErr(fmt.Errorf("preview: read header %s: %w", path, err))
	}

	bitpix, ok := h.Int("BITPIX")
	if !ok || !validBitpix(int(bitpix)) {
		return nil, detErr("preview: unsupported BITPIX %v", h)
	}
	naxis, _ := h.Int("NAXIS")
	w, wok := h.Int("NAXIS1")
	ht, hok := h.Int("NAXIS2")
	if !wok || !hok || w <= 0 || ht <= 0 {
		return nil, detErr("preview: bad NAXIS1/NAXIS2")
	}
	channels := int64(1)
	if naxis == 3 {
		n3, ok := h.Int("NAXIS3")
		if !ok || (n3 != 1 && n3 != 3) {
			return nil, detErr("preview: unsupported NAXIS3 %d", n3)
		}
		channels = n3
	} else if naxis != 2 {
		return nil, detErr("preview: unsupported NAXIS %d", naxis)
	}
	if err := checkPixelCeiling(w, ht); err != nil {
		return nil, detErr("preview: %v", err)
	}

	bzero, _ := h.Float("BZERO")
	bscale, ok := h.Float("BSCALE")
	if !ok {
		bscale = 1
	}

	pix, err := readPixels(path, int(bitpix), int(w), int(ht), int(channels), bscale, bzero)
	if err != nil {
		return nil, err
	}

	d := &decoded{W: int(w), H: int(ht), Channels: int(channels), Pix: pix}
	if ro, ok := h.Str("ROWORDER"); ok && normalizeRowOrder(ro) == "BOTTOM-UP" {
		d.bottomUp = true
	}
	// A CFA frame is a single-plane mosaic carrying BAYERPAT; multi-plane frames are
	// already colour-separated and never debayered.
	if d.Channels == 1 {
		if pat, ok := h.Str("BAYERPAT"); ok {
			xoff, _ := h.Int("XBAYROFF")
			yoff, _ := h.Int("YBAYROFF")
			if b, ok := parseBayer(pat, int(xoff), int(yoff)); ok {
				d.isCFA = true
				d.bayer = b
			}
		}
	}
	return d, nil
}

// readPixels decodes the primary HDU's samples with fitsio and normalizes them.
func readPixels(path string, bitpix, w, h, channels int, bscale, bzero float64) (pix []float32, err error) {
	f, oerr := os.Open(path)
	if oerr != nil {
		return nil, transErr(fmt.Errorf("preview: open %s: %w", path, oerr))
	}
	defer f.Close()

	// fitsio is not defensive against malformed data units; a recover keeps a bad
	// frame from taking down the render goroutine, and it is a deterministic failure.
	defer func() {
		if rec := recover(); rec != nil {
			pix, err = nil, detErr("preview: fitsio panic: %v", rec)
		}
	}()

	hdu, derr := fitsio.NewDecoder(f).DecodeHDU()
	if derr != nil {
		return nil, detErr("preview: decode HDU: %v", derr)
	}
	fim, ok := hdu.(fitsio.Image)
	if !ok {
		return nil, detErr("preview: primary HDU is not an image")
	}

	n := w * h * channels
	div := fullScale(bitpix)
	out := make([]float32, n)
	norm := func(physical float64) float32 {
		if math.IsNaN(physical) || math.IsInf(physical, 0) {
			return 0
		}
		return float32(clamp01(physical / div))
	}

	switch bitpix {
	case 8:
		raw := make([]uint8, n)
		if err := fim.Read(&raw); err != nil {
			return nil, detErr("preview: read 8-bit: %v", err)
		}
		for i, v := range raw {
			out[i] = norm(bscale*float64(v) + bzero)
		}
	case 16:
		raw := make([]int16, n)
		if err := fim.Read(&raw); err != nil {
			return nil, detErr("preview: read 16-bit: %v", err)
		}
		for i, v := range raw {
			out[i] = norm(bscale*float64(v) + bzero)
		}
	case 32:
		raw := make([]int32, n)
		if err := fim.Read(&raw); err != nil {
			return nil, detErr("preview: read 32-bit: %v", err)
		}
		for i, v := range raw {
			out[i] = norm(bscale*float64(v) + bzero)
		}
	case 64:
		raw := make([]int64, n)
		if err := fim.Read(&raw); err != nil {
			return nil, detErr("preview: read 64-bit: %v", err)
		}
		for i, v := range raw {
			out[i] = norm(bscale*float64(v) + bzero)
		}
	case -32:
		raw := make([]float32, n)
		if err := fim.Read(&raw); err != nil {
			return nil, detErr("preview: read float32: %v", err)
		}
		for i, v := range raw {
			out[i] = norm(bscale*float64(v) + bzero)
		}
	case -64:
		raw := make([]float64, n)
		if err := fim.Read(&raw); err != nil {
			return nil, detErr("preview: read float64: %v", err)
		}
		for i, v := range raw {
			out[i] = norm(bscale*v + bzero)
		}
	default:
		return nil, detErr("preview: unsupported BITPIX %d", bitpix)
	}
	return out, nil
}

func validBitpix(b int) bool {
	switch b {
	case 8, 16, 32, 64, -32, -64:
		return true
	}
	return false
}

// fullScale is the divisor mapping raw integer samples to [0,1]: the unsigned
// maximum of the bit depth. Floats (-32/-64) are taken as already-physical [0,1].
func fullScale(bitpix int) float64 {
	switch bitpix {
	case 8:
		return 255
	case 16:
		return 65535
	case 32:
		return 4294967295
	case 64:
		return 18446744073709551615
	}
	return 1
}

// checkPixelCeiling rejects geometry beyond maxPixels using division so the check
// cannot itself overflow int64 on a crafted header.
func checkPixelCeiling(w, h int64) error {
	if w > maxPixels || h > maxPixels || w > maxPixels/h {
		return fmt.Errorf("geometry %dx%d exceeds %d-pixel ceiling", w, h, int64(maxPixels))
	}
	return nil
}

func normalizeRowOrder(s string) string {
	return model.NormalizeKey(s)
}
