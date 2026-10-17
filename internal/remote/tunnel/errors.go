package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

// tunnelError is the cause inside every *net.OpError the client role returns.
// It carries the original error's redacted text and its timeout class, and
// unwraps only to a context error, so no caller can reach the unredacted text
// of a tailcat error through errors.Unwrap.
type tunnelError struct {
	msg     string
	timeout bool
	ctx     error
}

func (e *tunnelError) Error() string   { return e.msg }
func (e *tunnelError) Timeout() bool   { return e.timeout }
func (e *tunnelError) Temporary() bool { return e.timeout }
func (e *tunnelError) Unwrap() error   { return e.ctx }

// errTunnelClosed is the cause of a dial on a closed client.
var errTunnelClosed = errors.New("tunnel closed")

// opError turns any failure on the client's dial path into a *net.OpError for
// target, so smb1.IsNetworkError and every net.Error check classify it as a
// network failure, the same as a failed dial on the LAN.
func opError(op string, proto Proto, target net.Addr, err error, red *redactor) error {
	cause := &tunnelError{msg: red.redact(err.Error())}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		cause.timeout = true
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		cause.timeout = true
		cause.ctx = context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		cause.ctx = context.Canceled
	}
	return &net.OpError{Op: op, Net: string(proto), Addr: target, Err: cause}
}

// targetAddr is the net.Addr an error names for a LAN target.
func targetAddr(proto Proto, dst netip.AddrPort) net.Addr {
	if proto == UDP {
		return net.UDPAddrFromAddrPort(dst)
	}
	return net.TCPAddrFromAddrPort(dst)
}

// protocolAddr names the remote-protocol endpoint in errors, which has no LAN
// address.
type protocolAddr struct{}

func (protocolAddr) Network() string { return "tcp" }
func (protocolAddr) String() string  { return "remote-protocol" }
