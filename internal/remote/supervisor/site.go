package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
	"github.com/macourteau/asiair-commander/internal/remote/tunnel"
)

// site is one remote's netsite.Site: every dial, probe and broadcast goes
// through the remote's current tunnel client, which the supervisor replaces
// when it rebuilds. It is up only while the remote's heartbeat succeeds and the
// broadcast round that follows each (re)connect has completed.
type site struct {
	id      netsite.ID
	now     func() time.Time
	log     *slog.Logger
	limiter *netsite.ReadLimiter
	lat     netsite.LatencyEstimator
	// stall overrides RemoteStallWindow when nonzero, so a test can exercise
	// the stall rule in milliseconds. Production never sets it.
	stall time.Duration

	mu     sync.RWMutex
	name   string
	client tunnel.Client
	proto  *proto.Client
	up     bool
	// peerWhole and peerData are the digest-algorithm versions the remote's
	// last hello reported; known is false before any hello.
	peerWhole, peerData int
	peerKnown           bool

	// activity is when a tunnelled connection last moved a byte, or a remote
	// hash last reported progress (unix nanoseconds on the supervisor's clock).
	// The rebuild rule reads it: a client is never rebuilt while it is recent.
	activity atomic.Int64
}

var _ netsite.Site = (*site)(nil)

// The registry applies the remote's read limiter through LimitReads.
var _ netsite.LimitsReads = (*site)(nil)

func newSite(id netsite.ID, now func() time.Time, log *slog.Logger) *site {
	return &site{id: id, now: now, log: log, limiter: netsite.NewReadLimiter()}
}

func (s *site) ID() netsite.ID { return s.id }

// current returns the tunnel client and its remote-protocol client, both nil
// before the first is built.
func (s *site) current() (tunnel.Client, *proto.Client) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client, s.proto
}

// swap installs a fresh client (nil to drop it) and marks the site down; the
// caller decides when the site comes up again. It returns a func closing the
// replaced client, which ends every connection through it: a caller holding a
// lock under which no network call may be made (closing a tunnel client tears
// down its netstack) runs it after unlocking, any other runs it at once.
func (s *site) swap(c tunnel.Client, pc *proto.Client) (closeOld func()) {
	s.mu.Lock()
	oldC, oldP := s.client, s.proto
	s.client, s.proto, s.up = c, pc, false
	s.mu.Unlock()
	return func() {
		if oldP != nil {
			oldP.Close()
		}
		if oldC != nil {
			oldC.Close()
		}
	}
}

func (s *site) setUp(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up = up
}

func (s *site) setPeerDigests(whole, data int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerWhole, s.peerData, s.peerKnown = whole, data, true
}

// PeerDigestVersions is the whole-file and data-unit digest-algorithm versions
// the remote last reported, for the pre-delete hash to choose between the
// remote's digest and streaming the file itself; ok is false before any hello.
func (s *site) PeerDigestVersions() (whole, data int, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peerWhole, s.peerData, s.peerKnown
}

// touch records that the tunnel moved bytes.
func (s *site) touch() { s.activity.Store(s.now().UnixNano()) }

