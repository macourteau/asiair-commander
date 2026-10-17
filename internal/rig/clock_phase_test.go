package rig

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/control"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// fakeControl is a scripted control.Client. It lives here rather than reusing the
// control package's fake so these tests depend only on the interface the design
// fixes, not on another package's test helper.
type fakeControl struct {
	info     control.Info
	state    control.AppState
	times    []control.Time // successive GetTime replies
	getCalls int

	errInfo, errState, errGet, errSet, errConfirm error

	sets   []control.Time
	closed bool
}

func (f *fakeControl) Info(context.Context) (control.Info, error) {
	return f.info, f.errInfo
}

func (f *fakeControl) AppState(context.Context) (control.AppState, error) {
	return f.state, f.errState
}

func (f *fakeControl) GetTime(context.Context) (control.Time, error) {
	n := f.getCalls
	f.getCalls++
	if n == 0 && f.errGet != nil {
		return control.Time{}, f.errGet
	}
	if n > 0 && f.errConfirm != nil {
		return control.Time{}, f.errConfirm
	}
	if n < len(f.times) {
		return f.times[n], nil
	}
	if len(f.times) == 0 {
		return control.Time{}, nil
	}
	return f.times[len(f.times)-1], nil
}

func (f *fakeControl) SetTime(_ context.Context, t control.Time) error {
	if f.errSet != nil {
		return f.errSet
	}
	f.sets = append(f.sets, t)
	return nil
}

func (f *fakeControl) Close() error { f.closed = true; return nil }

// clockHarness wires a Worker with a scripted control channel and a frozen clock.
type clockHarness struct {
	w       *Worker
	c       *fakeControl
	dials   int
	dialed  netsite.Endpoint
	dialErr error
	now     time.Time
	profile *model.RigProfile
}

const (
	testGUID  = "0a1b2c3d"
	testCPUID = "0011223344556677"
	testZone  = "America/Toronto"
)

// deviceTimeAt renders an instant as the device would report it in testZone.
func deviceTimeAt(t time.Time) control.Time {
	loc, err := time.LoadLocation(testZone)
	if err != nil {
		panic(err)
	}
	l := t.In(loc)
	return control.Time{
		Year: l.Year(), Mon: int(l.Month()), Day: l.Day(),
		Hour: l.Hour(), Min: l.Minute(), Sec: l.Second(),
		TimeZone: testZone,
	}
}

func newClockHarness(t *testing.T) *clockHarness {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2026-07-27T21:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	h := &clockHarness{
		now: now,
		c: &fakeControl{
			info:  control.Info{GUID: testGUID, CPUID: testCPUID},
			state: control.AppState{Busy: false},
			// Stuck at the AirPlus firmware build date, the real failure mode.
			times: []control.Time{deviceTimeAt(now.AddDate(-7, -5, -13))},
		},
		profile: &model.RigProfile{
			ID: "rig-01", GUID: testGUID, CPUID: testCPUID, SyncClock: true,
		},
	}
	cfg := &atomic.Pointer[model.Config]{}
	cfg.Store(&model.Config{Rigs: []model.RigProfile{*h.profile}})

	h.w = &Worker{
		profileID: "rig-01",
		cfg:       cfg,
		now:       func() time.Time { return h.now },
		log:       discardLogger(),
		// The registry the cycle's site is resolved in; its costs only lengthen
		// the dial timeout, which these tests do not read.
		sites: netsite.NewRegistry(nil),
		dialControl: func(_ context.Context, ep netsite.Endpoint, _ time.Duration) (control.Client, error) {
			h.dials++
			h.dialed = ep
			if h.dialErr != nil {
				return nil, h.dialErr
			}
			return h.c, nil
		},
	}
	// The offset a stuck-in-2019 device produces at SMB negotiate: device − host.
	h.w.cycleOffset = -7 * 365 * 24 * time.Hour
	return h
}

// clockEndpoint is the cycle's endpoint. It names a site other than the local
// one so the dial proves the endpoint reaches the control dialer whole.
var clockEndpoint = netsite.Endpoint{Site: "r1", IP: "192.168.1.75"}

func (h *clockHarness) run() {
	h.w.clockPhase(context.Background(), h.w.newCycle(h.profile, clockEndpoint))
}

// The happy path: a device years out, idle and correctly identified, is corrected
// exactly once, in its own reported zone.
func TestClockPhaseSetsStuckClock(t *testing.T) {
	h := newClockHarness(t)
	h.run()

	if len(h.c.sets) != 1 {
		t.Fatalf("want exactly one pi_set_time, got %d", len(h.c.sets))
	}
	got := h.c.sets[0]
	if got.TimeZone != testZone {
		t.Errorf("zone = %q, want the device's own %q", got.TimeZone, testZone)
	}
	want := deviceTimeAt(h.now)
	if got != want {
		t.Errorf("set %+v, want %+v", got, want)
	}
	if !h.c.closed {
		t.Error("control connection was not closed")
	}
	if h.dialed != clockEndpoint {
		t.Errorf("dialed %+v, want the cycle's endpoint %+v", h.dialed, clockEndpoint)
	}
}

