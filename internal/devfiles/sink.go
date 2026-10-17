package devfiles

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// The sink is the destination half of every device write: the plan's check of
// the destination folder, and the run's folders, temps and commit. It knows
// nothing of where the bytes come from; a caller names its operation and
// destination place and hands each item's bytes over through a source. Every
// write lands on a fresh exclusively created temp name committed through the
// device's never-replacing rename, so a destination is replaced only when the
// request's overwrite list names it and it still has the size and mtime the
// user saw (doc.go, rule 1).

const (
	// copyBufSize is the chunk the run reads and writes, and so how often it
	// checks for a cancel.
	copyBufSize = 512 << 10
	// sameMtimeSlack is how far a destination's mtime may sit from the one the
	// user saw and still count as unchanged: FAT keeps 2 s resolution.
	sameMtimeSlack = 2
	// cleanupBudget bounds the best-effort temp cleanup after a failed or
	// cancelled item, which runs past the request's own context.
	cleanupBudget = 10 * time.Second
	// tmpSuffix ends every temp name: the harvester's junk filter drops .tmp.
	tmpSuffix = ".commander.tmp"

	conflictFile = "file"
	conflictType = "type"

	ledgerArchived   = "archived"
	ledgerUnarchived = "unarchived"

	// reservedListMax is how many reserved-path offenders a plan refusal names
	// before it counts the rest.
	reservedListMax = 10
)

// sinkWords is how the sink's messages and log lines name the operation that
// writes, so an upload never reads as a copy.
type sinkWords struct {
	noun string // in log messages: "copy", "upload"
	done string // the outcome a note qualifies: "Copied", "Uploaded"
	// reserved words the refusal of a file at rel, relative to the
	// destination folder, that would stand where the reserved path where
	// (share/root) must go, with the operation's fix.
	reserved func(rel, where string) string
}

// destView is what the destination's refusals found that the plan classifies
// with: the destination rig's profile and the excluded extensions.
type destView struct {
	profile *model.RigProfile
	exclude []string
}

// destCheck runs the destination's refusals (host-bound, no such share) and
// returns what the plan classifies with.
func (s *Service) destCheck(ctx context.Context, op string, to CopyPlace) (destView, error) {
	var dv destView
	err := s.withRig(ctx, op, to.Share, to.Rig, func(c *session) error {
		if err := s.requireWritable(c, to.Share); err != nil {
			return err
		}
		shares, err := c.sess.ListShares()
		if err != nil {
			return classify(err, c.rigID, c.name, "The share list")
		}
		if !slices.Contains(shares, to.Share) {
			return newErr(ErrNoShare, c.rigID, "%s has no share named %s.", c.name, to.Share)
		}
		dv = destView{profile: c.profile, exclude: c.cfg.ExcludeExtensions}
		return nil
	})
	return dv, err
}

// destFolder is an existing destination folder as listed: its path in the
// device's own case, and its entries by case-folded name.
type destFolder struct {
	path  string
	names map[string]device.FileInfo
}

// foldIndex indexes a listing by case-folded name, as FAT matches names.
func foldIndex(fis []device.FileInfo) map[string]device.FileInfo {
	m := make(map[string]device.FileInfo, len(fis))
	for _, fi := range fis {
		m[strings.ToLower(fi.Name)] = fi
	}
	return m
}

