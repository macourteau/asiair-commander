package devfiles

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
)

// sink is a DownloadWriter that records Begin and the body, and fails on
// demand: failAt makes the Write that would pass that many bytes fail.
type sink struct {
	begun  int
	info   DownloadInfo
	body   bytes.Buffer
	failAt int
}

func (s *sink) Begin(info DownloadInfo) {
	s.begun++
	s.info = info
}

func (s *sink) Write(p []byte) (int, error) {
	if s.begun != 1 {
		return 0, errors.New("write before Begin")
	}
	if s.failAt > 0 && s.body.Len()+len(p) >= s.failAt {
		return 0, errors.New("client went away")
	}
	return s.body.Write(p)
}

// readZip opens a downloaded zip and returns its entries by name.
func readZip(t *testing.T, b []byte) map[string]*zip.File {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	out := map[string]*zip.File{}
	for _, f := range zr.File {
		out[f.Name] = f
	}
	return out
}

func zipContent(t *testing.T, f *zip.File) string {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDownloadZip(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.ListMarkers(true)
	t1 := t0.Add(-48 * time.Hour)
	f.AddDir(emmc, "Preview/M42", t0)
	f.AddFile(emmc, "Preview/M42/a.fit", []byte("alpha"), t1)
	f.AddFile(emmc, "Preview/M42/sub/b.fit", []byte("bravo!"), t0)
	f.AddDir(emmc, "Preview/M42/empty", t1)
	f.SetMarker(emmc, "Preview/M42/"+model.MediaMarkerName, []byte("id"))
	f.AddFile(emmc, "Preview/ref.jpg", []byte("jpeg"), t0)

	var w sink
	if err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"M42"}, &w); err != nil {
		t.Fatal(err)
	}
	want := DownloadInfo{Filename: "Rig One-M42.zip", Files: 2, Bytes: 11, Zipped: true}
	if w.begun != 1 || w.info != want {
		t.Errorf("Begin %d times with %+v, want once with %+v", w.begun, w.info, want)
	}
	entries := readZip(t, w.body.Bytes())
	var names []string
	for n := range entries {
		names = append(names, n)
	}
	wantNames := []string{"M42/", "M42/a.fit", "M42/empty/", "M42/sub/", "M42/sub/b.fit"}
	if !sameSet(names, wantNames) {
		t.Errorf("entries = %v, want %v (no marker)", names, wantNames)
	}
	if got := zipContent(t, entries["M42/a.fit"]); got != "alpha" {
		t.Errorf("a.fit = %q", got)
	}
	if got := zipContent(t, entries["M42/sub/b.fit"]); got != "bravo!" {
		t.Errorf("b.fit = %q", got)
	}
	for name, mt := range map[string]time.Time{"M42/a.fit": t1, "M42/empty/": t1, "M42/sub/b.fit": t0} {
		e := entries[name]
		if e.Method != zip.Store {
			t.Errorf("%s method = %d, want Store", name, e.Method)
		}
		if !e.Modified.Equal(mt) {
			t.Errorf("%s modified = %v, want %v", name, e.Modified, mt)
		}
	}
	if !entries["M42/empty/"].FileInfo().IsDir() {
		t.Error("the empty folder is not a directory entry")
	}

	// Several items zip under the current folder's name; at a share root, the share's.
	w = sink{}
	if err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"M42", "ref.jpg"}, &w); err != nil {
		t.Fatal(err)
	}
	if w.info.Filename != "Rig One-Preview.zip" || len(readZip(t, w.body.Bytes())) != 6 {
		t.Errorf("info = %+v", w.info)
	}
	info, err := fx.svc.DownloadCheck(ctx, "r1", emmc, "", []string{"Preview"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Filename != "Rig One-Preview.zip" {
		t.Errorf("filename = %q", info.Filename)
	}
	f.AddFile(emmc, "top.txt", []byte("t"), t0)
	if info, err = fx.svc.DownloadCheck(ctx, "r1", emmc, "", []string{"Preview", "top.txt"}); err != nil || info.Filename != "Rig One-"+emmc+".zip" {
		t.Errorf("root zip = %+v, %v", info, err)
	}
	if got := f.Mutations(); got != 0 {
		t.Errorf("mutations = %d, want 0", got)
	}
}

func sameSet(a, b []string) bool {
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return len(a) == len(b)
}

func TestDownloadSingleFile(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	body := bytes.Repeat([]byte("0123456789"), 100_000) // two 512 KiB reads
	f.AddFile(emmc, "Preview/Ünïcode frame.fit", body, t0)
	f.AddFile(emmc, "Preview/empty.txt", nil, t0)

	var w sink
	if err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"Ünïcode frame.fit"}, &w); err != nil {
		t.Fatal(err)
	}
	want := DownloadInfo{Filename: "Ünïcode frame.fit", Files: 1, Bytes: int64(len(body))}
	if w.begun != 1 || w.info != want || !bytes.Equal(w.body.Bytes(), body) {
		t.Errorf("Begin %d, info %+v, body %d bytes; want once, %+v, %d bytes", w.begun, w.info, w.body.Len(), want, len(body))
	}

	// An empty file still begins its (empty) response.
	w = sink{}
	if err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"empty.txt"}, &w); err != nil {
		t.Fatal(err)
	}
	if w.begun != 1 || w.info.Bytes != 0 || w.body.Len() != 0 {
		t.Errorf("empty file: Begin %d, info %+v", w.begun, w.info)
	}

	// A refused open before the first byte is an ordinary error: nothing sent.
	f.FailNext(device.FakeOpOpenReader, 1, &os.PathError{Op: "open", Path: "x", Err: os.ErrPermission})
	w = sink{}
	err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"empty.txt"}, &w)
	wantKind(t, err, ErrDenied)
	if errors.Is(err, ErrMidStream) || w.begun != 0 {
		t.Errorf("error %v, Begin %d; want a plain error and no response", err, w.begun)
	}

	// A network failure on the open is retried.
	f.FailNext(device.FakeOpOpenReader, 2, errNetworkForTest())
	w = sink{}
	if err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"empty.txt"}, &w); err != nil {
		t.Errorf("open retried: %v", err)
	}
}

