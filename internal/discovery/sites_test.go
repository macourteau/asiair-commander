package discovery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// testSites is a siteSet over fakes, so the local site can be a Fake too and
// no test opens a real broadcast socket.
type testSites []netsite.Site

func (ts testSites) All() []netsite.Site { return ts }

func (ts testSites) Site(id netsite.ID) netsite.Site {
	for _, s := range ts {
		if s.ID() == id {
			return s
		}
	}
	return nil
}

// localFake is a Fake standing in for the local site: id Local, and replies
// taken as the device reports them.
func localFake() *netsite.Fake {
	f := netsite.NewFake(netsite.Local)
	f.SetRepliesMatchSource(false)
	return f
}

func dg(raw, src string) netsite.Datagram { return netsite.Datagram{Data: []byte(raw), SrcIP: src} }

func reply(guid, ip string) string {
	if ip == "" {
		return `{"method":"scan_air","result":{"guid":"` + guid + `"},"code":0}`
	}
	return `{"method":"scan_air","result":{"guid":"` + guid + `","ip":"` + ip + `"},"code":0}`
}

func byGUID(found []model.DiscoveredRig) map[string]model.DiscoveredRig {
	out := map[string]model.DiscoveredRig{}
	for _, d := range found {
		out[d.GUID] = d
	}
	return out
}

// TestScannerFansOutAndTagsSites: one scan runs a broadcast round on every up
// site, tags each reply with the site it came through, skips a site that is
// not up, and applies the result.ip rule only where the site asks for it.
func TestScannerFansOutAndTagsSites(t *testing.T) {
	// The same reply — result.ip set and differing from its source — arrives on
	// the local site and on r1.
	mismatch := dg(reply("M", "10.9.9.9"), "192.168.1.20")

	local := localFake()
	local.SetBroadcast([]netsite.Datagram{dg(reply("L", "192.168.1.10"), "192.168.1.10"), mismatch}, nil)
	r1 := netsite.NewFake("r1")
	r1.SetBroadcast([]netsite.Datagram{
		dg(reply("A", ""), "192.168.7.20"), // no result.ip: falls back to the source
		mismatch,
	}, nil)
	r2 := netsite.NewFake("r2")
	r2.SetBroadcast([]netsite.Datagram{dg(reply("B", "192.168.8.5"), "192.168.8.5")}, nil)
	down := netsite.NewFake("down")
	down.SetState(netsite.StateDown)
	down.SetBroadcast([]netsite.Datagram{dg(reply("C", "192.168.9.9"), "192.168.9.9")}, nil)
	failing := netsite.NewFake("failing")
	failing.SetBroadcast([]netsite.Datagram{dg(reply("F", "192.168.6.6"), "192.168.6.6")}, errors.New("round failed"))

	clk := newClock()
	tr := newUDPTransport(testSites{local, r1, r2, down, failing}, testLogger())
	s := New(tr, nil, testCfg(), clk.now, testLogger())
	found := byGUID(s.Scan(context.Background()))

	want := map[string]struct{ site, addr string }{
		"L": {"", "192.168.1.10"},
		"M": {"", "10.9.9.9"}, // kept on the local site
		"A": {"r1", "192.168.7.20"},
		"B": {"r2", "192.168.8.5"},
	}
	if len(found) != len(want) {
		t.Fatalf("Discovered = %+v, want exactly %v", found, want)
	}
	for guid, w := range want {
		d, ok := found[guid]
		if !ok || d.Site != w.site || d.Address != w.addr || !d.Online {
			t.Fatalf("Discovered[%s] = %+v ok=%v, want site %q address %q online", guid, d, ok, w.site, w.addr)
		}
		ep, online := s.Resolve(guid, "")
		if ep != (netsite.Endpoint{Site: netsite.ID(w.site), IP: w.addr}) || !online {
			t.Fatalf("Resolve(%s) = %+v online=%v, want %s..%s online", guid, ep, online, w.site, w.addr)
		}
	}

	// The mismatching reply on r1 was dropped, not merged as a second path.
	s.mu.Lock()
	_, onR1 := s.entries["M"].paths["r1"]
	s.mu.Unlock()
	if onR1 {
		t.Fatal("reply with result.ip != source on r1 was merged, want dropped")
	}
	// A site that is not up is never asked to broadcast.
	if calls := down.Calls(); len(calls) != 0 {
		t.Fatalf("down site calls = %v, want none", calls)
	}
	for _, f := range []*netsite.Fake{local, r1, r2, failing} {
		if calls := f.Calls(); len(calls) != 1 || calls[0] != "broadcast" {
			t.Fatalf("site %q calls = %v, want one broadcast", f.ID(), calls)
		}
	}
}

