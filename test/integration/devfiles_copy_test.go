//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// Service.Copy against real Samba: two managed rigs at the container's address
// stand in for two ASIAIRs, so a copy between them runs the cross-rig path on
// two sessions, through the real exclusive create, write deadline, rename and
// chtimes. Each test works in its own scratch folder at the share root.

const devfilesGUID = "integration-guid"

type devfilesStatus struct{ snap model.Snapshot }

func (s *devfilesStatus) Snapshot() model.Snapshot { return s.snap }

// devfilesProber answers every probe as the configured rigs' device.
type devfilesProber struct{}

func (devfilesProber) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return devfilesGUID, "", true
}

// devfilesService builds a service over rigs "a" and "b", both at the
// container. wrap, when set, wraps every device the service opens.
func devfilesService(t *testing.T, wrap func(device.Device) device.Device) *devfiles.Service {
	t.Helper()
	return devfilesServiceWith(t, devfilesOpts{wrap: wrap}).svc
}

// devfilesOpts tune devfilesServiceWith.
type devfilesOpts struct {
	// wrap, when set, wraps every device the service opens.
	wrap func(device.Device) device.Device
	// sources are both rigs' capture sources; none by default.
	sources []model.Source
	// base is the configuration the rigs are added to; nil starts empty.
	base *model.Config
	// logger replaces the test-output logger.
	logger *slog.Logger
}

// devfilesEnv is a service over rigs "a" and "b", its live configuration and
// its upload staging directory.
type devfilesEnv struct {
	svc     *devfiles.Service
	cfg     *atomic.Pointer[model.Config]
	staging string
}

// devfilesServiceWith builds a service over rigs "a" and "b", both at the
// container, with a real store and the real local filesystem, and upload
// staging started in a directory of its own.
func devfilesServiceWith(t *testing.T, o devfilesOpts) *devfilesEnv {
	t.Helper()
	newDevice(t) // skips when no container is reachable
	h := host()
	cfg := &atomic.Pointer[model.Config]{}
	profile := func(id string) model.RigProfile {
		return model.RigProfile{ID: id, Name: "Rig " + strings.ToUpper(id), GUID: devfilesGUID,
			Managed: true, User: "guest", Password: "12345678", Sources: o.sources}
	}
	c := model.Config{}
	if o.base != nil {
		c = *o.base
	}
	c.Rigs = []model.RigProfile{profile("a"), profile("b")}
	cfg.Store(&c)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	logger := o.logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	staging := filepath.Join(t.TempDir(), "upload-staging")
	sites := netsite.NewRegistry(nil)
	factory := device.NewFactory(sites)
	svc := devfiles.New(devfiles.Deps{
		Config:  cfg,
		Store:   st,
		LocalFS: localfs.New(),
		Status: &devfilesStatus{snap: model.Snapshot{Rigs: []model.RigStatus{
			{ID: "a", Address: h, Online: true}, {ID: "b", Address: h, Online: true},
		}}},
		Prober: devfilesProber{},
		Sites:  sites,
		NewDevice: func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
			d := factory(ep, user, pass, timeout)
			if o.wrap != nil {
				d = o.wrap(d)
			}
			return d
		},
		StagingDir: staging,
		Logger:     logger,
	})
	t.Cleanup(svc.Close)
	if err := svc.StartStaging(); err != nil {
		t.Fatalf("StartStaging: %v", err)
	}
	return &devfilesEnv{svc: svc, cfg: cfg, staging: staging}
}

// devfilesCopy runs a copy and returns its lines.
func devfilesCopy(t *testing.T, svc *devfiles.Service, req devfiles.CopyRequest) []any {
	t.Helper()
	var lines []any
	err := svc.Copy(context.Background(), req, func(line any) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if _, ok := lines[len(lines)-1].(devfiles.SummaryLine); !ok {
		t.Fatalf("last line = %#v, want a summary", lines[len(lines)-1])
	}
	return lines
}

func devfilesNoTemps(t *testing.T, sh device.Share, dir string) {
	t.Helper()
	entries, err := sh.ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name)
		if e.IsDir {
			devfilesNoTemps(t, sh, p)
		} else if strings.HasSuffix(e.Name, ".commander.tmp") {
			t.Errorf("temp file left behind: %s", p)
		}
	}
}