func TestDownloadAbortAfterFirstByte(t *testing.T) {
	ctx := context.Background()
	big := bytes.Repeat([]byte("x"), 64<<10) // past zip.Writer's buffer
	setup := func(t *testing.T, failSecond error) (*fixture, *device.Fake) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/M42/a.fit", big, t0)
		f.AddFile(emmc, "Preview/M42/b.fit", []byte("b"), t0)
		armed := false
		f.OnOpenReader = func(_, p string) {
			if p == "Preview/M42/b.fit" && !armed {
				armed = true
				f.FailNext(device.FakeOpOpenReader, 1, failSecond)
			}
		}
		return fx, f
	}

	t.Run("a refused entry after the first byte aborts", func(t *testing.T) {
		fx, _ := setup(t, &os.PathError{Op: "open", Path: "b.fit", Err: os.ErrPermission})
		var w sink
		err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"M42"}, &w)
		if !errors.Is(err, ErrMidStream) {
			t.Fatalf("error = %v, want ErrMidStream", err)
		}
		e := wantKind(t, err, ErrMidStream)
		if !errors.Is(err, ErrDenied) || e.Rig != "r1" {
			t.Errorf("error %#v should keep its cause and rig", e)
		}
		if w.begun != 1 || w.body.Len() == 0 {
			t.Errorf("Begin %d, %d bytes; want the response begun", w.begun, w.body.Len())
		}
		// What was sent is not a valid zip: the client must see it fail.
		if _, err := zip.NewReader(bytes.NewReader(w.body.Bytes()), int64(w.body.Len())); err == nil {
			t.Error("the truncated body reads as a complete zip")
		}
	})

	t.Run("an entry's open is still retried after the first byte", func(t *testing.T) {
		fx, _ := setup(t, errNetworkForTest())
		var w sink
		if err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"M42"}, &w); err != nil {
			t.Fatal(err)
		}
		if got := zipContent(t, readZip(t, w.body.Bytes())["M42/b.fit"]); got != "b" {
			t.Errorf("b.fit = %q", got)
		}
	})

	t.Run("a write failure mid-stream is not retried", func(t *testing.T) {
		fx, _ := setup(t, nil)
		opens := fx.opens.Load()
		w := sink{failAt: 600 << 10}
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/huge.fit", bytes.Repeat([]byte("h"), 1<<20), t0)
		err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"huge.fit"}, &w)
		if !errors.Is(err, ErrMidStream) {
			t.Fatalf("error = %v, want ErrMidStream", err)
		}
		if got := fx.opens.Load() - opens; got > 1 {
			t.Errorf("opens = %d, want at most 1 (no retry after the first byte)", got)
		}
	})
}

