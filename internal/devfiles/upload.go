package devfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// Upload: a staged file (staging.go) is planned and written like a one-file
// copy whose source is Commander's own disk. The plan is copy's destination
// check; the run is copy's write through the same sink and commit, emitting
// copy's NDJSON lines, so a destination is replaced only when the request
// binds it and it still has the size and mtime the user saw (doc.go, rule 1).
// No plan or run removes the staged file: Retry and Overwrite reuse it until
// the dialog discards it.

const (
	opUploadCheck = "Upload check"
	opUpload      = "Upload"
)

// UploadPlanRequest asks for an upload plan: the staged file and the folder
// it would be written into.
type UploadPlanRequest struct {
	ID string    `json:"id"`
	To CopyPlace `json:"to"`
}

// UploadPlan is a copy plan of the one staged file, its Rel being the file's
// name, plus the destination share's free bytes for the plan's space warning
// (nil when the device did not report them).
type UploadPlan struct {
	CopyPlan
	FreeBytes *int64 `json:"free_bytes,omitempty"`
}

// UploadRequest is an upload run. DstSize and DstMtime, both or neither, bind
// an overwrite to the destination as the user saw it (unix seconds).
type UploadRequest struct {
	ID       string    `json:"id"`
	To       CopyPlace `json:"to"`
	DstSize  *int64    `json:"dst_size,omitempty"`
	DstMtime *int64    `json:"dst_mtime,omitempty"`
}

// uploadWords names an upload in the sink's messages: a file standing where a
// reserved path must go can only be renamed on the PC.
func uploadWords() sinkWords {
	return sinkWords{noun: "upload", done: "Uploaded", reserved: func(rel, where string) string {
		return fmt.Sprintf("%s would stand where %s must go. Rename %s on your computer and choose it again.",
			rel, where, rel)
	}}
}

// checkUploadPlace validates an upload's destination: a managed rig, a share,
// a folder whose segments pass the strict check. It returns the configuration
// it checked against and the rig's profile in it, so a caller never reloads a
// configuration a save may since have dropped the rig from. That single load
// closes the reload race by construction; no test injects a save between two
// loads, since only a test-only hook in the middle of this function could.
func (s *Service) checkUploadPlace(to CopyPlace) (*model.Config, *model.RigProfile, error) {
	cfg := s.cfg.Load()
	profile, err := managedProfile(cfg, to.Rig)
	if err != nil {
		return nil, nil, err
	}
	if err := checkShare(to.Share); err != nil {
		return nil, nil, err
	}
	if err := checkDir(to.Dir); err != nil {
		return nil, nil, err
	}
	if err := checkStrictSegments(to.Dir); err != nil {
		return nil, nil, err
	}
	return cfg, profile, nil
}

// UploadPlan checks the file owner staged under req.ID against req.To, under
// the operation lock, writing nothing: copy's destination refusals, then
// copy's plan of one file entry built from the staged record. A missing folder
// is ErrNotFound, since an upload creates none; a file that would stand at a
// reserved path refuses the plan with ErrWatched.
func (s *Service) UploadPlan(ctx context.Context, owner string, req UploadPlanRequest) (*UploadPlan, error) {
	rec, unpin, err := s.pinStaged(owner, req.ID)
	if err != nil {
		return nil, err
	}
	defer unpin()
	to := req.To
	if _, _, err := s.checkUploadPlace(to); err != nil {
		return nil, err
	}
	unlock, err := s.lockOp(ctx, opUploadCheck, to.Rig)
	if err != nil {
		return nil, err
	}
	defer unlock()

	dv, err := s.destCheck(ctx, opUploadCheck, to)
	if err != nil {
		return nil, err
	}
	plan, err := s.planDest(ctx, opUploadCheck, to, false, dv,
		[]entry{{Rel: rec.name, Size: rec.size, Mtime: rec.mtime}}, uploadWords())
	if err != nil {
		return nil, err
	}
	free, err := s.shareFree(ctx, opUploadCheck, to)
	if err != nil {
		return nil, err
	}
	s.log.Debug("devfiles: upload plan", "rig", to.Rig, "share", to.Share, "op", opUploadCheck,
		"dir", to.Dir, "staging_id", rec.id, "name", rec.name, "bytes", rec.size,
		"conflicts", len(plan.Conflicts), "watched_frames", plan.Watched.Frames,
		"watched_previews", plan.Watched.Previews, "free", free)
	return &UploadPlan{CopyPlan: *plan, FreeBytes: free}, nil
}

// shareFree is the destination share's free bytes, or nil when the device
// does not answer them: they only feed a warning, so only a cancel fails the
// plan.
func (s *Service) shareFree(ctx context.Context, op string, to CopyPlace) (*int64, error) {
	var free int64
	err := s.withRig(ctx, op, to.Share, to.Rig, func(c *session) error {
		sh, err := c.share(to.Share)
		if err != nil {
			return err
		}
		f, _, err := sh.Statfs()
		if err != nil {
			return classify(err, c.rigID, c.name, "Free space of "+to.Share)
		}
		free = f
		return nil
	})
	switch {
	case errors.Is(err, ErrCancelled):
		return nil, err
	case err != nil:
		s.log.Warn("devfiles: share free space unknown", "rig", to.Rig, "share", to.Share, "op", op, "error", err)
		return nil, nil
	}
	return &free, nil
}

