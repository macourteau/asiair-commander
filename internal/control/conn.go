package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

const (
	// defaultDialTimeout bounds the TCP connect when the caller passes none.
	defaultDialTimeout = 3 * time.Second
	// callTimeout bounds one request/reply exchange, including the event pushes
	// discarded while waiting for the matching id.
	callTimeout = 5 * time.Second
	// firstRequestID is where this client's ids start. The app uses low ids;
	// there is no shared id space, but staying clear of it costs nothing.
	firstRequestID = 200
	// maxLineBytes caps one reply line, so a device that never sends a newline
	// cannot grow the read buffer without bound.
	maxLineBytes = 1 << 20

	// captureSubsystem is the one subsystem with a state field worth reading:
	// capture reports "idle" when it is not exposing.
	captureSubsystem = "capture"
	idleState        = "idle"
	// unreadableAppState is the reason reported when a get_app_state reply
	// arrives and cannot be read. It is a busy verdict, never an error.
	unreadableAppState = "unreadable app state"
)

// conn is one open control-channel connection.
type conn struct {
	c net.Conn
	// sc reads CRLF-delimited reply lines. A scanner caps the line length for
	// free and keeps its error, so a dead connection stays dead.
	sc *bufio.Scanner
	// nextID is the id of the next request written.
	nextID int
	// timeout bounds one exchange; a field rather than the constant so tests
	// can shorten it.
	timeout time.Duration
	closed  bool
}

var _ Client = (*conn)(nil)

// NewDialer returns the production Dialer: it opens the control channel on the
// endpoint's netsite.ControlPort through the endpoint's site in sites (see the
// package doc: the other ports a device answers on do not serve these
// methods). timeout bounds the
// TCP connect; zero means defaultDialTimeout plus the site's 1 round-trip
// margin.
func NewDialer(sites netsite.Sites) Dialer {
	return func(ctx context.Context, ep netsite.Endpoint, timeout time.Duration) (Client, error) {
		return dialAddr(ctx, sites.Site(ep.Site), ep.IP, netsite.ControlPort, timeout)
	}
}

func dialAddr(ctx context.Context, site netsite.Site, host string, port int, timeout time.Duration) (*conn, error) {
	t := defaultTimeouts(site)
	if timeout <= 0 {
		timeout = t.dial
	}
	c, err := site.DialTCP(ctx, host, port, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", net.JoinHostPort(host, strconv.Itoa(port)), err)
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), maxLineBytes)
	return &conn{c: c, sc: sc, nextID: firstRequestID, timeout: t.call}, nil
}

// timeouts are the control channel's budgets on one site.
type timeouts struct {
	dial, call time.Duration
}

// defaultTimeouts adds the site's latency margins to the constants, so on the
// local site they are the constants exactly. The connect is 1 round trip, and
// so is one call: the request and its reply, the event pushes discarded while
// waiting arriving on the same stream.
func defaultTimeouts(site netsite.Site) timeouts {
	return timeouts{
		dial: defaultDialTimeout + netsite.RoundTrips(site, 1),
		call: callTimeout + netsite.RoundTrips(site, 1),
	}
}

// Close closes the connection. Calls after it fail rather than dialing again.
func (c *conn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.c.Close()
}

// request is the wire form of a call. params is omitted for the methods that
// take none.
type request struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// reply is one line read from the channel: a reply to a request, or one of the
// unsolicited event pushes discarded while waiting for one.
type reply struct {
	Event  string          `json:"Event"`
	ID     json.RawMessage `json:"id"`
	Code   int             `json:"code"`
	Error  json.RawMessage `json:"error"`
	Result json.RawMessage `json:"result"`
}

// errorText renders the error field, which the device sends as a string.
// Anything else is passed through verbatim rather than dropped, so a firmware
// that changes the shape still produces a diagnosable message.
func (r reply) errorText() string {
	if len(r.Error) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Error, &s); err == nil {
		return s
	}
	return string(r.Error)
}

// matchesID reports whether this line answers request id. Ids come back as
// numbers on this channel, but the sibling scan_air protocol quotes them, so a
// quoted number is accepted too.
func (r reply) matchesID(id int) bool {
	if r.Event != "" || len(r.ID) == 0 {
		return false
	}
	raw := string(bytes.Trim(r.ID, `"`))
	got, err := strconv.Atoi(raw)
	return err == nil && got == id
}

// call writes one request and returns the result of the reply carrying its id,
// discarding event pushes and any other id along the way. A non-zero code
// becomes a *DeviceError.
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.closed {
		return nil, fmt.Errorf("%s: connection closed", method)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	id := c.nextID
	c.nextID++

	body, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", method, err)
	}
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.c.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	if _, err := c.c.Write(append(body, '\r', '\n')); err != nil {
		return nil, fmt.Errorf("%s: write request: %w", method, err)
	}

	for {
		line, err := c.readLine()
		if err != nil {
			return nil, fmt.Errorf("%s: awaiting reply: %w", method, err)
		}
		var rep reply
		// A line that does not decode is noise on a channel that is not ours
		// alone; it is discarded like an event push.
		if err := json.Unmarshal(line, &rep); err != nil {
			continue
		}
		if !rep.matchesID(id) {
			continue
		}
		if rep.Code != 0 {
			return nil, &DeviceError{Method: method, Code: rep.Code, Message: rep.errorText()}
		}
		return rep.Result, nil
	}
}

