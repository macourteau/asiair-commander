package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
	"github.com/macourteau/asiair-commander/internal/store"
)

// lanDevice is the ASIAIR on every fake remote's LAN.
const lanDevice = "192.168.1.50"

// clock is a settable clock for the supervisor's timekeeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// handler is a fake remote's side of the remote protocol: a scriptable hello,
// and a broadcast that fills the remote's forwarding allowlist with the LAN
// device, as the real remote's does.
type handler struct {
	mu         sync.Mutex
	instance   string
	whole      int
	data       int
	build      proto.Build
	warnings   []proto.Warning
	allow      map[netip.Addr]bool
	broadcasts int
	// gate, when set, holds every broadcast until it is closed; entered is
	// signalled as each one starts.
	gate    chan struct{}
	entered chan struct{}
	// hash, when set, is the remote's hash; otherwise it refuses.
	hash func(ctx context.Context, req proto.HashRequest, progress func(int64)) (proto.HashReply, error)
}

func newHandler(instance string) *handler {
	return &handler{
		instance: instance, whole: header.WholeDigestVersion, data: header.DataDigestVersion,
		build: proto.Build{Version: "v1.0.0", Date: "2026-10-01T00:00:00Z"},
		allow: map[netip.Addr]bool{},
	}
}

func (h *handler) Hello(context.Context, string) proto.HelloReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	return proto.HelloReply{
		WholeDigestVersion: h.whole, DataDigestVersion: h.data, Build: h.build,
		UptimeMS: 60_000, InstanceID: h.instance,
		Path:     proto.Path{Connected: true, Direct: true, RelayRegion: "fake"},
		Warnings: append([]proto.Warning(nil), h.warnings...),
	}
}

func (h *handler) Broadcast(ctx context.Context, _ string) (proto.BroadcastReply, error) {
	h.mu.Lock()
	h.broadcasts++
	gate, entered := h.gate, h.entered
	h.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return proto.BroadcastReply{}, ctx.Err()
		}
	}
	h.mu.Lock()
	h.allow[netip.MustParseAddr(lanDevice)] = true
	h.mu.Unlock()
	return proto.BroadcastReply{Replies: []proto.Datagram{{Data: []byte(`{"id":1}`), SrcIP: lanDevice}}}, nil
}

func (h *handler) Hash(ctx context.Context, _ string, req proto.HashRequest, progress func(int64)) (proto.HashReply, error) {
	h.mu.Lock()
	hash := h.hash
	h.mu.Unlock()
	if hash == nil {
		return proto.HashReply{}, &proto.Error{Code: proto.CodeFailed, Message: "not in these tests"}
	}
	return hash(ctx, req, progress)
}

// setHash scripts the remote's hash.
func (h *handler) setHash(hash func(ctx context.Context, req proto.HashRequest, progress func(int64)) (proto.HashReply, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hash = hash
}

// setDigestVersions sets the digest-algorithm versions the next hello reports.
func (h *handler) setDigestVersions(whole, data int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.whole, h.data = whole, data
}

func (h *handler) policy(_ tunnel.Proto, ip netip.Addr, _ uint16) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allow[ip]
}

func (h *handler) broadcastCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.broadcasts
}

// fakeRemote is one remote process on its PC, started from the env value the
// user pasted. Each start is a new process: a new tunnel.Fake, a new instance
// id and an empty allowlist; the clients of the previous one stay dead, as a
// real tailcat client does after its server restarts.
type fakeRemote struct {
	t   *testing.T
	env *tunnel.Env

	mu      sync.Mutex
	srv     *tunnel.Fake
	h       *handler
	starts  int
	lanEnds chan net.Conn // the device's end of each forwarded TCP flow
	// serve, when set, answers the remote protocol in place of the scripted
	// handler, which still holds the forwarding allowlist.
	serve proto.Handler
}

func (f *fakeRemote) start() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv != nil {
		f.srv.Close()
	}
	f.starts++
	h := newHandler("inst-" + string(rune('a'+f.starts-1)))
	var serve proto.Handler = h
	if f.serve != nil {
		serve = f.serve
	}
	srv, err := tunnel.NewFake(tunnel.ServerConfig{
		Env:      f.env,
		Policy:   h.policy,
		Protocol: func(c net.Conn) { proto.Serve(c, serve, proto.ServeOptions{ProgressInterval: 10 * time.Millisecond}) },
		DialLAN: func(_ context.Context, network, _ string) (net.Conn, error) {
			if network != "tcp" {
				return nil, errors.New("only TCP in these tests")
			}
			a, b := net.Pipe()
			f.lanEnds <- b
			return a, nil
		},
	})
	if err != nil {
		f.t.Fatalf("NewFake: %v", err)
	}
	f.srv, f.h = srv, h
}

