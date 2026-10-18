// Package starfield renders synthetic astrophotography frames of real star
// fields, so the demo build and the README screenshots show sky that previews,
// thumbnails and plate-solves like the real thing without any real capture
// (which would carry PII) being committed.
//
// Stars come from Gaia DR3, fetched one magnitude-limited cone per field from
// the ESA archive and cached on the local machine. Gaia data are licensed CC BY-NC
// 3.0 IGO (credit ESA/Gaia/DPAC), so no catalog data and no rendered frame is
// ever committed or shipped: everything Gaia-derived lives under the user cache
// directory. The renderer is deliberately small: a gnomonic projection, a
// magnitude-to-flux model, a Gaussian core with a Moffat halo, sky background,
// vignetting, and shot and read noise. It draws stars only.
//
// It is not part of the product: only tools/ and the `-tags demo` build import it.
package starfield

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// renderVersion is part of every cached frame's key: bump it when the renderer
// or the header changes, so stale renders are not reused.
const renderVersion = 1

// DefaultCacheDir is where the catalog cones and rendered frames are kept:
// <user cache dir>/asiair-commander/starfield, outside any checkout so nothing
// Gaia-derived can be committed or land in an image build context.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("starfield: no user cache dir: %w", err)
	}
	return filepath.Join(base, "asiair-commander", "starfield"), nil
}

// Sky renders targets, reusing a cached render when one exists.
type Sky struct {
	Dir  string // cache root: gaia/ holds catalog cones, frames/ holds renders
	Gaia *Gaia  // nil uses a default client caching under Dir/gaia
	Log  *slog.Logger
}

// Frame is a rendered target: the full FITS file as cached, and its data unit,
// which a caller re-headers per exposure.
type Frame struct {
	Target Target
	File   []byte
	Data   []byte
}

// Render returns target t's frame, from the cache when present, else by fetching
// its catalog cone and rendering it (and then caching the result).
func (s *Sky) Render(ctx context.Context, t Target) (Frame, error) {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	path := filepath.Join(s.Dir, "frames", frameKey(t)+".fit")
	if b, err := os.ReadFile(path); err == nil {
		data, derr := dataUnit(b)
		if want := t.Optics.Width() * t.Optics.Height() * 2; derr == nil && len(data) < want {
			derr = fmt.Errorf("data unit holds %d bytes, want %d", len(data), want)
		}
		if derr == nil {
			log.Info("starfield: render cache hit", "target", t.Name, "file", filepath.Base(path))
			return Frame{Target: t, File: b, Data: data}, nil
		}
		log.Warn("starfield: cached render unreadable; re-rendering", "file", filepath.Base(path), "error", derr)
	}

	g := s.Gaia
	if g == nil {
		g = &Gaia{Dir: filepath.Join(s.Dir, "gaia"), Log: log}
	}
	stars, err := g.Cone(ctx, t.RA, t.Dec, t.ConeRadius(), t.MagLimit)
	if err != nil {
		return Frame{}, err
	}
	start := time.Now()
	data := DataUnit(Render(t, stars, seedFor(t)))
	file := File(Cards(t, ReferenceExposure), data)
	log.Info("starfield: rendered", "target", t.Name, "stars", len(stars),
		"width", t.Optics.Width(), "height", t.Optics.Height(), "elapsed", time.Since(start).Round(time.Millisecond))
	if err := writeAtomic(path, file); err != nil {
		log.Warn("starfield: could not cache render", "file", path, "error", err)
	}
	return Frame{Target: t, File: file, Data: data}, nil
}

// FITS is this frame as exposure e: its pixels behind a header for e, so every
// exposure of a target is a distinct file with its own time and sequence.
func (f Frame) FITS(e Exposure) []byte {
	return File(Cards(f.Target, e), f.Data)
}

// ReferenceExposure heads a cached render and the files the starfield command
// writes. It is fixed so a cache entry's bytes depend only on the target.
var ReferenceExposure = Exposure{
	Start: time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC), SetTemp: -10, CCDTemp: -10, Seq: 1,
}

// frameKey names a cached render after the target and a digest of everything
// that shapes its pixels.
func frameKey(t Target) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%+v", renderVersion, t)
	slug := strings.ReplaceAll(strings.ToLower(t.Name), " ", "-")
	return fmt.Sprintf("%s-%016x", slug, h.Sum64())
}

// seedFor derives a target's noise seed from its name, so a re-render after a
// cache wipe gives identical pixels.
func seedFor(t Target) uint64 {
	h := fnv.New64a()
	h.Write([]byte(t.Name))
	return h.Sum64()
}

// dataUnit returns the bytes after a FITS primary header: the header's 80-column
// cards run to an END card, padded to a whole 2880-byte block.
func dataUnit(b []byte) ([]byte, error) {
	const block, cardLen = 2880, 80
	end := []byte(fmt.Sprintf("%-80s", "END"))
	for off := 0; off+cardLen <= len(b); off += cardLen {
		if bytes.Equal(b[off:off+cardLen], end) {
			start := (off/block + 1) * block
			if start >= len(b) {
				return nil, errors.New("starfield: FITS file has no data unit")
			}
			return b[start:], nil
		}
	}
	return nil, errors.New("starfield: no END card")
}

// Decode reads the pixels back out of a data unit DataUnit wrote.
func Decode(data []byte, width, height int) (Image, error) {
	if len(data) < width*height*2 {
		return Image{}, fmt.Errorf("starfield: data unit holds %d bytes, want %d", len(data), width*height*2)
	}
	img := Image{Width: width, Height: height, Pix: make([]uint16, width*height)}
	for i := range img.Pix {
		img.Pix[i] = binary.BigEndian.Uint16(data[2*i:]) ^ 0x8000
	}
	return img, nil
}
