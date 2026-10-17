//go:build integration && remotetest

package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/devfiles"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The device file browser across a remote: two rigs on two Samba servers, one
// reached directly on Commander's own LAN and one reached only through a real
// tailcat tunnel to the remote role (remote_harness_test.go).
//
//	rig "dfb-local":  site local, host():445 (the "samba" container)
//	rig "dfb-remote": site r1, remoteDevIP, which the remote's LAN dial maps
//	                  onto host():1445 (the "samba-remote" container)
//
// The service is wired as cmd/asiair-commander wires it, with the harness's
// site registry, a scanner over it for the remote rig's identity probe (the
// probe travels through the tunnel to the remote LAN's scan_air responder),
// and the harness's factory override of the remote SMB budget. The local
// container has no scan_air responder, so the local rig's probe is answered
// in-process, as the other devfiles integration tests do.

const (
	dfbLocal     = "dfb-local"
	dfbRemote    = "dfb-remote"
	dfbLocalGUID = "guid-dfb-local"
)

// remoteSMBAddr is the second Samba container, published on a port other than
// 445: Commander reaches it only through the remote's mapped LAN dial. It
// skips without the container, or fails when run.sh started it.
func remoteSMBAddr(t *testing.T) string {
	t.Helper()
	port := os.Getenv("AC_INTEGRATION_SMB_REMOTE_PORT")
	if port == "" {
		port = "1445"
	}
	addr := net.JoinHostPort(host(), port)
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		if os.Getenv("AC_INTEGRATION_SMB_HOST") == "" {
			t.Skipf("no second SMB server on %s; start both with test/integration/run.sh", addr)
		}
		t.Fatalf("second SMB server expected on %s: %v", addr, err)
	}
	c.Close()
	return addr
}

// dfbStatus publishes both rigs where their workers would have found them.
type dfbStatus struct{}

func (dfbStatus) Snapshot() model.Snapshot {
	return model.Snapshot{Rigs: []model.RigStatus{
		{ID: dfbLocal, Address: host(), Site: string(netsite.Local), Online: true},
		{ID: dfbRemote, Address: remoteDevIP, Site: remoteID, Online: true},
	}}
}

// dfbProber probes the remote rig for real, through the tunnel, and answers
// the local rig's probe in-process.
type dfbProber struct{ remote *discovery.Scanner }

func (p dfbProber) ProbeGUID(ctx context.Context, ep netsite.Endpoint) (string, string, bool) {
	if ep.Site == remoteID {
		return p.remote.ProbeGUID(ctx, ep)
	}
	return dfbLocalGUID, "", true
}

// dfbEnv is one remote harness and a devfiles.Service over both rigs, with
// the service's log lines and its upload staging directory.
type dfbEnv struct {
	h       *remoteHarness
	svc     *devfiles.Service
	logs    *logSink
	staging string
}

// newDfbEnv builds the harness over the second container and the service.
// wrapRemote, when set, wraps every device the service opens on the remote rig.
func newDfbEnv(t *testing.T, wrapRemote func(device.Device) device.Device) *dfbEnv {
	t.Helper()
	h := newRemoteHarness(t, remoteOptions{smbAddr: remoteSMBAddr(t)})

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	var cfg atomic.Pointer[model.Config]
	cfg.Store(&model.Config{Rigs: []model.RigProfile{
		{ID: dfbLocal, Name: "Local rig", GUID: dfbLocalGUID, Managed: true, User: smbUser, Password: smbPassword},
		{ID: dfbRemote, Name: "Remote rig", GUID: remoteGUID, CPUID: remoteCPUID, Managed: true, User: smbUser, Password: smbPassword},
	}})

	base := device.NewFactory(h.sites)
	newDev := func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
		if ep.Site != remoteID {
			return base(ep, user, pass, timeout)
		}
		d := base(ep, user, pass, commanderSMBBudget)
		if wrapRemote != nil {
			d = wrapRemote(d)
		}
		return d
	}
	// Commander's lines go to a sink of their own, which a test can read and
	// which prints only if the test fails.
	logs := &logSink{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("commander logs:\n%s", logs.String())
		}
	})
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})).With("side", "commander")
	staging := filepath.Join(t.TempDir(), "upload-staging")
	svc := devfiles.New(devfiles.Deps{
		Config:     &cfg,
		Store:      st,
		LocalFS:    localfs.New(),
		Status:     dfbStatus{},
		Prober:     dfbProber{remote: discovery.NewDefault(h.sites, base, nil, time.Now, log)},
		Sites:      h.sites,
		NewDevice:  newDev,
		StagingDir: staging,
		Logger:     log,
	})
	t.Cleanup(svc.Close)
	if err := svc.StartStaging(); err != nil {
		t.Fatalf("StartStaging: %v", err)
	}
	return &dfbEnv{h: h, svc: svc, logs: logs, staging: staging}
}

