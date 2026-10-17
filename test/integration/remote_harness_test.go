//go:build integration && remotetest

// The real-tunnel harness: a real tailcat tunnel through a local DERP and STUN
// relay (never public DERP), the real remote role, the real remote supervisor
// and its site, and the Samba NT1 container standing in for an ASIAIR on the
// remote's LAN. It needs the remotetest build tag, the only builds whose
// tunnel admits a test relay; run.sh builds the integration tests with it.
//
// The remote's LAN is mapped, not real. The container answers at a TEST-NET-1
// address (remoteDevIP) that the remote's LAN dialler maps onto the
// container's SMB port, so the remote's allowlist holds the container under
// that address, and no SMB byte from Commander can reach the container except
// through the tunnel: the address routes nowhere else. A scan_air responder
// per mapped device answers the remote's broadcast round, its own identity
// probe, and Commander's probe forwarded through the tunnel as a UDP flow,
// because the Samba container speaks SMB only.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/role"
	"github.com/macourteau/asiair-commander/internal/remote/supervisor"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel/tunneltest"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
	dto "github.com/prometheus/client_model/go"
)

// TestMain keeps every tunnel in this binary off the real network.
func TestMain(m *testing.M) {
	tunneltest.Hermetic()
	os.Exit(m.Run())
}

const (
	remoteID = "r1"
	// remoteDevIP is the container on the remote's LAN; remoteDeadIP is an
	// ASIAIR that answered the remote's broadcast once and was then powered
	// off. Both are TEST-NET-1 addresses, which route nowhere outside the
	// remote's mapped LAN.
	remoteDevIP  = "192.0.2.10"
	remoteDeadIP = "192.0.2.11"
	remoteGUID   = "guid-remote"
	remoteCPUID  = "cpu-remote"
	smbUser      = "guest"
	smbPassword  = "12345678"

	// commanderSMBBudget replaces the remote site's 60 s SMB budget in the
	// device factory the tests hand Commander's workers, so a transfer cut by
	// a drop fails in seconds rather than a minute. It changes how long a stall
	// is waited out, not what a stall means.
	commanderSMBBudget = 8 * time.Second

	// clockRate speeds up the supervisor's clock (and only the supervisor's),
	// the test seam that shortens its rebuild window: the rule waits for the
	// remote site's SMB budget (60 s) of tunnel silence, which at this rate is
	// two real seconds. Heartbeats, hello timeouts and every network deadline
	// run in real time.
	clockRate = 30

	// exitCheckInterval is how often the remote's exit rule looks at its
	// relay connection in these tests.
	exitCheckInterval = 200 * time.Millisecond
)

// remoteEP is the container as Commander reaches it: through the remote.
var remoteEP = netsite.Endpoint{Site: remoteID, IP: remoteDevIP}

// logSink collects every log line of a test and prints them only if the test
// fails. Background goroutines (tailcat, the supervisor) keep logging after a
// test returns, which t.Log would turn into a panic.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// newLogSink returns a logger for one test. It is registered before anything
// else the test cleans up, so it prints last, after everything has stopped.
func newLogSink(t *testing.T) *slog.Logger {
	t.Helper()
	sink := &logSink{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", sink.String())
		}
	})
	return slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// fastClock runs clockRate times faster than the wall clock from its start.
type fastClock struct{ start time.Time }

func (c fastClock) Now() time.Time { return c.start.Add(time.Since(c.start) * clockRate) }

// memMeta is the supervisor's connection-history store.
type memMeta struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memMeta) GetMeta(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memMeta) SetMeta(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

// lanDevice is one device on the remote's LAN: a scan_air responder, and for
// remoteDevIP the container's SMB port.
type lanDevice struct {
	ip    string
	reply []byte
	udp   *net.UDPConn
	// on is false once the device is powered off: it answers nothing.
	on atomic.Bool
}

func scanAirReply(guid, cpuID string) []byte {
	return []byte(fmt.Sprintf(`{"id":"132","method":"scan_air","result":{"guid":%q,"cpuId":%q,"model":"ASIAIR Plus"},"code":0}`, guid, cpuID))
}

func startLANDevice(t *testing.T, ip, guid, cpuID string) *lanDevice {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	d := &lanDevice{ip: ip, reply: scanAirReply(guid, cpuID), udp: pc}
	d.on.Store(true)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if d.on.Load() && bytes.Contains(buf[:n], []byte(`"scan_air"`)) {
				pc.WriteToUDP(d.reply, from)
			}
		}
	}()
	return d
}

