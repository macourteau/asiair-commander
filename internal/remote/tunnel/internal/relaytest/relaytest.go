// Package relaytest runs a DERP and STUN relay on loopback, built like
// tailscale's integration.RunDERPAndSTUN, so a test can run a real tailcat
// tunnel without ever reaching public DERP. The relay's DERP port sits behind a
// gate the test can close and reopen, which stands in for a remote's uplink
// dropping and coming back without changing the relay's address.
//
// Only tests import it: the tunnel package's own tests, and the integration
// tests through the remotetest-tagged tunneltest package. No shipped build
// links it.
package relaytest

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"tailscale.com/derp/derpserver"
	"tailscale.com/envknob"
	"tailscale.com/net/netcheck"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

// Hermetic keeps a test binary's tunnels off the real network, as tailcat's own
// tests do: IN_TS_TEST skips the portmapper's probes of the default gateway,
// and the captive-portal hook stops netcheck from making HTTP requests to the
// real world. Call it from TestMain, before any tunnel starts.
func Hermetic() {
	envknob.Setenv("IN_TS_TEST", "true")
	netcheck.HookStartCaptivePortalDetection.SetForTest(func(ctx context.Context, c *netcheck.Client, dm *tailcfg.DERPMap, preferredDERP tailcfg.DERPRegionID, setCaptivePortal func(bool)) (done <-chan struct{}, stop func()) {
		return syncs.ClosedChan(), func() {}
	})
}

// Relay is a running loopback relay.
type Relay struct {
	// Region is the relay's one region, marked InsecureForTests: its TLS
	// certificate is never verified.
	Region *tailcfg.DERPRegion
	// Map is a relay map holding only Region.
	Map *tailcfg.DERPMap

	gate *gate
	stop func()
}

// Start runs a relay until the test ends or Stop is called.
func Start(t testing.TB) *Relay {
	t.Helper()
	d := derpserver.New(key.NewNode(), logger.Discard)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(derpserver.Handler(d))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Config.ErrorLog = logger.StdLogger(logger.Discard)
	srv.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	srv.StartTLS()
	stunAddr, stunCleanup := stuntest.ServeWithPacketListener(t, nettype.Std{})
	g, err := newGate(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	reg := &tailcfg.DERPRegion{
		RegionID:   1,
		RegionCode: "test",
		Nodes: []*tailcfg.DERPNode{{
			Name:             "t1",
			RegionID:         1,
			HostName:         "127.0.0.1",
			IPv4:             "127.0.0.1",
			IPv6:             "none",
			STUNPort:         stunAddr.Port,
			DERPPort:         g.port(),
			InsecureForTests: true,
			STUNTestIP:       "127.0.0.1",
		}},
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			g.close()
			srv.CloseClientConnections()
			srv.Close()
			d.Close()
			stunCleanup()
		})
	}
	t.Cleanup(stop)
	return &Relay{
		Region: reg,
		Map:    &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{1: reg}},
		gate:   g,
		stop:   stop,
	}
}

// Stop shuts the relay down for good.
func (r *Relay) Stop() { r.stop() }

// Cut drops every connection to the relay's DERP port and refuses new ones
// until Restore, as an uplink outage would. STUN keeps answering: it carries
// no tunnel traffic.
func (r *Relay) Cut() { r.gate.setUp(false) }

// Restore lets connections reach the relay again, at the same address.
func (r *Relay) Restore() { r.gate.setUp(true) }

// gate is a TCP pass-through in front of the DERP server that can refuse and
// drop connections. TLS runs end to end through it.
type gate struct {
	ln      net.Listener
	backend string

	mu    sync.Mutex
	up    bool
	conns map[net.Conn]struct{}
}

func newGate(backend string) (*gate, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	g := &gate{ln: ln, backend: backend, up: true, conns: map[net.Conn]struct{}{}}
	go g.serve()
	return g, nil
}

func (g *gate) port() int { return g.ln.Addr().(*net.TCPAddr).Port }

func (g *gate) serve() {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		go g.pass(c)
	}
}

// pass forwards one connection while the gate is up. A connection accepted
// while it is down is closed at once, which the DERP client sees as the relay
// being unreachable.
func (g *gate) pass(c net.Conn) {
	if !g.track(c) {
		c.Close()
		return
	}
	defer g.untrack(c)
	b, err := net.Dial("tcp", g.backend)
	if err != nil {
		c.Close()
		return
	}
	if !g.track(b) {
		c.Close()
		b.Close()
		return
	}
	defer g.untrack(b)
	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		io.Copy(dst, src)
		dst.Close()
		src.Close()
		done <- struct{}{}
	}
	go pump(b, c)
	go pump(c, b)
	<-done
	<-done
}

// track records c as open; it refuses while the gate is down.
func (g *gate) track(c net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.up {
		return false
	}
	g.conns[c] = struct{}{}
	return true
}

func (g *gate) untrack(c net.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.conns, c)
}

func (g *gate) setUp(up bool) {
	g.mu.Lock()
	g.up = up
	var drop []net.Conn
	if !up {
		for c := range g.conns {
			drop = append(drop, c)
		}
	}
	g.mu.Unlock()
	for _, c := range drop {
		c.Close()
	}
}

func (g *gate) close() {
	g.ln.Close()
	g.setUp(false)
}
