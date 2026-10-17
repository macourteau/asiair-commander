package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// fakeFiles is a scripted DeviceFiles. err, when set, is every call's refusal
// before anything is written. A run emits lines and, with block, then waits for
// its request to be cancelled and records what a further emit returned.
type fakeFiles struct {
	mu    sync.Mutex
	calls []string
	err   error

	listing    *devfiles.Listing
	deletePlan *devfiles.DeletePlan
	copyPlan   *devfiles.CopyPlan
	info       *devfiles.DownloadInfo
	body       []byte
	midStream  bool // fail after the first byte
	lines      []any
	block      bool

	gotRig     string
	gotShare   string
	gotDir     string
	gotNames   []string
	gotMkdir   string
	gotCopy    devfiles.CopyRequest
	gotDelete  devfiles.DeleteRequest
	gotCopyReq devfiles.CopyPlanRequest

	uploadPlan    *devfiles.UploadPlan
	stageBroken   bool // UploadStage reads the body, then reports it broke off
	gotRename     [2]string
	gotOwners     []string // the owner key of every upload call, in order
	gotStageName  string
	gotStageSize  int64
	gotStageMtime time.Time
	gotStageBody  []byte
	gotUploadPlan devfiles.UploadPlanRequest
	gotUpload     devfiles.UploadRequest
	gotDiscard    string

	runStarted chan struct{}
	cancelled  chan error // the emit error after the cancel
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{runStarted: make(chan struct{}), cancelled: make(chan error, 1)}
}

func (f *fakeFiles) record(call, rig, share, dir string, names []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.gotRig, f.gotShare, f.gotDir, f.gotNames = rig, share, dir, names
	return f.err
}

func (f *fakeFiles) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeFiles) List(_ context.Context, rigID, share, dir string) (*devfiles.Listing, error) {
	if err := f.record("list", rigID, share, dir, nil); err != nil {
		return nil, err
	}
	l := *f.listing
	return &l, nil
}

func (f *fakeFiles) Mkdir(_ context.Context, rigID, share, dir, name string) error {
	f.mu.Lock()
	f.gotMkdir = name
	f.mu.Unlock()
	return f.record("mkdir", rigID, share, dir, nil)
}

func (f *fakeFiles) DownloadCheck(_ context.Context, rigID, share, dir string, names []string) (*devfiles.DownloadInfo, error) {
	if err := f.record("download check", rigID, share, dir, names); err != nil {
		return nil, err
	}
	return f.info, nil
}

func (f *fakeFiles) Download(_ context.Context, rigID, share, dir string, names []string, w devfiles.DownloadWriter) error {
	if err := f.record("download", rigID, share, dir, names); err != nil {
		return err
	}
	w.Begin(*f.info)
	if f.midStream {
		_, _ = w.Write(f.body[:len(f.body)/2])
		cause := &devfiles.Error{Kind: devfiles.ErrUnreachable, Msg: "gone"}
		return &devfiles.Error{Kind: devfiles.ErrMidStream, Msg: "download failed after it started", Err: cause}
	}
	_, err := w.Write(f.body)
	return err
}

func (f *fakeFiles) DeletePlan(_ context.Context, rigID, share, dir string, names []string) (*devfiles.DeletePlan, error) {
	if err := f.record("delete plan", rigID, share, dir, names); err != nil {
		return nil, err
	}
	p := *f.deletePlan
	return &p, nil
}

func (f *fakeFiles) Delete(ctx context.Context, rigID string, req devfiles.DeleteRequest, emit devfiles.Emit) error {
	f.mu.Lock()
	f.gotDelete = req
	f.mu.Unlock()
	if err := f.record("delete", rigID, req.Share, req.Dir, nil); err != nil {
		return err
	}
	return f.run(ctx, emit)
}

func (f *fakeFiles) CopyPlan(_ context.Context, req devfiles.CopyPlanRequest) (*devfiles.CopyPlan, error) {
	f.mu.Lock()
	f.gotCopyReq = req
	f.mu.Unlock()
	if err := f.record("copy plan", req.From.Rig, req.From.Share, req.From.Dir, req.From.Names); err != nil {
		return nil, err
	}
	p := *f.copyPlan
	return &p, nil
}

func (f *fakeFiles) Copy(ctx context.Context, req devfiles.CopyRequest, emit devfiles.Emit) error {
	f.mu.Lock()
	f.gotCopy = req
	f.mu.Unlock()
	if err := f.record("copy", req.From.Rig, req.From.Share, req.From.Dir, nil); err != nil {
		return err
	}
	return f.run(ctx, emit)
}

func (f *fakeFiles) Rename(_ context.Context, rigID, share, dir, name, newName string) error {
	f.mu.Lock()
	f.gotRename = [2]string{name, newName}
	f.mu.Unlock()
	return f.record("rename", rigID, share, dir, nil)
}

func (f *fakeFiles) owner(owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotOwners = append(f.gotOwners, owner)
}

// UploadStage refuses with err before reading a byte, as the service does,
// so a refusal leaves the body for the handler to drain.
func (f *fakeFiles) UploadStage(_ context.Context, owner, name string, size int64, mtime time.Time, body io.Reader) (devfiles.StagedUpload, error) {
	f.owner(owner)
	if err := f.record("upload stage", "", "", "", nil); err != nil {
		return devfiles.StagedUpload{}, err
	}
	b, err := io.ReadAll(body)
	f.mu.Lock()
	f.gotStageName, f.gotStageSize, f.gotStageMtime, f.gotStageBody = name, size, mtime, b
	broken := f.stageBroken
	f.mu.Unlock()
	if err != nil || broken {
		return devfiles.StagedUpload{}, &devfiles.Error{Kind: devfiles.ErrStageBody, Msg: "broke off", Err: err}
	}
	return devfiles.StagedUpload{ID: "0123456789abcdef0123456789abcdef", Size: int64(len(b))}, nil
}

func (f *fakeFiles) UploadPlan(_ context.Context, owner string, req devfiles.UploadPlanRequest) (*devfiles.UploadPlan, error) {
	f.owner(owner)
	f.mu.Lock()
	f.gotUploadPlan = req
	f.mu.Unlock()
	if err := f.record("upload plan", req.To.Rig, req.To.Share, req.To.Dir, nil); err != nil {
		return nil, err
	}
	p := *f.uploadPlan
	return &p, nil
}