// planDest checks pre-order entries, relative to to.Dir, against the
// destination and returns the plan, listing every folder it would write into
// once. It writes nothing. With mirror, to.Dir's own missing levels are
// counted rather than refused. A file that would stand at a reserved path
// refuses the whole plan before any device call (planReserved).
func (s *Service) planDest(ctx context.Context, op string, to CopyPlace, mirror bool,
	dv destView, entries []entry, words sinkWords) (*CopyPlan, error) {
	if err := s.planReserved(op, to, dv, entries, words); err != nil {
		return nil, err
	}
	missing, top, err := s.planDestRoot(ctx, op, to, mirror)
	if err != nil {
		return nil, err
	}

	plan := &CopyPlan{Dirs: []string{}, Items: []CopyPlanItem{}, Conflicts: []CopyConflict{}, ToDirMissing: missing}
	// lists holds the destination folder of every source folder that merges
	// into an existing one ("" is To.Dir); a folder absent from it is new, and
	// so is everything below it.
	lists := map[string]destFolder{}
	if top != nil {
		lists[""] = destFolder{path: to.Dir, names: top}
	}
	var excluded []string // source folders that meet a file: skipped whole
	for _, e := range entries {
		if !e.Dir {
			dst := joinRel(to.Dir, e.Rel)
			plan.Items = append(plan.Items, CopyPlanItem{
				Rel: e.Rel, Size: e.Size,
				Watched: rig.WatchedDir(dv.profile, to.Share, relDir(dst)),
			})
			plan.Files++
			plan.Bytes += e.Size
			switch {
			case rig.HarvestCandidate(dv.profile, dv.exclude, to.Share, dst):
				plan.Watched.Frames++
			case rig.HarvestPreview(dv.profile, dv.exclude, to.Share, dst):
				plan.Watched.Previews++
			}
		}
		if underAny(e.Rel, excluded) != "" {
			continue
		}
		parent, ok := lists[relDir(e.Rel)]
		if !ok {
			// The parent is created by the run, so this entry is new too.
			if e.Dir {
				plan.Dirs = append(plan.Dirs, e.Rel)
			}
			continue
		}
		dst, found := parent.names[strings.ToLower(path.Base(e.Rel))]
		dstPath := joinRel(parent.path, dst.Name)
		switch {
		case !found:
			if e.Dir {
				plan.Dirs = append(plan.Dirs, e.Rel)
			}
		case e.Dir && dst.IsDir:
			fis, err := s.listDir(ctx, op, to.Rig, to.Share, dstPath)
			switch {
			case errors.Is(err, ErrNotFound):
				// Gone since its parent was listed: the run creates it.
				plan.Dirs = append(plan.Dirs, e.Rel)
			case err != nil:
				return nil, err
			default:
				lists[e.Rel] = destFolder{path: dstPath, names: foldIndex(fis)}
			}
		case e.Dir != dst.IsDir:
			plan.Conflicts = append(plan.Conflicts, CopyConflict{
				Rel: e.Rel, Kind: conflictType,
				SrcSize: e.Size, SrcMtime: unixOrZero(e.Mtime),
				DstSize: dst.Size, DstMtime: unixOrZero(dst.Mtime),
			})
			if e.Dir {
				excluded = append(excluded, e.Rel)
			}
		default:
			c := CopyConflict{
				Rel: e.Rel, Kind: conflictFile,
				SrcSize: e.Size, SrcMtime: unixOrZero(e.Mtime),
				DstSize: dst.Size, DstMtime: unixOrZero(dst.Mtime),
			}
			if c.DstLedger, err = s.dstLedger(ctx, dv.profile, dv.exclude, to, dstPath, dst); err != nil {
				return nil, err
			}
			plan.Conflicts = append(plan.Conflicts, c)
		}
	}
	plan.Watched.KeepsOriginals = dv.profile.KeepOriginals

	if err := s.planLayoutGuard(ctx, op, to, plan, mirror); err != nil {
		return nil, err
	}
	return plan, nil
}

// planReserved refuses a plan any of whose files would stand where a reserved
// path must go (rig.ReservedPath): a watched root of the destination share,
// configured enabled or disabled, in either layout, or a removable share's
// ASIAIR folder. Such a file fails the share's listing, so nothing on it is
// archived, and blocks the firmware's own mkdir. It reads the configuration
// alone, never the store, and lists every offender up to reservedListMax.
func (s *Service) planReserved(op string, to CopyPlace, dv destView, entries []entry, words sinkWords) error {
	var msgs, paths []string
	for _, e := range entries {
		if e.Dir {
			continue
		}
		dst := joinRel(to.Dir, e.Rel)
		root, ok := rig.ReservedPath(dv.profile.Sources, to.Share, dst)
		if !ok {
			continue
		}
		paths = append(paths, dst)
		if len(msgs) < reservedListMax {
			msgs = append(msgs, words.reserved(e.Rel, displayPath(to.Share, root)))
		}
	}
	if len(paths) == 0 {
		return nil
	}
	s.log.Warn("devfiles: reserved path refused", "rig", to.Rig, "share", to.Share, "op", op,
		"paths", paths)
	if n := len(paths) - len(msgs); n > 0 {
		msgs = append(msgs, fmt.Sprintf("And %d more like these.", n))
	}
	return newErr(ErrWatched, to.Rig, "%s", strings.Join(msgs, " "))
}