// TestPathPrecedenceHysteresis: a device seen on two paths keeps its fresh
// current path against a slightly lower latency or a transient spike, moves on
// a clearly lower minimum latency or when its path goes stale, and goes local
// at its first local sighting. Each switch is logged once.
func TestPathPrecedenceHysteresis(t *testing.T) {
	local := localFake()
	r1 := netsite.NewFake("r1")
	r1.SetMinLatency(100 * time.Millisecond)
	r1.SetLatency(100 * time.Millisecond)
	r1.SetBroadcast([]netsite.Datagram{dg(reply("g", ""), "192.168.7.20")}, nil)
	r2 := netsite.NewFake("r2")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	clk := newClock()
	s := New(newUDPTransport(testSites{local, r1, r2}, logger), nil, testCfg(), clk.now, logger)
	ctx := context.Background()

	wantPath := func(step string, site netsite.ID, ip string) {
		t.Helper()
		ep, online := s.Resolve("g", "")
		if ep != (netsite.Endpoint{Site: site, IP: ip}) || !online {
			t.Fatalf("%s: Resolve = %+v online=%v, want %s..%s online", step, ep, online, site, ip)
		}
	}
	switches := func() int { return strings.Count(logBuf.String(), "device path switched") }

	s.Scan(ctx)
	wantPath("first sighting", "r1", "192.168.7.20")

	// r2 is slightly lower, and r1's smoothed latency spikes while its minimum
	// holds: neither moves the device.
	r2.SetMinLatency(60 * time.Millisecond)
	r2.SetLatency(60 * time.Millisecond)
	r2.SetBroadcast([]netsite.Datagram{dg(reply("g", ""), "192.168.8.20")}, nil)
	r1.SetLatency(900 * time.Millisecond)
	clk.advance(45 * time.Second)
	s.Scan(ctx)
	wantPath("slightly lower / spike", "r1", "192.168.7.20")

	// r2's minimum drops clearly below r1's: the device moves.
	r2.SetMinLatency(40 * time.Millisecond)
	clk.advance(45 * time.Second)
	s.Scan(ctx)
	wantPath("clearly lower", "r2", "192.168.8.20")
	if n := switches(); n != 1 {
		t.Fatalf("path switch logged %d times, want 1", n)
	}
	s.Scan(ctx)
	if n := switches(); n != 1 {
		t.Fatalf("a scan with no switch logged one: %d switch lines, want 1", n)
	}

	// r2 stops answering; while it is fresh the device stays, and once it goes
	// stale the device returns to r1 without waiting for another round.
	r2.SetBroadcast(nil, nil)
	clk.advance(stalenessWindow - time.Second)
	s.Scan(ctx)
	wantPath("current still fresh", "r2", "192.168.8.20")
	clk.advance(2 * time.Second)
	wantPath("current stale", "r1", "192.168.7.20")

	// A low-latency remote still loses to the local site at its next sighting.
	r1.SetMinLatency(3 * time.Millisecond)
	clk.advance(45 * time.Second)
	s.Scan(ctx)
	wantPath("remote at 3ms", "r1", "192.168.7.20")
	local.SetBroadcast([]netsite.Datagram{dg(reply("g", ""), "192.168.1.20")}, nil)
	clk.advance(45 * time.Second)
	s.Scan(ctx)
	wantPath("brought home", netsite.Local, "192.168.1.20")
	if d := byGUID(s.Discovered())["g"]; d.Site != "" || d.Address != "192.168.1.20" {
		t.Fatalf("Discovered[g] = %+v, want the local path", d)
	}
	if n := switches(); n != 3 {
		t.Fatalf("path switch logged %d times, want 3", n)
	}
}