func (f *fakeFiles) Upload(ctx context.Context, owner string, req devfiles.UploadRequest, emit devfiles.Emit) error {
	f.owner(owner)
	f.mu.Lock()
	f.gotUpload = req
	f.mu.Unlock()
	if err := f.record("upload", req.To.Rig, req.To.Share, req.To.Dir, nil); err != nil {
		return err
	}
	return f.run(ctx, emit)
}

func (f *fakeFiles) UploadDiscard(owner, id string) error {
	f.owner(owner)
	f.mu.Lock()
	f.gotDiscard = id
	f.mu.Unlock()
	return f.record("upload discard", "", "", "", nil)
}

func (f *fakeFiles) owners() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotOwners...)
}

func (f *fakeFiles) run(ctx context.Context, emit devfiles.Emit) error {
	for _, l := range f.lines {
		if err := emit(l); err != nil {
			return err
		}
	}
	if !f.block {
		return nil
	}
	close(f.runStarted)
	<-ctx.Done()
	// What the engine would see if it tried to write once more.
	f.cancelled <- emit(devfiles.ProgressLine{Type: "progress", Current: "after-cancel"})
	return &devfiles.Error{Kind: devfiles.ErrCancelled, Msg: "cancelled", Err: ctx.Err()}
}

// filesRoute is one device-files route and a request that reaches its handler.
type filesRoute struct {
	method, pattern, path string
	body                  any
}

var filesRoutes = []filesRoute{
	{"GET", "GET /api/rigs/{id}/files", "/api/rigs/r1/files?share=EMMC%20Images&dir=Autorun", nil},
	{"GET", "GET /api/rigs/{id}/files/download", "/api/rigs/r1/files/download?share=EMMC%20Images&dir=&name=a.fit&check=1", nil},
	{"POST", "POST /api/rigs/{id}/files/mkdir", "/api/rigs/r1/files/mkdir", map[string]any{"share": "EMMC Images", "dir": "", "name": "New"}},
	{"POST", "POST /api/rigs/{id}/files/delete/plan", "/api/rigs/r1/files/delete/plan", map[string]any{"share": "EMMC Images", "dir": "", "names": []string{"a.fit"}}},
	{"POST", "POST /api/rigs/{id}/files/delete", "/api/rigs/r1/files/delete", map[string]any{"share": "EMMC Images", "dir": "", "items": []any{}, "dirs": []string{}, "acknowledged_unarchived": 0}},
	{"POST", "POST /api/files/copy/plan", "/api/files/copy/plan", map[string]any{"from": map[string]any{"rig": "r1", "share": "EMMC Images", "dir": "", "names": []string{"a.fit"}}, "to": map[string]any{"rig": "r2", "share": "EMMC Images", "dir": ""}, "mirror": false}},
	{"POST", "POST /api/files/copy", "/api/files/copy", map[string]any{"from": map[string]any{"rig": "r1", "share": "EMMC Images", "dir": ""}, "to": map[string]any{"rig": "r2", "share": "EMMC Images", "dir": ""}, "dirs": []string{}, "items": []any{}, "overwrite": []any{}}},
	{"POST", "POST /api/rigs/{id}/files/rename", "/api/rigs/r1/files/rename", map[string]any{"share": "EMMC Images", "dir": "Preview", "name": "a.fit", "new_name": "b.fit"}},
	// The staging body is raw bytes; a JSON string is as good a body as any.
	{"POST", "POST /api/files/upload/stage", "/api/files/upload/stage?name=ref.fit&mtime_ms=1760000000000", "file bytes"},
	{"POST", "POST /api/files/upload/plan", "/api/files/upload/plan", map[string]any{"id": "abc", "to": map[string]any{"rig": "r2", "share": "EMMC Images", "dir": "Preview"}}},
	{"POST", "POST /api/files/upload", "/api/files/upload", map[string]any{"id": "abc", "to": map[string]any{"rig": "r2", "share": "EMMC Images", "dir": "Preview"}}},
	{"POST", "POST /api/files/upload/discard", "/api/files/upload/discard", map[string]any{"id": "abc"}},
}

// Indexes into filesRoutes of the routes tests name.
const (
	routeCopy        = 6
	routeRename      = 7
	routeStage       = 8
	routeUploadPlan  = 9
	routeUpload      = 10
	routeUploadClose = 11
)

func newFilesHarness(t *testing.T, df DeviceFiles) *harness {
	t.Helper()
	h := newHarness(t, true, func(d *Deps) { d.DeviceFiles = df })
	h.login()
	return h
}

// readLines reads an NDJSON body into decoded lines.
func readLines(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	return out
}

