package devfiles

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// srcTime is the source files' mtime: older than the fake's server clock (t0),
// which stamps every file the copy creates, so a preserved mtime shows.
var srcTime = t0.Add(-48 * time.Hour)

// copyHooks wraps the fixture's devices so a test can act inside a run: just
// before a rename, after each source Read, on a source Read that reports
// io.EOF, and count Chtimes calls.
type copyHooks struct {
	beforeRename func(share, oldPath, newPath string)
	afterRead    func(n int64)
	atEOF        func()
	reads        atomic.Int64
	chtimes      atomic.Int64
	// creates counts CreateNew calls; afterWrite runs after each successful
	// Write on a created file, with the count of such writes so far.
	creates    atomic.Int64
	writes     atomic.Int64
	afterWrite func(n int64)
}

func installHooks(fx *fixture) *copyHooks {
	h := &copyHooks{}
	inner := fx.svc.newDevice
	fx.svc.newDevice = func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
		return &hookDevice{Device: inner(ep, user, pass, timeout), h: h}
	}
	return h
}

type hookDevice struct {
	device.Device
	h *copyHooks
}

func (d *hookDevice) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &hookSession{Session: s, h: d.h}, nil
}

type hookSession struct {
	device.Session
	h *copyHooks
}

func (s *hookSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return &hookShare{Share: sh, h: s.h}, nil
}

type hookShare struct {
	device.Share
	h *copyHooks
}

func (s *hookShare) Rename(oldPath, newPath string) error {
	if f := s.h.beforeRename; f != nil {
		f(s.Name(), oldPath, newPath)
	}
	return s.Share.Rename(oldPath, newPath)
}

func (s *hookShare) Chtimes(p string, atime, mtime time.Time) error {
	s.h.chtimes.Add(1)
	return s.Share.Chtimes(p, atime, mtime)
}

func (s *hookShare) CreateNew(p string) (io.WriteCloser, error) {
	s.h.creates.Add(1)
	w, err := s.Share.CreateNew(p)
	if err != nil {
		return nil, err
	}
	return &hookWriter{WriteCloser: w, h: s.h}, nil
}

type hookWriter struct {
	io.WriteCloser
	h *copyHooks
}

func (w *hookWriter) Write(b []byte) (int, error) {
	n, err := w.WriteCloser.Write(b)
	if err == nil {
		k := w.h.writes.Add(1)
		if f := w.h.afterWrite; f != nil {
			f(k)
		}
	}
	return n, err
}

func (s *hookShare) OpenReader(p string) (io.ReadCloser, error) {
	rc, err := s.Share.OpenReader(p)
	if err != nil {
		return nil, err
	}
	return &hookReader{ReadCloser: rc, h: s.h}, nil
}

type hookReader struct {
	io.ReadCloser
	h *copyHooks
}

func (r *hookReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	k := r.h.reads.Add(1)
	if f := r.h.afterRead; f != nil {
		f(k)
	}
	if f := r.h.atEOF; f != nil && err == io.EOF {
		f()
	}
	return n, err
}

// lines collects a run's stream.
type lines struct {
	all []any
}

func (l *lines) emit(line any) error {
	l.all = append(l.all, line)
	return nil
}

func (l *lines) items() []ItemLine {
	var out []ItemLine
	for _, x := range l.all {
		if it, ok := x.(ItemLine); ok {
			out = append(out, it)
		}
	}
	return out
}

func (l *lines) summary(t *testing.T) SummaryLine {
	t.Helper()
	if len(l.all) == 0 {
		t.Fatal("no lines")
	}
	s, ok := l.all[len(l.all)-1].(SummaryLine)
	if !ok {
		t.Fatalf("last line = %#v, want a summary", l.all[len(l.all)-1])
	}
	return s
}

func (l *lines) item(t *testing.T, rel string) ItemLine {
	t.Helper()
	for _, it := range l.items() {
		if it.Rel == rel {
			return it
		}
	}
	t.Fatalf("no item line for %s in %#v", rel, l.all)
	return ItemLine{}
}

func runCopy(t *testing.T, fx *fixture, req CopyRequest) (*lines, error) {
	t.Helper()
	l := &lines{}
	err := fx.svc.Copy(context.Background(), req, l.emit)
	return l, err
}

// tree is every entry of a share as path -> content, "/" for a folder, read
// through a fresh fake session.
func tree(t *testing.T, f *device.Fake, share string) map[string]string {
	t.Helper()
	s, _ := f.Open(context.Background())
	sh, err := s.Mount(share)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var walkDir func(dir string)
	walkDir = func(dir string) {
		fis, err := sh.ListDir(dir)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", dir, err)
		}
		for _, fi := range fis {
			p := joinRel(dir, fi.Name)
			if fi.IsDir {
				out[p] = "/"
				walkDir(p)
				continue
			}
			rc, err := sh.OpenReader(p)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			out[p] = string(b)
		}
	}
	walkDir("")
	return out
}

func noTemps(t *testing.T, f *device.Fake, share string) {
	t.Helper()
	for p := range tree(t, f, share) {
		if strings.HasSuffix(p, tmpSuffix) {
			t.Errorf("temp file left behind: %s/%s", share, p)
		}
	}
}

func stat(t *testing.T, f *device.Fake, share, p string) device.FileInfo {
	t.Helper()
	s, _ := f.Open(context.Background())
	sh, _ := s.Mount(share)
	fi, err := sh.Stat(p)
	if err != nil {
		t.Fatalf("Stat(%s/%s): %v", share, p, err)
	}
	return fi
}

