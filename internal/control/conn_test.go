package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The payloads below are the shapes a real ASIAIR answers with, so a firmware
// change breaks a test here rather than a rig in the field.
const (
	realInfoResult = `{"is_pi4":true,"model":"ZWO AirPlus-RK3568 (Linux)","guid":"0a1b2c3d",` +
		`"cpuId":"0011223344556677","temp":36.111,"is_undervolt":false}`

	realIdleAppState = `{"page":"preview","annotate":{"is_working":false,"lapse_ms":0},
 "solve":{"is_working":false,"lapse_ms":0,"filename":""},
 "capture":{"exposure_mode":"single","is_working":false,"state":"idle"},
 "pa":{"is_working":false},"auto_goto":{"is_working":false},
 "stack":{"is_working":false,"frame_type":"light","stacked_frame":0},
 "auto_focus":{"result":{},"is_working":false,"focuser_opened":false,
               "reason":{"comment":"manually","code":0}},
 "avi_record":{"is_working":false,"lapse_sec":0,"fps":10.0},
 "merid_flip":{"is_working":false},"format_drive":{"is_working":false}}`

	realTimeResult = `{"time_zone":"America/Toronto","hour":16,"min":48,"sec":4,"day":27,"year":2026,"mon":7}`
)

var realTime = Time{Year: 2026, Mon: 7, Day: 27, Hour: 16, Min: 48, Sec: 4, TimeZone: "America/Toronto"}

// --- stub server ------------------------------------------------------------

type recordedRequest struct {
	ID     int
	Method string
	Params string
}

type recorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *recorder) add(req recordedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
}

func (r *recorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.reqs...)
}

// listen starts a one-connection stub server and returns its address. serve
// runs on the accepted connection and must not touch *testing.T: it outlives
// the test body when a case leaves the connection idle.
func listen(t *testing.T, serve func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		serve(c)
	}()
	return ln.Addr().String()
}

// scripted serves request lines by writing whatever handle returns for each,
// recording every request it read.
func scripted(rec *recorder, handle func(id int, method string, params json.RawMessage) []string) func(net.Conn) {
	return func(c net.Conn) {
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			var req struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				return
			}
			rec.add(recordedRequest{ID: req.ID, Method: req.Method, Params: string(req.Params)})
			for _, line := range handle(req.ID, req.Method, req.Params) {
				if _, err := c.Write([]byte(line + "\r\n")); err != nil {
					return
				}
			}
		}
	}
}

// okReply renders a successful reply in the device's envelope. The result is
// compacted first: the fixtures are indented for reading, and a reply spanning
// more than one line is not a thing the channel can carry.
func okReply(id int, method, result string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(result)); err != nil {
		buf.Reset()
		buf.WriteString(result)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"result":%s,"code":0,"id":%d}`, method, buf.String(), id)
}

// dialStub opens a client against a stub server, closed with the test.
func dialStub(t *testing.T, addr string) *conn {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	c, err := dialAddr(context.Background(), netsite.NewRegistry(nil).Site(netsite.Local), host, p, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// answering serves one method with a fixed result, ignoring everything else.
func answering(rec *recorder, result string) func(net.Conn) {
	return scripted(rec, func(id int, method string, _ json.RawMessage) []string {
		return []string{okReply(id, method, result)}
	})
}

// --- reply matching ---------------------------------------------------------

func TestCallMatchesReplyByID(t *testing.T) {
	tests := []struct {
		name  string
		lines func(id int) []string
	}{
		{
			name: "reply after two event pushes",
			lines: func(id int) []string {
				return []string{
					`{"Event":"Version","Timestamp":"1753","id":1,"version":"1.9"}`,
					// An event carrying our own id must still be discarded: an
					// Event is never a reply.
					fmt.Sprintf(`{"Event":"PiStatus","Timestamp":"1753","id":%d,"temp":36.1}`, id),
					okReply(id, "pi_get_info", realInfoResult),
				}
			},
		},
		{
			name: "reply after a foreign id",
			lines: func(id int) []string {
				return []string{
					`{"jsonrpc":"2.0","method":"get_connected_cameras","result":[],"code":0,"id":198}`,
					okReply(id, "pi_get_info", realInfoResult),
				}
			},
		},
		{
			name: "reply after another request's error",
			lines: func(id int) []string {
				return []string{
					`{"jsonrpc":"2.0","method":"get_controls","error":"device not connected","code":318,"id":230}`,
					okReply(id, "pi_get_info", realInfoResult),
				}
			},
		},
		{
			name: "reply after garbage lines",
			lines: func(id int) []string {
				return []string{
					`not json at all`,
					`{"truncated":`,
					``,
					okReply(id, "pi_get_info", realInfoResult),
				}
			},
		},
	}

	want := Info{GUID: "0a1b2c3d", CPUID: "0011223344556677", Model: "ZWO AirPlus-RK3568 (Linux)"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			addr := listen(t, scripted(rec, func(id int, _ string, _ json.RawMessage) []string {
				return tc.lines(id)
			}))
			c := dialStub(t, addr)

			got, err := c.Info(context.Background())
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			if got != want {
				t.Errorf("Info = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCallReportsDeviceErrorCode(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, scripted(rec, func(id int, method string, _ json.RawMessage) []string {
		return []string{fmt.Sprintf(
			`{"jsonrpc":"2.0","method":%q,"error":"device not connected","code":318,"id":%d}`, method, id)}
	}))
	c := dialStub(t, addr)

	_, err := c.Info(context.Background())
	if err == nil {
		t.Fatal("Info: want an error for a non-zero code, got nil")
	}
	var derr *DeviceError
	if !errors.As(err, &derr) {
		t.Fatalf("Info: want a *DeviceError, got %T: %v", err, err)
	}
	if derr.Code != 318 || derr.Message != "device not connected" || derr.Method != "pi_get_info" {
		t.Errorf("DeviceError = %+v, want code 318 / device not connected / pi_get_info", derr)
	}
}

func TestCallFailsOnTruncatedReply(t *testing.T) {
	addr := listen(t, func(c net.Conn) {
		sc := bufio.NewScanner(c)
		sc.Scan()
		// A line the device never finished, then the socket goes away.
		c.Write([]byte(`{"jsonrpc":"2.0","method":"pi_get_info","result":{"guid":"a2a`))
	})
	c := dialStub(t, addr)

	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("Info: want an error for a truncated reply, got nil")
	}
}

