package manager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/rig"
	"github.com/macourteau/asiair-commander/internal/store"
)

// fakeResolver is a no-op rig.Resolver: the manager tests never exercise the
// registry hot path (the applier reads pending binds from the store).
type fakeResolver struct{}

func (fakeResolver) Resolve(string, string) (netsite.Endpoint, bool) {
	return netsite.Endpoint{}, false
}
func (fakeResolver) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	return "", "", false
}

// testRig builds a minimal rig profile.
//
// The host is an IP literal, and hermetically so: resolveAddress falls through the
// registry and the last-known-good IP to firstIPv4, which resolves a NAME through
// net.LookupHost — a real <id>.local query that costs ~5s per worker in Stop, on
// every required build-test run. internal/rig's own harness stubs the worker's
// lookupHost; a worker the manager constructs has no such seam from this package,
// and an address net.ParseIP answers never reaches a resolver at all. TEST-NET-1
// (RFC 5737) rather than a loopback, so a stray dial cannot find a local service.
func testRig(id string, managed bool) model.RigProfile {
	return model.RigProfile{ID: id, Name: id, Host: "192.0.2.1", Managed: managed}
}

// testConfig returns a config with the given rigs and cheap, positive cadences so
// worker tickers never fire during a short test.
func testConfig(t *testing.T, rigs ...model.RigProfile) *model.Config {
	t.Helper()
	return &model.Config{
		SchemaVersion: model.CurrentSchemaVersion,
		Roots:         model.Roots{Light: filepath.Join(t.TempDir(), "light-does-not-exist")},
		Defaults: model.RigDefaults{
			PollInterval:  model.Duration(10 * time.Second),
			RetryInterval: model.Duration(10 * time.Second),
		},
		Rigs: rigs,
	}
}

func newManagerForTest(t *testing.T, cfg *model.Config) *Manager {
	t.Helper()
	m, _ := newManagerWithSeams(t, cfg, localfs.NewFake(t.TempDir()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m
}

// newManagerWithSeams builds the manager over an injected filesystem and logger and
// hands back the store it was given, so a test can seed the state Start reads and
// watch what it does with it before the workers exist.
func newManagerWithSeams(t *testing.T, cfg *model.Config, lf localfs.LocalFS, logger *slog.Logger) (*Manager, store.Store) {
	t.Helper()
	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hb := hub.New(logger)
	nf := &notify.Fake{}
	// Unreachable device → each cycle is a cheap Probe=false, no real SMB work.
	dev := device.NewFake()
	dev.SetReachable(false)
	factory := func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev }
	adm := NewAdmission(3, lf, cfg.Roots.Light, 0)
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	return New(ptr, adm, factory, lf, header.New(), st, hb, nf, nil, nil, time.UTC, logger,
		fakeResolver{}, netsite.NewRegistry(logger), &config.Guard{}, configPath, nil), st
}

// workerIDs returns the sorted ids of currently supervised workers.
func (m *Manager) workerIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.handles))
	for id := range m.handles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStartLaunchesOnlyManagedWorkers(t *testing.T) {
	cfg := testConfig(t, testRig("a", true), testRig("b", true), testRig("c", false))
	m := newManagerForTest(t, cfg)

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if got := m.workerIDs(); !eq(got, []string{"a", "b"}) {
		t.Fatalf("workers = %v, want [a b]", got)
	}
}