func readFile(t *testing.T, f *device.Fake, share, p string) string {
	t.Helper()
	s, _ := f.Open(context.Background())
	sh, _ := s.Mount(share)
	rc, err := sh.OpenReader(p)
	if err != nil {
		t.Fatalf("OpenReader(%s/%s): %v", share, p, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func setExclude(fx *fixture, exts ...string) {
	cur := fx.cfg.Load()
	next := *cur
	next.ExcludeExtensions = exts
	fx.cfg.Store(&next)
}

// withStore gives the service a temp-dir sqlite store.
func upsertCopyFrame(t *testing.T, st store.Store, f model.Frame) {
	t.Helper()
	if err := st.UpsertFrame(context.Background(), &f); err != nil {
		t.Fatalf("UpsertFrame: %v", err)
	}
}

func r1r2(fromDir, toDir string) (CopyPlace, CopyPlace) {
	return CopyPlace{Rig: "r1", Share: emmc, Dir: fromDir}, CopyPlace{Rig: "r2", Share: emmc, Dir: toDir}
}

// runFromPlan builds the run request a client sends for plan with every file
// conflict skipped except those in overwrite, and type conflicts dropped.
func runFromPlan(from, to CopyPlace, plan *CopyPlan, overwrite ...string) CopyRequest {
	req := CopyRequest{From: from, To: to, Dirs: []string{}, Items: []CopyItem{}, Overwrite: []CopyOverwrite{}}
	var types []string
	conflicts := map[string]CopyConflict{}
	for _, c := range plan.Conflicts {
		if c.Kind == conflictType {
			types = append(types, c.Rel)
		} else {
			conflicts[c.Rel] = c
		}
	}
	inType := func(rel string) bool {
		for _, ty := range types {
			if sameOrInside(rel, ty) {
				return true
			}
		}
		return false
	}
	for _, it := range plan.Items {
		if inType(it.Rel) {
			continue
		}
		if c, ok := conflicts[it.Rel]; ok {
			if !contains(overwrite, it.Rel) {
				continue
			}
			req.Overwrite = append(req.Overwrite, CopyOverwrite{Rel: c.Rel, DstSize: c.DstSize, DstMtime: c.DstMtime})
		}
		req.Items = append(req.Items, CopyItem{Rel: it.Rel, Size: it.Size})
	}
	for _, d := range plan.Dirs {
		if !inType(d) {
			req.Dirs = append(req.Dirs, d)
		}
	}
	return req
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestCopyPlanShape(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/M42/a.fit", []byte("aaaa"), srcTime)
	src.AddFile(emmc, "Preview/M42/flats/f1.fit", []byte("f1"), srcTime)
	src.AddFile(emmc, "Preview/NGC/x.fit", []byte("x"), srcTime)
	src.AddFile(emmc, "Preview/ref.fit", []byte("ref"), srcTime)
	src.AddFile(emmc, "Preview/other.fit", []byte("not selected"), srcTime)
	// The destination's names differ in case: FAT matches them anyway.
	dst.AddFile(emmc, "Dest/m42/A.fit", []byte("old"), t0)
	dst.AddFile(emmc, "Dest/NGC", []byte("a file"), t0)
	dst.AddFile(emmc, "Dest/ref.fit/inner", []byte("a folder"), t0)

	from, to := r1r2("Preview", "Dest")
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42", "NGC", "ref.fit"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	want := &CopyPlan{
		Files: 4, Bytes: 10,
		Dirs: []string{"M42/flats"},
		Items: []CopyPlanItem{
			{Rel: "M42/a.fit", Size: 4}, {Rel: "M42/flats/f1.fit", Size: 2},
			{Rel: "NGC/x.fit", Size: 1}, {Rel: "ref.fit", Size: 3},
		},
		Conflicts: []CopyConflict{
			{Rel: "M42/a.fit", Kind: "file", SrcSize: 4, SrcMtime: srcTime.Unix(), DstSize: 3, DstMtime: t0.Unix()},
			{Rel: "NGC", Kind: "type", DstSize: 6, DstMtime: t0.Unix()},
			{Rel: "ref.fit", Kind: "type", SrcSize: 3, SrcMtime: srcTime.Unix()},
		},
	}
	if !reflect.DeepEqual(plan, want) {
		t.Errorf("plan =\n%+v\nwant\n%+v", plan, want)
	}
	if got := dst.Mutations() + src.Mutations(); got != 0 {
		t.Errorf("mutations = %d, want 0 (a plan writes nothing)", got)
	}
}

func TestCopyPlanRefusals(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	fx.dev(ip1).AddFile(emmc, "Preview/M42/a.fit", []byte("a"), srcTime)
	fx.dev(ip1).AddDir(emmc, "Other", t0)
	plan := func(from CopyPlace, names []string, to CopyPlace, mirror bool) error {
		_, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: names}, To: to, Mirror: mirror})
		return err
	}
	same := CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}
	wantKind(t, plan(same, []string{"M42"}, same, false), ErrSameFolder)
	wantKind(t, plan(same, []string{"M42"}, CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview/M42"}, false), ErrIntoItself)
	wantKind(t, plan(same, []string{"M42"}, CopyPlace{Rig: "r1", Share: emmc, Dir: "preview/m42/sub"}, false), ErrIntoItself)
	wantKind(t, plan(same, []string{"M42"}, CopyPlace{Rig: "r2", Share: emmc, Dir: "Other"}, true), ErrBadPath)
	wantKind(t, plan(same, []string{"M42"}, CopyPlace{Rig: "r2", Share: "TF Images", Dir: ""}, false), ErrNoShare)
	wantKind(t, plan(same, []string{"M42"}, CopyPlace{Rig: "r3", Share: emmc}, false), ErrUnknownRig)
	wantKind(t, plan(same, []string{"../x"}, CopyPlace{Rig: "r2", Share: emmc}, false), ErrBadPath)

	fx.editRig("r2", func(p *model.RigProfile) { p.GUID = "" })
	wantKind(t, plan(same, []string{"M42"}, CopyPlace{Rig: "r2", Share: emmc}, false), ErrHostBound)

	// The run body cap: long names make a small selection too big to send.
	fx2 := newFixture(t)
	fx2.svc.maxFiles = 100_000
	long := strings.Repeat("n", 200)
	for i := range 16_000 {
		fx2.dev(ip1).AddFile(emmc, "Big/"+long+"/"+strings.Repeat("x", 10)+strconv.Itoa(i), []byte{}, srcTime)
	}
	from, to := r1r2("Big", "")
	_, err := fx2.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{long}}, To: to})
	wantKind(t, err, ErrTooMany)
}

func TestCopyPlanUnwatchedDestination(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	setExclude(fx, "jpg")
	st := withStore(t, fx)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Autorun/Light/M42/a.fit", []byte("frame"), srcTime)
	src.AddFile(emmc, "Autorun/Light/M42/a_thn.jpg", []byte("jpg"), srcTime)
	// Preview/ is not a configured root on r2: nothing landing there is archived.
	dst.AddFile(emmc, "Preview/M42/a.fit", []byte("other"), t0)
	upsertCopyFrame(t, st, model.Frame{RigID: "r2", SourceShare: emmc, SourcePath: "Preview/M42/a.fit",
		Size: 5, Mtime: t0.Unix(), State: model.StateVerified})

	from := CopyPlace{Rig: "r1", Share: emmc, Dir: "Autorun/Light"}
	to := CopyPlace{Rig: "r2", Share: emmc, Dir: "Preview"}
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Watched != (CopyWatched{}) {
		t.Errorf("watched = %+v, want zero counts", plan.Watched)
	}
	for _, it := range plan.Items {
		if it.Watched {
			t.Errorf("item %s watched, want false", it.Rel)
		}
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].DstLedger != "" {
		t.Errorf("conflicts = %+v, want one with an empty dst_ledger", plan.Conflicts)
	}
}

func TestCopyPlanPreviewCount(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	setExclude(fx, "jpg", "jpeg")
	src := fx.dev(ip1)
	for _, n := range []string{"a.fit", "a_thn.jpg", "b.fit", "orphan.jpg", "c.JPEG", "._a.fit"} {
		src.AddFile(emmc, "Preview/M42/"+n, []byte(n), srcTime)
	}
	fx.dev(ip2).AddDir(emmc, "Autorun/Light", t0)
	fx.editRig("r2", func(p *model.RigProfile) { p.KeepOriginals = true })

	from := CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}
	to := CopyPlace{Rig: "r2", Share: emmc, Dir: "Autorun/Light"}
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	// Every excluded-extension file counts as a preview, paired or not; the
	// AppleDouble file is junk to the harvester and counts as neither.
	want := CopyWatched{Frames: 2, Previews: 3, KeepsOriginals: true}
	if plan.Watched != want {
		t.Errorf("watched = %+v, want %+v", plan.Watched, want)
	}
	for _, it := range plan.Items {
		if !it.Watched {
			t.Errorf("item %s not watched", it.Rel)
		}
	}
	if !reflect.DeepEqual(plan.Dirs, []string{"M42"}) {
		t.Errorf("dirs = %v, want [M42]", plan.Dirs)
	}
}

