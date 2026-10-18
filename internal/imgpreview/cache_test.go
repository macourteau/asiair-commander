package imgpreview

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
)

func testCache(t *testing.T, maxBytes int64) *cache {
	t.Helper()
	return newCache(t.TempDir(), maxBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestCacheRoundtrip(t *testing.T) {
	c := testCache(t, 1<<30)
	if _, ok := c.cachedPath(digestA, SizeThumb); ok {
		t.Fatal("empty cache reported a hit")
	}
	data := make([]byte, 100)
	path, err := c.put(digestA, SizeThumb, data)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok := c.cachedPath(digestA, SizeThumb)
	if !ok || got != path {
		t.Fatalf("cachedPath after put: %q %v", got, ok)
	}
	if b, _ := os.ReadFile(path); len(b) != 100 {
		t.Errorf("cached file len %d", len(b))
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := testCache(t, 150) // holds one 100-byte entry, not two
	if _, err := c.put(digestA, SizeThumb, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.put(digestB, SizeThumb, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	// A (older) should be evicted; B (just written) retained.
	if _, ok := c.cachedPath(digestA, SizeThumb); ok {
		t.Error("LRU did not evict the older digest")
	}
	if _, ok := c.cachedPath(digestB, SizeThumb); !ok {
		t.Error("LRU evicted the just-written digest")
	}
}

func TestCacheSingleEntryAlwaysAdmitted(t *testing.T) {
	c := testCache(t, 10) // cap smaller than the entry
	path, err := c.put(digestA, SizeThumb, make([]byte, 100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("oversized single entry not admitted: %v", err)
	}
}

func TestCacheNegativeMarkerAndEvict(t *testing.T) {
	c := testCache(t, 1<<30)
	if c.hasFailMarker(digestA) {
		t.Fatal("unexpected marker")
	}
	c.writeFailMarker(digestA)
	if !c.hasFailMarker(digestA) {
		t.Fatal("marker not written")
	}
	c.put(digestA, SizeThumb, make([]byte, 10))
	c.evictDigest(digestA)
	if _, err := os.Stat(c.digestDir(digestA)); !os.IsNotExist(err) {
		t.Errorf("evictDigest left the dir: %v", err)
	}
	if c.hasFailMarker(digestA) {
		t.Error("evict should remove the marker too")
	}
}

func TestCacheStartupScanReclaimsTmp(t *testing.T) {
	dir := t.TempDir()
	c := newCache(dir, 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.put(digestA, SizeThumb, make([]byte, 100))
	// Simulate a crash-orphaned temp file.
	orphan := filepath.Join(c.digestDir(digestA), ".render-orphan.tmp")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A fresh cache over the same dir rebuilds the index and reaps the tmp.
	c2 := newCache(dir, 1<<30, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("startup scan left orphan tmp: %v", err)
	}
	if _, ok := c2.cachedPath(digestA, SizeThumb); !ok {
		t.Error("startup scan did not index existing file")
	}
}

// --- previewer integration through real synthetic FITS ---

func testPreviewer(t *testing.T, concurrency int) (Previewer, string) {
	t.Helper()
	dir := t.TempDir()
	p := New(dir, concurrency, 1<<30, header.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return p, dir
}

func sha(t *testing.T, s string) string {
	t.Helper()
	// A 64-hex-ish stand-in unique per test; content-addressing only needs a stable key.
	h := ""
	for len(h) < 64 {
		h += s
	}
	return h[:64]
}

func TestPreviewerRenderOnDemandCaches(t *testing.T) {
	p, dir := testPreviewer(t, 2)
	path := buildFITS(t, mono16Cards(64, 48, 32768), be16(make16(64*48)...))
	digest := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	got, err := p.RenderOnDemand(context.Background(), digest, path, SizeThumb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
	// A second call is a cache hit → same path, still under the cache dir.
	got2, _ := p.RenderOnDemand(context.Background(), digest, path, SizeThumb)
	if got2 != got {
		t.Errorf("cache miss on second call: %q vs %q", got2, got)
	}
	if rel, _ := filepath.Rel(dir, got); rel == "" || rel[:2] == ".." {
		t.Errorf("cache path escaped dir: %s", got)
	}
}

func TestPreviewerRenderResizedNeverUpscaleUncached(t *testing.T) {
	p, dir := testPreviewer(t, 2)
	path := buildFITS(t, mono16Cards(64, 48, 32768), be16(make16(64*48)...))
	digest := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	// Request a box larger than native → must clamp to 64x48, and write nothing.
	jpg, err := p.RenderResized(context.Background(), digest, path, 1000, 1000)
	if err != nil {
		t.Fatalf("resized: %v", err)
	}
	if len(jpg) == 0 {
		t.Fatal("empty jpeg")
	}
	// No cache directory should exist for this digest.
	dd := filepath.Join(dir, digest[0:2], digest[2:4], digest)
	if _, err := os.Stat(dd); !os.IsNotExist(err) {
		t.Errorf("arbitrary render wrote a cache dir: %v", err)
	}
}

func TestPreviewerConcurrency1FullNoDeadlock(t *testing.T) {
	// full → heavy weight min(2,1)=1, so a full render must complete at conc=1.
	p, _ := testPreviewer(t, 1)
	path := buildFITS(t, mono16Cards(64, 48, 32768), be16(make16(64*48)...))
	digest := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, err := p.RenderOnDemand(context.Background(), digest, path, SizeFull); err != nil {
		t.Fatalf("full render at concurrency 1: %v", err)
	}
}

func TestPreviewerDimensions(t *testing.T) {
	p, _ := testPreviewer(t, 2)
	path := buildFITS(t, mono16Cards(64, 48, 32768), be16(make16(64*48)...))
	w, h, err := p.Dimensions(path)
	if err != nil {
		t.Fatalf("dimensions: %v", err)
	}
	if w != 64 || h != 48 {
		t.Errorf("dims = %dx%d, want 64x48", w, h)
	}
}

// A width past int32 is not a dimension the lightbox can use; Dimensions refuses
// it rather than handing the zoom srcSet an absurd (or, on a 32-bit int, wrapped) value.
func TestPreviewerDimensionsRejectsOutOfRange(t *testing.T) {
	p, _ := testPreviewer(t, 2)
	path := buildFITS(t, mono16Cards(4294967298, 2, 32768), be16(0, 0, 0, 0))
	if w, h, err := p.Dimensions(path); err == nil {
		t.Fatalf("dimensions = %dx%d, want an error", w, h)
	}
}

func make16(n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(i%2000 - 1000) // some spread so the stretch isn't degenerate
	}
	return out
}

var _ = sha // reserved helper
