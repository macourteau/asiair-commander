package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/macourteau/asiair-commander/internal/devfiles"
)

// DeviceFiles is the device file browser's service (internal/devfiles), the
// File browser's (/files) view of the managed rigs' ASIAIRs. Its device I/O
// runs on the calling request's goroutine and shares no lock with the workers,
// the hub or the snapshot path (design
// device-file-browser §2 decision 2). Every refusal is a devfiles sentinel the
// handlers map to an error code (filesErrorCodes).
type DeviceFiles interface {
	List(ctx context.Context, rigID, share, dir string) (*devfiles.Listing, error)
	Mkdir(ctx context.Context, rigID, share, dir, name string) error
	DownloadCheck(ctx context.Context, rigID, share, dir string, names []string) (*devfiles.DownloadInfo, error)
	Download(ctx context.Context, rigID, share, dir string, names []string, w devfiles.DownloadWriter) error
	DeletePlan(ctx context.Context, rigID, share, dir string, names []string) (*devfiles.DeletePlan, error)
	Delete(ctx context.Context, rigID string, req devfiles.DeleteRequest, emit devfiles.Emit) error
	CopyPlan(ctx context.Context, req devfiles.CopyPlanRequest) (*devfiles.CopyPlan, error)
	Copy(ctx context.Context, req devfiles.CopyRequest, emit devfiles.Emit) error
	Rename(ctx context.Context, rigID, share, dir, name, newName string) error
	// The upload calls take the requesting session's owner key (uploadOwner),
	// so only the session that staged a file can plan, run or discard it.
	UploadStage(ctx context.Context, owner, name string, size int64, mtime time.Time, body io.Reader) (devfiles.StagedUpload, error)
	UploadPlan(ctx context.Context, owner string, req devfiles.UploadPlanRequest) (*devfiles.UploadPlan, error)
	Upload(ctx context.Context, owner string, req devfiles.UploadRequest, emit devfiles.Emit) error
	UploadDiscard(owner, id string) error
}

// filesErrorCode is the status and wire code one devfiles sentinel answers with.
type filesErrorCode struct {
	kind   error
	status int
	code   string
}

// filesErrorCodes is the API's Errors table. ErrCancelled has no row: a
// cancelled request writes nothing more, and neither ErrMidStream nor
// ErrStageBody has one: each aborts its request. ErrClosed (shutting down)
// answers as an unavailable service.
var filesErrorCodes = []filesErrorCode{
	{devfiles.ErrUnknownRig, http.StatusNotFound, "unknown_rig"},
	{devfiles.ErrBadPath, http.StatusBadRequest, "bad_path"},
	{devfiles.ErrTooMany, http.StatusBadRequest, "too_many"},
	{devfiles.ErrIntoItself, http.StatusBadRequest, "into_itself"},
	{devfiles.ErrSameFolder, http.StatusBadRequest, "same_folder"},
	{devfiles.ErrTypeConflict, http.StatusConflict, "type_conflict"},
	{devfiles.ErrNotFound, http.StatusNotFound, "not_found"},
	{devfiles.ErrExists, http.StatusConflict, "exists"},
	{devfiles.ErrNoShare, http.StatusConflict, "no_share"},
	{devfiles.ErrLayout, http.StatusConflict, "layout"},
	{devfiles.ErrWatched, http.StatusConflict, "watched"},
	{devfiles.ErrBusy, http.StatusConflict, "busy"},
	{devfiles.ErrUnarchived, http.StatusConflict, "unarchived"},
	{devfiles.ErrIdentity, http.StatusConflict, "identity"},
	{devfiles.ErrHostBound, http.StatusConflict, "host_bound"},
	{devfiles.ErrDenied, http.StatusForbidden, "denied"},
	{devfiles.ErrInUse, http.StatusConflict, "in_use"},
	{devfiles.ErrUnreachable, http.StatusServiceUnavailable, "unreachable"},
	{devfiles.ErrClosed, http.StatusServiceUnavailable, codeFilesUnavailable},
	{devfiles.ErrLengthRequired, http.StatusLengthRequired, "length_required"},
	{devfiles.ErrStagingGone, http.StatusGone, "staging_gone"},
	{devfiles.ErrStagingFull, http.StatusInsufficientStorage, "staging_full"},
}

const codeFilesUnavailable = "files_unavailable"

// filesAvailable answers 503 files_unavailable when no service is wired in.
func (s *Server) filesAvailable(w http.ResponseWriter) bool {
	if s.deps.DeviceFiles == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, codeFilesUnavailable,
			"the device file browser is not available")
		return false
	}
	return true
}

// filesCancelled reports whether err ends a request whose client went away:
// the service's ErrCancelled, or any failure once the request's context ended
// (a slot wait, say, returns the bare context error).
func filesCancelled(r *http.Request, err error) bool {
	return errors.Is(err, devfiles.ErrCancelled) || r.Context().Err() != nil
}

