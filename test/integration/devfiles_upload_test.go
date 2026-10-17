//go:build integration

package integration

import (
	"bufio"
	"bytes"
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
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/web"
)

// Upload against real Samba: a file staged on Commander's disk is planned and
// written onto rig "b" through the copy sink, over the real exclusive create,
// write deadline, never-replacing rename and chtimes. Each test works in its
// own scratch folder at the share root.

const uploadOwner = "integration-owner"

// uploadMtime is the PC-side time every staged file carries.
var uploadMtime = time.Date(2025, 3, 4, 5, 6, 8, 0, time.UTC)

// devfilesStage stages data under name for owner and returns the staging id.
func devfilesStage(t *testing.T, svc *devfiles.Service, owner, name string, data []byte) string {
	t.Helper()
	got, err := svc.UploadStage(context.Background(), owner, name, int64(len(data)), uploadMtime, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("UploadStage(%q): %v", name, err)
	}
	if got.Size != int64(len(data)) || len(got.ID) != 32 {
		t.Fatalf("UploadStage(%q) = %+v", name, got)
	}
	return got.ID
}

// devfilesUpload runs an upload that is expected to stream, and returns its
// one item line and its summary.
func devfilesUpload(t *testing.T, svc *devfiles.Service, req devfiles.UploadRequest) (devfiles.ItemLine, devfiles.SummaryLine) {
	t.Helper()
	var lines []any
	err := svc.Upload(context.Background(), uploadOwner, req, func(line any) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	items := itemLinesOf(lines)
	sum, ok := lines[len(lines)-1].(devfiles.SummaryLine)
	switch {
	case !ok || len(items) > 1:
		t.Fatalf("lines = %#v, want at most one item and a summary", lines)
	case len(items) == 1:
		return items[0], sum
	case sum.Done != 1:
		t.Fatalf("lines = %#v: no item line, yet nothing done", lines)
	}
	// A plain success has no item line of its own: copy reports it in the
	// summary alone.
	return devfiles.ItemLine{Outcome: "copied"}, sum
}

func itemLinesOf(lines []any) []devfiles.ItemLine {
	var out []devfiles.ItemLine
	for _, l := range lines {
		if it, ok := l.(devfiles.ItemLine); ok {
			out = append(out, it)
		}
	}
	return out
}

// stagedHeld reports whether the staging directory still holds id's file.
func stagedHeld(t *testing.T, dir, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat staged file: %v", err)
	}
	return err == nil
}

// stagingEntries lists the staging directory.
func stagingEntries(t *testing.T, dir string) []string {
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

// listNames is dir's listing as name → size (-1 for a folder).
func listNames(t *testing.T, sh device.Share, dir string) map[string]int64 {
	t.Helper()
	entries, err := sh.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		if e.IsDir {
			out[e.Name] = -1
		} else {
			out[e.Name] = e.Size
		}
	}
	return out
}

