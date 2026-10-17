package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/netstack"
)

// TestTunnelForwarding carries TCP to both forwarded TCP ports, a unicast UDP
// request/reply, and two concurrent remote-protocol connections, through the
// real tunnel and the fake alike.
func TestTunnelForwarding(t *testing.T) {
	forBoth(t, func(t *testing.T, h *harness) {
		for _, port := range []uint16{portSMB, portControl} {
			c, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanDevice, port)
			if err != nil {
				t.Fatalf("DialTCP(%v:%d): %v", lanDevice, port, err)
			}
			roundTrip(t, c, fmt.Sprintf("hello %d", port), fmt.Sprintf("hello %d", port))
			c.Close()
		}

		u, err := h.client.DialUDP(dialCtx(t, 20*time.Second), lanDevice, portScan)
		if err != nil {
			t.Fatalf("DialUDP: %v", err)
		}
		got, err := udpRoundTrip(u, "scan_air", 10, time.Second)
		if err != nil {
			t.Fatalf("UDP request/reply: %v", err)
		}
		if got != "echo:scan_air" {
			t.Fatalf("UDP reply = %q, want %q", got, "echo:scan_air")
		}
		u.Close()

		// Two protocol connections open at once, as a hash runs beside hello;
		// each must carry only its own bytes.
		p1, err := h.client.DialProtocol(dialCtx(t, 20*time.Second))
		if err != nil {
			t.Fatalf("DialProtocol 1: %v", err)
		}
		defer p1.Close()
		p2, err := h.client.DialProtocol(dialCtx(t, 20*time.Second))
		if err != nil {
			t.Fatalf("DialProtocol 2: %v", err)
		}
		defer p2.Close()
		var wg sync.WaitGroup
		for i, c := range []net.Conn{p1, p2} {
			wg.Go(func() {
				for j := range 20 {
					msg := fmt.Sprintf("conn%d-msg%02d;", i, j)
					c.SetDeadline(time.Now().Add(10 * time.Second))
					if _, err := io.WriteString(c, msg); err != nil {
						t.Errorf("conn %d write: %v", i, err)
						return
					}
					buf := make([]byte, len(msg))
					if _, err := io.ReadFull(c, buf); err != nil {
						t.Errorf("conn %d read: %v", i, err)
						return
					}
					if string(buf) != msg {
						t.Errorf("conn %d got %q, want %q", i, buf, msg)
						return
					}
				}
			})
		}
		wg.Wait()

		// The server reports the pinned client as its only peer.
		if h.name == "tailcat" {
			pin, _ := h.env.clientPub.MarshalText()
			peers := h.server.Peers()
			if len(peers) != 1 || peers[0].Key != string(pin) {
				t.Fatalf("Peers() = %+v, want only the pinned client %s", peers, pin)
			}
			// The region's display code survives the address, which strips it.
			if peers[0].RelayRegion != "test" {
				t.Fatalf("peer relay region = %q, want %q", peers[0].RelayRegion, "test")
			}
		}
	})
}

