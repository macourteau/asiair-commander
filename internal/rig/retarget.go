package rig

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/sanitize"
)

// archiveHash is the single accessor that generalizes every hash comparison in the
// codebase (design routing-and-filing §9.5): "what the file on the NAS should hash
// to." It is a stored value rather than a derivation, because the fallback it used
// to make — RetargetSHA256 when an overlay exists, SHA256 otherwise — rested on the
// device original hashing to SHA256 for the frame's whole life, which a device-side
// revision falsifies: once a supersede advances SHA256, a derived expectation would
// silently move the archive-side expectation of every frame carrying no overlay.
// Device-side reads keep comparing to f.SHA256, and archive-side reads take their
// length from f.ArchiveSize for the same reason (design frame-lifecycle §11.1).
func archiveHash(f model.Frame) string {
	return f.ArchiveSHA256
}

// retargetFrame runs the design routing-and-filing §9.3 seven-step retarget operation
// for one frame on the worker goroutine (never touching the device original): verify the
// archive is intact, rewrite its OBJECT card into a staging temp, prove the edit changed
// only the label, file the edited temp to a NEW disambiguated path, and remove the OLD
// file only after the DB commit points at the new one. sha256 stays the original;
// retarget_sha256 becomes the edited archive's hash. The frame's state is preserved (a
// deleted archive relabel stays deleted).
func (w *Worker) retargetFrame(ctx context.Context, cfg *model.Config, profile *model.RigProfile, f model.Frame, newTarget string) (model.RetargetResult, error) {
	res := model.RetargetResult{FrameID: f.ID, OldPath: f.DestPath}

	// 1. Precondition guards. Retarget is restricted to a verified or deleted frame
	// that still has an archive to edit; a state change is out of scope.
	if f.State != model.StateVerified && f.State != model.StateDeleted {
		res.Status = "skipped"
		res.Reason = "frame state " + string(f.State) + " is not retargetable"
		return res, nil
	}
	if f.DestPath == "" {
		res.Status = "skipped"
		res.Reason = "frame has no archive path"
		return res, nil
	}
	sanitized := sanitize.Component(newTarget)
	if sanitized == "" {
		res.Status = "skipped"
		res.Reason = "new target is empty after sanitize"
		return res, nil
	}
	// Fold onto the spelling this target was already filed under before the card is
	// written, not after: filing folds the row's target too, and a card that
	// disagreed with its own row would fail the re-apply hash check on every later
	// re-pull. Typing a target the archive already holds in another case is not a
	// rename — the folders are one — so the no-op guard below sees it as one.
	canonical, err := w.store.CanonicalTarget(ctx, sanitized)
	if err != nil {
		res.Status = "failed"
		res.Reason = "canonical target: " + err.Error()
		return res, err
	}
	// Re-sanitized because the ledger holds RAW OBJECT spellings: a canonical
	// "NGC:7380" must come back as the "NGC_7380" every other consumer of this
	// variable expects. Sanitizing is idempotent, so a pure case fold is untouched.
	sanitized = sanitize.Component(canonical)
	if sanitized == f.Target {
		res.Status = "skipped"
		res.Reason = "new target equals the current target"
		return res, nil
	}

	// 2. Admission gate. The edited temp transiently adds ~1× the ARCHIVE copy's size
	// before the old copy is removed — the temp is a rewrite of the file at DestPath, so
	// its length is ArchiveSize — and gates on destination space like the delete pass does.
	// The work touches only the NAS, so it is admitted on the local site whichever
	// site the rig's device is reached through.
	release, err := w.adm.Acquire(ctx, Requester{Rig: w.profileID, Site: netsite.Local}, f.ArchiveSize)
	if err != nil {
		res.Status = "failed"
		res.Reason = "admission: " + err.Error()
		return res, err
	}
	defer release()

	// 3. Verify the archive is intact before editing — never edit a drifted file
	// (INV-3). Archive side throughout: archiveHash for the content, ArchiveSize for
	// the length.
	atHash, verr := w.lfs.VerifyRead(f.DestPath, f.ArchiveSize)
	if verr != nil {
		res.Status = "failed"
		res.Reason = "archive verify: " + verr.Error()
		return res, nil
	}
	if atHash != archiveHash(f) {
		res.Status = "failed"
		res.Reason = "archive content drifted since filing; refusing to edit"
		w.log.Warn("rig: retarget refused; archive hash drifted", "dest", f.DestPath)
		return res, nil
	}

	// 4. Rewrite the OBJECT card to a temp in the staging root (same filesystem as the
	// dest, so RenameNoReplace works), named distinctly from restore's temp so the
	// staging-startup sweep can reclaim a stale one.
	staging := cfg.Roots.Staging
	if err := w.lfs.MkdirAll(staging); err != nil {
		res.Status = "failed"
		res.Reason = "staging mkdir: " + err.Error()
		return res, nil
	}
	tmp, err := os.CreateTemp(staging, "asiair-retarget-*.tmp")
	if err != nil {
		res.Status = "failed"
		res.Reason = "staging temp: " + err.Error()
		return res, nil
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = w.lfs.Remove(tmpPath) }()

	// The canonical spelling, not the raw one the operator typed: RewriteObject
	// sanitizes what it is handed, and the card has to end up saying what the row
	// will say once filing folds the target.
	rr, rerr := w.rewriter(f.DestPath, tmpPath, sanitized)
	if rerr != nil {
		res.Status = "failed"
		res.Reason = "rewrite OBJECT: " + rerr.Error()
		return res, nil
	}

	// 5. Faithfulness assertion (real, not "by construction"): the complement digests
	// prove every byte outside the OBJECT card survived, the size is unchanged, and the
	// re-classified target matches the requested label (closing the one gap the
	// complement cannot see — garbage written INSIDE the card window) (INV-4).
	if err := w.assertRetargetEdit(cfg, tmpPath, rr, f.ArchiveSize, sanitized); err != nil {
		res.Status = "failed"
		res.Reason = err.Error()
		w.log.Error("rig: retarget edit failed faithfulness check; refusing", "dest", f.DestPath, "error", err)
		return res, nil
	}

	// 6. Hash the edited temp and file it to a NEW path. sha256 stays put — a retarget
	// never touches the device original, so the device anchor has nothing to advance to;
	// diskHash/archive_sha256/retarget_sha256 carry the edited archive hash; retarget_from
	// holds the FIRST original target across repeated retargets; the grace anchor and
	// the frame state are preserved.
	newHash, verr := w.lfs.VerifyRead(tmpPath, f.ArchiveSize)
	if verr != nil {
		res.Status = "failed"
		res.Reason = "edited temp verify: " + verr.Error()
		return res, nil
	}
	// The at-rest read-back MUST equal the stream-derived expectation (the digest the
	// rewriter computed over the same bytes it wrote). Only VerifyRead's own read-back
	// otherwise anchors retarget_sha256, so a write fault between the streamed write and
	// the durable file would mint the safety anchor from corrupted bytes and later
	// authorize deleting the sole device original against a corrupted archive (INV-4).
	// Refuse before filing: the temp is removed by the defer, the old archive and the
	// device original are untouched, and the overlay is never set.
	if newHash != hex.EncodeToString(rr.OutputSHA256[:]) {
		res.Status = "failed"
		res.Reason = "at-rest verify mismatch after edit (write fault)"
		w.log.Error("rig: retarget refused; at-rest hash != streamed output hash (write fault)",
			"dest", f.DestPath, "atRest", newHash, "streamed", hex.EncodeToString(rr.OutputSHA256[:]))
		return res, nil
	}
	retargetFrom := f.RetargetFrom
	if retargetFrom == "" {
		retargetFrom = f.Target
	}
	filed := w.fileEditedArchive(ctx, cfg, profile, f, tmpPath, newHash, retargetFrom)
	if !filed {
		res.Status = "failed"
		res.Reason = "could not file the edited archive"
		// The OLD file is untouched (step 7 not reached): no data loss, un-retargeted.
		return res, nil
	}

	// 7. Remove the OLD file — ONLY after step 6's commit is PROVEN to have persisted.
	// fileFrom swallows an UpsertFrame failure (it still returns true), so re-read the
	// row and require the retarget overlay to have committed (retarget_sha256 == the
	// edited archive hash) before reporting success or removing the old archive. An
	// unproven commit (row re-read failed, or the overlay did not persist) leaves the
	// old archive and the device original intact and the row un-retargeted.
	fresh, ok, ferr := w.store.GetFrameByID(ctx, f.ID)
	if ferr != nil || !ok {
		res.Status = "failed"
		res.Reason = "edited archive filed but the row could not be re-read"
		return res, nil
	}
	res.NewPath = fresh.DestPath
	if fresh.RetargetSHA256 != newHash {
		res.Status = "failed"
		res.Reason = "edited archive filed but the row did not persist"
		return res, nil
	}
	if fresh.DestPath != "" && fresh.DestPath != f.DestPath {
		if err := w.lfs.Remove(f.DestPath); err != nil {
			// A cosmetic orphan (design routing-and-filing §9.3 crash-window row 3),
			// never data loss — the new archive is committed and proven.
			w.log.Warn("rig: retarget filed but old archive not removed (orphan)", "old", f.DestPath, "error", err)
		}
	}
	res.Status = "retargeted"
	w.log.Info("rig: retargeted frame", "frame", f.ID, "from", f.Target, "to", sanitized, "old", f.DestPath, "new", res.NewPath)
	return res, nil
}