// reservedItem is the write-time re-check of planReserved for one file at
// to.Dir/rel, against the live configuration: reachable only when the
// configuration changed since the plan. ok reports a refusal, which fails the
// item and lets the run go on.
func (k *sinkRun) reservedItem(rel string) (itemResult, bool) {
	to := k.to
	profile := k.s.cfg.Load().Rig(to.Rig)
	if profile == nil {
		// Not configured any more: the session checkout refuses the rig.
		return itemResult{}, false
	}
	dst := joinRel(to.Dir, rel)
	root, ok := rig.ReservedPath(profile.Sources, to.Share, dst)
	if !ok {
		return itemResult{}, false
	}
	k.s.log.Warn("devfiles: reserved path refused", "rig", to.Rig, "share", to.Share, "op", k.op,
		"paths", []string{dst})
	return itemResult{outcome: outcomeFailed, code: codeWatched,
		msg: k.words.reserved(rel, displayPath(to.Share, root))}, true
}

// planDestRoot lists To.Dir on the destination. For a mirror it checks To.Dir's
// levels from the share root instead: missing counts the levels that do not
// exist (top is then nil), and a file standing where one must go refuses the
// plan with ErrTypeConflict. Elsewhere a missing To.Dir is ErrNotFound.
func (s *Service) planDestRoot(ctx context.Context, op string, to CopyPlace, mirror bool) (missing int, top map[string]device.FileInfo, err error) {
	if !mirror || to.Dir == "" {
		fis, err := s.listDir(ctx, op, to.Rig, to.Share, to.Dir)
		if err != nil {
			return 0, nil, err
		}
		return 0, foldIndex(fis), nil
	}
	segs := strings.Split(to.Dir, "/")
	cur := ""
	for i, seg := range segs {
		fis, err := s.listDir(ctx, op, to.Rig, to.Share, cur)
		if err != nil {
			return 0, nil, err
		}
		fi, ok := foldIndex(fis)[strings.ToLower(seg)]
		if !ok {
			return len(segs) - i, nil, nil
		}
		cur = joinRel(cur, seg)
		if !fi.IsDir {
			return 0, nil, s.typeConflictErr(to.Rig, to.Share, cur)
		}
	}
	fis, err := s.listDir(ctx, op, to.Rig, to.Share, cur)
	if err != nil {
		return 0, nil, err
	}
	return 0, foldIndex(fis), nil
}

func (s *Service) typeConflictErr(rigID, share, p string) error {
	return newErr(ErrTypeConflict, rigID, "%s on %s is a file, so the folder cannot be created.",
		displayPath(share, p), s.nameOf(rigID))
}

// dstLedger is a file conflict's dst_ledger: set only when the destination p
// is a frame the destination harvester tracks, from its frame row and its NAS
// copy (provenArchived).
func (s *Service) dstLedger(ctx context.Context, profile *model.RigProfile, exclude []string,
	to CopyPlace, p string, dst device.FileInfo) (string, error) {
	if !rig.HarvestCandidate(profile, exclude, to.Share, p) {
		return "", nil
	}
	key, _ := rig.HarvestStoreKey(profile, to.Share, p)
	row, ok, err := s.store.GetFrame(ctx, to.Rig, to.Share, key)
	if err != nil {
		return "", fmt.Errorf("devfiles: frame lookup %s/%s: %w", to.Share, key, err)
	}
	if ok && s.provenArchived(row, dst.Size, dst.Mtime) {
		return ledgerArchived, nil
	}
	return ledgerUnarchived, nil
}

// planLayoutGuard runs the layout guard on every folder the plan creates at
// the destination share's root: a top-level planned folder when To.Dir is the
// root, or To.Dir's first level when a mirror creates it.
func (s *Service) planLayoutGuard(ctx context.Context, op string, to CopyPlace, plan *CopyPlan, mirror bool) error {
	var names []string
	if to.Dir == "" {
		for _, d := range plan.Dirs {
			if !strings.Contains(d, "/") {
				names = append(names, d)
			}
		}
	} else if mirror && plan.ToDirMissing == strings.Count(to.Dir, "/")+1 {
		names = append(names, strings.SplitN(to.Dir, "/", 2)[0])
	}
	if len(names) == 0 {
		return nil
	}
	return s.withRig(ctx, op, to.Share, to.Rig, func(c *session) error {
		sh, err := c.share(to.Share)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := s.layoutGuard(c, sh, to.Share, "", n); err != nil {
				return err
			}
		}
		return nil
	})
}

