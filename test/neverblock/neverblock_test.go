// Package neverblock is the design architecture §14 "never-block" assertion,
// proving design architecture §3: the HTTP/UI read path never performs SMB I/O.
// It meets the rig workers only at the in-memory Store and StatusHub, so a slow
// or hung rig can never stall the dashboard.
//
// The test stands up the real stack (store + hub + notify + localfs + admission +
// manager + web) with a device whose ListDir/OpenReader sleep for seconds,
// simulating a slow/hung SMB share. While the worker goroutines are blocked in
// that slow SMB, many concurrent clients hammer the UI read path (/healthz,
// /events, /api/alerts, /api/config). Every response must return in a tiny
// fraction of the device delay: if the HTTP path touched SMB, a single request
// would take at least the full device sleep and blow the latency bound.
package neverblock

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/manager"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/store"
	"github.com/macourteau/asiair-commander/internal/web"
	"golang.org/x/crypto/bcrypt"
)

// noopResolver is a rig.Resolver that never resolves and never confirms a probe:
// the never-block rigs are host-bound, so resolution falls through to the legacy
// Host and no guid migration bind is ever recorded.
type noopResolver struct{}

func (noopResolver) Resolve(string, string) (netsite.Endpoint, bool) {
	return netsite.Endpoint{}, false
}
func (noopResolver) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return "", "", false
}

const (
	// deviceDelay is how long the slow device sleeps inside every ListDir /
	// OpenReader — the stand-in for a hung SMB share. Any HTTP request that
	// reached SMB would inherit at least this delay.
	deviceDelay = 2 * time.Second
	// latencyBound is the ceiling every UI read-path response must stay under. It
	// is an order of magnitude below deviceDelay: comfortably above real localhost
	// round-trip jitter under -race, yet far below the 2s a device-touching path
	// would cost, so the two are never confusable.
	latencyBound = 500 * time.Millisecond
	// testPassword is the shared password used to authenticate the read path.
	testPassword = "correct-horse-battery-staple"
)

// slowDevice wraps a device.Device and makes its share listings and reads sleep,
// simulating a slow/hung rig. Every other operation delegates unchanged.
type slowDevice struct {
	inner device.Device
	delay time.Duration
	// hang, when set, replaces the sleep: ListDir signals entered (once) and then
	// blocks until hang is closed, a device that stops answering mid-listing.
	hang *hang
}

// hang is a listing that never returns until released.
type hang struct {
	entered chan struct{}
	once    sync.Once
	release chan struct{}
}

func newHang() *hang { return &hang{entered: make(chan struct{}), release: make(chan struct{})} }

func (h *hang) wait() {
	h.once.Do(func() { close(h.entered) })
	<-h.release
}

func (d slowDevice) Probe(ctx context.Context) bool { return d.inner.Probe(ctx) }

func (d slowDevice) Open(ctx context.Context) (device.Session, error) {
	sess, err := d.inner.Open(ctx)
	if err != nil {
		return nil, err
	}
	return slowSession{inner: sess, delay: d.delay, hang: d.hang}, nil
}

type slowSession struct {
	inner device.Session
	delay time.Duration
	hang  *hang
}

func (s slowSession) ServerTime() clock.ServerTime  { return s.inner.ServerTime() }
func (s slowSession) ListShares() ([]string, error) { return s.inner.ListShares() }
func (s slowSession) Close() error                  { return s.inner.Close() }

func (s slowSession) Mount(share string) (device.Share, error) {
	sh, err := s.inner.Mount(share)
	if err != nil {
		return nil, err
	}
	return slowShare{inner: sh, delay: s.delay, hang: s.hang}, nil
}

type slowShare struct {
	inner device.Share
	delay time.Duration
	hang  *hang
}

func (s slowShare) Name() string { return s.inner.Name() }

// ListDir and OpenReader are the two hot SMB paths a worker spends its cycle in;
// sleeping here keeps every worker parked in slow SMB for the duration of the test.
func (s slowShare) ListDir(dir string) ([]device.FileInfo, error) {
	if s.hang != nil {
		s.hang.wait()
	} else {
		time.Sleep(s.delay)
	}
	return s.inner.ListDir(dir)
}

