package discovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testCfg(rigs ...model.RigProfile) *atomic.Pointer[model.Config] {
	p := &atomic.Pointer[model.Config]{}
	p.Store(&model.Config{Rigs: rigs})
	return p
}

// testClock is a mutable injectable clock.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// fakeTransport scripts Broadcast and Probe so the registry runs without real
// sockets, mirroring the device/localfs seam philosophy.
type fakeTransport struct {
	broadcast    []scannedDevice
	broadcastErr error
	probeReply   scannedDevice
	probeOK      bool
	probeCalls   int
	probedEPs    []netsite.Endpoint
}

func (f *fakeTransport) Broadcast(context.Context) ([]scannedDevice, error) {
	return f.broadcast, f.broadcastErr
}

func (f *fakeTransport) Probe(_ context.Context, ep netsite.Endpoint, _ []byte) (scannedDevice, bool) {
	f.probeCalls++
	f.probedEPs = append(f.probedEPs, ep)
	return f.probeReply, f.probeOK
}

// Site is the local site, whose costs leave every timeout at its constant.
func (f *fakeTransport) Site(netsite.ID) netsite.Site {
	return netsite.NewRegistry(nil).Site(netsite.Local)
}

// fakeFactory hands back the fake device registered for an address, or an
// unreachable one for anything else — used only by DeviceShares.
func fakeFactory(byAddr map[string]*device.Fake) func(ep netsite.Endpoint, user, pass string, timeout time.Duration) device.Device {
	return func(ep netsite.Endpoint, _, _ string, _ time.Duration) device.Device {
		if f, ok := byAddr[ep.String()]; ok {
			return f
		}
		unreachable := device.NewFake()
		unreachable.SetReachable(false)
		unreachable.FailSessionOpen(device.NetworkError("unreachable"))
		return unreachable
	}
}

// --- protocol parsing --------------------------------------------------------

func TestParseReply(t *testing.T) {
	const good = `{"jsonrpc":"2.0","method":"scan_air","result":{"guid":"0a1b2c3d","cpuId":"0011223344556677","ip":"192.168.1.75","is_pi4":true,"model":"ZWO AirPlus-RK3568 (Linux)","ssid":"ASIAIR_0a1b2c3d","connect_lock":false},"code":0,"id":"132"}`

	d, ok := parseReply([]byte(good), "10.0.0.9")
	if !ok {
		t.Fatal("parseReply(good) ok=false, want true")
	}
	if d.GUID != "0a1b2c3d" || d.CPUID != "0011223344556677" || d.Address != "192.168.1.75" ||
		d.Model != "ZWO AirPlus-RK3568 (Linux)" || d.SSID != "ASIAIR_0a1b2c3d" {
		t.Fatalf("parseReply(good) = %+v", d)
	}

	// An owner-renamed ssid is the field that tells two same-model devices apart,
	// so it survives verbatim rather than being normalised.
	renamed := `{"method":"scan_air","result":{"guid":"5eed0001","model":"Raspberry Pi Compute Module 4 Rev 1.0","ssid":"MyRig"},"code":0}`
	if d, ok := parseReply([]byte(renamed), "10.0.0.7"); !ok || d.SSID != "MyRig" {
		t.Fatalf("parseReply(renamed) = %+v ok=%v, want SSID=MyRig", d, ok)
	}

	// A reply with no ssid at all still parses — the field is absent on some
	// firmware, and the UI falls back to the model-plus-guid label.
	noSSID := `{"method":"scan_air","result":{"guid":"g2","model":"ZWO AirPlus-RK3568 (Linux)"},"code":0}`
	if d, ok := parseReply([]byte(noSSID), "10.0.0.8"); !ok || d.SSID != "" {
		t.Fatalf("parseReply(noSSID) = %+v ok=%v, want SSID empty and ok=true", d, ok)
	}

	// result.ip blank falls back to the datagram source.
	noIP := `{"method":"scan_air","result":{"guid":"g1"},"code":0}`
	if d, ok := parseReply([]byte(noIP), "10.0.0.42"); !ok || d.Address != "10.0.0.42" {
		t.Fatalf("parseReply(noIP) = %+v ok=%v, want Address=10.0.0.42", d, ok)
	}

	for name, raw := range map[string]string{
		"empty guid":   `{"method":"scan_air","result":{"guid":""},"code":0}`,
		"wrong method": `{"method":"other","result":{"guid":"g"},"code":0}`,
		"nonzero code": `{"method":"scan_air","result":{"guid":"g"},"code":1}`,
		"garbage":      `not json`,
	} {
		if _, ok := parseReply([]byte(raw), "10.0.0.1"); ok {
			t.Fatalf("parseReply(%s) ok=true, want dropped", name)
		}
	}
}