// TestForwardPolicy checks the wrapper's part of the forwarding policy: only
// TCP 445, TCP 4700 and UDP 4720 to IPv4 targets are forwardable at all, the
// callback is consulted for those, and a refused flow never reaches the LAN.
func TestForwardPolicy(t *testing.T) {
	t.Run("decision", func(t *testing.T) {
		allowAll := func(Proto, netip.Addr, uint16) bool { return true }
		denyAll := func(Proto, netip.Addr, uint16) bool { return false }
		v4 := netip.MustParseAddr("192.168.1.20")
		v6 := netip.MustParseAddr("2001:db8::20")
		cases := []struct {
			policy Policy
			proto  Proto
			dst    netip.AddrPort
			want   error
		}{
			{allowAll, TCP, netip.AddrPortFrom(v4, 445), nil},
			{allowAll, TCP, netip.AddrPortFrom(v4, 4700), nil},
			{allowAll, UDP, netip.AddrPortFrom(v4, 4720), nil},
			{allowAll, TCP, netip.AddrPortFrom(v4, 4720), errPortNotForwardable},
			{allowAll, UDP, netip.AddrPortFrom(v4, 445), errPortNotForwardable},
			{allowAll, UDP, netip.AddrPortFrom(v4, 4700), errPortNotForwardable},
			{allowAll, TCP, netip.AddrPortFrom(v4, 22), errPortNotForwardable},
			{allowAll, TCP, netip.AddrPortFrom(v4, 80), errPortNotForwardable},
			{allowAll, UDP, netip.AddrPortFrom(v4, 53), errPortNotForwardable},
			{allowAll, TCP, netip.AddrPortFrom(v6, 445), errNotIPv4},
			{allowAll, TCP, netip.AddrPortFrom(netip.AddrFrom16(v4.As16()), 445), errNotIPv4},
			{denyAll, TCP, netip.AddrPortFrom(v4, 445), errPolicyRefused},
			{nil, TCP, netip.AddrPortFrom(v4, 445), errPolicyRefused},
		}
		for _, c := range cases {
			if got := checkForward(c.policy, c.proto, c.dst); got != c.want {
				t.Errorf("checkForward(%s %v) = %v, want %v", c.proto, c.dst, got, c.want)
			}
		}

		// The callback is consulted only for flows the wrapper forwards at all.
		var asked []string
		record := func(p Proto, ip netip.Addr, port uint16) bool {
			asked = append(asked, fmt.Sprintf("%s %v:%d", p, ip, port))
			return true
		}
		checkForward(record, TCP, netip.AddrPortFrom(v4, 22))
		checkForward(record, TCP, netip.AddrPortFrom(v6, 445))
		checkForward(record, UDP, netip.AddrPortFrom(v4, 4720))
		if want := []string{"udp 192.168.1.20:4720"}; fmt.Sprint(asked) != fmt.Sprint(want) {
			t.Errorf("policy consulted for %v, want %v", asked, want)
		}
	})

	forBoth(t, func(t *testing.T, h *harness) {
		// A refused TCP flow fails the dial and never reaches the LAN.
		refusedTCP := []netip.AddrPort{
			netip.AddrPortFrom(lanDevice, 22),       // port never forwarded
			netip.AddrPortFrom(lanDevice, portScan), // a UDP-only port, over TCP
			netip.AddrPortFrom(lanNotAllowed, 445),  // the policy refuses the IP
			netip.AddrPortFrom(lanNotAllowed, 4700), // likewise
		}
		for _, dst := range refusedTCP {
			c, err := h.client.DialTCP(dialCtx(t, 20*time.Second), dst.Addr(), dst.Port())
			if err == nil {
				c.Close()
				t.Errorf("DialTCP(%v) connected; want refused", dst)
				continue
			}
			requireNetworkError(t, "refused DialTCP", err)
			if h.lan.dialled("tcp", dst) {
				t.Errorf("refused TCP flow to %v reached the LAN", dst)
			}
		}
		if !h.consulted(TCP, netip.AddrPortFrom(lanNotAllowed, 445)) {
			t.Errorf("policy was not consulted for tcp %v:445", lanNotAllowed)
		}
		if h.consulted(TCP, netip.AddrPortFrom(lanDevice, 22)) {
			t.Errorf("policy was consulted for a never-forwarded port")
		}

		// A refused UDP flow gets no reply and never reaches the LAN.
		refusedUDP := []netip.AddrPort{
			netip.AddrPortFrom(lanDevice, portSMB),
			netip.AddrPortFrom(lanNotAllowed, portScan),
		}
		for _, dst := range refusedUDP {
			u, err := h.client.DialUDP(dialCtx(t, 20*time.Second), dst.Addr(), dst.Port())
			if err != nil {
				requireNetworkError(t, "refused DialUDP", err)
				continue
			}
			if got, err := udpRoundTrip(u, "probe", 3, 300*time.Millisecond); err == nil {
				t.Errorf("refused UDP flow to %v got reply %q", dst, got)
			}
			u.Close()
			if h.lan.dialled("udp", dst) {
				t.Errorf("refused UDP flow to %v reached the LAN", dst)
			}
		}
		if !h.consulted(UDP, netip.AddrPortFrom(lanNotAllowed, portScan)) {
			t.Errorf("policy was not consulted for udp %v:%d", lanNotAllowed, portScan)
		}

		// Every refusal is reported with its target and reason, and only a
		// policy refusal is marked as one.
		for _, c := range []struct {
			proto    Proto
			dst      netip.AddrPort
			byPolicy bool
		}{
			{TCP, netip.AddrPortFrom(lanDevice, 22), false},
			{TCP, netip.AddrPortFrom(lanDevice, portScan), false},
			{TCP, netip.AddrPortFrom(lanNotAllowed, 445), true},
			{UDP, netip.AddrPortFrom(lanDevice, portSMB), false},
			{UDP, netip.AddrPortFrom(lanNotAllowed, portScan), true},
		} {
			r, ok := h.refusal(c.proto, c.dst)
			if !ok {
				// A refused UDP flow's datagrams may never reach the real
				// server's handler before the client gives up; TCP always does.
				if c.proto == TCP || h.name == "fake" {
					t.Errorf("no refusal reported for %s %v", c.proto, c.dst)
				}
				continue
			}
			if r.ByPolicy != c.byPolicy || r.Reason == "" {
				t.Errorf("refusal for %s %v = %+v, want ByPolicy %v with a reason", c.proto, c.dst, r, c.byPolicy)
			}
		}

		// An allowed flow still goes through after the refusals.
		c, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanDevice, portSMB)
		if err != nil {
			t.Fatalf("allowed DialTCP: %v", err)
		}
		roundTrip(t, c, "still up", "still up")
		c.Close()
	})
}

