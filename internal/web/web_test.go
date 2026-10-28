package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const testPassword = "hunter2"
const testVersion = "v9.9.9-test"

// testStartedAt is a fixed process-start time the harness injects so server_info
// assertions have a known unix value.
var testStartedAt = time.Unix(1700000000, 0)

// fakeSupervisor records the per-rig commands it receives and returns
// ErrRigNotFound for any id outside its known set, so the 404-mapping path is
// exercised.
type fakeSupervisor struct {
	mu         sync.Mutex
	calls      []string
	known      map[string]bool
	applied    *model.Config
	discardErr error   // when non-nil, DiscardFrame returns it for a known rig
	restoreErr error   // when non-nil, RestoreFrame returns it for a known rig
	ackErr     error   // when non-nil, AcknowledgeMissing[IDs] returns it for a known rig
	ackIDs     []int64 // the ids AcknowledgeMissingIDs last received
	forgetErr  error   // when non-nil, ForgetFrame/ForgetLeftovers return it for a known rig
	forgetIDs  []int64 // the ids ForgetLeftovers last received
	// forgetApply, when non-nil, stands in for the worker's write so a test can
	// exercise the real store guard behind a bulk Forget confirm.
	forgetApply func(rigID string, ids []int64) (int, error)

	// Retarget: previews is returned verbatim by PreviewRetarget for a known rig;
	// retargetErr, when non-nil, is returned per-frame by RetargetFrame; otherwise
	// RetargetFrame reports a "retargeted" result. retargetIDs records the ids
	// RetargetFrame was called with, in order.
	previews    []model.RetargetPreview
	retargetErr error
	retargetIDs []int64

	// Re-file: refilePreview and refileResults/refileSummary are handed back for a
	// known rig; refileErr, when non-nil, is returned instead. refileScope and
	// refileArgs record what the handler passed through. refileBlock — when non-nil
	// — makes RefileGroup wait on it or on the request context, standing in for a
	// group in flight, and refileEntered is closed as it starts waiting, so a test
	// can cancel the request at exactly that point and prove the handler passed a
	// cancellable request context rather than a detached one.
	refilePreview model.RefilePreview
	refileResults []model.RefileResult
	refileSummary model.RefileSummary
	refileErr     error
	refileScope   string
	refileArgs    struct {
		toDir string
		ids   []int64
	}
	refileBlock   chan struct{}
	refileEntered chan struct{}
	refileCtxErr  error
}

var _ Supervisor = (*fakeSupervisor)(nil)

func (f *fakeSupervisor) cmd(name, id string) error {
	if !f.known[id] {
		return ErrRigNotFound
	}
	f.mu.Lock()
	f.calls = append(f.calls, name+":"+id)
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) Pause(id string) error         { return f.cmd("pause", id) }
func (f *fakeSupervisor) Resume(id string) error        { return f.cmd("resume", id) }
func (f *fakeSupervisor) RunNow(id string) error        { return f.cmd("run-now", id) }
func (f *fakeSupervisor) StopRun(id string) error       { return f.cmd("stop-run", id) }
func (f *fakeSupervisor) Reclassify(id string) error    { return f.cmd("reclassify", id) }
func (f *fakeSupervisor) ResetIdentity(id string) error { return f.cmd("reset-identity", id) }
func (f *fakeSupervisor) DiscardFrame(id string, frameID int64) error {
	if !f.known[id] {
		return ErrRigNotFound
	}
	if f.discardErr != nil {
		return f.discardErr
	}
	f.mu.Lock()
	f.calls = append(f.calls, "discard:"+id+":"+strconv.FormatInt(frameID, 10))
	f.mu.Unlock()
	return nil
}
func (f *fakeSupervisor) LinkLibrary(id, root string) error { return f.cmd("link:"+root, id) }
func (f *fakeSupervisor) RescanLibrary(id string) error     { return f.cmd("rescan", id) }
func (f *fakeSupervisor) RestoreFrame(id string, frameID int64) error {
	if !f.known[id] {
		return ErrRigNotFound
	}
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.mu.Lock()
	f.calls = append(f.calls, "restore:"+id+":"+strconv.FormatInt(frameID, 10))
	f.mu.Unlock()
	return nil
}
func (f *fakeSupervisor) CancelRestore(id string, frameID int64) error {
	return f.cmd("cancel-restore:"+strconv.FormatInt(frameID, 10), id)
}
func (f *fakeSupervisor) AcknowledgeMissing(id string, frameID int64) error {
	if !f.known[id] {
		return ErrRigNotFound
	}
	if f.ackErr != nil {
		return f.ackErr
	}
	f.mu.Lock()
	f.calls = append(f.calls, "ack:"+id+":"+strconv.FormatInt(frameID, 10))
	f.mu.Unlock()
	return nil
}
func (f *fakeSupervisor) AcknowledgeMissingIDs(id, dir string, ids []int64) (int, error) {
	if !f.known[id] {
		return 0, ErrRigNotFound
	}
	if f.ackErr != nil {
		return 0, f.ackErr
	}
	f.mu.Lock()
	f.calls = append(f.calls, "ack-ids:"+id+":"+dir+":"+strconv.Itoa(len(ids)))
	f.ackIDs = append([]int64(nil), ids...)
	f.mu.Unlock()
	return len(ids), nil
}
func (f *fakeSupervisor) ForgetFrame(id string, frameID int64) error {
	if !f.known[id] {
		return ErrRigNotFound
	}
	if f.forgetErr != nil {
		return f.forgetErr
	}
	f.mu.Lock()
	f.calls = append(f.calls, "forget:"+id+":"+strconv.FormatInt(frameID, 10))
	f.mu.Unlock()
	return nil
}
func (f *fakeSupervisor) ForgetLeftovers(id, dir string, olderThanDays int, ids []int64) (int, error) {
	if !f.known[id] {
		return 0, ErrRigNotFound
	}
	if f.forgetErr != nil {
		return 0, f.forgetErr
	}
	f.mu.Lock()
	f.calls = append(f.calls, "forget-ids:"+id+":"+dir+":"+strconv.Itoa(olderThanDays)+":"+strconv.Itoa(len(ids)))
	f.forgetIDs = append([]int64(nil), ids...)
	apply := f.forgetApply
	f.mu.Unlock()
	if apply != nil {
		return apply(id, ids)
	}
	return len(ids), nil
}
func (f *fakeSupervisor) RetargetFrame(id string, frameID int64, newTarget string) (model.RetargetResult, error) {
	if !f.known[id] {
		return model.RetargetResult{}, ErrRigNotFound
	}
	if f.retargetErr != nil {
		return model.RetargetResult{}, f.retargetErr
	}
	f.mu.Lock()
	f.retargetIDs = append(f.retargetIDs, frameID)
	f.mu.Unlock()
	return model.RetargetResult{
		FrameID: frameID, Status: "retargeted",
		OldPath: "/old/" + newTarget, NewPath: "/new/" + newTarget,
	}, nil
}
func (f *fakeSupervisor) PreviewRetarget(id string, ids []int64, newTarget string) ([]model.RetargetPreview, error) {
	if !f.known[id] {
		return nil, ErrRigNotFound
	}
	return f.previews, nil
}
func (f *fakeSupervisor) PreviewRefile(ctx context.Context, id, scope string) (model.RefilePreview, error) {
	if !f.known[id] {
		return model.RefilePreview{}, ErrRigNotFound
	}
	f.mu.Lock()
	f.refileScope = scope
	f.mu.Unlock()
	return f.refilePreview, f.refileErr
}

func (f *fakeSupervisor) RefileGroup(ctx context.Context, id, toDir string, ids []int64) ([]model.RefileResult, model.RefileSummary, error) {
	if !f.known[id] {
		return nil, model.RefileSummary{}, ErrRigNotFound
	}
	f.mu.Lock()
	f.refileArgs.toDir = toDir
	f.refileArgs.ids = append([]int64(nil), ids...)
	block := f.refileBlock
	if f.refileEntered != nil {
		close(f.refileEntered)
		f.refileEntered = nil
	}
	f.mu.Unlock()
	if block != nil {
		// Stand in for a group in flight: the worker checks its context between
		// frames, so a request the client abandoned must arrive here already cancelled.
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.refileCtxErr = ctx.Err()
			f.mu.Unlock()
			return nil, model.RefileSummary{}, ctx.Err()
		}
	}
	if f.refileErr != nil {
		return nil, model.RefileSummary{}, f.refileErr
	}
	return f.refileResults, f.refileSummary, nil
}

func (f *fakeSupervisor) PauseAll() {
	f.mu.Lock()
	f.calls = append(f.calls, "pause-all")
	f.mu.Unlock()
}
func (f *fakeSupervisor) ResumeAll() {
	f.mu.Lock()
	f.calls = append(f.calls, "resume-all")
	f.mu.Unlock()
}
func (f *fakeSupervisor) ApplyConfig(c *model.Config) {
	f.mu.Lock()
	f.applied = c
	f.mu.Unlock()
}

type fakeScanner struct {
	scanned       []model.DiscoveredRig
	discovered    []model.DiscoveredRig
	scanCalls     int
	onDemandCalls int
	shares        []string
	sharesErr     error
}

func (f *fakeScanner) Scan(ctx context.Context) []model.DiscoveredRig {
	f.scanCalls++
	return f.scanned
}

// onDemandCalls counts separately so a test can prove /api/scan takes the
// logging path rather than the background one.
func (f *fakeScanner) ScanOnDemand(ctx context.Context) []model.DiscoveredRig {
	f.onDemandCalls++
	return f.scanned
}
func (f *fakeScanner) Discovered() []model.DiscoveredRig { return f.discovered }
func (f *fakeScanner) DeviceShares(ctx context.Context, guid string) ([]string, error) {
	return f.shares, f.sharesErr
}

type harness struct {
	t       *testing.T
	srv     *Server
	ts      *httptest.Server
	client  *http.Client
	cfgPtr  *atomic.Pointer[model.Config]
	store   store.Store
	hub     *hub.StatusHub
	sup     *fakeSupervisor
	scanner *fakeScanner
	appLog  *LogBuffer
	lfs     *localfs.Fake
	prov    *fakeProv
	secret  string
	baseURL string
	dataDir string
}

