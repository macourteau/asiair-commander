package rig

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
)

// recentFramesForPrediction bounds how many of a rig's most-recently-filed frames
// the predictor scans. RecentFrames is id-ordered (filed order), so this is set
// well above predictSampleSize to leave room for interleaved non-lights and the
// duplicate H12 rows (two rows per capture) the predictor filters out.
const recentFramesForPrediction = 50

// publish pushes this worker's current RigStatus to the hub (a value copy; no
// mutable state escapes). Only the worker goroutine writes w.st.
func (w *Worker) publish() { w.hub.PublishRig(w.st) }

// setState updates the coarse activity state without publishing.
func (w *Worker) setState(s model.RigState) { w.st.State = s }

// setSafeVerdict is the only writer of SafeToPowerOff + PowerOffReason, so the two
// never diverge and neither goes stale across cycles (w.st is mutated in place, so
// a value left from a prior cycle would otherwise persist). reason is "" unless
// safe == model.SafeUnknown. It clears the verdict's listing time, which only
// publishListingStatus sets again: any other verdict came from no listing.
func (w *Worker) setSafeVerdict(safe model.SafeToPowerOff, reason string) {
	w.st.SafeToPowerOff = safe
	w.st.PowerOffReason = reason
	w.st.SafeAsOf = 0
}

// setError marks the rig in error and publishes.
func (w *Worker) setError(msg string) {
	w.st.State = model.RigStateError
	w.st.LastError = msg
	w.publish()
}