// TestDropSiteRemovesSightings: dropping a site removes every sighting made
// through it; a device seen only there leaves the registry, managed or not, and
// one also seen elsewhere moves to that path.
func TestDropSiteRemovesSightings(t *testing.T) {
	local := localFake()
	local.SetBroadcast([]netsite.Datagram{dg(reply("home", ""), "192.168.1.5")}, nil)
	r1 := netsite.NewFake("r1")
	r1.SetMinLatency(20 * time.Millisecond)
	r1.SetBroadcast([]netsite.Datagram{
		dg(reply("home", ""), "192.168.7.5"),
		dg(reply("only", ""), "192.168.7.6"),
		dg(reply("both", ""), "192.168.7.7"),
	}, nil)
	r2 := netsite.NewFake("r2")
	r2.SetMinLatency(30 * time.Millisecond)
	r2.SetBroadcast([]netsite.Datagram{dg(reply("both", ""), "192.168.8.7")}, nil)

	clk := newClock()
	cfg := testCfg(model.RigProfile{ID: "rig-only", GUID: "only", Managed: true})
	s := New(newUDPTransport(testSites{local, r1, r2}, testLogger()), nil, cfg, clk.now, testLogger())
	s.Scan(context.Background())
	if ep, _ := s.Resolve("both", ""); ep.Site != "r1" {
		t.Fatalf("before drop: Resolve(both) = %+v, want r1 (first seen, not clearly slower)", ep)
	}

	s.DropSite("r1")

	found := byGUID(s.Discovered())
	if _, ok := found["only"]; ok {
		t.Fatalf("device seen only through the dropped site is still listed: %+v", found["only"])
	}
	if ep, online := s.Resolve("only", ""); ep != (netsite.Endpoint{}) || online {
		t.Fatalf("Resolve(only) = %+v online=%v, want a miss", ep, online)
	}
	if ep, online := s.Resolve("both", ""); ep != (netsite.Endpoint{Site: "r2", IP: "192.168.8.7"}) || !online {
		t.Fatalf("Resolve(both) = %+v online=%v, want r2..192.168.8.7", ep, online)
	}
	if ep, online := s.Resolve("home", ""); ep != netsite.LocalEndpoint("192.168.1.5") || !online {
		t.Fatalf("Resolve(home) = %+v online=%v, want the local path untouched", ep, online)
	}
	s.mu.Lock()
	_, stale := s.entries["home"].paths["r1"]
	s.mu.Unlock()
	if stale {
		t.Fatal("home still holds an r1 sighting after DropSite(r1)")
	}
}

// TestDeviceSharesThroughSite: a remote-found, unadopted device has its shares
// listed through an endpoint naming its site, never a bare IP.
func TestDeviceSharesThroughSite(t *testing.T) {
	r1 := netsite.NewFake("r1")
	r1.SetBroadcast([]netsite.Datagram{dg(reply("g1", ""), "192.168.7.20")}, nil)
	fake := device.NewFake()
	fake.SetReachable(true)
	fake.AddShare("EMMC Images")

	clk := newClock()
	factory := fakeFactory(map[string]*device.Fake{"r1..192.168.7.20": fake})
	s := New(newUDPTransport(testSites{localFake(), r1}, testLogger()), factory, testCfg(), clk.now, testLogger())
	s.Scan(context.Background())

	shares, err := s.DeviceShares(context.Background(), "g1")
	if err != nil || len(shares) != 1 {
		t.Fatalf("DeviceShares(g1) = %v, %v; want one share listed through r1", shares, err)
	}
}

// TestConcurrentScanListAndDrop: scans, listings, lookups and site drops run
// concurrently over devices seen on two sites, as the background loop, the API
// and a remote's removal do; -race catches any registry state read after the
// lock is released.
func TestConcurrentScanListAndDrop(t *testing.T) {
	local := localFake()
	local.SetBroadcast([]netsite.Datagram{dg(reply("g", ""), "192.168.1.5")}, nil)
	r1 := netsite.NewFake("r1")
	r1.SetMinLatency(20 * time.Millisecond)
	r1.SetBroadcast([]netsite.Datagram{dg(reply("g", ""), "192.168.7.5"), dg(reply("h", ""), "192.168.7.6")}, nil)

	s := New(newUDPTransport(testSites{local, r1}, testLogger()), nil, testCfg(), newClock().now, testLogger())
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, work := range []func(){
		func() { s.Scan(ctx) },
		func() { s.Scan(ctx) },
		func() { _ = s.Discovered() },
		func() { s.Resolve("g", "") },
		func() { s.DropSite("r1") },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				work()
			}
		}()
	}
	wg.Wait()
}
