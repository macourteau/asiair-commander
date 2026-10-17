package devfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/rig"
)

// Copy: a plan walks the selection and checks the destination, writing
// nothing; a run copies the echoed items, each to a fresh exclusively created
// temp name committed through the device's never-replacing rename, so a
// destination is replaced only when the request's overwrite list names it and
// it still has the size and mtime the user saw (doc.go, rule 1).

const (
	opCopyCheck = "Copy check"
	opCopy      = "Copy"

	// maxRunBody bounds a plan's run request, well under the web layer's 4 MiB
	// decodeJSON cap.
	maxRunBody = 3 << 20
	// progressEvery is the most often a progress line is emitted within a file.
	progressEvery = time.Second
)

// CopyPlace is one side of a copy: a rig, a share and a share-relative folder.
type CopyPlace struct {
	Rig   string `json:"rig"`
	Share string `json:"share"`
	Dir   string `json:"dir"`
}

// CopySource is the plan's source: a place and the names selected in it.
type CopySource struct {
	CopyPlace
	Names []string `json:"names"`
}

// CopyPlanRequest asks for a copy plan. With Mirror, To's share and folder
// equal From's on another rig, and the plan counts the missing levels of the
// destination folder itself.
type CopyPlanRequest struct {
	From   CopySource `json:"from"`
	To     CopyPlace  `json:"to"`
	Mirror bool       `json:"mirror"`
}

// CopyPlan is what a copy would do. Items are every walked file, relative to
// From.Dir, type-conflicted subtrees included (the client drops them); Files
// and Bytes count them. Dirs are the folders the run creates inside To.Dir,
// parents first; ToDirMissing counts the levels of To.Dir itself that do not
// exist yet. Slices are never nil.
type CopyPlan struct {
	Files        int            `json:"files"`
	Bytes        int64          `json:"bytes"`
	Dirs         []string       `json:"dirs"`
	Items        []CopyPlanItem `json:"items"`
	Conflicts    []CopyConflict `json:"conflicts"`
	ToDirMissing int            `json:"to_dir_missing"`
	Watched      CopyWatched    `json:"watched"`
}

// CopyPlanItem is one planned file. Watched is whether its destination folder
// is one the destination harvester watches.
type CopyPlanItem struct {
	Rel     string `json:"rel"`
	Size    int64  `json:"size"`
	Watched bool   `json:"watched"`
}

// CopyConflict is a planned entry whose destination name is taken. Kind is
// "file" (a file over a file: overwrite or skip) or "type" (a file against a
// folder: skip only). DstLedger is "" unless the destination is a frame the
// destination harvester tracks, then "archived" or "unarchived". Mtimes are
// unix seconds in each device's clock.
type CopyConflict struct {
	Rel       string `json:"rel"`
	Kind      string `json:"kind"`
	SrcSize   int64  `json:"src_size"`
	SrcMtime  int64  `json:"src_mtime"`
	DstSize   int64  `json:"dst_size"`
	DstMtime  int64  `json:"dst_mtime"`
	DstLedger string `json:"dst_ledger"`
}

// CopyWatched counts the planned files the destination harvester will act on:
// frames it archives and previews it removes unarchived.
type CopyWatched struct {
	Frames         int  `json:"frames"`
	Previews       int  `json:"previews"`
	KeepsOriginals bool `json:"keeps_originals"`
}

// CopyRequest is a copy run: the items and folders the user confirmed,
// relative to From.Dir and To.Dir, and the overwrites the user chose, each
// bound to the destination's size and mtime as the user saw them.
type CopyRequest struct {
	From      CopyPlace       `json:"from"`
	To        CopyPlace       `json:"to"`
	Dirs      []string        `json:"dirs"`
	Items     []CopyItem      `json:"items"`
	Overwrite []CopyOverwrite `json:"overwrite"`
}

// CopyItem is one file to copy; Size feeds the progress totals only.
type CopyItem struct {
	Rel  string `json:"rel"`
	Size int64  `json:"size"`
}

// CopyOverwrite permits replacing the destination of Rel while it still has
// DstSize bytes and an mtime within 2 s of DstMtime (unix seconds).
type CopyOverwrite struct {
	Rel      string `json:"rel"`
	DstSize  int64  `json:"dst_size"`
	DstMtime int64  `json:"dst_mtime"`
}

