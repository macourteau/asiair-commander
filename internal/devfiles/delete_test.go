package devfiles

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// testStore is the fixture's sqlite store with the temp dir standing in for
// the NAS that its rows' DestPaths point into.
type testStore struct {
	store.Store
	nas string
}

// withStore gives the fixture's service a temp-dir sqlite store and a temp-dir
// NAS read through the production localfs, and makes jpg an excluded (preview)
// extension, as a default config does.
func withStore(t *testing.T, fx *fixture) *testStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fx.svc.store = st
	fx.svc.lfs = localfs.New()
	cur := fx.cfg.Load()
	next := *cur
	next.ExcludeExtensions = []string{"jpg", "jpeg"}
	fx.cfg.Store(&next)
	return &testStore{Store: st, nas: t.TempDir()}
}

// upsertFrame writes a frame row through the production UpsertFrame path,
// with its archive copy present on the NAS at ArchiveSize bytes, and returns
// it as stored.
func upsertFrame(t *testing.T, st *testStore, rigID, share, p string, size int64, mtime time.Time,
	state model.FrameState) *model.Frame {
	t.Helper()
	ctx := context.Background()
	dest := filepath.Join(st.nas, rigID, filepath.FromSlash(p))
	writeNAS(t, dest, size)
	f := &model.Frame{RigID: rigID, SourceShare: share, SourcePath: p, Size: size, Mtime: mtime.Unix(),
		FrameType: "LIGHT", State: state, DestPath: dest, ArchiveSize: size}
	if err := st.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert %s: %v", p, err)
	}
	got, ok, err := st.GetFrame(ctx, rigID, share, p)
	if err != nil || !ok {
		t.Fatalf("get %s: %v, %v", p, ok, err)
	}
	if got.ArchiveSize != size {
		t.Fatalf("get %s: archive size %d, want %d", p, got.ArchiveSize, size)
	}
	return got
}

