package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"

	"tailscale.com/tailcfg"
)

// FakeRegion is a relay region for pairings used only with the Fake, by tests
// outside this package that need a well-formed env value. Its relay host is in
// the .invalid domain, so a real tunnel minted on it never connects.
func FakeRegion() Region {
	return Region{ID: 1, Code: "fake", Name: "Fake", def: &tailcfg.DERPRegion{
		RegionID:   1,
		RegionCode: "fake",
		RegionName: "Fake",
		Nodes:      []*tailcfg.DERPNode{{Name: "f1", RegionID: 1, HostName: "derp.invalid"}},
	}}
}

// Fake is an in-memory tunnel for hermetic tests: a server and the clients
// connected to it, with no tailcat underneath. It applies the same forwarding
// decision as the real server (checkForward), dials LAN targets through the
// config's DialLAN, and surfaces the same error types as the real client.
type Fake struct {
	cfg ServerConfig

	mu      sync.Mutex
	closed  bool
	dialled bool
	clients map[*fakeClient]struct{}
	// relayDown stands for Commander losing its path to the relay; a real
	// client's relay connection does not depend on the server, so closing the
	// fake leaves it alone.
	relayDown bool
}

// NewFake starts a fake server from the same config as StartServer, and
// refuses the same configs.
func NewFake(cfg ServerConfig) (*Fake, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Fake{cfg: cfg, clients: map[*fakeClient]struct{}{}}, nil
}

// NewClient returns a client connected to the fake, as a client built from the
// pairing would be.
func (f *Fake) NewClient() Client {
	c := &fakeClient{f: f, conns: map[net.Conn]struct{}{}}
	f.mu.Lock()
	f.clients[c] = struct{}{}
	f.mu.Unlock()
	return c
}

// Reject reports a refused client key, as the real server does when a client
// other than the pinned one tries to connect.
func (f *Fake) Reject(clientKey string) {
	// Asynchronous, as the real server delivers it.
	if cb := f.cfg.OnRejectedClient; cb != nil {
		go cb(RejectedClient{Key: clientKey})
	}
}

// Peers reports the pinned client, on a direct path, once one has dialled.
func (f *Fake) Peers() []PeerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || !f.dialled {
		return nil
	}
	text, _ := f.cfg.Env.clientPub.MarshalText()
	return []PeerStatus{{Key: string(text), Direct: true}}
}

// HomeConnected is true until the fake is closed.
func (f *Fake) HomeConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.closed
}

// SetRelayReachable sets whether the fake's clients reach the relay
// (RelayConnected), as Commander's own internet would.
func (f *Fake) SetRelayReachable(up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relayDown = !up
}

// Close stops the fake: every open conn fails and later dials fail.
func (f *Fake) Close() error {
	f.mu.Lock()
	f.closed = true
	clients := make([]*fakeClient, 0, len(f.clients))
	for c := range f.clients {
		clients = append(clients, c)
	}
	f.mu.Unlock()
	for _, c := range clients {
		c.closeConns()
	}
	return nil
}

func (f *Fake) up() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.dialled = true
	return true
}

var _ Server = (*Fake)(nil)

// fakeClient is a client of a Fake.
type fakeClient struct {
	f *Fake

	mu     sync.Mutex
	closed bool
	// started is set by the first dial, which is what brings a real client,
	// and its relay connection, up.
	started bool
	conns   map[net.Conn]struct{}
}

// errFakeReset stands for the reset the real server sends for a refused or
// unreachable TCP target.
var errFakeReset = errors.New("connection was refused")