// checkCopyPlaces validates both places and the selection's top-level names,
// and refuses a copy onto its own folder or into a selected folder.
func (s *Service) checkCopyPlaces(from, to CopyPlace, tops []string) error {
	cfg := s.cfg.Load()
	for _, p := range []CopyPlace{from, to} {
		if _, err := managedProfile(cfg, p.Rig); err != nil {
			return err
		}
		if err := checkShare(p.Share); err != nil {
			return err
		}
		if err := checkDir(p.Dir); err != nil {
			return err
		}
	}
	// Samba resolves share names in any case, and FAT names are
	// case-insensitive, so the comparisons are too.
	if from.Rig != to.Rig || !strings.EqualFold(from.Share, to.Share) {
		return nil
	}
	if strings.EqualFold(from.Dir, to.Dir) {
		return newErr(ErrSameFolder, to.Rig, "The source and destination are the same folder.")
	}
	for _, n := range tops {
		if sameOrInside(to.Dir, joinRel(from.Dir, n)) {
			return newErr(ErrIntoItself, to.Rig, "%s cannot be copied into itself.", n)
		}
	}
	return nil
}

// sameOrInside reports whether p is dir or lies below it, ignoring case.
func sameOrInside(p, dir string) bool {
	if len(p) < len(dir) || !strings.EqualFold(p[:len(dir)], dir) {
		return false
	}
	return len(p) == len(dir) || p[len(dir)] == '/'
}

// underAny returns the first of dirs that rel lies strictly below, or "".
func underAny(rel string, dirs []string) string {
	for _, d := range dirs {
		if len(rel) > len(d) && sameOrInside(rel, d) {
			return d
		}
	}
	return ""
}

// relDir is rel's folder, "" at the top.
func relDir(rel string) string {
	if d := path.Dir(rel); d != "." {
		return d
	}
	return ""
}

// copyWords names a copy from fromRig in the sink's messages: a file standing
// where a reserved path must go is left out, or renamed on the source rig.
func (s *Service) copyWords(fromRig string) sinkWords {
	src := s.nameOf(fromRig)
	return sinkWords{noun: "copy", done: "Copied", reserved: func(rel, where string) string {
		return fmt.Sprintf("%s would stand where %s must go. Leave %s out of the selection, or rename it on %s first.",
			rel, where, rel, src)
	}}
}

// copyRigs is the rig list for the operation lock: one entry for a same-rig copy.
func copyRigs(from, to string) []string {
	if from == to {
		return []string{from}
	}
	return []string{from, to}
}

// CopyPlan walks the selection on the source rig and checks every folder it
// would write into on the destination rig, listing each once. It writes
// nothing, and runs under the operation lock.
func (s *Service) CopyPlan(ctx context.Context, req CopyPlanRequest) (*CopyPlan, error) {
	from, to := req.From.CopyPlace, req.To
	if req.Mirror && (to.Share != from.Share || to.Dir != from.Dir) {
		return nil, newErr(ErrBadPath, to.Rig, "a copy to the same location keeps the source's share and folder")
	}
	if err := s.checkCopyPlaces(from, to, req.From.Names); err != nil {
		return nil, err
	}
	unlock, err := s.lockOp(ctx, opCopyCheck, copyRigs(from.Rig, to.Rig)...)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// The destination's refusals first: they are cheaper than the walk.
	dv, err := s.destCheck(ctx, opCopyCheck, to)
	if err != nil {
		return nil, err
	}

	entries, err := s.walk(ctx, opCopyCheck, from.Rig, from.Share, from.Dir, req.From.Names)
	if err != nil {
		return nil, err
	}
	plan, err := s.planDest(ctx, opCopyCheck, to, req.Mirror, dv, entries, s.copyWords(from.Rig))
	if err != nil {
		return nil, err
	}
	if n := worstRunBody(from, to, plan); n > maxRunBody {
		return nil, newErr(ErrTooMany, from.Rig,
			"This selection is too large to copy in one go (its file list exceeds %d MiB). Select fewer files or a subfolder.",
			maxRunBody>>20)
	}
	s.log.Debug("devfiles: copy plan", "rig", from.Rig, "share", from.Share, "op", opCopyCheck,
		"to_rig", to.Rig, "to_share", to.Share, "to_dir", to.Dir, "files", plan.Files, "bytes", plan.Bytes,
		"dirs", len(plan.Dirs), "to_dir_missing", plan.ToDirMissing, "conflicts", len(plan.Conflicts),
		"watched_frames", plan.Watched.Frames, "watched_previews", plan.Watched.Previews)
	return plan, nil
}

