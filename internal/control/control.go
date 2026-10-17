// Package control is the seam over the ASIAIR control channel: one short-lived
// TCP session speaking line-delimited JSON-RPC, opened to serve the device
// clock and nothing else. It reads the device's identity and wall clock, asks
// whether the device is idle, and sets the clock. The real implementation
// dials the device; an in-memory Fake drives the rig worker's tests.
//
// Three properties of the channel shape the package:
//
//   - Port 4700, only. A device answers on several ports (4400 guider, 4500,
//     4700 imager, 4800, 4801, 4900) and they do not agree with each other:
//     pi_get_info, pi_get_time and pi_set_time are served here, and
//     get_app_state returns the full state object here where the same method on
//     4400 returns a bare string. Everything in this package assumes 4700.
//
//   - Replies must be matched by id. The channel pushes unsolicited events —
//     {"Event":"Version",...} on connect, then PiStatus and Station
//     periodically — so the next line to arrive is frequently not the answer to
//     the request just sent. Every call reads lines until one decodes carrying
//     the id it wrote, discarding everything else. Requests are
//     CRLF-terminated, and ids start at 200 and increment per call.
//
//   - Errors arrive as a non-zero top-level code plus a string error field, not
//     as JSON-RPC's standard error object:
//     {"jsonrpc":"2.0","method":"get_controls","error":"device not
//     connected","code":318,"id":230}. Such a reply becomes a *DeviceError; a
//     zero code is success.
package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// Client is one open control-channel connection to a device. A connection is
// opened for one exchange and closed; it is not safe for concurrent use, and
// callers drive it from a single goroutine.
type Client interface {
	// Info reports the device's identity (pi_get_info), so a caller can
	// corroborate which device is on the far end of the connection that will
	// carry a write.
	Info(ctx context.Context) (Info, error)

	// AppState reports whether the device is busy (get_app_state).
	//
	// Its failure contract is deliberately asymmetric, and callers depend on
	// it. A decode failure — a reply arrived and was not understood — returns
	// Busy true with the reason "unreadable app state" and a nil error, so a
	// device this package cannot read is never treated as idle. A transport
	// failure — no reply, a timeout, a non-zero code — returns an error,
	// because a device that cannot answer is not a device that is busy.
	//
	// The caller routes the two differently: a busy device is retried on the
	// next cycle, an erroring one is put in a long cooldown. Conflating them
	// turns a hung port into a per-cycle retry, and a per-cycle log line.
	AppState(ctx context.Context) (AppState, error)

	// GetTime reads the device's wall clock and IANA zone (pi_get_time).
	GetTime(ctx context.Context) (Time, error)

	// SetTime sets the device's wall clock (pi_set_time). The device applies it
	// in the zone t names, so callers express the instant in the zone the device
	// itself reported rather than the host's.
	SetTime(ctx context.Context, t Time) error

	// Close releases the connection. It is safe to call more than once.
	Close() error
}

// Dialer opens one connection to the device at ep, through ep's site. It is
// injected into the rig worker so tests substitute a fake, and timeout bounds
// the TCP connect only — each call on the returned Client carries its own
// deadline.
type Dialer func(ctx context.Context, ep netsite.Endpoint, timeout time.Duration) (Client, error)

// Info is a device's self-reported identity, from pi_get_info. GUID and CPUID
// are the same identifiers the scan_air discovery reply carries, so a caller
// can match them against a rig's binding.
type Info struct{ GUID, CPUID, Model string }

// AppState is the reduced verdict on get_app_state, not the raw object: Busy is
// true if any subsystem reports work in progress, and Reason names the first
// one found (or "unreadable app state" when the reply could not be read).
//
// The reduction is deliberately generic. Any top-level object carrying
// "is_working": true means busy, as does a capture.state other than "idle", so
// firmware that adds a subsystem is respected without a code change here.
type AppState struct {
	Busy   bool
	Reason string
}

// Time is a wall-clock reading in the device's own zone, as pi_get_time reports
// it and pi_set_time accepts it. The device has no sub-second resolution on
// this channel: the fields are whole seconds, which is what pins the useful
// correction threshold above ~2s.
type Time struct {
	Year, Mon, Day, Hour, Min, Sec int
	TimeZone                       string // IANA name, e.g. "America/Toronto"
}

// validate rejects a reading or a write this package should not act on. It is
// applied to what the device reports as well as to what a caller asks to write,
// because both directions feed a clock correction and a nonsense value in
// either one is worse than no correction at all.
func (t Time) validate() error {
	// time.LoadLocation("") resolves to UTC without an error, so an empty zone
	// would silently express a device's wall clock in the wrong zone rather
	// than failing.
	if t.TimeZone == "" {
		return errors.New("no time zone reported")
	}
	if t.Year <= 0 || t.Mon < 1 || t.Mon > 12 || t.Day < 1 || t.Day > 31 ||
		t.Hour < 0 || t.Hour > 23 || t.Min < 0 || t.Min > 59 || t.Sec < 0 || t.Sec > 60 {
		return fmt.Errorf("implausible reading %04d-%02d-%02d %02d:%02d:%02d",
			t.Year, t.Mon, t.Day, t.Hour, t.Min, t.Sec)
	}
	return nil
}

// DeviceError is a reply the device answered with a non-zero code. It is
// distinct from a transport error only for diagnosis — every caller treats both
// as a failed exchange.
type DeviceError struct {
	Method  string
	Code    int
	Message string
}

func (e *DeviceError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: device code %d", e.Method, e.Code)
	}
	return fmt.Sprintf("%s: device code %d: %s", e.Method, e.Code, e.Message)
}
