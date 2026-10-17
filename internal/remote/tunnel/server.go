package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"
)

// Server is the remote role's side of a tunnel.
type Server interface {
	// Peers reports each admitted client's path. Only the pinned client can
	// ever appear.
	Peers() []PeerStatus
	// HomeConnected reports whether the server's connection to its DERP home
	// region is up, the condition the remote's exit rule and healthcheck use.
	HomeConnected() bool
	// Close stops the server; the clients' connections stop moving bytes.
	Close() error
}

// PeerStatus is one admitted client's path, as the server sees it.
type PeerStatus struct {
	// Key is the client's public key ("nodekey:…").
	Key string
	// Direct is true when traffic flows over a direct UDP path, false when it
	// is relayed through DERP.
	Direct bool
	// RelayRegion is the code of the DERP region the peer is reached through
	// when relayed (and its home region otherwise).
	RelayRegion string
	// LastHandshake is the last WireGuard handshake with the peer; zero if none.
	LastHandshake time.Time
}

// RejectedClient is a client key the server refused to admit. The pinned key is
// never refused, so every rejection is evidence that the address reached
// someone else.
type RejectedClient struct {
	// Key is the refused client's public key ("nodekey:…").
	Key string
}

// RefusedForward is one flow the forwarding decision refused.
type RefusedForward struct {
	Proto  Proto
	Target netip.AddrPort
	// Reason says why, in words fit for the remote's warnings list.
	Reason string
	// ByPolicy is true when the wrapper would forward the flow and the
	// remote's policy refused it (the target is not on its allowlist), false
	// when the port or address family is never forwarded.
	ByPolicy bool
}

// ServerConfig is what the remote role passes to start a server.
type ServerConfig struct {
	// Env is the parsed pairing; its pinned client key is the only key the
	// server admits. Required.
	Env *Env
	// Policy decides which LAN targets are forwarded, among the ones the
	// wrapper forwards at all. Required.
	Policy Policy
	// Protocol handles one remote-protocol connection; it owns the conn and
	// must close it. It runs concurrently for concurrent connections. Required.
	Protocol func(net.Conn)
	// OnRejectedClient, if set, is called when a client key other than the
	// pinned one tries to connect, at most once per key per
	// rejectedEventInterval (a refused client retries every second). It runs
	// on its own goroutine, so a slow callback never stalls admission.
	OnRejectedClient func(RejectedClient)
	// OnRefusedForward, if set, is called for every flow the forwarding
	// decision refuses, after it is logged. A target that is allowed but does
	// not answer (a powered-off device) is not a refusal and is not reported.
	// It runs on the flow's own goroutine, so it must not block.
	OnRefusedForward func(RefusedForward)
	// DialLAN dials a forwarded flow's LAN target, after the forwarding
	// decision; nil uses a net.Dialer. Tests use it to map targets onto local
	// listeners.
	DialLAN func(ctx context.Context, network, address string) (net.Conn, error)
	// Logger receives the server's logs; nil uses slog.Default().
	Logger *slog.Logger
}

const (
	// protocolPort is the reserved in-tunnel port of the remote protocol, on
	// the server's own tunnel address. tailcat dispatches a flow to the
	// server's own address to OnTCP, never to OnTCPForward, so this target is
	// never forwarded to the LAN.
	protocolPort = 4747

	// udpIdleTimeout closes a forwarded UDP flow this long after its last
	// datagram. A scan_air probe is one request and one reply, so tailcat's
	// two-minute default would only hold sockets open on the remote.
	udpIdleTimeout = 15 * time.Second

	// lanDialTimeout bounds the dial to a LAN target that runs before a
	// forwarded TCP flow is accepted. A powered-off device fails within it,
	// and the client's dial is then reset instead of connecting.
	lanDialTimeout = 5 * time.Second

	// rejectedEventInterval rate-limits rejection events per key.
	rejectedEventInterval = time.Minute
	// maxTrackedRejections bounds the per-key rate-limit memory.
	maxTrackedRejections = 1024
)

// validate checks the parts of a config every server implementation needs. A
// server without a pin would admit everyone on tailcat v0.7.0, so it fails
// closed here.
func (cfg *ServerConfig) validate() error {
	switch {
	case cfg.Env == nil:
		return errors.New("tunnel: no pairing; refusing to start a remote without its pinned client key")
	case cfg.Env.clientPub.IsZero():
		return errors.New("tunnel: pairing has no pinned client key; refusing to start")
	case cfg.Env.serverKey.IsZero():
		return errors.New("tunnel: pairing has no server key; refusing to start")
	case cfg.Policy == nil:
		return errors.New("tunnel: no forwarding policy; refusing to start")
	case cfg.Protocol == nil:
		return errors.New("tunnel: no remote-protocol handler; refusing to start")
	}
	return nil
}