// stop ends the remote process: every connection through it fails.
func (f *fakeRemote) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv != nil {
		f.srv.Close()
		f.srv = nil
	}
}

func (f *fakeRemote) handler() *handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h
}

// client is what a tunnel client built now reaches: the running process, or
// nothing.
func (f *fakeRemote) client() tunnel.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.srv == nil {
		return deadClient{}
	}
	return &flakyClient{Client: f.srv.NewClient()}
}

// flakyClient lets a test fail the remote protocol alone while forwarded
// connections keep moving bytes, as when the remote's protocol listener
// wedges or its hello is lost.
type flakyClient struct {
	tunnel.Client
	protoDown atomic.Bool
	mu        sync.Mutex
	protos    []net.Conn
}

func (c *flakyClient) DialProtocol(ctx context.Context) (net.Conn, error) {
	if c.protoDown.Load() {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("protocol down")}
	}
	conn, err := c.Client.DialProtocol(ctx)
	if err == nil {
		c.mu.Lock()
		c.protos = append(c.protos, conn)
		c.mu.Unlock()
	}
	return conn, err
}

func (c *flakyClient) failProtocol() {
	c.protoDown.Store(true)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.protos {
		conn.Close()
	}
}

// deadClient is a client whose remote is not running, or holds another
// pairing: every dial fails as a network error.
type deadClient struct{}

func (deadClient) dead() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no remote answers")}
}
func (d deadClient) DialTCP(context.Context, netip.Addr, uint16) (net.Conn, error) {
	return nil, d.dead()
}
func (d deadClient) DialUDP(context.Context, netip.Addr, uint16) (net.Conn, error) {
	return nil, d.dead()
}
func (d deadClient) DialProtocol(context.Context) (net.Conn, error) { return nil, d.dead() }
func (deadClient) Close() error                                     { return nil }

// RelayConnected is true: the remote is not answering, but Commander reaches
// the relay.
func (deadClient) RelayConnected() bool { return true }

// fakeRegions offers only the fake region.
type fakeRegions struct{}

func (fakeRegions) Nearest(context.Context) (tunnel.Region, error) { return tunnel.FakeRegion(), nil }
func (fakeRegions) All(context.Context) ([]tunnel.Region, error) {
	return []tunnel.Region{tunnel.FakeRegion()}, nil
}

type recordingHub struct {
	mu      sync.Mutex
	last    []model.RemoteStatus
	relay   *model.RelayStatus
	unacked int
}

func (h *recordingHub) SetRelay(r *model.RelayStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.relay = r
}

func (h *recordingHub) SetUnackedAlerts(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unacked = n
}

func (h *recordingHub) SetRemotes(rs []model.RemoteStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = rs
}

type recordingDiscovery struct {
	mu      sync.Mutex
	dropped []netsite.ID
}

func (d *recordingDiscovery) DropSite(id netsite.ID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped = append(d.dropped, id)
}

// syncBuffer is a log sink safe for the heartbeat goroutines.
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

// harness is a Supervisor over fake remotes, a real store and a config file.
// Heartbeats are driven by the test (Start is not called) so every step is
// deterministic; the clock is the test's.
type harness struct {
	t     *testing.T
	clock *clock
	sites *netsite.Registry
	st    store.Store
	cfg   *atomic.Pointer[model.Config]
	path  string
	hub   *recordingHub
	// fleet is the status hub the rigs publish to and the remote alerts are
	// raised through.
	fleet   *hub.StatusHub
	disc    *recordingDiscovery
	logs    *syncBuffer
	sup     *Supervisor
	guard   *config.Guard
	remotes map[string]*fakeRemote // by pairing fingerprint
	// onBuild, when set, runs inside every client build, after the stored
	// pairing was read and before it is parsed, so a build that fails counts.
	onBuild func()
	// forgets records every ForgetEndpoints call, under mu.
	forgets []forgetCall
	mu      sync.Mutex
}

// forgetCall is one call of the supervisor's ForgetEndpoints hook.
type forgetCall struct {
	rigIDs []string
	site   netsite.ID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(config.Defaults())
	h := &harness{
		t: t, clock: newClock(), st: st, cfg: ptr, path: filepath.Join(dir, "config.yaml"),
		hub: &recordingHub{}, fleet: hub.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		disc: &recordingDiscovery{}, logs: &syncBuffer{},
		guard: &config.Guard{}, remotes: map[string]*fakeRemote{},
	}
	h.sup = h.newSupervisor()
	return h
}