// lastActivity is when the tunnel last moved bytes; zero if never.
func (s *site) lastActivity() time.Time {
	ns := s.activity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// HashProgress is the hook a remote hash's progress feeds: each report counts
// as the tunnel moving bytes, so a hash still reading is never cut by a client
// rebuild.
func (s *site) HashProgress(int64) { s.touch() }

// Hash asks the remote to measure one file in its own SMB session, with this
// site's SMB budget as the stall window; every progress report reaches
// HashProgress before onProgress. It is the transport only: what the reply
// means for a delete is the caller's.
func (s *site) Hash(ctx context.Context, req proto.HashRequest, onProgress func(int64)) (proto.HashReply, error) {
	_, pc := s.current()
	if pc == nil {
		return proto.HashReply{}, s.unavailable("no tunnel client")
	}
	return pc.Hash(ctx, newCallID(), req, proto.HashOptions{
		Stall: netsite.SMBBudget(s),
		OnProgress: func(n int64) {
			s.HashProgress(n)
			if onProgress != nil {
				onProgress(n)
			}
		},
	})
}

// stopPoll is how often a remote measurement checks its request's stop: about
// as often as the local read meets a buffer boundary on a slow link, so a Pause
// waits no longer for a remote hash than for a local one.
const stopPoll = 250 * time.Millisecond

// MeasureDigests has the remote measure the file in its own SMB session when
// both sides run the same whole-file digest algorithm. Otherwise the remote's
// digest could not be compared with the recorded anchor, so Commander streams
// the file itself through the tunnel, in its own session (req.Local). The
// remote's data-unit digest is recordable only when that algorithm matches too.
func (s *site) MeasureDigests(ctx context.Context, req netsite.DigestRequest) (netsite.DigestResult, error) {
	whole, data, ok := s.PeerDigestVersions()
	if !ok || whole != header.WholeDigestVersion {
		s.log.Debug("remote: whole-file digest algorithms differ; streaming the pre-delete hash through the tunnel",
			"remote_version", whole, "known", ok, "local_version", header.WholeDigestVersion)
		return netsite.MeasureLocally(req)
	}

	stop := req.Stop
	if stop == nil {
		stop = func() bool { return false }
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopped atomic.Bool
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(stopPoll)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if stop() {
					stopped.Store(true)
					// Cancelling closes the call's connection, which stops the
					// remote's read.
					cancel()
					return
				}
			}
		}
	}()

	hr, err := s.Hash(ctx, proto.HashRequest{
		IP: req.IP, Share: req.Share, Path: req.Path,
		User: req.User, Password: req.Password,
	}, nil)
	if err != nil {
		if stopped.Load() || stop() {
			return netsite.DigestResult{}, fmt.Errorf("%w: %v", netsite.ErrStopped, err)
		}
		return netsite.DigestResult{}, err
	}
	// The reply is network data. A digest that is not one the local tee could
	// produce would be compared (a false three-way mismatch that holds a good
	// frame) or recorded (a corrupt exposure identity), so it is a failed
	// measurement, which defers the frame.
	if !wellFormedDigest(hr.Whole) || (hr.Data != "" && !wellFormedDigest(hr.Data)) {
		s.log.Warn("remote: hash reply carries a malformed digest; deferring the frame",
			"whole_len", len(hr.Whole), "data_len", len(hr.Data))
		return netsite.DigestResult{}, errMalformedDigest
	}
	return netsite.DigestResult{
		Digests:        netsite.Digests{Whole: hr.Whole, Data: hr.Data},
		Stated:         true,
		Size:           hr.Size,
		Mtime:          hr.Mtime(),
		Probed:         true,
		GUID:           hr.GUID,
		CPUID:          hr.CPUID,
		DataRecordable: data == header.DataDigestVersion,
	}, nil
}

// errMalformedDigest is a hash reply whose digests are not well-formed.
var errMalformedDigest = errors.New("remote: hash reply carries a malformed digest")

// wellFormedDigest reports whether d has the shape header.DigestTee's digests
// have: lowercase hex of a SHA-256 sum.
func wellFormedDigest(d string) bool {
	if len(d) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *site) unavailable(reason string) error {
	return &netsite.UnavailableError{Site: s.id, Reason: reason}
}

// target parses a device address on the remote's LAN. Remote endpoints always
// carry an IP literal (netsite.Parse requires one), so anything else is a bug
// that fails closed.
func (s *site) target(host string, port int) (netip.Addr, uint16, error) {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, 0, s.unavailable("address is not an IP literal")
	}
	if port <= 0 || port > 0xffff {
		return netip.Addr{}, 0, s.unavailable("port out of range")
	}
	return ip, uint16(port), nil
}