// sinkRun is the destination state of one write run, used on the request's
// goroutine only: the operation its log and audit lines name, the place it
// writes into, the overwrites the user bound, and the folders already made.
type sinkRun struct {
	s         *Service
	ctx       context.Context
	op        string
	words     sinkWords
	to        CopyPlace
	overwrite map[string]CopyOverwrite // by item path relative to to.Dir

	// ensured caches ensureDir by case-folded destination path.
	ensured map[string]bool
}

// itemResult is one item's outcome: a copied item with no code needs no line.
// abort is the SummaryLine.Aborted value of a failure that ends a run (after
// every attempt, or a device Commander can't trust); abortRig is the rig whose
// side failed.
type itemResult struct {
	outcome, code, msg string
	dst                *device.FileInfo
	abort              string
	abortRig           string
}

// source is everything the write needs from where the bytes come from.
type source struct {
	rigs []string // withRetry order; the destination rig is last
	// open is called once per attempt on that attempt's sessions and returns a
	// reader positioned at byte 0 and the mtime to set, or a skip outcome. Its
	// error is returned to withRetry as it is, so it carries its own rig.
	open func(ss []*session) (rd io.ReadCloser, mtime time.Time, skip *itemResult, err error)
	// zeroMtimeIsNone makes a zero mtime from open mean no time to set, so
	// Chtimes is skipped; otherwise a zero mtime is set like any other.
	zeroMtimeIsNone bool
	// readFailure maps an error reading the opened reader, attributing it to
	// the source's side.
	readFailure func(err error) error
	progress    func(n int64) error // the throttled progress line, with n bytes of the item read
	logAttrs    []any               // appended to "file written" after the destination path
}

// ensureDir makes the folder name inside parent unless it is already a folder.
// It stats first, so an existing folder is neither recreated nor put to the
// layout guard; a missing one passes the guard, then Mkdir. A file in the way
// is ErrTypeConflict (smb1client's MkdirAll would swallow that collision).
func (k *sinkRun) ensureDir(c *session, sh device.Share, parent, name string) error {
	share := k.to.Share
	p := joinRel(parent, name)
	key := strings.ToLower(p)
	if k.ensured[key] {
		return nil
	}
	isDir := func(fi device.FileInfo, err error) (bool, error) {
		switch {
		case err == nil && fi.IsDir:
			return true, nil
		case err == nil:
			return false, k.s.typeConflictErr(c.rigID, share, p)
		case device.IsNotFound(err):
			return false, nil
		}
		return false, classify(err, c.rigID, c.name, displayPath(share, p))
	}
	ok, err := isDir(sh.Stat(p))
	if err != nil {
		return err
	}
	if !ok {
		if err := k.s.layoutGuard(c, sh, share, parent, name); err != nil {
			return err
		}
		if err := sh.Mkdir(p); err != nil {
			if !device.IsExist(err) {
				return classify(err, c.rigID, c.name, displayPath(share, p))
			}
			// Made since the stat, by another writer or by a reply this run lost.
			if ok, err = isDir(sh.Stat(p)); err != nil {
				return err
			}
			if !ok {
				return k.s.typeConflictErr(c.rigID, share, p)
			}
		} else {
			k.s.log.Info("devfiles: folder created", "rig", c.rigID, "share", share, "op", k.op, "path", p)
		}
	}
	k.ensured[key] = true
	return nil
}

// failedOutcome is skipped for a type conflict and failed otherwise.
func failedOutcome(err error) string {
	if errors.Is(err, ErrTypeConflict) {
		return outcomeSkipped
	}
	return outcomeFailed
}

// copyReason maps an item's final error onto its report code.
func copyReason(err error) string {
	switch {
	case errors.Is(err, ErrUnreachable):
		return codeUnreachable
	case errors.Is(err, ErrIdentity):
		return codeIdentity
	case errors.Is(err, ErrInUse):
		return codeInUse
	case errors.Is(err, ErrDenied):
		return codeDenied
	case errors.Is(err, ErrTypeConflict):
		return codeTypeConflict
	}
	return codeError
}

// readErr marks a failure on the source side of a stream.
type readErr struct{ err error }