// worstRunBody is the encoded size of the run request this plan can produce
// at most: every item sent and every file conflict overwritten.
func worstRunBody(from, to CopyPlace, plan *CopyPlan) int {
	req := CopyRequest{From: from, To: to, Dirs: plan.Dirs, Items: make([]CopyItem, len(plan.Items))}
	for i, it := range plan.Items {
		req.Items[i] = CopyItem{Rel: it.Rel, Size: it.Size}
	}
	for _, c := range plan.Conflicts {
		if c.Kind == conflictFile {
			req.Overwrite = append(req.Overwrite, CopyOverwrite{Rel: c.Rel, DstSize: c.DstSize, DstMtime: c.DstMtime})
		}
	}
	b, err := json.Marshal(req)
	if err != nil {
		// Strings and integers always encode; refuse rather than guess.
		return maxRunBody + 1
	}
	return len(b)
}

// copyRun is one copy run's state, used on the request's goroutine only. Its
// sinkRun is the destination half: the folders, temps and commits.
type copyRun struct {
	*sinkRun
	req  CopyRequest
	emit Emit
	rigs []string // the withRetry order: source, destination

	// failedDirs are the run's folders that could not be made, each failing
	// its subtree.
	failedDirs []failedDir

	total      int
	bytesTotal int64
	bytesDone  int64
	done       int
	skipped    int
	failed     int
	aborted    string
	abortedBy  string // the rig whose failure ended the run
	current    string
	lastEmit   time.Time

	stoppedAt atomic.Int64 // unix nanos of the cancel, for the stop log line
}

type failedDir struct {
	rel string
	err error
}

// Copy runs a copy: it validates the echoed request, takes the operation
// lock, makes To.Dir and the confirmed folders, then copies each item, one
// item being the unit of retry, and reports through emit. An error returned
// before the first emit is an ordinary refusal; after a cancel or a failed
// emit it returns that error and emits nothing more. The first unreachable
// rig ends the run: the remaining items are not attempted.
func (s *Service) Copy(ctx context.Context, req CopyRequest, emit Emit) error {
	overwrite, err := s.checkCopyRequest(req)
	if err != nil {
		return err
	}
	unlock, err := s.lockOp(ctx, opCopy, copyRigs(req.From.Rig, req.To.Rig)...)
	if err != nil {
		return err
	}
	defer unlock()

	r := &copyRun{
		sinkRun: &sinkRun{
			s: s, ctx: ctx, op: opCopy, words: s.copyWords(req.From.Rig), to: req.To,
			overwrite: overwrite,
			ensured:   map[string]bool{},
		},
		req: req,
		// Nothing follows a cancel: the client has aborted and builds its own report.
		emit: func(line any) error {
			if ctx.Err() != nil {
				return cancelled(ctx)
			}
			return emit(line)
		},
		rigs:  []string{req.From.Rig, req.To.Rig},
		total: len(req.Items),
	}
	for _, it := range req.Items {
		r.bytesTotal += it.Size
	}
	stop := context.AfterFunc(ctx, func() { r.stoppedAt.Store(s.now().UnixNano()) })
	defer stop()
	return r.run()
}

// checkCopyRequest validates every echoed path, the overwrite list against
// the items, and the size cap, before any device call.
func (s *Service) checkCopyRequest(req CopyRequest) (map[string]CopyOverwrite, error) {
	if len(req.Items) > s.maxFiles {
		return nil, newErr(ErrTooMany, req.From.Rig,
			"This selection has more than %d files. Select fewer files or a subfolder.", s.maxFiles)
	}
	var tops []string
	seen := map[string]bool{}
	for _, it := range req.Items {
		if err := checkRel(it.Rel); err != nil {
			return nil, err
		}
		if seen[it.Rel] {
			return nil, newErr(ErrBadPath, req.From.Rig, "%q is listed twice", it.Rel)
		}
		seen[it.Rel] = true
		tops = append(tops, strings.SplitN(it.Rel, "/", 2)[0])
	}
	for _, d := range req.Dirs {
		if err := checkRel(d); err != nil {
			return nil, err
		}
		tops = append(tops, strings.SplitN(d, "/", 2)[0])
	}
	overwrite := map[string]CopyOverwrite{}
	for _, o := range req.Overwrite {
		if err := checkRel(o.Rel); err != nil {
			return nil, err
		}
		if !seen[o.Rel] {
			return nil, newErr(ErrBadPath, req.To.Rig, "an overwrite names %q, which is not copied", o.Rel)
		}
		overwrite[o.Rel] = o
	}
	if err := s.checkCopyPlaces(req.From, req.To, tops); err != nil {
		return nil, err
	}
	return overwrite, nil
}