// writeNAS puts a size-byte file at path, replacing any.
func writeNAS(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runLines collects a run's emitted lines, round-tripped through JSON as the
// handler sends them.
type runLines struct {
	progress []ProgressLine
	items    []ItemLine
	summary  *SummaryLine
	all      []map[string]any
}

func (r *runLines) emit(line any) error {
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.all = append(r.all, m)
	switch l := line.(type) {
	case ProgressLine:
		r.progress = append(r.progress, l)
	case ItemLine:
		r.items = append(r.items, l)
	case SummaryLine:
		r.summary = &l
	default:
		return errors.New("unexpected line type")
	}
	return nil
}

func (r *runLines) codes() map[string]string {
	out := map[string]string{}
	for _, it := range r.items {
		out[it.Rel] = it.Outcome + "/" + it.Code
	}
	return out
}

// runRequest echoes a plan as the client does: every item and folder, with the
// plan's unarchived count acknowledged.
func runRequest(share, dir string, p *DeletePlan) DeleteRequest {
	return DeleteRequest{Share: share, Dir: dir, Items: p.Items, Dirs: p.DirsDeepestFirst,
		AcknowledgedUnarchived: p.Unarchived}
}

func exists(t *testing.T, f *device.Fake, share, p string) bool {
	t.Helper()
	s, _ := f.Open(context.Background())
	sh, _ := s.Mount(share)
	_, err := sh.Stat(p)
	if err != nil && !device.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestDeletePlan(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/M42/a.FIT", []byte("aaaa"), t0)
	f.AddFile(emmc, "Preview/M42/sub/b.fit", []byte("bb"), t0)
	f.AddFile(emmc, "Preview/M42/sub/c.jpg", []byte("c"), t0)
	f.AddFile(emmc, "Preview/M42/README", []byte("rrrrrrrr"), t0)
	f.AddDir(emmc, "Preview/M42/empty", t0)
	f.AddFile(emmc, "Preview/notes.txt", []byte("n"), t0)

	p, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"M42", "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Dirs != 3 || p.Files != 5 || p.Bytes != 16 {
		t.Errorf("counts = %d dirs, %d files, %d bytes; want 3, 5, 16", p.Dirs, p.Files, p.Bytes)
	}
	wantExt := []ExtTotal{{"", 1, 8}, {"fit", 2, 6}, {"jpg", 1, 1}, {"txt", 1, 1}}
	if !reflect.DeepEqual(p.ByExt, wantExt) {
		t.Errorf("by_ext = %+v, want %+v", p.ByExt, wantExt)
	}
	// Folders come out deepest first: a folder after everything inside it.
	wantDirs := []string{"M42/sub", "M42/empty", "M42"}
	if !reflect.DeepEqual(p.DirsDeepestFirst, wantDirs) {
		t.Errorf("dirs_deepest_first = %v, want %v", p.DirsDeepestFirst, wantDirs)
	}
	var rels []string
	for _, it := range p.Items {
		rels = append(rels, it.Rel)
		if it.Mtime != t0.Unix() {
			t.Errorf("%s mtime = %d, want %d", it.Rel, it.Mtime, t0.Unix())
		}
	}
	if want := []string{"M42/README", "M42/a.FIT", "M42/sub/b.fit", "M42/sub/c.jpg", "notes.txt"}; !reflect.DeepEqual(rels, want) {
		t.Errorf("items = %v, want %v", rels, want)
	}
	if p.UnwatchedFiles != 5 || p.Unarchived != 0 || len(p.WatchedRoots) != 0 {
		t.Errorf("unwatched %d, unarchived %d, roots %v; want 5, 0, []", p.UnwatchedFiles, p.Unarchived, p.WatchedRoots)
	}
	if got := f.Mutations(); got != 0 {
		t.Errorf("mutations = %d, want 0 (a plan writes nothing)", got)
	}

	// Empty slices serialize as [], never null.
	g := fx.dev(ip1)
	g.AddDir(emmc, "Lonely", t0)
	p, err = fx.svc.DeletePlan(ctx, "r1", emmc, "", []string{"Lonely"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	for _, k := range []string{`"by_ext":[]`, `"watched_roots":[]`, `"items":[]`, `"dirs_deepest_first":["Lonely"]`, `"dirs":1`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("plan JSON %s lacks %s", b, k)
		}
	}
}

func TestDeleteUnarchivedCount(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	st := withStore(t, fx)
	f := fx.dev(ip1)
	data := []byte("frame-bytes")
	size := int64(len(data))
	add := func(share, p string) { f.AddFile(share, p, data, t0) }

	// Under the watched eMMC Autorun.
	add(emmc, "Autorun/Light/norow.fit")
	add(emmc, "Autorun/Light/verified.fit")
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/verified.fit", size, t0, model.StateVerified)
	add(emmc, "Autorun/Light/unclassified.fit")
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/unclassified.fit", size, t0, model.StateUnclassified)
	add(emmc, "Autorun/Light/failed.fit")
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/failed.fit", size, t0, model.StateFailed)
	add(emmc, "Autorun/Light/held.fit")
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/held.fit", size, t0, model.StateHeld)
	add(emmc, "Autorun/Light/culled.fit")
	culled := upsertFrame(t, st, "r1", emmc, "Autorun/Light/culled.fit", size, t0, model.StateVerified)
	if err := st.SetFrameMissing(ctx, culled.ID, t0.Unix()); err != nil {
		t.Fatal(err)
	}
	add(emmc, "Autorun/Light/dismissed.fit")
	dismissed := upsertFrame(t, st, "r1", emmc, "Autorun/Light/dismissed.fit", size, t0, model.StateUnclassified)
	if err := st.DismissFrame(ctx, dismissed.ID); err != nil {
		t.Fatal(err)
	}
	add(emmc, "Autorun/Light/revised.fit") // the row describes an older revision
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/revised.fit", size+1, t0, model.StateVerified)
	add(emmc, "Autorun/Light/otherrig.fit") // archived, but for another rig
	upsertFrame(t, st, "r2", emmc, "Autorun/Light/otherrig.fit", size, t0, model.StateVerified)
	add(emmc, "Autorun/Light/norow_thn.jpg") // a preview
	add(emmc, "Autorun/Light/._norow.fit")   // junk
	add(emmc, "Preview/outside.fit")         // outside every watched root

	// A FAT-cased nested root: rows are keyed ASIAIR/<configured root>/….
	add(udisk, "asiair/AUTORUN/Light/nested.fit")
	upsertFrame(t, st, "r1", udisk, "ASIAIR/Autorun/Light/nested.fit", size, t0, model.StateVerified)
	add(udisk, "asiair/AUTORUN/Light/nested-norow.fit")

	p, err := fx.svc.DeletePlan(ctx, "r1", emmc, "", []string{"Autorun", "Preview"})
	if err != nil {
		t.Fatal(err)
	}
	// norow, failed, held, culled, dismissed, revised, otherrig.
	if p.Unarchived != 7 {
		t.Errorf("unarchived = %d, want 7", p.Unarchived)
	}
	// The preview and the junk file are not frames; outside.fit is outside.
	if p.UnwatchedFiles != 2 {
		t.Errorf("unwatched_files = %d, want 2 (junk and outside)", p.UnwatchedFiles)
	}

	p, err = fx.svc.DeletePlan(ctx, "r1", udisk, "", []string{"asiair"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Unarchived != 1 {
		t.Errorf("nested unarchived = %d, want 1 (the store key is rewritten to the harvester's case)", p.Unarchived)
	}
}

// statFailFS fails every Stat with a non-not-exist error, as an unmounted or
// unreadable NAS would.
type statFailFS struct{}

func (statFailFS) Stat(string) (int64, time.Time, error) {
	return 0, time.Time{}, &os.PathError{Op: "stat", Err: os.ErrPermission}
}

func (statFailFS) FreeSpace(string) (int64, int64, error) { return 0, 0, os.ErrPermission }

// TestDeleteUnarchivedNASMissing checks a row the store calls archived still
// counts as unarchived when its NAS copy is gone, has another length than
// ArchiveSize, or cannot be read; MissingSeenAt is only as fresh as the last
// library scan.
func TestDeleteUnarchivedNASMissing(t *testing.T) {
	ctx := context.Background()
	data := []byte("frame-bytes")
	size := int64(len(data))
	setup := func(t *testing.T) (*fixture, *testStore, *device.Fake, map[string]*model.Frame) {
		fx := newFixture(t)
		st := withStore(t, fx)
		f := fx.dev(ip1)
		rows := map[string]*model.Frame{}
		for _, n := range []string{"present", "gone", "resized"} {
			p := "Autorun/Light/" + n + ".fit"
			f.AddFile(emmc, p, data, t0)
			rows[n] = upsertFrame(t, st, "r1", emmc, p, size, t0, model.StateVerified)
		}
		return fx, st, f, rows
	}
	cull := func(t *testing.T, rows map[string]*model.Frame) {
		t.Helper()
		if err := os.Remove(rows["gone"].DestPath); err != nil {
			t.Fatal(err)
		}
		writeNAS(t, rows["resized"].DestPath, size+5)
	}

	t.Run("plan", func(t *testing.T) {
		fx, _, _, rows := setup(t)
		p, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Autorun", []string{"Light"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Unarchived != 0 {
			t.Fatalf("unarchived = %d, want 0 while every NAS copy is present", p.Unarchived)
		}
		cull(t, rows)
		p, err = fx.svc.DeletePlan(ctx, "r1", emmc, "Autorun", []string{"Light"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Unarchived != 2 {
			t.Errorf("unarchived = %d, want 2 (gone and resized)", p.Unarchived)
		}
	})

	t.Run("an unreadable NAS counts every hit", func(t *testing.T) {
		fx, _, _, _ := setup(t)
		fx.svc.lfs = statFailFS{}
		p, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Autorun", []string{"Light"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Unarchived != 3 {
			t.Errorf("unarchived = %d, want 3", p.Unarchived)
		}
	})

	t.Run("run recount", func(t *testing.T) {
		fx, _, f, rows := setup(t)
		plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Autorun", []string{"Light"})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Unarchived != 0 {
			t.Fatalf("unarchived = %d, want 0", plan.Unarchived)
		}
		cull(t, rows)
		var lines runLines
		err = fx.svc.Delete(ctx, "r1", runRequest(emmc, "Autorun", plan), lines.emit)
		wantKind(t, err, ErrUnarchived)
		var ue *UnarchivedError
		if !errors.As(err, &ue) || ue.Unarchived != 2 {
			t.Fatalf("error = %#v, want an UnarchivedError carrying 2", err)
		}
		if len(lines.all) != 0 || f.Mutations() != 0 {
			t.Errorf("lines %v, mutations %d; want none (nothing deleted)", lines.all, f.Mutations())
		}
	})
}

func TestDeleteRunUnarchivedRecount(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	st := withStore(t, fx)
	f := fx.dev(ip1)
	data := []byte("frame")
	for _, n := range []string{"a", "b", "c"} {
		p := "Autorun/Light/" + n + ".fit"
		f.AddFile(emmc, p, data, t0)
		upsertFrame(t, st, "r1", emmc, p, int64(len(data)), t0, model.StateVerified)
	}
	plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Autorun", []string{"Light"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Unarchived != 0 {
		t.Fatalf("unarchived = %d, want 0", plan.Unarchived)
	}

	// The ledger moves after the confirmation: two rows lose their proof.
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/a.fit", int64(len(data)), t0, model.StateHeld)
	upsertFrame(t, st, "r1", emmc, "Autorun/Light/b.fit", int64(len(data)), t0, model.StateFailed)

	var lines runLines
	err = fx.svc.Delete(ctx, "r1", runRequest(emmc, "Autorun", plan), lines.emit)
	e := wantKind(t, err, ErrUnarchived)
	var ue *UnarchivedError
	if !errors.As(err, &ue) || ue.Unarchived != 2 {
		t.Fatalf("error = %#v, want an UnarchivedError carrying 2", err)
	}
	if !strings.Contains(e.Msg, "now 2, was 0") {
		t.Errorf("message = %q", e.Msg)
	}
	if len(lines.all) != 0 || f.Mutations() != 0 {
		t.Errorf("lines %v, mutations %d; want none (nothing deleted)", lines.all, f.Mutations())
	}

	// An acknowledgement covering the fresh count runs.
	req := runRequest(emmc, "Autorun", plan)
	req.AcknowledgedUnarchived = 2
	if err := fx.svc.Delete(ctx, "r1", req, lines.emit); err != nil {
		t.Fatal(err)
	}
	if lines.summary == nil || lines.summary.Done != 4 {
		t.Errorf("summary = %+v, want 4 done", lines.summary)
	}
}

func TestDeleteRunOnlyEchoedItems(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/M42/a.fit", []byte("a"), t0)
	f.AddFile(emmc, "Preview/M42/b.fit", []byte("b"), t0)
	f.AddFile(emmc, "Preview/M42/sub/c.fit", []byte("c"), t0)
	f.AddFile(emmc, "Preview/M42/d.fit", []byte("d"), t0)
	f.AddFile(emmc, "Preview/other.fit", []byte("o"), t0)
	f.AddFile(emmc, "Elsewhere/x.fit", []byte("x"), t0)

	plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"M42"})
	if err != nil {
		t.Fatal(err)
	}

	// After the confirmation: a file arrives, one changes size, one changes
	// mtime by more than 2 s, and one is deleted by someone else.
	f.AddFile(emmc, "Preview/M42/new.fit", []byte("arrived"), t0)
	f.AddFile(emmc, "Preview/M42/b.fit", []byte("bigger"), t0)
	f.AddFile(emmc, "Preview/M42/d.fit", []byte("d"), t0.Add(3*time.Second))
	h, _ := f.Open(ctx)
	sh, _ := h.Mount(emmc)
	if err := sh.Delete("Preview/M42/sub/c.fit"); err != nil {
		t.Fatal(err)
	}

	var lines runLines
	if err := fx.svc.Delete(ctx, "r1", runRequest(emmc, "Preview", plan), lines.emit); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"M42/b.fit": "skipped/changed",
		"M42/d.fit": "skipped/changed",
		"M42":       "skipped/not_empty",
	}
	if got := lines.codes(); !reflect.DeepEqual(got, want) {
		t.Errorf("item lines = %v, want %v", got, want)
	}
	// a.fit deleted, c.fit already gone (done, not listed), sub deleted.
	if s := lines.summary; s == nil || s.Done != 3 || s.Skipped != 3 || s.Failed != 0 || s.Aborted != "" {
		t.Errorf("summary = %+v, want 3 done, 3 skipped", lines.summary)
	}
	for p, want := range map[string]bool{
		"Preview/M42/a.fit": false, "Preview/M42/sub": false,
		"Preview/M42/b.fit": true, "Preview/M42/d.fit": true, "Preview/M42/new.fit": true,
		"Preview/M42": true, "Preview/other.fit": true, "Elsewhere/x.fit": true,
	} {
		if got := exists(t, f, emmc, p); got != want {
			t.Errorf("%s exists = %v, want %v", p, got, want)
		}
	}
	// Progress names each item as it starts, files then folders, and counts both.
	var cur []string
	for _, p := range lines.progress {
		cur = append(cur, p.Current)
		if p.Total != 6 {
			t.Errorf("progress total = %d, want 6 (files plus folders)", p.Total)
		}
	}
	if want := []string{"M42/a.fit", "M42/b.fit", "M42/d.fit", "M42/sub/c.fit", "M42/sub", "M42"}; !reflect.DeepEqual(cur, want) {
		t.Errorf("progress order = %v, want %v", cur, want)
	}
	if last := lines.all[len(lines.all)-1]; last["type"] != "summary" || last["aborted"] != "" {
		t.Errorf("last line = %v, want the summary", last)
	}
}

