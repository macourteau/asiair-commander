package rig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
)

// Re-filing moves an already-archived frame to where the CURRENT templates say it
// belongs and rewrites nothing but its dest_path (design routing-and-filing §8). It
// is deliberately NOT the filing path: fileFrom/recordFiled/UpsertFrame exist to
// file a frame arriving from the device, and routing an archived file through them
// would reset the retarget overlay, resurrect a deleted row, erase the
// reconciliation marks, charge a local rename to the transfer budget and — on a
// byte-identical clash — delete the operator's archived file. None of those calls
// appear in this file, and that is the contract INV-M1 states.
const (
	// refileScopeMosaic is the default view: only frames the current config would
	// route through the mosaic template. Nothing records which template originally
	// filed a frame, so "changed" is not a computable predicate and "would route
	// through the mosaic template now" is the closest honest one.
	refileScopeMosaic = "mosaic"
	// refileScopeAll is the unrestricted diff, behind an explicit opt-in: routing
	// resolves <OTA>/<SESSION>/<RIG> from the LIVE rig profile, so a renamed OTA
	// makes every frame's routed directory differ.
	refileScopeAll = "all"
)

// Re-file result statuses, the wire values of model.RefileResult.Status.
const (
	refileMoved   = "moved"
	refileSkipped = "skipped"
	refileFailed  = "failed"
)

// previewRefile projects the whole migration for this rig from STORED columns, the
// way previewRetarget does, so previewing a library is a database scan rather than
// a walk. The only disk access is one ReadDir per directory the migration would
// vacate, which is what lets the preview name the directories it expects to
// strand — bounded by group count, not frame count.
//
// It runs on the caller's goroutine (the HTTP one), never the worker's: it is
// read-only, and a whole-library diff inside the Run select would block pulls and
// the poll ticker for its duration and buy nothing.
func (w *Worker) previewRefile(ctx context.Context, scope string) (model.RefilePreview, error) {
	if scope != refileScopeAll {
		scope = refileScopeMosaic
	}
	pv := model.RefilePreview{Scope: scope, Groups: []model.RefileGroup{}, Refusals: []model.Refusal{}}
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	if profile == nil {
		return pv, errors.New("rig not managed")
	}
	// RefileCandidates already applies the whole-row half of the eligibility rule
	// (verified/deleted, a non-empty dest_path, no missing mark and no standing
	// restore request), so a path whose every row is ineligible is never offered
	// and never reported — there is nothing to enumerate it from. What still has
	// to be decided here is the per-PATH half: a sharing row can be ineligible on
	// its own, and eligibility governs the file, not the row.
	frames, err := w.store.RefileCandidates(ctx, w.profileID)
	if err != nil {
		return pv, fmt.Errorf("refile candidates: %w", err)
	}

	type groupKey struct{ from, to string }
	groups := map[groupKey]*model.RefileGroup{}
	// moving records, per source directory, the basenames the migration takes out
	// of it, so the strand check can subtract them from a single ReadDir. It is
	// keyed per DIRECTORY rather than per group because two groups can vacate one
	// directory, and neither empties it alone.
	moving := map[string]map[string]bool{}
	seen := map[string]bool{}

	for i := range frames {
		if ctx.Err() != nil {
			return pv, ctx.Err()
		}
		f := &frames[i]
		if seen[f.DestPath] {
			continue // a sibling row of a file already considered (the H12 case)
		}
		seen[f.DestPath] = true

		cls, opts := w.refileRouteInputs(cfg, f)
		if scope == refileScopeMosaic && !route.IsMosaicPanel(cls.Target, cls.Type, opts) {
			// Out of scope rather than refused: with mosaic detection off this is
			// every frame, which is why the UI offers the scope disabled instead of
			// rendering an empty diff.
			continue
		}
		rows, refusal, err := w.refileSharers(ctx, f)
		if err != nil {
			pv.Refusals = append(pv.Refusals, model.Refusal{DestPath: f.DestPath, Reason: err.Error()})
			continue
		}
		if refusal != "" {
			pv.Refusals = append(pv.Refusals, model.Refusal{DestPath: f.DestPath, Reason: refusal})
			continue
		}
		destDir, rerr := w.refileDestDir(cfg, profile, cls, opts)
		if rerr != nil {
			// A frame the current templates would quarantine has no destination, so
			// it cannot be a group member; it is reported, never moved to
			// unclassified/.
			pv.Refusals = append(pv.Refusals, model.Refusal{DestPath: f.DestPath, Reason: "the current templates cannot route this frame: " + rerr.Error()})
			continue
		}
		fromDir := filepath.Dir(f.DestPath)
		if samePath(fromDir, destDir) {
			continue // already where the templates say it belongs
		}
		key := groupKey{from: fromDir, to: destDir}
		g := groups[key]
		if g == nil {
			g = &model.RefileGroup{FromDir: fromDir, ToDir: destDir}
			groups[key] = g
		}
		// Count counts distinct dest_paths, not rows: an H12 frame is two rows and
		// one file, and the operator-facing number must say one. FrameIDs still
		// carries every row, because every row's dest_path is rewritten.
		g.Count++
		for _, r := range rows {
			g.FrameIDs = append(g.FrameIDs, r.ID)
		}
		if moving[fromDir] == nil {
			moving[fromDir] = map[string]bool{}
		}
		moving[fromDir][filepath.Base(f.DestPath)] = true
	}

	// One ReadDir per vacated directory. On a macOS-mounted NAS the Finder's
	// .DS_Store strands most of them, and that is the deployment this feature was
	// requested for — so it is said up front rather than arriving as a pile of skip
	// reports at apply time.
	risks := map[string]model.StrandRisk{}
	for dir, names := range moving {
		if risk, ok := w.strandRisk(dir, names); ok {
			risks[dir] = risk
		}
	}
	for key, g := range groups {
		if risk, ok := risks[key.from]; ok {
			g.StrandRisk = append(g.StrandRisk, risk)
		}
		pv.Groups = append(pv.Groups, *g)
	}
	sort.Slice(pv.Groups, func(i, j int) bool {
		if pv.Groups[i].FromDir != pv.Groups[j].FromDir {
			return pv.Groups[i].FromDir < pv.Groups[j].FromDir
		}
		return pv.Groups[i].ToDir < pv.Groups[j].ToDir
	})
	sort.Slice(pv.Refusals, func(i, j int) bool { return pv.Refusals[i].DestPath < pv.Refusals[j].DestPath })
	return pv, nil
}

