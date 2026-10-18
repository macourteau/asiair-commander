package rig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/filter"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/logs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// pullOutcome is what one pull phase reports to runCycle. refused: the session did
// not open or the identity was not positively confirmed (a foreign device
// answered, or no probe reply), so the caller skips the delete pass too. listed:
// every share was listed and every frame ready to copy was attempted, the walk
// not cut short. listFailed: the walk was cut short by a fault rather than by a
// shutdown or a suspend, a share failing to list or an unreadable destination.
// copied: at least one frame was filed. A run until idle reads the last three for
// its idle test and progress count.
type pullOutcome struct{ refused, listed, listFailed, copied bool }

// pullPhase opens one short session, confirms the connected device is this rig's
// device by a live scan_air probe, lists the configured sources, and pulls every
// quiescent new/changed frame, filing each one atomically (design
// frame-lifecycle §2–§8).
func (w *Worker) pullPhase(ctx context.Context, cfg *model.Config, cy cycle, dev device.Device) (out pullOutcome) {
	profile, ep := cy.profile, cy.ep
	// Clear any prior "waiting to settle" count up front: a cycle that refuses
	// (session open failure, foreign or unconfirmed identity) returns before the
	// listing loop, and must not leave a stale count frozen on the card.
	w.st.WaitingToSettle = 0
	// The structurally-confirmed-empty slot set is per-cycle like the verdict: clear
	// it up front so a refusing cycle (offline, foreign/unconfirmed identity) that
	// returns before publishListingStatus publishes an empty set rather than a stale
	// membership that the disable guard would trust.
	w.st.EmptySlots = nil
	// Reset the live-feed pending accumulators for this cycle. PendingListed defaults
	// to false so every publish before the authoritative end-of-listing snapshot (and
	// every refusing cycle) carries false, and the client leaves pending rows alone.
	w.cycleCandidates = nil
	w.terminalThisCycle = map[string]bool{}
	w.destUnavailable = false
	w.st.PendingListed = false
	// A cycle that refuses below never measures an offset, so the clock phase reads
	// zero and cannot act on a stale reading from an earlier cycle.
	w.cycleOffset = 0
	sess, err := dev.Open(ctx)
	if err != nil {
		w.metrics.RecordPullPhaseRefused(w.profileID, metrics.RefuseSessionOpenFailed)
		w.log.Warn("rig: session open failed; skipping cycle", "error", err)
		w.setState(model.RigStateOffline)
		w.st.Online = false
		w.setSafeVerdict(model.SafeUnknown, "offline")
		w.publish()
		return pullOutcome{refused: true}
	}
	ms := newMountedSession(sess)
	defer ms.Close()

	// Confirm the device answering at this address is really ours, by a live
	// scan_air probe taken WHILE this SMB session is open (design
	// device-identity-and-media §4). Identity is a property of the device, not
	// each share, so it is confirmed once here, not per share.
	confirmed, foreign := w.confirmDeviceIdentity(ctx, profile, ep)
	switch {
	case foreign:
		// A different device answered at this address. Refuse everything this
		// cycle and alert; the delete pass is skipped via refused=true.
		w.noConfirmStreak = 0
		w.metrics.RecordPullPhaseRefused(w.profileID, metrics.RefuseIdentityForeign)
		w.raiseIdentityMismatch(ctx)
		w.setState(model.RigStateError)
		w.st.LastError = "identity mismatch: a different device answered at this address; refusing pulls and deletes"
		w.setSafeVerdict(model.SafeUnknown, "identity")
		w.endRunForIdentity()
		w.publish()
		return pullOutcome{refused: true}
	case !confirmed:
		// No scan_air reply: identity is unconfirmed, not proven wrong. Fail closed
		// (refuse this session) and, after three consecutive no-reply cycles, raise a
		// single degraded alert. We bail before listing, so listingOK is irrelevant.
		w.st.State = model.RigStateIdle
		w.st.Online = true
		w.setSafeVerdict(model.SafeUnknown, "identity")
		w.st.Current = nil
		w.publish()
		w.metrics.RecordPullPhaseRefused(w.profileID, metrics.RefuseIdentityUnconfirmed)
		w.noConfirmStreak++
		if w.noConfirmStreak == 3 {
			w.notify.Raise(ctx, model.Alert{
				RigID:    w.profileID,
				Severity: model.SeverityWarn,
				Category: model.AlertIdentityUnconfirmed,
				Message:  "can't confirm this rig's device identity (no scan_air reply); pulls are paused",
				Standing: true,
			})
		}
		return pullOutcome{refused: true}
	}
	// Identity confirmed. Clear any standing degraded alert and cache the address
	// as the last-known-good device IP (the host-bound guid bind, if any, was
	// already recorded inside confirmDeviceIdentity from that same probe).
	w.noConfirmStreak = 0
	w.resolveAlerts(ctx, model.AlertIdentityUnconfirmed)
	w.recordLastKnownIP(ctx, ep)

	cl := w.perConnectClock(sess.ServerTime())
	w.recordClockOffset(cl.Offset(), profile.SyncClock)
	excludeExts := cfg.ExcludeExtensions

	w.settle.beginCycle()
	defer w.settle.endCycle()

	// Merge sources that name the same share (each share is mounted, identity-
	// verified, and listed once).
	shareRoots := map[string][]string{}
	var order []string
	for _, src := range profile.EnabledSources() {
		if _, ok := shareRoots[src.Share]; !ok {
			order = append(order, src.Share)
		}
		shareRoots[src.Share] = append(shareRoots[src.Share], src.Roots...)
	}

	var allFrames []listedFrame
	var allPreviews []listedPreview
	listingOK := true
	// Absent media are tracked per share, not as one flag: an absent removable
	// share must be excluded from the capacity math (its statfs reports the
	// ASIAIR's own root filesystem, not a stick), while any absent share at all
	// still suppresses the safe verdict.
	absentShares := map[string]bool{}
	// emptySlots is the STRUCTURALLY-confirmed-empty subset published as
	// RigStatus.EmptySlots to gate the per-share disable guard. It is deliberately
	// narrower than absentShares: absentShares also captures a frame-bearing share
	// hit by a transient marker/store read error (mediumUsable returns present=false
	// for both a missing stick and an I/O error), which is fine for the verdict but
	// fatal for a disable authorization that permanently drops the share. Only a
	// proven-empty slot — one whose root listed successfully yet holds no ASIAIR/ and
	// no configured root — joins this set.
	emptySlots := map[string]bool{}

	for _, shareName := range order {
		share, err := ms.share(shareName)
		if err != nil {
			w.log.Warn("rig: mount failed", "share", shareName, "error", err)
			listingOK = false
			continue
		}
		// Opportunistically remove any leftover legacy rig-identity marker while we
		// hold the share; identity is confirmed by the live probe, so the marker
		// carries nothing Commander reads. Silent on all errors (a read-only root may
		// keep it — harmless; the filter already excludes it from listings).
		_ = share.Delete(model.IdentityMarkerName)
		// Resolve where this share's capture folders actually live. A fixed EMMC/TF
		// share IS the data folder (roots sit at the share root); a raw removable
		// Udisk nests everything under a top-level ASIAIR/ folder, so the same roots
		// live at ASIAIR/<root>. Resolving the prefix once here means the recursive
		// walk — and every pull/delete/orphan path it builds — inherits it, and the
		// session-log pull below reads log/ from the matching base.
		prefix, err := w.resolveDataPrefix(share, shareRoots[shareName])
		if err != nil {
			w.log.Warn("rig: list failed", "share", shareName, "error", err)
			listingOK = false
			continue
		}
		entries, err := w.listShareRoots(share, prefix, shareRoots[shareName])
		if err != nil {
			// An unreadable root (known=false) is not an empty slot: fall through to
			// the hard listing failure below, which reads unknown either way.
			slotEmpty, known := removableMediumAbsent(share, shareRoots[shareName])
			if isRemovableSlot(shareName) && device.IsNotFound(err) && slotEmpty && known {
				// An empty USB/SD slot: the ASIAIR still exports the (empty) removable
				// share, so the configured root resolves nowhere and the walk not-founds.
				// That is a missing stick, not a listing failure — skip it with a stable
				// Debug line (not a per-cycle WARN) and mark the medium absent, so it
				// neither counts as a hard listing failure nor lets the rig read
				// safe-to-power-off, exactly like a share whose media marker reads absent.
				w.log.Debug("rig: removable share has no medium inserted; skipping listing", "share", shareName)
				absentShares[shareName] = true
				// removableMediumAbsent returned slotEmpty && known: a structurally proven
				// empty slot (no ASIAIR/, no configured root), so it holds no frames and is
				// safe to offer for disabling.
				emptySlots[shareName] = true
				continue
			}
			w.log.Warn("rig: list failed", "share", shareName, "error", err)
			listingOK = false
			continue
		}
		// Record the completed listing size for this share, stamped now. Set ONLY on a
		// successful pass (the failure branches above `continue` without touching it),
		// so a frozen count with an old timestamp reads as stale rather than fresh —
		// the visible signature of a silently truncated listing (design architecture
		// §17.7).
		w.metrics.SetListingEntries(w.profileID, shareName, len(entries), w.now().Unix())
		frames, previews := pairListing(shareName, entries, excludeExts)
		// Only "the medium is provably gone" may suppress the safe-to-power-off
		// verdict; "we may not clean up here" must not (a drained stick is unusable
		// but plainly present). The cleanup verdict is consumed by the delete pass
		// via its own read, so it is discarded at this site.
		if _, present := w.mediumUsable(ctx, share, shareRoots[shareName], len(frames) > 0); isRemovableShare(shareName) && !present {
			absentShares[shareName] = true
			// present=false is returned identically for a structurally-absent stick AND a
			// transient marker/store read error on a frame-bearing share, so it must NOT
			// key the disable set. Re-probe the share root structurally: only a proven
			// slotEmpty && known joins EmptySlots, so a transient error on a share that
			// still holds frames never becomes disable-eligible (invariant D3).
			if slotEmpty, known := removableMediumAbsent(share, shareRoots[shareName]); slotEmpty && known {
				emptySlots[shareName] = true
			}
		}
		allFrames = append(allFrames, frames...)
		allPreviews = append(allPreviews, previews...)

		// Session-log capture (design architecture §11) runs within this same per-rig
		// session — one session per device — using relaxed rules (no quiescence, no
		// delete, no parse). The ASIAIR keeps its logs in a `log/` folder beside
		// Autorun/Live/Plan (design frame-lifecycle §3), not under each watched capture
		// root — so pull from the data base once (the resolved prefix, so a Udisk's
		// ASIAIR/log is found), not per watched root. A log-pull failure never fails the
		// frame cycle.
		if profile.PullLogs {
			lp := logs.New(w.lfs, w.store, cfg.Roots.Logs, w.log)
			if err := lp.Pull(ctx, share, prefix, w.profileID); err != nil {
				w.log.Warn("rig: log pull failed", "share", shareName, "error", err)
			}
		}
	}

	// The walk keeps the pill it inherited: Pulling is set only when a transfer
	// actually starts, so a walk with nothing to copy never reads "Transferring".
	listingTS := w.now().Unix()
	pendingWork := 0
	// readyToCopy counts the frames this walk left ready to copy: settled, not held
	// or failed, admitted, and not filed by the end of the walk. It alone decides
	// whether the listing's pill reads Pulling; pendingWork still decides the Safe
	// verdict, so settling, held and failed frames keep Safe at Working.
	readyToCopy := 0
	// missingWithSource is set when a frame whose device source is still listed is
	// marked missing on the NAS: the card holds the only copy, so the rig must not
	// report "safe to power off" this cycle.
	missingWithSource := false
	// waiting counts frames discovered on the device but not yet past the
	// settle/quiescence gate, surfaced as a plain-language "still saving" liveness
	// line. Reset each cycle so it reflects only this listing.
	waiting := 0

	// A staging write fault from an earlier cycle stands until the destination
	// takes a write again. Settle that with a probe before streaming anything, so a
	// broken volume is not re-proven by downloading a whole frame to throw away.
	if w.destWriteErr != nil {
		w.probeStaging(ctx, cfg)
	}

	for _, lf := range allFrames {
		w.drainCommands(ctx)
		if ctx.Err() != nil || w.workSuspended() || w.destUnavailable {
			break
		}
		pull, prior := w.decide(ctx, cfg, profile, lf)
		if prior != nil && prior.MissingSeenAt != 0 {
			missingWithSource = true
		}
		if !pull {
			// Not pulled: it only counts against "safe to power off" if it is not
			// already safely on the NAS. Both verified and unclassified frames have a
			// byte-verified NAS copy (an unclassified frame was verified at rest before
			// being filed to the quarantine area) — powering off loses nothing, so
			// neither blocks "safe". held/failed/un-pulled frames still count as
			// pending. A frame marked missing does not pin the rig at Pulling, which
			// would freeze it there — the missingWithSource downgrade above is what keeps
			// the power-off status honest for it. (The unclassified backlog is surfaced
			// separately as an alert.)
			if prior == nil || (!safelyOnNAS(prior.State) && prior.MissingSeenAt == 0) {
				pendingWork++
			}
			continue
		}
		pendingWork++ // needs work (or mid-transfer this cycle)
		// Record every pull-candidate for the live feed's pending set (design features
		// §4.3). A single append here — before the quiescence check and before transfer
		// — covers all non-terminal shapes uniformly (still-settling,
		// admission-deferred, transiently-retrying, mount-lost skip). Terminal outcomes
		// are subtracted via terminalThisCycle when the published set is built at
		// end-of-listing.
		w.cycleCandidates = append(w.cycleCandidates, model.PendingFrame{Share: lf.share, Source: lf.path, Size: lf.size})
		if !w.quiescent(cfg, profile, lf, cl) {
			waiting++
			w.log.Debug("rig: frame not quiescent yet; deferring", "path", lf.path)
			continue
		}
		// A staging write fault skips the copy but not the walk: every uncopied frame
		// must still be counted, or an empty tally reads "drained" and, on an idle
		// listing, "safe to power off" over frames that exist only on the device.
		if w.destWriteErr != nil {
			continue
		}
		share, err := ms.share(lf.share)
		if err != nil {
			w.log.Warn("rig: mount lost before pull", "share", lf.share, "error", err)
			continue
		}
		res := w.transferGuarded(ctx, cfg, cy, share, lf, prior)
		if res.filed {
			out.copied = true
		}
		// A frame admission turned away is not ready to copy; one whose copy started
		// and reached no terminal outcome (a transient failure) is retried next cycle.
		if res.started && !w.terminalThisCycle[settleKey(lf)] {
			readyToCopy++
		}
		w.devicePending = pendingWork
		w.refreshLiveCounters(ctx, profile, cl.Offset()) // live "transferred tonight" + "kept"/"resident" counts + next-frame prediction over SSE
	}

	// The orphan sweep deletes parentless preview/thumbnail files from the DEVICE,
	// so it is gated by keep-originals too: a rig set to never delete from the
	// device must not remove anything there, previews included.
	if listingOK && !profile.KeepOriginals && ctx.Err() == nil && !w.workSuspended() {
		w.orphanSweep(ctx, ms, shareRoots, allFrames, allPreviews, excludeExts)
	}

	if w.destUnavailable {
		// One line per cycle for the whole condition; the per-frame detail is Debug.
		w.log.Warn("rig: destination volume unreadable; abandoned the rest of this cycle", "frames_pending", len(allFrames))
	}
	if w.destWriteErr != nil {
		w.log.Warn("rig: staging area not writable; pulls paused until a write probe passes", "staging", cfg.Roots.Staging, "error", w.destWriteErr)
	}

	w.st.WaitingToSettle = waiting
	// Publish the live-feed pending set: this cycle's pull-candidates minus those that
	// reached a terminal outcome (filed or hard-failed). What remains is every frame
	// with no terminal outcome — settling, admission-deferred, or transiently retrying
	// — so the feed never sees a real in-flight frame "absent from a complete listing"
	// and never wrongly drops it (design features §4.3; §4.1 decision 4, "d1").
	pending := make([]model.PendingFrame, 0, len(w.cycleCandidates))
	for _, c := range w.cycleCandidates {
		if !w.terminalThisCycle[c.Share+"\x00"+c.Source] {
			pending = append(pending, c)
		}
	}
	w.st.Pending = pending
	// PendingListed marks this as the one authoritative, genuinely-complete listing
	// snapshot the client may reconcile/d1-remove against. Only a fully-listed
	// (listingOK), non-aborted (ctx ok), non-suspended pass qualifies — a per-share
	// listing failure or a shutdown/pause break would publish a partial set that must
	// not be treated as complete. Deliberately NOT gated on !KeepOriginals (unlike the
	// orphan sweep above): a keep-originals rig must still reconcile its pending rows.
	// A destination-unavailable abort truncates the pass the same way a pause does,
	// so it disqualifies the snapshot too — otherwise the client would d1-remove
	// pending rows for frames still sitting on the device.
	w.st.PendingListed = listingOK && ctx.Err() == nil && !w.workSuspended() && !w.destUnavailable
	sig := listingSignature(allFrames)
	prev := w.prevListingSig
	if listingOK && len(w.ownDeletes) > 0 {
		// A listing that differs from the last one only by files this worker removed
		// after proving them on the NAS shows no device activity, which is exactly
		// what an unchanged listing shows. Keys are exact (share, path), so no other
		// change — a new capture, a rewritten path, a changed size or mtime — is
		// discounted, and a partial listing compares against the raw signature.
		prev = listingSignature(withoutKeys(w.prevListingFrames, w.ownDeletes))
	}
	idle := sig == prev
	w.prevListingSig, w.prevListingFrames = sig, allFrames
	clear(w.ownDeletes) // the set means "since the previous listing", complete or not
	// A partial listing, or a walk cut short — a shutdown, a pause, an unreadable
	// destination — did not attempt every frame ready to copy, so it cannot show
	// that none is left. A run until idle behind such a fault ends on its
	// no-progress count instead.
	out.listed = listingOK && ctx.Err() == nil && !w.workSuspended() && !w.destUnavailable
	// A shutdown or a suspend ends the run on its own terms; only a fault that keeps
	// the listing from completing is one a run can sit behind indefinitely.
	out.listFailed = (!listingOK || w.destUnavailable) && ctx.Err() == nil && !w.workSuspended()
	w.publishListingStatus(ctx, cfg, profile, ms, order, listingOK, absentShares, emptySlots, pendingWork, readyToCopy, idle, listingTS, cl.Offset(), missingWithSource)
	return out
}