// ask sends one scan_air request to the device's responder and returns its
// reply, or false when it answers nothing within wait.
func (d *lanDevice) ask(ctx context.Context, req []byte, wait time.Duration) ([]byte, bool) {
	c, err := net.DialUDP("udp4", nil, d.udp.LocalAddr().(*net.UDPAddr))
	if err != nil {
		return nil, false
	}
	defer c.Close()
	deadline := time.Now().Add(wait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	c.SetDeadline(deadline)
	if _, err := c.Write(req); err != nil {
		return nil, false
	}
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		return nil, false
	}
	return buf[:n], true
}

// remoteLAN is the remote's LAN: the mapped devices, and the cut that the drop
// tests place on the container's SMB bytes as they enter the tunnel.
type remoteLAN struct {
	smbAddr string // the container's SMB port
	devs    map[string]*lanDevice

	mu      sync.Mutex
	smbFwd  int // forwarded SMB flows dialled
	cutAt   int64
	cutFn   func()
	cutDone chan struct{}
	sent    int64 // bytes forwarded toward Commander since the cut was armed
}

// dial is a dial on the remote's LAN. A powered-off device never answers, so
// its dial runs until ctx ends, as an unanswered SYN does.
func (l *remoteLAN) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	d, ok := l.devs[host]
	switch {
	case ok && !d.on.Load():
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
	case ok && network == "tcp" && port == "445" && host == remoteDevIP:
		var nd net.Dialer
		return nd.DialContext(ctx, "tcp", l.smbAddr)
	case ok && network == "udp" && port == "4720":
		var nd net.Dialer
		return nd.DialContext(ctx, "udp4", d.udp.LocalAddr().String())
	}
	return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
}

// forward is the tunnel's dial of a forwarded flow's target. A forwarded SMB
// flow carries the cut.
func (l *remoteLAN) forward(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := l.dial(ctx, network, address)
	if err != nil || network != "tcp" || !strings.HasSuffix(address, ":445") {
		return c, err
	}
	l.mu.Lock()
	l.smbFwd++
	l.mu.Unlock()
	return &cutConn{Conn: c, lan: l}, nil
}

func (l *remoteLAN) smbForwards() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.smbFwd
}

// armCut places a cut after n more bytes of the container's SMB replies have
// entered the tunnel: the read that crosses n is held back, cut runs (it takes
// the tunnel down), and the held bytes are discarded. The returned channel is
// closed once cut has returned.
func (l *remoteLAN) armCut(n int64, cut func()) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cutAt, l.cutFn, l.sent = n, cut, 0
	l.cutDone = make(chan struct{})
	return l.cutDone
}

// passed counts n bytes toward the armed cut and reports whether they cross
// it; the cut fires once.
func (l *remoteLAN) passed(n int) (cut func(), done chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cutFn == nil {
		return nil, nil
	}
	l.sent += int64(n)
	if l.sent < l.cutAt {
		return nil, nil
	}
	cut, done = l.cutFn, l.cutDone
	l.cutFn = nil
	return cut, done
}

// cutConn is a forwarded SMB flow's LAN side.
type cutConn struct {
	net.Conn
	lan *remoteLAN
}

func (c *cutConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		if cut, done := c.lan.passed(n); cut != nil {
			// These bytes never reach the tunnel: the cut lands before them.
			// tailcat's Close does not wait for its forwarding goroutines, so
			// running the cut here, on one of them, cannot deadlock.
			cut()
			close(done)
			c.Conn.Close()
			return 0, net.ErrClosed
		}
	}
	return n, err
}

// remoteLANSite is the remote's local site over the mapped LAN: its broadcast
// round asks every responder, its probe asks the device's, and its dials go to
// the mapped LAN.
type remoteLANSite struct {
	netsite.Site
	lan *remoteLAN
}