func sameListing(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestDevfilesServiceUpload plans and writes staged files onto real Samba:
// several write chunks byte for byte with the PC's mtime, a destination
// conflict left alone unless the overwrite binding still matches, a folder at
// the name, a name at the upload cap, and the staged file kept through every
// run until the discard.
func TestDevfilesServiceUpload(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	env := devfilesServiceWith(t, devfilesOpts{})
	svc, ctx := env.svc, context.Background()
	to := devfiles.CopyPlace{Rig: "b", Share: shareName, Dir: dir}

	t.Run("Written", func(t *testing.T) {
		big := patternedBytes(3<<20+4321, 7) // several of the sink's 512 KiB chunks
		id := devfilesStage(t, svc, uploadOwner, "big.fit", big)

		plan, err := svc.UploadPlan(ctx, uploadOwner, devfiles.UploadPlanRequest{ID: id, To: to})
		if err != nil {
			t.Fatalf("UploadPlan: %v", err)
		}
		if plan.Files != 1 || len(plan.Conflicts) != 0 || len(plan.Dirs) != 0 ||
			len(plan.Items) != 1 || plan.Items[0].Rel != "big.fit" || plan.Items[0].Size != int64(len(big)) {
			t.Fatalf("plan = %+v", plan)
		}
		if plan.FreeBytes == nil || *plan.FreeBytes <= 0 {
			t.Errorf("plan free bytes = %v, want the share's free space", plan.FreeBytes)
		}

		it, sum := devfilesUpload(t, svc, devfiles.UploadRequest{ID: id, To: to})
		if it.Outcome != "copied" || sum.Done != 1 || sum.Skipped+sum.Failed != 0 {
			t.Fatalf("item %+v, summary %+v", it, sum)
		}
		check := devfilesMount(t)
		if got := devfilesRead(t, check, path.Join(dir, "big.fit")); !bytes.Equal(got, big) {
			t.Errorf("big.fit differs: %d bytes, want %d", len(got), len(big))
		}
		fi, err := check.Stat(path.Join(dir, "big.fit"))
		if err != nil {
			t.Fatal(err)
		}
		if d := fi.Mtime.Sub(uploadMtime); d < -2*time.Second || d > 2*time.Second {
			t.Errorf("mtime = %v, want the PC's %v", fi.Mtime, uploadMtime)
		}
		devfilesNoTemps(t, check, dir)
		if !stagedHeld(t, env.staging, id) {
			t.Fatal("a successful run removed the staged file")
		}

		if err := svc.UploadDiscard(uploadOwner, id); err != nil {
			t.Fatalf("UploadDiscard: %v", err)
		}
		if stagedHeld(t, env.staging, id) {
			t.Fatal("the discard left the staged file")
		}
		if _, err := svc.UploadPlan(ctx, uploadOwner, devfiles.UploadPlanRequest{ID: id, To: to}); !errors.Is(err, devfiles.ErrStagingGone) {
			t.Fatalf("plan after discard err = %v, want staging_gone", err)
		}
	})

	t.Run("Conflict", func(t *testing.T) {
		theirs := []byte("already on the rig")
		devfilesWrite(t, sh, path.Join(dir, "ref.fit"), theirs, 1<<20)
		ours := patternedBytes(1<<20+99, 13)
		id := devfilesStage(t, svc, uploadOwner, "ref.fit", ours)

		plan, err := svc.UploadPlan(ctx, uploadOwner, devfiles.UploadPlanRequest{ID: id, To: to})
		if err != nil {
			t.Fatalf("UploadPlan: %v", err)
		}
		if len(plan.Conflicts) != 1 {
			t.Fatalf("plan conflicts = %+v, want one", plan.Conflicts)
		}
		c := plan.Conflicts[0]
		if c.Rel != "ref.fit" || c.Kind != "file" || c.DstSize != int64(len(theirs)) ||
			c.SrcSize != int64(len(ours)) || c.SrcMtime != uploadMtime.Unix() {
			t.Fatalf("conflict = %+v", c)
		}
		unchanged := func(what string) {
			t.Helper()
			if got := devfilesRead(t, devfilesMount(t), path.Join(dir, "ref.fit")); !bytes.Equal(got, theirs) {
				t.Fatalf("%s: the destination was replaced (%d bytes)", what, len(got))
			}
		}

		// Unbound: the never-replacing rename meets the destination.
		it, _ := devfilesUpload(t, svc, devfiles.UploadRequest{ID: id, To: to})
		if it.Outcome != "skipped" || it.Code != "appeared" || it.DstSize == nil || *it.DstSize != int64(len(theirs)) {
			t.Fatalf("unbound run: item = %+v, want skipped appeared", it)
		}
		unchanged("unbound run")

		// Bound to a size the destination no longer has.
		stale := c.DstSize + 1
		it, _ = devfilesUpload(t, svc, devfiles.UploadRequest{ID: id, To: to, DstSize: &stale, DstMtime: &c.DstMtime})
		if it.Outcome != "skipped" || it.Code != "changed" {
			t.Fatalf("stale binding: item = %+v, want skipped changed", it)
		}
		unchanged("stale binding")

		// Bound as the plan saw it: replaced, from the same staged file.
		it, _ = devfilesUpload(t, svc, devfiles.UploadRequest{ID: id, To: to, DstSize: &c.DstSize, DstMtime: &c.DstMtime})
		if it.Outcome != "copied" {
			t.Fatalf("bound run: item = %+v, want copied", it)
		}
		check := devfilesMount(t)
		if got := devfilesRead(t, check, path.Join(dir, "ref.fit")); !bytes.Equal(got, ours) {
			t.Fatalf("the bound overwrite did not land: %d bytes, want %d", len(got), len(ours))
		}
		devfilesNoTemps(t, check, dir)
		if !stagedHeld(t, env.staging, id) {
			t.Fatal("a run removed the staged file")
		}
	})

	t.Run("FolderAtName", func(t *testing.T) {
		if err := sh.Mkdir(path.Join(dir, "folder.fit")); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		id := devfilesStage(t, svc, uploadOwner, "folder.fit", []byte("a file"))
		plan, err := svc.UploadPlan(ctx, uploadOwner, devfiles.UploadPlanRequest{ID: id, To: to})
		if err != nil {
			t.Fatalf("UploadPlan: %v", err)
		}
		if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != "type" {
			t.Fatalf("plan conflicts = %+v, want one type conflict", plan.Conflicts)
		}
		it, _ := devfilesUpload(t, svc, devfiles.UploadRequest{ID: id, To: to})
		if it.Outcome != "skipped" || it.Code != "type_conflict" {
			t.Fatalf("item = %+v, want skipped type_conflict", it)
		}
		check := devfilesMount(t)
		if fi, err := check.Stat(path.Join(dir, "folder.fit")); err != nil || !fi.IsDir {
			t.Fatalf("folder.fit: Stat = %+v, %v; want the folder untouched", fi, err)
		}
		devfilesNoTemps(t, check, dir)
	})

	// A name at the upload cap leaves its temp name within the device's 255
	// bytes, so the real server takes both.
	t.Run("NameAtCap", func(t *testing.T) {
		name := strings.Repeat("n", 224-len(".fit")) + ".fit"
		id := devfilesStage(t, svc, uploadOwner, name, []byte("long name"))
		it, _ := devfilesUpload(t, svc, devfiles.UploadRequest{ID: id, To: to})
		if it.Outcome != "copied" {
			t.Fatalf("item = %+v, want copied", it)
		}
		check := devfilesMount(t)
		if got := devfilesRead(t, check, path.Join(dir, name)); string(got) != "long name" {
			t.Fatalf("content = %q", got)
		}
		devfilesNoTemps(t, check, dir)
	})

	t.Run("OtherSession", func(t *testing.T) {
		id := devfilesStage(t, svc, uploadOwner, "mine.fit", []byte("mine"))
		if _, err := svc.UploadPlan(ctx, "someone-else", devfiles.UploadPlanRequest{ID: id, To: to}); !errors.Is(err, devfiles.ErrStagingGone) {
			t.Fatalf("plan by another session err = %v, want staging_gone", err)
		}
		err := svc.Upload(ctx, "someone-else", devfiles.UploadRequest{ID: id, To: to}, func(any) error {
			t.Fatal("a run by another session emitted a line")
			return nil
		})
		if !errors.Is(err, devfiles.ErrStagingGone) {
			t.Fatalf("run by another session err = %v, want staging_gone", err)
		}
		if err := svc.UploadDiscard("someone-else", id); !errors.Is(err, devfiles.ErrStagingGone) {
			t.Fatalf("discard by another session err = %v, want staging_gone", err)
		}
		if !stagedHeld(t, env.staging, id) {
			t.Fatal("another session's requests removed the staged file")
		}
		if _, err := devfilesMount(t).Stat(path.Join(dir, "mine.fit")); !device.IsNotFound(err) {
			t.Fatalf("mine.fit reached the device: Stat err = %v", err)
		}
	})
}

// TestDevfilesUploadRefusals checks the name rules refuse a staging request
// before anything is staged, and a file that would stand at a reserved path
// is refused by the plan and the run with nothing written to the device.
func TestDevfilesUploadRefusals(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	env := devfilesServiceWith(t, devfilesOpts{})
	svc, ctx := env.svc, context.Background()

	for _, name := range []string{
		"a:b.fit", "a*.fit", "dark.", "dark ", " dark", model.MediaMarkerName,
		strings.Repeat("n", 225-len(".fit")) + ".fit",
	} {
		_, err := svc.UploadStage(ctx, uploadOwner, name, 3, uploadMtime, strings.NewReader("abc"))
		if !errors.Is(err, devfiles.ErrBadPath) {
			t.Errorf("UploadStage(%q) err = %v, want bad_path", name, err)
		}
	}
	if got := stagingEntries(t, env.staging); len(got) != 0 {
		t.Fatalf("refused staging requests left %v", got)
	}

	// Configured roots under the scratch folder, one of them turned off: a
	// file named like either, or like a folder holding one, is refused.
	cfg := *env.cfg.Load()
	sources := []model.Source{
		{Share: shareName, Roots: []string{path.Join(dir, "Autorun")}},
		{Share: shareName, Roots: []string{path.Join(dir, "Sub", "Deep")}},
	}
	cfg.Rigs = append([]model.RigProfile(nil), cfg.Rigs...)
	for i := range cfg.Rigs {
		cfg.Rigs[i].Sources = sources
	}
	cfg.Rigs[1].Sources = []model.Source{sources[0], {Share: shareName, Roots: sources[1].Roots, Disabled: true}}
	env.cfg.Store(&cfg)

	to := devfiles.CopyPlace{Rig: "b", Share: shareName, Dir: dir}
	before := listNames(t, sh, dir)
	for _, name := range []string{"Autorun", "autorun", "Sub"} {
		id := devfilesStage(t, svc, uploadOwner, name, []byte("not a folder"))
		_, err := svc.UploadPlan(ctx, uploadOwner, devfiles.UploadPlanRequest{ID: id, To: to})
		if !errors.Is(err, devfiles.ErrWatched) || !strings.Contains(err.Error(), "Rename "+name+" on your computer") {
			t.Errorf("plan of %q err = %v, want watched with upload's fix", name, err)
		}
		err = svc.Upload(ctx, uploadOwner, devfiles.UploadRequest{ID: id, To: to}, func(any) error {
			t.Errorf("a refused run of %q emitted a line", name)
			return nil
		})
		if !errors.Is(err, devfiles.ErrWatched) {
			t.Errorf("run of %q err = %v, want watched", name, err)
		}
		if !stagedHeld(t, env.staging, id) {
			t.Errorf("a refusal of %q removed the staged file", name)
		}
		if err := svc.UploadDiscard(uploadOwner, id); err != nil {
			t.Fatalf("UploadDiscard: %v", err)
		}
	}
	if after := listNames(t, devfilesMount(t), dir); !sameListing(before, after) {
		t.Fatalf("refused uploads changed the folder: %v → %v", before, after)
	}

	// A folder that vanished since the plan is the pre-write check's
	// not_found, answered before any line.
	id := devfilesStage(t, svc, uploadOwner, "late.fit", []byte("late"))
	gone := devfiles.CopyPlace{Rig: "b", Share: shareName, Dir: path.Join(dir, "Gone")}
	err := svc.Upload(ctx, uploadOwner, devfiles.UploadRequest{ID: id, To: gone}, func(any) error {
		t.Error("a run into a missing folder emitted a line")
		return nil
	})
	if !errors.Is(err, devfiles.ErrNotFound) {
		t.Fatalf("run into a missing folder err = %v, want not_found", err)
	}
	if !stagedHeld(t, env.staging, id) {
		t.Fatal("the not_found refusal removed the staged file")
	}
}

// TestDevfilesServiceUploadRace: a destination another writer creates between
// the temp write and the commit survives byte-identical, the item is skipped
// as appeared, and the temp is removed.
func TestDevfilesServiceUploadRace(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	racer := devfilesMount(t)
	theirs := []byte("written by another session")
	raced := 0
	env := devfilesServiceWith(t, devfilesOpts{wrap: func(d device.Device) device.Device {
		return &raceDevice{Device: d, race: func(newPath string) {
			raced++
			devfilesWrite(t, racer, newPath, theirs, 1<<20)
		}}
	}})
	data := bytes.Repeat([]byte("uploaded "), 200_000)
	id := devfilesStage(t, env.svc, uploadOwner, "a.fit", data)
	it, _ := devfilesUpload(t, env.svc, devfiles.UploadRequest{ID: id,
		To: devfiles.CopyPlace{Rig: "b", Share: shareName, Dir: dir}})
	if raced != 1 {
		t.Fatalf("raced %d times, want 1", raced)
	}
	if it.Outcome != "skipped" || it.Code != "appeared" || it.DstSize == nil || *it.DstSize != int64(len(theirs)) {
		t.Fatalf("item = %+v, want skipped appeared with the racer's size", it)
	}
	check := devfilesMount(t)
	if got := devfilesRead(t, check, path.Join(dir, "a.fit")); !bytes.Equal(got, theirs) {
		t.Errorf("destination replaced: %d bytes", len(got))
	}
	devfilesNoTemps(t, check, dir)
	if !stagedHeld(t, env.staging, id) {
		t.Fatal("the run removed the staged file")
	}
}

func patternedBytes(n, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*seed + i>>12)
	}
	return b
}