// TestParseScanAirReply: the exported reader a remote uses applies parseReply's
// rules, the source fallback included.
func TestParseScanAirReply(t *testing.T) {
	id, ok := ParseScanAirReply([]byte(`{"method":"scan_air","result":{"guid":"g1","cpuId":"c1","ip":"10.0.0.5"},"code":0}`), "10.0.0.6")
	if !ok || id != (ScanAirIdentity{GUID: "g1", CPUID: "c1", Address: "10.0.0.5"}) {
		t.Fatalf("ParseScanAirReply = %+v ok=%v", id, ok)
	}
	if id, ok := ParseScanAirReply([]byte(`{"method":"scan_air","result":{"guid":"g1"},"code":0}`), "10.0.0.6"); !ok || id.Address != "10.0.0.6" {
		t.Fatalf("ParseScanAirReply(no ip) = %+v ok=%v, want the source address", id, ok)
	}
	if _, ok := ParseScanAirReply([]byte(`{"method":"scan_air","result":{"guid":""},"code":0}`), "10.0.0.6"); ok {
		t.Fatal("ParseScanAirReply accepted a reply discovery drops")
	}
}

// --- merge / registry --------------------------------------------------------

func TestScanMergesByGUIDAndIndexesCPU(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{
		{GUID: "g1", CPUID: "cpu1", Model: "M", Address: "10.0.0.1"},
		{GUID: "g2", CPUID: "", Model: "M", Address: "10.0.0.2"}, // empty cpuId not indexed
	}}
	s := New(tr, nil, testCfg(), clk.now, testLogger())
	s.Scan(context.Background())

	if got := len(s.Discovered()); got != 2 {
		t.Fatalf("Discovered() = %d, want 2", got)
	}
	s.mu.Lock()
	_, hasCPU1 := s.byCPU["cpu1"]
	_, hasEmpty := s.byCPU[""]
	s.mu.Unlock()
	if !hasCPU1 {
		t.Fatal("cpu1 should be indexed")
	}
	if hasEmpty {
		t.Fatal("empty cpuId must not be indexed")
	}

	// A later round updates the address and LastSeen of a known guid.
	clk.advance(10 * time.Second)
	tr.broadcast = []scannedDevice{{GUID: "g1", CPUID: "cpu1", Address: "10.0.0.9"}}
	s.Scan(context.Background())
	if addr, online := s.Resolve("g1", "cpu1"); addr != netsite.LocalEndpoint("10.0.0.9") || !online {
		t.Fatalf("Resolve(g1) = %q online=%v, want 10.0.0.9 online", addr, online)
	}
}

func TestResolveByGUIDAndCPUFallback(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{}
	s := New(tr, nil, testCfg(), clk.now, testLogger())

	// Seed the old guid at an address, then let it go offline while the same
	// device reappears under a new guid but the same cpuId.
	tr.broadcast = []scannedDevice{{GUID: "old", CPUID: "cpu1", Address: "10.0.0.1"}}
	s.Scan(context.Background())

	clk.advance(stalenessWindow + time.Second) // old goes offline
	tr.broadcast = []scannedDevice{{GUID: "new", CPUID: "cpu1", Address: "10.0.0.5"}}
	s.Scan(context.Background())

	// Resolving the OLD guid with its cpuId falls back to the new fresh entry.
	if addr, online := s.Resolve("old", "cpu1"); addr != netsite.LocalEndpoint("10.0.0.5") || !online {
		t.Fatalf("Resolve(old,cpu1) = %q online=%v, want 10.0.0.5 online (cpu fallback)", addr, online)
	}
	// With no cpuId the fallback is skipped: the stale guid resolves offline.
	if addr, online := s.Resolve("old", ""); online {
		t.Fatalf("Resolve(old,\"\") online=%v addr=%q, want offline (no cpu fallback)", online, addr)
	}
	// A total miss.
	if addr, online := s.Resolve("nope", ""); addr != (netsite.Endpoint{}) || online {
		t.Fatalf("Resolve(nope) = %q online=%v, want miss", addr, online)
	}
}

