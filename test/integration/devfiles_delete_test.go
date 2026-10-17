//go:build integration

package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The device file browser's delete run and zip download, through the real
// devfiles.Service, against real Samba: a held-open file reports in_use, a
// non-empty folder is refused and kept, and a zip reads back.

const deleteRigID = "devfiles-delete"

type deleteStatus struct{}

func (deleteStatus) Snapshot() model.Snapshot {
	return model.Snapshot{Rigs: []model.RigStatus{
		{ID: deleteRigID, Address: host(), Site: string(netsite.Local), Online: true},
	}}
}

// deleteProber answers the identity probe with the rig's GUID: the container
// has no scan_air responder, and the probe is not what these tests exercise.
type deleteProber struct{}

func (deleteProber) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return "g-delete", "", true
}

// newDeleteService builds a devfiles.Service over the container with one
// managed, identified rig watching nothing, and a temp-dir store.
func newDeleteService(t *testing.T) *devfiles.Service {
	t.Helper()
	newDevice(t) // skip when no container is up
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var cfg atomic.Pointer[model.Config]
	cfg.Store(&model.Config{Rigs: []model.RigProfile{{
		ID: deleteRigID, Name: "Integration rig", GUID: "g-delete", Managed: true,
		User: "guest", Password: "12345678",
	}}})
	sites := netsite.NewRegistry(nil)
	svc := devfiles.New(devfiles.Deps{
		Config: &cfg, Store: st, Status: deleteStatus{}, Prober: deleteProber{}, Sites: sites,
		NewDevice: func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
			return device.New(sites, ep, user, pass, timeout)
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(svc.Close)
	return svc
}

// deleteRun plans and runs a delete of names in dir, returning the item lines
// by rel and the summary.
func deleteRun(t *testing.T, svc *devfiles.Service, dir string, names []string) (map[string]devfiles.ItemLine, devfiles.SummaryLine) {
	t.Helper()
	ctx := context.Background()
	plan, err := svc.DeletePlan(ctx, deleteRigID, shareName, dir, names)
	if err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	items := map[string]devfiles.ItemLine{}
	var sum devfiles.SummaryLine
	err = svc.Delete(ctx, deleteRigID, devfiles.DeleteRequest{
		Share: shareName, Dir: dir, Items: plan.Items, Dirs: plan.DirsDeepestFirst,
		AcknowledgedUnarchived: plan.Unarchived,
	}, func(line any) error {
		switch l := line.(type) {
		case devfiles.ItemLine:
			items[l.Rel] = l
		case devfiles.SummaryLine:
			sum = l
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	return items, sum
}

// A file another session holds open is reported in_use and kept; the run goes
// on and deletes its sibling.
func TestDevfilesServiceDeleteHeldOpen(t *testing.T) {
	svc := newDeleteService(t)
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	devfilesWrite(t, sh, path.Join(dir, "held.fit"), []byte("held"), 4)
	devfilesWrite(t, sh, path.Join(dir, "free.fit"), []byte("free"), 4)

	holder := devfilesMount(t)
	rc, err := holder.OpenReader(path.Join(dir, "held.fit"))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer rc.Close()

	items, sum := deleteRun(t, svc, dir, []string{"held.fit", "free.fit"})
	if l, ok := items["held.fit"]; !ok || l.Outcome != "failed" || l.Code != "in_use" || len(items) != 1 {
		t.Fatalf("item lines = %+v, want only held.fit failed/in_use", items)
	}
	if sum.Done != 1 || sum.Failed != 1 {
		t.Errorf("summary = %+v, want 1 done, 1 failed", sum)
	}
	if got := devfilesRead(t, sh, path.Join(dir, "held.fit")); string(got) != "held" {
		t.Errorf("held file = %q", got)
	}
	if _, err := sh.Stat(path.Join(dir, "free.fit")); !device.IsNotFound(err) {
		t.Errorf("free.fit: Stat err = %v, want deleted", err)
	}
}

// A folder that gained a file after the plan is refused by the device and
// reported not_empty, with the folder and the late file kept.
func TestDevfilesServiceDeleteNonEmptyRefused(t *testing.T) {
	svc := newDeleteService(t)
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	night := path.Join(dir, "Night1")
	if err := sh.Mkdir(night); err != nil {
		t.Fatal(err)
	}
	devfilesWrite(t, sh, path.Join(night, "a.fit"), []byte("a"), 1)

	ctx := context.Background()
	plan, err := svc.DeletePlan(ctx, deleteRigID, shareName, dir, []string{"Night1"})
	if err != nil {
		t.Fatal(err)
	}
	devfilesWrite(t, sh, path.Join(night, "late.fit"), []byte("late"), 4)

	items := map[string]devfiles.ItemLine{}
	err = svc.Delete(ctx, deleteRigID, devfiles.DeleteRequest{
		Share: shareName, Dir: dir, Items: plan.Items, Dirs: plan.DirsDeepestFirst,
	}, func(line any) error {
		if l, ok := line.(devfiles.ItemLine); ok {
			items[l.Rel] = l
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := items["Night1"]; !ok || l.Code != "not_empty" || len(items) != 1 {
		t.Fatalf("item lines = %+v, want only Night1 skipped/not_empty", items)
	}
	if got := devfilesRead(t, sh, path.Join(night, "late.fit")); string(got) != "late" {
		t.Errorf("late file = %q", got)
	}
	if _, err := sh.Stat(path.Join(night, "a.fit")); !device.IsNotFound(err) {
		t.Errorf("a.fit: Stat err = %v, want deleted", err)
	}
}

// zipSink is a DownloadWriter collecting the body.
type zipSink struct {
	info  devfiles.DownloadInfo
	begun int
	bytes.Buffer
}

func (s *zipSink) Begin(info devfiles.DownloadInfo) { s.info, s.begun = info, s.begun+1 }

// A zip download of a folder reads back with archive/zip: names, contents,
// the empty folder and the device mtime.
func TestDevfilesServiceZipDownload(t *testing.T) {
	svc := newDeleteService(t)
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	m42 := path.Join(dir, "M42")
	for _, d := range []string{m42, path.Join(m42, "empty"), path.Join(m42, "sub")} {
		if err := sh.Mkdir(d); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, 3<<20+777)
	for i := range big {
		big[i] = byte(i*7 + i>>9)
	}
	devfilesWrite(t, sh, path.Join(m42, "big.fit"), big, 512<<10)
	devfilesWrite(t, sh, path.Join(m42, "sub", "small.txt"), []byte("small"), 5)
	mtime := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
	if err := sh.Chtimes(path.Join(m42, "big.fit"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	var w zipSink
	if err := svc.Download(context.Background(), deleteRigID, shareName, dir, []string{"M42"}, &w); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if w.begun != 1 || !w.info.Zipped || w.info.Filename != "Integration rig-M42.zip" || w.info.Files != 2 {
		t.Errorf("Begin %d, info %+v", w.begun, w.info)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Bytes()), int64(w.Len()))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	got := map[string]*zip.File{}
	for _, f := range zr.File {
		got[f.Name] = f
	}
	for _, n := range []string{"M42/", "M42/big.fit", "M42/empty/", "M42/sub/", "M42/sub/small.txt"} {
		if got[n] == nil {
			t.Errorf("zip lacks %s (has %d entries)", n, len(got))
		}
	}
	if len(got) != 5 {
		t.Errorf("zip has %d entries, want 5", len(got))
	}
	read := func(n string) []byte {
		rc, err := got[n].Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(read("M42/big.fit"), big) {
		t.Error("big.fit differs after the round trip")
	}
	if string(read("M42/sub/small.txt")) != "small" {
		t.Error("small.txt differs")
	}
	if m := got["M42/big.fit"].Modified; !m.Equal(mtime) {
		t.Errorf("big.fit modified = %v, want %v", m, mtime)
	}
}