// A plan and run between two rigs copy a file several write chunks long, and
// a nested one, byte for byte, with the source's mtime, leaving no temp.
func TestDevfilesServiceCopy(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	src, dst := path.Join(dir, "src"), path.Join(dir, "dst")
	for _, d := range []string{src, path.Join(src, "M42"), dst} {
		if err := sh.Mkdir(d); err != nil {
			t.Fatalf("Mkdir(%q): %v", d, err)
		}
	}
	big := make([]byte, 3<<20+4321) // several of the copy's 512 KiB chunks
	for i := range big {
		big[i] = byte(i*7 + i>>13)
	}
	devfilesWrite(t, sh, path.Join(src, "big.fit"), big, 1<<20)
	devfilesWrite(t, sh, path.Join(src, "M42", "ref.fit"), []byte("reference"), 1<<20)
	old := time.Date(2025, 3, 4, 5, 6, 8, 0, time.UTC)
	for _, p := range []string{path.Join(src, "big.fit"), path.Join(src, "M42", "ref.fit")} {
		if err := sh.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes(%q): %v", p, err)
		}
	}

	svc := devfilesService(t, nil)
	from := devfiles.CopyPlace{Rig: "a", Share: shareName, Dir: src}
	to := devfiles.CopyPlace{Rig: "b", Share: shareName, Dir: dst}
	plan, err := svc.CopyPlan(context.Background(), devfiles.CopyPlanRequest{
		From: devfiles.CopySource{CopyPlace: from, Names: []string{"M42", "big.fit"}}, To: to})
	if err != nil {
		t.Fatalf("CopyPlan: %v", err)
	}
	if plan.Files != 2 || len(plan.Conflicts) != 0 || len(plan.Dirs) != 1 || plan.Dirs[0] != "M42" {
		t.Fatalf("plan = %+v", plan)
	}
	req := devfiles.CopyRequest{From: from, To: to, Dirs: plan.Dirs}
	for _, it := range plan.Items {
		req.Items = append(req.Items, devfiles.CopyItem{Rel: it.Rel, Size: it.Size})
	}
	lines := devfilesCopy(t, svc, req)
	if s := lines[len(lines)-1].(devfiles.SummaryLine); s.Done != 2 || s.Skipped+s.Failed != 0 {
		t.Fatalf("summary = %+v, lines %+v", s, lines)
	}

	check := devfilesMount(t)
	if got := devfilesRead(t, check, path.Join(dst, "big.fit")); !bytes.Equal(got, big) {
		t.Errorf("big.fit differs: %d bytes, want %d", len(got), len(big))
	}
	if got := devfilesRead(t, check, path.Join(dst, "M42", "ref.fit")); string(got) != "reference" {
		t.Errorf("ref.fit = %q", got)
	}
	fi, err := check.Stat(path.Join(dst, "big.fit"))
	if err != nil {
		t.Fatal(err)
	}
	if d := fi.Mtime.Sub(old); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("mtime = %v, want the source's %v", fi.Mtime, old)
	}
	devfilesNoTemps(t, check, dir)
}

// raceDevice writes the destination through its own session just before the
// copy's commit rename, so the real server's never-replacing rename meets it.
type raceDevice struct {
	device.Device
	race func(newPath string)
}

func (d *raceDevice) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &raceSession{Session: s, race: d.race}, nil
}

type raceSession struct {
	device.Session
	race func(newPath string)
}

func (s *raceSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return &raceShare{Share: sh, race: s.race}, nil
}

type raceShare struct {
	device.Share
	race func(newPath string)
}

func (s *raceShare) Rename(oldPath, newPath string) error {
	if strings.HasSuffix(oldPath, ".commander.tmp") {
		s.race(newPath)
	}
	return s.Share.Rename(oldPath, newPath)
}

// A destination another writer creates between the temp write and the commit
// survives byte-identical: the item is skipped as appeared and its temp removed.
func TestDevfilesServiceCopyRenameRace(t *testing.T) {
	sh := devfilesMount(t)
	dir := devfilesScratch(t, sh)
	src, dst := path.Join(dir, "src"), path.Join(dir, "dst")
	for _, d := range []string{src, dst} {
		if err := sh.Mkdir(d); err != nil {
			t.Fatalf("Mkdir(%q): %v", d, err)
		}
	}
	data := bytes.Repeat([]byte("copied "), 200_000)
	devfilesWrite(t, sh, path.Join(src, "a.fit"), data, 1<<20)

	racer := devfilesMount(t)
	theirs := []byte("written by another session")
	raced := 0
	svc := devfilesService(t, func(d device.Device) device.Device {
		return &raceDevice{Device: d, race: func(newPath string) {
			raced++
			devfilesWrite(t, racer, newPath, theirs, 1<<20)
		}}
	})
	from := devfiles.CopyPlace{Rig: "a", Share: shareName, Dir: src}
	to := devfiles.CopyPlace{Rig: "b", Share: shareName, Dir: dst}
	lines := devfilesCopy(t, svc, devfiles.CopyRequest{From: from, To: to,
		Items: []devfiles.CopyItem{{Rel: "a.fit", Size: int64(len(data))}}})

	if raced != 1 {
		t.Fatalf("raced %d times, want 1", raced)
	}
	var item *devfiles.ItemLine
	for _, l := range lines {
		if it, ok := l.(devfiles.ItemLine); ok {
			item = &it
		}
	}
	if item == nil || item.Outcome != "skipped" || item.Code != "appeared" ||
		item.DstSize == nil || *item.DstSize != int64(len(theirs)) {
		t.Fatalf("item = %+v, want skipped appeared with the racer's size", item)
	}
	check := devfilesMount(t)
	if got := devfilesRead(t, check, path.Join(dst, "a.fit")); !bytes.Equal(got, theirs) {
		t.Errorf("destination replaced: %d bytes", len(got))
	}
	devfilesNoTemps(t, check, dir)
}
