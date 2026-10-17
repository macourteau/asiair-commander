package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortenRetries keeps the backoff schedule's shape (two retries) while making the
// waits negligible, so the retry paths are exercised at test speed.
func shortenRetries(t *testing.T) {
	t.Helper()
	saved := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = saved })
}

// serve returns a server whose handler is driven by handler, plus a counter of the
// requests it received.
func serve(t *testing.T, handler func(w http.ResponseWriter, hits int64)) (url string, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handler(w, hits.Add(1))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits
}

func TestFetchPinnedRetriesTransientFailure(t *testing.T) {
	for _, status := range []int{
		http.StatusServiceUnavailable, // server-side blip
		http.StatusTooManyRequests,    // SourceForge throttling a CI download
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			shortenRetries(t)
			body := []byte("astap-zip-bytes")
			url, hits := serve(t, func(w http.ResponseWriter, n int64) {
				if n < 3 {
					w.WriteHeader(status)
					return
				}
				w.Write(body)
			})

			data, mismatched, err := fetchPinned(plainSources(url), sha256hex(body))
			if err != nil {
				t.Fatalf("fetchPinned: %v", err)
			}
			if mismatched {
				t.Error("mismatched = true, want false")
			}
			if string(data) != string(body) {
				t.Errorf("data = %q, want %q", data, body)
			}
			if got := hits.Load(); got != 3 {
				t.Errorf("requests = %d, want 3 (two %d responses retried, then success)", got, status)
			}
		})
	}
}

func TestFetchPinnedGivesUpAfterRetryBudget(t *testing.T) {
	shortenRetries(t)
	url, hits := serve(t, func(w http.ResponseWriter, _ int64) {
		w.WriteHeader(http.StatusBadGateway)
	})

	_, mismatched, err := fetchPinned(plainSources(url), sha256hex([]byte("x")))
	if err == nil {
		t.Fatal("fetchPinned succeeded, want error")
	}
	if mismatched {
		t.Error("mismatched = true, want false: no bytes were ever compared to the pin")
	}
	if got, want := hits.Load(), int64(len(retryDelays)+1); got != want {
		t.Errorf("requests = %d, want %d", got, want)
	}
}

func TestFetchPinnedFallsBackToMirrorWithoutRetrying404(t *testing.T) {
	shortenRetries(t)
	body := []byte("mirrored-astap-zip-bytes")
	primary, primaryHits := serve(t, func(w http.ResponseWriter, _ int64) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	mirror, mirrorHits := serve(t, func(w http.ResponseWriter, _ int64) {
		w.Write(body)
	})

	data, mismatched, err := fetchPinned(plainSources(primary, mirror), sha256hex(body))
	if err != nil {
		t.Fatalf("fetchPinned: %v", err)
	}
	if mismatched {
		t.Error("mismatched = true, want false")
	}
	if string(data) != string(body) {
		t.Errorf("data = %q, want %q", data, body)
	}
	if got := primaryHits.Load(); got != 1 {
		t.Errorf("primary requests = %d, want 1: a 404 is upstream's final answer", got)
	}
	if got := mirrorHits.Load(); got != 1 {
		t.Errorf("mirror requests = %d, want 1", got)
	}
}

func TestFetchPinnedReportsMismatchWithoutRetrying(t *testing.T) {
	shortenRetries(t)
	primary, primaryHits := serve(t, func(w http.ResponseWriter, _ int64) {
		w.Write([]byte("rolled-release-bytes"))
	})
	mirror, mirrorHits := serve(t, func(w http.ResponseWriter, _ int64) {
		w.Write([]byte("also-rolled-bytes"))
	})

	_, mismatched, err := fetchPinned(plainSources(primary, mirror), sha256hex([]byte("pinned-bytes")))
	if err == nil {
		t.Fatal("fetchPinned succeeded, want error")
	}
	if !mismatched {
		t.Error("mismatched = false, want true: both URLs served bytes that failed the digest check")
	}
	if got := primaryHits.Load(); got != 1 {
		t.Errorf("primary requests = %d, want 1: a checksum mismatch is not transient", got)
	}
	if got := mirrorHits.Load(); got != 1 {
		t.Errorf("mirror requests = %d, want 1", got)
	}

	msg := fetchFailure("windows/amd64", mismatched, err)
	if !strings.Contains(msg, "update-astap-cli.yml") {
		t.Errorf("stale-pin message lacks the re-pin advice: %s", msg)
	}
	if strings.Contains(msg, "retry the job") {
		t.Errorf("stale-pin message wrongly advises retrying the job: %s", msg)
	}
}

func TestFetchFailureSeparatesTransportFromStalePin(t *testing.T) {
	transport := fetchFailure("windows/amd64", false, &httpError{status: 503, url: "http://example.invalid/x"})
	if !strings.Contains(transport, "retry the job") {
		t.Errorf("transport message lacks the retry advice: %s", transport)
	}
	if strings.Contains(transport, "update-astap-cli") {
		t.Errorf("transport message wrongly points at the update workflow: %s", transport)
	}
}

// TestFetchSourcesFallBackToTheMirror pins the fallback the real manifest produces.
// SourceForge's older/ folder is no fallback: it archives a single historical CLI
// snapshot under a per-platform folder name that cannot be predicted, so the mirror
// is the only fallback that survives an upstream roll.
func TestFetchSourcesFallBackToTheMirror(t *testing.T) {
	m := loadRepoManifest(t)
	if m.MirrorRepo == "" {
		t.Fatal("manifest has no mirror_repo: the fetch fallback has nowhere to look")
	}
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"", 2},       // upstream latest, then the mirror's public download URL
		{"s3cret", 3}, // ... then the mirror through the API with the token
	} {
		t.Setenv("GH_TOKEN", tc.token)
		t.Setenv("GITHUB_TOKEN", "")
		for key, p := range m.Platforms {
			srcs := fetchSources(m, p)
			if len(srcs) != tc.want {
				t.Fatalf("%s (token %q): %d sources, want %d", key, tc.token, len(srcs), tc.want)
			}
			for _, s := range srcs[1:] {
				if !strings.Contains(s.describe, m.MirrorRepo) ||
					!strings.Contains(s.describe, mirrorTag(m.Version)) ||
					!strings.Contains(s.describe, p.File) {
					t.Errorf("%s: fallback %q does not name the mirror repo, tag and file", key, s.describe)
				}
			}
			if want := "https://github.com/" + m.MirrorRepo + "/releases/download/"; !strings.HasPrefix(srcs[1].describe, want) {
				t.Errorf("%s: first fallback %q is not the public download URL", key, srcs[1].describe)
			}
		}
	}
}