func (cfg *ServerConfig) logger() *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.Default()
}

// refused logs a flow the forwarding decision refused and reports it to
// OnRefusedForward. The real server and the fake share it, so both report
// exactly the same refusals.
func (cfg *ServerConfig) refused(log *slog.Logger, proto Proto, dst netip.AddrPort, err error) {
	log.Warn("tunnel: refused forward", "proto", proto, "target", dst.String(), "reason", err.Error())
	if cb := cfg.OnRefusedForward; cb != nil {
		cb(RefusedForward{Proto: proto, Target: dst, Reason: err.Error(), ByPolicy: errors.Is(err, errPolicyRefused)})
	}
}

func (cfg *ServerConfig) dialLAN() func(ctx context.Context, network, address string) (net.Conn, error) {
	if cfg.DialLAN != nil {
		return cfg.DialLAN
	}
	var d net.Dialer
	return d.DialContext
}

// tailcatServer is the real server role.
type tailcatServer struct {
	ts      *tailcat.Server
	cfg     ServerConfig
	log     *slog.Logger
	red     *redactor
	homeID  int
	home    atomic.Bool
	closed  atomic.Bool
	rejMu   sync.Mutex
	rejSeen map[string]time.Time
}

// StartServer starts the remote's tunnel server from its pairing. It refuses to
// start without a well-formed pinned client key.
func StartServer(cfg ServerConfig) (Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	env := cfg.Env
	s := &tailcatServer{
		cfg: cfg,
		log: cfg.logger(),
		red: newRedactor(
			[]string{env.value, string(env.addr)},
			[][]byte{nodePrivateRaw(env.serverKey), pskRaw(env.ci.PresharedKey)},
		),
		rejSeen: map[string]time.Time{},
	}
	ts := &tailcat.Server{
		Key:          env.serverKey,
		PresharedKey: env.ci.PresharedKey,
		// The pin: on v0.7.0 a non-empty list admits only these keys, and
		// validate guarantees it is non-empty.
		AllowedClients: []key.NodePublic{env.clientPub},
		Logf:           tailcatLogf(s.log, "server", s.red, s.observe),
		OnTCP:          s.onTCP,
		OnTCPForward:   s.onTCPForward,
		OnUDPForward:   s.onUDPForward,
		// Defence in depth: the packet filter admits only the protocol port on
		// the server's own address; OnTCP is the per-connection gate behind it.
		ServedTCPPorts: []filter.PortRange{{First: protocolPort, Last: protocolPort}},
		UDPIdleTimeout: udpIdleTimeout,
	}
	// parseAddr guarantees exactly one embedded region, so tailcat starts
	// without fetching a relay map or running a netcheck.
	ts.Region = env.ci.Region[0]
	s.homeID = int(ts.Region.RegionID)
	if err := ts.Start(); err != nil {
		return nil, errors.New("tunnel: start server: " + s.red.redact(err.Error()))
	}
	s.ts = ts
	s.log.Info("tunnel: server started", "fingerprint", env.Fingerprint(), "relay_region", ts.Region.RegionCode)
	return s, nil
}

func (s *tailcatServer) onTCP(port uint16) func(net.Conn) {
	if port != protocolPort {
		return nil
	}
	return s.cfg.Protocol
}

