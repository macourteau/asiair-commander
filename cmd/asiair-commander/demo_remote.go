//go:build demo

// Demo remotes: in a `-tags demo` build whose data dir holds a tools/seedremote
// scenario, every paired remote is an in-memory remote process (a tunnel.Fake
// serving a scripted remote protocol) instead of a tailcat tunnel. Only the
// tunnel client is swapped: the supervisor, its heartbeat, its state machine,
// its sites, discovery's broadcast rounds through them and the rig workers all
// run unchanged, so every card state the UI shows is derived exactly as in
// production. The timeline (a remote stopping, Commander's own link dropping)
// is driven by marker files, see demoscenario.ControlDir.
//
// This file is compiled ONLY under `-tags demo`; the production binary never
// contains it (see demo_off.go for the no-op seam).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/demoscenario"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/supervisor"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// controlPoll is how often the timeline markers are looked at.
const controlPoll = 500 * time.Millisecond

// demoScenario is the loaded scenario, nil when the data dir has none. It is
// set before the workers start and read-only after, so installDemo's resolver
// and device factory can route the rigs reached through a remote.
var demoScenario *demoscenario.Scenario

// installDemoRemotes loads the data dir's scenario and, when there is one,
// replaces the supervisor's tunnel client constructor with the demo remotes'.
func installDemoRemotes(ctx context.Context, dataDir string, opts supervisor.Options, logger *slog.Logger) supervisor.Options {
	sc, err := demoscenario.Load(dataDir)
	if err != nil {
		logger.Error("DEMO MODE: unreadable remote scenario; remotes use real tunnels", "error", err)
		return opts
	}
	if sc == nil {
		return opts
	}
	d := &demoRemotes{byID: map[string]*demoRemote{}, link: &linkGate{conns: map[net.Conn]struct{}{}}, log: logger}
	for _, seed := range sc.Remotes {
		r, err := newDemoRemote(seed)
		if err != nil {
			logger.Error("DEMO MODE: remote scenario entry refused; it uses a real tunnel", "remote", seed.ID, "error", err)
			continue
		}
		d.byID[seed.ID] = r
	}
	demoScenario = sc
	opts.NewClient = d.newClient
	go d.watch(ctx, filepath.Join(dataDir, demoscenario.ControlDir))
	logger.Info("DEMO MODE: remotes are in-memory remote processes", "remotes", len(d.byID), "rigs", len(sc.Rigs))
	return opts
}

// demoRemotes is every scripted remote and Commander's own link.
type demoRemotes struct {
	byID map[string]*demoRemote
	link *linkGate
	log  *slog.Logger
}

// newClient is the supervisor's tunnel client constructor: a client of the
// remote's process, behind Commander's link. A pairing other than the one the
// remote's .env holds (a Re-pair the user never installed) reaches no remote
// process at all.
func (d *demoRemotes) newClient(p model.RemoteProfile, log *slog.Logger) (tunnel.Client, error) {
	r := d.byID[p.ID]
	if r == nil {
		return tunnel.NewClient(tunnel.ClientConfig{Address: tunnel.Secret(p.Address), ClientKey: tunnel.Secret(p.ClientKey), Logger: log})
	}
	if p.Address != r.seed.Address {
		return d.link.wrap(r.uninstalled.NewClient()), nil
	}
	return d.link.wrap(r.srv.NewClient()), nil
}