func TestCopyPlanDstLedger(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	st := withStore(t, fx)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	names := []string{"archived.fit", "changed.fit", "norow.fit", "culled.fit", "nasgone.fit", "nasresized.fit"}
	for _, n := range names {
		src.AddFile(emmc, "Preview/"+n, []byte("new content"), srcTime)
		dst.AddFile(emmc, "Autorun/Light/"+n, []byte("device"), t0)
	}
	frame := func(name string, size int64) *model.Frame {
		return upsertFrame(t, st, "r2", emmc, "Autorun/Light/"+name, size, t0, model.StateVerified)
	}
	frame("archived.fit", 6)
	frame("changed.fit", 99) // the row describes another revision
	culled := frame("culled.fit", 6)
	if err := st.SetFrameMissing(ctx, culled.ID, t0.Unix()); err != nil {
		t.Fatal(err)
	}
	// Culled on the NAS since the last library scan: the row still reads archived.
	if err := os.Remove(frame("nasgone.fit", 6).DestPath); err != nil {
		t.Fatal(err)
	}
	writeNAS(t, frame("nasresized.fit", 6).DestPath, 7)

	from := CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}
	to := CopyPlace{Rig: "r2", Share: emmc, Dir: "Autorun/Light"}
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from,
		Names: names}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range plan.Conflicts {
		got[c.Rel] = c.DstLedger
	}
	want := map[string]string{"archived.fit": "archived", "changed.fit": "unarchived",
		"culled.fit": "unarchived", "norow.fit": "unarchived", "nasgone.fit": "unarchived",
		"nasresized.fit": "unarchived"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dst_ledger = %v, want %v", got, want)
	}
}

func TestCopyCrossRig(t *testing.T) {
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	big := bytes.Repeat([]byte("0123456789abcdef"), 3*copyBufSize/16+1234) // several chunks
	src.AddFile(emmc, "Preview/M42/big.fit", big, srcTime)
	src.AddDir(emmc, "Preview/M42/empty", srcTime)
	src.AddFile(emmc, "Preview/ref.fit", []byte("ref"), srcTime)
	dst.AddDir(emmc, "Dest", t0)

	from, to := r1r2("Preview", "Dest")
	plan, err := fx.svc.CopyPlan(context.Background(), CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42", "ref.fit"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	l, err := runCopy(t, fx, runFromPlan(from, to, plan))
	if err != nil {
		t.Fatal(err)
	}
	if s := l.summary(t); s != (SummaryLine{Type: "summary", Done: 2}) {
		t.Errorf("summary = %+v", s)
	}
	if len(l.items()) != 0 {
		t.Errorf("item lines = %+v, want none", l.items())
	}
	if got := readFile(t, dst, emmc, "Dest/M42/big.fit"); got != string(big) {
		t.Errorf("big.fit differs: %d bytes, want %d", len(got), len(big))
	}
	if fi := stat(t, dst, emmc, "Dest/M42/empty"); !fi.IsDir {
		t.Error("empty folder not created")
	}
	noTemps(t, dst, emmc)
	first, ok := l.all[0].(ProgressLine)
	if !ok || first.Current != "M42/big.fit" || first.Total != 2 || first.BytesTotal != int64(len(big)+3) {
		t.Errorf("first line = %+v", l.all[0])
	}
}

func TestCopySkipsAppearedDestination(t *testing.T) {
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/a.fit", []byte("source"), srcTime)
	src.AddFile(emmc, "Preview/b.fit", []byte("bbb"), srcTime)
	dst.AddDir(emmc, "Dest", t0)
	from, to := r1r2("Preview", "Dest")
	plan, err := fx.svc.CopyPlan(context.Background(), CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"a.fit", "b.fit"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v", plan.Conflicts)
	}
	// Between the plan and the run, someone writes a.fit.
	appearedAt := t0.Add(time.Minute)
	dst.AddFile(emmc, "Dest/a.fit", []byte("theirs"), appearedAt)

	l, err := runCopy(t, fx, runFromPlan(from, to, plan))
	if err != nil {
		t.Fatal(err)
	}
	it := l.item(t, "a.fit")
	if it.Outcome != "skipped" || it.Code != "appeared" || it.DstSize == nil || *it.DstSize != 6 ||
		it.DstMtime == nil || *it.DstMtime != appearedAt.Unix() {
		t.Errorf("item = %+v", it)
	}
	if got := readFile(t, dst, emmc, "Dest/a.fit"); got != "theirs" {
		t.Errorf("destination changed: %q", got)
	}
	if got := readFile(t, dst, emmc, "Dest/b.fit"); got != "bbb" {
		t.Errorf("b.fit = %q", got)
	}
	if s := l.summary(t); s.Done != 1 || s.Skipped != 1 {
		t.Errorf("summary = %+v", s)
	}
	noTemps(t, dst, emmc)
}

func TestCopyCommitCollisionSkips(t *testing.T) {
	fx := newFixture(t)
	h := installHooks(fx)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/a.fit", []byte("source"), srcTime)
	dst.AddDir(emmc, "Dest", t0)
	// The destination appears after the temp is written, just before the commit.
	h.beforeRename = func(share, oldPath, newPath string) {
		if strings.HasSuffix(oldPath, tmpSuffix) {
			dst.AddFile(emmc, newPath, []byte("raced in"), t0)
		}
	}
	from, to := r1r2("Preview", "Dest")
	l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: 6}}})
	if err != nil {
		t.Fatal(err)
	}
	if it := l.item(t, "a.fit"); it.Outcome != "skipped" || it.Code != "appeared" {
		t.Errorf("item = %+v", it)
	}
	if got := readFile(t, dst, emmc, "Dest/a.fit"); got != "raced in" {
		t.Errorf("destination replaced: %q", got)
	}
	noTemps(t, dst, emmc)
}