func (e *readErr) Error() string { return e.err.Error() }
func (e *readErr) Unwrap() error { return e.err }

// errStopCopy ends a stream whose request was cancelled.
var errStopCopy = errors.New("devfiles: copy stopped")

// chunkReader checks the request context before every chunk, counts the
// bytes read and reports them to progress.
type chunkReader struct {
	ctx      context.Context
	rd       io.Reader
	progress func(n int64) error
	n        int64
	emitErr  error
}

func (cr *chunkReader) Read(p []byte) (int, error) {
	if cr.ctx.Err() != nil {
		return 0, errStopCopy
	}
	n, err := cr.rd.Read(p)
	cr.n += int64(n)
	if n > 0 {
		if eerr := cr.progress(cr.n); eerr != nil {
			cr.emitErr = eerr
			return n, eerr
		}
	}
	if err != nil && err != io.EOF {
		err = &readErr{err}
	}
	return n, err
}

// writeItem writes one file at to.Dir/rel: it re-checks the reserved-path
// rule, then within withRetry over src.rigs it drops earlier attempts' temps,
// opens the source, CreateNew's a fresh temp, streams in copyBufSize chunks
// checking ctx per chunk, re-checks ctx, commits (overwrite binding,
// sameAsSeen, never-replacing Rename) and sets the mtime best-effort, unless
// the source has none; then removes any temps left, on a fresh session. Its
// error is non-nil only when the run must end at once: a cancel or a failed
// progress emit.
func (k *sinkRun) writeItem(rel string, src source) (itemResult, error) {
	s, to := k.s, k.to
	ow, isOverwrite := k.overwrite[rel]
	dst := joinRel(to.Dir, rel)
	if res, refused := k.reservedItem(rel); refused {
		return res, nil
	}

	var temps []string // every temp name this item created and has not removed
	var res itemResult
	var emitErr error
	err := s.withRetry(k.ctx, k.op, to.Share, src.rigs, func(ss []*session) error {
		// The destination is the last session: a one-rig run has only it.
		dc := ss[len(ss)-1]
		res = itemResult{}
		if err := s.requireWritable(dc, to.Share); err != nil {
			return onRig(dc.rigID, err)
		}
		dsh, err := dc.share(to.Share)
		if err != nil {
			return onRig(dc.rigID, err)
		}
		if temps, err = k.dropTemps(dc, dsh, temps); err != nil {
			return onRig(dc.rigID, err)
		}

		rd, mtime, skip, err := src.open(ss)
		if err != nil {
			return err
		}
		if skip != nil {
			res = *skip
			return nil
		}
		defer rd.Close()

		tmp := dst + "." + randomHex() + tmpSuffix
		w, err := dsh.CreateNew(tmp)
		if err != nil {
			return onRig(dc.rigID, classify(err, dc.rigID, dc.name, displayPath(to.Share, tmp)))
		}
		temps = append(temps, tmp)
		cr := &chunkReader{ctx: k.ctx, rd: rd, progress: src.progress}
		_, cerr := io.CopyBuffer(w, cr, make([]byte, copyBufSize))
		werr := w.Close()
		var re *readErr
		switch {
		case cr.emitErr != nil:
			emitErr = cr.emitErr
			return emitErr
		case errors.Is(cerr, errStopCopy):
			return cancelled(k.ctx)
		case errors.As(cerr, &re):
			return src.readFailure(re.err)
		case cerr != nil:
			return onRig(dc.rigID, classify(cerr, dc.rigID, dc.name, displayPath(to.Share, tmp)))
		case werr != nil:
			return onRig(dc.rigID, classify(werr, dc.rigID, dc.name, displayPath(to.Share, tmp)))
		}
		// A cancel that landed on the last read leaves a complete temp; committing
		// it would still rename, and for an overwrite delete the old file, after
		// the user stopped. The temp is dropped by the cleanup below.
		if k.ctx.Err() != nil {
			return cancelled(k.ctx)
		}

		var committed bool
		res, committed, err = k.commit(dc, dsh, tmp, dst, ow, isOverwrite)
		if err != nil {
			return onRig(dc.rigID, err)
		}
		if !committed {
			if k.dropTemp(dc, dsh, tmp) {
				temps = slices.DeleteFunc(temps, func(t string) bool { return t == tmp })
			}
			return nil
		}
		temps = slices.DeleteFunc(temps, func(t string) bool { return t == tmp })
		attrs := append([]any{"rig", dc.rigID, "share", to.Share, "op", k.op, "path", dst}, src.logAttrs...)
		s.log.Info("devfiles: file written", append(attrs, "bytes", cr.n, "overwrite", isOverwrite)...)
		res = itemResult{outcome: outcomeCopied}
		// An upload whose browser gave no lastModified has no time to set; FAT
		// cannot hold the epoch, so the device keeps the write time.
		if src.zeroMtimeIsNone && mtime.IsZero() {
			return nil
		}
		// Best-effort and outside the unit of retry: the file is committed, so a
		// retry would recopy it for a cosmetic property.
		if k.ctx.Err() != nil {
			res.code = codeMtimeNotPreserved
			return nil
		}
		if err := dsh.Chtimes(dst, mtime, mtime); err != nil {
			dc.markBroken(err)
			s.log.Warn("devfiles: "+k.words.noun+" mtime not preserved", "rig", dc.rigID, "share", to.Share, "op", k.op,
				"path", dst, "error", err)
			res.code = codeMtimeNotPreserved
			res.msg = fmt.Sprintf("%s; %s did not accept the timestamp, so it shows the %s time.",
				k.words.done, dc.name, k.words.noun)
		}
		return nil
	})
	if len(temps) > 0 {
		k.cleanup(temps)
	}
	switch {
	case emitErr != nil:
		return res, emitErr
	case errors.Is(err, ErrCancelled):
		return res, err
	case err != nil:
		res = itemResult{outcome: failedOutcome(err), code: copyReason(err), msg: err.Error()}
		if res.abort = runAbort(err); res.abort != "" {
			var e *Error
			if errors.As(err, &e) {
				res.abortRig = e.Rig
			}
		}
		return res, nil
	}
	return res, nil
}