// confirmDeviceIdentity corroborates that the device answering at ep is this
// rig's device, by a live unicast scan_air probe taken WHILE the SMB session is
// open (design device-identity-and-media §4), sent through ep's site — the site
// that carries the session. It returns:
//
//   - ok=true,  foreign=false — confirmed: the probed guid equals profile.GUID,
//     or (when both are present) the probed cpuId equals profile.CPUID.
//   - ok=false, foreign=true  — a probed, bound rig whose device reported neither
//     the stored guid nor cpuId: a wrong device answered. Refuse and alert.
//   - ok=false, foreign=false — no scan_air reply: identity is unconfirmed, not
//     proven wrong. Refuse (fail closed), but do not alert as a mismatch.
//
// A host-bound rig (no guid yet) has no stored identity to match against; it
// trusts the device its Host address probes and records a pending guid bind from
// THIS probe, exactly as the Host binding already trusts that address — no
// separate re-probe. cpuId stays a bonus fallback: the CM4 device class reports
// an empty cpuId, so both-empty falls through to the guid compare.
func (w *Worker) confirmDeviceIdentity(ctx context.Context, profile *model.RigProfile, ep netsite.Endpoint) (ok, foreign bool) {
	guid, cpuID, probed := w.resolver.ProbeGUID(ctx, ep)
	if !probed {
		return false, false // no reply: unconfirmed, not proven foreign
	}
	if profile.GUID == "" {
		// Host-bound TOFU-by-host: adopt the guid the Host address reports, reusing
		// this probe (the manager binds it within a maintenance tick).
		w.recordPendingGUIDValues(ctx, guid, cpuID)
		return true, false
	}
	if identityMatches(profile, guid, cpuID) {
		return true, false
	}
	return false, true
}

