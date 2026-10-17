package devfiles

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
)

// receivingIDs are the ids of the records still being received.
func (u *uploadFixture) receivingIDs() []string {
	u.svc.staging.mu.Lock()
	defer u.svc.staging.mu.Unlock()
	var ids []string
	for id, r := range u.svc.staging.recs {
		if r.receiving {
			ids = append(ids, id)
		}
	}
	return ids
}

// wantGone asserts a plan, a run and a discard of id by owner answer
// ErrStagingGone without touching the device.
func (u *uploadFixture) wantGone(t *testing.T, owner, id string) {
	t.Helper()
	d := u.dev(ip2)
	muts := d.Mutations()
	_, err := u.svc.UploadPlan(context.Background(), owner, UploadPlanRequest{ID: id, To: dest})
	wantKind(t, err, ErrStagingGone)
	l, err := u.upload(owner, UploadRequest{ID: id, To: dest})
	wantKind(t, err, ErrStagingGone)
	if len(l.all) != 0 {
		t.Errorf("lines = %#v", l.all)
	}
	wantKind(t, u.svc.UploadDiscard(owner, id), ErrStagingGone)
	if got := d.Mutations(); got != muts {
		t.Errorf("mutations = %d, want %d", got, muts)
	}
}

// failingBody yields data, then calls fail once and returns what it returns.
type failingBody struct {
	data string
	fail func() error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if b.data != "" {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, b.fail()
}

// stageFailing stages a body that fails after half its declared bytes, and
// returns the id it was received under and the staging error.
func stageFailing(t *testing.T, u *uploadFixture, ctx context.Context, fail func() error) (string, error) {
	t.Helper()
	var id string
	body := &failingBody{data: "half", fail: func() error {
		ids := u.receivingIDs()
		if len(ids) != 1 {
			t.Fatalf("receiving = %v, want one record", ids)
		}
		id = ids[0]
		// Not usable while it is still being received.
		if _, err := u.svc.UploadPlan(context.Background(), owner1, UploadPlanRequest{ID: id, To: dest}); !errors.Is(err, ErrStagingGone) {
			t.Errorf("plan while receiving: %v, want staging_gone", err)
		}
		return fail()
	}}
	_, err := u.svc.UploadStage(ctx, owner1, "a.fit", 8, srcTime, body)
	return id, err
}

func wantNothingStaged(t *testing.T, u *uploadFixture, id string) {
	t.Helper()
	if got := u.files(t); len(got) != 0 {
		t.Errorf("staged files = %v, want none", got)
	}
	if u.records() != 0 {
		t.Errorf("records = %d, want none", u.records())
	}
	u.wantGone(t, owner1, id)
}

// TestStageShortBody: a body ending before its declared length leaves nothing.
func TestStageShortBody(t *testing.T) {
	u := newUploadFixture(t)
	id, err := stageFailing(t, u, context.Background(), func() error { return io.EOF })
	wantKind(t, err, ErrStageBody)
	wantNothingStaged(t, u, id)

	// One byte too many is no complete file either.
	_, err = u.svc.UploadStage(context.Background(), owner1, "b.fit", 3, srcTime, strings.NewReader("four"))
	wantKind(t, err, ErrStageBody)
	if got := u.files(t); len(got) != 0 || u.records() != 0 {
		t.Errorf("staged files = %v, records = %d", got, u.records())
	}
}

// TestStageBodyReadError: a body erroring mid-read leaves nothing.
func TestStageBodyReadError(t *testing.T) {
	u := newUploadFixture(t)
	cause := errors.New("connection reset by peer")
	id, err := stageFailing(t, u, context.Background(), func() error { return cause })
	wantKind(t, err, ErrStageBody)
	if !errors.Is(err, cause) {
		t.Errorf("error %v does not carry its cause", err)
	}
	wantNothingStaged(t, u, id)
}

// TestStageCancel: a request cancelled mid-body leaves nothing.
func TestStageCancel(t *testing.T) {
	u := newUploadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id, err := stageFailing(t, u, ctx, func() error {
		cancel()
		return context.Canceled
	})
	wantKind(t, err, ErrCancelled)
	wantNothingStaged(t, u, id)
}

// TestStagingComplete: a complete body is staged byte-identical, mode 0600,
// under its id in a 0700 directory.
func TestStagingComplete(t *testing.T) {
	u := newUploadFixture(t)
	body := strings.Repeat("c", 3*copyBufSize+1)
	id := u.stage(t, owner1, "a.fit", body, srcTime)
	if len(id) != 32 {
		t.Errorf("id = %q, want 128 bits in hex", id)
	}
	if got := u.files(t); !slices.Equal(got, []string{id}) {
		t.Errorf("staged files = %v, want only the id", got)
	}
	if got := u.staged(t, id); got != body {
		t.Error("staged content differs from the body")
	}
	fi, err := os.Stat(filepath.Join(u.dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(u.dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", di.Mode().Perm())
	}
}

// TestStagingKeptByRuns: no run removes the staged file, whatever it reports,
// and a second run with the same id (Retry) writes it again from Commander.
func TestStagingKeptByRuns(t *testing.T) {
	t.Run("copied, then Retry answers appeared", func(t *testing.T) {
		u := newUploadFixture(t)
		u.dev(ip2).AddDir(emmc, "Dest", t0)
		id := u.stage(t, owner1, "a.fit", "data", srcTime)
		if _, err := u.upload(owner1, UploadRequest{ID: id, To: dest}); err != nil {
			t.Fatal(err)
		}
		u.staged(t, id)
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Code != codeAppeared {
			t.Errorf("retry item = %+v, want appeared", it)
		}
		u.staged(t, id)
	})
	t.Run("failed, then Retry writes it", func(t *testing.T) {
		u := newUploadFixture(t)
		d := u.dev(ip2)
		d.AddDir(emmc, "Dest", t0)
		d.FailNext(device.FakeOpCreateNew, 4, errNetworkForTest())
		id := u.stage(t, owner1, "a.fit", "data", srcTime)
		l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Code != codeUnreachable {
			t.Errorf("item = %+v", it)
		}
		u.staged(t, id)
		l, err = u.upload(owner1, UploadRequest{ID: id, To: dest})
		if err != nil {
			t.Fatal(err)
		}
		if s := l.summary(t); s.Done != 1 {
			t.Errorf("retry summary = %+v", s)
		}
		if got := readFile(t, d, emmc, "Dest/a.fit"); got != "data" {
			t.Errorf("destination = %q", got)
		}
	})
	for _, c := range []struct {
		name  string
		hook  func(h *copyHooks, cancel func())
		final bool // whether the file reached its final name
	}{
		{"stopped mid-stream", func(h *copyHooks, cancel func()) {
			h.afterWrite = func(int64) { cancel() }
		}, false},
		{"stopped after the commit", func(h *copyHooks, cancel func()) {
			h.beforeRename = func(string, string, string) { cancel() }
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUploadFixture(t)
			h := installHooks(u.fixture)
			d := u.dev(ip2)
			d.AddDir(emmc, "Dest", t0)
			body := strings.Repeat("s", 2*copyBufSize)
			id := u.stage(t, owner1, "a.fit", body, srcTime)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.hook(h, cancel)
			err := u.svc.Upload(ctx, owner1, UploadRequest{ID: id, To: dest}, (&lines{}).emit)
			wantKind(t, err, ErrCancelled)
			if got := u.staged(t, id); got != body {
				t.Error("the staged file changed or went away after a Stop")
			}
			noTemps(t, d, emmc)
			if _, ok := tree(t, d, emmc)["Dest/a.fit"]; ok != c.final {
				t.Errorf("final name present = %v, want %v", ok, c.final)
			}
			h.afterWrite, h.beforeRename = nil, nil
			l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
			if err != nil {
				t.Fatal(err)
			}
			if c.final {
				if it := l.item(t, "a.fit"); it.Code != codeAppeared {
					t.Errorf("retry item = %+v, want appeared", it)
				}
			} else if s := l.summary(t); s.Done != 1 {
				t.Errorf("retry summary = %+v", s)
			}
		})
	}
}

// TestStagingDiscard: a discard removes the owner's idle file; a foreign or
// repeated discard answers staging_gone, and one while a run uses the file
// removes it when the run lets go of it.
func TestStagingDiscard(t *testing.T) {
	u := newUploadFixture(t)
	id := u.stage(t, owner1, "a.fit", "data", srcTime)
	u.wantGone(t, owner2, id)
	u.staged(t, id)

	if err := u.svc.UploadDiscard(owner1, id); err != nil {
		t.Fatal(err)
	}
	if got := u.files(t); len(got) != 0 || u.records() != 0 {
		t.Errorf("staged files = %v, records = %d", got, u.records())
	}
	u.wantGone(t, owner1, id)

	// Stop, then close: the discard lands while the run still holds the file.
	id = u.stage(t, owner1, "b.fit", "data", srcTime)
	_, unpin, err := u.svc.pinStaged(owner1, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.svc.UploadDiscard(owner1, id); err != nil {
		t.Fatalf("discard while in use: %v", err)
	}
	u.staged(t, id)
	// A discarded id is gone to every later plan, run or discard.
	u.wantGone(t, owner1, id)
	unpin()
	if got := u.files(t); len(got) != 0 || u.records() != 0 {
		t.Errorf("after the run let go: staged files = %v, records = %d", got, u.records())
	}
	u.wantGone(t, owner1, id)
}

// TestStagingExpiry: the sweep removes a file unused for an hour, keeps one
// used 59 minutes ago, and never removes one a run is using.
func TestStagingExpiry(t *testing.T) {
	u := newUploadFixture(t)
	old := u.stage(t, owner1, "old.fit", "o", srcTime)
	u.clock.advance(time.Minute)
	recent := u.stage(t, owner1, "recent.fit", "r", srcTime)
	u.clock.advance(59 * time.Minute)
	u.svc.sweepStaging()
	if got := u.files(t); !slices.Equal(got, []string{recent}) {
		t.Errorf("after an hour: files = %v, want only %s", got, recent)
	}
	u.wantGone(t, owner1, old)

	// A run in progress spans more than an hour of the clock: its file stays.
	h := installHooks(u.fixture)
	u.dev(ip2).AddDir(emmc, "Dest", t0)
	h.beforeRename = func(string, string, string) {
		u.clock.advance(2 * time.Hour)
		u.svc.sweepStaging()
	}
	if _, err := u.upload(owner1, UploadRequest{ID: recent, To: dest}); err != nil {
		t.Fatal(err)
	}
	if got := u.files(t); !slices.Equal(got, []string{recent}) {
		t.Errorf("after a long run: files = %v", got)
	}
	// The run's end is a use: an hour less a minute later it is still held.
	h.beforeRename = nil
	u.clock.advance(59 * time.Minute)
	u.svc.sweepStaging()
	u.staged(t, recent)
	u.clock.advance(time.Minute)
	u.svc.sweepStaging()
	if got := u.files(t); len(got) != 0 {
		t.Errorf("files = %v, want none", got)
	}
}

// TestStagingStartEmpties: starting over a directory holding files and a stray
// subdirectory empties it.
func TestStagingStartEmpties(t *testing.T) {
	u := newUploadFixture(t)
	id := u.stage(t, owner1, "a.fit", "data", srcTime)
	if err := os.WriteFile(filepath.Join(u.dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(u.dir, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(u.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := u.svc.StartStaging(); err != nil {
		t.Fatal(err)
	}
	if got := u.files(t); len(got) != 0 {
		t.Errorf("files = %v, want none", got)
	}
	if di, _ := os.Stat(u.dir); di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", di.Mode().Perm())
	}
	u.wantGone(t, owner1, id)
}

// TestStagingCap: at the cap a new file evicts the least recently used idle
// one; with every staged file in use it is refused.
func TestStagingCap(t *testing.T) {
	u := newUploadFixture(t)
	var ids []string
	for i := range stagingCap {
		ids = append(ids, u.stage(t, owner1, string(rune('a'+i))+".fit", "x", srcTime))
		u.clock.advance(time.Second)
	}
	// Using the first makes the second the least recently used.
	_, unpin, err := u.svc.pinStaged(owner1, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	unpin()
	fifth := u.stage(t, owner1, "e.fit", "x", srcTime)
	u.wantGone(t, owner1, ids[1])
	if _, err := os.Stat(filepath.Join(u.dir, ids[1])); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("evicted file still on disk: %v", err)
	}
	if u.records() != stagingCap {
		t.Errorf("records = %d", u.records())
	}

	for _, id := range []string{ids[0], ids[2], ids[3], fifth} {
		_, unpin, err := u.svc.pinStaged(owner1, id)
		if err != nil {
			t.Fatal(err)
		}
		defer unpin()
	}
	read := 0
	_, err = u.svc.UploadStage(context.Background(), owner1, "f.fit", 1, srcTime,
		readerFunc(func([]byte) (int, error) { read++; return 0, io.EOF }))
	e := wantKind(t, err, ErrStagingFull)
	if !strings.Contains(e.Msg, "4 uploads") || read != 0 {
		t.Errorf("message %q, body reads %d", e.Msg, read)
	}
	if len(u.files(t)) != stagingCap {
		t.Errorf("files = %v", u.files(t))
	}
}

// TestStagingReserve: a file is accepted only while Commander's volume keeps
// stagingReserve free after it and the declared rest of every file still
// being received.
func TestStagingReserve(t *testing.T) {
	u := newUploadFixture(t)
	u.lfs.FreeBytes = stagingReserve + 100
	read := 0
	_, err := u.svc.UploadStage(context.Background(), owner1, "big.fit", 101, srcTime,
		readerFunc(func([]byte) (int, error) { read++; return 0, io.EOF }))
	wantKind(t, err, ErrStagingFull)
	if read != 0 || len(u.files(t)) != 0 || u.records() != 0 {
		t.Errorf("reads %d, files %v, records %d", read, u.files(t), u.records())
	}
	u.stage(t, owner1, "fits.fit", strings.Repeat("x", 100), srcTime)

	// Two files still being received hold their declared lengths.
	u.lfs.FreeBytes = stagingReserve + 1000
	var pipes []*io.PipeWriter
	done := make(chan error, 2)
	for _, n := range []int64{300, 400} {
		pr, pw := io.Pipe()
		pipes = append(pipes, pw)
		go func() {
			_, err := u.svc.UploadStage(context.Background(), owner1, "r.fit", n, srcTime, pr)
			done <- err
		}()
	}
	waitFor(t, "two receiving", func() bool { return len(u.receivingIDs()) == 2 })
	_, err = u.svc.UploadStage(context.Background(), owner1, "third.fit", 301, srcTime, strings.NewReader(strings.Repeat("t", 301)))
	wantKind(t, err, ErrStagingFull)
	u.stage(t, owner1, "third.fit", strings.Repeat("t", 300), srcTime)

	for _, pw := range pipes {
		_ = pw.CloseWithError(errors.New("client went away"))
	}
	for range pipes {
		if err := <-done; !errors.Is(err, ErrStageBody) {
			t.Errorf("receive = %v, want a body failure", err)
		}
	}
}

// TestStagingNameCap: a 224-byte name stages and writes with a temp name
// within the device's 255 bytes; a 225-byte one is refused.
func TestStagingNameCap(t *testing.T) {
	u := newUploadFixture(t)
	h := installHooks(u.fixture)
	u.dev(ip2).AddDir(emmc, "Dest", t0)
	long := strings.Repeat("n", 220) + ".fit"
	if len(long) != 224 || uploadNameMax != 224 {
		t.Fatalf("name %d bytes, cap %d", len(long), uploadNameMax)
	}
	_, err := u.svc.UploadStage(context.Background(), owner1, long[:223]+"nf", 1, srcTime, strings.NewReader("x"))
	wantKind(t, err, ErrBadPath)

	id := u.stage(t, owner1, long, "x", srcTime)
	var temp string
	h.beforeRename = func(_, oldPath, _ string) { temp = filepath.Base(oldPath) }
	l, err := u.upload(owner1, UploadRequest{ID: id, To: dest})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.summary(t); s.Done != 1 {
		t.Errorf("summary = %+v", s)
	}
	if len(temp) > maxNameBytes || !strings.HasSuffix(temp, tmpSuffix) || !strings.HasPrefix(temp, long+".") {
		t.Errorf("temp name %q (%d bytes)", temp, len(temp))
	}
}

// TestStagingNeverLogsContentOrOwner runs staging, a plan, a run, a refusal
// and a discard, and finds neither the staged content nor the owner key in any
// log line.
func TestStagingNeverLogsContentOrOwner(t *testing.T) {
	u := newUploadFixture(t)
	h := recordLogs(u.fixture)
	u.dev(ip2).AddDir(emmc, "Dest", t0)
	const content = "STAGED-CONTENT-7d1f"
	const owner = "OWNER-KEY-9c3a"
	id := u.stage(t, owner, "a.fit", content, srcTime)
	if _, err := u.svc.UploadPlan(context.Background(), owner, UploadPlanRequest{ID: id, To: dest}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.upload(owner, UploadRequest{ID: id, To: dest}); err != nil {
		t.Fatal(err)
	}
	_, _ = u.upload(owner2, UploadRequest{ID: id, To: dest})
	_, _ = u.svc.UploadStage(context.Background(), owner, "b.fit", 50, srcTime, strings.NewReader(content))
	if err := u.svc.UploadDiscard(owner, id); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) == 0 {
		t.Fatal("no log lines recorded")
	}
	for _, l := range h.lines {
		if strings.Contains(l, content) || strings.Contains(l, owner) {
			t.Errorf("log line carries staged content or the owner key: %s", l)
		}
	}
}

// lockCheckingFS fails the test when the staging store's lock is held while
// Commander's free space is read.
type lockCheckingFS struct {
	*localfs.Fake
	t  *testing.T
	st *stagingStore
}

func (f *lockCheckingFS) FreeSpace(root string) (int64, int64, error) {
	if !f.st.mu.TryLock() {
		f.t.Error("free space read under the staging lock")
	} else {
		f.st.mu.Unlock()
	}
	return f.Fake.FreeSpace(root)
}

// TestStagingFreeSpaceOutsideLock: the free-space read, a filesystem call that
// can stall, never holds the lock every plan, run and discard needs.
func TestStagingFreeSpaceOutsideLock(t *testing.T) {
	u := newUploadFixture(t)
	u.svc.lfs = &lockCheckingFS{Fake: u.lfs, t: t, st: u.svc.staging}
	u.stage(t, owner1, "a.fit", "data", srcTime)
}

// TestStagePanicLeavesNothing: a receive that panics still removes its record
// and partial file, and the panic carries on to the caller.
func TestStagePanicLeavesNothing(t *testing.T) {
	u := newUploadFixture(t)
	sent := false
	body := readerFunc(func(p []byte) (int, error) {
		if !sent {
			sent = true
			return copy(p, "half"), nil
		}
		panic("body exploded")
	})
	func() {
		defer func() {
			if r := recover(); r != "body exploded" {
				t.Errorf("recovered %v, want the body's panic", r)
			}
		}()
		_, _ = u.svc.UploadStage(context.Background(), owner1, "a.fit", 8, srcTime, body)
	}()
	if got := u.files(t); len(got) != 0 || u.records() != 0 {
		t.Errorf("staged files = %v, records = %d", got, u.records())
	}
}
