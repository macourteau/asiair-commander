package rig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/clock"
	"github.com/macourteau/asiair-commander/internal/config"
	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
	dto "github.com/prometheus/client_model/go"
)

// --- test scaffolding ----------------------------------------------------------

// fakeAdmission is a no-op transfer gate that counts acquire/release calls to
// prove the terminal-outcome release contract. acqErr, when set, fails Acquire so
// a test can model the disk-reserve/admission gate deferring a transfer.
type fakeAdmission struct {
	mu       sync.Mutex
	acquired int
	released int
	acqErr   error
	// reqs records each Acquire's requester, in order.
	reqs []Requester
}

func (a *fakeAdmission) Acquire(_ context.Context, req Requester, _ int64) (func(), error) {
	a.mu.Lock()
	a.acquired++
	a.reqs = append(a.reqs, req)
	err := a.acqErr
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return func() {
		a.mu.Lock()
		a.released++
		a.mu.Unlock()
	}, nil
}

func (a *fakeAdmission) requesters() []Requester {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Requester(nil), a.reqs...)
}

func (a *fakeAdmission) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acquired, a.released
}

// panicReader is a header.Reader that panics, to exercise the per-file recover.
type panicReader struct{}

func (panicReader) ReadHeader(string) (model.Header, error) { panic("boom: malformed card") }

// fakeResolver is a scriptable rig.Resolver for the worker tests. Resolve hands
// back a fixed address/online; ProbeGUID replays a scripted live-probe reply.
type fakeResolver struct {
	mu            sync.Mutex
	resolveAddr   string
	resolveOnline bool
	probeGUID     string
	probeCPU      string
	probeOK       bool
	probeCalls    int
}

func (r *fakeResolver) Resolve(string, string) (netsite.Endpoint, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return netsite.LocalEndpoint(r.resolveAddr), r.resolveOnline
}

func (r *fakeResolver) ProbeGUID(context.Context, netsite.Endpoint) (string, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probeCalls++
	return r.probeGUID, r.probeCPU, r.probeOK
}

func (r *fakeResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probeCalls
}

// noLookup is a hermetic lookupHost that resolves nothing, so a legacy Host
// falls back to its literal value without a real DNS query in tests.
func noLookup(string) ([]string, error) { return nil, errNoTestHost }

var errNoTestHost = errNoHost{}

type errNoHost struct{}

func (errNoHost) Error() string { return "no such host (test)" }

type harness struct {
	t        *testing.T
	dir      string
	cfg      *model.Config
	cfgPtr   *atomic.Pointer[model.Config]
	dev      *device.Fake
	lfs      *localfs.Fake
	store    store.Store
	hub      *hub.StatusHub
	notify   *notify.Fake
	adm      *fakeAdmission
	resolver *fakeResolver
	nowVal   time.Time
	w        *Worker
	profile  *model.RigProfile
	share    string
	logBuf   *lockedBuffer
	metrics  *metrics.Metrics
}

// lockedBuffer is a concurrency-safe log sink for tests that assert on emitted
// log lines; the worker and hub may write from more than one goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, share string, hdr header.Reader) *harness {
	t.Helper()
	dir := t.TempDir()

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
	cfg.Rigs = []model.RigProfile{{
		ID: "rig-01", Name: "Test", Host: "h", OTA: "FRA300", Session: "S",
		Managed: true, CapacityConstrained: true,
		Sources: []model.Source{{Share: share, Roots: []string{"Autorun"}}},
	}}

	ptr := &atomic.Pointer[model.Config]{}
	ptr.Store(cfg)

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	lf := localfs.NewFake(dir)
	logBuf := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hb := hub.New(logger)
	nf := &notify.Fake{}
	dev := device.NewFake()
	dev.AddShare(share)
	adm := &fakeAdmission{}

	// A healthy device answers the scan_air identity probe; the default host-bound
	// profile confirms on any reply (the probed guid is bound by the manager, not
	// asserted here). Tests that model a no-reply/foreign device override probeOK.
	res := &fakeResolver{probeOK: true}
	mx := metrics.New("test")
	h := &harness{
		t: t, dir: dir, cfg: cfg, cfgPtr: ptr, dev: dev, lfs: lf,
		store: st, hub: hb, notify: nf, adm: adm, resolver: res, share: share,
		nowVal: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		logBuf: logBuf, metrics: mx,
	}
	factory := func(netsite.Endpoint, string, string, time.Duration) device.Device { return dev }
	h.w = NewWorker("rig-01", ptr, factory, res, netsite.NewRegistry(logger), lf, hdr, st, hb, nf, adm, nil, nil,
		time.UTC, func() time.Time { return h.nowVal }, mx, logger)
	h.w.lookupHost = noLookup
	h.profile = cfg.Rig("rig-01")
	return h
}

func (h *harness) cycle() { h.w.runCycle(context.Background()) }

// localTarget is the hash target of a delete slice on the local site, for a
// test that runs one frame's delete outside a slice: the local site measures
// with the request's own read, whatever address and login it names.
func (h *harness) localTarget() hashTarget { return hashTarget{site: h.w.sites.Site(netsite.Local)} }

func (h *harness) status() model.RigStatus {
	snap := h.hub.Snapshot()
	if len(snap.Rigs) == 0 {
		return model.RigStatus{}
	}
	return snap.Rigs[0]
}

func (h *harness) getFrame(path string) (*model.Frame, bool) {
	f, ok, err := h.store.GetFrame(context.Background(), "rig-01", h.share, path)
	if err != nil {
		h.t.Fatalf("GetFrame: %v", err)
	}
	return f, ok
}

func (h *harness) deviceHas(path string) bool {
	sess, err := h.dev.Open(context.Background())
	if err != nil {
		return false
	}
	defer sess.Close()
	sh, err := sess.Mount(h.share)
	if err != nil {
		return false
	}
	_, err = sh.Stat(path)
	return err == nil
}

func (h *harness) raisedCategories() []string {
	var out []string
	for _, a := range h.notify.Raised() {
		out = append(out, string(a.Category))
	}
	return out
}

// counterValue sums the value of every series of counter `name` whose labels are a
// superset of the requested ones, read from the harness's real metrics registry.
func (h *harness) counterValue(name string, labels map[string]string) float64 {
	h.t.Helper()
	fams, err := h.metrics.Registry().Gather()
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

func (h *harness) hasAlert(cat model.Category) bool {
	for _, a := range h.notify.Raised() {
		if a.Category == cat {
			return true
		}
	}
	return false
}

// alertSeverity returns the severity of the last raised alert of a category, or
// empty if none was raised.
func (h *harness) alertSeverity(cat model.Category) model.Severity {
	var sev model.Severity
	for _, a := range h.notify.Raised() {
		if a.Category == cat {
			sev = a.Severity
		}
	}
	return sev
}

// logContains reports whether the captured log output holds sub, so a test can
// assert a WARN was or was not emitted.
func (h *harness) logContains(sub string) bool {
	return strings.Contains(h.logBuf.String(), sub)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "header", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeLocal(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func stagingEmpty(t *testing.T, dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "staging"))
	if err != nil {
		return true // not created
	}
	return len(entries) == 0
}

const emmc = "EMMC Images"
const tf = "TF Images"

// TestRemovableSlotClassification pins the split predicates (design
// device-identity-and-media §7.1): isRemovableSlot governs empty-slot handling for
// the verdict/capacity (USB and microSD both qualify, eMMC never, and an incidental
// "tf" substring must not false-match), while isRemovableShare — the delete-pass
// media-identity marker — stays Udisk-only.
func TestRemovableSlotClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"TF Images", true},
		{"tf", true},
		{"Udisk Images", true},
		{"EMMC Images", false},
		{"catfish", false},
		{"outfit", false},
	} {
		if got := isRemovableSlot(tc.name); got != tc.want {
			t.Errorf("isRemovableSlot(%q)=%v, want %v", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"Udisk Images", true},
		{"TF Images", false},
		{"EMMC Images", false},
	} {
		if got := isRemovableShare(tc.name); got != tc.want {
			t.Errorf("isRemovableShare(%q)=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- settle tracker ------------------------------------------------------------

func TestSettleTracker(t *testing.T) {
	s := newSettleTracker(2 * time.Second)
	mt := clock.FromOrigin(time.Unix(1000, 0))
	key := "share\x00path"

	s.beginCycle()
	if got := s.observe(key, 100, mt); got != 0 {
		t.Fatalf("first sighting stable=%d, want 0", got)
	}
	s.endCycle()

	s.beginCycle()
	if got := s.observe(key, 100, mt); got != 1 {
		t.Fatalf("second unchanged sighting stable=%d, want 1", got)
	}
	s.endCycle()

	s.beginCycle()
	if got := s.observe(key, 100, mt); got != 2 {
		t.Fatalf("third unchanged sighting stable=%d, want 2", got)
	}
	s.endCycle()

	// A size change resets.
	s.beginCycle()
	if got := s.observe(key, 200, mt); got != 0 {
		t.Fatalf("changed sighting stable=%d, want 0 (reset)", got)
	}
	s.endCycle()

	// A gone key is evicted.
	s.beginCycle()
	s.endCycle()
	if _, ok := s.obs[key]; ok {
		t.Fatalf("unobserved key not evicted")
	}
}

// --- decide --------------------------------------------------------------------

func TestDecideNoRowPulls(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	lf := listedFrame{share: emmc, path: "Autorun/x.fit", name: "x.fit", size: 10, mtime: h.nowVal}
	pull, prior := h.w.decide(context.Background(), h.cfg, h.profile, lf)
	if !pull || prior != nil {
		t.Fatalf("no-row decide: pull=%v prior=%v, want pull with nil prior", pull, prior)
	}
}

func TestDecideMatchVerifiedSkips(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	mt := h.nowVal
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		SHA256: "abc", Size: 10, Mtime: mt.Unix(), State: model.StateVerified, PulledAt: mt.Unix()}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	lf := listedFrame{share: emmc, path: "Autorun/x.fit", name: "x.fit", size: 10, mtime: mt}
	if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); pull {
		t.Fatalf("match+verified should skip")
	}
}

func TestDecideFailedRetryBackoff(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	mt := h.nowVal
	base := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		Size: 10, Mtime: mt.Unix(), State: model.StateFailed, Attempts: 1}
	lf := listedFrame{share: emmc, path: "Autorun/x.fit", name: "x.fit", size: 10, mtime: mt}

	// Not due: next_retry_at in the future.
	base.NextRetryAt = mt.Add(time.Hour).Unix()
	base.ID = 0
	if err := h.store.UpsertFrame(ctx, base); err != nil {
		t.Fatal(err)
	}
	if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); pull {
		t.Fatalf("failed+not-due should skip")
	}

	// Due: next_retry_at in the past.
	got, _ := h.getFrame("Autorun/x.fit")
	got.NextRetryAt = mt.Add(-time.Minute).Unix()
	if err := h.store.UpdateFrame(ctx, got); err != nil {
		t.Fatal(err)
	}
	if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); !pull {
		t.Fatalf("failed+due should retry (pull)")
	}
}

