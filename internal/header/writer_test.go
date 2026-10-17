package header

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header/fitstest"
)

// card renders one 80-column card image space-padded from s.
func card(s string) []byte { return fitstest.Card(s) }

// buildFITS assembles a minimal but real FITS file: the given cards followed by
// END, blank-padded to a whole number of 2880-byte header blocks, then the data
// unit zero-padded to a whole number of blocks. This exercises the writer's real
// byte layout (header blocks + a data unit the complement must cover). The bytes
// come from fitstest so that every synthetic FITS file in the repository — these,
// the committed testdata fixture, and the revision pairs other packages build — is
// laid out by one writer.
func buildFITS(cards []string, data []byte) []byte { return fitstest.Build(cards, data) }

// writeFITS writes a fixture file and returns its path.
func writeFITS(t *testing.T, dir, name string, cards []string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buildFITS(cards, data), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return p
}

// complementOf recomputes the SHA-256 over every byte of path outside the 80-byte
// window at off — the same set the writer digests, used to independently check the
// digest's sensitivity to non-OBJECT bytes.
func complementOf(t *testing.T, path string, off int64) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	h := sha256.New()
	h.Write(data[:off])
	h.Write(data[off+cardSize:])
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

var baseCards = []string{
	"SIMPLE  =                    T / conforms to FITS standard",
	"BITPIX  =                    8 / bits per data pixel",
	"NAXIS   =                    0 / number of data axes",
	"IMAGETYP= 'Light   '           / Type of image",
	"OBJECT  = 'Moon'               / name of object being imaged",
	"FILTER  = 'HA      '           / Filter used",
}

func TestRewriteObject_OnlyObjectCardChanges(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("pixel-data-goes-here"))
	dst := filepath.Join(dir, "dst.fit")

	res, err := RewriteObject(src, dst, "NGC 7000")
	if err != nil {
		t.Fatalf("RewriteObject: %v", err)
	}

	// INV-4: complements equal => only the OBJECT window differs.
	if res.ComplementIn != res.ComplementOut {
		t.Errorf("ComplementIn != ComplementOut: a byte outside the OBJECT card changed")
	}

	srcBytes, _ := os.ReadFile(src)
	dstBytes, _ := os.ReadFile(dst)
	if len(srcBytes) != len(dstBytes) {
		t.Fatalf("size changed: src %d, dst %d", len(srcBytes), len(dstBytes))
	}
	off := res.Offset
	if !bytes.Equal(srcBytes[:off], dstBytes[:off]) || !bytes.Equal(srcBytes[off+cardSize:], dstBytes[off+cardSize:]) {
		t.Errorf("bytes outside the OBJECT card window differ between src and dst")
	}
	if bytes.Equal(srcBytes[off:off+cardSize], dstBytes[off:off+cardSize]) {
		t.Errorf("OBJECT card window did not change")
	}
	if !bytes.Equal(dstBytes[off:off+cardSize], res.NewCard[:]) {
		t.Errorf("dst OBJECT window != res.NewCard")
	}

	// Re-read via the real reader: the new OBJECT is what we asked for.
	h, err := New().ReadHeader(dst)
	if err != nil {
		t.Fatalf("ReadHeader(dst): %v", err)
	}
	if got, _ := h.Str("OBJECT"); got != "NGC 7000" {
		t.Errorf("OBJECT = %q, want %q", got, "NGC 7000")
	}
}

func TestRewriteObject_RefusesDuplicateObject(t *testing.T) {
	dir := t.TempDir()
	cards := append([]string{}, baseCards...)
	cards = append(cards, "OBJECT  = 'Mars'               / a second, pathological OBJECT card")
	src := writeFITS(t, dir, "dup.fit", cards, []byte("data"))

	if _, err := RewriteObject(src, filepath.Join(dir, "out.fit"), "NGC 7000"); err == nil {
		t.Errorf("RewriteObject with duplicate OBJECT = nil error, want error")
	}
}

func TestRewriteObject_RefusesNoObject(t *testing.T) {
	dir := t.TempDir()
	cards := []string{
		"SIMPLE  =                    T / conforms to FITS standard",
		"BITPIX  =                    8",
		"FILTER  = 'HA      '",
	}
	src := writeFITS(t, dir, "noobj.fit", cards, []byte("data"))

	if _, err := RewriteObject(src, filepath.Join(dir, "out.fit"), "NGC 7000"); err == nil {
		t.Errorf("RewriteObject with no OBJECT = nil error, want error")
	}
}

func TestRewriteObject_RefusesCommentaryObject(t *testing.T) {
	// An OBJECT keyword on a commentary card (no "= " value indicator) is not a
	// value card and must not be treated as the target.
	dir := t.TempDir()
	cards := []string{
		"SIMPLE  =                    T",
		"OBJECT    this is commentary, not a value card",
	}
	src := writeFITS(t, dir, "commentary.fit", cards, []byte("data"))

	if _, err := RewriteObject(src, filepath.Join(dir, "out.fit"), "NGC 7000"); err == nil {
		t.Errorf("RewriteObject with only a commentary OBJECT = nil error, want error")
	}
}