func TestCallTimesOutWhenNothingAnswers(t *testing.T) {
	addr := listen(t, func(c net.Conn) {
		// Accept, read nothing, answer nothing: the hung port that must not be
		// mistaken for a busy device.
		<-time.After(2 * time.Second)
	})
	c := dialStub(t, addr)
	c.timeout = 150 * time.Millisecond

	st, err := c.AppState(context.Background())
	if err == nil {
		t.Fatal("AppState: want a transport error from a hung port, got nil")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("AppState error = %v, want a deadline error", err)
	}
	if st.Busy {
		t.Error("AppState: a hung port must not report Busy; the caller routes busy and failure differently")
	}
}

func TestRequestIDsStartAt200AndIncrement(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, scripted(rec, func(id int, method string, _ json.RawMessage) []string {
		result := realInfoResult
		if method == "pi_get_time" {
			result = realTimeResult
		}
		return []string{okReply(id, method, result)}
	}))
	c := dialStub(t, addr)

	ctx := context.Background()
	if _, err := c.Info(ctx); err != nil {
		t.Fatalf("Info: %v", err)
	}
	if _, err := c.Info(ctx); err != nil {
		t.Fatalf("Info: %v", err)
	}
	if _, err := c.GetTime(ctx); err != nil {
		t.Fatalf("GetTime: %v", err)
	}

	got := rec.all()
	wantIDs := []int{200, 201, 202}
	if len(got) != len(wantIDs) {
		t.Fatalf("recorded %d requests, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("request %d id = %d, want %d", i, got[i].ID, want)
		}
		// The read methods take no params; sending an empty one is not the
		// shape the app uses.
		if got[i].Params != "" {
			t.Errorf("request %d carried params %q, want none", i, got[i].Params)
		}
	}
}

func TestCallAfterCloseFails(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, answering(rec, realInfoResult))
	c := dialStub(t, addr)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("Info after Close: want an error, got nil")
	}
}

// --- app state --------------------------------------------------------------