func TestOverwriteSkipsChangedDestination(t *testing.T) {
	setup := func(t *testing.T) (*fixture, *CopyPlan, CopyPlace, CopyPlace) {
		fx := newFixture(t)
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("new"), srcTime)
		fx.dev(ip2).AddFile(emmc, "Dest/a.fit", []byte("old"), t0)
		from, to := r1r2("Preview", "Dest")
		plan, err := fx.svc.CopyPlan(context.Background(), CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"a.fit"}}, To: to})
		if err != nil || len(plan.Conflicts) != 1 {
			t.Fatalf("plan = %+v, %v", plan, err)
		}
		return fx, plan, from, to
	}

	t.Run("an unchanged destination is overwritten", func(t *testing.T) {
		fx, plan, from, to := setup(t)
		// FAT's 2 s mtime resolution: a second's drift still counts as unchanged.
		fx.dev(ip2).SetMtime(emmc, "Dest/a.fit", t0.Add(time.Second))
		l, err := runCopy(t, fx, runFromPlan(from, to, plan, "a.fit"))
		if err != nil {
			t.Fatal(err)
		}
		if s := l.summary(t); s.Done != 1 {
			t.Errorf("summary = %+v", s)
		}
		if got := readFile(t, fx.dev(ip2), emmc, "Dest/a.fit"); got != "new" {
			t.Errorf("destination = %q, want new", got)
		}
		noTemps(t, fx.dev(ip2), emmc)
	})

	t.Run("a skipped conflict is never touched", func(t *testing.T) {
		fx, plan, from, to := setup(t)
		// The client omits a skipped file; an overwrite naming a file not sent is refused.
		req := runFromPlan(from, to, plan)
		if len(req.Items) != 0 {
			t.Fatalf("items = %+v", req.Items)
		}
		req.Overwrite = []CopyOverwrite{{Rel: "a.fit", DstSize: 3, DstMtime: t0.Unix()}}
		_, err := runCopy(t, fx, req)
		wantKind(t, err, ErrBadPath)
		if got := readFile(t, fx.dev(ip2), emmc, "Dest/a.fit"); got != "old" {
			t.Errorf("destination = %q", got)
		}
	})

	for _, c := range []struct {
		name   string
		change func(f *device.Fake)
		code   string
		dst    *int64
	}{
		{"size moved", func(f *device.Fake) { f.AddFile(emmc, "Dest/a.fit", []byte("longer"), t0) }, "changed", ptr(6)},
		{"mtime moved", func(f *device.Fake) { f.SetMtime(emmc, "Dest/a.fit", t0.Add(3*time.Second)) }, "changed", ptr(3)},
		{"a folder now", func(f *device.Fake) {
			f.RemoveFile(emmc, "Dest/a.fit")
			f.AddFile(emmc, "Dest/a.fit/inner", []byte("x"), t0)
		}, "type_conflict", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx, plan, from, to := setup(t)
			c.change(fx.dev(ip2))
			before := tree(t, fx.dev(ip2), emmc)
			l, err := runCopy(t, fx, runFromPlan(from, to, plan, "a.fit"))
			if err != nil {
				t.Fatal(err)
			}
			it := l.item(t, "a.fit")
			if it.Outcome != "skipped" || it.Code != c.code {
				t.Errorf("item = %+v, want skipped %s", it, c.code)
			}
			if (it.DstSize == nil) != (c.dst == nil) || (c.dst != nil && *it.DstSize != *c.dst) {
				t.Errorf("dst_size = %v, want %v", it.DstSize, c.dst)
			}
			if after := tree(t, fx.dev(ip2), emmc); !reflect.DeepEqual(before, after) {
				t.Errorf("destination changed: %v -> %v", before, after)
			}
		})
	}
}

func ptr(v int64) *int64 { return &v }

func TestCopyRetryLeavesNoTemp(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 2*copyBufSize+77)

	t.Run("two failures then success", func(t *testing.T) {
		fx := newFixture(t)
		h := installHooks(fx)
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", big, srcTime)
		fx.dev(ip2).AddDir(emmc, "Dest", t0)
		fx.dev(ip2).FailNext(device.FakeOpWrite, 2, errNetworkForTest())
		// Each retry deletes the earlier attempts' temps before writing its own,
		// so only the committing attempt's temp is there at the commit.
		h.beforeRename = func(share, _, _ string) {
			var temps []string
			for p := range tree(t, fx.dev(ip2), share) {
				if strings.HasSuffix(p, tmpSuffix) {
					temps = append(temps, p)
				}
			}
			if len(temps) != 1 {
				t.Errorf("temps at the commit = %v, want only the committing one", temps)
			}
		}
		from, to := r1r2("Preview", "Dest")
		l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: int64(len(big))}}})
		if err != nil {
			t.Fatal(err)
		}
		if s := l.summary(t); s != (SummaryLine{Type: "summary", Done: 1}) {
			t.Errorf("summary = %+v", s)
		}
		if got := readFile(t, fx.dev(ip2), emmc, "Dest/a.fit"); got != string(big) {
			t.Errorf("content differs")
		}
		noTemps(t, fx.dev(ip2), emmc)
	})

	t.Run("a final failure", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", big, srcTime)
		fx.dev(ip2).AddDir(emmc, "Dest", t0)
		fx.dev(ip2).FailNext(device.FakeOpWrite, 100, errNetworkForTest())
		from, to := r1r2("Preview", "Dest")
		l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: int64(len(big))}}})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Outcome != "failed" || it.Code != "unreachable" {
			t.Errorf("item = %+v", it)
		}
		if s := l.summary(t); s.Failed != 1 || s.Aborted != "unreachable" {
			t.Errorf("summary = %+v", s)
		}
		noTemps(t, fx.dev(ip2), emmc)
		if _, err := fx.dev(ip2).Open(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := tree(t, fx.dev(ip2), emmc); !reflect.DeepEqual(got, map[string]string{"Dest": "/"}) {
			t.Errorf("destination holds %v, want only Dest/", slices.Sorted(maps.Keys(got)))
		}
	})
}

func TestMarkersInvisibleCopy(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.ListMarkers(true)
	dst.ListMarkers(true)
	src.AddFile(udisk, "ASIAIR/Autorun/Light/a.fit", []byte("frame"), srcTime)
	s, _ := src.Open(ctx)
	sh, _ := s.Mount(udisk)
	if err := sh.WriteMarkerAt("ASIAIR", model.MediaMarkerName, []byte("rig one's stick")); err != nil {
		t.Fatal(err)
	}

	from := CopyPlace{Rig: "r1", Share: udisk, Dir: ""}
	to := CopyPlace{Rig: "r2", Share: udisk, Dir: ""}
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"ASIAIR"}}, To: to, Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		if isMarker(filepath.Base(it.Rel)) {
			t.Errorf("plan lists the marker: %s", it.Rel)
		}
	}
	if _, err := runCopy(t, fx, runFromPlan(from, to, plan)); err != nil {
		t.Fatal(err)
	}
	got := tree(t, dst, udisk)
	want := map[string]string{"ASIAIR": "/", "ASIAIR/Autorun": "/", "ASIAIR/Autorun/Light": "/", "ASIAIR/Autorun/Light/a.fit": "frame"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("destination = %v, want %v (no marker)", got, want)
	}

	// An echoed run naming a marker anywhere is refused before any device call.
	for _, req := range []CopyRequest{
		{From: from, To: to, Items: []CopyItem{{Rel: "ASIAIR/" + model.MediaMarkerName}}},
		{From: from, To: to, Items: []CopyItem{{Rel: "a/" + strings.ToUpper(model.IdentityMarkerName)}}},
		{From: from, To: to, Dirs: []string{model.MediaMarkerName}},
		{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}}, Overwrite: []CopyOverwrite{{Rel: model.IdentityMarkerName}}},
		{From: from, To: CopyPlace{Rig: "r2", Share: udisk, Dir: "ASIAIR/" + model.MediaMarkerName}, Items: []CopyItem{{Rel: "a.fit"}}},
	} {
		before := fx.opens.Load()
		_, err := runCopy(t, fx, req)
		wantKind(t, err, ErrBadPath)
		if fx.opens.Load() != before {
			t.Errorf("a refused run opened a session")
		}
	}
}