// onTCPForward runs before the client's handshake completes. It dials the LAN
// target first and returns nil (a reset) when that fails, so a dial through the
// tunnel to a powered-off device fails instead of connecting.
func (s *tailcatServer) onTCPForward(dst netip.AddrPort) func(net.Conn) {
	if err := checkForward(s.cfg.Policy, TCP, dst); err != nil {
		s.cfg.refused(s.log, TCP, dst, err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lanDialTimeout)
	defer cancel()
	upstream, err := s.cfg.dialLAN()(ctx, "tcp", dst.String())
	if err != nil {
		s.log.Info("tunnel: forward target unreachable", "proto", TCP, "target", dst.String(), "error", err)
		return nil
	}
	s.log.Debug("tunnel: forwarding", "proto", TCP, "target", dst.String())
	return func(c net.Conn) { tailcat.ProxyConns(c, upstream) }
}

func (s *tailcatServer) onUDPForward(dst netip.AddrPort) func(tailcat.ConnPacketConn) {
	if err := checkForward(s.cfg.Policy, UDP, dst); err != nil {
		s.cfg.refused(s.log, UDP, dst, err)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), lanDialTimeout)
	defer cancel()
	upstream, err := s.cfg.dialLAN()(ctx, "udp", dst.String())
	if err != nil {
		s.log.Info("tunnel: forward target unreachable", "proto", UDP, "target", dst.String(), "error", err)
		return nil
	}
	s.log.Debug("tunnel: forwarding", "proto", UDP, "target", dst.String())
	return func(c tailcat.ConnPacketConn) { proxyDatagrams(c, upstream) }
}

// proxyDatagrams copies datagrams both ways between two connected datagram
// conns until either fails (tailcat closes the tunnel side once the flow has
// been idle for udpIdleTimeout), then closes both.
func proxyDatagrams(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	pump := func(dst, src net.Conn) {
		defer closeBoth()
		buf := make([]byte, 64*1024)
		for {
			n, err := src.Read(buf)
			if err != nil {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}
	go pump(a, b)
	pump(b, a)
}

// observe recovers from tailcat's log lines what v0.7.0 exposes no API for:
// rejected client keys and the DERP home connection's state.
func (s *tailcatServer) observe(line string) {
	if m := rejectedMeowLine.FindStringSubmatch(line); m != nil {
		s.noteRejected(m[1])
		return
	}
	if m := derpConnectedLine.FindStringSubmatch(line); m != nil {
		if id, ok := parseRegionID(m); ok && id == s.homeID {
			if !s.home.Swap(true) {
				s.log.Info("tunnel: relay home region connected", "relay_region", id)
			}
		}
		return
	}
	if m := derpLostLine.FindStringSubmatch(line); m != nil {
		if id, ok := parseRegionID(m); ok && id == s.homeID {
			if s.home.Swap(false) {
				s.log.Warn("tunnel: relay home region disconnected", "relay_region", id)
			}
		}
	}
}

func (s *tailcatServer) noteRejected(k string) {
	now := time.Now()
	s.rejMu.Lock()
	if last, ok := s.rejSeen[k]; ok && now.Sub(last) < rejectedEventInterval {
		s.rejMu.Unlock()
		return
	}
	if len(s.rejSeen) >= maxTrackedRejections {
		for old, at := range s.rejSeen {
			if now.Sub(at) >= rejectedEventInterval {
				delete(s.rejSeen, old)
			}
		}
	}
	// Keys come from untrusted peers, so a flood of fresh ones evicts the
	// oldest entry rather than growing the map past its cap.
	if len(s.rejSeen) >= maxTrackedRejections {
		var oldest string
		var oldestAt time.Time
		for old, at := range s.rejSeen {
			if oldest == "" || at.Before(oldestAt) {
				oldest, oldestAt = old, at
			}
		}
		delete(s.rejSeen, oldest)
	}
	s.rejSeen[k] = now
	s.rejMu.Unlock()

	s.log.Warn("tunnel: rejected a client key other than the pinned one", "client_key", k)
	// tailcat logs the rejection while holding its backend lock, so the
	// callback must not run on this goroutine: a blocking one would stall
	// every meow, the pinned client's included.
	if cb := s.cfg.OnRejectedClient; cb != nil {
		go cb(RejectedClient{Key: k})
	}
}

func (s *tailcatServer) Peers() []PeerStatus {
	if s.closed.Load() {
		return nil
	}
	st := s.ts.Status()
	out := make([]PeerStatus, 0, len(st.Peer))
	for k, ps := range st.Peer {
		text, _ := k.MarshalText()
		out = append(out, PeerStatus{
			Key:           string(text),
			Direct:        ps.CurAddr != "",
			RelayRegion:   ps.Relay,
			LastHandshake: ps.LastHandshake,
		})
	}
	slices.SortFunc(out, func(a, b PeerStatus) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func (s *tailcatServer) HomeConnected() bool {
	return !s.closed.Load() && s.home.Load()
}

func (s *tailcatServer) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.home.Store(false)
	if err := s.ts.Close(); err != nil {
		return errors.New("tunnel: close server: " + s.red.redact(err.Error()))
	}
	return nil
}

var _ Server = (*tailcatServer)(nil)
