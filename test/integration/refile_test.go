//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The mosaic under test: a 2x5 grid, a shape the ASIAIR's mosaic planner produces.
const (
	refileRigID   = "rig-01"
	refileOTA     = "FRA300"
	refileSession = "S"
	refilePanelR  = 2
	refilePanelC  = 5
)

// refileAdmission grants every transfer immediately: the gate is not what this
// test is about, and a rename takes no slot at all.
type refileAdmission struct{}

func (refileAdmission) Acquire(context.Context, rig.Requester, int64) (func(), error) {
	return func() {}, nil
}

// refileResolver binds the rig by GUID so the worker never reaches for DNS: a
// GUID-bound profile resolves through the registry snapshot and the identity
// probe replies with the same GUID, which is what a healthy device does.
type refileResolver struct{ addr, guid string }

func (r refileResolver) Resolve(string, string) (netsite.Endpoint, bool) {
	return netsite.LocalEndpoint(r.addr), true
}
func (r refileResolver) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return r.guid, "cpu-1", true
}

// refileHarness runs a real worker over a real on-disk store and the real
// OS-backed localfs, with the device fake standing in for the ASIAIR. The device
// side is not what is under test here; the destination side is, and it is real.
type refileHarness struct {
	t      *testing.T
	dir    string
	light  string
	cfgPtr *atomic.Pointer[model.Config]
	cfg    *model.Config
	store  store.Store
	worker *rig.Worker
	ctx    context.Context
}

// panelTarget renders the ASIAIR's pane name for row/col: <base>_<row>-<col>.
func panelTarget(row, col int) string { return fmt.Sprintf("Sadr_%d-%d", row, col) }

// seedPanelFrames writes one real FITS per pane by rewriting the fixture's OBJECT
// card with the production writer, so every frame reaching the worker is a genuine
// ASIAIR light that differs from its siblings only in the card the routing reads.
func seedPanelFrames(t *testing.T, dev *device.Fake, share, workDir string) []string {
	t.Helper()
	src := filepath.Join(repoRoot(t), filepath.FromSlash(fixtureRel))
	mtime := time.Now().Add(-time.Hour)
	var targets []string
	for row := 1; row <= refilePanelR; row++ {
		for col := 1; col <= refilePanelC; col++ {
			target := panelTarget(row, col)
			dst := filepath.Join(workDir, target+".fit")
			if _, err := header.RewriteObject(src, dst, target); err != nil {
				t.Fatalf("RewriteObject(%q): %v", target, err)
			}
			data, err := os.ReadFile(dst)
			if err != nil {
				t.Fatalf("read seeded frame %q: %v", dst, err)
			}
			dev.AddFile(share, "Autorun/Light/"+target+"/Light_"+target+"_0001.fit", data, mtime)
			targets = append(targets, target)
		}
	}
	return targets
}

func newRefileHarness(t *testing.T) *refileHarness {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "seed")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	const share = "EMMC Images"
	dev := device.NewFake()
	dev.AddShare(share)
	seedPanelFrames(t, dev, share, work)

	cfg := config.Defaults()
	cfg.Defaults.MinFileAge = model.Duration(0)
	cfg.Defaults.SettleCycles = 0
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	cfg.Rigs = []model.RigProfile{{
		ID: refileRigID, Name: "Test", GUID: "guid-1", CPUID: "cpu-1",
		OTA: refileOTA, Session: refileSession, Managed: true,
		Sources: []model.Source{{Share: share, Roots: []string{"Autorun"}}},
	}}

	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := rig.NewWorker(
		refileRigID, ptr,
		func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev },
		refileResolver{addr: "127.0.0.1", guid: "guid-1"}, netsite.NewRegistry(logger),
		localfs.New(), header.New(), st, hub.New(logger), &notify.Fake{},
		refileAdmission{}, nil, nil, time.UTC, time.Now, metrics.New("integration"), logger,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	h := &refileHarness{
		t: t, dir: dir, light: cfg.Roots.Light, cfgPtr: ptr, cfg: cfg,
		store: st, worker: w, ctx: context.Background(),
	}
	h.waitForVerified(refilePanelR * refilePanelC)
	return h
}

