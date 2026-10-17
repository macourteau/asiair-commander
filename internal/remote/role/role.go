// Package role is the remote role: the process a remote site runs (the
// `remote` verb), a tailcat server pinned to one Commander that forwards to the
// ASIAIRs on its LAN under a least-privilege policy and serves the remote
// protocol.
//
// It keeps no durable state and makes no decision: it writes no file (no store,
// no web UI, no config file), logs only to stdout, and holds its forwarding
// allowlist and its recent warnings in memory, so a restart starts both empty.
// It imports neither internal/store nor internal/rig (TestRemoteRoleIsStateless).
//
// Its whole configuration is one environment variable, EnvVar, holding the env
// value Commander shows once at pairing.
package role

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// EnvVar names the environment variable holding the pairing's env value. The
// tunnel package owns the pairing format, the .env line included.
const EnvVar = tunnel.EnvVar

const (
	// ExitAfter is how long the remote runs without its relay home region
	// before exiting non-zero, so its restart policy recovers a wedged tunnel
	// or a boot without network. Ten minutes rides out an ordinary uplink
	// blip without restart churn.
	ExitAfter = 10 * time.Minute
	// exitCheckInterval is how often the exit rule looks at the relay.
	exitCheckInterval = 15 * time.Second

	// HealthAddr is the loopback address the healthcheck asks.
	HealthAddr = "127.0.0.1:4748"

	// smbTimeout is the remote's SMB budget to a device on its own LAN, the
	// local site's budget in Commander.
	smbTimeout = netsite.LocalStallWindow
	// maxWarnings bounds the in-memory warnings list.
	maxWarnings = 32
)

// Sites is what the role needs of its LAN: the local site, for the broadcast
// round, the identity probe and the SMB dial, and the site list discovery's
// probe takes. *netsite.Registry implements it.
type Sites interface {
	netsite.Sites
	All() []netsite.Site
}

// Config is the role's configuration. Only the build fields are required;
// every other field is a seam for tests and defaults to production.
type Config struct {
	// Build identifies this binary in hello.
	Build proto.Build
	// Getenv reads the environment; nil uses os.Getenv.
	Getenv func(string) string
	// Logger receives the role's logs; nil uses slog.Default().
	Logger *slog.Logger
	// Now reads the clock; nil uses time.Now.
	Now func() time.Time

	// StartServer starts the tunnel server; nil uses tunnel.StartServer.
	StartServer func(tunnel.ServerConfig) (tunnel.Server, error)
	// DialLAN dials a forwarded flow's target; nil lets the tunnel dial.
	DialLAN func(ctx context.Context, network, address string) (net.Conn, error)
	// Sites carries the broadcast round, the probe and the SMB dial; nil uses
	// a site registry holding only the local site.
	Sites Sites
	// NewDevice opens the remote's own SMB session for a hash; nil uses
	// device.NewFactory over Sites.
	NewDevice func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device

	// ProgressInterval is how often a hash reports progress; zero uses the
	// protocol's default.
	ProgressInterval time.Duration

	// HealthAddr is the healthcheck's loopback listen address; "" uses
	// HealthAddr.
	HealthAddr string
	// ExitAfter and ExitCheckInterval set the exit rule; zero uses ExitAfter
	// and exitCheckInterval.
	ExitAfter         time.Duration
	ExitCheckInterval time.Duration
}

// Remote is one remote-role process.
type Remote struct {
	cfg        Config
	log        *slog.Logger
	now        func() time.Time
	env        *tunnel.Env
	sites      Sites
	newDevice  func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device
	scanner    *discovery.Scanner
	start      time.Time
	instanceID string

	allow    allowlist
	warnings warnings

	mu         sync.RWMutex
	server     tunnel.Server
	healthAddr string // the health listener's bound address, once listening
}

// ErrRelayUnreachable is Run's error when the exit rule fires.
var ErrRelayUnreachable = errors.New("remote: no connection to the relay home region; exiting so the restart policy recovers")