func TestStalenessMarksOffline(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{{GUID: "g1", Address: "10.0.0.1"}}}
	s := New(tr, nil, testCfg(), clk.now, testLogger())
	s.Scan(context.Background())

	if _, online := s.Resolve("g1", ""); !online {
		t.Fatal("fresh entry should be online")
	}
	clk.advance(stalenessWindow + time.Second)
	if _, online := s.Resolve("g1", ""); online {
		t.Fatal("entry past stalenessWindow should be offline")
	}
	if d := s.Discovered(); len(d) != 1 || d[0].Online {
		t.Fatalf("Discovered() = %+v, want one offline entry", d)
	}
}

func TestForgetWindowEvictsUnmanagedButKeepsManaged(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{
		{GUID: "managed", CPUID: "mc", Address: "10.0.0.1"},
		{GUID: "loose", CPUID: "lc", Address: "10.0.0.2"},
	}}
	cfg := testCfg(model.RigProfile{ID: "r1", Managed: true, GUID: "managed", CPUID: "mc"})
	s := New(tr, nil, cfg, clk.now, testLogger())
	s.Scan(context.Background())

	// Advance past forgetWindow and scan an empty round: the loose device is
	// evicted, the managed one is exempt.
	clk.advance(forgetWindow + time.Second)
	tr.broadcast = nil
	s.Scan(context.Background())

	d := s.Discovered()
	if len(d) != 1 || d[0].GUID != "managed" {
		t.Fatalf("Discovered() = %+v, want only the managed guid retained", d)
	}
}

func TestMaxEntriesCapExemptsManaged(t *testing.T) {
	clk := newClock()
	round := make([]scannedDevice, 0, maxEntries+6)
	round = append(round, scannedDevice{GUID: "managed", CPUID: "mc", Address: "10.9.9.9"})
	for i := 0; i < maxEntries+5; i++ {
		round = append(round, scannedDevice{GUID: fmt.Sprintf("loose-%d", i), Address: fmt.Sprintf("10.0.%d.%d", i/256, i%256)})
	}
	tr := &fakeTransport{broadcast: round}
	cfg := testCfg(model.RigProfile{ID: "r1", Managed: true, GUID: "managed"})
	s := New(tr, nil, cfg, clk.now, testLogger())
	s.Scan(context.Background())

	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	if n > maxEntries {
		t.Fatalf("registry holds %d entries, want <= %d", n, maxEntries)
	}
	if addr, _ := s.Resolve("managed", ""); addr != netsite.LocalEndpoint("10.9.9.9") {
		t.Fatalf("managed guid was cap-dropped: Resolve = %q", addr)
	}
}

func TestDiscoveredReportsManagedAndOnline(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{
		{GUID: "bound", CPUID: "c1", Model: "AirPlus", Address: "10.0.0.1", SSID: "Backyard"},
		{GUID: "free", Model: "AirMini", Address: "10.0.0.2"},
	}}
	cfg := testCfg(model.RigProfile{ID: "rig-01", Managed: true, GUID: "bound", CPUID: "c1"})
	s := New(tr, nil, cfg, clk.now, testLogger())
	s.Scan(context.Background())

	byGUID := map[string]model.DiscoveredRig{}
	for _, d := range s.Discovered() {
		byGUID[d.GUID] = d
	}
	if b := byGUID["bound"]; !b.Managed || b.RigID != "rig-01" || !b.Online {
		t.Fatalf("bound = %+v, want Managed RigID=rig-01 Online", b)
	}
	if f := byGUID["free"]; f.Managed || f.RigID != "" {
		t.Fatalf("free = %+v, want unmanaged", f)
	}
	// The ssid reaches the UI: without it an adopt dialog can only offer the
	// model string, which is identical across devices of the same generation.
	if b, f := byGUID["bound"], byGUID["free"]; b.SSID != "Backyard" || f.SSID != "" {
		t.Fatalf("ssid not carried: bound=%q free=%q, want %q and empty", b.SSID, f.SSID, "Backyard")
	}
}

func TestProbeGUID(t *testing.T) {
	tr := &fakeTransport{probeReply: scannedDevice{GUID: "g1", CPUID: "c1"}, probeOK: true}
	s := New(tr, nil, testCfg(), newClock().now, testLogger())

	guid, cpu, ok := s.ProbeGUID(context.Background(), netsite.LocalEndpoint("10.0.0.1"))
	if !ok || guid != "g1" || cpu != "c1" {
		t.Fatalf("ProbeGUID = %q,%q,%v want g1,c1,true", guid, cpu, ok)
	}
	// The endpoint reaches the transport whole, site included, so the transport
	// can route the probe by it.
	remote := netsite.Endpoint{Site: "r1", IP: "10.0.0.1"}
	s.ProbeGUID(context.Background(), remote)
	if got := tr.probedEPs[len(tr.probedEPs)-1]; got != remote {
		t.Fatalf("transport probed %+v, want %+v", got, remote)
	}

	tr.probeOK = false
	if _, _, ok := s.ProbeGUID(context.Background(), netsite.LocalEndpoint("10.0.0.1")); ok {
		t.Fatal("ProbeGUID ok=true on no reply, want false")
	}
}

