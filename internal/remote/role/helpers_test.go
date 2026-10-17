package role

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// pairingEnv mints a pairing on the fake relay region and returns its env
// value, as Commander would show it.
func pairingEnv(t *testing.T) string {
	t.Helper()
	p, err := tunnel.Mint(tunnel.FakeRegion())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return p.EnvValue.Reveal()
}

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// lan is the remote's LAN for tests: a scriptable local site (the broadcast
// responder and the probe's answer) and the device the hash opens.
type lan struct {
	site *netsite.Fake
	dev  *device.Fake

	mu     sync.Mutex
	events []string // "open <ip>", "probe <ip>:4720", "broadcast", in order
	// slow, when set, delays every read of an opened file.
	slow  time.Duration
	reads int
}

func newLAN() *lan {
	l := &lan{site: netsite.NewFake(netsite.Local), dev: device.NewFake()}
	l.site.SetRepliesMatchSource(false)
	l.site.OnCall(func(call string) { l.event(call) })
	return l
}

func (l *lan) event(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *lan) Events() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *lan) Reads() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

func (l *lan) setSlow(d time.Duration) {
	l.mu.Lock()
	l.slow = d
	l.mu.Unlock()
}

func (l *lan) Site(netsite.ID) netsite.Site { return l.site }
func (l *lan) All() []netsite.Site          { return []netsite.Site{l.site} }

// newDevice is the role's device seam: it records the endpoint opened and
// wraps the fake so reads can be slowed and counted.
func (l *lan) newDevice(ep netsite.Endpoint, _, _ string, _ time.Duration) device.Device {
	return lanDevice{l: l, ep: ep}
}

type lanDevice struct {
	l  *lan
	ep netsite.Endpoint
}

func (d lanDevice) Probe(ctx context.Context) bool { return d.l.dev.Probe(ctx) }

func (d lanDevice) Open(ctx context.Context) (device.Session, error) {
	d.l.event("open " + string(d.ep.Site) + "/" + d.ep.IP)
	s, err := d.l.dev.Open(ctx)
	if err != nil {
		return nil, err
	}
	return lanSession{Session: s, l: d.l}, nil
}

type lanSession struct {
	device.Session
	l *lan
}

func (s lanSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return lanShare{Share: sh, l: s.l}, nil
}

type lanShare struct {
	device.Share
	l *lan
}

func (s lanShare) OpenReader(path string) (io.ReadCloser, error) {
	rc, err := s.Share.OpenReader(path)
	if err != nil {
		return nil, err
	}
	return &lanReader{ReadCloser: rc, l: s.l}, nil
}

// lanReader hands out at most 64 KiB per read, so a slowed read spans many
// calls.
type lanReader struct {
	io.ReadCloser
	l *lan
}

func (r *lanReader) Read(p []byte) (int, error) {
	r.l.mu.Lock()
	r.l.reads++
	slow := r.l.slow
	r.l.mu.Unlock()
	if slow > 0 {
		time.Sleep(slow)
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	return r.ReadCloser.Read(p)
}

// scanReply is a scan_air reply as an ASIAIR sends it; an empty ip omits
// result.ip.
func scanReply(guid, cpuID, ip string) []byte {
	ipField := ""
	if ip != "" {
		ipField = fmt.Sprintf(`,"ip":%q`, ip)
	}
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"scan_air","result":{"guid":%q,"cpuId":%q%s,"model":"ZWO AirPlus"},"code":0,"id":"132"}`, guid, cpuID, ipField))
}

// fakeClock is an injectable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// running is a remote started on the fake tunnel, with a Commander-side
// protocol client connected to it.
type running struct {
	r      *Remote
	lan    *lan
	fake   *tunnel.Fake
	tc     tunnel.Client
	client *proto.Client
	logs   *syncBuffer
	done   chan error
	cancel context.CancelFunc
}

type startOpts struct {
	cfg  Config // seams to override; Getenv, Sites, NewDevice and StartServer default to the test ones
	wrap func(tunnel.Server) tunnel.Server
}

func startRemote(t *testing.T, o startOpts) *running {
	t.Helper()
	rr := &running{lan: newLAN(), logs: &syncBuffer{}, done: make(chan error, 1)}
	cfg := o.cfg
	if cfg.Getenv == nil {
		cfg.Getenv = envOf(map[string]string{EnvVar: pairingEnv(t)})
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(rr.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if cfg.Sites == nil {
		cfg.Sites = rr.lan
	}
	if cfg.NewDevice == nil {
		cfg.NewDevice = rr.lan.newDevice
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = "127.0.0.1:0"
	}
	fakeReady := make(chan struct{})
	cfg.StartServer = func(sc tunnel.ServerConfig) (tunnel.Server, error) {
		f, err := tunnel.NewFake(sc)
		if err != nil {
			return nil, err
		}
		rr.fake = f
		close(fakeReady)
		if o.wrap != nil {
			return o.wrap(f), nil
		}
		return f, nil
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rr.r = r
	ctx, cancel := context.WithCancel(context.Background())
	rr.cancel = cancel
	go func() { rr.done <- r.Run(ctx) }()
	select {
	case <-fakeReady:
	case err := <-rr.done:
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel server never started")
	}
	for r.currentServer() == nil {
		time.Sleep(time.Millisecond)
	}
	rr.tc = rr.fake.NewClient()
	rr.client = proto.NewClient(rr.tc.DialProtocol, nil)
	t.Cleanup(func() {
		rr.client.Close()
		rr.stop(t)
	})
	return rr
}

// stop ends Run and returns its error; repeated calls return nil.
func (rr *running) stop(t *testing.T) error {
	t.Helper()
	if rr.cancel == nil {
		return nil
	}
	rr.cancel()
	rr.cancel = nil
	select {
	case err := <-rr.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
		return nil
	}
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// listen returns a loopback TCP listener address that accepts and holds
// connections, standing in for a device port behind the remote.
func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}
