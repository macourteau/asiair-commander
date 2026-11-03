package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// maxTargetLen caps an OBJECT edit's value in bytes. It is the FITS card value
// budget (design routing-and-filing §9.1); a longer target is rejected at the
// boundary rather than truncated, which would silently retarget to a name the
// operator did not type. The writer's own 60-rune ceiling is the looser of the two
// for multibyte input, so the two never disagree the other way (§9.8).
const maxTargetLen = 60

// editBody is the shared request shape for both edit endpoints: the frame ids to
// act on, the header keyword and its new value (design header-edits §9.1).
type editBody struct {
	IDs   []int64 `json:"ids"`
	Key   string  `json:"key"`
	Value string  `json:"value"`
}

// decodeEditBody decodes and validates the shared {ids, key, value} body, writing
// the 400 itself and returning ok=false on any problem: a key off the allowlist or
// a value outside its key's rule is refused before any frame is touched. The rules
// are header.EditableKeys', the same ones the writer applies, so a value accepted
// here is one the writer accepts. The key is returned normalized and the value
// trimmed; the worker applies the key's rule itself.
func decodeEditBody(w http.ResponseWriter, r *http.Request) (editBody, bool) {
	var body editBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid edit body")
		return editBody{}, false
	}
	body.Key = model.NormalizeKey(body.Key)
	rule, ok := header.EditableKeys[body.Key]
	if !ok {
		writeError(w, http.StatusBadRequest, "not an editable header keyword")
		return editBody{}, false
	}
	body.Value = strings.TrimSpace(body.Value)
	if body.Value == "" {
		writeError(w, http.StatusBadRequest, "a value is required")
		return editBody{}, false
	}
	if body.Key == "OBJECT" && len(body.Value) > maxTargetLen {
		writeError(w, http.StatusBadRequest, "target is too long")
		return editBody{}, false
	}
	if _, err := rule.Normalize(body.Value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return editBody{}, false
	}
	return body, true
}

// handleEditPreview returns the read-only projection of what the edit would do to
// each frame. No NAS/device I/O happens server-side (design header-edits §9.1), so
// this is a plain request/response, not a stream.
func (s *Server) handleEditPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, ok := decodeEditBody(w, r)
	if !ok {
		return
	}
	previews, err := s.deps.Supervisor.PreviewEdit(id, body.IDs, body.Key, body.Value)
	switch {
	case err == nil:
		if previews == nil {
			previews = []model.EditPreview{}
		}
		writeJSON(w, http.StatusOK, previews)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	default:
		s.log.Error("web: edit preview failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not preview edit")
	}
}

// handleEditApply edits each frame in turn and streams one EditResult per line as
// NDJSON, flushing after each so the Tools page shows live progress rather than a
// minutes-long opaque wait (design header-edits §9.1). Each frame is independent: a
// per-frame error becomes a {status:"failed"} line, never an aborted stream. A
// rig-level ErrRigNotFound discovered before the first line is written maps to 404,
// since no stream has begun; once streaming has committed to a 200 it can only be
// reported as a failed line.
func (s *Server) handleEditApply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, ok := decodeEditBody(w, r)
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
		res, err := s.deps.Supervisor.EditFrame(id, frameID, body.Key, body.Value)
		if err != nil {
			if errors.Is(err, s.deps.RigNotFound) && !started {
				writeError(w, http.StatusNotFound, "no such rig")
				return
			}
			s.log.Warn("web: edit frame failed", "rig", id, "frame", frameID, "key", body.Key, "err", err)
			res = model.EditResult{FrameID: frameID, Status: "failed", Reason: err.Error()}
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
