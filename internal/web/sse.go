package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
)

// keepaliveInterval reaps dead SSE connections (design architecture §8): a
// comment line every ~20s forces a write that surfaces a broken pipe so the
// handler exits and the hub releases the subscriber slot.
const keepaliveInterval = 20 * time.Second

// handleEvents is the dashboard's SSE stream (design architecture §8). It is
// authorized by the session cookie plus the Host allowlist and an Origin check —
// NOT the CSRF header, since EventSource cannot set one — so a cross-origin
// (DNS-rebinding) EventSource handshake is rejected while the same-origin SPA
// connects. It writes the current snapshot immediately, then snapshots (coalesced
// latest-wins) and discrete events as they arrive, unsubscribing on client
// disconnect or when the hub drops it as lossy.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.checkHost(w, r) {
		return
	}
	// EventSource sends Origin on a cross-origin handshake (and same-origin in
	// modern browsers); reject it when present and disallowed, but tolerate its
	// absence (a same-origin GET may omit it) so the legitimate SPA still connects.
	if host, present := originHost(r); present && !s.allowedHosts(s.currentConfig())[host] {
		s.log.Warn("web: cross-origin SSE rejected", "origin_host", host)
		writeError(w, http.StatusForbidden, "cross-origin event stream rejected")
		return
	}
	if !s.requireSession(w, r) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat proxy buffering if any
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe before announcing, so a server_info re-announced between the two
	// (AnnounceServerInfo) is queued rather than lost.
	_, snaps, events, cancel := s.deps.Hub.Subscribe()
	defer cancel()

	// Announce the server info once, up front. It rides its own event instead of
	// every snapshot frame: the process constants never change within a
	// connection (a server update drops the stream, and the reconnect re-announces
	// them), and the one field that can change is re-announced on its own.
	if err := writeSSE(w, "server_info", s.serverInfo()); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case snap, ok := <-snaps:
			if !ok {
				return // hub dropped us (lossy) → client reconnects and refetches
			}
			if err := writeSSE(w, "snapshot", snap); err != nil {
				return
			}
			flusher.Flush()
		case evt, ok := <-events:
			if !ok {
				return
			}
			if err := writeSSE(w, evt.Name, frameEventData(evt.Data)); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleAppLogStream is a DEDICATED SSE stream for the app log (design
// architecture §18), separate from /events so a burst of log lines can never
// overflow the dashboard stream's bounded event queue. It authorizes like
// /events (session + Host + Origin, no CSRF header — EventSource can't set one),
// sends a filtered snapshot of recent lines, then tails new lines live. The
// LogBuffer drops on a slow client rather than blocking, and a reconnecting
// EventSource gets a fresh snapshot.
func (s *Server) handleAppLogStream(w http.ResponseWriter, r *http.Request) {
	if !s.checkHost(w, r) {
		return
	}
	if host, present := originHost(r); present && !s.allowedHosts(s.currentConfig())[host] {
		s.log.Warn("web: cross-origin log stream rejected", "origin_host", host)
		writeError(w, http.StatusForbidden, "cross-origin log stream rejected")
		return
	}
	if !s.requireSession(w, r) {
		return
	}
	if s.deps.AppLog == nil {
		writeError(w, http.StatusServiceUnavailable, "app log unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	minLevel := parseLevel(r.URL.Query().Get("level"))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.deps.AppLog.Subscribe()
	defer cancel()

	// A fresh (or reconnecting) client gets the recent tail before the live feed.
	if err := writeSSE(w, "snapshot", s.deps.AppLog.Entries(minLevel, 300)); err != nil {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if levelValue(e.Level) < minLevel {
				continue
			}
			if err := writeSSE(w, "log", e); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// serverInfo is the payload announced when an SSE client connects: the
// build-time app version, the process-start time (unix seconds, 0 if unset) and
// the newest published release when it is newer than the running build ("" for
// none), which the SPA shows in its chrome. It is web-layer metadata,
// deliberately kept out of model.Snapshot so build info never leaks into the
// domain status model.
type serverInfo struct {
	Version       string `json:"version"`
	StartedAt     int64  `json:"started_at"`
	LatestVersion string `json:"latest_version"`
}

func (s *Server) serverInfo() serverInfo {
	info := serverInfo{Version: s.deps.Version}
	if !s.deps.StartedAt.IsZero() {
		info.StartedAt = s.deps.StartedAt.Unix()
	}
	if s.deps.LatestVersion != nil {
		info.LatestVersion = s.deps.LatestVersion()
	}
	return info
}

// AnnounceServerInfo pushes a fresh server_info to every connected client. The
// update checker calls it when the newest release changes, and a config save
// calls it when the update check is switched (design features §7).
func (s *Server) AnnounceServerInfo() {
	if s.deps.Hub == nil {
		return
	}
	s.deps.Hub.Emit(hub.Event{Name: "server_info", Data: s.serverInfo()})
}

// frameEventData converts a frame-carrying event payload (frame.done /
// frame.failed / frame.deleted, whose Data is a model.Frame) into the one shared
// frameDTO wire shape the REST endpoints emit, so the live feed decodes every
// frame source identically. Any other payload (e.g. frame.started's plain map, or
// a test's non-frame Data) rides through untouched — a checked assertion, never a
// bare one, so a mismatched payload cannot panic the SSE goroutine.
func frameEventData(data any) any {
	if f, ok := data.(model.Frame); ok {
		return toFrameDTO(f)
	}
	return data
}

// writeSSE emits one `event: <name>\ndata: <json>\n\n` message.
func writeSSE(w io.Writer, name string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
	return err
}
