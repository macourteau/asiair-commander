package rig

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/panel"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/sanitize"
)

// fileOutcome is the terminal-outcome signal a filing hands back. Two of its cases
// are the pipeline's originals; the third exists because a revision of a frame the
// operator culled on the NAS is a terminal outcome that FILES NOTHING, and neither
// original fits it — fileDeferred would record a file_failed outcome and leave the
// frame in the live feed's pending set forever, and fileFiled would claim an
// archive copy that was deliberately never written (design frame-lifecycle
// §11.12).
type fileOutcome int

const (
	// fileDeferred: nothing terminal happened. The frame keeps its place in the
	// pending set and is re-pulled next cycle.
	fileDeferred fileOutcome = iota
	// fileFiled: the archive copy holds this frame's bytes.
	fileFiled
	// fileNothingFiled: terminal, with nothing written to the archive.
	fileNothingFiled
)

// classifyAndFile reads the FITS header from the local staging temp, classifies
// and routes the frame, and files it atomically (design frame-lifecycle §7–§8).
// Any header or routing failure (unknown IMAGETYP, missing routing-critical
// variable, parse error) sends the frame to the quarantine area — filed, never
// deleted, alerted. It returns the frame's terminal outcome for this cycle.
func (w *Worker) classifyAndFile(ctx context.Context, cfg *model.Config, profile *model.RigProfile, lf listedFrame, prior *model.Frame, tmpPath, diskHash, dataHash string) fileOutcome {
	// design routing-and-filing §9.4 re-apply rule: freshly-pulled device bytes carry the
	// ORIGINAL OBJECT, so a retargeted frame re-pulled here (differ jitter,
	// deleted-readopt, DB-loss no-row) would otherwise re-classify under the original
	// header and revert to the old folder. When the incoming bytes are the frame whose
	// OBJECT card was edited, re-apply the edit before classification and file with the
	// overlay preserved; genuinely-new content skips the re-apply and resets the overlay.
	// This one chokepoint makes every re-pull path correct.
	srcPath, srcHash := tmpPath, diskHash
	// The archive-side length of the file about to be filed. It is the incoming
	// length on both branches: the streamed temp was verified at the listed device
	// length, and the re-apply rewrites exactly one card of THAT file, so the edit it
	// produces has the incoming length too (design frame-lifecycle §11.11:
	// every assertion around the rewrite is re-anchored to the file in hand).
	srcSize := lf.size
	var reSHA, reFrom string
	if p2, h2, err := w.reapplyRetargetIfNeeded(cfg, tmpPath, diskHash, dataHash, lf.size, prior); err != nil {
		w.log.Error("rig: re-apply retarget failed; deferring file", "path", lf.path, "error", err)
		return fileDeferred
	} else if p2 != tmpPath {
		// A second temp holds the re-applied edit; its cleanup is ours (transfer's defer
		// only removes the original streamed temp).
		srcPath, srcHash = p2, h2
		// The audit record follows the file the edit just produced rather than the one
		// it produced last time: on an unrevised re-pull the two are the same value,
		// and on a revision they are not — an audit record that silently stops matching
		// the file it describes is how the archive-side fallback began.
		reSHA, reFrom = srcHash, prior.RetargetFrom
		defer func() { _ = w.lfs.Remove(srcPath) }()
	}
	// sha256 is the DEVICE-side anchor, so it is always the hash of the bytes just
	// pulled off the device — diskHash, whether or not an overlay was re-applied on
	// top of them. Committing prior.SHA256 on the re-apply branch was correct only
	// while a device original could never change: after a device-side revision there
	// is no such original left to point at, and the pre-revision hash would leave
	// deviceHash != f.SHA256 on every delete pass — a permanent critical
	// delete_mismatch on the very path revision handling exists for (design
	// frame-lifecycle §11.11).
	sha := diskHash

	h, err := w.hdr.ReadHeader(srcPath)
	if err != nil {
		w.log.Warn("rig: header read failed; filing unclassified", "path", lf.path, "error", err)
		return w.fileUnclassified(ctx, cfg, profile, lf, prior, srcPath, srcHash, srcSize, sha, dataHash, reSHA, reFrom, nil, model.Classification{}, "FITS header could not be read: "+err.Error())
	}
	// Extract the embedded WCS once, here, and thread it through every filing path so
	// a newly-filed frame carries its coordinates immediately (no wait for backfill).
	astro := astrometry.FromHeader(h)
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	if cls.Type == model.TypeUnknown {
		msg := "unknown IMAGETYP"
		if cls.RawImageType != "" {
			msg = "unknown IMAGETYP " + strconv.Quote(cls.RawImageType)
		}
		w.log.Warn("rig: unknown IMAGETYP; filing unclassified", "path", lf.path, "imagetyp", cls.RawImageType)
		return w.fileUnclassified(ctx, cfg, profile, lf, prior, srcPath, srcHash, srcSize, sha, dataHash, reSHA, reFrom, astro, cls, msg)
	}

	var night int
	// The observing date feeds the <DATE> token for every frame type (cal
	// templates fold into per-date folders too); compute it whenever DATE-OBS is
	// present. A template that needs <DATE> but has no DATE-OBS quarantines in
	// route.Route, so an absent value never silently collapses to the zero date.
	var obs time.Time
	if cls.DateObsPresent {
		obs = route.ObservingDate(cls.DateObs, w.siteLoc)
	}
	opts := routeOptionsFor(cfg, obs)
	if cls.Type == model.TypeLight {
		// The night-derivation inputs are routing-critical for a light; pre-check
		// them so a missing value never pollutes night_counters with a bogus date.
		if !cls.TargetPresent {
			return w.fileUnclassified(ctx, cfg, profile, lf, prior, srcPath, srcHash, srcSize, sha, dataHash, reSHA, reFrom, astro, cls, "light frame missing OBJECT")
		}
		if !cls.DateObsPresent {
			return w.fileUnclassified(ctx, cfg, profile, lf, prior, srcPath, srcHash, srcSize, sha, dataHash, reSHA, reFrom, astro, cls, "light frame missing DATE-OBS")
		}
		n, err := w.nightFor(ctx, cfg, profile, &cls, opts)
		var qe *route.QuarantineError
		switch {
		case err == nil:
			night = n
		case errors.As(err, &qe):
			// The destination cannot be rendered, so there is no folder to consult and
			// no ordinal to spend. Fall through with night 0: route.Route re-derives
			// the identical quarantine below and the frame is filed unclassified,
			// loudly and recoverably — routing it into the transient path instead
			// would re-pull it every cycle forever.
			w.log.Warn("rig: night folder quarantine", "path", lf.path, "reason", qe.Error())
		default:
			// Transient store error: do not file with a wrong night; re-pull next cycle.
			w.log.Error("rig: assign night failed; deferring file", "path", lf.path, "error", err)
			return fileDeferred
		}
	}

	opts.Night = night
	res, rerr := route.Route(cls, *profile, opts)
	if rerr != nil {
		var qe *route.QuarantineError
		if errors.As(rerr, &qe) {
			w.log.Warn("rig: routing quarantine; filing unclassified", "path", lf.path, "reason", qe.Error())
			return w.fileUnclassified(ctx, cfg, profile, lf, prior, srcPath, srcHash, srcSize, sha, dataHash, reSHA, reFrom, astro, cls, "routing: "+qe.Error())
		}
		// A defensive guard (non-canonical type / missing template): quarantine.
		w.log.Warn("rig: routing error; filing unclassified", "path", lf.path, "error", rerr)
		return w.fileUnclassified(ctx, cfg, profile, lf, prior, srcPath, srcHash, srcSize, sha, dataHash, reSHA, reFrom, astro, cls, "routing: "+rerr.Error())
	}

	root := cfg.Roots.Light
	if res.Kind == route.RootCal {
		root = cfg.Roots.Cal
	}
	destDir := joinLocal(root, res.Dir)
	name := w.candidateBaseName(lf.name, srcHash)
	nowUnix := w.now().Unix()
	return w.fileFrom(ctx, fileArgs{
		lf:             lf,
		prior:          prior,
		cls:            cls,
		astro:          astro,
		night:          night,
		srcPath:        srcPath,
		diskHash:       srcHash,
		sha256:         sha,
		dataSHA256:     dataHash,
		retargetSHA256: reSHA,
		retargetFrom:   reFrom,
		archiveSHA256:  srcHash,
		archiveSize:    srcSize,
		destDir:        destDir,
		canonicalName:  name,
		state:          model.StateVerified,
		pulledAtNew:    nowUnix,
		pulledAtKeep:   priorPulledAt(prior, nowUnix),
	})
}

