package imgpreview

import (
	"context"
	"fmt"
	"image"
	"log/slog"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/metrics"
)

// previewer is the concrete Previewer. One instance is shared process-wide by the
// worker (warming) and the web handler (serving).
type previewer struct {
	cache   *cache
	hdr     header.Reader
	sem     *semaphore.Weighted
	heavy   int64 // weight for a full-resolution (bilinear) render, min(2, concurrency)
	group   singleflight.Group
	logger  *slog.Logger
	metrics *metrics.Metrics

	// inFlight mirrors the number of renders currently holding a semaphore slot.
	// x/sync/semaphore cannot report its own occupancy (hazard P9), so it is counted
	// here at the acquire/release sites and read lock-free by InFlight. It counts
	// renders, not weight units — that is what an operator reads.
	inFlight atomic.Int64
}

// InFlight reports the number of preview renders currently executing.
func (p *previewer) InFlight() int { return int(p.inFlight.Load()) }

// CacheStats reports the preview cache's entry count and total bytes.
func (p *previewer) CacheStats() (entries int, bytes int64) { return p.cache.stats() }

// New builds a Previewer over cacheDir with the given decode concurrency and LRU
// cap. hdr is the FITS header reader (the pre-allocation validation gate). A nil
// logger falls back to slog.Default().
func New(cacheDir string, concurrency int, maxBytes int64, hdr header.Reader, mx *metrics.Metrics, logger *slog.Logger) Previewer {
	if logger == nil {
		logger = slog.Default()
	}
	if concurrency < 1 {
		concurrency = 1
	}
	heavy := int64(2)
	if int64(concurrency) < heavy {
		heavy = int64(concurrency)
	}
	return &previewer{
		cache:   newCache(cacheDir, maxBytes, logger),
		hdr:     hdr,
		sem:     semaphore.NewWeighted(int64(concurrency)),
		heavy:   heavy,
		logger:  logger,
		metrics: mx,
	}
}

// Warm best-effort renders the thumbnail for a just-filed frame. It dispatches a
// goroutine that try-acquires one slot (skipping when the pool is busy), so it never
// blocks the worker, and never records a decode failure (warming competes with
// transfers over the NAS, so a transient read must not poison the frame).
func (p *previewer) Warm(sha256, destPath string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				p.logger.Warn("preview: warm panic", "sha", sha256, "recover", r)
			}
		}()
		if !p.sem.TryAcquire(1) {
			p.metrics.RecordPreviewWarmDropped()
			return
		}
		p.inFlight.Add(1)
		defer func() { p.inFlight.Add(-1); p.sem.Release(1) }()
		if _, ok := p.cache.cachedPath(sha256, SizeThumb); ok {
			return
		}
		if p.cache.hasFailMarker(sha256) {
			return
		}
		img, err := decodeRenderNamed(p.hdr, destPath, SizeThumb)
		if err != nil {
			p.logger.Debug("preview: warm skipped", "sha", sha256, "err", err)
			return
		}
		data, err := encodeJPEG(img)
		if err != nil {
			return
		}
		if _, err := p.cache.put(sha256, SizeThumb, data); err != nil {
			p.logger.Warn("preview: warm cache write", "sha", sha256, "err", err)
		}
	}()
}

