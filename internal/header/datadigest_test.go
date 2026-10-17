package header

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"

	"github.com/macourteau/asiair-commander/internal/header/fitstest"
)

// hexSHA is the independent expectation the tee is checked against: a plain
// SHA-256 of a byte slice, computed without any of the code under test.
func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// teeOf streams b through a DigestTee the way a consumer does — as one writer of
// an io.MultiWriter fed by an io.Copy — and returns both digests.
func teeOf(t *testing.T, b []byte) (whole, data string) {
	t.Helper()
	tee := NewDigestTee()
	if _, err := io.Copy(io.MultiWriter(io.Discard, tee), bytes.NewReader(b)); err != nil {
		t.Fatalf("stream through tee: %v", err)
	}
	return tee.Sums()
}

// writeTemp writes b to a temp file and returns its path.
func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestEmptyDataDigestIsSHA256OfNothing(t *testing.T) {
	if got := hexSHA(nil); got != EmptyDataDigest {
		t.Errorf("EmptyDataDigest = %q, want %q", EmptyDataDigest, got)
	}
}

func TestIsUsableDataDigest(t *testing.T) {
	cases := map[string]bool{
		"":                    false, // an unparseable header's answer
		EmptyDataDigest:       false, // every zero-length data unit's answer
		hexSHA([]byte("abc")): true,
	}
	for d, want := range cases {
		if got := IsUsableDataDigest(d); got != want {
			t.Errorf("IsUsableDataDigest(%q) = %v, want %v", d, got, want)
		}
	}
}

// TestTeeAndOffsetAgree is the two-shapes check: the streaming tee, which finds
// the data unit by scanning the bytes as they pass, and DataOffset, which finds it
// by reading header blocks off disk, must name the same data unit.
func TestTeeAndOffsetAgree(t *testing.T) {
	data := fitstest.SyntheticImage(60, 48)
	file := fitstest.Build(fitstest.LightCards(60, 48, "NGC 7000"), data)
	path := writeTemp(t, "light.fit", file)

	whole, teeData := teeOf(t, file)
	if whole != hexSHA(file) {
		t.Errorf("tee whole digest = %q, want %q", whole, hexSHA(file))
	}

	off, err := DataOffset(path)
	if err != nil {
		t.Fatalf("DataOffset: %v", err)
	}
	if off != blockSize {
		t.Errorf("DataOffset = %d, want %d (a one-block header)", off, blockSize)
	}
	if want := hexSHA(file[off:]); teeData != want {
		t.Errorf("tee data digest = %q, want %q (bytes from the offset lookup)", teeData, want)
	}
	if !IsUsableDataDigest(teeData) {
		t.Errorf("a non-empty data unit must yield a usable digest, got %q", teeData)
	}
}

// TestRevisionPairSharesDataDigest is the core claim of design frame-lifecycle
// §11.3: the ASIAIR's in-place header rewrite changes every whole-file byte
// offset and the whole-file digest with it, while the data unit — the exposure
// itself — is untouched and hashes the same.
func TestRevisionPairSharesDataDigest(t *testing.T) {
	data := fitstest.SyntheticImage(60, 48)
	original, revised := fitstest.Revision(fitstest.LightCards(60, 48, "SH2-132"), data)

	if len(revised) != len(original)+blockSize {
		t.Fatalf("revised is %d bytes, want %d (one header block more)", len(revised), len(original)+blockSize)
	}

	origWhole, origData := teeOf(t, original)
	revWhole, revData := teeOf(t, revised)

	if origWhole == revWhole {
		t.Errorf("whole-file digests match across the rewrite; the pair is not a revision")
	}
	if origData != revData {
		t.Errorf("data-unit digests differ across a header-only rewrite:\n orig %s\n rev  %s", origData, revData)
	}
	if !IsUsableDataDigest(origData) {
		t.Errorf("data digest %q is not usable as an identity", origData)
	}
	// And the digest really is over the exposure's bytes, not an artefact of the
	// scan: the same pixels hashed directly agree with it.
	if want := hexSHA(data); origData != want {
		t.Errorf("data digest = %q, want %q (the image bytes alone)", origData, want)
	}
}