func TestUnreachableStopsRunCopy(t *testing.T) {
	for _, c := range []struct {
		name    string
		failing string // the IP whose device fails
		op      device.FakeOp
		rigName string
	}{
		{"destination", ip2, device.FakeOpCreateNew, "Rig Two"},
		{"source", ip1, device.FakeOpOpenReader, "Rig One"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t)
			for _, n := range []string{"a.fit", "b.fit", "c.fit"} {
				fx.dev(ip1).AddFile(emmc, "Preview/"+n, []byte(n), srcTime)
			}
			fx.dev(ip2).AddDir(emmc, "Dest", t0)
			from, to := r1r2("Preview", "Dest")
			req := CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}, {Rel: "b.fit"}, {Rel: "c.fit"}}}
			// a.fit copies; b.fit fails four times, armed as b.fit starts.
			var opensAtFailure int64
			l := &lines{}
			err := fx.svc.Copy(context.Background(), req, func(line any) error {
				switch line := line.(type) {
				case ProgressLine:
					if line.Current == "b.fit" {
						fx.dev(c.failing).FailNext(c.op, 4, errNetworkForTest())
					}
				case ItemLine:
					if line.Code == "unreachable" {
						opensAtFailure = fx.opens.Load()
					}
				}
				return l.emit(line)
			})
			if err != nil {
				t.Fatal(err)
			}
			b := l.item(t, "b.fit")
			if b.Outcome != "failed" || b.Code != "unreachable" || !strings.Contains(b.Message, c.rigName) {
				t.Errorf("b.fit = %+v, want unreachable naming %s", b, c.rigName)
			}
			cl := l.item(t, "c.fit")
			if cl.Outcome != "failed" || cl.Code != "not_attempted" {
				t.Errorf("c.fit = %+v, want not_attempted", cl)
			}
			if got := fx.opens.Load(); got != opensAtFailure {
				t.Errorf("opens = %d after the failure, want %d (no further Open)", got, opensAtFailure)
			}
			if s := l.summary(t); s != (SummaryLine{Type: "summary", Done: 1, Failed: 2, Aborted: "unreachable"}) {
				t.Errorf("summary = %+v", s)
			}
			// No progress line follows the failure: not_attempted items get no I/O.
			for _, line := range l.all[len(l.all)-3:] {
				if _, ok := line.(ProgressLine); ok {
					t.Errorf("progress after the abort: %+v", l.all)
				}
			}
		})
	}
}

func TestCopyRetryNestedItem(t *testing.T) {
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/M42/flats/f1.fit", []byte("f1"), srcTime)
	src.AddFile(emmc, "Preview/M42/flats/f2.fit", []byte("f2"), srcTime)
	dst.AddDir(emmc, "Dest", t0)
	h := installHooks(fx)
	from, to := r1r2("Preview", "Dest")
	plan, err := fx.svc.CopyPlan(context.Background(), CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	// f2's commit meets a sharing violation, so the first run reports it.
	h.beforeRename = func(_, _, newPath string) {
		if strings.HasSuffix(newPath, "f2.fit") {
			dst.FailNext(device.FakeOpRename, 1, device.SharingViolationLinkError("rename", "tmp", newPath))
		}
	}
	l, err := runCopy(t, fx, runFromPlan(from, to, plan))
	if err != nil {
		t.Fatal(err)
	}
	if it := l.item(t, "M42/flats/f2.fit"); it.Code != "in_use" {
		t.Fatalf("f2 = %+v", it)
	}
	h.beforeRename = nil

	// The report row's retry: one item with its ancestors, which now exist.
	for _, dirs := range [][]string{{"M42", "M42/flats"}, nil} {
		dst.RemoveFile(emmc, "Dest/M42/flats/f2.fit")
		l, err = runCopy(t, fx, CopyRequest{From: from, To: to, Dirs: dirs, Items: []CopyItem{{Rel: "M42/flats/f2.fit", Size: 2}}})
		if err != nil {
			t.Fatal(err)
		}
		if s := l.summary(t); s.Done != 1 {
			t.Errorf("dirs %v: summary = %+v, lines %+v", dirs, s, l.all)
		}
		if got := readFile(t, dst, emmc, "Dest/M42/flats/f2.fit"); got != "f2" {
			t.Errorf("f2 = %q", got)
		}
	}
	noTemps(t, dst, emmc)
}

func TestMirrorCreatesAncestors(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	src, dst := fx.dev(ip1), fx.dev(ip2)
	src.AddFile(emmc, "Preview/Refs/M42/ref.fit", []byte("ref"), srcTime)
	src.AddDir(emmc, "Preview/Refs/empty", srcTime)
	from, _ := r1r2("Preview/Refs", "")
	to := CopyPlace{Rig: "r2", Share: emmc, Dir: "Preview/Refs"}
	plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42", "empty"}}, To: to, Mirror: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ToDirMissing != 2 || !reflect.DeepEqual(plan.Dirs, []string{"M42", "empty"}) {
		t.Errorf("to_dir_missing = %d, dirs = %v; want 2, [M42 empty]", plan.ToDirMissing, plan.Dirs)
	}
	// One level present: only the missing one counts.
	dst.AddDir(emmc, "Preview", t0)
	plan, err = fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42", "empty"}}, To: to, Mirror: true})
	if err != nil || plan.ToDirMissing != 1 {
		t.Fatalf("plan = %+v, %v; want to_dir_missing 1", plan, err)
	}
	l, err := runCopy(t, fx, runFromPlan(from, to, plan))
	if err != nil {
		t.Fatal(err)
	}
	if s := l.summary(t); s.Done != 1 {
		t.Errorf("summary = %+v", s)
	}
	if got := readFile(t, dst, emmc, "Preview/Refs/M42/ref.fit"); got != "ref" {
		t.Errorf("ref.fit = %q", got)
	}
	if !stat(t, dst, emmc, "Preview/Refs/empty").IsDir {
		t.Error("empty folder not created")
	}

	t.Run("a file in the way refuses", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).AddFile(emmc, "Preview/Refs/ref.fit", []byte("ref"), srcTime)
		fx.dev(ip2).AddFile(emmc, "Preview", []byte("a file"), t0)
		before := tree(t, fx.dev(ip2), emmc)
		_, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"ref.fit"}}, To: to, Mirror: true})
		wantKind(t, err, ErrTypeConflict)
		_, err = runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "ref.fit", Size: 3}}})
		wantKind(t, err, ErrTypeConflict)
		if after := tree(t, fx.dev(ip2), emmc); !reflect.DeepEqual(before, after) {
			t.Errorf("destination changed: %v -> %v", before, after)
		}
	})

	t.Run("a file where a planned folder goes fails its subtree", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).AddFile(emmc, "Preview/M42/a.fit", []byte("a"), srcTime)
		fx.dev(ip1).AddFile(emmc, "Preview/b.fit", []byte("b"), srcTime)
		fx.dev(ip2).AddDir(emmc, "Dest", t0)
		from, to := r1r2("Preview", "Dest")
		plan, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"M42", "b.fit"}}, To: to})
		if err != nil {
			t.Fatal(err)
		}
		fx.dev(ip2).AddFile(emmc, "Dest/M42", []byte("in the way"), t0)
		l, err := runCopy(t, fx, runFromPlan(from, to, plan))
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "M42/a.fit"); it.Outcome != "skipped" || it.Code != "type_conflict" {
			t.Errorf("a.fit = %+v", it)
		}
		if it := l.item(t, "M42"); it.Outcome != "skipped" || it.Code != "type_conflict" {
			t.Errorf("M42 = %+v", it)
		}
		// The folder and the file inside it each count.
		if s := l.summary(t); s.Done != 1 || s.Skipped != 2 {
			t.Errorf("summary = %+v", s)
		}
	})
}