// newSupervisor builds a supervisor over the harness's store and config, as a
// Commander (re)start does, with a fresh site registry.
func (h *harness) newSupervisor() *Supervisor {
	h.sites = netsite.NewRegistry(slog.New(slog.NewTextHandler(h.logs, nil)))
	sup, err := New(Options{
		Sites: h.sites, Discovery: h.disc, Store: h.st, Hub: h.hub,
		Notify: notify.New(h.st, h.fleet, nil, slog.New(slog.NewTextHandler(io.Discard, nil))),
		Alerts: h.st, Rigs: h.fleet,
		Config: h.cfg, Guard: h.guard, ConfigPath: h.path,
		Build:   proto.Build{Version: "v1.0.0", Date: "2026-10-01T00:00:00Z"},
		Logger:  slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Regions: fakeRegions{}, Now: h.clock.now,
		HelloTimeout: 300 * time.Millisecond,
		ForgetEndpoints: func(_ context.Context, rigIDs []string, site netsite.ID) (int, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.forgets = append(h.forgets, forgetCall{rigIDs: append([]string(nil), rigIDs...), site: site})
			return 1, nil
		},
		NewClient: func(p model.RemoteProfile, _ *slog.Logger) (tunnel.Client, error) {
			h.mu.Lock()
			hook := h.onBuild
			h.mu.Unlock()
			if hook != nil {
				hook()
			}
			fp, err := tunnel.Fingerprint(tunnel.Secret(p.Address), tunnel.Secret(p.ClientKey))
			if err != nil {
				return nil, err
			}
			h.mu.Lock()
			f := h.remotes[fp]
			h.mu.Unlock()
			if f == nil {
				return deadClient{}, nil
			}
			return f.client(), nil
		},
	})
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	h.t.Cleanup(sup.Stop)
	return sup
}

// add pairs a remote and returns the fake remote PC its env value is for, not
// yet started.
func (h *harness) add(name string) (string, *fakeRemote) {
	h.t.Helper()
	p, err := h.sup.Add(context.Background(), name, 0)
	if err != nil {
		h.t.Fatalf("Add: %v", err)
	}
	if want := tunnel.EnvFile(tunnel.Secret(p.EnvValue)).Reveal(); p.EnvFile != want {
		h.t.Fatal("Add: EnvFile is not the .env line for its env value")
	}
	return p.Remote.ID, h.install(p)
}

// install is the user putting a pairing's env value on the remote PC.
func (h *harness) install(p model.RemotePairing) *fakeRemote {
	h.t.Helper()
	env, err := tunnel.ParseEnv(p.EnvValue)
	if err != nil {
		h.t.Fatalf("ParseEnv: %v", err)
	}
	f := &fakeRemote{t: h.t, env: env, lanEnds: make(chan net.Conn, 16)}
	h.mu.Lock()
	h.remotes[env.Fingerprint()] = f
	h.mu.Unlock()
	h.t.Cleanup(f.stop)
	return f
}

func (h *harness) remote(id string) *remote {
	h.sup.mu.Lock()
	defer h.sup.mu.Unlock()
	r := h.sup.remotes[id]
	if r == nil {
		h.t.Fatalf("remote %s is not supervised", id)
	}
	return r
}

func (h *harness) beat(id string) { h.remote(id).heartbeat(context.Background()) }

func (h *harness) status(id string) model.RemoteStatus {
	h.t.Helper()
	for _, s := range h.sup.List() {
		if s.ID == id {
			return s
		}
	}
	h.t.Fatalf("no status for %s", id)
	return model.RemoteStatus{}
}

func (h *harness) wantState(id string, want model.RemoteState) model.RemoteStatus {
	h.t.Helper()
	st := h.status(id)
	if st.State != want {
		h.t.Fatalf("remote %s state %q, want %q", id, st.State, want)
	}
	return st
}

// connect starts f if needed and beats until the remote is connected; the
// fake clock moves past the rebuild window so a stale client is replaced.
func (h *harness) connect(id string, f *fakeRemote) {
	h.t.Helper()
	if f.handler() == nil {
		f.start()
	}
	for i := 0; i < 3; i++ {
		h.beat(id)
		if h.status(id).State == model.RemoteConnected {
			return
		}
		h.clock.advance(netsite.RemoteStallWindow + time.Second)
	}
	h.t.Fatalf("remote %s did not connect: %+v", id, h.status(id))
}

func (h *harness) clientsBuilt(id string) int {
	h.t.Helper()
	r := h.remote(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rebuilds
}

func countLines(logs, substr string) int {
	n := 0
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}