// fileUnclassified files a frame to the per-rig quarantine area
// <lightRoot>/unclassified/<rig_id>/ (still an atomic disambiguating rename) and
// raises the unclassified alert (design routing-and-filing §2). Such a frame keeps
// its device original as the authoritative copy while its NAS copy is present; only
// once the operator culls that NAS copy does it become deletion-eligible via the
// cull drain (DeletionEligible's unclassified-AND-missing branch).
func (w *Worker) fileUnclassified(ctx context.Context, cfg *model.Config, profile *model.RigProfile, lf listedFrame, prior *model.Frame, tmpPath, diskHash string, archiveSize int64, sha, dataSHA256, retargetSHA256, retargetFrom string, astro *model.Astrometry, cls model.Classification, reason string) fileOutcome {
	destDir := joinLocal(cfg.Roots.Light, "unclassified/"+sanitize.Component(w.profileID))
	name := w.candidateBaseName(lf.name, diskHash)
	nowUnix := w.now().Unix()
	out := w.fileFrom(ctx, fileArgs{
		lf:       lf,
		prior:    prior,
		cls:      cls,
		astro:    astro,
		night:    0,
		srcPath:  tmpPath,
		diskHash: diskHash,
		// sha256/overlay carry the same anchors the classified path would use: sha is the
		// device copy just proven, and a re-applied retargeted frame that then fails to
		// classify/route keeps its overlay, never anchoring sha256 to the edited archive
		// hash (F4).
		sha256:         sha,
		dataSHA256:     dataSHA256,
		retargetSHA256: retargetSHA256,
		retargetFrom:   retargetFrom,
		// diskHash IS the archive-side hash here, as it is on every filing site; the
		// column is written from an explicit field so a later writer that files
		// something other than what it verified cannot bind the wrong one silently.
		archiveSHA256: diskHash,
		archiveSize:   archiveSize,
		destDir:       destDir,
		canonicalName: name,
		state:         model.StateUnclassified,
		pulledAtNew:   nowUnix,
		pulledAtKeep:  priorPulledAt(prior, nowUnix),
	})
	// The alert names a frame sitting in the quarantine area, so only a real filing
	// earns it: a revision of a culled frame writes nothing there.
	if out == fileFiled {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertUnclassified,
			Message:  "unclassified frame " + lf.path + ": " + reason,
		})
	}
	return out
}