// identityMatches is the identity comparison for a bound rig's probe answer:
// the probed guid equals profile.GUID, or, when both are present, the probed
// cpuId equals profile.CPUID.
func identityMatches(profile *model.RigProfile, guid, cpuID string) bool {
	return guid == profile.GUID || (profile.CPUID != "" && cpuID != "" && cpuID == profile.CPUID)
}

// IdentityMatches is identityMatches for callers outside the worker (the device
// file browser re-proves each session's device with the worker's own rule).
func IdentityMatches(profile *model.RigProfile, guid, cpuID string) bool {
	return identityMatches(profile, guid, cpuID)
}

// recordPendingGUIDValues records a pending guid bind (encoded "guid,cpuId") the
// manager applies (design device-identity-and-media §3.4), reusing values a
// caller already probed rather than re-probing. A blank guid is never persisted
// — a ",<cpuId>" key would bind nothing. Best-effort: a failed write just
// retries next cycle.
func (w *Worker) recordPendingGUIDValues(ctx context.Context, guid, cpuID string) {
	if guid == "" {
		w.log.Debug("rig: probe reported no guid; nothing to bind this cycle")
		return
	}
	if err := w.store.SetMeta(ctx, PendingGUIDKey(w.profileID), guid+","+cpuID); err != nil {
		w.log.Warn("rig: could not record pending guid bind", "error", err)
		return
	}
	w.log.Info("rig: recorded pending guid bind from live probe", "guid", guid)
}

