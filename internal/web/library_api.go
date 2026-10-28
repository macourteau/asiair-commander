package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// Machine-readable acknowledge refusal codes (design library-reconciliation
// §10.7). The SPA branches on these rather than server prose, which fails silently
// across a version skew. Note ACK_QUEUED rides a 202 success body, not an error
// body — a timed-out-but-applied acknowledgement must not be reported as failure.
const (
	codeAckConfirmationInvalid = "ACK_CONFIRMATION_INVALID"
	codeAckNotMissing          = "ACK_NOT_MISSING"
	codeAckNotVerified         = "ACK_NOT_VERIFIED"
	codeAckRestorePending      = "ACK_RESTORE_PENDING"
	codeAckKeepOriginals       = "ACK_KEEP_ORIGINALS"
	codeAckNothingToDo         = "ACK_NOTHING_TO_DO"
	codeAckQueued              = "ACK_QUEUED"
)

// rootLinkStatus is one configured destination root's library-link state for the
// Settings library view.
type rootLinkStatus struct {
	Root   string `json:"root"`
	Linked bool   `json:"linked"`
	Reason string `json:"reason,omitempty"`
}

// handleLibraryStatus reports each configured destination root's link state,
// computed fresh from the store and local filesystem (not the periodic scan
// snapshot) so a just-linked or just-broken root shows immediately. It drives the
// Settings library section, where the operator links roots and re-links one whose
// marker was removed.
func (s *Server) handleLibraryStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	roots := []rootLinkStatus{}
	if s.deps.LocalFS != nil {
		seen := map[string]bool{}
		for _, root := range []string{cfg.Roots.Light, cfg.Roots.Cal} {
			if strings.TrimSpace(root) == "" {
				continue
			}
			// Light and Cal may resolve to the same root; report it once so the UI
			// never renders duplicate rows.
			if k := rig.RootKey(root); seen[k] {
				continue
			} else {
				seen[k] = true
			}
			linked, reason := rig.RootLinkState(r.Context(), s.deps.Store, s.deps.LocalFS, root)
			roots = append(roots, rootLinkStatus{Root: root, Linked: linked, Reason: reason})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
}

// handleLinkLibrary links a destination root to this installation's library.
func (s *Server) handleLinkLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Root == "" {
		writeError(w, http.StatusBadRequest, "a destination root is required")
		return
	}
	err := s.deps.Supervisor.LinkLibrary(id, body.Root)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	default:
		s.log.Warn("web: link library failed", "rig", id, "root", body.Root, "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// handleRescanLibrary queues a library scan on a rig's next cycle.
func (s *Server) handleRescanLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.deps.Supervisor.RescanLibrary(id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	default:
		s.log.Error("web: rescan library failed", "rig", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not queue rescan")
	}
}

// handleRestoreFrame flags a missing frame for re-pull from the device.
func (s *Server) handleRestoreFrame(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	frameID, err := strconv.ParseInt(r.PathValue("frameId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "frame id must be an integer")
		return
	}
	err = s.deps.Supervisor.RestoreFrame(id, frameID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	case errors.Is(err, s.deps.FrameNotMissing):
		// A non-missing frame is a benign 409, not a server error.
		writeError(w, http.StatusConflict, "frame is not marked missing")
	default:
		s.log.Error("web: restore frame failed", "rig", id, "frame", frameID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not request restore")
	}
}

// handleAcknowledgeFrame runs the two-step confirmation for acknowledging one
// culled frame (design library-reconciliation §10.7). An empty or tokenless body
// is the challenge, which mints a per-target token and returns the server's
// description of what would be deleted; a body carrying that token is the
// confirm, which arms the frame's device original for deletion on the next pass.
func (s *Server) handleAcknowledgeFrame(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Token string `json:"token"`
	}
	// decodeJSON already caps + DisallowUnknownFields; an empty body is io.EOF, which
	// is a challenge ({} and a bare token are too — branch on Token, not on io.EOF).
	if err := decodeJSON(r.Body, &body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid acknowledge body")
		return
	}

	// Steps 1–2 run for BOTH challenge and confirm: the rig must still be configured
	// and not keep_originals. A frame row outlives its rig's config removal, so
	// Rig(id) can be nil for a real frame id — reading .KeepOriginals off it directly
	// would panic. Re-checking on confirm closes the mint→confirm window a
	// keep_originals toggle would otherwise open (design library-reconciliation
	// §10.7).
	profile := s.currentConfig().Rig(id)
	if profile == nil {
		writeError(w, http.StatusNotFound, "no such rig")
		return
	}
	if profile.KeepOriginals {
		writeErrorCode(w, http.StatusConflict, codeAckKeepOriginals, "this rig keeps originals; nothing would be deleted")
		return
	}

	frameID, err := strconv.ParseInt(r.PathValue("frameId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "frame id must be an integer")
		return
	}

	if body.Token == "" {
		s.acknowledgeFrameChallenge(w, r, id, frameID)
		return
	}

	if _, ok := s.lookupAckToken(ackFrameKey(id, frameID), body.Token); !ok {
		writeErrorCode(w, http.StatusForbidden, codeAckConfirmationInvalid, "confirmation token missing, wrong, or expired")
		return
	}
	err = s.deps.Supervisor.AcknowledgeMissing(id, frameID)
	if s.writeAckError(w, id, err) {
		return
	}
	s.consumeAckToken(ackFrameKey(id, frameID), body.Token)
	w.WriteHeader(http.StatusNoContent)
}

// acknowledgeFrameChallenge reads the frame, refuses with a reason if it cannot be
// acknowledged, else mints a token and returns the server's description.
func (s *Server) acknowledgeFrameChallenge(w http.ResponseWriter, r *http.Request, id string, frameID int64) {
	f, ok, err := s.deps.Store.GetFrameByID(r.Context(), frameID)
	if err != nil {
		s.log.Error("web: acknowledge frame lookup failed", "rig", id, "frame", frameID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read frame")
		return
	}
	if !ok || f.RigID != id {
		writeError(w, http.StatusNotFound, "no such frame")
		return
	}
	if code, msg, already, refuse := ackMintRefusal(*f); refuse {
		if already {
			writeJSON(w, http.StatusOK, map[string]any{"already": true})
			return
		}
		writeErrorCode(w, http.StatusConflict, code, msg)
		return
	}
	token, expires, err := s.mintAckToken(ackFrameKey(id, frameID), nil)
	if err != nil {
		s.log.Error("web: mint ack token failed", "rig", id, "frame", frameID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not issue confirmation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires_at": expires.Unix(),
		"dest_path": f.DestPath, "source_path": f.SourcePath, "state": string(f.State),
	})
}

// handleAcknowledgeGroup runs the two-step confirmation for acknowledging every
// acknowledgeable culled frame under one directory (design library-reconciliation
// §10.7). The challenge binds the candidate frame ids to the token, so the confirm
// can only acknowledge the set the operator was shown — a directory that GREW
// between dialog and confirm cannot widen the acknowledgement.
func (s *Server) handleAcknowledgeGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Dir   string `json:"dir"`
		Token string `json:"token"`
	}
	if err := decodeJSON(r.Body, &body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid acknowledge body")
		return
	}

	profile := s.currentConfig().Rig(id)
	if profile == nil {
		writeError(w, http.StatusNotFound, "no such rig")
		return
	}
	if profile.KeepOriginals {
		writeErrorCode(w, http.StatusConflict, codeAckKeepOriginals, "this rig keeps originals; nothing would be deleted")
		return
	}
	if body.Dir == "" {
		writeError(w, http.StatusBadRequest, "a directory is required")
		return
	}

	if body.Token == "" {
		s.acknowledgeGroupChallenge(w, r, id, body.Dir)
		return
	}

	entry, ok := s.lookupAckToken(ackGroupKey(id, body.Dir), body.Token)
	if !ok {
		writeErrorCode(w, http.StatusForbidden, codeAckConfirmationInvalid, "confirmation token missing, wrong, or expired")
		return
	}
	n, err := s.deps.Supervisor.AcknowledgeMissingIDs(id, body.Dir, entry.ids)
	if s.writeAckError(w, id, err) {
		return
	}
	s.consumeAckToken(ackGroupKey(id, body.Dir), body.Token)
	writeJSON(w, http.StatusOK, map[string]any{"acknowledged": n})
}

// acknowledgeGroupChallenge computes the acknowledgeable ids under dir (every
// missing row, matching the unfiltered panel), a skipped breakdown for the rows
// that don't qualify, and whether the directory itself has vanished — the
// move/rename signature (design library-reconciliation §10.2) the dialog leads
// with. It binds the candidate ids to the minted token.
func (s *Server) acknowledgeGroupChallenge(w http.ResponseWriter, r *http.Request, id, dir string) {
	rows, err := s.deps.Store.MissingUnderDir(r.Context(), id, dir)
	if err != nil {
		s.log.Error("web: missing under dir failed", "rig", id, "dir", dir, "err", err)
		writeError(w, http.StatusInternalServerError, "could not read missing frames")
		return
	}

	var ids []int64
	var skipGone, skipRestore, skipNotVerified, skipAlready int
	for _, f := range rows {
		// Mint-order classification (state before restore before idempotence, design
		// library-reconciliation §10.8), so a held row is counted as not_verified
		// rather than silently dropped. Rows from MissingUnderDir already carry a
		// live missing mark.
		//
		// This is the GROUP path and stays state == verified ONLY (do NOT widen to
		// unclassified): it routes every non-verified row — including a culled
		// unclassified quarantine frame — to skipNotVerified, so a group-ack on the flat
		// quarantine directory drains nothing. Only the per-frame ackMintRefusal widens.
		//
		// A deleted row (a leftover, design library-reconciliation §13.1) is counted
		// as gone, apart from not_verified, so the refusal can say why.
		switch {
		case f.State == model.StateDeleted:
			skipGone++
		case f.State != model.StateVerified:
			skipNotVerified++
		case f.RestoreRequestedAt != 0:
			skipRestore++
		case f.MissingAckedAt != 0:
			skipAlready++
		default:
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		writeErrorCode(w, http.StatusConflict, codeAckNothingToDo, nothingToAcknowledge(skipGone, skipNotVerified, skipRestore, skipAlready))
		return
	}

	token, expires, err := s.mintAckToken(ackGroupKey(id, dir), ids)
	if err != nil {
		s.log.Error("web: mint ack token failed", "rig", id, "dir", dir, "err", err)
		writeError(w, http.StatusInternalServerError, "could not issue confirmation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires_at": expires.Unix(),
		"dir": dir, "count": len(ids), "dir_gone": s.dirGone(dir),
		"skipped": map[string]int{
			"gone":            skipGone,
			"restore_pending": skipRestore,
			"not_verified":    skipNotVerified,
			"already":         skipAlready,
		},
	})
}

// nothingToAcknowledge builds the 409 ACK_NOTHING_TO_DO message from the non-zero
// skip counts, so the refusal explains itself (design library-reconciliation
// §13.3): "Nothing to acknowledge in this folder: 69 already removed from the
// device, 1 restore pending."
func nothingToAcknowledge(gone, notVerified, restorePending, already int) string {
	var parts []string
	for _, c := range []struct {
		n    int
		what string
	}{
		{gone, "already removed from the device"},
		{notVerified, "not verified"},
		{restorePending, "restore pending"},
		{already, "already acknowledged"},
	} {
		if c.n > 0 {
			parts = append(parts, strconv.Itoa(c.n)+" "+c.what)
		}
	}
	if len(parts) == 0 {
		return "Nothing to acknowledge in this folder."
	}
	return "Nothing to acknowledge in this folder: " + strings.Join(parts, ", ") + "."
}

// ackMintRefusal classifies why a single frame cannot be acknowledged at mint time,
// in the mint's order — state before idempotence — so a held row is explained rather
// than silently reported "already done" (design library-reconciliation §10.8).
// already=true (with an empty code) is the acknowledged-already case, a 200
// {"already":true} and not an error; refuse=false means the frame is
// acknowledgeable.
func ackMintRefusal(f model.Frame) (code, msg string, already, refuse bool) {
	switch {
	case f.MissingSeenAt == 0:
		return codeAckNotMissing, "frame is not marked missing", false, true
	case !f.State.ProvenArchived():
		return codeAckNotVerified, "held or deleted frames cannot be acknowledged", false, true
	case f.RestoreRequestedAt != 0:
		return codeAckRestorePending, "cancel the pending restore first", false, true
	case f.MissingAckedAt != 0:
		return "", "", true, true
	default:
		return "", "", false, false
	}
}

// dirGone reports whether dir itself no longer exists, mirroring reconcileDir's
// dirGone discipline (design library-reconciliation §10.2): TRUE only on
// fs.ErrNotExist. Any other error concludes nothing (false), as does a nil
// LocalFS (optional, never wired in some tests).
func (s *Server) dirGone(dir string) bool {
	if s.deps.LocalFS == nil {
		return false
	}
	_, _, err := s.deps.LocalFS.Stat(dir)
	return errors.Is(err, fs.ErrNotExist)
}

// writeAckError maps a supervisor acknowledge error to its HTTP response, returning
// false only for a nil error — where the caller consumes the token and writes the
// success body. A DeadlineExceeded is NOT a failure (the worker applies the request
// despite the timed-out reply, design library-reconciliation §10.7): it becomes a
// 202 ACK_QUEUED via writeJSON, and the token is left unconsumed so the operator's
// retry still works.
func (s *Server) writeAckError(w http.ResponseWriter, rigID string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusAccepted, map[string]string{
			"code": codeAckQueued, "message": "the rig is busy; this may already have applied",
		})
	case errors.Is(err, s.deps.RigNotFound) || errors.Is(err, rig.ErrFrameNotFound):
		writeError(w, http.StatusNotFound, "no such rig or frame")
	case errors.Is(err, rig.ErrNotAcknowledgeable):
		writeErrorCode(w, http.StatusConflict, codeAckNotVerified, "held or deleted frames cannot be acknowledged")
	case errors.Is(err, s.deps.FrameNotMissing):
		writeErrorCode(w, http.StatusConflict, codeAckNotMissing, "frame is not marked missing")
	case errors.Is(err, rig.ErrRestorePending):
		writeErrorCode(w, http.StatusConflict, codeAckRestorePending, "cancel the pending restore first")
	default:
		s.log.Error("web: acknowledge failed", "rig", rigID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not acknowledge")
	}
	return true
}

// handleCancelRestore clears a pending restore request.
func (s *Server) handleCancelRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	frameID, err := strconv.ParseInt(r.PathValue("frameId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "frame id must be an integer")
		return
	}
	err = s.deps.Supervisor.CancelRestore(id, frameID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, s.deps.RigNotFound):
		writeError(w, http.StatusNotFound, "no such rig")
	default:
		s.log.Error("web: cancel restore failed", "rig", id, "frame", frameID, "err", err)
		writeError(w, http.StatusInternalServerError, "could not cancel restore")
	}
}
