package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/remote/tunnel/internal/relaytest"
	smb1 "github.com/macourteau/smb1client"
	"tailscale.com/tailcfg"
)

// TestMain keeps the real-tunnel tests hermetic (relaytest.Hermetic). Every
// tunnel in this package's tests runs through a local DERP and STUN server.
func TestMain(m *testing.M) {
	relaytest.Hermetic()
	os.Exit(m.Run())
}

// localRelay is a DERP and STUN server on loopback (relaytest.Start), with its
// region wrapped for Mint.
type localRelay struct {
	region Region
	dm     *tailcfg.DERPMap
	stop   func()
}

func startLocalRelay(t *testing.T) *localRelay {
	t.Helper()
	r := relaytest.Start(t)
	return &localRelay{
		region: Region{ID: 1, Code: "test", def: r.Region},
		dm:     r.Map,
		stop:   r.Stop,
	}
}

// testRegion is a relay region for tests that never reach a relay (the fake,
// parse failures).
var testRegion = Region{ID: 1, Code: "test", def: &tailcfg.DERPRegion{
	RegionID:   1,
	RegionCode: "test",
	Nodes:      []*tailcfg.DERPNode{{Name: "n1", RegionID: 1, HostName: "derp.invalid"}},
}}

// deadRegion is a relay region nothing answers on.
func deadRegion(t *testing.T) Region {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return Region{ID: 1, Code: "dead", def: &tailcfg.DERPRegion{
		RegionID:   1,
		RegionCode: "dead",
		Nodes: []*tailcfg.DERPNode{{
			Name: "d1", RegionID: 1, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none",
			STUNPort: port, DERPPort: port, InsecureForTests: true, STUNTestIP: "127.0.0.1",
		}},
	}}
}

// testLogger sends Info and above to the test log; tailcat's debug chatter is
// left out.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(tWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

type tWriter struct{ t *testing.T }

func (w tWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(b))
	return len(b), nil
}

// Test LAN targets live in TEST-NET-1; the fake LAN maps them onto loopback
// listeners, so the forwardable ports need no privileges.
var (
	lanDevice     = netip.MustParseAddr("192.0.2.10") // answers on every port
	lanDead       = netip.MustParseAddr("192.0.2.11") // nothing listens
	lanSilent     = netip.MustParseAddr("192.0.2.12") // accepts TCP, never writes
	lanNotAllowed = netip.MustParseAddr("192.0.2.99") // the policy refuses it
)

// allowTestLAN is the test forwarding policy: every test LAN address but
// lanNotAllowed.
func allowTestLAN(_ Proto, ip netip.Addr) bool {
	return ip != lanNotAllowed && netip.MustParsePrefix("192.0.2.0/24").Contains(ip)
}

// fakeLAN maps test LAN targets onto loopback servers and records every dial,
// so a test can tell whether a flow ever reached the LAN.
type fakeLAN struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]string // "tcp 192.0.2.10:445" -> "127.0.0.1:port"
	dials  []string
}

func newFakeLAN(t *testing.T) *fakeLAN {
	l := &fakeLAN{t: t, routes: map[string]string{}}
	echoTCP := listenTCP(t, func(c net.Conn) { io.Copy(c, c); c.Close() })
	silentTCP := listenTCP(t, func(c net.Conn) { io.Copy(io.Discard, c); c.Close() })
	deadTCP := closedTCPPort(t)
	echoUDP := listenUDPEcho(t)
	for _, port := range []uint16{portSMB, portControl, 22, portScan} {
		l.routes["tcp "+netip.AddrPortFrom(lanDevice, port).String()] = echoTCP
		l.routes["tcp "+netip.AddrPortFrom(lanNotAllowed, port).String()] = echoTCP
		l.routes["tcp "+netip.AddrPortFrom(lanSilent, port).String()] = silentTCP
		l.routes["tcp "+netip.AddrPortFrom(lanDead, port).String()] = deadTCP
		l.routes["udp "+netip.AddrPortFrom(lanDevice, port).String()] = echoUDP
		l.routes["udp "+netip.AddrPortFrom(lanNotAllowed, port).String()] = echoUDP
	}
	return l
}

func (l *fakeLAN) dial(ctx context.Context, network, address string) (net.Conn, error) {
	l.mu.Lock()
	l.dials = append(l.dials, network+" "+address)
	to, ok := l.routes[network+" "+address]
	l.mu.Unlock()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, to)
}

func (l *fakeLAN) dialled(network string, dst netip.AddrPort) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range l.dials {
		if d == network+" "+dst.String() {
			return true
		}
	}
	return false
}

func listenTCP(t *testing.T, handle func(net.Conn)) string {
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
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

func closedTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func listenUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	return pc.LocalAddr().String()
}

// echoProtocol is the test remote-protocol handler: it echoes until EOF.
func echoProtocol(c net.Conn) {
	io.Copy(c, c)
	c.Close()
}

// harness is one tunnel, real or fake, with its LAN and policy recorder.
type harness struct {
	name     string
	client   Client
	server   Server
	lan      *fakeLAN
	pairing  *Pairing
	env      *Env
	relay    *localRelay
	mu       sync.Mutex
	consults []string
	protoSrc []net.Conn // server ends of protocol conns, for their source address
	rejected chan RejectedClient
	refusals []RefusedForward
}

