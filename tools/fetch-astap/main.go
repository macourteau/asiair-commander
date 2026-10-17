// Command fetch-astap downloads the pinned ASTAP command-line solver (astap_cli)
// for a target platform, verifies its SHA-256 against deploy/astap/astap-cli.json,
// and unzips the binary into an output directory. The Docker image build and the
// Windows installer build call it to bundle the solver; plate solving then resolves
// astap_cli beside the service binary at runtime.
//
// Usage:
//
//	go run ./tools/fetch-astap fetch <goos/goarch> <outdir>
//	go run ./tools/fetch-astap mirror <outdir>   # stage every platform's pinned zip for the mirror release
//	go run ./tools/fetch-astap update            # refresh the manifest's checksums + version
//	go run ./tools/fetch-astap check             # report pinned-vs-live checksums (non-mutating)
//
// The pin is a specific ASTAP release + digest. `fetch` downloads the version-less
// "latest" URL and, if its bytes no longer match the pinned digest (upstream rolled
// the release), falls back to our own mirror of that version's zips so a pinned build
// stays byte-reproducible. The mirror exists because upstream retention cannot carry
// it: SourceForge overwrites `astap_command-line_version_*.zip` in place and its
// `older/` folder holds a single historical CLI snapshot, so every superseded pin is
// simply gone. Transient download failures on either URL are retried with backoff.
//
// The mirror is a public release, so the fallback downloads its plain
// releases/download URL without credentials. When GH_TOKEN or GITHUB_TOKEN is set
// and that URL fails, it tries once more through the API's asset endpoint with the
// token, which is the only way to read a mirror on a private repository (a fork, or
// the repository before it is published). No build needs a token for anything else.
//
// `update` (run by the weekly workflow) re-pins to whatever is current, `mirror`
// stages those bytes for publication, and the workflow opens a PR for manual review.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const defaultManifest = "deploy/astap/astap-cli.json"