// fileEditedArchive files the edited staging temp to its re-derived folder, mirroring
// reclassifyOne (a local rename with the grace anchor preserved) but with the sha256/
// diskHash split and the retarget overlay made explicit: sha256 is the device-side anchor,
// unmoved because the device original is not touched here, while diskHash, the archive
// anchors and retarget_sha256 all carry the edited archive hash, and the frame state is
// carried through so a deleted archive relabel stays deleted. It returns whether the
// frame reached a terminal filed outcome.
func (w *Worker) fileEditedArchive(ctx context.Context, cfg *model.Config, profile *model.RigProfile, f model.Frame, tmpPath, newHash, retargetFrom string) bool {
	h, err := w.hdr.ReadHeader(tmpPath)
	if err != nil {
		w.log.Error("rig: retarget re-read edited header failed", "error", err)
		return false
	}
	astro := astrometry.FromHeader(h)
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	if cls.Type == model.TypeUnknown {
		w.log.Error("rig: retarget edited frame has unknown IMAGETYP; refusing")
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
			w.log.Error("rig: retarget edited light missing OBJECT/DATE-OBS; refusing")
			return false
		}
		n, err := w.nightFor(ctx, cfg, profile, &cls, opts)
		var qe *route.QuarantineError
		switch {
		case err == nil:
			night = n
		case errors.As(err, &qe):
			// Fall through with night 0: route.Route re-derives the same quarantine
			// below and refuses the retarget there, rather than here as a transient
			// store failure that would be retried forever.
			w.log.Warn("rig: retarget night folder quarantine", "frame", f.ID, "reason", qe.Error())
		default:
			w.log.Error("rig: retarget assign night failed", "error", err)
			return false
		}
	}
	opts.Night = night
	res, rerr := route.Route(cls, *profile, opts)
	if rerr != nil {
		w.log.Error("rig: retarget routing failed; refusing", "error", rerr)
		return false
	}
	root := cfg.Roots.Light
	if res.Kind == route.RootCal {
		root = cfg.Roots.Cal
	}
	destDir := joinLocal(root, res.Dir)
	name := w.candidateBaseName(filepath.Base(f.DestPath), archiveHash(f))
	lf := listedFrame{
		share:    f.SourceShare,
		path:     f.SourcePath,
		name:     name,
		size:     f.Size,
		mtime:    time.Unix(f.Mtime, 0),
		sidecars: f.Sidecars,
	}
	// A retarget files an EDITED archive copy and never touches the device original,
	// so it carries the row's own device anchor and can never present itself as a
	// revision: the filing path's third outcome is unreachable from here, and this
	// path's contract stays "did the edited copy land".
	return w.fileFrom(ctx, fileArgs{
		lf:             lf,
		prior:          &f,
		cls:            cls,
		astro:          astro,
		night:          night,
		srcPath:        tmpPath,
		diskHash:       newHash,  // the edited archive's on-disk hash
		sha256:         f.SHA256, // the device anchor: the device original is never touched
		retargetSHA256: newHash,  // the audit record: what the re-label produced
		retargetFrom:   retargetFrom,
		// The archive side describes the EDITED bytes — the copy that lands — and the
		// length they were verified at, which a rewrite preserves.
		archiveSHA256: newHash,
		archiveSize:   f.ArchiveSize,
		destDir:       destDir,
		canonicalName: name,
		state:         f.State,     // preserve deleted; do not resurrect to verified
		deletedAt:     f.DeletedAt, // preserve the reclaim clock; do not null a deleted relabel
		pulledAtNew:   f.PulledAt,
		pulledAtKeep:  f.PulledAt,
		reclassify:    true, // a local rename of an edited temp — zero network bytes
	}) == fileFiled
}