func TestDeviceShares(t *testing.T) {
	clk := newClock()
	fake := device.NewFake()
	fake.SetReachable(true)
	fake.AddShare("EMMC Images")
	fake.AddShare("Udisk Images")

	tr := &fakeTransport{broadcast: []scannedDevice{{GUID: "g1", Address: "10.0.0.5"}}}
	s := New(tr, fakeFactory(map[string]*device.Fake{"10.0.0.5": fake}), testCfg(), clk.now, testLogger())
	s.Scan(context.Background())

	shares, err := s.DeviceShares(context.Background(), "g1")
	if err != nil {
		t.Fatalf("DeviceShares: %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("DeviceShares = %v, want 2 shares", shares)
	}

	// An unresolved guid errors rather than opening SMB.
	if _, err := s.DeviceShares(context.Background(), "unknown"); err == nil {
		t.Fatal("DeviceShares(unknown) err=nil, want an error")
	}
}

// TestStaleIdentityMarkerStillAdoptable: a device carrying a leftover foreign
// rig-identity marker file is judged by its guid alone (matchManaged) — an
// unbound guid is unmanaged, so the device stays adoptable. Identity does not
// key on the marker.
func TestStaleIdentityMarkerStillAdoptable(t *testing.T) {
	clk := newClock()
	fake := device.NewFake()
	fake.SetReachable(true)
	fake.AddShare("EMMC Images")
	fake.SetMarker("EMMC Images", model.IdentityMarkerName, []byte("some-other-rig 11111111-1111-1111-1111-111111111111\n"))

	tr := &fakeTransport{broadcast: []scannedDevice{{GUID: "fresh", Address: "10.0.0.6"}}}
	// The config binds a DIFFERENT guid, so "fresh" is unbound → unmanaged.
	cfg := testCfg(model.RigProfile{ID: "r1", Managed: true, GUID: "someone-elses-guid"})
	s := New(tr, fakeFactory(map[string]*device.Fake{"10.0.0.6": fake}), cfg, clk.now, testLogger())
	s.Scan(context.Background())

	var found bool
	for _, d := range s.Discovered() {
		if d.GUID == "fresh" {
			found = true
			if d.Managed || d.RigID != "" {
				t.Fatalf("a device with an unbound guid must be adoptable (unmanaged), got %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("expected the fresh device in the discovered list")
	}
}

// --- on-demand scan logging --------------------------------------------------

// captureLogger returns a logger writing into buf, so a test can read what a
// scan reported.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// TestScanOnDemandLogsWhatAnsweredAndWhatDidNot covers the question an operator
// presses Scan to answer: which devices are really out there, and what is that
// unexpected one? The raw reply is logged because the fields the registry drops
// (is_pi4, connect_lock) are often what identifies it.
func TestScanOnDemandLogsWhatAnsweredAndWhatDidNot(t *testing.T) {
	clk := newClock()
	raw := `{"jsonrpc":"2.0","method":"scan_air","result":{"guid":"g1","ssid":"BACKYARD"},"code":0,"id":1}`
	tr := &fakeTransport{broadcast: []scannedDevice{
		{GUID: "g1", CPUID: "cpu1", Model: "AirPlus", Address: "10.0.0.1", Raw: raw},
	}}
	cfg := testCfg(model.RigProfile{ID: "rig-1", Managed: true, GUID: "gone"})
	s := New(tr, nil, cfg, clk.now, testLogger())

	// An earlier round saw a second device; it is managed, so it stays in the
	// registry after it stops answering.
	tr.broadcast = append(tr.broadcast, scannedDevice{GUID: "gone", Address: "10.0.0.2"})
	s.Scan(context.Background())
	tr.broadcast = tr.broadcast[:1]
	clk.advance(stalenessWindow + time.Second)

	var buf bytes.Buffer
	s.logger = captureLogger(&buf)
	s.ScanOnDemand(context.Background())
	out := buf.String()

	for _, want := range []string{
		"device answered", "guid=g1", "address=10.0.0.1", "BACKYARD",
		"did not answer", "guid=gone", "managed=true", "rig=rig-1",
		"answered=1", "listed=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("on-demand scan log missing %q\nlog:\n%s", want, out)
		}
	}
}

// TestBackgroundScanStaysQuiet keeps the 45-second timer off the info log: the
// same lines every round would bury everything else.
func TestBackgroundScanStaysQuiet(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{{GUID: "g1", Address: "10.0.0.1", Raw: "{}"}}}
	var buf bytes.Buffer
	s := New(tr, nil, testCfg(), clk.now, captureLogger(&buf))
	s.Scan(context.Background())
	if out := buf.String(); out != "" {
		t.Errorf("background scan logged at info level:\n%s", out)
	}
}

// --- waking the rigs whose devices answered ----------------------------------

// recordingWaker captures the rig ids a scan round woke.
type recordingWaker struct {
	mu   sync.Mutex
	woke []string
}

func (w *recordingWaker) WakeRig(rigID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woke = append(w.woke, rigID)
}

func (w *recordingWaker) ids() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.woke...)
}

