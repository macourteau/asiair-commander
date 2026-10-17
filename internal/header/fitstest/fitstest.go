// Package fitstest builds synthetic FITS files for tests. It exists because a
// real-world FITS capture is treated as PII, so every fixture that
// carries a data unit has to be generated from code rather than captured from a
// camera — and because the shapes design frame-lifecycle §11 (H13) tests
// against are needed
// by several packages: a frame with a non-empty data unit, the revision pair whose
// data unit is identical behind a header one 2880-byte block longer, the same pair
// at an unchanged total length, and a header-only frame whose zero-length data unit
// every identity test must refuse.
//
// It writes FITS bytes and never parses them; the single END-card parser lives in
// internal/header. It deliberately does not import internal/header, so a test
// inside that package can use it without an import cycle.
package fitstest

import (
	"encoding/binary"
	"fmt"
	"strings"
)

const (
	// BlockSize is the FITS block: a header is a whole number of these and so is a
	// data unit, the data unit zero-padded out to the boundary.
	BlockSize = 2880
	// CardSize is the width of one card image, in columns.
	CardSize = 80
	// cardsPerBlock is how many card images one header block holds.
	cardsPerBlock = BlockSize / CardSize
)

// SynthLightName is the committed fixture in internal/header/testdata carrying a
// non-empty data unit. SynthLight generates its exact bytes; a test in that
// package pins the two together, so the committed file is never opaque.
const SynthLightName = "synth_light_60x48_16bit.fit"

// synthWidth and synthHeight size the committed fixture's image: 60x48 16-bit
// pixels is 5760 bytes, exactly two data blocks, which keeps the fixture at 8640
// bytes — the size of the header-only light fixtures beside it.
const (
	synthWidth  = 60
	synthHeight = 48
)

// SynthLight returns the exact bytes of testdata/synth_light_60x48_16bit.fit: a
// conforming primary HDU over a two-block synthetic image. It carries no site,
// observer or GPS keyword, so it passes the privacy guard that walks that
// directory.
func SynthLight() []byte {
	return Build(LightCards(synthWidth, synthHeight, "Synthetic Field"), SyntheticImage(synthWidth, synthHeight))
}

// Card renders one 80-column card image, space-padded from s. A string longer
// than 80 columns is truncated, which is what a malformed-card test wants.
func Card(s string) []byte {
	b := []byte(strings.Repeat(" ", CardSize))
	copy(b, s)
	return b
}

// Build assembles a conforming FITS file: the given cards followed by END,
// blank-padded to a whole number of header blocks, then data zero-padded to a
// whole number of blocks. Passing nil data yields a header-only file — legal
// FITS, and the zero-length data unit the H13 identity tests must refuse.
func Build(cards []string, data []byte) []byte {
	out := make([]byte, 0, HeaderBlocks(cards)*BlockSize+len(data)+BlockSize)
	for _, c := range cards {
		out = append(out, Card(c)...)
	}
	out = append(out, Card("END")...)
	for len(out)%BlockSize != 0 {
		out = append(out, Card("")...)
	}
	out = append(out, data...)
	for len(out)%BlockSize != 0 {
		out = append(out, 0)
	}
	return out
}

// HeaderBlocks reports how many 2880-byte blocks Build's header will occupy for
// these cards, counting the END card.
func HeaderBlocks(cards []string) int {
	return (len(cards) + 1 + cardsPerBlock - 1) / cardsPerBlock
}

// Revision returns the pair hardware finding H13 describes: `original` as the
// device first writes the frame, and `revised` as the device rewrites it in place
// after its plate solve, injecting a WCS that overflows the header into one more
// 2880-byte block. The data unit is byte-identical across the pair and sits at a
// different offset in each, which is the whole point — the two files hash
// differently whole-file and identically over the data unit.
func Revision(cards []string, data []byte) (original, revised []byte) {
	grown := append([]string(nil), cards...)
	want := HeaderBlocks(cards) + 1
	for HeaderBlocks(grown) < want {
		grown = append(grown, growthCard(len(grown)-len(cards)))
	}
	return Build(cards, data), Build(grown, data)
}