// DialTCP dials through the current tunnel client. Its errors are the tunnel's
// own *net.OpError, or an UnavailableError when there is no client.
func (s *site) DialTCP(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	c, _ := s.current()
	if c == nil {
		return nil, s.unavailable("no tunnel client")
	}
	ip, p, err := s.target(host, port)
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	conn, err := c.DialTCP(ctx, ip, p)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, s: s}, nil
}

// ProbeUDP sends req on a forwarded UDP flow and reads replies until accept
// takes one or timeout elapses. The flow is connected to host, so every reply
// comes from it.
func (s *site) ProbeUDP(ctx context.Context, host string, port int, req []byte, timeout time.Duration, accept func(reply []byte, srcIP string) bool) bool {
	c, _ := s.current()
	if c == nil {
		return false
	}
	ip, p, err := s.target(host, port)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.DialUDP(ctx, ip, p)
	if err != nil {
		return false
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	// Network deadlines run on the wall clock whatever the supervisor's clock.
	conn.SetDeadline(time.Now().Add(timeout))
	cc := &countingConn{Conn: conn, s: s}
	if _, err := cc.Write(req); err != nil {
		return false
	}
	buf := make([]byte, 4096)
	for {
		n, err := cc.Read(buf)
		if err != nil || ctx.Err() != nil {
			return false
		}
		if accept(buf[:n], host) {
			return true
		}
	}
}

// Broadcast runs the remote's own scan_air round on its LAN.
func (s *site) Broadcast(ctx context.Context) ([]netsite.Datagram, error) {
	_, pc := s.current()
	if pc == nil {
		return nil, s.unavailable("no tunnel client")
	}
	return pc.Broadcast(ctx, newCallID())
}

func (s *site) State() netsite.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.up {
		return netsite.StateUp
	}
	return netsite.StateDown
}

// Costs are a remote's: its measured latency, the remote stall window and
// handshake allowance, and its cap. RepliesMatchSource is true: the remote
// forwards only to an address whose scan_air reply came from it. MinLatency is
// nonzero once the site is up: it comes up only after a hello, whose round trip
// is a sample, and samples are floored above zero.
func (s *site) Costs() netsite.Costs {
	return netsite.Costs{
		Latency:            s.lat.Latency(),
		MinLatency:         s.lat.Min(),
		StallWindow:        s.stallWindow(),
		HandshakeAllowance: netsite.RemoteHandshakeAllowance,
		RepliesMatchSource: true,
		RateLimitMBps:      s.limiter.MBps(),
		Remote:             true,
		RemoteName:         s.displayName(),
	}
}

func (s *site) stallWindow() time.Duration {
	if s.stall > 0 {
		return s.stall
	}
	return netsite.RemoteStallWindow
}

// Latency is the remote's padded round trip, for the supervisor's own logs and
// metrics.
func (s *site) Latency() time.Duration { return s.lat.Latency() }

// LimitReads wraps a connection the registry dialled through the site with the
// remote's limiter.
func (s *site) LimitReads(c net.Conn) net.Conn { return s.limiter.Wrap(c) }

// SetRateLimit caps the remote's read rate at mbps megabytes (10^6 bytes) per
// second; 0 or less removes the cap. It applies to connections already open.
// Only the supervisor sets it; the sites rigs see expose the cap read-only.
func (s *site) SetRateLimit(mbps float64) { s.limiter.Set(mbps) }

// RateLimit is the remote's read cap in megabytes per second, 0 when off.
func (s *site) RateLimit() float64 { return s.limiter.MBps() }

// setName records the remote's display name, which its Costs report.
func (s *site) setName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = name
}

func (s *site) displayName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// countingConn records the tunnel moving bytes on every read and write that
// moves any.
type countingConn struct {
	net.Conn
	s *site
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.s.touch()
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.s.touch()
	}
	return n, err
}

// newCallID is a fresh remote-protocol call id, which the remote echoes in its
// log lines and in any warning the call produces.
func newCallID() string {
	var b [8]byte
	rand.Read(b[:])
	return "c-" + hex.EncodeToString(b[:])
}