// commit moves tmp onto dst. With an overwrite it first removes dst, only while
// dst is still the file the user saw. committed is false for a skip or an
// item failure, and the caller then deletes tmp. A device error that is not an
// outcome is returned for withRetry.
func (k *sinkRun) commit(c *session, sh device.Share, tmp, dst string, ow CopyOverwrite, isOverwrite bool) (res itemResult, committed bool, err error) {
	share := k.to.Share
	where := displayPath(share, dst)
	removedOld := false
	if isOverwrite {
		fi, err := sh.Stat(dst)
		switch {
		case err == nil && fi.IsDir:
			return itemResult{outcome: outcomeSkipped, code: codeTypeConflict,
				msg: fmt.Sprintf("A folder now stands at %s on %s, so it was not overwritten.", where, c.name)}, false, nil
		case err == nil && !sameAsSeen(fi, ow):
			return itemResult{outcome: outcomeSkipped, code: codeChanged, dst: &fi,
				msg: fmt.Sprintf("%s changed on %s since you confirmed, so it was not overwritten.", where, c.name)}, false, nil
		case err == nil:
			if err := sh.Delete(dst); err != nil && !device.IsNotFound(err) {
				if device.IsInUse(err) {
					k.s.log.Warn("devfiles: overwrite refused: in use", "rig", c.rigID, "share", share, "op", k.op, "path", dst)
					return itemResult{outcome: outcomeFailed, code: codeInUse,
						msg: fmt.Sprintf("%s is in use on %s, so it was not overwritten.", where, c.name)}, false, nil
				}
				return itemResult{}, false, classify(err, c.rigID, c.name, where)
			}
			removedOld = true
			k.s.log.Info("devfiles: file removed for overwrite", "rig", c.rigID, "share", share, "op", k.op,
				"path", dst, "size", fi.Size, "mtime", fi.Mtime)
		case !device.IsNotFound(err):
			return itemResult{}, false, classify(err, c.rigID, c.name, where)
		}
	}

	err = sh.Rename(tmp, dst)
	switch {
	case err == nil:
		return itemResult{}, true, nil
	case device.IsExist(err):
		// The never-replacing rename is the conflict check: a destination that
		// appeared after the plan, or after the overwrite's delete, stays.
		res := itemResult{outcome: outcomeSkipped, code: codeAppeared,
			msg: fmt.Sprintf("%s appeared on %s after the check, so it was not overwritten.", where, c.name)}
		if fi, serr := sh.Stat(dst); serr == nil {
			if fi.IsDir {
				res = itemResult{outcome: outcomeSkipped, code: codeTypeConflict,
					msg: fmt.Sprintf("A folder now stands at %s on %s.", where, c.name)}
			} else {
				res.dst = &fi
			}
		}
		k.s.log.Info("devfiles: "+k.words.noun+" skipped: destination appeared", "rig", c.rigID, "share", share, "op", k.op, "path", dst)
		return res, false, nil
	case device.IsInUse(err):
		k.s.log.Warn("devfiles: commit refused: in use", "rig", c.rigID, "share", share, "op", k.op, "path", dst)
		msg := fmt.Sprintf("%s is in use on %s.", where, c.name)
		if removedOld {
			msg += " The old file was removed; retry to finish."
		}
		return itemResult{outcome: outcomeFailed, code: codeInUse, msg: msg}, false, nil
	case removedOld && !device.IsNetwork(err):
		cerr := classify(err, c.rigID, c.name, where)
		return itemResult{outcome: outcomeFailed, code: copyReason(cerr),
			msg: cerr.Error() + " The old file was removed; retry to finish."}, false, nil
	}
	return itemResult{}, false, classify(err, c.rigID, c.name, where)
}