// recordLastKnownIP caches ep as the rig's last-known-good device endpoint, in
// its stored form, only writing when it changed (design
// device-identity-and-media §3.2). The stored form names the site, so the
// cached address is only ever dialled again on the network it was confirmed on;
// a local endpoint stores the bare address, as it always has.
func (w *Worker) recordLastKnownIP(ctx context.Context, ep netsite.Endpoint) {
	stored := ep.String()
	if cur, ok, err := w.store.GetMeta(ctx, LastIPKey(w.profileID)); err == nil && ok && cur == stored {
		return
	}
	if err := w.store.SetMeta(ctx, LastIPKey(w.profileID), stored); err != nil {
		w.log.Warn("rig: could not record last-known IP", "error", err)
		return
	}
	w.log.Debug("rig: recorded last-known endpoint", "endpoint", stored)
}

// transferResult is what one frame's transfer reports to the listing walk.
// started: admission passed and the stream started, which tells a frame deferred
// by admission (not ready to copy) from one whose copy started and did not finish.
type transferResult struct {
	filed   bool
	started bool
}

// transferGuarded wraps one frame's transfer in a per-file recover so a
// malformed header or an unexpected value turns THAT frame into a failed row
// plus an alert, never a panic that takes down the worker or the service (design
// frame-lifecycle, design architecture §12). transfer's own defers (admission
// release, staging-temp removal) run as the panic unwinds, before this recover
// marks the frame failed. It reports what transfer does; a recovered panic
// filed nothing, and reports no stream started, since marking the frame failed
// is a terminal outcome.
func (w *Worker) transferGuarded(ctx context.Context, cfg *model.Config, cy cycle, share device.Share, lf listedFrame, prior *model.Frame) (res transferResult) {
	defer func() {
		if r := recover(); r != nil {
			// The panic unwinds OUT of transfer rather than returning, so this is where
			// the panic outcome is recorded, not inside transfer (hazard P7).
			w.metrics.RecordTransferOutcome(w.profileID, metrics.OutcomePanic)
			w.log.Error("rig: per-file panic recovered", "path", lf.path, "panic", fmt.Sprintf("%v", r))
			w.markFailed(ctx, cfg, lf, prior, model.FailPanic, fmt.Sprintf("panic during transfer: %v", r))
		}
	}()
	return w.transfer(ctx, cfg, cy, share, lf, prior)
}