func TestDecideDifferBenignThenPull(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	nasContent := []byte("the-old-good-frame")
	nasPath := filepath.Join(h.dir, "light", "old", "frame.fit")
	writeLocal(t, nasPath, nasContent)

	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		SHA256: sha256hex(nasContent), Size: int64(len(nasContent)), Mtime: h.nowVal.Unix(),
		ArchiveSHA256: sha256hex(nasContent), ArchiveSize: int64(len(nasContent)),
		DestPath: nasPath, State: model.StateVerified, PulledAt: h.nowVal.Unix()}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	// New content differs in size → differ branch; old NAS copy is intact → benign.
	lf := listedFrame{share: emmc, path: "Autorun/x.fit", name: "x.fit", size: 999, mtime: h.nowVal}
	pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf)
	if !pull {
		t.Fatalf("differ should pull the new content")
	}
	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("benign overwrite must NOT alert frame-lost; got %v", h.raisedCategories())
	}
}

func TestDecideDifferMissingNASAlertsLost(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		SHA256: "deadbeef", Size: 100, Mtime: h.nowVal.Unix(),
		DestPath: filepath.Join(h.dir, "light", "gone", "frame.fit"), // does not exist
		State:    model.StateVerified, PulledAt: h.nowVal.Unix()}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}
	lf := listedFrame{share: emmc, path: "Autorun/x.fit", name: "x.fit", size: 200, mtime: h.nowVal}
	pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf)
	if !pull {
		t.Fatalf("differ should still pull new content")
	}
	if !h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("missing NAS copy on differ must alert frame-lost; got %v", h.raisedCategories())
	}
}

// --- quiescence ----------------------------------------------------------------

func TestQuiescence(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.MinFileAge = model.Duration(120 * time.Second)
	h.cfg.Defaults.SettleCycles = 1
	cl := h.w.perConnectClock(clock.ServerTime{})

	// Still-writing: young file (mtime == now) fails min_file_age.
	young := listedFrame{share: emmc, path: "Autorun/w.fit", name: "w.fit", size: 100, mtime: h.nowVal}
	h.w.settle.beginCycle()
	if h.w.quiescent(h.cfg, h.profile, young, cl) {
		t.Fatalf("young file should not be quiescent")
	}
	h.w.settle.endCycle()

	// Old + stable: mtime 200s in the past, unchanged across two cycles.
	old := listedFrame{share: emmc, path: "Autorun/o.fit", name: "o.fit", size: 100, mtime: h.nowVal.Add(-200 * time.Second)}
	h.w.settle.beginCycle()
	if h.w.quiescent(h.cfg, h.profile, old, cl) {
		t.Fatalf("first observation should not yet be settled (settle_cycles=1)")
	}
	h.w.settle.endCycle()
	h.w.settle.beginCycle()
	if !h.w.quiescent(h.cfg, h.profile, old, cl) {
		t.Fatalf("old + stable file should be quiescent on the second cycle")
	}
	h.w.settle.endCycle()

	// Still-writing old file: size changes each cycle → settle keeps resetting.
	h.w.settle.beginCycle()
	writing := listedFrame{share: emmc, path: "Autorun/g.fit", name: "g.fit", size: 100, mtime: h.nowVal.Add(-200 * time.Second)}
	_ = h.w.quiescent(h.cfg, h.profile, writing, cl)
	h.w.settle.endCycle()
	h.w.settle.beginCycle()
	writing.size = 150 // grew
	if h.w.quiescent(h.cfg, h.profile, writing, cl) {
		t.Fatalf("a growing file must never be quiescent")
	}
	h.w.settle.endCycle()
}

// TestShippedMinFileAgeHoldsAFrameForFiveMinutes (design frame-lifecycle AC22)
// pins the shipped min_file_age default to the gate it feeds. The value is the
// whole of the third piece of design frame-lifecycle §11 — a pull that lands
// after the device has finished revising the frame carries the WCS and needs no
// supersede at all — so
// a default silently rolled back to the old 120s would leave the rest of H13 in
// place and quietly restore the race it exists to narrow. The harness zeroes
// min_file_age for every other test, so this one puts the shipped value back
// rather than naming a number of its own, and leaves settle_cycles at the
// harness's zero so the only gate under test is the age one.
func TestShippedMinFileAgeHoldsAFrameForFiveMinutes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	shipped := config.Defaults().Defaults.MinFileAge
	if shipped.D() != 300*time.Second {
		t.Fatalf("the shipped min_file_age default is %s, want 300s", shipped.D())
	}
	h.cfg.Defaults.MinFileAge = shipped

	// A device clock running 90s ahead of the host's, so the age the gate applies is
	// the CORRECTED one: a listed mtime read literally would be 90s out and would
	// answer the wrong side of the boundary either way.
	const skew = 90 * time.Second
	cl := h.w.perConnectClock(clock.ServerTime{Time: h.nowVal.Add(skew), ReceivedAt: h.nowVal})
	deviceMtime := func(age time.Duration) time.Time { return h.nowVal.Add(skew).Add(-age) }

	lf := listedFrame{share: emmc, path: "Autorun/q.fit", name: "q.fit", size: 100, mtime: deviceMtime(299 * time.Second)}
	h.w.settle.beginCycle()
	if h.w.quiescent(h.cfg, h.profile, lf, cl) {
		t.Fatal("a frame 299s old is inside the shipped 300s window and is not a pull candidate")
	}
	h.w.settle.endCycle()

	// The same frame, two seconds later: nothing about it changed but its age.
	h.nowVal = h.nowVal.Add(2 * time.Second)
	h.w.settle.beginCycle()
	if !h.w.quiescent(h.cfg, h.profile, lf, cl) {
		t.Fatal("the same frame past 300s is a pull candidate")
	}
	h.w.settle.endCycle()
}

// TestShippedMinFileAgeDefersARealPull (design frame-lifecycle AC22) is the same
// boundary through a whole cycle rather than the gate alone: at the shipped default
// a frame inside the window is listed and left alone, and the very same file is
// pulled once it ages out.
func TestShippedMinFileAgeDefersARealPull(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.MinFileAge = config.Defaults().Defaults.MinFileAge
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-299*time.Second))

	h.cycle()
	if _, ok := h.getFrame(src); ok {
		t.Fatal("a frame inside the shipped min_file_age window must not be pulled")
	}
	if st := h.status(); st.WaitingToSettle != 1 {
		t.Fatalf("waiting_to_settle=%d, want the deferred frame counted", st.WaitingToSettle)
	}

	h.nowVal = h.nowVal.Add(2 * time.Second)
	h.cycle()
	if _, ok := h.getFrame(src); !ok {
		t.Fatal("the same frame must be pulled once it is older than the shipped min_file_age")
	}
}

// --- pull + verify -------------------------------------------------------------

func TestPullHappyLightFiles(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("frame not recorded")
	}
	if f.State != model.StateVerified {
		t.Fatalf("state=%s, want verified", f.State)
	}
	if f.SHA256 != sha256hex(data) {
		t.Fatalf("recorded sha mismatch")
	}
	if f.Night != 1 {
		t.Fatalf("night=%d, want 1", f.Night)
	}
	if !strings.Contains(f.DestPath, "C-2025A6") || !strings.Contains(f.DestPath, "NIGHT_1") {
		t.Fatalf("dest=%q missing target/night", f.DestPath)
	}
	if _, err := os.Stat(f.DestPath); err != nil {
		t.Fatalf("dest file not on disk: %v", err)
	}
	if acq, rel := h.adm.counts(); acq != 1 || rel != 1 {
		t.Fatalf("admission acquired=%d released=%d, want 1/1", acq, rel)
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatalf("staging temp leaked")
	}
}

// TestPullRecordsMetrics proves the pipeline instrumentation actually fires on a
// real successful pull (not just that it compiles): the filed outcome, the
// fresh-disposition filed counter, and the streamed byte total.
func TestPullRecordsMetrics(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	if got := h.counterValue("asiair_commander_transfer_outcomes_total",
		map[string]string{"rig": "rig-01", "outcome": "filed"}); got != 1 {
		t.Errorf("transfer_outcomes_total{filed} = %v, want 1", got)
	}
	if got := h.counterValue("asiair_commander_frames_filed_total",
		map[string]string{"rig": "rig-01", "class": "light", "disposition": "fresh"}); got != 1 {
		t.Errorf("frames_filed_total{light,fresh} = %v, want 1", got)
	}
	if got := h.counterValue("asiair_commander_transfer_bytes_total",
		map[string]string{"rig": "rig-01"}); got != float64(len(data)) {
		t.Errorf("transfer_bytes_total = %v, want %d", got, len(data))
	}
}

// TestStatusStampsNextPoll proves an idle (Monitoring) rig publishes the
// unix-seconds time of its next scheduled cycle, so the dashboard can render a
// live "next check in Xs" countdown. It must equal now + the poll interval — the
// online path always reschedules a full poll out.
func TestStatusStampsNextPoll(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	h.dev.AddFile(emmc, "Autorun/Light/C-2025 A6/Light_NGC_0001.fit", data, h.nowVal.Add(-10*time.Minute))

	// First cycle pulls the frame (state reads "pulling"); a second finds it
	// already verified with nothing pending, so the rig settles to idle.
	h.cycle()
	h.cycle()

	st := h.status()
	if st.State != model.RigStateIdle {
		t.Fatalf("state=%q, want idle after a clean cycle", st.State)
	}
	want := h.nowVal.Add(safeInterval(h.cfg.EffectivePollInterval(h.profile))).Unix()
	if st.NextPollAt != want {
		t.Fatalf("NextPollAt=%d, want %d (now + poll interval)", st.NextPollAt, want)
	}
}

// TestPullUdiskNestedUnderASIAIR is the raw-removable-Udisk regression: the ASIAIR
// writes capture folders under a top-level ASIAIR/ folder, so a configured
// "Autorun" root actually lives at ASIAIR/Autorun/... The listing must resolve that
// prefix, walk the nested tree, pair the preview, and pull the frame — exactly as
// the flat EMMC layout does in TestPullHappyLightFiles. A top-level
// ReadDir("Autorun") answers not-found there and would abort the whole share.
func TestPullUdiskNestedUnderASIAIR(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "ASIAIR/Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	preview := "ASIAIR/Autorun/Light/C-2025 A6/Light_NGC_0001_thn.jpg"
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(udisk, src, data, mt)
	h.dev.AddFile(udisk, preview, []byte("preview-bytes"), mt)

	h.cycle()

	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("nested Udisk frame not listed/recorded")
	}
	if f.State != model.StateVerified {
		t.Fatalf("state=%s, want verified", f.State)
	}
	if f.SHA256 != sha256hex(data) {
		t.Fatalf("recorded sha mismatch")
	}
	if len(f.Sidecars) != 1 {
		t.Fatalf("nested preview should pair as one sidecar, got %d", len(f.Sidecars))
	}
	if _, err := os.Stat(f.DestPath); err != nil {
		t.Fatalf("dest file not on disk: %v", err)
	}
}

// TestResolveDataPrefix pins the per-share prefix resolution directly: a flat EMMC
// layout resolves to no prefix, an ASIAIR/-nested Udisk resolves to "ASIAIR", a
// non-not-found error is a hard failure (not masked as an empty listing), and a
// root absent from both layouts falls back to no prefix (the walk then surfaces it).
func TestResolveDataPrefix(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	mt := h.nowVal
	h.dev.AddFile(emmc, "Autorun/Light/x.fit", []byte("a"), mt)
	h.dev.AddFile(udisk, "ASIAIR/Autorun/Light/y.fit", []byte("b"), mt)

	flat := h.mountShare(emmc)
	if got, err := h.w.resolveDataPrefix(flat, []string{"Autorun"}); err != nil || got != "" {
		t.Fatalf("flat EMMC prefix = %q, err %v; want \"\", nil", got, err)
	}

	nested := h.mountShare(udisk)
	if got, err := h.w.resolveDataPrefix(nested, []string{"Autorun"}); err != nil || got != "ASIAIR" {
		t.Fatalf("nested Udisk prefix = %q, err %v; want \"ASIAIR\", nil", got, err)
	}

	// A non-not-found error must propagate, not fall through to a prefix.
	h.dev.FailListDir(emmc, device.NetworkError("boom"))
	if _, err := h.w.resolveDataPrefix(flat, []string{"Autorun"}); err == nil {
		t.Fatalf("a non-not-found list error must be returned, got nil")
	}

	// A root present in neither layout falls back to the empty prefix.
	if got, err := h.w.resolveDataPrefix(nested, []string{"Nope"}); err != nil || got != "" {
		t.Fatalf("absent root prefix = %q, err %v; want \"\", nil", got, err)
	}
}