// publishListingStatus computes and publishes the per-cycle status after a
// listing (design architecture §8/§9): per-share free space, drain,
// frames-tonight, and the conservatively-computed safe-to-power-off. It also
// raises the NAS-reserve, device-disk-low, and capacity-backlog alerts.
//
// "Safe" may read Yes only when the rig is online, EVERY watched share was freshly
// and successfully listed (no failed/stale listing, no absent removable medium),
// there is zero pending work (nothing new, nothing within min_file_age/not-yet-
// settled, nothing mid-transfer, every listed frame verified on the NAS), AND the
// rig was idle a full cycle (listing unchanged since the prior one).
func (w *Worker) publishListingStatus(ctx context.Context, cfg *model.Config, profile *model.RigProfile, ms *mountedSession, shareOrder []string, listingOK bool, absentShares, emptySlots map[string]bool, pendingWork, readyToCopy int, idle bool, listingTS int64, offset time.Duration, missingWithSource bool) {
	shares := make([]model.ShareFree, 0, len(shareOrder))
	tightest := int64(-1)
	for _, name := range shareOrder {
		if absentShares[name] {
			// No medium in the slot. The ASIAIR still exports the share, and its
			// statfs then reports the ASIAIR's OWN root filesystem — a small, nearly
			// full partition unrelated to any stick. Counting that as a share's free
			// space reports a phantom figure on the dashboard and drags the tightest
			// share under the reserve, raising a device-disk-low alert for a slot that
			// simply has nothing in it.
			continue
		}
		share, err := ms.share(name)
		if err != nil {
			continue
		}
		free, _, err := share.Statfs()
		if err != nil {
			w.log.Debug("rig: statfs failed", "share", name, "error", err)
			continue
		}
		shares = append(shares, model.ShareFree{Share: name, Free: free})
		if tightest < 0 || free < tightest {
			tightest = free
		}
	}
	w.st.Shares = shares

	// Publish the structurally-confirmed-empty slot set (share-order for
	// determinism). Rebuilt every listing so an offline/partial cycle — which reset
	// it to nil at the top of pullPhase and never reaches here — publishes empty, and
	// a share that regains its medium drops out. This gates the disable guard; nil
	// when nothing is empty.
	var empty []string
	for _, name := range shareOrder {
		if emptySlots[name] {
			empty = append(empty, name)
		}
	}
	w.st.EmptySlots = empty

	w.devicePending = pendingWork
	if resident, err := w.store.ResidentCount(ctx, w.profileID); err == nil {
		w.st.Drain = drainLabel(resident, w.devicePending, profile.KeepOriginals)
	} else {
		w.log.Warn("rig: resident-count query failed", "error", err)
	}

	since := localNoon(w.now(), w.siteLoc).Unix()
	if n, err := w.store.CountFramesTonight(ctx, w.profileID, since); err == nil {
		w.st.FramesTonight = n
	}
	w.st.Prediction = w.framePrediction(ctx, since, offset)

	safe := model.SafeUnknown
	// A pause delivered mid-listing breaks the loop after a partial tally, so this
	// cycle's pendingWork counts only the frames it got to — enough to read zero and
	// publish "safe to power off" over a card still holding frames. A suspended
	// rig's verdict is Unknown whatever the tally says; a paused rig running until
	// idle lists in full, so its verdict is computed like an unpaused rig's.
	suspended := w.workSuspended()
	if w.st.Online && listingOK && len(absentShares) == 0 && !suspended {
		if pendingWork == 0 && idle {
			safe = model.SafeYes
		} else {
			safe = model.SafeWorking
		}
	}
	// A frame whose device source is still listed but is missing from the NAS
	// library means the card holds the only copy — never report "safe to power
	// off", and say why. Moves confidence down, never up.
	if missingWithSource {
		if safe == model.SafeYes {
			safe = model.SafeUnknown
		}
		w.st.MissingWarning = "frames are missing from the library; the device holds the only copy"
	} else {
		w.st.MissingWarning = ""
	}
	// Explain an Unknown verdict so the badge can be truthful. missing_on_device
	// ranks above removable_absent (a frame known to exist only on the device is a
	// concrete data risk, an absent slot is merely un-checkable); paused ranks first
	// because a pause can itself truncate the listing, so "clears next check" would
	// be false. offline is defensive — an offline rig usually early-returns.
	reason := ""
	if safe == model.SafeUnknown {
		switch {
		case suspended:
			reason = "paused"
		case !w.st.Online:
			reason = "offline"
		case missingWithSource:
			reason = "missing_on_device"
		case len(absentShares) != 0:
			reason = "removable_absent"
		default:
			reason = "listing_failed"
		}
	}
	w.setSafeVerdict(safe, reason)
	w.recordLastKnown(safe, reason, listingTS, pendingWork)
	if !suspended {
		// A suspended verdict comes from the pause, whatever the tally says, so it
		// dates from no listing, as the same verdict from setPaused does not.
		w.st.SafeAsOf = listingTS
	}
	if listingOK {
		w.st.ListingTS = listingTS
	}

	switch {
	case suspended:
		w.st.State = model.RigStatePaused
		// A pause delivered mid-cycle (via drainCommands) breaks the listing loop
		// after a partial tally; a paused rig observes nothing, so show no count.
		w.st.WaitingToSettle = 0
		// The end-of-listing snapshot set PendingListed=true just before this call, but
		// a pause means the listing broke early — the pending set is partial, not
		// authoritative, so the client must not reconcile/d1-remove against it.
		w.st.PendingListed = false
	case readyToCopy > 0:
		// Transferring only for real work: frames that are only settling, held or
		// failed leave the listing at Monitoring, and the waiting-to-settle line and
		// the alerts carry them.
		w.st.State = model.RigStatePulling
	default:
		w.st.State = model.RigStateIdle
	}
	// A delete slice that follows this listing restores this state when it ends.
	// A suspended listing's Paused is not recorded: a Resume or a run's start puts
	// the listed state back on a rig that is no longer paused, so it must be what
	// the last unsuspended listing read.
	if w.st.State == model.RigStateIdle || w.st.State == model.RigStatePulling {
		w.listedState = w.st.State
	}
	w.st.Current = nil
	w.st.LastError = ""

	// Both destination conditions below are FLEET-WIDE: the NAS is one shared
	// volume, so they are raised with no rig id (one row, not one per rig) and
	// auto-acked through the matching fleet-scoped helper when they clear.
	if free, _, err := w.lfs.FreeSpace(cfg.Roots.Light); err == nil {
		w.hub.SetNASFree(free)
		if free < cfg.DiskReserveBytes {
			// A stable message (no fluctuating byte count) so the alert dedups to one
			// unacked row per condition instead of flooding a new row every cycle;
			// the live free-space figure is on the dashboard.
			w.notify.Raise(ctx, model.Alert{
				Severity: model.SeverityWarn,
				Category: model.AlertNASDiskLow,
				Message:  "NAS destination free space is below the reserve; pulls will pause",
				Standing: true,
			})
		} else {
			// Space freed up; a standing low-disk warning is stale.
			w.resolveFleetAlerts(ctx, model.AlertNASDiskLow)
		}
		// The destination is readable again; clear any standing unreadable alert.
		w.resolveFleetAlerts(ctx, model.AlertDestinationUnavailable)
	} else {
		// Unreadable is not the same condition as full: it needs its own alert, or a
		// rig stalled behind an unmounted destination stalls silently.
		w.log.Warn("rig: NAS free-space query failed", "error", err)
		w.notify.Raise(ctx, model.Alert{
			Severity: model.SeverityWarn,
			Category: model.AlertDestinationUnavailable,
			Message:  "the NAS destination can't be read; pulls are paused",
			Standing: true,
		})
	}
	// A staging write fault is this rig's own observation — the volume answered the
	// free-space query above but refused a write or flush — so it alerts rig-scoped.
	// Raised fleet-wide, every other rig whose free-space query succeeds would ack
	// it each cycle and the alert would flap. It clears once a write probe passes.
	if w.destWriteErr != nil {
		w.st.LastError = "can't write to the NAS destination: " + w.destWriteErr.Error()
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertDestinationUnavailable,
			Message:  "the NAS destination can't be written; pulls are paused until it accepts writes again",
			Standing: true,
		})
	} else {
		w.resolveAlerts(ctx, model.AlertDestinationUnavailable)
	}

	w.checkBacklogAndCapacity(ctx, cfg, profile, tightest)
	w.publish()
}

