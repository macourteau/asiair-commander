package rig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/filter"
	"github.com/macourteau/asiair-commander/internal/mediaid"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// mountedSession caches the shares mounted through one device session so a share
// is mounted at most once per phase; Close umounts every share and logs off.
type mountedSession struct {
	sess   device.Session
	mounts map[string]device.Share
}

func newMountedSession(sess device.Session) *mountedSession {
	return &mountedSession{sess: sess, mounts: make(map[string]device.Share)}
}

func (m *mountedSession) share(name string) (device.Share, error) {
	if s, ok := m.mounts[name]; ok {
		return s, nil
	}
	s, err := m.sess.Mount(name)
	if err != nil {
		return nil, err
	}
	m.mounts[name] = s
	return s, nil
}

func (m *mountedSession) Close() error { return m.sess.Close() }

// resetIdentity re-confirms this rig's device and clears its removable-media
// marker (and persisted media id) from every watched share, so a deliberately
// swapped stick re-adopts cleanly instead of tripping the swap guard forever. A
// leftover legacy rig-identity marker is swept best-effort at the same time.
// Device identity itself is confirmed by a live scan_air GUID probe, not a
// marker (design device-identity-and-media §4); reset is the explicit operator
// path for the media-swap case. recover-guarded so a device quirk can't kill
// the worker.
func (w *Worker) resetIdentity(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("rig: reset-identity panic recovered", "panic", r)
		}
	}()
	cfg := w.cfg.Load()
	profile := cfg.Rig(w.profileID)
	if profile == nil {
		return
	}

	// Reset clears markers, so it must positively confirm the right device before
	// clearing anything.
	ep, ok := w.resolveAddress(ctx, profile)
	if !ok {
		w.refuseReset(ctx, profile, "device not reachable right now")
		return
	}
	if profile.GUID == "" {
		// A still-host-bound rig has no device identity to confirm against. This is
		// transient (a rig migrates within a cycle or two of its device being
		// discovered), so refuse rather than risk clearing a stale-IP wrong device.
		w.refuseReset(ctx, profile, "this rig is still binding to its device; try again in a moment")
		return
	}

	dev := w.newDevice(ep, profile.User, profile.Password, smbTimeout(w.sites.Site(ep.Site)))
	sess, err := dev.Open(ctx)
	if err != nil {
		w.log.Warn("rig: reset-identity could not connect", "error", err)
		return
	}
	ms := newMountedSession(sess)
	defer ms.Close()

	// Corroborate the device identity WHILE this SMB session is open (same
	// live-connection guarantee as the migration, design device-identity-and-media
	// §3.4): an IP handoff between now and the marker clears would break this
	// established connection rather than silently redirect the clear to a different
	// device. Probe mismatch/no-reply → refuse.
	liveGUID, _, probed := w.resolver.ProbeGUID(ctx, ep)
	if !probed || liveGUID != profile.GUID {
		w.refuseReset(ctx, profile, "couldn't confirm this is the right device right now")
		return
	}

	seen := map[string]bool{}
	cleared, failed := 0, 0
	for _, src := range profile.EnabledSources() {
		if seen[src.Share] {
			continue
		}
		seen[src.Share] = true
		share, err := ms.share(src.Share)
		if err != nil {
			w.log.Warn("rig: reset-identity mount failed", "share", src.Share, "error", err)
			failed++
			continue
		}
		// Clear the media marker and its persisted id so a DELIBERATE swap re-adopts
		// cleanly rather than tripping the swap guard on the new stick's foreign
		// marker forever. The marker is cleared at BOTH the canonical ASIAIR/
		// location and the legacy share root. On removable media the ASIAIR/ copy is
		// authoritative (read first), so a stuck legacy root copy on a read-only root
		// is inert once ASIAIR/ is clear and must not fail the reset; on fixed media
		// the root is the only location. Any leftover legacy rig-identity marker is
		// swept best-effort — it no longer gates anything, so a failure to remove it
		// (a read-only root) must not fail the reset either.
		_ = share.Delete(model.IdentityMarkerName)
		shareFailed := false
		removable := isRemovableShare(src.Share)
		if err := clearMarker(share, path.Join(mediaMarkerPrefix, model.MediaMarkerName)); err != nil {
			w.log.Warn("rig: reset-identity could not delete ASIAIR/ media marker", "share", src.Share, "error", err)
			if removable {
				shareFailed = true
			}
		}
		if err := clearMarker(share, model.MediaMarkerName); err != nil {
			w.log.Warn("rig: reset-identity could not delete root media marker", "share", src.Share, "error", err)
			if !removable {
				shareFailed = true
			}
		}
		// An empty value reads back as "no persisted id" (GetMeta ok, value ""), so
		// the next pass re-adopts via AbsentMarker rather than judging Foreign.
		if err := w.store.SetMeta(ctx, MediaIDKey(w.profileID, src.Share), ""); err != nil {
			w.log.Warn("rig: reset-identity could not clear persisted media id", "share", src.Share, "error", err)
			shareFailed = true
		}
		if shareFailed {
			failed++
			continue
		}
		cleared++
		w.log.Info("rig: cleared media marker; a swapped stick will re-adopt next cycle", "share", src.Share)
	}
	w.log.Info("rig: reset-identity complete", "cleared", cleared, "failed", failed)

	// Auto-resolve this rig's identity/media alerts once every watched share is
	// clear — the operator re-confirmed the device and cleared the media marker,
	// so a standing warning is stale. If any share couldn't be cleared, leave the
	// alert standing.
	if failed == 0 {
		w.resolveAlerts(ctx, model.AlertIdentityMismatch)
		w.resolveAlerts(ctx, model.AlertMediaChanged)
	}
}

