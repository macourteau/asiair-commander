package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/macourteau/asiair-commander/internal/imgpreview"
	"github.com/macourteau/asiair-commander/internal/model"
)

// renderDeadline bounds how long a cache-miss render may wait for a decode slot
// before the handler sheds the request as 503 rather than piling up connections.
const renderDeadline = 8 * time.Second

// handleFrameImage serves an auto-stretched JPEG preview of a downloaded frame:
// a named ?size= (thumb|card|screen|full, cached) or an arbitrary ?w=&h= box
// (uncached, never upscaled). Only frames whose bytes are on disk are eligible.
func (s *Server) handleFrameImage(w http.ResponseWriter, r *http.Request) {
	if s.deps.Previewer == nil {
		writeError(w, http.StatusServiceUnavailable, "previews are disabled")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("frameId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid frame id")
		return
	}
	frame, ok, err := s.deps.Store.GetFrameByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !ok || !eligibleForImage(frame) {
		writeError(w, http.StatusNotFound, "no preview for this frame")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), renderDeadline)
	defer cancel()
	q := r.URL.Query()

	// Explicit named size wins; else an arbitrary box; else the default thumbnail.
	if szName := q.Get("size"); szName != "" {
		size, ok := imgpreview.ParseSize(szName)
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown size")
			return
		}
		s.serveNamedImage(ctx, w, r, frame, size)
		return
	}
	if q.Get("w") != "" || q.Get("h") != "" {
		mw, okw := parseDim(q.Get("w"))
		mh, okh := parseDim(q.Get("h"))
		if !okw || !okh || (mw == 0 && mh == 0) {
			writeError(w, http.StatusBadRequest, "invalid w/h")
			return
		}
		s.serveArbitraryImage(ctx, w, r, frame, mw, mh)
		return
	}
	s.serveNamedImage(ctx, w, r, frame, imgpreview.SizeThumb)
}

// handleFrameImageMeta returns a frame's native pixel dimensions (read from the
// FITS header, no pixel decode) so the lightbox can build a zoom srcSet that loads
// the full-resolution render on zoom-in.
func (s *Server) handleFrameImageMeta(w http.ResponseWriter, r *http.Request) {
	if s.deps.Previewer == nil {
		writeError(w, http.StatusServiceUnavailable, "previews are disabled")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("frameId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid frame id")
		return
	}
	frame, ok, err := s.deps.Store.GetFrameByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !ok || !eligibleForImage(frame) {
		writeError(w, http.StatusNotFound, "no preview for this frame")
		return
	}
	width, height, err := s.deps.Previewer.Dimensions(frame.DestPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "no dimensions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"width": width, "height": height})
}

// serveNamedImage renders (or serves from cache) a named size and streams the
// on-disk file with an immutable, content-versioned cache header.
func (s *Server) serveNamedImage(ctx context.Context, w http.ResponseWriter, r *http.Request, frame *model.Frame, size imgpreview.Size) {
	path, err := s.deps.Previewer.RenderOnDemand(ctx, frame.SHA256, frame.DestPath, size)
	if err != nil {
		s.writeImageError(w, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// The file was evicted in the race window between render and open; retry once.
		if path, err = s.deps.Previewer.RenderOnDemand(ctx, frame.SHA256, frame.DestPath, size); err == nil {
			f, err = os.Open(path)
		}
		if err != nil {
			writeError(w, http.StatusNotFound, "no preview")
			return
		}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusNotFound, "no preview")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("ETag", fmt.Sprintf("%q", frame.SHA256+"-"+string(size)+"-"+imgpreview.ParamsVersion))
	// The browser URL is content-versioned (frontend appends v=<sha256prefix>), so a
	// year-long immutable cache is safe: new content is a new URL.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "preview.jpg", fi.ModTime(), f)
}

// serveArbitraryImage renders an uncached box to memory and streams it. Arbitrary
// callers do not carry the frontend's content stamp, so the cache header omits
// `immutable` and relies on ETag revalidation instead.
func (s *Server) serveArbitraryImage(ctx context.Context, w http.ResponseWriter, r *http.Request, frame *model.Frame, mw, mh int) {
	data, err := s.deps.Previewer.RenderResized(ctx, frame.SHA256, frame.DestPath, mw, mh)
	if err != nil {
		s.writeImageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("ETag", fmt.Sprintf("%q", fmt.Sprintf("%s-%dx%d-%s", frame.SHA256, mw, mh, imgpreview.ParamsVersion)))
	w.Header().Set("Cache-Control", "private")
	http.ServeContent(w, r, "preview.jpg", time.Time{}, bytes.NewReader(data))
}

// writeImageError maps a render error to a status: a slot-wait timeout is a
// transient 503; anything else (a decode failure, a missing file) is a 404.
func (s *Server) writeImageError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "preview renderer busy")
		return
	}
	writeError(w, http.StatusNotFound, "no preview")
}

// eligibleForImage reports whether a frame's bytes are on disk and complete: it has
// a destination path, a content hash, and a state that retains its NAS copy.
func eligibleForImage(f *model.Frame) bool {
	if f.DestPath == "" || f.SHA256 == "" {
		return false
	}
	switch f.State {
	case model.StateVerified, model.StateDeleted, model.StateUnclassified, model.StateHeld:
		return true
	}
	return false
}

// parseDim parses a non-negative dimension query param; an empty value is 0 (unset).
func parseDim(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
