package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/macourteau/asiair-commander/internal/model"
)

// refilePreviewBody selects how much of the library the diff covers: "mosaic" (the
// frames the current config would route through the mosaic template) or "all". The
// value is passed through verbatim — the supervisor owns the vocabulary and
// normalises anything else to the default, so the wire values are not spelled out
// a second time here and cannot drift.
type refilePreviewBody struct {
	Scope string `json:"scope"`
}

// refileApplyBody is one selected group: the destination the operator saw in the
// diff, and every row id it covers. ToDir is not decoration — apply re-routes each
// frame and refuses any whose re-derived directory is not this one, so a frame is
// never moved somewhere the operator never saw (INV-M10). The field names mirror
// model.RefileGroup, which is where the SPA gets them.
type refileApplyBody struct {
	ToDir    string  `json:"to_dir"`
	FrameIDs []int64 `json:"frame_ids"`
}

// handleRefilePreview returns the projected migration for one rig: every eligible
// frame whose routed directory differs from its stored one, grouped old dir → new
// dir, with the directories the prune expects to be unable to remove. The work is a
// database scan plus one ReadDir per vacated directory and happens on this
// goroutine, never the worker's, so it is a plain request/response.
func (s *Server) handleRefilePreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body refilePreviewBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid refile preview body")
		return
	}
	pv, err := s.deps.Supervisor.PreviewRefile(r.Context(), id, body.Scope)
	switch {
	case err == nil:
		// Serialize an empty diff as [] rather than null: the SPA maps over both.
		if pv.Groups == nil {
			pv.Groups = []model.RefileGroup{}
		}
		if pv.Refusals == nil {
			pv.Refusals = []model.Refusal{}
		}
		writeJSON(w, http.StatusOK, pv)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	default:
		s.log.Error("web: refile preview failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not preview the re-file")
	}
}

// handleRefileApply moves one selected group and streams NDJSON: one RefileResult
// per FILE, then a final RefileSummary carrying the prune outcomes — directories,
// not frames, so they cannot be results. The group runs to completion on the worker
// without yielding, so the lines arrive as one burst; progress in the UI is
// per-group, which is why the SPA applies groups sequentially.
//
// r.Context() is handed to the supervisor, so an operator who cancels the request
// stops the migration between frames. There is no resume state: re-running the
// preview shows whatever remains and a second apply finishes it.
func (s *Server) handleRefileApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body refileApplyBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid refile apply body")
		return
	}
	if body.ToDir == "" {
		// Without it every frame would be refused for routing somewhere other than
		// "", which is a confusing way to spell a malformed request.
		writeError(w, http.StatusBadRequest, "a destination directory is required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Lazily begun rather than deferred, so a rig-level 404 discovered before the
	// first line is still an ordinary error response.
	started := false
	begin := func() {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		started = true
	}
	writeLine := func(v any) {
		if !started {
			begin()
		}
		line, _ := json.Marshal(v)
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
	}

	results, summary, err := s.deps.Supervisor.RefileGroup(r.Context(), id, body.ToDir, body.FrameIDs)
	if err != nil {
		if errors.Is(err, s.deps.RigNotFound) && !started {
			writeError(w, http.StatusNotFound, "no such rig")
			return
		}
		// Anything else is reported in-band: the group either never ran or was
		// abandoned mid-way, and both are outcomes the operator has to see.
		s.log.Warn("web: refile group failed", "rig", id, "to", body.ToDir, "err", err)
		writeLine(model.RefileResult{Status: "failed", Reason: err.Error()})
	}
	for _, res := range results {
		writeLine(res)
	}
	// Always last, and always present — even for an empty group, so the client's
	// stream reader terminates on the summary rather than on end-of-body.
	writeLine(summary)
}