// refuseReset logs and raises a one-shot alert that a reset-identity request was
// declined because the device could not be positively identified. Reset is an
// async command with no synchronous reply, so a refusal surfaces on the alert
// channel other worker-side conditions use.
func (w *Worker) refuseReset(ctx context.Context, profile *model.RigProfile, reason string) {
	w.log.Warn("rig: refusing reset-identity", "reason", reason)
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityWarn,
		Category: model.AlertConfig,
		Message:  fmt.Sprintf("couldn't reset identity for rig %q: %s", profile.Name, reason),
	})
}

// clearMarker deletes a share-root marker, treating an already-absent marker as
// success (a share that never carried the marker is already in the desired state).
func clearMarker(share device.Share, name string) error {
	if err := share.Delete(name); err != nil && !device.IsNotFound(err) {
		return err
	}
	return nil
}

// resolveAlerts records that this rig's condition of the given category cleared:
// its standing alerts are resolved (and acknowledged, if the operator had not),
// so the condition raises anew if it returns (design architecture §10), and the
// hub's unacked count is refreshed so the dashboard badge and Alerts view update.
func (w *Worker) resolveAlerts(ctx context.Context, category model.Category) {
	w.resolveAlertsScoped(ctx, category, w.profileID)
}

// resolveFleetAlerts resolves FLEET-WIDE alerts of the given category — those
// raised with an empty rig id, which the store maps to a NULL rig_id (store.go
// nullStr) so one condition on a shared resource dedups to a single row instead
// of one per rig. The destination-volume conditions are exactly that: the NAS is
// fleet-wide, not per-rig. Without this, such an alert could never be resolved
// (the rig-scoped matcher never matches an empty rig id) and would stand forever
// after the condition cleared.
func (w *Worker) resolveFleetAlerts(ctx context.Context, category model.Category) {
	w.resolveAlertsScoped(ctx, category, "")
}

// resolveAlertsByPrefix resolves this rig's standing alerts of the category whose
// message starts with prefix, for a category that carries several conditions.
func (w *Worker) resolveAlertsByPrefix(ctx context.Context, category model.Category, prefix string) {
	w.resolveMatching(ctx, w.profileID, category, prefix)
}

