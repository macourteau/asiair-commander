package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
)

// handleListAlerts returns the problem feed (design architecture §10). Acked
// alerts are excluded unless include_acked=true; limit caps the rows (default
// 200).
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	includeAcked := r.URL.Query().Get("include_acked") == "true"
	limit := atoiDefault(r.URL.Query().Get("limit"), 200)

	alerts, err := s.deps.Store.ListAlerts(r.Context(), includeAcked, limit)
	if err != nil {
		s.log.Error("web: list alerts failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list alerts")
		return
	}
	if alerts == nil {
		alerts = []model.Alert{}
	}
	writeJSON(w, http.StatusOK, alerts)
}

// handleAlertCommand acknowledges an alert. The URL carries "{id}:ack" in one
// path segment (design architecture §8). After acking it refreshes the hub's
// authoritative unacked count so the dashboard badge (which rides the snapshot)
// self-corrects.
func (s *Server) handleAlertCommand(w http.ResponseWriter, r *http.Request) {
	idStr, action, ok := strings.Cut(r.PathValue("idAction"), ":")
	if !ok || action != "ack" {
		writeError(w, http.StatusBadRequest, "expected /api/alerts/{id}:ack")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "alert id must be an integer")
		return
	}

	if err := s.deps.Store.AckAlert(r.Context(), id, s.now().Unix()); err != nil {
		s.log.Error("web: ack alert failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not acknowledge alert")
		return
	}
	if n, err := s.deps.Store.UnackedCount(r.Context()); err == nil {
		s.deps.Hub.SetUnackedAlerts(n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "acked", "id": id})
}