type platform struct {
	Dir    string `json:"dir"`
	File   string `json:"file"`
	Bin    string `json:"bin"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Comment    string              `json:"_comment,omitempty"`
	Version    string              `json:"version"`
	BaseURL    string              `json:"base_url"`
	MirrorRepo string              `json:"mirror_repo"`
	Platforms  map[string]platform `json:"platforms"`
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: fetch-astap <fetch|update|check> ...")
	}
	switch os.Args[1] {
	case "fetch":
		if len(os.Args) != 4 {
			fatalf("usage: fetch-astap fetch <goos/goarch> <outdir>")
		}
		runFetch(os.Args[2], os.Args[3])
	case "mirror":
		if len(os.Args) != 3 {
			fatalf("usage: fetch-astap mirror <outdir>")
		}
		runMirror(os.Args[2])
	case "update":
		runUpdate(false)
	case "check":
		runUpdate(true)
	default:
		fatalf("unknown subcommand %q", os.Args[1])
	}
}

// runFetch downloads, verifies, and unzips the pinned binary for key into outdir.
func runFetch(key, outdir string) {
	m := load()
	p, ok := m.Platforms[key]
	if !ok {
		fatalf("no pinned astap_cli for platform %q", key)
	}
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		fatalf("mkdir %s: %v", outdir, err)
	}

	data, mismatched, err := fetchPinned(fetchSources(m, p), p.SHA256)
	if err != nil {
		fatalf("%s", fetchFailure(key, mismatched, err))
	}
	dst := filepath.Join(outdir, p.Bin)
	if err := unzipBin(data, p.Bin, dst); err != nil {
		fatalf("unzip %s: %v", p.File, err)
	}
	fmt.Printf("fetch-astap: bundled %s (astap %s) → %s\n", key, m.Version, dst)
}

// runMirror downloads every platform's zip from upstream, verifies it against the
// pinned digest, and writes it into outdir under its upstream filename. The weekly
// update workflow publishes those files as the mirror release that runFetch falls
// back to, which is what keeps a pinned build reproducible once SourceForge has
// overwritten the version-less URL. It refuses to stage anything that does not match
// the manifest, so the mirror can never disagree with the pin it claims to serve.
func runMirror(outdir string) {
	m := load()
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		fatalf("mkdir %s: %v", outdir, err)
	}
	keys := make([]string, 0, len(m.Platforms))
	for key := range m.Platforms {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := m.Platforms[key]
		data, mismatched, err := fetchPinned(plainSources(fileURL(m.BaseURL, p.Dir, p.File)), p.SHA256)
		if err != nil {
			fatalf("%s", fetchFailure(key, mismatched, err))
		}
		dst := filepath.Join(outdir, p.File)
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			fatalf("write %s: %v", dst, err)
		}
		fmt.Printf("fetch-astap: staged %-14s (astap %s) → %s\n", key, m.Version, dst)
	}
	fmt.Printf("fetch-astap: mirror release tag %s\n", mirrorTag(m.Version))
}

// fetchPinned tries each URL in order and returns the first payload whose SHA-256
// matches want. mismatched reports whether any URL actually served bytes that failed
// the digest check, which is what separates an upstream re-pin from a transport
// failure that never got as far as comparing anything.
func fetchPinned(srcs []source, want string) (data []byte, mismatched bool, err error) {
	var lastErr error
	for i, s := range srcs {
		u, header, err := s.resolve()
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", s.describe, err)
			logFallback(srcs, i, lastErr)
			continue
		}
		b, err := downloadRetrying(u, header)
		if err != nil {
			lastErr = err
			logFallback(srcs, i, lastErr)
			continue
		}
		got := sha256hex(b)
		if !strings.EqualFold(got, want) {
			mismatched = true
			lastErr = fmt.Errorf("checksum mismatch from %s: got %s, want %s", s.describe, got, want)
			logFallback(srcs, i, lastErr)
			continue
		}
		if i > 0 {
			fmt.Fprintf(os.Stderr, "fetch-astap: pinned zip supplied by %s\n", s.describe)
		}
		return b, false, nil
	}
	return nil, mismatched, lastErr
}

// logFallback says why fetchPinned is moving past a source. It is silent after the
// last source, where the caller's fatal message carries the error instead — and so
// for `mirror`, which passes a single source.
func logFallback(srcs []source, i int, err error) {
	if i+1 < len(srcs) {
		fmt.Fprintf(os.Stderr, "fetch-astap: %v; trying %s\n", err, srcs[i+1].describe)
	}
}

// fetchFailure renders the fatal message for a failed fetch. The two causes need
// opposite responses: a stale pin is fixed by re-pinning, while a download that never
// completed is fixed by running the job again, and pointing the reader at the update
// workflow for a transport blip sends them to a manifest that is already correct.
func fetchFailure(key string, mismatched bool, err error) string {
	if mismatched {
		return fmt.Sprintf("no astap_cli matching the pin for %s: %v\n"+
			"  (upstream is serving different bytes and the mirror did not supply the pinned\n"+
			"   version — the log above says why each source failed; a missing mirror release\n"+
			"   is published by update-astap-cli.yml, and a mirror on a private repository is\n"+
			"   reachable only with GH_TOKEN or GITHUB_TOKEN set)", key, err)
	}
	return fmt.Sprintf("could not download astap_cli for %s: %v\n"+
		"  (every candidate URL failed to download and the pin was never compared;\n"+
		"   this is a transport failure, not a stale pin — retry the job)", key, err)
}

// runUpdate re-fetches every platform's current latest URL, recomputes its SHA-256,
// and (unless checkOnly) rewrites the manifest in place, refreshing the version from
// the binary matching this runner's platform. It prints a per-platform report and
// leaves the exit code 0; the caller inspects the git diff to decide whether to open
// a PR.
func runUpdate(checkOnly bool) {
	m := load()
	changed := false
	for key, p := range m.Platforms {
		data, err := download(fileURL(m.BaseURL, p.Dir, p.File), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %-14s ERROR %v\n", key, err)
			continue
		}
		got := sha256hex(data)
		state := "unchanged"
		if !strings.EqualFold(got, p.SHA256) {
			state = "CHANGED"
			changed = true
			if !checkOnly {
				p.SHA256 = got
				m.Platforms[key] = p
			}
		}
		fmt.Printf("  %-14s %s  %s\n", key, state, got)

		// Refresh the version label from the binary matching this runner.
		if !checkOnly && key == runtime.GOOS+"/"+runtime.GOARCH {
			if v := versionFrom(data, p.Bin); v != "" && v != m.Version {
				fmt.Printf("  version: %s → %s\n", m.Version, v)
				m.Version = v
			}
		}
	}
	if checkOnly {
		if changed {
			fmt.Println("astap_cli: a newer release is available")
		}
		return
	}
	if !changed {
		fmt.Println("astap_cli: already current")
		return
	}
	save(m)
	fmt.Println("astap_cli: manifest updated")
}

// versionFrom extracts the ASTAP version from a binary's startup banner
// ("ASTAP astrometric solver version CLI-2026.07.16"). It runs the binary only when
// it matches this runner's platform (so the workflow, on linux/amd64, reads it).
func versionFrom(zipData []byte, bin string) string {
	tmp, err := os.MkdirTemp("", "astapver-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmp)
	exe := filepath.Join(tmp, bin)
	if err := unzipBin(zipData, bin, exe); err != nil {
		return ""
	}
	out, _ := exec.Command(exe).CombinedOutput() // exits non-zero with usage; banner is on stdout/stderr
	if m := regexp.MustCompile(`CLI-([0-9][0-9.\-]+)`).FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// --- helpers -------------------------------------------------------------------

func fileURL(base, dir, file string) string {
	esc := func(s string) string { return strings.ReplaceAll(s, " ", "%20") }
	return base + "/" + esc(dir) + "/" + esc(file) + "/download"
}

// source is one place a pinned zip can come from. resolve runs only when a source is
// actually reached, which matters because resolving the mirror through the API costs
// a lookup that the common path, where upstream still serves the pinned bytes, should
// never pay.
type source struct {
	describe string
	resolve  func() (url string, header http.Header, err error)
}

// plainSources wraps bare URLs that need no lookup and no headers.
func plainSources(urls ...string) []source {
	srcs := make([]source, 0, len(urls))
	for _, u := range urls {
		u := u
		srcs = append(srcs, source{
			describe: u,
			resolve:  func() (string, http.Header, error) { return u, nil, nil },
		})
	}
	return srcs
}

// fetchSources lists where a platform's pinned zip can be found, in preference order:
// upstream's version-less latest, then our mirror of the pinned version — its public
// download URL, and, only when a token is available, the API asset endpoint. The
// mirror comes after upstream because it only ever holds what upstream has already
// superseded.
func fetchSources(m manifest, p platform) []source {
	srcs := plainSources(
		fileURL(m.BaseURL, p.Dir, p.File),
		mirrorDownloadURL(m.MirrorRepo, m.Version, p.File),
	)
	if token := githubToken(); token != "" {
		srcs = append(srcs, mirrorAPISource(m.MirrorRepo, m.Version, p.File, token))
	}
	return srcs
}

// mirrorDownloadURL is a mirror asset's public download URL. It needs no credentials
// while the mirror repository is public, and answers 404 when it is private.
func mirrorDownloadURL(repo, version, file string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", githubBase, repo, mirrorTag(version), url.PathEscape(file))
}

// mirrorAPISource resolves a platform's zip through the API's asset endpoint, the only
// way to read a mirror asset on a private repository: its releases/download URL
// answers 404 even when a token is supplied. That costs a lookup, because the
// endpoint is addressed by asset id rather than name.
func mirrorAPISource(repo, version, file, token string) source {
	tag := mirrorTag(version)
	return source{
		describe: "mirror " + repo + " " + tag + "/" + file + " via the API",
		resolve: func() (string, http.Header, error) {
			id, err := mirrorAssetID(repo, tag, file, token)
			if err != nil {
				return "", nil, err
			}
			h := http.Header{}
			h.Set("Authorization", "Bearer "+token)
			h.Set("Accept", "application/octet-stream")
			return fmt.Sprintf("%s/repos/%s/releases/assets/%d", apiBase, repo, id), h, nil
		},
	}
}

// mirrorAssetID finds the id of the asset named file on the release tagged tag.
func mirrorAssetID(repo, tag, file, token string) (int64, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/vnd.github+json")
	body, err := downloadRetrying(fmt.Sprintf("%s/repos/%s/releases/tags/%s", apiBase, repo, tag), h)
	if err != nil {
		return 0, fmt.Errorf("look up release %s: %w", tag, err)
	}
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			ID   int64  `json:"id"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return 0, fmt.Errorf("parse release %s: %w", tag, err)
	}
	for _, a := range rel.Assets {
		if a.Name == file {
			return a.ID, nil
		}
	}
	return 0, fmt.Errorf("release %s has no asset named %s", tag, file)
}