func (w *Worker) resolveAlertsScoped(ctx context.Context, category model.Category, rigID string) {
	w.resolveMatching(ctx, rigID, category, "")
}

func (w *Worker) resolveMatching(ctx context.Context, rigID string, category model.Category, prefix string) {
	n, err := w.store.ResolveAlerts(ctx, rigID, category, prefix, w.now().Unix())
	if err != nil {
		w.log.Warn("rig: could not resolve cleared alerts", "category", category, "error", err)
		return
	}
	if n > 0 {
		if c, err := w.store.UnackedCount(ctx); err == nil {
			w.hub.SetUnackedAlerts(c)
		}
		w.log.Info("rig: resolved cleared alerts", "category", category, "count", n)
	}
}

// listedFrame is one science frame observed in a share listing, with its paired
// preview sidecars (design frame-lifecycle §3).
type listedFrame struct {
	share    string
	path     string // source_path, relative to the share root
	name     string // basename
	size     int64
	mtime    time.Time // device clock domain
	sidecars []model.Sidecar
}

// listedPreview is one excluded preview file (jpg/jpeg) observed in a listing —
// not transferred, tracked as a sidecar of its parent, and swept if its parent
// no longer exists on the device (design frame-lifecycle §3 orphan-preview
// sweep).
type listedPreview struct {
	share string
	path  string
	name  string
}

// walkEntry is one non-junk file found during a recursive listing.
type walkEntry struct {
	path string
	fi   device.FileInfo
}

// joinPath joins a share-relative directory and a child name; the share root is
// the empty string (H2), not ".".
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// walk lists dir recursively, pruning junk directories entirely and dropping
// junk files, appending every surviving file to out (design frame-lifecycle §3).
func (w *Worker) walk(share device.Share, dir string, out *[]walkEntry) error {
	entries, err := share.ListDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := joinPath(dir, e.Name)
		if e.IsDir {
			if walkPrunesDir(e.Name) {
				continue
			}
			if err := w.walk(share, full, out); err != nil {
				return err
			}
			continue
		}
		if walkDropsFile(e.Name) {
			continue
		}
		*out = append(*out, walkEntry{path: full, fi: e})
	}
	return nil
}

// walkPrunesDir and walkDropsFile are walk's junk rules, and isListedPreview is
// pairListing's frame/preview split. The harvest view (harvestview.go) predicts
// the pull listing through these same functions, so a change to what the walk
// keeps is a change to what the device file browser reports as harvested.
func walkPrunesDir(name string) bool { return filter.IsJunkDir(name) }

func walkDropsFile(name string) bool { return filter.IsJunkFile(name) }

func isListedPreview(name string, previewExts []string) bool {
	return filter.IsExcluded(name, previewExts)
}

// pairListing splits a share's surviving files into science frames and preview
// sidecars, pairing each preview to its parent by the strict _thn rule
// (filter.IsSidecarOf). previewExts are the exclude_extensions (jpg/jpeg).
func pairListing(shareName string, entries []walkEntry, previewExts []string) (frames []listedFrame, previews []listedPreview) {
	var science, prev []walkEntry
	for _, e := range entries {
		if isListedPreview(e.fi.Name, previewExts) {
			prev = append(prev, e)
		} else {
			science = append(science, e)
		}
	}
	for _, e := range science {
		lf := listedFrame{
			share: shareName,
			path:  e.path,
			name:  e.fi.Name,
			size:  e.fi.Size,
			mtime: e.fi.Mtime,
		}
		for _, p := range prev {
			if filter.IsSidecarOf(p.fi.Name, e.fi.Name, previewExts) {
				lf.sidecars = append(lf.sidecars, model.Sidecar{
					Path:  p.path,
					Size:  p.fi.Size,
					Mtime: p.fi.Mtime.Unix(),
				})
			}
		}
		frames = append(frames, lf)
	}
	for _, p := range prev {
		previews = append(previews, listedPreview{share: shareName, path: p.path, name: p.fi.Name})
	}
	return frames, previews
}