// TestPullRemovableAbsentMediumQuiet is the empty-USB-slot regression: the ASIAIR
// keeps exporting an empty removable Udisk share when nothing is inserted, so the
// configured root resolves nowhere and the listing walk not-founds. That must read
// as a missing stick — a stable Debug line, the medium marked absent — never the
// per-cycle "list failed" WARN that also forced a hard listing failure.
// TestPullRemovableAbsentMediumQuiet covers both removable slots (A2): an empty
// USB (Udisk) and an empty microSD (TF) are each treated as an absent removable
// medium — no list-failed WARN, listing not a hard failure, and safe suppressed to
// Unknown so H5 holds.
func TestPullRemovableAbsentMediumQuiet(t *testing.T) {
	for _, share := range []string{udisk, tf} {
		t.Run(share, func(t *testing.T) {
			h := newHarness(t, share, header.New())
			// newHarness already registered an empty removable share: it mounts and its
			// root lists, but holds no ASIAIR/ folder and no configured capture root.

			h.cycle()

			if h.logContains("list failed") {
				t.Fatalf("an empty removable slot must not emit a list-failed WARN; log:\n%s", h.logBuf.String())
			}
			if !h.logContains("no medium inserted") {
				t.Fatalf("an empty removable slot should log the absent-medium Debug line; log:\n%s", h.logBuf.String())
			}
			st := h.status()
			// listingOK stayed true (not a hard failure), so the successful-listing timestamp advanced.
			if st.ListingTS != h.nowVal.Unix() {
				t.Fatalf("ListingTS=%d, want %d — an absent medium must not be a hard listing failure", st.ListingTS, h.nowVal.Unix())
			}
			// Routed through the existing media-absent path: safe reads Unknown for an empty
			// slot (nothing to assert), never a hard error.
			if st.SafeToPowerOff != model.SafeUnknown {
				t.Fatalf("SafeToPowerOff=%q, want %q for an absent removable medium", st.SafeToPowerOff, model.SafeUnknown)
			}
		})
	}
}

// TestEmptyTFSlotTreatedAsAbsent (A1) pins that an empty microSD (TF) slot is
// handled exactly like an empty Udisk: excluded from the capacity math, raising no
// device-disk-low alert, and logged at Debug (not a per-cycle WARN). The absent
// error flows through the real listing cycle — the same device.IsNotFound path the
// Udisk slot takes — not a synthesized one.
func TestEmptyTFSlotTreatedAsAbsent(t *testing.T) {
	h := newHarness(t, tf, header.New())
	// An empty slot: the share mounts and its root lists, but holds no ASIAIR/ and
	// no capture root. Its statfs reports the ASIAIR's own tiny rootfs (well under
	// the reserve), so without the exclusion it would publish a phantom figure and
	// raise a false device-disk-low.
	h.dev.SetStatfs(tf, 964<<20, 7159<<20)

	h.cycle()

	if h.logContains("list failed") {
		t.Fatalf("an empty TF slot must not emit a list-failed WARN; log:\n%s", h.logBuf.String())
	}
	if !h.logContains("no medium inserted") {
		t.Fatalf("an empty TF slot should log the absent-medium Debug line; log:\n%s", h.logBuf.String())
	}
	st := h.status()
	if st.ListingTS != h.nowVal.Unix() {
		t.Fatalf("ListingTS=%d, want %d — an absent TF medium must not be a hard listing failure", st.ListingTS, h.nowVal.Unix())
	}
	for _, s := range st.Shares {
		if s.Share == tf {
			t.Fatalf("an empty TF slot must be excluded from capacity; got %d bytes for %q", s.Free, s.Share)
		}
	}
	if h.hasAlert(model.AlertDeviceDiskLow) {
		t.Fatalf("an empty TF slot must not raise device-disk-low; got %v", h.raisedCategories())
	}
}

// TestPopulatedTFShareUnchanged (A3) confirms the removable-slot split does not
// disturb a populated TF share: its capture roots sit at the share root (TF is
// fixed-layout, not nested under ASIAIR/ like a raw Udisk), and it lists, pairs, and
// pulls exactly as before.
func TestPopulatedTFShareUnchanged(t *testing.T) {
	h := newHarness(t, tf, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(tf, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	if h.logContains("no medium inserted") {
		t.Fatalf("a populated TF share must not be read as absent; log:\n%s", h.logBuf.String())
	}
	if h.logContains("list failed") {
		t.Fatalf("a populated TF share must list cleanly; log:\n%s", h.logBuf.String())
	}
	f, ok := h.getFrame(src)
	if !ok || f.State != model.StateVerified {
		t.Fatalf("TF frame not listed/pulled: ok=%v frame=%+v", ok, f)
	}
}

// TestTFDeleteGateUnchanged (A4) pins that the removable-slot split does NOT
// extend the delete-pass media-identity gate to TF: isRemovableShare stays
// Udisk-only, so a TF share is fixed media for deletion and authorizes cleanup
// without any media marker.
func TestTFDeleteGateUnchanged(t *testing.T) {
	h := newHarness(t, tf, header.New())
	share := h.mountShare(tf)
	if !h.w.mediaConfirmedForDelete(share) {
		t.Fatalf("a TF device deletion must not be newly gated on a media marker")
	}
	if isRemovableShare(tf) {
		t.Fatalf("isRemovableShare must stay Udisk-only; TF must not become a marker-gated share")
	}
}

// TestPullRemovableWithDataListsNormally confirms the absent-medium shortcut does
// not swallow a real listing: a removable Udisk with capture data under ASIAIR/
// still lists, pairs, and pulls, emitting neither the absent-medium Debug line nor
// a list-failed WARN.
func TestPullRemovableWithDataListsNormally(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "ASIAIR/Autorun/Light/C-2025 A6/Light_NGC_0001.fit"
	h.dev.AddFile(udisk, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	if h.logContains("no medium inserted") {
		t.Fatalf("a populated removable share must not be read as absent; log:\n%s", h.logBuf.String())
	}
	if h.logContains("list failed") {
		t.Fatalf("a populated removable share must list cleanly; log:\n%s", h.logBuf.String())
	}
	f, ok := h.getFrame(src)
	if !ok || f.State != model.StateVerified {
		t.Fatalf("nested Udisk frame not listed/pulled: ok=%v frame=%+v", ok, f)
	}
}

// TestPullFixedShareMissingRootWarns pins that the absent-medium shortcut is
// removable-only: a fixed EMMC/TF share whose configured root is genuinely missing
// is a real misconfiguration and must still surface the list-failed WARN and count
// as a hard listing failure (no advanced listing timestamp).
func TestPullFixedShareMissingRootWarns(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	// emmc is a fixed share registered with no Autorun root — a genuine misconfig,
	// not a removable empty slot.

	h.cycle()

	if !h.logContains("list failed") {
		t.Fatalf("a fixed share with a missing root must still WARN; log:\n%s", h.logBuf.String())
	}
	if h.logContains("no medium inserted") {
		t.Fatalf("a fixed share must never be read as an absent removable medium; log:\n%s", h.logBuf.String())
	}
	if st := h.status(); st.ListingTS != 0 {
		t.Fatalf("ListingTS=%d, want 0 — a missing root on a fixed share is a hard listing failure", st.ListingTS)
	}
}

// TestPullRemovableTransientErrorWarns pins that a transient (non-not-found) list
// error on a removable share is never mistaken for an absent medium: it must still
// WARN and count as a hard listing failure, so a real network/permission fault is
// not silently hidden.
func TestPullRemovableTransientErrorWarns(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	h.dev.FailListDir(udisk, device.NetworkError("boom"))

	h.cycle()

	if !h.logContains("list failed") {
		t.Fatalf("a transient list error on a removable share must still WARN; log:\n%s", h.logBuf.String())
	}
	if h.logContains("no medium inserted") {
		t.Fatalf("a transient error must not be read as an absent medium; log:\n%s", h.logBuf.String())
	}
	if st := h.status(); st.ListingTS != 0 {
		t.Fatalf("ListingTS=%d, want 0 — a transient list error is a hard listing failure", st.ListingTS)
	}
}

// joinLocal must compose a single-separator host-native path: the router's
// "/"-joined relative dir is rewritten to the OS separator so a filed dest_path
// never mixes "\" and "/" (the reported Windows bug). We assert it equals the
// host-native filepath.Join and that, with a filename appended the way filing
// does, the whole path is free of the foreign separator on this platform.
func TestJoinLocalHostNative(t *testing.T) {
	base := filepath.Join("nas", "Lights")
	got := joinLocal(base, "M31/OTA/NIGHT_1")
	if want := filepath.Join(base, "M31", "OTA", "NIGHT_1"); got != want {
		t.Fatalf("joinLocal = %q, want %q", got, want)
	}
	full := filepath.Join(got, "frame.fit")
	foreign := byte('/')
	if filepath.Separator == '/' {
		foreign = '\\'
	}
	if strings.IndexByte(full, foreign) >= 0 {
		t.Errorf("path %q mixes separators (contains %q)", full, string(foreign))
	}
}

func TestPullSizeMismatchDiscardsTemp(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(emmc, src, data, mt)

	sess, _ := h.dev.Open(ctx)
	share, _ := sess.Mount(emmc)
	// A listedFrame whose size lies (larger than the real bytes) → the streamed
	// byte-count check fails and the temp is discarded, nothing filed.
	lf := listedFrame{share: emmc, path: src, name: "Light_0001.fit", size: int64(len(data)) + 100, mtime: mt}
	if h.w.transfer(ctx, h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint("10.0.0.1")), share, lf, nil).filed {
		t.Fatalf("size-mismatch transfer should not file")
	}
	if _, ok := h.getFrame(src); ok {
		t.Fatalf("no frame should be recorded on size mismatch")
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatalf("staging temp leaked on size mismatch")
	}
}

func TestPullCorruptAtRestNeverFiles(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	// Simulate at-rest corruption: VerifyRead reports a different hash than the
	// bytes actually streamed → the design delete-safety §6 data-loss guard must
	// refuse to file.
	h.lfs.CorruptAfterWrite = func(_, honest string) string {
		return strings.Repeat("0", len(honest))
	}
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	if _, ok := h.getFrame(src); ok {
		t.Fatalf("a frame whose at-rest verify FAILED must never be filed")
	}
	if !h.hasAlert(model.AlertVerifyMismatch) {
		t.Fatalf("at-rest mismatch must alert; got %v", h.raisedCategories())
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatalf("staging temp leaked after corrupt verify")
	}
	// The device original is untouched — a re-pull can recover.
	if !h.deviceHas(src) {
		t.Fatalf("device original must remain after a failed verify")
	}
}

// --- filing --------------------------------------------------------------------

func TestFileIdempotentRePullPreservesPulledAt(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(emmc, src, data, mt)

	h.cycle()
	first, ok := h.getFrame(src)
	if !ok {
		t.Fatal("first pull did not record")
	}
	t0 := first.PulledAt

	// A re-pull later (same content, existing dest) must adopt the NAS copy and
	// NOT restart the grace clock.
	h.nowVal = h.nowVal.Add(30 * time.Minute)
	sess, _ := h.dev.Open(ctx)
	share, _ := sess.Mount(emmc)
	lf := listedFrame{share: emmc, path: src, name: "Light_0001.fit", size: int64(len(data)), mtime: mt}
	if !h.w.transfer(ctx, h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint("10.0.0.1")), share, lf, first).filed {
		t.Fatalf("idempotent re-pull should succeed")
	}
	again, _ := h.getFrame(src)
	if again.PulledAt != t0 {
		t.Fatalf("pulled_at reset to %d, want preserved %d", again.PulledAt, t0)
	}
	// No duplicate on disk: exactly one file in the dest dir.
	entries, _ := os.ReadDir(filepath.Dir(again.DestPath))
	if len(entries) != 1 {
		t.Fatalf("dest dir has %d files, want 1 (no duplicate)", len(entries))
	}
}

func TestFileDifferentContentSameNameBothKept(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	base := fixture(t, "asiair_light_2600mm_ha.fit")
	// Two files sharing a basename but with different content (one padding byte
	// past the END card differs, so the header — and thus the route — is identical).
	a := append([]byte(nil), base...)
	b := append([]byte(nil), base...)
	b[len(b)-1] = 'X'
	if sha256hex(a) == sha256hex(b) {
		t.Fatal("test setup: contents must differ")
	}
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(emmc, "Autorun/Light/o1/Light_0001.fit", a, mt)
	h.dev.AddFile(emmc, "Autorun/Light/o2/Light_0001.fit", b, mt)

	h.cycle()

	fa, oka := h.getFrame("Autorun/Light/o1/Light_0001.fit")
	fb, okb := h.getFrame("Autorun/Light/o2/Light_0001.fit")
	if !oka || !okb {
		t.Fatalf("both frames should record (a=%v b=%v)", oka, okb)
	}
	if fa.DestPath == fb.DestPath {
		t.Fatalf("distinct content must file under distinct names; both=%q", fa.DestPath)
	}
	if _, err := os.Stat(fa.DestPath); err != nil {
		t.Fatalf("frame a missing on disk: %v", err)
	}
	if _, err := os.Stat(fb.DestPath); err != nil {
		t.Fatalf("frame b missing on disk: %v", err)
	}
	if !h.hasAlert(model.AlertRotClash) {
		t.Fatalf("a canonical-name content clash must raise rot-clash; got %v", h.raisedCategories())
	}
}

func TestQuarantineUnclassifiedNeverDeletable(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	// A BIAS routed by <SENSOR> with the sensor map cleared → unresolvable →
	// quarantine (design routing-and-filing §2).
	h.cfg.SensorMap = map[string]string{}
	data := fixture(t, "asiair_bias_6200mm.fit")
	src := "Autorun/Bias/Bias_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("quarantined frame not recorded")
	}
	if f.State != model.StateUnclassified {
		t.Fatalf("state=%s, want unclassified", f.State)
	}
	wantDir := filepath.Join(h.dir, "light", "unclassified", "rig-01")
	if !strings.HasPrefix(f.DestPath, wantDir) {
		t.Fatalf("dest=%q not under quarantine %q", f.DestPath, wantDir)
	}
	if _, err := os.Stat(f.DestPath); err != nil {
		t.Fatalf("quarantined frame missing on disk: %v", err)
	}
	if !h.hasAlert(model.AlertUnclassified) {
		t.Fatalf("quarantine must alert; got %v", h.raisedCategories())
	}
	// Never deletion-eligible, even long past grace.
	h.nowVal = h.nowVal.Add(10 * time.Hour)
	elig, err := h.store.DeletionEligible(ctx, "rig-01", h.nowVal.Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(elig) != 0 {
		t.Fatalf("unclassified frame must never be deletion-eligible; got %d", len(elig))
	}
}

// --- delete pass ---------------------------------------------------------------

// fileAndAge pulls one light frame (with an optional preview sidecar) and returns
// its recorded row, advancing the clock past grace so it is deletion-eligible.
func fileAndAge(t *testing.T, h *harness, withPreview bool) (*model.Frame, string) {
	t.Helper()
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(emmc, src, data, mt)
	preview := "Autorun/Light/o/Light_0001_thn.jpg"
	if withPreview {
		h.dev.AddFile(emmc, preview, []byte("preview-bytes"), mt)
	}
	h.cycle()
	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("frame not filed")
	}
	h.nowVal = h.nowVal.Add(2 * time.Hour) // past the 1h grace
	return f, preview
}

