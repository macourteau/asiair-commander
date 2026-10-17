//go:build !windows

package netsite

import (
	"net"

	"golang.org/x/sys/unix"
)

// setBroadcast enables SO_BROADCAST on an unconnected UDP socket so WriteToUDP
// to a broadcast address is permitted (Linux/macOS reject it otherwise).
func setBroadcast(conn *net.UDPConn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}