// writeHook wraps a device so that every write to a file it creates calls
// onWrite with the file's path first.
type writeHook struct {
	device.Device
	onWrite func(p string)
}

func (d writeHook) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	if err != nil {
		return nil, err
	}
	return writeHookSession{Session: s, onWrite: d.onWrite}, nil
}

type writeHookSession struct {
	device.Session
	onWrite func(p string)
}

func (s writeHookSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return writeHookShare{Share: sh, onWrite: s.onWrite}, nil
}

type writeHookShare struct {
	device.Share
	onWrite func(p string)
}

func (s writeHookShare) CreateNew(p string) (io.WriteCloser, error) {
	w, err := s.Share.CreateNew(p)
	if err != nil {
		return nil, err
	}
	return writeHookWriter{WriteCloser: w, p: p, onWrite: s.onWrite}, nil
}

type writeHookWriter struct {
	io.WriteCloser
	p       string
	onWrite func(p string)
}

func (w writeHookWriter) Write(b []byte) (int, error) {
	w.onWrite(w.p)
	return w.WriteCloser.Write(b)
}

const uploadHTTPPassword = "integration-password"

// uploadClient is one signed-in browser session against the real web server.
type uploadClient struct {
	t      *testing.T
	base   string
	client *http.Client
}

func (c *uploadClient) csrf() string {
	u, _ := url.Parse(c.base)
	for _, k := range c.client.Jar.Cookies(u) {
		if k.Name == "XSRF-TOKEN" {
			return k.Value
		}
	}
	return ""
}