// refileGroup moves one selected group's frames on the worker goroutine and prunes
// the directories it emptied. It carries a whole group rather than one frame
// because a 500-frame migration dispatched per frame would wait a poll cycle each
// time; a group costs a Stat, a rename and a small transaction per frame, so it
// runs to completion in seconds without yielding.
//
// wantDir is the group's expected destination. Every frame is routed again here,
// and one whose re-derived directory is not wantDir is refused rather than moved
// somewhere the operator never saw in the diff (INV-M10).
func (w *Worker) refileGroup(ctx context.Context, wantDir string, frameIDs []int64) ([]model.RefileResult, model.RefileSummary, error) {
	var sum model.RefileSummary
	out := make([]model.RefileResult, 0, len(frameIDs))
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	if profile == nil {
		return out, sum, errors.New("rig not managed")
	}
	// handled keys BOTH the pre-move and post-move path of every file already
	// reported, so a sharing row — whose re-read now names the new path — does not
	// produce a second line for one move. Results are per file, not per row.
	handled := map[string]bool{}
	vacated := map[string]bool{}

	for _, id := range frameIDs {
		// Cancellation is checked only BETWEEN frames: the commit itself is
		// uncancellable, because a cancel arriving mid-frame would otherwise fail
		// that frame's MoveDestPath and drive it through the commit-failure path,
		// turning a routine button press into the emergency route every time.
		if ctx.Err() != nil {
			break
		}
		f, ok, err := w.store.GetFrameByID(ctx, id)
		if err != nil {
			out = append(out, w.refileOutcome(model.RefileResult{FrameID: id, Status: refileFailed, Reason: "lookup: " + err.Error()}))
			continue
		}
		if !ok || f.RigID != w.profileID {
			out = append(out, w.refileOutcome(model.RefileResult{FrameID: id, Status: refileSkipped, Reason: "frame not found"}))
			continue
		}
		if f.DestPath != "" && handled[f.DestPath] {
			continue
		}
		from := f.DestPath
		res := w.refileOne(ctx, cfg, profile, f, wantDir)
		if from != "" {
			// Never keyed on "": a pathless row names no file, so it can share one
			// with nothing, and swallowing its report would be worse than repeating it.
			handled[from] = true
		}
		if res.To != "" {
			handled[res.To] = true
		}
		out = append(out, w.refileOutcome(res))
		if res.Status == refileMoved && from != "" {
			vacated[filepath.Dir(from)] = true
		}
	}

	roots := scanRoots(cfg)
	dirs := make([]string, 0, len(vacated))
	for d := range vacated {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		pruned, skipped := w.pruneEmptyDirs(d, roots)
		sum.PrunedDirs = append(sum.PrunedDirs, pruned...)
		sum.SkippedDirs = append(sum.SkippedDirs, skipped...)
	}
	return out, sum, nil
}

