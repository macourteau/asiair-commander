package devfiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
)

const (
	owner1 = "owner-key-one"
	owner2 = "owner-key-two"
)

// testClock is a settable clock for staging expiry.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// uploadFixture is the fixture with staging under a temp directory, a fake
// local filesystem reporting lfs.FreeBytes, and a settable clock.
type uploadFixture struct {
	*fixture
	lfs   *localfs.Fake
	clock *testClock
	dir   string
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	fx := newFixture(t)
	u := &uploadFixture{fixture: fx, clock: &testClock{t: t0}, dir: filepath.Join(t.TempDir(), "upload-staging")}
	u.lfs = localfs.NewFake(u.dir)
	u.lfs.FreeBytes = 100 << 30
	fx.svc.lfs = u.lfs
	fx.svc.now = u.clock.Now
	fx.svc.staging = newStagingStore(u.dir)
	if err := fx.svc.StartStaging(); err != nil {
		t.Fatal(err)
	}
	return u
}

// stage stages body as name for owner and returns its id.
func (u *uploadFixture) stage(t *testing.T, owner, name, body string, mtime time.Time) string {
	t.Helper()
	st, err := u.svc.UploadStage(context.Background(), owner, name, int64(len(body)), mtime, strings.NewReader(body))
	if err != nil {
		t.Fatalf("UploadStage(%s): %v", name, err)
	}
	if st.Size != int64(len(body)) {
		t.Fatalf("staged size = %d, want %d", st.Size, len(body))
	}
	return st.ID
}

func (u *uploadFixture) upload(owner string, req UploadRequest) (*lines, error) {
	l := &lines{}
	err := u.svc.Upload(context.Background(), owner, req, l.emit)
	return l, err
}

// files are the names in the staging directory.
func (u *uploadFixture) files(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (u *uploadFixture) records() int {
	u.svc.staging.mu.Lock()
	defer u.svc.staging.mu.Unlock()
	return len(u.svc.staging.recs)
}

func (u *uploadFixture) staged(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(u.dir, id))
	if err != nil {
		t.Fatalf("staged file %s: %v", id, err)
	}
	return string(b)
}

var dest = CopyPlace{Rig: "r2", Share: emmc, Dir: "Dest"}

// bind is a run request overwriting the destination as fi shows it.
func bind(id string, fi device.FileInfo) UploadRequest {
	size, mtime := fi.Size, fi.Mtime.Unix()
	return UploadRequest{ID: id, To: dest, DstSize: &size, DstMtime: &mtime}
}

// TestUploadWritesStagedFile uploads a multi-chunk file: the plan is a
// one-file copy plan with the share's free space, the run writes it
// byte-identical with its mtime, reports copied without an item line, and
// leaves the staged file in place.
func TestUploadWritesStagedFile(t *testing.T) {
	u := newUploadFixture(t)
	d := u.dev(ip2)
	d.AddDir(emmc, "Dest", t0)
	d.SetStatfs(emmc, 9<<30, 32<<30)
	body := strings.Repeat("f", copyBufSize*2+7)
	id := u.stage(t, owner1, "ref.fit", body, srcTime)

	plan, err := u.svc.UploadPlan(context.Background(), owner1, UploadPlanRequest{ID: id, To: dest})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Files != 1 || plan.Bytes != int64(len(body)) || len(plan.Items) != 1 || plan.Items[0].Rel != "ref.fit" ||
		len(plan.Conflicts) != 0 || len(plan.Dirs) != 0 {
		t.Errorf("plan = %+v", plan)
	}
	if plan.FreeBytes == nil || *plan.FreeBytes != 9<<30 {
		t.Errorf("free bytes = %v, want 9 GiB", plan.FreeBytes)
	}

	l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.summary(t); s.Done != 1 || s.Skipped != 0 || s.Failed != 0 || s.Aborted != "" {
		t.Errorf("summary = %+v", s)
	}
	if len(l.items()) != 0 {
		t.Errorf("items = %+v, want none", l.items())
	}
	first, ok := l.all[0].(ProgressLine)
	if !ok || first.Total != 1 || first.BytesTotal != int64(len(body)) || first.Current != "ref.fit" {
		t.Errorf("first line = %#v", l.all[0])
	}
	if got := readFile(t, d, emmc, "Dest/ref.fit"); got != body {
		t.Error("destination content differs from the staged file")
	}
	if fi := stat(t, d, emmc, "Dest/ref.fit"); !fi.Mtime.Equal(srcTime) {
		t.Errorf("mtime = %v, want %v", fi.Mtime, srcTime)
	}
	noTemps(t, d, emmc)
	if got := u.staged(t, id); got != body {
		t.Error("the staged file changed or went away after a successful run")
	}
}

