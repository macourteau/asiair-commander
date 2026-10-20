//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRunVerbPIDFileLifecycle drives the real flag: the running server's PID is
// in the file, a second start against the same file is refused without
// disturbing it, and a SIGTERM shutdown removes it.
func TestRunVerbPIDFileLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	pidFile := filepath.Join(dataDir, "ac.pid")
	done := startRunVerb(t, dataDir, pidFile)

	if got, err := readPIDFile(pidFile); err != nil || got != os.Getpid() {
		t.Fatalf("pid file holds %d (%v), want %d", got, err, os.Getpid())
	}
	if code := realMain([]string{"run", "-data-dir", t.TempDir(), "-pid-file", pidFile}); code != 1 {
		t.Fatalf("second start against a live pid file exited %d, want 1", code)
	}
	if got, _ := readPIDFile(pidFile); got != os.Getpid() {
		t.Fatalf("the refused start changed the pid file to %d", got)
	}

	// The run verb's signal context catches this, as it would for `kill <pid>`.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run verb exited %d after SIGTERM, want 0", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run verb did not shut down within 20s of SIGTERM")
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file survived a clean shutdown: %v", err)
	}
}
