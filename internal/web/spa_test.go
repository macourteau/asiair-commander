package web

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// getBody fetches path from the harness and returns the status and body.
func getBody(t *testing.T, h *harness, path string) (int, string) {
	t.Helper()
	resp := h.do("GET", path, nil, reqOpts{})
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(b)
}

// A binary built without the SPA has no index.html in its static tree, and must
// still serve a page explaining that rather than a 500 — for the root and for a
// client-side route alike.
func TestSPAWithoutIndexServesPlaceholder(t *testing.T) {
	empty := t.TempDir()
	h := newHarness(t, true, func(d *Deps) { d.DevDir = empty })

	for _, p := range []string{"/", "/devices"} {
		code, body := getBody(t, h, p)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, code)
		}
		if body != string(placeholderHTML) {
			t.Fatalf("GET %s served %q, want the placeholder", p, body)
		}
	}
}

// A staged SPA build takes precedence over the placeholder, for the root, for a
// client-side route and for its assets.
func TestSPABuildTakesPrecedenceOverPlaceholder(t *testing.T) {
	dir := t.TempDir()
	const index = "<!doctype html><title>built</title>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, true, func(d *Deps) { d.DevDir = dir })

	for _, p := range []string{"/", "/devices"} {
		if code, body := getBody(t, h, p); code != http.StatusOK || body != index {
			t.Fatalf("GET %s = %d %q, want 200 with the built index", p, code, body)
		}
	}
	if code, body := getBody(t, h, "/assets/app.js"); code != http.StatusOK || body != "console.log(1)" {
		t.Fatalf("GET /assets/app.js = %d %q, want the asset", code, body)
	}
}

// The placeholder is a real page naming how to embed the UI, so an operator who
// reaches it knows what happened.
func TestPlaceholderNamesTheFix(t *testing.T) {
	if !strings.Contains(string(placeholderHTML), "make spa") {
		t.Fatal("placeholder.html does not say how to embed the UI")
	}
}