// transfer pulls one frame: stream + stream-hash into a staging temp, verify the
// byte count and re-stat the source, verify the copy at rest with a cache-
// bypassing read-back, then classify, route, and atomically file it (design
// frame-lifecycle §5–§8). Admission is acquired here and released exactly once
// at the terminal outcome (filed or aborted) via defer; the staging temp is
// removed on every exit path.
func (w *Worker) transfer(ctx context.Context, cfg *model.Config, cy cycle, share device.Share, lf listedFrame, prior *model.Frame) transferResult {
	profile := cy.profile
	started := false
	// out records the terminal outcome at each exit and reports nothing filed, so
	// every non-filed path is counted exactly once (the filed/file_failed pair is
	// recorded at the classifyAndFile delegation, and panic in transferGuarded —
	// hazards P6/P7).
	out := func(o metrics.TransferOutcome) transferResult {
		w.metrics.RecordTransferOutcome(w.profileID, o)
		return transferResult{started: started}
	}

	// Admission wait is measured around Acquire; the transfer work timer starts only
	// after Acquire returns, so queueing does not inflate the transfer histogram (P8).
	waitStart := w.now()
	release, err := w.adm.Acquire(ctx, w.requester(cy.site), lf.size)
	w.metrics.ObserveAdmissionWait(w.profileID, w.now().Sub(waitStart).Seconds())
	if err != nil {
		if errors.Is(err, ErrDestinationUnavailable) {
			// Cycle-level, not per-frame: every queued frame would fail identically.
			// Latch it so the listing loop abandons the rest of this cycle and logs
			// once; a per-frame WARN here floods the log with one line per frame.
			w.destUnavailable = true
			w.log.Debug("rig: admission aborted; destination unreadable", "path", lf.path, "error", err)
			return out(metrics.OutcomeDestUnavailable)
		}
		if errors.Is(err, ErrDiskReserve) {
			// A soft skip for lack of disk headroom — distinct from a ctx-cancel abort,
			// though the two share this log line.
			w.log.Warn("rig: admission acquire aborted", "path", lf.path, "error", err)
			return out(metrics.OutcomeDiskReserve)
		}
		w.log.Warn("rig: admission acquire aborted", "path", lf.path, "error", err)
		return out(metrics.OutcomeAdmissionAborted)
	}
	defer release()
	workStart := w.now()

	w.st.Current = &model.CurrentFile{File: lf.name, Share: lf.share, Source: lf.path, Pct: 0}
	// The copy starts here, so this is where the pill turns to Transferring; a
	// listing walk alone never sets it.
	w.setState(model.RigStatePulling)
	started = true
	w.hub.Emit(hub.Event{Name: "frame.started", Data: map[string]any{"rig_id": w.profileID, "source": lf.path}})
	w.publish()
	defer func() { w.st.Current = nil }()

	staging := cfg.Roots.Staging
	// Every staging failure below records destWriteErr: the destination is refusing
	// writes, which pauses pulls and alerts rather than failing each frame in turn.
	if err := w.lfs.MkdirAll(staging); err != nil {
		w.log.Error("rig: staging mkdir failed", "staging", staging, "error", err)
		w.destWriteErr = err
		return out(metrics.OutcomeStagingMkdirFailed)
	}
	tmp, err := os.CreateTemp(staging, "asiair-*.tmp")
	if err != nil {
		w.log.Error("rig: create staging temp failed", "staging", staging, "error", err)
		w.destWriteErr = err
		return out(metrics.OutcomeTempCreateFailed)
	}
	tmpPath := tmp.Name()
	// A defer removes the temp on EVERY exit path (mismatch, cancel, I/O error,
	// recovered panic). After a successful rename the temp is already gone, so the
	// Remove simply no-ops.
	defer func() { _ = w.lfs.Remove(tmpPath) }()

	rc, err := share.OpenReader(lf.path)
	if err != nil {
		_ = tmp.Close()
		if device.IsNetwork(err) {
			w.log.Warn("rig: open source transient error; will retry", "path", lf.path, "error", err)
			return out(metrics.OutcomeOpenSourceTransient)
		}
		w.markFailed(ctx, cfg, lf, prior, model.FailOpenSource, "open source: "+err.Error())
		return out(metrics.OutcomeOpenSourceFailed)
	}
	h := sha256.New()
	buf := make([]byte, header.CopyBufSize)
	// pw counts bytes as they flow and throttles a progress/throughput publish to
	// once per progressSampleInterval, so a fast transfer doesn't publish on every
	// buffer. The hash is still fed through the MultiWriter.
	pw := &progressWriter{dst: io.MultiWriter(stagingWriter{tmp}, h), total: lf.size, now: w.now}
	pw.onSample = func(pct int, bytesPerSec int64) {
		cur := w.st.Current
		if cur == nil {
			return
		}
		// Replace the pointer rather than mutate through it: a published RigStatus
		// shares this *CurrentFile with the hub snapshot and its SSE/HTTP readers, so
		// writing the struct in place would race their marshal. Progressed builds a
		// fresh value carrying the file identity (share+source) through, so the
		// transferring row keeps matching its key across every sample.
		w.st.Current = cur.Progressed(pct, bytesPerSec)
		w.publish()
	}
	n, cerr := io.CopyBuffer(pw, rc, buf)
	_ = rc.Close()
	// Count bytes actually streamed over SMB on every exit from here on, not only on
	// success: a rig that streams and fails repeatedly must not report zero bytes.
	w.metrics.AddTransferBytes(w.profileID, n)
	if cerr != nil {
		_ = tmp.Close()
		if errors.Is(cerr, errStagingWrite) {
			w.log.Error("rig: write to staging temp failed", "path", lf.path, "error", cerr)
			w.destWriteErr = cerr
			return out(metrics.OutcomeCopyFailed)
		}
		w.log.Warn("rig: stream copy failed; discarding temp, will retry", "path", lf.path, "error", cerr)
		return out(metrics.OutcomeCopyFailed)
	}
	streamHash := hex.EncodeToString(h.Sum(nil))
	if n != lf.size {
		_ = tmp.Close()
		w.log.Warn("rig: streamed byte-count mismatch; discarding temp", "path", lf.path, "want", lf.size, "got", n)
		return out(metrics.OutcomeShortRead)
	}
	// Copy done: show 100% and clear the live rate while the at-rest verify runs.
	if cur := w.st.Current; cur != nil {
		w.st.Current = cur.Progressed(100, 0)
		w.publish()
	}
	restat, serr := share.Stat(lf.path)
	if serr != nil {
		_ = tmp.Close()
		w.log.Warn("rig: re-stat source failed; discarding temp, will retry", "path", lf.path, "error", serr)
		return out(metrics.OutcomeRestatFailed)
	}
	if restat.Size != lf.size || !clock.FromOrigin(restat.Mtime).EqualWithin(clock.FromOrigin(lf.mtime), mtimeTol) {
		_ = tmp.Close()
		w.log.Warn("rig: source changed during pull; discarding temp, will re-pull", "path", lf.path)
		return out(metrics.OutcomeSourceChanged)
	}
	if err := w.syncTemp(tmp); err != nil {
		_ = tmp.Close()
		w.log.Error("rig: fsync staging temp failed", "path", lf.path, "error", err)
		w.destWriteErr = err
		return out(metrics.OutcomeSyncFailed)
	}
	if err := tmp.Close(); err != nil {
		w.log.Error("rig: close staging temp failed", "path", lf.path, "error", err)
		w.destWriteErr = err
		return out(metrics.OutcomeCloseFailed)
	}

	// Leg 1 of the three-way data-unit agreement that authorizes a supersede
	// (design frame-lifecycle §11.7): the exposure identity is teed off the AT-REST
	// verify below — the read that proves what the disk holds — never off the SMB
	// stream above, which is a distinct read reconciled with it only by the
	// diskHash != streamHash check that follows. The offset costs the header blocks
	// alone, so the file is still read whole exactly once.
	dataOff, oerr := header.DataOffset(tmpPath)
	if oerr != nil {
		// Not a statement about the header: DataOffset reports offset 0 and no error
		// for one that does not parse. A genuine read failure leaves the identity
		// unrecorded, which costs the frame nothing but the ordinary path, and the
		// verify below reads the same file and reports the failure that matters.
		w.log.Warn("rig: data-unit offset read failed; filing without an exposure identity", "path", lf.path, "error", oerr)
	}
	diskHash, dataHash, verr := w.lfs.VerifyReadTee(tmpPath, lf.size, dataOff)
	if verr != nil {
		w.log.Error("rig: at-rest verify read failed; discarding temp, will re-pull", "path", lf.path, "error", verr)
		return out(metrics.OutcomeVerifyReadFailed)
	}
	if diskHash != streamHash {
		w.log.Error("rig: at-rest verify MISMATCH; discarding temp, will re-pull", "path", lf.path, "stream", streamHash, "disk", diskHash)
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityCritical,
			Category: model.AlertVerifyMismatch,
			Message:  "at-rest verify mismatch for " + lf.path + " (write fault or bad sector); re-pulling",
		})
		return out(metrics.OutcomeVerifyMismatch)
	}

	// Delegation: classifyAndFile reports fileDeferred on real filing failures (night
	// assignment, dest mkdir, rename, stat, at-rest verify), so record filed only on
	// its fileFiled result — never on the delegation itself (hazard P6).
	// transfer_duration is observed only for a filed frame, from after admission to
	// here (P8). Its third case is a terminal outcome that files nothing — a revision
	// of a frame the operator culled on the NAS, whose anchors the filing path
	// advanced without writing an archive copy — and it earns an outcome of its own
	// rather than the file_failed a deferral records, the closed outcome set carrying
	// one value per exit path (design frame-lifecycle §11.12).
	// Not named `out`: that is the early-exit recorder above.
	filed := false
	switch res := w.classifyAndFile(ctx, cfg, profile, lf, prior, tmpPath, diskHash, dataHash); res {
	case fileFiled:
		filed = true
		w.metrics.ObserveTransferDuration(w.profileID, w.now().Sub(workStart).Seconds())
		w.metrics.RecordTransferOutcome(w.profileID, metrics.OutcomeFiled)
	case fileNothingFiled:
		w.metrics.RecordTransferOutcome(w.profileID, metrics.OutcomeCulledRevision)
	default:
		w.metrics.RecordTransferOutcome(w.profileID, metrics.OutcomeFileFailed)
	}
	return transferResult{filed: filed, started: true}
}