func TestCancelStopsCopy(t *testing.T) {
	fx := newFixture(t)
	h := installHooks(fx)
	big := bytes.Repeat([]byte("y"), 4*copyBufSize)
	fx.dev(ip1).AddFile(emmc, "Preview/a.fit", big, srcTime)
	fx.dev(ip1).AddFile(emmc, "Preview/b.fit", []byte("b"), srcTime)
	fx.dev(ip2).AddDir(emmc, "Dest", t0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.afterRead = func(n int64) {
		if n == 2 {
			cancel()
		}
	}
	from, to := r1r2("Preview", "Dest")
	closesBefore := fx.closes.Load()
	l := &lines{}
	var afterCancel int
	err := fx.svc.Copy(ctx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}, {Rel: "b.fit"}}},
		func(line any) error {
			if ctx.Err() != nil {
				afterCancel++
			}
			return l.emit(line)
		})
	wantKind(t, err, ErrCancelled)
	if got := h.reads.Load(); got != 2 {
		t.Errorf("source reads = %d, want 2 (none after the cancel)", got)
	}
	if afterCancel != 0 {
		t.Errorf("%d lines emitted after the cancel: %+v", afterCancel, l.all)
	}
	noTemps(t, fx.dev(ip2), emmc)
	if got := tree(t, fx.dev(ip2), emmc); !reflect.DeepEqual(got, map[string]string{"Dest": "/"}) {
		t.Errorf("destination holds %v, want only Dest/", slices.Sorted(maps.Keys(got)))
	}
	// The cancelled sessions were closed, not parked: both rigs' sessions.
	if fx.closes.Load()-closesBefore < 2 {
		t.Errorf("closes = %d, want the cancelled sessions closed", fx.closes.Load()-closesBefore)
	}
	// The lock is free again.
	unlock, err := fx.svc.lockOp(context.Background(), "Copy", "r1")
	if err != nil {
		t.Fatalf("lock still held: %v", err)
	}
	unlock()
}

// A cancel that lands on the source's last read, after every byte reached the
// temp, still ends the item uncommitted: no rename, and for an overwrite no
// delete of the old file.
func TestCancelAtEOFCommitsNothing(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("overwrite=%v", overwrite), func(t *testing.T) {
			fx := newFixture(t)
			h := installHooks(fx)
			fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("new"), srcTime)
			fx.dev(ip2).AddDir(emmc, "Dest", t0)
			req := CopyRequest{Items: []CopyItem{{Rel: "a.fit"}}}
			req.From, req.To = r1r2("Preview", "Dest")
			if overwrite {
				fx.dev(ip2).AddFile(emmc, "Dest/a.fit", []byte("old"), t0)
				req.Overwrite = []CopyOverwrite{{Rel: "a.fit", DstSize: 3, DstMtime: t0.Unix()}}
			}
			before := tree(t, fx.dev(ip2), emmc)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.atEOF = cancel
			var renames atomic.Int64
			h.beforeRename = func(string, string, string) { renames.Add(1) }
			l := &lines{}
			err := fx.svc.Copy(ctx, req, l.emit)
			wantKind(t, err, ErrCancelled)
			if got := renames.Load(); got != 0 {
				t.Errorf("renames = %d, want 0", got)
			}
			if after := tree(t, fx.dev(ip2), emmc); !reflect.DeepEqual(before, after) {
				t.Errorf("destination changed: %v -> %v", before, after)
			}
			if overwrite {
				if got := stat(t, fx.dev(ip2), emmc, "Dest/a.fit").Mtime; !got.Equal(t0) {
					t.Errorf("old file mtime = %v, want %v", got, t0)
				}
			}
		})
	}
}

func TestCopyPreservesMtime(t *testing.T) {
	fx := newFixture(t)
	fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("a"), srcTime)
	fx.dev(ip2).AddDir(emmc, "Dest", t0)
	from, to := r1r2("Preview", "Dest")
	if _, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: 1}}}); err != nil {
		t.Fatal(err)
	}
	if got := stat(t, fx.dev(ip2), emmc, "Dest/a.fit").Mtime; !got.Equal(srcTime) {
		t.Errorf("mtime = %v, want the source's %v", got, srcTime)
	}
}

// TestCopyZeroMtimeStillSet: a source whose device reports no time is still
// handed to Chtimes, and a device refusing it reports mtime_not_preserved;
// only an upload without a browser time skips the call (TestUploadMtime).
func TestCopyZeroMtimeStillSet(t *testing.T) {
	fx := newFixture(t)
	h := installHooks(fx)
	fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("a"), time.Time{})
	fx.dev(ip2).AddDir(emmc, "Dest", t0)
	fx.dev(ip2).FailChtimes(emmc, &os.PathError{Op: "chtimes", Path: "a.fit", Err: os.ErrInvalid})
	from, to := r1r2("Preview", "Dest")
	l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if it := l.item(t, "a.fit"); it.Outcome != outcomeCopied || it.Code != codeMtimeNotPreserved {
		t.Errorf("item = %+v", it)
	}
	if got := h.chtimes.Load(); got != 1 {
		t.Errorf("Chtimes calls = %d, want 1", got)
	}
}

