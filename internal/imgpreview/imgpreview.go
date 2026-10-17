// Package imgpreview renders STF-autostretched JPEG previews of downloaded FITS
// frames and caches them, keyed by content digest, at a small set of named sizes
// (plus uncached arbitrary sizes). It debayers one-shot-color frames so colour
// captures render in colour, and applies a PixInsight-style midtones-transfer
// auto-stretch so the otherwise-black raw data is legible.
//
// The rendering pipeline (decode → optional debayer → per-channel STF → downscale
// → JPEG) is decode-heavy, so a bounded worker pool caps concurrency and a
// content-addressed on-disk cache with an LRU bound serves repeat requests without
// re-decoding.
package imgpreview

import "context"

// Size is a named preview size: a longest-edge pixel bound (aspect preserved,
// never upscaled), each cached on disk.
type Size string

const (
	SizeThumb  Size = "thumb"  // 256 px — list-row thumbnails (eager-warmed)
	SizeCard   Size = "card"   // 1024 px — inline detail preview
	SizeScreen Size = "screen" // 2048 px — lightbox initial view; also the stretch proxy bound
	SizeFull   Size = "full"   // native resolution — lightbox zoom / download / copy
)

// proxyDim is the longest-edge bound of the plane the STF statistics are computed
// on. Deriving the stretch from one fixed-resolution proxy and applying it to every
// size keeps all sizes (including full) visually identical.
const proxyDim = 2048

// jpegQuality is the encode quality for every rendered preview.
const jpegQuality = 85

// ParamsVersion is baked into each cache filename; bump it when the render pipeline
// or stretch constants change so old files are invalidated rather than orphaned.
const ParamsVersion = "v1"

// STF constants — the values that make the auto-stretch look correct. Shadows are
// clipped at median + shadowsClip·MADN (MADN units, negative); the midtones transfer
// function then pins the clipped median to targetBackground.
const (
	targetBackground = 0.25
	shadowsClip      = -2.8
)

// maxDim returns a named size's longest-edge bound, or 0 for SizeFull (no cap).
func (s Size) maxDim() int {
	switch s {
	case SizeThumb:
		return 256
	case SizeCard:
		return 1024
	case SizeScreen:
		return 2048
	case SizeFull:
		return 0
	}
	return 0
}

// ParseSize maps a size name to a Size; ok=false for anything else.
func ParseSize(s string) (Size, bool) {
	switch Size(s) {
	case SizeThumb, SizeCard, SizeScreen, SizeFull:
		return Size(s), true
	}
	return "", false
}

// Previewer is the seam the worker and web layer depend on. A nil Previewer means
// the feature is disabled: callers guard for it (the worker skips warming, the web
// handler returns 503).
type Previewer interface {
	// Warm best-effort pre-renders the thumbnail for a just-filed frame. It never
	// blocks the caller and never records a decode failure.
	Warm(sha256, destPath string)
	// RenderOnDemand renders (or serves from cache) a named size, returning the
	// path of a ready-on-disk JPEG. The caller opens its own handle.
	RenderOnDemand(ctx context.Context, sha256, destPath string, size Size) (path string, err error)
	// RenderResized renders an arbitrary bounding box (never upscaled) to an
	// in-memory JPEG, bypassing the cache.
	RenderResized(ctx context.Context, sha256, destPath string, maxW, maxH int) (jpeg []byte, err error)
	// Dimensions returns the frame's native pixel dimensions, read cheaply from the
	// FITS header (no pixel decode). Used to build the lightbox's zoom srcSet.
	Dimensions(destPath string) (w, h int, err error)
	// EvictDigest removes a frame's cached previews (best-effort, non-blocking).
	EvictDigest(sha256 string)
	// InFlight reports how many renders are currently executing (for metrics).
	InFlight() int
	// CacheStats reports the on-disk cache's entry count and total bytes (for metrics).
	CacheStats() (entries int, bytes int64)
}
