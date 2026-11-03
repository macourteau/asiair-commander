package rig

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/sanitize"
	"github.com/macourteau/asiair-commander/internal/store"
)

// fitsBlock is the FITS block size: a header grows by whole blocks of it, so it
// bounds an insertion's growth (design header-edits §4.1).
const fitsBlock = 2880

// fitsCard is one header card image, in bytes.
const fitsCard = 80

// cardsPerFITSBlock is how many card images one header block holds.
const cardsPerFITSBlock = fitsBlock / fitsCard

// Edit statuses (design header-edits §9.1).
const (
	editEdited  = "edited"
	editSkipped = "skipped"
	editFailed  = "failed"
)

// editDerivesNothing names the editable keys from which nothing else on the row is
// derived. Only an edit made entirely of such keys may supersede the archive in
// place; OBJECT derives the night ordinal and the panel columns, which only the
// filing path writes (design header-edits §8).
var editDerivesNothing = map[string]bool{"FILTER": true}

// editFrame edits one allowlisted header card of one archived frame, on the worker
// goroutine, never touching the device original (design header-edits §8,
// routing-and-filing §9.3): verify the archive is intact, write the edited copy to
// a staging temp, prove from the files at rest that only the edited card changed,
// then commit it — in place under its own name when nothing derived from the edit
// changes its folder, otherwise through retargeting's filing path. sha256 stays
// the device anchor; the archive anchors and edit_sha256 become the edited file's.
// The frame's state is preserved (a deleted archive's edit stays deleted).
func (w *Worker) editFrame(ctx context.Context, cfg *model.Config, profile *model.RigProfile, f model.Frame, key, value string) (model.EditResult, error) {
	res := model.EditResult{FrameID: f.ID, OldPath: f.DestPath}
	skip := func(reason string) (model.EditResult, error) {
		res.Status, res.Reason = editSkipped, reason
		return res, nil
	}
	fail := func(reason string) (model.EditResult, error) {
		res.Status, res.Reason = editFailed, reason
		return res, nil
	}

	// 1. Scope guards (design header-edits §9.2): a verified or deleted frame that
	// still has an archive to edit, a key on the allowlist and a value inside its
	// rule. A state change is out of scope.
	if f.State != model.StateVerified && f.State != model.StateDeleted {
		return skip("frame state " + string(f.State) + " is not editable")
	}
	if f.DestPath == "" {
		return skip("frame has no archive path")
	}
	key = model.NormalizeKey(key)
	rule, ok := header.EditableKeys[key]
	if !ok {
		return skip(key + " is not an editable header keyword")
	}
	v, err := rule.Normalize(value)
	if err != nil {
		return skip(err.Error())
	}
	switch key {
	case "OBJECT":
		// Fold onto the spelling this target was already filed under before the card
		// is written, not after: filing folds the row's target too, and a card that
		// disagreed with its own row would fail the re-apply hash check on every
		// later re-pull. Typing a target the archive already holds in another case
		// is not a rename — the folders are one — so the no-op guard sees it as one
		// (design routing-and-filing §6.6).
		canonical, cerr := w.store.CanonicalTarget(ctx, v)
		if cerr != nil {
			res.Status, res.Reason = editFailed, "canonical target: "+cerr.Error()
			return res, cerr
		}
		// Re-sanitized because the ledger holds RAW OBJECT spellings: a canonical
		// "NGC:7380" must come back as the "NGC_7380" the card will carry.
		v = sanitize.Component(canonical)
		if v == f.Target {
			return skip("new target equals the current target")
		}
	case "FILTER":
		// Exact, not case-folded: the filter is not night identity, and whether
		// stacking software groups "Ha" with "HA" is the software's decision (design
		// header-edits §9.1). A stored "" is an absent card, which never equals a
		// value the rule accepted.
		if v == f.Filter {
			return skip("new filter equals the current filter")
		}
	}

	// A second swap over an intent left open would overwrite that intent and orphan
	// its copy set aside where nothing names it, and a re-file mid-move has the
	// archive's whereabouts in question; both are what the delete pass defers on
	// (design header-edits §8).
	if reason, ierr := w.openIntentAt(ctx, f.DestPath); ierr != nil {
		return fail("open intents: " + ierr.Error())
	} else if reason != "" {
		return skip(reason)
	}

	// 2. Admission. The edited temp transiently adds one copy of the ARCHIVE before
	// the old one is removed, plus the one header block an insertion can grow it by
	// (design header-edits §7). The work touches only the NAS, so it is admitted on
	// the local site whichever site the rig's device is reached through.
	need := f.ArchiveSize
	if rule.Insertable {
		need += fitsBlock
	}
	release, err := w.adm.Acquire(ctx, Requester{Rig: w.profileID, Site: netsite.Local}, need)
	if err != nil {
		res.Status, res.Reason = editFailed, "admission: "+err.Error()
		return res, err
	}
	defer release()

	// 3. Verify the archive before editing — never edit a drifted file (HE-2). The
	// verify tees the data digest off the same read, so the proof below compares
	// the edit's data unit to bytes just proven to be the archive, not to the row's
	// data_sha256, which a row the backfill has not reached does not carry (design
	// header-edits §4.3).
	off, err := header.DataOffset(f.DestPath)
	if err != nil {
		return fail("archive header: " + err.Error())
	}
	atHash, srcData, verr := w.lfs.VerifyReadTee(f.DestPath, f.ArchiveSize, off)
	if verr != nil {
		return fail("archive verify: " + verr.Error())
	}
	if atHash != archiveHash(f) {
		w.log.Warn("rig: edit refused; archive hash drifted", "frame", f.ID, "dest", f.DestPath)
		return fail("archive content drifted since filing; refusing to edit")
	}

	// 4. Write the edit to a temp in the staging root (same filesystem as the
	// destination, so both commits' renames work).
	staging := cfg.Roots.Staging
	if err := w.lfs.MkdirAll(staging); err != nil {
		return fail("staging mkdir: " + err.Error())
	}
	tmp, err := os.CreateTemp(staging, "asiair-edit-*.tmp")
	if err != nil {
		return fail("staging temp: " + err.Error())
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	// A commit consumes the temp by renaming it; the remove is then a no-op.
	defer func() { _ = w.lfs.Remove(tmpPath) }()

	edits := []header.CardEdit{{Key: key, Value: v}}
	er, err := w.rewriter(f.DestPath, tmpPath, edits)
	if err != nil {
		return fail("rewrite " + key + ": " + err.Error())
	}

	// 5. The worker-side proof, from the files at rest (HE-3).
	newHash, err := w.proveEdit(cfg, editProof{
		src: f.DestPath, srcSize: f.ArchiveSize, srcData: srcData,
		out: tmpPath, er: er, edits: edits,
	})
	if err != nil {
		w.log.Error("rig: edit failed its faithfulness proof; refusing", "frame", f.ID, "dest", f.DestPath, "error", err)
		return fail(err.Error())
	}

	// The entry's from is the device copy's original: Set keeps an existing entry's
	// from, so it is consulted only on the key's first edit, when the stored value
	// is still the device copy's. An empty stored value is an absent card, except
	// for a FILTER card that is present with an empty value: the row stores both as
	// "", so presence is read from the archive's header, which step 3 just proved,
	// rather than recording a card the writer replaced as one it inserted.
	var from *string
	if cur, present := storedValue(f, key); present {
		from = &cur
	} else if key == "FILTER" {
		if h, herr := w.hdr.ReadHeader(f.DestPath); herr == nil {
			if cardVal, ok := h.Str("FILTER"); ok {
				from = &cardVal
			}
		} else {
			w.log.Warn("rig: could not re-read the archive header for the edit's audit record",
				"frame", f.ID, "dest", f.DestPath, "error", herr)
		}
	}
	overlay := f.HeaderEdits.Set(key, v, from)

	// 6. Commit (design header-edits §8).
	inPlace, editedFilter, err := w.editStaysInPlace(cfg, profile, f, tmpPath, edits)
	if err != nil {
		return fail(err.Error())
	}
	if inPlace {
		serr := w.supersedeArchive(ctx, supersedeArgs{
			kind:    store.SupersedeEdit,
			frame:   f,
			srcPath: tmpPath,
			// The device was not read: its anchors are carried for the intent's
			// record only, and the edit commit does not write them.
			sha256:        f.SHA256,
			size:          f.Size,
			mtime:         f.Mtime,
			archiveSHA256: newHash,
			archiveSize:   er.OutputSize,
			editSHA256:    newHash,
			headerEdits:   overlay,
			filter:        editedFilter,
		})
		switch {
		case errors.Is(serr, errSupersedeRefused):
			return skip(strings.TrimPrefix(serr.Error(), errSupersedeRefused.Error()+": "))
		case serr != nil:
			w.log.Error("rig: in-place edit failed", "frame", f.ID, "dest", f.DestPath, "error", serr)
			return fail("in-place edit: " + serr.Error())
		}
	} else if !w.fileEditedArchive(ctx, cfg, profile, f, tmpPath, newHash, er.OutputSize, overlay) {
		// The old file is untouched: no data loss, unedited.
		return fail("could not file the edited archive")
	}

	// 7. Prove the commit persisted before anything else follows from it. fileFrom
	// swallows an UpsertFrame failure, and the in-place commit finds no row when
	// the hourly prune removed a deleted one mid-swap (it then leaves the edited
	// file in place and returns nil), so the row is re-read and must carry the
	// edited hash.
	fresh, ok, ferr := w.store.GetFrameByID(ctx, f.ID)
	if ferr != nil || !ok {
		return fail("edited archive committed but the row could not be re-read")
	}
	res.NewPath = fresh.DestPath
	if fresh.EditSHA256 != newHash {
		return fail("edited archive committed but the row did not persist")
	}
	if !inPlace && fresh.DestPath != "" && fresh.DestPath != f.DestPath {
		// Only after the commit is proven (design routing-and-filing §9.3 step 7).
		if err := w.lfs.Remove(f.DestPath); err != nil {
			// A cosmetic orphan, never data loss — the new archive is committed and
			// proven.
			w.log.Warn("rig: edit filed but old archive not removed (orphan)", "old", f.DestPath, "error", err)
		}
	}
	// target and filter are written from the re-classified edited header, the
	// overlay from the edit; asserting they agree is what keeps them from drifting
	// apart (design header-edits §5.1).
	if msg := overlayDisagreement(fresh); msg != "" {
		w.log.Error("rig: edited row disagrees with its overlay", "frame", f.ID, "dest", fresh.DestPath, "detail", msg)
		return fail(msg)
	}
	if inPlace {
		w.hub.Emit(hub.Event{Name: "frame.done", Data: *fresh})
		open, oerr := w.store.OpenSupersedePaths(ctx, w.profileID)
		if oerr != nil || open[f.DestPath] {
			// Step 4's unlink failed (or the intent could not be read back): the edit
			// is committed and every row describes the file in place, but the open
			// intent keeps the frame out of the delete pass until startup recovery
			// removes the leftover (design header-edits §8).
			w.log.Error("rig: edit committed but its supersede intent is still open; the device original is held until the next restart",
				"frame", f.ID, "dest", f.DestPath, "error", oerr)
			res.Status = editEdited
			res.Reason = "edited, but the set-aside copy could not be removed; the device original is held until the next restart"
			return res, nil
		}
	}
	res.Status = editEdited
	w.log.Info("rig: edited frame header", "frame", f.ID, "key", key, "value", v,
		"in_place", inPlace, "old", f.DestPath, "new", res.NewPath)
	return res, nil
}

// storedValue is the row's value for an editable key and whether its card is
// present. The row records an absent card as an empty string for both keys.
func storedValue(f model.Frame, key string) (string, bool) {
	var v string
	switch key {
	case "OBJECT":
		v = f.Target
	case "FILTER":
		v = f.Filter
	}
	return v, v != ""
}

// overlayDisagreement reports how a committed row's classified columns differ from
// its overlay's entries, or "" when they agree.
func overlayDisagreement(f *model.Frame) string {
	for _, e := range f.HeaderEdits {
		if cur, _ := storedValue(*f, e.Key); cur != e.Value {
			return fmt.Sprintf("edited row's %s %q disagrees with its header edit %q", e.Key, cur, e.Value)
		}
	}
	return ""
}

// openIntentAt names the open intent that refuses an edit of path, or "".
func (w *Worker) openIntentAt(ctx context.Context, path string) (string, error) {
	sup, err := w.store.OpenSupersedePaths(ctx, w.profileID)
	if err != nil {
		return "", err
	}
	if sup[path] {
		return "an archive swap at this path has not finished; it resolves at the next restart", nil
	}
	ref, err := w.store.OpenIntentPaths(ctx, w.profileID)
	if err != nil {
		return "", err
	}
	if ref[path] {
		return "a re-file of this path has not finished; it resolves at the next restart", nil
	}
	return "", nil
}

// editStaysInPlace decides the commit (design header-edits §8): in place when
// every edited key derives nothing else on the row and the edited header routes to
// the directory the archive is already in. It also returns the edited header's
// classified filter, which the in-place commit writes to every sharer.
//
// The night is the row's own: no key that may commit in place is night identity,
// so the ordinal the frame was filed under is the one its folder still names.
func (w *Worker) editStaysInPlace(cfg *model.Config, profile *model.RigProfile, f model.Frame, editedPath string, edits []header.CardEdit) (bool, string, error) {
	h, err := w.hdr.ReadHeader(editedPath)
	if err != nil {
		return false, "", fmt.Errorf("edit: re-read edited header: %w", err)
	}
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	filter := ""
	if cls.FilterPresent {
		filter = cls.Filter
	}
	for _, e := range edits {
		if !editDerivesNothing[model.NormalizeKey(e.Key)] {
			return false, filter, nil
		}
	}
	var obs time.Time
	if cls.DateObsPresent {
		obs = route.ObservingDate(cls.DateObs, w.siteLoc)
	}
	opts := routeOptionsFor(cfg, obs)
	opts.Night = int(f.Night)
	routed, rerr := route.Route(cls, *profile, opts)
	if rerr != nil {
		// Unroutable as edited: the filing path re-derives the same refusal and
		// reports it, so an edit never lands in place under a route it cannot name.
		return false, filter, nil
	}
	root := cfg.Roots.Light
	if routed.Kind == route.RootCal {
		root = cfg.Roots.Cal
	}
	return joinLocal(root, routed.Dir) == filepath.Dir(f.DestPath), filter, nil
}

// fileEditedArchive files the edited staging temp to its re-derived folder, mirroring
// reclassifyOne (a local rename with the grace anchor preserved) but with the sha256/
// diskHash split and the edit overlay made explicit: sha256 is the device-side anchor,
// unmoved because the device original is not touched here, while diskHash, the archive
// anchors and edit_sha256 all carry the edited archive hash, archiveSize is the edited
// file's length (an insertion can grow it, design header-edits §7), and the frame
// state is carried through so a deleted archive's edit stays deleted. It returns
// whether the frame reached a terminal filed outcome.
func (w *Worker) fileEditedArchive(ctx context.Context, cfg *model.Config, profile *model.RigProfile, f model.Frame, tmpPath, newHash string, archiveSize int64, edits model.HeaderEdits) bool {
	h, err := w.hdr.ReadHeader(tmpPath)
	if err != nil {
		w.log.Error("rig: edit re-read edited header failed", "error", err)
		return false
	}
	astro := astrometry.FromHeader(h)
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	if cls.Type == model.TypeUnknown {
		w.log.Error("rig: edited frame has unknown IMAGETYP; refusing")
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
			w.log.Error("rig: edited light missing OBJECT/DATE-OBS; refusing")
			return false
		}
		n, err := w.nightFor(ctx, cfg, profile, &cls, opts)
		var qe *route.QuarantineError
		switch {
		case err == nil:
			night = n
		case errors.As(err, &qe):
			// Fall through with night 0: route.Route re-derives the same quarantine
			// below and refuses the edit there, rather than here as a transient store
			// failure that would be retried forever.
			w.log.Warn("rig: edit night folder quarantine", "frame", f.ID, "reason", qe.Error())
		default:
			w.log.Error("rig: edit assign night failed", "error", err)
			return false
		}
	}
	opts.Night = night
	res, rerr := route.Route(cls, *profile, opts)
	if rerr != nil {
		w.log.Error("rig: edit routing failed; refusing", "error", rerr)
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
	// An edit files an EDITED archive copy and never touches the device original,
	// so it carries the row's own device anchor and can never present itself as a
	// revision: the filing path's third outcome is unreachable from here, and this
	// path's contract stays "did the edited copy land".
	return w.fileFrom(ctx, fileArgs{
		lf:          lf,
		prior:       &f,
		cls:         cls,
		astro:       astro,
		night:       night,
		srcPath:     tmpPath,
		diskHash:    newHash,  // the edited archive's on-disk hash
		sha256:      f.SHA256, // the device anchor: the device original is never touched
		editSHA256:  newHash,  // the audit record: what the edit produced
		headerEdits: edits,
		// The archive side describes the EDITED bytes — the copy that lands — at the
		// length they were proven at.
		archiveSHA256: newHash,
		archiveSize:   archiveSize,
		destDir:       destDir,
		canonicalName: name,
		state:         f.State,     // preserve deleted; do not resurrect to verified
		deletedAt:     f.DeletedAt, // preserve the reclaim clock; do not null a deleted edit
		pulledAtNew:   f.PulledAt,
		pulledAtKeep:  f.PulledAt,
		reclassify:    true, // a local rename of an edited temp — zero network bytes
	}) == fileFiled
}

// editProof is one edit to prove: the file it was made from, that file's length
// and data digest as the caller proved them from the bytes at rest, the edited
// temp, the writer's report, and the edits handed to the writer (values already
// normalized, so each canonical card is exactly what the writer must have built).
type editProof struct {
	src     string
	srcSize int64
	srcData string // hex data-unit digest of src
	out     string
	er      header.EditResult
	edits   []header.CardEdit
}

// proveEdit is the worker-side faithfulness proof of design header-edits §4.3,
// shared by the edit operation and every re-apply. It returns the edited file's
// at-rest hash, which becomes its archive anchor.
//
// The writer's own digests are checked first, but they are computed over the
// writer's buffers and so prove only its self-consistency; everything after them
// is the worker's own read of the files at rest, through code that shares nothing
// with the writer, so a writer that corrupts and reports agreeing digests is still
// refused before anything is committed (HE-3). Every length is the file in hand's:
// the source's as the caller verified it, the output's as the writer reports it
// and as it is on disk — never a row's recorded length, which describes some
// other copy.
func (w *Worker) proveEdit(cfg *model.Config, p editProof) (string, error) {
	if p.er.CardComplementIn != p.er.CardComplementOut {
		return "", errors.New("edit: the writer's card complement digests disagree — the edit changed a card it was not asked to")
	}
	if p.er.DataIn != p.er.DataOut {
		return "", errors.New("edit: the writer's data digests disagree — the edit changed the data unit")
	}
	if p.srcData == "" {
		return "", errors.New("edit: the source's data unit could not be located; refusing to prove an edit of it")
	}

	// Length: what is on disk is what the writer reports.
	sz, _, err := w.lfs.Stat(p.out)
	if err != nil {
		return "", fmt.Errorf("edit: stat edited temp: %w", err)
	}
	if sz != p.er.OutputSize {
		return "", fmt.Errorf("edit: edited temp is %d bytes, the writer reported %d", sz, p.er.OutputSize)
	}

	// Raw cards: the output's card images, END included, are exactly the source's
	// with each replaced card swapped for its canonical card at its own index and
	// each inserted card at the source's END index, in edit order.
	srcRaw, err := header.ReadRawHeader(p.src)
	if err != nil {
		return "", fmt.Errorf("edit: read source header: %w", err)
	}
	outRaw, err := header.ReadRawHeader(p.out)
	if err != nil {
		return "", fmt.Errorf("edit: read edited header: %w", err)
	}
	want, inserted, err := expectedEditCards(srcRaw, p.edits)
	if err != nil {
		return "", err
	}
	if len(outRaw.Cards) != len(want) {
		return "", fmt.Errorf("edit: edited header has %d cards through END, want %d", len(outRaw.Cards), len(want))
	}
	for i := range want {
		if outRaw.Cards[i] != want[i] {
			return "", fmt.Errorf("edit: edited header card %d is %q, want %q", i, outRaw.Cards[i][:], want[i][:])
		}
	}

	// Growth: pinned by the two headers, not by the writer's report of it.
	growth := outRaw.DataOffset - srcRaw.DataOffset
	if growth != p.er.OutputSize-p.srcSize {
		return "", fmt.Errorf("edit: header grew by %d bytes but the file by %d", growth, p.er.OutputSize-p.srcSize)
	}
	maxGrowth := int64((inserted+cardsPerFITSBlock-1)/cardsPerFITSBlock) * fitsBlock
	if growth < 0 || growth > maxGrowth {
		return "", fmt.Errorf("edit: header grew by %d bytes; %d insertion(s) allow 0 to %d", growth, inserted, maxGrowth)
	}

	// Padding after END is covered by no digest, so it is checked directly.
	if inserted == 0 {
		if !bytes.Equal(outRaw.Padding, srcRaw.Padding) {
			return "", errors.New("edit: a replace-only edit changed the padding after END")
		}
	} else if len(bytes.Trim(outRaw.Padding, " ")) != 0 {
		return "", errors.New("edit: an insertion left non-blank padding after END")
	}

	// At rest: a cache-bypassing read of the output is the bytes the writer
	// streamed, and its data unit is the source's.
	off, err := header.DataOffset(p.out)
	if err != nil {
		return "", fmt.Errorf("edit: edited data offset: %w", err)
	}
	if off != outRaw.DataOffset {
		return "", fmt.Errorf("edit: edited data offset %d disagrees with its header's END block %d", off, outRaw.DataOffset)
	}
	atRest, data, err := w.lfs.VerifyReadTee(p.out, p.er.OutputSize, off)
	if err != nil {
		return "", fmt.Errorf("edit: at-rest verify: %w", err)
	}
	if want := hex.EncodeToString(p.er.OutputSHA256[:]); atRest != want {
		// Without this a write fault between the streamed write and the durable file
		// would mint the archive anchor from corrupted bytes, and later authorize
		// deleting the device original against them.
		return "", fmt.Errorf("edit: at-rest hash %s != the writer's output hash %s (write fault)", atRest, want)
	}
	if data != p.srcData {
		return "", errors.New("edit: the edited data unit differs from the source's")
	}

	// Read-back: the edited header classifies to the values written, which is also
	// what guarantees the folder an edit routes to matches the label it carries.
	h, err := w.hdr.ReadHeader(p.out)
	if err != nil {
		return "", fmt.Errorf("edit: re-read edited header: %w", err)
	}
	cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
	for _, e := range p.edits {
		switch model.NormalizeKey(e.Key) {
		case "OBJECT":
			if cls.Target != e.Value {
				return "", fmt.Errorf("edit: edited OBJECT %q does not classify to %q", cls.Target, e.Value)
			}
		case "FILTER":
			if !cls.FilterPresent || cls.Filter != e.Value {
				return "", fmt.Errorf("edit: edited FILTER %q does not classify to %q", cls.Filter, e.Value)
			}
		}
	}
	return atRest, nil
}

// expectedEditCards builds the card images an edit of src must produce, through
// END, and counts the insertions. It is the worker's own statement of the writer's
// layout (design header-edits §4.1), so it refuses exactly where that layout has no
// answer: a key with several value cards, or an absent key that may not be
// inserted.
func expectedEditCards(src header.RawHeader, edits []header.CardEdit) ([][fitsCard]byte, int, error) {
	out := append([][fitsCard]byte(nil), src.Cards...)
	var insert [][fitsCard]byte
	for _, e := range edits {
		key := model.NormalizeKey(e.Key)
		card, err := header.CanonicalCard(key, e.Value)
		if err != nil {
			return nil, 0, fmt.Errorf("edit: canonical %s card: %w", key, err)
		}
		at := -1
		for i := 0; i < src.End; i++ {
			c := src.Cards[i]
			if c[8] == '=' && c[9] == ' ' && model.NormalizeKey(string(c[:8])) == key {
				if at >= 0 {
					return nil, 0, fmt.Errorf("edit: source carries more than one %s value card", key)
				}
				at = i
			}
		}
		switch {
		case at >= 0:
			out[at] = card
		case header.EditableKeys[key].Insertable:
			insert = append(insert, card)
		default:
			return nil, 0, fmt.Errorf("edit: source carries no %s value card to replace", key)
		}
	}
	if len(insert) > 0 {
		end := out[src.End]
		out = append(append(out[:src.End:src.End], insert...), end)
	}
	return out, len(insert), nil
}

// cardEditsOf turns an overlay into the writer's edit list, in the overlay's
// first-edit order, which is what makes a one-pass re-apply reproduce an archive
// edited in several steps (design header-edits §5.1).
func cardEditsOf(e model.HeaderEdits) []header.CardEdit {
	out := make([]header.CardEdit, 0, len(e))
	for _, h := range e {
		out = append(out, header.CardEdit{Key: h.Key, Value: h.Value})
	}
	return out
}

// reapplyEditsIfNeeded is the re-apply rule (design header-edits §6,
// routing-and-filing §9.4) at both places freshly pulled device bytes are filed
// against a prior row: the pull's classifyAndFile and the restore's source read.
// When the bytes are the frame whose archive was edited (prior.EditSHA256 != ""),
// it applies the row's whole edit list into a second temp, so the archive keeps
// its edits instead of reverting to the device copy's header. It returns the
// file to file, its at-rest hash and its length after proving the edit, or (tmp,
// streamHash, incomingSize) unchanged when no re-apply is needed (no prior row,
// prior never edited, or genuinely new content — which resets the overlay).
//
// "The frame whose archive was edited" is a disjunction (design frame-lifecycle
// §11.11): the incoming data unit is the exposure the row records, OR the
// incoming bytes are byte-identical to the device original the row records. The
// first arm is what survives a device-side revision rewriting the header of an
// exposure already pulled; the second is kept because a byte-identical re-pull is
// the same exposure by definition and a row the backfill has not reached carries
// no data_sha256 at all. IsUsableDataDigest is the precondition on the first arm:
// without it two header-only frames match on the one digest every zero-length data
// unit shares.
//
// incomingSize and dataHash describe the file at tmp, as its caller proved them.
// The proof is anchored to that file and the returned length is the OUTPUT's: an
// insertion grows the file, and an archive length read at the incoming length
// would hash a prefix and refuse every re-pull of an edited filterless frame
// (design header-edits §7).
func (w *Worker) reapplyEditsIfNeeded(cfg *model.Config, tmp, streamHash, dataHash string, incomingSize int64, prior *model.Frame) (string, string, int64, error) {
	if prior == nil || prior.EditSHA256 == "" {
		return tmp, streamHash, incomingSize, nil
	}
	sameExposure := header.IsUsableDataDigest(dataHash) && dataHash == prior.DataSHA256
	if !sameExposure && streamHash != prior.SHA256 {
		return tmp, streamHash, incomingSize, nil
	}
	if len(prior.HeaderEdits) == 0 {
		// edit_sha256 says the archive is an edit, and nothing says of what: filing
		// the device's own header would revert it silently, so refuse loudly.
		return "", "", 0, errors.New("re-apply: the row records an edit but no header edits to apply")
	}
	tmp2, err := os.CreateTemp(filepath.Dir(tmp), "asiair-edit-*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("re-apply temp: %w", err)
	}
	tmp2Path := tmp2.Name()
	_ = tmp2.Close()
	edits := cardEditsOf(prior.HeaderEdits)
	er, rerr := w.rewriter(tmp, tmp2Path, edits)
	if rerr != nil {
		_ = w.lfs.Remove(tmp2Path)
		return "", "", 0, fmt.Errorf("re-apply header edits: %w", rerr)
	}
	h, perr := w.proveEdit(cfg, editProof{
		src: tmp, srcSize: incomingSize, srcData: dataHash,
		out: tmp2Path, er: er, edits: edits,
	})
	if perr != nil {
		_ = w.lfs.Remove(tmp2Path)
		return "", "", 0, perr
	}
	return tmp2Path, h, er.OutputSize, nil
}

// previewEdit projects, read-only (no NAS/device I/O), what applying key=value to
// each frame would do (design header-edits §9.1). It applies the same scope rules
// as editFrame in the same order, so a row the preview calls eligible is one apply
// attempts. It runs on the caller's goroutine.
func (w *Worker) previewEdit(ctx context.Context, ids []int64, key, value string) []model.EditPreview {
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	out := make([]model.EditPreview, 0, len(ids))

	key = model.NormalizeKey(key)
	rule, known := header.EditableKeys[key]
	var v string
	var verr error
	if known {
		v, verr = rule.Normalize(value)
	}
	var proj *nightProjector
	var filterSpellings []string
	if known && verr == nil && profile != nil {
		switch key {
		case "OBJECT":
			// Apply folds the typed target onto the spelling already filed, so the
			// preview must project the same folder. A read that fails says nothing
			// about the frames: keep the typed spelling and let the rows explain
			// themselves, as the night projection does with its own failed read.
			if canonical, err := w.store.CanonicalTarget(ctx, v); err != nil {
				w.log.Warn("rig: edit preview could not fold target spelling", "target", v, "error", err)
			} else {
				v = sanitize.Component(canonical)
			}
			proj = &nightProjector{
				w: w, cfg: cfg, profile: profile, target: v,
				folders:  map[string]nightHint{},
				resolved: map[string]projectedNight{},
			}
		case "FILTER":
			filterSpellings = w.caseVariantFilters(ctx, v)
		}
	}

	for _, id := range ids {
		p := model.EditPreview{FrameID: id}
		f, ok, err := w.store.GetFrameByID(ctx, id)
		if err != nil || !ok || f.RigID != w.profileID {
			p.Reason = "frame not found"
			out = append(out, p)
			continue
		}
		p.CurrentValue, p.CurrentPresent = storedValue(*f, key)
		p.OldPath = f.DestPath
		switch {
		case profile == nil:
			p.Reason = "rig not managed"
		case f.State != model.StateVerified && f.State != model.StateDeleted:
			p.Reason = "frame state " + string(f.State) + " is not editable"
		case f.DestPath == "":
			p.Reason = "frame has no archive path"
		case !known:
			p.Reason = key + " is not an editable header keyword"
		case verr != nil:
			p.Reason = verr.Error()
		case key == "OBJECT" && v == f.Target:
			// apply skips this as a no-op, so preview must not dangle an eligible
			// "move" the apply would refuse.
			p.Reason = "new target equals the current target"
		case key == "FILTER" && v == f.Filter:
			p.Reason = "new filter equals the current filter"
		case key == "OBJECT":
			w.projectObjectEdit(ctx, cfg, profile, proj, f, v, &p)
		case key == "FILTER":
			w.projectFilterEdit(cfg, profile, f, v, &p)
			p.ExistingSpellings = filterSpellings
		}
		out = append(out, p)
	}
	return out
}

// caseVariantFilters returns the rig's filter values that equal v
// case-insensitively but not exactly, sorted. A failed read reports none: the
// list is advice, and the preview's eligibility does not depend on it.
func (w *Worker) caseVariantFilters(ctx context.Context, v string) []string {
	all, err := w.store.DistinctFilters(ctx, w.profileID)
	if err != nil {
		w.log.Warn("rig: edit preview could not read the rig's filters", "error", err)
		return nil
	}
	var out []string
	for _, f := range all {
		if f != v && strings.EqualFold(f, v) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// projectFilterEdit routes the row with the edited filter. It rebuilds the
// classification the way re-file does — the stored night and every stored scalar
// — because a FILTER edit changes nothing else on the row and keeps its night, so
// that rebuild routes what apply's in-place check routes from the edited header.
func (w *Worker) projectFilterEdit(cfg *model.Config, profile *model.RigProfile, f *model.Frame, v string, p *model.EditPreview) {
	cls, opts := w.refileRouteInputs(cfg, f)
	cls.Filter, cls.FilterPresent = v, true
	dir, err := w.refileDestDir(cfg, profile, cls, opts)
	if err != nil {
		p.Reason = "routing: " + err.Error()
		return
	}
	if dir == filepath.Dir(f.DestPath) {
		p.InPlace = true
		p.NewPath = f.DestPath
	} else {
		p.NewPath = filepath.Join(dir, w.candidateBaseName(filepath.Base(f.DestPath), archiveHash(*f)))
	}
	p.Eligible = true
}

// projectObjectEdit is retargeting's projection (design routing-and-filing §9.6):
// it rebuilds a classification from the row's stored columns with the folded,
// sanitized target, resolves the night ordinal the way apply would, routes it, and
// derives the canonical path fileFrom would — never calling AssignNight (which
// mutates night_counters). The numeric *Present flags are approximated from the
// stored values; apply re-reads the real header. An OBJECT edit always takes the
// filing path, so it is never in place.
func (w *Worker) projectObjectEdit(ctx context.Context, cfg *model.Config, profile *model.RigProfile, proj *nightProjector, f *model.Frame, target string, p *model.EditPreview) {
	cls := model.Classification{Type: model.FrameType(f.FrameType)}
	cls.Target = target
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
		return
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
}