// refileOutcome records the per-file counter and returns res unchanged, so every
// exit from the apply loop is counted exactly once. A move is not a filing and
// must never touch frames_filed_total.
func (w *Worker) refileOutcome(res model.RefileResult) model.RefileResult {
	switch res.Status {
	case refileMoved:
		w.metrics.RecordRefileFrame(metrics.RefileMoved)
	case refileFailed:
		w.metrics.RecordRefileFrame(metrics.RefileFailed)
	default:
		w.metrics.RecordRefileFrame(metrics.RefileSkipped)
	}
	return res
}

// refileOne is the six-step move primitive for one FILE: resolve the destination
// directory from the current templates, choose the destination name, announce the
// intent, rename, repoint every row sharing the old path in one transaction, and
// clear the intent. It rewrites no bytes and writes no column but dest_path.
func (w *Worker) refileOne(ctx context.Context, cfg *model.Config, profile *model.RigProfile, f *model.Frame, wantDir string) model.RefileResult {
	res := model.RefileResult{FrameID: f.ID, Status: refileMoved, From: f.DestPath}
	skip := func(reason string) model.RefileResult {
		res.Status, res.Reason = refileSkipped, reason
		return res
	}
	fail := func(reason string) model.RefileResult {
		res.Status, res.Reason = refileFailed, reason
		return res
	}

	// Each row is re-read immediately before its move, so a frame that stopped
	// being eligible between preview and apply is skipped and reported rather than
	// moved from a stale snapshot.
	if reason := refileRowRefusal(*f); reason != "" {
		return skip(reason)
	}
	rows, refusal, err := w.refileSharers(ctx, f)
	if err != nil {
		return fail(err.Error())
	}
	if refusal != "" {
		return skip(refusal)
	}

	cls, opts := w.refileRouteInputs(cfg, f)
	destDir, rerr := w.refileDestDir(cfg, profile, cls, opts)
	if rerr != nil {
		return skip("the current templates cannot route this frame: " + rerr.Error())
	}
	if !samePath(destDir, wantDir) {
		// INV-M10: the operator selected a diff, not a re-route. A frame whose
		// re-derived directory is not the one they saw is left alone.
		return skip("routes to " + destDir + ", which is not the selected destination " + wantDir)
	}
	if samePath(filepath.Dir(f.DestPath), destDir) {
		return skip("already in the destination directory")
	}

	// The name is chosen HERE rather than delegated, so the exact destination path
	// is known before the rename and the intent can name it. candidateName exhausts
	// purely on hash length, so it is handed the full 64-hex archive hash.
	hash := archiveHash(*f)
	stem, ext := splitExt(w.candidateBaseName(filepath.Base(f.DestPath), hash))
	dateTok := refileDateToken(f.DateObs)
	renamed, adopted := false, false
	to := ""
	for i := 0; ; i++ {
		cand := candidateName(stem, ext, dateTok, hash, i)
		if cand == "" {
			// Reachable only with a file at BOTH ends. Re-file deliberately excludes
			// fileFrom's source removal — deleting archived data is not this
			// operation's business — so the old copy is left where it is and said so.
			w.clearRefileIntent(ctx, f.DestPath)
			return fail("no free name in " + destDir + "; the file was left at its old path")
		}
		to = filepath.Join(destDir, cand)

		// The intent is written BEFORE the rename, and a failure to write it aborts
		// before renaming: the crash-safety guarantee is void without it, and the
		// conditions that break this write are the ones that break the commit.
		if err := w.store.PutRefileIntent(ctx, store.RefileIntent{
			RigID: w.profileID, FromPath: f.DestPath, ToPath: to, CreatedAt: w.now().Unix(),
		}); err != nil {
			return fail("could not record the move intent: " + err.Error())
		}
		if err := w.lfs.MkdirAll(destDir); err != nil {
			w.clearRefileIntent(ctx, f.DestPath)
			return fail("mkdir " + destDir + ": " + err.Error())
		}

		rerr := w.lfs.RenameNoReplace(f.DestPath, to)
		if rerr == nil {
			renamed = true
			if w.crashAfterRefileRename != nil && w.crashAfterRefileRename(to) {
				// Modelled process crash between the rename and the commit: the file
				// is at `to`, the row still at `from`, and the intent is open —
				// exactly what startup recovery resolves, and what refile_in_flight
				// keeps out of DeletionEligible meanwhile.
				res.To = to
				return fail("interrupted after the rename")
			}
			break
		}
		if localfs.IsCrossDevice(rerr) {
			// EXDEV refuses rather than falling back: carrying an untested
			// copy-verify-delete branch is what would make re-file rewrite bytes.
			w.clearRefileIntent(ctx, f.DestPath)
			return fail("the destination is on another volume; a rename cannot span volumes")
		}
		if !localfs.IsExist(rerr) {
			w.clearRefileIntent(ctx, f.DestPath)
			return fail("rename to " + to + ": " + rerr.Error())
		}

		// The destination is taken: compare before advancing. Blind advancement
		// would duplicate a frame an interrupted run had already moved and, on a
		// case-insensitive volume, permanently append a date token to a frame that
		// was already correct.
		if strings.EqualFold(f.DestPath, to) {
			// A case-only rename on a case-insensitive volume: `from` and `to` name
			// the same file, so there is no second copy and nothing to report. The
			// paths are compared case-insensitively because localfs.Stat returns only
			// size and mtime and cannot answer SameFile.
			break
		}
		same, cerr := w.sameArchiveAt(to, f)
		if cerr != nil {
			w.clearRefileIntent(ctx, f.DestPath)
			return fail("could not compare the file already at " + to + ": " + cerr.Error())
		}
		if same {
			// Already where it belongs: commit the path and stop. The copy at the old
			// path is left untouched, and reported.
			adopted = true
			res.Reason = "the destination already held a byte-identical copy; a copy remains at " + f.DestPath
			break
		}
		// Different content at this name: advance, rewriting the intent so it always
		// names the path the rename is about to attempt.
	}

	// The commit is uncancellable: a cancel arriving here would fail MoveDestPath
	// and drive a routine cancellation through the emergency rename-back route.
	commitCtx := context.WithoutCancel(ctx)
	if _, err := w.store.MoveDestPath(commitCtx, w.profileID, f.DestPath, to, store.RequireRows); err != nil {
		return w.refileCommitFailed(commitCtx, f, to, renamed, err)
	}
	// A failed clear is benign: it leaves an intent whose recovery row is "row
	// already at `to` → drop".
	w.clearRefileIntent(commitCtx, f.DestPath)
	res.To = to
	if adopted {
		w.log.Info("rig: re-file adopted an identical copy already at the destination",
			"frame", f.ID, "from", f.DestPath, "to", to, "rows", len(rows))
		return res
	}
	w.log.Info("rig: re-filed frame", "frame", f.ID, "from", f.DestPath, "to", to, "rows", len(rows))
	return res
}

