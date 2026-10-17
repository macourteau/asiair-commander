// Package clock implements the timestamp-domain contract: timestamps live in
// distinct types by domain, and cross-domain comparison does not compile.
// DeviceTime carries raw device-domain timestamps (SMB directory listings and
// stats, and the state DB, which stores them unchanged); the per-session
// Clock is the only bridge between a device's clock and the host's, owning
// the measured offset and the monotonic source.
package clock

import "time"

// DeviceTime is a raw device-domain timestamp: stamped by the device's
// clock, never corrected, and comparable only against other timestamps from
// the same device. Device-to-device comparisons (decide-to-pull, the
// pre-delete stat) are methods here, tolerance included, and never see a
// clock offset; anything comparing device time against "now" goes through
// the Clock.
type DeviceTime struct {
	t time.Time
}

// FromOrigin wraps a timestamp exactly as the device protocol delivered it —
// smb1client's interpretation-free FILETIME conversion, or a local stat on
// the device's own volume. The value stays in the device's clock domain.
func FromOrigin(t time.Time) DeviceTime {
	return DeviceTime{t: t}
}

// FromHostClock is the contract's one sanctioned escape hatch: a timestamp
// the host itself stamped (the persisted `pulled_at` wall-clock reading used
// by grace timing, design architecture §12) is already in the host's own
// domain, so it enters DeviceTime through this named constructor rather than
// FromOrigin. It stays self-consistent for its own comparisons, which never mix
// with device-share timestamps, because domains never cross this boundary.
func FromHostClock(t time.Time) DeviceTime {
	return DeviceTime{t: t}
}

// ParseRFC3339 restores a DeviceTime from its state-DB representation.
func ParseRFC3339(s string) (DeviceTime, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return DeviceTime{}, err
	}
	return DeviceTime{t: t}, nil
}

// RFC3339 renders the timestamp for the state DB: RFC 3339 at full device
// precision, the raw device-domain value unchanged.
func (t DeviceTime) RFC3339() string {
	return t.t.Format(time.RFC3339Nano)
}

// IsZero reports whether the timestamp is the zero value.
func (t DeviceTime) IsZero() bool {
	return t.t.IsZero()
}

// EqualWithin reports whether the two device timestamps match within the
// tolerance — the decide-to-pull and pre-delete-stat comparison. The 2-second
// tolerance callers pass absorbs device timestamp precision limits
// (FAT-formatted media store mtimes with 2-second granularity).
func (t DeviceTime) EqualWithin(o DeviceTime, tol time.Duration) bool {
	d := t.t.Sub(o.t)
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// NewerThan reports whether t is newer than o beyond the tolerance — the
// changed-at-device half of decide-to-pull.
func (t DeviceTime) NewerThan(o DeviceTime, tol time.Duration) bool {
	return t.t.Sub(o.t) > tol
}
