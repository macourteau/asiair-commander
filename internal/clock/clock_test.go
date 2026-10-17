package clock

import (
	"testing"
	"time"
)

// fake builds a Clock over a controllable wall clock and monotonic source.
type fake struct {
	offset time.Duration
	now    time.Time
	mono   time.Duration
}

func (f *fake) clock() *Clock {
	return NewWithSources(f.offset, func() time.Time { return f.now }, func() time.Duration { return f.mono })
}

func TestForSessionMeasuresOffset(t *testing.T) {
	received := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		device time.Time
		want   time.Duration
	}{
		{"device ahead", received.Add(90 * time.Second), 90 * time.Second},
		{"device behind", received.Add(-3 * time.Minute), -3 * time.Minute},
		{"in sync", received, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ForSession(ServerTime{Time: tt.device, ReceivedAt: received})
			if got := c.Offset(); got != tt.want {
				t.Errorf("Offset() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLocalHasZeroOffset(t *testing.T) {
	if got := Local().Offset(); got != 0 {
		t.Errorf("Local().Offset() = %v, want 0", got)
	}
}

// TestYoungerThanUnderSkew is the min_file_age rule under skewed clocks: the
// corrected-mtime comparison must be robust to either clock being wrong,
// because the offset is a measured difference, not a correction toward truth.
func TestYoungerThanUnderSkew(t *testing.T) {
	hostNow := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	const age = 120 * time.Second

	tests := []struct {
		name string
		// device clock runs ahead of host by skew
		skew time.Duration
		// the file's true age in host terms (written trueAge before hostNow,
		// so its device-domain mtime is hostNow - trueAge + skew)
		trueAge time.Duration
		want    bool
	}{
		{"fresh file, synced clocks", 0, 30 * time.Second, true},
		{"old file, synced clocks", 0, 10 * time.Minute, false},
		{"fresh file, device 1h ahead", time.Hour, 30 * time.Second, true},
		{"old file, device 1h ahead", time.Hour, 10 * time.Minute, false},
		{"fresh file, device 1h behind", -time.Hour, 30 * time.Second, true},
		{"old file, device 1h behind", -time.Hour, 10 * time.Minute, false},
		{"exactly at the age boundary", 0, age, false}, // "within the last N seconds" is strict
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fake{offset: tt.skew, now: hostNow}
			mtime := FromOrigin(hostNow.Add(-tt.trueAge).Add(tt.skew))
			if got := f.clock().YoungerThan(mtime, age); got != tt.want {
				t.Errorf("YoungerThan(true age %v, skew %v) = %v, want %v", tt.trueAge, tt.skew, got, tt.want)
			}
		})
	}
}

// TestYoungerThanBothClocksWrong: an error in the host's own wall clock
// cancels out of the corrected comparison just as the device's does — the
// measurement requires neither clock to be absolutely correct.
func TestYoungerThanBothClocksWrong(t *testing.T) {
	// True time is 12:00. The host clock is 2h slow (reads 10:00); the device
	// clock is 30m fast (reads 12:30). The measured offset is device - host =
	// +2h30m. A file written 1 minute ago carries device mtime 12:29.
	hostNow := time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC)
	offset := 2*time.Hour + 30*time.Minute
	mtime := FromOrigin(time.Date(2026, 1, 10, 12, 29, 0, 0, time.UTC))

	f := &fake{offset: offset, now: hostNow}
	if !f.clock().YoungerThan(mtime, 120*time.Second) {
		t.Error("a file written 1 minute ago must read younger than 120s despite both clocks being wrong")
	}
	old := FromOrigin(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)) // written 30m ago
	if f.clock().YoungerThan(old, 120*time.Second) {
		t.Error("a file written 30 minutes ago must not read younger than 120s")
	}
}

func TestMonotonicSettleWindow(t *testing.T) {
	f := &fake{}
	c := f.clock()

	start := c.Monotonic()
	f.mono = 45 * time.Second
	later := c.Monotonic()

	if got := later.Sub(start); got != 45*time.Second {
		t.Errorf("Sub() = %v, want 45s", got)
	}

	// A wall-clock step must not move monotonic readings: the wall source is
	// not consulted at all.
	f.now = f.now.Add(-24 * time.Hour)
	f.mono = 60 * time.Second
	if got := c.Monotonic().Sub(start); got != 60*time.Second {
		t.Errorf("Sub() after wall step = %v, want 60s", got)
	}
}

func TestRealMonotonicAdvances(t *testing.T) {
	c := Local()
	a := c.Monotonic()
	b := c.Monotonic()
	if b.Sub(a) < 0 {
		t.Error("real monotonic source went backwards")
	}
}

func TestEqualWithinTolerance(t *testing.T) {
	base := FromOrigin(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC))
	tol := 2 * time.Second

	tests := []struct {
		name  string
		other time.Duration // other = base + this
		want  bool
	}{
		{"identical", 0, true},
		{"1s later", time.Second, true},
		{"exactly +2s", 2 * time.Second, true}, // FAT's 2s granularity is inclusive
		{"exactly -2s", -2 * time.Second, true},
		{"just beyond +2s", 2*time.Second + time.Nanosecond, false},
		{"just beyond -2s", -2*time.Second - time.Nanosecond, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := FromOrigin(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC).Add(tt.other))
			if got := base.EqualWithin(other, tol); got != tt.want {
				t.Errorf("EqualWithin(%v) = %v, want %v", tt.other, got, tt.want)
			}
			if got := other.EqualWithin(base, tol); got != tt.want {
				t.Errorf("EqualWithin is not symmetric for %v", tt.other)
			}
		})
	}
}

func TestNewerThanTolerance(t *testing.T) {
	base := FromOrigin(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC))
	tol := 2 * time.Second

	tests := []struct {
		name  string
		other time.Duration
		want  bool
	}{
		{"identical", 0, false},
		{"2s newer (within tolerance)", 2 * time.Second, false},
		{"just beyond tolerance", 2*time.Second + time.Nanosecond, true},
		{"much newer", time.Hour, true},
		{"older", -time.Hour, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := FromOrigin(time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC).Add(tt.other))
			if got := other.NewerThan(base, tol); got != tt.want {
				t.Errorf("NewerThan(%+v) = %v, want %v", tt.other, got, tt.want)
			}
		})
	}
}

func TestRFC3339RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		t    time.Time
	}{
		{"second precision", time.Date(2026, 1, 10, 12, 0, 3, 0, time.UTC)},
		{"full nanosecond precision", time.Date(2026, 1, 10, 12, 0, 3, 123456789, time.UTC)},
		{"FAT 2s bucket", time.Date(2019, 6, 2, 22, 14, 34, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := FromOrigin(tt.t)
			back, err := ParseRFC3339(dt.RFC3339())
			if err != nil {
				t.Fatalf("ParseRFC3339: %v", err)
			}
			if !back.EqualWithin(dt, 0) {
				t.Errorf("round-trip changed the value: %s vs %s", back.RFC3339(), dt.RFC3339())
			}
		})
	}

	if _, err := ParseRFC3339("not a timestamp"); err == nil {
		t.Error("ParseRFC3339 accepted garbage")
	}
}

func TestIsZero(t *testing.T) {
	if !(DeviceTime{}).IsZero() {
		t.Error("zero DeviceTime must report IsZero")
	}
	if FromOrigin(time.Now()).IsZero() {
		t.Error("non-zero DeviceTime must not report IsZero")
	}
}
