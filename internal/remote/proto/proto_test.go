package proto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// testHandler is a scriptable Handler.
type testHandler struct {
	hello     func(callID string) HelloReply
	broadcast func(ctx context.Context) (BroadcastReply, error)
	hash      func(ctx context.Context, req HashRequest, progress func(int64)) (HashReply, error)
}

func (h *testHandler) Hello(_ context.Context, callID string) HelloReply {
	if h.hello != nil {
		return h.hello(callID)
	}
	return HelloReply{InstanceID: "test"}
}

func (h *testHandler) Broadcast(ctx context.Context, _ string) (BroadcastReply, error) {
	if h.broadcast != nil {
		return h.broadcast(ctx)
	}
	return BroadcastReply{}, nil
}

func (h *testHandler) Hash(ctx context.Context, _ string, req HashRequest, progress func(int64)) (HashReply, error) {
	return h.hash(ctx, req, progress)
}

// pipeServer serves h over in-memory connections and counts dials; failFirst
// dials fail like a tunnel that is not up yet.
type pipeServer struct {
	h         Handler
	opts      ServeOptions
	failFirst int32
	dials     atomic.Int32
}

func (p *pipeServer) dial(ctx context.Context) (net.Conn, error) {
	n := p.dials.Add(1)
	if n <= p.failFirst {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("tunnel not up")}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	go Serve(server, p.h, p.opts)
	return client, nil
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// TestHelloWhileHashRuns: a hash in flight never holds up a hello, which rides
// its own connection.
func TestHelloWhileHashRuns(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	h := &testHandler{hash: func(ctx context.Context, _ HashRequest, progress func(int64)) (HashReply, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return HashReply{Whole: "w"}, nil
	}}
	p := &pipeServer{h: h}
	c := NewClient(p.dial, nil)
	defer c.Close()

	done := make(chan error, 1)
	go func() {
		_, err := c.Hash(ctxFor(t, 10*time.Second), "h1", HashRequest{IP: "10.0.0.1"}, HashOptions{Stall: 10 * time.Second})
		done <- err
	}()
	<-started
	start := time.Now()
	if _, _, err := c.Hello(ctxFor(t, 2*time.Second), "hello1"); err != nil {
		t.Fatalf("Hello while a hash runs: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Hello took %v while a hash ran", d)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Hash: %v", err)
	}
}

// TestClientRetriesWithinDeadline: transport failures are retried with backoff
// until the call succeeds, and a call that keeps failing ends at its deadline
// rather than past it.
func TestClientRetriesWithinDeadline(t *testing.T) {
	p := &pipeServer{h: &testHandler{broadcast: func(context.Context) (BroadcastReply, error) {
		return BroadcastReply{Replies: []Datagram{{Data: []byte("x"), SrcIP: "10.0.0.2"}}}, nil
	}}, failFirst: 2}
	c := NewClient(p.dial, nil)
	defer c.Close()
	if _, _, err := c.Hello(ctxFor(t, 5*time.Second), "h1"); err != nil {
		t.Fatalf("Hello after two failed dials: %v", err)
	}
	if got := p.dials.Load(); got != 3 {
		t.Errorf("dials = %d, want 3", got)
	}
	// The hello connection is kept: a second hello dials nothing.
	if _, _, err := c.Hello(ctxFor(t, 5*time.Second), "h2"); err != nil {
		t.Fatal(err)
	}
	if got := p.dials.Load(); got != 3 {
		t.Errorf("second hello dialled again (dials = %d)", got)
	}
	p.failFirst = 4
	dgs, err := c.Broadcast(ctxFor(t, 5*time.Second), "b1")
	if err != nil || len(dgs) != 1 || dgs[0].SrcIP != "10.0.0.2" {
		t.Fatalf("Broadcast after a failed dial = %v, %v", dgs, err)
	}

	dead := &pipeServer{h: &testHandler{}, failFirst: 1 << 30}
	dc := NewClient(dead.dial, nil)
	start := time.Now()
	_, _, err = dc.Hello(ctxFor(t, 700*time.Millisecond), "h3")
	if err == nil {
		t.Fatal("Hello to a dead remote succeeded")
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Errorf("Hello retried past its deadline: %v", d)
	}
	if dead.dials.Load() < 2 {
		t.Errorf("Hello did not retry (dials = %d)", dead.dials.Load())
	}

}

// TestRemoteErrorIsNotRetried: a remote's error reply is final.
func TestRemoteErrorIsNotRetried(t *testing.T) {
	p := &pipeServer{h: &testHandler{hash: func(context.Context, HashRequest, func(int64)) (HashReply, error) {
		return HashReply{}, &Error{Code: CodeRefused, Message: "not on the allowlist"}
	}}}
	c := NewClient(p.dial, nil)
	_, err := c.Hash(ctxFor(t, 5*time.Second), "h1", HashRequest{}, HashOptions{Stall: 5 * time.Second})
	if CodeOf(err) != CodeRefused {
		t.Fatalf("Hash = %v, want a refused error", err)
	}
	if got := p.dials.Load(); got != 1 {
		t.Errorf("dials = %d, want 1", got)
	}
}

// TestHashStallRule: a hash keeps going while progress arrives, however long
// it runs, and gives up once none arrives within the caller's stall window.
func TestHashStallRule(t *testing.T) {
	const stall = 300 * time.Millisecond
	steady := &pipeServer{h: &testHandler{hash: func(ctx context.Context, _ HashRequest, progress func(int64)) (HashReply, error) {
		for i := int64(1); i <= 20; i++ {
			progress(i * 1000)
			time.Sleep(50 * time.Millisecond)
		}
		return HashReply{Whole: "w", Size: 20000}, nil
	}}, opts: ServeOptions{ProgressInterval: 20 * time.Millisecond}}
	c := NewClient(steady.dial, nil)
	var reports atomic.Int32
	start := time.Now()
	hr, err := c.Hash(context.Background(), "h1", HashRequest{}, HashOptions{Stall: stall, OnProgress: func(int64) { reports.Add(1) }})
	if err != nil {
		t.Fatalf("a progressing hash failed after %v: %v", time.Since(start), err)
	}
	if time.Since(start) < 3*stall || hr.Whole != "w" || reports.Load() == 0 {
		t.Errorf("hash returned %+v after %v with %d progress reports", hr, time.Since(start), reports.Load())
	}

	stuck := &pipeServer{h: &testHandler{hash: func(ctx context.Context, _ HashRequest, progress func(int64)) (HashReply, error) {
		progress(1)
		<-ctx.Done()
		return HashReply{}, ctx.Err()
	}}, opts: ServeOptions{ProgressInterval: 20 * time.Millisecond}}
	c = NewClient(stuck.dial, nil)
	start = time.Now()
	_, err = c.Hash(context.Background(), "h2", HashRequest{}, HashOptions{Stall: stall})
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("a stalled hash returned %v, want ErrStalled", err)
	}
	if d := time.Since(start); d < stall || d > 3*stall {
		t.Errorf("stalled hash gave up after %v, want about %v", d, stall)
	}

	if _, err := c.Hash(context.Background(), "h3", HashRequest{}, HashOptions{}); err == nil {
		t.Error("Hash without a stall window succeeded")
	}
}