// orphanSweep removes a preview from the device only once its parent frame no
// longer exists on the device — nothing to protect (design frame-lifecycle §3). A
// preview whose parent is still resident is kept (deleted with the parent at
// delete time, or preserved while the parent stays unresolved).
func (w *Worker) orphanSweep(ctx context.Context, ms *mountedSession, shareRoots map[string][]string, frames []listedFrame, previews []listedPreview, excludeExts []string) {
	for _, p := range previews {
		if ctx.Err() != nil {
			return
		}
		hasParent := false
		for _, f := range frames {
			if f.share == p.share && filter.IsSidecarOf(p.name, f.name, excludeExts) {
				hasParent = true
				break
			}
		}
		if hasParent {
			continue
		}
		share, err := ms.share(p.share)
		if err != nil {
			continue
		}
		// This delete needs the cleanup verdict, not the presence one.
		if usable, _ := w.mediumUsable(ctx, share, shareRoots[p.share], true); isRemovableShare(p.share) && !usable {
			continue
		}
		if err := share.Delete(p.path); err != nil {
			w.log.Debug("rig: orphan preview delete failed", "path", p.path, "error", err)
			continue
		}
		w.log.Info("rig: swept orphan preview (parent no longer on device)", "path", p.path)
	}
}

// perConnectClock builds the per-session clock carrying the measured device-time
// offset, over the worker's injectable wall clock so min_file_age is
// deterministic in tests (equivalent to clock.ForSession otherwise).
func (w *Worker) perConnectClock(st clock.ServerTime) *clock.Clock {
	var offset time.Duration
	if !st.Time.IsZero() && !st.ReceivedAt.IsZero() {
		offset = st.Time.Sub(st.ReceivedAt)
	}
	return clock.NewWithSources(offset, w.now, func() time.Duration { return 0 })
}