// scratchName is a unique share-root folder name for t; devfilesScratch's
// would nest a subtest's folder under its parent's name.
func scratchName(t *testing.T) string {
	return fmt.Sprintf("DevFiles-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
}

// remoteScratch makes a scratch folder at the root of the remote rig's share,
// directly, and removes it with everything in it at cleanup.
func (e *dfbEnv) remoteScratch(t *testing.T) string {
	t.Helper()
	dir := scratchName(t)
	if err := e.h.direct.Mkdir(dir); err != nil {
		t.Fatalf("Mkdir(%q) on the remote server: %v", dir, err)
	}
	t.Cleanup(func() { devfilesRemoveAll(t, e.h.direct, dir) })
	return dir
}

// localScratch makes a scratch folder at the root of the local rig's share,
// and removes it at cleanup through a session of its own.
func localScratch(t *testing.T, sh device.Share) string {
	t.Helper()
	dir := scratchName(t)
	if err := sh.Mkdir(dir); err != nil {
		t.Fatalf("Mkdir(%q): %v", dir, err)
	}
	t.Cleanup(func() { devfilesRemoveAll(t, devfilesMount(t), dir) })
	return dir
}

type zipEntry struct {
	data  []byte
	mtime time.Time
}

// readZip reads every entry of a zip body.
func readZip(t *testing.T, body []byte) map[string]zipEntry {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("zip: %v", err)
	}
	out := map[string]zipEntry{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("zip open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("zip read %s: %v", f.Name, err)
		}
		out[f.Name] = zipEntry{data: data, mtime: f.Modified}
	}
	return out
}

func mkdirs(t *testing.T, sh device.Share, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := sh.Mkdir(d); err != nil {
			t.Fatalf("Mkdir(%q): %v", d, err)
		}
	}
}

func patterned(n, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*seed + i>>12)
	}
	return b
}

// stageTree writes big.fit and M42/ref.fit under dir with a fixed old mtime,
// and returns big's bytes and that mtime.
func stageTree(t *testing.T, sh device.Share, dir string) ([]byte, time.Time) {
	t.Helper()
	mkdirs(t, sh, path.Join(dir, "M42"))
	big := patterned(5<<20+4321, 7) // ten of the copy's 512 KiB chunks and a tail
	devfilesWrite(t, sh, path.Join(dir, "big.fit"), big, 1<<20)
	devfilesWrite(t, sh, path.Join(dir, "M42", "ref.fit"), []byte("reference"), 1<<20)
	old := time.Date(2025, 3, 4, 5, 6, 8, 0, time.UTC)
	for _, p := range []string{path.Join(dir, "big.fit"), path.Join(dir, "M42", "ref.fit")} {
		if err := sh.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes(%q): %v", p, err)
		}
	}
	return big, old
}