func TestDeleteThreeWayMatchDeletesFrameAndSidecar(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, preview := fileAndAge(t, h, true)
	if len(f.Sidecars) != 1 {
		t.Fatalf("expected 1 recorded sidecar, got %d", len(f.Sidecars))
	}

	h.cycle() // runs the delete pass; frame is now eligible

	if h.deviceHas(f.SourcePath) {
		t.Fatalf("proven original should be deleted from the device")
	}
	if h.deviceHas(preview) {
		t.Fatalf("recorded sidecar should be deleted with its parent")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", got.State)
	}
}

func TestDeleteCorruptNASHoldsNoDelete(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	// Corrupt the NAS re-hash at delete time → three-way check fails → hold.
	h.lfs.CorruptAfterWrite = func(_, honest string) string {
		return strings.Repeat("0", len(honest))
	}

	h.cycle()

	if !h.deviceHas(f.SourcePath) {
		t.Fatalf("a failed three-way check must NEVER delete the device original")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
	if !h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("delete mismatch must alert; got %v", h.raisedCategories())
	}
}

func TestDeleteSourceVanishedGoodNASDeletes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.dev.RemoveFile(emmc, f.SourcePath) // source vanished, NAS copy intact

	h.cycle()

	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("vanished source with good NAS copy should mark deleted; got %s", got.State)
	}
	if h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("a benign vanish must not alert frame-lost")
	}
}

// TestDeleteDoublyVanishedConverges: a frame the operator culled on BOTH sides
// (source gone AND NAS copy gone) is routine cleanup. It reaches a terminal
// `deleted` state — so it leaves DeletionEligible and is not re-probed every poll
// forever — and alerts frame_lost at WARN, not the critical alarm a genuinely
// unexplained loss raises (design library-reconciliation §4.4, I23).
func TestDeleteDoublyVanishedConverges(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false)
	h.dev.RemoveFile(emmc, f.SourcePath)          // source gone
	if err := os.Remove(f.DestPath); err != nil { // NAS copy gone too
		t.Fatal(err)
	}

	h.cycle()

	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("a both-sides cull must reach terminal deleted so it stops spinning; got %s", got.State)
	}
	if !h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("doubly-vanished must alert frame-lost; got %v", h.raisedCategories())
	}
	if sev := h.alertSeverity(model.AlertFrameLost); sev != model.SeverityWarn {
		t.Fatalf("a both-sides cull's frame_lost must be warn, not %s", sev)
	}
}

func TestKeepOriginalsSkipsDeleteThenDrains(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.KeepOriginals = true

	f, _ := fileAndAge(t, h, false) // pulled, verified, past grace → deletion-eligible

	h.cycle() // delete pass is gated off by keep-originals

	if !h.deviceHas(f.SourcePath) {
		t.Fatalf("keep-originals rig must never delete the device original")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified (held on device)", got.State)
	}

	// Flipping keep-originals off drains the backlog on the very next pass with no
	// migration — eligibility is derived from verified state + grace.
	h.profile.KeepOriginals = false

	h.cycle()

	if h.deviceHas(f.SourcePath) {
		t.Fatalf("un-gated delete pass should remove the now-eligible original")
	}
	got, _ = h.getFrame(f.SourcePath)
	if got.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted after keep-originals turned off", got.State)
	}
}

// A keep-originals rig never frees device space, so its device line must count
// originals kept — never the deleting rig's drained — and the status must flag the
// keep-originals mode so the dashboard drops the "space freed"/"in grace" framing.
// Flipping the toggle off drains the backlog and the status reverts to drained.
func TestKeepOriginalsDrainReadsKeptNotDrained(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.KeepOriginals = true

	f, _ := fileAndAge(t, h, false) // filed + verified → resident on the device

	h.cycle() // keep-originals: no delete pass, the original stays resident

	st := h.status()
	if !st.KeepOriginals {
		t.Fatalf("status.KeepOriginals=false, want true for a keep-originals rig")
	}
	if got, want := st.DeviceFrames, (&model.DeviceFrames{Kept: 1}); got == nil || *got != *want {
		t.Fatalf("device frames=%+v, want %+v (a keep-originals rig never drains/frees space)", got, want)
	}

	// Turning keep-originals off deletes the now-eligible original and the device
	// truly drains: the status drops the flag and reverts to the deleting wording.
	// The delete pass runs after this cycle's status is published, so a second pass
	// is needed for the device breakdown to reflect the removal.
	h.profile.KeepOriginals = false
	h.cycle() // deletes the original (status still published pre-delete)
	h.cycle() // the device breakdown now reflects the removal

	if h.deviceHas(f.SourcePath) {
		t.Fatalf("un-gated delete pass should remove the now-eligible original")
	}
	st = h.status()
	if st.KeepOriginals {
		t.Fatalf("status.KeepOriginals=true after the toggle was turned off")
	}
	if got, want := st.DeviceFrames, (&model.DeviceFrames{Drained: true}); got == nil || *got != *want {
		t.Fatalf("device frames=%+v, want %+v after the device drained", got, want)
	}
}

// A keep-originals rig must not delete ANYTHING from the device — including an
// orphaned preview the orphan sweep would otherwise remove.
func TestKeepOriginalsSkipsOrphanSweep(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.KeepOriginals = true

	f, preview := fileAndAge(t, h, true)
	// Remove the parent frame so its preview is now orphaned on the device.
	h.dev.RemoveFile(emmc, f.SourcePath)

	h.cycle() // the orphan sweep is gated off by keep-originals

	if !h.deviceHas(preview) {
		t.Fatalf("keep-originals rig must not delete an orphaned preview from the device")
	}
}

// refreshLiveCounters publishes the real transferred-tonight count (the same query
// the cycle-end status uses), so the dashboard climbs live during a pull.
func TestRefreshLiveCountersPublishesRealCount(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		PulledAt: h.nowVal.Unix(), DestPath: filepath.Join(h.cfg.Roots.Light, "x.fit"),
	}
	if err := h.store.UpsertFrame(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	h.w.refreshLiveCounters(context.Background(), h.cfg.Rig("rig-01"), 0)
	if got := h.status().FramesTonight; got != 1 {
		t.Fatalf("live refresh FramesTonight = %d, want 1", got)
	}
}

// A keep-originals rig's kept count on the device line climbs live during a pull,
// mirroring transferred-tonight, instead of updating only at the cycle boundary.
func TestRefreshLiveCountersUpdatesKeptDrain(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	p := h.cfg.Rig("rig-01")
	p.KeepOriginals = true
	f := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/x.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateVerified,
		PulledAt: h.nowVal.Unix(), DestPath: filepath.Join(h.cfg.Roots.Light, "x.fit"),
	}
	if err := h.store.UpsertFrame(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	h.w.refreshLiveCounters(context.Background(), p, 0)
	if got, want := h.status().DeviceFrames, (&model.DeviceFrames{Kept: 1}); got == nil || *got != *want {
		t.Fatalf("live refresh device frames = %+v, want %+v", got, want)
	}
}