// assertRetargetEdit is the design routing-and-filing §9.3 step 5 faithfulness proof,
// shared by retargetFrame and every re-apply path: the two complement digests must be
// equal (every byte outside the OBJECT card byte-identical between source and edit), the
// size unchanged, and the edited header must re-classify to the requested (sanitized)
// target. Any mismatch means a corrupting writer bug or a garbled card — refuse before
// anything is committed.
//
// wantSize is the length of the FILE BEING PROVED — the file the edit was made from —
// and which length that is depends on which of the two callers is asking. An
// operator's retarget edits the archive copy in place, so it hands ArchiveSize:
// handing it a device length would refuse the operation with "edited size !=
// original" on any frame whose two copies have diverged (design frame-lifecycle
// §11.1). The re-apply edits a file freshly streamed off the DEVICE, so it hands
// that file's own length — and it is also the restore's only route to this
// assertion, readRestoreSource re-applying through it rather than calling here
// itself, which is why no caller hands an archive length for a device copy. That
// anchoring is what makes the assertion hold for a frame the device has revised
// (design frame-lifecycle §11.11). Either way the Stat compare below is a comparison
// against the file in hand that spends no read, and so is invisible to a rule
// written over VerifyRead alone.
func (w *Worker) assertRetargetEdit(cfg *model.Config, editedPath string, rr header.RewriteResult, wantSize int64, wantTarget string) error {
	if rr.ComplementIn != rr.ComplementOut {
		return errors.New("retarget: complement digest mismatch — the edit changed bytes outside the OBJECT card")
	}
	sz, _, err := w.lfs.Stat(editedPath)
	if err != nil {
		return fmt.Errorf("retarget: stat edited temp: %w", err)
	}
	if sz != wantSize {
		return fmt.Errorf("retarget: edited size %d != original %d", sz, wantSize)
	}
	h, err := w.hdr.ReadHeader(editedPath)
	if err != nil {
		return fmt.Errorf("retarget: re-read edited header: %w", err)
	}
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	if want := sanitize.Component(wantTarget); cls.Target != want {
		return fmt.Errorf("retarget: edited OBJECT %q does not classify to %q", cls.Target, want)
	}
	return nil
}

