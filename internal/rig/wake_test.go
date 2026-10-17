package rig

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// --- discovery-wake scaffolding -------------------------------------------------

// wakeClock is a race-free injectable clock. The harness's nowVal is a plain
// field written by the test goroutine, which is fine for the synchronous
// h.cycle() tests but not for these: the worker's Run loop reads the clock from
// its own goroutine while the test advances it.
type wakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *wakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *wakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// startWorker builds a worker on the harness's dependencies and runs its real
// select loop, with a device that answers the reachability probe or does not, on
// a clock the test controls. It returns once the initial cycle has been counted.
//
// cycles reports how many cycles of a given result the worker has run — the same
// counter the production metrics surface exposes, so these tests observe probing
// the way an operator would rather than by reading worker fields.
func startWorker(t *testing.T, h *harness, reachable bool) (w *Worker, clk *wakeClock, cycles func(result string) int) {
	t.Helper()
	h.dev.SetReachable(reachable)
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.7", true

	clk = &wakeClock{t: h.nowVal}
	logger := slog.New(slog.NewTextHandler(h.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	factory := func(netsite.Endpoint, string, string, time.Duration) device.Device { return h.dev }
	w = NewWorker("rig-01", h.cfgPtr, factory, h.resolver, netsite.NewRegistry(logger), h.lfs, header.New(), h.store, h.hub,
		h.notify, h.adm, nil, nil, time.UTC, clk.now, h.metrics, logger)
	w.lookupHost = noLookup

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	cycles = func(result string) int {
		return int(h.counterValue("asiair_commander_cycles_total",
			map[string]string{"rig": "rig-01", "result": result}))
	}
	want := "offline"
	if reachable {
		want = "ok"
	}
	waitFor(t, func() bool { return cycles(want) >= 1 }) // Run's initial cycle
	return w, clk, cycles
}

// logCount counts how many times a line containing sub was emitted.
func logCount(h *harness, sub string) int {
	return strings.Count(h.logBuf.String(), sub)
}

// --- a scan answer wakes an offline rig -----------------------------------------

// A device answering the scan_air broadcast is the earliest evidence the network
// has that a rig is back. Feeding the worker only an address would leave an
// offline rig sitting out its whole retry interval — five minutes by default —
// while its ASIAIR was plainly on the LAN answering every scan, and the operator
// would see "Scan finished, 1 device answered" beside a card still reading
// Offline.
func TestDiscoveryAnswerProbesLongBeforeTheRetryInterval(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	w, _, cycles := startWorker(t, h, false)

	if got := cycles("offline"); got != 1 {
		t.Fatalf("precondition: offline cycles=%d after start, want 1", got)
	}
	retry := h.cfg.EffectiveRetryInterval(h.profile)
	if retry < time.Minute {
		t.Fatalf("precondition: retry interval=%s, want the multi-minute default", retry)
	}

	start := time.Now()
	w.WakeFromDiscovery() // exactly what the manager calls when a scan round answers
	waitFor(t, func() bool { return cycles("offline") >= 2 })

	// waitFor's own five-second deadline is the real bound here; the assertion
	// states the property under test.
	if elapsed := time.Since(start); elapsed >= retry {
		t.Fatalf("re-probe took %s, want well under the %s retry interval", elapsed, retry)
	}
	if !h.logContains("probing now rather than waiting out the retry interval") {
		t.Fatal("a discovery-driven probe must say so in the log")
	}
}

// A device whose UDP responder answers while its SMB server stays down must not
// buy a probe out of every discovery round: the wake backoff starts at the poll
// interval and doubles, so an unreachable-but-answering rig converges on the
// retry cadence it would have had anyway.
func TestDiscoveryWakeBacksOffWhileSMBStaysDown(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	w, clk, cycles := startWorker(t, h, false)
	poll := h.cfg.EffectivePollInterval(h.profile)
	const declined = "wake backoff has not elapsed"

	// First answer of the offline episode: free.
	w.WakeFromDiscovery()
	waitFor(t, func() bool { return cycles("offline") >= 2 })

	// Answering again immediately buys nothing — the gap is now one poll interval.
	w.WakeFromDiscovery()
	waitFor(t, func() bool { return logCount(h, declined) >= 1 })
	if got := cycles("offline"); got != 2 {
		t.Fatalf("offline cycles=%d after a wake inside the backoff, want 2", got)
	}

	// One poll interval later it is allowed again, and the gap doubles.
	clk.advance(poll)
	w.WakeFromDiscovery()
	waitFor(t, func() bool { return cycles("offline") >= 3 })

	// So the same wait no longer suffices: the gap is two poll intervals now.
	clk.advance(poll)
	w.WakeFromDiscovery()
	waitFor(t, func() bool { return logCount(h, declined) >= 2 })
	if got := cycles("offline"); got != 3 {
		t.Fatalf("offline cycles=%d after a wake inside the doubled backoff, want 3", got)
	}

	clk.advance(poll)
	w.WakeFromDiscovery()
	waitFor(t, func() bool { return cycles("offline") >= 4 })

	if got := cycles("offline"); got != 4 {
		t.Fatalf("five discovery answers produced %d probes, want 4 (initial cycle + 3 wakes)", got)
	}
}

// A rig that is already online is on the 45-second poll cadence, which a wake
// cannot better. Acting on one would also reschedule the ticker on every
// discovery round — a rig whose device answers every round would then never
// reach its own scheduled cycle.
func TestDiscoveryWakeIsIgnoredWhileTheRigIsOnline(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	w, _, cycles := startWorker(t, h, true)

	before := cycles("ok")
	w.WakeFromDiscovery()
	waitFor(t, func() bool { return h.logContains("the rig is already online") })

	if got := cycles("ok"); got != before {
		t.Fatalf("ok cycles=%d after waking an online rig, want %d", got, before)
	}
}