// TestTeeUnparseableHeaderNeverFails pins the contract that keeps a rotted archive
// copy's alert critical: garbage in yields the honest whole-file digest, an empty
// data digest, and no error at all.
func TestTeeUnparseableHeaderNeverFails(t *testing.T) {
	cases := map[string][]byte{
		"not-fits-at-all":  []byte("this is not a FITS file"),
		"block-of-garbage": bytes.Repeat([]byte{'x'}, blockSize*3),
		// Cut before the END card: nothing in the stream says where a data unit
		// would start.
		"header-truncated": fitstest.Build(fitstest.LightCards(8, 8, "M31"), nil)[:cardSize*12+40],
		"empty":            {},
		"no-end-in-budget": bytes.Repeat(fitstest.Card("COMMENT   filler"), maxHeaderBlocks*blockSize/cardSize+1),
	}
	for name, b := range cases {
		tee := NewDigestTee()
		n, err := tee.Write(b)
		if err != nil || n != len(b) {
			t.Fatalf("%s: Write = (%d, %v), want (%d, nil)", name, n, err, len(b))
		}
		whole, data := tee.Sums()
		if whole != hexSHA(b) {
			t.Errorf("%s: whole digest = %q, want %q", name, whole, hexSHA(b))
		}
		if data != "" {
			t.Errorf("%s: data digest = %q, want the empty string", name, data)
		}
		if IsUsableDataDigest(data) {
			t.Errorf("%s: an unparseable header must not yield a usable identity", name)
		}
	}
}

// TestHeaderOnlyFileYieldsEmptyInputDigest separates the two refusals: a file that
// parses but carries no data unit hashes to the empty-input digest — a real,
// recordable value — rather than to the empty string, and IsUsableDataDigest
// refuses it all the same.
func TestHeaderOnlyFileYieldsEmptyInputDigest(t *testing.T) {
	file := fitstest.Build(fitstest.LightCards(0, 0, "M31"), nil)
	if len(file) != blockSize {
		t.Fatalf("header-only fixture is %d bytes, want one block", len(file))
	}

	_, data := teeOf(t, file)
	if data != EmptyDataDigest {
		t.Errorf("data digest = %q, want EmptyDataDigest", data)
	}
	if IsUsableDataDigest(data) {
		t.Errorf("a zero-length data unit must never be a usable identity")
	}

	path := writeTemp(t, "headeronly.fit", file)
	off, err := DataOffset(path)
	if err != nil {
		t.Fatalf("DataOffset: %v", err)
	}
	if off != int64(len(file)) {
		t.Errorf("DataOffset = %d, want %d (the data unit starts at EOF)", off, len(file))
	}
}

// TestTruncatedInsideHeaderBlockDoesNotParse covers the file cut after its END
// card but inside the header block carrying it. A header is a whole number of
// blocks, so that file has no complete header and ReadHeader says so; both digest
// shapes must agree with it rather than name a data unit that begins past EOF.
func TestTruncatedInsideHeaderBlockDoesNotParse(t *testing.T) {
	full := fitstest.Build(fitstest.LightCards(8, 8, "M31"), []byte("pixels"))
	// Three cuts: past END but inside its block, and two earlier ones.
	for _, cut := range []int{blockSize - cardSize, 2000, 1000} {
		b := full[:cut]

		if _, err := New().ReadHeader(writeTemp(t, "cut.fit", b)); err == nil {
			t.Fatalf("cut=%d: ReadHeader accepted a partial header; the premise of this test is wrong", cut)
		}
		if _, data := teeOf(t, b); data != "" {
			t.Errorf("cut=%d: tee data digest = %q, want the empty string", cut, data)
		}
		off, err := DataOffset(writeTemp(t, "cut.fit", b))
		if err != nil {
			t.Errorf("cut=%d: DataOffset must not fail on a header that does not parse: %v", cut, err)
		}
		if off != 0 {
			t.Errorf("cut=%d: DataOffset = %d, want 0 (no data unit known)", cut, off)
		}
	}
}

// TestTeeIsIndifferentToChunking guards the card-spanning scan state: a consumer
// hands the tee whatever the network or the copy buffer gives it, so a card, a
// block boundary or the END card itself can be split across writes.
func TestTeeIsIndifferentToChunking(t *testing.T) {
	file := fitstest.Build(fitstest.LightCards(60, 48, "M31"), fitstest.SyntheticImage(60, 48))
	wantWhole, wantData := teeOf(t, file)

	for _, chunk := range []int{1, 3, 7, 79, 80, 81, 2879, 2880, 2881, 4096} {
		tee := NewDigestTee()
		for off := 0; off < len(file); off += chunk {
			end := off + chunk
			if end > len(file) {
				end = len(file)
			}
			if _, err := tee.Write(file[off:end]); err != nil {
				t.Fatalf("chunk=%d: Write: %v", chunk, err)
			}
		}
		whole, data := tee.Sums()
		if whole != wantWhole || data != wantData {
			t.Errorf("chunk=%d: sums (%s, %s), want (%s, %s)", chunk, whole, data, wantWhole, wantData)
		}
	}
}