func TestDeleteRunNotEmptyFromDeviceError(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddDir(emmc, "Preview/Empty", t0)
	// The device refuses the folder in its real shape, though the listing is
	// empty: the refusal alone is the signal.
	f.FailNext(device.FakeOpDelete, 1, device.DirNotEmptyError("Preview/Empty"))

	var lines runLines
	req := DeleteRequest{Share: emmc, Dir: "Preview", Dirs: []string{"Empty"}}
	if err := fx.svc.Delete(ctx, "r1", req, lines.emit); err != nil {
		t.Fatal(err)
	}
	if got := lines.codes(); !reflect.DeepEqual(got, map[string]string{"Empty": "skipped/not_empty"}) {
		t.Errorf("item lines = %v", got)
	}
	if !exists(t, f, emmc, "Preview/Empty") {
		t.Error("the refused folder was removed")
	}
	if got := fx.opens.Load(); got != 1 {
		t.Errorf("opens = %d, want 1 (a refusal is not retried)", got)
	}
}

func TestDeleteRunDirTypeCheck(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.ListMarkers(true)
	f.AddDir(emmc, "Preview/Became", t0)
	f.AddDir(emmc, "Preview/Keep", t0)
	f.SetMarker(emmc, "Preview/Keep/"+model.MediaMarkerName, []byte("id"))
	f.AddDir(emmc, "Preview/Mixed", t0)
	f.SetMarker(emmc, "Preview/Mixed/"+model.IdentityMarkerName, []byte("id"))
	f.AddFile(emmc, "Preview/Mixed/late.fit", []byte("x"), t0)

	plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"Became", "Keep"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 0 || !reflect.DeepEqual(plan.DirsDeepestFirst, []string{"Keep", "Became"}) {
		t.Fatalf("plan = %+v, want two empty folders and no items", plan)
	}

	// The echoed folder Became is replaced by a file of that name.
	h, _ := f.Open(ctx)
	sh, _ := h.Mount(emmc)
	if err := sh.Delete("Preview/Became"); err != nil {
		t.Fatal(err)
	}
	f.AddFile(emmc, "Preview/Became", []byte("a file now"), t0)

	var lines runLines
	req := runRequest(emmc, "Preview", plan)
	req.Dirs = append(req.Dirs, "Mixed")
	if err := fx.svc.Delete(ctx, "r1", req, lines.emit); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Became": "skipped/changed",
		"Keep":   "skipped/kept_marker",
		"Mixed":  "skipped/not_empty",
	}
	if got := lines.codes(); !reflect.DeepEqual(got, want) {
		t.Errorf("item lines = %v, want %v", got, want)
	}
	for _, p := range []string{"Preview/Became", "Preview/Keep", "Preview/Mixed", "Preview/Mixed/late.fit"} {
		if !exists(t, f, emmc, p) {
			t.Errorf("%s was removed", p)
		}
	}
	marker, err := sh.ReadMarker("Preview/Keep/" + model.MediaMarkerName)
	if err != nil || string(marker) != "id" {
		t.Errorf("marker = %q, %v; want it kept", marker, err)
	}
}