// nestedDataPrefix is the top-level folder a raw removable Udisk nests the
// ASIAIR's data under. Store keys carry it in exactly this case, because
// listShareRoots builds every listed path from it.
const nestedDataPrefix = "ASIAIR"

// resolveDataPrefix determines the base under which a share's configured capture
// roots live. On a fixed EMMC/TF share the roots sit at the share root; on a raw
// removable Udisk the ASIAIR nests all its data under a top-level ASIAIR/ folder,
// so the same roots live at ASIAIR/<root>. It probes each configured root at the
// share root and, only on a confirmed not-found, retries it under ASIAIR/; the
// first layout that resolves wins and its prefix ("" or "ASIAIR") is returned.
// Any error other than not-found is a hard failure so a genuine problem
// (permission, transient network) still surfaces rather than being masked as an
// empty listing. When no configured root exists in either layout the empty prefix
// is returned and the subsequent walk surfaces the missing root as it always has.
func (w *Worker) resolveDataPrefix(share device.Share, roots []string) (string, error) {
	for _, root := range roots {
		_, err := share.ListDir(root)
		if err == nil {
			return "", nil
		}
		if !device.IsNotFound(err) {
			return "", err
		}
		nested := path.Join(nestedDataPrefix, root)
		_, err = share.ListDir(nested)
		if err == nil {
			return nestedDataPrefix, nil
		}
		if !device.IsNotFound(err) {
			return "", err
		}
	}
	return "", nil
}