// refileCommitFailed handles a database write that failed without a crash — disk
// full, I/O error, SQLITE_BUSY — which startup recovery would not resolve for
// hours. Left alone, the file sits at `to` and the row at `from`, the frame is
// marked missing, the settle window elapses and the delete pass releases a device
// original against an archive nothing references.
func (w *Worker) refileCommitFailed(ctx context.Context, f *model.Frame, to string, renamed bool, cause error) model.RefileResult {
	res := model.RefileResult{FrameID: f.ID, Status: refileFailed, From: f.DestPath, To: to}
	if errors.Is(cause, store.ErrNoRowsMoved) {
		// PruneDeleted runs hourly and deletes one of re-file's two eligible states,
		// so the row can legitimately vanish between the pre-move re-read and the
		// commit. Unlike a genuine commit failure this does NOT rename back:
		// restoring the file would re-create something nothing references.
		w.clearRefileIntent(ctx, f.DestPath)
		w.log.Warn("rig: re-file found no row to commit; the file is at its new path",
			"frame", f.ID, "from", f.DestPath, "to", to)
		res.Reason = "the frame's row was removed while the move was in flight; the file now sits at " + to
		return res
	}
	if !renamed {
		// Nothing moved (the destination already held this frame), so there is
		// nothing to put back.
		w.clearRefileIntent(ctx, f.DestPath)
		res.Reason = "commit failed: " + cause.Error()
		return res
	}
	if back := w.lfs.RenameNoReplace(to, f.DestPath); back != nil {
		// The intent SURVIVES deliberately: refile_in_flight is what keeps this
		// frame out of DeletionEligible however the move failed, and an alert alone
		// would be visibility without prevention.
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityCritical,
			Category: model.AlertRefile,
			Message: "re-file could not commit and could not move the file back: " + to +
				" holds frame " + f.DestPath + ", whose row still names the old path (" + back.Error() + ")",
		})
		w.log.Error("rig: re-file commit failed and the rename back failed; the intent is left open",
			"frame", f.ID, "from", f.DestPath, "to", to, "commit", cause, "renameBack", back)
		res.Reason = "commit failed and the file could not be moved back: " + back.Error()
		return res
	}
	w.clearRefileIntent(ctx, f.DestPath)
	w.log.Warn("rig: re-file commit failed; the file was moved back", "frame", f.ID, "dest", f.DestPath, "error", cause)
	res.Reason = "commit failed; the file was moved back: " + cause.Error()
	return res
}

