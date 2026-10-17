package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

var (
	errFakeRemoteNotFound   = errors.New("fake: no such remote")
	errFakeRegionUnknown    = errors.New("fake: no such region")
	errFakeRelayUnavailable = errors.New("fake: relay unreachable")
)

// fakeRemotes records what the API asks of the supervisor and holds a pairing
// secret the way the real one does: in its own state, never in a status.
type fakeRemotes struct {
	remotes   map[string]model.RemoteStatus
	address   string // stands in for a stored pairing secret
	calls     []string
	regionsFn func(context.Context) ([]model.RemoteRegion, error)
	seq       int
	addErr    error // returned by Add when set
	repairErr error // returned by Repair when set
}

const (
	fakeEnvValue = "acremote:ENV-VALUE-SECRET"
	// fakeEnvFile is what the supervisor hands back; the API passes it through.
	fakeEnvFile = "ASIAIR_REMOTE_PAIRING=" + fakeEnvValue + "\n"
	fakeAddress = "tc-secret-address-with-psk"
)

func newFakeRemotes() *fakeRemotes {
	return &fakeRemotes{remotes: map[string]model.RemoteStatus{}, address: fakeAddress}
}

func (f *fakeRemotes) List() []model.RemoteStatus {
	var out []model.RemoteStatus
	for i := 1; i <= f.seq; i++ {
		if r, ok := f.remotes[fmt.Sprintf("r%d", i)]; ok {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeRemotes) Regions(ctx context.Context) ([]model.RemoteRegion, error) {
	if f.regionsFn != nil {
		return f.regionsFn(ctx)
	}
	return []model.RemoteRegion{{ID: 9, Code: "tor", Name: "Toronto", Nearest: true}, {ID: 1, Code: "fra", Name: "Frankfurt"}}, nil
}

func (f *fakeRemotes) Add(_ context.Context, name string, regionID int) (model.RemotePairing, error) {
	f.calls = append(f.calls, fmt.Sprintf("add %s %d", name, regionID))
	if f.addErr != nil {
		return model.RemotePairing{}, f.addErr
	}
	f.seq++
	st := model.RemoteStatus{ID: fmt.Sprintf("r%d", f.seq), Name: name, State: model.RemoteWaiting, Region: "tor", Fingerprint: "ab:cd"}
	f.remotes[st.ID] = st
	return model.RemotePairing{Remote: st, EnvValue: fakeEnvValue, EnvFile: fakeEnvFile}, nil
}

func (f *fakeRemotes) Repair(_ context.Context, id string, regionID int) (model.RemotePairing, error) {
	f.calls = append(f.calls, fmt.Sprintf("repair %s %d", id, regionID))
	if f.repairErr != nil {
		return model.RemotePairing{}, f.repairErr
	}
	st, ok := f.remotes[id]
	if !ok {
		return model.RemotePairing{}, fmt.Errorf("repair: %w", errFakeRemoteNotFound)
	}
	return model.RemotePairing{Remote: st, EnvValue: fakeEnvValue + "-2", EnvFile: "X=" + fakeEnvValue + "-2\n"}, nil
}

func (f *fakeRemotes) Rename(id, name string) error {
	f.calls = append(f.calls, "rename "+id+" "+name)
	st, ok := f.remotes[id]
	if !ok {
		return errFakeRemoteNotFound
	}
	st.Name = name
	f.remotes[id] = st
	return nil
}

func (f *fakeRemotes) SetRateLimit(id string, mbps float64) error {
	f.calls = append(f.calls, fmt.Sprintf("limit %s %g", id, mbps))
	st, ok := f.remotes[id]
	if !ok {
		return errFakeRemoteNotFound
	}
	st.RateLimitMBps = mbps
	f.remotes[id] = st
	return nil
}

func (f *fakeRemotes) Remove(id string) error {
	f.calls = append(f.calls, "remove "+id)
	if _, ok := f.remotes[id]; !ok {
		return errFakeRemoteNotFound
	}
	delete(f.remotes, id)
	return nil
}

func newRemotesHarness(t *testing.T) (*harness, *fakeRemotes) {
	t.Helper()
	h := newHarness(t, true)
	f := newFakeRemotes()
	h.srv.deps.Remotes = f
	h.srv.deps.RemoteNotFound = errFakeRemoteNotFound
	h.srv.deps.RemoteRegionUnknown = errFakeRegionUnknown
	h.srv.deps.RelayUnavailable = errFakeRelayUnavailable
	h.login()
	return h, f
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

type pairingBody struct {
	Remote      model.RemoteStatus `json:"remote"`
	EnvValue    string             `json:"env_value"`
	EnvFile     string             `json:"env_file"`
	ComposeFile string             `json:"compose_file"`
	SetupZip    string             `json:"setup_zip"`
	Fingerprint string             `json:"fingerprint"`
}

func createRemote(t *testing.T, h *harness) pairingBody {
	t.Helper()
	resp := h.do("POST", "/api/remotes", map[string]any{"name": "Observatory"}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	var p pairingBody
	decode(t, resp, &p)
	return p
}

// TestRemotesAPIRedactsSecrets is the API half of design remote-sites AC10:
// only the create and re-pair responses carry the env value, and no GET returns
// a stored address, a key or the env value afterwards.
func TestRemotesAPIRedactsSecrets(t *testing.T) {
	h, _ := newRemotesHarness(t)
	cfg := h.cfgPtr.Load()
	cfg.Remotes = []model.RemoteProfile{{ID: "r1", Name: "Observatory", Address: fakeAddress, ClientKey: "privkey:secret-client-key"}}

	p := createRemote(t, h)
	if p.EnvValue != fakeEnvValue || p.EnvFile != fakeEnvFile || p.Fingerprint != "ab:cd" {
		t.Fatalf("create response: %+v", p)
	}
	resp := h.do("POST", "/api/remotes/r1:repair", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repair: want 200, got %d", resp.StatusCode)
	}
	var rp pairingBody
	decode(t, resp, &rp)
	if rp.EnvValue != fakeEnvValue+"-2" || rp.ComposeFile == "" || !strings.Contains(rp.EnvFile, rp.EnvValue) {
		t.Fatalf("repair response: %+v", rp)
	}

	if p.SetupZip == "" || rp.SetupZip == "" {
		t.Fatal("create and repair must carry setup_zip")
	}
	secrets := []string{fakeAddress, "privkey:secret-client-key", "ENV-VALUE-SECRET", "acremote:", "setup_zip", p.SetupZip, rp.SetupZip}
	for _, path := range []string{"/api/remotes", "/api/remotes/regions", "/api/config", "/api/rigs"} {
		got := bodyOf(t, h.do("GET", path, nil, reqOpts{}))
		for _, s := range secrets {
			if strings.Contains(got, s) {
				t.Errorf("GET %s returned a secret (%q)", path, s)
			}
		}
	}

	// Neither the app log nor an error carries it either.
	for _, e := range h.appLog.Entries(slog.LevelDebug, 1000) {
		if strings.Contains(e.Message, "ENV-VALUE-SECRET") || strings.Contains(fmt.Sprint(e.Attrs), "ENV-VALUE-SECRET") {
			t.Errorf("log entry carries the env value: %+v", e)
		}
	}
}

func TestRemotesCreateAndRepairAreNoStore(t *testing.T) {
	h, _ := newRemotesHarness(t)
	resp := h.do("POST", "/api/remotes", map[string]any{"name": "A"}, reqOpts{csrf: true})
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("create Cache-Control = %q, want no-store", got)
	}
	resp.Body.Close()
	resp = h.do("POST", "/api/remotes/r1:repair", nil, reqOpts{csrf: true})
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("repair Cache-Control = %q, want no-store", got)
	}
	resp.Body.Close()
}

// TestRemotesRoutesLogNoBodies: a create and a re-pair log no request or
// response content, only the remote's id.
func TestRemotesRoutesLogNoBodies(t *testing.T) {
	h, _ := newRemotesHarness(t)
	resp := h.do("POST", "/api/remotes", map[string]any{"name": "SecretNameInBody"}, reqOpts{csrf: true})
	resp.Body.Close()
	resp = h.do("POST", "/api/remotes/r1:repair", nil, reqOpts{csrf: true})
	resp.Body.Close()
	for _, e := range h.appLog.Entries(slog.LevelDebug, 1000) {
		line := e.Message + fmt.Sprint(e.Attrs)
		for _, s := range []string{"SecretNameInBody", "ENV-VALUE-SECRET", "compose", "services:"} {
			if strings.Contains(line, s) {
				t.Errorf("log entry %q contains %q", line, s)
			}
		}
	}
}

// TestRemoteComposeMatchesDeploy: the compose file the API returns is the exact
// bytes of deploy/remote/docker-compose.yml.
func TestRemoteComposeMatchesDeploy(t *testing.T) {
	want, err := os.ReadFile("../../deploy/remote/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := newRemotesHarness(t)
	if got := createRemote(t, h).ComposeFile; got != string(want) {
		t.Fatal("create's compose_file differs from deploy/remote/docker-compose.yml; copy it to internal/web/remote_compose.yml")
	}
}

// TestRemoteSetupZip: the setup zip holds exactly the env and compose files of
// the same response, in one asiair-remote folder, with the .env private.
func TestRemoteSetupZip(t *testing.T) {
	h, _ := newRemotesHarness(t)
	createRemote(t, h)
	resp := h.do("POST", "/api/remotes/r1:repair", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repair: want 200, got %d", resp.StatusCode)
	}
	var rp pairingBody
	decode(t, resp, &rp)
	for name, p := range map[string]pairingBody{"create": createRemote(t, h), "repair": rp} {
		raw, err := base64.StdEncoding.DecodeString(p.SetupZip)
		if err != nil {
			t.Fatalf("%s: setup_zip is not base64: %v", name, err)
		}
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("%s: setup_zip is not a zip: %v", name, err)
		}
		want := []struct {
			name, body string
			mode       os.FileMode
		}{
			{"asiair-remote/.env", p.EnvFile, 0o600},
			{"asiair-remote/docker-compose.yml", p.ComposeFile, 0o644},
		}
		if len(zr.File) != len(want) {
			t.Fatalf("%s: zip has %d entries, want %d", name, len(zr.File), len(want))
		}
		for i, w := range want {
			f := zr.File[i]
			if f.Name != w.name {
				t.Errorf("%s: entry %d is %q, want %q", name, i, f.Name, w.name)
			}
			if got := f.Mode().Perm(); got != w.mode {
				t.Errorf("%s: %s mode %v, want %v", name, f.Name, got, w.mode)
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("%s: open %s: %v", name, f.Name, err)
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatalf("%s: read %s: %v", name, f.Name, err)
			}
			if string(b) != w.body {
				t.Errorf("%s: %s differs from the response's own file", name, f.Name)
			}
		}
	}
}

// TestRemoveRemoteCallsSupervisor is the API half of a removal: DELETE reaches
// the supervisor's Remove, which owns the cleanup, and an unknown id is a 404.
func TestRemoveRemoteCallsSupervisor(t *testing.T) {
	h, f := newRemotesHarness(t)
	createRemote(t, h)
	resp := h.do("DELETE", "/api/remotes/r1", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if last := f.calls[len(f.calls)-1]; last != "remove r1" {
		t.Fatalf("supervisor calls = %v, want a final remove r1", f.calls)
	}
	resp = h.do("DELETE", "/api/remotes/r1", nil, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRemotesRenameAndRateLimit(t *testing.T) {
	h, f := newRemotesHarness(t)
	createRemote(t, h)

	resp := h.do("PATCH", "/api/remotes/r1", map[string]any{"name": " Roof "}, reqOpts{csrf: true})
	var st model.RemoteStatus
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: got %d", resp.StatusCode)
	}
	decode(t, resp, &st)
	if st.Name != "Roof" {
		t.Fatalf("rename: name %q", st.Name)
	}

	resp = h.do("PUT", "/api/remotes/r1/rate-limit", map[string]any{"mbps": 2.5}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rate-limit: got %d", resp.StatusCode)
	}
	decode(t, resp, &st)
	if st.RateLimitMBps != 2.5 {
		t.Fatalf("rate limit = %v", st.RateLimitMBps)
	}

	n := len(f.calls)
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PATCH", "/api/remotes/r1", map[string]any{"name": "  "}, 400},
		{"PATCH", "/api/remotes/r1", map[string]any{"nme": "x"}, 400},
		{"PUT", "/api/remotes/r1/rate-limit", map[string]any{"mbps": -1}, 400},
		{"POST", "/api/remotes", map[string]any{"name": ""}, 400},
		{"POST", "/api/remotes", map[string]any{"name": "x", "region": -2}, 400},
		{"POST", "/api/remotes/r1:nope", nil, 400},
		{"PATCH", "/api/remotes/r9", map[string]any{"name": "x"}, 404},
		{"PUT", "/api/remotes/r9/rate-limit", map[string]any{"mbps": 1}, 404},
		{"POST", "/api/remotes/r9:repair", nil, 404},
	} {
		resp := h.do(c.method, c.path, c.body, reqOpts{csrf: true})
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: want %d, got %d", c.method, c.path, c.want, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// The 400s never reached the supervisor.
	for _, call := range f.calls[n:] {
		if !strings.Contains(call, "r9") {
			t.Errorf("a rejected request reached the supervisor: %q", call)
		}
	}
}

func TestRemotesWritesNeedOriginAndCSRF(t *testing.T) {
	h, f := newRemotesHarness(t)
	createRemote(t, h)
	n := len(f.calls)
	for _, w := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/remotes", map[string]any{"name": "x"}},
		{"PATCH", "/api/remotes/r1", map[string]any{"name": "x"}},
		{"PUT", "/api/remotes/r1/rate-limit", map[string]any{"mbps": 1}},
		{"POST", "/api/remotes/r1:repair", nil},
		{"DELETE", "/api/remotes/r1", nil},
	} {
		for name, o := range map[string]reqOpts{
			"no csrf":      {},
			"foreign":      {csrf: true, origin: "http://evil.example"},
			"missing orig": {csrf: true, origin: "-"},
		} {
			resp := h.do(w.method, w.path, w.body, o)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s (%s): want 403, got %d", w.method, w.path, name, resp.StatusCode)
			}
			resp.Body.Close()
		}
	}
	if len(f.calls) != n {
		t.Fatalf("a rejected write reached the supervisor: %v", f.calls[n:])
	}
}

func TestRemoteRegions(t *testing.T) {
	h, f := newRemotesHarness(t)
	var regions []model.RemoteRegion
	decode(t, h.do("GET", "/api/remotes/regions", nil, reqOpts{}), &regions)
	if len(regions) != 2 || !regions[0].Nearest {
		t.Fatalf("regions = %+v", regions)
	}

	var deadline bool
	f.regionsFn = func(ctx context.Context) ([]model.RemoteRegion, error) {
		_, deadline = ctx.Deadline()
		return nil, fmt.Errorf("%w: offline", errFakeRelayUnavailable)
	}
	resp := h.do("GET", "/api/remotes/regions", nil, reqOpts{})
	if resp.StatusCode != http.StatusBadGateway || !deadline {
		t.Fatalf("offline: status %d, bounded %v", resp.StatusCode, deadline)
	}
	resp.Body.Close()
}

// TestRemotePairingErrorStatuses: an unknown relay region is the caller's 400,
// an unreachable relay service a 502, and anything else a 500.
func TestRemotePairingErrorStatuses(t *testing.T) {
	h, f := newRemotesHarness(t)
	createRemote(t, h)
	for _, c := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: 7", errFakeRegionUnknown), http.StatusBadRequest},
		{fmt.Errorf("%w: down", errFakeRelayUnavailable), http.StatusBadGateway},
		{errors.New("disk full"), http.StatusInternalServerError},
	} {
		f.addErr, f.repairErr = c.err, c.err
		for _, req := range []struct {
			path string
			body any
		}{
			{"/api/remotes", map[string]any{"name": "x", "region": 7}},
			{"/api/remotes/r1:repair", map[string]any{"region": 7}},
		} {
			resp := h.do("POST", req.path, req.body, reqOpts{csrf: true})
			if resp.StatusCode != c.want {
				t.Errorf("POST %s with %v: want %d, got %d", req.path, c.err, c.want, resp.StatusCode)
			}
			resp.Body.Close()
		}
	}
}

func TestRemotesUnavailableWithoutSupervisor(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	resp := h.do("GET", "/api/remotes", nil, reqOpts{})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