// fileArgs bundles the inputs to fileFrom, the one atomic disambiguating filing
// path shared by new pulls, quarantine, and reclassify moves.
type fileArgs struct {
	lf       listedFrame
	prior    *model.Frame
	cls      model.Classification
	astro    *model.Astrometry // header-derived WCS/hints, nil when the header was unreadable
	night    int
	srcPath  string // staging temp (pull) or an existing NAS file (reclassify)
	diskHash string // the at-rest hash of srcPath's content (drives disambiguation + idempotent-keep)
	// sha256 is the device-side anchor written to the sha256 column: what the device copy
	// hashes to as last proven. On a path with an incoming device file it is that file's
	// hash — diskHash — whether or not an overlay was re-applied on top of it, since the
	// device holds what was just pulled and nothing else. On a local-rename path
	// (retarget, reclassify) it is the device anchor carried through from the row, those
	// paths never touching the device copy, and there diskHash is the edited archive
	// hash instead. Omitting it writes '' → drops the frame from DeletionEligible, so
	// every filing site sets it.
	sha256 string
	// dataSHA256 is the exposure identity of the incoming file: the SHA-256 of its
	// FITS data unit, teed off the at-rest verify that proved the staging temp. It is
	// leg 1 of the three-way agreement a supersede needs and the value the row
	// records for leg 2 of the next one. A site with no incoming device file (a
	// reclassify, a retarget) leaves it empty, which UpsertFrame preserves against
	// and which every identity test refuses (design frame-lifecycle §11.3).
	dataSHA256 string
	// retargetSHA256/retargetFrom are the retarget overlay (design routing-and-filing
	// §9.2), written straight into the columns from explicit inputs (never a sticky
	// CASE): a retarget/re-apply passes the real values; a genuinely-new-content pull
	// passes empty (→ NULL), resetting the overlay when the archive is replaced by new
	// content.
	retargetSHA256 string
	retargetFrom   string
	// archiveSHA256/archiveSize are the archive-side anchors written to the
	// archive_sha256 and archive_size columns: what the file this call is filing must
	// hash to, and the length it was verified at. They describe srcPath — the edited
	// bytes on a retarget, the pulled bytes otherwise — which is not always what the
	// device holds, so they are passed explicitly by every site rather than derived
	// from the device anchors. An unset pair turns a perfectly good archive copy into
	// a critical delete_mismatch, which is why nothing bridges a missing one
	// (design frame-lifecycle §11.1).
	archiveSHA256 string
	archiveSize   int64
	destDir       string
	canonicalName string
	state         model.FrameState
	// deletedAt carries the row's deleted_at through a re-file so it is preserved. A
	// normal filing site passes 0 (not deleted); retargeting a deleted frame passes the
	// existing deleted_at so the relabel does not resurrect the row (it would otherwise
	// write NULL, and the frame would never prune / lose its reclaim clock).
	deletedAt    int64
	pulledAtNew  int64 // pulled_at on a fresh rename success
	pulledAtKeep int64 // pulled_at on an idempotent keep
	reclassify   bool  // true only for the reclassify path (a local rename, zero network bytes)
}