// TestFilesRoutesRegistered checks every device-files route is listed for the
// no-CORS sweep and reaches its handler through the right guard.
func TestFilesRoutesRegistered(t *testing.T) {
	df := newFakeFiles()
	df.listing = &devfiles.Listing{}
	df.deletePlan = &devfiles.DeletePlan{}
	df.copyPlan = &devfiles.CopyPlan{}
	df.uploadPlan = &devfiles.UploadPlan{}
	df.info = &devfiles.DownloadInfo{Filename: "a.fit", Files: 1, Bytes: 3}
	h := newFilesHarness(t, df)

	for _, rt := range filesRoutes {
		if !slices.Contains(registeredRoutes, rt.pattern) {
			t.Errorf("registeredRoutes does not list %q", rt.pattern)
		}
		resp := h.do(rt.method, rt.path, rt.body, reqOpts{csrf: rt.method == "POST"})
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Errorf("%s %s: want 2xx, got %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	want := []string{"list", "download check", "mkdir", "delete plan", "delete", "copy plan", "copy",
		"rename", "upload stage", "upload plan", "upload", "upload discard"}
	if got := df.callList(); !slices.Equal(got, want) {
		t.Fatalf("service calls = %v, want %v", got, want)
	}
	if df.gotMkdir != "New" {
		t.Fatalf("mkdir body not passed through: name %q", df.gotMkdir)
	}
	if df.gotRename != [2]string{"a.fit", "b.fit"} {
		t.Fatalf("rename body not passed through: %q", df.gotRename)
	}
	if df.gotStageName != "ref.fit" || !df.gotStageMtime.Equal(time.UnixMilli(1760000000000)) ||
		string(df.gotStageBody) != `"file bytes"` || df.gotStageSize != int64(len(`"file bytes"`)) {
		t.Fatalf("staging request not passed through: name %q mtime %v size %d body %q",
			df.gotStageName, df.gotStageMtime, df.gotStageSize, df.gotStageBody)
	}
	if df.gotUploadPlan.ID != "abc" || df.gotUploadPlan.To.Dir != "Preview" ||
		df.gotUpload.ID != "abc" || df.gotUpload.To.Rig != "r2" || df.gotDiscard != "abc" {
		t.Fatalf("upload requests not passed through: plan %+v run %+v discard %q",
			df.gotUploadPlan, df.gotUpload, df.gotDiscard)
	}
}

// TestFilesNoRouteServesStagedBytes checks the staged-upload routes are all
// writes: no route reads a staged file back.
func TestFilesNoRouteServesStagedBytes(t *testing.T) {
	for _, r := range registeredRoutes {
		if strings.Contains(r, "upload") && !strings.HasPrefix(r, "POST ") {
			t.Errorf("route %q reaches staged uploads without being a POST", r)
		}
	}
}

// TestFilesNilServiceAnswers503 checks every route answers 503
// files_unavailable when no service is wired in.
func TestFilesNilServiceAnswers503(t *testing.T) {
	h := newHarness(t, true)
	h.login()
	for _, rt := range filesRoutes {
		resp := h.do(rt.method, rt.path, rt.body, reqOpts{csrf: rt.method == "POST"})
		if resp.StatusCode != http.StatusServiceUnavailable {
			resp.Body.Close()
			t.Fatalf("%s %s: want 503, got %d", rt.method, rt.path, resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "files_unavailable" {
			t.Fatalf("%s %s: code %q, want files_unavailable", rt.method, rt.path, code)
		}
	}
}

// TestFilesPostsRequireCSRFAndOrigin checks every POST, the read-only plans
// included, is refused without the CSRF header or from a foreign Origin, and
// every GET without a session, before the service is called.
func TestFilesPostsRequireCSRFAndOrigin(t *testing.T) {
	df := newFakeFiles()
	h := newFilesHarness(t, df)
	for _, rt := range filesRoutes {
		if rt.method != "POST" {
			continue
		}
		for name, o := range map[string]reqOpts{
			"no csrf":    {csrf: false},
			"bad origin": {csrf: true, origin: "http://evil.example"},
			"no origin":  {csrf: true, origin: "-"},
		} {
			resp := h.do(rt.method, rt.path, rt.body, o)
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s (%s): want 403, got %d", rt.method, rt.path, name, resp.StatusCode)
			}
		}
	}

	anon := newHarness(t, true, func(d *Deps) { d.DeviceFiles = df })
	for _, rt := range filesRoutes {
		if rt.method != "GET" {
			continue
		}
		resp := anon.do(rt.method, rt.path, nil, reqOpts{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: want 401, got %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	if calls := df.callList(); len(calls) != 0 {
		t.Fatalf("a refused request reached the service: %v", calls)
	}
}

// TestFilesRunStreamsNDJSON checks a copy and a delete stream every line as
// NDJSON, ending with the summary, and that the echoed request reaches the
// service intact.
func TestFilesRunStreamsNDJSON(t *testing.T) {
	dstSize, dstMtime := int64(16), int64(1760000400)
	df := newFakeFiles()
	df.lines = []any{
		devfiles.ProgressLine{Type: "progress", Done: 0, Total: 2, BytesTotal: 32, Current: "M42/a.fit"},
		devfiles.ItemLine{Type: "item", Rel: "M42/b.fit", Outcome: "skipped", Code: "appeared", DstSize: &dstSize, DstMtime: &dstMtime},
		devfiles.SummaryLine{Type: "summary", Done: 1, Skipped: 1},
	}
	h := newFilesHarness(t, df)

	for _, rt := range filesRoutes {
		if rt.pattern != "POST /api/files/copy" && rt.pattern != "POST /api/rigs/{id}/files/delete" &&
			rt.pattern != "POST /api/files/upload" {
			continue
		}
		body := rt.body
		if rt.pattern == "POST /api/files/copy" {
			body = map[string]any{
				"from": map[string]any{"rig": "r1", "share": "EMMC Images", "dir": "Preview"},
				"to":   map[string]any{"rig": "r2", "share": "EMMC Images", "dir": "Preview"},
				"dirs": []string{"M42"}, "items": []any{map[string]any{"rel": "M42/a.fit", "size": 16}},
				"overwrite": []any{map[string]any{"rel": "M42/a.fit", "dst_size": 16, "dst_mtime": 1760000000}},
			}
		}
		resp := h.do("POST", rt.path, body, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("%s: want 200, got %d", rt.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
			t.Fatalf("%s: Content-Type %q", rt.path, ct)
		}
		lines := readLines(t, resp)
		if len(lines) != 3 || lines[len(lines)-1]["type"] != "summary" {
			t.Fatalf("%s: lines %v, want three ending with the summary", rt.path, lines)
		}
		if lines[1]["dst_mtime"] != float64(dstMtime) || lines[2]["aborted"] != "" {
			t.Fatalf("%s: line shapes %v", rt.path, lines)
		}
	}
	c := df.gotCopy
	if c.From.Dir != "Preview" || c.To.Rig != "r2" || len(c.Items) != 1 || c.Items[0].Size != 16 ||
		len(c.Overwrite) != 1 || c.Overwrite[0].DstMtime != 1760000000 || !slices.Equal(c.Dirs, []string{"M42"}) {
		t.Fatalf("copy request = %+v", c)
	}
	if df.gotDelete.Share != "EMMC Images" {
		t.Fatalf("delete request = %+v", df.gotDelete)
	}
	u := df.gotUpload
	if u.ID != "abc" || u.To.Share != "EMMC Images" || u.DstSize != nil || u.DstMtime != nil {
		t.Fatalf("upload request = %+v", u)
	}
}

// TestFilesRunRefusalIsPlainJSON checks a refusal before the first line is an
// ordinary JSON error, not a stream.
func TestFilesRunRefusalIsPlainJSON(t *testing.T) {
	df := newFakeFiles()
	df.err = &devfiles.Error{Kind: devfiles.ErrBusy, Msg: "Copy on Rig One is running, started 21:00."}
	h := newFilesHarness(t, df)
	resp := h.do("POST", "/api/files/copy", filesRoutes[routeCopy].body, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q, want JSON", ct)
	}
	var body map[string]any
	decode(t, resp, &body)
	if body["code"] != "busy" || body["message"] != "Copy on Rig One is running, started 21:00." {
		t.Fatalf("body = %v", body)
	}
}

// TestFilesRunCancelReachesService checks a client that goes away mid-run
// cancels the service's context, and that nothing more is written after it.
// An upload run is copy's handler, so its Stop is the same cancel.
func TestFilesRunCancelReachesService(t *testing.T) {
	for _, rt := range []filesRoute{filesRoutes[routeCopy], filesRoutes[routeUpload]} {
		t.Run(rt.path, func(t *testing.T) {
			df := newFakeFiles()
			df.lines = []any{devfiles.ProgressLine{Type: "progress", Total: 1, Current: "a.fit"}}
			df.block = true
			h := newFilesHarness(t, df)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b, _ := json.Marshal(rt.body)
			req, _ := http.NewRequestWithContext(ctx, "POST", h.baseURL+rt.path, bytes.NewReader(b))
			req.Header.Set("Origin", h.baseURL)
			req.Header.Set(csrfHeader, h.csrf())
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			defer resp.Body.Close()
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			if err != nil || !strings.Contains(line, `"progress"`) {
				t.Fatalf("first line %q, %v", line, err)
			}
			<-df.runStarted
			cancel()

			select {
			case emitErr := <-df.cancelled:
				if emitErr == nil {
					t.Fatal("an emit after the cancel succeeded: the handler wrote past the cancel")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the request cancel never reached the service")
			}
		})
	}
}

// TestFilesEmptySlicesSerializeAsArrays checks no list in a listing or a plan
// reaches the SPA as null.
func TestFilesEmptySlicesSerializeAsArrays(t *testing.T) {
	df := newFakeFiles()
	df.listing = &devfiles.Listing{Share: "EMMC Images"}
	df.deletePlan = &devfiles.DeletePlan{}
	df.copyPlan = &devfiles.CopyPlan{}
	df.uploadPlan = &devfiles.UploadPlan{}
	h := newFilesHarness(t, df)

	check := func(resp *http.Response, keys ...string) {
		t.Helper()
		var body map[string]any
		decode(t, resp, &body)
		for _, k := range keys {
			if v, ok := body[k].([]any); !ok || len(v) != 0 {
				t.Errorf("%s = %#v, want []", k, body[k])
			}
		}
	}
	check(h.do("GET", filesRoutes[0].path, nil, reqOpts{}), "entries")
	check(h.do("POST", filesRoutes[3].path, filesRoutes[3].body, reqOpts{csrf: true}),
		"by_ext", "watched_roots", "items", "dirs_deepest_first")
	check(h.do("POST", filesRoutes[5].path, filesRoutes[5].body, reqOpts{csrf: true}),
		"dirs", "items", "conflicts")
	check(h.do("POST", filesRoutes[routeUploadPlan].path, filesRoutes[routeUploadPlan].body, reqOpts{csrf: true}),
		"dirs", "items", "conflicts")
}

// TestFilesSentinelCodes checks every devfiles sentinel answers its status and
// code, the outermost one winning when a cause wraps another, and that an
// unclassified failure is a 500.
func TestFilesSentinelCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{devfiles.ErrUnknownRig, 404, "unknown_rig"},
		{devfiles.ErrBadPath, 400, "bad_path"},
		{devfiles.ErrTooMany, 400, "too_many"},
		{devfiles.ErrIntoItself, 400, "into_itself"},
		{devfiles.ErrSameFolder, 400, "same_folder"},
		{devfiles.ErrTypeConflict, 409, "type_conflict"},
		{devfiles.ErrNotFound, 404, "not_found"},
		{devfiles.ErrExists, 409, "exists"},
		{devfiles.ErrNoShare, 409, "no_share"},
		{devfiles.ErrLayout, 409, "layout"},
		{devfiles.ErrWatched, 409, "watched"},
		{devfiles.ErrBusy, 409, "busy"},
		{devfiles.ErrUnarchived, 409, "unarchived"},
		{devfiles.ErrIdentity, 409, "identity"},
		{devfiles.ErrHostBound, 409, "host_bound"},
		{devfiles.ErrDenied, 403, "denied"},
		{devfiles.ErrInUse, 409, "in_use"},
		{devfiles.ErrUnreachable, 503, "unreachable"},
		{devfiles.ErrClosed, 503, "files_unavailable"},
		{devfiles.ErrLengthRequired, 411, "length_required"},
		{devfiles.ErrStagingGone, 410, "staging_gone"},
		{devfiles.ErrStagingFull, 507, "staging_full"},
	}
	df := newFakeFiles()
	h := newFilesHarness(t, df)
	get := func(err error) *http.Response {
		df.mu.Lock()
		df.err = err
		df.mu.Unlock()
		return h.do("GET", filesRoutes[0].path, nil, reqOpts{})
	}
	for _, c := range cases {
		for _, err := range []error{
			&devfiles.Error{Kind: c.err, Rig: "r1", Msg: "msg " + c.code},
			fmt.Errorf("wrapped: %w", c.err), // a bare sentinel
		} {
			resp := get(err)
			if resp.StatusCode != c.status {
				resp.Body.Close()
				t.Fatalf("%v: status %d, want %d", err, resp.StatusCode, c.status)
			}
			if code := errorCode(t, resp); code != c.code {
				t.Fatalf("%v: code %q, want %q", err, code, c.code)
			}
		}
	}

	nested := &devfiles.Error{Kind: devfiles.ErrUnreachable, Msg: "Rig One is unreachable",
		Err: &devfiles.Error{Kind: devfiles.ErrIdentity, Msg: "inner"}}
	if code := errorCode(t, get(nested)); code != "unreachable" {
		t.Fatalf("nested: code %q, want the outer unreachable", code)
	}
	resp := get(errors.New("boom"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("unclassified: status %d, want 500", resp.StatusCode)
	}
}

// TestFilesEverySentinelAnswers checks every devfiles sentinel has a row in the
// API's error table that is not a 500, so one added to devfiles without a row
// fails here rather than reaching users as an unexpected failure. ErrCancelled
// writes nothing, ErrMidStream aborts the download and ErrStageBody the
// staging request; none has a row.
func TestFilesEverySentinelAnswers(t *testing.T) {
	unanswered := map[error]bool{devfiles.ErrCancelled: true, devfiles.ErrMidStream: true, devfiles.ErrStageBody: true}
	for _, kind := range devfiles.AllErrors() {
		var row *filesErrorCode
		for i := range filesErrorCodes {
			if filesErrorCodes[i].kind == kind {
				row = &filesErrorCodes[i]
			}
		}
		switch {
		case unanswered[kind]:
			if row != nil {
				t.Errorf("%v: has a row %+v, want none", kind, *row)
			}
		case row == nil:
			t.Errorf("%v: no row in filesErrorCodes", kind)
		case row.status == http.StatusInternalServerError || row.code == "":
			t.Errorf("%v: row %+v, want a non-500 status and a code", kind, *row)
		}
	}
}

// TestFilesUnarchivedCarriesCount checks the delete run's 409 unarchived body
// carries the fresh count beside the error fields.
func TestFilesUnarchivedCarriesCount(t *testing.T) {
	df := newFakeFiles()
	df.err = &devfiles.UnarchivedError{
		Err:        &devfiles.Error{Kind: devfiles.ErrUnarchived, Rig: "r1", Msg: "now 40, was 3"},
		Unarchived: 40,
	}
	h := newFilesHarness(t, df)
	resp := h.do("POST", filesRoutes[4].path, filesRoutes[4].body, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var body struct {
		Error      string `json:"error"`
		Code       string `json:"code"`
		Message    string `json:"message"`
		Unarchived *int   `json:"unarchived"`
	}
	decode(t, resp, &body)
	if body.Code != "unarchived" || body.Message != "now 40, was 3" || body.Error == "" ||
		body.Unarchived == nil || *body.Unarchived != 40 {
		t.Fatalf("body = %+v", body)
	}
}

// TestFilesDeleteRefusesNegativeAcknowledgement checks a delete whose
// acknowledged unarchived count is negative is a bad request that never reaches
// the service.
func TestFilesDeleteRefusesNegativeAcknowledgement(t *testing.T) {
	df := newFakeFiles()
	h := newFilesHarness(t, df)
	body := map[string]any{"share": "EMMC Images", "dir": "", "items": []any{}, "dirs": []string{"M42"},
		"acknowledged_unarchived": -1}
	resp := h.do("POST", "/api/rigs/r1/files/delete", body, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var got map[string]any
	decode(t, resp, &got)
	if got["code"] != "bad_request" {
		t.Fatalf("body = %v, want code bad_request", got)
	}
	if calls := df.callList(); len(calls) != 0 {
		t.Fatalf("service calls = %v, want none", calls)
	}
}

// TestFilesDownloadHeaders checks a single file's headers, a non-ASCII name
// included, a zip's, the repeated name parameters and the check preview.
func TestFilesDownloadHeaders(t *testing.T) {
	df := newFakeFiles()
	const name = "Ωmega «M42» été.fit"
	df.body = []byte("0123456789")
	df.info = &devfiles.DownloadInfo{Filename: name, Files: 1, Bytes: 10}
	h := newFilesHarness(t, df)

	resp := h.do("GET", "/api/rigs/r1/files/download?share=EMMC%20Images&dir=Preview&name="+
		strings.ReplaceAll(name, " ", "%20"), nil, reqOpts{})
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, df.body) {
		t.Fatalf("body %q, %v", got, err)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" ||
		resp.Header.Get("Content-Length") != "10" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", resp.Header)
	}
	disp, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil || disp != "attachment" || params["filename"] != name {
		t.Fatalf("Content-Disposition %q: %q %v %v", resp.Header.Get("Content-Disposition"), disp, params, err)
	}

	// Larger than net/http's response buffer, so the server cannot compute a
	// length itself and the absence of one is the handler's.
	df.body = bytes.Repeat([]byte("z"), 64<<10)
	df.info = &devfiles.DownloadInfo{Filename: "Rig One-Preview.zip", Files: 2, Bytes: 10, Zipped: true}
	resp = h.do("GET", "/api/rigs/r1/files/download?share=EMMC%20Images&dir=Preview&name=a.fit&name=b%20c", nil, reqOpts{})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/zip" || resp.ContentLength != -1 ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("zip headers %v (length %d)", resp.Header, resp.ContentLength)
	}
	if _, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); params["filename"] != "Rig One-Preview.zip" {
		t.Fatalf("zip disposition %q", resp.Header.Get("Content-Disposition"))
	}
	if df.gotRig != "r1" || df.gotShare != "EMMC Images" || df.gotDir != "Preview" ||
		!slices.Equal(df.gotNames, []string{"a.fit", "b c"}) {
		t.Fatalf("download args: rig %q share %q dir %q names %q", df.gotRig, df.gotShare, df.gotDir, df.gotNames)
	}

	resp = h.do("GET", "/api/rigs/r1/files/download?share=EMMC%20Images&dir=Preview&name=a.fit&name=b%20c&check=1", nil, reqOpts{})
	var info map[string]any
	decode(t, resp, &info)
	if info["filename"] != "Rig One-Preview.zip" || info["files"] != float64(2) || info["zipped"] != true {
		t.Fatalf("check = %v", info)
	}
}

// TestFilesDownloadAbortAfterFirstByte checks a failure after the first byte
// reaches the client as an aborted response, not a clean end, for a zip and
// for a single file.
func TestFilesDownloadAbortAfterFirstByte(t *testing.T) {
	for _, zipped := range []bool{true, false} {
		df := newFakeFiles()
		df.body = bytes.Repeat([]byte("x"), 64<<10)
		df.info = &devfiles.DownloadInfo{Filename: "f", Files: 1, Bytes: int64(len(df.body)), Zipped: zipped}
		df.midStream = true
		h := newFilesHarness(t, df)
		resp := h.do("GET", "/api/rigs/r1/files/download?share=s&dir=&name=f", nil, reqOpts{})
		_, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err == nil {
			t.Fatalf("zipped=%v: status %d, read error %v; want 200 and an aborted body", zipped, resp.StatusCode, err)
		}
	}
}

// newFilesE2E serves the handlers on the real devfiles.Service over two
// device.Fake rigs, r1 and r2, each with a Preview folder, uploads staged in
// the returned directory.
func newFilesE2E(t *testing.T) (*harness, map[string]*device.Fake, string) {
	t.Helper()
	t0 := time.Date(2026, 10, 7, 21, 0, 0, 0, time.UTC)
	const share = "EMMC Images"
	devs := map[string]*device.Fake{"10.0.0.1": device.NewFake(), "10.0.0.2": device.NewFake()}
	for _, d := range devs {
		d.AddShare(share)
	}
	devs["10.0.0.1"].AddFile(share, "Preview/M42/a.fit", []byte("frame-a"), t0)
	devs["10.0.0.2"].AddFile(share, "Preview/other.txt", []byte("x"), t0)

	status := &staticStatus{snap: model.Snapshot{Rigs: []model.RigStatus{
		{ID: "r1", Address: "10.0.0.1"}, {ID: "r2", Address: "10.0.0.2"},
	}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stagingDir := filepath.Join(t.TempDir(), "upload-staging")
	var svc *devfiles.Service
	h := newHarness(t, true, func(d *Deps) {
		cfg := *d.Config.Load()
		sources := []model.Source{{Share: share, Roots: []string{"Autorun"}}}
		cfg.Rigs = []model.RigProfile{
			{ID: "r1", Name: "Rig One", GUID: "g1", Managed: true, Sources: sources},
			{ID: "r2", Name: "Rig Two", GUID: "g2", Managed: true, Sources: sources},
		}
		d.Config.Store(&cfg)
		svc = devfiles.New(devfiles.Deps{
			Config: d.Config, Store: d.Store, Status: status,
			Prober:  guidProber{"10.0.0.1": "g1", "10.0.0.2": "g2"},
			Sites:   netsite.NewRegistry(logger),
			LocalFS: localfs.NewFake(t.TempDir()),
			NewDevice: func(ep netsite.Endpoint, _, _ string, _ time.Duration) device.Device {
				return devs[ep.IP]
			},
			StagingDir: stagingDir,
			Logger:     logger,
		})
		d.DeviceFiles = svc
	})
	t.Cleanup(svc.Close)
	if err := svc.StartStaging(); err != nil {
		t.Fatalf("StartStaging: %v", err)
	}
	h.login()
	return h, devs, stagingDir
}

// TestFilesEndToEnd drives the handlers against the real devfiles.Service on
// device.Fake rigs, so the wire shapes are proven against the real types:
// list a folder, plan a copy to another rig, run it, and find the file there.
func TestFilesEndToEnd(t *testing.T) {
	const share = "EMMC Images"
	h, devs, _ := newFilesE2E(t)

	resp := h.do("GET", "/api/rigs/r1/files?share=EMMC%20Images&dir=Preview", nil, reqOpts{})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("list: status %d", resp.StatusCode)
	}
	var listing struct {
		Share, Dir string
		Writable   bool
		Entries    []struct {
			Name string
			Dir  bool
		}
	}
	decode(t, resp, &listing)
	if listing.Share != share || listing.Dir != "Preview" || !listing.Writable ||
		len(listing.Entries) != 1 || listing.Entries[0].Name != "M42" || !listing.Entries[0].Dir {
		t.Fatalf("listing = %+v", listing)
	}

	from := map[string]any{"rig": "r1", "share": share, "dir": "Preview"}
	to := map[string]any{"rig": "r2", "share": share, "dir": "Preview"}
	resp = h.do("POST", "/api/files/copy/plan", map[string]any{
		"from": map[string]any{"rig": "r1", "share": share, "dir": "Preview", "names": []string{"M42"}},
		"to":   to, "mirror": false,
	}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("copy plan: status %d", resp.StatusCode)
	}
	var plan struct {
		Files     int      `json:"files"`
		Dirs      []string `json:"dirs"`
		Conflicts []any    `json:"conflicts"`
		Items     []struct {
			Rel  string `json:"rel"`
			Size int64  `json:"size"`
		} `json:"items"`
	}
	decode(t, resp, &plan)
	if plan.Files != 1 || !slices.Equal(plan.Dirs, []string{"M42"}) || plan.Conflicts == nil ||
		len(plan.Items) != 1 || plan.Items[0].Rel != "M42/a.fit" {
		t.Fatalf("plan = %+v", plan)
	}

	resp = h.do("POST", "/api/files/copy", map[string]any{
		"from": from, "to": to, "dirs": plan.Dirs, "items": plan.Items, "overwrite": []any{},
	}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("copy: status %d", resp.StatusCode)
	}
	lines := readLines(t, resp)
	last := lines[len(lines)-1]
	if last["type"] != "summary" || last["done"] != float64(1) || last["failed"] != float64(0) {
		t.Fatalf("copy lines = %v", lines)
	}

	sess, err := devs["10.0.0.2"].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sess.Mount(share)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := sh.OpenReader("Preview/M42/a.fit")
	if err != nil {
		t.Fatalf("copied file missing on r2: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "frame-a" {
		t.Fatalf("copied content %q", got)
	}
}

// staticStatus is a fixed hub snapshot.
type staticStatus struct{ snap model.Snapshot }

func (s *staticStatus) Snapshot() model.Snapshot { return s.snap }

// guidProber answers each IP's identity probe with its GUID.
type guidProber map[string]string

func (p guidProber) ProbeGUID(_ context.Context, ep netsite.Endpoint) (string, string, bool) {
	g, ok := p[ep.IP]
	return g, "", ok
}

// stageRequest is a staging request for name carrying body, with length as
// its Content-Length (-1: none, so the client sends it chunked).
func (h *harness) stageRequest(ctx context.Context, name string, body io.Reader, length int64) (*http.Response, error) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST",
		h.baseURL+"/api/files/upload/stage?name="+url.QueryEscape(name)+"&mtime_ms=1760000000000", body)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", h.baseURL)
	req.Header.Set(csrfHeader, h.csrf())
	return h.client.Do(req)
}

// secondSession is h with a client of its own, logged in separately: another
// login, so another session.
func (h *harness) secondSession() *harness {
	h.t.Helper()
	jar, _ := cookiejar.New(nil)
	h2 := *h
	h2.client = &http.Client{Jar: jar}
	h2.login()
	if h2.csrf() == h.csrf() {
		h.t.Fatal("two logins share an XSRF token")
	}
	return &h2
}

// stagedFiles lists the staging directory's entries.
func stagedFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging directory: %v", err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// waitStaged polls the staging directory until it holds want entries.
func waitStaged(t *testing.T, dir string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(stagedFiles(t, dir)) != want {
		if time.Now().After(deadline) {
			t.Fatalf("staging directory holds %v, want %d entries", stagedFiles(t, dir), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFilesUploadStageRequiresLength checks a staging request without a
// Content-Length answers 411 length_required and stages nothing, whether its
// body is chunked or the header is simply absent (which net/http would
// otherwise read as an empty file).
func TestFilesUploadStageRequiresLength(t *testing.T) {
	h, _, stagingDir := newFilesE2E(t)

	resp, err := h.stageRequest(context.Background(), "ref.fit", io.MultiReader(strings.NewReader("abc")), -1)
	if err != nil {
		t.Fatalf("chunked staging request: %v", err)
	}
	if resp.StatusCode != http.StatusLengthRequired {
		resp.Body.Close()
		t.Fatalf("chunked: status %d, want 411", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "length_required" {
		t.Fatalf("chunked: code %q, want length_required", code)
	}

	// A raw HTTP/1.1 request with no Content-Length and no body at all.
	u, _ := url.Parse(h.baseURL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	var cookies []string
	for _, c := range h.client.Jar.Cookies(u) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	fmt.Fprintf(conn, "POST /api/files/upload/stage?name=ref.fit HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\n"+
		"Cookie: %s\r\n%s: %s\r\nConnection: close\r\n\r\n",
		u.Host, h.baseURL, strings.Join(cookies, "; "), csrfHeader, h.csrf())
	resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read raw response: %v", err)
	}
	if resp.StatusCode != http.StatusLengthRequired {
		resp.Body.Close()
		t.Fatalf("no header: status %d, want 411", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "length_required" {
		t.Fatalf("no header: code %q, want length_required", code)
	}
	if got := stagedFiles(t, stagingDir); len(got) != 0 {
		t.Fatalf("staging directory holds %v after refusals", got)
	}
}

// countingBody yields size bytes and counts how many were read from it.
type countingBody struct {
	size int64
	read atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	left := b.size - b.read.Load()
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	for i := range p {
		p[i] = 'x'
	}
	b.read.Add(int64(len(p)))
	return len(p), nil
}

// TestFilesUploadStageDrainsBeforeRefusal checks a staging refusal reaches
// the client as JSON only after the server consumed every body byte. The body
// is far larger than net/http's own short discard, so a handler that did not
// drain would close the connection with most of it unsent.
func TestFilesUploadStageDrainsBeforeRefusal(t *testing.T) {
	const size = 16 << 20
	cases := []struct {
		name   string
		err    error
		query  string
		status int
		code   string
	}{
		{"service refusal", &devfiles.Error{Kind: devfiles.ErrStagingFull, Msg: "Commander is already holding 4 uploads in progress."},
			"name=ref.fit", http.StatusInsufficientStorage, "staging_full"},
		{"bad name", &devfiles.Error{Kind: devfiles.ErrBadPath, Msg: "Invalid file name"},
			"name=ref.", http.StatusBadRequest, "bad_path"},
		{"bad mtime", nil, "name=ref.fit&mtime_ms=yesterday", http.StatusBadRequest, "bad_request"},
		{"no service", nil, "name=ref.fit", http.StatusServiceUnavailable, "files_unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h *harness
			if c.code == "files_unavailable" {
				h = newHarness(t, true)
				h.login()
			} else {
				df := newFakeFiles()
				df.err = c.err
				h = newFilesHarness(t, df)
			}
			body := &countingBody{size: size}
			req, _ := http.NewRequest("POST", h.baseURL+"/api/files/upload/stage?"+c.query, body)
			req.ContentLength = size
			req.Header.Set("Origin", h.baseURL)
			req.Header.Set(csrfHeader, h.csrf())
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("staging request: %v (read %d of %d body bytes)", err, body.read.Load(), size)
			}
			if resp.StatusCode != c.status {
				resp.Body.Close()
				t.Fatalf("status %d, want %d", resp.StatusCode, c.status)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type %q, want JSON", ct)
			}
			if code := errorCode(t, resp); code != c.code {
				t.Fatalf("code %q, want %q", code, c.code)
			}
			if n := body.read.Load(); n != size {
				t.Fatalf("server consumed %d of %d body bytes before refusing", n, size)
			}
		})
	}
}

// TestFilesUploadStageAbortsBrokenBody checks a staging body the service
// reports broken gets no answer: the request is aborted, so the browser sees
// a failed send rather than a response to a body it never finished.
func TestFilesUploadStageAbortsBrokenBody(t *testing.T) {
	df := newFakeFiles()
	df.stageBroken = true
	h := newFilesHarness(t, df)
	resp, err := h.stageRequest(context.Background(), "ref.fit", strings.NewReader("abcdef"), 6)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("a broken staging body was answered %d, want an aborted request", resp.StatusCode)
	}
}

// TestFilesUploadClientAbortLeavesNothingStaged checks a client that goes
// away mid-body leaves no staged file behind, on the real service.
func TestFilesUploadClientAbortLeavesNothingStaged(t *testing.T) {
	h, _, stagingDir := newFilesE2E(t)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		resp, err := h.stageRequest(context.Background(), "ref.fit", pr, 1<<20)
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("answered %d", resp.StatusCode)
		} else {
			err = nil
		}
		done <- err
	}()
	if _, err := pw.Write(bytes.Repeat([]byte("y"), 64<<10)); err != nil {
		t.Fatalf("write first half: %v", err)
	}
	waitStaged(t, stagingDir, 1) // the receive has begun
	pw.CloseWithError(errors.New("tab closed"))
	if err := <-done; err != nil {
		t.Fatalf("aborted staging request: %v", err)
	}
	waitStaged(t, stagingDir, 0)
}

// TestFilesUploadOwnerKey checks every upload call carries the session's
// owner key: one key per login, the same on every call of that login, derived
// from the session's XSRF claim and never the token itself.
func TestFilesUploadOwnerKey(t *testing.T) {
	df := newFakeFiles()
	df.uploadPlan = &devfiles.UploadPlan{}
	h := newFilesHarness(t, df)
	for _, i := range []int{routeStage, routeUploadPlan, routeUpload, routeUploadClose} {
		rt := filesRoutes[i]
		h.do(rt.method, rt.path, rt.body, reqOpts{csrf: true}).Body.Close()
	}
	h2 := h.secondSession()
	h2.do("POST", filesRoutes[routeUploadPlan].path, filesRoutes[routeUploadPlan].body, reqOpts{csrf: true}).Body.Close()

	owners := df.owners()
	if len(owners) != 5 {
		t.Fatalf("owners %v, want 5 calls", owners)
	}
	sum := sha256.Sum256([]byte(h.csrf()))
	want := hex.EncodeToString(sum[:])
	for _, o := range owners[:4] {
		if o != want {
			t.Fatalf("owner %q, want the SHA-256 of the session's XSRF token %q", o, want)
		}
	}
	if owners[4] == want || owners[4] == "" || owners[4] == h2.csrf() {
		t.Fatalf("second session's owner %q: want its own key, not the first's or its raw token", owners[4])
	}
}

// TestFilesUploadSessionsIsolated checks, on the real service, that a file one
// session staged answers staging_gone to another session's plan, run and
// discard, and stays usable by its own.
func TestFilesUploadSessionsIsolated(t *testing.T) {
	h, _, stagingDir := newFilesE2E(t)
	resp, err := h.stageRequest(context.Background(), "ref.fit", strings.NewReader("reference"), 9)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stage: %v %v", resp, err)
	}
	var staged struct{ ID string }
	decode(t, resp, &staged)
	to := map[string]any{"rig": "r2", "share": "EMMC Images", "dir": "Preview"}

	other := h.secondSession()
	for _, c := range []struct{ path string }{
		{"/api/files/upload/plan"}, {"/api/files/upload"}, {"/api/files/upload/discard"},
	} {
		body := map[string]any{"id": staged.ID, "to": to}
		if c.path == "/api/files/upload/discard" {
			body = map[string]any{"id": staged.ID}
		}
		resp := other.do("POST", c.path, body, reqOpts{csrf: true})
		if resp.StatusCode != http.StatusGone {
			resp.Body.Close()
			t.Fatalf("%s from another session: status %d, want 410", c.path, resp.StatusCode)
		}
		if code := errorCode(t, resp); code != "staging_gone" {
			t.Fatalf("%s from another session: code %q", c.path, code)
		}
	}
	if got := stagedFiles(t, stagingDir); !slices.Equal(got, []string{staged.ID}) {
		t.Fatalf("staging directory %v, want the staged file untouched", got)
	}
	resp = h.do("POST", "/api/files/upload/plan", map[string]any{"id": staged.ID, "to": to}, reqOpts{csrf: true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("own plan after the other session's attempts: status %d", resp.StatusCode)
	}
}

// TestFilesUploadEndToEnd stages, plans, runs and discards an upload through
// the real service: the file lands on the device byte for byte, the staged
// file outlives the run for Retry, and the discard removes it.
func TestFilesUploadEndToEnd(t *testing.T) {
	h, devs, stagingDir := newFilesE2E(t)
	content := bytes.Repeat([]byte("ref-frame "), 100_000) // 1 MB, several chunks
	resp, err := h.stageRequest(context.Background(), "ref.fit", bytes.NewReader(content), int64(len(content)))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stage: %v %v", resp, err)
	}
	var staged struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	decode(t, resp, &staged)
	if len(staged.ID) != 32 || staged.Size != int64(len(content)) {
		t.Fatalf("staged = %+v", staged)
	}

	to := map[string]any{"rig": "r2", "share": "EMMC Images", "dir": "Preview"}
	resp = h.do("POST", "/api/files/upload/plan", map[string]any{"id": staged.ID, "to": to}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("plan: status %d", resp.StatusCode)
	}
	var plan map[string]any
	decode(t, resp, &plan)
	items, _ := plan["items"].([]any)
	if plan["files"] != float64(1) || len(items) != 1 || items[0].(map[string]any)["rel"] != "ref.fit" {
		t.Fatalf("plan = %v", plan)
	}
	for _, k := range []string{"dirs", "conflicts"} {
		if v, ok := plan[k].([]any); !ok || len(v) != 0 {
			t.Fatalf("plan %s = %#v, want []", k, plan[k])
		}
	}

	resp = h.do("POST", "/api/files/upload", map[string]any{"id": staged.ID, "to": to}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		resp.Body.Close()
		t.Fatalf("run: status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := readLines(t, resp)
	last := lines[len(lines)-1]
	if lines[0]["type"] != "progress" || last["type"] != "summary" || last["done"] != float64(1) || last["failed"] != float64(0) {
		t.Fatalf("run lines = %v", lines)
	}
	sess, err := devs["10.0.0.2"].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sess.Mount("EMMC Images")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := sh.OpenReader("Preview/ref.fit")
	if err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("uploaded %d bytes, want %d identical", len(got), len(content))
	}
	if fi, err := sh.Stat("Preview/ref.fit"); err != nil || !fi.Mtime.Equal(time.UnixMilli(1760000000000)) {
		t.Fatalf("uploaded mtime: %v, %v", fi, err)
	}
	if files := stagedFiles(t, stagingDir); !slices.Equal(files, []string{staged.ID}) {
		t.Fatalf("staging directory after the run: %v, want the staged file kept for Retry", files)
	}

	resp = h.do("POST", "/api/files/upload/discard", map[string]any{"id": staged.ID}, reqOpts{csrf: true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("discard: status %d", resp.StatusCode)
	}
	waitStaged(t, stagingDir, 0)
	resp = h.do("POST", "/api/files/upload/discard", map[string]any{"id": staged.ID}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusGone {
		resp.Body.Close()
		t.Fatalf("second discard: status %d, want 410", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "staging_gone" {
		t.Fatalf("second discard: code %q", code)
	}
	resp = h.do("POST", "/api/files/upload", map[string]any{"id": staged.ID, "to": to}, reqOpts{csrf: true})
	if resp.StatusCode != http.StatusGone || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		resp.Body.Close()
		t.Fatalf("run after discard: status %d, want a 410 JSON refusal", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestFilesRenameEndToEnd renames a file through the real service and checks
// a refusal answers its code: renaming a folder to a watched root's name.
func TestFilesRenameEndToEnd(t *testing.T) {
	h, devs, _ := newFilesE2E(t)
	resp := h.do("POST", "/api/rigs/r2/files/rename",
		map[string]any{"share": "EMMC Images", "dir": "Preview", "name": "other.txt", "new_name": "notes.txt"},
		reqOpts{csrf: true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename: status %d", resp.StatusCode)
	}
	sess, err := devs["10.0.0.2"].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sess.Mount("EMMC Images")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Stat("Preview/notes.txt"); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}
	if _, err := sh.Stat("Preview/other.txt"); err == nil {
		t.Fatal("the old name still exists")
	}

	resp = h.do("POST", "/api/rigs/r2/files/rename",
		map[string]any{"share": "EMMC Images", "dir": "", "name": "Preview", "new_name": "Autorun"},
		reqOpts{csrf: true})
	if resp.StatusCode != http.StatusConflict {
		resp.Body.Close()
		t.Fatalf("rename onto a watched root: status %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "watched" {
		t.Fatalf("rename onto a watched root: code %q, want watched", code)
	}
}
