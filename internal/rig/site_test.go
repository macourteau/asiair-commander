package rig

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/discovery"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestLastKnownEndpointRoundTrip: recordLastKnownIP stores the endpoint's string
// form and resolveAddress reads it back to the same site and IP; a legacy bare
// value names the local site; a value that does not parse is never dialled.
func TestLastKnownEndpointRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "g1"
	h.profile.Host = ""
	h.resolver.resolveOnline = false // the registry misses, so lastip decides

	remote := netsite.Endpoint{Site: "r1", IP: "10.0.0.9"}
	h.w.recordLastKnownIP(ctx, remote)
	if v := h.meta(LastIPKey("rig-01")); v != "r1..10.0.0.9" {
		t.Fatalf("stored lastip = %q, want the remote form r1..10.0.0.9", v)
	}
	if ep, ok := h.w.resolveAddress(ctx, h.profile); !ok || ep != remote {
		t.Fatalf("resolve = %+v ok=%v, want %+v", ep, ok, remote)
	}

	local := netsite.LocalEndpoint("10.0.0.9")
	h.w.recordLastKnownIP(ctx, local)
	if v := h.meta(LastIPKey("rig-01")); v != "10.0.0.9" {
		t.Fatalf("stored lastip = %q, want the bare address", v)
	}
	if ep, ok := h.w.resolveAddress(ctx, h.profile); !ok || ep != local {
		t.Fatalf("resolve = %+v ok=%v, want %+v", ep, ok, local)
	}

	// Values written before sites existed keep their meaning with no migration.
	for _, legacy := range []string{"192.168.1.75", "asiair.local"} {
		if err := h.store.SetMeta(ctx, LastIPKey("rig-01"), legacy); err != nil {
			t.Fatalf("seed lastip: %v", err)
		}
		if ep, ok := h.w.resolveAddress(ctx, h.profile); !ok || ep != netsite.LocalEndpoint(legacy) {
			t.Fatalf("legacy %q resolves to %+v ok=%v, want the local site", legacy, ep, ok)
		}
	}

	// An unreadable value is skipped, not guessed onto a network.
	if err := h.store.SetMeta(ctx, LastIPKey("rig-01"), "r.1..10.0.0.9"); err != nil {
		t.Fatalf("seed lastip: %v", err)
	}
	if ep, ok := h.w.resolveAddress(ctx, h.profile); ok {
		t.Fatalf("unparsable lastip resolved to %+v, want offline", ep)
	}
	h.profile.Host = "10.0.0.50"
	if ep, ok := h.w.resolveAddress(ctx, h.profile); !ok || ep != netsite.LocalEndpoint("10.0.0.50") {
		t.Fatalf("unparsable lastip with a Host resolves to %+v ok=%v, want the local Host", ep, ok)
	}
}

// eventLog is one ordered record shared by the device and the site, so a test
// can see which happened first.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// orderedDevice records when the worker opens its SMB session.
type orderedDevice struct {
	device.Device
	log *eventLog
}

func (d orderedDevice) Open(ctx context.Context) (device.Session, error) {
	d.log.add("open")
	return d.Device.Open(ctx)
}