// fileFrom claims the destination name with an atomic exclusive rename,
// disambiguating a content clash deterministically (design frame-lifecycle §8).
// It never overwrites: an identical-content clash is an idempotent keep; a
// different- content clash appends a DATE-OBS-then-sha256-prefix token; a
// different-content clash at the frame's own canonical name also raises the
// rot-clash alert. It returns the frame's terminal outcome for this cycle.
func (w *Worker) fileFrom(ctx context.Context, a fileArgs) fileOutcome {
	// A frame the DEVICE revised in place is the same exposure the row already
	// describes, not a clash, so it replaces the archive copy under the name that row
	// names rather than claiming a second one below. The proof is the caller's file
	// against the row's own copy, and anything short of it falls through to the
	// disambiguating claim like any other content clash (design frame-lifecycle
	// §11.5).
	if out, done := w.resolveRevision(ctx, a); done {
		return out
	}
	if err := w.lfs.MkdirAll(a.destDir); err != nil {
		w.log.Error("rig: mkdir dest failed", "dir", a.destDir, "error", err)
		return fileDeferred
	}
	stem, ext := splitExt(a.canonicalName)
	dateTok := ""
	if a.cls.DateObsPresent {
		dateTok = a.cls.DateObs.UTC().Format("20060102-150405")
	}
	canonicalAlerted := false

	for i := 0; ; i++ {
		cand := candidateName(stem, ext, dateTok, a.diskHash, i)
		if cand == "" {
			w.log.Error("rig: exhausted disambiguation candidates", "name", a.canonicalName, "dir", a.destDir)
			return fileDeferred
		}
		destPath := filepath.Join(a.destDir, cand)

		err := w.lfs.RenameNoReplace(a.srcPath, destPath)
		if err == nil {
			if w.crashAfterRename != nil && w.crashAfterRename(destPath) {
				// Modelled process crash after the rename, before the DB commit (design
				// routing-and-filing §9.3 crash-window row 2, INV-6): the new file is on
				// disk but the row is never updated. Abandon exactly as a crash would; the
				// caller does not reach its old-file removal, so both copies persist and the
				// row stays un-retargeted.
				return fileDeferred
			}
			disp := metrics.DispositionFresh
			if a.reclassify {
				disp = metrics.DispositionReclassified
			}
			w.recordFiled(ctx, a, destPath, a.pulledAtNew, disp)
			w.log.Info("rig: filed frame", "dest", destPath, "type", string(a.cls.Type))
			return fileFiled
		}
		if !localfs.IsExist(err) {
			w.log.Error("rig: file rename failed", "dest", destPath, "error", err)
			return fileDeferred
		}

		// Target exists: compare content with the cache-bypassing read-back.
		existingSize, _, serr := w.lfs.Stat(destPath)
		if serr != nil {
			w.log.Warn("rig: cannot stat existing dest; will retry", "dest", destPath, "error", serr)
			return fileDeferred
		}
		// Both halves are ARCHIVE-side: the question is whether the file already at
		// this name is the file this call is about to put there, and a.archiveSize is
		// that file's length. Keyed on the device length instead, a frame whose two
		// copies have diverged in length — a re-applied overlay, a superseded row —
		// skips the content compare entirely and lands on the token-and-alert path
		// with an identical copy already on disk (design frame-lifecycle §11.1).
		if existingSize == a.archiveSize {
			ehash, verr := w.lfs.VerifyRead(destPath, a.archiveSize)
			if verr != nil {
				w.log.Warn("rig: cannot verify existing dest; will retry", "dest", destPath, "error", verr)
				return fileDeferred
			}
			if ehash == a.diskHash {
				// Idempotent keep: a re-pull of a frame already proven on the NAS. For a
				// real pull this means the full file was streamed over SMB and then
				// discarded — bytes paid for and thrown away — so count them as wasted.
				// The reclassify path moved no network bytes, so it is not wasted.
				disp := metrics.DispositionAdopted
				if a.reclassify {
					disp = metrics.DispositionReclassified
				} else {
					w.metrics.AddTransferBytesWasted(w.profileID, a.lf.size)
				}
				w.recordFiled(ctx, a, destPath, a.pulledAtKeep, disp)
				if a.srcPath != destPath {
					_ = w.lfs.Remove(a.srcPath)
				}
				w.log.Info("rig: idempotent keep (existing NAS copy adopted)", "dest", destPath)
				return fileFiled
			}
		}
		// Different content at this name.
		if cand == a.canonicalName && !canonicalAlerted {
			canonicalAlerted = true
			w.notify.Raise(ctx, model.Alert{
				RigID:    w.profileID,
				Severity: model.SeverityWarn,
				Category: model.AlertRotClash,
				Message:  "content clash at canonical name " + destPath + " (possible rot); filing the new frame under a token",
			})
		}
		// Advance to the next deterministic token.
	}
}