func (r *copyRun) run() error {
	s, from, to := r.s, r.req.From, r.req.To
	start := s.now()
	cfg := s.cfg.Load()
	var frames, previews int
	if profile := cfg.Rig(to.Rig); profile != nil {
		for _, it := range r.req.Items {
			p := joinRel(to.Dir, it.Rel)
			if rig.HarvestCandidate(profile, cfg.ExcludeExtensions, to.Share, p) {
				frames++
			} else if rig.HarvestPreview(profile, cfg.ExcludeExtensions, to.Share, p) {
				previews++
			}
		}
	}
	s.log.Info("devfiles: copy started", "rig", from.Rig, "share", from.Share, "op", opCopy,
		"from_dir", from.Dir, "to_rig", to.Rig, "to_share", to.Share, "to_dir", to.Dir,
		"files", r.total, "bytes", r.bytesTotal, "dirs", len(r.req.Dirs), "overwrites", len(r.overwrite),
		"watched_frames", frames, "watched_previews", previews)

	if err := r.ensureAll(); err != nil {
		if errors.Is(err, ErrCancelled) {
			r.logStopped()
		}
		return err
	}
	for _, fd := range r.failedDirs {
		if err := r.record(fd.rel, itemResult{outcome: failedOutcome(fd.err), code: copyReason(fd.err), msg: fd.err.Error()}); err != nil {
			return err
		}
	}

	for i, it := range r.req.Items {
		if r.aborted != "" {
			r.failed++
			msg := "Not attempted: " + s.nameOf(r.abortedBy) + " became unreachable."
			if r.aborted == abortedIdentity {
				msg = "Not attempted: Commander could not confirm it was talking to " + s.nameOf(r.abortedBy) + "."
			}
			if err := r.emitItem(it.Rel, outcomeFailed, codeNotAttempted, msg, nil); err != nil {
				return err
			}
			continue
		}
		if r.ctx.Err() != nil {
			r.logStopped()
			return cancelled(r.ctx)
		}
		r.current = it.Rel
		if err := r.progress(i, 0, true); err != nil {
			return err
		}
		var err error
		res := r.underFailedDir(it.Rel)
		if res.outcome == "" {
			res, err = r.item(i, it)
		}
		if err != nil {
			if errors.Is(err, ErrCancelled) {
				r.logStopped()
			}
			return err
		}
		r.bytesDone += it.Size
		if err := r.record(it.Rel, res); err != nil {
			return err
		}
	}

	s.log.Info("devfiles: copy finished", "rig", from.Rig, "share", from.Share, "op", opCopy,
		"to_rig", to.Rig, "done", r.done, "skipped", r.skipped, "failed", r.failed,
		"aborted", r.aborted, "duration", s.now().Sub(start))
	return r.emit(SummaryLine{Type: lineSummary, Done: r.done, Skipped: r.skipped, Failed: r.failed, Aborted: r.aborted})
}

func (r *copyRun) logStopped() {
	waited := time.Duration(0)
	if at := r.stoppedAt.Load(); at != 0 {
		waited = r.s.now().Sub(time.Unix(0, at))
	}
	r.s.log.Info("devfiles: copy cancelled", "rig", r.req.From.Rig, "share", r.req.From.Share, "op", opCopy,
		"to_rig", r.req.To.Rig, "done", r.done, "skipped", r.skipped, "failed", r.failed,
		"in_flight", r.current, "stop_waited", waited)
}

// record counts an item's outcome and emits its line when it has one.
func (r *copyRun) record(rel string, res itemResult) error {
	switch res.outcome {
	case outcomeCopied:
		r.done++
		if res.code == "" {
			return nil
		}
	case outcomeSkipped:
		r.skipped++
	default:
		r.failed++
	}
	return r.emitItem(rel, res.outcome, res.code, res.msg, res.dst)
}

func (r *copyRun) emitItem(rel, outcome, code, msg string, dst *device.FileInfo) error {
	line := ItemLine{Type: lineItem, Rel: rel, Outcome: outcome, Code: code, Message: msg}
	if dst != nil {
		size, mtime := dst.Size, unixOrZero(dst.Mtime)
		line.DstSize, line.DstMtime = &size, &mtime
	}
	return r.emit(line)
}

// progress emits a progress line for item i with n of its bytes copied: always
// when the item starts, and at most once per progressEvery within it.
func (r *copyRun) progress(i int, n int64, starting bool) error {
	now := r.s.now()
	if !starting && now.Sub(r.lastEmit) < progressEvery {
		return nil
	}
	r.lastEmit = now
	return r.emit(ProgressLine{
		Type: lineProgress, Done: i, Total: r.total,
		BytesDone: r.bytesDone + n, BytesTotal: r.bytesTotal, Current: r.current,
	})
}