// apiBase and githubBase are variables so tests can point the mirror at a stub server.
var (
	apiBase    = "https://api.github.com"
	githubBase = "https://github.com"
)

// githubToken reads the token for the API mirror fallback, preferring the name `gh`
// sets so a developer's `GH_TOKEN=$(gh auth token)` wins over a narrower CI token.
func githubToken() string {
	if t := os.Getenv("GH_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GITHUB_TOKEN")
}

// mirrorTag names the release that mirrors one pinned ASTAP version's zips. It is a
// prerelease so it stays out of the product release stream.
func mirrorTag(version string) string { return "astap-cli-" + version }

// retryDelays is the backoff schedule for transient download failures: one entry per
// retry, so a URL is attempted len(retryDelays)+1 times at most.
var retryDelays = []time.Duration{2 * time.Second, 4 * time.Second}

// httpError is a non-200 response, kept typed so the retry logic can tell a server-side
// blip from a definitive answer.
type httpError struct {
	status int
	url    string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d for %s", e.status, e.url) }

// transient reports whether err is worth retrying: network and read failures always
// are, and so is a 5xx. A 4xx is upstream's final answer — in particular a 404 on the
// latest URL is the signal to fall back to the mirror, not to keep asking.
//
// Two 4xx are exceptions, and the rule is not "5xx only" however much it looks like it:
// 429 is SourceForge throttling a CI download, and 408 is the server abandoning a
// request it was willing to serve. Both describe a moment rather than a verdict, and 429
// in particular is the likeliest shape of the blip this retry exists to survive.
func transient(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status >= 500 ||
			he.status == http.StatusTooManyRequests ||
			he.status == http.StatusRequestTimeout
	}
	return true
}