func TestApplyConfigAddsAndRemovesWorkers(t *testing.T) {
	cfg := testConfig(t, testRig("a", true), testRig("b", false))
	m := newManagerForTest(t, cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if got := m.workerIDs(); !eq(got, []string{"a"}) {
		t.Fatalf("initial workers = %v, want [a]", got)
	}

	// Flip b→managed, a→unmanaged, add a new managed d, drop nothing else.
	m.ApplyConfig(testConfig(t, testRig("a", false), testRig("b", true), testRig("d", true)))
	if got := m.workerIDs(); !eq(got, []string{"b", "d"}) {
		t.Fatalf("after ApplyConfig workers = %v, want [b d]", got)
	}

	// Deleting a rig entirely also removes its worker.
	m.ApplyConfig(testConfig(t, testRig("b", true)))
	if got := m.workerIDs(); !eq(got, []string{"b"}) {
		t.Fatalf("after delete workers = %v, want [b]", got)
	}
}

func TestPerRigCommands(t *testing.T) {
	cfg := testConfig(t, testRig("a", true))
	m := newManagerForTest(t, cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	for _, tc := range []struct {
		name string
		fn   func(string) error
	}{
		{"Pause", m.Pause},
		{"Resume", m.Resume},
		{"RunNow", m.RunNow},
		{"Reclassify", m.Reclassify},
	} {
		if err := tc.fn("a"); err != nil {
			t.Errorf("%s(a): unexpected error %v", tc.name, err)
		}
		if err := tc.fn("nope"); !errors.Is(err, ErrRigNotFound) {
			t.Errorf("%s(nope): want ErrRigNotFound, got %v", tc.name, err)
		}
	}
}

// The manager is the discovery.RigWaker the composition root injects into the
// Scanner, so the signature is a contract with another package rather than a
// private helper. Unlike the operator commands it is silent about ids it does
// not supervise: every scan round carries devices bound to no worker.
var _ discovery.RigWaker = (*Manager)(nil)

func TestWakeRigAcceptsStrangersSilently(t *testing.T) {
	cfg := testConfig(t, testRig("a", true), testRig("b", false))
	m := newManagerForTest(t, cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	m.WakeRig("a")    // supervised: the worker decides what to do with it
	m.WakeRig("b")    // configured but unmanaged, so no worker
	m.WakeRig("nope") // never configured
	m.WakeRig("")     // a device bound to no profile at all
}

func TestStartTwiceErrors(t *testing.T) {
	cfg := testConfig(t, testRig("a", true))
	m := newManagerForTest(t, cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("second Start: want error, got nil")
	}
}

func TestStopIsIdempotentAndCancels(t *testing.T) {
	cfg := testConfig(t, testRig("a", true), testRig("b", true))
	m := newManagerForTest(t, cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Stop() // must return once all workers finish
	if got := m.workerIDs(); len(got) != 0 {
		t.Fatalf("after Stop workers = %v, want none", got)
	}
	m.Stop() // second Stop is a no-op
}

func TestConcurrentManagerMethodsNoRace(t *testing.T) {
	cfg := testConfig(t, testRig("a", true), testRig("b", true), testRig("c", true))
	m := newManagerForTest(t, cfg)
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	rigs := []string{"a", "b", "c", "d"}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := rigs[i%len(rigs)]
			switch i % 6 {
			case 0:
				_ = m.Pause(id)
			case 1:
				_ = m.Resume(id)
			case 2:
				_ = m.RunNow(id)
			case 3:
				_ = m.Reclassify(id)
			case 4:
				m.ApplyConfig(testConfig(t,
					testRig("a", true), testRig("b", i%2 == 0), testRig("c", true), testRig("d", i%3 == 0)))
			case 5:
				m.Reconcile()
			}
		}(i)
	}
	wg.Wait()
}

func TestPauseStatePersistsAcrossRestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// First run: a global pause plus one individually-paused rig, persisted.
	m1 := &Manager{store: st, log: logger, baseCtx: context.Background(), pausedRigs: map[string]bool{"rig-2": true}}
	m1.globalPaused = true
	m1.persistPauseState()

	// Simulated restart: a fresh manager over the same store restores the state.
	m2 := &Manager{store: st, log: logger}
	m2.loadPauseStateLocked(context.Background())
	if !m2.globalPaused {
		t.Fatal("global pause not restored across restart")
	}
	if !m2.pausedRigs["rig-2"] {
		t.Fatal("per-rig pause not restored across restart")
	}

	// Resuming everything clears it, and the cleared state also persists.
	m2.baseCtx = context.Background()
	m2.handles = map[string]*handle{}
	m2.ResumeAll()
	m3 := &Manager{store: st, log: logger}
	m3.loadPauseStateLocked(context.Background())
	if m3.globalPaused || len(m3.pausedRigs) != 0 {
		t.Fatalf("resume-all should clear persisted pause; got global=%v rigs=%v", m3.globalPaused, m3.pausedRigs)
	}
}

// migratableRig is a fully valid managed rig for the applier tests: it watches a
// share and folder (validateSources), keeps originals (so grace is not required),
// and is host-bound (no GUID yet), the exact shape the migration applier binds.
func migratableRig(id, host string) model.RigProfile {
	return model.RigProfile{
		ID: id, Name: id, Host: host, Managed: true, KeepOriginals: true,
		OTA: "RC8", Session: "S",
		Sources: []model.Source{{Share: "EMMC Images", Roots: []string{"Autorun"}}},
	}
}

// applierConfig is a fully valid config (from Defaults) carrying the given rigs —
// the applier validates the whole config with a nil resolver, so it must pass
// every non-volume rule, not just the binding ones.
func applierConfig(rigs ...model.RigProfile) *model.Config {
	c := config.Defaults()
	c.Rigs = rigs
	return c
}

func TestApplyPendingBindsBindsAndClears(t *testing.T) {
	cfg := applierConfig(migratableRig("rig-01", "asiair.local"))
	m := newManagerForTest(t, cfg)
	ctx := context.Background()

	if err := m.store.SetMeta(ctx, rig.PendingGUIDKey("rig-01"), "guid-abc,cpu-xyz"); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	m.applyPendingBinds(ctx)

	got := m.cfg.Load().Rig("rig-01")
	if got.GUID != "guid-abc" || got.CPUID != "cpu-xyz" || got.Host != "" {
		t.Fatalf("bind = %+v, want GUID=guid-abc CPUID=cpu-xyz Host cleared", *got)
	}
	if v, ok, _ := m.store.GetMeta(ctx, rig.PendingGUIDKey("rig-01")); ok && v != "" {
		t.Fatalf("pending key not cleared: %q", v)
	}
}

func TestApplyPendingBindsSkipsGUIDCollision(t *testing.T) {
	a := migratableRig("rig-a", "a.local")
	a.GUID = "shared-guid"
	a.Host = ""
	b := migratableRig("rig-b", "b.local") // still host-bound
	cfg := applierConfig(a, b)
	m := newManagerForTest(t, cfg)
	ctx := context.Background()

	// rig-b's pending bind names the guid rig-a already holds: skip + clear + alert,
	// and rig-b stays host-bound.
	if err := m.store.SetMeta(ctx, rig.PendingGUIDKey("rig-b"), "shared-guid,cpu-b"); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	m.applyPendingBinds(ctx)

	gotB := m.cfg.Load().Rig("rig-b")
	if gotB.GUID != "" {
		t.Fatalf("rig-b bound to a taken guid: %+v", *gotB)
	}
	if v, ok, _ := m.store.GetMeta(ctx, rig.PendingGUIDKey("rig-b")); ok && v != "" {
		t.Fatalf("collision pending key not cleared: %q", v)
	}
	if len(m.notify.(*notify.Fake).Raised()) == 0 {
		t.Fatal("a guid collision should raise an alert")
	}
}

func TestApplyPendingBindsClearsStaleKeyOnBoundRig(t *testing.T) {
	r := migratableRig("rig-01", "asiair.local")
	r.GUID = "already-bound"
	r.Host = ""
	cfg := testConfig(t, r)
	m := newManagerForTest(t, cfg)
	ctx := context.Background()

	// A late worker write after the bind: the applier just clears it.
	if err := m.store.SetMeta(ctx, rig.PendingGUIDKey("rig-01"), "already-bound,cpu"); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	m.applyPendingBinds(ctx)

	if v, ok, _ := m.store.GetMeta(ctx, rig.PendingGUIDKey("rig-01")); ok && v != "" {
		t.Fatalf("stale key on bound rig not cleared: %q", v)
	}
}

// TestStartSeedsUnackedAlertCount: the hub's unacked-alert count is in-memory and
// is only updated when an alert is raised or acked, so without seeding it at
// startup the nav badge reads zero after every restart while unacked alerts sit in
// the database — under-reporting until the next raise happens to refresh it.
func TestStartSeedsUnackedAlertCount(t *testing.T) {
	cfg := testConfig(t, testRig("rig-01", true))
	m := newManagerForTest(t, cfg)
	ctx := context.Background()

	// An alert left unacked by a previous process.
	if _, err := m.store.RaiseAlert(ctx, model.Alert{
		RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertDeviceDiskLow,
		Message: "device free space is low", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}

	sh, ok := m.hub.(*hub.StatusHub)
	if !ok {
		t.Fatalf("hub is %T, want *hub.StatusHub", m.hub)
	}
	if got := sh.Snapshot().UnackedAlerts; got != 0 {
		t.Fatalf("precondition: badge should start at 0, got %d", got)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Stop)

	if got := sh.Snapshot().UnackedAlerts; got != 1 {
		t.Fatalf("unacked badge = %d after Start, want 1 — a restart must not zero the badge", got)
	}
}

// TestDeepCopyConfigKeepsRemotes: the pending-guid applier saves its copy of
// the live config straight to disk, so the copy must carry the remotes and
// their pairing secrets; losing them would unpair every remote on a bind.
func TestDeepCopyConfigKeepsRemotes(t *testing.T) {
	cfg := &model.Config{
		Remotes:   []model.RemoteProfile{{ID: "r1", Name: "Obs", Address: "tc-address", ClientKey: "privkey:x", Region: "fra", RateLimitMBps: 1.5, CreatedAt: 7}},
		RemoteSeq: 4,
	}
	cp, err := deepCopyConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cp.Remotes, cfg.Remotes) || cp.RemoteSeq != 4 {
		t.Fatalf("copy lost the remotes: %+v seq %d", cp.Remotes, cp.RemoteSeq)
	}
}