// post sends a write request as the SPA does: Origin, and the XSRF cookie
// echoed as the CSRF header.
func (c *uploadClient) post(ctx context.Context, route string, body io.Reader, length int64, contentType string) (*http.Response, error) {
	c.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+route, body)
	if err != nil {
		c.t.Fatalf("NewRequest: %v", err)
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", c.base)
	req.Header.Set("X-CSRF-Token", c.csrf())
	return c.client.Do(req)
}

func (c *uploadClient) postJSON(ctx context.Context, route string, v any) (*http.Response, error) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	return c.post(ctx, route, bytes.NewReader(b), int64(len(b)), "application/json")
}

// newUploadServer serves the real web server, on the real service over the
// container, and signs one browser session in.
func newUploadServer(t *testing.T, wrap func(device.Device) device.Device) (*devfilesEnv, *uploadClient) {
	t.Helper()
	base := config.Defaults()
	base.Web.BcryptCost = bcrypt.MinCost
	hash, err := bcrypt.GenerateFromPassword([]byte(uploadHTTPPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	base.PasswordHash = string(hash)
	base.SessionSecret = "integration-session-secret-0123456789"
	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	env := devfilesServiceWith(t, devfilesOpts{base: base, wrap: wrap, logger: logger})
	srv := web.New(web.Deps{
		Config:      env.cfg,
		Supervisor:  setupSupervisor{},
		DeviceFiles: env.svc,
		Logger:      logger,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	c := &uploadClient{t: t, base: ts.URL, client: &http.Client{Jar: jar}}
	b := []byte(`{"password":"` + uploadHTTPPassword + `"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/session", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || c.csrf() == "" {
		t.Fatalf("login: status %d, XSRF cookie %q", resp.StatusCode, c.csrf())
	}
	return env, c
}

// waitFor polls cond every 20 ms until it holds, failing after d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDevfilesUploadHTTP drives the upload routes on the real web server and
// the real service: a browser that breaks off mid-send leaves nothing staged
// and touches no device, and one that breaks off mid-run leaves no temp and
// no final file on the device and the staged file in place, which a second
// run then writes without the file being sent again.
func TestDevfilesUploadHTTP(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)

	// While slow is set, every write to the device waits, so a cancel lands
	// mid-run; the first write closes writing.
	var slow atomic.Bool
	writing := make(chan struct{})
	var once sync.Once
	env, c := newUploadServer(t, func(d device.Device) device.Device {
		return writeHook{Device: d, onWrite: func(string) {
			once.Do(func() { close(writing) })
			if slow.Load() {
				time.Sleep(40 * time.Millisecond)
			}
		}}
	})
	stageRoute := func(name string) string {
		return "/api/files/upload/stage?name=" + url.QueryEscape(name) + "&mtime_ms=" +
			strconv.FormatInt(uploadMtime.UnixMilli(), 10)
	}
	to := map[string]any{"rig": "b", "share": shareName, "dir": dir}

	t.Run("AbortMidSend", func(t *testing.T) {
		before := listNames(t, sh, dir)
		ctx, cancel := context.WithCancel(context.Background())
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			resp, err := c.post(ctx, stageRoute("sent.fit"), pr, 4<<20, "application/octet-stream")
			if err == nil {
				resp.Body.Close()
				err = errors.New("the staging request was answered: " + resp.Status)
			}
			done <- err
		}()
		if _, err := pw.Write(make([]byte, 1<<20)); err != nil {
			t.Fatalf("write the first MiB: %v", err)
		}
		// The receive has begun once its file exists in the staging directory.
		waitFor(t, 5*time.Second, "the staging receive to start", func() bool {
			return len(stagingEntries(t, env.staging)) == 1
		})
		cancel()
		pw.CloseWithError(context.Canceled)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("the aborted staging request ended with %v", err)
		}
		waitFor(t, 5*time.Second, "the partial staged file to go", func() bool {
			return len(stagingEntries(t, env.staging)) == 0
		})
		if after := listNames(t, devfilesMount(t), dir); !sameListing(before, after) {
			t.Fatalf("an aborted send changed the device: %v → %v", before, after)
		}
	})

	t.Run("AbortMidRun", func(t *testing.T) {
		ctx := context.Background()
		data := patternedBytes(8<<20+123, 3)
		resp, err := c.post(ctx, stageRoute("big.fit"), bytes.NewReader(data), int64(len(data)), "application/octet-stream")
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		var staged devfiles.StagedUpload
		if err := json.NewDecoder(resp.Body).Decode(&staged); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("stage: status %d, decode %v", resp.StatusCode, err)
		}
		resp.Body.Close()

		resp, err = c.postJSON(ctx, "/api/files/upload/plan", map[string]any{"id": staged.ID, "to": to})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		var plan devfiles.UploadPlan
		if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("plan: status %d, decode %v", resp.StatusCode, err)
		}
		resp.Body.Close()
		if plan.Files != 1 || len(plan.Conflicts) != 0 {
			t.Fatalf("plan = %+v", plan)
		}

		slow.Store(true)
		runCtx, cancel := context.WithCancel(ctx)
		go func() {
			<-writing
			cancel()
		}()
		resp, err = c.postJSON(runCtx, "/api/files/upload", map[string]any{"id": staged.ID, "to": to})
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the stopped run ended with %v, want the client's cancel", err)
		}
		slow.Store(false)

		// The cancelled run removes its temp on a fresh session after the
		// browser has gone.
		waitFor(t, 30*time.Second, "the stopped run's temp to go", func() bool {
			for name := range listNames(t, devfilesMount(t), dir) {
				if strings.HasSuffix(name, ".commander.tmp") {
					return false
				}
			}
			return true
		})
		if _, err := devfilesMount(t).Stat(path.Join(dir, "big.fit")); !device.IsNotFound(err) {
			t.Fatalf("big.fit stands under its final name after the stop: Stat err = %v", err)
		}
		if !stagedHeld(t, env.staging, staged.ID) {
			t.Fatal("the stopped run removed the staged file")
		}

		// Retry: the same id writes the file again, nothing re-sent. The
		// stopped run may still hold the operation lock for a moment.
		var lines []map[string]any
		waitFor(t, 30*time.Second, "the retried run to stream", func() bool {
			resp, err := c.postJSON(ctx, "/api/files/upload", map[string]any{"id": staged.ID, "to": to})
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusConflict {
				return false // busy
			}
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("retry: status %d: %s", resp.StatusCode, b)
			}
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				var l map[string]any
				if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
					t.Fatalf("retry line %q: %v", sc.Text(), err)
				}
				lines = append(lines, l)
			}
			return true
		})
		last := lines[len(lines)-1]
		if last["type"] != "summary" || last["done"] != float64(1) || last["failed"] != float64(0) {
			t.Fatalf("retry lines = %v", lines)
		}
		check := devfilesMount(t)
		if got := devfilesRead(t, check, path.Join(dir, "big.fit")); !bytes.Equal(got, data) {
			t.Fatalf("big.fit differs after the retry: %d bytes, want %d", len(got), len(data))
		}
		devfilesNoTemps(t, check, dir)

		resp, err = c.postJSON(ctx, "/api/files/upload/discard", map[string]any{"id": staged.ID})
		if err != nil {
			t.Fatalf("discard: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("discard: status %d", resp.StatusCode)
		}
		if got := stagingEntries(t, env.staging); len(got) != 0 {
			t.Fatalf("the discard left %v", got)
		}
	})
}
