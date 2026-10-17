package devfiles

import (
	"context"
	"errors"
	"path"
	"slices"
	"sort"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/filter"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// deleteMtimeSlack is how far a file's mtime may move from the one the user
// confirmed and still count as the same file (FAT's 2 s resolution).
const deleteMtimeSlack = 2 * time.Second

// DeletePlan is the delete dialog's summary of a walked selection. Items and
// DirsDeepestFirst are what the run request echoes back; every path is
// relative to the selection's folder.
type DeletePlan struct {
	Dirs             int          `json:"dirs"`
	Files            int          `json:"files"`
	Bytes            int64        `json:"bytes"`
	ByExt            []ExtTotal   `json:"by_ext"`
	Unarchived       int          `json:"unarchived"`
	UnwatchedFiles   int          `json:"unwatched_files"`
	WatchedRoots     []string     `json:"watched_roots"`
	Items            []DeleteItem `json:"items"`
	DirsDeepestFirst []string     `json:"dirs_deepest_first"`
}

// ExtTotal is one extension's share of a selection: Ext is filter.Ext's
// lower-cased extension, "" for a file with no extension (a dotfile included).
type ExtTotal struct {
	Ext   string `json:"ext"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// DeleteItem is one file of a plan, with the size and mtime (unix seconds,
// device clock) the user confirmed; the run deletes it only while it still
// has them.
type DeleteItem struct {
	Rel   string `json:"rel"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

// DeleteRequest is a delete run: the plan's items and folders (deepest first)
// as echoed by the client, and the unarchived count the user acknowledged.
type DeleteRequest struct {
	Share                  string       `json:"share"`
	Dir                    string       `json:"dir"`
	Items                  []DeleteItem `json:"items"`
	Dirs                   []string     `json:"dirs"`
	AcknowledgedUnarchived int          `json:"acknowledged_unarchived"`
}

// UnarchivedError is the run's refusal when its fresh unarchived count exceeds
// the acknowledged one. It unwraps to an *Error of kind ErrUnarchived, and
// carries the fresh count the 409 body reports.
type UnarchivedError struct {
	Err        *Error
	Unarchived int
}

func (e *UnarchivedError) Error() string { return e.Err.Error() }
func (e *UnarchivedError) Unwrap() error { return e.Err }

// DeletePlan walks the selection names in dir on share under the operation
// lock and summarizes it: counts, bytes by extension, the files with no
// archived copy, the files outside watched folders, the watched roots the
// selection is or contains, and the items a run echoes. It writes nothing,
// and a listing failure fails it whole.
func (s *Service) DeletePlan(ctx context.Context, rigID, share, dir string, names []string) (*DeletePlan, error) {
	unlock, err := s.lockOp(ctx, "Delete check", rigID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	entries, err := s.walk(ctx, "Delete check", rigID, share, dir, names)
	if err != nil {
		return nil, err
	}
	cfg := s.cfg.Load()
	profile, err := managedProfile(cfg, rigID)
	if err != nil {
		return nil, err
	}

	plan := &DeletePlan{
		ByExt:            []ExtTotal{},
		WatchedRoots:     []string{},
		Items:            []DeleteItem{},
		DirsDeepestFirst: []string{},
	}
	byExt := map[string]*ExtTotal{}
	for _, e := range entries {
		p := joinRel(dir, e.Rel)
		if e.Dir {
			plan.Dirs++
			plan.DirsDeepestFirst = append(plan.DirsDeepestFirst, e.Rel)
			if rig.WatchedRoot(profile, share, p) {
				plan.WatchedRoots = append(plan.WatchedRoots, e.Rel)
			}
			continue
		}
		plan.Files++
		plan.Bytes += e.Size
		ext := filter.Ext(path.Base(e.Rel))
		t := byExt[ext]
		if t == nil {
			t = &ExtTotal{Ext: ext}
			byExt[ext] = t
		}
		t.Files++
		t.Bytes += e.Size
		plan.Items = append(plan.Items, DeleteItem{Rel: e.Rel, Size: e.Size, Mtime: unixOrZero(e.Mtime)})

		if !rig.HarvestCandidate(profile, cfg.ExcludeExtensions, share, p) &&
			!rig.HarvestPreview(profile, cfg.ExcludeExtensions, share, p) {
			plan.UnwatchedFiles++
		}
		un, err := s.unarchived(ctx, cfg, profile, share, p, e.Size, e.Mtime)
		if err != nil {
			return nil, err
		}
		if un {
			plan.Unarchived++
		}
	}
	// The walk is pre-order, so its reverse puts every folder after its
	// subfolders.
	slices.Reverse(plan.DirsDeepestFirst)
	for _, t := range byExt {
		plan.ByExt = append(plan.ByExt, *t)
	}
	sort.Slice(plan.ByExt, func(i, j int) bool {
		a, b := plan.ByExt[i], plan.ByExt[j]
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		return a.Ext < b.Ext
	})
	s.log.Debug("devfiles: delete plan", "rig", rigID, "share", share, "op", "Delete check", "dir", dir,
		"files", plan.Files, "dirs", plan.Dirs, "bytes", plan.Bytes, "unarchived", plan.Unarchived,
		"watched_roots", len(plan.WatchedRoots))
	return plan, nil
}

// unarchived reports whether share/p is a file the harvester treats as a frame
// with no proven archived copy at this size and mtime (provenArchived). The
// store row is looked up under the harvester's key form.
func (s *Service) unarchived(ctx context.Context, cfg *model.Config, profile *model.RigProfile,
	share, p string, size int64, mtime time.Time) (bool, error) {
	if !rig.HarvestCandidate(profile, cfg.ExcludeExtensions, share, p) {
		return false, nil
	}
	key, _ := rig.HarvestStoreKey(profile, share, p)
	row, found, err := s.store.GetFrame(ctx, profile.ID, share, key)
	if err != nil {
		return false, err
	}
	if !found {
		row = nil
	}
	return !s.provenArchived(row, size, mtime), nil
}

// provenArchived reports whether row proves a NAS copy of the device file at
// this size and mtime: rig.ArchivedCopy, and the copy at DestPath still stats
// at ArchiveSize. The store's MissingSeenAt is only as fresh as the last
// library scan, so a copy culled since still reads archived there; a copy that
// is gone, has another length, or cannot be stat'ed is not proof. Both the
// delete count and the copy conflict's dst_ledger decide through it.
func (s *Service) provenArchived(row *model.Frame, size int64, mtime time.Time) bool {
	if !rig.ArchivedCopy(row, size, mtime) {
		return false
	}
	nasSize, _, err := s.lfs.Stat(row.DestPath)
	switch {
	case err != nil:
		s.log.Debug("devfiles: NAS copy unreadable; not proven archived", "rig", row.RigID,
			"share", row.SourceShare, "path", row.SourcePath, "dest", row.DestPath, "error", err)
		return false
	case nasSize != row.ArchiveSize:
		s.log.Debug("devfiles: NAS copy length differs; not proven archived", "rig", row.RigID,
			"share", row.SourceShare, "path", row.SourcePath, "dest", row.DestPath,
			"size", nasSize, "archive_size", row.ArchiveSize)
		return false
	}
	return true
}

// Delete runs a confirmed delete plan on rigID, streaming its lines to emit.
// It deletes only the echoed items, each only while its size and mtime still
// match, then the echoed folders in the order given (deepest first), each only
// while it is still a folder and the device accepts it as empty. Before
// anything is deleted it recounts the unarchived files among the items and
// refuses with *UnarchivedError when the count grew past the acknowledged one.
//
// An error returned before the first emit (validation, busy, unarchived,
// host_bound, unreachable) is an ordinary error for the handler to answer. A
// run that is not cancelled ends with a SummaryLine and returns nil; a cancel
// returns ErrCancelled with nothing more emitted, and an emit failure is
// returned as is.
func (s *Service) Delete(ctx context.Context, rigID string, req DeleteRequest, emit Emit) error {
	const op = "Delete"
	if err := s.validateDelete(rigID, req); err != nil {
		return err
	}
	unlock, err := s.lockOp(ctx, op, rigID)
	if err != nil {
		return err
	}
	defer unlock()

	cfg := s.cfg.Load()
	profile, err := managedProfile(cfg, rigID)
	if err != nil {
		return err
	}
	var bytesTotal int64
	fresh := 0
	for _, it := range req.Items {
		bytesTotal += it.Size
		un, err := s.unarchived(ctx, cfg, profile, req.Share, joinRel(req.Dir, it.Rel), it.Size, time.Unix(it.Mtime, 0))
		if err != nil {
			return err
		}
		if un {
			fresh++
		}
	}
	if fresh > req.AcknowledgedUnarchived {
		s.log.Info("devfiles: delete refused: unarchived count grew", "rig", rigID, "share", req.Share,
			"op", op, "acknowledged", req.AcknowledgedUnarchived, "unarchived", fresh)
		return &UnarchivedError{
			Err: newErr(ErrUnarchived, rigID,
				"Since you confirmed, more files lost their archived copy: now %d, was %d. Nothing was deleted.",
				fresh, req.AcknowledgedUnarchived),
			Unarchived: fresh,
		}
	}

	// Prove the session (identity, writable, share) before the stream begins, so
	// those refusals reach the client as plain errors. The parked session serves
	// the first item.
	err = s.withRig(ctx, op, req.Share, rigID, func(c *session) error {
		if err := s.requireWritable(c, req.Share); err != nil {
			return err
		}
		_, err := c.share(req.Share)
		return err
	})
	if err != nil {
		return err
	}

	started := s.now()
	total := len(req.Items) + len(req.Dirs)
	s.log.Info("devfiles: delete started", "rig", rigID, "share", req.Share, "op", op, "dir", req.Dir,
		"files", len(req.Items), "dirs", len(req.Dirs), "bytes", bytesTotal, "unarchived", fresh)

	// The run order: files, then folders as echoed (deepest first).
	type step struct {
		rel  string
		file *DeleteItem
	}
	steps := make([]step, 0, total)
	for i := range req.Items {
		steps = append(steps, step{rel: req.Items[i].Rel, file: &req.Items[i]})
	}
	for _, d := range req.Dirs {
		steps = append(steps, step{rel: d})
	}

	sum := SummaryLine{Type: lineSummary}
	var bytesDone int64
	for i, st := range steps {
		if ctx.Err() != nil {
			s.log.Info("devfiles: delete cancelled", "rig", rigID, "share", req.Share, "op", op,
				"done", i, "total", total, "duration", s.now().Sub(started))
			return cancelled(ctx)
		}
		if err := emit(ProgressLine{Type: lineProgress, Done: i, Total: total,
			BytesDone: bytesDone, BytesTotal: bytesTotal, Current: st.rel}); err != nil {
			return err
		}
		p := joinRel(req.Dir, st.rel)
		var code string
		if st.file != nil {
			code, err = s.deleteFile(ctx, rigID, req.Share, p, *st.file)
			bytesDone += st.file.Size
		} else {
			code, err = s.deleteDir(ctx, rigID, req.Share, p)
		}
		if errors.Is(err, ErrCancelled) {
			s.log.Info("devfiles: delete cancelled", "rig", rigID, "share", req.Share, "op", op,
				"done", i, "total", total, "in_flight", p, "duration", s.now().Sub(started))
			return err
		}
		var line *ItemLine
		switch {
		case err != nil:
			sum.Failed++
			line = &ItemLine{Type: lineItem, Rel: st.rel, Outcome: outcomeFailed,
				Code: deleteFailureCode(err), Message: err.Error()}
		case code == "" || code == codeAlreadyGone:
			sum.Done++
		default:
			sum.Skipped++
			line = &ItemLine{Type: lineItem, Rel: st.rel, Outcome: outcomeSkipped, Code: code}
		}
		if line != nil {
			if err := emit(*line); err != nil {
				return err
			}
		}
		if abort := runAbort(err); abort != "" {
			// One exhausted item ends the run, as does a wrong device or a rig that
			// lost its identity: the rest are not attempted, with no further I/O
			// against a rig that would only fail them the same way.
			for _, rest := range steps[i+1:] {
				sum.Failed++
				if err := emit(ItemLine{Type: lineItem, Rel: rest.rel, Outcome: outcomeFailed,
					Code: codeNotAttempted}); err != nil {
					return err
				}
			}
			sum.Aborted = abort
			break
		}
	}
	s.log.Info("devfiles: delete finished", "rig", rigID, "share", req.Share, "op", op,
		"done", sum.Done, "skipped", sum.Skipped, "failed", sum.Failed, "aborted", sum.Aborted,
		"duration", s.now().Sub(started))
	return emit(sum)
}

// validateDelete checks every echoed path before any lock or device call.
func (s *Service) validateDelete(rigID string, req DeleteRequest) error {
	if err := checkShare(req.Share); err != nil {
		return err
	}
	if err := checkDir(req.Dir); err != nil {
		return err
	}
	if len(req.Items) == 0 && len(req.Dirs) == 0 {
		return newErr(ErrBadPath, rigID, "nothing to delete")
	}
	if len(req.Items) > s.maxFiles {
		return newErr(ErrTooMany, rigID,
			"This selection has more than %d files. Select fewer files or a subfolder.", s.maxFiles)
	}
	seen := make(map[string]bool, len(req.Items)+len(req.Dirs))
	check := func(rel string) error {
		if err := checkRel(rel); err != nil {
			return err
		}
		if seen[rel] {
			return newErr(ErrBadPath, rigID, "%q is listed twice", rel)
		}
		seen[rel] = true
		return nil
	}
	for _, it := range req.Items {
		if err := check(it.Rel); err != nil {
			return err
		}
	}
	for _, d := range req.Dirs {
		if err := check(d); err != nil {
			return err
		}
	}
	return nil
}

// deleteFile deletes the file p, one retry unit, only while it still has the
// confirmed size and mtime. It answers "" when deleted, codeAlreadyGone when
// the file is not there (the harvester may have got there first, or a retry
// follows a lost reply), codeChanged when it moved; any other outcome is err.
func (s *Service) deleteFile(ctx context.Context, rigID, share, p string, it DeleteItem) (string, error) {
	var code string
	err := s.withRig(ctx, "Delete", share, rigID, func(c *session) error {
		code = ""
		if err := s.requireWritable(c, share); err != nil {
			return err
		}
		sh, err := c.share(share)
		if err != nil {
			return err
		}
		fi, err := sh.Stat(p)
		switch {
		case device.IsNotFound(err):
			code = codeAlreadyGone
			return nil
		case err != nil:
			return classify(err, c.rigID, c.name, displayPath(share, p))
		}
		if fi.IsDir || fi.Size != it.Size || !withinSlack(fi.Mtime, it.Mtime) {
			s.log.Info("devfiles: delete skipped: changed since the plan", "rig", rigID, "share", share,
				"op", c.op, "path", p, "dir", fi.IsDir, "size", fi.Size, "want_size", it.Size)
			code = codeChanged
			return nil
		}
		if err := sh.Delete(p); err != nil {
			if device.IsNotFound(err) {
				code = codeAlreadyGone
				return nil
			}
			if device.IsInUse(err) {
				s.log.Warn("devfiles: delete refused: in use", "rig", rigID, "share", share, "op", c.op, "path", p)
			}
			return classify(err, c.rigID, c.name, path.Base(p))
		}
		s.log.Info("devfiles: file deleted", "rig", rigID, "share", share, "op", c.op, "path", p, "size", it.Size)
		return nil
	})
	return code, err
}

// deleteDir deletes the folder p, one retry unit, only while it is still a
// folder. The device refuses a non-empty folder, and that refusal is the
// not_empty signal; only then is the folder listed, markers included, to tell
// a folder holding nothing but Commander's markers (kept_marker) apart. That
// listing guards nothing, so its failure reads not_empty.
func (s *Service) deleteDir(ctx context.Context, rigID, share, p string) (string, error) {
	var code string
	err := s.withRig(ctx, "Delete", share, rigID, func(c *session) error {
		code = ""
		if err := s.requireWritable(c, share); err != nil {
			return err
		}
		sh, err := c.share(share)
		if err != nil {
			return err
		}
		fi, err := sh.Stat(p)
		switch {
		case device.IsNotFound(err):
			code = codeAlreadyGone
			return nil
		case err != nil:
			return classify(err, c.rigID, c.name, displayPath(share, p))
		case !fi.IsDir:
			// smb1client's Remove would delete a file that took the folder's name.
			s.log.Info("devfiles: delete skipped: no longer a folder", "rig", rigID, "share", share,
				"op", c.op, "path", p)
			code = codeChanged
			return nil
		}
		err = sh.Delete(p)
		switch {
		case err == nil:
			s.log.Info("devfiles: folder deleted", "rig", rigID, "share", share, "op", c.op, "path", p)
			return nil
		case device.IsNotFound(err):
			code = codeAlreadyGone
			return nil
		case device.IsDirNotEmpty(err):
			code = s.notEmptyCode(c, sh, share, p)
			s.log.Info("devfiles: folder kept: not empty", "rig", rigID, "share", share, "op", c.op,
				"path", p, "code", code)
			return nil
		case device.IsInUse(err):
			s.log.Warn("devfiles: delete refused: in use", "rig", rigID, "share", share, "op", c.op, "path", p)
		}
		return classify(err, c.rigID, c.name, path.Base(p))
	})
	return code, err
}

// notEmptyCode words a refused folder delete: kept_marker when the folder
// holds only Commander's markers, otherwise (a listing failure included)
// not_empty.
func (s *Service) notEmptyCode(c *session, sh device.Share, share, p string) string {
	fis, err := sh.ListDir(p)
	if err != nil {
		c.markBroken(err)
		s.log.Debug("devfiles: listing a refused folder failed", "rig", c.rigID, "share", share,
			"op", c.op, "path", p, "error", err)
		return codeNotEmpty
	}
	if len(fis) == 0 {
		return codeNotEmpty
	}
	for _, fi := range fis {
		if !isMarker(fi.Name) {
			return codeNotEmpty
		}
	}
	return codeKeptMarker
}

// withinSlack reports whether the listed mtime t is within deleteMtimeSlack of
// the confirmed unix mtime.
func withinSlack(t time.Time, unix int64) bool {
	d := t.Sub(time.Unix(unix, 0))
	return d <= deleteMtimeSlack && d >= -deleteMtimeSlack
}

// deleteFailureCode maps a failed item's error onto its report code.
func deleteFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrInUse):
		return codeInUse
	case errors.Is(err, ErrDenied):
		return codeDenied
	case errors.Is(err, ErrUnreachable):
		return codeUnreachable
	case errors.Is(err, ErrIdentity):
		return codeIdentity
	}
	return codeError
}