func TestRewriteObject_RefusesOverLength(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))

	long := ""
	for len(long) <= maxTargetLen {
		long += "X"
	}
	if _, err := RewriteObject(src, filepath.Join(dir, "out.fit"), long); err == nil {
		t.Errorf("RewriteObject with over-length target = nil error, want error")
	}
}

func TestRewriteObject_RefusesEmptyTarget(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))

	// "." sanitizes to "" (trailing-dot trim), the empty-after-sanitize case.
	if _, err := RewriteObject(src, filepath.Join(dir, "out.fit"), "."); err == nil {
		t.Errorf("RewriteObject with empty-after-sanitize target = nil error, want error")
	}
}

func TestRewriteObject_MinPadding(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))
	dst := filepath.Join(dir, "dst.fit")

	res, err := RewriteObject(src, dst, "M31")
	if err != nil {
		t.Fatalf("RewriteObject: %v", err)
	}

	// FITS fixed-format minimum: the closing quote must land at column >= 20
	// (0-indexed >= 19).
	last := bytes.LastIndexByte(bytes.TrimRight(res.NewCard[:], " "), '\'')
	if last < 19 {
		t.Errorf("closing quote at column %d, want >= 20", last+1)
	}
	h, _ := New().ReadHeader(dst)
	if got, _ := h.Str("OBJECT"); got != "M31" {
		t.Errorf("OBJECT = %q, want %q", got, "M31")
	}
}

func TestRewriteObject_EmbeddedQuoteRoundTrips(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("data"))
	dst := filepath.Join(dir, "dst.fit")

	const target = "Barnard's Loop"
	res, err := RewriteObject(src, dst, target)
	if err != nil {
		t.Fatalf("RewriteObject: %v", err)
	}
	if res.ComplementIn != res.ComplementOut {
		t.Errorf("ComplementIn != ComplementOut")
	}
	// The doubled-quote escape must be present in the raw card image.
	if !bytes.Contains(res.NewCard[:], []byte("Barnard''s Loop")) {
		t.Errorf("new card %q missing doubled-quote escape", string(res.NewCard[:]))
	}
	h, err := New().ReadHeader(dst)
	if err != nil {
		t.Fatalf("ReadHeader(dst): %v", err)
	}
	if got, _ := h.Str("OBJECT"); got != target {
		t.Errorf("OBJECT = %q, want %q", got, target)
	}
}

// TestRewriteObject_ComplementCatchesDataCorruption is the INV-4 protection: the
// complement digest must be sensitive to any byte outside the OBJECT window. A
// faithful edit yields equal digests; corrupting one data byte makes the
// complement of that corrupted stream diverge from the input's.
func TestRewriteObject_ComplementCatchesDataCorruption(t *testing.T) {
	dir := t.TempDir()
	src := writeFITS(t, dir, "src.fit", baseCards, []byte("pixel-data-goes-here"))
	dst := filepath.Join(dir, "dst.fit")

	res, err := RewriteObject(src, dst, "NGC 7000")
	if err != nil {
		t.Fatalf("RewriteObject: %v", err)
	}
	// Sanity: recomputing the output complement matches what the writer returned.
	if complementOf(t, dst, res.Offset) != res.ComplementOut {
		t.Fatalf("independent complement of dst != res.ComplementOut")
	}

	// Corrupt a data-unit byte (the last byte, well outside the OBJECT window) and
	// recompute the complement: it must differ from the input complement.
	corrupt := filepath.Join(dir, "corrupt.fit")
	dstBytes, _ := os.ReadFile(dst)
	dstBytes[len(dstBytes)-1] ^= 0xFF
	if err := os.WriteFile(corrupt, dstBytes, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if complementOf(t, corrupt, res.Offset) == res.ComplementIn {
		t.Errorf("complement unchanged after corrupting a data byte; INV-4 protection is blind")
	}
}

// TestBuildObjectCard_GoldenBytes freezes the canonical OBJECT card image byte-for-
// byte. Re-apply (design routing-and-filing §9.4) and restore (design
// routing-and-filing §9.4) reproduce this card and require the result to hash back
// to the stored retarget_sha256, so any change to the layout — the "OBJECT = '"
// prefix, the min-pad, or the closing quote — would break every
// previously-retargeted frame. The expected images use hardcoded pad counts (not the
// production constants) so a change to those constants fails here loudly.
func TestBuildObjectCard_GoldenBytes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []byte
	}{
		// "M31" → 5 blank pad chars (min 8 value chars), closing quote at column 20.
		{"short", "M31", card("OBJECT  = '" + "M31" + strings.Repeat(" ", 5) + "'")},
		// "O'Ryan" → escaped "O''Ryan" (7 chars) + 1 pad char, closing quote at column 20.
		{"embedded-quote", "O'Ryan", card("OBJECT  = '" + "O''Ryan" + strings.Repeat(" ", 1) + "'")},
	}
	for _, c := range cases {
		got, err := buildObjectCard(c.value)
		if err != nil {
			t.Fatalf("%s: buildObjectCard(%q): %v", c.name, c.value, err)
		}
		if !bytes.Equal(got[:], c.want) {
			t.Errorf("%s: buildObjectCard(%q) golden mismatch\n got %q\nwant %q",
				c.name, c.value, string(got[:]), string(c.want))
		}
	}
}
