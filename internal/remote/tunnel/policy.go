package tunnel

import (
	"errors"
	"net/netip"

	"github.com/macourteau/asiair-commander/internal/netsite"
)

// Proto names the transport of a forwarded flow.
type Proto string

// The two transports a remote can forward.
const (
	TCP Proto = "tcp"
	UDP Proto = "udp"
)

// Policy is the remote role's forwarding decision for one flow to a LAN
// target: it reports whether proto to ip:port may be forwarded. It is consulted
// only for targets the wrapper itself would forward at all (see checkForward).
type Policy func(proto Proto, ip netip.Addr, port uint16) bool

// The ports a remote forwards at most: SMB, the ASIAIR control port and the
// scan_air port. Nothing else is forwardable, whatever the policy says, so even
// a stolen client key cannot turn a remote into a general exit node.
const (
	portSMB     = netsite.SMBPort
	portControl = netsite.ControlPort
	portScan    = netsite.ScanAirPort
)

// Reasons a flow is refused before it reaches the LAN.
var (
	errPortNotForwardable = errors.New("port is never forwarded")
	errNotIPv4            = errors.New("target is not an IPv4 address")
	errPolicyRefused      = errors.New("target refused by the forwarding policy")
)

// checkForward is the one forwarding decision, shared by the tailcat server and
// the fake so both refuse exactly the same flows: the port must be one of the
// three forwardable ones for its transport, the target an IPv4 address, and
// the policy must allow it. The policy is consulted last, so it never sees a
// flow the wrapper refuses on its own.
func checkForward(policy Policy, proto Proto, dst netip.AddrPort) error {
	switch {
	case proto == TCP && (dst.Port() == portSMB || dst.Port() == portControl):
	case proto == UDP && dst.Port() == portScan:
	default:
		return errPortNotForwardable
	}
	if !dst.Addr().Is4() {
		return errNotIPv4
	}
	if policy == nil || !policy(proto, dst.Addr(), dst.Port()) {
		return errPolicyRefused
	}
	return nil
}