// An unconfigured (empty) light root must skip the night-counter rebuild rather
// than walk the drive root and hit protected system directories (which a
// low-privilege service account cannot read).
func TestRebuildNightCountersEmptyRootSkips(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	cfg := &model.Config{} // Roots.Light is empty
	if err := RebuildNightCounters(context.Background(), cfg, st, header.New(), localfs.New(), time.UTC, nil); err != nil {
		t.Fatalf("empty light root must skip cleanly, got %v", err)
	}
}

func TestDeleteGraceNotElapsedNotEligible(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
	h.cycle() // pulled_at = now; grace 1h not elapsed

	h.cycle() // a second cycle without advancing the clock

	if !h.deviceHas(src) {
		t.Fatalf("frame within grace must not be deleted")
	}
	got, _ := h.getFrame(src)
	if got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified", got.State)
	}
}

// --- identity ------------------------------------------------------------------

// TestIdentityForeignRefusesAndAlerts: a guid-bound rig whose live probe reports
// a DIFFERENT device (incl. the CM4 both-empty-cpuId case here — profile carries
// no cpuId) refuses every pull and delete, alerts, and surfaces an "identity"
// LastError so the UI banner fires.
func TestIdentityForeignRefusesAndAlerts(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "someone-else", true // a different device answers
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	if _, ok := h.getFrame(src); ok {
		t.Fatalf("a foreign device must refuse all pulls")
	}
	if !h.deviceHas(src) {
		t.Fatalf("a foreign device must not delete either")
	}
	if !h.hasAlert(model.AlertIdentityMismatch) {
		t.Fatalf("a foreign device must alert; got %v", h.raisedCategories())
	}
	if s := h.status(); s.State != model.RigStateError || !strings.Contains(s.LastError, "identity") {
		t.Fatalf("status = %s / %q, want error state with an 'identity' LastError", s.State, s.LastError)
	}
}

// TestIdentityConfirmedPulls: a guid-bound rig whose live probe reports its own
// guid confirms and pulls, caching the connect address as the last-known IP.
func TestIdentityConfirmedPulls(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle()

	if _, ok := h.getFrame(src); !ok {
		t.Fatal("a confirmed device should pull its frames")
	}
	if v := h.meta(LastIPKey("rig-01")); v != "10.0.0.1" {
		t.Fatalf("a confirmed cycle should record lastip=10.0.0.1, got %q", v)
	}
}

// TestStatusPublishesResolvedAddress: a confirmed cycle publishes the resolved
// device IP on RigStatus.Address (serialized), and an offline cycle still carries
// the last resolved address so the UI can offer the SMB shortcut while unreachable.
func TestStatusPublishesResolvedAddress(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true

	h.cycle()

	st := h.status()
	if st.Address != "10.0.0.1" {
		t.Fatalf("online status Address=%q, want 10.0.0.1", st.Address)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if !strings.Contains(string(b), `"address":"10.0.0.1"`) {
		t.Fatalf("status serialized as %s, want an address field", b)
	}

	// The device drops off the network; the resolved address is still known.
	h.dev.SetReachable(false)
	h.cycle()
	if st := h.status(); st.Online || st.Address != "10.0.0.1" {
		t.Fatalf("offline status = online:%v address:%q, want online:false address:10.0.0.1", st.Online, st.Address)
	}
}

// TestNoReplyStreakDegradesThenAutoAcks: three consecutive no-reply cycles raise
// exactly one degraded alert (dedup'd), and a later confirmed cycle auto-acks a
// standing one.
func TestNoReplyStreakDegradesThenAutoAcks(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeOK = false // no scan_air reply

	h.cycle()
	h.cycle()
	if h.hasAlert(model.AlertIdentityUnconfirmed) {
		t.Fatal("two no-reply cycles must not raise the degraded alert yet")
	}
	h.cycle() // third consecutive
	count := 0
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertIdentityUnconfirmed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("three no-reply cycles should raise exactly one degraded alert, got %d", count)
	}
	h.cycle() // a fourth no-reply must not re-raise
	count = 0
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertIdentityUnconfirmed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the degraded alert must not re-raise while unresolved, got %d", count)
	}

	// A standing degraded alert is auto-acked once identity is confirmed again.
	id, _, err := h.store.RaiseAlert(ctx, model.Alert{
		RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertIdentityUnconfirmed,
		Message: "unconfirmed", CreatedAt: h.nowVal.Unix(),
	})
	if err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
	h.cycle()
	unacked, err := h.store.ListAlerts(ctx, false, 0)
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	for _, a := range unacked {
		if a.ID == id {
			t.Fatal("a confirmed cycle should auto-ack the standing degraded alert")
		}
	}
}

// --- reclassify ----------------------------------------------------------------

func TestReclassifyResolvesAfterAliasPreservesPulledAt(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	// Quarantine a bias by clearing the sensor map, then add it and reclassify.
	h.cfg.SensorMap = map[string]string{}
	data := fixture(t, "asiair_bias_6200mm.fit")
	src := "Autorun/Bias/Bias_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
	h.cycle()

	before, ok := h.getFrame(src)
	if !ok || before.State != model.StateUnclassified {
		t.Fatalf("frame should be unclassified first (ok=%v)", ok)
	}
	pulledAt := before.PulledAt

	// Add the sensor mapping and swap the live config, then reclassify.
	newCfg := *h.cfg
	newCfg.SensorMap = map[string]string{"ZWO ASI6200MM Pro": "6200MM"}
	h.cfgPtr.Store(&newCfg)
	h.w.runReclassify(ctx)

	after, _ := h.getFrame(src)
	if after.State != model.StateVerified {
		t.Fatalf("reclassify should resolve to verified; got %s", after.State)
	}
	if !strings.Contains(after.DestPath, filepath.Join("cal", "6200MM", "BIAS")) {
		t.Fatalf("dest=%q not routed into the cal tree", after.DestPath)
	}
	if after.PulledAt != pulledAt {
		t.Fatalf("reclassify must preserve pulled_at (%d != %d)", after.PulledAt, pulledAt)
	}
	if _, err := os.Stat(after.DestPath); err != nil {
		t.Fatalf("reclassified file missing on disk: %v", err)
	}
}

// withoutCard blanks one FITS header card, keyword and all, as a camera that never
// wrote it would leave the header.
func withoutCard(t *testing.T, data []byte, keyword string) []byte {
	t.Helper()
	out := bytes.Clone(data)
	key := []byte(fmt.Sprintf("%-8s=", keyword))
	for off := 0; off+80 <= len(out); off += 80 {
		if bytes.HasPrefix(out[off:off+80], key) {
			copy(out[off:off+80], bytes.Repeat([]byte(" "), 80))
			return out
		}
		if bytes.HasPrefix(out[off:off+80], []byte("END     ")) {
			break
		}
	}
	t.Fatalf("fixture has no %s card", keyword)
	return nil
}

// An uncooled dark (no SET-TEMP card) files into the uncooled bucket instead of
// quarantining, and records its absence so a later re-file keeps it there. A dark
// quarantined for the same reason before the bucket existed resolves on
// reclassify.
func TestUncooledDarkFilesToUncooledBucket(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	data := withoutCard(t, fixture(t, "asiair_dark_6200mm.fit"), "SET-TEMP")
	src := "Autorun/Dark/Dark_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
	h.cycle()

	f, ok := h.getFrame(src)
	if !ok || f.State != model.StateVerified {
		t.Fatalf("an uncooled dark must file, not quarantine (ok=%v state=%v)", ok, f.State)
	}
	if !strings.Contains(f.DestPath, string(filepath.Separator)+route.Uncooled+string(filepath.Separator)) {
		t.Fatalf("dest=%q, want an uncooled path component", f.DestPath)
	}
	if !f.SetTempAbsent {
		t.Fatal("the row must record that SET-TEMP was absent")
	}

	// The same frame quarantined earlier: reclassify moves it into the bucket.
	src2 := "Autorun/Dark/Dark_0002.fit"
	h.cfg.SensorMap = map[string]string{}
	h.dev.AddFile(emmc, src2, data, h.nowVal.Add(-10*time.Minute))
	h.cycle()
	if g, _ := h.getFrame(src2); g.State != model.StateUnclassified {
		t.Fatalf("setup: want the second dark unclassified, got %s", g.State)
	}
	newCfg := *h.cfg
	newCfg.SensorMap = map[string]string{"ZWO ASI6200MM Pro": "6200MM"}
	h.cfgPtr.Store(&newCfg)
	h.w.runReclassify(ctx)
	g, _ := h.getFrame(src2)
	if g.State != model.StateVerified || !strings.Contains(g.DestPath, route.Uncooled) || !g.SetTempAbsent {
		t.Fatalf("reclassify: state=%s dest=%q absent=%v", g.State, g.DestPath, g.SetTempAbsent)
	}
}

// --- per-file recover ----------------------------------------------------------

func TestPerFileRecoverYieldsFailedRow(t *testing.T) {
	h := newHarness(t, emmc, panicReader{})
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle() // header read panics downstream; the worker must survive

	f, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("a panicked frame should still yield a row")
	}
	if f.State != model.StateFailed {
		t.Fatalf("state=%s, want failed", f.State)
	}
	if f.Attempts != 1 {
		t.Fatalf("attempts=%d, want 1", f.Attempts)
	}
	if !h.hasAlert(model.AlertTransferFailed) {
		t.Fatalf("a failed frame must alert; got %v", h.raisedCategories())
	}
	if !stagingEmpty(t, h.dir) {
		t.Fatalf("staging temp leaked after a recovered panic")
	}
}

// --- safe to power off ---------------------------------------------------------

func TestSafeToPowerOffOfflineUnknown(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.dev.SetReachable(false)
	h.cycle()
	if got := h.status().SafeToPowerOff; got != model.SafeUnknown {
		t.Fatalf("offline safe=%s, want unknown", got)
	}
}

func TestInitialStatusSerializesSharesAsEmptyNotNull(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	// A rig that has not completed a listing (offline/unreachable at startup) still
	// gets published. Its shares must serialize as [] rather than null so the
	// dashboard can map over the slice; a null crashes the rig card.
	h.w.publish()
	st := h.status()
	if st.Shares == nil {
		t.Fatalf("shares is nil before any listing; want a non-nil empty slice")
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if !strings.Contains(string(b), `"shares":[]`) {
		t.Fatalf("shares serialized as %s, want %q", b, `"shares":[]`)
	}
}

func TestSafeToPowerOffFreshVerifiedIdleSafe(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))

	h.cycle() // pulls the frame this cycle → working
	if got := h.status().SafeToPowerOff; got == model.SafeYes {
		t.Fatalf("the cycle that pulled a frame must not read safe")
	}
	h.cycle() // nothing new, frame verified, idle a full cycle → safe
	if got := h.status().SafeToPowerOff; got != model.SafeYes {
		t.Fatalf("fresh+verified+idle safe=%s, want safe", got)
	}

	// A new unpulled file flips it back to working.
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0002.fit", data, h.nowVal.Add(-10*time.Minute))
	h.cycle()
	if got := h.status().SafeToPowerOff; got != model.SafeWorking {
		t.Fatalf("a new unpulled file safe=%s, want working", got)
	}
}

