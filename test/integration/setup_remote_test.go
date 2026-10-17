//go:build integration

// This file proves the first-run guard against a REAL socket. Every other
// assertion about the setup window lives in internal/web's unit tests, which
// reach it through httptest — and httptest always dials 127.0.0.1, so those
// tests can only describe a remote peer by hand-setting RemoteAddr. The
// proxy-shape cases (a loopback peer presenting a loopback Host and Origin)
// deliberately stay there, on synthetic addresses; what cannot be faked is the
// thing tested here: a genuine non-loopback client, connecting over a routable
// interface address, is refused without the token and accepted with it.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/web"
)

// setupSupervisor is the no-op Supervisor the setup handler applies the saved
// config to. Nothing here drives a rig.
type setupSupervisor struct{}

func (setupSupervisor) Pause(string) error                     { return nil }
func (setupSupervisor) Resume(string) error                    { return nil }
func (setupSupervisor) RunNow(string) error                    { return nil }
func (setupSupervisor) StopRun(string) error                   { return nil }
func (setupSupervisor) Reclassify(string) error                { return nil }
func (setupSupervisor) ResetIdentity(string) error             { return nil }
func (setupSupervisor) DiscardFrame(string, int64) error       { return nil }
func (setupSupervisor) LinkLibrary(string, string) error       { return nil }
func (setupSupervisor) RescanLibrary(string) error             { return nil }
func (setupSupervisor) RestoreFrame(string, int64) error       { return nil }
func (setupSupervisor) CancelRestore(string, int64) error      { return nil }
func (setupSupervisor) AcknowledgeMissing(string, int64) error { return nil }
func (setupSupervisor) AcknowledgeMissingIDs(string, string, []int64) (int, error) {
	return 0, nil
}
func (setupSupervisor) RetargetFrame(_ string, frameID int64, _ string) (model.RetargetResult, error) {
	return model.RetargetResult{FrameID: frameID, Status: "retargeted"}, nil
}
func (setupSupervisor) PreviewRetarget(string, []int64, string) ([]model.RetargetPreview, error) {
	return nil, nil
}
func (setupSupervisor) PreviewRefile(context.Context, string, string) (model.RefilePreview, error) {
	return model.RefilePreview{}, nil
}
func (setupSupervisor) RefileGroup(context.Context, string, string, []int64) ([]model.RefileResult, model.RefileSummary, error) {
	return nil, model.RefileSummary{}, nil
}
func (setupSupervisor) PauseAll()                 {}
func (setupSupervisor) ResumeAll()                {}
func (setupSupervisor) ApplyConfig(*model.Config) {}

var _ web.Supervisor = setupSupervisor{}

// routableAddr returns a non-loopback IPv4 address of an up interface, which is
// what makes the client in this test genuinely remote from the server's point of
// view. A machine with no such interface (an isolated CI container) skips.
func routableAddr(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
				continue
			}
			return ipnet.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 interface: cannot connect as a real remote client")
	return ""
}