// TestForwardRefusesDeadTarget: a dial to an allowed target that nothing
// answers on fails rather than connects, so a powered-off ASIAIR behind a live
// remote takes the same offline path as on the LAN.
func TestForwardRefusesDeadTarget(t *testing.T) {
	forBoth(t, func(t *testing.T, h *harness) {
		start := time.Now()
		c, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanDead, portSMB)
		if err == nil {
			c.Close()
			t.Fatalf("DialTCP to a dead target connected")
		}
		requireNetworkError(t, "dead-target DialTCP", err)
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("dead-target dial timed out (%v) instead of being reset", time.Since(start))
		}
		if !h.lan.dialled("tcp", netip.AddrPortFrom(lanDead, portSMB)) {
			t.Errorf("the server never tried the LAN target")
		}
		// An allowed target that does not answer is not a refusal.
		if r, ok := h.refusal(TCP, netip.AddrPortFrom(lanDead, portSMB)); ok {
			t.Errorf("a dead allowed target was reported as refused: %+v", r)
		}
	})
}

// TestTunnelErrorsAreNetworkErrors: dial failures, stream failures after the
// server stops, and the client's startup failures all classify as network
// errors, and SetDeadline aborts a stalled read.
func TestTunnelErrorsAreNetworkErrors(t *testing.T) {
	forBoth(t, func(t *testing.T, h *harness) {
		// SetDeadline aborts a stalled read.
		c, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanSilent, portSMB)
		if err != nil {
			t.Fatalf("DialTCP silent: %v", err)
		}
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		start := time.Now()
		_, err = c.Read(make([]byte, 1))
		requireNetworkError(t, "stalled read", err)
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("stalled read error %v is not a timeout", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("stalled read returned after %v, want about 300ms", d)
		}
		c.Close()

		// A stream open when the server stops fails as a network error.
		live, err := h.client.DialTCP(dialCtx(t, 20*time.Second), lanDevice, portSMB)
		if err != nil {
			t.Fatalf("DialTCP: %v", err)
		}
		roundTrip(t, live, "before", "before")
		if err := h.server.Close(); err != nil {
			t.Fatalf("server Close: %v", err)
		}
		live.SetDeadline(time.Now().Add(2 * time.Second))
		_, werr := io.WriteString(live, "after")
		_, rerr := live.Read(make([]byte, 5))
		if werr == nil && rerr == nil {
			t.Fatalf("stream kept working after the server stopped")
		}
		if rerr != nil {
			requireNetworkError(t, "read after server stop", rerr)
		} else {
			requireNetworkError(t, "write after server stop", werr)
		}
		live.Close()

		// A dial after the server stopped fails as a network error.
		_, err = h.client.DialTCP(dialCtx(t, 3*time.Second), lanDevice, portSMB)
		requireNetworkError(t, "DialTCP after server stop", err)
		_, err = h.client.DialProtocol(dialCtx(t, 3*time.Second))
		requireNetworkError(t, "DialProtocol after server stop", err)

		// A closed client fails every dial as a network error.
		h.client.Close()
		_, err = h.client.DialTCP(dialCtx(t, 3*time.Second), lanDevice, portSMB)
		requireNetworkError(t, "DialTCP on closed client", err)
		_, err = h.client.DialUDP(dialCtx(t, 3*time.Second), lanDevice, portScan)
		requireNetworkError(t, "DialUDP on closed client", err)
		_, err = h.client.DialProtocol(dialCtx(t, 3*time.Second))
		requireNetworkError(t, "DialProtocol on closed client", err)
	})

	t.Run("startup", func(t *testing.T) {
		t.Parallel()
		// A client whose relay never answers: tailcat's startup and connect
		// failures surface as network errors.
		p, _ := mintEnv(t, deadRegion(t))
		c, err := newClientForTests(ClientConfig{Address: p.Address, ClientKey: p.ClientKey, Logger: testLogger(t)})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		defer c.Close()
		_, err = c.DialTCP(dialCtx(t, 2*time.Second), lanDevice, portSMB)
		requireNetworkError(t, "DialTCP with a dead relay", err)
		_, err = c.DialProtocol(dialCtx(t, 2*time.Second))
		requireNetworkError(t, "DialProtocol with a dead relay", err)
		_, err = c.DialUDP(dialCtx(t, 2*time.Second), lanDevice, portScan)
		requireNetworkError(t, "DialUDP with a dead relay", err)
	})

	t.Run("no server", func(t *testing.T) {
		t.Parallel()
		// A live relay but no remote behind it: the meow is never answered.
		relay := startLocalRelay(t)
		p, _ := mintEnv(t, relay.region)
		c, err := newClientForTests(ClientConfig{Address: p.Address, ClientKey: p.ClientKey, Logger: testLogger(t)})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		defer c.Close()
		_, err = c.DialTCP(dialCtx(t, 2*time.Second), lanDevice, portSMB)
		requireNetworkError(t, "DialTCP with no remote", err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("DialTCP with no remote: %v does not unwrap to the context deadline", err)
		}
		// A cancelled dial is a network error too; a bare context.Canceled
		// would not be.
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		_, err = c.DialProtocol(ctx)
		requireNetworkError(t, "cancelled DialProtocol", err)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled DialProtocol: %v does not unwrap to context.Canceled", err)
		}
	})
}