// recordFiled upserts the frame row for a filed frame and emits frame.done. On an
// idempotent keep a prior row's pulled_at is preserved by the caller's choice of
// pulledAt (the grace clock is never restarted, design frame-lifecycle
// §8/design routing-and-filing §2).
func (w *Worker) recordFiled(ctx context.Context, a fileArgs, destPath string, pulledAt int64, disp metrics.FrameDisposition) {
	f := &model.Frame{
		RigID:       w.profileID,
		SourceShare: a.lf.share,
		SourcePath:  a.lf.path,
		// SHA256 is the device-side anchor (== diskHash for a normal file); the archive
		// anchors and the retarget overlay carry the edited archive hash separately (design
		// routing-and-filing §9.2/§9.3).
		SHA256:         a.sha256,
		DataSHA256:     a.dataSHA256,
		RetargetSHA256: a.retargetSHA256,
		RetargetFrom:   a.retargetFrom,
		ArchiveSHA256:  a.archiveSHA256,
		ArchiveSize:    a.archiveSize,
		Size:           a.lf.size,
		Mtime:          a.lf.mtime.Unix(),
		FrameType:      string(a.cls.Type),
		Target:         a.cls.Target,
		Night:          int64(a.night),
		DestPath:       destPath,
		Sidecars:       a.lf.sidecars,
		State:          a.state,
		DeletedAt:      a.deletedAt,
		PulledAt:       pulledAt,
		Astrometry:     a.astro,
	}
	// The pane coordinates come from the RAW target — the same string written to
	// frames.target and the same input the store's backfill parses — not the
	// transformed one routing splits. A migration has no config to read
	// StripTargetSpaces from, so a transformed live path and a raw backfill would
	// disagree on "NGC 7000_1-1" under the shipped strip_target_spaces, splitting one
	// mosaic across two groups. Parsed unconditionally: these are derived data
	// nothing routes on, and the detection toggle governs routing, not this
	// (design routing-and-filing §7.4).
	if base, _, row, col, ok := panel.ParsePanel(a.cls.Target); ok {
		f.PanelBase, f.PanelRow, f.PanelCol = base, row, col
	}
	if a.cls.FilterPresent {
		f.Filter = a.cls.Filter
	}
	if a.cls.ExptimePresent {
		f.Exptime = a.cls.Exptime
	}
	if a.cls.SetTempPresent {
		f.SetTemp = a.cls.SetTempC
	}
	f.SetTempAbsent = !a.cls.SetTempPresent
	if a.cls.GainPresent {
		f.Gain = a.cls.Gain
	}
	if a.cls.OffsetPresent {
		f.Offset = a.cls.Offset
	}
	if a.cls.BinPresent {
		f.Binning = binningToken(a.cls.BinX, a.cls.BinY)
	}
	if a.cls.SensorMapped {
		f.Sensor = a.cls.Sensor
	}
	if a.cls.DateObsPresent {
		f.DateObs = a.cls.DateObs.UTC().Format(time.RFC3339Nano)
	}
	if a.prior != nil {
		f.ID = a.prior.ID
	}
	if err := w.store.UpsertFrame(ctx, f); err != nil {
		// The frame transferred and verified but its row did not persist — the worst
		// outcome in the pipeline, otherwise only a single Error log.
		w.metrics.RecordFrameUpsertFailure(w.profileID)
		w.log.Error("rig: upsert filed frame failed", "dest", destPath, "error", err)
		return
	}
	w.metrics.RecordFrameFiled(w.profileID, metrics.FrameClassOf(string(a.cls.Type)), disp)
	// This key reached a terminal outcome this cycle, so it drops out of the live
	// feed's Pending set (design features §4.3). Harmless when recordFiled runs
	// off-cycle (e.g. reclassify): the key just won't match any cycle candidate.
	w.terminalThisCycle[settleKey(a.lf)] = true
	// Carry the full frame by value so the SSE boundary can render a complete row
	// (design features §4.4); the thin map is replaced by one DTO shape shared with
	// REST.
	w.hub.Emit(hub.Event{Name: "frame.done", Data: *f})
	// A freshly-filed light with no embedded WCS is a Tier-1 solve candidate — nudge
	// the solve worker for low latency. UpsertFrame set f.Astrometry to the EFFECTIVE
	// post-conflict state (RETURNING, design features §3.5.2), so a re-file of an
	// already-solved frame reports solved here and is not re-enqueued. Covers the
	// reclassify path too (it funnels through fileFrom→recordFiled).
	w.maybeEnqueueSolve(f)
	// Best-effort thumbnail warm so list pages hit the cache. Warm only dispatches
	// (try-acquire + goroutine) and never blocks this latency-sensitive goroutine.
	if w.previewer != nil {
		w.previewer.Warm(f.SHA256, f.DestPath)
	}
	// This verified file proves its destination root is a real, mounted, writable
	// library — auto-link it so the operator never has to (design
	// library-reconciliation §8). Cheap after the first frame per root (a cached
	// map lookup).
	w.ensureLibraryLinked(ctx, destPath)
}