// reapplyRetargetIfNeeded is the design routing-and-filing §9.4 re-apply rule at the
// pull/classify filing chokepoint. When freshly-streamed device bytes are the frame
// whose OBJECT card was edited (prior.RetargetSHA256 != ""), it re-applies that edit
// into a second temp so the archive keeps its corrected label instead of reverting
// to the original folder. It returns (tmp2, the edited file's at-rest hash) after
// proving the edit faithful, or (tmp, streamHash) unchanged when no re-apply is
// needed (no prior row, prior not retargeted, or genuinely-new content — which
// resets the overlay).
//
// "The frame whose card was edited" is a disjunction (design frame-lifecycle §11.11):
// the incoming data unit is the exposure the row records, OR the incoming bytes are
// byte-identical to the device original the row records. The first arm is what the
// rule always meant and could not express before, a device-side revision rewriting
// the header of an exposure already pulled; the second is the test as it stood, kept
// because a byte-identical re-pull is the same exposure by definition and because a
// row the backfill has not reached carries no data_sha256 at all, which no incoming
// file matches — replacing the test outright would reset the overlay on an ordinary
// re-pull. IsUsableDataDigest is the precondition on the first arm: without it two
// header-only frames match on the one digest every zero-length data unit shares, the
// overlay is re-applied to a file it does not describe, and the frame is deferred
// forever.
//
// incomingSize is the length of the file at tmp, verified at rest by the caller. All
// three faithfulness assertions are anchored to it rather than to the row's recorded
// lengths: they are the INV-4 assertions that authorize deleting a device original,
// and the claim they prove — the edit changed exactly the OBJECT card and left the
// science data untouched — is proved of the file actually in hand, exactly as it was
// proved of the pre-revision one. None of them is relaxed.
func (w *Worker) reapplyRetargetIfNeeded(cfg *model.Config, tmp, streamHash, dataHash string, incomingSize int64, prior *model.Frame) (string, string, error) {
	if prior == nil || prior.RetargetSHA256 == "" {
		return tmp, streamHash, nil
	}
	sameExposure := header.IsUsableDataDigest(dataHash) && dataHash == prior.DataSHA256
	if !sameExposure && streamHash != prior.SHA256 {
		return tmp, streamHash, nil
	}
	tmp2, err := os.CreateTemp(filepath.Dir(tmp), "asiair-retarget-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("re-apply retarget temp: %w", err)
	}
	tmp2Path := tmp2.Name()
	_ = tmp2.Close()
	rr, rerr := w.rewriter(tmp, tmp2Path, prior.Target)
	if rerr != nil {
		_ = w.lfs.Remove(tmp2Path)
		return "", "", fmt.Errorf("re-apply rewrite OBJECT: %w", rerr)
	}
	if err := w.assertRetargetEdit(cfg, tmp2Path, rr, incomingSize, prior.Target); err != nil {
		_ = w.lfs.Remove(tmp2Path)
		return "", "", err
	}
	h, verr := w.lfs.VerifyRead(tmp2Path, incomingSize)
	if verr != nil {
		_ = w.lfs.Remove(tmp2Path)
		return "", "", fmt.Errorf("re-apply at-rest verify: %w", verr)
	}
	// The at-rest read-back is compared to the digest the rewriter computed over every
	// byte it wrote, which is the comparison retargetFrame already makes at its own
	// site. Compared to prior.RetargetSHA256 instead, a revised frame fails here on the
	// header block the device grew and is never filed at all; and that hash proves
	// nothing about THIS rewrite, while rr.OutputSHA256 does. It keeps its read-back
	// because it is the only at-rest verification on this path: without it a write
	// fault between the streamed write and the durable file would mint the archive
	// anchor from corrupted bytes (INV-4).
	if want := hex.EncodeToString(rr.OutputSHA256[:]); h != want {
		_ = w.lfs.Remove(tmp2Path)
		return "", "", fmt.Errorf("re-apply at-rest hash %s != the rewriter's output hash %s (write fault)", h, want)
	}
	return tmp2Path, h, nil
}

