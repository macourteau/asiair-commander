package imgpreview

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
)

// --- synthetic FITS builder (real production decode path exercised end to end) ---

func card(key, val string) string {
	return fmt.Sprintf("%-80s", fmt.Sprintf("%-8s= %20s", key, val))
}

func strCard(key, s string) string {
	return fmt.Sprintf("%-80s", fmt.Sprintf("%-8s= '%-8s'", key, s))
}

func endCard() string { return fmt.Sprintf("%-80s", "END") }

// buildFITS writes a minimal valid FITS primary HDU (header blocks + big-endian
// data unit) and returns its path. cards are pre-formatted 80-char records without
// the END card.
func buildFITS(t *testing.T, cards []string, data []byte) string {
	t.Helper()
	var hdr bytes.Buffer
	for _, c := range cards {
		hdr.WriteString(c)
	}
	hdr.WriteString(endCard())
	for hdr.Len()%2880 != 0 {
		hdr.WriteByte(' ')
	}
	var d bytes.Buffer
	d.Write(data)
	for d.Len()%2880 != 0 {
		d.WriteByte(0)
	}
	path := filepath.Join(t.TempDir(), "test.fits")
	if err := os.WriteFile(path, append(hdr.Bytes(), d.Bytes()...), 0o644); err != nil {
		t.Fatalf("write fits: %v", err)
	}
	return path
}

func be16(vals ...int16) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func be8(vals ...uint8) []byte { return append([]byte{}, vals...) }

func beF32(vals ...float32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.BigEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

func mono16Cards(w, h int, bzero int, extra ...string) []string {
	cards := []string{
		card("SIMPLE", "T"),
		card("BITPIX", "16"),
		card("NAXIS", "2"),
		card("NAXIS1", fmt.Sprint(w)),
		card("NAXIS2", fmt.Sprint(h)),
		card("BZERO", fmt.Sprint(bzero)),
	}
	return append(cards, extra...)
}

func decodeFile(t *testing.T, path string) *decoded {
	t.Helper()
	d, err := decode(header.New(), path)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return d
}

// --- decode / normalize ---

func TestDecodeMono16UnsignedSingleScale(t *testing.T) {
	// BZERO=32768: int16 -32768→0, 0→0.5, 32767→~1.0 after /65535. A double-scale
	// would push these off-scale (all white), so this guards that regression.
	path := buildFITS(t, mono16Cards(2, 2, 32768), be16(-32768, 0, 16384, 32767))
	d := decodeFile(t, path)
	if d.W != 2 || d.H != 2 || d.Channels != 1 || d.isCFA {
		t.Fatalf("geometry: %+v", d)
	}
	got := d.plane(0)
	want := []float32{0, 0.5, 0.75, 1}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 0.001 {
			t.Errorf("pixel %d = %f, want %f", i, got[i], want[i])
		}
	}
}

func TestDecode8Unsigned(t *testing.T) {
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "8"), card("NAXIS", "2"),
		card("NAXIS1", "2"), card("NAXIS2", "2"),
	}
	path := buildFITS(t, cards, be8(0, 128, 191, 255))
	got := decodeFile(t, path).plane(0)
	want := []float32{0, 128.0 / 255, 191.0 / 255, 1}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 0.005 {
			t.Errorf("pixel %d = %f, want %f", i, got[i], want[i])
		}
	}
}

func TestDecodeFloat32WithNaN(t *testing.T) {
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "-32"), card("NAXIS", "2"),
		card("NAXIS1", "2"), card("NAXIS2", "2"),
	}
	path := buildFITS(t, cards, beF32(0, 0.5, float32(math.NaN()), float32(math.Inf(1))))
	got := decodeFile(t, path).plane(0)
	want := []float32{0, 0.5, 0, 0} // non-finite (NaN and Inf) both map to 0
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 0.001 {
			t.Errorf("pixel %d = %f, want %f", i, got[i], want[i])
		}
	}
}

func TestDecodeRejectsDeterministic(t *testing.T) {
	// Geometry over the pixel ceiling: rejected before any allocation, deterministic.
	over := []string{
		card("SIMPLE", "T"), card("BITPIX", "16"), card("NAXIS", "2"),
		card("NAXIS1", "200000000"), card("NAXIS2", "200000000"),
	}
	path := buildFITS(t, over, be16(0))
	_, err := decode(header.New(), path)
	if err == nil || !isDeterministic(err) {
		t.Fatalf("want deterministic error, got %v", err)
	}
}

func TestDecodeCFADetected(t *testing.T) {
	cards := mono16Cards(2, 2, 32768, strCard("BAYERPAT", "RGGB"))
	path := buildFITS(t, cards, be16(0, 0, 0, 0))
	d := decodeFile(t, path)
	if !d.isCFA {
		t.Fatal("expected CFA frame")
	}
}