// sameAsSeen reports whether fi is still the destination the user saw.
func sameAsSeen(fi device.FileInfo, ow CopyOverwrite) bool {
	d := fi.Mtime.Unix() - ow.DstMtime
	return fi.Size == ow.DstSize && d >= -sameMtimeSlack && d <= sameMtimeSlack
}

// dropTemp deletes tmp after an uncommitted outcome and reports whether it is
// gone. A failure leaves it for the item's final cleanup, and a network failure
// marks the session broken.
func (k *sinkRun) dropTemp(c *session, sh device.Share, tmp string) bool {
	if err := sh.Delete(tmp); err != nil && !device.IsNotFound(err) {
		c.markBroken(err)
		k.s.log.Warn("devfiles: temp cleanup failed", "rig", c.rigID, "share", k.to.Share, "op", k.op,
			"path", tmp, "error", err)
		return false
	}
	return true
}

// dropTemps deletes an item's earlier attempts' temps on a retry's fresh
// session and returns those it could not remove. A network failure is
// returned so the attempt is retried.
func (k *sinkRun) dropTemps(c *session, sh device.Share, temps []string) ([]string, error) {
	var left []string
	for _, t := range temps {
		err := sh.Delete(t)
		switch {
		case err == nil || device.IsNotFound(err):
		case device.IsNetwork(err):
			return temps, err
		default:
			k.s.log.Warn("devfiles: temp cleanup failed", "rig", c.rigID, "share", k.to.Share, "op", k.op,
				"path", t, "error", err)
			left = append(left, t)
		}
	}
	return left, nil
}

// cleanup deletes an item's remaining temps, best-effort, on a fresh session
// with its own budget: the request's context may be the thing that ended. The
// failed session is already checked in, so its slot is free. A temp left
// behind is junk to the harvester and visible in the pane; it is only logged.
func (k *sinkRun) cleanup(temps []string) {
	to := k.to
	ctx, cancel := context.WithTimeout(context.WithoutCancel(k.ctx), cleanupBudget)
	defer cancel()
	c, err := k.s.acquire(ctx, to.Rig, k.op)
	if err != nil {
		k.s.log.Warn("devfiles: temp cleanup failed", "rig", to.Rig, "share", to.Share, "op", k.op,
			"temps", temps, "error", err)
		return
	}
	var last error
	sh, err := c.share(to.Share)
	if err != nil {
		last = err
	} else {
		for _, t := range temps {
			if err := sh.Delete(t); err != nil && !device.IsNotFound(err) {
				last = err
				k.s.log.Warn("devfiles: temp cleanup failed", "rig", to.Rig, "share", to.Share, "op", k.op,
					"path", t, "error", err)
				if device.IsNetwork(err) {
					break
				}
			}
		}
	}
	if last != nil && !device.IsNetwork(last) {
		last = nil
	}
	k.s.release(c, last)
}

// randomHex is 16 random hex characters: a 64-bit temp suffix that does not
// collide in practice, and an exclusive create if it ever did.
func randomHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails; it crashes the program instead
	return hex.EncodeToString(b[:])
}