// nightProjector projects the ordinal apply would issue to each frame of one preview
// batch, without writing: AssignNight is never called from a preview, because a
// previewed-then-abandoned retarget must not permanently spend an ordinal
// (design routing-and-filing §6.5).
//
// It reads the destination folder through the same nightHints the filing path uses
// rather than deriving it a second way — two implementations of that read is exactly
// the divergence the design exists to eliminate — and carries three distinct pieces of
// state, because conflating any two of them breaks the projection.
type nightProjector struct {
	w       *Worker
	cfg     *model.Config
	profile *model.RigProfile
	// target is the scope's target half: the sanitized string, because
	// RewriteObject sanitizes before writing the OBJECT card, so that is what apply
	// re-classifies and keys the ledger on.
	target string

	// folders caches one folder read per (prefix, observing date). The date belongs
	// in the key because the matching count is a function of the date window: a
	// prefix-only key would hand every frame of a multi-date batch the first
	// frame's answer.
	folders map[string]nightHint
	// resolved memoizes ordinals by observing date ALONE. Apply's LookupNight hit is
	// scope-wide, so under a template that varies above the night segment two frames
	// of one date in different prefixes still get one ordinal; a (prefix, date) key
	// would show two. It also makes a repeated id — handleRetargetApply does not
	// dedupe — cost nothing.
	resolved map[string]projectedNight
	// runningMax is advanced by every arm that takes an ordinal — mints, adoptions
	// and the error arm — because each changes what the next frame of the same apply
	// would be issued. Advancing on an adoption matters when a batch spans several
	// prefixes; within one prefix it costs nothing, since an adopted ordinal is
	// never above that folder's high-water mark.
	runningMax int

	scopeMax   int
	scopeMaxOK bool
}

// projectedNight is one frame's projected ordinal. exact is true only for a date the
// ledger has already assigned: that one cannot move, because an assigned night is
// never renumbered, while an adoption or a mint can be shifted by anything filed
// between the preview and the apply.
type projectedNight struct {
	night int
	exact bool
}

