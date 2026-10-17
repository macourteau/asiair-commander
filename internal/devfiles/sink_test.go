package devfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
)

// logRecorder is a slog handler keeping each record as its message followed
// by its attributes in order, so a test can assert a log line field by field.
// Values that vary from run to run (durations, temp names) read "*".
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *logRecorder) WithGroup(string) slog.Handler            { return h }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	parts := []string{r.Level.String(), r.Message}
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.String()
		switch {
		case a.Value.Kind() == slog.KindDuration:
			v = "*"
		case strings.HasSuffix(v, tmpSuffix):
			v = "*" + tmpSuffix
		}
		parts = append(parts, a.Key+"="+v)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, strings.Join(parts, " "))
	return nil
}

// shareLines are the recorded lines that name a share, the ones the copy
// engine writes; session housekeeping is left out.
func (h *logRecorder) shareLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, l := range h.lines {
		if strings.Contains(l, " share=") {
			out = append(out, l)
		}
	}
	return out
}

func recordLogs(fx *fixture) *logRecorder {
	h := &logRecorder{}
	fx.svc.log = slog.New(h)
	return h
}

// TestSinkCopyLogLinesUnchanged pins every line a copy plan and run log, field
// by field and in order, across a folder made, a retry, an overwrite, a
// destination that appeared and a timestamp the device refused: the copy's
// destination half lives in the sink, and its lines, "file written" with its
// from_* fields included, read as they always have.
func TestSinkCopyLogLinesUnchanged(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/a.fit", []byte("aaa"), srcTime)
	src.AddFile(emmc, "Preview/b.fit", []byte("bbbb"), srcTime)
	src.AddFile(emmc, "Preview/c.fit", []byte("cc"), srcTime)
	src.AddFile(emmc, "Preview/sub/d.fit", []byte("d"), srcTime)
	dst.AddFile(emmc, "Dest/b.fit", []byte("old"), t0)
	from, to := r1r2("Preview", "Dest")

	h := recordLogs(fx)
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{
		From: CopySource{CopyPlace: from, Names: []string{"a.fit", "b.fit", "c.fit", "sub"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	dst.AddFile(emmc, "Dest/c.fit", []byte("theirs"), t0)
	dst.FailNext(device.FakeOpWrite, 1, errNetworkForTest())
	dst.FailChtimes(emmc, &os.PathError{Op: "chtimes", Path: "x", Err: os.ErrPermission})
	if _, err := runCopy(t, fx, runFromPlan(from, to, plan, "b.fit")); err != nil {
		t.Fatal(err)
	}

	chtimesErr := "chtimes x: permission denied"
	written := func(p, fromPath string, n int, overwrite bool) string {
		return fmt.Sprintf("INFO devfiles: file written rig=r2 share=%s op=Copy path=%s from_rig=r1 from_share=%s from_path=%s bytes=%d overwrite=%t",
			emmc, p, emmc, fromPath, n, overwrite)
	}
	mtime := func(p string) string {
		return fmt.Sprintf("WARN devfiles: copy mtime not preserved rig=r2 share=%s op=Copy path=%s error=%s", emmc, p, chtimesErr)
	}
	want := []string{
		"DEBUG devfiles: listed rig=r1 share=" + emmc + " op=Copy check dir=Preview entries=4",
		"DEBUG devfiles: listed rig=r1 share=" + emmc + " op=Copy check dir=Preview/sub entries=1",
		"DEBUG devfiles: listed rig=r2 share=" + emmc + " op=Copy check dir=Dest entries=1",
		"DEBUG devfiles: copy plan rig=r1 share=" + emmc + " op=Copy check to_rig=r2 to_share=" + emmc +
			" to_dir=Dest files=4 bytes=10 dirs=1 to_dir_missing=0 conflicts=1 watched_frames=0 watched_previews=0",
		"INFO devfiles: copy started rig=r1 share=" + emmc + " op=Copy from_dir=Preview to_rig=r2 to_share=" + emmc +
			" to_dir=Dest files=4 bytes=10 dirs=1 overwrites=1 watched_frames=0 watched_previews=0",
		"INFO devfiles: folder created rig=r2 share=" + emmc + " op=Copy path=Dest/sub",
		"WARN devfiles: retrying rig=r2 share=" + emmc + " op=Copy attempt=1 backoff=* error=connection reset: use of closed network connection",
		written("Dest/a.fit", "Preview/a.fit", 3, false),
		mtime("Dest/a.fit"),
		"INFO devfiles: file removed for overwrite rig=r2 share=" + emmc + " op=Copy path=Dest/b.fit size=3 mtime=" + t0.String(),
		written("Dest/b.fit", "Preview/b.fit", 4, true),
		mtime("Dest/b.fit"),
		"INFO devfiles: copy skipped: destination appeared rig=r2 share=" + emmc + " op=Copy path=Dest/c.fit",
		written("Dest/sub/d.fit", "Preview/sub/d.fit", 1, false),
		mtime("Dest/sub/d.fit"),
		"INFO devfiles: copy finished rig=r1 share=" + emmc + " op=Copy to_rig=r2 done=3 skipped=1 failed=0 aborted= duration=*",
	}
	if got := h.shareLines(); !reflect.DeepEqual(got, want) {
		t.Errorf("log lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestSinkPlanDestMatchesCopy feeds planDest the destination and entries a copy
// plan checks, from the copy's own walk and from hand-built file entries (the
// shape a single-file caller builds), and gets the copy's plan both times.
func TestSinkPlanDestMatchesCopy(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/a.fit", []byte("aaaa"), srcTime)
	src.AddFile(emmc, "Preview/M42/b.fit", []byte("bb"), srcTime)
	dst.AddFile(emmc, "Dest/A.fit", []byte("old"), t0)
	dst.AddFile(emmc, "Dest/m42/x.fit", []byte("x"), t0)
	from, to := r1r2("Preview", "Dest")

	copyPlan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{
		From: CopySource{CopyPlace: from, Names: []string{"a.fit", "M42"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	dv, err := fx.svc.destCheck(ctx, opCopyCheck, to)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fx.svc.walk(ctx, opCopyCheck, from.Rig, from.Share, from.Dir, []string{"a.fit", "M42"})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := fx.svc.planDest(ctx, opCopyCheck, to, false, dv, entries, fx.svc.copyWords(from.Rig))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct, copyPlan) {
		t.Errorf("planDest = %+v\nCopyPlan = %+v", direct, copyPlan)
	}
	if len(copyPlan.Conflicts) != 1 || copyPlan.Conflicts[0].Rel != "a.fit" {
		t.Fatalf("conflicts = %+v, want a.fit's", copyPlan.Conflicts)
	}

	one, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"a.fit"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	built, err := fx.svc.planDest(ctx, opCopyCheck, to, false, dv,
		[]entry{{Rel: "a.fit", Size: 4, Mtime: srcTime}}, fx.svc.copyWords(from.Rig))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built, one) {
		t.Errorf("planDest of one built entry = %+v\nCopyPlan = %+v", built, one)
	}
}

// TestSinkWriteItemNamesItsOp runs the sink under another operation name with
// a one-rig source: the write, its retry, its overwrite and its log lines name
// that operation and the source's own attributes, never copy's.
func TestSinkWriteItemNamesItsOp(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	dst := fx.dev(ip2)
	dst.AddFile(emmc, "Dest/a.fit", []byte("old"), t0)
	dst.FailNext(device.FakeOpCreateNew, 1, errNetworkForTest())
	fi := stat(t, dst, emmc, "Dest/a.fit")
	body := strings.Repeat("u", copyBufSize+3)
	to := CopyPlace{Rig: "r2", Share: emmc, Dir: "Dest"}

	h := recordLogs(fx)
	k := &sinkRun{s: fx.svc, ctx: ctx, op: "Upload", words: uploadWords(), to: to, ensured: map[string]bool{},
		overwrite: map[string]CopyOverwrite{"a.fit": {Rel: "a.fit", DstSize: fi.Size, DstMtime: fi.Mtime.Unix()}}}
	var opens, sessions int
	var progressed int64
	res, err := k.writeItem("a.fit", source{
		rigs: []string{"r2"},
		open: func(ss []*session) (io.ReadCloser, time.Time, *itemResult, error) {
			opens++
			sessions = len(ss)
			return io.NopCloser(strings.NewReader(body)), srcTime, nil, nil
		},
		readFailure: func(err error) error { return err },
		progress:    func(n int64) error { progressed = n; return nil },
		logAttrs:    []any{"staging_id", "s1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != outcomeCopied || res.code != "" {
		t.Errorf("result = %+v, want copied", res)
	}
	if opens != 2 || sessions != 1 {
		t.Errorf("opens = %d, sessions = %d; want a reopen per attempt on one session", opens, sessions)
	}
	if progressed != int64(len(body)) {
		t.Errorf("progress reached %d bytes, want %d", progressed, len(body))
	}
	if got := readFile(t, dst, emmc, "Dest/a.fit"); got != body {
		t.Error("destination content differs")
	}
	if got := stat(t, dst, emmc, "Dest/a.fit"); !got.Mtime.Equal(srcTime) {
		t.Errorf("mtime = %v, want %v", got.Mtime, srcTime)
	}
	noTemps(t, dst, emmc)

	want := []string{
		"WARN devfiles: retrying rig=r2 share=" + emmc + " op=Upload attempt=1 backoff=* error=connection reset: use of closed network connection",
		"INFO devfiles: file removed for overwrite rig=r2 share=" + emmc + " op=Upload path=Dest/a.fit size=3 mtime=" + t0.String(),
		fmt.Sprintf("INFO devfiles: file written rig=r2 share=%s op=Upload path=Dest/a.fit staging_id=s1 bytes=%d overwrite=true",
			emmc, len(body)),
	}
	if got := h.shareLines(); !reflect.DeepEqual(got, want) {
		t.Errorf("log lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestSinkPlanUploadMatchesCopy plans the same file into the same destination
// from both callers of planDest, a copy from a rig and an upload of a staged
// file with the same name, size and mtime, and gets equal plans: the
// conflict, its ledger and the watched counts come from the one sink.
func TestSinkPlanUploadMatchesCopy(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		to   CopyPlace
	}{
		{"unwatched, with a conflict", CopyPlace{Rig: "r2", Share: emmc, Dir: "Dest"}},
		{"watched", CopyPlace{Rig: "r2", Share: emmc, Dir: "Autorun/Light"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUploadFixture(t)
			u.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("aaaa"), srcTime)
			u.dev(ip2).AddFile(emmc, joinRel(c.to.Dir, "A.fit"), []byte("old"), t0)
			from := CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}

			copyPlan, err := u.svc.CopyPlan(ctx, CopyPlanRequest{
				From: CopySource{CopyPlace: from, Names: []string{"a.fit"}}, To: c.to})
			if err != nil {
				t.Fatal(err)
			}
			id := u.stage(t, owner1, "a.fit", "aaaa", srcTime)
			up, err := u.svc.UploadPlan(ctx, owner1, UploadPlanRequest{ID: id, To: c.to})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(&up.CopyPlan, copyPlan) {
				t.Errorf("upload plan = %+v\ncopy plan   = %+v", up.CopyPlan, *copyPlan)
			}
			if len(copyPlan.Conflicts) != 1 {
				t.Errorf("conflicts = %+v, want one", copyPlan.Conflicts)
			}
		})
	}
}

// failStore fails every read: the reserved-path rule must not need one.
type failStore struct{}

var errStoreDown = errors.New("store down")

func (failStore) GetFrame(context.Context, string, string, string) (*model.Frame, bool, error) {
	return nil, false, errStoreDown
}
func (failStore) GetMeta(context.Context, string) (string, bool, error) {
	return "", false, errStoreDown
}
func (failStore) ResidentLeads(context.Context, string, string) ([]string, error) {
	return nil, errStoreDown
}

// TestPlanReservedRootName: a copy or an upload of a file that would stand
// where a reserved path must go, at any depth, refuses the whole plan with
// watched, naming every offending path with its operation's fix, from the
// configuration alone; a configuration change between a copy's plan and run
// fails that item at write time and the run goes on.
func TestPlanReservedRootName(t *testing.T) {
	ctx := context.Background()
	fooBar := func(p *model.RigProfile) {
		p.Sources = append(slices.Clone(p.Sources), model.Source{Share: emmc, Roots: []string{"Foo/Bar"}, Disabled: true})
	}
	cases := []struct {
		name  string
		share string
		dir   string // the destination folder
		file  string // the file written into it
		root  string // the reserved path it would stand at
		edit  func(*model.RigProfile)
	}{
		{"a root's name at an eMMC root", emmc, "", "AUTORUN", "Autorun", nil},
		// ASIAIR holds the nested root, which names the refusal more precisely.
		{"ASIAIR at a Udisk root", udisk, "", "asiair", "ASIAIR/Autorun", nil},
		{"ASIAIR at a Udisk root with no source", udisk, "", "asiair", "ASIAIR",
			func(p *model.RigProfile) { p.Sources = nil }},
		{"the nested root on a Udisk", udisk, "ASIAIR", "Autorun", "ASIAIR/Autorun", nil},
		{"a multi-segment root", emmc, "Foo", "Bar", "Foo/Bar", fooBar},
		{"a multi-segment root's first level", emmc, "", "Foo", "Foo/Bar", fooBar},
	}
	for _, c := range cases {
		t.Run("upload: "+c.name, func(t *testing.T) {
			u := newUploadFixture(t)
			u.svc.store = failStore{}
			if c.edit != nil {
				u.editRig("r2", c.edit)
			}
			to := CopyPlace{Rig: "r2", Share: c.share, Dir: c.dir}
			id := u.stage(t, owner1, c.file, "x", srcTime)
			muts := u.dev(ip2).Mutations()
			_, err := u.svc.UploadPlan(ctx, owner1, UploadPlanRequest{ID: id, To: to})
			e := wantKind(t, err, ErrWatched)
			want := c.file + " would stand where " + c.share + "/" + c.root + " must go. Rename " + c.file +
				" on your computer and choose it again."
			if e.Msg != want {
				t.Errorf("message %q\nwant      %q", e.Msg, want)
			}
			// The run refuses it before the lock, from the configuration too.
			l, err := u.upload(owner1, UploadRequest{ID: id, To: to})
			if e := wantKind(t, err, ErrWatched); e.Msg != want || len(l.all) != 0 {
				t.Errorf("run: %q, lines %v", e.Msg, l.all)
			}
			if got := u.dev(ip2).Mutations(); got != muts {
				t.Errorf("mutations = %d, want %d", got, muts)
			}
		})
		t.Run("copy: "+c.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.svc.store = failStore{}
			if c.edit != nil {
				fx.editRig("r2", c.edit)
			}
			fx.dev(ip1).AddFile(emmc, "Preview/"+c.file, []byte("x"), srcTime)
			muts := fx.dev(ip2).Mutations()
			_, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{
				From: CopySource{CopyPlace: CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}, Names: []string{c.file}},
				To:   CopyPlace{Rig: "r2", Share: c.share, Dir: c.dir}})
			e := wantKind(t, err, ErrWatched)
			want := c.file + " would stand where " + c.share + "/" + c.root + " must go. Leave " + c.file +
				" out of the selection, or rename it on Rig One first."
			if e.Msg != want {
				t.Errorf("message %q\nwant      %q", e.Msg, want)
			}
			if got := fx.dev(ip2).Mutations(); got != muts {
				t.Errorf("mutations = %d, want %d", got, muts)
			}
		})
	}

	t.Run("copy: at depth, every path listed and capped", func(t *testing.T) {
		fx := newFixture(t)
		fx.svc.store = failStore{}
		var roots, names []string
		for i := range reservedListMax + 2 {
			roots = append(roots, fmt.Sprintf("Sel/R%02d", i))
			names = append(names, fmt.Sprintf("R%02d", i))
			fx.dev(ip1).AddFile(emmc, fmt.Sprintf("Preview/Sel/R%02d", i), []byte("x"), srcTime)
		}
		fx.editRig("r2", func(p *model.RigProfile) {
			p.Sources = append(slices.Clone(p.Sources), model.Source{Share: emmc, Roots: roots})
		})
		_, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{
			From: CopySource{CopyPlace: CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}, Names: []string{"Sel"}},
			To:   CopyPlace{Rig: "r2", Share: emmc}})
		e := wantKind(t, err, ErrWatched)
		for i, n := range names {
			listed := strings.Contains(e.Msg, "Leave Sel/"+n+" out of the selection")
			if listed != (i < reservedListMax) {
				t.Errorf("%s listed = %v", n, listed)
			}
		}
		if !strings.HasSuffix(e.Msg, " And 2 more like these.") {
			t.Errorf("message %q does not count the rest", e.Msg)
		}
	})

	t.Run("copy: a configuration change after the plan fails the item", func(t *testing.T) {
		fx := newFixture(t)
		fx.svc.store = failStore{}
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("a"), srcTime)
		fx.dev(ip1).AddFile(emmc, "Preview/b.fit", []byte("b"), srcTime)
		fx.dev(ip2).AddDir(emmc, "Dest", t0)
		from, to := r1r2("Preview", "Dest")
		plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{
			From: CopySource{CopyPlace: from, Names: []string{"a.fit", "b.fit"}}, To: to})
		if err != nil {
			t.Fatal(err)
		}
		fx.editRig("r2", func(p *model.RigProfile) {
			p.Sources = append(slices.Clone(p.Sources), model.Source{Share: emmc, Roots: []string{"Dest/a.fit"}, Disabled: true})
		})
		l, err := runCopy(t, fx, runFromPlan(from, to, plan))
		if err != nil {
			t.Fatal(err)
		}
		it := l.item(t, "a.fit")
		want := "a.fit would stand where " + emmc + "/Dest/a.fit must go. Leave a.fit out of the selection, or rename it on Rig One first."
		if it.Outcome != outcomeFailed || it.Code != codeWatched || it.Message != want {
			t.Errorf("item = %+v", it)
		}
		if s := l.summary(t); s.Done != 1 || s.Failed != 1 || s.Aborted != "" {
			t.Errorf("summary = %+v", s)
		}
		tr := tree(t, fx.dev(ip2), emmc)
		if _, ok := tr["Dest/a.fit"]; ok {
			t.Error("the reserved path was written")
		}
		if tr["Dest/b.fit"] != "b" {
			t.Errorf("tree = %v, want b.fit copied", tr)
		}
		noTemps(t, fx.dev(ip2), emmc)
	})
}
