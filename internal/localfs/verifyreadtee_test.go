package localfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyInputHash is what a zero-length range hashes to. It is written out here
// rather than imported so this package's tests stay as format-blind as the package
// itself: the constant is a property of SHA-256, not of FITS.
var emptyInputHash = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// rangeHash is the independent expectation: a plain SHA-256 of b[lo:hi].
func rangeHash(b []byte, lo, hi int) string {
	sum := sha256.Sum256(b[lo:hi])
	return hex.EncodeToString(sum[:])
}

// TestVerifyReadTeeOffsets covers every answer the offset argument can produce,
// against both the real platform implementation and the Fake, so the two cannot
// drift. The offsets are plain byte counts: this seam parses nothing.
func TestVerifyReadTeeOffsets(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("frame-bytes-"), 900) // 10800 bytes, not a sector multiple
	p := writeFile(t, dir, data)
	size := int64(len(data))

	cases := []struct {
		name   string
		offset int64
		want   string
	}{
		{"no-data-unit-known", 0, ""},
		{"negative-offset", -1, ""},
		{"whole-file", 1, rangeHash(data, 1, len(data))},
		{"mid-file", 2880, rangeHash(data, 2880, len(data))},
		{"at-size-is-empty-range", size, emptyInputHash},
		{"past-size-is-empty-range", size + 4096, emptyInputHash},
	}

	for _, impl := range []struct {
		name string
		fs   LocalFS
	}{{"OS", New()}, {"Fake", NewFake(dir)}} {
		for _, c := range cases {
			whole, got, err := impl.fs.VerifyReadTee(p, size, c.offset)
			if err != nil {
				t.Fatalf("%s/%s: VerifyReadTee: %v", impl.name, c.name, err)
			}
			if want := plainHash(data, len(data)); whole != want {
				t.Errorf("%s/%s: whole digest = %s, want %s", impl.name, c.name, whole, want)
			}
			if got != c.want {
				t.Errorf("%s/%s: data digest = %q, want %q", impl.name, c.name, got, c.want)
			}
		}
	}
}

// TestVerifyReadTeeMatchesVerifyRead pins the variant to the method it extends:
// the whole-file digest must be the same value VerifyRead has always returned,
// including its tail handling when the file on disk is longer than `size`.
func TestVerifyReadTeeMatchesVerifyRead(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte{0xA5}, 9000)
	p := writeFile(t, dir, data)

	for _, size := range []int64{0, 1, 4095, 4096, 4097, 8000, 9000} {
		want, err := New().VerifyRead(p, size)
		if err != nil {
			t.Fatalf("size=%d VerifyRead: %v", size, err)
		}
		got, _, err := New().VerifyReadTee(p, size, 100)
		if err != nil {
			t.Fatalf("size=%d VerifyReadTee: %v", size, err)
		}
		if got != want {
			t.Errorf("size=%d: tee whole digest = %s, want %s", size, got, want)
		}
	}
}

// TestVerifyReadTeeShortFile keeps the corruption discrimination: a file shorter
// than its recorded size is ErrShortFile through the tee exactly as through
// VerifyRead, not a silently shorter digest.
func TestVerifyReadTeeShortFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, []byte("short"))
	for _, fs := range []LocalFS{New(), NewFake(dir)} {
		if _, _, err := fs.VerifyReadTee(p, 100, 2); !errors.Is(err, ErrShortFile) {
			t.Errorf("VerifyReadTee on a truncated file: want ErrShortFile, got %v", err)
		}
	}
}

// TestHashNTeeIsOnePass is the read-count assertion behind the "no second
// whole-file pass" rule: an instrumented reader proves both digests come out of a
// single traversal of the source. It is asserted here, at hashNTee, because that
// is where the property lives — every platform's VerifyRead and VerifyReadTee
// makes exactly one hashNTee call per invocation and hands it the only descriptor
// it opens, so one pass through hashNTee is one pass over the file. The other half
// of the rule — that a CONSUMER does not call the seam twice for one file — can
// only be asserted where a consumer exists, and belongs to the steps that add one.
func TestHashNTeeIsOnePass(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 1000)
	src := &countingReader{r: bytes.NewReader(data)}

	whole, part, err := hashNTee(src, int64(len(data)), 2880)
	if err != nil {
		t.Fatalf("hashNTee: %v", err)
	}
	if src.read != int64(len(data)) {
		t.Errorf("read %d bytes, want %d: the tee must cost one pass", src.read, len(data))
	}
	if want := plainHash(data, len(data)); whole != want {
		t.Errorf("whole digest = %s, want %s", whole, want)
	}
	if want := rangeHash(data, 2880, len(data)); part != want {
		t.Errorf("data digest = %s, want %s", part, want)
	}
}

// countingReader records how many bytes a consumer pulled.
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// TestFakeVerifyReadTeeRidesTheSameRead proves the tee is not a second, buffered
// read alongside the cache-bypassing one: the Fake's at-rest corruption hook is
// what stands in for a disk that lies, and a tee reading independently of it would
// keep reporting the honest digest.
func TestFakeVerifyReadTeeRidesTheSameRead(t *testing.T) {
	dir := t.TempDir()
	data := []byte("verified-on-disk-with-a-data-unit")
	p := writeFile(t, dir, data)
	honest := plainHash(data, len(data))

	f := NewFake(dir)
	f.CorruptAfterWrite = func(_ string, honest string) string { return "deadbeef" + honest[8:] }

	whole, part, err := f.VerifyReadTee(p, int64(len(data)), 8)
	if err != nil {
		t.Fatalf("VerifyReadTee: %v", err)
	}
	if whole == honest {
		t.Error("CorruptAfterWrite did not reach the tee's whole-file digest")
	}
	if want := f.CorruptAfterWrite(p, honest); whole != want {
		t.Errorf("whole digest = %s, want the corrupted %s", whole, want)
	}
	// The hook doctors one value — the whole-file digest the at-rest verify
	// compares — so the data digest stays the honest one the same read produced.
	if want := rangeHash(data, 8, len(data)); part != want {
		t.Errorf("data digest = %s, want %s", part, want)
	}
}

// TestPackageHasNoFormatKnowledge asserts the property the digest's design is
// buying with the offset argument: the FITS block structure lives in
// internal/header alone. This package must not import it and must carry none of
// its magic numbers, or the tee would have quietly become a second FITS parser.
func TestPackageHasNoFormatKnowledge(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			for _, imp := range file.Imports {
				if strings.Contains(imp.Path.Value, "internal/header") {
					t.Errorf("%s imports %s: localfs must stay format-blind", name, imp.Path.Value)
				}
			}
		}
	}

	// An import path is only half of it: a hand-rolled block scan would need no
	// import at all, so the sources are also read for the constants that would
	// betray one.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, marker := range []string{"2880", "internal/header", `"END"`} {
			if bytes.Contains(src, []byte(marker)) {
				t.Errorf("%s mentions %s: FITS structure belongs in internal/header", e.Name(), marker)
			}
		}
	}
}