// opts adjust the Deps before the server is built (an optional collaborator, say).
func newHarness(t *testing.T, withPassword bool, opts ...func(*Deps)) *harness {
	t.Helper()
	appLog := NewLogBuffer(200)
	logger := slog.New(appLog.Handler())

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	h := hub.New(logger)

	cfg := config.Defaults()
	cfg.Web.BcryptCost = bcrypt.MinCost // fast hashing in tests
	cfg.Web.LoginBackoffBase = model.Duration(30 * time.Second)
	secret := "test-session-secret-0123456789"
	if withPassword {
		hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		cfg.PasswordHash = string(hash)
		cfg.SessionSecret = secret
	}

	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	sup := &fakeSupervisor{known: map[string]bool{"rig-1": true}}
	scanner := &fakeScanner{}
	lfs := localfs.NewFake(t.TempDir())
	prov := &fakeProv{installed: map[string]bool{}}

	dataDir := t.TempDir()
	deps := Deps{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		DataDir:    dataDir,
		Config:     ptr,
		Supervisor: sup,
		Store:      st,
		Hub:        h,
		Scanner:    scanner,
		LocalFS:    lfs,
		Prov:       prov,
		AppLog:     appLog,
		AppLogPath: filepath.Join(dataDir, "logs", "asiair-commander.log"),
		Logger:     logger,
		Version:    testVersion,
		StartedAt:  testStartedAt,
	}
	for _, o := range opts {
		o(&deps)
	}
	srv := New(deps)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	return &harness{
		t: t, srv: srv, ts: ts, client: &http.Client{Jar: jar},
		cfgPtr: ptr, store: st, hub: h, sup: sup, scanner: scanner,
		appLog: appLog, lfs: lfs, prov: prov, secret: secret, baseURL: ts.URL,
		dataDir: dataDir,
	}
}

// reqOpts tunes a request: whether to attach the CSRF header, and Host/Origin
// overrides for the negative-path tests.
type reqOpts struct {
	csrf   bool
	origin string // "" → default to baseURL; "-" → omit
	host   string // "" → leave default
}

func (h *harness) do(method, path string, body any, o reqOpts) *http.Response {
	h.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.baseURL+path, r)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	switch o.origin {
	case "":
		req.Header.Set("Origin", h.baseURL)
	case "-":
		// omit
	default:
		req.Header.Set("Origin", o.origin)
	}
	if o.host != "" {
		req.Host = o.host
	}
	if o.csrf {
		req.Header.Set(csrfHeader, h.csrf())
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// csrf returns the current XSRF-TOKEN cookie value from the jar.
func (h *harness) csrf() string {
	u, err := url.Parse(h.baseURL)
	if err != nil {
		h.t.Fatalf("parse base url: %v", err)
	}
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == xsrfCookie {
			return c.Value
		}
	}
	return ""
}