// design features I2: the toggle off means the control channel is never even
// dialed.
func TestClockPhaseNeverDialsWhenDisabled(t *testing.T) {
	h := newClockHarness(t)
	h.profile.SyncClock = false
	h.run()

	if h.dials != 0 {
		t.Errorf("dialed %d times with sync_clock off, want 0", h.dials)
	}
}

// design features I1: a device answering with someone else's guid is refused, and
// refusal costs a cooldown so a mis-bound rig does not probe a stranger every
// cycle.
func TestClockPhaseRefusesForeignDevice(t *testing.T) {
	h := newClockHarness(t)
	h.c.info = control.Info{GUID: "deadbeef", CPUID: "ffeeddccbbaa9988"}
	h.run()

	if len(h.c.sets) != 0 {
		t.Fatalf("wrote to a foreign device: %+v", h.c.sets)
	}
	if h.w.clockRetryAt.IsZero() {
		t.Error("a foreign device did not start the cooldown")
	}
}

// design features I1: a rig with no bound guid is skipped before anything is
// dialed — reading from a device Commander cannot name is tolerable, writing to
// one is not.
func TestClockPhaseRefusesUnboundRig(t *testing.T) {
	h := newClockHarness(t)
	h.profile.GUID = ""
	h.run()

	if h.dials != 0 {
		t.Errorf("dialed %d times for an unbound rig, want 0", h.dials)
	}
}

// The cpuId fallback: a device whose guid changed (factory reset) still resolves
// through a matching cpuId, exactly as the pull path's rule does.
func TestClockPhaseAcceptsCPUIDFallback(t *testing.T) {
	h := newClockHarness(t)
	h.c.info = control.Info{GUID: "changed01", CPUID: testCPUID}
	h.run()

	if len(h.c.sets) != 1 {
		t.Errorf("cpuId fallback did not match: %d writes", len(h.c.sets))
	}
}

// An empty cpuId on both sides (the CM4 device class) must not be read as a match.
func TestClockPhaseEmptyCPUIDIsNotAMatch(t *testing.T) {
	h := newClockHarness(t)
	h.profile.CPUID = ""
	h.c.info = control.Info{GUID: "changed01", CPUID: ""}
	h.run()

	if len(h.c.sets) != 0 {
		t.Errorf("two empty cpuIds matched each other: %+v", h.c.sets)
	}
}

// design features I3: a working subsystem stops the write, and — unlike a
// failure — starts no cooldown, because a plan can end at any time.
func TestClockPhaseSkipsBusyDevice(t *testing.T) {
	h := newClockHarness(t)
	h.c.state = control.AppState{Busy: true, Reason: "capture"}
	h.run()

	if len(h.c.sets) != 0 {
		t.Fatalf("wrote to a busy device: %+v", h.c.sets)
	}
	if !h.w.clockRetryAt.IsZero() {
		t.Error("a busy device started a cooldown; it should be retried next cycle")
	}
}

// design features I8: a device that cannot answer is a transport failure, not a
// busy device, and must back off — otherwise a hung port is retried and logged
// every cycle.
func TestClockPhaseCooldownAfterTransportFailure(t *testing.T) {
	h := newClockHarness(t)
	h.c.errState = errors.New("i/o timeout")
	h.run()

	if len(h.c.sets) != 0 {
		t.Fatalf("wrote despite a transport failure: %+v", h.c.sets)
	}
	if h.w.clockRetryAt.IsZero() {
		t.Fatal("a transport failure did not start the cooldown")
	}
	// A second cycle inside the window must not dial again.
	before := h.dials
	h.run()
	if h.dials != before {
		t.Errorf("dialed again inside the cooldown: %d → %d", before, h.dials)
	}
}

// design features I4: an unresolvable zone leaves the device alone rather than
// guessing, which would silently move its wall clock by hours.
func TestClockSyncSkipsUnknownZone(t *testing.T) {
	h := newClockHarness(t)
	bad := h.c.times[0]
	bad.TimeZone = "Mars/Olympus_Mons"
	h.c.times = []control.Time{bad}
	h.run()

	if len(h.c.sets) != 0 {
		t.Fatalf("wrote with an unresolvable zone: %+v", h.c.sets)
	}
}