// New reads the pairing and checks the environment. It refuses to build a
// remote without a well-formed pinned Commander key, and while tailcat's
// address-printing knob is set.
func New(cfg Config) (*Remote, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if err := tunnel.CheckDebugAddr(getenv); err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	value := getenv(EnvVar)
	if value == "" {
		return nil, fmt.Errorf("remote: %s is not set; refusing to start without the pairing and its pinned Commander key", EnvVar)
	}
	env, err := tunnel.ParseEnv(value)
	if err != nil {
		return nil, fmt.Errorf("remote: %s: %w", EnvVar, err)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sites := cfg.Sites
	if sites == nil {
		sites = netsite.NewRegistry(log)
	}
	newDevice := cfg.NewDevice
	if newDevice == nil {
		newDevice = device.NewFactory(sites)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("remote: instance id: %w", err)
	}
	return &Remote{
		cfg:       cfg,
		log:       log,
		now:       now,
		env:       env,
		sites:     sites,
		newDevice: newDevice,
		// Commander's own probe: the same request, budget and reply parsing.
		// The scanner's registry stays empty; only ProbeGUID is used.
		scanner:    discovery.NewDefault(sites, newDevice, nil, now, log),
		start:      now(),
		instanceID: hex.EncodeToString(id[:]),
		allow:      allowlist{ips: map[netip.Addr]struct{}{}},
		warnings:   warnings{max: maxWarnings},
	}, nil
}

// Run starts the tunnel server and serves until ctx ends (nil) or the exit rule
// fires (ErrRelayUnreachable). A missing hello never ends it: a remote whose
// Commander is off, removed or re-paired keeps running with its warnings.
func (r *Remote) Run(ctx context.Context) error {
	after, interval := r.cfg.ExitAfter, r.cfg.ExitCheckInterval
	if after <= 0 {
		after = ExitAfter
	}
	if interval <= 0 {
		interval = exitCheckInterval
	}
	// The window starts with the process, before the relay is first tried.
	rule := exitRule{after: after, lastHome: r.now()}

	start := r.cfg.StartServer
	if start == nil {
		start = tunnel.StartServer
	}
	r.log.Info("remote: starting", "fingerprint", r.env.Fingerprint(), "version", r.cfg.Build.Version,
		"commit", r.cfg.Build.Commit, "built", r.cfg.Build.Date, "instance_id", r.instanceID)
	srv, err := start(tunnel.ServerConfig{
		Env:              r.env,
		Policy:           r.policy,
		Protocol:         r.serveConn,
		OnRejectedClient: r.rejectedClient,
		OnRefusedForward: r.refusedForward,
		DialLAN:          r.cfg.DialLAN,
		Logger:           r.log,
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.server = srv
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.server = nil
		r.mu.Unlock()
		srv.Close()
	}()

	stopHealth := r.serveHealth()
	defer stopHealth()

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("remote: stopping")
			return nil
		case <-tick.C:
			if rule.expired(r.now(), srv.HomeConnected()) {
				r.log.Error("remote: relay home region unreachable; exiting", "for", after)
				return ErrRelayUnreachable
			}
		}
	}
}

// exitRule fires once the relay home region has been unreachable for after.
// It reads only the relay connection, never Commander's hellos.
type exitRule struct {
	after    time.Duration
	lastHome time.Time
}

func (e *exitRule) expired(now time.Time, home bool) bool {
	if home {
		e.lastHome = now
		return false
	}
	return now.Sub(e.lastHome) >= e.after
}

func (r *Remote) currentServer() tunnel.Server {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.server
}

// healthy reports the healthcheck's condition: the tunnel server is up, its
// pin is loaded, and the relay home region is connected. It never depends on
// Commander or an ASIAIR.
func (r *Remote) healthy() (bool, string) {
	srv := r.currentServer()
	switch {
	case srv == nil:
		return false, "tunnel server not running"
	case r.env == nil:
		return false, "no pairing loaded"
	case !srv.HomeConnected():
		return false, "relay home region not connected"
	}
	return true, "healthy"
}