// project resolves the ordinal one light frame would file under, and the raw target
// spellings already filed in the folder that ordinal names.
func (p *nightProjector) project(ctx context.Context, cls model.Classification, opts route.Options) (projectedNight, []string, error) {
	key := route.ObservingDateKey(opts.ObservingDate)
	h, err := p.folderHint(ctx, cls, opts, key)
	if err != nil {
		return projectedNight{}, nil, err
	}
	pn, err := p.ordinal(ctx, key, h)
	if err != nil {
		return projectedNight{}, nil, err
	}
	spellings, err := p.spellings(ctx, h, pn.night)
	if err != nil {
		return projectedNight{}, nil, err
	}
	return pn, spellings, nil
}

// folderHint returns what the frame's destination folder holds, one read per
// (prefix, observing date). The prefix is rendered here only to key the cache —
// rendering is pure and free of I/O — while the read itself stays in nightHints, so
// the preview and the filing path cannot disagree about what a folder holds.
func (p *nightProjector) folderHint(ctx context.Context, cls model.Classification, opts route.Options, dateKey string) (nightHint, error) {
	prefix, _, ok, err := route.NightFolder(cls, *p.profile, opts)
	if err != nil {
		return nightHint{}, err
	}
	if !ok {
		return nightHint{}, nil
	}
	key := prefix + "\x00" + dateKey
	if h, cached := p.folders[key]; cached {
		return h, nil
	}
	h, err := p.w.nightHints(ctx, p.cfg, p.profile, cls, opts)
	if err != nil {
		return nightHint{}, err
	}
	p.folders[key] = h
	return h, nil
}

// ordinal is the three-armed projection of design routing-and-filing §6.5: a date
// the ledger has already assigned keeps its ordinal; a folder holding this date
// lends its own, unless another date in the scope already holds it; otherwise the
// ordinal is apply's own mint formula, the scope maximum included, advanced by what
// this batch has already taken.
func (p *nightProjector) ordinal(ctx context.Context, dateKey string, h nightHint) (projectedNight, error) {
	if pn, ok := p.resolved[dateKey]; ok {
		return pn, nil
	}
	n, assigned, err := p.w.store.LookupNight(ctx, p.target, p.profile.OTA, dateKey)
	if err != nil {
		return projectedNight{}, err
	}
	if assigned {
		pn := projectedNight{night: n, exact: true}
		p.resolved[dateKey] = pn
		return pn, nil
	}
	if !p.scopeMaxOK {
		m, merr := p.w.store.MaxNight(ctx, p.target, p.profile.OTA)
		if merr != nil {
			return projectedNight{}, merr
		}
		p.scopeMax, p.scopeMaxOK = m, true
	}

	night := 0
	if h.Preferred >= 1 {
		held, herr := p.heldByOtherDate(ctx, h.Preferred, dateKey)
		if herr != nil {
			return projectedNight{}, herr
		}
		if !held {
			night = h.Preferred
		}
	}
	if night == 0 {
		night = p.scopeMax
		if h.FolderHigh > night {
			night = h.FolderHigh
		}
		if p.runningMax > night {
			night = p.runningMax
		}
		night++
	}
	if night > p.runningMax {
		p.runningMax = night
	}
	pn := projectedNight{night: night}
	p.resolved[dateKey] = pn
	return pn, nil
}

// heldByOtherDate is AssignNight's rule 2 asked as a read: an ordinal some other
// observing date in this scope already holds is not adoptable, and a projection that
// skipped the rule would state an ordinal apply refuses to issue. The batch's own
// projections are consulted alongside the ledger, because an earlier frame of this
// same apply may already have taken the ordinal — a preview writes nothing, so the
// ledger cannot yet know that.
func (p *nightProjector) heldByOtherDate(ctx context.Context, night int, dateKey string) (bool, error) {
	for date, pn := range p.resolved {
		if pn.night == night && date != dateKey {
			return true, nil
		}
	}
	return p.w.store.NightHeldByOtherDate(ctx, p.target, p.profile.OTA, night, dateKey)
}