// isRemovableShare reports whether a share is a removable Udisk medium, which
// gates cleanup on the media-identity marker (eMMC/TF are fixed media, H10).
func isRemovableShare(name string) bool {
	return strings.Contains(strings.ToLower(name), "udisk")
}

// IsRemovableShare is isRemovableShare for callers outside the worker: the
// device file browser's layout guard applies only where resolveDataPrefix can
// find the ASIAIR/-nested layout, the removable share.
func IsRemovableShare(name string) bool { return isRemovableShare(name) }

// regexpTFShare word-anchors "tf" so it matches the microSD share ("TF Images" or
// a bare "tf") but not an incidental substring like "catfish" or "outfit". ASIAIR
// firmware names this share "TF Images".
var regexpTFShare = regexp.MustCompile(`^tf\b|\btf images\b`)

// isTFShare reports whether a lowercased share name is the microSD (TF) slot.
func isTFShare(lower string) bool {
	return regexpTFShare.MatchString(lower)
}

// isRemovableSlot reports whether a share is backed by a removable slot (USB or
// microSD) whose medium can be absent. Unlike isRemovableShare — which gates the
// delete-pass media-identity marker and stays Udisk-only (H10 scopes the marker to
// Udisk) — this predicate governs empty-slot handling for the safe-to-power-off
// verdict and the capacity math, where the microSD slot behaves exactly like the
// USB slot. eMMC is fixed and never matches.
func isRemovableSlot(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "udisk") || isTFShare(n)
}

// removableMediumAbsent reports whether a removable share currently has no medium
// inserted. The ASIAIR keeps exporting a removable Udisk/TF share even with its
// USB/SD slot empty, so a configured capture root resolves nowhere and the listing
// walk hits a not-found — indistinguishable, without this probe, from a genuine
// listing failure. The share root itself always lists; a present medium carries an
// ASIAIR/ folder (the nested layout) or the capture roots directly (the flat one),
// so a root that lists successfully yet holds neither means the slot is empty. A
// root that will not list at all (a transient or permission error) is not treated
// as absent — the caller still surfaces that as a listing failure.
// known=false means the probe could not look at all (the root listing failed).
// Callers must treat that as "cannot tell", never as "a medium is there": this
// oracle gates the safe-to-power-off verdict, so its one unreadable case has to
// fail closed like every other read failure on that path.
func removableMediumAbsent(share device.Share, roots []string) (absent, known bool) {
	entries, err := share.ListDir("")
	if err != nil {
		return false, false
	}
	dirs := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir {
			dirs[strings.ToLower(e.Name)] = true
		}
	}
	if dirs["asiair"] {
		return false, true
	}
	for _, root := range roots {
		if dirs[strings.ToLower(root)] {
			return false, true
		}
	}
	return true, true
}

// MediaIDKey is the store-meta key holding the adopted media id for one rig's
// share — the persisted identity mediaid.Verify judges a later pass's marker
// against, so a swapped stick carrying a foreign marker is caught. Exported
// alongside LastIPKey and PendingGUIDKey so rig deletion can clear it: a key that
// outlives its rig makes a genuinely new stick verify Foreign under a reused id.
func MediaIDKey(rigID, share string) string {
	return "mediaid:" + rigID + ":" + share
}

// LastIPKey is the store-meta key holding a rig's last-known-good device
// endpoint, in netsite's stored form (a bare IP or hostname for the local site),
// the resolution fallback resilient to a discovery/broadcast outage (design
// device-identity-and-media §3.2). The worker writes it after a
// pre-existing-match listing; resolveAddress reads it.
func LastIPKey(rigID string) string { return "lastip:" + rigID }