// writeFilesError answers a devfiles failure that happened before anything was
// written. The outermost *devfiles.Error decides the code, since its cause may
// itself wrap another sentinel; a 409 unarchived also carries the fresh count
// the dialog shows. A cancelled request gets nothing: its client is gone.
func (s *Server) writeFilesError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if filesCancelled(r, err) {
		s.log.Debug("web: device files request cancelled", "op", op, "path", r.URL.Path)
		return
	}
	kind := error(nil)
	var fe *devfiles.Error
	if errors.As(err, &fe) {
		kind = fe.Kind
	}
	for _, c := range filesErrorCodes {
		if (kind != nil && kind == c.kind) || (kind == nil && errors.Is(err, c.kind)) {
			s.log.Debug("web: device files request refused", "op", op, "path", r.URL.Path,
				"code", c.code, "err", err)
			var ue *devfiles.UnarchivedError
			if c.kind == devfiles.ErrUnarchived && errors.As(err, &ue) {
				writeJSON(w, c.status, map[string]any{
					"error": http.StatusText(c.status), "code": c.code, "message": err.Error(),
					"unarchived": ue.Unarchived,
				})
				return
			}
			writeErrorCode(w, c.status, c.code, err.Error())
			return
		}
	}
	s.log.Error("web: device files request failed", "op", op, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "the device files request failed unexpectedly")
}

// handleFilesList answers GET /api/rigs/{id}/files: the rig's shares (no
// share) or one folder of a share.
func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	q := r.URL.Query()
	l, err := s.deps.DeviceFiles.List(r.Context(), r.PathValue("id"), q.Get("share"), q.Get("dir"))
	if err != nil {
		s.writeFilesError(w, r, "list", err)
		return
	}
	if l.Entries == nil {
		l.Entries = []devfiles.ListEntry{}
	}
	writeJSON(w, http.StatusOK, l)
}

// filesMkdirBody is POST /api/rigs/{id}/files/mkdir: the new folder name
// inside dir on share.
type filesMkdirBody struct {
	Share string `json:"share"`
	Dir   string `json:"dir"`
	Name  string `json:"name"`
}

func (s *Server) handleFilesMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	var body filesMkdirBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid new folder request")
		return
	}
	if err := s.deps.DeviceFiles.Mkdir(r.Context(), r.PathValue("id"), body.Share, body.Dir, body.Name); err != nil {
		s.writeFilesError(w, r, "mkdir", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// filesSelectionBody is a delete plan request: the names selected in dir on share.
type filesSelectionBody struct {
	Share string   `json:"share"`
	Dir   string   `json:"dir"`
	Names []string `json:"names"`
}

func (s *Server) handleFilesDeletePlan(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	var body filesSelectionBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid delete plan request")
		return
	}
	p, err := s.deps.DeviceFiles.DeletePlan(r.Context(), r.PathValue("id"), body.Share, body.Dir, body.Names)
	if err != nil {
		s.writeFilesError(w, r, "delete plan", err)
		return
	}
	writeJSON(w, http.StatusOK, nonNilDeletePlan(p))
}

// nonNilDeletePlan makes every slice of p serialize as [] rather than null.
func nonNilDeletePlan(p *devfiles.DeletePlan) *devfiles.DeletePlan {
	if p.ByExt == nil {
		p.ByExt = []devfiles.ExtTotal{}
	}
	if p.WatchedRoots == nil {
		p.WatchedRoots = []string{}
	}
	if p.Items == nil {
		p.Items = []devfiles.DeleteItem{}
	}
	if p.DirsDeepestFirst == nil {
		p.DirsDeepestFirst = []string{}
	}
	return p
}

func (s *Server) handleFilesDelete(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	var req devfiles.DeleteRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid delete request")
		return
	}
	// A negative acknowledgement is no confirmation at all; the service would only
	// ever compare it, so refuse it here rather than let it read as "none".
	if req.AcknowledgedUnarchived < 0 {
		s.log.Debug("web: delete refused: negative acknowledged_unarchived", "path", r.URL.Path,
			"acknowledged", req.AcknowledgedUnarchived)
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid delete request: acknowledged_unarchived is negative")
		return
	}
	id := r.PathValue("id")
	s.streamFilesRun(w, r, "delete", func(emit devfiles.Emit) error {
		return s.deps.DeviceFiles.Delete(r.Context(), id, req, emit)
	})
}

func (s *Server) handleFilesCopyPlan(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	var req devfiles.CopyPlanRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid copy plan request")
		return
	}
	p, err := s.deps.DeviceFiles.CopyPlan(r.Context(), req)
	if err != nil {
		s.writeFilesError(w, r, "copy plan", err)
		return
	}
	writeJSON(w, http.StatusOK, nonNilCopyPlan(p))
}

