//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is GetExitCodeProcess's code for a process that has not exited.
const stillActive = 259

// processAlive reports whether pid names a running process. A handle can
// outlive its process, so the exit code is checked as well; access denied
// means the process exists under another account.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
