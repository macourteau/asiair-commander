package netsite

import (
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// LocalStallWindow is the local site's SMB budget: how long a LAN connection
	// may move no bytes before an operation on it is given up.
	LocalStallWindow = 30 * time.Second
	// RemoteStallWindow is the SMB budget of a site reached over a link that can
	// drop. A stalled TCP flow resumes only at its next retransmit, which after a
	// one-minute outage came 43 s after the link returned, so 60 s rides out
	// outages of roughly 30 to 40 s; a longer one still cuts the transfer, which
	// is retried next cycle.
	RemoteStallWindow = 60 * time.Second
	// RemoteHandshakeAllowance is the first-handshake time of a freshly built
	// tunnel client, for a site that has one.
	RemoteHandshakeAllowance = 5 * time.Second

	// marginPerRoundTrip is how many latencies one round trip is given. A
	// site's latency is a smoothed estimate, and a transfer's own bytes queue
	// ahead of a request on a narrow link, so a single round trip can take well
	// over the estimate; twice it absorbs that without hiding a dead link.
	marginPerRoundTrip = 2

	// smbRoundTrips is how many round trips SMBBudget covers: the request and
	// reply of the operation it bounds, and one retransmission of either.
	smbRoundTrips = 2

	// readBurst is the limiter's bucket, about one TCP receive window on a WAN
	// path. Reads may run this far ahead of the cap, on top of what the socket
	// buffer already holds, so a capped link sees about one window of burst per
	// transfer and the cap on average. It is also the most a single Read returns
	// while capped, so one wait never needs more tokens than the bucket holds.
	readBurst = 256 * 1024

	// bytesPerMB is the unit of a rate cap: megabytes, not mebibytes, as a
	// link's speed is quoted.
	bytesPerMB = 1_000_000
)

// RoundTrips is the latency allowance for n round trips through s, each given
// marginPerRoundTrip of the site's latency. It is 0 on the local site, so a
// timeout built from it is unchanged there.
func RoundTrips(s Site, n int) time.Duration {
	return s.Costs().RoundTrips(n)
}

// RoundTrips is the latency allowance for n round trips at c's latency, each
// given marginPerRoundTrip of it.
func (c Costs) RoundTrips(n int) time.Duration {
	return time.Duration(n*marginPerRoundTrip) * c.Latency
}

// SMBBudget is the SMB budget on s: its stall window plus the margins for
// smbRoundTrips round trips. It bounds an SMB dial and each SMB operation, and
// is the stall window of a streaming read. The rebuild rule and a remote
// digest's progress stall read this same value. On the local site it is
// LocalStallWindow exactly.
func SMBBudget(s Site) time.Duration {
	c := s.Costs()
	return c.StallWindow + c.RoundTrips(smbRoundTrips)
}

// CappedReadAllowance is how long reading n bytes takes at s's rate cap, 0 when
// the site is uncapped, so a timeout bounding a read of n bytes can add it and
// a low cap does not trip it.
func CappedReadAllowance(s Site, n int) time.Duration {
	mbps := s.Costs().RateLimitMBps
	if mbps <= 0 {
		return 0
	}
	return time.Duration(float64(n) / (mbps * bytesPerMB) * float64(time.Second))
}

// ReadLimiter is a site's read-rate limiter: one token bucket shared by every
// connection it wraps. The zero value is not usable; build it with
// NewReadLimiter. It is safe for concurrent use.
type ReadLimiter struct {
	lim *rate.Limiter
}

// NewReadLimiter returns a limiter with no cap.
func NewReadLimiter() *ReadLimiter {
	return &ReadLimiter{lim: rate.NewLimiter(rate.Inf, readBurst)}
}

// Set caps reads at mbps megabytes per second; 0 or less removes the cap.
func (l *ReadLimiter) Set(mbps float64) {
	if mbps <= 0 {
		l.lim.SetLimit(rate.Inf)
		return
	}
	l.lim.SetLimit(rate.Limit(mbps * bytesPerMB))
}

// MBps is the cap in megabytes per second, 0 when off.
func (l *ReadLimiter) MBps() float64 {
	r := l.lim.Limit()
	if r == rate.Inf {
		return 0
	}
	return float64(r) / bytesPerMB
}

// Wrap returns c with its reads drawn from the limiter. A nil c is returned
// as is, so a failed dial's result can be wrapped unconditionally.
func (l *ReadLimiter) Wrap(c net.Conn) net.Conn {
	if c == nil {
		return nil
	}
	return &limitedConn{Conn: c, lim: l.lim, closed: make(chan struct{})}
}

// limitedConn pays for each read after it returns: the bytes are already off
// the socket, and the wait before the next read is what lets the receive buffer
// fill and TCP's backpressure slow the sender to the cap.
type limitedConn struct {
	net.Conn
	lim       *rate.Limiter
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *limitedConn) Read(p []byte) (int, error) {
	if c.lim.Limit() == rate.Inf {
		return c.Conn.Read(p)
	}
	if len(p) > readBurst {
		p = p[:readBurst]
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.wait(n)
	}
	return n, err
}

// wait blocks until n tokens are paid for, or the connection is closed. The
// bytes are returned either way: they were read, and a caller closing the
// connection is not waiting for them to be paced.
func (c *limitedConn) wait(n int) {
	r := c.lim.ReserveN(time.Now(), n)
	if !r.OK() {
		// Unreachable while reads are capped at the burst; never block forever.
		return
	}
	d := r.Delay()
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.closed:
	}
}

func (c *limitedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