func TestDeletePlanWatchedRoots(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Autorun/Light/a.fit", []byte("a"), t0)
	f.AddFile(emmc, "Preview/b.fit", []byte("b"), t0)
	f.AddFile(udisk, "ASIAIR/Autorun/Light/c.fit", []byte("c"), t0)
	f.AddFile(udisk, "Backup/AUTORUN/d.fit", []byte("d"), t0)
	f.AddDir(udisk, "autorun", t0)

	cases := []struct {
		share, dir string
		names      []string
		want       []string
	}{
		{emmc, "", []string{"Autorun"}, []string{"Autorun"}},        // the root itself
		{emmc, "", []string{"Preview"}, []string{}},                 // not watched
		{emmc, "Autorun", []string{"Light"}, []string{}},            // inside a root, not one
		{udisk, "", []string{"ASIAIR"}, []string{"ASIAIR/Autorun"}}, // an ancestor of a nested root
		{udisk, "", []string{"autorun", "Backup"}, []string{"autorun"}},
	}
	for _, c := range cases {
		p, err := fx.svc.DeletePlan(ctx, "r1", c.share, c.dir, c.names)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.WatchedRoots, c.want) {
			t.Errorf("%s/%s %v: watched_roots = %v, want %v", c.share, c.dir, c.names, p.WatchedRoots, c.want)
		}
	}
}