// ensureAll makes To.Dir from the share root, then the confirmed folders
// parents first. A refusal that concerns the whole run (unreachable, identity,
// host-bound, the layout guard, a file in To.Dir's way, a cancel) is returned
// before any line; any other failure fails that folder's subtree.
func (r *copyRun) ensureAll() error {
	to := r.req.To
	err := r.s.withRig(r.ctx, opCopy, to.Share, to.Rig, func(c *session) error {
		if err := r.s.requireWritable(c, to.Share); err != nil {
			return err
		}
		sh, err := c.share(to.Share)
		if err != nil {
			return err
		}
		cur := ""
		for _, seg := range splitRel(to.Dir) {
			if err := r.ensureDir(c, sh, cur, seg); err != nil {
				return err
			}
			cur = joinRel(cur, seg)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, d := range r.req.Dirs {
		if underAny(d, r.failedRels()) != "" {
			continue
		}
		err := r.s.withRig(r.ctx, opCopy, to.Share, to.Rig, func(c *session) error {
			sh, err := c.share(to.Share)
			if err != nil {
				return err
			}
			return r.ensureDir(c, sh, joinRel(to.Dir, relDir(d)), path.Base(d))
		})
		switch {
		case err == nil:
		case errors.Is(err, ErrUnreachable), errors.Is(err, ErrCancelled), errors.Is(err, ErrIdentity),
			errors.Is(err, ErrHostBound), errors.Is(err, ErrLayout), errors.Is(err, ErrClosed):
			return err
		default:
			r.s.log.Warn("devfiles: copy folder not created", "rig", to.Rig, "share", to.Share, "op", opCopy,
				"path", joinRel(to.Dir, d), "error", err)
			r.failedDirs = append(r.failedDirs, failedDir{rel: d, err: err})
		}
	}
	return nil
}

// underFailedDir is the outcome of an item inside a folder the run could not
// make: that folder's failure. Its outcome is "" for any other item.
func (r *copyRun) underFailedDir(rel string) itemResult {
	for _, fd := range r.failedDirs {
		if sameOrInside(rel, fd.rel) {
			return itemResult{outcome: failedOutcome(fd.err), code: copyReason(fd.err), msg: fd.err.Error()}
		}
	}
	return itemResult{}
}

func (r *copyRun) failedRels() []string {
	out := make([]string, len(r.failedDirs))
	for i, fd := range r.failedDirs {
		out[i] = fd.rel
	}
	return out
}

// splitRel splits a share-relative path into its segments; "" has none.
func splitRel(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// item copies one file through the sink, retrying the whole unit (stat and
// open of the source through the commit) on fresh sessions. Its error is
// non-nil only when the run must end at once: a cancel or a failed emit. The
// first item to fail unreachable, on a wrong device or on a rig not yet
// identified ends the run.
func (r *copyRun) item(index int, it CopyItem) (itemResult, error) {
	from := r.req.From
	srcPath := joinRel(from.Dir, it.Rel)
	var sc *session // the attempt's source session, for readFailure
	gone := func() *itemResult {
		return &itemResult{outcome: outcomeSkipped, code: codeSourceGone,
			msg: fmt.Sprintf("%s is no longer on %s.", displayPath(from.Share, srcPath), sc.name)}
	}
	res, err := r.writeItem(it.Rel, source{
		rigs: r.rigs,
		open: func(ss []*session) (io.ReadCloser, time.Time, *itemResult, error) {
			sc = ss[0]
			ssh, err := sc.share(from.Share)
			if err != nil {
				return nil, time.Time{}, nil, onRig(sc.rigID, err)
			}
			fi, err := ssh.Stat(srcPath)
			switch {
			case err == nil && fi.IsDir, device.IsNotFound(err):
				return nil, time.Time{}, gone(), nil
			case err != nil:
				return nil, time.Time{}, nil, onRig(sc.rigID, classify(err, sc.rigID, sc.name, displayPath(from.Share, srcPath)))
			}
			rd, err := ssh.OpenReader(srcPath)
			if err != nil {
				if device.IsNotFound(err) {
					return nil, time.Time{}, gone(), nil
				}
				return nil, time.Time{}, nil, onRig(sc.rigID, classify(err, sc.rigID, sc.name, displayPath(from.Share, srcPath)))
			}
			return rd, fi.Mtime, nil, nil
		},
		readFailure: func(err error) error {
			return onRig(sc.rigID, classify(err, sc.rigID, sc.name, displayPath(from.Share, srcPath)))
		},
		progress: func(n int64) error { return r.progress(index, n, false) },
		logAttrs: []any{"from_rig", from.Rig, "from_share", from.Share, "from_path", srcPath},
	})
	if res.abort != "" {
		r.aborted = res.abort
		r.abortedBy = res.abortRig
	}
	return res, err
}
