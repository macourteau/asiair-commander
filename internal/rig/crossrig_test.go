package rig

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/store"
)

// TestCrossRigConcurrentFilingNoLoss is the design delete-safety §6 cross-rig
// concurrent-filing data-loss test: two rigs filing the SAME calibration name at
// the SAME instant, with DIFFERENT content, must never lose the loser.
//
// Both workers share ONE real (os-backed) localfs and ONE store, so the filing
// rename is a genuine atomic-exclusive rename (renamex_np RENAME_EXCL on darwin,
// renameat2 RENAME_NOREPLACE on linux, MoveFileEx-without-replace on windows) —
// NOT the fake. Each worker has its own fake device holding a DARK frame that
// routes to the identical cal directory and identical candidate filename but with
// distinct bytes (one byte past the END card flipped → distinct sha256, identical
// header → identical route). Their cycles run concurrently.
//
// The guarantee (design frame-lifecycle §8): the exclusive rename lets exactly
// one worker claim the canonical name; the loser observes the different-content
// clash, raises rot-clash, and files under a deterministic disambiguation token.
// Neither overwrites the other, so BOTH frames survive at rest with their own
// bytes.
//
// Run under -race: the two workers share store/localfs/hub/config, exactly as the
// manager wires them in production.
func TestCrossRigConcurrentFilingNoLoss(t *testing.T) {
	dir := t.TempDir()
	lfs := localfs.New() // the REAL os-backed impl, not the fake

	cfg := config.Defaults()
	cfg.StripTargetSpaces = true
	cfg.Defaults.MinFileAge = model.Duration(0)
	cfg.Defaults.SettleCycles = 0
	cfg.Roots = model.Roots{
		Light:   filepath.Join(dir, "light"),
		Cal:     filepath.Join(dir, "cal"),
		Staging: filepath.Join(dir, "staging"),
		Logs:    filepath.Join(dir, "logs"),
	}
	const share = "EMMC Images"
	const src = "Autorun/Dark/Dark_0001.fit" // same basename on both rigs → same candidate name
	cfg.Rigs = []model.RigProfile{
		{ID: "rig-01", Name: "A", Host: "a", OTA: "FRA300", Session: "S", Managed: true,
			Sources: []model.Source{{Share: share, Roots: []string{"Autorun"}}}},
		{ID: "rig-02", Name: "B", Host: "b", OTA: "FRA300", Session: "S", Managed: true,
			Sources: []model.Source{{Share: share, Roots: []string{"Autorun"}}}},
	}
	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hb := hub.New(logger)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }

	// Two DARK frames: identical header (identical route + identical canonical
	// name) but different bytes past the END card, so genuinely distinct sha256.
	base := fixture(t, "asiair_dark_6200mm.fit")
	contentA := append([]byte(nil), base...)
	contentB := append([]byte(nil), base...)
	contentB[len(contentB)-1] ^= 0xFF
	hashA, hashB := sha256hex(contentA), sha256hex(contentB)
	if hashA == hashB {
		t.Fatal("test setup: the two frames must differ in content")
	}
	mt := now.Add(-10 * time.Minute)

	devA := device.NewFake()
	devA.AddShare(share)
	devA.AddFile(share, src, contentA, mt)
	devB := device.NewFake()
	devB.AddShare(share)
	devB.AddFile(share, src, contentB, mt)

	ntfA, ntfB := &notify.Fake{}, &notify.Fake{}
	admA, admB := &fakeAdmission{}, &fakeAdmission{}

	mkWorker := func(id string, dev *device.Fake, ntf *notify.Fake, adm *fakeAdmission) *Worker {
		factory := func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev }
		// A healthy device answers the scan_air identity probe; the host-bound rigs
		// confirm on any reply.
		w := NewWorker(id, ptr, factory, &fakeResolver{probeOK: true}, netsite.NewRegistry(logger), lfs, header.New(), st, hb, ntf, adm, nil, nil,
			time.UTC, nowFn, nil, logger)
		w.lookupHost = noLookup
		return w
	}
	wA := mkWorker("rig-01", devA, ntfA, admA)
	wB := mkWorker("rig-02", devB, ntfB, admB)

	// Run both cycles concurrently: both race to file the same cal name.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); wA.runCycle(context.Background()) }()
	go func() { defer wg.Done(); wB.runCycle(context.Background()) }()
	wg.Wait()

	ctx := context.Background()
	fa, oka, err := st.GetFrame(ctx, "rig-01", share, src)
	if err != nil {
		t.Fatalf("GetFrame rig-01: %v", err)
	}
	fb, okb, err := st.GetFrame(ctx, "rig-02", share, src)
	if err != nil {
		t.Fatalf("GetFrame rig-02: %v", err)
	}
	if !oka || !okb {
		t.Fatalf("both frames must be recorded (rig-01=%v rig-02=%v)", oka, okb)
	}
	if fa.State != model.StateVerified || fb.State != model.StateVerified {
		t.Fatalf("both frames must be verified; rig-01=%s rig-02=%s", fa.State, fb.State)
	}
	if fa.DestPath == fb.DestPath {
		t.Fatalf("the loser overwrote the winner: both filed at %q", fa.DestPath)
	}

	// Both on-disk files must exist and carry the two DISTINCT hashes: no byte
	// loss, neither clobbered the other.
	gotA, err := os.ReadFile(fa.DestPath)
	if err != nil {
		t.Fatalf("read rig-01 dest %q: %v", fa.DestPath, err)
	}
	gotB, err := os.ReadFile(fb.DestPath)
	if err != nil {
		t.Fatalf("read rig-02 dest %q: %v", fb.DestPath, err)
	}
	onDisk := map[string]bool{sha256hex(gotA): true, sha256hex(gotB): true}
	if !onDisk[hashA] || !onDisk[hashB] {
		t.Fatalf("on-disk hashes %v do not cover both distinct frames {%s,%s}", onDisk, hashA[:12], hashB[:12])
	}
	// The recorded rows must agree with what is on disk (each row points at its
	// own bytes).
	if fa.SHA256 != sha256hex(gotA) || fb.SHA256 != sha256hex(gotB) {
		t.Fatalf("recorded sha does not match on-disk bytes (rig-01=%v rig-02=%v)",
			fa.SHA256 == sha256hex(gotA), fb.SHA256 == sha256hex(gotB))
	}

	// Exactly one worker — the loser of the exclusive-rename race — must have
	// raised the canonical-name content-clash (rot) alert.
	rotClashes := 0
	for _, ntf := range []*notify.Fake{ntfA, ntfB} {
		for _, a := range ntf.Raised() {
			if a.Category == model.AlertRotClash {
				rotClashes++
			}
		}
	}
	if rotClashes != 1 {
		t.Fatalf("expected exactly one rot-clash alert from the losing rig, got %d", rotClashes)
	}
}