func TestAppStateVerdict(t *testing.T) {
	tests := []struct {
		name       string
		result     string
		wantBusy   bool
		wantReason string
	}{
		{
			name:   "real idle device",
			result: realIdleAppState,
		},
		{
			name: "capture state is not idle",
			result: `{"page":"preview","capture":{"exposure_mode":"single","is_working":false,"state":"exposing"},
			          "solve":{"is_working":false}}`,
			wantBusy:   true,
			wantReason: "capture.state=exposing",
		},
		{
			name: "a subsystem is working",
			result: `{"page":"preview","capture":{"is_working":false,"state":"idle"},
			          "solve":{"is_working":true,"lapse_ms":120,"filename":""}}`,
			wantBusy:   true,
			wantReason: "solve",
		},
		{
			name: "a subsystem this package has never heard of is working",
			result: `{"page":"preview","capture":{"is_working":false,"state":"idle"},
			          "hypertelescope":{"is_working":true}}`,
			wantBusy:   true,
			wantReason: "hypertelescope",
		},
		{
			name: "a nested reason code is not a subsystem",
			result: `{"auto_focus":{"result":{},"is_working":false,"focuser_opened":false,
			           "reason":{"comment":"manually","code":0}}}`,
		},
		{
			name:       "result is a bare string",
			result:     `"preview"`,
			wantBusy:   true,
			wantReason: unreadableAppState,
		},
		{
			name:       "result is null",
			result:     `null`,
			wantBusy:   true,
			wantReason: unreadableAppState,
		},
		{
			name:       "result is an array",
			result:     `[1,2,3]`,
			wantBusy:   true,
			wantReason: unreadableAppState,
		},
		{
			name:       "a subsystem field has an unexpected type",
			result:     `{"capture":{"is_working":"yes"}}`,
			wantBusy:   true,
			wantReason: unreadableAppState,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			addr := listen(t, answering(rec, tc.result))
			c := dialStub(t, addr)

			st, err := c.AppState(context.Background())
			// Every case here is a reply that arrived: an unreadable one is a
			// busy verdict, never an error.
			if err != nil {
				t.Fatalf("AppState: unexpected error %v", err)
			}
			if st.Busy != tc.wantBusy || st.Reason != tc.wantReason {
				t.Errorf("AppState = %+v, want busy=%v reason=%q", st, tc.wantBusy, tc.wantReason)
			}
		})
	}
}

func TestAppStateTransportFailureIsAnError(t *testing.T) {
	addr := listen(t, func(c net.Conn) {
		sc := bufio.NewScanner(c)
		sc.Scan()
		c.Close() // read the request, answer nothing
	})
	c := dialStub(t, addr)

	st, err := c.AppState(context.Background())
	if err == nil {
		t.Fatal("AppState: want an error when no reply arrives, got nil")
	}
	if st.Busy {
		t.Error("AppState: a connection that dies must not report Busy")
	}
}

// --- clock ------------------------------------------------------------------

func TestGetTime(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, answering(rec, realTimeResult))
	c := dialStub(t, addr)

	got, err := c.GetTime(context.Background())
	if err != nil {
		t.Fatalf("GetTime: %v", err)
	}
	if got != realTime {
		t.Errorf("GetTime = %+v, want %+v", got, realTime)
	}
}

