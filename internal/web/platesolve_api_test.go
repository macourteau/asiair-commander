package web

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// fakeProv is a scriptable CatalogProvisioner for the download-endpoint tests.
type fakeProv struct {
	mu           sync.Mutex
	installed    map[string]bool
	downloading  string
	calls        []string
	resolveCount int
	resolveErr   error
}

func (p *fakeProv) CatalogInstalled(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.installed[name]
}

func (p *fakeProv) Downloading() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.downloading, p.downloading != ""
}

func (p *fakeProv) DownloadCatalog(_ context.Context, name string) error {
	p.mu.Lock()
	p.calls = append(p.calls, name)
	p.mu.Unlock()
	return nil
}

func (p *fakeProv) ResolveFailed(_ context.Context) (int, error) {
	return p.resolveCount, p.resolveErr
}

func (p *fakeProv) called() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	return out
}

func TestDownloadCatalogUnknownName(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	resp := h.do("POST", "/api/platesolve/catalogs/nope", nil, reqOpts{csrf: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown catalog: want 400, got %d", resp.StatusCode)
	}
}

func TestDownloadCatalogStarted(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	resp := h.do("POST", "/api/platesolve/catalogs/d20", nil, reqOpts{csrf: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start download: want 202, got %d", resp.StatusCode)
	}
	// The download runs in a goroutine; poll briefly for the call.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if calls := h.prov.called(); len(calls) == 1 && calls[0] == "d20" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("DownloadCatalog was not invoked; calls=%v", h.prov.called())
}

func TestDownloadCatalogAlreadyInstalled(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.prov.installed["d05"] = true
	resp := h.do("POST", "/api/platesolve/catalogs/d05", nil, reqOpts{csrf: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("already installed: want 200, got %d", resp.StatusCode)
	}
	if calls := h.prov.called(); len(calls) != 0 {
		t.Fatalf("installed no-op must not call DownloadCatalog; calls=%v", calls)
	}
}

func TestDownloadCatalogBusy(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.prov.downloading = "d20"
	resp := h.do("POST", "/api/platesolve/catalogs/d50", nil, reqOpts{csrf: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy: want 409, got %d", resp.StatusCode)
	}
}

func TestDownloadCatalogRequiresCSRF(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	resp := h.do("POST", "/api/platesolve/catalogs/d20", nil, reqOpts{csrf: false})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: want 403, got %d", resp.StatusCode)
	}
}

func TestResolveFailedReturnsCount(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.prov.resolveCount = 4
	resp := h.do("POST", "/api/platesolve/resolve", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: want 200, got %d", resp.StatusCode)
	}
	var body map[string]int
	decode(t, resp, &body)
	if body["requeued"] != 4 {
		t.Fatalf("requeued = %d, want 4", body["requeued"])
	}
}

func TestResolveFailedRequiresCSRF(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	resp := h.do("POST", "/api/platesolve/resolve", nil, reqOpts{csrf: false})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF: want 403, got %d", resp.StatusCode)
	}
}