// sessionSiteHarness wires a worker whose identity probe runs through the real
// discovery scanner and site registry, with a fake site standing in for a
// network other than the local one. The rig's last-known endpoint names that
// site, so every session and probe of the cycle must go through it.
func sessionSiteHarness(t *testing.T) (*harness, *netsite.Fake, *eventLog, *[]netsite.Endpoint) {
	t.Helper()
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.profile.Host = ""

	log := &eventLog{}
	sites := netsite.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	site := netsite.NewFake("r1")
	site.SetProbeReply([]byte(`{"method":"scan_air","result":{"guid":"dev-guid"},"code":0}`), "192.168.7.20")
	site.OnCall(log.add)
	if err := sites.Add(site); err != nil {
		t.Fatalf("add site: %v", err)
	}
	h.w.resolver = discovery.NewDefault(sites, nil, h.cfgPtr, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var built []netsite.Endpoint
	h.w.newDevice = func(ep netsite.Endpoint, _, _ string, _ time.Duration) device.Device {
		built = append(built, ep)
		return orderedDevice{Device: h.dev, log: log}
	}
	if err := h.store.SetMeta(context.Background(), LastIPKey("rig-01"), "r1..192.168.7.20"); err != nil {
		t.Fatalf("seed lastip: %v", err)
	}
	return h, site, log, &built
}

// checkProbesFollowOpen fails unless the log holds at least one probe, every
// probe went to the session's address through the session's site, and none
// was sent before a session was open (design remote-sites §12 invariant 2).
func checkProbesFollowOpen(t *testing.T, events []string) {
	t.Helper()
	opened, probes := false, 0
	for _, e := range events {
		switch {
		case e == "open":
			opened = true
		case strings.HasPrefix(e, "probe "):
			probes++
			if !opened {
				t.Fatalf("probe sent before any session was open: %v", events)
			}
			if e != "probe 192.168.7.20:4720" {
				t.Fatalf("probe %q, want the session's address on its site", e)
			}
		default:
			t.Fatalf("unexpected event %q in %v", e, events)
		}
	}
	if probes == 0 {
		t.Fatalf("no identity probe went through the session's site: %v", events)
	}
}

// TestIdentityProbeRoutesThroughSessionSite (design remote-sites §12 invariant
// 2): confirmDeviceIdentity sends its scan_air through the site of the endpoint
// the session was opened on, and only once that session is open — in the cycle
// and on the reset-identity path alike.
func TestIdentityProbeRoutesThroughSessionSite(t *testing.T) {
	want := netsite.Endpoint{Site: "r1", IP: "192.168.7.20"}

	t.Run("cycle", func(t *testing.T) {
		h, _, log, built := sessionSiteHarness(t)
		h.cycle()

		for _, ep := range *built {
			if ep != want {
				t.Fatalf("device built for %+v, want the resolved endpoint %+v", ep, want)
			}
		}
		checkProbesFollowOpen(t, log.all())
		if st := h.status(); st.LastError != "" || st.Address != "192.168.7.20" {
			t.Fatalf("status LastError=%q Address=%q, want confirmed at the bare IP", st.LastError, st.Address)
		}
		if v := h.meta(LastIPKey("rig-01")); v != want.String() {
			t.Fatalf("lastip = %q, want %q", v, want.String())
		}
	})

	t.Run("reset identity", func(t *testing.T) {
		h, _, log, built := sessionSiteHarness(t)
		h.w.resetIdentity(context.Background())

		if len(*built) != 1 || (*built)[0] != want {
			t.Fatalf("reset built devices for %+v, want one for %+v", *built, want)
		}
		checkProbesFollowOpen(t, log.all())
	})
}

// TestForgetLastEndpointsOnSite is design remote-sites AC11's rig half, the
// cleanup a remote's removal runs: every last-known endpoint naming the removed
// site is cleared, and no other value is touched, whether it names another
// site, the local one, or nothing that parses.
func TestForgetLastEndpointsOnSite(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	const lanDevice = "192.168.7.20"
	lastIP := map[string]string{
		"rig-a": netsite.Endpoint{Site: "r1", IP: lanDevice}.String(),
		"rig-b": netsite.Endpoint{Site: "r2", IP: lanDevice}.String(),
		"rig-c": lanDevice,
		"rig-d": "r.1..10.0.0.9", // does not parse
	}
	for rigID, v := range lastIP {
		if err := h.store.SetMeta(ctx, LastIPKey(rigID), v); err != nil {
			t.Fatal(err)
		}
	}

	cleared, err := ForgetLastEndpointsOnSite(ctx, h.store, []string{"rig-a", "rig-b", "rig-c", "rig-d", "rig-none"}, "r1")
	if err != nil || cleared != 1 {
		t.Fatalf("ForgetLastEndpointsOnSite = %d, %v; want 1 cleared and no error", cleared, err)
	}
	for rigID, v := range lastIP {
		want := v
		if rigID == "rig-a" {
			want = ""
		}
		if got := h.meta(LastIPKey(rigID)); got != want {
			t.Errorf("lastip:%s = %q, want %q", rigID, got, want)
		}
	}
	if got := h.meta(LastIPKey("rig-none")); got != "" {
		t.Errorf("a rig with no endpoint gained one: %q", got)
	}
}