func TestMarkersInvisibleDelete(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.ListMarkers(true)
	// Explicit folders, as on a device: the fake's Stat does not see a folder
	// implied only by a marker.
	f.AddDir(udisk, "ASIAIR", t0)
	f.AddDir(udisk, "ASIAIR/Autorun", t0)
	f.AddFile(udisk, "ASIAIR/Autorun/x.fit", []byte("x"), t0)
	f.SetMarker(udisk, "ASIAIR/"+model.MediaMarkerName, []byte("id"))

	t.Run("plan", func(t *testing.T) {
		p, err := fx.svc.DeletePlan(ctx, "r1", udisk, "", []string{"ASIAIR"})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 1 || p.Items[0].Rel != "ASIAIR/Autorun/x.fit" || p.Files != 1 {
			t.Errorf("plan items = %+v, want only x.fit", p.Items)
		}
	})

	t.Run("an echoed marker is refused before any device call", func(t *testing.T) {
		opens, mut := fx.opens.Load(), f.Mutations()
		for _, req := range []DeleteRequest{
			{Share: udisk, Items: []DeleteItem{{Rel: "ASIAIR/" + model.MediaMarkerName, Size: 2}}},
			{Share: udisk, Items: []DeleteItem{{Rel: strings.ToUpper(model.IdentityMarkerName)}}},
			{Share: udisk, Dirs: []string{model.MediaMarkerName}},
		} {
			var lines runLines
			wantKind(t, fx.svc.Delete(ctx, "r1", req, lines.emit), ErrBadPath)
			if len(lines.all) != 0 {
				t.Errorf("lines = %v, want none", lines.all)
			}
		}
		if fx.opens.Load() != opens || f.Mutations() != mut {
			t.Error("a refused run reached the device")
		}
	})

	t.Run("a run of the whole plan keeps the marker and its folder", func(t *testing.T) {
		p, err := fx.svc.DeletePlan(ctx, "r1", udisk, "", []string{"ASIAIR"})
		if err != nil {
			t.Fatal(err)
		}
		var lines runLines
		if err := fx.svc.Delete(ctx, "r1", runRequest(udisk, "", p), lines.emit); err != nil {
			t.Fatal(err)
		}
		if got := lines.codes(); !reflect.DeepEqual(got, map[string]string{"ASIAIR": "skipped/kept_marker"}) {
			t.Errorf("item lines = %v", got)
		}
		h, _ := f.Open(ctx)
		sh, _ := h.Mount(udisk)
		if _, err := sh.ReadMarker("ASIAIR/" + model.MediaMarkerName); err != nil {
			t.Errorf("marker: %v", err)
		}
	})
}

