package rig

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/mediaid"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// rootKey normalises a destination root to a stable meta-key fragment: absolute,
// cleaned, and lowercased, so a trailing slash or a case change does not mint a
// fresh key that silently discards the recorded library id.
func RootKey(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return strings.ToLower(filepath.Clean(abs))
}

// libraryIDKey is the meta key holding a root's recorded library id. Per-root,
// not per-rig: multiple rigs may share a destination root, and linking one links
// it for all.
func LibraryIDKey(root string) string { return "libraryid:" + RootKey(root) }

// rootConfirmed reports whether a destination root is currently linked to this
// installation's library — the marker id on disk equals the recorded id — and, if
// not, a reason for the report/alert. It only reads (never adopts or writes): a
// skipped root defers everything under it either way. See RootLinkState for the
// reason values.
func (w *Worker) rootConfirmed(ctx context.Context, root string) (ok bool, reason string) {
	return RootLinkState(ctx, w.store, w.lfs, root)
}

// RootLinkState evaluates a destination root's library-link state from the
// recorded id and the on-disk marker, returning (true, "") when the marker matches
// and otherwise false with a reason:
//
//	"" (empty root)      not configured; skip silently
//	"not linked"         no recorded id yet — the next verified file auto-links it
//	"marker missing"     an id is recorded but its marker file is gone (deleted, or
//	                     the NAS is unmounted): reconcile is paused, re-link to fix
//	"different library"  a parseable marker whose id differs from the record
//	"marker corrupt"     a present-but-garbled marker
//	"marker unreadable"  the marker (or the recorded id) could not be read
//
// Shared by the worker's reconcile gate (rootConfirmed) and the Settings library
// view so the two never diverge. Pure and read-only: it never writes a marker.
func RootLinkState(ctx context.Context, st store.Store, fsys localfs.LocalFS, root string) (bool, string) {
	if strings.TrimSpace(root) == "" {
		return false, ""
	}
	recordedID, _, err := st.GetMeta(ctx, LibraryIDKey(root))
	if err != nil {
		// A store error fails closed: conclude nothing about this root.
		return false, "marker unreadable"
	}
	if recordedID == "" {
		return false, "not linked"
	}
	verdict, m := mediaid.VerifyNamed(localfs.MarkerOrigin(fsys, root), model.LibraryMarkerName, recordedID, "")
	switch verdict {
	case mediaid.Match:
		return true, ""
	case mediaid.Foreign:
		// A parsed marker with a different id is a genuinely different library
		// mounted at this destination — worth its own critical alert. A nil *Media
		// (present-but-garbled) is a damaged marker, not evidence of another library.
		if m != nil {
			return false, "different library"
		}
		return false, "marker corrupt"
	case mediaid.AbsentMarker:
		// An id is recorded but the marker is gone: a deleted sentinel or an
		// unmounted NAS. Distinct from "not linked" (no recorded id) so the UI can
		// prompt a re-link rather than wait for an auto-link that will not come.
		return false, "marker missing"
	default: // Unreadable, SelfRecognized (unreachable with empty fingerprint)
		return false, "marker unreadable"
	}
}

// linkLibrary records a destination root as this installation's library. If a
// parseable marker already exists it adopts that id WITHOUT rewriting the file
// (so linking never destroys another installation's identity); if the marker is
// absent or corrupt it generates and writes a new one. When the recorded id
// changes it clears the missing marks of every row under the root (any rig),
// since a mark formed against the previous library had a tainted clock. It must
// not be called for a root that cannot be read.
func (w *Worker) linkLibrary(ctx context.Context, root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("no destination root given to link")
	}
	origin := localfs.MarkerOrigin(w.lfs, root)
	verdict, m := mediaid.VerifyNamed(origin, model.LibraryMarkerName, "", "")
	key := LibraryIDKey(root)
	prevID, _, err := w.store.GetMeta(ctx, key)
	if err != nil {
		return fmt.Errorf("read recorded library id: %w", err)
	}

	var newID string
	switch {
	case m != nil:
		// An existing parseable marker: adopt its id, do not rewrite the file.
		newID = m.ID
	case verdict == mediaid.AbsentMarker || verdict == mediaid.Foreign:
		// Absent, or present-but-corrupt (Foreign with a nil *Media): mint a new
		// identity and write it.
		gen, gerr := mediaid.Generate("library", "")
		if gerr != nil {
			return fmt.Errorf("generate library id: %w", gerr)
		}
		if aerr := gen.AdoptNamed(origin, model.LibraryMarkerName); aerr != nil {
			return fmt.Errorf("write library marker: %w", aerr)
		}
		newID = gen.ID
	default: // Unreadable
		return fmt.Errorf("cannot read destination root %q to link it", root)
	}

	if err := w.store.SetMeta(ctx, key, newID); err != nil {
		return fmt.Errorf("record library id: %w", err)
	}
	if newID != prevID {
		if n, err := w.store.ClearMissingUnderRoot(ctx, root); err != nil {
			w.log.Warn("rig: clear missing under relinked root failed", "root", root, "error", err)
		} else if n > 0 {
			w.log.Info("rig: relink cleared missing marks", "root", root, "count", n)
		}
	}
	return nil
}