func (s *remoteLANSite) Broadcast(ctx context.Context) ([]netsite.Datagram, error) {
	var out []netsite.Datagram
	for _, d := range s.lan.devs {
		if reply, ok := d.ask(ctx, netsite.ScanAirRequest, time.Second); ok {
			out = append(out, netsite.Datagram{Data: reply, SrcIP: d.ip})
		}
	}
	return out, nil
}

func (s *remoteLANSite) ProbeUDP(ctx context.Context, host string, _ int, req []byte, timeout time.Duration, accept func([]byte, string) bool) bool {
	d, ok := s.lan.devs[host]
	if !ok {
		return false
	}
	reply, ok := d.ask(ctx, req, timeout)
	return ok && accept(reply, host)
}

func (s *remoteLANSite) DialTCP(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return s.lan.dial(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
}

type remoteLANSites struct{ site *remoteLANSite }

func (s remoteLANSites) Site(netsite.ID) netsite.Site { return s.site }
func (s remoteLANSites) All() []netsite.Site          { return []netsite.Site{s.site} }

// readTap wraps a device factory so every OpenReader on its shares is counted
// by path, and optionally slowed to a trickle.
type readTap struct {
	slow atomic.Bool

	mu    sync.Mutex
	opens map[string]int
}

func newReadTap() *readTap { return &readTap{opens: map[string]int{}} }

func (r *readTap) count(p string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opens[p]
}

func (r *readTap) wrap(d device.Device) device.Device { return tapDevice{Device: d, tap: r} }

type tapDevice struct {
	device.Device
	tap *readTap
}

func (d tapDevice) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	if err != nil {
		return nil, err
	}
	return tapSession{Session: s, tap: d.tap}, nil
}

type tapSession struct {
	device.Session
	tap *readTap
}

func (s tapSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return tapShare{Share: sh, tap: s.tap}, nil
}

type tapShare struct {
	device.Share
	tap *readTap
}

func (s tapShare) OpenReader(p string) (io.ReadCloser, error) {
	s.tap.mu.Lock()
	s.tap.opens[p]++
	s.tap.mu.Unlock()
	rc, err := s.Share.OpenReader(p)
	if err != nil || !s.tap.slow.Load() {
		return rc, err
	}
	return &slowReader{ReadCloser: rc}, nil
}

// slowReader reads at most slowChunk every slowPause: a 2 MiB file takes
// about 13 seconds, with bytes moving every 50 ms throughout.
type slowReader struct{ io.ReadCloser }

const (
	slowChunk = 8 << 10
	slowPause = 50 * time.Millisecond
)

func (r *slowReader) Read(b []byte) (int, error) {
	time.Sleep(slowPause)
	return r.ReadCloser.Read(b[:min(len(b), slowChunk)])
}

// helloGate is the remote's side of one remote-protocol connection. While
// fail is set it drops a connection the moment a hello arrives on it, so
// Commander's heartbeat fails while every other call (a hash, a broadcast) and
// every forwarded flow keep working.
type helloGate struct {
	net.Conn
	fail *atomic.Bool
}

func (g *helloGate) Read(b []byte) (int, error) {
	n, err := g.Conn.Read(b)
	if n > 0 && g.fail.Load() && bytes.Contains(b[:n], []byte(`"op":"`+string(proto.OpHello)+`"`)) {
		g.Conn.Close()
		return 0, net.ErrClosed
	}
	return n, err
}

// remoteRun is one run of the remote role, as one start of its container.
type remoteRun struct {
	cancel context.CancelFunc
	// exited is closed when Run returns, with its error in err.
	exited chan struct{}
	err    error
	srv    atomic.Pointer[tunnel.Server]
}

func (r *remoteRun) homeConnected() bool {
	s := r.srv.Load()
	return s != nil && (*s).HomeConnected()
}

// remoteOptions tune one harness.
type remoteOptions struct {
	// exitAfter shortens the remote's exit window; zero keeps production's.
	exitAfter time.Duration
	// smbAddr is the SMB server mapped onto remoteDevIP on the remote's LAN;
	// empty maps the container every direct test also uses.
	smbAddr string
}