// ForgetLastEndpointsOnSite clears the last-known endpoint of each rig in rigIDs
// whose stored endpoint names site, and leaves every other value alone: a local
// one, another site's, and one that does not parse. Removing a remote calls it,
// as rig deletion clears its own key, so no rig falls back to an endpoint on a
// site that is gone. A rig whose key cannot be read or cleared does not stop the
// others; every such failure is returned, joined, beside the count cleared.
func ForgetLastEndpointsOnSite(ctx context.Context, st store.Store, rigIDs []string, site netsite.ID) (cleared int, err error) {
	var errs []error
	for _, id := range rigIDs {
		key := LastIPKey(id)
		v, ok, err := st.GetMeta(ctx, key)
		if err != nil {
			errs = append(errs, fmt.Errorf("rig %s: read last-known endpoint: %w", id, err))
			continue
		}
		if !ok || v == "" {
			continue
		}
		ep, err := netsite.Parse(v)
		if err != nil || ep.Site != site {
			continue
		}
		// The store has no delete; an empty value reads back as no endpoint.
		if err := st.SetMeta(ctx, key, ""); err != nil {
			errs = append(errs, fmt.Errorf("rig %s: clear last-known endpoint: %w", id, err))
			continue
		}
		cleared++
	}
	return cleared, errors.Join(errs...)
}

// PendingGUIDKey is the store-meta key holding a rig's pending guid bind, encoded
// "guid,cpuId". The worker records it from a live probe on a pre-existing match;
// the manager applies and clears it (design device-identity-and-media §3.4).
func PendingGUIDKey(rigID string) string { return "pendingguid:" + rigID }

// mediaMarkerPrefix is the removable medium's canonical media-marker directory:
// the ASIAIR capture folder the firmware writes into, which is guest-writable
// even on devices whose share root is not. The marker's legacy share-root
// location is still read as a fallback and migrated forward on a positive
// identity match. Fixed media (EMMC/TF) never carries a media marker, so this
// prefix is irrelevant there.
const mediaMarkerPrefix = "ASIAIR"