// spellings reports the raw OBJECT spellings already filed in the resolved night
// folder, when they differ from the target as it will be WRITTEN — RewriteObject
// sanitizes before writing the card, so comparing against the typed string would
// report a spurious divergence for "NGC:7380". Unifying the night does not unify the
// archive: anything grouping by OBJECT, re-file included, still sees two targets.
//
// It reads the resolved folder rather than the prefix, or a plain-target retarget
// under the shipped mosaic template would report every panel's spelling. On the mint
// arm the folder does not exist yet, so the answer is empty by construction rather
// than by a special case.
func (p *nightProjector) spellings(ctx context.Context, h nightHint, night int) ([]string, error) {
	if !h.OK || night < 1 {
		return nil, nil
	}
	name, err := h.NightName(night)
	if err != nil {
		// The folder's own segment cannot be rendered for this ordinal, so there is no
		// folder to read; route.Route re-derives the same quarantine moments later.
		return nil, nil
	}
	lo, hi := folderRange(joinLocal(joinLocal(p.cfg.Roots.Light, h.Prefix), name))
	targets, err := p.w.store.DistinctTargetsUnder(ctx, lo, hi)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range targets {
		if t != p.target {
			out = append(out, t)
		}
	}
	// SELECT DISTINCT has no defined order; the operator reads this list.
	sort.Strings(out)
	return out, nil
}