// watch applies the timeline markers as they appear: a stop marker stops that
// remote's process for good, and the link marker drops Commander's link while
// it exists.
func (d *demoRemotes) watch(ctx context.Context, dir string) {
	t := time.NewTicker(controlPoll)
	defer t.Stop()
	stopped := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for id, r := range d.byID {
			if stopped[id] || !exists(filepath.Join(dir, demoscenario.StopMarker(id))) {
				continue
			}
			stopped[id] = true
			r.srv.Close()
			d.log.Info("DEMO MODE: remote process stopped", "remote", id)
		}
		down := exists(filepath.Join(dir, demoscenario.LinkDown))
		if d.link.set(down) {
			d.log.Info("DEMO MODE: Commander's link changed", "down", down)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// demoRemote is one remote PC: the process its .env runs, and a server no
// process answers for, standing in for any pairing it never installed.
type demoRemote struct {
	seed        demoscenario.Remote
	srv         *tunnel.Fake
	uninstalled *tunnel.Fake
}

func newDemoRemote(seed demoscenario.Remote) (*demoRemote, error) {
	env, err := tunnel.ParseEnv(seed.EnvValue)
	if err != nil {
		return nil, err
	}
	h := newDemoHandler(seed)
	srv, err := tunnel.NewFake(tunnel.ServerConfig{
		Env:      env,
		Policy:   h.policy,
		Protocol: func(c net.Conn) { proto.Serve(c, h, proto.ServeOptions{}) },
		// The devices answer scan_air only: the demo device factory serves
		// their files without a dial, so no forwarded flow reaches a LAN.
		DialLAN: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("demo remote: no LAN behind this remote")
		},
	})
	if err != nil {
		return nil, err
	}
	if !seed.Running {
		srv.Close()
	}
	uninstalled, err := tunnel.NewFake(tunnel.ServerConfig{
		Env:      env,
		Policy:   func(tunnel.Proto, netip.Addr, uint16) bool { return false },
		Protocol: func(c net.Conn) { c.Close() },
	})
	if err != nil {
		return nil, err
	}
	uninstalled.Close()
	return &demoRemote{seed: seed, srv: srv, uninstalled: uninstalled}, nil
}

// demoHandler is a remote process's side of the remote protocol, scripted from
// its scenario entry. Its broadcast fills the forwarding allowlist, as the real
// remote's does.
type demoHandler struct {
	seed     demoscenario.Remote
	instance string
	// started is the process's start, backdated by its seeded uptime; born is
	// when this Commander started it, from which the seeded warning ages run.
	started time.Time
	born    time.Time

	mu    sync.Mutex
	allow map[netip.Addr]bool
}

func newDemoHandler(seed demoscenario.Remote) *demoHandler {
	now := time.Now()
	return &demoHandler{
		seed: seed, instance: "demo-" + seed.ID,
		started: now.Add(-time.Duration(seed.UptimeS) * time.Second), born: now,
		allow: map[netip.Addr]bool{},
	}
}

func (h *demoHandler) Hello(ctx context.Context, _ string) proto.HelloReply {
	// The link's round trip, so the card's latency reads like a real path's.
	select {
	case <-time.After(time.Duration(h.seed.LatencyMS) * time.Millisecond):
	case <-ctx.Done():
	}
	whole, data := header.WholeDigestVersion, header.DataDigestVersion
	if v := h.seed.WholeDigestVersion; v != nil {
		whole = *v
	}
	if v := h.seed.DataDigestVersion; v != nil {
		data = *v
	}
	since := time.Since(h.born).Milliseconds()
	warnings := make([]proto.Warning, len(h.seed.Warnings))
	for i, w := range h.seed.Warnings {
		w.Seq = uint64(i + 1)
		w.AgeMS += since
		warnings[i] = w
	}
	return proto.HelloReply{
		WholeDigestVersion: whole, DataDigestVersion: data, Build: h.seed.Build,
		UptimeMS: time.Since(h.started).Milliseconds(), InstanceID: h.instance,
		Path:     proto.Path{Connected: true, Direct: h.seed.Direct, RelayRegion: h.seed.RelayRegion},
		Warnings: warnings,
	}
}

func (h *demoHandler) Broadcast(context.Context, string) (proto.BroadcastReply, error) {
	var out proto.BroadcastReply
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, d := range h.seed.Devices {
		if ip, err := netip.ParseAddr(d.IP); err == nil {
			h.allow[ip] = true
		}
		out.Replies = append(out.Replies, proto.Datagram{Data: demoscenario.ScanAirReply(d), SrcIP: d.IP})
	}
	return out, nil
}

// Hash is refused: the scenario's remote rigs keep their originals, so no
// delete ever measures a device copy through a remote.
func (h *demoHandler) Hash(context.Context, string, proto.HashRequest, func(int64)) (proto.HashReply, error) {
	return proto.HashReply{}, &proto.Error{Code: proto.CodeRefused, Message: "demo remote: no hash"}
}

func (h *demoHandler) policy(_ tunnel.Proto, ip netip.Addr, _ uint16) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allow[ip]
}

// linkGate is Commander's own link to the relays: while it is down no client
// reaches its relay region and every tunnel flow fails, the open ones
// included, as they would with Commander's internet down.
type linkGate struct {
	mu    sync.Mutex
	down  bool
	conns map[net.Conn]struct{}
}

// set changes the link's state and reports whether it changed; dropping it
// cuts every open flow.
func (g *linkGate) set(down bool) bool {
	g.mu.Lock()
	if g.down == down {
		g.mu.Unlock()
		return false
	}
	g.down = down
	var cut []net.Conn
	if down {
		for c := range g.conns {
			cut = append(cut, c)
		}
		g.conns = map[net.Conn]struct{}{}
	}
	g.mu.Unlock()
	for _, c := range cut {
		c.Close()
	}
	return true
}

func (g *linkGate) isDown() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.down
}

func (g *linkGate) wrap(c tunnel.Client) tunnel.Client { return &gatedClient{Client: c, g: g} }

// errLinkDown is a dial's cause while Commander's link is down.
var errLinkDown = errors.New("network is unreachable")

// gatedClient is a tunnel client behind the link gate.
type gatedClient struct {
	tunnel.Client
	g *linkGate
}

// admit tracks a flow the gate must cut when the link drops, or fails the
// dial while it is down. Errors stay *net.OpError, as the tunnel's own are.
func (c *gatedClient) admit(network string, conn net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	if c.g.down {
		conn.Close()
		return nil, &net.OpError{Op: "dial", Net: network, Err: errLinkDown}
	}
	tc := &gatedConn{Conn: conn, g: c.g}
	c.g.conns[tc] = struct{}{}
	return tc, nil
}

func (c *gatedClient) DialTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	conn, err := c.Client.DialTCP(ctx, ip, port)
	return c.admit("tcp", conn, err)
}

func (c *gatedClient) DialUDP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	conn, err := c.Client.DialUDP(ctx, ip, port)
	return c.admit("udp", conn, err)
}

func (c *gatedClient) DialProtocol(ctx context.Context) (net.Conn, error) {
	conn, err := c.Client.DialProtocol(ctx)
	return c.admit("tcp", conn, err)
}

func (c *gatedClient) RelayConnected() bool { return !c.g.isDown() && c.Client.RelayConnected() }

// gatedConn leaves the gate's tracking when closed.
type gatedConn struct {
	net.Conn
	g *linkGate
}

func (c *gatedConn) Close() error {
	c.g.mu.Lock()
	delete(c.g.conns, c)
	c.g.mu.Unlock()
	return c.Conn.Close()
}