// TestClientAcceptsNoInbound: a connection attempted from the server's side
// toward the client's tunnel address is refused. tailcat's Server has no dial
// method, so the test reaches into the server's netstack, as a malicious
// remote holding the server identity could.
func TestClientAcceptsNoInbound(t *testing.T) {
	t.Parallel()
	h := newRealHarness(t, nil)
	// Bring the tunnel up so the server has the client as a WireGuard peer.
	p, err := h.client.DialProtocol(dialCtx(t, 20*time.Second))
	if err != nil {
		t.Fatalf("DialProtocol: %v", err)
	}
	roundTrip(t, p, "up", "up")
	defer p.Close()

	ts := h.server.(*tailcatServer).ts
	ns := serverNetstack(t, ts)
	// Control: the reflected dialer does open connections, so the refusals
	// below come from the client and not from a dead dialer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	self, err := ns.DialContextTCP(ctx, netip.AddrPortFrom(ts.Addr(), protocolPort))
	cancel()
	if err != nil {
		t.Fatalf("control dial to the server's own protocol port: %v", err)
	}
	roundTrip(t, self, "self", "self")
	self.Close()

	clientAddr := tunnelAddrForKey(h.env.clientPub)
	h.mu.Lock()
	src := h.protoSrc[0].RemoteAddr().String()
	h.mu.Unlock()
	if ap, err := netip.ParseAddrPort(src); err != nil || ap.Addr() != clientAddr {
		t.Fatalf("client's tunnel address is %s, test computed %v", src, clientAddr)
	}
	// sentToClient is what the server has encrypted to the client so far. An
	// idle tunnel sends nothing, so growth across an attempt proves its SYNs
	// left the server for the client, and the refusal is the client's.
	sentToClient := func() int64 {
		ps, ok := ts.Status().Peer[h.env.clientPub]
		if !ok {
			t.Fatal("the client is not a peer of the server")
		}
		return ps.TxBytes
	}
	// One SYN wrapped in IPv6 and WireGuard is well over this.
	const minSYNBytes = 80
	for _, port := range []uint16{protocolPort, portSMB, 22, 80} {
		before := sentToClient()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := ns.DialContextTCP(ctx, netip.AddrPortFrom(clientAddr, port))
		cancel()
		if err == nil {
			c.Close()
			t.Fatalf("server-side dial to the client's tunnel address port %d connected", port)
		}
		// The client's filter drops inbound SYNs silently (no reset), so the
		// attempt can only end at its deadline.
		var ne net.Error
		if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &ne) && ne.Timeout()) {
			t.Fatalf("server-side dial to port %d failed with %v, want a timeout from a silent drop", port, err)
		}
		if sent := sentToClient() - before; sent < minSYNBytes {
			t.Fatalf("server-side dial to port %d sent %d bytes to the client; the attempt never reached it", port, sent)
		}
	}
	// The tunnel itself is unaffected.
	roundTrip(t, p, "still", "still")
}