// readLine returns the next line, valid only until the following call.
func (c *conn) readLine() ([]byte, error) {
	if !c.sc.Scan() {
		if err := c.sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return c.sc.Bytes(), nil
}

// Info implements Client.
func (c *conn) Info(ctx context.Context) (Info, error) {
	raw, err := c.call(ctx, "pi_get_info", nil)
	if err != nil {
		return Info{}, err
	}
	var r struct {
		GUID  string `json:"guid"`
		CPUID string `json:"cpuId"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Info{}, fmt.Errorf("pi_get_info: decode result: %w", err)
	}
	if r.GUID == "" && r.CPUID == "" {
		return Info{}, fmt.Errorf("pi_get_info: device reported no identity")
	}
	return Info{GUID: r.GUID, CPUID: r.CPUID, Model: r.Model}, nil
}

// AppState implements Client. The asymmetry between a decode failure (busy,
// nil error) and a transport failure (error) is the contract documented on the
// interface, and is the whole reason this method exists rather than exposing
// the raw object.
func (c *conn) AppState(ctx context.Context) (AppState, error) {
	raw, err := c.call(ctx, "get_app_state", nil)
	if err != nil {
		return AppState{}, err
	}
	return appStateVerdict(raw), nil
}

// appStateVerdict reduces a get_app_state result to a busy/idle verdict.
//
// The device answers with one entry per subsystem, plus scalars such as
// "page" that carry no state:
//
//	{"page":"preview","capture":{"exposure_mode":"single","is_working":false,
//	 "state":"idle"},"solve":{"is_working":false,...},...}
//
// Any object value with "is_working": true means busy, whether or not this
// package has heard of it, and capture reports a state alongside its flag.
// Subsystems carry nested objects of their own (auto_focus has a reason object
// with its own code) so the scan is one level deep by construction. Keys are
// scanned in order so the reason a caller logs is stable.
func appStateVerdict(raw json.RawMessage) AppState {
	var subsystems map[string]json.RawMessage
	// A nil map means the result was JSON null: it decoded, but says nothing
	// about the device, which is not evidence of idleness.
	if err := json.Unmarshal(raw, &subsystems); err != nil || subsystems == nil {
		return AppState{Busy: true, Reason: unreadableAppState}
	}
	names := make([]string, 0, len(subsystems))
	for name := range subsystems {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		body := subsystems[name]
		if !isJSONObject(body) {
			continue
		}
		var sub struct {
			IsWorking bool    `json:"is_working"`
			State     *string `json:"state"`
		}
		if err := json.Unmarshal(body, &sub); err != nil {
			return AppState{Busy: true, Reason: unreadableAppState}
		}
		if sub.IsWorking {
			return AppState{Busy: true, Reason: name}
		}
		if name == captureSubsystem && sub.State != nil && *sub.State != idleState {
			return AppState{Busy: true, Reason: captureSubsystem + ".state=" + *sub.State}
		}
	}
	return AppState{}
}

// isJSONObject reports whether raw is a JSON object, so scalar entries such as
// "page" are skipped rather than treated as an unreadable subsystem.
func isJSONObject(raw json.RawMessage) bool {
	return bytes.HasPrefix(bytes.TrimLeft(raw, " \t\r\n"), []byte("{"))
}

// deviceTime is the wire form of a clock reading, shared by pi_get_time's
// result and pi_set_time's params.
type deviceTime struct {
	Year     int    `json:"year"`
	Mon      int    `json:"mon"`
	Day      int    `json:"day"`
	Hour     int    `json:"hour"`
	Min      int    `json:"min"`
	Sec      int    `json:"sec"`
	TimeZone string `json:"time_zone"`
}

// GetTime implements Client.
func (c *conn) GetTime(ctx context.Context) (Time, error) {
	raw, err := c.call(ctx, "pi_get_time", nil)
	if err != nil {
		return Time{}, err
	}
	var r deviceTime
	if err := json.Unmarshal(raw, &r); err != nil {
		return Time{}, fmt.Errorf("pi_get_time: decode result: %w", err)
	}
	t := Time{Year: r.Year, Mon: r.Mon, Day: r.Day, Hour: r.Hour, Min: r.Min, Sec: r.Sec, TimeZone: r.TimeZone}
	if err := t.validate(); err != nil {
		return Time{}, fmt.Errorf("pi_get_time: %w", err)
	}
	return t, nil
}

// SetTime implements Client. The device takes its params as a one-element
// array wrapping the object.
func (c *conn) SetTime(ctx context.Context, t Time) error {
	if err := t.validate(); err != nil {
		return fmt.Errorf("pi_set_time: %w", err)
	}
	params := []deviceTime{{
		Year: t.Year, Mon: t.Mon, Day: t.Day,
		Hour: t.Hour, Min: t.Min, Sec: t.Sec,
		TimeZone: t.TimeZone,
	}}
	_, err := c.call(ctx, "pi_set_time", params)
	return err
}