func TestSafeToPowerOffStaleListingUnknown(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.dev.FailListDir(emmc, device.NetworkError("list boom"))
	h.cycle()
	if got := h.status().SafeToPowerOff; got != model.SafeUnknown {
		t.Fatalf("failed listing safe=%s, want unknown", got)
	}
}

// A reclassify re-files frames that are already on the NAS: no transfer happens,
// nothing appears in the live feed, and the held table simply empties. The counts
// it publishes are the only report the operator gets that the pass did anything,
// so every completed pass must emit them — including the one that moved nothing.
func TestReclassifyEmitsOutcome(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.cfg.SensorMap = map[string]string{} // no mapping → the bias is held
	h.dev.AddFile(emmc, "Autorun/Bias/Bias_0001.fit",
		fixture(t, "asiair_bias_6200mm.fit"), h.nowVal.Add(-10*time.Minute))
	h.cycle()

	counts := func(evts []hub.Event) (examined, moved int) {
		e := findEvent(evts, "rig.reclassified")
		if e == nil {
			t.Fatalf("no rig.reclassified event; got %v", evts)
		}
		d, ok := e.Data.(map[string]any)
		if !ok {
			t.Fatalf("event data %T, want a map", e.Data)
		}
		if got := d["rig_id"]; got != "rig-01" {
			t.Fatalf("rig_id=%v, want rig-01", got)
		}
		return d["examined"].(int), d["moved"].(int)
	}

	// Still no alias: the pass examines the held frame and moves nothing.
	if examined, moved := counts(h.captureEvents(func() { h.w.runReclassify(ctx) })); examined != 1 || moved != 0 {
		t.Fatalf("unresolved pass examined=%d moved=%d, want 1/0", examined, moved)
	}

	newCfg := *h.cfg
	newCfg.SensorMap = map[string]string{"ZWO ASI6200MM Pro": "6200MM"}
	h.cfgPtr.Store(&newCfg)

	if examined, moved := counts(h.captureEvents(func() { h.w.runReclassify(ctx) })); examined != 1 || moved != 1 {
		t.Fatalf("resolving pass examined=%d moved=%d, want 1/1", examined, moved)
	}

	// Nothing left held: the pass still reports, so the UI can say so.
	if examined, moved := counts(h.captureEvents(func() { h.w.runReclassify(ctx) })); examined != 0 || moved != 0 {
		t.Fatalf("empty pass examined=%d moved=%d, want 0/0", examined, moved)
	}
}

// A paused rig runs no listing, so nothing refreshes its power-off verdict. Pause
// must downgrade it rather than leave the last cycle's answer frozen on the card:
// the hero is the trust signal, and a paused rig reading "Safe to power off" — or
// "Working — don't power off" while it does nothing at all — reports a conclusion
// no check stands behind.
func TestPauseDowngradesPowerOffVerdict(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit",
		fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.cycle() // pulls the frame
	h.cycle() // verified and idle a full cycle → safe
	if got := h.status().SafeToPowerOff; got != model.SafeYes {
		t.Fatalf("precondition: safe=%s, want safe", got)
	}

	h.w.apply(context.Background(), Pause)

	st := h.status()
	if st.SafeToPowerOff != model.SafeUnknown {
		t.Fatalf("paused safe=%s, want unknown", st.SafeToPowerOff)
	}
	if st.PowerOffReason != "paused" {
		t.Fatalf("paused reason=%q, want %q", st.PowerOffReason, "paused")
	}
}

// Resume re-checks at once. Otherwise the paused verdict — whose help text reads
// "Resume to refresh" — stands until the next tick, telling the operator to resume
// a rig they just resumed.
func TestResumeRechecksImmediately(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cycle()
	h.w.apply(context.Background(), Pause)
	if got := h.status().PowerOffReason; got != "paused" {
		t.Fatalf("precondition: reason=%q, want %q", got, "paused")
	}

	h.w.apply(context.Background(), Resume)

	st := h.status()
	if st.Paused {
		t.Fatalf("rig still publishes Paused after a resume")
	}
	if st.State == model.RigStatePaused {
		t.Fatalf("state=%s after a resume, want the state of a fresh cycle", st.State)
	}
	if st.PowerOffReason == "paused" {
		t.Fatalf("resume left the paused verdict standing; want it recomputed")
	}
}

// A pause that lands mid-cycle is drained between files, not at the command
// select, and flipping the flag alone is not enough: with no log line, no Paused
// on the card and no verdict downgrade, the rig would sit out every later cycle
// while still publishing a working state, and the operator's only clue would be
// that nothing ever moved. A pause is a pause whichever path delivers it.
func TestPauseDrainedMidCycleIsPublished(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit",
		fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.cycle() // pulls the frame
	h.cycle() // verified and idle a full cycle → safe
	if got := h.status().SafeToPowerOff; got != model.SafeYes {
		t.Fatalf("precondition: safe=%s, want safe", got)
	}

	// Queue it the way the manager does, then let a cycle consume it between files.
	h.w.Send(Pause)
	h.cycle()

	st := h.status()
	if !st.Paused {
		t.Fatal("a pause drained mid-cycle is not published as Paused")
	}
	if st.State != model.RigStatePaused {
		t.Fatalf("state=%s after a mid-cycle pause, want paused", st.State)
	}
	if st.SafeToPowerOff != model.SafeUnknown || st.PowerOffReason != "paused" {
		t.Fatalf("mid-cycle pause safe=%s reason=%q, want unknown/paused",
			st.SafeToPowerOff, st.PowerOffReason)
	}
	if !strings.Contains(h.logBuf.String(), "rig: paused") {
		t.Fatal("a mid-cycle pause must be logged like any other")
	}
}

// The manager queues Pause before Run to restore a pause that outlived a restart.
// If Run's first cycle swallowed it between files, the restored pause would never
// reach the command select: the rig would pull and delete its way through that
// cycle, then go quiet forever with nothing on the card to say why.
func TestQueuedPauseIsAppliedBeforeTheFirstCycle(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit",
		fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))

	h.w.Send(Pause) // exactly what startWorkerLocked does for a persisted pause

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.w.Run(ctx); close(done) }()
	waitFor(t, func() bool { return h.status().Paused })
	cancel()
	<-done

	if _, ok := h.getFrame("Autorun/Light/o/Light_0001.fit"); ok {
		t.Fatal("a rig that starts paused pulled a frame on its first cycle")
	}
	if st := h.status(); st.State != model.RigStatePaused {
		t.Fatalf("state=%s on a rig that started paused, want paused", st.State)
	}
}

// waitFor polls cond until it holds, failing the test rather than hanging the
// suite when the worker never gets there.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the worker to publish the expected state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- regression: delete-pass TOCTOU (BUG-1) ------------------------------------

// TestDeleteSourceOverwrittenBetweenVerifyAndDeleteIsNotLost exercises the final
// source re-stat immediately before share.Delete. The delete pass hashes for
// seconds (a full device re-read + the NAS re-hash); a counter-reset run can
// overwrite the exact source path in that window with new, never-verified content
// of the SAME byte size (all ASIAIR frames are one size) but a later mtime. The
// worker must NOT delete that content. Here we advance the source mtime on the
// SECOND stat of it (the pre-delete stat), leaving its bytes — hence its hash —
// unchanged so the three-way check still passes, isolating the re-stat guard.
func TestDeleteSourceOverwrittenBetweenVerifyAndDeleteIsNotLost(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f, _ := fileAndAge(t, h, false) // filed, verified, grace elapsed

	var statCount int
	h.dev.OnStat = func(share, path string) {
		if share == emmc && path == f.SourcePath {
			statCount++
			if statCount == 2 { // between the hashes and the delete
				h.dev.SetMtime(emmc, f.SourcePath, time.Unix(f.Mtime, 0).Add(time.Hour))
			}
		}
	}

	h.w.deleteOne(context.Background(), h.localTarget(), h.mountedSession(t), f.ID, map[string]bool{}, noIntents, noIntents, defaultReconcileSettle)
	h.dev.OnStat = nil

	if !h.deviceHas(f.SourcePath) {
		t.Fatalf("source overwritten between verify and delete must NOT be deleted (would lose never-verified content)")
	}
	got, _ := h.getFrame(f.SourcePath)
	if got.State != model.StateVerified {
		t.Fatalf("frame should stay verified (still safe on NAS), got %s", got.State)
	}
	if h.hasAlert(model.AlertDeleteMismatch) || h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("a benign mid-pass overwrite must not raise a scary alert; got %v", h.raisedCategories())
	}
}

// --- regression: reclassify panic isolation (BUG-2) ----------------------------

// TestReclassifyPanicDoesNotKillWorker proves an idle-dispatched reclassify
// survives a panic reading a malformed header: the frame stays unclassified and
// the worker goroutine is not killed (which would stop the rig draining — the
// exact failure the tool exists to prevent).
func TestReclassifyPanicDoesNotKillWorker(t *testing.T) {
	h := newHarness(t, emmc, panicReader{})
	src := "Autorun/Light/o/x_0001.fit"
	if err := h.store.UpsertFrame(context.Background(), &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: src,
		SHA256: "abc", Size: 100, Mtime: 1,
		DestPath: filepath.Join(h.cfg.Roots.Light, "unclassified", "rig-01", "x_0001.fit"),
		State:    model.StateUnclassified, PulledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	h.w.runReclassify(context.Background()) // must not panic out

	got, ok := h.getFrame(src)
	if !ok || got.State != model.StateUnclassified {
		t.Fatalf("frame should remain unclassified after a recovered panic; ok=%v state=%v", ok, got.State)
	}
}

// TestSafeToPowerOffDrainedRemovableIsSafe is the end-to-end counterpart to
// TestPullRemovableAbsentMediumQuiet: a removable stick that is inserted but has
// been fully drained must read SAFE. Mistaking the empty listing for an absent medium
// would set mediaAbsent and pin the rig at "unknown" on every later cycle —
// punishing a successful drain, with no user action able to clear it.
// TestSafeToPowerOffDrainedRemovableIsSafe covers both removable slots (A2): a
// medium that is inserted but fully drained reads Safe, for a Udisk (data nested
// under ASIAIR/) and for a microSD (TF is fixed-layout, capture roots at the share
// root). Only a genuinely absent slot suppresses the verdict.
func TestSafeToPowerOffDrainedRemovableIsSafe(t *testing.T) {
	for _, tc := range []struct{ share, drainedDir string }{
		{udisk, "ASIAIR/Autorun"},
		{tf, "Autorun"},
	} {
		t.Run(tc.share, func(t *testing.T) {
			h := newHarness(t, tc.share, header.New())
			// The medium is inserted — its capture roots are there — but every frame has
			// already been pulled and removed, leaving the capture folder empty.
			h.dev.AddDir(tc.share, tc.drainedDir, h.nowVal)

			// Two cycles: "safe" additionally requires a full idle cycle (no new files
			// since the prior listing), so the first cycle establishes the signature.
			h.cycle()
			h.cycle()

			if h.logContains("no medium inserted") {
				t.Fatalf("a drained but inserted medium must not read as absent; log:\n%s", h.logBuf.String())
			}
			if got := h.status().SafeToPowerOff; got != model.SafeYes {
				t.Fatalf("SafeToPowerOff=%q, want %q for a drained but inserted removable medium", got, model.SafeYes)
			}
		})
	}
}

// TestPowerOffReasonPriority (B1) pins that PowerOffReason is set for each Unknown
// condition in the documented priority (paused > offline > missing_on_device >
// removable_absent > listing_failed), and is empty whenever the verdict is not
// Unknown. It drives publishListingStatus directly with crafted inputs so
// overlapping conditions can be exercised without a multi-share device.
func TestPowerOffReasonPriority(t *testing.T) {
	for _, tc := range []struct {
		name        string
		paused      bool
		online      bool
		listingOK   bool
		absent      bool
		missing     bool
		pendingWork int
		idle        bool
		wantSafe    model.SafeToPowerOff
		wantReason  string
	}{
		// A safe verdict carries no reason.
		{"safe carries no reason", false, true, true, false, false, 0, true, model.SafeYes, ""},
		// paused outranks every other condition (a pause can truncate the listing).
		{"paused ranks first", true, true, true, true, true, 0, true, model.SafeUnknown, "paused"},
		// offline is defensive here (an offline rig usually early-returns).
		{"offline", false, false, true, false, false, 0, true, model.SafeUnknown, "offline"},
		// missing-on-device (a concrete data risk) outranks an absent slot.
		{"missing over absent", false, true, true, true, true, 0, true, model.SafeUnknown, "missing_on_device"},
		// an absent removable slot outranks a bare listing failure.
		{"absent over listing_failed", false, true, false, true, false, 0, true, model.SafeUnknown, "removable_absent"},
		{"listing_failed default", false, true, false, false, false, 0, true, model.SafeUnknown, "listing_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, udisk, header.New())
			ctx := context.Background()
			h.w.paused = tc.paused
			h.w.st.Online = tc.online
			sess, err := h.dev.Open(ctx)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			ms := newMountedSession(sess)
			defer ms.Close()
			absent := map[string]bool{}
			var order []string
			if tc.absent {
				absent[udisk] = true
				order = []string{udisk}
			}
			h.w.publishListingStatus(ctx, h.cfg, h.profile, ms, order, tc.listingOK, absent, nil, tc.pendingWork, 0, tc.idle, h.nowVal.Unix(), 0, tc.missing)

			st := h.status()
			if st.SafeToPowerOff != tc.wantSafe {
				t.Fatalf("SafeToPowerOff=%q, want %q", st.SafeToPowerOff, tc.wantSafe)
			}
			if st.PowerOffReason != tc.wantReason {
				t.Fatalf("PowerOffReason=%q, want %q", st.PowerOffReason, tc.wantReason)
			}
		})
	}
}