func TestGetTimeRejectsUnusableReadings(t *testing.T) {
	tests := []struct {
		name   string
		result string
	}{
		// An empty zone must not reach the caller: time.LoadLocation("")
		// resolves to UTC without an error, which would express a device's
		// clock in the wrong zone rather than failing.
		{name: "no time zone", result: `{"hour":16,"min":48,"sec":4,"day":27,"year":2026,"mon":7}`},
		{name: "no reading at all", result: `{}`},
		{name: "impossible month", result: `{"time_zone":"UTC","year":2026,"mon":13,"day":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			addr := listen(t, answering(rec, tc.result))
			c := dialStub(t, addr)

			if _, err := c.GetTime(context.Background()); err == nil {
				t.Fatal("GetTime: want an error, got nil")
			}
		})
	}
}

func TestSetTimeSendsTheDeviceParamShape(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, scripted(rec, func(id int, method string, _ json.RawMessage) []string {
		return []string{okReply(id, method, "0")}
	}))
	c := dialStub(t, addr)

	if err := c.SetTime(context.Background(), realTime); err != nil {
		t.Fatalf("SetTime: %v", err)
	}

	got := rec.all()
	if len(got) != 1 || got[0].Method != "pi_set_time" {
		t.Fatalf("recorded requests = %+v, want one pi_set_time", got)
	}
	// The device takes its params as a one-element array wrapping the object.
	var params []map[string]any
	if err := json.Unmarshal([]byte(got[0].Params), &params); err != nil {
		t.Fatalf("decode params %q: %v", got[0].Params, err)
	}
	if len(params) != 1 {
		t.Fatalf("params = %v, want a one-element array", params)
	}
	want := map[string]any{
		"time_zone": "America/Toronto",
		"hour":      float64(16), "min": float64(48), "sec": float64(4),
		"day": float64(27), "year": float64(2026), "mon": float64(7),
	}
	if !reflect.DeepEqual(params[0], want) {
		t.Errorf("params[0] = %v, want %v", params[0], want)
	}
}

func TestSetTimeRefusesAnUnusableValue(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, scripted(rec, func(id int, method string, _ json.RawMessage) []string {
		return []string{okReply(id, method, "0")}
	}))
	c := dialStub(t, addr)

	if err := c.SetTime(context.Background(), Time{Year: 2026, Mon: 7, Day: 27}); err == nil {
		t.Fatal("SetTime: want an error for a reading with no zone, got nil")
	}
	if got := rec.all(); len(got) != 0 {
		t.Errorf("recorded requests = %+v, want none: a refused write must not reach the device", got)
	}
}

func TestSetTimeReportsADeviceRefusal(t *testing.T) {
	rec := &recorder{}
	addr := listen(t, scripted(rec, func(id int, method string, _ json.RawMessage) []string {
		return []string{fmt.Sprintf(
			`{"jsonrpc":"2.0","method":%q,"error":"busy","code":404,"id":%d}`, method, id)}
	}))
	c := dialStub(t, addr)

	if err := c.SetTime(context.Background(), realTime); err == nil {
		t.Fatal("SetTime: want an error for a non-zero code, got nil")
	}
}

// --- fake -------------------------------------------------------------------

func TestFakeRecordsWhatWasAskedOfTheDevice(t *testing.T) {
	f := NewFake()
	f.SetInfo(Info{GUID: "0a1b2c3d", CPUID: "0011223344556677"})
	f.SetDeviceTime(Time{Year: 2019, Mon: 2, Day: 14, Hour: 5, Min: 15, Sec: 11, TimeZone: "America/Toronto"})

	ctx := context.Background()
	client, err := f.Dialer()(ctx, netsite.LocalEndpoint("192.0.2.7"), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := f.Dials(); !reflect.DeepEqual(got, []netsite.Endpoint{netsite.LocalEndpoint("192.0.2.7")}) {
		t.Errorf("Dials = %v, want [192.0.2.7]", got)
	}

	st, err := client.AppState(ctx)
	if err != nil || st.Busy {
		t.Fatalf("AppState = %+v, %v; want an idle device", st, err)
	}
	if err := client.SetTime(ctx, realTime); err != nil {
		t.Fatalf("SetTime: %v", err)
	}
	if got := f.SetTimeCalls(); !reflect.DeepEqual(got, []Time{realTime}) {
		t.Errorf("SetTimeCalls = %+v, want one write of %+v", got, realTime)
	}
	// The write is applied, so the confirming read reports it.
	after, err := client.GetTime(ctx)
	if err != nil || after != realTime {
		t.Errorf("GetTime after SetTime = %+v, %v; want %+v", after, err, realTime)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.Closes() != 1 {
		t.Errorf("Closes = %d, want 1", f.Closes())
	}
}

func TestFakeInjectsFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	f := NewFake()
	f.FailDial(boom)
	if _, err := f.Dialer()(ctx, netsite.LocalEndpoint("192.0.2.7"), time.Second); !errors.Is(err, boom) {
		t.Errorf("dial error = %v, want boom", err)
	}
	if _, err := FailingDialer(boom)(ctx, netsite.LocalEndpoint("192.0.2.7"), time.Second); !errors.Is(err, boom) {
		t.Errorf("FailingDialer error = %v, want boom", err)
	}

	// The second read fails while the first succeeds — the clock phase reads
	// twice, and a failed confirming read is its own case.
	g := NewFake()
	g.SetDeviceTime(realTime)
	g.OnGetTime = func(call int) {
		if call == 2 {
			g.FailGetTime(boom)
		}
	}
	if _, err := g.GetTime(ctx); err != nil {
		t.Fatalf("first GetTime: %v", err)
	}
	if _, err := g.GetTime(ctx); !errors.Is(err, boom) {
		t.Errorf("second GetTime error = %v, want boom", err)
	}
	if g.GetTimeCalls() != 2 {
		t.Errorf("GetTimeCalls = %d, want 2", g.GetTimeCalls())
	}

	h := NewFake()
	h.FailSetTime(boom)
	if err := h.SetTime(ctx, realTime); !errors.Is(err, boom) {
		t.Errorf("SetTime error = %v, want boom", err)
	}
	if got := h.SetTimeCalls(); len(got) != 0 {
		t.Errorf("SetTimeCalls = %+v, want none for a failed write", got)
	}
}