// --- debayer (white-box: construct decoded directly) ---

func cfaFrame(w, h int, mono []float32, pat string) *decoded {
	b, _ := parseBayer(pat, 0, 0)
	return &decoded{W: w, H: h, Channels: 1, Pix: mono, isCFA: true, bayer: b}
}

func TestSuperpixelChannelRouting(t *testing.T) {
	// One RGGB quad: R=0.9 (0,0), G=0.5/0.6 (0,1)/(1,0), B=0.1 (1,1).
	mono := []float32{0.9, 0.5, 0.6, 0.1}
	b, _ := parseBayer("RGGB", 0, 0)
	planes, ow, oh := superpixel(mono, 2, 2, b)
	if ow != 1 || oh != 1 {
		t.Fatalf("dims %dx%d", ow, oh)
	}
	if planes[cRed][0] != 0.9 || planes[cBlue][0] != 0.1 {
		t.Errorf("R=%f B=%f", planes[cRed][0], planes[cBlue][0])
	}
	if math.Abs(float64(planes[cGreen][0]-0.55)) > 1e-6 {
		t.Errorf("G=%f want 0.55", planes[cGreen][0])
	}
}

func TestBilinearPreservesDimsAndKnownChannel(t *testing.T) {
	mono := make([]float32, 16) // 4x4
	for i := range mono {
		mono[i] = float32(i) / 16
	}
	b, _ := parseBayer("RGGB", 0, 0)
	out := bilinear(mono, 4, 4, b)
	if len(out[cRed]) != 16 {
		t.Fatalf("bilinear should preserve 4x4=16, got %d", len(out[cRed]))
	}
	// (0,0) is a red site → red channel equals the raw value there.
	if out[cRed][0] != mono[0] {
		t.Errorf("red site: got %f want %f", out[cRed][0], mono[0])
	}
}

func TestBottomUpFlipMono(t *testing.T) {
	// 1x2 mono, bottom-up: the rendered top row is the originally-bottom pixel.
	d := &decoded{W: 1, H: 2, Channels: 1, Pix: []float32{0.2, 0.8}, bottomUp: true}
	img := renderToImage(d, 1, 2).(*image.Gray)
	top := img.GrayAt(0, 0).Y
	bot := img.GrayAt(0, 1).Y
	if top <= bot {
		t.Errorf("bottom-up not flipped: top=%d bot=%d (want top>bot from value 0.8 over 0.2)", top, bot)
	}
}

// --- stretch ---

func TestDegenerateGuardPassthrough(t *testing.T) {
	flat := make([]float32, 64)
	for i := range flat {
		flat[i] = 0.3
	}
	s := computeStretch(flat, targetBackground, shadowsClip)
	if !s.passthrough {
		t.Fatal("flat channel should pass through")
	}
	out := s.apply(flat)
	if math.Abs(float64(out[0]-0.3)) > 1e-6 {
		t.Errorf("passthrough value = %f, want 0.3", out[0])
	}
}

func TestStretchLiftsMidtone(t *testing.T) {
	// A low-signal noisy plane should be lifted so its median moves toward target.
	pix := make([]float32, 1000)
	for i := range pix {
		pix[i] = 0.02 + float32(i%7)*0.001 // low median, small spread
	}
	s := computeStretch(pix, targetBackground, shadowsClip)
	if s.passthrough {
		t.Fatal("noisy plane should not be degenerate")
	}
	out := s.apply(pix)
	med, _ := medianMADN(out)
	if med < 0.1 { // lifted well above the raw ~0.02
		t.Errorf("stretched median %f not lifted", med)
	}
}

// --- fitDims / never upscale ---

func TestFitDimsNeverUpscale(t *testing.T) {
	if w, h := fitDims(100, 80, 256); w != 100 || h != 80 {
		t.Errorf("small image upscaled: %dx%d", w, h)
	}
	if w, h := fitDims(4000, 2000, 2048); w != 2048 || h != 1024 {
		t.Errorf("downscale: %dx%d want 2048x1024", w, h)
	}
	if w, h := fitDims(4000, 2000, 0); w != 4000 || h != 2000 {
		t.Errorf("full (maxDim 0) resized: %dx%d", w, h)
	}
}

func TestBoxDimsNeverUpscale(t *testing.T) {
	d := &decoded{W: 1000, H: 500}
	if w, h := d.boxDims(5000, 5000); w != 1000 || h != 500 {
		t.Errorf("box above native upscaled: %dx%d", w, h)
	}
	if w, h := d.boxDims(400, 400); w != 400 || h != 200 {
		t.Errorf("box fit: %dx%d want 400x200", w, h)
	}
}