// maybeEnqueueSolve sends a non-blocking nudge to the Tier-1 solve worker when f is
// a light frame the ASIAIR left without an embedded WCS. A full channel is fine —
// the DB is the queue of record, and the solve worker's from-zero re-sweep catches
// any dropped nudge.
func (w *Worker) maybeEnqueueSolve(f *model.Frame) {
	if w.nudge == nil || f.ID == 0 {
		return
	}
	if f.FrameType != string(model.TypeLight) || f.Astrometry == nil ||
		f.Astrometry.Status != model.AstroStatusNone {
		return
	}
	select {
	case w.nudge <- f.ID:
	default:
	}
}

// markFailed records a transfer/parse failure as a failed row with exponential
// backoff and raises the transfer-failed alert (design frame-lifecycle §4.3). A
// frame that exhausts its attempt budget stays failed (a permanent-failure
// alert) until Run now grants it another attempt, rather than being hammered.
func (w *Worker) markFailed(ctx context.Context, cfg *model.Config, lf listedFrame, prior *model.Frame, reason model.FailReason, msg string) {
	attempts := 1
	if prior != nil {
		attempts = prior.Attempts + 1
	}
	fr := cfg.Defaults.FailedRetry
	exhausted := fr.MaxAttempts > 0 && attempts >= fr.MaxAttempts
	// An exhausted frame has no scheduled retry, only the operator's Run now
	// (design frame-lifecycle §4.3), so it records none rather than a time the UI
	// would show as "the next try".
	var nextRetry int64
	if !exhausted {
		nextRetry = w.now().Add(computeBackoff(fr, attempts)).Unix()
	}

	f := &model.Frame{
		RigID:       w.profileID,
		SourceShare: lf.share,
		SourcePath:  lf.path,
		Size:        lf.size,
		Mtime:       lf.mtime.Unix(),
		State:       model.StateFailed,
		Attempts:    attempts,
		NextRetryAt: nextRetry,
		Error:       msg,
	}
	if prior != nil {
		f.ID = prior.ID
	}
	upsertErr := w.store.UpsertFrame(ctx, f)
	if upsertErr != nil {
		w.log.Error("rig: upsert failed frame failed", "path", lf.path, "error", upsertErr)
	}
	w.log.Warn("rig: frame failed", "path", lf.path, "reason", string(reason), "attempts", attempts, "error", msg)
	// The transfer genuinely failed, so alert regardless of whether the row persisted.
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityWarn,
		Category: model.AlertTransferFailed,
		Message:  "transfer failed for " + lf.path + ": " + msg,
	})
	// Reflect the failure in the live feed only when it actually persisted (lockstep
	// with the store, as recordFiled/markDeleted do): mark the key terminal so it
	// leaves the Pending set and show the "failed" pill. On an upsert error the frame
	// stays pending and re-pulls next cycle rather than showing a failed row with no
	// backing DB record. (A transient abort never calls markFailed, so it too stays
	// in Pending and keeps retrying — design features §4.3.)
	if upsertErr == nil {
		w.terminalThisCycle[settleKey(lf)] = true
		w.hub.Emit(hub.Event{Name: "frame.failed", Data: *f})
	}
	if exhausted {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityCritical,
			Category: model.AlertTransferFailed,
			Message:  "frame permanently failed after " + strconv.Itoa(attempts) + " attempts: " + lf.path,
		})
	}
}

// runReclassify re-runs classification over this rig's unclassified frames and
// re-files any that now resolve, via the same atomic disambiguating rename, with
// pulled_at preserved (design routing-and-filing §2). It runs on the worker
// goroutine, never in an HTTP handler, so the moves never race live filing.
func (w *Worker) runReclassify(ctx context.Context) {
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	if profile == nil {
		return
	}
	frames, err := w.store.UnclassifiedFrames(ctx, w.profileID)
	if err != nil {
		w.log.Error("rig: reclassify list failed", "error", err)
		return
	}
	moved := 0
	for i := range frames {
		if ctx.Err() != nil {
			break
		}
		// Each frame is reclassified under its own recover(): a malformed header or
		// an unexpected Card value must turn into "leave it unclassified", never a
		// panic that escapes this command handler and kills the worker goroutine —
		// which, on the idle-dispatched path, has no other recover above it (design
		// frame-lifecycle §10, design architecture §12). One bad frame is skipped;
		// the rest of the batch still proceeds.
		if w.reclassifyOneGuarded(ctx, cfg, profile, &frames[i]) {
			moved++
		}
	}
	// Clear stale "unclassified" warnings: any whose frame is no longer in the
	// unclassified set (re-filed correctly now or in an earlier run) is resolved,
	// so the operator isn't left acking a backlog by hand. Frames that are still
	// genuinely unclassified keep their warning.
	w.ackResolvedUnclassified(ctx)
	w.log.Info("rig: reclassify complete", "examined", len(frames), "moved", moved)
	// Report the outcome to whoever asked for it. A reclassify re-files frames that
	// are already on the NAS, so it produces no transfer to watch and no row in the
	// live feed — without this the operator is told the pass started and never told
	// anything again, which reads as "nothing happened".
	w.hub.Emit(hub.Event{Name: "rig.reclassified", Data: map[string]any{
		"rig_id":   w.profileID,
		"examined": len(frames),
		"moved":    moved,
	}})
}