// TestRemoteClientSetupRequiresToken binds a real listener on a routable address
// and connects to it as a real remote client.
//
// What it proves, stated plainly: a request whose peer address is genuinely not
// loopback, and whose Host is therefore not in the pre-setup allowlist, is
// refused with code=setup_token_required when it carries no token, and completes
// setup when it carries the token the service wrote to its data directory.
func TestRemoteClientSetupRequiresToken(t *testing.T) {
	ip := routableAddr(t)
	dataDir := t.TempDir()

	cfgPtr := &atomic.Pointer[model.Config]{}
	cfgPtr.Store(config.Defaults())

	srv := web.New(web.Deps{
		ConfigPath: filepath.Join(dataDir, "config.yaml"),
		DataDir:    dataDir,
		Config:     cfgPtr,
		Supervisor: setupSupervisor{},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// Record what the server sees, so the test's own premise — that this client
	// is not loopback — is asserted rather than assumed.
	var peer atomic.Value
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer.Store(r.RemoteAddr)
		srv.Handler().ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	defer ln.Close()
	base := "http://" + ln.Addr().String()
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go httpSrv.Serve(ln) //nolint:errcheck // Serve always returns on Close
	defer httpSrv.Close()

	// The token file is the machine-readable channel the installer reads; using it
	// here exercises the same delivery path rather than reaching into the server.
	if err := srv.WriteSetupTokenFile(base + "/"); err != nil {
		t.Fatalf("WriteSetupTokenFile: %v", err)
	}
	token := tokenFromFile(t, filepath.Join(dataDir, web.SetupTokenFileName), base+"/")

	client := &http.Client{Timeout: 10 * time.Second}
	body := []byte(`{"password":"integration-password"}`)

	// No token: refused, with the machine-readable code the SPA routes on.
	resp, err := client.Post(base+"/api/setup", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/setup (no token): %v", err)
	}
	code, payload := decodeSetupResponse(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tokenless setup from a remote client: want 403, got %d (%s)", resp.StatusCode, payload)
	}
	if code != "setup_token_required" {
		t.Errorf("tokenless setup: code = %q, want %q (%s)", code, "setup_token_required", payload)
	}

	// The premise: the peer the server saw is a real, non-loopback address.
	seen, _ := peer.Load().(string)
	host, _, err := net.SplitHostPort(seen)
	if err != nil {
		t.Fatalf("RemoteAddr %q is not host:port: %v", seen, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		t.Fatalf("RemoteAddr host %q does not parse: %v", host, err)
	}
	if addr.IsLoopback() {
		t.Fatalf("the client connected over loopback (%s): this test proves nothing httptest could not", seen)
	}
	t.Logf("server saw peer %s, Host %s", seen, ln.Addr())

	// Still no password: the window is open, and the config is untouched.
	if cfgPtr.Load().PasswordHash != "" {
		t.Fatal("a refused setup left a password behind")
	}

	// With the token: accepted, from the same genuinely remote client.
	req, err := http.NewRequest(http.MethodPost, base+"/api/setup", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Setup-Token", token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/setup (with token): %v", err)
	}
	_, payload = decodeSetupResponse(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup with a valid token from a remote client: want 200, got %d (%s)", resp.StatusCode, payload)
	}
	if cfgPtr.Load().PasswordHash == "" {
		t.Error("setup returned 200 but no password was stored")
	}

	// The window is closed behind it: the token file is gone and a replay fails.
	if _, err := os.Stat(filepath.Join(dataDir, web.SetupTokenFileName)); !os.IsNotExist(err) {
		t.Errorf("the token file survived a completed setup: %v", err)
	}
	req, err = http.NewRequest(http.MethodPost, base+"/api/setup", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Setup-Token", token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/setup (replay): %v", err)
	}
	code, payload = decodeSetupResponse(t, resp)
	if resp.StatusCode != http.StatusForbidden || code != "setup_complete" {
		t.Errorf("replayed token: got %d code=%q, want 403 code=setup_complete (%s)", resp.StatusCode, code, payload)
	}
}

// tokenFromFile reads the two-line token file exactly as the Windows installer
// does — first line the bound URL, second the token — and checks the URL line,
// because a wrong one there is what would send the installer's browser somewhere
// the service is not listening.
func tokenFromFile(t *testing.T, path, wantURL string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("token file has %d lines, want 2: %q", len(lines), data)
	}
	if lines[0] != wantURL {
		t.Errorf("token file URL = %q, want %q", lines[0], wantURL)
	}
	if lines[1] == "" {
		t.Fatal("token file carries an empty token")
	}
	return lines[1]
}

// decodeSetupResponse returns the response's error code (empty when it carries
// none) and the raw body, for legible failure messages.
func decodeSetupResponse(t *testing.T, resp *http.Response) (code, body string) {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var parsed struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return parsed.Code, string(raw)
}
