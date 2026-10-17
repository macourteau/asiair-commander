package netsite

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// localSite is the null-object site: Commander's own network through plain
// sockets, always up, with no added latency.
type localSite struct {
	logger *slog.Logger
}

func (localSite) ID() ID       { return Local }
func (localSite) State() State { return StateUp }

// localCosts are the local site's: no latency, so a device seen on the local
// network is preferred over any remote path at its next sighting and every
// timeout is the plain-LAN one; the 30 s SMB budget it has always had; no
// handshake, since plain sockets need no path built before they carry traffic;
// never capped; and a reply's result.ip taken as the device reports it, since
// the local site forwards nothing.
var localCosts = Costs{StallWindow: LocalStallWindow}

func (localSite) Costs() Costs { return localCosts }

// MeasureDigests is the streaming read in Commander's own session, exactly as
// the caller supplied it.
func (localSite) MeasureDigests(_ context.Context, req DigestRequest) (DigestResult, error) {
	return MeasureLocally(req)
}

// DialTCP is a plain net.Dialer connect; the error is the dialer's own.
func (localSite) DialTCP(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// ProbeUDP sends req to host:port on a fresh unconnected IPv4 socket — not
// DialUDP, which would filter out a reply arriving from a different source
// port — and returns once accept takes a reply or timeout elapses. host may be
// a hostname, resolved to its IPv4 address; scan_air is IPv4-only, so a host
// with no IPv4 address is not probed.
func (s localSite) ProbeUDP(ctx context.Context, host string, port int, req []byte, timeout time.Duration, accept func(reply []byte, srcIP string) bool) bool {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		s.logger.Debug("netsite: probe socket open failed", "address", host, "error", err)
		return false
	}
	defer conn.Close()

	ip := net.ParseIP(host)
	if ip == nil {
		if resolved, rerr := net.ResolveIPAddr("ip4", host); rerr == nil {
			ip = resolved.IP
		}
	}
	if ip == nil || ip.To4() == nil {
		s.logger.Debug("netsite: probe address is not IPv4", "address", host)
		return false
	}

	if _, err := conn.WriteToUDP(req, &net.UDPAddr{IP: ip, Port: port}); err != nil {
		s.logger.Debug("netsite: probe send failed", "address", host, "error", err)
		return false
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return false
		}
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return false
		}
		if accept(buf[:n], src.IP.String()) {
			return true
		}
	}
}

// Broadcast opens one unconnected IPv4 UDP socket (NOT DialUDP — a connected
// socket would filter out the unicast replies that arrive from
// device:ScanAirPort), enables SO_BROADCAST, sends ScanAirRequest to the global
// broadcast address and to each up, non-loopback interface's directed broadcast
// address (multi-NIC NAS), then reads replies until collectWindow elapses.
// Per-interface send failures are logged, not fatal.
func (s localSite) Broadcast(ctx context.Context) ([]Datagram, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open broadcast socket: %w", err)
	}
	defer conn.Close()

	if err := setBroadcast(conn); err != nil {
		return nil, fmt.Errorf("enable broadcast: %w", err)
	}

	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: ScanAirPort}
	if _, err := conn.WriteToUDP(ScanAirRequest, dst); err != nil {
		s.logger.Debug("netsite: global broadcast send failed", "error", err)
	}
	for _, b := range directedBroadcasts() {
		if _, err := conn.WriteToUDP(ScanAirRequest, &net.UDPAddr{IP: b, Port: ScanAirPort}); err != nil {
			s.logger.Debug("netsite: directed broadcast send failed", "address", b.String(), "error", err)
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(collectWindow))

	var out []Datagram
	buf := make([]byte, 4096)
	for i := 0; i < maxReplies; i++ {
		if ctx.Err() != nil {
			break
		}
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline reached or socket closed
		}
		out = append(out, Datagram{Data: append([]byte(nil), buf[:n]...), SrcIP: src.IP.String()})
	}
	return out, nil
}

// directedBroadcasts returns the directed-broadcast address of every up,
// non-loopback interface's IPv4 networks, so a multi-homed NAS reaches every
// attached broadcast domain, not just the default route's.
func directedBroadcasts() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			mask := ipnet.Mask
			if len(mask) != net.IPv4len {
				continue
			}
			b := make(net.IP, net.IPv4len)
			for i := range ip4 {
				b[i] = ip4[i] | ^mask[i]
			}
			out = append(out, b)
		}
	}
	return out
}