// serverNetstack returns the tailcat server's netstack, an unexported field.
// If tailcat renames it, this test fails and the gate holds the bump.
func serverNetstack(t *testing.T, s *tailcat.Server) *netstack.Impl {
	t.Helper()
	lb := reflect.ValueOf(s).Elem().FieldByName("lb")
	if !lb.IsValid() || lb.IsNil() {
		t.Fatal("tailcat.Server has no started lb field; tailcat internals changed")
	}
	f := lb.Elem().FieldByName("ns")
	if !f.IsValid() {
		t.Fatal("tailcat locoBackend has no ns field; tailcat internals changed")
	}
	ns, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*netstack.Impl)
	if !ok || ns == nil {
		t.Fatal("tailcat locoBackend.ns is not a *netstack.Impl")
	}
	return ns
}

// tunnelAddrForKey mirrors tailcat's address assignment: Tailscale's ULA
// prefix fd7a:115c:a1e0::/48 filled from the node key.
func tunnelAddrForKey(k key.NodePublic) netip.Addr {
	var a [16]byte
	r := k.Raw32()
	copy(a[:6], []byte{0xfd, 0x7a, 0x11, 0x5c, 0xa1, 0xe0})
	copy(a[6:], r[:10])
	return netip.AddrFrom16(a)
}

// TestRemoteRefusesWithoutPin: a missing, empty or malformed pin refuses to
// start, in the env parser and in both server implementations.
func TestRemoteRefusesWithoutPin(t *testing.T) {
	p, env := mintEnv(t, testRegion)
	payload := decodeEnv(t, p.EnvValue.Reveal())

	for name, client := range map[string]string{
		"missing":   "",
		"malformed": "nodekey:not-hex",
		"wrong tag": strings.Replace(payload.Client, "nodekey:", "privkey:", 1),
		"short":     payload.Client[:len(payload.Client)-2],
		"zero":      "nodekey:" + strings.Repeat("0", 64),
	} {
		bad := payload
		bad.Client = client
		if _, err := ParseEnv(encodeEnv(t, bad)); err == nil {
			t.Errorf("ParseEnv with a %s pin succeeded", name)
		}
	}

	noPin := *env
	noPin.clientPub = key.NodePublic{}
	base := ServerConfig{Policy: allowTestLANPolicy, Protocol: echoProtocol}
	for name, e := range map[string]*Env{"nil env": nil, "zero pin": &noPin} {
		cfg := base
		cfg.Env = e
		if s, err := StartServer(cfg); err == nil {
			s.Close()
			t.Errorf("StartServer with %s succeeded", name)
		}
		if _, err := NewFake(cfg); err == nil {
			t.Errorf("NewFake with %s succeeded", name)
		}
	}
}

func allowTestLANPolicy(p Proto, ip netip.Addr, _ uint16) bool { return allowTestLAN(p, ip) }