// listShareRoots lists every configured root of a share recursively, walking each
// under the resolved data prefix so both the flat (EMMC) and ASIAIR/-nested
// (Udisk) layouts are handled transparently.
func (w *Worker) listShareRoots(share device.Share, prefix string, roots []string) ([]walkEntry, error) {
	var all []walkEntry
	for _, root := range roots {
		if err := w.walk(share, path.Join(prefix, root), &all); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// listingSignature is a stable fingerprint of a listing (share|path|size|mtime),
// used to decide whether a rig has been idle a full cycle (design architecture
// §8 "safe to power off").
func listingSignature(frames []listedFrame) string {
	parts := make([]string, len(frames))
	for i, f := range frames {
		parts[i] = fmt.Sprintf("%s|%s|%d|%d", f.share, f.path, f.size, f.mtime.Unix())
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// withoutKeys returns frames minus those whose listingKey is in keys. Matching is
// exact, never by prefix, so removing "a.fit" never removes "a.fits".
func withoutKeys(frames []listedFrame, keys map[string]bool) []listedFrame {
	out := make([]listedFrame, 0, len(frames))
	for _, f := range frames {
		if !keys[listingKey(f.share, f.path)] {
			out = append(out, f)
		}
	}
	return out
}

// listingKey is share + "\x00" + path, the exact identity of one listing line.
// A frame row's (SourceShare, SourcePath) is copied verbatim from the listedFrame
// it was filed from, so the key built from either names the same device file.
func listingKey(share, path string) string { return share + "\x00" + path }

// isNotExist reports a confirmed file-absence error from the local FS (distinct
// from a transient read error), used to separate "vanished" from "unreadable".
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// progressSampleInterval throttles the mid-transfer progress/throughput publish
// so a fast copy updates the dashboard about once a second rather than on every
// buffer.
const progressSampleInterval = time.Second

// errStagingWrite tags a copy error raised by the staging temp rather than the
// device: io.CopyBuffer reports either side's failure alike, and only the local
// side means the destination is refusing writes.
var errStagingWrite = errors.New("write to staging temp")

// stagingWriter wraps the staging temp so a failed write is distinguishable
// (errStagingWrite) from a failed read off the device.
type stagingWriter struct{ f *os.File }

func (s stagingWriter) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %w", errStagingWrite, err)
	}
	return n, err
}

// probeStaging clears a standing staging write fault once the staging area takes
// a real write again (create, write, fsync, remove), or re-records it when it
// still does not. Only the recovery is logged here; the standing fault is logged
// once per cycle by the caller. Recovery acks the rig's write-fault alert and
// clears the card's error at once: the cycle that follows may spend minutes
// copying the backlog before its end-of-cycle status would.
func (w *Worker) probeStaging(ctx context.Context, cfg *model.Config) {
	err := w.lfs.MkdirAll(cfg.Roots.Staging)
	if err == nil {
		err = w.lfs.WriteProbe(cfg.Roots.Staging)
	}
	if err != nil {
		w.log.Debug("rig: staging write probe failed", "staging", cfg.Roots.Staging, "error", err)
		w.destWriteErr = err
		return
	}
	w.log.Info("rig: staging area writable again; resuming pulls", "staging", cfg.Roots.Staging, "previous_error", w.destWriteErr)
	w.destWriteErr = nil
	w.st.LastError = ""
	w.resolveAlerts(ctx, model.AlertDestinationUnavailable)
	w.publish()
}

// progressWriter wraps the copy destination to sample transfer progress. It runs
// entirely on the worker goroutine that owns w.st and w.publish, so onSample
// needs no locking. Between samples it just forwards bytes; once
// progressSampleInterval has elapsed it computes the percent complete and the
// bytes/sec since the previous sample and invokes onSample.
type progressWriter struct {
	dst      io.Writer
	total    int64 // the source file size, for the percent
	now      func() time.Time
	onSample func(pct int, bytesPerSec int64)

	written    int64
	lastSample time.Time
	lastBytes  int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.dst.Write(p)
	pw.written += int64(n)
	t := pw.now()
	if pw.lastSample.IsZero() {
		pw.lastSample = t
		return n, err
	}
	elapsed := t.Sub(pw.lastSample)
	if elapsed >= progressSampleInterval {
		rate := int64(float64(pw.written-pw.lastBytes) / elapsed.Seconds())
		if pw.onSample != nil {
			pw.onSample(pw.pct(), rate)
		}
		pw.lastSample = t
		pw.lastBytes = pw.written
	}
	return n, err
}

// pct is the integer percent transferred, clamped to [0,100]; 0 when the size is
// unknown so a bad total never yields a nonsense bar.
func (pw *progressWriter) pct() int {
	if pw.total <= 0 {
		return 0
	}
	p := int(pw.written * 100 / pw.total)
	if p > 100 {
		p = 100
	}
	return p
}