// RenderOnDemand serves a named size from cache, rendering on a miss behind the
// singleflight+semaphore. It returns the on-disk path; the caller opens its own
// handle. A deterministic decode failure records a negative-cache marker.
func (p *previewer) RenderOnDemand(ctx context.Context, sha256, destPath string, size Size) (string, error) {
	if path, ok := p.cache.cachedPath(sha256, size); ok {
		return path, nil
	}
	if p.cache.hasFailMarker(sha256) {
		return "", detErr("preview: known-bad frame %s", sha256)
	}
	v, err, _ := p.group.Do(sha256+"/"+string(size), func() (any, error) {
		if path, ok := p.cache.cachedPath(sha256, size); ok {
			return path, nil
		}
		path, rerr := p.renderNamed(ctx, destPath, size, sha256)
		if rerr != nil && isDeterministic(rerr) {
			p.cache.writeFailMarker(sha256)
		}
		return path, rerr
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// renderNamed acquires a slot, decodes, renders the named size, and writes the cache.
// Panic-isolated with a deferred release so a crafted frame cannot leak a slot.
func (p *previewer) renderNamed(ctx context.Context, destPath string, size Size, sha256 string) (path string, err error) {
	defer func() {
		if r := recover(); r != nil {
			path, err = "", detErr("preview: render panic: %v", r)
		}
	}()
	w := p.weightForNamed(size)
	if aerr := p.sem.Acquire(ctx, w); aerr != nil {
		return "", aerr
	}
	p.inFlight.Add(1)
	defer func() { p.inFlight.Add(-1); p.sem.Release(w) }()
	img, derr := decodeRenderNamed(p.hdr, destPath, size)
	if derr != nil {
		return "", derr
	}
	data, eerr := encodeJPEG(img)
	if eerr != nil {
		return "", eerr
	}
	return p.cache.put(sha256, size, data)
}

// RenderResized renders an arbitrary bounding box to an in-memory JPEG, never
// upscaling and never caching. Bounded by the semaphore; panic-isolated.
func (p *previewer) RenderResized(ctx context.Context, sha256, destPath string, maxW, maxH int) (out []byte, err error) {
	if p.cache.hasFailMarker(sha256) {
		return nil, detErr("preview: known-bad frame %s", sha256)
	}
	w := p.weightForBox(maxW, maxH)
	func() {
		defer func() {
			if r := recover(); r != nil {
				out, err = nil, detErr("preview: render panic: %v", r)
			}
		}()
		if aerr := p.sem.Acquire(ctx, w); aerr != nil {
			err = aerr
			return
		}
		p.inFlight.Add(1)
		defer func() { p.inFlight.Add(-1); p.sem.Release(w) }()
		d, derr := decode(p.hdr, destPath)
		if derr != nil {
			err = derr
			return
		}
		dw, dh := d.boxDims(maxW, maxH)
		out, err = encodeJPEG(renderToImage(d, dw, dh))
	}()
	if err != nil && isDeterministic(err) {
		p.cache.writeFailMarker(sha256)
	}
	return out, err
}

// Dimensions reads the frame's native pixel dimensions from the FITS header (no
// pixel decode), for the lightbox zoom srcSet.
func (p *previewer) Dimensions(destPath string) (int, int, error) {
	h, err := p.hdr.ReadHeader(destPath)
	if err != nil {
		return 0, 0, err
	}
	w, wok := h.Int("NAXIS1")
	ht, hok := h.Int("NAXIS2")
	if !wok || !hok || w <= 0 || ht <= 0 {
		return 0, 0, fmt.Errorf("preview: missing NAXIS1/NAXIS2 in %s", destPath)
	}
	return int(w), int(ht), nil
}

// EvictDigest removes a frame's cached previews (lifecycle eviction).
func (p *previewer) EvictDigest(sha256 string) {
	if sha256 == "" {
		return
	}
	p.cache.evictDigest(sha256)
}

func (p *previewer) weightForNamed(size Size) int64 {
	if size == SizeFull {
		return p.heavy
	}
	return 1
}

// weightForBox charges the heavy weight to an arbitrary render likely to take the
// full-resolution bilinear path (a box larger than the proxy bound on either axis).
func (p *previewer) weightForBox(maxW, maxH int) int64 {
	if maxW > proxyDim || maxH > proxyDim || maxW <= 0 || maxH <= 0 {
		return p.heavy
	}
	return 1
}

// decodeRenderNamed decodes destPath and renders it to an in-memory image at a
// named size.
func decodeRenderNamed(hdr header.Reader, destPath string, size Size) (image.Image, error) {
	d, err := decode(hdr, destPath)
	if err != nil {
		return nil, err
	}
	dw, dh := d.outputDims(size)
	return renderToImage(d, dw, dh), nil
}