// readMediaMarker reads a removable share's media marker, preferring the
// canonical ASIAIR/ location and falling back to the legacy share-root copy only
// when ASIAIR/ holds no marker. foundAtRoot reports which location answered. It
// is READ-ONLY — it never writes or relocates a marker — so the delete phase can
// share it without breaking the "delete pass never writes a marker" invariant
// (delete.go). A not-found at BOTH locations returns an fs.ErrNotExist-matching
// error; any other (transient) read error propagates unchanged so a caller keeps
// deferring rather than falling through to a stale root copy. The ASIAIR/→root
// fallback gates on fs.ErrNotExist — matched by both the smb path's adaptErr
// normalisation and the fake's NotFoundError, unlike the smb1-specific
// device.IsNotFound.
func readMediaMarker(share device.Share) (data []byte, foundAtRoot bool, err error) {
	data, err = share.ReadMarker(path.Join(mediaMarkerPrefix, model.MediaMarkerName))
	if err == nil {
		return data, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	data, err = share.ReadMarker(model.MediaMarkerName)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// mediumUsable reports whether a removable share's medium is confirmed present
// AND is the same medium whose identity was adopted, so its cleanup (and
// vanished-source handling) may proceed (design frame-lifecycle §3, H5/H10). A
// fixed medium is always usable. For a Udisk share the media marker breaks the
// empty-mount tie and, judged against the persisted media id via mediaid.Verify,
// catches a swapped stick carrying a foreign marker. The marker's canonical home
// is ASIAIR/; a legacy share-root copy is read as a fallback and migrated
// forward. The rule is judge-first, migrate-second: the location is fixed, the
// verdict taken, and only then is a matched medium's root marker relocated.
//
//   - Match          → the adopted medium: usable. A root-found marker is
//     migrated into ASIAIR/ (reusing the same id — a relocation, not a fresh
//     adoption).
//   - SelfRecognized → own marker after a local state loss: usable, re-persist,
//     and migrate a root-found copy (inert while no key fingerprint is wired —
//     Generate(_, ""), so the marker carries none to recognise — but handled for
//     when keys exist).
//   - AbsentMarker + populated root → first-cleanup adoption: mint, adopt into
//     ASIAIR/, persist, usable. An empty persisted id is treated the same
//     (re-adopt) wherever the marker sits: with no prior adoption to protect there
//     is nothing for a swap guard to guard, so Foreign is never the right verdict.
//   - AbsentMarker + empty root      → unusable, and absent only when the share
//     root has no data structure either (see `present` below).
//   - Foreign        → the medium was swapped (marker id ≠ persisted, or
//     garbled): unusable, and raise a media-changed alert once. Nothing is
//     written, migrated, or deleted on a medium we did not adopt.
//
// The second result, `present`, answers a DIFFERENT question: was this medium
// proven to physically exist this cycle? It exists because "we may not clean up
// here" and "there is no stick here" are not the same fact, and only the latter
// may suppress the safe-to-power-off verdict (design frame-lifecycle §14). A
// drained stick is unusable (nothing adopted, nothing to clean) yet plainly
// present, and reporting it absent would pin the rig at "unknown" forever. A
// marker or store we cannot read leaves `present` false: that
// is our own failure to know, and the verdict stays conservatively unknown.
//   - Unreadable     → transient read failure: unusable this cycle, no alert.
//
// The own-key fingerprint stays "" — no key infrastructure is wired, so
// self-recognition-by-key never fires; the persisted-id path is the live
// discriminator. Messages are kept free of fluctuating values so alert dedup
// collapses a repeated swap to one row.
func (w *Worker) mediumUsable(ctx context.Context, share device.Share, roots []string, hasScience bool) (usable, present bool) {
	if !isRemovableShare(share.Name()) {
		return true, true
	}
	key := MediaIDKey(w.profileID, share.Name())
	persistedID, _, err := w.store.GetMeta(ctx, key)
	if err != nil {
		// Without the persisted id we cannot tell the adopted medium from a swap.
		// Fail closed: refuse cleanup this cycle rather than risk deleting from a
		// medium we cannot identify. A healthy medium recovers next cycle once the
		// read succeeds; no frame is lost (content hashing still guards deletes).
		w.log.Warn("rig: could not read persisted media id; treating medium as unusable this cycle", "share", share.Name(), "error", err)
		return false, false
	}

	// Locate the marker (ASIAIR/ first, else the legacy root) BEFORE judging, so
	// the same location drives both the read and the verdict. Read-only here: no
	// write or migration happens until an identity match is proven below.
	markerData, foundAtRoot, rerr := readMediaMarker(share)
	switch {
	case rerr == nil:
		// A marker was found; judge it at its location below.
	case errors.Is(rerr, fs.ErrNotExist):
		// No marker at either location: first-contact adoption, or an absent stick.
		return w.adoptMedium(ctx, share, key, roots, hasScience)
	default:
		w.log.Warn("rig: media marker unreadable; treating medium as unusable this cycle", "share", share.Name(), "error", rerr)
		return false, false
	}

	// No persisted identity: there is no prior adoption to protect, so a marker
	// found here is never a swap to refuse. WHERE it sits says which case this is,
	// and the two want opposite treatment.
	if persistedID == "" {
		if foundAtRoot {
			// Post-reset: reset cleared the persisted id and the ASIAIR/ marker but
			// could not delete a legacy root copy (a read-only root). Reset means
			// "forget this identity", so MINT a fresh one — reusing the stale id
			// would resurrect exactly what the operator just cleared.
			return w.adoptMedium(ctx, share, key, roots, hasScience)
		}
		// A marker at the canonical location with no persisted id is local state
		// loss (reinstall, host move), not a reset. Adopt the medium's EXISTING id
		// in place so the install recognises the stick instead of re-branding it:
		// minting here would overwrite the marker, and any other install sharing
		// this medium would then see a foreign marker and be pushed into a
		// permanent media-changed alert as the two took turns claiming it.
		if m, perr := mediaid.Parse(markerData); perr == nil {
			if err := w.store.SetMeta(ctx, key, m.ID); err != nil {
				w.log.Warn("rig: failed to persist the medium's existing media id", "share", share.Name(), "error", err)
				return false, true
			}
			w.log.Info("rig: adopted the medium's existing identity after local state loss", "share", share.Name())
			return true, true
		}
		// An unparseable marker carries no identity to preserve, so there is nothing
		// to adopt in place — mint a fresh one exactly as for a marker-less medium.
		return w.adoptMedium(ctx, share, key, roots, hasScience)
	}

	location := mediaMarkerPrefix
	if foundAtRoot {
		location = ""
	}
	verdict, m := mediaid.Verify(mediaid.PrefixedOrigin(share, location), persistedID, "")
	switch verdict {
	case mediaid.Match:
		if foundAtRoot {
			w.migrateMediaMarker(share, m)
		}
		return true, true
	case mediaid.SelfRecognized:
		// Positively our own medium; re-persist is best-effort — a failed write
		// does not make an identified medium unusable. Dormant while no key
		// fingerprint is wired (Verify is called with ownFingerprint ""), so this
		// arm cannot be reached; it is the correct destination once key
		// infrastructure exists and its state-loss re-persist must be preserved.
		if err := w.store.SetMeta(ctx, key, m.ID); err != nil {
			w.log.Warn("rig: failed to re-persist recognized media id", "share", share.Name(), "error", err)
		}
		w.log.Info("rig: recognized own media marker after state loss; re-persisting", "share", share.Name())
		if foundAtRoot {
			w.migrateMediaMarker(share, m)
		}
		return true, true
	case mediaid.Foreign:
		// A foreign marker proves a medium is inserted — it is simply not ours.
		// Cleanup is refused, but the stick is present, so it must not read absent.
		w.log.Warn("rig: media marker does not match the adopted medium; medium changed, refusing cleanup", "share", share.Name())
		w.raiseMediaChanged(ctx, share.Name())
		return false, true
	default: // mediaid.AbsentMarker (marker vanished mid-check) or Unreadable
		w.log.Warn("rig: media marker unreadable or vanished mid-check; treating medium as unusable this cycle", "share", share.Name())
		return false, false
	}
}

// adoptMedium runs the AbsentMarker path for a removable share: a populated stick
// is first-cleanup adoption — mint a media id, adopt it into ASIAIR/ (proven by
// read-back), and persist it. ASIAIR/ is guaranteed to exist on that path:
// hasScience means captures are present, and on a raw Udisk captures live under
// ASIAIR/, so no directory-create primitive is needed.
//
// With no science and no marker the two candidate states — a removed stick and a
// fully drained one — are indistinguishable by content alone, which is hazard H5.
// removableMediumAbsent settles it structurally: the ASIAIR/ folder (or a
// configured root) survives a drain but goes away with the stick. Neither state is
// usable (there is nothing adopted and nothing to clean), but only a genuinely
// missing medium may read absent and suppress the safe-to-power-off verdict.
//
// The three adoption FAILURES below all return present=true: each is reachable
// only with hasScience true, so frames are sitting on the medium and its presence
// is not in question — adoption failed, presence did not.
func (w *Worker) adoptMedium(ctx context.Context, share device.Share, key string, roots []string, hasScience bool) (usable, present bool) {
	if !hasScience {
		absent, known := removableMediumAbsent(share, roots)
		if !known {
			// The root would not list, so presence is unproven. Fail closed like every
			// other read failure here: an unreadable root must never read "safe".
			w.log.Debug("rig: removable share root unreadable; presence unproven", "share", share.Name())
			return false, false
		}
		if absent {
			w.log.Debug("rig: removable share empty with no medium inserted; treating as absent stick", "share", share.Name())
			return false, false
		}
		w.log.Debug("rig: removable medium present but drained; nothing to adopt", "share", share.Name())
		return false, true
	}
	gen, err := mediaid.Generate(w.profileID, "")
	if err != nil {
		w.log.Warn("rig: failed to generate media marker", "share", share.Name(), "error", err)
		return false, true
	}
	if err := gen.Adopt(mediaid.PrefixedOrigin(share, mediaMarkerPrefix)); err != nil {
		w.log.Warn("rig: could not write media marker; skipping removable cleanup this cycle", "share", share.Name(), "error", err)
		return false, true
	}
	if err := w.store.SetMeta(ctx, key, gen.ID); err != nil {
		// The marker is on the medium but its id is unpersisted. Fail closed:
		// refuse cleanup this cycle. (A persistently failing store means bigger
		// problems; the adopted marker is re-read and re-persisted next pass.)
		w.log.Error("rig: failed to persist adopted media id; treating medium as unusable this cycle", "share", share.Name(), "error", err)
		return false, true
	}
	w.log.Info("rig: adopted media-identity marker (first managed cleanup)", "share", share.Name())
	return true, true
}

// migrateMediaMarker relocates a legacy share-root media marker into ASIAIR/ on a
// medium whose identity was already positively matched. It writes the marker
// under ASIAIR/ and proves the write by reading it back (mediaid.Adopt), then
// deletes the root copy only when the read-back verified. The relocation REUSES
// the medium's existing identity (m.ID) — it is not a fresh adoption, so the
// persisted id is left untouched. Best-effort: a failed ASIAIR/ write, or a
// failed root delete on a read-only root, leaves an authoritative copy in place
// (ASIAIR/ once the write took, else the untouched root) and cleanup still
// proceeds because identity was already matched.
func (w *Worker) migrateMediaMarker(share device.Share, m *mediaid.Media) {
	if err := m.Adopt(mediaid.PrefixedOrigin(share, mediaMarkerPrefix)); err != nil {
		w.log.Warn("rig: could not relocate media marker into ASIAIR/; leaving the legacy root copy in place", "share", share.Name(), "error", err)
		return
	}
	if err := clearMarker(share, model.MediaMarkerName); err != nil {
		w.log.Warn("rig: relocated media marker into ASIAIR/ but could not delete the legacy root copy; the ASIAIR/ copy is authoritative", "share", share.Name(), "error", err)
		return
	}
	w.log.Info("rig: relocated media marker from the share root into ASIAIR/", "share", share.Name())
}

// raiseMediaChanged alerts that a removable medium was swapped: its media marker
// no longer matches the adopted one, so cleanup is refused until an operator
// re-adopts (a reset clears the persisted id and the on-medium marker). The
// message carries no fluctuating value so alert dedup collapses repeats.
func (w *Worker) raiseMediaChanged(ctx context.Context, share string) {
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityCritical,
		Category: model.AlertMediaChanged,
		Message:  "removable medium on share " + share + " changed since adoption; refusing cleanup until re-adopted",
	})
}

// raiseIdentityMismatch alerts that a different device answered at this rig's
// address (a live scan_air probe reported a foreign guid); all pulls and deletes
// are refused until the device is re-confirmed. Raised once per session, so the
// message carries no fluctuating value and alert dedup collapses repeats.
func (w *Worker) raiseIdentityMismatch(ctx context.Context) {
	w.notify.Raise(ctx, model.Alert{
		RigID:    w.profileID,
		Severity: model.SeverityCritical,
		Category: model.AlertIdentityMismatch,
		Message:  "the device answering at this rig's address reported a different identity; refusing all pulls and deletes until it is re-confirmed",
	})
}
