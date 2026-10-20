//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process. Signal 0 probes
// without delivering anything; EPERM means it exists under another user.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