func TestDeleteRunRefusesBadRequests(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	fx.svc.maxFiles = 2
	for _, c := range []struct {
		req  DeleteRequest
		kind error
	}{
		{DeleteRequest{Share: emmc}, ErrBadPath},
		{DeleteRequest{Share: emmc, Items: []DeleteItem{{Rel: "../x"}}}, ErrBadPath},
		{DeleteRequest{Share: emmc, Items: []DeleteItem{{Rel: "/x"}}}, ErrBadPath},
		{DeleteRequest{Share: emmc, Dir: "a\\b", Items: []DeleteItem{{Rel: "x"}}}, ErrBadPath},
		{DeleteRequest{Share: emmc, Items: []DeleteItem{{Rel: "x"}}, Dirs: []string{"x"}}, ErrBadPath},
		{DeleteRequest{Share: "", Items: []DeleteItem{{Rel: "x"}}}, ErrBadPath},
		{DeleteRequest{Share: emmc, Items: []DeleteItem{{Rel: "x"}, {Rel: "y"}, {Rel: "z"}}}, ErrTooMany},
	} {
		var lines runLines
		wantKind(t, fx.svc.Delete(ctx, "r1", c.req, lines.emit), c.kind)
	}
	if got := fx.opens.Load(); got != 0 {
		t.Errorf("opens = %d, want 0", got)
	}
}

func TestDeleteRunHostBound(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", []byte("a"), t0)
	plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"a.fit"})
	if err != nil {
		t.Fatal(err)
	}
	fx.editRig("r1", func(p *model.RigProfile) { p.GUID = "" })
	var lines runLines
	wantKind(t, fx.svc.Delete(ctx, "r1", runRequest(emmc, "Preview", plan), lines.emit), ErrHostBound)
	if len(lines.all) != 0 || f.Mutations() != 0 {
		t.Errorf("lines %v, mutations %d; want a refusal before the stream", lines.all, f.Mutations())
	}
}

