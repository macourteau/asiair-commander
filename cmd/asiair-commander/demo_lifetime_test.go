//go:build demo

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDemoMaxLifetimeShutsDownCleanly sets a short cap through the real flag
// and asserts the run verb exits 0 on its own, logs the expiry and removes its
// pid file, which only the graceful path does.
func TestDemoMaxLifetimeShutsDownCleanly(t *testing.T) {
	dataDir := t.TempDir()
	pidFile := filepath.Join(dataDir, "ac.pid")
	done := startRunVerb(t, dataDir, pidFile, "-demo-max-lifetime", "3s")
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run verb exited %d at the lifetime cap, want 0", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run verb was still running 30s after a 3s lifetime cap")
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pid file survived the lifetime shutdown: %v", err)
	}
	log, err := os.ReadFile(appLogPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"max_lifetime=3s",
		"demo lifetime of 3s reached; shutting down; restart it if still needed",
		"asiair-commander stopped",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("app log lacks %q", want)
		}
	}
}

func TestDemoMaxLifetimeFlag(t *testing.T) {
	if code := realMain([]string{"run", "-data-dir", t.TempDir(), "-demo-max-lifetime", "-1s"}); code != 2 {
		t.Errorf("a negative lifetime exited %d, want the usage error 2", code)
	}
	for d, want := range map[time.Duration]string{
		16 * time.Hour:   "16h",
		90 * time.Minute: "1h30m",
		10 * time.Second: "10s",
		2 * time.Minute:  "2m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
