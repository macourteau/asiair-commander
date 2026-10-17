package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// rawRigID reads just the "id" from a rig body without a strict full decode, so a
// create whose body failed to decode (e.g. a unit-less duration) can still name
// the rig in the field path it returns.
func rawRigID(raw []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.ID
}

// redactedRigs returns the config's rigs with each SMB password redacted.
func redactedRigs(cfg *model.Config) []model.RigProfile {
	rigs := config.Redact(cfg).Rigs
	if rigs == nil {
		return []model.RigProfile{}
	}
	return rigs
}

// handleListRigs returns every configured rig, passwords redacted.
func (s *Server) handleListRigs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, redactedRigs(s.currentConfig()))
}

// handleGetRig returns one rig by id, password redacted, 404 if absent.
func (s *Server) handleGetRig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, rig := range redactedRigs(s.currentConfig()) {
		if rig.ID == id {
			writeJSON(w, http.StatusOK, rig)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such rig")
}

// handleCreateRig appends a rig and runs the full config write pipeline. The
// body's SMB password is stored as sent (a new rig has no prior secret to
// preserve).
func (s *Server) handleCreateRig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var rig model.RigProfile
	if err := decodeJSON(bytes.NewReader(body), &rig); err != nil {
		// The id is the field path's prefix, so read it straight from the raw body
		// (a partial decode may not have reached it).
		if problems := config.LocalizeRigDecodeError(body, rawRigID(body)); len(problems) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid rig", "problems": problems,
			})
			return
		}
		writeError(w, http.StatusBadRequest, "invalid rig body")
		return
	}
	if strings.TrimSpace(rig.ID) == "" {
		writeError(w, http.StatusBadRequest, "rig id is required")
		return
	}

	stored := s.currentConfig()
	if stored.Rig(rig.ID) != nil {
		writeError(w, http.StatusConflict, "a rig with that id already exists")
		return
	}

	candidate, err := deepCopyConfig(stored)
	if err != nil {
		s.log.Error("web: clone config failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not process request")
		return
	}
	candidate.Rigs = append(candidate.Rigs, rig)

	merged, ok := s.saveConfig(w, candidate)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, config.Redact(merged).Rig(rig.ID))
}

// handleUpdateRig replaces a rig by id. A sentinel password in the body is
// preserved from the stored rig (ApplyRedactedWrite matches by id inside the
// pipeline).
func (s *Server) handleUpdateRig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var rig model.RigProfile
	if err := decodeJSON(bytes.NewReader(body), &rig); err != nil {
		// The path is authoritative for the id, so it prefixes the field path.
		if problems := config.LocalizeRigDecodeError(body, id); len(problems) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid rig", "problems": problems,
			})
			return
		}
		writeError(w, http.StatusBadRequest, "invalid rig body")
		return
	}
	rig.ID = id // the path is authoritative

	candidate, err := deepCopyConfig(s.currentConfig())
	if err != nil {
		s.log.Error("web: clone config failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not process request")
		return
	}
	idx := -1
	for i := range candidate.Rigs {
		if candidate.Rigs[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "no such rig")
		return
	}
	candidate.Rigs[idx] = rig

	merged, ok := s.saveConfig(w, candidate)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, config.Redact(merged).Rig(id))
}