// refileSharers returns the rig's rows sharing f's dest_path, together with the
// refusal the PATH earns as a whole, if any. Eligibility is decided per dest_path
// rather than per row: a hold is applied to a single row and the H12 case is two
// rows on one path, so one share's row can be held while its sibling is verified —
// and moving on behalf of the eligible one would drag the ineligible one along.
func (w *Worker) refileSharers(ctx context.Context, f *model.Frame) ([]model.Frame, string, error) {
	total, err := w.store.CountFramesByDestAnyRig(ctx, f.DestPath)
	if err != nil {
		return nil, "", fmt.Errorf("could not count the rows sharing this file: %w", err)
	}
	if total == 1 {
		// The candidate row is the only row referencing this file anywhere in the
		// fleet, so there is no sharer to check and no second query to make.
		return []model.Frame{*f}, "", nil
	}
	rows, err := w.store.FramesByDestPath(ctx, w.profileID, f.DestPath)
	if err != nil {
		return nil, "", fmt.Errorf("could not read the rows sharing this file: %w", err)
	}
	if total > len(rows) {
		// One goroutine owns a rig's frame mutations, so rig A's worker must not
		// rewrite rig B's rows. A cross-rig sharer is left alone and reported.
		return rows, "the file is also referenced by another rig's frame", nil
	}
	for _, r := range rows {
		if reason := refileRowRefusal(r); reason != "" {
			return rows, "a row sharing this file is not re-fileable: " + reason, nil
		}
	}
	return rows, "", nil
}