// design features I4: a device naming no zone at all must not be written to.
// time.LoadLocation answers "" with UTC and a nil error, so without an explicit
// guard this device would have its wall clock silently rewritten into UTC.
func TestClockSyncSkipsEmptyZone(t *testing.T) {
	h := newClockHarness(t)
	blank := h.c.times[0]
	blank.TimeZone = ""
	h.c.times = []control.Time{blank}
	h.run()

	if len(h.c.sets) != 0 {
		t.Fatalf("wrote with no zone reported: %+v", h.c.sets)
	}
}

// design features I6: a failure injected at any single step ends the phase with
// nothing written.
func TestClockPhaseWritesNothingOnAnyFailure(t *testing.T) {
	boom := errors.New("boom")
	steps := map[string]func(*clockHarness){
		"dial":          func(h *clockHarness) { h.dialErr = boom },
		"pi_get_info":   func(h *clockHarness) { h.c.errInfo = boom },
		"get_app_state": func(h *clockHarness) { h.c.errState = boom },
		"pi_get_time":   func(h *clockHarness) { h.c.errGet = boom },
		"pi_set_time":   func(h *clockHarness) { h.c.errSet = boom },
	}
	for name, inject := range steps {
		t.Run(name, func(t *testing.T) {
			h := newClockHarness(t)
			inject(h)
			h.run()

			if len(h.c.sets) != 0 {
				t.Errorf("failure at %s still wrote: %+v", name, h.c.sets)
			}
			if h.w.clockRetryAt.IsZero() {
				t.Errorf("failure at %s did not start the cooldown", name)
			}
		})
	}
}

// A failed confirming read must not undo a correction that already landed: the
// device is right, so no cooldown is owed.
func TestClockPhaseConfirmFailureKeepsTheCorrection(t *testing.T) {
	h := newClockHarness(t)
	h.c.errConfirm = errors.New("connection reset")
	h.run()

	if len(h.c.sets) != 1 {
		t.Fatalf("want the write to have landed, got %d", len(h.c.sets))
	}
	if !h.w.clockRetryAt.IsZero() {
		t.Error("a failed confirm started a cooldown against an already-corrected device")
	}
}

// design features I7: the phase acts only on an offset measured this cycle. A
// cycle that refused before opening a session leaves it zero, which must not
// authorise a write.
func TestClockPhaseIgnoresPriorCycleOffset(t *testing.T) {
	h := newClockHarness(t)
	h.w.cycleOffset = 0
	h.run()

	if h.dials != 0 {
		t.Errorf("dialed on an unmeasured cycle: %d", h.dials)
	}
}

// A device that drifted back into tolerance between the SMB handshake and the
// control connect is left alone.
func TestClockPhaseSkipsWhenRecoveredBeforeWrite(t *testing.T) {
	h := newClockHarness(t)
	h.c.times = []control.Time{deviceTimeAt(h.now.Add(-time.Second))}
	h.run()

	if len(h.c.sets) != 0 {
		t.Errorf("wrote to a device already within tolerance: %+v", h.c.sets)
	}
}

// Toggling sync_clock clears the latches, so enabling the feature on a rig that is
// already out of band re-announces it and drops any standing cooldown.
func TestNoteSyncClockSettingResetsLatches(t *testing.T) {
	h := newClockHarness(t)
	h.w.offsetOutOfBand = true
	h.w.clockFailLogged = true
	h.w.clockRetryAt = h.now.Add(time.Hour)
	h.w.syncClockWas = false

	h.w.noteSyncClockSetting(true)

	if h.w.offsetOutOfBand || h.w.clockFailLogged || !h.w.clockRetryAt.IsZero() {
		t.Errorf("latches survived the toggle: band=%v logged=%v retry=%v",
			h.w.offsetOutOfBand, h.w.clockFailLogged, h.w.clockRetryAt)
	}
	// An unchanged setting must not clear anything.
	h.w.offsetOutOfBand = true
	h.w.noteSyncClockSetting(true)
	if !h.w.offsetOutOfBand {
		t.Error("an unchanged toggle cleared the band latch")
	}
}

// The offset warning reports the transition, not the level: a standing condition
// must not log once per poll interval.
func TestRecordClockOffsetLatchesTheBand(t *testing.T) {
	h := newClockHarness(t)
	far := 2 * clockOffsetThreshold

	h.w.recordClockOffset(-far, true)
	if !h.w.offsetOutOfBand {
		t.Fatal("a far offset did not set the band latch")
	}
	h.w.recordClockOffset(-far, true)
	if !h.w.offsetOutOfBand {
		t.Fatal("the latch cleared while the condition stood")
	}
	h.w.recordClockOffset(time.Second, true)
	if h.w.offsetOutOfBand {
		t.Fatal("returning to tolerance did not clear the latch")
	}
	if h.w.cycleOffset != time.Second {
		t.Errorf("cycleOffset = %v, want 1s", h.w.cycleOffset)
	}
}