// previewRetarget projects, read-only (no NAS/device I/O), where each frame's archive
// would move under newTarget (design routing-and-filing §9.6). It rebuilds the
// classification from the row's already-classified columns, resolves the night ordinal
// the way apply would, routes it, and derives the same canonical path fileFrom would —
// never calling AssignNight (which mutates night_counters). The numeric *Present flags
// are approximated from the stored values; apply re-reads the real header.
func (w *Worker) previewRetarget(ctx context.Context, ids []int64, newTarget string) []model.RetargetPreview {
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	out := make([]model.RetargetPreview, 0, len(ids))
	sanitized := sanitize.Component(newTarget)
	// Apply folds the typed target onto the spelling already filed, so the preview
	// must project the same folder. A read that fails says nothing about the frames:
	// keep the typed spelling and let the rows explain themselves, as the night
	// projection does with its own failed read.
	if canonical, err := w.store.CanonicalTarget(ctx, sanitized); err != nil {
		w.log.Warn("rig: retarget preview could not fold target spelling", "target", sanitized, "error", err)
	} else {
		sanitized = sanitize.Component(canonical)
	}
	proj := &nightProjector{
		w: w, cfg: cfg, profile: profile, target: sanitized,
		folders:  map[string]nightHint{},
		resolved: map[string]projectedNight{},
	}
	for _, id := range ids {
		p := model.RetargetPreview{FrameID: id}
		f, ok, err := w.store.GetFrameByID(ctx, id)
		if err != nil || !ok || f.RigID != w.profileID {
			p.Reason = "frame not found"
			out = append(out, p)
			continue
		}
		p.CurrentTarget = f.Target
		p.OldPath = f.DestPath
		if profile == nil {
			p.Reason = "rig not managed"
			out = append(out, p)
			continue
		}
		if f.State != model.StateVerified && f.State != model.StateDeleted {
			p.Reason = "frame state " + string(f.State) + " is not retargetable"
			out = append(out, p)
			continue
		}
		if f.DestPath == "" {
			p.Reason = "frame has no archive path"
			out = append(out, p)
			continue
		}
		if sanitized == "" {
			p.Reason = "new target is empty after sanitize"
			out = append(out, p)
			continue
		}
		if sanitized == f.Target {
			// apply skips this as a no-op (retargetFrame step 1), so preview must not
			// dangle an eligible "move" the apply would refuse.
			p.Reason = "new target equals the current target"
			out = append(out, p)
			continue
		}

		cls := model.Classification{Type: model.FrameType(f.FrameType)}
		cls.Target = sanitized
		cls.TargetPresent = true
		if f.Filter != "" {
			cls.Filter = f.Filter
			cls.FilterPresent = true
		}
		if f.Exptime != 0 {
			cls.Exptime = f.Exptime
			cls.ExptimePresent = true
		}
		// Recorded presence, not a != 0 test: a cooled camera at 0C is present.
		if !f.SetTempAbsent {
			cls.SetTempC = f.SetTemp
			cls.SetTempPresent = true
		}
		if f.Gain != 0 {
			cls.Gain = f.Gain
			cls.GainPresent = true
		}
		if f.Offset != 0 {
			cls.Offset = f.Offset
			cls.OffsetPresent = true
		}
		if f.Sensor != "" {
			cls.Sensor = f.Sensor
			cls.SensorMapped = true
		}
		if x, y, ok := parseBinningToken(f.Binning); ok {
			cls.BinX, cls.BinY = x, y
			cls.BinPresent = true
		}
		if f.DateObs != "" {
			if t, err := time.Parse(time.RFC3339Nano, f.DateObs); err == nil {
				cls.DateObs = t
				cls.DateObsPresent = true
			}
		}

		var obs time.Time
		if cls.DateObsPresent {
			obs = route.ObservingDate(cls.DateObs, w.siteLoc)
		}
		opts := routeOptionsFor(cfg, obs)

		// The night is resolved BEFORE routing, so opts carries it and the preview
		// states the folder the frame lands in. Rendering NIGHT_0 — a folder that
		// cannot exist — is the artifact that let a wrong ordinal reach disk unnoticed.
		var pn projectedNight
		var spellings []string
		if cls.Type == model.TypeLight {
			var perr error
			pn, spellings, perr = proj.project(ctx, cls, opts)
			var qe *route.QuarantineError
			switch {
			case perr == nil:
			case errors.As(perr, &qe):
				// The destination cannot be rendered, so there is no folder to consult;
				// route.Route re-derives the identical quarantine from the same segments
				// below and the row reports it as the routing failure it is.
			default:
				// A read the preview could not make says nothing about this frame, so the
				// row stays eligible and carries the reason instead. It renders at the next
				// ordinal the batch would take — advancing it, so two failed frames do not
				// project one ordinal — rather than at NIGHT_0.
				proj.runningMax++
				pn = projectedNight{night: proj.runningMax}
				p.Reason = "night: " + perr.Error()
			}
		}
		opts.Night = pn.night

		routed, rerr := route.Route(cls, *profile, opts)
		if rerr != nil {
			p.Reason = "routing: " + rerr.Error()
			out = append(out, p)
			continue
		}
		root := cfg.Roots.Light
		tmpl := cfg.Templates.Light
		switch {
		case routed.Kind == route.RootCal:
			root = cfg.Roots.Cal
			tmpl = cfg.Templates.Cal[cls.Type]
		case route.IsMosaicPanel(cls.Target, cls.Type, opts):
			// The mosaic template shares the light root, so the root alone cannot
			// tell the two apart: without this arm a retarget TO a panel name reads
			// the light template's tokens while routing through the mosaic one, and
			// the NightApprox badge would be derived from the wrong template. The
			// predicate is asked with the same target string Route just routed.
			tmpl = cfg.Templates.Mosaic
		}
		name := w.candidateBaseName(filepath.Base(f.DestPath), archiveHash(*f))
		p.NewPath = filepath.Join(joinLocal(root, routed.Dir), name)
		// Only a date the ledger has already assigned is exact. The <n> conjunct is
		// what keeps the caveat off a frame whose folder carries no night at all — a
		// cal frame routes through the cal template and consults no ledger, and a light
		// template need not name a night segment either.
		p.NightApprox = strings.Contains(tmpl, "<NIGHT>") ||
			(strings.Contains(tmpl, "<n>") && !pn.exact)
		p.ExistingSpellings = spellings
		p.Eligible = true
		out = append(out, p)
	}
	return out
}

// parseBinningToken is the inverse of binningToken: it parses "Bin1" → (1,1) and
// "Bin1x2" → (1,2) so previewRetarget can rebuild BinX/BinY from the stored column. It
// reports ok=false for an empty or unparseable token.
func parseBinningToken(tok string) (x, y int64, ok bool) {
	rest, found := strings.CutPrefix(tok, "Bin")
	if !found || rest == "" {
		return 0, 0, false
	}
	if a, b, split := strings.Cut(rest, "x"); split {
		xi, e1 := strconv.ParseInt(a, 10, 64)
		yi, e2 := strconv.ParseInt(b, 10, 64)
		if e1 != nil || e2 != nil {
			return 0, 0, false
		}
		return xi, yi, true
	}
	n, e := strconv.ParseInt(rest, 10, 64)
	if e != nil {
		return 0, 0, false
	}
	return n, n, true
}