func TestUnreachableStopsRunDelete(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	for _, n := range []string{"a", "b", "c"} {
		f.AddFile(emmc, "Preview/M42/"+n+".fit", []byte(n), t0)
	}
	plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"M42"})
	if err != nil {
		t.Fatal(err)
	}
	// b.fit's Stat fails at the network level on every attempt.
	f.OnStat = func(_, p string) {
		if p == "Preview/M42/b.fit" {
			f.FailNext(device.FakeOpStat, 1, errNetworkForTest())
		}
	}
	var opensAtAbort int64
	var lines runLines
	emit := func(line any) error {
		if l, ok := line.(ItemLine); ok && l.Code == codeUnreachable {
			opensAtAbort = fx.opens.Load()
		}
		return lines.emit(line)
	}
	if err := fx.svc.Delete(ctx, "r1", runRequest(emmc, "Preview", plan), emit); err != nil {
		t.Fatal(err)
	}
	f.OnStat = nil
	want := map[string]string{
		"M42/b.fit": "failed/unreachable",
		"M42/c.fit": "failed/not_attempted",
		"M42":       "failed/not_attempted",
	}
	if got := lines.codes(); !reflect.DeepEqual(got, want) {
		t.Errorf("item lines = %v, want %v", got, want)
	}
	if s := lines.summary; s == nil || s.Done != 1 || s.Failed != 3 || s.Aborted != "unreachable" {
		t.Errorf("summary = %+v, want 1 done, 3 failed, aborted unreachable", lines.summary)
	}
	if got := fx.opens.Load(); got != opensAtAbort {
		t.Errorf("opens went %d -> %d after the abort, want no further Open", opensAtAbort, got)
	}
	if !strings.Contains(lines.items[0].Message, "Rig One is unreachable") {
		t.Errorf("message = %q, want the rig named", lines.items[0].Message)
	}
	for _, p := range []string{"Preview/M42/b.fit", "Preview/M42/c.fit"} {
		if !exists(t, f, emmc, p) {
			t.Errorf("%s was deleted", p)
		}
	}
}

func TestInUseReportedDelete(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", []byte("a"), t0)
	f.AddFile(emmc, "Preview/b.fit", []byte("b"), t0)
	f.AddDir(emmc, "Preview/Busy", t0)
	plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"a.fit", "b.fit", "Busy"})
	if err != nil {
		t.Fatal(err)
	}
	f.FailNext(device.FakeOpDelete, 1, device.SharingViolationError("remove", "Preview/a.fit"))
	var deletes int
	before := f.Mutations()

	var lines runLines
	if err := fx.svc.Delete(ctx, "r1", runRequest(emmc, "Preview", plan), func(line any) error {
		if l, ok := line.(ItemLine); ok && l.Rel == "a.fit" {
			deletes = f.Mutations() - before
		}
		return lines.emit(line)
	}); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Errorf("device calls for a.fit = %d, want 1 (in_use is never retried)", deletes)
	}
	if got := lines.codes(); !reflect.DeepEqual(got, map[string]string{"a.fit": "failed/in_use"}) {
		t.Errorf("item lines = %v", got)
	}
	if msg := lines.items[0].Message; !strings.Contains(msg, "in use") {
		t.Errorf("message = %q", msg)
	}
	if !exists(t, f, emmc, "Preview/a.fit") || exists(t, f, emmc, "Preview/b.fit") || exists(t, f, emmc, "Preview/Busy") {
		t.Error("want a.fit kept, b.fit and Busy deleted (the run goes on)")
	}

	// A folder delete meeting a sharing violation is in_use too.
	f.AddDir(emmc, "Preview/Held", t0)
	f.FailNext(device.FakeOpDelete, 1, device.SharingViolationError("remove", "Preview/Held"))
	lines = runLines{}
	if err := fx.svc.Delete(ctx, "r1", DeleteRequest{Share: emmc, Dir: "Preview", Dirs: []string{"Held"}}, lines.emit); err != nil {
		t.Fatal(err)
	}
	if got := lines.codes(); !reflect.DeepEqual(got, map[string]string{"Held": "failed/in_use"}) {
		t.Errorf("item lines = %v", got)
	}
}