func (h *harness) refusedForward(r RefusedForward) {
	h.mu.Lock()
	h.refusals = append(h.refusals, r)
	h.mu.Unlock()
}

// refusal returns the refusal reported for proto to dst, if any.
func (h *harness) refusal(proto Proto, dst netip.AddrPort) (RefusedForward, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.refusals {
		if r.Proto == proto && r.Target == dst {
			return r, true
		}
	}
	return RefusedForward{}, false
}

func (h *harness) policy(proto Proto, ip netip.Addr, port uint16) bool {
	h.mu.Lock()
	h.consults = append(h.consults, string(proto)+" "+netip.AddrPortFrom(ip, port).String())
	h.mu.Unlock()
	return allowTestLAN(proto, ip)
}

func (h *harness) consulted(proto Proto, dst netip.AddrPort) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.consults {
		if c == string(proto)+" "+dst.String() {
			return true
		}
	}
	return false
}

func (h *harness) serverConfig(t *testing.T, log *slog.Logger) ServerConfig {
	return ServerConfig{
		Env:    h.env,
		Policy: h.policy,
		Protocol: func(c net.Conn) {
			h.mu.Lock()
			h.protoSrc = append(h.protoSrc, c)
			h.mu.Unlock()
			echoProtocol(c)
		},
		OnRejectedClient: func(r RejectedClient) { h.rejected <- r },
		OnRefusedForward: h.refusedForward,
		DialLAN:          h.lan.dial,
		Logger:           log,
	}
}

func mintEnv(t *testing.T, region Region) (*Pairing, *Env) {
	t.Helper()
	p, err := Mint(region)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	env, err := parseEnvForTests(p.EnvValue.Reveal())
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	return p, env
}

// parseEnvForTests is the test-only constructor: it parses like ParseEnv but
// admits a local test relay, which ParseEnv refuses.
func parseEnvForTests(value string) (*Env, error) { return parseEnv(value, true) }

// newClientForTests and fingerprintForTests are the test-only forms of
// NewClient and Fingerprint: they admit a local test relay, which the
// production forms refuse.
func newClientForTests(cfg ClientConfig) (Client, error) { return newClient(cfg, true) }

func fingerprintForTests(address, clientKey Secret) (string, error) {
	return fingerprintOf(address, clientKey, true)
}

// newRealHarness starts a real tailcat server and client through a local
// relay.
func newRealHarness(t *testing.T, log *slog.Logger) *harness {
	t.Helper()
	if log == nil {
		log = testLogger(t)
	}
	relay := startLocalRelay(t)
	p, env := mintEnv(t, relay.region)
	h := &harness{name: "tailcat", lan: newFakeLAN(t), pairing: p, env: env, relay: relay, rejected: make(chan RejectedClient, 16)}
	srv, err := StartServer(h.serverConfig(t, log))
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	c, err := newClientForTests(ClientConfig{Address: p.Address, ClientKey: p.ClientKey, Logger: log})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	h.server, h.client = srv, c
	return h
}

// newFakeHarness builds the in-memory equivalent of newRealHarness.
func newFakeHarness(t *testing.T) *harness {
	t.Helper()
	p, env := mintEnv(t, testRegion)
	h := &harness{name: "fake", lan: newFakeLAN(t), pairing: p, env: env, rejected: make(chan RejectedClient, 16)}
	f, err := NewFake(h.serverConfig(t, testLogger(t)))
	if err != nil {
		t.Fatalf("NewFake: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	h.server, h.client = f, f.NewClient()
	return h
}

// forBoth runs a behaviour against the real tunnel and the fake, so the fake
// is held to the same contract.
func forBoth(t *testing.T, fn func(t *testing.T, h *harness)) {
	t.Run("tailcat", func(t *testing.T) {
		t.Parallel()
		fn(t, newRealHarness(t, nil))
	})
	t.Run("fake", func(t *testing.T) {
		t.Parallel()
		fn(t, newFakeHarness(t))
	})
}

func dialCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// requireNetworkError checks that err classifies as a network failure the way
// the SMB and control layers test for it.
func requireNetworkError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got nil error, want a network error", what)
	}
	var ne net.Error
	if !errors.As(err, &ne) {
		t.Errorf("%s: %T %q is not a net.Error", what, err, err)
	}
	if !smb1.IsNetworkError(err) {
		t.Errorf("%s: smb1.IsNetworkError(%q) = false", what, err)
	}
}

// roundTrip writes msg and reads the reply within a deadline.
func roundTrip(t *testing.T, c net.Conn, msg string, want string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != want {
		t.Fatalf("reply = %q, want %q", buf, want)
	}
}

// udpRoundTrip sends a datagram until a reply comes back. The first datagram of
// a fresh tunnel can be lost while WireGuard's handshake settles, which a lone
// UDP probe does not retransmit.
func udpRoundTrip(c net.Conn, msg string, tries int, wait time.Duration) (string, error) {
	buf := make([]byte, 2048)
	var err error
	for range tries {
		if _, err = c.Write([]byte(msg)); err != nil {
			return "", err
		}
		c.SetReadDeadline(time.Now().Add(wait))
		var n int
		n, err = c.Read(buf)
		if err == nil {
			return string(buf[:n]), nil
		}
	}
	return "", err
}