// checkTree checks a copied stageTree under dir on sh.
func checkTree(t *testing.T, sh device.Share, dir string, big []byte, old time.Time) {
	t.Helper()
	if got := devfilesRead(t, sh, path.Join(dir, "big.fit")); !bytes.Equal(got, big) {
		t.Errorf("big.fit differs: %d bytes, want %d", len(got), len(big))
	}
	if got := devfilesRead(t, sh, path.Join(dir, "M42", "ref.fit")); string(got) != "reference" {
		t.Errorf("M42/ref.fit = %q", got)
	}
	for _, p := range []string{path.Join(dir, "big.fit"), path.Join(dir, "M42", "ref.fit")} {
		fi, err := sh.Stat(p)
		if err != nil {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if d := fi.Mtime.Sub(old); d < -2*time.Second || d > 2*time.Second {
			t.Errorf("%s mtime = %v, want the source's %v", p, fi.Mtime, old)
		}
	}
}

// planAndCopy plans a copy of names and runs it as planned, without
// overwrites, returning the plan and the summary.
func (e *dfbEnv) planAndCopy(t *testing.T, from, to devfiles.CopyPlace, names []string) (*devfiles.CopyPlan, []any) {
	t.Helper()
	plan, err := e.svc.CopyPlan(context.Background(), devfiles.CopyPlanRequest{
		From: devfiles.CopySource{CopyPlace: from, Names: names}, To: to})
	if err != nil {
		t.Fatalf("CopyPlan: %v", err)
	}
	return plan, devfilesCopy(t, e.svc, runOf(from, to, plan))
}

func runOf(from, to devfiles.CopyPlace, plan *devfiles.CopyPlan) devfiles.CopyRequest {
	req := devfiles.CopyRequest{From: from, To: to, Dirs: plan.Dirs}
	for _, it := range plan.Items {
		req.Items = append(req.Items, devfiles.CopyItem{Rel: it.Rel, Size: it.Size})
	}
	return req
}

func itemLines(lines []any) map[string]devfiles.ItemLine {
	out := map[string]devfiles.ItemLine{}
	for _, l := range lines {
		if it, ok := l.(devfiles.ItemLine); ok {
			out[it.Rel] = it
		}
	}
	return out
}

func summaryOf(lines []any) devfiles.SummaryLine {
	return lines[len(lines)-1].(devfiles.SummaryLine)
}

// TestDevfilesRemote runs the device file browser's operations against a rig
// reached through the tunnel, and copies between it and a rig on the local
// site, each on its own Samba server.
func TestDevfilesRemote(t *testing.T) {
	e := newDfbEnv(t, nil)
	local := devfilesMount(t)
	ctx := context.Background()
	// Every remote session is a forwarded flow: the remote rig's bytes crossed
	// the tunnel, not a direct dial.
	t.Cleanup(func() {
		if n := e.h.lan.smbForwards(); n == 0 {
			t.Errorf("no SMB flow was forwarded through the tunnel")
		} else {
			t.Logf("SMB flows forwarded through the tunnel: %d", n)
		}
	})

	t.Run("List", func(t *testing.T) {
		dir := e.remoteScratch(t)
		mkdirs(t, e.h.direct, path.Join(dir, "Night1"))
		devfilesWrite(t, e.h.direct, path.Join(dir, "Night1", "a.fit"), []byte("abc"), 3)

		shares, err := e.svc.List(ctx, dfbRemote, "", "")
		if err != nil {
			t.Fatalf("List shares: %v", err)
		}
		found := false
		for _, en := range shares.Entries {
			found = found || en.Name == shareName
		}
		if !found {
			t.Fatalf("share list = %+v, want %q", shares.Entries, shareName)
		}

		root, err := e.svc.List(ctx, dfbRemote, shareName, "")
		if err != nil {
			t.Fatalf("List share root: %v", err)
		}
		found = false
		for _, en := range root.Entries {
			found = found || (en.Name == dir && en.Dir)
		}
		if !found || !root.Writable {
			t.Fatalf("share root (writable %v) lacks the scratch folder %s: %+v", root.Writable, dir, root.Entries)
		}

		l, err := e.svc.List(ctx, dfbRemote, shareName, path.Join(dir, "Night1"))
		if err != nil {
			t.Fatalf("List folder: %v", err)
		}
		if len(l.Entries) != 1 || l.Entries[0].Name != "a.fit" || l.Entries[0].Size != 3 || l.Entries[0].Dir {
			t.Fatalf("folder entries = %+v", l.Entries)
		}
		// The local rig's share has no such folder: the two rigs are two servers.
		if _, err := local.Stat(dir); !device.IsNotFound(err) {
			t.Fatalf("the remote's scratch folder is on the local server too: %v", err)
		}
	})

	t.Run("CopyLocalToRemote", func(t *testing.T) {
		src := localScratch(t, local)
		big, old := stageTree(t, local, src)
		dst := e.remoteScratch(t)

		from := devfiles.CopyPlace{Rig: dfbLocal, Share: shareName, Dir: src}
		to := devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: dst}
		plan, lines := e.planAndCopy(t, from, to, []string{"M42", "big.fit"})
		if plan.Files != 2 || len(plan.Conflicts) != 0 || len(plan.Dirs) != 1 || plan.Dirs[0] != "M42" {
			t.Fatalf("plan = %+v", plan)
		}
		if s := summaryOf(lines); s.Done != 2 || s.Skipped+s.Failed != 0 || s.Aborted != "" {
			t.Fatalf("summary = %+v, lines %+v", s, lines)
		}
		checkTree(t, e.h.direct, dst, big, old)
		devfilesNoTemps(t, e.h.direct, dst)
	})

	t.Run("CopyRemoteToLocal", func(t *testing.T) {
		src := e.remoteScratch(t)
		big, old := stageTree(t, e.h.direct, src)
		dst := localScratch(t, local)

		from := devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: src}
		to := devfiles.CopyPlace{Rig: dfbLocal, Share: shareName, Dir: dst}
		plan, lines := e.planAndCopy(t, from, to, []string{"M42", "big.fit"})
		if plan.Files != 2 || len(plan.Conflicts) != 0 || len(plan.Dirs) != 1 {
			t.Fatalf("plan = %+v", plan)
		}
		if s := summaryOf(lines); s.Done != 2 || s.Skipped+s.Failed != 0 || s.Aborted != "" {
			t.Fatalf("summary = %+v, lines %+v", s, lines)
		}
		check := devfilesMount(t)
		checkTree(t, check, dst, big, old)
		devfilesNoTemps(t, check, dst)
	})

	t.Run("Conflict", func(t *testing.T) {
		src := localScratch(t, local)
		ours := patterned(1<<20+99, 13)
		devfilesWrite(t, local, path.Join(src, "a.fit"), ours, 1<<20)
		dst := e.remoteScratch(t)
		theirs := []byte("already on the remote")
		devfilesWrite(t, e.h.direct, path.Join(dst, "a.fit"), theirs, 1<<20)

		from := devfiles.CopyPlace{Rig: dfbLocal, Share: shareName, Dir: src}
		to := devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: dst}
		plan, err := e.svc.CopyPlan(ctx, devfiles.CopyPlanRequest{
			From: devfiles.CopySource{CopyPlace: from, Names: []string{"a.fit"}}, To: to})
		if err != nil {
			t.Fatalf("CopyPlan: %v", err)
		}
		if len(plan.Conflicts) != 1 {
			t.Fatalf("plan conflicts = %+v, want one", plan.Conflicts)
		}
		c := plan.Conflicts[0]
		if c.Rel != "a.fit" || c.Kind != "file" || c.DstSize != int64(len(theirs)) || c.SrcSize != int64(len(ours)) {
			t.Fatalf("conflict = %+v", c)
		}

		// Without an overwrite, the item a stale client still sends meets the
		// never-replacing rename through the tunnel: skipped as appeared. (A
		// conflict the user chose to skip never reaches the run: the client
		// drops it and reports chose_skip itself.)
		lines := devfilesCopy(t, e.svc, runOf(from, to, plan))
		it, ok := itemLines(lines)["a.fit"]
		if !ok || it.Outcome != "skipped" || it.Code != "appeared" || it.DstSize == nil || *it.DstSize != int64(len(theirs)) {
			t.Fatalf("run without overwrite: item = %+v (lines %+v), want skipped appeared", it, lines)
		}
		if s := summaryOf(lines); s.Skipped != 1 || s.Done+s.Failed != 0 {
			t.Fatalf("run without overwrite: summary = %+v", s)
		}
		if got := devfilesRead(t, e.h.direct, path.Join(dst, "a.fit")); !bytes.Equal(got, theirs) {
			t.Fatalf("the destination was replaced without an overwrite: %d bytes", len(got))
		}
		devfilesNoTemps(t, e.h.direct, dst)

		// With the overwrite the user confirmed, bound to what the plan saw.
		req := runOf(from, to, plan)
		req.Overwrite = []devfiles.CopyOverwrite{{Rel: c.Rel, DstSize: c.DstSize, DstMtime: c.DstMtime}}
		lines = devfilesCopy(t, e.svc, req)
		if s := summaryOf(lines); s.Done != 1 || s.Skipped+s.Failed != 0 {
			t.Fatalf("run with overwrite: summary = %+v, lines %+v", s, lines)
		}
		if got := devfilesRead(t, e.h.direct, path.Join(dst, "a.fit")); !bytes.Equal(got, ours) {
			t.Fatalf("the overwrite did not land: %d bytes, want %d", len(got), len(ours))
		}
		devfilesNoTemps(t, e.h.direct, dst)
	})

	t.Run("Delete", func(t *testing.T) {
		dir := e.remoteScratch(t)
		sh := e.h.direct
		mkdirs(t, sh, path.Join(dir, "Empty"), path.Join(dir, "Night1"))
		devfilesWrite(t, sh, path.Join(dir, "a.fit"), []byte("a"), 1)
		devfilesWrite(t, sh, path.Join(dir, "Night1", "x.fit"), []byte("x"), 1)

		plan, err := e.svc.DeletePlan(ctx, dfbRemote, shareName, dir, []string{"a.fit", "Empty", "Night1"})
		if err != nil {
			t.Fatalf("DeletePlan: %v", err)
		}
		if plan.Files != 2 || plan.Dirs != 2 {
			t.Fatalf("plan = %+v, want 2 files and 2 folders", plan)
		}
		// A file lands in Night1 after the plan: the device must refuse the folder.
		devfilesWrite(t, sh, path.Join(dir, "Night1", "late.fit"), []byte("late"), 4)

		items := map[string]devfiles.ItemLine{}
		var sum devfiles.SummaryLine
		err = e.svc.Delete(ctx, dfbRemote, devfiles.DeleteRequest{
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
		if l, ok := items["Night1"]; !ok || l.Code != "not_empty" || len(items) != 1 {
			t.Fatalf("item lines = %+v, want only Night1 not_empty", items)
		}
		if sum.Done != 3 {
			t.Errorf("summary = %+v, want 3 done (a.fit, Night1/x.fit, Empty)", sum)
		}
		for _, p := range []string{"a.fit", "Empty", "Night1/x.fit"} {
			if _, err := sh.Stat(path.Join(dir, p)); !device.IsNotFound(err) {
				t.Errorf("%s: Stat err = %v, want deleted", p, err)
			}
		}
		if got := devfilesRead(t, sh, path.Join(dir, "Night1", "late.fit")); string(got) != "late" {
			t.Errorf("late.fit = %q", got)
		}
	})

	t.Run("Upload", func(t *testing.T) {
		dst := e.remoteScratch(t)
		data := patterned(3<<20+555, 9) // several of the sink's 512 KiB chunks
		id := devfilesStage(t, e.svc, uploadOwner, "ref.fit", data)
		to := devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: dst}
		plan, err := e.svc.UploadPlan(ctx, uploadOwner, devfiles.UploadPlanRequest{ID: id, To: to})
		if err != nil {
			t.Fatalf("UploadPlan: %v", err)
		}
		if plan.Files != 1 || len(plan.Conflicts) != 0 || plan.FreeBytes == nil {
			t.Fatalf("plan = %+v (free %v)", plan, plan.FreeBytes)
		}
		start := time.Now()
		it, sum := devfilesUpload(t, e.svc, devfiles.UploadRequest{ID: id, To: to})
		t.Logf("upload of %d bytes through the tunnel took %v", len(data), time.Since(start).Round(time.Millisecond))
		if it.Outcome != "copied" || sum.Done != 1 {
			t.Fatalf("item %+v, summary %+v", it, sum)
		}
		if got := devfilesRead(t, e.h.direct, path.Join(dst, "ref.fit")); !bytes.Equal(got, data) {
			t.Fatalf("ref.fit differs after the tunnel: %d bytes, want %d", len(got), len(data))
		}
		fi, err := e.h.direct.Stat(path.Join(dst, "ref.fit"))
		if err != nil {
			t.Fatal(err)
		}
		if d := fi.Mtime.Sub(uploadMtime); d < -2*time.Second || d > 2*time.Second {
			t.Errorf("mtime = %v, want the PC's %v", fi.Mtime, uploadMtime)
		}
		devfilesNoTemps(t, e.h.direct, dst)
		if !stagedHeld(t, e.staging, id) {
			t.Fatal("the run removed the staged file")
		}
		// Local, so the remote's copy of the folder has no such file.
		if _, err := local.Stat(path.Join(dst, "ref.fit")); !device.IsNotFound(err) {
			t.Fatalf("the upload landed on the local server: %v", err)
		}
	})

	t.Run("Rename", func(t *testing.T) {
		dir := e.remoteScratch(t)
		devfilesWrite(t, e.h.direct, path.Join(dir, "a.fit"), []byte("frame a"), 1<<20)
		devfilesWrite(t, e.h.direct, path.Join(dir, "c.fit"), []byte("frame c"), 1<<20)
		if err := e.svc.Rename(ctx, dfbRemote, shareName, dir, "a.fit", "b.fit"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		if got := devfilesRead(t, e.h.direct, path.Join(dir, "b.fit")); string(got) != "frame a" {
			t.Fatalf("b.fit = %q", got)
		}
		if _, err := e.h.direct.Stat(path.Join(dir, "a.fit")); !device.IsNotFound(err) {
			t.Fatalf("a.fit still there: %v", err)
		}
		err := e.svc.Rename(ctx, dfbRemote, shareName, dir, "b.fit", "c.fit")
		if !errors.Is(err, devfiles.ErrExists) || !strings.Contains(err.Error(), "Remote rig already has") {
			t.Fatalf("Rename onto a taken name err = %v, want exists naming the remote rig", err)
		}
		if got := devfilesRead(t, e.h.direct, path.Join(dir, "c.fit")); string(got) != "frame c" {
			t.Fatalf("c.fit was replaced: %q", got)
		}
	})

	t.Run("ZipDownload", func(t *testing.T) {
		dir := e.remoteScratch(t)
		sh := e.h.direct
		m42 := path.Join(dir, "M42")
		mkdirs(t, sh, m42, path.Join(m42, "empty"), path.Join(m42, "sub"))
		big := patterned(3<<20+777, 11)
		devfilesWrite(t, sh, path.Join(m42, "big.fit"), big, 512<<10)
		devfilesWrite(t, sh, path.Join(m42, "sub", "small.txt"), []byte("small"), 5)
		mtime := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
		if err := sh.Chtimes(path.Join(m42, "big.fit"), mtime, mtime); err != nil {
			t.Fatal(err)
		}

		var w zipSink
		if err := e.svc.Download(ctx, dfbRemote, shareName, dir, []string{"M42"}, &w); err != nil {
			t.Fatalf("Download: %v", err)
		}
		if w.begun != 1 || !w.info.Zipped || w.info.Filename != "Remote rig-M42.zip" || w.info.Files != 2 {
			t.Errorf("Begin %d, info %+v", w.begun, w.info)
		}
		got := readZip(t, w.Bytes())
		for _, n := range []string{"M42/", "M42/big.fit", "M42/empty/", "M42/sub/", "M42/sub/small.txt"} {
			if _, ok := got[n]; !ok {
				t.Errorf("zip lacks %s (has %d entries)", n, len(got))
			}
		}
		if len(got) != 5 {
			t.Errorf("zip has %d entries, want 5", len(got))
		}
		if !bytes.Equal(got["M42/big.fit"].data, big) {
			t.Error("big.fit differs after the round trip through the tunnel")
		}
		if string(got["M42/sub/small.txt"].data) != "small" {
			t.Error("small.txt differs")
		}
		if m := got["M42/big.fit"].mtime; !m.Equal(mtime) {
			t.Errorf("big.fit modified = %v, want %v", m, mtime)
		}
	})
}

// cutWrites wraps the remote rig's devices so that the first file write to
// reach after bytes runs cut (taking the tunnel down) before going on.
type cutWrites struct {
	after int64
	cut   func()

	once    sync.Once
	written atomic.Int64

	mu      sync.Mutex
	creates []string // every path CreateNew was asked for, in order
}

func (c *cutWrites) created() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.creates...)
}