// checkBacklogAndCapacity raises the never-deletable-backlog alert, escalated to
// a "capture will stop" critical on a capacity-constrained rig whose tightest
// share is low, plus a device-disk-low alert (design architecture §10). The
// tightest share is what matters so a roomy USB never masks a filling eMMC after
// an H12 failover.
func (w *Worker) checkBacklogAndCapacity(ctx context.Context, cfg *model.Config, profile *model.RigProfile, tightestFree int64) {
	neverDeletable := w.countNeverDeletable(ctx)

	deviceLow := tightestFree >= 0 && tightestFree < cfg.DiskReserveBytes

	// Unknown free space (-1: no share could be statted) counts as tight: a device we
	// cannot measure must not be scheduled as if it had room.
	tight := tightestFree < 0 || tightestFree < deletePressureFactor*cfg.DiskReserveBytes
	if tight != w.deviceTight {
		// Logged on change only, so a rig that is always tight is one line, not silence.
		if tight {
			reason := "tight"
			if tightestFree < 0 {
				reason = "unknown"
			}
			w.log.Info("rig: device delete pressure changed", "tight", tight, "tightest_free", tightestFree, "reason", reason)
		} else {
			w.log.Info("rig: device delete pressure changed", "tight", tight, "tightest_free", tightestFree)
		}
	}
	w.deviceTight = tight

	if neverDeletable > 0 {
		sev := model.SeverityWarn
		// No frame count in the message: it fluctuates each cycle and would defeat
		// the (rig, category, message) dedup, flooding a new alert per poll. The
		// Alerts page links the alert to exactly these frames (store
		// ListNeverDeletable), where the count is.
		msg := fmt.Sprintf("Some frames on %q can't be removed from the device automatically because they are "+
			"unclassified, held or failed; they stay on it, using its space, until each one is fixed or dismissed.", profile.Name)
		if profile.CapacityConstrained && deviceLow {
			sev = model.SeverityCritical
			msg = fmt.Sprintf("Capture may stop on %q: its device is low on free space, and some frames on it can't be "+
				"removed automatically because they are unclassified, held or failed.", profile.Name)
		}
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: sev,
			Category: model.AlertBacklog,
			Message:  msg,
			Standing: true,
		})
		w.backlogRaised = true
	} else if w.backlogRaised {
		// The never-deletable set just emptied: ack the aggregate backlog alert once,
		// on this resolving cycle only, so a healthy rig never scans alerts per cycle.
		w.resolveAlerts(ctx, model.AlertBacklog)
		w.backlogRaised = false
	}

	if deviceLow {
		w.notify.Raise(ctx, model.Alert{
			RigID:    w.profileID,
			Severity: model.SeverityWarn,
			Category: model.AlertDeviceDiskLow,
			// Stable message (no byte count) so the alert dedups per condition; the
			// live figure is on the dashboard.
			Message:  fmt.Sprintf("device free space is low on rig %q (tightest share below reserve)", profile.Name),
			Standing: true,
		})
		w.deviceLowRaised = true
	} else if w.deviceLowRaised {
		// Space freed up (or the share that looked tight was an empty slot): ack on
		// the resolving cycle only, so a healthy rig never scans alerts every cycle.
		// Without this the warning stands forever once raised — there is no other
		// path that retires it.
		w.resolveAlerts(ctx, model.AlertDeviceDiskLow)
		w.deviceLowRaised = false
	}
}