// TestRemoteRejectsUnpinnedClient: a client presenting any key but the pinned
// one is never admitted, and its rejection surfaces as an event.
func TestRemoteRejectsUnpinnedClient(t *testing.T) {
	t.Parallel()
	h := newRealHarness(t, nil)

	other := key.NewNode()
	otherText, _ := other.MarshalText()
	intruder, err := newClientForTests(ClientConfig{Address: h.pairing.Address, ClientKey: Secret(otherText), Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer intruder.Close()
	if c, err := intruder.DialProtocol(dialCtx(t, 3*time.Second)); err == nil {
		c.Close()
		t.Fatal("a client with another key was admitted")
	} else {
		requireNetworkError(t, "unpinned DialProtocol", err)
	}
	if c, err := intruder.DialTCP(dialCtx(t, 3*time.Second), lanDevice, portSMB); err == nil {
		c.Close()
		t.Fatal("a client with another key reached the LAN")
	}

	wantKey, _ := other.Public().MarshalText()
	select {
	case r := <-h.rejected:
		if r.Key != string(wantKey) {
			t.Fatalf("rejection event for %s, want %s", r.Key, wantKey)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no rejection event for the unpinned client")
	}
	// A rejected client retries every second; the event is rate-limited.
	select {
	case r := <-h.rejected:
		t.Fatalf("second rejection event within the rate limit: %+v", r)
	case <-time.After(1500 * time.Millisecond):
	}

	// The pinned client is admitted, and it is the only peer.
	p, err := h.client.DialProtocol(dialCtx(t, 20*time.Second))
	if err != nil {
		t.Fatalf("pinned DialProtocol: %v", err)
	}
	roundTrip(t, p, "pinned", "pinned")
	p.Close()
	pin, _ := h.env.clientPub.MarshalText()
	for _, ps := range h.server.Peers() {
		if ps.Key != string(pin) {
			t.Fatalf("server lists a peer other than the pin: %+v", ps)
		}
	}
	if h.lan.dialled("tcp", netip.AddrPortFrom(lanDevice, portSMB)) {
		t.Fatal("the unpinned client's flow reached the LAN")
	}
}

// TestTailcatEmptyAllowlistAdmitsEveryone pins the tailcat behaviour the
// fail-closed pin check exists for: on v0.7.0 an empty AllowedClients list
// admits any client key. If a bump changes that, this test fails so the
// wrapper's assumptions are re-read before the bump merges.
func TestTailcatEmptyAllowlistAdmitsEveryone(t *testing.T) {
	t.Parallel()
	relay := startLocalRelay(t)
	s := &tailcat.Server{Key: key.NewNode(), Region: relay.region.def, Logf: func(string, ...any) {}}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	c := &tailcat.Client{Server: s.TailcatAddr(), Key: key.NewNode(), Logf: func(string, ...any) {}}
	defer c.Close()
	if _, err := c.Ping(dialCtx(t, 20*time.Second)); err != nil {
		t.Fatalf("a random client key was not admitted by an empty allowlist: %v", err)
	}
	if _, ok := s.Status().Peer[c.PublicKey()]; !ok {
		t.Fatal("the random client is not a peer")
	}
}

// TestRejectedKeyLogLineForm pins the debug line tailcat v0.7.0 logs for a
// refused key, which is the only report of it; the real-tunnel rejection test
// pins that tailcat still emits it.
func TestRejectedKeyLogLineForm(t *testing.T) {
	k := key.NewNode().Public()
	line := fmt.Sprintf("ignoring meow from %v: not in allowedClients", k.String())
	m := rejectedMeowLine.FindStringSubmatch(line)
	if m == nil || m[1] != k.String() {
		t.Fatalf("rejected-key line %q not recognised (match %v)", line, m)
	}
	if rejectedMeowLine.MatchString(fmt.Sprintf("got meow from %v", k.String())) {
		t.Fatal("an admitted meow was taken for a rejection")
	}
}

// TestServerHomeConnected: the DERP home connection's state is recovered from
// tailcat's log lines; it reads connected once the relay answers and
// disconnected once the relay goes away.
func TestServerHomeConnected(t *testing.T) {
	t.Parallel()
	h := newRealHarness(t, nil)
	waitFor(t, 20*time.Second, "home region connected", h.server.HomeConnected)
	h.relay.stop()
	waitFor(t, 20*time.Second, "home region disconnected", func() bool { return !h.server.HomeConnected() })

	// A server whose relay never answers never reads connected.
	_, env := mintEnv(t, deadRegion(t))
	s, err := StartServer(ServerConfig{Env: env, Policy: allowTestLANPolicy, Protocol: echoProtocol, Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer s.Close()
	time.Sleep(2 * time.Second)
	if s.HomeConnected() {
		t.Fatal("a server with a dead relay reads connected")
	}
}

// TestClientRelayConnected: the client's own relay connection is recovered
// from tailcat's log lines like the server's. It comes up with the client's
// first dial whether or not the remote is running, so it tells Commander's
// internet apart from a remote that is down; it goes down with the relay and
// with Close.
func TestClientRelayConnected(t *testing.T) {
	t.Parallel()
	h := newRealHarness(t, nil)
	if h.client.RelayConnected() {
		t.Fatal("a client that has not dialled reads connected to its relay")
	}
	conn, err := h.client.DialProtocol(dialCtx(t, 20*time.Second))
	if err != nil {
		t.Fatalf("DialProtocol: %v", err)
	}
	conn.Close()
	waitFor(t, 20*time.Second, "client relay connected", h.client.RelayConnected)
	h.relay.stop()
	waitFor(t, 20*time.Second, "client relay disconnected", func() bool { return !h.client.RelayConnected() })

	// With no remote running, the client still reaches the relay.
	relay := startLocalRelay(t)
	p, _ := mintEnv(t, relay.region)
	c, err := newClientForTests(ClientConfig{Address: p.Address, ClientKey: p.ClientKey, Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.DialProtocol(dialCtx(t, 3*time.Second)); err == nil {
		t.Fatal("a dial to a remote that is not running succeeded")
	}
	waitFor(t, 20*time.Second, "relay connected with no remote", c.RelayConnected)
	c.Close()
	if c.RelayConnected() {
		t.Fatal("a closed client reads connected to its relay")
	}

	// The fake follows the same contract: up while Commander reaches the relay,
	// whether or not its server runs, and down after Close.
	f := newFakeHarness(t)
	fake := f.server.(*Fake)
	if f.client.RelayConnected() {
		t.Fatal("a fake client that has not dialled reads connected to its relay")
	}
	if conn, err := f.client.DialProtocol(dialCtx(t, time.Second)); err != nil {
		t.Fatalf("fake DialProtocol: %v", err)
	} else {
		conn.Close()
	}
	if !f.client.RelayConnected() {
		t.Fatal("fake client does not read connected to its relay")
	}
	fake.Close()
	if !f.client.RelayConnected() {
		t.Fatal("a fake client lost its relay when its server stopped")
	}
	fake.SetRelayReachable(false)
	if f.client.RelayConnected() {
		t.Fatal("fake client reads connected with the relay unreachable")
	}
	fake.SetRelayReachable(true)
	f.client.Close()
	if f.client.RelayConnected() {
		t.Fatal("a closed fake client reads connected to its relay")
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRejectionEventsBounded: rejected keys come from untrusted peers, so the
// rate-limit memory stays within its cap under a flood of fresh keys, every
// fresh key still raises its event, and a blocking callback never stalls the
// caller (tailcat's logf, which runs under tailcat's backend lock).
func TestRejectionEventsBounded(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	got := 0
	s := &tailcatServer{
		cfg: ServerConfig{OnRejectedClient: func(RejectedClient) {
			<-release
			mu.Lock()
			got++
			mu.Unlock()
		}},
		log:     slog.New(slog.DiscardHandler),
		rejSeen: map[string]time.Time{},
	}
	const n = maxTrackedRejections + 100
	done := make(chan struct{})
	go func() {
		for i := range n {
			s.noteRejected(fmt.Sprintf("nodekey:%064x", i))
		}
		s.noteRejected(fmt.Sprintf("nodekey:%064x", n-1)) // rate-limited repeat
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("noteRejected blocked on the callback")
	}
	s.rejMu.Lock()
	tracked := len(s.rejSeen)
	s.rejMu.Unlock()
	if tracked > maxTrackedRejections {
		t.Fatalf("tracking %d keys, cap is %d", tracked, maxTrackedRejections)
	}
	close(release)
	waitFor(t, 10*time.Second, "every fresh key's event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got == n
	})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if got != n {
		t.Fatalf("%d events, want %d (one per fresh key, none for the repeat)", got, n)
	}
}
