package web

import (
	"context"
	"net/http"

	"github.com/macourteau/asiair-commander/internal/platesolve"
)

// handleDownloadCatalog kicks off (or reports the state of) a Tier-1 star-catalog
// download. It is the ONLY new Tier-1 route: provisioning progress is push-only via
// the status snapshot (model.ProvisionStatus on the SSE stream), so there is no read
// endpoint. As a state-changing mutation it goes through guardWrite (Host allowlist +
// Origin/Referer + session + CSRF), like every other write.
//
// The download itself runs under the app context the provisioner captured at
// startup, not this request's context, so returning 202 immediately does not cancel
// an in-flight download; only service shutdown does.
func (s *Server) handleDownloadCatalog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !platesolve.KnownCatalog(name) {
		writeError(w, http.StatusBadRequest, "unknown star catalog")
		return
	}
	if s.deps.Prov == nil {
		// Plate solving disabled: no provisioner wired. Nil-guarded like Previewer.
		writeError(w, http.StatusServiceUnavailable, "plate solving is disabled")
		return
	}
	if s.deps.Prov.CatalogInstalled(name) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "installed"})
		return
	}
	if busy, inflight := s.deps.Prov.Downloading(); inflight {
		writeJSON(w, http.StatusConflict, map[string]string{"status": "busy", "downloading": busy})
		return
	}
	// Launch in the background: DownloadCatalog runs under the provisioner's app
	// context (it ignores this throwaway one when the app context is set), dedups a
	// racing double-submit internally, and reports progress through the status
	// snapshot. The request returns at once.
	prov := s.deps.Prov
	go func() {
		if err := prov.DownloadCatalog(context.Background(), name); err != nil {
			s.log.Warn("catalog download failed", "catalog", name, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "catalog": name})
}

// handleResolveFailed re-queues the frames that failed to solve (e.g. under a
// too-shallow catalog) for another attempt with the currently-installed catalogs —
// the manual counterpart to the automatic below-floor re-queue a catalog download
// performs. It returns the number re-queued so the UI can report it.
func (s *Server) handleResolveFailed(w http.ResponseWriter, r *http.Request) {
	if s.deps.Prov == nil {
		writeError(w, http.StatusServiceUnavailable, "plate solving is disabled")
		return
	}
	n, err := s.deps.Prov.ResolveFailed(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not re-queue failed frames")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"requeued": n})
}