func (s slowShare) OpenReader(path string) (io.ReadCloser, error) {
	time.Sleep(s.delay)
	return s.inner.OpenReader(path)
}

func (s slowShare) Stat(path string) (device.FileInfo, error)   { return s.inner.Stat(path) }
func (s slowShare) Delete(path string) error                    { return s.inner.Delete(path) }
func (s slowShare) Statfs() (free, total int64, err error)      { return s.inner.Statfs() }
func (s slowShare) VolumeInfo() (label, serial string, e error) { return s.inner.VolumeInfo() }
func (s slowShare) ReadMarker(name string) ([]byte, error)      { return s.inner.ReadMarker(name) }
func (s slowShare) WriteMarkerAt(dir, name string, data []byte) error {
	return s.inner.WriteMarkerAt(dir, name, data)
}
func (s slowShare) CreateNew(path string) (io.WriteCloser, error) { return s.inner.CreateNew(path) }
func (s slowShare) Mkdir(path string) error                       { return s.inner.Mkdir(path) }
func (s slowShare) Rename(oldPath, newPath string) error          { return s.inner.Rename(oldPath, newPath) }
func (s slowShare) Chtimes(path string, atime, mtime time.Time) error {
	return s.inner.Chtimes(path, atime, mtime)
}
func (s slowShare) Umount() error { return s.inner.Umount() }

func TestHTTPReadPathNeverBlocksOnSlowSMB(t *testing.T) {
	dir := t.TempDir()
	lfs := localfs.New()

	cfg := config.Defaults()
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	// Roots must exist before the manager's startup night-counter rebuild scans them.
	for _, root := range []string{cfg.Roots.Light, cfg.Roots.Cal, cfg.Roots.Staging, cfg.Roots.Logs} {
		if err := lfs.MkdirAll(root); err != nil {
			t.Fatalf("mkdir %s: %v", root, err)
		}
	}
	// A short poll keeps the worker constantly re-entering the slow device; no
	// quiescence delay so it also attempts the (slow) pull.
	cfg.Defaults.PollInterval = model.Duration(50 * time.Millisecond)
	cfg.Defaults.RetryInterval = model.Duration(50 * time.Millisecond)
	cfg.Defaults.MinFileAge = model.Duration(0)
	cfg.Defaults.SettleCycles = 0
	cfg.DiskReserveBytes = 0

	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	cfg.PasswordHash = string(hash)
	cfg.SessionSecret = "dGVzdC1zZXNzaW9uLXNlY3JldC1uZXZlcmJsb2Nr" // fixed base64 secret

	const share = "EMMC Images"
	cfg.Rigs = []model.RigProfile{{
		ID: "rig-01", Name: "Slow", Host: "slow.invalid", OTA: "FRA300", Session: "S",
		Managed: true,
		Sources: []model.Source{{Share: share, Roots: []string{"Autorun"}}},
	}}

	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hb := hub.New(logger)
	ntf := notify.New(st, hb, nil, logger)

	// A fake device with a listable frame, wrapped so its SMB path sleeps.
	fake := device.NewFake()
	fake.AddShare(share)
	darkPath := filepath.Join("..", "..", "internal", "header", "testdata", "asiair_dark_6200mm.fit")
	darkBytes, err := os.ReadFile(darkPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fake.AddFile(share, "Autorun/Dark/Dark_0001.fit", darkBytes, time.Now().Add(-time.Hour))
	factory := func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
		return slowDevice{inner: fake, delay: deviceDelay}
	}

	adm := manager.NewAdmission(3, lfs, cfg.Roots.Staging, 0)
	mgr := manager.New(ptr, adm, factory, lfs, header.New(), st, hb, ntf, nil, nil, time.UTC, logger,
		noopResolver{}, netsite.NewRegistry(logger), &config.Guard{}, filepath.Join(dir, "config.yaml"), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("manager.Start: %v", err)
	}
	defer mgr.Stop()

	srv := web.New(web.Deps{
		ConfigPath: filepath.Join(dir, "config.yaml"),
		Config:     ptr,
		Supervisor: mgr,
		Store:      st,
		Hub:        hb,
		LocalFS:    lfs,
		Header:     header.New(),
		Logger:     logger,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Authenticate once (the login itself is not part of the measured read path).
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	login(t, client, ts.URL)

	// Let the worker enter a cycle so it is parked inside the slow device's
	// 2s ListDir when the hammering starts.
	time.Sleep(300 * time.Millisecond)

	// Read-path endpoints: /healthz (unauth), /api/alerts + /api/config (authed
	// reads), and /events (the SSE snapshot). None may touch SMB.
	type target struct {
		path string
		sse  bool
	}
	targets := []target{
		{path: "/healthz"},
		{path: "/api/alerts"},
		{path: "/api/config"},
		{path: "/events", sse: true},
	}

	var (
		mu    sync.Mutex
		durs  []time.Duration
		fails []string
	)
	record := func(d time.Duration, err error, what string) {
		mu.Lock()
		defer mu.Unlock()
		durs = append(durs, d)
		if err != nil {
			fails = append(fails, what+": "+err.Error())
		}
	}

	const goroutines = 16
	const rounds = 12
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for _, tg := range targets {
					var d time.Duration
					var err error
					if tg.sse {
						d, err = timeSSE(ctx, client, ts.URL+tg.path)
					} else {
						d, err = timeGet(client, ts.URL+tg.path)
					}
					record(d, err, tg.path)
				}
			}
		}()
	}
	wg.Wait()

	if len(fails) > 0 {
		t.Fatalf("%d read-path requests failed, e.g. %s", len(fails), fails[0])
	}

	maxD, p99, mean := stats(durs)
	t.Logf("never-block: %d read-path requests; device SMB delay=%s; max=%s p99=%s mean=%s; bound=%s",
		len(durs), deviceDelay, maxD, p99, mean, latencyBound)

	if maxD >= deviceDelay {
		t.Fatalf("a read-path request took %s (>= the %s device delay): the HTTP path touched SMB", maxD, deviceDelay)
	}
	if maxD > latencyBound {
		t.Fatalf("max read-path latency %s exceeds the %s bound (device delay %s)", maxD, latencyBound, deviceDelay)
	}
}