func TestCopyChtimesFailureKeepsFile(t *testing.T) {
	fx := newFixture(t)
	h := installHooks(fx)
	big := bytes.Repeat([]byte("z"), copyBufSize+5)
	fx.dev(ip1).AddFile(emmc, "Preview/a.fit", big, srcTime)
	fx.dev(ip2).AddDir(emmc, "Dest", t0)
	fx.dev(ip2).FailChtimes(emmc, &os.PathError{Op: "chtimes", Path: "a.fit", Err: os.ErrPermission})
	from, to := r1r2("Preview", "Dest")
	l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: int64(len(big))}}})
	if err != nil {
		t.Fatal(err)
	}
	if it := l.item(t, "a.fit"); it.Outcome != "copied" || it.Code != "mtime_not_preserved" {
		t.Errorf("item = %+v", it)
	}
	if s := l.summary(t); s.Done != 1 || s.Failed != 0 {
		t.Errorf("summary = %+v", s)
	}
	if got := readFile(t, fx.dev(ip2), emmc, "Dest/a.fit"); got != string(big) {
		t.Error("content differs")
	}
	if fi := stat(t, fx.dev(ip2), emmc, "Dest/a.fit"); !fi.Mtime.Equal(t0) {
		t.Errorf("mtime = %v, want the copy time %v", fi.Mtime, t0)
	}
	if got := h.chtimes.Load(); got != 1 {
		t.Errorf("Chtimes calls = %d, want 1 (never retried)", got)
	}

	t.Run("a network failure keeps the file and closes the session", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("a"), srcTime)
		fx.dev(ip1).AddFile(emmc, "Preview/b.fit", []byte("b"), srcTime)
		fx.dev(ip2).AddDir(emmc, "Dest", t0)
		fx.dev(ip2).FailNext(device.FakeOpChtimes, 1, errNetworkForTest())
		closes := fx.closes.Load()
		l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}, {Rel: "b.fit"}}})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Code != "mtime_not_preserved" {
			t.Errorf("a.fit = %+v", it)
		}
		if s := l.summary(t); s.Done != 2 {
			t.Errorf("summary = %+v", s)
		}
		if fx.closes.Load() == closes {
			t.Error("the session that failed Chtimes was parked, not closed")
		}
		if got := readFile(t, fx.dev(ip2), emmc, "Dest/b.fit"); got != "b" {
			t.Errorf("b.fit = %q", got)
		}
	})
}

func TestInUseReportedCopy(t *testing.T) {
	t.Run("the commit rename", func(t *testing.T) {
		fx := newFixture(t)
		h := installHooks(fx)
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("a"), srcTime)
		fx.dev(ip1).AddFile(emmc, "Preview/b.fit", []byte("b"), srcTime)
		fx.dev(ip2).AddDir(emmc, "Dest", t0)
		renames := 0
		h.beforeRename = func(_, _, newPath string) {
			if strings.HasSuffix(newPath, "a.fit") {
				renames++
				fx.dev(ip2).FailNext(device.FakeOpRename, 1, device.SharingViolationLinkError("rename", "tmp", newPath))
			}
		}
		from, to := r1r2("Preview", "Dest")
		l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}, {Rel: "b.fit"}}})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Outcome != "failed" || it.Code != "in_use" {
			t.Errorf("a.fit = %+v", it)
		}
		if renames != 1 {
			t.Errorf("a.fit renames = %d, want 1 (in_use is not retried)", renames)
		}
		if got := readFile(t, fx.dev(ip2), emmc, "Dest/b.fit"); got != "b" {
			t.Errorf("the run did not go on: b.fit = %q", got)
		}
		if got := tree(t, fx.dev(ip2), emmc); got["Dest/a.fit"] != "" {
			t.Errorf("a.fit landed: %v", got)
		}
		noTemps(t, fx.dev(ip2), emmc)
	})

	t.Run("the overwrite's delete", func(t *testing.T) {
		fx := newFixture(t)
		fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("new"), srcTime)
		fx.dev(ip2).AddFile(emmc, "Dest/a.fit", []byte("old"), t0)
		// The first Delete is the overwrite's; only one is armed.
		fx.dev(ip2).FailNext(device.FakeOpDelete, 1, device.SharingViolationError("remove", "Dest/a.fit"))
		from, to := r1r2("Preview", "Dest")
		l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit", Size: 3}},
			Overwrite: []CopyOverwrite{{Rel: "a.fit", DstSize: 3, DstMtime: t0.Unix()}}})
		if err != nil {
			t.Fatal(err)
		}
		if it := l.item(t, "a.fit"); it.Outcome != "failed" || it.Code != "in_use" {
			t.Errorf("a.fit = %+v", it)
		}
		if got := readFile(t, fx.dev(ip2), emmc, "Dest/a.fit"); got != "old" {
			t.Errorf("destination = %q, want untouched", got)
		}
		noTemps(t, fx.dev(ip2), emmc)
	})
}

func TestLayoutGuardRefusesFlatRootCopy(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		from   CopyPlace
		names  []string
		to     CopyPlace
		mirror bool
		run    CopyRequest
		// roots, when set, replaces r2's configured roots on the Udisk.
		roots []string
	}{
		{
			name:  "a configured root copied to a Udisk root",
			from:  CopyPlace{Rig: "r1", Share: emmc, Dir: ""},
			names: []string{"Autorun"},
			to:    CopyPlace{Rig: "r2", Share: udisk, Dir: ""},
		},
		{
			name:   "a mirror that creates the root's flat folder",
			from:   CopyPlace{Rig: "r1", Share: udisk, Dir: "Autorun/Light"},
			names:  []string{"M42"},
			to:     CopyPlace{Rig: "r2", Share: udisk, Dir: "Autorun/Light"},
			mirror: true,
		},
		{
			name:  "a flat root that precedes the nested root in config order",
			from:  CopyPlace{Rig: "r1", Share: emmc, Dir: ""},
			names: []string{"Plan"},
			to:    CopyPlace{Rig: "r2", Share: udisk, Dir: ""},
			roots: []string{"Plan", "Autorun"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t)
			if c.roots != nil {
				fx.editRig("r2", func(p *model.RigProfile) {
					p.Sources = []model.Source{{Share: udisk, Roots: c.roots}}
				})
			}
			top := c.names[0]
			if c.mirror {
				top = "Autorun"
			}
			fx.dev(ip1).AddFile(emmc, top+"/Light/M42/a.fit", []byte("a"), srcTime)
			fx.dev(ip1).AddFile(udisk, top+"/Light/M42/a.fit", []byte("a"), srcTime)
			g := fx.dev(ip2)
			g.AddFile(udisk, "ASIAIR/Autorun/Light/b.fit", []byte("b"), t0)
			before := tree(t, g, udisk)
			_, err := fx.svc.CopyPlan(ctx, CopyPlanRequest{From: CopySource{CopyPlace: c.from, Names: c.names}, To: c.to, Mirror: c.mirror})
			wantKind(t, err, ErrLayout)

			req := CopyRequest{From: c.from, To: c.to, Items: []CopyItem{{Rel: top + "/Light/M42/a.fit"}},
				Dirs: []string{top, top + "/Light", top + "/Light/M42"}}
			if c.mirror {
				req.Items, req.Dirs = []CopyItem{{Rel: "M42/a.fit"}}, []string{"M42"}
			}
			_, err = runCopy(t, fx, req)
			wantKind(t, err, ErrLayout)
			if got := g.Mutations(); got != 0 {
				t.Errorf("mutations = %d, want 0", got)
			}
			if after := tree(t, g, udisk); !reflect.DeepEqual(before, after) {
				t.Errorf("share changed: %v -> %v", before, after)
			}
		})
	}
}