// ensureLibraryLinked auto-links the destination root that just received a
// verified frame, the instant the file+verify proves it is a real, mounted,
// writable filesystem holding this installation's own data — so the operator never
// performs a manual "Link library" step (design library-reconciliation §8). It
// links ONLY a root with no recorded id yet: a root linked before but whose
// marker is now gone (recorded id present) is deliberately left untouched, so a
// deleted sentinel surfaces as the recovery alert rather than being silently
// rewritten (design library-reconciliation §8.2/§8.3).
func (w *Worker) ensureLibraryLinked(ctx context.Context, destPath string) {
	root := w.rootForDest(destPath)
	if root == "" {
		return // filed nowhere under a reconcilable root
	}
	rk := RootKey(root)
	if w.ensuredRoots[rk] {
		return // handled this session already: one map lookup, no store read
	}
	id, _, err := w.store.GetMeta(ctx, LibraryIDKey(root))
	if err != nil {
		return // store hiccup: retry on the next filed frame
	}
	if id != "" {
		// Already linked (by us or another rig sharing this root). A missing marker
		// here is the recovery case (design library-reconciliation §8.3), not ours to
		// silently heal.
		w.ensuredRoots[rk] = true
		return
	}
	if err := w.autoLinkRoot(ctx, root); err != nil {
		w.log.Warn("rig: auto-link library failed; will retry on the next filed frame", "root", root, "error", err)
		return
	}
	w.ensuredRoots[rk] = true
	w.log.Info("rig: auto-linked library root on a verified file", "root", root)
}

// tryLinkFromExistingData auto-links a not-yet-linked root that already holds this
// installation's filed frames — the existing/drained-library case, where no fresh
// filed frame will ever fire the file-path auto-link (design
// library-reconciliation §8). It confirms the root is really mounted the same way
// filing does: by re-verifying a filed frame at rest. If none of the sampled
// frames verifies (an unmounted or wrong mount presents none), it links nothing.
// Returns whether it linked.
func (w *Worker) tryLinkFromExistingData(ctx context.Context, root string, allRoots []string) bool {
	samples, err := w.store.FiledSamplesUnderRoot(ctx, w.profileID, root, allRoots, 8)
	if err != nil || len(samples) == 0 {
		return false
	}
	for _, s := range samples {
		// Archive-side verify, against the stored archive anchors. Re-deriving the
		// expectation here instead — the overlay hash when set, else sha256 — is what
		// the stored columns exist to avoid: a sample whose archive copy has been
		// superseded fails a derived expectation, and the root is then simply never
		// linked, with no alert, no error and nothing in a log to lead an operator to
		// the cause (design frame-lifecycle §11.1).
		if h, verr := w.lfs.VerifyRead(s.DestPath, s.ArchiveSize); verr == nil && h == s.ArchiveSHA256 {
			if err := w.autoLinkRoot(ctx, root); err != nil {
				w.log.Warn("rig: auto-link from existing data failed", "root", root, "error", err)
				return false
			}
			w.log.Info("rig: auto-linked library root from existing verified data", "root", root)
			return true
		}
	}
	return false
}

// rootForDest returns the configured Light/Cal root that most-specifically
// contains destPath, or "" if none does.
func (w *Worker) rootForDest(destPath string) string {
	best, bestLen := "", -1
	for _, r := range scanRoots(w.cfg.Load()) {
		if n := len(filepath.Clean(r)); store.UnderRoot(destPath, r) && n > bestLen {
			best, bestLen = r, n
		}
	}
	return best
}

// autoLinkRoot links a never-linked root race-safely: it arbitrates the library id
// through the store's single-writer EnsureMeta, so concurrent auto-links from
// multiple rigs sharing one root converge on a SINGLE id, then writes the marker
// with that arbitrated id — so the on-disk marker always matches the recorded id,
// never a self-inflicted mismatch (design library-reconciliation §8.4). Unlike
// linkLibrary (the manual re-link), it refuses to guess for a corrupt/unreadable
// marker, leaving that to recovery.
func (w *Worker) autoLinkRoot(ctx context.Context, root string) error {
	origin := localfs.MarkerOrigin(w.lfs, root)
	verdict, m := mediaid.VerifyNamed(origin, model.LibraryMarkerName, "", "")
	var candidate string
	switch {
	case m != nil:
		candidate = m.ID // a parseable marker already on disk: adopt its id
	case verdict == mediaid.AbsentMarker:
		gen, err := mediaid.Generate("library", "")
		if err != nil {
			return fmt.Errorf("generate library id: %w", err)
		}
		candidate = gen.ID
	default: // Foreign (corrupt) or Unreadable: don't guess — leave to a manual re-link
		return fmt.Errorf("root %q marker unusable for auto-link (%v)", root, verdict)
	}
	effective, err := w.store.EnsureMeta(ctx, LibraryIDKey(root), candidate)
	if err != nil {
		return fmt.Errorf("record library id: %w", err)
	}
	// Write the marker with the arbitrated id unless a matching one is already on
	// disk, so every racing worker ends with the same on-disk id.
	if m == nil || m.ID != effective {
		// Re-read immediately before writing. The first read may have observed a
		// stale mount point; if the real NAS mounted into this path in the window
		// since, it could now carry a DIFFERENT library's marker, and overwriting
		// that would plant our id on a filesystem that does not hold the frames our
		// evidence came from — defeating the gate (design library-reconciliation
		// §8.1). If a matching marker appeared, a racer already wrote it; if a
		// different one did, abort and let the next filed frame re-evaluate rather
		// than clobber it.
		switch v2, m2 := mediaid.VerifyNamed(origin, model.LibraryMarkerName, effective, ""); {
		case v2 == mediaid.Match:
			return nil
		case m2 != nil && m2.ID != effective:
			return fmt.Errorf("root %q gained a different library marker mid-link; not overwriting", root)
		}
		marker := &mediaid.Media{ID: effective, Instance: "library"}
		if err := marker.AdoptNamed(origin, model.LibraryMarkerName); err != nil {
			return fmt.Errorf("write library marker: %w", err)
		}
	}
	return nil
}