// refileRowRefusal states why one row makes its file un-re-fileable, or "" when it
// does not. Both eligible states mean an archived copy exists; every other
// condition here means its whereabouts or its correctness is already in question,
// and moving such a file is not this tool's business.
func refileRowRefusal(f model.Frame) string {
	if f.DestPath == "" {
		return "the row names no archived file"
	}
	if f.State != model.StateVerified && f.State != model.StateDeleted {
		return "frame state " + string(f.State) + " is not re-fileable"
	}
	if f.MissingSeenAt != 0 {
		return "the archived file is marked missing"
	}
	if f.RestoreRequestedAt != 0 {
		return "a restore is pending for this frame"
	}
	return ""
}

// refileRouteInputs rebuilds one archived row's classification and routing options
// from its STORED columns.
//
// Two details are load-bearing and deliberately differ from previewRetarget:
//
// The frame's stored night is passed through. previewRetarget passes Night: 0
// because a preview must never assign an ordinal; every template in play here
// contains NIGHT_<n>, so a zero would render NIGHT_0 for every frame and INV-M10
// would then refuse them all. AssignNight is never called: it would mint a new
// ordinal and strand the frame in a NIGHT_n its siblings do not share.
//
// Every stored scalar but SET-TEMP is treated as PRESENT. Presence is not
// recoverable for them: UpsertFrame binds exptime, gain and offset raw, so a
// legitimate zero is stored as 0 and never NULL. A != 0 test would report a
// cooled camera's legitimate 0 as unroutable across a whole library. SET-TEMP
// alone records its absence (Frame.SetTempAbsent), because an absent SET-TEMP
// routes to the uncooled bucket rather than quarantining, so assuming presence
// would move every uncooled frame into a 0C directory.
//
// Apply assumes presence identically, by calling this same helper — which is what
// makes preview and apply agree by construction. The residual cost is narrow: a
// frame that genuinely lacks GAIN re-files into a gain0 directory where live
// filing would have quarantined it, reachable only when a template acquires
// <GAIN> after the frame was filed, since a frame missing a token its template
// names could never have been filed with a dest_path at all.
func (w *Worker) refileRouteInputs(cfg *model.Config, f *model.Frame) (model.Classification, route.Options) {
	cls := model.Classification{Type: model.FrameType(f.FrameType)}
	// The RAW stored target, matching the filing path: template SELECTION depends
	// on this string, so a preview that transformed it differently from apply would
	// disagree about whether a frame is in scope at all.
	cls.Target, cls.TargetPresent = f.Target, true
	cls.Filter, cls.FilterPresent = f.Filter, true
	cls.Exptime, cls.ExptimePresent = f.Exptime, true
	cls.SetTempC, cls.SetTempPresent = f.SetTemp, !f.SetTempAbsent
	cls.Gain, cls.GainPresent = f.Gain, true
	cls.Offset, cls.OffsetPresent = f.Offset, true
	cls.Sensor, cls.SensorMapped = f.Sensor, true
	if x, y, ok := parseBinningToken(f.Binning); ok {
		cls.BinX, cls.BinY, cls.BinPresent = x, y, true
	}
	if t, err := time.Parse(time.RFC3339Nano, f.DateObs); err == nil {
		cls.DateObs, cls.DateObsPresent = t, true
	}
	var obs time.Time
	if cls.DateObsPresent {
		obs = route.ObservingDate(cls.DateObs, w.siteLoc)
	}
	return cls, route.Options{
		Templates:         cfg.Templates,
		Night:             int(f.Night),
		ObservingDate:     obs,
		DateFormat:        cfg.DateFormat,
		StripTargetSpaces: cfg.StripTargetSpaces,
		MosaicDetection:   cfg.MosaicDetection,
	}
}