func TestCopyRunRefusals(t *testing.T) {
	fx := newFixture(t)
	fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("a"), srcTime)
	from, to := r1r2("Preview", "")
	cases := []struct {
		req  CopyRequest
		kind error
	}{
		{CopyRequest{From: from, To: from, Items: []CopyItem{{Rel: "a.fit"}}}, ErrSameFolder},
		{CopyRequest{From: from, To: CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview/M42"}, Items: []CopyItem{{Rel: "M42/a.fit"}}}, ErrIntoItself},
		// Samba resolves a mis-cased share name to the same share.
		{CopyRequest{From: from, To: CopyPlace{Rig: "r1", Share: "EMMC IMAGES", Dir: "Preview"}, Items: []CopyItem{{Rel: "a.fit"}}}, ErrSameFolder},
		{CopyRequest{From: from, To: CopyPlace{Rig: "r1", Share: "emmc images", Dir: "Preview/M42"}, Items: []CopyItem{{Rel: "M42/a.fit"}}}, ErrIntoItself},
		{CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}, {Rel: "a.fit"}}}, ErrBadPath},
		{CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "../a.fit"}}}, ErrBadPath},
		{CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "/a.fit"}}}, ErrBadPath},
		{CopyRequest{From: from, To: CopyPlace{Rig: "r3", Share: emmc}, Items: []CopyItem{{Rel: "a.fit"}}}, ErrUnknownRig},
	}
	for _, c := range cases {
		_, err := runCopy(t, fx, c.req)
		wantKind(t, err, c.kind)
	}
	if got := fx.opens.Load(); got != 0 {
		t.Errorf("opens = %d, want 0", got)
	}

	fx.editRig("r2", func(p *model.RigProfile) { p.GUID = "" })
	_, err := runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}}})
	wantKind(t, err, ErrHostBound)
	if got := fx.dev(ip2).Mutations(); got != 0 {
		t.Errorf("mutations = %d on a host-bound rig", got)
	}

	// A run holds the operation lock: a second one is busy.
	unlock, err := fx.svc.lockOp(context.Background(), "Delete", "r1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runCopy(t, fx, CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}}})
	wantKind(t, err, ErrBusy)
	_, err = fx.svc.CopyPlan(context.Background(), CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"a.fit"}}, To: to})
	wantKind(t, err, ErrBusy)
	unlock()
}

func TestCopySameRig(t *testing.T) {
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", []byte("a"), srcTime)
	f.AddDir(emmc, "Other", t0)
	from := CopyPlace{Rig: "r1", Share: emmc, Dir: "Preview"}
	to := CopyPlace{Rig: "r1", Share: emmc, Dir: "Other"}
	plan, err := fx.svc.CopyPlan(context.Background(), CopyPlanRequest{From: CopySource{CopyPlace: from, Names: []string{"a.fit"}}, To: to})
	if err != nil {
		t.Fatal(err)
	}
	opens := fx.opens.Load()
	l, err := runCopy(t, fx, runFromPlan(from, to, plan))
	if err != nil {
		t.Fatal(err)
	}
	if s := l.summary(t); s.Done != 1 {
		t.Errorf("summary = %+v", s)
	}
	if got := readFile(t, f, emmc, "Other/a.fit"); got != "a" {
		t.Errorf("a.fit = %q", got)
	}
	if got := fx.opens.Load() - opens; got > 1 {
		t.Errorf("opens = %d, want at most 1: one session reads and writes", got)
	}
}

// A planned folder that can't be made reports its line and counts it, so a
// folders-only run is not summarised as a clean success.
func TestCopyFailedEmptyFolderCounted(t *testing.T) {
	fx := newFixture(t)
	fx.dev(ip1).AddDir(emmc, "Preview/Empty", srcTime)
	fx.dev(ip2).AddDir(emmc, "Dest", t0)
	fx.dev(ip2).FailNext(device.FakeOpMkdir, 1, &os.PathError{Op: "mkdir", Path: "Dest/Empty", Err: os.ErrPermission})
	from, to := r1r2("Preview", "Dest")
	l, err := runCopy(t, fx, CopyRequest{From: from, To: to, Dirs: []string{"Empty"}, Items: []CopyItem{}})
	if err != nil {
		t.Fatal(err)
	}
	if it := l.item(t, "Empty"); it.Outcome != "failed" || it.Code != "denied" {
		t.Errorf("Empty = %+v, want failed denied", it)
	}
	if s := l.summary(t); s != (SummaryLine{Type: "summary", Failed: 1}) {
		t.Errorf("summary = %+v, want the folder counted as failed", s)
	}
}

// A wrong device or a rig that lost its identity ends the run as unreachable
// does: the rest are not attempted, with no further dial or probe.
func TestIdentityStopsRunCopy(t *testing.T) {
	for _, c := range []struct {
		name     string
		arm      func(fx *fixture)
		wantCode string
	}{
		{"identity", func(fx *fixture) {
			fx.prober.script(ip2, probeAnswer{guid: "someone-else", ok: true})
		}, "identity"},
		{"host-bound", func(fx *fixture) {
			fx.editRig("r2", func(p *model.RigProfile) { p.GUID = "" })
		}, "error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t)
			for _, n := range []string{"a.fit", "b.fit", "c.fit"} {
				fx.dev(ip1).AddFile(emmc, "Preview/"+n, []byte(n), srcTime)
			}
			fx.dev(ip2).AddDir(emmc, "Dest", t0)
			from, to := r1r2("Preview", "Dest")
			req := CopyRequest{From: from, To: to, Items: []CopyItem{{Rel: "a.fit"}, {Rel: "b.fit"}, {Rel: "c.fit"}}}
			// As b.fit starts, the destination session drops and the re-dial
			// meets the armed identity failure.
			var opensAtFailure, probesAtFailure int64
			l := &lines{}
			err := fx.svc.Copy(context.Background(), req, func(line any) error {
				switch line := line.(type) {
				case ProgressLine:
					if line.Current == "b.fit" {
						c.arm(fx)
						fx.dev(ip2).FailNext(device.FakeOpCreateNew, 1, errNetworkForTest())
					}
				case ItemLine:
					if line.Rel == "b.fit" {
						opensAtFailure, probesAtFailure = fx.opens.Load(), int64(fx.prober.count())
					}
				}
				return l.emit(line)
			})
			if err != nil {
				t.Fatal(err)
			}
			if b := l.item(t, "b.fit"); b.Outcome != "failed" || b.Code != c.wantCode {
				t.Errorf("b.fit = %+v, want failed %s", b, c.wantCode)
			}
			cl := l.item(t, "c.fit")
			if cl.Outcome != "failed" || cl.Code != "not_attempted" || !strings.Contains(cl.Message, "Rig Two") {
				t.Errorf("c.fit = %+v, want not_attempted naming Rig Two", cl)
			}
			if got := fx.opens.Load(); got != opensAtFailure {
				t.Errorf("opens = %d after the failure, want %d (no further dial)", got, opensAtFailure)
			}
			if got := int64(fx.prober.count()); got != probesAtFailure {
				t.Errorf("probes = %d after the failure, want %d (no further probe)", got, probesAtFailure)
			}
			if s := l.summary(t); s != (SummaryLine{Type: "summary", Done: 1, Failed: 2, Aborted: "identity"}) {
				t.Errorf("summary = %+v", s)
			}
			if got := tree(t, fx.dev(ip2), emmc); got["Dest/c.fit"] != "" {
				t.Errorf("c.fit landed: %v", got)
			}
		})
	}
}
