package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/macourteau/asiair-commander/internal/store"
)

// logPullDTO is the JSON shape for a device-log pull (store.LogPull has no JSON
// tags).
type logPullDTO struct {
	RigID      string `json:"rig_id"`
	LogType    string `json:"log_type"`
	SourcePath string `json:"source_path"`
	DestPath   string `json:"dest_path,omitempty"`
	Size       int64  `json:"size"`
	PulledAt   int64  `json:"pulled_at"`
}

// handleRigLogs lists the device logs pulled for a rig (design architecture
// §11), newest first.
func (s *Server) handleRigLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pulls, err := s.deps.Store.ListLogPulls(r.Context(), id)
	if err != nil {
		s.log.Error("web: list log pulls failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not list device logs")
		return
	}
	out := make([]logPullDTO, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, toLogPullDTO(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func toLogPullDTO(p store.LogPull) logPullDTO {
	return logPullDTO{
		RigID: p.RigID, LogType: p.LogType, SourcePath: p.SourcePath,
		DestPath: p.DestPath, Size: p.Size, PulledAt: p.PulledAt,
	}
}

// handleAppLogs returns the app's own recent log lines from the in-memory ring
// (design architecture §18), filterable by minimum level and count. This is
// what lets a postmortem start in the UI instead of RDP-ing into the NAS.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps.AppLog == nil {
		writeJSON(w, http.StatusOK, []LogEntry{})
		return
	}
	level := parseLevel(r.URL.Query().Get("level"))
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)
	entries := s.deps.AppLog.Entries(level, limit)
	if entries == nil {
		entries = []LogEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleAppLogDownload serves the current (unrotated) app-log file as a download,
// so an operator can pull the full file — not just the ring the UI shows — without
// RDP-ing into the NAS. The whole file is read into memory first (os.ReadFile) so a
// concurrent append or rotation can't hand back a torn response; lumberjack only
// appends to and renames this path, so a single read is a consistent snapshot.
func (s *Server) handleAppLogDownload(w http.ResponseWriter, r *http.Request) {
	if s.deps.AppLogPath == "" {
		writeError(w, http.StatusNotFound, "no log file is available")
		return
	}
	data, err := os.ReadFile(s.deps.AppLogPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "the log file does not exist yet")
			return
		}
		s.log.Error("web: read app log for download failed", "path", s.deps.AppLogPath, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read the log file")
		return
	}
	name := filepath.Base(s.deps.AppLogPath)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.log.Warn("web: writing app-log download failed", "err", err)
	}
}

// parseLevel maps a query string to an slog.Level, defaulting to Debug (return
// everything) for an absent or unrecognized value.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelDebug
	}
}