func (h *harness) login() {
	h.t.Helper()
	resp := h.do("POST", "/api/session", map[string]string{"password": testPassword}, reqOpts{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("login: want 200, got %d", resp.StatusCode)
	}
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// --- tests ---------------------------------------------------------------------

func TestLoginSuccessAndFailure(t *testing.T) {
	h := newHarness(t, true)

	resp := h.do("POST", "/api/session", map[string]string{"password": "wrong"}, reqOpts{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A fresh harness avoids the backoff the failure above registered.
	h2 := newHarness(t, true)
	resp = h2.do("POST", "/api/session", map[string]string{"password": testPassword}, reqOpts{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct password: want 200, got %d", resp.StatusCode)
	}
	var sawSession, sawXSRF bool
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.HttpOnly && c.SameSite == http.SameSiteStrictMode {
			sawSession = true
		}
		if c.Name == xsrfCookie && !c.HttpOnly {
			sawXSRF = true
		}
	}
	resp.Body.Close()
	if !sawSession || !sawXSRF {
		t.Fatalf("expected session (HttpOnly) + XSRF (readable) cookies; session=%v xsrf=%v", sawSession, sawXSRF)
	}
}

func TestLoginRateLimitBackoff(t *testing.T) {
	h := newHarness(t, true)

	r1 := h.do("POST", "/api/session", map[string]string{"password": "wrong"}, reqOpts{})
	if r1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first wrong: want 401, got %d", r1.StatusCode)
	}
	r1.Body.Close()

	r2 := h.do("POST", "/api/session", map[string]string{"password": "wrong"}, reqOpts{})
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second wrong within backoff: want 429, got %d", r2.StatusCode)
	}
	if r2.Header.Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 429")
	}
	r2.Body.Close()
}

func TestMutationRequiresCSRF(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	// Session cookie present + valid Origin, but no CSRF header → 403.
	resp := h.do("POST", "/api/rigs/rig-1:pause", nil, reqOpts{csrf: false})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation without CSRF: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMutationBadOrigin(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1:pause", nil, reqOpts{csrf: true, origin: "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad Origin: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRequestBadHost(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("GET", "/api/config", nil, reqOpts{host: "evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad Host: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGetConfigRedactsSecrets(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("GET", "/api/config", nil, reqOpts{})
	var cfg model.Config
	decode(t, resp, &cfg)
	if cfg.PasswordHash != model.RedactedSentinel {
		t.Errorf("password_hash not redacted: %q", cfg.PasswordHash)
	}
	if cfg.SessionSecret != model.RedactedSentinel {
		t.Errorf("session_secret not redacted: %q", cfg.SessionSecret)
	}
}

func TestPutConfigStaleVersion(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig()
	cfg.UpdatedAt = 999999 // never matches the stored 0
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale write: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPutConfigPreservesSentinelSecret(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig() // secrets arrive as the sentinel
	cfg.StripTargetSpaces = !cfg.StripTargetSpaces
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()

	if got := h.cfgPtr.Load().SessionSecret; got != h.secret {
		t.Errorf("session secret not preserved: got %q want %q", got, h.secret)
	}
	if h.cfgPtr.Load().PasswordHash == model.RedactedSentinel || h.cfgPtr.Load().PasswordHash == "" {
		t.Errorf("password hash not preserved: %q", h.cfgPtr.Load().PasswordHash)
	}
}

func TestPutConfigValidationError(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig()
	cfg.MaxConcurrentTransfers = 0 // hard validation error
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("validation error: want 400, got %d", resp.StatusCode)
	}
	var body struct {
		Problems []config.Problem `json:"problems"`
	}
	decode(t, resp, &body)
	if len(body.Problems) == 0 {
		t.Errorf("expected problems in 400 body")
	}
}

// A "\"-authored template (an operator on Windows) is canonicalized to "/" on
// save, so the stored config stays host-independent while both separators are
// accepted from the client.
func TestPutConfigNormalizesTemplateSeparators(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	cfg := h.getConfig()
	cfg.Templates.Light = `<TARGET>\<OTA>\NIGHT_<n>`
	resp := h.do("PUT", "/api/config", cfg, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put: want 200, got %d (%s)", resp.StatusCode, b)
	}
	var saved model.Config
	decode(t, resp, &saved)
	if saved.Templates.Light != "<TARGET>/<OTA>/NIGHT_<n>" {
		t.Errorf("returned light template = %q, want canonical forward slashes", saved.Templates.Light)
	}
	if got := h.cfgPtr.Load().Templates.Light; got != "<TARGET>/<OTA>/NIGHT_<n>" {
		t.Errorf("stored light template = %q, want canonical forward slashes", got)
	}
}

func TestRigCommand(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1:pause", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause known rig: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.sup.mu.Lock()
	calls := append([]string(nil), h.sup.calls...)
	h.sup.mu.Unlock()
	if len(calls) != 1 || calls[0] != "pause:rig-1" {
		t.Fatalf("supervisor calls = %v, want [pause:rig-1]", calls)
	}

	resp = h.do("POST", "/api/rigs/nope:pause", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pause unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Stop run is a rig action beside run-now, on the same authenticated, CSRF-checked
// command route.
func TestRigCommandStopRun(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("POST", "/api/rigs/rig-1:stop-run", nil, reqOpts{csrf: false})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stop-run without CSRF token: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("POST", "/api/rigs/rig-1:stop-run", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop-run known rig: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.sup.mu.Lock()
	calls := append([]string(nil), h.sup.calls...)
	h.sup.mu.Unlock()
	if len(calls) != 1 || calls[0] != "stop-run:rig-1" {
		t.Fatalf("supervisor calls = %v, want [stop-run:rig-1]", calls)
	}

	resp = h.do("POST", "/api/rigs/nope:stop-run", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop-run unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDiscardFrame(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	// Success → 204, and the supervisor recorded the discard call.
	resp := h.do("DELETE", "/api/rigs/rig-1/frames/42", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("discard success: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	h.sup.mu.Lock()
	calls := append([]string(nil), h.sup.calls...)
	h.sup.mu.Unlock()
	if len(calls) != 1 || calls[0] != "discard:rig-1:42" {
		t.Fatalf("supervisor calls = %v, want [discard:rig-1:42]", calls)
	}

	// A non-integer frame id → 400.
	resp = h.do("DELETE", "/api/rigs/rig-1/frames/nope", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-integer frame id: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A non-unclassified frame → 409.
	h.sup.discardErr = rig.ErrNotDismissable
	resp = h.do("DELETE", "/api/rigs/rig-1/frames/42", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("non-unclassified frame: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A frame not found on the rig → 404.
	h.sup.discardErr = rig.ErrFrameNotFound
	resp = h.do("DELETE", "/api/rigs/rig-1/frames/42", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown frame: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A purge/internal failure → 500.
	h.sup.discardErr = errors.New("nas unreachable")
	resp = h.do("DELETE", "/api/rigs/rig-1/frames/42", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("purge failure: want 500, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// An unknown rig → 404.
	h.sup.discardErr = nil
	resp = h.do("DELETE", "/api/rigs/nope/frames/42", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMissingFramesEmptyEnvelope pins the wire format for a drained rig with
// nothing missing: frames and groups must serialize to [] and NOT null. The UI
// reads groups.length / frames directly, and a null crashes the missing panel
// ("Cannot read properties of null (reading 'length')").
func TestMissingFramesEmptyEnvelope(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	resp := h.do("GET", "/api/rigs/rig-1/frames?missing=true", nil, reqOpts{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("missing frames: want 200, got %d", resp.StatusCode)
	}
	var env map[string]json.RawMessage
	decode(t, resp, &env)
	for _, key := range []string{"groups", "frames"} {
		raw, ok := env[key]
		if !ok {
			t.Fatalf("%q missing from the envelope", key)
		}
		if string(raw) == "null" {
			t.Errorf("%q serialized to null; must be [] so the UI's .length read is safe", key)
		}
	}
}

func TestRestoreFrame(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	// Success → 202, and the supervisor recorded the restore call.
	resp := h.do("POST", "/api/rigs/rig-1/frames/42/restore", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restore success: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	h.sup.mu.Lock()
	calls := append([]string(nil), h.sup.calls...)
	h.sup.mu.Unlock()
	if len(calls) != 1 || calls[0] != "restore:rig-1:42" {
		t.Fatalf("supervisor calls = %v, want [restore:rig-1:42]", calls)
	}

	// A non-integer frame id → 400.
	resp = h.do("POST", "/api/rigs/rig-1/frames/nope/restore", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-integer frame id: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A frame that is not marked missing → 409, NOT a server error.
	h.sup.restoreErr = ErrFrameNotMissing
	resp = h.do("POST", "/api/rigs/rig-1/frames/42/restore", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("frame not missing: want 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A genuine failure (e.g. a busy-worker timeout) → 500, not a misleading 409.
	h.sup.restoreErr = errors.New("worker busy")
	resp = h.do("POST", "/api/rigs/rig-1/frames/42/restore", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("restore failure: want 500, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// An unknown rig → 404.
	h.sup.restoreErr = nil
	resp = h.do("POST", "/api/rigs/nope/frames/42/restore", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown rig: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestFramesPagination(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	for i := 0; i < 3; i++ {
		f := &model.Frame{
			RigID: "rig-1", SourceShare: "EMMC Images",
			SourcePath: "Autorun/Light/x_" + string(rune('a'+i)) + ".fit",
			Size:       100, Mtime: 1, State: model.StateVerified,
		}
		if err := h.store.UpsertFrame(context.Background(), f); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	resp := h.do("GET", "/api/rigs/rig-1/frames?page=1", nil, reqOpts{})
	var body struct {
		Frames []frameDTO `json:"frames"`
		Page   int        `json:"page"`
		Total  int        `json:"total"`
	}
	decode(t, resp, &body)
	if body.Total != 3 || len(body.Frames) != 3 || body.Page != 1 {
		t.Fatalf("frames envelope = %+v, want total 3, len 3, page 1", body)
	}
}

func TestRecentFramesEndpoint(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	for i := 0; i < 5; i++ {
		f := &model.Frame{
			RigID: "rig-1", SourceShare: "EMMC Images",
			SourcePath: "Autorun/Light/x_" + string(rune('a'+i)) + ".fit",
			Size:       100, Mtime: 1, State: model.StateVerified,
		}
		if err := h.store.UpsertFrame(context.Background(), f); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	resp := h.do("GET", "/api/rigs/rig-1/frames?limit=2", nil, reqOpts{})
	var body struct {
		Frames   []frameDTO `json:"frames"`
		Page     int        `json:"page"`
		PageSize int        `json:"page_size"`
	}
	decode(t, resp, &body)
	if len(body.Frames) != 2 || body.PageSize != 2 {
		t.Fatalf("recent envelope = %+v, want len 2, page_size 2", body)
	}
	// Newest first: the last-inserted frame (x_e) leads.
	if body.Frames[0].SourcePath != "Autorun/Light/x_e.fit" {
		t.Errorf("expected newest-first, got %s first", body.Frames[0].SourcePath)
	}
}

func TestTransferHistory(t *testing.T) {
	h := newHarness(t, true)

	// Seed across two rigs: pulled frames at distinct times, plus one never pulled.
	seed := []*model.Frame{
		{RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/a.fit", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 1000, DestPath: "a"},
		{RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/b.fit", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 2000, DestPath: "b"},
		{RigID: "rig-2", SourceShare: "EMMC Images", SourcePath: "Autorun/z.fit", Size: 1, Mtime: 1, State: model.StateDeleted, PulledAt: 3000, DestPath: "z"},
		{RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/never.fit", Size: 1, Mtime: 1, State: model.StateFailed, PulledAt: 0, DestPath: ""},
	}
	for _, f := range seed {
		if err := h.store.UpsertFrame(context.Background(), f); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Unauthenticated → 401 (route exists; not a 404).
	resp := h.do("GET", "/api/frames", nil, reqOpts{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("history unauth: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.login()

	type page struct {
		Frames   []frameDTO `json:"frames"`
		Page     int        `json:"page"`
		PageSize int        `json:"page_size"`
		Total    int        `json:"total"`
	}

	// All rigs: three pulled frames (never-pulled excluded), newest-first.
	var all page
	decode(t, h.do("GET", "/api/frames", nil, reqOpts{}), &all)
	if all.Total != 3 || len(all.Frames) != 3 || all.Page != 1 || all.PageSize != 50 {
		t.Fatalf("history envelope = %+v, want total 3, len 3, page 1, size 50", all)
	}
	if all.Frames[0].SourcePath != "Autorun/z.fit" {
		t.Errorf("expected newest-first, got %s", all.Frames[0].SourcePath)
	}

	// Rig filter.
	var r1 page
	decode(t, h.do("GET", "/api/frames?rig=rig-1", nil, reqOpts{}), &r1)
	if r1.Total != 2 || len(r1.Frames) != 2 {
		t.Errorf("rig filter = %+v, want total 2, len 2", r1)
	}

	// Inclusive date window [2000,3000] → b and z.
	var win page
	decode(t, h.do("GET", "/api/frames?from=2000&to=3000", nil, reqOpts{}), &win)
	if win.Total != 2 || len(win.Frames) != 2 {
		t.Errorf("window = %+v, want total 2, len 2", win)
	}

	// Pagination: size 2 over 3 rows → page 1 has 2, page 2 has 1; total unchanged.
	var pg1, pg2 page
	decode(t, h.do("GET", "/api/frames?page=1&page_size=2", nil, reqOpts{}), &pg1)
	decode(t, h.do("GET", "/api/frames?page=2&page_size=2", nil, reqOpts{}), &pg2)
	if pg1.Total != 3 || len(pg1.Frames) != 2 || pg1.PageSize != 2 {
		t.Errorf("page 1 = %+v, want total 3, len 2, size 2", pg1)
	}
	if len(pg2.Frames) != 1 {
		t.Errorf("page 2 len = %d, want 1", len(pg2.Frames))
	}

	// The backlog alert's view: the rig's never-deletable frames, pulled or not;
	// the date window does not apply to it.
	var nd page
	decode(t, h.do("GET", "/api/frames?rig=rig-1&view=never_deletable&from=5000", nil, reqOpts{}), &nd)
	if nd.Total != 1 || len(nd.Frames) != 1 || nd.Frames[0].SourcePath != "Autorun/never.fit" {
		t.Errorf("never_deletable view = %+v, want just the failed frame", nd)
	}
	resp = h.do("GET", "/api/frames?view=bogus", nil, reqOpts{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown view: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAlertsAck(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	id, _, err := h.store.RaiseAlert(context.Background(), model.Alert{
		RigID: "rig-1", Severity: model.SeverityWarn, Category: model.AlertUnclassified,
		Message: "an unclassified frame", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("raise alert: %v", err)
	}

	resp := h.do("POST", "/api/alerts/"+strconv.FormatInt(id, 10)+":ack", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	alerts, err := h.store.ListAlerts(context.Background(), false, 0)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected no unacked alerts after ack, got %d", len(alerts))
	}
	if h.hub.Snapshot().UnackedAlerts != 0 {
		t.Errorf("hub unacked count not refreshed: %d", h.hub.Snapshot().UnackedAlerts)
	}
}

func TestHealthzUnauth(t *testing.T) {
	h := newHarness(t, true)
	resp, err := http.Get(h.baseURL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz unauth: want 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("healthz status = %q", body["status"])
	}
}

func TestPprofRequiresAuth(t *testing.T) {
	h := newHarness(t, true)

	// No session → 401.
	resp := h.do("POST", "/api/diagnostics/pprof?seconds=1", nil, reqOpts{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pprof unauth: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.login()
	resp = h.do("POST", "/api/diagnostics/pprof?seconds=1", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pprof authed: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("pprof content-type = %q", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) == 0 {
		t.Errorf("expected a non-empty profile")
	}
}

func TestHeapProfileRequiresAuth(t *testing.T) {
	h := newHarness(t, true)

	// No session → 401.
	resp := h.do("POST", "/api/diagnostics/heap", nil, reqOpts{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("heap unauth: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	h.login()
	resp = h.do("POST", "/api/diagnostics/heap", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heap authed: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("heap content-type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="heap.pprof"`) {
		t.Errorf("heap content-disposition = %q", cd)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// A debug=0 profile is a gzipped protobuf; the text form (debug=1) is not.
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("heap profile is not gzip (%d bytes): %v", len(b), err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress heap profile: %v", err)
	}
	// The proto's string table names the heap sample types; a CPU profile would
	// carry "samples"/"cpu" instead.
	if !bytes.Contains(raw, []byte("inuse_space")) {
		t.Errorf("heap profile (%d bytes) lacks the inuse_space sample type", len(raw))
	}
}

func TestAppLogsReturnsBufferedLines(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	slog.New(h.appLog.Handler()).Info("a distinctive test log line", "k", "v")

	resp := h.do("GET", "/api/app-logs?limit=50", nil, reqOpts{})
	var entries []LogEntry
	decode(t, resp, &entries)
	found := false
	for _, e := range entries {
		if e.Message == "a distinctive test log line" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the buffered log line in /api/app-logs, got %d entries", len(entries))
	}
}

func TestAppLogDownloadServesCurrentFile(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	logPath := filepath.Join(h.dataDir, "logs", "asiair-commander.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	const contents = "time=2026-07-23T00:00:00Z level=INFO msg=\"a distinctive line\"\n"
	if err := os.WriteFile(logPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	resp := h.do("GET", "/api/app-logs/download", nil, reqOpts{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: want 200, got %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, "asiair-commander.log") {
		t.Errorf("Content-Disposition = %q, want an attachment named asiair-commander.log", cd)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != contents {
		t.Errorf("downloaded body = %q, want %q", body, contents)
	}
}

func TestAppLogDownloadMissingFileIs404(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	// No log file has been written under h.dataDir/logs.
	resp := h.do("GET", "/api/app-logs/download", nil, reqOpts{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("download of a missing file: want 404, got %d", resp.StatusCode)
	}
}

func TestSetupRejectedWhenPasswordSet(t *testing.T) {
	h := newHarness(t, true) // password already set

	// Even from loopback (httptest is 127.0.0.1) the window is closed.
	resp := h.do("POST", "/api/setup", map[string]string{"password": "new"}, reqOpts{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("setup after password set: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Loopback grants nothing: the console is refused without the token and admitted
// with it, exactly like every other client.
func TestSetupTokenIsTheOnlyGate(t *testing.T) {
	h := newHarness(t, false) // no password → setup open, httptest dials 127.0.0.1

	resp := h.do("POST", "/api/setup", map[string]string{"password": "newpass"}, reqOpts{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("setup from loopback without token: want 403, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != codeSetupTokenRequired {
		t.Fatalf("want code %q, got %q", codeSetupTokenRequired, code)
	}

	// With the one-time token it succeeds.
	req, _ := http.NewRequest("POST", h.baseURL+"/api/setup", bytes.NewReader([]byte(`{"password":"newpass"}`)))
	req.Header.Set("Origin", h.baseURL)
	req.Header.Set(setupTokenHeader, h.srv.SetupToken())
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("setup with token: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup with token: want 200, got %d (%s)", resp.StatusCode, b)
	}
	resp.Body.Close()
	if h.cfgPtr.Load().PasswordHash == "" {
		t.Errorf("expected a password hash after setup")
	}
}

// A drive-by cross-origin POST is refused for want of the token, not for its
// Origin: checkOrigin has left this route, and the token subsumes it.
func TestSetupCrossOriginRejectedForMissingToken(t *testing.T) {
	h := newHarness(t, false) // no password → setup open; the client is loopback
	resp := h.do("POST", "/api/setup", map[string]string{"password": "newpass"}, reqOpts{origin: "http://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin setup: want 403, got %d", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != codeSetupTokenRequired {
		t.Fatalf("want code %q, got %q", codeSetupTokenRequired, code)
	}
	if h.cfgPtr.Load().PasswordHash != "" {
		t.Errorf("cross-origin setup must not set a password")
	}
}

// errorCode reads the machine-readable code out of an error body and closes it.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Code
}

// Logout rotates the session secret, so a cookie captured while logged in must
// stop verifying afterwards — a stolen cookie cannot outlive the logout.
func TestLogoutRevokesOutstandingCookie(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	u, _ := url.Parse(h.baseURL)
	var session *http.Cookie
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie after login")
	}

	resp := h.do("DELETE", "/api/session", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Replay the captured cookie on a jar-less client: it must no longer verify.
	req, _ := http.NewRequest("GET", h.baseURL+"/api/config", nil)
	req.Header.Set("Origin", h.baseURL)
	req.AddCookie(session)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed cookie after logout: want 401, got %d", resp.StatusCode)
	}
}

// failingVolumeFS resolves every volume query with an error, as GetVolumePathName
// does on Windows when a placeholder default root points at an absent drive.
type failingVolumeFS struct{ localfs.LocalFS }

func (failingVolumeFS) SameVolume(paths ...string) (bool, error) {
	return false, errors.New(`GetVolumePathName D:\: The system cannot find the file specified.`)
}

// First-run setup seeds default roots the operator has not chosen yet, so a
// volume resolver that cannot resolve those placeholders must not block setup —
// the operator's real roots are validated by the later config PUT.
func TestSetupSeedsDefaultsDespiteFailingVolumeResolver(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ptr := &atomic.Pointer[model.Config]{}
	cfg := config.Defaults()
	cfg.Web.BcryptCost = bcrypt.MinCost
	ptr.Store(cfg)
	appLog := NewLogBuffer(50)
	logger := slog.New(appLog.Handler())

	srv := New(Deps{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		Config:     ptr,
		Supervisor: &fakeSupervisor{known: map[string]bool{}},
		Store:      st,
		Hub:        hub.New(logger),
		Scanner:    &fakeScanner{},
		LocalFS:    failingVolumeFS{localfs.NewFake(t.TempDir())},
		AppLog:     appLog,
		Logger:     logger,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST", ts.URL+"/api/setup", bytes.NewReader([]byte(`{"password":"newpass123"}`)))
	req.Header.Set("Origin", ts.URL)
	req.Header.Set(setupTokenHeader, srv.SetupToken())
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup with failing volume resolver: want 200, got %d (%s)", resp.StatusCode, b)
	}
	if ptr.Load().PasswordHash == "" {
		t.Error("expected a password hash after setup")
	}
}

func TestSSEAnnouncesServerInfo(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.baseURL+"/events", nil)
	req.Header.Set("Origin", h.baseURL)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE: want 200, got %d", resp.StatusCode)
	}

	// server_info is announced up front, before any snapshot. Scan for its event
	// line, then decode the data line that follows.
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	var got serverInfo
	var found bool
	for scanner.Scan() {
		if scanner.Text() == "event: server_info" {
			if !scanner.Scan() {
				t.Fatal("server_info event had no data line")
			}
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				t.Fatalf("server_info: unexpected data line %q", scanner.Text())
			}
			if err := json.Unmarshal([]byte(data), &got); err != nil {
				t.Fatalf("server_info: bad json %q: %v", data, err)
			}
			found = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if !found {
		t.Fatal("expected a server_info event")
	}
	if got.Version != testVersion {
		t.Errorf("server_info version = %q, want %q", got.Version, testVersion)
	}
	if got.StartedAt != testStartedAt.Unix() {
		t.Errorf("server_info started_at = %d, want %d", got.StartedAt, testStartedAt.Unix())
	}
}

func TestSSEEmitsSnapshotThenEvent(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.baseURL+"/events", nil)
	req.Header.Set("Origin", h.baseURL)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("SSE content-type = %q", ct)
	}

	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.hub.Emit(hub.Event{Name: "frame.done", Data: map[string]string{"rig_id": "rig-1"}})
			}
		}
	}()
	defer close(stop)

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	var gotSnap, gotEvent bool
	for scanner.Scan() {
		switch scanner.Text() {
		case "event: snapshot":
			gotSnap = true
		case "event: frame.done":
			gotEvent = true
		}
		if gotSnap && gotEvent {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if !gotSnap {
		t.Errorf("expected an initial snapshot event")
	}
	if !gotEvent {
		t.Errorf("expected a subsequent frame.done event")
	}
}

// getConfig fetches and decodes the current redacted config as the SPA would.
func (h *harness) getConfig() model.Config {
	h.t.Helper()
	resp := h.do("GET", "/api/config", nil, reqOpts{})
	var cfg model.Config
	decode(h.t, resp, &cfg)
	return cfg
}

func TestAppLogStreamSnapshotThenLive(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	lg := slog.New(h.appLog.Handler())
	lg.Info("seed line before connect") // ensures a non-empty snapshot

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.baseURL+"/api/app-logs/stream", nil)
	req.Header.Set("Origin", h.baseURL)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("open log stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("log stream: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("log stream content-type = %q", ct)
	}

	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				lg.Warn("live line")
			}
		}
	}()
	defer close(stop)

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	var gotSnap, gotLog bool
	for scanner.Scan() {
		switch scanner.Text() {
		case "event: snapshot":
			gotSnap = true
		case "event: log":
			gotLog = true
		}
		if gotSnap && gotLog {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if !gotSnap {
		t.Errorf("expected an initial snapshot event")
	}
	if !gotLog {
		t.Errorf("expected a live log event")
	}
}

// TestRootsCheckWritable probes real temp-dir roots: every one is writable, so the
// endpoint returns ok for each and names the account the service runs as.
func TestRootsCheckWritable(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	base := t.TempDir()
	body := map[string]any{"roots": map[string]string{
		"light":   filepath.Join(base, "lights"),
		"cal":     filepath.Join(base, "cal"),
		"staging": filepath.Join(base, "staging"),
		// logs left empty → skipped.
	}}
	resp := h.do("POST", "/api/roots/check", body, reqOpts{csrf: true})
	var out struct {
		Account string `json:"account"`
		Results []struct {
			Path  string `json:"path"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	decode(t, resp, &out)
	if out.Account == "" {
		t.Error("expected a non-empty service account name")
	}
	if len(out.Results) != 3 {
		t.Fatalf("results = %d, want 3 (empty logs skipped)", len(out.Results))
	}
	for _, r := range out.Results {
		if !r.OK || r.Error != "" {
			t.Errorf("root %s: ok=%v error=%q, want writable", r.Path, r.OK, r.Error)
		}
	}
}

// TestRootsCheckUnwritable drives the failure branch through the Fake's injected
// probe error: the endpoint still answers 200 but marks the root not-ok so the
// wizard can block setup and guide the operator.
func TestRootsCheckUnwritable(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.lfs.ProbeErr = func(string) error { return errors.New("access is denied") }

	body := map[string]any{"roots": map[string]string{
		"light":   filepath.Join(t.TempDir(), "lights"),
		"staging": filepath.Join(t.TempDir(), "staging"),
	}}
	resp := h.do("POST", "/api/roots/check", body, reqOpts{csrf: true})
	var out struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	decode(t, resp, &out)
	if len(out.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(out.Results))
	}
	for _, r := range out.Results {
		if r.OK || r.Error == "" {
			t.Errorf("result ok=%v error=%q, want not-ok with a message", r.OK, r.Error)
		}
	}
}

// TestRootsCheckRejectsRelativePath: a relative root would resolve against the
// service's working directory, so the probe rejects it up front.
func TestRootsCheckRejectsRelativePath(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	body := map[string]any{"roots": map[string]string{"light": "Astro/Lights"}}
	resp := h.do("POST", "/api/roots/check", body, reqOpts{csrf: true})
	var out struct {
		Results []struct {
			Path  string `json:"path"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	decode(t, resp, &out)
	if len(out.Results) != 1 || out.Results[0].OK || out.Results[0].Error == "" {
		t.Fatalf("relative path must be rejected not-ok with a message, got %+v", out.Results)
	}
}

// TestRootsCheckRequiresAuth confirms the endpoint is guarded like other
// mutations: no session → 401.
func TestRootsCheckRequiresAuth(t *testing.T) {
	h := newHarness(t, true)
	resp := h.do("POST", "/api/roots/check", map[string]any{"roots": map[string]string{}}, reqOpts{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("roots check unauth: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- discovery / adoption (scan_air) ----------------------------------------

// TestScanReturnsDiscoveredShape confirms POST /api/scan surfaces the guid-keyed
// DiscoveredRig shape from the scanner.
func TestScanReturnsDiscoveredShape(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.scanner.scanned = []model.DiscoveredRig{{
		GUID: "0a1b2c3d", CPUID: "0011223344556677", Model: "AirPlus", Address: "192.168.1.75",
		Online: true, Managed: false, FirstSeen: 111,
	}}
	resp := h.do("POST", "/api/scan", nil, reqOpts{csrf: true})
	var got []model.DiscoveredRig
	decode(t, resp, &got)
	if len(got) != 1 || got[0].GUID != "0a1b2c3d" || !got[0].Online || got[0].Address != "192.168.1.75" {
		t.Fatalf("scan = %+v, want the guid-keyed device", got)
	}
}

// TestDiscoveredReturnsRegistry confirms GET /api/discovered returns the registry
// snapshot without re-scanning.
func TestDiscoveredReturnsRegistry(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.scanner.discovered = []model.DiscoveredRig{{GUID: "g1", Managed: true, RigID: "rig-1"}}
	resp := h.do("GET", "/api/discovered", nil, reqOpts{})
	var got []model.DiscoveredRig
	decode(t, resp, &got)
	if len(got) != 1 || got[0].GUID != "g1" || !got[0].Managed || got[0].RigID != "rig-1" {
		t.Fatalf("discovered = %+v", got)
	}
	if h.scanner.scanCalls != 0 || h.scanner.onDemandCalls != 0 {
		t.Fatalf("GET /api/discovered must not scan; scanCalls=%d onDemandCalls=%d",
			h.scanner.scanCalls, h.scanner.onDemandCalls)
	}
}

// TestScanTakesTheOnDemandPath pins /api/scan to the round that logs what
// answered. Every scan through this endpoint was asked for by an operator — the
// Scan button or opening the devices page — and the silent background round is
// the timer's, so a regression here costs the log lines that explain an
// unexpected device.
func TestScanTakesTheOnDemandPath(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.do("POST", "/api/scan", nil, reqOpts{csrf: true})
	if h.scanner.onDemandCalls != 1 || h.scanner.scanCalls != 0 {
		t.Fatalf("POST /api/scan: onDemandCalls=%d scanCalls=%d, want 1 and 0",
			h.scanner.onDemandCalls, h.scanner.scanCalls)
	}
}

// TestDeviceSharesReturnsShares exercises the adopt-time share list. Whether a
// device is already managed comes from the discovered list, not this endpoint.
func TestDeviceSharesReturnsShares(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	h.scanner.shares = []string{"EMMC Images", "Udisk Images"}

	resp := h.do("POST", "/api/discovered/g1/shares", nil, reqOpts{csrf: true})
	var out struct {
		Shares    []string `json:"shares"`
		ManagedBy string   `json:"managed_by"`
	}
	decode(t, resp, &out)
	if len(out.Shares) != 2 {
		t.Fatalf("device-shares = %+v, want 2 shares", out)
	}
	if out.ManagedBy != "" {
		t.Fatalf("device-shares must not report managed_by, got %q", out.ManagedBy)
	}
}

// TestCreateRigAcceptsGUIDAndCPUID confirms adoption via POST /api/rigs carries
// the picked device's guid+cpuId through the create pipeline.
func TestCreateRigAcceptsGUIDAndCPUID(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	rig := model.RigProfile{
		ID: "adopted", Name: "Adopted", GUID: "dev-guid", CPUID: "dev-cpu",
		User: "guest", Password: "12345678", OTA: "RC8", Session: "S", Managed: true,
		Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
	}
	resp := h.do("POST", "/api/rigs", rig, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create rig: want 201, got %d: %s", resp.StatusCode, b)
	}
	var got model.RigProfile
	decode(t, resp, &got)
	if got.GUID != "dev-guid" || got.CPUID != "dev-cpu" {
		t.Fatalf("created rig = %+v, want guid/cpuId preserved", got)
	}
}

// TestFrameEventDataMatchesREST pins the "one DTO on the wire" invariant: a
// frame-carrying SSE event (frame.done/frame.failed/frame.deleted) serializes to
// exactly the frameDTO shape the REST endpoints emit, so the live feed decodes
// every frame source identically. A non-frame payload (frame.started's map, or a
// stray value) rides through untouched — the checked assertion never panics.
func TestFrameEventDataMatchesREST(t *testing.T) {
	f := model.Frame{
		ID: 7, RigID: "rig-1", SourceShare: "EMMC Images", SourcePath: "Autorun/Light/o/L_0001.fit",
		SHA256: "deadbeef", Size: 123, Mtime: 1000, FrameType: "Light", Target: "NGC 7000",
		DestPath: "/nas/light/NGC7000/L_0001.fit", State: model.StateVerified, PulledAt: 2000,
	}

	got := frameEventData(f)
	if _, ok := got.(frameDTO); !ok {
		t.Fatalf("frameEventData(model.Frame) returned %T, want frameDTO", got)
	}
	viaEvent, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal event DTO: %v", err)
	}
	viaREST, err := json.Marshal(toFrameDTO(f))
	if err != nil {
		t.Fatalf("marshal REST DTO: %v", err)
	}
	if string(viaEvent) != string(viaREST) {
		t.Fatalf("event DTO != REST DTO:\n event: %s\n  rest: %s", viaEvent, viaREST)
	}

	// Non-frame payloads pass through unchanged and never panic.
	if got := frameEventData(42); got != 42 {
		t.Fatalf("frameEventData(42) = %v, want 42 passed through", got)
	}
	m := map[string]any{"rig_id": "rig-1", "source": "x"}
	if got := frameEventData(m); got == nil {
		t.Fatalf("frameEventData(map) dropped the payload")
	}
}

// TestFrameDTOAstrometry pins the astrometry wire shape: a frame with a solution
// emits a nested "astrometry" object carrying the snake_case solution fields, and a
// frame without astrometry omits the key entirely.
func TestFrameDTOAstrometry(t *testing.T) {
	withSol := model.Frame{
		ID: 1, RigID: "r", State: model.StateVerified,
		Astrometry: &model.Astrometry{
			Status: model.AstroStatusHeader,
			Solution: &model.WCSSolution{
				CenterRA: 180.5, CenterDec: -12.25, PositionAngle: 91.3,
				PixelScale: 1.23, FOVWidth: 1.5, FOVHeight: 1.0, Flipped: true,
			},
		},
	}
	b, err := json.Marshal(toFrameDTO(withSol))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"astrometry"`, `"status":"header"`, `"center_ra":180.5`, `"pos_angle":91.3`, `"flipped":true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("DTO JSON missing %s:\n%s", want, b)
		}
	}

	without := model.Frame{ID: 2, RigID: "r", State: model.StateVerified}
	b2, err := json.Marshal(toFrameDTO(without))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b2), "astrometry") {
		t.Errorf("expected astrometry omitted for a frame without it:\n%s", b2)
	}
}

// --- first-run setup: the uniform token rule -------------------------------------

// rawServer is a Server driven directly through its mux, so a test can present a
// synthetic RemoteAddr. httptest always dials 127.0.0.1, and the whole point of
// the uniform rule is that no peer address grants anything — which a test that
// can only ever be loopback cannot demonstrate.
type rawServer struct {
	srv     *Server
	cfgPtr  *atomic.Pointer[model.Config]
	appLog  *LogBuffer
	dataDir string
	cfgPath string
}

// newRawServer builds a Server over cfg. A nil cfg gets config.Defaults() with
// no password AND no session secret — what a genuine first run ships.
func newRawServer(t *testing.T, cfg *model.Config) *rawServer {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if cfg == nil {
		cfg = config.Defaults()
		cfg.PasswordHash = ""
		cfg.SessionSecret = ""
	}
	cfg.Web.BcryptCost = bcrypt.MinCost

	appLog := NewLogBuffer(200)
	logger := slog.New(appLog.Handler())
	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	dataDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	srv := New(Deps{
		ConfigPath: cfgPath,
		DataDir:    dataDir,
		Config:     ptr,
		Supervisor: &fakeSupervisor{known: map[string]bool{}},
		Store:      st,
		Hub:        hub.New(logger),
		Scanner:    &fakeScanner{},
		AppLog:     appLog,
		Logger:     logger,
	})
	return &rawServer{srv: srv, cfgPtr: ptr, appLog: appLog, dataDir: dataDir, cfgPath: cfgPath}
}

// rawReq describes one synthetic request.
type rawReq struct {
	method  string
	path    string
	body    string
	peer    string // RemoteAddr; "" → 127.0.0.1:40000
	host    string // Host header; "" → localhost:8686
	origin  string // Origin header; "" → omitted
	token   string // X-Setup-Token; "" → omitted
	cookies []*http.Cookie
	csrf    string
}

func (rs *rawServer) do(req rawReq) *httptest.ResponseRecorder {
	var body io.Reader
	if req.body != "" {
		body = strings.NewReader(req.body)
	}
	r := httptest.NewRequest(req.method, req.path, body)
	r.RemoteAddr = "127.0.0.1:40000"
	if req.peer != "" {
		r.RemoteAddr = req.peer
	}
	r.Host = "localhost:8686"
	if req.host != "" {
		r.Host = req.host
	}
	if req.origin != "" {
		r.Header.Set("Origin", req.origin)
	}
	if req.token != "" {
		r.Header.Set(setupTokenHeader, req.token)
	}
	for _, c := range req.cookies {
		r.AddCookie(c)
	}
	if req.csrf != "" {
		r.Header.Set(csrfHeader, req.csrf)
	}
	w := httptest.NewRecorder()
	rs.srv.mux.ServeHTTP(w, r)
	return w
}

func rawCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return body.Code
}

// design architecture §8.2 invariant 1: from a config with no password and no
// session secret, no client can complete setup without the token — from any
// peer, Host, or Origin. The proxy shape is called out explicitly: a
// default-configured nginx rewrites Host to localhost, so a request from the
// internet can present a loopback peer, a loopback Host and a loopback Origin at
// once. That break is what killed the console exemption; it must never pass
// again.
func TestSetupWithoutTokenAlwaysRejected(t *testing.T) {
	cases := []struct {
		name string
		req  rawReq
	}{
		{"loopback console", rawReq{peer: "127.0.0.1:51000", host: "localhost:8686", origin: "http://localhost:8686"}},
		{"loopback ipv6 console", rawReq{peer: "[::1]:51000", host: "[::1]:8686", origin: "http://[::1]:8686"}},
		{"nginx proxy shape", rawReq{peer: "127.0.0.1:51000", host: "localhost:8686", origin: "http://localhost:8686"}},
		{"lan client by ip", rawReq{peer: "192.168.1.220:51000", host: "192.168.1.10:8686", origin: "http://192.168.1.10:8686"}},
		{"lan client by name", rawReq{peer: "192.168.1.220:51000", host: "nas.local", origin: "http://nas.local"}},
		{"container gateway", rawReq{peer: "172.16.99.1:51000", host: "nas.local", origin: "http://nas.local"}},
		{"public peer", rawReq{peer: "203.0.113.7:51000", host: "commander.example.com", origin: "https://commander.example.com"}},
		{"no origin at all", rawReq{peer: "127.0.0.1:51000", host: "localhost:8686"}},
		{"wrong token", rawReq{peer: "127.0.0.1:51000", host: "localhost:8686", token: "not-the-token"}},
		{"empty token header", rawReq{peer: "127.0.0.1:51000", host: "localhost:8686", token: " "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRawServer(t, nil)
			req := tc.req
			req.method, req.path, req.body = "POST", "/api/setup", `{"password":"attacker"}`
			w := rs.do(req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d (%s)", w.Code, w.Body.String())
			}
			if code := rawCode(t, w); code != codeSetupTokenRequired {
				t.Fatalf("want code %q, got %q", codeSetupTokenRequired, code)
			}
			if got := rs.cfgPtr.Load().PasswordHash; got != "" {
				t.Fatalf("a password was set without a token: %q", got)
			}
			if rs.srv.SetupToken() == "" {
				t.Fatal("a refused attempt must not consume the token")
			}
		})
	}
}

// design architecture §8.2 invariant 2: once a password exists the window is
// closed to everyone, the token holder included, and the refusal is
// machine-readable so the SPA can route a retry to the login page instead of
// stranding it on a dead wizard.
func TestSetupClosedAfterPasswordEvenWithToken(t *testing.T) {
	rs := newRawServer(t, nil)
	tok := rs.srv.SetupToken()
	if tok == "" {
		t.Fatal("expected a minted token on a fresh install")
	}

	cfg := config.Defaults()
	cfg.PasswordHash = "$2a$04$alreadyset"
	rs.cfgPtr.Store(cfg)

	w := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"second"}`, token: tok})
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d (%s)", w.Code, w.Body.String())
	}
	if code := rawCode(t, w); code != codeSetupComplete {
		t.Fatalf("want code %q, got %q", codeSetupComplete, code)
	}
}

// design architecture §8.2 invariant 3: the Host bypass is scoped to the open
// window. Once a password is set the allowlist applies on the setup routes
// exactly as it did before.
func TestCheckHostStrictAfterSetup(t *testing.T) {
	cfg := config.Defaults()
	cfg.PasswordHash = "$2a$04$alreadyset"
	cfg.Web.AllowedHosts = []string{"localhost", "nas.local"}
	rs := newRawServer(t, cfg)

	for _, path := range []string{"/api/setup"} {
		w := rs.do(rawReq{method: "GET", path: path, host: "evil.example"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET %s from a foreign Host: want 403, got %d", path, w.Code)
		}
		w = rs.do(rawReq{method: "POST", path: path, body: `{"password":"x"}`, host: "evil.example"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("POST %s from a foreign Host: want 403, got %d", path, w.Code)
		}
		// An allowlisted Host gets past checkHost and dies on the closed window.
		w = rs.do(rawReq{method: "POST", path: path, body: `{"password":"x"}`, host: "nas.local"})
		if code := rawCode(t, w); code != codeSetupComplete {
			t.Fatalf("POST %s from an allowed Host: want code %q, got %q", path, codeSetupComplete, code)
		}
	}
}

// design architecture §8.2 invariant 4: two concurrent POSTs with the valid
// token produce exactly one winner; the loser is refused and the winner's
// password is what is stored. This needs genuine concurrency under -race, which
// is also what surfaces the token race if the atomic is ever dropped.
func TestConcurrentSetupSingleWinner(t *testing.T) {
	rs := newRawServer(t, nil)
	tok := rs.srv.SetupToken()

	const n = 8
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = rs.do(rawReq{
				method: "POST", path: "/api/setup",
				body:   `{"password":"racer-` + strconv.Itoa(i) + `"}`,
				origin: "http://localhost:8686", token: tok,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	winner := -1
	for i, w := range results {
		switch w.Code {
		case http.StatusOK:
			winners++
			winner = i
		case http.StatusForbidden:
			if code := rawCode(t, w); code != codeSetupComplete {
				t.Errorf("loser %d: want code %q, got %q", i, codeSetupComplete, code)
			}
		default:
			t.Errorf("attempt %d: unexpected status %d (%s)", i, w.Code, w.Body.String())
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly one winner, got %d", winners)
	}
	stored := rs.cfgPtr.Load()
	if err := bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash),
		[]byte("racer-"+strconv.Itoa(winner))); err != nil {
		t.Fatalf("stored password is not the winner's: %v", err)
	}
	if rs.srv.SetupToken() != "" {
		t.Error("the token must be cleared once a winner has saved")
	}
}

// design architecture §8.2 invariant 5: setup allowlists the browser's Origin
// host — not its Host — so the follow-on config PUT from that same browser
// passes checkOrigin. The two differ here exactly as they do behind a
// Host-rewriting proxy.
func TestRemoteSetupThenConfigPutSucceeds(t *testing.T) {
	rs := newRawServer(t, nil)
	tok := rs.srv.SetupToken()

	w := rs.do(rawReq{
		method: "POST", path: "/api/setup", body: `{"password":"operator-pass"}`,
		peer: "172.16.99.1:51000", host: "localhost:8686",
		origin: "https://commander.example.com", token: tok,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("setup: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var setupResp struct {
		SeededHost string `json:"seeded_host"`
		SeededIsIP bool   `json:"seeded_host_is_ip"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setupResp); err != nil {
		t.Fatalf("decode setup response: %v", err)
	}
	if setupResp.SeededHost != "commander.example.com" {
		t.Fatalf("want the Origin host seeded, got %q", setupResp.SeededHost)
	}
	if setupResp.SeededIsIP {
		t.Error("a hostname must not be reported as an IP literal")
	}

	stored := rs.cfgPtr.Load()
	if !rs.srv.allowedHosts(stored)["commander.example.com"] {
		t.Fatalf("Origin host not in the stored allowlist: %v", stored.Web.AllowedHosts)
	}

	// The browser's next mutation carries the same Origin and must pass.
	var session, xsrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case sessionCookie:
			session = c
		case xsrfCookie:
			xsrf = c
		}
	}
	if session == nil || xsrf == nil {
		t.Fatal("setup must issue session and XSRF cookies")
	}
	body, err := json.Marshal(config.Redact(stored))
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	put := rs.do(rawReq{
		method: "PUT", path: "/api/config", body: string(body),
		host: "localhost:8686", origin: "https://commander.example.com",
		cookies: []*http.Cookie{session, xsrf}, csrf: xsrf.Value,
	})
	if put.Code != http.StatusOK {
		t.Fatalf("follow-on config PUT: want 200, got %d (%s)", put.Code, put.Body.String())
	}
}

// design architecture §8.2 invariant 6: completion clears the token and removes
// its file, and a replay of the same token is refused.
func TestSetupTokenClearedAfterCompletion(t *testing.T) {
	rs := newRawServer(t, nil)
	tok := rs.srv.SetupToken()
	tokenFile := filepath.Join(rs.dataDir, SetupTokenFileName)
	if err := rs.srv.WriteSetupTokenFile("http://localhost:8686/"); err != nil {
		t.Fatalf("WriteSetupTokenFile: %v", err)
	}
	if _, err := os.Stat(tokenFile); err != nil {
		t.Fatalf("expected the token file to exist before setup: %v", err)
	}

	w := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"operator-pass"}`, token: tok})
	if w.Code != http.StatusOK {
		t.Fatalf("setup: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if rs.srv.SetupToken() != "" {
		t.Error("the in-memory token must be cleared on completion")
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Errorf("the token file must be removed on completion, stat err = %v", err)
	}

	replay := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"replayed"}`, token: tok})
	if replay.Code != http.StatusForbidden {
		t.Fatalf("replay: want 403, got %d", replay.Code)
	}
}

// design architecture §8.2 invariant 12: a setup attempt that fails to save
// leaves the token valid, so the operator can correct and retry without
// restarting the service. A success-path test cannot catch a token cleared too
// early.
func TestSetupSaveFailureKeepsTokenValid(t *testing.T) {
	rs := newRawServer(t, nil)
	tok := rs.srv.SetupToken()
	// A config path inside a directory that does not exist: Save's temp-file
	// create fails, standing in for a full disk or an ACL the service cannot use.
	rs.srv.deps.ConfigPath = filepath.Join(rs.cfgPath, "nope", "config.yaml")

	w := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"operator-pass"}`, token: tok})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 from a failing save, got %d (%s)", w.Code, w.Body.String())
	}
	if rs.srv.SetupToken() != tok {
		t.Fatal("a failed save must not destroy the token: the operator would be stranded")
	}
	if rs.cfgPtr.Load().PasswordHash != "" {
		t.Fatal("a failed save must not leave a password set")
	}

	// The retry, against a working path, still works with the same token.
	rs.srv.deps.ConfigPath = rs.cfgPath
	retry := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"operator-pass"}`, token: tok})
	if retry.Code != http.StatusOK {
		t.Fatalf("retry after a failed save: want 200, got %d (%s)", retry.Code, retry.Body.String())
	}
}

// design architecture §8.2 invariant 10: the Host bypass reaches the two setup
// routes only. Every other route enforces the allowlist unchanged, even while
// no password is set.
func TestHostBypassLimitedToSetupRoutes(t *testing.T) {
	rs := newRawServer(t, nil) // no password → window open

	// The setup routes answer from a Host that is not allowlisted...
	if w := rs.do(rawReq{method: "GET", path: "/api/setup", host: "nas.local"}); w.Code != http.StatusOK {
		t.Fatalf("GET /api/setup from an unlisted Host: want 200, got %d", w.Code)
	}
	w := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"x"}`, host: "nas.local"})
	if code := rawCode(t, w); code != codeSetupTokenRequired {
		t.Fatalf("POST /api/setup from an unlisted Host: want the token refusal, got %q", code)
	}

	// ...and nothing else does.
	others := []rawReq{
		{method: "GET", path: "/api/config"},
		{method: "PUT", path: "/api/config", body: "{}"},
		{method: "POST", path: "/api/session", body: `{"password":"x"}`},
		{method: "POST", path: "/api/roots/check", body: "{}"},
		{method: "GET", path: "/api/rigs"},
		{method: "GET", path: "/events"},
		{method: "GET", path: "/api/app-logs"},
	}
	for _, req := range others {
		req.host = "nas.local"
		req.origin = "http://nas.local"
		w := rs.do(req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s from an unlisted Host: want 403, got %d", req.method, req.path, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "not allowed") {
			t.Errorf("%s %s: want the Host refusal, got %s", req.method, req.path, w.Body.String())
		}
	}
}

// design architecture §8.2 invariant 11: a rejected token value is never
// written to any log sink. A near-miss recorded in the log an operator might
// paste into a bug report is a credential leak, and the value is
// attacker-supplied besides.
func TestRejectedTokenValueNeverLogged(t *testing.T) {
	rs := newRawServer(t, nil)
	const attempted = "zzz-distinctive-attempted-token-value-zzz"

	w := rs.do(rawReq{method: "POST", path: "/api/setup", body: `{"password":"x"}`, token: attempted})
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), attempted) {
		t.Error("the response echoed the attempted token")
	}
	for _, e := range rs.appLog.Entries(slog.LevelDebug, 200) {
		blob, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal log entry: %v", err)
		}
		if strings.Contains(string(blob), attempted) {
			t.Fatalf("the attempted token reached the log: %s", blob)
		}
	}
}

// The refusal WARN is collapsed to one per peer per minute, so an attacker
// cannot evict the startup line the operator is reading the token from.
func TestSetupRejectionLoggingIsRateLimited(t *testing.T) {
	var l setupRejectionLog
	base := time.Unix(1700000000, 0)

	if !l.allow("10.0.0.1", base) {
		t.Fatal("the first refusal from a peer must be logged")
	}
	if l.allow("10.0.0.1", base.Add(30*time.Second)) {
		t.Error("a repeat within the interval must be suppressed")
	}
	if !l.allow("10.0.0.1", base.Add(2*time.Minute)) {
		t.Error("a repeat after the interval must be logged again")
	}

	// The table is keyed by unauthenticated input, so it must not grow with the
	// number of distinct sources an attacker presents.
	for i := 0; i < 500; i++ {
		l.allow("10.1."+strconv.Itoa(i/256)+"."+strconv.Itoa(i%256), base)
	}
	l.mu.Lock()
	size := len(l.peers)
	l.mu.Unlock()
	if size > setupRejectionPeers {
		t.Fatalf("peer table grew to %d, want at most %d", size, setupRejectionPeers)
	}
}

// design architecture §8.2 invariant 14: the unauthenticated setup-status
// endpoint discloses neither the token nor any absolute filesystem path, and
// says nothing at all once the wizard is no longer needed.
func TestSetupStatusDisclosesNothingSensitive(t *testing.T) {
	rs := newRawServer(t, nil)
	tok := rs.srv.SetupToken()
	if err := rs.srv.WriteSetupTokenFile("http://localhost:8686/"); err != nil {
		t.Fatalf("WriteSetupTokenFile: %v", err)
	}

	w := rs.do(rawReq{method: "GET", path: "/api/setup"})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, tok) {
		t.Fatal("the setup status leaked the token")
	}
	for _, secret := range []string{rs.dataDir, rs.cfgPath, SetupTokenFileName} {
		if strings.Contains(body, secret) {
			t.Fatalf("the setup status leaked %q: %s", secret, body)
		}
	}
	var status struct {
		Needed   bool   `json:"needed"`
		Platform string `json:"platform"`
		OS       string `json:"os"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !status.Needed {
		t.Fatal("want needed=true on a fresh install")
	}
	switch status.Platform {
	case PlatformContainer, PlatformKubernetes, PlatformOther:
	default:
		t.Fatalf("unexpected platform category %q", status.Platform)
	}
	if status.OS != runtime.GOOS {
		t.Fatalf("os %q, want %q", status.OS, runtime.GOOS)
	}

	// The category is reported only while the wizard is needed.
	done := config.Defaults()
	done.PasswordHash = "$2a$04$alreadyset"
	rs.cfgPtr.Store(done)
	w = rs.do(rawReq{method: "GET", path: "/api/setup"})
	if body := w.Body.String(); strings.Contains(body, "platform") || strings.Contains(body, `"os"`) {
		t.Fatalf("platform and os must be omitted once setup is complete: %s", body)
	}
}

// design architecture §8.2 invariant 9: no route may ever carry an
// Access-Control-Allow-* header. The token's sufficiency rests on it being
// unreadable cross-origin, which is only true while no response invites a
// foreign document to read it — so this is asserted rather than left as a fact
// a well-meaning CORS middleware could remove.
func TestNoCORSHeadersOnAnyRoute(t *testing.T) {
	rs := newRawServer(t, nil)
	placeholder := strings.NewReplacer(
		"{id}", "rig-1", "{idAction}", "rig-1:pause", "{frameId}", "1",
		"{guid}", "guid-1", "{name}", "v17",
	)
	for _, route := range registeredRoutes {
		method, pattern, ok := strings.Cut(route, " ")
		if !ok {
			t.Fatalf("malformed route entry %q", route)
		}
		path := placeholder.Replace(pattern)
		w := rs.do(rawReq{method: method, path: path, body: "{}", origin: "https://evil.example"})
		for name := range w.Header() {
			if strings.HasPrefix(strings.ToLower(name), "access-control-allow-") {
				t.Errorf("%s %s emitted %s", method, path, name)
			}
		}
	}
}

// design architecture §8.2 invariant 15: the token file is never observable at a
// permissive ACL. It is written through config.WriteFileRestricted, which
// restricts a temp file before any token byte reaches it and then renames
// atomically — so the target appears already-restricted rather than being
// tightened afterwards.
//
// READ THIS BEFORE TRUSTING THIS TEST: the ordering property itself — restricted
// BEFORE the first token byte is written — is not proven here and cannot be
// proven race-free from outside the writer, because there is no observer that
// can inspect a temp file between its creation and its first write. What is
// asserted is the observable part: the file lands at 0600 even under a
// world-writable parent, it appears atomically with no second, laxer copy beside
// it, and it carries the exact two-line contract. The ordering rests entirely on
// this code path continuing to reuse config.WriteFileRestricted. A change that
// switched to create → write → restrict would still pass this test while
// reintroducing the exposure window, so review that call site rather than
// treating a green run here as the guarantee.
func TestTokenFileRestrictedBeforeWrite(t *testing.T) {
	rs := newRawServer(t, nil)
	// A permissive parent directory: a create-then-restrict writer would inherit
	// it for the duration of the write.
	if err := os.Chmod(rs.dataDir, 0o777); err != nil {
		t.Fatalf("chmod data dir: %v", err)
	}
	if err := rs.srv.WriteSetupTokenFile("http://localhost:8686/"); err != nil {
		t.Fatalf("WriteSetupTokenFile: %v", err)
	}

	path := filepath.Join(rs.dataDir, SetupTokenFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
	// No temp file survives the write, so there is no second, laxer copy of the
	// credential sitting beside it.
	entries, err := os.ReadDir(rs.dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != SetupTokenFileName {
		t.Errorf("want only the token file in the data dir, got %v", entries)
	}

	// The two-line contract the installer reads: URL, then token, each terminated.
	want := "http://localhost:8686/\n" + rs.srv.SetupToken() + "\n"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(got) != want {
		t.Errorf("token file = %q, want %q", got, want)
	}
}

// A leftover file from a previous process is removed before the mint, so its
// existence proves the running process wrote it — which is what lets the
// installer hand-off skip mtime arithmetic on an upgrade.
func TestStartupRemovesStaleTokenFile(t *testing.T) {
	dataDir := t.TempDir()
	stale := filepath.Join(dataDir, SetupTokenFileName)
	if err := os.WriteFile(stale, []byte("http://old/\nold-token\n"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}

	ptr := &atomic.Pointer[model.Config]{}
	cfg := config.Defaults()
	cfg.PasswordHash = "$2a$04$alreadyset" // even with the window closed
	ptr.Store(cfg)
	New(Deps{
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		DataDir:    dataDir,
		Config:     ptr,
		Supervisor: &fakeSupervisor{known: map[string]bool{}},
		Logger:     slog.New(NewLogBuffer(10).Handler()),
	})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stale token file must be removed at startup, stat err = %v", err)
	}
}

// An empty data directory disables the file channel entirely, so a server built
// with a zero-value Deps never touches the working directory.
func TestEmptyDataDirDisablesTokenFile(t *testing.T) {
	rs := newRawServer(t, nil)
	rs.srv.deps.DataDir = ""
	if err := rs.srv.WriteSetupTokenFile("http://localhost:8686/"); err != nil {
		t.Fatalf("WriteSetupTokenFile with no data dir: %v", err)
	}
	if _, err := os.Stat(SetupTokenFileName); !os.IsNotExist(err) {
		t.Fatalf("a relative token file was created in the working directory: %v", err)
	}
}

func TestSeedRequestOrigin(t *testing.T) {
	cases := []struct {
		name     string
		origin   string
		host     string
		existing []string
		want     string
		wantIP   bool
	}{
		{"origin wins over host", "https://commander.example.com", "localhost:8686", nil, "commander.example.com", false},
		{"port stripped", "http://nas.local:8686", "localhost:8686", nil, "nas.local", false},
		{"case folded", "http://NAS.Local", "localhost:8686", nil, "nas.local", false},
		{"trailing dot stripped", "http://nas.local./", "localhost:8686", nil, "nas.local", false},
		{"referer fallback handled by originHost", "", "nas.local:8686", nil, "nas.local", false},
		{"ip literal reported", "http://192.168.1.10:8686", "localhost:8686", nil, "192.168.1.10", true},
		{"ipv6 literal reported", "http://[fd00::1]:8686", "localhost:8686", nil, "fd00::1", true},
		{"already present is not duplicated", "http://nas.local", "localhost:8686", []string{"NAS.local"}, "nas.local", false},
		// An unparseable or "null" Origin reports ("", true) from originHost; the
		// fallback must trigger on the empty host, not on the present flag.
		{"null origin falls back to host", "null", "nas.local:8686", nil, "nas.local", false},
		{"wildcard origin falls back to host", "http://*", "nas.local:8686", nil, "nas.local", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Web.AllowedHosts = append([]string{}, tc.existing...)
			before := len(cfg.Web.AllowedHosts)

			r := httptest.NewRequest("POST", "/api/setup", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}

			got, isIP := seedRequestOrigin(cfg, r)
			if got != tc.want {
				t.Fatalf("seeded %q, want %q", got, tc.want)
			}
			if isIP != tc.wantIP {
				t.Errorf("isIP = %v, want %v", isIP, tc.wantIP)
			}
			if tc.existing != nil && len(cfg.Web.AllowedHosts) != before {
				t.Errorf("an existing entry must not be duplicated: %v", cfg.Web.AllowedHosts)
			}
		})
	}
}

// design architecture §8.2 invariant 7: a config PUT that would remove the
// requesting browser's own origin is refused and the stored config is
// untouched. The LAN-IP case is the one that matters: checkHost auto-admits a
// private literal while checkOrigin does not, so testing the wrong rule would
// let this through and leave the operator with working reads and refused
// writes.
func TestConfigPutSelfLockoutRejected(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	// The operator is reaching Commander at a LAN IP that is already allowlisted —
	// checkOrigin would refuse them otherwise, and the guard is about what the PUT
	// would do to an operator who is currently fine.
	cfg := seedAllowedHosts(h, "localhost", "192.168.1.10")
	updatedBefore := cfg.UpdatedAt

	next := config.Redact(cfg)
	next.Web.AllowedHosts = []string{"someone.else.local"}
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// checkHost admits the private literal on its own, so only the Origin rule can
	// catch this — which is the whole point of testing checkOrigin's rule here.
	resp := h.doAt("PUT", "/api/config", body, "192.168.1.10:8686")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-lockout PUT: want 400, got %d", resp.StatusCode)
	}
	blob, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(blob), "192.168.1.10") {
		t.Errorf("the refusal must name the entry that has to stay: %s", blob)
	}
	if h.cfgPtr.Load().UpdatedAt != updatedBefore {
		t.Error("a refused PUT must leave the stored config unchanged")
	}
}

// The same PUT keeping the requesting origin is accepted, so the guard is not
// simply refusing every allowlist edit.
func TestConfigPutKeepingOwnOriginAccepted(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	next := config.Redact(seedAllowedHosts(h, "localhost", "192.168.1.10"))
	next.Web.AllowedHosts = []string{"192.168.1.10", "nas.local"}
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := h.doAt("PUT", "/api/config", body, "192.168.1.10:8686")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		blob, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d (%s)", resp.StatusCode, blob)
	}
	if got := h.cfgPtr.Load().Web.AllowedHosts; len(got) != 2 || got[0] != "192.168.1.10" {
		t.Errorf("stored allowlist = %v", got)
	}
}

// A wildcard the operator is introducing is refused by the config PUT path.
func TestConfigPutRejectsNewWildcardHost(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	next := config.Redact(h.cfgPtr.Load())
	next.Web.AllowedHosts = append(next.Web.AllowedHosts, "*")
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := h.doRaw("PUT", "/api/config", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wildcard PUT: want 400, got %d", resp.StatusCode)
	}
}

// A wildcard already stored is grandfathered — the PUT succeeds — but the
// warning has to reach the operator, or the entry lives forever with no signal
// and the downgrade from error to warning becomes a silent exemption. It rides
// the 200 body alongside the config, whose shape is unchanged so a client that
// only knows about the config still parses it.
func TestConfigPutGrandfatheredWildcardWarns(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	next := config.Redact(seedAllowedHosts(h, "localhost", "*"))
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := h.doRaw("PUT", "/api/config", body)
	defer resp.Body.Close()
	blob, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grandfathered wildcard PUT: want 200, got %d (%s)", resp.StatusCode, blob)
	}

	var got struct {
		model.Config
		Problems []config.Problem `json:"problems"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal 200 body: %v", err)
	}
	if len(got.Problems) != 1 {
		t.Fatalf("want one warning about the grandfathered entry, got %+v", got.Problems)
	}
	if got.Problems[0].Severity != config.SeverityWarning ||
		got.Problems[0].Field != "web.allowed_hosts" ||
		!strings.Contains(got.Problems[0].Message, "wildcard") {
		t.Errorf("warning does not point at the wildcard entry: %+v", got.Problems[0])
	}
	// The config still arrives as the top-level object, not nested under a key an
	// older client would not look in.
	if len(got.Web.AllowedHosts) != 2 {
		t.Errorf("the 200 body must still be the config itself: %s", blob)
	}
}

// The same warning rides GET, so an install whose allowlist was hand-edited is
// flagged when the editor is opened rather than only after an unrelated save —
// an operator who never saves would otherwise never be told.
func TestConfigGetReportsStoredHostProblems(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	seedAllowedHosts(h, "localhost", "*")

	resp := h.doRaw("GET", "/api/config", nil)
	defer resp.Body.Close()
	blob, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", resp.StatusCode, blob)
	}
	var got struct {
		model.Config
		Problems []config.Problem `json:"problems"`
	}
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Problems) != 1 || got.Problems[0].Severity != config.SeverityWarning ||
		!strings.Contains(got.Problems[0].Message, "wildcard") {
		t.Fatalf("want one wildcard warning, got %+v", got.Problems)
	}
	if len(got.Web.AllowedHosts) != 2 {
		t.Errorf("the body must still be the config itself: %s", blob)
	}
}

// A clean allowlist produces no problems key at all, so the field is absent
// rather than an empty array in the ordinary case.
func TestConfigPutCleanHostsCarriesNoProblems(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	next := config.Redact(seedAllowedHosts(h, "localhost", "nas.local"))
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp := h.doRaw("PUT", "/api/config", body)
	defer resp.Body.Close()
	blob, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", resp.StatusCode, blob)
	}
	if strings.Contains(string(blob), "problems") {
		t.Errorf("a clean save must not carry a problems key: %s", blob)
	}
}

// seedAllowedHosts installs an allowlist on the live config and returns it, so a
// test can act as an operator already reaching Commander at one of the entries.
func seedAllowedHosts(h *harness, hosts ...string) *model.Config {
	h.t.Helper()
	cfg := *h.cfgPtr.Load()
	cfg.Web.AllowedHosts = hosts
	h.cfgPtr.Store(&cfg)
	return &cfg
}

// doAt issues a CSRF-carrying request presenting hostport as both Host and
// Origin, so a test can act as a browser reaching Commander at another address.
// The session cookies are attached by hand: net/http's jar does not apply to a
// request whose Host has been overridden, and a silently session-less request
// would 401 before reaching the rule under test.
func (h *harness) doAt(method, path string, body []byte, hostport string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.baseURL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Host = hostport
	req.Header.Set("Origin", "http://"+hostport)
	req.Header.Set(csrfHeader, h.csrf())
	u, err := url.Parse(h.baseURL)
	if err != nil {
		h.t.Fatalf("parse base url: %v", err)
	}
	for _, c := range h.client.Jar.Cookies(u) {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// doRaw issues a CSRF-carrying request with a pre-marshalled body.
func (h *harness) doRaw(method, path string, body []byte) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.baseURL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", h.baseURL)
	req.Header.Set(csrfHeader, h.csrf())
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// TestDeleteRigClearsMediaID pins that deleting a rig leaves no per-share media-id
// key behind. A surviving mediaid: key makes a genuinely new stick verify Foreign
// if the rig id is ever reused — blocking cleanup on hardware the user just
// adopted, with nothing in the UI to explain why.
func TestDeleteRigClearsMediaID(t *testing.T) {
	h := newHarness(t, true)
	h.login()

	profile := model.RigProfile{
		ID: "doomed", Name: "Doomed", GUID: "dev-guid", User: "guest", Password: "12345678",
		OTA: "RC8", Session: "S", Managed: true,
		Sources: []model.Source{
			{Share: "Udisk Images", Roots: []string{"Autorun"}},
			{Share: "EMMC Images", Roots: []string{"Autorun"}},
		},
	}
	resp := h.do("POST", "/api/rigs", profile, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create rig: want 201, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	// The worker would have adopted each removable medium and persisted its id.
	ctx := context.Background()
	for _, src := range profile.Sources {
		if err := h.store.SetMeta(ctx, rig.MediaIDKey("doomed", src.Share), "adopted-id"); err != nil {
			t.Fatalf("seed media id for %s: %v", src.Share, err)
		}
	}

	resp = h.do("DELETE", "/api/rigs/doomed", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("delete rig: want 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	for _, src := range profile.Sources {
		v, _, err := h.store.GetMeta(ctx, rig.MediaIDKey("doomed", src.Share))
		if err != nil {
			t.Fatalf("GetMeta for %s: %v", src.Share, err)
		}
		if v != "" {
			t.Fatalf("media id for %s survived rig deletion as %q; a reused rig id would judge a new stick foreign", src.Share, v)
		}
	}
}