func TestDeleteRunCancel(t *testing.T) {
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	for _, n := range []string{"a", "b", "c"} {
		f.AddFile(emmc, "Preview/"+n+".fit", []byte(n), t0)
	}
	plan, err := fx.svc.DeletePlan(context.Background(), "r1", emmc, "Preview", []string{"a.fit", "b.fit", "c.fit"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lines runLines
	err = fx.svc.Delete(ctx, "r1", runRequest(emmc, "Preview", plan), func(line any) error {
		if l, ok := line.(ProgressLine); ok && l.Current == "b.fit" {
			cancel()
		}
		return lines.emit(line)
	})
	wantKind(t, err, ErrCancelled)
	if n := len(lines.all); n != 2 || lines.summary != nil {
		t.Errorf("lines = %v, want two progress lines and nothing after the cancel", lines.all)
	}
	if exists(t, f, emmc, "Preview/a.fit") || !exists(t, f, emmc, "Preview/c.fit") {
		t.Error("want a.fit deleted and c.fit untouched")
	}
	// The lock is free again.
	if _, err := fx.svc.DeletePlan(context.Background(), "r1", emmc, "Preview", []string{"c.fit"}); err != nil {
		t.Errorf("plan after cancel: %v", err)
	}
}

func TestWalkFailsWholeDeletePlan(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	withStore(t, fx)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/M42/a.fit", []byte("a"), t0)
	f.AddFile(emmc, "Preview/M42/sub/b.fit", []byte("b"), t0)
	f.OnListDir = func(_, dir string) {
		if dir == "Preview/M42/sub" {
			f.FailNext(device.FakeOpListDir, 1, &os.PathError{Op: "readdir", Path: dir, Err: os.ErrPermission})
		}
	}
	p, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"M42"})
	wantKind(t, err, ErrDenied)
	if p != nil {
		t.Errorf("plan = %+v, want none from a partial walk", p)
	}
}

// A wrong device or a rig that lost its identity ends the run as unreachable
// does: nothing more is dialled, probed or deleted.
func TestIdentityStopsRunDelete(t *testing.T) {
	for _, c := range []struct {
		name     string
		arm      func(fx *fixture)
		wantCode string
	}{
		{"identity", func(fx *fixture) {
			fx.prober.script(ip1, probeAnswer{guid: "someone-else", ok: true})
		}, "failed/identity"},
		{"host-bound", func(fx *fixture) {
			fx.editRig("r1", func(p *model.RigProfile) { p.GUID = "" })
		}, "failed/error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newFixture(t)
			withStore(t, fx)
			f := fx.dev(ip1)
			for _, n := range []string{"a", "b", "c"} {
				f.AddFile(emmc, "Preview/M42/"+n+".fit", []byte(n), t0)
			}
			plan, err := fx.svc.DeletePlan(ctx, "r1", emmc, "Preview", []string{"M42"})
			if err != nil {
				t.Fatal(err)
			}
			// b.fit's Stat drops the session once; the re-dial meets the armed failure.
			armed := false
			f.OnStat = func(_, p string) {
				if p == "Preview/M42/b.fit" && !armed {
					armed = true
					c.arm(fx)
					f.FailNext(device.FakeOpStat, 1, errNetworkForTest())
				}
			}
			var opensAtAbort, probesAtAbort int64
			var lines runLines
			emit := func(line any) error {
				if l, ok := line.(ItemLine); ok && l.Rel == "M42/b.fit" {
					opensAtAbort, probesAtAbort = fx.opens.Load(), int64(fx.prober.count())
				}
				return lines.emit(line)
			}
			if err := fx.svc.Delete(ctx, "r1", runRequest(emmc, "Preview", plan), emit); err != nil {
				t.Fatal(err)
			}
			f.OnStat = nil
			want := map[string]string{
				"M42/b.fit": c.wantCode,
				"M42/c.fit": "failed/not_attempted",
				"M42":       "failed/not_attempted",
			}
			if got := lines.codes(); !reflect.DeepEqual(got, want) {
				t.Errorf("item lines = %v, want %v", got, want)
			}
			if s := lines.summary; s == nil || s.Done != 1 || s.Failed != 3 || s.Aborted != "identity" {
				t.Errorf("summary = %+v, want 1 done, 3 failed, aborted identity", lines.summary)
			}
			if got := fx.opens.Load(); got != opensAtAbort {
				t.Errorf("opens went %d -> %d after the abort, want no further dial", opensAtAbort, got)
			}
			if got := int64(fx.prober.count()); got != probesAtAbort {
				t.Errorf("probes went %d -> %d after the abort, want no further probe", probesAtAbort, got)
			}
			for _, p := range []string{"Preview/M42/b.fit", "Preview/M42/c.fit"} {
				if !exists(t, f, emmc, p) {
					t.Errorf("%s was deleted", p)
				}
			}
		})
	}
}