// A device answering is the earliest evidence the network has that a rig is
// back; without a wake, a worker that found its rig unreachable would wait out
// its whole retry interval while the device answered every round. The
// registry hands the answering rig's id to the waker — by guid or by the cpuId
// fallback, once per rig per round, and only for devices a profile binds.
func TestScanWakesTheRigsWhoseDevicesAnswered(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{
		{GUID: "bound", CPUID: "c1", Address: "10.0.0.1"},
		{GUID: "rebadged", CPUID: "c2", Address: "10.0.0.2"}, // guid changed; cpuId still binds
		{GUID: "stranger", CPUID: "c9", Address: "10.0.0.3"}, // nobody's rig
	}}
	cfg := testCfg(
		model.RigProfile{ID: "rig-01", Managed: true, GUID: "bound", CPUID: "c1"},
		model.RigProfile{ID: "rig-02", Managed: true, GUID: "was-something-else", CPUID: "c2"},
	)
	s := New(tr, nil, cfg, clk.now, testLogger())
	w := &recordingWaker{}
	s.SetWaker(w)

	s.Scan(context.Background())

	got := w.ids()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "rig-01" || got[1] != "rig-02" {
		t.Fatalf("woken rigs = %v, want [rig-01 rig-02]", got)
	}
}

// A device that stays silent must not wake its rig, however long the registry
// retains it: a managed device is retained indefinitely, so waking on the listed
// set rather than the round's replies would probe a powered-off rig every 45
// seconds forever.
func TestARetainedButSilentDeviceWakesNothing(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{{GUID: "bound", CPUID: "c1", Address: "10.0.0.1"}}}
	cfg := testCfg(model.RigProfile{ID: "rig-01", Managed: true, GUID: "bound", CPUID: "c1"})
	s := New(tr, nil, cfg, clk.now, testLogger())
	w := &recordingWaker{}
	s.SetWaker(w)
	s.Scan(context.Background()) // it answers once

	tr.broadcast = nil
	clk.advance(stalenessWindow + time.Second)
	s.Scan(context.Background())
	s.ScanOnDemand(context.Background())

	if got := w.ids(); len(got) != 1 {
		t.Fatalf("woken rigs = %v, want only the round the device answered", got)
	}
	if listed := s.Discovered(); len(listed) != 1 || listed[0].Online {
		t.Fatalf("precondition: the silent device should still be listed as offline, got %+v", listed)
	}
}

// The waker is optional: a Scanner built before the manager exists (the
// composition root's warm-up round) must scan normally with none installed.
func TestScanWithoutAWakerIsHarmless(t *testing.T) {
	clk := newClock()
	tr := &fakeTransport{broadcast: []scannedDevice{{GUID: "bound", Address: "10.0.0.1"}}}
	cfg := testCfg(model.RigProfile{ID: "rig-01", Managed: true, GUID: "bound"})
	s := New(tr, nil, cfg, clk.now, testLogger())

	if got := s.Scan(context.Background()); len(got) != 1 {
		t.Fatalf("Discovered() = %+v, want the one device", got)
	}
	s.SetWaker(&recordingWaker{})
	s.SetWaker(nil) // clearing it is equally harmless
	if got := s.Scan(context.Background()); len(got) != 1 {
		t.Fatalf("Discovered() after clearing the waker = %+v, want the one device", got)
	}
}