// Upload writes the file owner staged under req.ID into req.To. Before the
// lock it resolves the id, validates the place and refuses a reserved path
// from the configuration; under the lock it runs the pre-write check, then
// writes through the sink on the destination rig alone, reopening the staged
// file at byte 0 for every attempt, and reports through emit with copy's
// lines. An error returned before the first emit is an ordinary refusal; after
// a cancel or a failed emit it returns that error and emits nothing more. It
// never removes the staged file.
func (s *Service) Upload(ctx context.Context, owner string, req UploadRequest, emit Emit) error {
	rec, unpin, err := s.pinStaged(owner, req.ID)
	if err != nil {
		return err
	}
	defer unpin()
	to := req.To
	cfg, profile, err := s.checkUploadPlace(to)
	if err != nil {
		return err
	}
	overwrite := map[string]CopyOverwrite{}
	switch {
	case (req.DstSize == nil) != (req.DstMtime == nil):
		return newErr(ErrBadPath, to.Rig, "an overwrite needs both dst_size and dst_mtime")
	case req.DstSize != nil:
		overwrite[rec.name] = CopyOverwrite{Rel: rec.name, DstSize: *req.DstSize, DstMtime: *req.DstMtime}
	}
	dst := joinRel(to.Dir, rec.name)
	if root, ok := rig.ReservedPath(profile.Sources, to.Share, dst); ok {
		s.log.Warn("devfiles: reserved path refused", "rig", to.Rig, "share", to.Share, "op", opUpload,
			"paths", []string{dst})
		return newErr(ErrWatched, to.Rig, "%s", uploadWords().reserved(rec.name, displayPath(to.Share, root)))
	}

	unlock, err := s.lockOp(ctx, opUpload, to.Rig)
	if err != nil {
		return err
	}
	defer unlock()

	start := s.now()
	s.log.Info("devfiles: upload started", "rig", to.Rig, "share", to.Share, "op", opUpload,
		"dir", to.Dir, "staging_id", rec.id, "name", rec.name, "bytes", rec.size,
		"overwrite", len(overwrite) > 0,
		"watched_frame", rig.HarvestCandidate(profile, cfg.ExcludeExtensions, to.Share, dst),
		"watched_preview", rig.HarvestPreview(profile, cfg.ExcludeExtensions, to.Share, dst))
	logStopped := func() {
		s.log.Info("devfiles: upload cancelled", "rig", to.Rig, "share", to.Share, "op", opUpload,
			"staging_id", rec.id, "name", rec.name)
	}

	if err := s.uploadPreWrite(ctx, to); err != nil {
		if errors.Is(err, ErrCancelled) {
			logStopped()
		}
		return err
	}

	// A one-item copy run carries the counters and copy's NDJSON lines; the
	// sink does the write.
	r := &copyRun{
		sinkRun: &sinkRun{
			s: s, ctx: ctx, op: opUpload, words: uploadWords(), to: to,
			overwrite: overwrite,
			ensured:   map[string]bool{},
		},
		// Nothing follows a cancel: the client has aborted and builds its own report.
		emit: func(line any) error {
			if ctx.Err() != nil {
				return cancelled(ctx)
			}
			return emit(line)
		},
		total:      1,
		bytesTotal: rec.size,
		current:    rec.name,
	}
	if err := r.progress(0, 0, true); err != nil {
		return err
	}
	res, err := r.writeItem(rec.name, s.stagedSource(rec, r))
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			logStopped()
		}
		return err
	}
	r.aborted = res.abort
	r.bytesDone = rec.size
	if err := r.record(rec.name, res); err != nil {
		return err
	}
	s.log.Info("devfiles: upload finished", "rig", to.Rig, "share", to.Share, "op", opUpload,
		"staging_id", rec.id, "outcome", res.outcome, "code", res.code, "duration", s.now().Sub(start))
	return r.emit(SummaryLine{Type: lineSummary, Done: r.done, Skipped: r.skipped, Failed: r.failed, Aborted: r.aborted})
}

// uploadPreWrite is the first half of copy's ensureAll without creating
// anything: the session checkout's identity probe, the host-bound refusal,
// the share, and to.Dir still a folder. Its failures are refusals returned
// before any stream line.
func (s *Service) uploadPreWrite(ctx context.Context, to CopyPlace) error {
	return s.withRig(ctx, opUpload, to.Share, to.Rig, func(c *session) error {
		if err := s.requireWritable(c, to.Share); err != nil {
			return err
		}
		sh, err := c.share(to.Share)
		if err != nil {
			return err
		}
		if to.Dir == "" {
			return nil
		}
		fi, err := sh.Stat(to.Dir)
		switch {
		case err != nil:
			return classify(err, c.rigID, c.name, displayPath(to.Share, to.Dir))
		case !fi.IsDir:
			return s.typeConflictErr(c.rigID, to.Share, to.Dir)
		}
		return nil
	})
}

// stagedSource is the sink's source for a staged file: every attempt reopens
// it at byte 0 on Commander's disk, on the destination rig's session alone.
func (s *Service) stagedSource(rec *stagedFile, r *copyRun) source {
	return source{
		rigs: []string{r.to.Rig},
		open: func([]*session) (io.ReadCloser, time.Time, *itemResult, error) {
			f, err := os.Open(s.staging.path(rec.id))
			if err != nil {
				return nil, time.Time{}, nil, s.stagedReadErr(rec, err)
			}
			return f, rec.mtime, nil, nil
		},
		zeroMtimeIsNone: true,
		readFailure:     func(err error) error { return s.stagedReadErr(rec, err) },
		progress:        func(n int64) error { return r.progress(0, n, false) },
		logAttrs:        []any{"staging_id", rec.id},
	}
}

// stagedReadErr is a failure reading Commander's own staged copy. It wraps
// nothing, so withRetry never mistakes it for the rig's network failure and
// the item fails once as an error; its message names no local path.
func (s *Service) stagedReadErr(rec *stagedFile, err error) error {
	s.log.Warn("devfiles: staged file unreadable", "staging_id", rec.id, "name", rec.name, "error", err)
	return errors.New("Commander couldn't read the copy of " + rec.name + " it holds. Close this and upload the file again.")
}