// refileDestDir routes a rebuilt classification and joins the root its Kind
// selects, producing the ABSOLUTE directory dest_path is built from (route.Result
// .Dir is root-relative). Re-joining the selected root is also what makes the
// cal-root prune stop meaningful: an unrestricted re-file moves calibration frames
// too.
func (w *Worker) refileDestDir(cfg *model.Config, profile *model.RigProfile, cls model.Classification, opts route.Options) (string, error) {
	res, err := route.Route(cls, *profile, opts)
	if err != nil {
		return "", err
	}
	root := cfg.Roots.Light
	if res.Kind == route.RootCal {
		root = cfg.Roots.Cal
	}
	return joinLocal(root, res.Dir), nil
}

// refileDateToken renders the stored DATE-OBS as the disambiguation token
// candidateName expects — the stored RFC3339 string is not that token. An empty or
// unparseable value yields an empty token rather than an error, which simply drops
// that rung of the ladder.
func refileDateToken(dateObs string) string {
	t, err := time.Parse(time.RFC3339Nano, dateObs)
	if err != nil {
		return ""
	}
	return t.UTC().Format("20060102-150405")
}

// sameArchiveAt reports whether the file already at path IS this frame: the size
// must match first, and only then is the cache-bypassing read-back spent. Both
// halves are archive-side — ArchiveSize and archiveHash — so a frame whose archive
// copy has come to differ from its device copy is still recognised as its own file.
// The size short-circuit spends no read, so a rule written over VerifyRead alone
// would miss it and report a perfectly good archive copy as not this frame
// (design frame-lifecycle §11.1).
func (w *Worker) sameArchiveAt(path string, f *model.Frame) (bool, error) {
	size, _, err := w.lfs.Stat(path)
	if err != nil {
		return false, err
	}
	if size != f.ArchiveSize {
		return false, nil
	}
	hash, err := w.lfs.VerifyRead(path, f.ArchiveSize)
	if err != nil {
		return false, err
	}
	return hash == archiveHash(*f), nil
}

// clearRefileIntent drops an intent best-effort. Every failure path clears
// unconditionally, and a failed clear is benign: startup recovery resolves the
// leftover as "row already at `to` → drop".
func (w *Worker) clearRefileIntent(ctx context.Context, fromPath string) {
	if err := w.store.ClearRefileIntent(ctx, w.profileID, fromPath); err != nil {
		w.log.Warn("rig: could not clear a re-file intent; startup recovery will resolve it",
			"path", fromPath, "error", err)
	}
}

// strandRisk reports whether the directory a migration vacates will survive it,
// and why. moving names the basenames the migration takes out of dir; anything
// else left in it — a Finder .DS_Store, a CAL folder of the operator's stacked
// masters, stacking output — means os.Remove will refuse and the operator would
// otherwise be left with leftover folders and no explanation.
func (w *Worker) strandRisk(dir string, moving map[string]bool) (model.StrandRisk, bool) {
	ents, err := w.lfs.ReadDir(dir)
	if err != nil {
		if isNotExist(err) {
			return model.StrandRisk{}, false
		}
		return model.StrandRisk{Dir: dir, Reason: "cannot be listed: " + err.Error()}, true
	}
	var leftover []string
	for _, e := range ents {
		if !e.IsDir && moving[e.Name] {
			continue
		}
		if e.Name == model.LibraryMarkerName {
			return model.StrandRisk{Dir: dir, Reason: "holds the library link marker " + model.LibraryMarkerName + ", so it is never removed"}, true
		}
		leftover = append(leftover, e.Name)
	}
	if len(leftover) == 0 {
		return model.StrandRisk{}, false
	}
	return model.StrandRisk{Dir: dir, Reason: describeLeftovers(leftover)}, true
}