// TestUploadAppeared: a destination present at run time that the request does
// not bind is left byte-identical and reported appeared, with its size.
func TestUploadAppeared(t *testing.T) {
	u := newUploadFixture(t)
	d := u.dev(ip2)
	d.AddDir(emmc, "Dest", t0)
	id := u.stage(t, owner1, "a.fit", "mine", srcTime)
	if _, err := u.svc.UploadPlan(context.Background(), owner1, UploadPlanRequest{ID: id, To: dest}); err != nil {
		t.Fatal(err)
	}
	d.AddFile(emmc, "Dest/a.fit", []byte("theirs"), t0)

	l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
	if err != nil {
		t.Fatal(err)
	}
	it := l.item(t, "a.fit")
	if it.Outcome != outcomeSkipped || it.Code != codeAppeared || it.DstSize == nil || *it.DstSize != 6 {
		t.Errorf("item = %+v", it)
	}
	if got := readFile(t, d, emmc, "Dest/a.fit"); got != "theirs" {
		t.Errorf("destination = %q, want it untouched", got)
	}
	noTemps(t, d, emmc)
}

// TestUploadOverwriteBound: an overwrite replaces only a destination that
// still has the size and mtime the user saw; one that moved is changed, and a
// folder at the name is a type conflict, in the plan and at run time.
func TestUploadOverwriteBound(t *testing.T) {
	setup := func(t *testing.T) (*uploadFixture, *device.Fake, string, device.FileInfo) {
		u := newUploadFixture(t)
		d := u.dev(ip2)
		d.AddFile(emmc, "Dest/a.fit", []byte("old"), t0)
		id := u.stage(t, owner1, "a.fit", "newer", srcTime)
		plan, err := u.svc.UploadPlan(context.Background(), owner1, UploadPlanRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != conflictFile || plan.Conflicts[0].SrcSize != 5 ||
			plan.Conflicts[0].SrcMtime != srcTime.Unix() || plan.Conflicts[0].DstSize != 3 {
			t.Fatalf("conflicts = %+v", plan.Conflicts)
		}
		return u, d, id, stat(t, d, emmc, "Dest/a.fit")
	}

	t.Run("matching binding replaces", func(t *testing.T) {
		u, d, id, fi := setup(t)
		l, err := u.upload(owner1, bind(id, fi))
		if err != nil {
			t.Fatal(err)
		}
		if s := l.summary(t); s.Done != 1 {
			t.Errorf("summary = %+v", s)
		}
		if got := readFile(t, d, emmc, "Dest/a.fit"); got != "newer" {
			t.Errorf("destination = %q", got)
		}
		noTemps(t, d, emmc)
	})
	for _, c := range []struct {
		name string
		edit func(d *device.Fake)
	}{
		{"size moved", func(d *device.Fake) { d.AddFile(emmc, "Dest/a.fit", []byte("old!"), t0) }},
		{"mtime moved", func(d *device.Fake) { d.SetMtime(emmc, "Dest/a.fit", t0.Add(time.Minute)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, d, id, fi := setup(t)
			c.edit(d)
			before := readFile(t, d, emmc, "Dest/a.fit")
			l, err := u.upload(owner1, bind(id, fi))
			if err != nil {
				t.Fatal(err)
			}
			if it := l.item(t, "a.fit"); it.Outcome != outcomeSkipped || it.Code != codeChanged {
				t.Errorf("item = %+v", it)
			}
			if got := readFile(t, d, emmc, "Dest/a.fit"); got != before {
				t.Errorf("destination = %q, want %q", got, before)
			}
			noTemps(t, d, emmc)
		})
	}

	t.Run("a folder at the name", func(t *testing.T) {
		u := newUploadFixture(t)
		d := u.dev(ip2)
		d.AddDir(emmc, "Dest/a.fit", t0)
		id := u.stage(t, owner1, "a.fit", "newer", srcTime)
		plan, err := u.svc.UploadPlan(context.Background(), owner1, UploadPlanRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Conflicts) != 1 || plan.Conflicts[0].Kind != conflictType {
			t.Fatalf("conflicts = %+v", plan.Conflicts)
		}
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Outcome != outcomeSkipped || it.Code != codeTypeConflict {
			t.Errorf("item = %+v", it)
		}
		noTemps(t, d, emmc)
	})

	t.Run("a binding needs both fields", func(t *testing.T) {
		u, _, id, fi := setup(t)
		size := fi.Size
		_, err := u.upload(owner1, UploadRequest{ID: id, To: dest, DstSize: &size})
		wantKind(t, err, ErrBadPath)
	})
}

// TestUploadRetry: an upload retries as a copy item does, every attempt from
// byte 0 of the staged file on a fresh temp, and a failure reading
// Commander's own staged copy fails once as an error.
func TestUploadRetry(t *testing.T) {
	body := strings.Repeat("r", copyBufSize+11)
	setup := func(t *testing.T) (*uploadFixture, *device.Fake, *copyHooks, string) {
		u := newUploadFixture(t)
		h := installHooks(u.fixture)
		d := u.dev(ip2)
		d.AddDir(emmc, "Dest", t0)
		return u, d, h, u.stage(t, owner1, "a.fit", body, srcTime)
	}
	copied := func(t *testing.T, u *uploadFixture, d *device.Fake, id string) {
		t.Helper()
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if s := l.summary(t); s.Done != 1 || len(l.items()) != 0 {
			t.Errorf("summary = %+v, items = %+v", s, l.items())
		}
		if got := readFile(t, d, emmc, "Dest/a.fit"); got != body {
			t.Error("destination differs from the staged file")
		}
		noTemps(t, d, emmc)
	}

	t.Run("network failures at CreateNew", func(t *testing.T) {
		u, d, h, id := setup(t)
		d.FailNext(device.FakeOpCreateNew, 2, errNetworkForTest())
		copied(t, u, d, id)
		if got := h.creates.Load(); got != 3 {
			t.Errorf("CreateNew calls = %d, want 3", got)
		}
	})
	t.Run("a writer failure after the first chunk restarts from byte 0", func(t *testing.T) {
		u, d, h, id := setup(t)
		h.afterWrite = func(n int64) {
			if n == 1 {
				d.FailNext(device.FakeOpWrite, 1, errNetworkForTest())
			}
		}
		copied(t, u, d, id)
		if got := h.creates.Load(); got != 2 {
			t.Errorf("CreateNew calls = %d, want 2 (a fresh temp for the retry)", got)
		}
	})
	t.Run("a commit failure after a complete stream", func(t *testing.T) {
		u, d, h, id := setup(t)
		d.FailNext(device.FakeOpRename, 1, errNetworkForTest())
		copied(t, u, d, id)
		if got := h.creates.Load(); got != 2 {
			t.Errorf("CreateNew calls = %d, want 2", got)
		}
	})
	t.Run("four failures are unreachable", func(t *testing.T) {
		u, d, _, id := setup(t)
		d.FailNext(device.FakeOpCreateNew, 4, errNetworkForTest())
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Outcome != outcomeFailed || it.Code != codeUnreachable {
			t.Errorf("item = %+v", it)
		}
		if s := l.summary(t); s.Failed != 1 || s.Aborted != abortedUnreachable {
			t.Errorf("summary = %+v", s)
		}
		noTemps(t, d, emmc)
		if got := u.staged(t, id); got != body {
			t.Error("the staged file changed after a failed run")
		}
	})
	t.Run("a staged-file read error fails once", func(t *testing.T) {
		u, d, h, id := setup(t)
		// A folder in its place opens, then fails its first read.
		p := filepath.Join(u.dir, id)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		it := l.item(t, "a.fit")
		if it.Outcome != outcomeFailed || it.Code != codeError || strings.Contains(it.Message, u.dir) {
			t.Errorf("item = %+v", it)
		}
		if got := h.creates.Load(); got != 1 {
			t.Errorf("CreateNew calls = %d, want 1 (not retried)", got)
		}
		noTemps(t, d, emmc)
	})
	t.Run("a staged file gone from disk fails once", func(t *testing.T) {
		u, d, h, id := setup(t)
		if err := os.Remove(filepath.Join(u.dir, id)); err != nil {
			t.Fatal(err)
		}
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Outcome != outcomeFailed || it.Code != codeError {
			t.Errorf("item = %+v", it)
		}
		if got := h.creates.Load(); got != 0 {
			t.Errorf("CreateNew calls = %d, want 0", got)
		}
		noTemps(t, d, emmc)
	})
}

// TestUploadMtime: the browser's time is set; a refused Chtimes still copies,
// worded for an upload; no time skips the call.
func TestUploadMtime(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		u := newUploadFixture(t)
		u.dev(ip2).AddDir(emmc, "Dest", t0)
		id := u.stage(t, owner1, "a.fit", "x", srcTime)
		if _, err := u.upload(owner1, UploadRequest{ID: id, To: dest}); err != nil {
			t.Fatal(err)
		}
		if fi := stat(t, u.dev(ip2), emmc, "Dest/a.fit"); !fi.Mtime.Equal(srcTime) {
			t.Errorf("mtime = %v, want %v", fi.Mtime, srcTime)
		}
	})
	t.Run("refused", func(t *testing.T) {
		u := newUploadFixture(t)
		u.dev(ip2).AddDir(emmc, "Dest", t0)
		u.dev(ip2).FailChtimes(emmc, &os.PathError{Op: "chtimes", Path: "a.fit", Err: os.ErrPermission})
		id := u.stage(t, owner1, "a.fit", "x", srcTime)
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		it := l.item(t, "a.fit")
		if it.Outcome != outcomeCopied || it.Code != codeMtimeNotPreserved ||
			it.Message != "Uploaded; Rig Two did not accept the timestamp, so it shows the upload time." {
			t.Errorf("item = %+v", it)
		}
		if got := readFile(t, u.dev(ip2), emmc, "Dest/a.fit"); got != "x" {
			t.Errorf("destination = %q", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		for _, mtime := range []time.Time{{}, time.UnixMilli(0)} {
			u := newUploadFixture(t)
			h := installHooks(u.fixture)
			u.dev(ip2).AddDir(emmc, "Dest", t0)
			id := u.stage(t, owner1, "a.fit", "x", mtime)
			l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
			if err != nil {
				t.Fatal(err)
			}
			if s := l.summary(t); s.Done != 1 || len(l.items()) != 0 {
				t.Errorf("summary = %+v, items = %+v", s, l.items())
			}
			if got := h.chtimes.Load(); got != 0 {
				t.Errorf("mtime %v: Chtimes calls = %d, want 0", mtime, got)
			}
			if fi := stat(t, u.dev(ip2), emmc, "Dest/a.fit"); !fi.Mtime.Equal(t0) {
				t.Errorf("mtime = %v, want the device's write time %v", fi.Mtime, t0)
			}
		}
	})
}

// TestUploadRefusals: the staging request refuses bad names before writing
// anything; the run refuses bad folders, reserved paths, a busy lock and an
// unknown or foreign id before the lock, and the pre-write check's failures
// after it, each as a plain refusal with no stream line, no device mutation,
// no CreateNew and the staged file kept.
func TestUploadRefusals(t *testing.T) {
	t.Run("staging names", func(t *testing.T) {
		u := newUploadFixture(t)
		for _, name := range []string{
			"", "a/b.fit", "a\\b.fit", "a*.fit", "a?.fit", "a:b.fit", "a.fit.", "a.fit ", " a.fit", "a​.fit",
			model.IdentityMarkerName, strings.ToUpper(model.MediaMarkerName),
			strings.Repeat("n", uploadNameMax+1),
		} {
			read := 0
			body := readerFunc(func(p []byte) (int, error) { read++; return 0, errors.New("read") })
			_, err := u.svc.UploadStage(context.Background(), owner1, name, 1, srcTime, body)
			wantKind(t, err, ErrBadPath)
			if read != 0 {
				t.Errorf("%q: the body was read before the refusal", name)
			}
		}
		if got := u.files(t); len(got) != 0 || u.records() != 0 {
			t.Errorf("staged files = %v, records = %d", got, u.records())
		}
		_, err := u.svc.UploadStage(context.Background(), owner1, "a.fit", -1, srcTime, strings.NewReader(""))
		wantKind(t, err, ErrLengthRequired)
	})

	type setupFn func(t *testing.T, u *uploadFixture, id string) (string, UploadRequest)
	run := func(t *testing.T, kind error, setup setupFn) {
		t.Helper()
		u := newUploadFixture(t)
		h := installHooks(u.fixture)
		d := u.dev(ip2)
		d.AddDir(emmc, "Dest", t0)
		id := u.stage(t, owner1, "a.fit", "keep", srcTime)
		owner, req := setup(t, u, id)
		muts := d.Mutations()
		l, err := u.upload(owner, req)
		wantKind(t, err, kind)
		if len(l.all) != 0 {
			t.Errorf("lines = %#v, want none", l.all)
		}
		if got := d.Mutations(); got != muts {
			t.Errorf("mutations = %d, want %d", got, muts)
		}
		if got := h.creates.Load(); got != 0 {
			t.Errorf("CreateNew calls = %d", got)
		}
		if got := u.staged(t, id); got != "keep" {
			t.Errorf("staged file = %q", got)
		}
	}
	plain := func(dir string) setupFn {
		return func(_ *testing.T, _ *uploadFixture, id string) (string, UploadRequest) {
			return owner1, UploadRequest{ID: id, To: CopyPlace{Rig: "r2", Share: emmc, Dir: dir}}
		}
	}
	cases := []struct {
		name  string
		kind  error
		setup setupFn
	}{
		{"wildcard folder", ErrBadPath, plain("De*st")},
		{"trailing-dot folder", ErrBadPath, plain("Dest.")},
		{"unknown id", ErrStagingGone, func(_ *testing.T, _ *uploadFixture, _ string) (string, UploadRequest) {
			return owner1, UploadRequest{ID: "0123456789abcdef0123456789abcdef", To: dest}
		}},
		{"foreign id", ErrStagingGone, func(_ *testing.T, _ *uploadFixture, id string) (string, UploadRequest) {
			return owner2, UploadRequest{ID: id, To: dest}
		}},
		{"reserved at run time", ErrWatched, func(_ *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			u.editRig("r2", func(p *model.RigProfile) {
				p.Sources = append(slices.Clone(p.Sources), model.Source{Share: emmc, Roots: []string{"Dest/A.FIT"}, Disabled: true})
			})
			return owner1, UploadRequest{ID: id, To: dest}
		}},
		{"busy", ErrBusy, func(t *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			unlock, err := u.svc.lockOp(context.Background(), "Copy", "r1", "r2")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(unlock)
			return owner1, UploadRequest{ID: id, To: dest}
		}},
		{"identity", ErrIdentity, func(_ *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			u.prober.script(ip2, probeAnswer{guid: "someone-else", ok: true})
			return owner1, UploadRequest{ID: id, To: dest}
		}},
		{"host-bound", ErrHostBound, func(_ *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			u.editRig("r2", func(p *model.RigProfile) { p.GUID = "" })
			return owner1, UploadRequest{ID: id, To: dest}
		}},
		{"share gone", ErrNotFound, func(_ *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			u.dev(ip2).FailMount(emmc, device.NotFoundError("mount"))
			return owner1, UploadRequest{ID: id, To: dest}
		}},
		{"folder gone", ErrNotFound, plain("Gone")},
		{"a file at the folder's path", ErrTypeConflict, func(_ *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			u.dev(ip2).AddFile(emmc, "Flat", []byte("f"), t0)
			return owner1, UploadRequest{ID: id, To: CopyPlace{Rig: "r2", Share: emmc, Dir: "Flat"}}
		}},
		{"unreachable", ErrUnreachable, func(_ *testing.T, u *uploadFixture, id string) (string, UploadRequest) {
			u.dev(ip2).FailNext(device.FakeOpStat, 4, errNetworkForTest())
			return owner1, UploadRequest{ID: id, To: dest}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { run(t, c.kind, c.setup) })
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