func (c *cutWrites) wrap(d device.Device) device.Device { return cutDevice{Device: d, c: c} }

type cutDevice struct {
	device.Device
	c *cutWrites
}

func (d cutDevice) Open(ctx context.Context) (device.Session, error) {
	s, err := d.Device.Open(ctx)
	if err != nil {
		return nil, err
	}
	return cutSession{Session: s, c: d.c}, nil
}

type cutSession struct {
	device.Session
	c *cutWrites
}

func (s cutSession) Mount(name string) (device.Share, error) {
	sh, err := s.Session.Mount(name)
	if err != nil {
		return nil, err
	}
	return cutShare{Share: sh, c: s.c}, nil
}

type cutShare struct {
	device.Share
	c *cutWrites
}

func (s cutShare) CreateNew(p string) (io.WriteCloser, error) {
	s.c.mu.Lock()
	s.c.creates = append(s.c.creates, p)
	s.c.mu.Unlock()
	w, err := s.Share.CreateNew(p)
	if err != nil {
		return nil, err
	}
	return cutWriter{WriteCloser: w, c: s.c}, nil
}

type cutWriter struct {
	io.WriteCloser
	c *cutWrites
}

func (w cutWriter) Write(b []byte) (int, error) {
	if w.c.written.Add(int64(len(b))) > w.c.after {
		w.c.once.Do(w.c.cut)
	}
	return w.WriteCloser.Write(b)
}

