package web

import (
	"net/http"
	"path/filepath"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/svcacct"
)

// rootsCheckRequest carries the candidate destination roots the wizard collected.
// Any empty field is skipped (cal/logs are optional).
type rootsCheckRequest struct {
	Roots model.Roots `json:"roots"`
}

// rootResult reports one root's writability probe: ok=true means the service
// account can create files there; error is a human message when it cannot.
type rootResult struct {
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// rootsCheckResponse is the endpoint's reply: the account the service runs as
// (so the UI can name it in the fix-it guidance) and one result per checked root.
type rootsCheckResponse struct {
	Account string       `json:"account"`
	Results []rootResult `json:"results"`
}

// handleRootsCheck tests whether the SERVICE ACCOUNT can actually write each
// candidate root, IN-PROCESS (so the probe exercises the real process identity and
// its NTFS permissions, not the admin browser's). For every non-empty, not-yet-seen
// root it runs MkdirAll then a write-probe; the per-root outcome plus the service
// account name go back to the wizard, which blocks setup until every root passes
// and guides the operator to grant access where it does not.
//
// It is guarded like any mutation (session + CSRF + Host/Origin) and creates
// directories, so it is a POST. A nil LocalFS (never wired in production) yields a
// 503 rather than a panic.
func (s *Server) handleRootsCheck(w http.ResponseWriter, r *http.Request) {
	if s.deps.LocalFS == nil {
		writeError(w, http.StatusServiceUnavailable, "filesystem access is unavailable")
		return
	}

	var req rootsCheckRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid roots-check body")
		return
	}

	resp := rootsCheckResponse{Account: svcacct.ServiceAccountName()}
	seen := make(map[string]bool)
	for _, path := range []string{req.Roots.Light, req.Roots.Cal, req.Roots.Staging, req.Roots.Logs} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		resp.Results = append(resp.Results, s.probeRoot(path))
	}

	writeJSON(w, http.StatusOK, resp)
}

// probeRoot creates path (and parents) then write-probes it, mapping any failure
// to a per-root result. A failed probe is logged at boundary with the path and
// error so an operator debugging a permission problem has a trail.
func (s *Server) probeRoot(path string) rootResult {
	// A relative path resolves against the service's working directory
	// (C:\Windows\System32 for a Windows service), so probing or creating it there
	// would silently succeed or give misleading guidance. Require an absolute path.
	if !filepath.IsAbs(path) {
		return rootResult{Path: path, OK: false, Error: `use an absolute path, e.g. D:\Astro\Lights`}
	}
	// The response messages stay clean (the raw OS error can carry a \\?\ extended
	// path and other noise); the actionable detail — the account and this path — is
	// in the result and the account field. The raw error is logged for debugging.
	if err := s.deps.LocalFS.MkdirAll(path); err != nil {
		s.log.Warn("web: roots check could not create destination", "path", path, "err", err)
		return rootResult{Path: path, OK: false, Error: "could not create the folder"}
	}
	if err := s.deps.LocalFS.WriteProbe(path); err != nil {
		s.log.Warn("web: roots check write-probe failed", "path", path, "err", err)
		return rootResult{Path: path, OK: false, Error: "the folder is not writable by the service account"}
	}
	return rootResult{Path: path, OK: true}
}
