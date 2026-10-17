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
	"path"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/header/fitstest"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
	dto "github.com/prometheus/client_model/go"
)

// drainDir is this test's own device-side directory (design
// frame-lifecycle §12), pre-created by the Dockerfile: reusing frameDir or
// bigDir would put the rig's delete pass in charge of their fixtures too.
const (
	drainDir      = "Autorun/Light/DPYDRAIN"
	drainRigID    = "rig-drain"
	drainOTA      = "FRA400"
	drainSession  = "S"
	drainFrameCnt = 20
)

// drainAdmission grants every transfer immediately: admission gating is not
// what this test is about.
type drainAdmission struct{}

func (drainAdmission) Acquire(context.Context, rig.Requester, int64) (func(), error) {
	return func() {}, nil
}

// drainResolver binds the rig by GUID so the worker never reaches for DNS or a
// real scan_air probe, which the Samba container does not answer.
type drainResolver struct{ addr, guid string }

func (r drainResolver) Resolve(string, string) (netsite.Endpoint, bool) {
	return netsite.LocalEndpoint(r.addr), true
}
func (r drainResolver) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return r.guid, "", true
}

// drainHarness runs a real worker, a real on-disk store and the real SMB1
// device against the live container, with a short grace and a 1 s poll so a
// backlog of drainFrameCnt frames drains within the test's own deadline
// regardless of how many slices a fast Docker link needs.
type drainHarness struct {
	t     *testing.T
	share device.Share
	store store.Store
	w     *rig.Worker
	ctx   context.Context
	light string
	mx    *metrics.Metrics
}

func newDrainHarness(t *testing.T, grace, poll time.Duration) *drainHarness {
	t.Helper()
	dev := newDevice(t)
	ctx := context.Background()

	sess, err := dev.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	share, err := sess.Mount(shareName)
	if err != nil {
		t.Fatalf("Mount(%q): %v", shareName, err)
	}
	t.Cleanup(func() { share.Umount() })

	dir := t.TempDir()
	cfg := config.Defaults()
	// Both quiescence gates off, as the revision harness beside this one does:
	// the frame's age is not what this test is about.
	cfg.Defaults.MinFileAge = model.Duration(0)
	cfg.Defaults.SettleCycles = 0
	cfg.Defaults.Grace = model.Duration(grace)
	cfg.Defaults.PollInterval = model.Duration(poll)
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	cfg.Rigs = []model.RigProfile{{
		ID: drainRigID, Name: "Drain", GUID: "guid-drain", CPUID: "cpu-drain",
		OTA: drainOTA, Session: drainSession, Managed: true,
		// Scoped to this test's own directory: BIGNIGHT's 400 junk files and
		// NGC7000's seeded frame must never enter this rig's delete pass.
		Sources: []model.Source{{Share: shareName, Roots: []string{drainDir}}},
	}}

	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mx := metrics.New("integration-drain")
	w := rig.NewWorker(
		drainRigID, ptr,
		func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev },
		drainResolver{addr: host(), guid: "guid-drain"}, netsite.NewRegistry(logger),
		localfs.New(), header.New(), st, hub.New(logger), &notify.Fake{},
		drainAdmission{}, nil, nil, time.UTC, time.Now, mx, logger,
	)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return &drainHarness{t: t, share: share, store: st, w: w, ctx: ctx, light: cfg.Roots.Light, mx: mx}
}

func drainObject(i int) string    { return fmt.Sprintf("DPYDRAIN%02d", i) }
func drainFrameName(i int) string { return fmt.Sprintf("Light_%s_0001.fit", drainObject(i)) }

// writeFrame puts a synthetic light frame for index i directly on the share, at
// its own OBJECT so it routes to its own destination directory: classification
// reads the header, not the source directory or file name, so every frame is
// distinguishable by content alone.
func (h *drainHarness) writeFrame(i int) {
	h.t.Helper()
	cards := fitstest.LightCards(60, 48, drainObject(i))
	body := fitstest.Build(cards, fitstest.SyntheticImage(60, 48))
	if err := h.share.WriteMarkerAt(drainDir, drainFrameName(i), body); err != nil {
		h.t.Fatalf("write frame %d: %v", i, err)
	}
}

