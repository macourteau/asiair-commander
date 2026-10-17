package control

import (
	"context"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// Fake is an in-memory Client for the rig worker's tests: a scripted identity,
// app state and clock, a recorded log of SetTime writes, and an injectable
// error per method. Dialer hands the same Fake to every dial, so a test can
// assert both what was asked of the device and whether it was dialed at all.
//
// It is safe for concurrent use; the rig worker touches a client from more than
// one goroutine.
type Fake struct {
	mu       sync.Mutex
	info     Info
	appState AppState
	time     Time

	dialed   []netsite.Endpoint
	setTimes []Time
	getTimes int
	closes   int

	errDial     error
	errInfo     error
	errAppState error
	errGetTime  error
	errSetTime  error

	// OnGetTime, when set, is invoked (without the fake's lock held) at the
	// start of every GetTime with the number of that call, 1 for the first. A
	// test uses it to change the fake between a caller's successive reads — the
	// clock phase reads twice, and the confirming second read is a different
	// case from the first.
	OnGetTime func(call int)

	// OnSetTime, when set, is invoked (without the fake's lock held) after a
	// write is recorded and applied, so a test can model a device that does not
	// keep what it was given.
	OnSetTime func(t Time)
}

var _ Client = (*Fake)(nil)

// NewFake returns a Fake reporting an idle device with no identity and a zero
// clock; tests set what they need.
func NewFake() *Fake { return &Fake{} }

// Dialer returns a Dialer handing out this Fake, recording each endpoint dialed.
func (f *Fake) Dialer() Dialer {
	return func(ctx context.Context, ep netsite.Endpoint, timeout time.Duration) (Client, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.dialed = append(f.dialed, ep)
		if f.errDial != nil {
			return nil, f.errDial
		}
		return f, nil
	}
}

// FailingDialer returns a Dialer that never connects — the unreachable or
// firewalled control port, with no Fake behind it to be reached by accident.
func FailingDialer(err error) Dialer {
	return func(context.Context, netsite.Endpoint, time.Duration) (Client, error) {
		return nil, err
	}
}

// --- test-driving API -------------------------------------------------------

// SetInfo sets the identity Info reports.
func (f *Fake) SetInfo(info Info) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.info = info
}

// SetAppState sets the verdict AppState reports.
func (f *Fake) SetAppState(st AppState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appState = st
}

// SetDeviceTime sets the clock GetTime reports.
func (f *Fake) SetDeviceTime(t Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.time = t
}

// DeviceTime returns the clock the fake currently reports — the value written
// by the last successful SetTime, unless a test has changed it since.
func (f *Fake) DeviceTime() Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.time
}

// FailDial makes the Dialer return err instead of a client (nil clears it).
func (f *Fake) FailDial(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errDial = err
}

// FailInfo makes Info return err.
func (f *Fake) FailInfo(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errInfo = err
}

// FailAppState makes AppState return err — a transport failure, distinct from
// the busy verdict SetAppState scripts.
func (f *Fake) FailAppState(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errAppState = err
}

// FailGetTime makes GetTime return err.
func (f *Fake) FailGetTime(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errGetTime = err
}

// FailSetTime makes SetTime return err without recording the write.
func (f *Fake) FailSetTime(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errSetTime = err
}

// Dials returns the endpoints dialed through this fake's Dialer, in order. An
// empty result is the assertion that no connection was ever opened.
func (f *Fake) Dials() []netsite.Endpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netsite.Endpoint(nil), f.dialed...)
}

// SetTimeCalls returns the clock writes recorded, in order. An empty result is
// the assertion that the device was left untouched.
func (f *Fake) SetTimeCalls() []Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Time(nil), f.setTimes...)
}

// GetTimeCalls returns how many times the clock was read.
func (f *Fake) GetTimeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getTimes
}

// Closes returns how many times the client was closed, so a test can assert the
// connection did not leak.
func (f *Fake) Closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// --- Client -----------------------------------------------------------------

// Info implements Client.
func (f *Fake) Info(ctx context.Context) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errInfo != nil {
		return Info{}, f.errInfo
	}
	return f.info, nil
}

// AppState implements Client, including its asymmetry: FailAppState models the
// transport failure that returns an error, SetAppState the busy verdict that
// does not.
func (f *Fake) AppState(ctx context.Context) (AppState, error) {
	if err := ctx.Err(); err != nil {
		return AppState{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errAppState != nil {
		return AppState{}, f.errAppState
	}
	return f.appState, nil
}

// GetTime implements Client.
func (f *Fake) GetTime(ctx context.Context) (Time, error) {
	if err := ctx.Err(); err != nil {
		return Time{}, err
	}
	f.mu.Lock()
	f.getTimes++
	call, hook := f.getTimes, f.OnGetTime
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errGetTime != nil {
		return Time{}, f.errGetTime
	}
	return f.time, nil
}

// SetTime implements Client. A recorded write is applied, so the confirming
// read that follows it reports what was written.
func (f *Fake) SetTime(ctx context.Context, t Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	if f.errSetTime != nil {
		err := f.errSetTime
		f.mu.Unlock()
		return err
	}
	f.setTimes = append(f.setTimes, t)
	f.time = t
	hook := f.OnSetTime
	f.mu.Unlock()

	if hook != nil {
		hook(t)
	}
	return nil
}

// Close implements Client.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}
