package starfield

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/macourteau/asiair-commander/internal/header/fitstest"
)

// Exposure is one frame's capture metadata: when it started, the cooler, and
// where the mount reported it was pointing.
type Exposure struct {
	Start   time.Time // DATE-OBS, UTC
	SetTemp float64   // SET-TEMP, °C
	CCDTemp float64   // CCD-TEMP, °C
	Seq     int       // the frame-name sequence number
}

// pointingErrRA and pointingErrDec are how far off the field centre the mount's reported RA/DEC
// sits, in degrees: a real goto lands a few arcminutes out, so the plate solver
// has a hint to correct rather than the answer.
const pointingErrRA, pointingErrDec = 0.04, -0.03

// Cards are an ASIAIR-style primary header for a light of target, shaped like the
// ASIAIR fixtures in internal/header/testdata. They carry no site, observer or
// GPS keyword (header.PIIDenylist), and no WCS: the ASIAIR solves only some
// frames, and a frame without one is what Commander's own plate solve is for.
func Cards(t Target, e Exposure) []string {
	o := t.Optics
	ra := math.Mod(t.RA+pointingErrRA/math.Cos(t.Dec*deg)+360, 360)
	dec := t.Dec + pointingErrDec
	return []string{
		card("SIMPLE", "T", "file does conform to FITS standard"),
		card("BITPIX", "16", "number of bits per data pixel"),
		card("NAXIS", "2", "number of data axes"),
		card("NAXIS1", fmt.Sprint(o.Width()), "length of data axis 1"),
		card("NAXIS2", fmt.Sprint(o.Height()), "length of data axis 2"),
		card("EXTEND", "T", "FITS dataset may contain extensions"),
		card("BZERO", "32768", "offset data range to that of unsigned short"),
		card("BSCALE", "1", "default scaling factor"),
		card("CREATOR", quote("ZWO ASIAIR Plus"), "Capture software"),
		card("OFFSET", fmt.Sprint(o.Camera.Offset), "camera offset"),
		card("XORGSUBF", "0", "Subframe X position in binned pixels"),
		card("YORGSUBF", "0", "Subframe Y position in binned pixels"),
		card("FOCALLEN", fmt.Sprint(int(o.FocalLengthMm)), "Focal length of telescope in mm"),
		card("SET-TEMP", fmt.Sprintf("%.0f", e.SetTemp), "CCD temperature setpoint in degrees C"),
		card("EGAIN", fmt.Sprint(o.Camera.EGain), "Electronic gain in e-/ADU"),
		card("XBINNING", fmt.Sprint(o.Bin), "Camera X Bin"),
		card("YBINNING", fmt.Sprint(o.Bin), "Camera Y Bin"),
		card("CCDXBIN", fmt.Sprint(o.Bin), "Camera X Bin"),
		card("CCDYBIN", fmt.Sprint(o.Bin), "Camera Y Bin"),
		card("XPIXSZ", fmt.Sprint(o.PixelUm()), "pixel size in microns (with binning)"),
		card("YPIXSZ", fmt.Sprint(o.PixelUm()), "pixel size in microns (with binning)"),
		card("IMAGETYP", quote("Light"), "Type of image"),
		card("EXPOSURE", fmt.Sprintf("%.1f", t.Exposure), "Exposure time in seconds"),
		card("EXPTIME", fmt.Sprintf("%.1f", t.Exposure), "Exposure time in seconds"),
		card("CCD-TEMP", fmt.Sprintf("%.1f", e.CCDTemp), "sensor temperature in C"),
		card("RA", fmt.Sprintf("%.6f", ra), "Object Right Ascension in degrees"),
		card("DEC", fmt.Sprintf("%.6f", dec), "Object Declination in degrees"),
		card("DATE-OBS", quote(e.Start.UTC().Format("2006-01-02T15:04:05.000000")), "Image exposure start time"),
		card("FILTER", quote(t.Filter), "Filter used when taking image"),
		card("INSTRUME", quote(o.Camera.Model), "Camera model"),
		card("GAIN", fmt.Sprint(o.Camera.Gain), "Gain Value"),
		card("TELESCOP", quote("EQMod Mount"), "Telescope name"),
		card("OBJECT", quote(t.Name), "name or catalog number of object being imaged"),
		card("COMMENT", "", "Synthetic frame: stars from ESA/Gaia/DPAC DR3 (CC BY-NC 3.0 IGO)"),
	}
}

// FrameName is the file name an ASIAIR's Autorun gives this frame, e.g.
// Light_M45_120.0s_Bin2_533MM_gain100_20261007-231502_-10.0C_0001.fit: the
// start time is local to the device clock, which the demo keeps in UTC.
func FrameName(t Target, e Exposure) string {
	o := t.Optics
	return fmt.Sprintf("Light_%s_%.1fs_Bin%d_%s_gain%d_%s_%.1fC_%04d.fit",
		t.Name, t.Exposure, o.Bin, o.Camera.Short, o.Camera.Gain,
		e.Start.UTC().Format("20060102-150405"), e.CCDTemp, e.Seq)
}

// DataUnit encodes img as a FITS 16-bit data unit, BZERO 32768, big-endian and
// zero-padded to whole blocks, ready to follow any header Cards build.
func DataUnit(img Image) []byte {
	n := len(img.Pix) * 2
	out := make([]byte, (n+fitstest.BlockSize-1)/fitstest.BlockSize*fitstest.BlockSize)
	for i, v := range img.Pix {
		binary.BigEndian.PutUint16(out[2*i:], v^0x8000) // v - 32768 as two's complement
	}
	return out
}

// File assembles a whole FITS file from cards and an encoded data unit.
func File(cards []string, data []byte) []byte {
	return fitstest.Build(cards, data)
}

// card renders a fixed-format FITS card image: an 8-column keyword, "= ", the
// value right-aligned to column 30 (strings are left-aligned at column 11), then
// the comment. COMMENT cards carry their text after the keyword instead.
func card(key, value, comment string) string {
	if key == "COMMENT" {
		return fmt.Sprintf("%-8s  %s", key, comment)
	}
	if len(value) > 0 && value[0] == '\'' {
		return fmt.Sprintf("%-8s= %-20s / %s", key, value, comment)
	}
	return fmt.Sprintf("%-8s= %20s / %s", key, value, comment)
}

// quote renders a FITS string value, padded to the standard's eight-character
// minimum.
func quote(s string) string {
	return fmt.Sprintf("'%-8s'", s)
}