// ackResolvedUnclassified acknowledges every "unclassified" alert for this rig
// whose frame is no longer unclassified, correlating by the source path carried in
// the alert message, and refreshes the hub's unacked count.
func (w *Worker) ackResolvedUnclassified(ctx context.Context) {
	remaining, err := w.store.UnclassifiedFrames(ctx, w.profileID)
	if err != nil {
		w.log.Warn("rig: could not list unclassified frames to auto-ack", "error", err)
		return
	}
	stillUnclassified := make(map[string]bool, len(remaining))
	for _, f := range remaining {
		stillUnclassified[f.SourcePath] = true
	}

	alerts, err := w.store.ListAlerts(ctx, false, 5000)
	if err != nil {
		w.log.Warn("rig: could not list alerts to auto-ack reclassified frames", "error", err)
		return
	}
	now := w.now().Unix()
	acked := 0
	for _, a := range alerts {
		if a.RigID != w.profileID || a.Category != model.AlertUnclassified {
			continue
		}
		// The message is "unclassified frame <sourcePath>: <reason>" (fileUnclassified).
		p, ok := unclassifiedAlertPath(a.Message)
		if !ok || stillUnclassified[p] {
			continue // unparseable, or the frame is still genuinely unclassified
		}
		if err := w.store.AckAlert(ctx, a.ID, now); err == nil {
			acked++
		}
	}
	if acked > 0 {
		if n, err := w.store.UnackedCount(ctx); err == nil {
			w.hub.SetUnackedAlerts(n)
		}
		w.log.Info("rig: auto-acked resolved unclassified alerts", "count", acked)
	}
}

// unclassifiedAlertPath extracts the source path from an unclassified alert
// message of the form "unclassified frame <path>: <reason>".
func unclassifiedAlertPath(msg string) (string, bool) {
	const prefix = "unclassified frame "
	rest, ok := strings.CutPrefix(msg, prefix)
	if !ok {
		return "", false
	}
	path, _, ok := strings.Cut(rest, ": ")
	if !ok {
		return "", false
	}
	return path, true
}

// reclassifyOneGuarded runs reclassifyOne under a recover so a panic on one frame
// leaves it unclassified and the worker survives.
func (w *Worker) reclassifyOneGuarded(ctx context.Context, cfg *model.Config, profile *model.RigProfile, fp *model.Frame) (moved bool) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("rig: reclassify panic on frame; leaving unclassified", "path", fp.DestPath, "panic", r)
			moved = false
		}
	}()
	return w.reclassifyOne(ctx, cfg, profile, fp)
}

// reclassifyOne re-runs classification+routing over one previously-unclassified
// frame and, if it now resolves, moves it via the same atomic disambiguating
// filing (grace anchor preserved). Returns whether it was moved.
func (w *Worker) reclassifyOne(ctx context.Context, cfg *model.Config, profile *model.RigProfile, fp *model.Frame) bool {
	f := *fp
	h, err := w.hdr.ReadHeader(f.DestPath)
	if err != nil {
		return false // still unreadable → stays unclassified
	}
	// The header is already in hand here, so populate astrometry on the move too —
	// zero extra I/O, and it closes the NULL window for a reclassified frame.
	astro := astrometry.FromHeader(h)
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	if cls.Type == model.TypeUnknown {
		return false
	}
	var night int
	var obs time.Time
	if cls.DateObsPresent {
		obs = route.ObservingDate(cls.DateObs, w.siteLoc)
	}
	opts := routeOptionsFor(cfg, obs)
	if cls.Type == model.TypeLight {
		if !cls.TargetPresent || !cls.DateObsPresent {
			return false
		}
		n, err := w.nightFor(ctx, cfg, profile, &cls, opts)
		var qe *route.QuarantineError
		switch {
		case err == nil:
			night = n
		case errors.As(err, &qe):
			// Nothing to consult and nothing to spend: fall through with night 0 and
			// let route.Route re-derive the same quarantine, leaving the frame
			// unclassified as it already is.
			w.log.Debug("rig: reclassify night folder quarantine", "path", f.DestPath, "reason", qe.Error())
		default:
			w.log.Debug("rig: reclassify assign night failed; retrying next cycle", "path", f.DestPath, "error", err)
			return false
		}
	}
	opts.Night = night
	res, rerr := route.Route(cls, *profile, opts)
	if rerr != nil {
		return false // still unresolvable → stays unclassified
	}
	root := cfg.Roots.Light
	if res.Kind == route.RootCal {
		root = cfg.Roots.Cal
	}
	destDir := joinLocal(root, res.Dir)
	name := w.candidateBaseName(filepath.Base(f.DestPath), f.SHA256)
	lf := listedFrame{
		share:    f.SourceShare,
		path:     f.SourcePath,
		name:     name,
		size:     f.Size,
		mtime:    time.Unix(f.Mtime, 0),
		sidecars: f.Sidecars,
	}
	// A reclassify moves an existing NAS file and reads no device bytes, so it
	// carries the row's own device anchor and can never present itself as a revision:
	// the filing path's third outcome is unreachable from here.
	return w.fileFrom(ctx, fileArgs{
		lf:       lf,
		prior:    fp,
		cls:      cls,
		astro:    astro,
		night:    night,
		srcPath:  f.DestPath, // move the existing NAS file
		diskHash: f.SHA256,   // already proven at rest when first filed
		sha256:   f.SHA256,   // an unclassified frame is never retargeted: sha256 == diskHash
		// A local rename changes no bytes, so the archive anchors are carried through
		// rather than re-derived: the file this moves is the one they already describe.
		archiveSHA256: f.ArchiveSHA256,
		archiveSize:   f.ArchiveSize,
		destDir:       destDir,
		canonicalName: name,
		state:         model.StateVerified,
		pulledAtNew:   f.PulledAt, // PRESERVE the grace anchor, even on success
		pulledAtKeep:  f.PulledAt,
		reclassify:    true, // a local rename of an existing NAS file — zero network bytes
	}) == fileFiled
}