// serveHealth answers the healthcheck on a loopback-only listener. A loopback
// socket writes nothing to disk, unlike a status file, and a container's
// healthcheck runs in the remote's own network namespace. A listener that
// cannot bind leaves the remote running, reporting unhealthy, rather than
// taking forwarding down with it.
func (r *Remote) serveHealth() (stop func()) {
	addr := r.cfg.HealthAddr
	if addr == "" {
		addr = HealthAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		r.log.Error("remote: healthcheck listener failed; the healthcheck will report unhealthy", "address", addr, "error", err)
		return func() {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		ok, why := r.healthy()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintln(w, why)
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go hs.Serve(ln)
	r.mu.Lock()
	r.healthAddr = ln.Addr().String()
	r.mu.Unlock()
	r.log.Debug("remote: healthcheck listening", "address", ln.Addr().String())
	return func() { hs.Close() }
}

// Healthcheck asks a running remote's loopback health endpoint and returns the
// exit code: 0 when healthy. It writes no file.
func Healthcheck(addr string) int {
	if addr == "" {
		addr = HealthAddr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy: remote not answering:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: status", resp.StatusCode)
		return 1
	}
	fmt.Println("healthy")
	return 0
}

// policy is the forwarding policy: SMB and the control port over TCP, scan_air
// over UDP, and only to an address on the allowlist. The tunnel checks the
// ports too; the role states its own policy whole rather than lean on that.
func (r *Remote) policy(p tunnel.Proto, ip netip.Addr, port uint16) bool {
	switch {
	case p == tunnel.TCP && (port == netsite.SMBPort || port == netsite.ControlPort):
	case p == tunnel.UDP && port == netsite.ScanAirPort:
	default:
		return false
	}
	return r.allow.has(ip)
}

func (r *Remote) refusedForward(f tunnel.RefusedForward) {
	reason := f.Reason
	if f.ByPolicy {
		if r.allow.empty() {
			reason = "no broadcast round has run since the remote started"
		} else {
			reason = "the address has not answered the remote's scan_air broadcast"
		}
	}
	r.warnings.add(r.now(), proto.Warning{
		Kind:   proto.WarnRefusedForward,
		Proto:  string(f.Proto),
		Target: f.Target.String(),
		Reason: reason,
	})
}

func (r *Remote) rejectedClient(c tunnel.RejectedClient) {
	r.warnings.add(r.now(), proto.Warning{
		Kind:      proto.WarnRejectedClient,
		ClientKey: c.Key,
		Reason:    "a client key other than the pinned one tried to connect; the pairing may have leaked",
	})
}

func (r *Remote) serveConn(c net.Conn) {
	proto.Serve(c, r, proto.ServeOptions{Logger: r.log, ProgressInterval: r.cfg.ProgressInterval})
}

// Hello implements proto.Handler.
func (r *Remote) Hello(_ context.Context, callID string) proto.HelloReply {
	now := r.now()
	hr := proto.HelloReply{
		WholeDigestVersion: header.WholeDigestVersion,
		DataDigestVersion:  header.DataDigestVersion,
		Build:              r.cfg.Build,
		UptimeMS:           now.Sub(r.start).Milliseconds(),
		InstanceID:         r.instanceID,
		Warnings:           r.warnings.snapshot(now),
	}
	if srv := r.currentServer(); srv != nil {
		// Only the pinned client is ever admitted, so there is at most one.
		if peers := srv.Peers(); len(peers) > 0 {
			hr.Path = proto.Path{Connected: true, Direct: peers[0].Direct, RelayRegion: peers[0].RelayRegion}
		}
	}
	r.log.Debug("remote: hello", "call_id", callID)
	return hr
}

// Broadcast implements proto.Handler: one scan_air round with the remote's own
// request (the local site's), whose replies grow the allowlist.
func (r *Remote) Broadcast(ctx context.Context, callID string) (proto.BroadcastReply, error) {
	dgs, err := r.sites.Site(netsite.Local).Broadcast(ctx)
	if err != nil {
		r.log.Warn("remote: broadcast round failed", "call_id", callID, "error", err)
		r.warnings.add(r.now(), proto.Warning{Kind: proto.WarnBroadcastFailed, CallID: callID, Reason: err.Error()})
		return proto.BroadcastReply{}, &proto.Error{Code: proto.CodeNetwork, Message: "broadcast round failed: " + err.Error()}
	}
	for _, ip := range r.allow.admit(dgs) {
		r.log.Info("remote: address joined the forwarding allowlist", "call_id", callID, "address", ip.String())
	}
	// Never nil: a round nothing answered is an empty list on the wire, not
	// null.
	out := proto.BroadcastReply{Replies: append([]proto.Datagram{}, dgs...)}
	r.log.Debug("remote: broadcast round", "call_id", callID, "replies", len(dgs))
	return out, nil
}

// allowlist is the set of addresses the remote forwards to. An address joins
// only from a scan_air reply and never leaves; it lives in memory only.
type allowlist struct {
	mu  sync.RWMutex
	ips map[netip.Addr]struct{}
}

// admit adds the source of every reply that discovery would accept as a device
// and whose result.ip is empty or equal to its source, and returns the
// addresses that were not already on the list.
func (a *allowlist) admit(dgs []netsite.Datagram) []netip.Addr {
	var added []netip.Addr
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, dg := range dgs {
		id, ok := discovery.ParseScanAirReply(dg.Data, dg.SrcIP)
		if !ok {
			continue
		}
		src, err := netip.ParseAddr(dg.SrcIP)
		if err != nil {
			continue
		}
		src = src.Unmap()
		// The parser falls back to the source when result.ip is empty, so
		// this one comparison is the whole rule.
		addr, err := netip.ParseAddr(id.Address)
		if err != nil || addr.Unmap() != src || !src.Is4() {
			continue
		}
		if _, ok := a.ips[src]; !ok {
			a.ips[src] = struct{}{}
			added = append(added, src)
		}
	}
	return added
}

func (a *allowlist) has(ip netip.Addr) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.ips[ip.Unmap()]
	return ok
}

func (a *allowlist) empty() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.ips) == 0
}

// warnings is the bounded in-memory list hello carries. Entries keep the
// remote's monotonic reading so hello reports ages, never timestamps.
type warnings struct {
	max int

	mu   sync.Mutex
	seq  uint64
	list []warning
}

type warning struct {
	w  proto.Warning
	at time.Time
}

func (ws *warnings) add(now time.Time, w proto.Warning) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.seq++
	w.Seq = ws.seq
	ws.list = append(ws.list, warning{w: w, at: now})
	if len(ws.list) > ws.max {
		ws.list = append(ws.list[:0:0], ws.list[len(ws.list)-ws.max:]...)
	}
}

func (ws *warnings) snapshot(now time.Time) []proto.Warning {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := make([]proto.Warning, 0, len(ws.list))
	for _, e := range ws.list {
		w := e.w
		w.AgeMS = now.Sub(e.at).Milliseconds()
		out = append(out, w)
	}
	return out
}