// nonNilCopyPlan makes every slice of p serialize as [] rather than null.
func nonNilCopyPlan(p *devfiles.CopyPlan) *devfiles.CopyPlan {
	if p.Dirs == nil {
		p.Dirs = []string{}
	}
	if p.Items == nil {
		p.Items = []devfiles.CopyPlanItem{}
	}
	if p.Conflicts == nil {
		p.Conflicts = []devfiles.CopyConflict{}
	}
	return p
}

func (s *Server) handleFilesCopy(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	var req devfiles.CopyRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid copy request")
		return
	}
	s.streamFilesRun(w, r, "copy", func(emit devfiles.Emit) error {
		return s.deps.DeviceFiles.Copy(r.Context(), req, emit)
	})
}

// filesRenameBody is POST /api/rigs/{id}/files/rename: the entry name in dir
// on share, exactly as listed, and the name it should take.
type filesRenameBody struct {
	Share   string `json:"share"`
	Dir     string `json:"dir"`
	Name    string `json:"name"`
	NewName string `json:"new_name"`
}

func (s *Server) handleFilesRename(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	var body filesRenameBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid rename request")
		return
	}
	if err := s.deps.DeviceFiles.Rename(r.Context(), r.PathValue("id"), body.Share, body.Dir, body.Name, body.NewName); err != nil {
		s.writeFilesError(w, r, "rename", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadOwner is the requesting session's owner key for staged uploads: the
// SHA-256 of the session's XSRF claim, which is minted at login, kept by every
// idle-window refresh and different for every login, so a staged file is
// reachable only from the session that sent it. guardWrite has already
// validated the session; a failure here answers as the guard would. The key
// is never logged.
func (s *Server) uploadOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	claims, err := s.session(r)
	if err != nil || claims.XSRF == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return "", false
	}
	sum := sha256.Sum256([]byte(claims.XSRF))
	return hex.EncodeToString(sum[:]), true
}

// handleFilesUploadStage answers POST /api/files/upload/stage?name=&mtime_ms=:
// the raw body is the one picked file, which the service stages on
// Commander's disk without touching any device. The name and the browser's
// lastModified travel in the query so every refusal is decided before the
// body is read.
//
// A refusal drains the body first, bounded by Content-Length: on HTTP/1.1 Go
// otherwise closes the connection after a short discard and the browser
// reports a network error instead of the refusal. A body that breaks off has
// no reader for an answer, so the request is aborted, the service having
// already removed the partial file.
func (s *Server) handleFilesUploadStage(w http.ResponseWriter, r *http.Request) {
	if s.deps.DeviceFiles == nil {
		s.drainStageBody(r)
		s.filesAvailable(w)
		return
	}
	owner, ok := s.uploadOwner(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var mtime time.Time
	if v := q.Get("mtime_ms"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.log.Debug("web: upload staging refused: bad mtime_ms", "path", r.URL.Path, "mtime_ms", v)
			s.drainStageBody(r)
			writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid upload request: mtime_ms is not an integer")
			return
		}
		mtime = time.UnixMilli(ms)
	}
	// Without a Content-Length header net/http reads an HTTP/1.1 body as empty
	// (length 0), which would stage an empty file; the header's absence, not
	// the length, is what the 411 is for.
	size := r.ContentLength
	if _, declared := r.Header["Content-Length"]; !declared {
		size = -1
	}

	staged, err := s.deps.DeviceFiles.UploadStage(r.Context(), owner, q.Get("name"), size, mtime, r.Body)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, staged)
	case errors.Is(err, devfiles.ErrStageBody):
		s.log.Warn("web: upload staging body broke off; aborting the request", "path", r.URL.Path, "err", err)
		panic(http.ErrAbortHandler)
	case filesCancelled(r, err):
		s.log.Debug("web: upload staging cancelled", "path", r.URL.Path)
	default:
		if size >= 0 {
			s.drainStageBody(r)
		}
		s.writeFilesError(w, r, "upload stage", err)
	}
}

// drainStageBody reads what is left of a staging request's body, bounded by
// its Content-Length, so the refusal that follows reaches the browser.
func (s *Server) drainStageBody(r *http.Request) {
	if r.ContentLength <= 0 {
		return
	}
	n, err := io.CopyN(io.Discard, r.Body, r.ContentLength)
	if n > 0 || err != nil {
		s.log.Debug("web: upload staging body drained before a refusal", "path", r.URL.Path,
			"bytes", n, "err", err)
	}
}

func (s *Server) handleFilesUploadPlan(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	owner, ok := s.uploadOwner(w, r)
	if !ok {
		return
	}
	var req devfiles.UploadPlanRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid upload plan request")
		return
	}
	p, err := s.deps.DeviceFiles.UploadPlan(r.Context(), owner, req)
	if err != nil {
		s.writeFilesError(w, r, "upload plan", err)
		return
	}
	nonNilCopyPlan(&p.CopyPlan)
	writeJSON(w, http.StatusOK, p)
}

