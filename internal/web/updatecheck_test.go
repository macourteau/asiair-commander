package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/updatecheck"
)

// fakeReleases is a stand-in for the GitHub latest-release endpoint.
func fakeReleases(t *testing.T, tag *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"`+tag.Load().(string)+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// updateHarness wires a real update checker into the web server exactly as the
// composition root does: its switch reads the live config pointer, Latest feeds
// server_info, and OnChange re-announces it. tick fires one scheduled check.
type updateHarness struct {
	*harness
	tick chan time.Time
}

func newUpdateHarness(t *testing.T, running string, tag *atomic.Value) *updateHarness {
	t.Helper()
	gh := fakeReleases(t, tag)
	tick := make(chan time.Time)
	var srv *Server
	var checker *updatecheck.Checker
	h := newHarness(t, true, func(d *Deps) {
		cfg := d.Config
		checker = updatecheck.New(updatecheck.Options{
			Version:  running,
			Enabled:  func() bool { return !cfg.Load().UpdateCheckDisabled },
			OnChange: func() { srv.AnnounceServerInfo() },
			Logger:   d.Logger,
			BaseURL:  gh.URL,
			After:    func(time.Duration) <-chan time.Time { return tick },
		})
		d.Version = running
		d.LatestVersion = checker.Latest
	})
	srv = h.srv
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go checker.Run(ctx)
	return &updateHarness{harness: h, tick: tick}
}

// serverInfoStream opens /events and returns a channel of every server_info it
// receives, in order.
func (h *harness) serverInfoStream() <-chan serverInfo {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", h.baseURL+"/events", nil)
	req.Header.Set("Origin", h.baseURL)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("open SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("SSE: want 200, got %d", resp.StatusCode)
	}
	out := make(chan serverInfo, 16)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "event: server_info" || !sc.Scan() {
				continue
			}
			var info serverInfo
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok && json.Unmarshal([]byte(data), &info) == nil {
				out <- info
			}
		}
	}()
	return out
}

func nextInfo(t *testing.T, ch <-chan serverInfo) serverInfo {
	t.Helper()
	select {
	case info := <-ch:
		return info
	case <-time.After(5 * time.Second):
		t.Fatal("no server_info arrived")
		return serverInfo{}
	}
}

func (u *updateHarness) fireCheck(t *testing.T) {
	t.Helper()
	select {
	case u.tick <- time.Time{}:
	case <-time.After(5 * time.Second):
		t.Fatal("checker was not waiting for its next tick")
	}
}

// TestSSECarriesLatestVersion: a connected client gets latest_version empty at
// first, then a pushed server_info once a check finds a newer release, and a
// client connecting afterwards gets it up front.
func TestSSECarriesLatestVersion(t *testing.T) {
	var tag atomic.Value
	tag.Store("v1.4.0")
	h := newUpdateHarness(t, "v1.3.2", &tag)
	h.login()

	stream := h.serverInfoStream()
	if got := nextInfo(t, stream); got.Version != "v1.3.2" || got.LatestVersion != "" {
		t.Fatalf("initial server_info = %+v, want version v1.3.2 and no latest", got)
	}

	h.fireCheck(t)
	if got := nextInfo(t, stream); got.LatestVersion != "v1.4.0" || got.Version != "v1.3.2" {
		t.Fatalf("pushed server_info = %+v, want latest v1.4.0", got)
	}

	if got := nextInfo(t, h.serverInfoStream()); got.LatestVersion != "v1.4.0" {
		t.Errorf("a new connection's server_info = %+v, want latest v1.4.0", got)
	}
}

// TestUpdateCheckToggleAppliesLive: switching the check off in Settings pushes a
// server_info with no latest version, and switching it back on pushes it again.
func TestUpdateCheckToggleAppliesLive(t *testing.T) {
	var tag atomic.Value
	tag.Store("v2.0.0")
	h := newUpdateHarness(t, "v1.0.0", &tag)
	h.login()
	stream := h.serverInfoStream()
	nextInfo(t, stream)
	h.fireCheck(t)
	if got := nextInfo(t, stream); got.LatestVersion != "v2.0.0" {
		t.Fatalf("after check, latest = %q, want v2.0.0", got.LatestVersion)
	}

	put := func(disabled bool) {
		t.Helper()
		cfg := h.getConfig()
		cfg.UpdateCheckDisabled = disabled
		resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("put: want 200, got %d (%s)", resp.StatusCode, b)
		}
		resp.Body.Close()
	}

	put(true)
	if got := nextInfo(t, stream); got.LatestVersion != "" {
		t.Errorf("after switching off, latest = %q, want empty", got.LatestVersion)
	}
	put(false)
	if got := nextInfo(t, stream); got.LatestVersion != "v2.0.0" {
		t.Errorf("after switching on, latest = %q, want v2.0.0", got.LatestVersion)
	}
}

// TestPutConfigOmittingUpdateCheckPreservesIt: an older SPA's whole-config PUT
// has no update_check_disabled key; it must not switch a disabled check on.
func TestPutConfigOmittingUpdateCheckPreservesIt(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	cfg := *h.cfgPtr.Load()
	cfg.UpdateCheckDisabled = true
	h.cfgPtr.Store(&cfg)

	resp := putConfigWithout(h, []string{"update_check_disabled"}, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("omitting PUT: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()
	if !h.cfgPtr.Load().UpdateCheckDisabled {
		t.Error("update_check_disabled was reversed by a PUT that never mentioned it")
	}
}