func TestDownloadCheck(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", []byte("aaa"), t0)
	f.AddFile(emmc, "Preview/M42/b.fit", []byte("bb"), t0)

	info, err := fx.svc.DownloadCheck(ctx, "r1", emmc, "Preview", []string{"a.fit"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (DownloadInfo{Filename: "a.fit", Files: 1, Bytes: 3}); *info != want {
		t.Errorf("single = %+v, want %+v", *info, want)
	}
	info, err = fx.svc.DownloadCheck(ctx, "r1", emmc, "Preview", []string{"a.fit", "M42"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (DownloadInfo{Filename: "Rig One-Preview.zip", Files: 2, Bytes: 5, Zipped: true}); *info != want {
		t.Errorf("zip = %+v, want %+v", *info, want)
	}
	if got := f.ContentReads(); got != 0 {
		t.Errorf("content reads = %d, want 0 (a check reads no file)", got)
	}

	// A vanished name, and an unreachable rig, are ordinary errors.
	_, err = fx.svc.DownloadCheck(ctx, "r1", emmc, "Preview", []string{"gone.fit"})
	wantKind(t, err, ErrNotFound)
	f.FailNext(device.FakeOpListDir, 4, errNetworkForTest())
	_, err = fx.svc.DownloadCheck(ctx, "r1", emmc, "Preview", []string{"a.fit"})
	e := wantKind(t, err, ErrUnreachable)
	if !strings.Contains(e.Msg, "Rig One is unreachable") {
		t.Errorf("message = %q", e.Msg)
	}

	// Busy while another operation holds the lock.
	unlock, err := fx.svc.lockOp(ctx, "Delete", "r2")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.svc.DownloadCheck(ctx, "r1", emmc, "Preview", []string{"a.fit"})
	if e := wantKind(t, err, ErrBusy); !strings.HasPrefix(e.Msg, "Delete on Rig Two is running") {
		t.Errorf("busy message = %q", e.Msg)
	}
	var w sink
	wantKind(t, fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"a.fit"}, &w), ErrBusy)
	unlock()
	if w.begun != 0 {
		t.Error("a busy download began its response")
	}
}

func TestDownloadCancel(t *testing.T) {
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", bytes.Repeat([]byte("a"), 1<<20), t0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelSink{cancel: cancel}
	err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"a.fit"}, w)
	wantKind(t, err, ErrCancelled)
	if w.writes != 1 {
		t.Errorf("writes = %d, want 1 (the stream stops at the next chunk)", w.writes)
	}
	// The lock is free again.
	if _, err := fx.svc.DownloadCheck(context.Background(), "r1", emmc, "Preview", []string{"a.fit"}); err != nil {
		t.Errorf("check after cancel: %v", err)
	}
}

// cancelSink cancels the request on its first write.
type cancelSink struct {
	cancel func()
	writes int
}

func (s *cancelSink) Begin(DownloadInfo) {}
func (s *cancelSink) Write(p []byte) (int, error) {
	s.writes++
	s.cancel()
	return len(p), nil
}

func TestMarkersInvisibleZip(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.ListMarkers(true)
	f.AddFile(udisk, "ASIAIR/Autorun/x.fit", []byte("x"), t0)
	f.SetMarker(udisk, "ASIAIR/"+model.MediaMarkerName, []byte("id"))
	f.SetMarker(udisk, "ASIAIR/Autorun/"+strings.ToUpper(model.IdentityMarkerName), []byte("id"))

	var w sink
	if err := fx.svc.Download(ctx, "r1", udisk, "", []string{"ASIAIR"}, &w); err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range readZip(t, w.body.Bytes()) {
		names = append(names, n)
	}
	if want := []string{"ASIAIR/", "ASIAIR/Autorun/", "ASIAIR/Autorun/x.fit"}; !sameSet(names, want) {
		t.Errorf("zip entries = %v, want %v", names, want)
	}
	// A selection naming a marker is refused.
	w = sink{}
	err := fx.svc.Download(ctx, "r1", udisk, "ASIAIR", []string{model.MediaMarkerName}, &w)
	wantKind(t, err, ErrBadPath)
	if w.begun != 0 {
		t.Error("a refused download began")
	}
}

func TestWalkFailsWholeZip(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/M42/a.fit", []byte("a"), t0)
	f.AddFile(emmc, "Preview/M42/sub/b.fit", []byte("b"), t0)
	f.OnListDir = func(_, dir string) {
		if dir == "Preview/M42/sub" {
			f.FailNext(device.FakeOpListDir, 4, errNetworkForTest())
		}
	}
	var w sink
	err := fx.svc.Download(ctx, "r1", emmc, "Preview", []string{"M42"}, &w)
	wantKind(t, err, ErrUnreachable)
	if errors.Is(err, ErrMidStream) || w.begun != 0 || w.body.Len() != 0 {
		t.Errorf("error %v, Begin %d, %d bytes; want nothing sent", err, w.begun, w.body.Len())
	}
	if got := f.ContentReads(); got != 0 {
		t.Errorf("content reads = %d, want 0 (no zip from a partial walk)", got)
	}
	_, err = fx.svc.DownloadCheck(ctx, "r1", emmc, "Preview", []string{"M42"})
	wantKind(t, err, ErrUnreachable)
}
