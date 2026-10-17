package rig

import (
	"context"
	"errors"
	"time"

	"github.com/macourteau/asiair-commander/internal/control"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

const (
	// clockOffsetThreshold is the device-clock divergence that is both worth
	// warning about and worth correcting. One constant serves both so the log and
	// the action agree on what "out of tolerance" means. The floor is set by the
	// protocol: pi_set_time carries whole seconds, so residual drift after a
	// correction is ~1s and any threshold below ~2s would write on every cycle.
	clockOffsetThreshold = 5 * time.Minute
	// clockSyncRetryAfter backs off after a failed attempt, so a control port that
	// is closed, firewalled, or hung is not retried — and logged — every cycle.
	// Without it a failing sync reproduces the very log wall this feature removes.
	clockSyncRetryAfter = 30 * time.Minute
	// controlDialTimeout bounds the control-channel dial, so a wedged device
	// cannot stall the worker goroutine; controlDialTimeoutOn adds the site's
	// margin.
	controlDialTimeout = 3 * time.Second
)

// controlDialTimeoutOn is controlDialTimeout on site: the connect is 1 round
// trip, so it adds one margin of the site's latency, and on the local site it is
// controlDialTimeout exactly. The calls on the channel are bounded by the
// control package's own call timeout.
func controlDialTimeoutOn(site netsite.Site) time.Duration {
	return controlDialTimeout + netsite.RoundTrips(site, 1)
}

// errForeignDevice is the refusal when the control channel's own pi_get_info
// reports an identity that is not this rig's bound device.
var errForeignDevice = errors.New("control channel reported a different device")

// errNoDeviceZone is the refusal when a device reports no time zone at all.
var errNoDeviceZone = errors.New("device reported no time zone")

// recordClockOffset stores this cycle's measured device-minus-host offset and
// reports the band as a transition rather than a level: the previous behaviour
// logged one warning per rig per poll interval for as long as the condition
// stood, which buried every other line in the log and could not be cleared.
//
// It is called only where an offset was actually measured, so a cycle that never
// opened a session cannot be mistaken for a return to tolerance and log a false
// all-clear; a rig that goes offline while out of band comes back still out of
// band, silently, which is the truth.
func (w *Worker) recordClockOffset(offset time.Duration, syncEnabled bool) {
	w.cycleOffset = offset
	out := offset > clockOffsetThreshold || offset < -clockOffsetThreshold
	switch {
	case out && !w.offsetOutOfBand:
		w.log.Warn("rig: device clock offset out of tolerance",
			"offset", offset.String(), "threshold", clockOffsetThreshold.String(), "sync_clock", syncEnabled)
	case !out && w.offsetOutOfBand:
		w.log.Info("rig: device clock offset back within tolerance", "offset", offset.String())
	}
	w.offsetOutOfBand = out
}

// noteSyncClockSetting clears the clock latches when the operator flips the
// toggle. Without this the feature's most common first experience produces no log
// at all: a rig sits out of band having already logged its one warning, the
// operator enables sync, and the band latch is still set so nothing is said.
// Flipping the toggle also drops any standing cooldown — an operator who just
// acted should not wait out a backoff earned before they did.
func (w *Worker) noteSyncClockSetting(enabled bool) {
	if enabled == w.syncClockWas {
		return
	}
	w.syncClockWas = enabled
	w.offsetOutOfBand = false
	w.clockRetryAt = time.Time{}
	w.clockFailLogged = false
}

// clockPhase corrects the device's clock when the operator has opted in and the
// device is both far enough out and demonstrably idle (design
// features §6).
//
// An ASIAIR has no battery-backed clock and no NTP client: it boots at its
// firmware's build date and keeps it until the phone app connects and calls
// pi_set_time. A rig left headless therefore timestamps its frames years wrong.
//
// The phase fails closed. Exactly one path reaches the write — enabled, bound,
// past the threshold, identity confirmed on the control channel itself, every
// subsystem idle, zone resolvable, and the re-measured offset still past the
// threshold. Any error, refusal, or sign of activity leaves the device untouched.
// It returns nothing, so no caller can act on its outcome: a clock is the least
// important thing this worker does, and must never affect a pull or a delete.
func (w *Worker) clockPhase(ctx context.Context, cy cycle) {
	profile := cy.profile
	w.noteSyncClockSetting(profile.SyncClock)
	if !profile.SyncClock {
		return
	}
	// Before the cooldown check: an unbound rig can never succeed, so letting it
	// reach a path that only logs would mean one line per cycle forever. The
	// config validator surfaces this case to the operator as a warning.
	if profile.GUID == "" {
		return
	}
	if w.cycleOffset <= clockOffsetThreshold && w.cycleOffset >= -clockOffsetThreshold {
		return
	}
	if w.now().Before(w.clockRetryAt) {
		w.log.Debug("rig: clock sync in cooldown", "retry_at", w.clockRetryAt)
		return
	}

	c, err := w.dialControl(ctx, cy.ep, controlDialTimeoutOn(cy.site))
	if err != nil {
		w.clockSyncFailed("dial", err)
		return
	}
	defer c.Close()

	info, err := c.Info(ctx)
	if err != nil {
		w.clockSyncFailed("pi_get_info", err)
		return
	}
	if !controlIdentityMatches(profile, info) {
		w.clockSyncFailed("identity", errForeignDevice)
		return
	}

	// A response that arrives but does not parse counts as busy; a response that
	// never arrives is a transport failure. Conflating them would retry a hung
	// port every cycle with no backoff.
	state, err := c.AppState(ctx)
	if err != nil {
		w.clockSyncFailed("get_app_state", err)
		return
	}
	if state.Busy {
		w.log.Debug("rig: device busy; leaving its clock alone", "reason", state.Reason)
		return
	}

	reported, err := c.GetTime(ctx)
	if err != nil {
		w.clockSyncFailed("pi_get_time", err)
		return
	}
	// An empty zone is checked before LoadLocation, which answers "" with UTC and
	// a nil error — a device that named no zone would otherwise have its wall
	// clock silently rewritten into UTC. The control package rejects this too;
	// the guard is repeated here so the fail-closed property is local to the
	// only code that writes a clock.
	if reported.TimeZone == "" {
		w.clockSyncFailed("timezone", errNoDeviceZone)
		return
	}
	loc, err := time.LoadLocation(reported.TimeZone)
	if err != nil {
		// Guessing a zone would silently move the rig's wall clock by hours.
		w.clockSyncFailed("timezone", err)
		return
	}

	// offset = device − host, the convention internal/clock uses throughout.
	offset := controlInstant(reported, loc).Sub(w.now())
	if offset <= clockOffsetThreshold && offset >= -clockOffsetThreshold {
		w.log.Debug("rig: device clock recovered before the correction; leaving it alone",
			"offset", offset.String())
		return
	}

	before := controlInstant(reported, loc)
	if err := c.SetTime(ctx, controlTimeIn(w.now(), loc, reported.TimeZone)); err != nil {
		w.clockSyncFailed("pi_set_time", err)
		return
	}
	// The correction has landed. Clear the failure latches regardless of what the
	// confirming read does: treating a failed confirm as a sync failure would start
	// a cooldown against a device that is now right.
	w.clockRetryAt = time.Time{}
	w.clockFailLogged = false

	confirmed, err := c.GetTime(ctx)
	if err != nil {
		w.log.Info("rig: set device clock, could not confirm",
			"before", before, "zone", reported.TimeZone, "error", err)
		return
	}
	after := controlInstant(confirmed, loc)
	w.log.Info("rig: set device clock",
		"before", before, "after", after, "zone", reported.TimeZone,
		"residual", after.Sub(w.now()).String())
}

// clockSyncFailed starts the backoff and logs at most one warning per failing
// run: the first failure after a success, or after the toggle changed. Later
// suppressed attempts log at debug, so a rig whose control port is closed costs
// one visible line rather than one per poll interval.
func (w *Worker) clockSyncFailed(step string, err error) {
	w.clockRetryAt = w.now().Add(clockSyncRetryAfter)
	if w.clockFailLogged {
		w.log.Debug("rig: clock sync failed again", "step", step, "error", err)
		return
	}
	w.clockFailLogged = true
	w.log.Warn("rig: clock sync failed; backing off",
		"step", step, "error", err, "retry_after", clockSyncRetryAfter.String())
}

// controlIdentityMatches applies the pull path's identity rule to the control
// channel's own pi_get_info, so the device is corroborated on the very connection
// that will carry the write rather than on a separate probe taken earlier in the
// cycle. cpuId stays a bonus fallback: the CM4 device class reports an empty one,
// so both-empty falls through to the guid compare.
func controlIdentityMatches(profile *model.RigProfile, info control.Info) bool {
	if profile.GUID == "" {
		return false // never write to a device this rig cannot name
	}
	return info.GUID == profile.GUID ||
		(profile.CPUID != "" && info.CPUID != "" && info.CPUID == profile.CPUID)
}

// controlInstant is the absolute instant a pi_get_time reading denotes, read in
// the zone the device reported.
func controlInstant(t control.Time, loc *time.Location) time.Time {
	return time.Date(t.Year, time.Month(t.Mon), t.Day, t.Hour, t.Min, t.Sec, 0, loc)
}

// controlTimeIn expresses an instant as a pi_set_time payload in the device's own
// zone, so syncing a rig configured for a remote observing site never drags it
// into whatever zone the Commander host runs in.
func controlTimeIn(at time.Time, loc *time.Location, zone string) control.Time {
	l := at.In(loc)
	return control.Time{
		Year: l.Year(), Mon: int(l.Month()), Day: l.Day(),
		Hour: l.Hour(), Min: l.Minute(), Sec: l.Second(),
		TimeZone: zone,
	}
}