func (c *fakeClient) DialTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	dst := netip.AddrPortFrom(ip, port)
	target := targetAddr(TCP, dst)
	if !c.usable() {
		return nil, opError("dial", TCP, target, errTunnelClosed, nil)
	}
	if err := checkForward(c.f.cfg.Policy, TCP, dst); err != nil {
		c.f.cfg.refused(c.f.cfg.logger(), TCP, dst, err)
		return nil, opError("dial", TCP, target, errFakeReset, nil)
	}
	conn, err := c.dialLAN(ctx, "tcp", dst)
	if err != nil {
		if ctx.Err() != nil {
			return nil, opError("dial", TCP, target, ctx.Err(), nil)
		}
		return nil, opError("dial", TCP, target, errFakeReset, nil)
	}
	return c.track(&opConn{Conn: conn, proto: TCP}), nil
}

// DialUDP, like the real client, succeeds locally even for a flow the server
// refuses: the refused flow's datagrams are dropped and its reads time out.
func (c *fakeClient) DialUDP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	dst := netip.AddrPortFrom(ip, port)
	target := targetAddr(UDP, dst)
	if !c.usable() {
		return nil, opError("dial", UDP, target, errTunnelClosed, nil)
	}
	if err := checkForward(c.f.cfg.Policy, UDP, dst); err != nil {
		c.f.cfg.refused(c.f.cfg.logger(), UDP, dst, err)
		return c.track(blackhole()), nil
	}
	conn, err := c.dialLAN(ctx, "udp", dst)
	if err != nil {
		return c.track(blackhole()), nil
	}
	return c.track(&opConn{Conn: conn, proto: UDP}), nil
}

func (c *fakeClient) DialProtocol(ctx context.Context) (net.Conn, error) {
	if !c.usable() {
		return nil, opError("dial", TCP, protocolAddr{}, errTunnelClosed, nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, opError("dial", TCP, protocolAddr{}, err, nil)
	}
	client, server := net.Pipe()
	go c.f.cfg.Protocol(server)
	return c.track(&opConn{Conn: client, proto: TCP, peer: server}), nil
}

func (c *fakeClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.closeConns()
	return nil
}

func (c *fakeClient) RelayConnected() bool {
	c.mu.Lock()
	up := c.started && !c.closed
	c.mu.Unlock()
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	return up && !c.f.relayDown
}

func (c *fakeClient) usable() bool {
	c.mu.Lock()
	c.started = true
	closed := c.closed
	c.mu.Unlock()
	return !closed && c.f.up()
}

func (c *fakeClient) dialLAN(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, lanDialTimeout)
	defer cancel()
	return c.f.cfg.dialLAN()(ctx, network, dst.String())
}

func (c *fakeClient) track(conn net.Conn) net.Conn {
	c.mu.Lock()
	c.conns[conn] = struct{}{}
	c.mu.Unlock()
	return conn
}

func (c *fakeClient) closeConns() {
	c.mu.Lock()
	conns := c.conns
	c.conns = map[net.Conn]struct{}{}
	c.mu.Unlock()
	for conn := range conns {
		conn.Close()
	}
}

// opConn makes a fake conn fail like a tunnelled one: every error other than
// EOF is a *net.OpError.
type opConn struct {
	net.Conn
	proto Proto
	peer  net.Conn // the server's end of a pipe, closed with this end
}

func (c *opConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, c.wrap("read", err)
}

func (c *opConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, c.wrap("write", err)
}

func (c *opConn) Close() error {
	if c.peer != nil {
		c.peer.Close()
	}
	return c.Conn.Close()
}

func (c *opConn) wrap(op string, err error) error {
	var oe *net.OpError
	if err == nil || err == io.EOF || errors.As(err, &oe) {
		return err
	}
	return &net.OpError{Op: op, Net: string(c.proto), Addr: c.Conn.RemoteAddr(), Err: err}
}

// blackhole is a datagram conn whose writes vanish and whose reads only end at
// a deadline or on close, like a UDP flow the server refused.
func blackhole() net.Conn {
	a, b := net.Pipe()
	go func() {
		io.Copy(io.Discard, b)
		b.Close()
	}()
	return &opConn{Conn: a, proto: UDP, peer: b}
}