// handleFilesUpload runs an upload of a staged file on copy's NDJSON stream:
// the same lines, the same refusals before the first one, and a cancelled
// request is copy's Stop.
func (s *Server) handleFilesUpload(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	owner, ok := s.uploadOwner(w, r)
	if !ok {
		return
	}
	var req devfiles.UploadRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid upload request")
		return
	}
	s.streamFilesRun(w, r, "upload", func(emit devfiles.Emit) error {
		return s.deps.DeviceFiles.Upload(r.Context(), owner, req, emit)
	})
}

// filesUploadDiscardBody is POST /api/files/upload/discard: the staged file
// the closing dialog no longer needs.
type filesUploadDiscardBody struct {
	ID string `json:"id"`
}

func (s *Server) handleFilesUploadDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	owner, ok := s.uploadOwner(w, r)
	if !ok {
		return
	}
	var body filesUploadDiscardBody
	if err := decodeJSON(r.Body, &body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid upload discard request")
		return
	}
	if err := s.deps.DeviceFiles.UploadDiscard(owner, body.ID); err != nil {
		s.writeFilesError(w, r, "upload discard", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// streamFilesRun runs a copy, upload or delete and streams its lines as NDJSON, the
// refile pattern: the stream begins lazily with the first line, so a refusal
// found before it (busy, unarchived, host_bound, a bad path) is an ordinary
// JSON error. A run that is not cancelled ends with its summary line. After a
// cancel nothing more is written, so the client builds the Stopped report
// itself; any other failure after the first line just ends the stream, which
// the client reports as interrupted.
func (s *Server) streamFilesRun(w http.ResponseWriter, r *http.Request, op string, run func(devfiles.Emit) error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	started := false
	emit := func(line any) error {
		// The client is gone: write nothing more, and stop the run.
		if err := r.Context().Err(); err != nil {
			return err
		}
		b, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	err := run(emit)
	switch {
	case err == nil:
	case filesCancelled(r, err):
		s.log.Debug("web: device files run cancelled", "op", op, "path", r.URL.Path, "started", started)
	case started:
		s.log.Warn("web: device files run ended without a summary", "op", op, "path", r.URL.Path, "err", err)
	default:
		s.writeFilesError(w, r, op, err)
	}
}

// handleFilesDownload answers GET /api/rigs/{id}/files/download: one selected
// file as itself, anything else as a streamed zip, or with check=1 the
// download's JSON preview. A failure after the first byte cannot become an
// error response any more, so it aborts the connection (http.ErrAbortHandler):
// the browser then marks the download failed instead of saving a truncated
// file, which matters most for a zip, whose end nothing else betrays.
func (s *Server) handleFilesDownload(w http.ResponseWriter, r *http.Request) {
	if !s.filesAvailable(w) {
		return
	}
	q := r.URL.Query()
	id, share, dir, names := r.PathValue("id"), q.Get("share"), q.Get("dir"), q["name"]
	if q.Get("check") == "1" {
		info, err := s.deps.DeviceFiles.DownloadCheck(r.Context(), id, share, dir, names)
		if err != nil {
			s.writeFilesError(w, r, "download check", err)
			return
		}
		writeJSON(w, http.StatusOK, info)
		return
	}

	err := s.deps.DeviceFiles.Download(r.Context(), id, share, dir, names, downloadResponse{w})
	switch {
	case err == nil:
	case errors.Is(err, devfiles.ErrMidStream):
		// Tested first: the mid-stream error also unwraps to its cause's sentinel.
		s.log.Warn("web: device download aborted after its first byte", "rig", id, "share", share, "err", err)
		panic(http.ErrAbortHandler)
	default:
		s.writeFilesError(w, r, "download", err)
	}
}

// downloadResponse is the DownloadWriter over the response: Begin sets the
// headers just before the first byte.
type downloadResponse struct {
	w http.ResponseWriter
}

func (d downloadResponse) Begin(info devfiles.DownloadInfo) {
	h := d.w.Header()
	if info.Zipped {
		h.Set("Content-Type", "application/zip")
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatInt(info.Bytes, 10))
	}
	h.Set("X-Content-Type-Options", "nosniff")
	// FormatMediaType encodes a non-ASCII name per RFC 2231 (filename*=utf-8''…);
	// it returns "" only for a name it cannot express, and then the browser
	// names the file from the URL.
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": info.Filename})
	if disposition == "" {
		disposition = "attachment"
	}
	h.Set("Content-Disposition", disposition)
	d.w.WriteHeader(http.StatusOK)
}

func (d downloadResponse) Write(p []byte) (int, error) { return d.w.Write(p) }
