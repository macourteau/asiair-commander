//go:build windows

package netsite

import (
	"net"

	"golang.org/x/sys/windows"
)

// setBroadcast enables SO_BROADCAST on an unconnected UDP socket so WriteToUDP
// to a broadcast address is permitted.
func setBroadcast(conn *net.UDPConn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) {
		setErr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}