// describeLeftovers names what re-file will not move, capped so one crowded
// directory cannot produce an unreadable line.
func describeLeftovers(names []string) string {
	sort.Strings(names)
	shown := names
	suffix := ""
	if len(shown) > 3 {
		shown, suffix = shown[:3], ", …"
	}
	noun := " entries re-file does not manage remain ("
	if len(names) == 1 {
		noun = " entry re-file does not manage remains ("
	}
	return "will not be empty: " + strconv.Itoa(len(names)) + noun + strings.Join(shown, ", ") + suffix + ")"
}

// pruneEmptyDirs removes the directory a migration emptied and walks upward
// removing empty parents, stopping at the LONGEST-matching configured root. The
// ranking is on the cleaned root, as store.longestRoot does and unlike the delete
// path's inline first-match loop: a first match would let the prune walk past a
// Roots.Cal nested inside Roots.Light and eventually remove the configured Cal
// root itself, and an unrestricted re-file moves calibration frames too.
//
// Nothing outside the frames the operator selected is ever removed: a directory
// that is not empty is reported, never emptied, and MkdirAll runs before every
// rename, so a concurrent filing race costs a failed rename rather than a lost
// frame.
//
// Only the VACATED directory earns a "could not remove" report, plus any
// directory holding a library marker and any outright failure. An ancestor that
// merely still holds the migration's other folders is where the walk was always
// going to stop, and reporting it would tell the operator a directory they never
// expected to lose survived. That is also the symmetry with the preview, which
// examines exactly the vacated directories.
func (w *Worker) pruneEmptyDirs(dir string, roots []string) (pruned []string, skipped []model.StrandRisk) {
	root := longestConfiguredRoot(dir, roots)
	if root == "" {
		return nil, []model.StrandRisk{{Dir: dir, Reason: "lies outside every configured root, so it was left alone"}}
	}
	cur := filepath.Clean(dir)
	stop := filepath.Clean(root)
	for depth := 0; cur != stop && store.UnderRoot(cur, stop); depth++ {
		ents, err := w.lfs.ReadDir(cur)
		if err != nil {
			if isNotExist(err) {
				cur = filepath.Dir(cur) // already gone; its parent may still be prunable
				continue
			}
			skipped = append(skipped, model.StrandRisk{Dir: cur, Reason: "cannot be listed: " + err.Error()})
			return pruned, skipped
		}
		if len(ents) > 0 {
			names := make([]string, 0, len(ents))
			marker := false
			for _, e := range ents {
				if e.Name == model.LibraryMarkerName {
					marker = true
				}
				names = append(names, e.Name)
			}
			switch {
			case marker:
				skipped = append(skipped, model.StrandRisk{Dir: cur,
					Reason: "holds the library link marker " + model.LibraryMarkerName + ", so it is never removed"})
			case depth == 0:
				skipped = append(skipped, model.StrandRisk{Dir: cur, Reason: describeLeftovers(names)})
			}
			return pruned, skipped
		}
		if err := w.lfs.Remove(cur); err != nil {
			reason := "could not be removed: " + err.Error()
			if localfs.IsNotEmpty(err) {
				reason = "is not empty, so it was left in place"
			}
			skipped = append(skipped, model.StrandRisk{Dir: cur, Reason: reason})
			return pruned, skipped
		}
		pruned = append(pruned, cur)
		cur = filepath.Dir(cur)
	}
	return pruned, skipped
}

// longestConfiguredRoot returns the configured root that most-specifically
// contains dir, mirroring store.longestRoot (unexported there): specificity is
// measured on the CLEANED root so a root written with redundant separators cannot
// out-rank a genuinely more specific nested one.
func longestConfiguredRoot(dir string, roots []string) string {
	best, bestLen := "", -1
	for _, r := range roots {
		if n := len(filepath.Clean(r)); store.UnderRoot(dir, r) && n > bestLen {
			best, bestLen = r, n
		}
	}
	return best
}

// samePath compares two local paths after cleaning them, and deliberately does
// NOT fold case: on a case-sensitive volume a template edit that changes only case
// is a genuine move, and pre-judging it here would skip it. The one place case is
// folded is the EEXIST branch of the rename ladder, where the volume has already
// told us the two names resolve to one file.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}