// TestDestinationUnavailableAbortsCycleAndAlertsOnce pins the unreadable-
// destination behaviour: one cycle-level WARN rather than one per queued frame,
// one deduped alert so the stall is not silent, and the pending snapshot marked
// non-authoritative because the pass was truncated.
func TestDestinationUnavailableAbortsCycleAndAlertsOnce(t *testing.T) {
	h := newHarness(t, udisk, header.New())

	// Several frames are waiting; the destination volume cannot be statted at all.
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	for _, n := range []string{"a", "b", "c"} {
		h.dev.AddFile(udisk, "Autorun/Light/"+n+".fit", data, h.nowVal.Add(-10*time.Minute))
	}
	h.lfs.FreeSpaceErr = errors.New("statfs: no such file or directory")
	h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)

	h.cycle()

	// One line for the condition, not one per frame.
	if got := strings.Count(h.logBuf.String(), "admission acquire aborted"); got != 0 {
		t.Fatalf("an unreadable destination must not emit the per-frame admission WARN, got %d", got)
	}
	if got := strings.Count(h.logBuf.String(), "abandoned the rest of this cycle"); got != 1 {
		t.Fatalf("want exactly 1 cycle-level WARN for an unreadable destination, got %d; log:\n%s", got, h.logBuf.String())
	}
	// The stall is alerted, fleet-wide: the NAS is one shared volume, so the alert
	// carries no rig id and dedups to a single row however many rigs observe it.
	var found int
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertDestinationUnavailable {
			found++
			if a.RigID != "" {
				t.Fatalf("destination alert RigID=%q, want fleet-wide (empty) — the NAS is not per-rig", a.RigID)
			}
		}
	}
	if found != 1 {
		t.Fatalf("want exactly 1 destination-unavailable alert, got %d (raised: %v)", found, h.raisedCategories())
	}
	// A truncated pass must not be published as the authoritative pending snapshot.
	if h.status().PendingListed {
		t.Fatal("an aborted cycle must not mark its partial pending set as complete")
	}
}

// TestDestinationUnavailableDeletePassDoesNotFlood is the delete-side counterpart
// to the pull-side abort: Admission runs the same free-space probe for every
// eligible frame regardless of size, so an unreadable destination would otherwise
// log one WARN per frame in the delete pass — the very flood the pull-side fix
// removed.
func TestDestinationUnavailableDeletePassDoesNotFlood(t *testing.T) {
	h := newHarness(t, emmc, header.New())

	// Three frames pulled, verified, and past the grace window — all delete-eligible.
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	mt := h.nowVal.Add(-10 * time.Minute)
	for _, n := range []string{"a", "b", "c"} {
		h.dev.AddFile(emmc, "Autorun/Light/o/Light_000"+n+".fit", data, mt)
	}
	h.cycle()
	h.nowVal = h.nowVal.Add(2 * time.Hour) // past the 1h grace

	// The destination volume goes unreadable before the next cycle's delete pass.
	h.lfs.FreeSpaceErr = errors.New("statfs: no such file or directory")
	h.adm.acqErr = fmt.Errorf("%w: statfs: no such file or directory", ErrDestinationUnavailable)

	h.cycle()

	// The WARN form is the per-frame flood; the Debug form is the latched one-shot.
	if got := strings.Count(h.logBuf.String(), `level=WARN msg="rig: delete admission aborted"`); got != 0 {
		t.Fatalf("an unreadable destination must not emit a per-frame delete WARN, got %d; log:\n%s", got, h.logBuf.String())
	}
	// The latch stops the pass after the first frame rather than retrying each one.
	if got := strings.Count(h.logBuf.String(), "delete admission aborted; destination unreadable"); got != 1 {
		t.Fatalf("want exactly 1 latched delete-abort line for 3 eligible frames, got %d; log:\n%s", got, h.logBuf.String())
	}
}

// TestFleetWideAlertsAutoAckOnRecovery pins that the destination-volume alerts
// clear themselves. They are fleet-wide (no rig id, so one shared NAS yields one
// row rather than one per rig), and the rig-scoped auto-ack matcher can never
// match an empty rig id — so without a fleet-scoped ack they would stand forever
// after the condition cleared, exactly the stale-state class these alerts exist to
// avoid.
func TestFleetWideAlertsAutoAckOnRecovery(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	// Both conditions stood at some earlier point, raised fleet-wide.
	for _, cat := range []model.Category{model.AlertNASDiskLow, model.AlertDestinationUnavailable} {
		if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
			Severity:  model.SeverityWarn,
			Category:  cat,
			Message:   "stale " + string(cat),
			CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			t.Fatalf("seed %s alert: %v", cat, err)
		}
	}

	// A cycle with a readable destination and plenty of room resolves both.
	h.cycle()

	alerts, err := h.store.ListAlerts(ctx, false, 100)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	for _, a := range alerts {
		if a.Category == model.AlertNASDiskLow || a.Category == model.AlertDestinationUnavailable {
			t.Fatalf("fleet-wide %s alert still unacked after recovery; it can never clear itself", a.Category)
		}
	}
}

// TestNASDiskLowStaysWhileSpaceIsLow guards the counterpart: the auto-ack must not
// fire while the condition still holds, or the warning would flap away every cycle.
func TestNASDiskLowStaysWhileSpaceIsLow(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
		Severity:  model.SeverityWarn,
		Category:  model.AlertNASDiskLow,
		Message:   "NAS destination free space is below the reserve; pulls will pause",
		CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	h.lfs.FreeBytes = 1 // still below the reserve

	h.cycle()

	alerts, err := h.store.ListAlerts(ctx, false, 100)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	var found bool
	for _, a := range alerts {
		if a.Category == model.AlertNASDiskLow {
			found = true
		}
	}
	if !found {
		t.Fatal("the low-disk alert must stand while space is still low")
	}
}

// TestAbsentRemovableShareExcludedFromCapacity reproduces an observed hardware
// case: with no stick in the slot the ASIAIR still exports the Udisk share, and
// its statfs then reports the ASIAIR's OWN root filesystem — a small, nearly full
// partition. Counting that as the share's free space published a phantom figure
// and dragged the "tightest share" under the reserve, raising a device-disk-low
// alert for a slot that simply had nothing in it.
func TestAbsentRemovableShareExcludedFromCapacity(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	// An empty slot: the share mounts and its root lists, but holds no ASIAIR/
	// folder and no capture root. Its statfs reports the device's own tiny rootfs.
	h.dev.SetStatfs(udisk, 964<<20, 7159<<20)

	h.cycle()

	for _, s := range h.status().Shares {
		if s.Share == udisk {
			t.Fatalf("an absent removable share must not publish a free-space figure; got %d bytes for %q", s.Free, s.Share)
		}
	}
	if h.hasAlert(model.AlertDeviceDiskLow) {
		t.Fatalf("an empty slot must not raise device-disk-low; got %v", h.raisedCategories())
	}
}

// TestDeviceDiskLowAutoAcksOnRecovery: the device-disk-low alert has no path that
// retires it other than this auto-ack, so without it the warning stands forever
// once raised — including after the tight "share" turns out to have been an empty
// slot whose stick was reinserted.
func TestDeviceDiskLowAutoAcksOnRecovery(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()

	// A genuinely tight share raises the alert.
	h.dev.AddDir(udisk, "ASIAIR/Autorun", h.nowVal)
	h.dev.SetStatfs(udisk, 1<<20, 32<<30)
	h.cycle()
	if !h.hasAlert(model.AlertDeviceDiskLow) {
		t.Fatalf("a tight share should raise device-disk-low; got %v", h.raisedCategories())
	}
	if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
		RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertDeviceDiskLow,
		Message: "device free space is low", CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}

	// Space frees up.
	h.dev.SetStatfs(udisk, 30<<30, 32<<30)
	h.cycle()

	alerts, err := h.store.ListAlerts(ctx, false, 100)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	for _, a := range alerts {
		if a.Category == model.AlertDeviceDiskLow {
			t.Fatal("device-disk-low must auto-ack once the tightest share is roomy again")
		}
	}
}

// TestStaleAlertFromPreviousProcessAutoAcks reproduces an observed rollout: an
// alert raised by an earlier process, whose condition is already resolved by the
// time the new process starts. The auto-ack latches exist so a healthy rig does
// not scan the alert table every cycle, but starting them false meant a worker
// that boots with the condition already clear never takes the resolving branch —
// leaving the stale alert standing forever, dismissible only by hand.
func TestStaleAlertFromPreviousProcessAutoAcks(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	ctx := context.Background()

	// The previous process left these behind; this worker never raised them.
	for _, cat := range []model.Category{model.AlertDeviceDiskLow, model.AlertBacklog} {
		if _, _, err := h.store.RaiseAlert(ctx, model.Alert{
			RigID: "rig-01", Severity: model.SeverityWarn, Category: cat,
			Message: "stale " + string(cat), CreatedAt: h.nowVal.Unix(),
		}); err != nil {
			t.Fatalf("seed %s: %v", cat, err)
		}
	}

	// A healthy cycle: stick present and roomy, no never-deletable backlog.
	h.dev.AddDir(udisk, "ASIAIR/Autorun", h.nowVal)
	h.dev.SetStatfs(udisk, 30<<30, 32<<30)
	h.cycle()

	alerts, err := h.store.ListAlerts(ctx, false, 100)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	for _, a := range alerts {
		if a.Category == model.AlertDeviceDiskLow || a.Category == model.AlertBacklog {
			t.Fatalf("stale %s alert from a previous process was never retired; a restart must not strand it", a.Category)
		}
	}
}