// remoteHarness is one remote and the Commander side that supervises it.
type remoteHarness struct {
	t       *testing.T
	log     *slog.Logger
	opts    remoteOptions
	relay   *tunneltest.Relay
	pairing *tunnel.Pairing
	lan     *remoteLAN
	dev     *lanDevice // the container
	dead    *lanDevice // the ASIAIR that is powered off
	// remoteTap counts and can slow the remote's own reads (its hash);
	// failHellos makes the remote drop every hello.
	remoteTap  *readTap
	failHellos atomic.Bool

	mu  sync.Mutex
	run *remoteRun

	cfg    *atomic.Pointer[model.Config]
	sites  *netsite.Registry
	sup    *supervisor.Supervisor
	supMx  *metrics.Metrics
	builds atomic.Int32 // tunnel clients the supervisor built
	// direct is a local SMB session to the container, for staging frames and
	// checking the device the way a test would look at it, never through the
	// tunnel.
	direct device.Share
}

// newRemoteHarness starts a relay, the remote role and Commander's
// supervisor, and waits for the remote's site to come up. It skips without
// the container.
func newRemoteHarness(t *testing.T, opts remoteOptions) *remoteHarness {
	t.Helper()
	log := newLogSink(t)
	newDevice(t) // skips without the container
	if opts.smbAddr == "" {
		opts.smbAddr = net.JoinHostPort(host(), "445")
	}

	relay := tunneltest.StartRelay(t)
	p, err := tunnel.Mint(relay.Region())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	h := &remoteHarness{
		t: t, log: log, opts: opts, relay: relay, pairing: p,
		remoteTap: newReadTap(),
		dev:       startLANDevice(t, remoteDevIP, remoteGUID, remoteCPUID),
		dead:      startLANDevice(t, remoteDeadIP, "guid-dead", "cpu-dead"),
	}
	h.lan = &remoteLAN{
		smbAddr: opts.smbAddr,
		devs:    map[string]*lanDevice{remoteDevIP: h.dev, remoteDeadIP: h.dead},
	}
	h.direct = h.openDirect()
	h.startRemote()
	t.Cleanup(h.stopRemote)

	cfg := config.Defaults()
	cfg.Remotes = []model.RemoteProfile{{
		ID: remoteID, Name: "Integration remote",
		Address: p.Address.Reveal(), ClientKey: p.ClientKey.Reveal(),
		Region: relay.Region().Code, CreatedAt: time.Now().Unix(),
	}}
	h.cfg = &atomic.Pointer[model.Config]{}
	h.cfg.Store(cfg)
	h.sites = netsite.NewRegistry(log)
	h.supMx = metrics.New("integration-remote")
	h.sup, err = supervisor.New(supervisor.Options{
		Sites:   h.sites,
		Store:   &memMeta{m: map[string]string{}},
		Config:  h.cfg,
		Metrics: h.supMx,
		Logger:  log.With("side", "commander"),
		// The harness never removes its remote and keeps no rig endpoints;
		// the cleanup itself is rig.TestForgetLastEndpointsOnSite's.
		ForgetEndpoints: func(context.Context, []string, netsite.ID) (int, error) { return 0, nil },
		NewClient: func(p model.RemoteProfile, log *slog.Logger) (tunnel.Client, error) {
			h.builds.Add(1)
			return tunnel.NewClient(tunnel.ClientConfig{Address: tunnel.Secret(p.Address), ClientKey: tunnel.Secret(p.ClientKey), Logger: log})
		},
		Now:               fastClock{start: time.Now()}.Now,
		HeartbeatInterval: 300 * time.Millisecond,
		HelloTimeout:      2 * time.Second,
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	h.sup.Start(context.Background())
	t.Cleanup(h.sup.Stop)
	h.waitSiteUp("the remote's site to come up")
	return h
}

// startRemote starts one run of the remote role with the harness's pairing,
// as its container starting (or a restart policy restarting it) would.
func (h *remoteHarness) startRemote() *remoteRun {
	h.t.Helper()
	env := map[string]string{role.EnvVar: h.pairing.EnvValue.Reveal()}
	run := &remoteRun{exited: make(chan struct{})}
	lanSites := remoteLANSites{site: &remoteLANSite{Site: netsite.NewRegistry(h.log).Site(netsite.Local), lan: h.lan}}
	factory := device.NewFactory(lanSites)
	r, err := role.New(role.Config{
		Build:   proto.Build{Version: "integration"},
		Getenv:  func(k string) string { return env[k] },
		Logger:  h.log.With("side", "remote"),
		Sites:   lanSites,
		DialLAN: h.lan.forward,
		NewDevice: func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
			return h.remoteTap.wrap(factory(ep, user, pass, timeout))
		},
		StartServer: func(cfg tunnel.ServerConfig) (tunnel.Server, error) {
			serve := cfg.Protocol
			cfg.Protocol = func(c net.Conn) { serve(&helloGate{Conn: c, fail: &h.failHellos}) }
			srv, err := tunnel.StartServer(cfg)
			if err == nil {
				run.srv.Store(&srv)
			}
			return srv, err
		},
		ProgressInterval:  100 * time.Millisecond,
		HealthAddr:        "127.0.0.1:0",
		ExitAfter:         h.opts.exitAfter,
		ExitCheckInterval: exitCheckInterval,
	})
	if err != nil {
		h.t.Fatalf("role.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go func() {
		run.err = r.Run(ctx)
		close(run.exited)
	}()
	h.mu.Lock()
	h.run = run
	h.mu.Unlock()
	return run
}

// openDirect opens an SMB session to the mapped server over the remote's LAN
// dial, never through the tunnel, and mounts the share; smb1client dials only
// port 445, so this is the one way to reach a server on another port.
func (h *remoteHarness) openDirect() device.Share {
	h.t.Helper()
	lanSites := remoteLANSites{site: &remoteLANSite{Site: netsite.NewRegistry(h.log).Site(netsite.Local), lan: h.lan}}
	sess, err := device.New(lanSites, netsite.LocalEndpoint(remoteDevIP), smbUser, smbPassword, 5*time.Second).
		Open(context.Background())
	if err != nil {
		h.t.Fatalf("Open %s directly: %v", h.lan.smbAddr, err)
	}
	h.t.Cleanup(func() { sess.Close() })
	sh, err := sess.Mount(shareName)
	if err != nil {
		h.t.Fatalf("Mount %s directly: %v", h.lan.smbAddr, err)
	}
	return sh
}

// remote is the current run of the remote role.
func (h *remoteHarness) remote() *remoteRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.run
}

// stopRemote stops the current run, if it is still running.
func (h *remoteHarness) stopRemote() {
	if r := h.remote(); r != nil {
		r.cancel()
		select {
		case <-r.exited:
		case <-time.After(30 * time.Second):
			h.t.Errorf("the remote role did not stop within 30s")
		}
	}
}

// restartRemote stops the remote and starts it again with the same pairing:
// a container restart (watchtower, a power cut) as Commander sees it.
func (h *remoteHarness) restartRemote() {
	h.t.Helper()
	h.stopRemote()
	h.startRemote()
}

func (h *remoteHarness) site() netsite.Site { return h.sites.Site(remoteID) }

func (h *remoteHarness) waitSiteUp(what string) {
	h.t.Helper()
	waitUntil(h.t, 90*time.Second, what, func() bool { return h.site().State() == netsite.StateUp })
}

func (h *remoteHarness) status() model.RemoteStatus {
	for _, s := range h.sup.List() {
		if s.ID == remoteID {
			return s
		}
	}
	h.t.Fatalf("remote %s is not supervised", remoteID)
	return model.RemoteStatus{}
}

// supCounter reads one of the supervisor's counters for this remote.
func (h *remoteHarness) supCounter(name string, labels map[string]string) float64 {
	h.t.Helper()
	return counterIn(h.t, h.supMx, name, labels)
}

func (h *remoteHarness) heartbeatsFailed() float64 {
	return h.supCounter("asiair_commander_remote_heartbeats_total",
		map[string]string{"remote": remoteID, "result": string(metrics.RemoteCallFailed)})
}

func (h *remoteHarness) rebuilds() float64 {
	return h.supCounter("asiair_commander_remote_client_rebuilds_total", map[string]string{"remote": remoteID})
}

// counterIn sums every series of counter name whose labels include labels.
func counterIn(t *testing.T, mx *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := mx.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	total := 0.0
	for _, mf := range fams {
		if mf.GetName() != name || mf.GetType() != dto.MetricType_COUNTER {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue series
				}
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// openRemote opens an SMB session to the container through the tunnel, as
// Commander's device factory does, and mounts the share.
func (h *remoteHarness) openRemote(timeout time.Duration) (device.Session, device.Share) {
	h.t.Helper()
	sess, err := device.New(h.sites, remoteEP, smbUser, smbPassword, timeout).Open(context.Background())
	if err != nil {
		h.t.Fatalf("Open through the tunnel: %v", err)
	}
	h.t.Cleanup(func() { sess.Close() })
	share, err := sess.Mount(shareName)
	if err != nil {
		h.t.Fatalf("Mount through the tunnel: %v", err)
	}
	return sess, share
}

// writeFile puts data on the container's share directly, never through the
// tunnel, and removes it when the test ends.
func (h *remoteHarness) writeFile(dir, name string, data []byte) string {
	h.t.Helper()
	if err := h.direct.WriteMarkerAt(dir, name, data); err != nil {
		h.t.Fatalf("write %s/%s: %v", dir, name, err)
	}
	p := path.Join(dir, name)
	h.t.Cleanup(func() { _ = h.direct.Delete(p) })
	return p
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// remoteResolver binds the rig to the container behind the remote, and
// re-proves its identity with Commander's own unicast probe, which travels
// through the remote's site as a forwarded UDP flow.
type remoteResolver struct {
	ep      netsite.Endpoint
	scanner *discovery.Scanner
}

func (r remoteResolver) Resolve(string, string) (netsite.Endpoint, bool) { return r.ep, true }
func (r remoteResolver) ProbeGUID(ctx context.Context, ep netsite.Endpoint) (string, string, bool) {
	return r.scanner.ProbeGUID(ctx, ep)
}

// remoteRig is a real rig worker, store and NAS reaching the container through
// the remote.
type remoteRig struct {
	h      *remoteHarness
	id     string
	dir    string
	store  store.Store
	mx     *metrics.Metrics
	notify *notify.Fake
	hub    *hub.StatusHub
	// tap counts Commander's own reads of each device file.
	tap *readTap
}

type rigOptions struct {
	dir   string // the rig's one source directory on the share
	ep    netsite.Endpoint
	grace time.Duration
	// unreachableAfter is the rig's unreachable alert threshold; zero keeps
	// the default.
	unreachableAfter time.Duration
}

// startRig runs a worker for one rig bound to the container through the remote.
func (h *remoteHarness) startRig(o rigOptions) *remoteRig {
	t := h.t
	t.Helper()
	dir := t.TempDir()
	cfg := config.Clone(h.cfg.Load())
	cfg.Defaults.MinFileAge = model.Duration(0)
	cfg.Defaults.SettleCycles = 0
	cfg.Defaults.Grace = model.Duration(o.grace)
	cfg.Defaults.PollInterval = model.Duration(time.Second)
	cfg.Defaults.RetryInterval = model.Duration(500 * time.Millisecond)
	if o.unreachableAfter > 0 {
		cfg.Defaults.UnreachableAlertAfter = model.Duration(o.unreachableAfter)
	}
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	rigID := "rig-remote"
	cfg.Rigs = []model.RigProfile{{
		ID: rigID, Name: "Remote", GUID: remoteGUID, CPUID: remoteCPUID,
		User: smbUser, Password: smbPassword,
		OTA: "FRA400", Session: "S", Managed: true,
		Sources: []model.Source{{Share: shareName, Roots: []string{o.dir}}},
	}}
	cfgPtr := &atomic.Pointer[model.Config]{}
	cfgPtr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	tap := newReadTap()
	base := device.NewFactory(h.sites)
	factory := func(ep netsite.Endpoint, user, pass string, _ time.Duration) device.Device {
		return tap.wrap(base(ep, user, pass, commanderSMBBudget))
	}
	log := h.log.With("side", "commander")
	scanner := discovery.NewDefault(h.sites, factory, nil, time.Now, log)
	r := &remoteRig{
		h: h, id: rigID, dir: o.dir, store: st, mx: metrics.New("integration-remote-rig"),
		notify: &notify.Fake{}, hub: hub.New(log), tap: tap,
	}
	w := rig.NewWorker(
		rigID, cfgPtr, factory, remoteResolver{ep: o.ep, scanner: scanner}, h.sites,
		localfs.New(), header.New(), st, r.hub, r.notify,
		drainAdmission{}, nil, nil, time.UTC, time.Now, r.mx, log,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r
}

// writeFrame puts synthetic light frame i of width x height pixels in dir on
// the device, directly. Its OBJECT is the directory's name and i, so each
// frame files to its own destination.
func (h *remoteHarness) writeFrame(dir string, i, width, height int) string {
	h.t.Helper()
	object := fmt.Sprintf("%s%02d", strings.ToUpper(path.Base(dir)), i)
	body := fitstest.Build(fitstest.LightCards(width, height, object), fitstest.SyntheticImage(width, height))
	return h.writeFile(dir, fmt.Sprintf("Light_%s_0001.fit", object), body)
}

func (r *remoteRig) frame(p string) (model.Frame, bool) {
	r.h.t.Helper()
	f, ok, err := r.store.GetFrame(context.Background(), r.id, shareName, p)
	if err != nil {
		r.h.t.Fatalf("GetFrame(%s): %v", p, err)
	}
	if !ok {
		return model.Frame{}, false
	}
	return *f, true
}

// waitStates waits until every path's row is in one of states, failing the
// test the moment any row reads failed.
func (r *remoteRig) waitStates(d time.Duration, paths []string, states ...model.FrameState) {
	r.h.t.Helper()
	waitUntil(r.h.t, d, fmt.Sprintf("%d frames to reach %v", len(paths), states), func() bool {
		all := true
		for _, p := range paths {
			f, ok := r.frame(p)
			if ok && f.State == model.StateFailed {
				r.h.t.Fatalf("frame %s recorded failed: %s", p, f.Error)
			}
			if !ok || !hasState(f.State, states) {
				all = false
			}
		}
		return all
	})
}

func hasState(s model.FrameState, states []model.FrameState) bool {
	for _, x := range states {
		if s == x {
			return true
		}
	}
	return false
}

func (r *remoteRig) counter(name string, labels map[string]string) float64 {
	r.h.t.Helper()
	labels["rig"] = r.id
	return counterIn(r.h.t, r.mx, name, labels)
}

func (r *remoteRig) outcomes(o metrics.TransferOutcome) float64 {
	return r.counter("asiair_commander_transfer_outcomes_total", map[string]string{"outcome": string(o)})
}

// rigState is the worker's published state.
func (r *remoteRig) rigState() model.RigState {
	for _, s := range r.hub.Snapshot().Rigs {
		if s.ID == r.id {
			return s.State
		}
	}
	return ""
}

// noTransferFailedAlerts fails the test if any frame was recorded failed: the
// store row and the alert are written together (markFailed).
func (r *remoteRig) noTransferFailedAlerts() {
	r.h.t.Helper()
	for _, a := range r.notify.Raised() {
		if a.Category == model.AlertTransferFailed {
			r.h.t.Fatalf("a frame was recorded failed: %s", a.Message)
		}
	}
}

// checkNASCopies re-hashes every frame's archive copy against its record.
func (r *remoteRig) checkNASCopies(paths []string) {
	r.h.t.Helper()
	for _, p := range paths {
		f, ok := r.frame(p)
		if !ok {
			r.h.t.Fatalf("frame %s missing from the store", p)
		}
		b, err := os.ReadFile(f.DestPath)
		if err != nil {
			r.h.t.Fatalf("read NAS copy of %s: %v", p, err)
		}
		if got := sha256Hex(b); got != f.SHA256 {
			r.h.t.Fatalf("%s: NAS copy re-hashes to %s, recorded %s", p, got, f.SHA256)
		}
	}
}

// requireNetworkError checks that err classifies as a network failure, the
// way the rig's pull and listing paths test for one.
func requireNetworkError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	if !device.IsNetwork(err) {
		t.Fatalf("%s: %v (%T) does not classify as a network error", what, err, err)
	}
}
