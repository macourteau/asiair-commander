package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/remote/role"
	"github.com/macourteau/asiair-commander/internal/web"
)

func TestResolveDataDir(t *testing.T) {
	t.Run("explicit override wins", func(t *testing.T) {
		got, err := resolveDataDir("/custom/path")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/custom/path" {
			t.Fatalf("got %q, want /custom/path", got)
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("XDG_CONFIG_HOME path", func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "/xdg/root")
			got, err := resolveDataDir("")
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join("/xdg/root", "asiair-commander")
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})

		t.Run("HOME fallback", func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("HOME", "/home/tester")
			got, err := resolveDataDir("")
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join("/home/tester", ".config", "asiair-commander")
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestRemoteVerbRefusesWithoutPin: the remote verb refuses to start without its
// pairing, and creates no data directory, before or after refusing.
func TestRemoteVerbRefusesWithoutPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("ProgramData", home)
	t.Setenv(role.EnvVar, "")
	if code := realMain([]string{"remote"}); code == 0 {
		t.Fatal("remote verb started without a pairing")
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the remote verb created %v", entries)
	}
}

// TestLoggingFanOut verifies a single log line reaches every sink: the text
// writer and the in-memory ring the UI reads.
func TestLoggingFanOut(t *testing.T) {
	var text strings.Builder
	level := new(slog.LevelVar)
	ring := web.NewLogBuffer(10)
	handler := newMultiHandler(level,
		slog.NewTextHandler(&text, &slog.HandlerOptions{Level: level}),
		ring.Handler(),
	)
	logger := slog.New(handler)

	logger.Info("hello ring", "unit", "test")

	if !strings.Contains(text.String(), "hello ring") {
		t.Fatalf("text sink missing line: %q", text.String())
	}
	entries := ring.Entries(slog.LevelDebug, 0)
	if len(entries) != 1 {
		t.Fatalf("ring got %d entries, want 1", len(entries))
	}
	if entries[0].Message != "hello ring" {
		t.Fatalf("ring message = %q", entries[0].Message)
	}
	if entries[0].Attrs["unit"] != "test" {
		t.Fatalf("ring attrs = %v, want unit=test", entries[0].Attrs)
	}

	// A sub-INFO line must be dropped by the shared level gate.
	logger.Debug("suppressed")
	if got := len(ring.Entries(slog.LevelDebug, 0)); got != 1 {
		t.Fatalf("debug line leaked past level gate: %d entries", got)
	}
}

// TestRunStackHealthz exercises the whole wiring end-to-end on an ephemeral port:
// it serves /healthz and asserts 200, then cancels and asserts a clean, bounded
// shutdown. Hermetic — no real SMB, only loopback.
func TestRunStackHealthz(t *testing.T) {
	dataDir := t.TempDir()

	// Write a Defaults() config on a free port so the bind fallback is not
	// exercised and the URL is predictable; the ready hook reports the truth
	// regardless.
	cfg := config.Defaults()
	cfg.WebPort = freePort(t)
	if err := config.Save(filepath.Join(dataDir, "config.yaml"), cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	appLog := web.NewLogBuffer(100)

	ctx, cancel := context.WithCancel(context.Background())
	urlCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runStack(ctx, dataDir, logger, appLog, func(url string) { urlCh <- url })
	}()

	var base string
	select {
	case base = <-urlCh:
	case err := <-done:
		t.Fatalf("runStack returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server did not become ready within 10s")
	}

	resp, err := http.Get(base + "healthz")
	if err != nil {
		cancel()
		t.Fatalf("GET healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runStack returned error on shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runStack did not shut down within 15s")
	}
}

// TestRunStackDualPort covers the dual-port invariants that only real listeners
// can prove: plain HTTP is always served (1), both ports answer through the same
// handler (2), the TLS port is exactly the bound HTTP port + 1 (4), and cancelling
// shuts both down (5).
func TestRunStackDualPort(t *testing.T) {
	dataDir := t.TempDir()

	cfg := config.Defaults()
	cfg.WebPort = freePortPair(t)
	if err := config.Save(filepath.Join(dataDir, "config.yaml"), cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	urlCh := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runStack(ctx, dataDir, logger, web.NewLogBuffer(100), func(url string) { urlCh <- url })
	}()

	var base string
	select {
	case base = <-urlCh:
	case err := <-done:
		t.Fatalf("runStack returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server did not become ready within 10s")
	}
	defer cancel()

	// design architecture §8.5 invariant 1: the hand-off URL is plain HTTP and it
	// serves.
	if !strings.HasPrefix(base, "http://") {
		t.Errorf("ready URL = %q, want an http:// URL", base)
	}
	httpBody := getOK(t, http.DefaultClient, base+"healthz")

	// design architecture §8.5 invariant 4: HTTPS lives on the bound port + 1.
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse ready URL %q: %v", base, err)
	}
	httpPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", base, err)
	}
	tlsURL := fmt.Sprintf("https://localhost:%d/healthz", httpPort+1)

	// The cert is self-signed by design; this asserts the listener speaks TLS,
	// not that anyone trusts it.
	tlsClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	// The TLS listener binds after the ready hook fires, so allow it a moment.
	var tlsBody string
	for deadline := time.Now().Add(10 * time.Second); ; {
		resp, err := tlsClient.Get(tlsURL)
		if err == nil {
			resp.Body.Close()
			tlsBody = getOK(t, tlsClient, tlsURL)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no HTTPS listener on %s within 10s: %v", tlsURL, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// design architecture §8.5 invariant 2: one handler behind both ports.
	if httpBody != tlsBody {
		t.Errorf("healthz differs across ports:\n http: %s\nhttps: %s", httpBody, tlsBody)
	}

	// design architecture §8.5 invariant 5: both listeners stop.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runStack returned error on shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runStack did not shut down within 15s")
	}
	if _, err := tlsClient.Get(tlsURL); err == nil {
		t.Error("HTTPS listener still accepting connections after shutdown")
	}
	if _, err := http.Get(base + "healthz"); err == nil {
		t.Error("HTTP listener still accepting connections after shutdown")
	}
}