// counterValue sums the value of every series of counter `name` whose labels
// are a superset of the requested ones, read from the worker's real metrics
// registry — the same pattern internal/rig's harness uses, since
// frames_deleted_total{manner="device_deleted"} is the one place a duplicate
// device delete for the same frame would show up as a count above the number
// of originals, rather than just an idempotent re-write of state=deleted.
func (h *drainHarness) counterValue(name string, labels map[string]string) float64 {
	h.t.Helper()
	fams, err := h.mx.Registry().Gather()
	if err != nil {
		h.t.Fatalf("gather: %v", err)
	}
	total := 0.0
	for _, mf := range fams {
		if mf.GetName() != name || mf.GetType() != dto.MetricType_COUNTER {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

func (h *drainHarness) frame(i int) (model.Frame, bool) {
	h.t.Helper()
	f, ok, err := h.store.GetFrame(h.ctx, drainRigID, shareName, path.Join(drainDir, drainFrameName(i)))
	if err != nil {
		h.t.Fatalf("GetFrame(%d): %v", i, err)
	}
	if !ok {
		return model.Frame{}, false
	}
	return *f, true
}

// TestSlicedDrainExactCount (design frame-lifecycle §12.9) stages a backlog
// through the real pull path against a live Samba/NT1 container, with a short
// grace and a 1 s poll, and drops one more frame on the share mid-drain. A fast
// Docker link is not deterministic about how many slices the drain takes — the
// design says so explicitly — so this asserts only the end state: every
// eligible original gone from the device exactly once, every NAS copy
// re-hashing to its recorded sha256, the mid-drain frame filed and verified,
// and nothing else touched. On a slow runner the drain can outlast the
// mid-drain frame's own grace, so that frame may end deleted too; the device
// listing and the exact delete count follow whichever end state it reached.
func TestSlicedDrainExactCount(t *testing.T) {
	const grace = 6 * time.Second
	const poll = 1 * time.Second
	h := newDrainHarness(t, grace, poll)

	t.Cleanup(func() {
		for i := 1; i <= drainFrameCnt+1; i++ {
			_ = h.share.Delete(path.Join(drainDir, drainFrameName(i)))
		}
	})

	for i := 1; i <= drainFrameCnt; i++ {
		h.writeFrame(i)
	}

	// Wait for every original to be pulled and verified before grace opens the
	// gate, so the drain's eligible set starts as the whole backlog at once.
	// grace is long enough that the first-pulled frame cannot already be
	// delete-eligible while the last is still settling (State == Deleted is
	// accepted too, as a defensive fallback, since it still proves the frame
	// was once pulled and verified).
	pullDeadline := time.Now().Add(60 * time.Second)
	for {
		done := true
		for i := 1; i <= drainFrameCnt; i++ {
			f, ok := h.frame(i)
			if !ok || (f.State != model.StateVerified && f.State != model.StateDeleted) {
				done = false
				break
			}
		}
		if done {
			break
		}
		if time.Now().After(pullDeadline) {
			t.Fatalf("the %d originals were not all pulled and verified within the deadline", drainFrameCnt)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Drop the extra frame after grace opens the gate: whether the drain then
	// takes one cycle or several is exactly the non-determinism the design
	// says not to assert on, so this is a best-effort "mid-drain", not a
	// synchronized one.
	go func() {
		time.Sleep(grace + 300*time.Millisecond)
		h.writeFrame(drainFrameCnt + 1)
	}()

	// Drive cycles (the worker's own ticker; poll is 1 s) until every original
	// is deleted and the mid-drain frame is filed and verified — or, once its own
	// grace has passed, deleted in turn. Bounded so a stuck drain fails loudly
	// instead of hanging the test run.
	drainDeadline := time.Now().Add(90 * time.Second)
	for {
		allDeleted := true
		for i := 1; i <= drainFrameCnt; i++ {
			f, ok := h.frame(i)
			if !ok || f.State != model.StateDeleted {
				allDeleted = false
				break
			}
		}
		newFrame, newOK := h.frame(drainFrameCnt + 1)
		if allDeleted && newOK && (newFrame.State == model.StateVerified || newFrame.State == model.StateDeleted) {
			break
		}
		if time.Now().After(drainDeadline) {
			t.Fatalf("drain did not complete within the deadline: originals deleted=%v, new frame present=%v state=%q",
				allDeleted, newOK, newFrame.State)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Every eligible original is gone from the device listing, and the
	// mid-drain frame is the only thing left in its place unless it was deleted
	// in turn, which leaves the directory empty.
	entries, err := h.share.ListDir(drainDir)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", drainDir, err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for i := 1; i <= drainFrameCnt; i++ {
		if names[drainFrameName(i)] {
			t.Fatalf("original %d is still on the device after its drain", i)
		}
	}
	endFrame, _ := h.frame(drainFrameCnt + 1)
	lateDeleted := endFrame.State == model.StateDeleted
	wantEntries, wantDeletes := 1, drainFrameCnt
	if lateDeleted {
		wantEntries, wantDeletes = 0, drainFrameCnt+1
	}
	if names[drainFrameName(drainFrameCnt+1)] == lateDeleted {
		t.Fatalf("mid-drain frame on the device = %v with its row %q", !lateDeleted, endFrame.State)
	}
	if len(entries) != wantEntries {
		t.Fatalf("device directory holds %d entries, want %d: %v", len(entries), wantEntries, names)
	}

	// Every deleted frame was deleted exactly once: State == Deleted alone cannot
	// tell a single device delete from a second, redundant one, so this reads the
	// worker's own frames_deleted_total{manner="device_deleted"} counter,
	// incremented once per real Share.Delete that deviceDeleteFrame confirms
	// removed a row's source — the originals, and the mid-drain frame when it was
	// deleted in turn, are the only device_deleted frames this rig can produce.
	if got := h.counterValue("asiair_commander_frames_deleted_total",
		map[string]string{"rig": drainRigID, "manner": string(metrics.MannerDeviceDeleted)}); got != float64(wantDeletes) {
		t.Fatalf("frames_deleted_total{manner=device_deleted} = %v, want exactly %d", got, wantDeletes)
	}

	// Whether the drain actually sliced, and whether the mid-drain frame was filed
	// before the backlog emptied, depend on the Docker link's speed, so neither is
	// asserted; they are logged so a run shows whether slicing was exercised.
	yields := h.counterValue("asiair_commander_delete_pass_yield_total", map[string]string{"rig": drainRigID})
	var lastDeleted int64
	for i := 1; i <= drainFrameCnt; i++ {
		if f, ok := h.frame(i); ok {
			lastDeleted = max(lastDeleted, f.DeletedAt)
		}
	}
	newFrame, _ := h.frame(drainFrameCnt + 1)
	t.Logf("delete_pass_yield_total{rig=%q} = %v; mid-drain frame pulled_at=%d, last original deleted_at=%d (filed before the backlog emptied: %v, to the second)",
		drainRigID, yields, newFrame.PulledAt, lastDeleted, newFrame.PulledAt < lastDeleted)

	// Every NAS copy re-hashes to its recorded sha256, and the private archive
	// root holds nothing beyond the drainFrameCnt+1 expected files — aside from
	// the library-link marker the worker adopts once at the root, unrelated to
	// any frame this test stages.
	var nasFiles []string
	if err := filepath.WalkDir(h.light, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() != model.LibraryMarkerName {
			nasFiles = append(nasFiles, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk NAS root: %v", err)
	}
	if len(nasFiles) != drainFrameCnt+1 {
		t.Fatalf("NAS root holds %d files, want %d: %v", len(nasFiles), drainFrameCnt+1, nasFiles)
	}
	for i := 1; i <= drainFrameCnt+1; i++ {
		f, ok := h.frame(i)
		if !ok {
			t.Fatalf("frame %d missing from the store", i)
		}
		b, err := os.ReadFile(f.DestPath)
		if err != nil {
			t.Fatalf("read NAS copy for frame %d: %v", i, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			t.Fatalf("frame %d: NAS copy re-hashes to %s, recorded sha256 is %s", i, got, f.SHA256)
		}
	}
}