// stubMirror points the public mirror URL and the API at one stub server, which
// answers the public download with publicStatus (serving body on 200) and the API
// release lookup and asset download as GitHub does.
func stubMirror(t *testing.T, publicStatus int, body []byte) (publicHits, apiHits *atomic.Int64) {
	t.Helper()
	publicHits, apiHits = &atomic.Int64{}, &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/owner/repo/releases/download/astap-cli-2026.09.01/"):
			publicHits.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("public download sent credentials: %q", r.Header.Get("Authorization"))
			}
			if publicStatus != http.StatusOK {
				w.WriteHeader(publicStatus)
				return
			}
			w.Write(body)
		case r.URL.Path == "/repos/owner/repo/releases/tags/astap-cli-2026.09.01":
			apiHits.Add(1)
			w.Write([]byte(`{"assets":[{"name":"want file.zip","id":42}]}`))
		case r.URL.Path == "/repos/owner/repo/releases/assets/42":
			apiHits.Add(1)
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	savedAPI, savedGitHub := apiBase, githubBase
	apiBase, githubBase = srv.URL, srv.URL
	t.Cleanup(func() { apiBase, githubBase = savedAPI, savedGitHub })
	return publicHits, apiHits
}

// rolledUpstream returns a manifest whose upstream latest URL serves bytes that no
// longer match the pin, so every fetch has to reach the mirror. The file name has a
// space so the public URL's escaping is exercised.
func rolledUpstream(t *testing.T) (manifest, platform) {
	t.Helper()
	base, _ := serve(t, func(w http.ResponseWriter, _ int64) {
		w.Write([]byte("rolled-release-bytes"))
	})
	return manifest{BaseURL: base, MirrorRepo: "owner/repo", Version: "2026.09.01"},
		platform{Dir: "d", File: "want file.zip"}
}