// downloadRetrying downloads url, backing off over retryDelays on transient failures so
// a single blip on SourceForge does not fail an entire build.
func downloadRetrying(url string, header http.Header) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		data, err := download(url, header)
		if err == nil {
			return data, nil
		}
		if attempt >= len(retryDelays) || !transient(err) {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "fetch-astap: %v; retrying in %s\n", err, retryDelays[attempt])
		time.Sleep(retryDelays[attempt])
	}
}

func download(url string, header http.Header) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{status: resp.StatusCode, url: url}
	}
	return io.ReadAll(resp.Body)
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// unzipBin extracts the entry whose base name is bin (guarding against a
// subdirectory path) from a zip in memory, writing it to dst with 0755.
func unzipBin(zipData []byte, bin, dst string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != bin {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, rc)
		return err
	}
	return fmt.Errorf("%s not found in archive", bin)
}

func manifestPath() string {
	if p := os.Getenv("ASTAP_MANIFEST"); p != "" {
		return p
	}
	return defaultManifest
}

func load() manifest {
	b, err := os.ReadFile(manifestPath())
	if err != nil {
		fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		fatalf("parse manifest: %v", err)
	}
	return m
}

func save(m manifest) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath(), append(b, '\n'), 0o644); err != nil {
		fatalf("write manifest: %v", err)
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "fetch-astap: "+format+"\n", a...)
	os.Exit(1)
}