// TestDevfilesRemoteTunnelDrop takes the remote down in the middle of a copy,
// on either side of it: the copy reports the remote rig unreachable for the
// item in flight and the rest as not attempted, and no destination file
// appears under its final name. An upload whose tunnel drops mid-write is
// retried as a copy's item is, each attempt from byte 0 of the staged file on
// a fresh temp.
func TestDevfilesRemoteTunnelDrop(t *testing.T) {
	names := []string{"a.fit", "b.fit", "c.fit"}
	stage := func(t *testing.T, sh device.Share, dir string) {
		devfilesWrite(t, sh, path.Join(dir, "a.fit"), patterned(4<<20, 5), 1<<20)
		devfilesWrite(t, sh, path.Join(dir, "b.fit"), []byte("b"), 1)
		devfilesWrite(t, sh, path.Join(dir, "c.fit"), []byte("c"), 1)
	}
	// check asserts the run's lines and that no item reached its final name.
	check := func(t *testing.T, e *dfbEnv, lines []any, dst device.Share, dstDir string) {
		t.Helper()
		items := itemLines(lines)
		if a := items["a.fit"]; a.Outcome != "failed" || a.Code != "unreachable" || !strings.Contains(a.Message, "Remote rig") {
			t.Errorf("a.fit = %+v, want failed unreachable naming the remote rig", a)
		}
		for _, n := range []string{"b.fit", "c.fit"} {
			if it := items[n]; it.Outcome != "failed" || it.Code != "not_attempted" || !strings.Contains(it.Message, "Remote rig") {
				t.Errorf("%s = %+v, want failed not_attempted naming the remote rig", n, it)
			}
		}
		if s := summaryOf(lines); s.Aborted != "unreachable" || s.Failed != 3 || s.Done+s.Skipped != 0 {
			t.Errorf("summary = %+v, want 3 failed, aborted unreachable", s)
		}
		for _, n := range names {
			if _, err := dst.Stat(path.Join(dstDir, n)); !device.IsNotFound(err) {
				t.Errorf("%s exists under its final name after the drop (Stat err %v)", n, err)
			}
		}
		t.Logf("lines: %+v", lines)
	}
	run := func(t *testing.T, e *dfbEnv, from, to devfiles.CopyPlace) []any {
		t.Helper()
		plan, err := e.svc.CopyPlan(context.Background(), devfiles.CopyPlanRequest{
			From: devfiles.CopySource{CopyPlace: from, Names: names}, To: to})
		if err != nil {
			t.Fatalf("CopyPlan: %v", err)
		}
		start := time.Now()
		lines := devfilesCopy(t, e.svc, runOf(from, to, plan))
		t.Logf("copy across the drop took %v", time.Since(start).Round(time.Millisecond))
		return lines
	}

	// The remote is the source: the cut lands in its replies, 1 MiB into a.fit.
	t.Run("RemoteSource", func(t *testing.T) {
		e := newDfbEnv(t, nil)
		local := devfilesMount(t)
		src := e.remoteScratch(t)
		stage(t, e.h.direct, src)
		dst := localScratch(t, local)

		from := devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: src}
		to := devfiles.CopyPlace{Rig: dfbLocal, Share: shareName, Dir: dst}
		plan, err := e.svc.CopyPlan(context.Background(), devfiles.CopyPlanRequest{
			From: devfiles.CopySource{CopyPlace: from, Names: names}, To: to})
		if err != nil {
			t.Fatalf("CopyPlan: %v", err)
		}
		cut := e.h.lan.armCut(1<<20, e.h.stopRemote)
		lines := devfilesCopy(t, e.svc, runOf(from, to, plan))
		select {
		case <-cut:
		default:
			t.Fatal("the copy ended without reaching the cut")
		}
		check(t, e, lines, local, dst)
		// The destination is reachable, so the temp of the item in flight is gone.
		devfilesNoTemps(t, devfilesMount(t), dst)
	})

	// The remote is the destination: the cut lands 1 MiB into a.fit's write.
	// The temp in flight may stay on the unreachable remote (cleanup is
	// best-effort and junk to the harvester); only the final names must not.
	t.Run("RemoteDestination", func(t *testing.T) {
		cw := &cutWrites{after: 1 << 20}
		e := newDfbEnv(t, cw.wrap)
		cw.cut = e.h.stopRemote
		local := devfilesMount(t)
		src := localScratch(t, local)
		stage(t, local, src)
		dst := e.remoteScratch(t)

		from := devfiles.CopyPlace{Rig: dfbLocal, Share: shareName, Dir: src}
		to := devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: dst}
		lines := run(t, e, from, to)
		check(t, e, lines, e.h.direct, dst)
		entries, err := e.h.direct.ListDir(dst)
		if err != nil {
			t.Fatalf("ListDir: %v", err)
		}
		for _, en := range entries {
			t.Logf("left on the remote: %s (%d bytes)", en.Name, en.Size)
		}
	})

	// The remote is an upload's destination: the cut lands 1 MiB into the
	// write, and the remote either restarts at once, as a container restart
	// would, or stays down. Whether a restarted tunnel is back within the
	// attempts depends on the supervisor's reconnect, so either end is
	// accepted for both, each with its own guarantees: complete and
	// byte-identical after a retry on a fresh temp, or unreachable with
	// nothing under the final name. The staged file is kept either way.
	for _, tc := range []struct {
		name    string
		restart bool
	}{{"UploadRemoteDestinationRestarts", true}, {"UploadRemoteDestinationStaysDown", false}} {
		t.Run(tc.name, func(t *testing.T) { uploadAcrossDrop(t, tc.restart) })
	}
}