// TestHashCancelStopsRemoteRead: cancelling the call closes its connection,
// which cancels the handler's context.
func TestHashCancelStopsRemoteRead(t *testing.T) {
	stopped := make(chan struct{})
	reading := make(chan struct{})
	p := &pipeServer{h: &testHandler{hash: func(ctx context.Context, _ HashRequest, progress func(int64)) (HashReply, error) {
		close(reading)
		<-ctx.Done()
		close(stopped)
		return HashReply{}, ctx.Err()
	}}}
	c := NewClient(p.dial, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Hash(ctx, "h1", HashRequest{}, HashOptions{Stall: 10 * time.Second})
		done <- err
	}()
	<-reading
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Hash returned %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the remote's hash kept running after the call was cancelled")
	}
	if got := p.dials.Load(); got != 1 {
		t.Errorf("a cancelled hash was retried (dials = %d)", got)
	}
}

// TestServerRejectsMalformedRequests: a request without a valid call id, in a
// version the remote does not speak, or for an unknown op gets an error frame;
// the connection survives each.
func TestServerRejectsMalformedRequests(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go Serve(server, &testHandler{}, ServeOptions{})
	client.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(client)
	for _, c := range []struct {
		req  string
		code ErrorCode
	}{
		{`not json`, CodeBadRequest},
		{`{"v":1,"op":"hello"}`, CodeBadRequest},
		{`{"v":1,"call_id":"bad id\n","op":"hello"}`, CodeBadRequest},
		{`{"v":1,"call_id":"` + strings.Repeat("x", 65) + `","op":"hello"}`, CodeBadRequest},
		{`{"v":2,"call_id":"c1","op":"hello"}`, CodeUnsupported},
		{`{"v":1,"call_id":"c2","op":"reboot"}`, CodeUnsupported},
		{`{"v":1,"call_id":"c3","op":"hash"}`, CodeBadRequest},
	} {
		if _, err := client.Write([]byte(c.req + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := readFrame(br, maxReplyFrame)
		if err != nil {
			t.Fatalf("%s: %v", c.req, err)
		}
		if !bytes.Contains(line, []byte(`"code":"`+string(c.code)+`"`)) {
			t.Errorf("%s: reply %s, want code %s", c.req, line, c.code)
		}
	}
	// Still serving.
	client.Write([]byte(`{"v":1,"call_id":"ok","op":"hello"}` + "\n"))
	line, err := readFrame(br, maxReplyFrame)
	if err != nil || !bytes.Contains(line, []byte(`"kind":"result"`)) {
		t.Fatalf("hello after malformed requests: %s, %v", line, err)
	}
}

// TestVersionRule: a client writes V1 until a hello tells it the remote's
// versions, and then the newest version both sides speak, never one only the
// remote does.
func TestVersionRule(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	dial := func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			br := bufio.NewReader(server)
			for {
				line, err := readFrame(br, maxRequestFrame)
				if err != nil {
					return
				}
				mu.Lock()
				seen = append(seen, string(line))
				mu.Unlock()
				var req request
				json.Unmarshal(line, &req)
				fmt.Fprintf(server, `{"v":1,"call_id":%q,"kind":"result","hello":{"versions":[1,99]}}`+"\n", req.CallID)
			}
		}()
		return client, nil
	}
	c := NewClient(dial, nil)
	defer c.Close()
	for i := range 2 {
		hr, _, err := c.Hello(ctxFor(t, 5*time.Second), fmt.Sprintf("h%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if len(hr.Versions) != 2 {
			t.Fatalf("versions = %v", hr.Versions)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range seen {
		if !strings.Contains(s, `"v":1,`) {
			t.Errorf("client wrote %s, want version 1", s)
		}
	}
}

// TestHashRequestNeverFormatsCredentials: no formatting of a hash request, its
// credentials or a log line holding them shows a credential or argument.
func TestHashRequestNeverFormatsCredentials(t *testing.T) {
	req := HashRequest{IP: "10.9.8.7", Share: "EMMC Images", Path: "Light/a.fit", User: "operator-name", Password: "hunter2-secret"}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Info("x", "req", req, "user", req.User, "password", req.Password)
	log.Info("y", "req", &req)
	text := buf.String() + fmt.Sprintf("%v %+v %#v %s %v %+v %#v", req, req, req, req, &req, req.Password, req.Password)
	for _, secret := range []string{"10.9.8.7", "EMMC Images", "Light/a.fit", "operator-name", "hunter2-secret"} {
		if strings.Contains(text, secret) {
			t.Errorf("formatted hash request shows %q: %s", secret, text)
		}
	}
}

// TestNetsiteWireForm pins the netsite types the protocol carries as its own
// wire form: Datagram's JSON field set and encoding, and Credential as a plain
// string inside HashRequest. A field added to or renamed on either, or a
// marshaller added to Credential, fails here; such a change is a protocol
// change and needs a new protocol version.
func TestNetsiteWireForm(t *testing.T) {
	var fields []string
	dt := reflect.TypeFor[netsite.Datagram]()
	for i := range dt.NumField() {
		f := dt.Field(i)
		fields = append(fields, f.Name+":"+f.Type.String()+":"+f.Tag.Get("json"))
	}
	if got, want := strings.Join(fields, ","), "Data:[]uint8:data,SrcIP:string:src_ip"; got != want {
		t.Fatalf("netsite.Datagram fields = %s, want %s; its JSON is the protocol's wire form", got, want)
	}
	raw, err := json.Marshal(BroadcastReply{Replies: []Datagram{{Data: []byte{1, 2}, SrcIP: "192.168.1.5"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"replies":[{"data":"AQI=","src_ip":"192.168.1.5"}]}`; got != want {
		t.Fatalf("broadcast reply wire form = %s, want %s", got, want)
	}

	raw, err = json.Marshal(HashRequest{IP: "192.168.1.5", Share: "EMMC Images", Path: "a.fit", User: "guest", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), `{"ip":"192.168.1.5","share":"EMMC Images","path":"a.fit","user":"guest","password":"pw"}`; got != want {
		t.Fatalf("hash request wire form = %s, want %s", got, want)
	}
}