// countNeverDeletable counts the rig's frames that are never removed from the
// device — the driver of the backlog alert. The predicate lives in the store
// (ListNeverDeletable: held, failed, and unclassified not culled, design
// library-reconciliation §11.5, I36) so the alert's condition and the frames its
// link lists are one query.
func (w *Worker) countNeverDeletable(ctx context.Context) int {
	_, n, err := w.store.ListNeverDeletable(ctx, w.profileID, 0, 1)
	if err != nil {
		w.log.Warn("rig: never-deletable count query failed", "error", err)
		return 0
	}
	return n
}

// maybeAckBacklog acks the aggregate backlog alert when the never-deletable set is
// empty, called from discardOne after a tombstone write to give the operator an
// immediate ack. It deliberately does NOT gate on backlogRaised: a rig dismissed
// while paused/offline has never run a cycle to set the flag, so gating there would
// leave the alert stuck — the exact treadmill this feature removes. Dismiss is a
// rare operator action, so an unconditional count+scan here is free.
func (w *Worker) maybeAckBacklog(ctx context.Context) {
	if w.countNeverDeletable(ctx) == 0 {
		w.resolveAlerts(ctx, model.AlertBacklog)
		w.backlogRaised = false
	}
}

// recomputeCounters recomputes w.st.Drain + w.st.FramesTonight and publishes, so a
// dismiss drops the "N kept" / "transferred tonight" numbers immediately rather
// than at the next cycle. It runs the same queries refreshLiveCounters uses, minus
// its throttle and the prediction refresh (unaffected by a dismiss).
func (w *Worker) recomputeCounters(ctx context.Context) {
	profile := w.cfg.Load().Rig(w.profileID)
	if profile == nil {
		w.publish()
		return
	}
	since := localNoon(w.now(), w.siteLoc).Unix()
	if n, err := w.store.CountFramesTonight(ctx, w.profileID, since); err == nil {
		w.st.FramesTonight = n
	}
	if resident, err := w.store.ResidentCount(ctx, w.profileID); err == nil {
		w.st.Drain = drainLabel(resident, w.devicePending, profile.KeepOriginals)
	}
	w.publish()
}