// TestDeleteSweepsAppleDoubleTwin: a macOS finder browsing the device leaves a
// "._<name>" companion beside every file it touches. Those are junk to every read
// path — never pulled, never paired, never recorded as a sidecar — so nothing
// removed them and they accumulated on the device forever, one per frame. The
// delete pass now takes the twin of a frame it just proved and deleted.
func TestDeleteSweepsAppleDoubleTwin(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	preview := "Autorun/Light/o/Light_0001_thn.jpg"
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(emmc, src, data, mt)
	h.dev.AddFile(emmc, preview, []byte("preview-bytes"), mt)
	// The AppleDouble twins a Mac left behind for both.
	twin := "Autorun/Light/o/._Light_0001.fit"
	previewTwin := "Autorun/Light/o/._Light_0001_thn.jpg"
	h.dev.AddFile(emmc, twin, []byte("AppleDouble metadata"), mt)
	h.dev.AddFile(emmc, previewTwin, []byte("AppleDouble metadata"), mt)

	h.cycle()
	h.nowVal = h.nowVal.Add(2 * time.Hour) // past the 1h grace
	h.cycle()

	share := h.mountShare(emmc)
	if _, err := share.Stat(src); !device.IsNotFound(err) {
		t.Fatalf("precondition: the frame should have been deleted, stat err = %v", err)
	}
	if _, err := share.Stat(twin); !device.IsNotFound(err) {
		t.Fatalf("the AppleDouble twin must be swept with its parent, stat err = %v", err)
	}
	if _, err := share.Stat(previewTwin); !device.IsNotFound(err) {
		t.Fatalf("the preview's AppleDouble twin must be swept too, stat err = %v", err)
	}
}

// TestDeleteLeavesOrphanAppleDoubleAlone pins the narrow scope: the sweep only
// takes the twin of a file this pass deleted. A twin whose parent Commander never
// owned is left alone — the delete pass does not go hunting for junk.
func TestDeleteLeavesOrphanAppleDoubleAlone(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	src := "Autorun/Light/o/Light_0001.fit"
	mt := h.nowVal.Add(-10 * time.Minute)
	h.dev.AddFile(emmc, src, data, mt)
	// An orphan twin: no corresponding frame on the device at all.
	orphan := "Autorun/Light/o/._Light_9999_gone.fit"
	h.dev.AddFile(emmc, orphan, []byte("AppleDouble metadata"), mt)

	h.cycle()
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.cycle()

	share := h.mountShare(emmc)
	if _, err := share.Stat(orphan); err != nil {
		t.Fatalf("an orphan AppleDouble file must be left alone, stat err = %v", err)
	}
}

// TestAutoLinkConvergesAcrossTwoRigs is the multi-rig case (design
// library-reconciliation §8.4): two rigs sharing one destination root auto-link it
// concurrently and must converge on a single library id — never a self-inflicted
// marker/record mismatch that would fire a false "different library" critical
// alert. Runs under -race.
func TestAutoLinkConvergesAcrossTwoRigs(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	root := h.cfg.Roots.Light
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// A second worker for a different rig, sharing the SAME store, localfs and
	// config pointer — the multi-rig shared-root topology.
	factory := func(netsite.Endpoint, string, string, time.Duration) device.Device { return h.dev }
	w2 := NewWorker("rig-02", h.cfgPtr, factory, h.resolver, netsite.NewRegistry(nil), h.lfs, header.New(),
		h.store, h.hub, h.notify, h.adm, nil, nil, time.UTC,
		func() time.Time { return h.nowVal }, h.metrics,
		slog.New(slog.NewTextHandler(&lockedBuffer{}, nil)))

	var wg sync.WaitGroup
	for _, w := range []*Worker{h.w, w2} {
		wg.Add(1)
		go func(w *Worker) { defer wg.Done(); _ = w.autoLinkRoot(ctx, root) }(w)
	}
	wg.Wait()

	// Both rigs must now see the root as confirmed — i.e. the on-disk marker id
	// equals the recorded id for both, with no mismatch.
	for i, w := range []*Worker{h.w, w2} {
		if ok, reason := w.rootConfirmed(ctx, root); !ok {
			t.Fatalf("rig %d: root not confirmed after concurrent auto-link; reason=%q", i, reason)
		}
	}
}

// --- night ordinals from the destination folder --------------------------------

// TestLivePullAdoptsFolderNightAfterSpellingChange is the live-pull half of the
// repair: nothing about it involves a retarget. An ASIAIR target renamed between
// autoruns — here "NGC 7380" typed once without its space, which strip_target_spaces
// files into the very same folder — starts a fresh ledger scope, and MAX(night)+1 over
// an empty scope is 1. The second spelling's first frame must take the night its own
// destination folder already holds for that observing date.
func TestLivePullAdoptsFolderNightAfterSpellingChange(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
		fileLightAs(t, h, fmt.Sprintf("Autorun/Light/run1/Light_%04d.fit", i+1), "NGC 7380", dateObs)
	}

	renamed := fileLightAs(t, h, "Autorun/Light/run2/Light_0001.fit", "NGC7380", dateObsNight3)
	wantFiledUnder(t, h, renamed, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_3"))
}

// TestLivePullFoldsObjectCase pins what the case-folding exists for: when OBJECT
// arrives as "ic 5146" for a target whose nights are already filed as "IC 5146",
// the next night's subs must continue that target's night count rather than land
// inside an earlier NIGHT_n folder. A case difference is not a rename. The other
// spelling renders "ic5146", which on the case-insensitive filesystems Commander
// archives to IS the "IC5146" folder already there — but unfolded it would fork a
// fresh ledger scope whose MAX(night)+1 is 1, and the byte-exact destination-folder
// read cannot see the frames filed under the other spelling either, so neither the
// ledger nor the folder consultation would catch it.
//
// The frame must take the next night of the one target, filed under the one folder,
// and be recorded under the spelling that folder was created with.
func TestLivePullFoldsObjectCase(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	for i, dateObs := range []string{dateObsNight1, dateObsNight2, dateObsNight3} {
		fileLightAs(t, h, fmt.Sprintf("Autorun/Light/run1/Light_%04d.fit", i+1), "NGC 7380", dateObs)
	}

	lower := fileLightAs(t, h, "Autorun/Light/run2/Light_0001.fit", "ngc 7380", dateObsNight4)
	wantFiledUnder(t, h, lower, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_4"))

	got, ok, err := h.store.GetFrameByID(ctx, lower.ID)
	if err != nil || !ok {
		t.Fatalf("GetFrameByID: (%v,%v)", ok, err)
	}
	if got.Target != "NGC 7380" {
		t.Fatalf("stored target %q; want the spelling the target was first filed under —\n"+
			"two spellings of one target split every grouping that reads the column", got.Target)
	}
}

// TestLivePullFoldsObjectCaseMidNight is the same fault inside a single observing
// date: the case flips between subs of one night, and the second half must join the
// night the first half is already in rather than open a second one.
func TestLivePullFoldsObjectCaseMidNight(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	first := fileLightAs(t, h, "Autorun/Light/run1/Light_0001.fit", "NGC 7380", dateObsNight1)
	wantFiledUnder(t, h, first, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_1"))

	second := fileLightAs(t, h, "Autorun/Light/run1/Light_0002.fit", "ngc 7380", dateObsNight1)
	wantFiledUnder(t, h, second, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_1"))
}

// TestPrunedFolderMintsRatherThanAdopts pins the degradation the design accepts: the
// folder is read out of the frames table, so a night whose rows were pruned is
// invisible. The result must be a MISSED adoption — a mint — never a wrong one, and
// filing must still succeed into a directory that already exists on disk.
func TestPrunedFolderMintsRatherThanAdopts(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	// Three nights of NGC 7380 as they survive a prune: the files are on the NAS, the
	// rows that named them are gone.
	for n := 1; n <= 3; n++ {
		writeLocal(t, filepath.Join(h.dir, "light", "NGC7380", "FRA300", "S",
			fmt.Sprintf("NIGHT_%d", n), "Light_0001.fit"), []byte("pruned"))
	}

	filed := fileLightAs(t, h, "Autorun/Light/run1/Light_0001.fit", "NGC7380", dateObsNight3)
	wantFiledUnder(t, h, filed, filepath.Join("NGC7380", "FRA300", "S", "NIGHT_1"))
}

// TestTemplateWithoutNightSegmentStillFiles covers a light template naming no <n>.
// config.Load does not validate, so such a config can reach routing; there is then no
// night folder to consult, and filing must behave exactly as it does without any
// consultation — an ordinal is still assigned, and nothing errors.
func TestTemplateWithoutNightSegmentStillFiles(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Templates.Light = "<TARGET>/<OTA>/<SESSION>"

	filed := fileLightAs(t, h, "Autorun/Light/run1/Light_0001.fit", "NGC 7380", dateObsNight1)
	wantFiledUnder(t, h, filed, filepath.Join("NGC7380", "FRA300", "S"))
	if filed.Night != 1 {
		t.Fatalf("night=%d, want 1 — the ledger is unchanged by the absence of an <n> segment", filed.Night)
	}
}

// TestPrefixQuarantineFilesUnclassified is the quarantine-discrimination case. A
// routing-critical token in a segment ABOVE <n> cannot be resolved, so the destination
// folder cannot be rendered — but that is a quarantine, not a store failure, and the
// two must not share a path. Treating it as a store failure would return before any row
// was recorded, re-pulling the frame every cycle forever instead of filing it
// unclassified once, loudly and recoverably.
//
// The ordinal is deliberately not spent: the frame quarantines before the ledger is
// touched, unlike a quarantine in a segment below <n>.
func TestPrefixQuarantineFilesUnclassified(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.cfg.Templates.Light = "<SENSOR>/<TARGET>/NIGHT_<n>"
	h.cfg.SensorMap = map[string]string{}

	src := "Autorun/Light/run1/Light_0001.fit"
	h.dev.AddFile(emmc, src, dateObsFixture(t, "NGC 7380", dateObsNight1), h.nowVal.Add(-10*time.Minute))
	h.cycle()

	f, ok := h.getFrame(src)
	if !ok {
		t.Fatal("a prefix quarantine must still record a row; without one the frame is re-pulled every cycle")
	}
	if f.State != model.StateUnclassified {
		t.Fatalf("state=%s, want unclassified", f.State)
	}
	wantDir := filepath.Join(h.dir, "light", "unclassified", "rig-01")
	if !strings.HasPrefix(f.DestPath, wantDir) {
		t.Fatalf("dest=%q not under quarantine %q", f.DestPath, wantDir)
	}
	if n, err := h.store.MaxNight(ctx, "NGC 7380", "FRA300"); err != nil || n != 0 {
		t.Fatalf("MaxNight=%d err=%v; a frame that quarantines on its prefix spends no ordinal", n, err)
	}

	// A second cycle leaves it exactly where it is rather than re-pulling it.
	h.cycle()
	again, _ := h.getFrame(src)
	if again.State != model.StateUnclassified || again.DestPath != f.DestPath || again.PulledAt != f.PulledAt {
		t.Fatalf("second cycle disturbed the quarantined frame: state=%s dest=%q pulled_at=%d",
			again.State, again.DestPath, again.PulledAt)
	}
}