// TestFetchFallsBackToPublicMirrorWithoutToken covers the common fallback: upstream
// rolled, no token is set, and the public mirror serves the pinned bytes.
func TestFetchFallsBackToPublicMirrorWithoutToken(t *testing.T) {
	shortenRetries(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	body := []byte("mirrored-astap-zip-bytes")
	publicHits, apiHits := stubMirror(t, http.StatusOK, body)
	m, p := rolledUpstream(t)

	data, _, err := fetchPinned(fetchSources(m, p), sha256hex(body))
	if err != nil {
		t.Fatalf("fetchPinned: %v", err)
	}
	if string(data) != string(body) {
		t.Errorf("data = %q, want %q", data, body)
	}
	if publicHits.Load() != 1 || apiHits.Load() != 0 {
		t.Errorf("public hits = %d, API hits = %d; want 1 and 0", publicHits.Load(), apiHits.Load())
	}
}

// TestFetchUsesAPIMirrorOnlyWhenPublicFails covers a private mirror: its public URL
// 404s, and with a token set the API asset endpoint supplies the pinned bytes. While
// the public URL serves, the token is never spent on the API.
func TestFetchUsesAPIMirrorOnlyWhenPublicFails(t *testing.T) {
	shortenRetries(t)
	t.Setenv("GH_TOKEN", "s3cret")
	body := []byte("mirrored-astap-zip-bytes")

	t.Run("public serves", func(t *testing.T) {
		publicHits, apiHits := stubMirror(t, http.StatusOK, body)
		m, p := rolledUpstream(t)
		if _, _, err := fetchPinned(fetchSources(m, p), sha256hex(body)); err != nil {
			t.Fatalf("fetchPinned: %v", err)
		}
		if publicHits.Load() != 1 || apiHits.Load() != 0 {
			t.Errorf("public hits = %d, API hits = %d; want 1 and 0", publicHits.Load(), apiHits.Load())
		}
	})
	t.Run("public 404s", func(t *testing.T) {
		publicHits, apiHits := stubMirror(t, http.StatusNotFound, body)
		m, p := rolledUpstream(t)
		data, _, err := fetchPinned(fetchSources(m, p), sha256hex(body))
		if err != nil {
			t.Fatalf("fetchPinned: %v", err)
		}
		if string(data) != string(body) {
			t.Errorf("data = %q, want %q", data, body)
		}
		if publicHits.Load() != 1 || apiHits.Load() != 2 {
			t.Errorf("public hits = %d, API hits = %d; want 1 and 2 (lookup, download)", publicHits.Load(), apiHits.Load())
		}
	})
}

// TestFetchRejectsMirrorBytesThatMissThePin keeps checksum verification as strict on
// the public mirror as on upstream: wrong bytes are a mismatch, never a success.
func TestFetchRejectsMirrorBytesThatMissThePin(t *testing.T) {
	shortenRetries(t)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	stubMirror(t, http.StatusOK, []byte("tampered-bytes"))
	m, p := rolledUpstream(t)

	_, mismatched, err := fetchPinned(fetchSources(m, p), sha256hex([]byte("pinned-bytes")))
	if err == nil {
		t.Fatal("fetchPinned succeeded with bytes that miss the pin")
	}
	if !mismatched {
		t.Error("mismatched = false, want true")
	}
}

// TestMirrorAPISourceResolvesAssetIDAndAuthenticates covers the shape the private-repo
// fallback depends on: the plain releases/download URL 404s even with a token, so the
// zip is only reachable by looking its id up on the release and asking the API asset
// endpoint for octet-stream.
func TestMirrorAPISourceResolvesAssetIDAndAuthenticates(t *testing.T) {
	var gotPath, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.Write([]byte(`{"assets":[{"name":"other.zip","id":1},{"name":"want.zip","id":42}]}`))
	}))
	t.Cleanup(srv.Close)
	saved := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = saved })

	url, header, err := mirrorAPISource("owner/repo", "2026.09.01", "want.zip", "s3cret").resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := "/repos/owner/repo/releases/tags/astap-cli-2026.09.01"; gotPath != want {
		t.Errorf("lookup path = %q, want %q", gotPath, want)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("lookup Authorization = %q, want the bearer token", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("lookup Accept = %q, want the JSON API media type", gotAccept)
	}
	if !strings.HasSuffix(url, "/repos/owner/repo/releases/assets/42") {
		t.Errorf("asset URL = %q, want the id-addressed asset endpoint for the matching name", url)
	}
	if got := header.Get("Accept"); got != "application/octet-stream" {
		t.Errorf("download Accept = %q, want application/octet-stream", got)
	}
	if got := header.Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("download Authorization = %q, want the bearer token", got)
	}
}

// TestManifestRoundTripsUnchanged guards update-astap-cli.yml, which decides whether
// to open a PR from `git diff --quiet` on the manifest. If load+save were not
// byte-stable — a field the struct silently drops, a different key order or escaping —
// every weekly run would read as a bump and open an empty PR.
func TestManifestRoundTripsUnchanged(t *testing.T) {
	want, err := os.ReadFile(repoManifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "astap-cli.json")
	if err := os.WriteFile(dst, want, 0o644); err != nil {
		t.Fatalf("stage manifest: %v", err)
	}
	t.Setenv("ASTAP_MANIFEST", dst)
	save(load())

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read round-tripped manifest: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("manifest changed on load+save\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// repoManifest is the checked-in pin, read relative to this package's directory.
const repoManifest = "../../" + defaultManifest

func loadRepoManifest(t *testing.T) manifest {
	t.Helper()
	t.Setenv("ASTAP_MANIFEST", repoManifest)
	return load()
}

func TestTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404", &httpError{status: 404}, false},
		{"403", &httpError{status: 403}, false},
		{"400", &httpError{status: 400}, false},
		{"408", &httpError{status: 408}, true},
		{"429", &httpError{status: 429}, true},
		{"500", &httpError{status: 500}, true},
		{"503", &httpError{status: 503}, true},
		{"network", http.ErrHandlerTimeout, true},
	} {
		if got := transient(tc.err); got != tc.want {
			t.Errorf("transient(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