// uploadAcrossDrop uploads to the remote rig and cuts the tunnel 1 MiB into
// the write, restarting the remote at once when restart is set.
func uploadAcrossDrop(t *testing.T, restart bool) {
	cw := &cutWrites{after: 1 << 20}
	e := newDfbEnv(t, cw.wrap)
	cw.cut = e.h.stopRemote
	if restart {
		cw.cut = e.h.restartRemote
	}
	dst := e.remoteScratch(t)
	data := patterned(4<<20+77, 5)
	id := devfilesStage(t, e.svc, uploadOwner, "a.fit", data)

	start := time.Now()
	it, sum := devfilesUpload(t, e.svc, devfiles.UploadRequest{ID: id,
		To: devfiles.CopyPlace{Rig: dfbRemote, Share: shareName, Dir: dst}})
	t.Logf("upload across the drop took %v: item %+v, summary %+v", time.Since(start).Round(time.Millisecond), it, sum)
	if cw.written.Load() <= cw.after {
		t.Fatal("the upload ended without reaching the cut")
	}
	logs := e.logs.String()
	retried := false
	for _, l := range strings.Split(logs, "\n") {
		retried = retried || (strings.Contains(l, `msg="devfiles: retrying"`) &&
			strings.Contains(l, "rig=dfb-remote") && strings.Contains(l, "op=Upload"))
	}
	if !retried {
		t.Fatalf("the upload was not retried after the drop")
	}
	creates := cw.created()
	t.Logf("CreateNew calls: %q", creates)
	for _, p := range creates {
		if !strings.HasSuffix(p, ".commander.tmp") {
			t.Errorf("CreateNew(%q): the upload created something other than a temp", p)
		}
	}

	entries := listNames(t, e.h.direct, dst)
	switch {
	case it.Outcome == "copied":
		// Retried from byte 0 on a fresh temp: a second, differently named
		// temp, and the whole file under the final name.
		if len(creates) < 2 || creates[0] == creates[len(creates)-1] {
			t.Errorf("CreateNew calls %q, want a fresh temp for the retry", creates)
		}
		if got := devfilesRead(t, e.h.direct, path.Join(dst, "a.fit")); !bytes.Equal(got, data) {
			t.Errorf("a.fit differs after the retry: %d bytes, want %d", len(got), len(data))
		}
		devfilesNoTemps(t, e.h.direct, dst)
	case it.Outcome == "failed" && it.Code == "unreachable":
		if !strings.Contains(it.Message, "Remote rig") || sum.Aborted != "unreachable" {
			t.Errorf("item %+v, summary %+v: want unreachable naming the remote rig", it, sum)
		}
		if _, ok := entries["a.fit"]; ok {
			t.Error("a.fit stands under its final name after an unreachable upload")
		}
		// Cleanup is best-effort on an unreachable rig: a temp may stay,
		// junk to the harvester, and its failed removal is logged.
		t.Logf("left on the remote: %v", entries)
		for name := range entries {
			if !strings.HasSuffix(name, ".commander.tmp") {
				t.Errorf("left on the remote: %s, which is not a temp", name)
			} else if !strings.Contains(logs, `msg="devfiles: temp cleanup failed"`) {
				t.Errorf("temp %s left with no cleanup failure logged", name)
			}
		}
	default:
		t.Fatalf("item = %+v, want copied after a retry or failed unreachable", it)
	}
	if !stagedHeld(t, e.staging, id) {
		t.Fatal("the run removed the staged file")
	}
}
