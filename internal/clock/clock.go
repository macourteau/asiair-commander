package clock

import "time"

// processStart anchors the real monotonic source. time.Since reads the
// monotonic clock embedded in the base value, so readings are immune to wall
// steps.
var processStart = time.Now()

// ServerTime mirrors smb1client's ServerTime structurally — the device's
// clock as reported at session negotiate, and the local receive time. It is
// a mirror rather than an import so internal/clock stays free of the device
// transport: this package has no dependency beyond stdlib time, and every
// other seam is free to depend on it without pulling in smb1client.
type ServerTime struct {
	// Time is the device's clock at the moment it built the response.
	Time time.Time
	// ReceivedAt is the host time at which the response was received.
	ReceivedAt time.Time
}

// Instant is a reading of the host's monotonic clock. Settle windows and
// grace timing subtract Instants; wall steps never move them.
type Instant struct {
	d time.Duration
}

// Sub returns the elapsed monotonic time between the two readings.
func (i Instant) Sub(o Instant) time.Duration {
	return i.d - o.d
}

// Clock is the single per-session bridge between a device's clock domain and
// the host's. It owns the measured offset and the monotonic source; the
// minimum-age check goes through it, and device-to-device comparisons never
// do. The time sources are injectable so every timestamp rule tests against
// fake clocks.
type Clock struct {
	offset time.Duration        // device clock minus host clock
	now    func() time.Time     // host wall clock
	mono   func() time.Duration // monotonic reading
}

// ForSession measures the offset from a session's negotiate response:
// offset = st.Time.Sub(st.ReceivedAt). Neither clock needs to be absolutely
// correct — an error in the host's own wall clock cancels out of the
// corrected-mtime comparison exactly as the device's does.
func ForSession(st ServerTime) *Clock {
	return &Clock{
		offset: st.Time.Sub(st.ReceivedAt),
		now:    time.Now,
		mono:   func() time.Duration { return time.Since(processStart) },
	}
}

// Local returns the zero-offset Clock for single-clock-domain sources: files
// read from the host's own truly-local volumes, so the offset is zero by
// construction.
func Local() *Clock {
	return ForSession(ServerTime{})
}

// NewWithSources builds a Clock over injected time sources — the test seam.
// offset is the device clock minus the host clock; now is the host wall
// clock; mono is the monotonic reading.
func NewWithSources(offset time.Duration, now func() time.Time, mono func() time.Duration) *Clock {
	return &Clock{offset: offset, now: now, mono: mono}
}

// Offset returns the measured device-minus-host offset, logged at session
// setup and warned on beyond the sanity threshold by the caller.
func (c *Clock) Offset() time.Duration {
	return c.offset
}

// YoungerThan reports whether the device timestamp's offset-corrected age is
// under age — the min_file_age quiescence check. The offset is applied
// internally; callers never see it.
func (c *Clock) YoungerThan(t DeviceTime, age time.Duration) bool {
	corrected := t.t.Add(-c.offset) // the device timestamp in the host's domain
	return c.now().Sub(corrected) < age
}

// Monotonic returns the current monotonic reading for settle windows and
// grace timing.
func (c *Clock) Monotonic() Instant {
	return Instant{d: c.mono()}
}
