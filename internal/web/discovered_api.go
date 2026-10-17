package web

import (
	"context"
	"net/http"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// scanTimeout bounds a synchronous POST /api/scan so a slow subnet can't hang the
// request goroutine.
const scanTimeout = 30 * time.Second

// handleScan triggers a discovery scan and returns the registry as the round
// left it (design architecture §8). It runs synchronously under a bounded
// context so the UI gets the fresh list in the response; the background loop
// keeps Discovered() warm between calls. The list is not this round's replies:
// the registry retains a device that has stopped answering (a managed one
// indefinitely), so a caller counting devices that are actually on the network
// must count the online ones.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()
	found := s.deps.Scanner.ScanOnDemand(ctx)
	if found == nil {
		found = []model.DiscoveredRig{}
	}
	writeJSON(w, http.StatusOK, found)
}

// handleDiscovered returns the last scan's result without re-probing (I/O-free
// registry read, so it needs no timeout).
func (s *Server) handleDiscovered(w http.ResponseWriter, r *http.Request) {
	found := s.deps.Scanner.Discovered()
	if found == nil {
		found = []model.DiscoveredRig{}
	}
	writeJSON(w, http.StatusOK, found)
}

// handleDeviceShares enumerates one discovered device's SMB shares for the adopt
// dialog (design architecture §9). It is a thin wrapper over
// Scanner.DeviceShares, which owns the bounded guest SMB session; whether a
// device is already managed comes from the discovered list, so the UI filters
// those out before adoption.
func (s *Server) handleDeviceShares(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	if guid == "" {
		writeError(w, http.StatusBadRequest, "a device guid is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()
	shares, err := s.deps.Scanner.DeviceShares(ctx, guid)
	if err != nil {
		s.log.Warn("web: device-shares listing failed", "guid", guid, "err", err)
		writeError(w, http.StatusBadGateway, "couldn't reach that device to list its shares")
		return
	}
	if shares == nil {
		shares = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": shares})
}