// countingReader records how many bytes a consumer pulled, so a test can assert a
// read count rather than only a digest value.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// TestTeeCostsOneWholeFilePass is the read-count assertion: both digests come out
// of a single traversal of the source, which is what forbids a consumer from
// re-opening the file to hash its data unit.
func TestTeeCostsOneWholeFilePass(t *testing.T) {
	file := fitstest.Build(fitstest.LightCards(60, 48, "M31"), fitstest.SyntheticImage(60, 48))
	src := &countingReader{r: bytes.NewReader(file)}

	tee := NewDigestTee()
	if _, err := io.CopyBuffer(io.MultiWriter(io.Discard, tee), src, make([]byte, 4096)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	whole, data := tee.Sums()

	if src.read != int64(len(file)) {
		t.Errorf("read %d bytes, want %d: both digests must come from one pass", src.read, len(file))
	}
	if whole != hexSHA(file) || data != hexSHA(file[blockSize:]) {
		t.Errorf("sums from the single pass are wrong: (%s, %s)", whole, data)
	}
}

// TestCommittedFixtureMatchesGenerator keeps the committed fixture readable: its
// bytes are exactly what fitstest.SynthLight produces, so a reader can see what is
// in it without a hex dump, and it carries a real, usable data unit.
func TestCommittedFixtureMatchesGenerator(t *testing.T) {
	path := filepath.Join("testdata", fitstest.SynthLightName)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if want := fitstest.SynthLight(); !bytes.Equal(got, want) {
		t.Fatalf("%s is %d bytes, generator produces %d; regenerate it from fitstest.SynthLight",
			path, len(got), len(want))
	}

	off, err := DataOffset(path)
	if err != nil {
		t.Fatalf("DataOffset: %v", err)
	}
	if off != blockSize {
		t.Errorf("DataOffset = %d, want %d", off, blockSize)
	}
	whole, data := teeOf(t, got)
	if whole != hexSHA(got) {
		t.Errorf("whole digest = %q, want %q", whole, hexSHA(got))
	}
	if !IsUsableDataDigest(data) {
		t.Errorf("the one fixture with a data unit must yield a usable digest, got %q", data)
	}
	if _, err := New().ReadHeader(path); err != nil {
		t.Errorf("the fixture must parse as FITS: %v", err)
	}
}

func TestDataOffsetUnparseableAndMissing(t *testing.T) {
	dir := t.TempDir()
	unparseable := filepath.Join(dir, "garbage.fit")
	if err := os.WriteFile(unparseable, []byte("not a FITS file"), 0o600); err != nil {
		t.Fatal(err)
	}
	off, err := DataOffset(unparseable)
	if err != nil {
		t.Errorf("DataOffset on an unparseable file must not fail, got %v", err)
	}
	if off != 0 {
		t.Errorf("DataOffset = %d, want 0 (no data unit known)", off)
	}

	if _, err := DataOffset(filepath.Join(dir, "absent.fit")); err == nil {
		t.Errorf("DataOffset on a missing file = nil error, want an I/O error")
	}
}

// TestStreamDigests pins the one streaming digest read: the digests it returns
// are the tee's over the whole stream, it reads in CopyBufSize buffers and
// reports the running total after each, it stops between buffers once stop
// reports true, and both hooks are optional.
func TestStreamDigests(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789abcdef"), 3*CopyBufSize/16+7) // three full buffers and a tail
	wantWhole, wantData := teeOf(t, content)

	t.Run("nil hooks read the whole stream", func(t *testing.T) {
		whole, data, err := StreamDigests(bytes.NewReader(content), nil, nil)
		if err != nil {
			t.Fatalf("StreamDigests: %v", err)
		}
		if whole != wantWhole || data != wantData {
			t.Fatalf("digests (%s, %s), want the tee's (%s, %s)", whole, data, wantWhole, wantData)
		}
	})

	t.Run("progress reports the running total after every buffer", func(t *testing.T) {
		var got []int64
		if _, _, err := StreamDigests(bytes.NewReader(content), nil, func(n int64) { got = append(got, n) }); err != nil {
			t.Fatalf("StreamDigests: %v", err)
		}
		want := []int64{CopyBufSize, 2 * CopyBufSize, 3 * CopyBufSize, int64(len(content))}
		if len(got) != len(want) {
			t.Fatalf("progress %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("progress %v, want %v", got, want)
			}
		}
	})

	t.Run("stop ends the stream between buffers", func(t *testing.T) {
		var read int64
		stops := 0
		stop := func() bool {
			stops++
			return read >= CopyBufSize // one buffer in, the next read is refused
		}
		_, _, err := StreamDigests(bytes.NewReader(content), stop, func(n int64) { read = n })
		if !errors.Is(err, ErrStreamStopped) {
			t.Fatalf("err %v, want ErrStreamStopped", err)
		}
		if read != CopyBufSize || stops != 2 {
			t.Fatalf("read %d bytes over %d stop checks, want one buffer (%d) and a check before each read", read, stops, CopyBufSize)
		}
	})

	t.Run("a read error is returned as is", func(t *testing.T) {
		boom := errors.New("boom")
		_, _, err := StreamDigests(io.MultiReader(bytes.NewReader(content[:100]), iotest.ErrReader(boom)), nil, nil)
		if !errors.Is(err, boom) {
			t.Fatalf("err %v, want the read error", err)
		}
	})
}
