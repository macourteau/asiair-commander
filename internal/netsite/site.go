package netsite

import (
	"context"
	"fmt"
	"net"
	"time"
)

// State is a site's reachability as its owner reports it. Only an up site
// carries traffic.
type State int

const (
	// StateDown means the site cannot carry traffic right now.
	StateDown State = iota
	// StateUp means the site carries traffic.
	StateUp
)

func (s State) String() string {
	switch s {
	case StateUp:
		return "up"
	case StateDown:
		return "down"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// The device ports a site carries traffic to: SMB, the ASIAIR control channel
// and scan_air. Every package that dials, forwards or polices them reads them
// here.
const (
	// SMBPort is the ASIAIR's SMB1 port.
	SMBPort = 445
	// ControlPort is the ASIAIR's control channel.
	ControlPort = 4700
	// ScanAirPort is the UDP port scan_air requests and replies use.
	ScanAirPort = 4720
)

// Site carries traffic to devices on one network. It is the transport alone;
// what using it costs, and how it presents itself, is the Costs value it
// reports, so an implementation writes these methods and fills that value. An
// implementation that caps its read rate also implements LimitsReads, which
// the registry applies to every connection dialled through it.
type Site interface {
	// ID is the site's id; the local site's is Local.
	ID() ID
	// DialTCP opens a TCP connection to host:port, the connect bounded by
	// timeout. A failure is a net.Error, so callers classify it as a network
	// error exactly as a refused or timed-out socket dial.
	DialTCP(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error)
	// ProbeUDP sends req as one datagram to host:port and reads replies until
	// accept takes one (true) or timeout elapses (false). accept receives each
	// reply with its source IP.
	ProbeUDP(ctx context.Context, host string, port int, req []byte, timeout time.Duration, accept func(reply []byte, srcIP string) bool) bool
	// Broadcast runs one scan_air broadcast round on the site's network: it
	// sends the scan_air request (ScanAirRequest on the local site; a remote
	// sends its own fixed request) and returns the raw replies, each with its
	// UDP source address, that arrive within the collect window.
	Broadcast(ctx context.Context) ([]Datagram, error)
	// MeasureDigests measures a device file's pre-delete digests. The local
	// site runs req.Local, the streaming read in Commander's own session; a
	// site that can read the file on its own network measures it there, in its
	// own session, and reports what it probed. It never compares.
	MeasureDigests(ctx context.Context, req DigestRequest) (DigestResult, error)
	// State is whether the site carries traffic right now.
	State() State
	// Costs is what using the site costs right now, and how it presents
	// itself. Each call reads the current figures.
	Costs() Costs
}

// Costs is a site's cost figures and policies at one moment, and how it
// presents itself to a rig's card.
type Costs struct {
	// Latency is the site's round-trip cost on top of the device's own; the
	// local site's is 0.
	Latency time.Duration
	// MinLatency is the lowest round trip the site measured recently, the
	// figure path selection compares: unlike a smoothed estimate, a rig's own
	// transfer cannot inflate it. The local site's is 0.
	MinLatency time.Duration
	// StallWindow is the base of the SMB budget on this site: how long a
	// connection through it may move no bytes before the operation is given up.
	// SMBBudget adds the latency margins to it. The local site's is
	// LocalStallWindow; a site that must ride out a link outage reports longer.
	StallWindow time.Duration
	// HandshakeAllowance is the time a freshly built path may spend on its first
	// handshake before it carries traffic, which a timeout that may be the first
	// use of the path adds. The local site's is 0.
	HandshakeAllowance time.Duration
	// RepliesMatchSource reports whether a broadcast reply whose result.ip is
	// set must equal its UDP source address to be trusted. A site whose
	// forwarding allowlist admits only such replies reports true, so the device
	// address discovery records is one the site will forward to; the local site
	// forwards everything and reports false.
	RepliesMatchSource bool
	// RateLimitMBps is the site's read cap in megabytes (10^6 bytes) per
	// second, 0 when off. The local site is never capped.
	RateLimitMBps float64
	// Remote reports that the site is a supervised remote, whose own state
	// stands in for a rig's while it is down; RemoteName is its display name.
	// The local site and an unknown one are not remotes.
	Remote     bool
	RemoteName string
}

// LimitsReads is implemented by a site with a read-rate limiter (typically a
// ReadLimiter). The registry wraps every connection it dials through such a
// site with LimitReads, so an implementation's DialTCP must not apply it again;
// a site without one is never throttled.
type LimitsReads interface {
	LimitReads(c net.Conn) net.Conn
}

// Datagram is one raw reply to a broadcast round with its UDP source address.
//
// It is also the remote protocol's wire form of a broadcast reply
// (proto.Datagram is this type), so its JSON tags are a frozen wire contract:
// adding, renaming or retyping a field is a protocol change, made only behind a
// protocol version a peer's hello reports. TestNetsiteWireForm pins it.
type Datagram struct {
	Data  []byte `json:"data"`
	SrcIP string `json:"src_ip"`
}

// Sites looks up the site that carries an endpoint's traffic. *Registry
// implements it; the device factory, the control dialer and discovery's probe
// take it rather than the concrete registry so tests can substitute their own.
type Sites interface {
	// Site returns the site registered under id. An unknown id, or a site that
	// is not up, yields a Site whose every dial and probe fails without opening
	// a socket.
	Site(id ID) Site
}

// UnavailableError is the dial failure for an endpoint whose site cannot carry
// traffic: unknown, removed, or not up. It is a net.Error so the caller takes
// its ordinary unreachable path.
type UnavailableError struct {
	Site   ID
	Reason string
}

var _ net.Error = (*UnavailableError)(nil)

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("site %q unavailable: %s", string(e.Site), e.Reason)
}

// Timeout is false: nothing was attempted, so nothing timed out.
func (e *UnavailableError) Timeout() bool { return false }

// Temporary is false. Deprecated in net.Error, but part of the interface.
func (e *UnavailableError) Temporary() bool { return false }
