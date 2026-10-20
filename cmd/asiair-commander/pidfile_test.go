package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPIDFileWrittenRefusedAndRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ac.pid")
	release, err := claimPIDFile(path, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := readPIDFile(path); err != nil || got != os.Getpid() {
		t.Fatalf("pid file holds %d (%v), want this process's %d", got, err, os.Getpid())
	}

	// This process is alive, so a second claim must refuse and leave the file.
	if _, err := claimPIDFile(path, discardLogger()); !errors.Is(err, errPIDFileHeld) {
		t.Fatalf("second claim: err = %v, want errPIDFileHeld", err)
	}
	if got, _ := readPIDFile(path); got != os.Getpid() {
		t.Fatalf("the refused claim changed the pid file to %d", got)
	}

	release()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file survived release: %v", err)
	}
	if left, _ := filepath.Glob(path + "*"); len(left) != 0 {
		t.Errorf("claim left temp files: %v", left)
	}
}

// deadPID is the PID of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestPIDFileStaleIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ac.pid")
	dead := deadPID(t)
	if err := os.WriteFile(path, []byte(strconv.Itoa(dead)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := claimPIDFile(path, discardLogger())
	if err != nil {
		t.Fatalf("claim over a stale file from dead PID %d: %v", dead, err)
	}
	defer release()
	if got, _ := readPIDFile(path); got != os.Getpid() {
		t.Fatalf("pid file holds %d after replacing the stale one, want %d", got, os.Getpid())
	}
}

func TestPIDFileUnreadableIsRefusedAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ac.pid")
	if err := os.WriteFile(path, []byte("not a pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := claimPIDFile(path, discardLogger()); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("claim over a non-PID file: err = %v, want an unreadable refusal", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "not a pid\n" {
		t.Errorf("the refused claim touched the file: %q", b)
	}
}

// A file someone else's start replaced is not this process's to remove.
func TestPIDFileReleaseLeavesAnotherOwnersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ac.pid")
	release, err := claimPIDFile(path, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release()
	if got, _ := readPIDFile(path); got != 1 {
		t.Fatalf("release removed or changed another owner's pid file (now %d)", got)
	}
}

// startRunVerb runs `run -data-dir <dir> -pid-file <path> extra...` through
// realMain on a free port and waits for /healthz. It returns the exit-code
// channel.
func startRunVerb(t *testing.T, dataDir, pidFile string, extra ...string) <-chan int {
	t.Helper()
	cfg := config.Defaults()
	cfg.WebPort = freePortPair(t)
	if err := config.Save(filepath.Join(dataDir, "config.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	args := append([]string{"run", "-data-dir", dataDir, "-pid-file", pidFile}, extra...)
	go func() { done <- realMain(args) }()

	url := "http://localhost:" + strconv.Itoa(cfg.WebPort) + "/healthz"
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case code := <-done:
			t.Fatalf("run verb exited with %d before serving", code)
		default:
		}
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return done
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("run verb did not serve /healthz within 15s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