// staticStatus is a fixed hub snapshot publishing where the rig's worker last
// found its device, which is all the device file browser reads from the hub.
type staticStatus struct{ snap model.Snapshot }

func (s staticStatus) Snapshot() model.Snapshot { return s.snap }

// echoProber confirms every endpoint as the rig's own device.
type echoProber struct{ guid string }

func (p echoProber) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return p.guid, "", true
}

// TestDevfilesListingNeverBlocksReadPath extends the never-block assertion to
// the device file browser, whose device I/O runs on the request goroutine
// (design device-file-browser §2 decision 2): while a listing
// is parked inside a device that has stopped answering, /api/alerts and
// /api/config keep answering within the bound, because the service shares no
// lock with the hub, the store or the config path.
func TestDevfilesListingNeverBlocksReadPath(t *testing.T) {
	dir := t.TempDir()

	cfg := config.Defaults()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	cfg.PasswordHash = string(hash)
	cfg.SessionSecret = "dGVzdC1zZXNzaW9uLXNlY3JldC1uZXZlcmJsb2Nr"
	const share = "EMMC Images"
	cfg.Rigs = []model.RigProfile{{
		ID: "rig-01", Name: "Hung", GUID: "g1", OTA: "FRA300", Session: "S", Managed: true,
		Sources: []model.Source{{Share: share, Roots: []string{"Autorun"}}},
	}}
	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hb := hub.New(logger)

	fake := device.NewFake()
	fake.AddShare(share)
	fake.AddFile(share, "Preview/a.fit", []byte("frame"), time.Now().Add(-time.Hour))
	hung := newHang()
	files := devfiles.New(devfiles.Deps{
		Config: ptr,
		Store:  st,
		Status: staticStatus{snap: model.Snapshot{Rigs: []model.RigStatus{{ID: "rig-01", Address: "10.0.0.1"}}}},
		Prober: echoProber{guid: "g1"},
		Sites:  netsite.NewRegistry(logger),
		NewDevice: func(netsite.Endpoint, string, string, time.Duration) device.Device {
			return slowDevice{inner: fake, hang: hung}
		},
		Logger: logger,
	})
	defer files.Close()

	srv := web.New(web.Deps{
		ConfigPath:  filepath.Join(dir, "config.yaml"),
		Config:      ptr,
		Store:       st,
		Hub:         hb,
		Header:      header.New(),
		DeviceFiles: files,
		Logger:      logger,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	login(t, client, ts.URL)

	// Park a listing inside the hung device. Released at the end, or on failure,
	// so the server can shut down.
	listed := make(chan int, 1)
	go func() {
		resp, err := client.Get(ts.URL + "/api/rigs/rig-01/files?share=EMMC%20Images&dir=Preview")
		if err != nil {
			listed <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		listed <- resp.StatusCode
	}()
	released := false
	release := func() {
		if !released {
			released = true
			close(hung.release)
		}
	}
	defer release()
	select {
	case <-hung.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the listing never reached the device")
	}

	var (
		mu    sync.Mutex
		durs  []time.Duration
		fails []string
	)
	const goroutines = 16
	const rounds = 12
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				for _, path := range []string{"/api/alerts", "/api/config"} {
					d, err := timeGet(client, ts.URL+path)
					mu.Lock()
					durs = append(durs, d)
					if err != nil {
						fails = append(fails, path+": "+err.Error())
					}
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	select {
	case code := <-listed:
		t.Fatalf("the hung listing returned (%d) before it was released: the test measured nothing", code)
	default:
	}
	if len(fails) > 0 {
		t.Fatalf("%d read-path requests failed, e.g. %s", len(fails), fails[0])
	}
	maxD, p99, mean := stats(durs)
	t.Logf("never-block (devfiles): %d read-path requests beside a hung listing; max=%s p99=%s mean=%s; bound=%s",
		len(durs), maxD, p99, mean, latencyBound)
	if maxD > latencyBound {
		t.Fatalf("max read-path latency %s beside a hung devfiles listing exceeds the %s bound", maxD, latencyBound)
	}

	release()
	select {
	case code := <-listed:
		if code != http.StatusOK {
			t.Fatalf("the released listing answered %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the released listing never answered")
	}
}

// login performs the shared-password login, seeding the client's cookie jar with
// the session + XSRF cookies used for the authed reads.
func login(t *testing.T, client *http.Client, base string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/session",
		strings.NewReader(`{"password":"`+testPassword+`"}`))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	req.Header.Set("Origin", base) // guardLogin checks Host + Origin
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status=%d body=%s", resp.StatusCode, body)
	}
}

// timeGet issues a GET, drains the (small) body, and returns the round-trip time.
func timeGet(client *http.Client, url string) (time.Duration, error) {
	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		return time.Since(start), err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return time.Since(start), nil
}

// timeSSE opens the SSE stream, measures the time to the response headers +
// initial snapshot bytes, then closes it. The stream is otherwise unbounded, so
// the request context is cancelled after the first read.
func timeSSE(parent context.Context, client *http.Client, url string) (time.Duration, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), err
	}
	defer resp.Body.Close()
	// Read the immediately-flushed initial snapshot to prove data actually flows.
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	return time.Since(start), nil
}

// stats returns the max, p99, and mean of the samples.
func stats(ds []time.Duration) (maxD, p99, mean time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	maxD = sorted[len(sorted)-1]
	p99idx := (len(sorted) * 99) / 100
	if p99idx >= len(sorted) {
		p99idx = len(sorted) - 1
	}
	p99 = sorted[p99idx]
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	mean = total / time.Duration(len(sorted))
	return maxD, p99, mean
}