// TestBindTLSFailureIsNotFatal covers design architecture §8.5 invariant 3:
// when the TLS port cannot be bound, bindTLS reports no listener instead of
// failing, leaving plain HTTP to carry the app.
func TestBindTLSFailureIsNotFatal(t *testing.T) {
	httpPort := freePortPair(t)

	// Occupy the port bindTLS will reach for.
	blocker, err := net.Listen("tcp", fmt.Sprintf(":%d", httpPort+1))
	if err != nil {
		t.Fatalf("occupy TLS port: %v", err)
	}
	defer blocker.Close()

	srv, ln, tlsURL := bindTLS(t.TempDir(), http.NotFoundHandler(), httpPort,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if srv != nil || ln != nil || tlsURL != "" {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("bindTLS on an occupied port = (%v, %v, %q), want all zero", srv, ln, tlsURL)
	}
}

// getOK fetches url, requires 200, and returns the body.
func getOK(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", url, err)
	}
	return string(body)
}

// freePortPair returns a port p where both p and p+1 are free, so the dual-port
// bind is not defeated by an unrelated listener on the neighbouring port.
func freePortPair(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		p := freePort(t)
		next, err := net.Listen("tcp", fmt.Sprintf(":%d", p+1))
		if err != nil {
			continue
		}
		next.Close()
		return p
	}
	t.Fatal("could not find an adjacent free port pair in 20 attempts")
	return 0
}

// freePort grabs an OS-assigned free TCP port and releases it. A tiny race
// window exists before runStack rebinds it, but the ready hook decouples the
// test from the actual port, so a lost race still yields a working server.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