// waitForVerified blocks until the worker's first cycle has filed and verified
// want frames, so the migration starts from a library the production filing path
// built rather than from hand-inserted rows.
func (h *refileHarness) waitForVerified(want int) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		frames, _, err := h.store.ListFrames(h.ctx, refileRigID, string(model.StateVerified), "", 0, 100)
		if err != nil {
			h.t.Fatalf("ListFrames: %v", err)
		}
		if len(frames) >= want {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("only %d of %d frames reached verified within the deadline", len(frames), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// enableMosaic hot-swaps a config with detection on, the operator action that
// makes the mosaic template the one the panels route through. The stored config
// is replaced rather than mutated: the worker goroutine reads it concurrently.
func (h *refileHarness) enableMosaic() {
	h.t.Helper()
	next := *h.cfg
	next.MosaicDetection = true
	if next.Templates.Mosaic == "" {
		h.t.Fatal("the shipped default leaves templates.mosaic empty; nothing would change")
	}
	h.cfgPtr.Store(&next)
}

func (h *refileHarness) preview(scope string) model.RefilePreview {
	h.t.Helper()
	pv, err := h.worker.PreviewRefile(h.ctx, scope)
	if err != nil {
		h.t.Fatalf("PreviewRefile(%q): %v", scope, err)
	}
	return pv
}

// flatPanelDir is where a panel lands with detection off — a top-level sibling of
// every other target, which is the problem the feature exists to remove.
func (h *refileHarness) flatPanelDir(target string) string {
	return filepath.Join(h.light, target, refileOTA, refileSession, "NIGHT_1")
}

// nestedPanelDir is where the default mosaic template puts the same panel.
func (h *refileHarness) nestedPanelDir(target string) string {
	return filepath.Join(h.light, "Sadr", refileOTA, refileSession, target, "NIGHT_1")
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// TestRefileMosaicMigrationRealFS is the end-to-end re-file case (acceptance
// criteria 22, 23 and 31), and it is here rather than in internal/rig because the
// prune's contract is a property of a real filesystem: os.Remove refusing a
// non-empty directory is the mechanism that leaves an operator's own files alone,
// and localfs.Fake only emulates it. The frames are built through the production
// filing path against a real on-disk store, so the rows the migration reads are
// the rows the app would have written.
func TestRefileMosaicMigrationRealFS(t *testing.T) {
	h := newRefileHarness(t)

	// The premise: ten panels filed flat, one top-level directory each — the
	// library shape the operator asked to have put "under one roof".
	for row := 1; row <= refilePanelR; row++ {
		for col := 1; col <= refilePanelC; col++ {
			if dir := h.flatPanelDir(panelTarget(row, col)); !dirExists(dir) {
				t.Fatalf("panel %s did not file to %s", panelTarget(row, col), dir)
			}
		}
	}

	// design routing-and-filing AC22: nothing has changed, so the unrestricted
	// diff — the widest scope there is — has nothing to offer. The mosaic scope
	// would be empty by construction while detection is off and would prove
	// nothing.
	if pv := h.preview("all"); len(pv.Groups) != 0 {
		t.Errorf("preview over an unchanged library returned %d groups, want 0: %+v", len(pv.Groups), pv.Groups)
	} else if len(pv.Refusals) != 0 {
		t.Errorf("preview over an unchanged library refused %d paths, want 0: %+v", len(pv.Refusals), pv.Refusals)
	}

	// Two obstructions, seeded into directories the migration is about to vacate.
	// Both are the ordinary case on a real NAS: a Finder .DS_Store, an operator's
	// stacked masters, a linked library root.
	operatorPanel := panelTarget(1, 2)
	operatorDir := h.flatPanelDir(operatorPanel)
	operatorFile := filepath.Join(operatorDir, "stacked_master.txt")
	if err := os.WriteFile(operatorFile, []byte("the operator's own file"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerPanel := panelTarget(1, 3)
	markerDir := filepath.Dir(h.flatPanelDir(markerPanel)) // the session folder
	markerFile := filepath.Join(markerDir, model.LibraryMarkerName)
	if err := os.WriteFile(markerFile, []byte("library-id=integration"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.enableMosaic()

	// design routing-and-filing AC23, first half: the diff groups all ten panel
	// directories, one group per panel, each naming the nested destination the
	// mosaic template renders.
	pv := h.preview("mosaic")
	if len(pv.Groups) != refilePanelR*refilePanelC {
		t.Fatalf("mosaic preview returned %d groups, want %d: %+v", len(pv.Groups), refilePanelR*refilePanelC, pv.Groups)
	}
	if len(pv.Refusals) != 0 {
		t.Errorf("mosaic preview refused %d paths, want 0: %+v", len(pv.Refusals), pv.Refusals)
	}
	wantGroups := map[string]string{}
	for row := 1; row <= refilePanelR; row++ {
		for col := 1; col <= refilePanelC; col++ {
			target := panelTarget(row, col)
			wantGroups[h.flatPanelDir(target)] = h.nestedPanelDir(target)
		}
	}
	for _, g := range pv.Groups {
		want, ok := wantGroups[g.FromDir]
		if !ok {
			t.Errorf("preview offered an unexpected source directory %q", g.FromDir)
			continue
		}
		if g.ToDir != want {
			t.Errorf("group %q → %q, want → %q", g.FromDir, g.ToDir, want)
		}
		if g.Count != 1 || len(g.FrameIDs) != 1 {
			t.Errorf("group %q: Count=%d FrameIDs=%v, want one frame", g.FromDir, g.Count, g.FrameIDs)
		}
		delete(wantGroups, g.FromDir)
	}
	if len(wantGroups) != 0 {
		t.Errorf("preview missed %d panel directories: %v", len(wantGroups), wantGroups)
	}

	// The preview warns about the directory it expects to strand, so the operator
	// is not left with a leftover folder and no explanation. It examines exactly
	// the vacated directories, so the marker one — which is an ancestor — is the
	// prune's report to make, not the preview's.
	var previewWarned bool
	for _, g := range pv.Groups {
		for _, r := range g.StrandRisk {
			if r.Dir == operatorDir && strings.Contains(r.Reason, "stacked_master.txt") {
				previewWarned = true
			}
		}
	}
	if !previewWarned {
		t.Errorf("preview did not warn that %s will not empty", operatorDir)
	}

	// Record every frame's identity before the move so the file at the new path
	// can be proven to be the same bytes, not merely a file of the right name.
	type before struct {
		id        int64
		sha, from string
	}
	pre := map[string]before{} // keyed by source directory
	for _, g := range pv.Groups {
		f, ok, err := h.store.GetFrameByID(h.ctx, g.FrameIDs[0])
		if err != nil || !ok {
			t.Fatalf("GetFrameByID(%d): ok=%v err=%v", g.FrameIDs[0], ok, err)
		}
		pre[g.FromDir] = before{id: f.ID, sha: fileSHA256(t, f.DestPath), from: f.DestPath}
	}

	// design routing-and-filing AC23, second half: apply every group. One HTTP
	// apply carries one group, so the migration is a sequence of them, exactly as
	// the SPA drives it.
	var pruned []string
	var skipped []model.StrandRisk
	for _, g := range pv.Groups {
		results, summary, err := h.worker.RefileGroup(h.ctx, g.ToDir, g.FrameIDs)
		if err != nil {
			t.Fatalf("RefileGroup(%q): %v", g.ToDir, err)
		}
		if len(results) != 1 {
			t.Fatalf("RefileGroup(%q) returned %d results, want 1: %+v", g.ToDir, len(results), results)
		}
		res := results[0]
		if res.Status != "moved" {
			t.Errorf("group %q → %q: status %q (%s), want moved", g.FromDir, g.ToDir, res.Status, res.Reason)
			continue
		}
		if filepath.Dir(res.To) != g.ToDir {
			t.Errorf("frame %d moved to %q, outside the selected group %q", res.FrameID, res.To, g.ToDir)
		}
		// dest_path is committed, and the bytes are readable and unchanged at it.
		f, ok, err := h.store.GetFrameByID(h.ctx, res.FrameID)
		if err != nil || !ok {
			t.Fatalf("GetFrameByID(%d) after the move: ok=%v err=%v", res.FrameID, ok, err)
		}
		if f.DestPath != res.To {
			t.Errorf("frame %d: dest_path %q, want %q", f.ID, f.DestPath, res.To)
		}
		if got := fileSHA256(t, f.DestPath); got != pre[g.FromDir].sha {
			t.Errorf("frame %d: bytes at %q hash %s, want %s", f.ID, f.DestPath, got, pre[g.FromDir].sha)
		}
		if _, err := os.Stat(pre[g.FromDir].from); err == nil {
			t.Errorf("frame %d: the original file survives at %q; a move must leave nothing behind", f.ID, pre[g.FromDir].from)
		}
		pruned = append(pruned, summary.PrunedDirs...)
		skipped = append(skipped, summary.SkippedDirs...)
	}

	// design routing-and-filing AC31, first half: a panel whose directories
	// genuinely emptied is removed all the way up to — but never including — the
	// configured root.
	prunedSet := map[string]bool{}
	for _, d := range pruned {
		prunedSet[d] = true
	}
	for row := 1; row <= refilePanelR; row++ {
		for col := 1; col <= refilePanelC; col++ {
			target := panelTarget(row, col)
			if target == operatorPanel || target == markerPanel {
				continue
			}
			top := filepath.Join(h.light, target)
			if dirExists(top) {
				t.Errorf("%s survived the migration; the flat panel folders are what it exists to remove", top)
			}
			if !prunedSet[top] {
				t.Errorf("prune did not report removing %s: %v", top, pruned)
			}
		}
	}
	if !dirExists(h.light) {
		t.Fatalf("the prune walked past the configured light root and removed %s", h.light)
	}

	// design routing-and-filing AC31, second half: a directory holding an operator
	// file, and one holding the library-link marker, are both left in place AND
	// reported. os.Remove refusing a non-empty directory is what makes the first
	// inert; the marker check is what makes the second deliberate.
	if !dirExists(operatorDir) {
		t.Errorf("%s was removed even though it holds an operator file", operatorDir)
	}
	if _, err := os.Stat(operatorFile); err != nil {
		t.Errorf("the operator's own file did not survive: %v", err)
	}
	if !dirExists(markerDir) {
		t.Errorf("%s was removed even though it holds %s", markerDir, model.LibraryMarkerName)
	}
	if _, err := os.Stat(markerFile); err != nil {
		t.Errorf("the library marker did not survive: %v", err)
	}
	if !reportedSkip(skipped, operatorDir, "stacked_master.txt") {
		t.Errorf("the prune did not report %s as skipped: %+v", operatorDir, skipped)
	}
	if !reportedSkip(skipped, markerDir, model.LibraryMarkerName) {
		t.Errorf("the prune did not report %s as skipped: %+v", markerDir, skipped)
	}
	// The marker panel's night folder did empty, so it is gone; only the session
	// folder above it survives.
	if dirExists(h.flatPanelDir(markerPanel)) {
		t.Errorf("%s emptied but was not removed", h.flatPanelDir(markerPanel))
	}

	// Every frame is readable where the database says it is — the whole point of
	// the migration, asserted once more over the finished library.
	frames, _, err := h.store.ListFrames(h.ctx, refileRigID, string(model.StateVerified), "", 0, 100)
	if err != nil {
		t.Fatalf("ListFrames: %v", err)
	}
	if len(frames) != refilePanelR*refilePanelC {
		t.Fatalf("after the migration %d frames are verified, want %d", len(frames), refilePanelR*refilePanelC)
	}
	var dirs []string
	for i := range frames {
		f := &frames[i]
		if _, err := os.Stat(f.DestPath); err != nil {
			t.Errorf("frame %d is not readable at its dest_path %q: %v", f.ID, f.DestPath, err)
		}
		dirs = append(dirs, filepath.Dir(f.DestPath))
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if !strings.HasPrefix(d, filepath.Join(h.light, "Sadr")+string(filepath.Separator)) {
			t.Errorf("frame directory %q is not under the single Sadr roof the migration exists to create", d)
		}
	}

	// Re-running the diff offers nothing: the migration is idempotent by
	// construction, which is why it needs no resume state.
	if pv := h.preview("mosaic"); len(pv.Groups) != 0 {
		t.Errorf("a second mosaic preview returned %d groups, want 0: %+v", len(pv.Groups), pv.Groups)
	}
}

// reportedSkip reports whether the prune named dir among the directories it could
// not remove, with a reason mentioning what is left in it.
func reportedSkip(skipped []model.StrandRisk, dir, mention string) bool {
	for _, s := range skipped {
		if s.Dir == dir && strings.Contains(s.Reason, mention) {
			return true
		}
	}
	return false
}