// drainLabel renders the device-drain status line from the resident-frame count.
// A keep-originals rig never runs the delete pass, so it never frees device space
// and is never "drained": its originals stay resident by design and read as
// "kept". Otherwise the count is the grace-period delete backlog ("drained" at
// zero). An empty backlog is not a drained device while frames not yet copied
// (pending) still sit on it — nothing has reached the backlog yet — so that case
// renders no line at all rather than a false "drained". Shared by the cycle-end
// status and the live mid-cycle refresh so both render identically.
func drainLabel(resident, pending int, keepOriginals bool) string {
	switch {
	case keepOriginals:
		return fmt.Sprintf("%d kept", resident)
	case resident == 0 && pending > 0:
		return ""
	case resident == 0:
		return "drained"
	default:
		return fmt.Sprintf("%d resident", resident)
	}
}

// refreshLiveCounters re-queries the transferred-tonight and resident-frame counts
// mid-cycle and publishes them, throttled, so the dashboard's "transferred
// tonight" stat and the device-drain line ("N kept" / "N resident") climb
// alongside the live transfer progress instead of jumping only at the cycle
// boundary. It runs the SAME queries publishListingStatus uses, so the live values
// always match what a page refresh would compute — no drift. Called from the worker
// goroutine.
func (w *Worker) refreshLiveCounters(ctx context.Context, profile *model.RigProfile, offset time.Duration) {
	now := w.now()
	if now.Sub(w.lastLiveCounterPub) < liveCounterRefresh {
		return
	}
	w.lastLiveCounterPub = now
	since := localNoon(now, w.siteLoc).Unix()
	if n, err := w.store.CountFramesTonight(ctx, w.profileID, since); err == nil {
		w.st.FramesTonight = n
	} else {
		w.log.Warn("rig: frames-tonight refresh query failed", "error", err)
	}
	if resident, err := w.store.ResidentCount(ctx, w.profileID); err == nil {
		w.st.Drain = drainLabel(resident, w.devicePending, profile.KeepOriginals)
	} else {
		w.log.Warn("rig: resident-count refresh query failed", "error", err)
	}
	w.st.Prediction = w.framePrediction(ctx, since, offset)
	w.publish()
}

// framePrediction sources tonight's most recent light captures and runs the pure
// predictor over them, returning nil (→ Unknown) when nothing usable is available.
// It filters the rig's RecentFrames in Go — lights only, captured since local noon,
// deduped by dest_path (an H12 frame is two rows, one capture, as CountFramesTonight
// dedupes) — ordered by capture time, keeping the last predictSampleSize. Capture
// time is DATE-OBS parsed with a device-mtime fallback; never the filed time.
// Filtering the existing query in Go (rather than adding a store query) keeps the
// DATE-OBS/mtime capture-time choice in one place instead of encoding it in SQL.
func (w *Worker) framePrediction(ctx context.Context, since int64, offset time.Duration) *model.FramePrediction {
	frames, err := w.store.RecentFrames(ctx, w.profileID, recentFramesForPrediction)
	if err != nil {
		w.log.Warn("rig: recent-frames query for prediction failed", "error", err)
		return nil
	}
	type capture struct {
		t   time.Time
		exp float64
	}
	seen := make(map[string]bool)
	var caps []capture
	for _, f := range frames {
		if f.FrameType != string(model.TypeLight) || f.DestPath == "" || seen[f.DestPath] {
			continue
		}
		seen[f.DestPath] = true
		ct := frameCaptureTime(f)
		if ct.Unix() < since {
			continue
		}
		caps = append(caps, capture{t: ct, exp: f.Exptime})
	}
	if len(caps) == 0 {
		return nil
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].t.Before(caps[j].t) })
	if len(caps) > predictSampleSize {
		caps = caps[len(caps)-predictSampleSize:]
	}
	times := make([]time.Time, len(caps))
	for i, c := range caps {
		times[i] = c.t
	}
	return predictFrames(times, caps[len(caps)-1].exp, w.now(), offset)
}

// localNoon returns the opening local noon of the current observing night — the
// since-boundary for frames-tonight.
func localNoon(now time.Time, loc *time.Location) time.Time {
	return route.ObservingDate(now, loc).Add(12 * time.Hour)
}
