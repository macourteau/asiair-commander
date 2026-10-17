package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
)

// maxTargetLen caps a retarget's new OBJECT value. It is the FITS card value budget
// (design routing-and-filing §9.1); a longer target is rejected at the boundary
// rather than truncated, which would silently retarget to a name the operator did
// not type.
const maxTargetLen = 60

// retargetBody is the shared request shape for both retarget endpoints: the frame
// ids to act on and the new target label. It matches web/src/api/retarget.ts, which
// posts {ids, target} to both /retarget/preview and /retarget/apply.
type retargetBody struct {
	IDs    []int64 `json:"ids"`
	Target string  `json:"target"`
}

// decodeRetargetBody decodes and validates the shared {ids, target} body, writing
// the 400 itself and returning ok=false on any problem. target must be non-empty
// after trimming and within the card value budget; it is returned trimmed so the
// supervisor sanitizes a single canonical value.
func decodeRetargetBody(w http.ResponseWriter, r *http.Request) (retargetBody, bool) {
	var body retargetBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid retarget body")
		return retargetBody{}, false
	}
	body.Target = strings.TrimSpace(body.Target)
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "a target is required")
		return retargetBody{}, false
	}
	if len(body.Target) > maxTargetLen {
		writeError(w, http.StatusBadRequest, "target is too long")
		return retargetBody{}, false
	}
	return body, true
}

// handleRetargetPreview returns the read-only projection of where each frame would
// move under the new target. No NAS/device I/O happens server-side (design
// routing-and-filing §9.6), so this is a plain request/response, not a stream.
func (s *Server) handleRetargetPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, ok := decodeRetargetBody(w, r)
	if !ok {
		return
	}
	previews, err := s.deps.Supervisor.PreviewRetarget(id, body.IDs, body.Target)
	switch {
	case err == nil:
		if previews == nil {
			previews = []model.RetargetPreview{}
		}
		writeJSON(w, http.StatusOK, previews)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	default:
		s.log.Error("web: retarget preview failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not preview retarget")
	}
}

// handleRetargetApply retargets each frame in turn and streams one RetargetResult
// per line as NDJSON, flushing after each so the Tools page shows live progress
// rather than a minutes-long opaque wait (design routing-and-filing §9.6). Each
// frame is independent: a per-frame error becomes a {status:"failed"} line, never
// an aborted stream. A rig-level ErrRigNotFound discovered before the first line is
// written maps to 404, since no stream has begun; once streaming has committed to a
// 200 it can only be reported as a failed line.
func (s *Server) handleRetargetApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, ok := decodeRetargetBody(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	started := false
	begin := func() {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		started = true
	}
	for _, frameID := range body.IDs {
		res, err := s.deps.Supervisor.RetargetFrame(id, frameID, body.Target)
		if err != nil {
			if errors.Is(err, s.deps.RigNotFound) && !started {
				writeError(w, http.StatusNotFound, "no such rig")
				return
			}
			s.log.Warn("web: retarget frame failed", "rig", id, "frame", frameID, "err", err)
			res = model.RetargetResult{FrameID: frameID, Status: "failed", Reason: err.Error()}
		}
		if !started {
			begin()
		}
		line, _ := json.Marshal(res)
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		flusher.Flush()
	}
	// An empty id set streams nothing; still answer with a valid empty 200 NDJSON so
	// the client's stream reader completes cleanly rather than seeing a bare 200.
	if !started {
		begin()
	}
}