// handleDeleteRig removes a rig by id.
func (s *Server) handleDeleteRig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	candidate, err := deepCopyConfig(s.currentConfig())
	if err != nil {
		s.log.Error("web: clone config failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not process request")
		return
	}
	kept := candidate.Rigs[:0]
	found := false
	// deleted holds the removed profile so its per-share media-id keys can be
	// cleared below. It must not be named `rig`: that would shadow the rig package
	// used for the key helpers.
	var deleted model.RigProfile
	for _, r := range candidate.Rigs {
		if r.ID == id {
			found = true
			deleted = r
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such rig")
		return
	}
	candidate.Rigs = kept

	if _, ok := s.saveConfig(w, candidate); !ok {
		return
	}
	// Clear the deleted rig's per-rig store meta so no stale last-known-IP or
	// pending-guid key leaks into a later reuse of the id.
	if s.deps.Store != nil {
		ctx := r.Context()
		if err := s.deps.Store.SetMeta(ctx, rig.LastIPKey(id), ""); err != nil {
			s.log.Warn("web: could not clear last-known IP for deleted rig", "rig", id, "err", err)
		}
		if err := s.deps.Store.SetMeta(ctx, rig.PendingGUIDKey(id), ""); err != nil {
			s.log.Warn("web: could not clear pending guid for deleted rig", "rig", id, "err", err)
		}
		// One media-id key per configured source share. Left behind, it makes a
		// genuinely new stick verify Foreign if the rig id is ever reused.
		for _, src := range deleted.Sources {
			if err := s.deps.Store.SetMeta(ctx, rig.MediaIDKey(id, src.Share), ""); err != nil {
				s.log.Warn("web: could not clear media id for deleted rig", "rig", id, "share", src.Share, "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRigCommand dispatches a per-rig command. The URL carries the command as
// "{id}:{action}" in one path segment (design architecture §8): rig-01:pause /
// :resume / :run-now / :stop-run / :reclassify. It returns 202 immediately (the
// commands are non-blocking) and 404 for an unknown rig.
func (s *Server) handleRigCommand(w http.ResponseWriter, r *http.Request) {
	id, action, ok := strings.Cut(r.PathValue("idAction"), ":")
	if !ok || id == "" || action == "" {
		writeError(w, http.StatusBadRequest, "expected /api/rigs/{id}:{action}")
		return
	}

	var err error
	switch action {
	case "pause":
		err = s.deps.Supervisor.Pause(id)
	case "resume":
		err = s.deps.Supervisor.Resume(id)
	case "run-now":
		err = s.deps.Supervisor.RunNow(id)
	case "stop-run":
		err = s.deps.Supervisor.StopRun(id)
	case "reclassify":
		err = s.deps.Supervisor.Reclassify(id)
	case "reset-identity":
		err = s.deps.Supervisor.ResetIdentity(id)
	default:
		writeError(w, http.StatusBadRequest, "unknown command: "+action)
		return
	}

	if err != nil {
		if errors.Is(err, s.deps.RigNotFound) {
			writeError(w, http.StatusNotFound, "no such rig")
			return
		}
		s.log.Error("web: rig command failed", "rig", id, "action", action, "err", err)
		writeError(w, http.StatusInternalServerError, "command failed")
		return
	}
	s.log.Info("web: rig command", "rig", id, "action", action)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "rig": id, "action": action})
}

// handleDiscardFrame dismisses one permanently-stuck unclassified frame: the
// worker purges its NAS quarantine copy and tombstones the row so it stops driving
// the backlog alert (design frame-lifecycle §13). The typed rig errors map to
// HTTP status codes via errors.Is; a purge failure is not distinguished from other
// internal failures (the operator action is simply "retry").
func (s *Server) handleDiscardFrame(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	frameID, err := strconv.ParseInt(r.PathValue("frameId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "frame id must be an integer")
		return
	}

	err = s.deps.Supervisor.DiscardFrame(id, frameID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, rig.ErrNotDismissable):
		writeError(w, http.StatusConflict, "only unclassified frames can be dismissed")
	case errors.Is(err, rig.ErrFrameNotFound) || errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such frame")
	default:
		s.log.Error("web: dismiss frame failed", "rig", id, "frame", frameID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not dismiss frame")
	}
}

// handlePauseAll pauses every managed rig (the global switch).
func (s *Server) handlePauseAll(w http.ResponseWriter, r *http.Request) {
	s.deps.Supervisor.PauseAll()
	s.log.Info("web: paused all rigs")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "action": "pause-all"})
}

// handleResumeAll resumes every managed rig.
func (s *Server) handleResumeAll(w http.ResponseWriter, r *http.Request) {
	s.deps.Supervisor.ResumeAll()
	s.log.Info("web: resumed all rigs")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "action": "resume-all"})
}