// SameSizeRevision returns the H13 pair for a camera whose base header has room
// for the solve's cards inside the blocks it already occupies: `revised` carries
// the same injected WCS as Revision's, written over blank padding rather than into
// a further block, so the pair differs byte for byte while its total length is
// identical and its data unit sits at the same offset in both. Nothing in the
// discriminator turns on the size, and this is the shape that proves it — the
// +2880-byte growth is one sensor's behaviour, not the rule.
//
// It panics when the WCS does not fit, which is a fixture that no longer builds
// the shape its callers ask for rather than a condition a test should handle.
func SameSizeRevision(cards []string, data []byte) (original, revised []byte) {
	grown := append(append([]string(nil), cards...), wcsCards...)
	if HeaderBlocks(grown) != HeaderBlocks(cards) {
		panic("fitstest: the injected WCS does not fit the header blocks these cards already occupy")
	}
	return Build(cards, data), Build(grown, data)
}

// wcsCards are the solved-astrometry cards the device injects. Sky coordinates
// describe where the telescope pointed, not where the observer stands, so none of
// them is on the privacy denylist.
var wcsCards = []string{
	"CTYPE1  = 'RA---TAN'           / axis 1 projection",
	"CTYPE2  = 'DEC--TAN'           / axis 2 projection",
	"CRPIX1  =                 30.0 / axis 1 reference pixel",
	"CRPIX2  =                 24.0 / axis 2 reference pixel",
	"CRVAL1  =           351.500000 / axis 1 reference value (deg)",
	"CRVAL2  =            61.200000 / axis 2 reference value (deg)",
	"CD1_1   =        -3.900000E-04 / transformation matrix",
	"CD1_2   =         0.000000E+00 / transformation matrix",
	"CD2_1   =         0.000000E+00 / transformation matrix",
	"CD2_2   =         3.900000E-04 / transformation matrix",
	"EQUINOX =               2000.0 / equinox of the reference frame",
	"RADESYS = 'FK5     '           / reference frame",
}

// growthCard supplies the i-th card of the revision's header growth: the injected
// WCS first, then commentary padding once that runs out, so growing a header by a
// whole block never needs more real keywords than a solve would write.
func growthCard(i int) string {
	if i < len(wcsCards) {
		return wcsCards[i]
	}
	return fmt.Sprintf("COMMENT   plate solve residual sample %d", i-len(wcsCards))
}

// LightCards returns a minimal conforming primary header for a 16-bit light frame
// of the given pixel dimensions, shaped like the ASIAIR fixtures beside it and
// carrying nothing that identifies a site or an observer.
func LightCards(width, height int, object string) []string {
	return []string{
		"SIMPLE  =                    T / conforms to FITS standard",
		"BITPIX  =                   16 / bits per data pixel",
		"NAXIS   =                    2 / number of data axes",
		fmt.Sprintf("NAXIS1  = %20d / length of data axis 1", width),
		fmt.Sprintf("NAXIS2  = %20d / length of data axis 2", height),
		"BZERO   =                32768 / offset to true values",
		"BSCALE  =                    1 / scale factor for true values",
		"IMAGETYP= 'Light   '           / Type of image",
		fmt.Sprintf("OBJECT  = %-20s / name of object being imaged", "'"+object+"'"),
		"EXPTIME =                300.0 / Exposure time in seconds",
		"FILTER  = 'HA      '           / Filter used",
		"INSTRUME= 'ZWO ASI6200MM Pro'  / Camera model",
		"TELESCOP= 'Synthetic OTA'      / Telescope model",
		"DATE-OBS= '2026-09-14T03:21:07.500' / UTC start of exposure",
		"XBINNING=                    1 / Binning factor in width",
		"YBINNING=                    1 / Binning factor in height",
		"GAIN    =                  100 / Sensor gain",
		"CCD-TEMP=                -10.0 / Sensor temperature (C)",
	}
}

// SyntheticImage returns width*height big-endian 16-bit pixels: a smooth gradient
// with a repeating comb of brighter pixels, so the bytes are neither constant nor
// random and a change anywhere in the unit moves the digest. FITS stores integers
// big-endian, so this is a data unit a reader would accept as an image.
func SyntheticImage(width, height int) []byte {
	out := make([]byte, 0, width*height*2)
	var px [2]byte
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint16(400 + 7*x + 13*y)
			if x%17 == 0 && y%11 == 0 {
				v += 9000 // a sparse "star" comb, so the unit is not a pure ramp
			}
			binary.BigEndian.PutUint16(px[:], v)
			out = append(out, px[0], px[1])
		}
	}
	return out
}
