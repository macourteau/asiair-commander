package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// Client is Commander's side of a tunnel to one remote.
//
// Every error a dial returns is a *net.OpError, tailcat startup and connect
// failures included, so smb1.IsNetworkError classifies it as a network failure.
// Returned conns honour SetDeadline. The client accepts no inbound flow.
//
// A client does not recover by itself after its remote restarts; the owner
// detects that and builds a fresh client.
type Client interface {
	// DialTCP opens a TCP connection to ip:port on the remote's LAN.
	DialTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error)
	// DialUDP opens a connected UDP flow to ip:port on the remote's LAN. Each
	// Write sends one datagram and each Read receives one.
	DialUDP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error)
	// DialProtocol opens one remote-protocol connection. Several may be open
	// at once.
	DialProtocol(ctx context.Context) (net.Conn, error)
	// Close tears the tunnel down; open conns stop moving bytes and later
	// dials fail.
	Close() error
	// RelayConnected reports whether the client holds its connection to the
	// remote's relay region. It does not depend on the remote: the client
	// connects to the relay whether or not the remote is running, so it tells
	// Commander's own internet apart from a remote that is down. False before
	// the first dial brings the client up, and after Close.
	RelayConnected() bool
}

// ClientConfig is what Commander stores for a remote.
type ClientConfig struct {
	// Address is the remote's tailcat address.
	Address Secret
	// ClientKey is Commander's private key for this remote.
	ClientKey Secret
	// Logger receives the client's logs; nil uses slog.Default().
	Logger *slog.Logger
}

// tailcatClient is the real client role.
type tailcatClient struct {
	tc  *tailcat.Client
	red *redactor
	log *slog.Logger

	// homeID is the relay region the address names, and relay whether the
	// client's connection to it is up, recovered from tailcat's log lines as
	// the server's home connection is.
	homeID int
	relay  atomic.Bool

	mu     sync.Mutex
	closed bool
}

// NewClient builds a client from a stored pairing. It validates the address and
// key but does no network access: the tunnel comes up on the first dial. Like
// ParseEnv, it refuses an address whose relay region is a test relay, whose TLS
// certificate tailcat never verifies, except in a build made with the
// remotetest tag (see testRelayAllowed).
func NewClient(cfg ClientConfig) (Client, error) {
	return newClient(cfg, testRelayAllowed)
}

// newClient is NewClient; allowTestRelay admits a test relay, which only tests
// running a local relay ever need.
func newClient(cfg ClientConfig, allowTestRelay bool) (Client, error) {
	if err := CheckDebugAddr(nil); err != nil {
		return nil, err
	}
	ci, err := parseAddr(tailcat.Addr(cfg.Address))
	if err != nil {
		return nil, err
	}
	if !allowTestRelay && isTestRelay(ci) {
		return nil, errTestRelay
	}
	var k key.NodePrivate
	if err := k.UnmarshalText([]byte(cfg.ClientKey)); err != nil || k.IsZero() {
		return nil, errors.New("tunnel: malformed client key")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	red := newRedactor(
		[]string{string(cfg.Address), string(cfg.ClientKey)},
		[][]byte{nodePrivateRaw(k), pskRaw(ci.PresharedKey)},
	)
	c := &tailcatClient{red: red, log: log, homeID: int(ci.Region[0].RegionID)}
	c.tc = &tailcat.Client{
		Server: tailcat.Addr(cfg.Address),
		Key:    k,
		Logf:   tailcatLogf(log, "client", red, c.observe),
	}
	return c, nil
}

// observe follows the client's relay connection from tailcat's log lines; the
// same lines, pinned by the same behaviour tests, give the server's.
func (c *tailcatClient) observe(line string) {
	if m := derpConnectedLine.FindStringSubmatch(line); m != nil {
		if id, ok := parseRegionID(m); ok && id == c.homeID && !c.relay.Swap(true) {
			c.log.Debug("tunnel: client connected to its relay region", "relay_region", id)
		}
		return
	}
	if m := derpLostLine.FindStringSubmatch(line); m != nil {
		if id, ok := parseRegionID(m); ok && id == c.homeID && c.relay.Swap(false) {
			c.log.Debug("tunnel: client lost its relay region", "relay_region", id)
		}
	}
}

func (c *tailcatClient) RelayConnected() bool { return !c.isClosed() && c.relay.Load() }

func (c *tailcatClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *tailcatClient) DialTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	dst := netip.AddrPortFrom(ip, port)
	target := targetAddr(TCP, dst)
	if c.isClosed() {
		return nil, opError("dial", TCP, target, errTunnelClosed, c.red)
	}
	conn, err := c.tc.DialTCP(ctx, dst)
	return c.finishDial(conn, err, TCP, target)
}

func (c *tailcatClient) DialUDP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	dst := netip.AddrPortFrom(ip, port)
	target := targetAddr(UDP, dst)
	if c.isClosed() {
		return nil, opError("dial", UDP, target, errTunnelClosed, c.red)
	}
	conn, err := c.tc.DialUDP(ctx, dst)
	if err != nil {
		return nil, opError("dial", UDP, target, err, c.red)
	}
	return c.finishDial(conn, nil, UDP, target)
}

func (c *tailcatClient) DialProtocol(ctx context.Context) (net.Conn, error) {
	if c.isClosed() {
		return nil, opError("dial", TCP, protocolAddr{}, errTunnelClosed, c.red)
	}
	conn, err := c.tc.DialTCPPort(ctx, protocolPort)
	return c.finishDial(conn, err, TCP, protocolAddr{})
}

// finishDial wraps a dial's error, and undoes a dial that raced with Close: a
// first dial can bring the tunnel up after Close found nothing to tear down,
// so the stack is closed again here (tailcat's Close is idempotent).
func (c *tailcatClient) finishDial(conn net.Conn, err error, proto Proto, target net.Addr) (net.Conn, error) {
	if err != nil {
		return nil, opError("dial", proto, target, err, c.red)
	}
	if c.isClosed() {
		conn.Close()
		c.tc.Close()
		return nil, opError("dial", proto, target, errTunnelClosed, c.red)
	}
	return conn, nil
}

func (c *tailcatClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	if err := c.tc.Close(); err != nil {
		return errors.New("tunnel: close client: " + c.red.redact(err.Error()))
	}
	return nil
}

var _ Client = (*tailcatClient)(nil)