// --- small helpers -------------------------------------------------------------

// candidateBaseName sanitizes a source basename into a single path component,
// falling back to a content-derived name if it sanitizes away entirely.
func (w *Worker) candidateBaseName(name, diskHash string) string {
	s := sanitize.Component(name)
	if s != "" {
		return s
	}
	_, ext := splitExt(name)
	if ext == "" {
		ext = ".fit"
	}
	return "frame_" + shortHash(diskHash) + ext
}

// candidateName renders the i-th deterministic filing candidate: index 0 is the
// canonical name, then (if present) the DATE-OBS token, then lengthening sha256
// prefixes until free or exhausted (design frame-lifecycle §8). It returns ""
// once the sha256 is exhausted (a terminating guard; two files with the same
// full sha256 are byte-identical and resolve as an idempotent keep before this
// point).
func candidateName(stem, ext, dateTok, hash string, i int) string {
	if i == 0 {
		return stem + ext
	}
	idx := i - 1
	prefix := stem
	if dateTok != "" {
		prefix = stem + "_" + dateTok
		if idx == 0 {
			return prefix + ext
		}
		idx--
	}
	length := 8 + idx*2
	if length > len(hash) {
		return ""
	}
	return prefix + "_" + hash[:length] + ext
}

// splitExt splits a file name into its stem and extension (the extension keeps
// its leading dot; a dotfile has no extension).
func splitExt(name string) (stem, ext string) {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i], name[i:]
	}
	return name, ""
}

// binningToken renders the binning column value ("Bin1" / "Bin1x2").
func binningToken(x, y int64) string {
	if x == y {
		return "Bin" + strconv.FormatInt(x, 10)
	}
	return "Bin" + strconv.FormatInt(x, 10) + "x" + strconv.FormatInt(y, 10)
}

// joinLocal joins a managed root and the router's forward-slash relative
// directory into one host-native local path. The root is already OS-native (the
// operator typed it — "\" on Windows) and the router's relative part is
// "/"-separated, so filepath.Join rewrites the whole result to the OS separator:
// a filed dest_path is therefore internally consistent and never mixes "\" and
// "/". The local-FS seam applies any \\?\ extension on top internally.
func joinLocal(base, rel string) string {
	return filepath.Join(base, rel)
}

// shortHash returns the first 12 chars of a hex hash (or the whole thing).
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// priorPulledAt returns the prior row's pulled_at (the preserved grace anchor) or
// the fallback when there is no prior row.
func priorPulledAt(prior *model.Frame, fallback int64) int64 {
	if prior != nil && prior.PulledAt != 0 {
		return prior.PulledAt
	}
	return fallback
}

// computeBackoff returns base*factor^(attempts-1) capped at max (design
// frame-lifecycle §4).
func computeBackoff(fr model.FailedRetry, attempts int) time.Duration {
	base := fr.Base.D()
	if base <= 0 {
		base = 2 * time.Minute
	}
	factor := fr.Factor
	if factor < 1 {
		factor = 2
	}
	d := float64(base)
	for i := 1; i < attempts; i++ {
		d *= factor
	}
	if max := fr.Max.D(); max > 0 && time.Duration(d) > max {
		return max
	}
	return time.Duration(d)
}
