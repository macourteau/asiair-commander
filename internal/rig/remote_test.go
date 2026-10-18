package rig

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/notify"
	"github.com/macourteau/asiair-commander/internal/store"
)

// remoteIP is the rig's address on the remote's LAN.
const remoteIP = "192.168.7.20"

// remoteHarness is a guid-bound rig whose last-known endpoint names remote r1
// ("Backyard"), a fake site the test brings up and down that presents itself
// as that remote, with the remote's state published on the hub as the
// supervisor publishes it. dials counts the devices the worker builds, each of
// which would dial.
func remoteHarness(t *testing.T) (h *harness, site *netsite.Fake, dials *int) {
	t.Helper()
	h = newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.profile.Host = ""
	h.resolver.resolveOnline = false // the registry misses, so lastip decides
	h.resolver.probeGUID = "dev-guid"
	h.cfg.Remotes = []model.RemoteProfile{{ID: "r1", Name: "Backyard"}}

	sites := netsite.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	site = netsite.NewFake("r1")
	site.SetRemote("Backyard")
	if err := sites.Add(site); err != nil {
		t.Fatalf("add site: %v", err)
	}
	h.w.sites = sites
	dials = new(int)
	h.w.newDevice = func(netsite.Endpoint, string, string, time.Duration) device.Device {
		*dials++
		return h.dev
	}
	if err := h.store.SetMeta(context.Background(), LastIPKey("rig-01"), "r1.."+remoteIP); err != nil {
		t.Fatalf("seed lastip: %v", err)
	}
	h.setRemote(model.RemoteConnected)
	return h, site, dials
}

// remoteHelloAt is when remote r1 last answered, as setRemote publishes it.
func (h *harness) remoteHelloAt() int64 { return h.nowVal.Add(-time.Hour).Unix() }

// setRemote publishes remote r1's state, as the supervisor does.
func (h *harness) setRemote(state model.RemoteState, warnings ...model.RemoteWarning) {
	h.hub.SetRemotes([]model.RemoteStatus{{
		ID: "r1", Name: "Backyard", State: state, EverConnected: true,
		LastHelloAt: h.remoteHelloAt(), Warnings: warnings,
	}})
}

// TestRemoteDownRigState (design remote-sites AC23): for each remote state, the
// rig card and its alerts follow the doc's table. With the tunnel up the card
// is the rig's own. With it down the worker's cycle writes the remote-down
// card: the remote's state, Safe to power off Unknown with the last known
// verdict and its time kept; nothing is dialled, and no per-rig alert is raised
// however long it lasts. A rig with a live session shows its own state, via the
// remote. The rig's own unreachable timer starts when the tunnel is back, and
// after Remove the rig shows the generic Offline.
func TestRemoteDownRigState(t *testing.T) {
	h, site, dials := remoteHarness(t)
	h.dev.AddFile(emmc, "Autorun/Light/o/Light_0001.fit", fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))

	// Connected: the rig's own states, with its site and remote published.
	h.cycle()
	h.cycle() // a second unchanged listing is idle, so the verdict is Safe
	st := h.status()
	if st.Site != "r1" || st.RemoteName != "Backyard" || st.Address != remoteIP {
		t.Fatalf("site %q remote %q address %q, want r1, Backyard, %s", st.Site, st.RemoteName, st.Address, remoteIP)
	}
	if st.Remote != nil || st.LastKnown != nil || !st.Online || st.SafeToPowerOff != model.SafeYes || st.InSession {
		t.Fatalf("connected remote: card %+v, want the rig's own Safe card, no live session after the cycle", st)
	}
	listedAt := st.SafeAsOf

	// Needs update: the tunnel is up, so the card is still the rig's own.
	h.setRemote(model.RemoteNeedsUpdate)
	if st := h.status(); st.Remote != nil || st.SafeToPowerOff != model.SafeYes {
		t.Fatalf("needs-update remote: card %+v, want the rig's own", st)
	}

	checkDown := func(when string, want model.RemoteState, helloAt int64) {
		t.Helper()
		st := h.status()
		if st.Remote == nil || st.Remote.State != want || !st.Remote.Down || st.Remote.LastHelloAt != helloAt {
			t.Fatalf("%s: remote view %+v, want %s, down, last hello %d", when, st.Remote, want, helloAt)
		}
		if st.RemoteName != "Backyard" || st.Online || st.State != model.RigStateOffline {
			t.Fatalf("%s: card %+v, want offline through Backyard", when, st)
		}
		if st.SafeToPowerOff != model.SafeUnknown || st.PowerOffReason != "remote" {
			t.Fatalf("%s: safe %s (%q), want unknown because of the remote", when, st.SafeToPowerOff, st.PowerOffReason)
		}
		if st.LastKnown == nil || st.LastKnown.Safe != model.SafeYes || st.LastKnown.At != listedAt {
			t.Fatalf("%s: last known %+v, want the Safe verdict as of %d", when, st.LastKnown, listedAt)
		}
	}

	site.SetState(netsite.StateDown)
	after := h.cfg.Defaults.UnreachableAlertAfter.D()
	for _, state := range []model.RemoteState{model.RemoteReconnecting, model.RemoteUnreachable, model.RemoteWaiting} {
		h.setRemote(state)
		helloAt := h.remoteHelloAt()
		before := *dials
		events := h.captureEvents(func() {
			h.cycle() // the cycle the supervisor's wake runs
			checkDown(string(state)+" after the woken cycle", state, helloAt)
			h.nowVal = h.nowVal.Add(after + time.Minute)
			h.cycle()
		})
		if *dials != before {
			t.Fatalf("%s: the worker built %d devices while the tunnel was down", state, *dials-before)
		}
		if findEvent(events, "rig.offline") != nil {
			t.Fatalf("%s: rig.offline emitted for a remote that is down", state)
		}
		checkDown(string(state)+" after cycles", state, helloAt)
		if n := h.countAlerts(model.AlertUnreachable); n != 0 {
			t.Fatalf("%s: %d per-rig unreachable alerts while the remote is down", state, n)
		}
	}

	// A rig with a live session shows its own state, reached via the remote:
	// the remote's heartbeat fails while the session still moves bytes.
	site.SetState(netsite.StateUp)
	h.setRemote(model.RemoteConnected)
	var during model.RigStatus
	h.dev.OnListDir = func(string, string) {
		if during.ID == "" {
			h.setRemote(model.RemoteReconnecting)
			during = h.status()
		}
	}
	h.cycle()
	h.dev.OnListDir = nil
	if during.Remote == nil || during.Remote.Down || during.Remote.State != model.RemoteReconnecting {
		t.Fatalf("live session: remote view %+v, want reconnecting and not down", during.Remote)
	}
	// Its verdict is still the one its last cycle published until this listing
	// completes; the view leaves it alone.
	if !during.InSession || !during.Online || during.State == model.RigStateOffline || during.LastKnown != nil {
		t.Fatalf("live session: card %+v, want the worker's own state", during)
	}
	// The session over, the cycle the transition's wake queued writes the
	// remote-down card.
	site.SetState(netsite.StateDown)
	h.cycle()
	if st := h.status(); st.Remote == nil || !st.Remote.Down {
		t.Fatalf("after the session: remote view %+v, want down", st.Remote)
	}

	// The ASIAIR is powered off with the tunnel up, which starts the rig's own
	// unreachable timer; then the remote goes down for longer than the rig's
	// threshold.
	site.SetState(netsite.StateUp)
	h.setRemote(model.RemoteConnected)
	h.dev.SetReachable(false)
	h.cycle()
	site.SetState(netsite.StateDown)
	h.setRemote(model.RemoteUnreachable)
	h.nowVal = h.nowVal.Add(2 * after)
	h.cycle()
	if n := h.countAlerts(model.AlertUnreachable); n != 0 {
		t.Fatalf("%d per-rig unreachable alerts while the remote is down", n)
	}

	// Back up, the ASIAIR still off: the rig's own offline state, and its
	// unreachable timer starts now rather than when the device went off.
	site.SetState(netsite.StateUp)
	h.setRemote(model.RemoteConnected)
	h.cycle()
	if st := h.status(); st.Remote != nil || st.State != model.RigStateOffline || st.PowerOffReason != "offline" {
		t.Fatalf("tunnel up, device off: card %+v, want the rig's own offline", st)
	}
	h.nowVal = h.nowVal.Add(after - time.Second)
	h.cycle()
	if n := h.countAlerts(model.AlertUnreachable); n != 0 {
		t.Fatalf("unreachable alert raised %v after the tunnel came back, before the threshold", after-time.Second)
	}
	h.nowVal = h.nowVal.Add(2 * time.Second)
	h.cycle()
	if n := h.countAlerts(model.AlertUnreachable); n != 1 {
		t.Fatalf("got %d unreachable alerts past the threshold after the tunnel came back, want 1", n)
	}

	// Removed: the supervisor unregisters the site, so the endpoint names an
	// unknown site, the rig is plainly offline, and no remote's state stands
	// in for its own.
	h.cfg.Remotes = nil
	h.hub.SetRemotes(nil)
	site.SetState(netsite.StateDown)
	h.w.sites.(*netsite.Registry).Remove("r1")
	h.cycle()
	if st := h.status(); st.Remote != nil || st.RemoteName != "" || st.PowerOffReason != "offline" || st.State != model.RigStateOffline {
		t.Fatalf("after Remove: card %+v, want generic Offline", st)
	}
}

// TestRefusedForwardOnRigCard: a forward the remote refused shows on the card
// of the rig at that IP on that remote, and on no other.
func TestRefusedForwardOnRigCard(t *testing.T) {
	h, _, _ := remoteHarness(t)
	h.cycle()
	mine := model.RemoteWarning{At: 1, Kind: model.RemoteWarningRefusedForward, Proto: "tcp", Target: remoteIP + ":445", Reason: "not on the allowlist"}
	other := model.RemoteWarning{At: 2, Kind: model.RemoteWarningRefusedForward, Proto: "tcp", Target: "192.168.7.21:445"}
	rejected := model.RemoteWarning{At: 3, Kind: "rejected_client", ClientKey: "nodekey:00"}
	h.setRemote(model.RemoteConnected, mine, other, rejected)
	if got := h.status().RefusedForwards; len(got) != 1 || got[0] != mine {
		t.Fatalf("refused forwards on the card = %+v, want only the one at %s", got, remoteIP)
	}

	// The same IP on the local network is another device.
	h.w.st.Site = ""
	h.w.publish()
	if got := h.status().RefusedForwards; len(got) != 0 {
		t.Fatalf("a local rig at the same IP shows the remote's refusals: %+v", got)
	}
}

// TestRigMovesBetweenSites (design remote-sites AC16): a rig adopted directly,
// then reached through a remote, then directly again keeps its profile, its
// frame history and its media-id keys; only its site, pill and Address switch,
// with no re-adoption.
func TestRigMovesBetweenSites(t *testing.T) {
	h, _, _ := remoteHarness(t)
	ctx := context.Background()
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	mediaKey := MediaIDKey("rig-01", udisk)
	if err := h.store.SetMeta(ctx, mediaKey, "media-1"); err != nil {
		t.Fatal(err)
	}
	profile := *h.profile

	pull := func(i int, direct bool) model.RigStatus {
		t.Helper()
		if direct {
			h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
		} else {
			h.resolver.resolveOnline = false
			if err := h.store.SetMeta(ctx, LastIPKey("rig-01"), "r1.."+remoteIP); err != nil {
				t.Fatal(err)
			}
		}
		src := "Autorun/Light/o/Light_000" + string(rune('1'+i)) + ".fit"
		h.dev.AddFile(emmc, src, data, h.nowVal.Add(-10*time.Minute))
		h.cycle()
		if f, ok := h.getFrame(src); !ok || f.RigID != "rig-01" {
			t.Fatalf("frame %d not filed under the rig: %+v", i, f)
		}
		return h.status()
	}

	if st := pull(0, true); st.Site != "" || st.RemoteName != "" || st.Address != "10.0.0.1" {
		t.Fatalf("direct: site %q remote %q address %q", st.Site, st.RemoteName, st.Address)
	}
	if st := pull(1, false); st.Site != "r1" || st.RemoteName != "Backyard" || st.Address != remoteIP {
		t.Fatalf("through the remote: site %q remote %q address %q", st.Site, st.RemoteName, st.Address)
	}
	if st := pull(2, true); st.Site != "" || st.RemoteName != "" || st.Address != "10.0.0.1" {
		t.Fatalf("direct again: site %q remote %q address %q", st.Site, st.RemoteName, st.Address)
	}

	frames, err := h.store.RecentFrames(ctx, "rig-01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("frame history has %d frames, want all 3 under the one rig", len(frames))
	}
	if got := h.cfg.Rig("rig-01"); got == nil || len(h.cfg.Rigs) != 1 || got.GUID != profile.GUID || got.Name != profile.Name {
		t.Fatalf("profile changed or re-adopted: %+v", h.cfg.Rigs)
	}
	if v, _, _ := h.store.GetMeta(ctx, mediaKey); v != "media-1" {
		t.Fatalf("media-id key = %q, want it kept", v)
	}
	for _, c := range []model.Category{model.AlertIdentityMismatch, model.AlertIdentityUnconfirmed, model.AlertRemoteStalled} {
		if n := h.countAlerts(c); n != 0 {
			t.Fatalf("%d %s alerts across the moves", n, c)
		}
	}
}

// errLink stands for a tunnel that drops under each transfer: a network error,
// as a tunnelled SMB operation reports one.
var errLink = &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset through the tunnel")}

// TestRemoteStalledProgressAlert (design remote-sites AC27): a remote rig whose
// deletes keep failing with its tunnel up raises one Warn alert naming its
// remote, once, after stallPasses passes, with the blocked deletes logged;
// passes cut by the tunnel going down never count; the next success
// acknowledges it.
func TestRemoteStalledProgressAlert(t *testing.T) {
	h, site, _ := remoteHarness(t)
	h.seedEligible(t, 2)
	h.dev.FailOpen(emmc, errLink) // every pre-delete re-hash fails

	// The tunnel drops during every pass: the remote's state explains those.
	h.dev.OnOpenReader = func(string, string) { site.SetState(netsite.StateDown) }
	for range stallPasses + 1 {
		site.SetState(netsite.StateUp)
		h.cycle()
	}
	if n := h.countAlerts(model.AlertRemoteStalled); n != 0 || h.w.stallPasses != 0 {
		t.Fatalf("passes cut by the tunnel going down counted: %d alerts, %d passes", n, h.w.stallPasses)
	}

	// A remote that is down never runs a pass, so never raises it.
	h.setRemote(model.RemoteReconnecting)
	for range stallPasses + 1 {
		h.cycle()
	}
	if n := h.countAlerts(model.AlertRemoteStalled); n != 0 {
		t.Fatalf("raised %d stalled alerts while the remote was down", n)
	}

	h.dev.OnOpenReader = nil
	site.SetState(netsite.StateUp)
	h.setRemote(model.RemoteConnected)
	for i := 1; i < stallPasses; i++ {
		h.cycle()
	}
	if n := h.countAlerts(model.AlertRemoteStalled); n != 0 {
		t.Fatalf("raised after %d failed passes, want %d", stallPasses-1, stallPasses)
	}
	h.cycle()
	h.cycle() // raised once, not once per pass
	var got []model.Alert
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertRemoteStalled {
			got = append(got, a)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d stalled alerts, want 1", len(got))
	}
	a := got[0]
	if a.Severity != model.SeverityWarn || a.RigID != "rig-01" || !strings.Contains(a.Message, `"Backyard"`) ||
		!strings.Contains(h.logBuf.String(), "deletes_blocked=6") {
		t.Fatalf("alert %+v, want a Warn on rig-01 naming Backyard, with the 6 blocked deletes of two frames over three passes logged", a)
	}

	// The next success acknowledges it.
	id, _, err := h.store.RaiseAlert(context.Background(), model.Alert{
		RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertRemoteStalled, Message: a.Message, CreatedAt: h.nowVal.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.dev.FailOpen(emmc, nil)
	h.cycle()
	if h.unacked(t, id) {
		t.Fatal("the stalled alert was not acknowledged by the next success")
	}
	if h.w.stallPasses != 0 || h.w.stallBlocked != 0 {
		t.Fatalf("count not reset by a success: %d passes, %d blocked", h.w.stallPasses, h.w.stallBlocked)
	}
}

// TestStalledProgressOnlyThroughARemote: a rig on a site other than the local
// one that is not a supervised remote counts no stalled passes and raises no
// alert naming a remote, however many of its passes fail.
func TestStalledProgressOnlyThroughARemote(t *testing.T) {
	h, _, dials := remoteHarness(t)
	sites := netsite.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := sites.Add(netsite.NewFake("x1")); err != nil { // up, not SetRemote
		t.Fatalf("add site: %v", err)
	}
	h.w.sites = sites
	if err := h.store.SetMeta(context.Background(), LastIPKey("rig-01"), "x1.."+remoteIP); err != nil {
		t.Fatalf("seed lastip: %v", err)
	}
	h.seedEligible(t, 2)
	h.dev.FailOpen(emmc, errLink) // every pre-delete re-hash fails
	for range stallPasses + 1 {
		h.cycle()
	}
	if *dials == 0 {
		t.Fatal("no pass reached the device through the site")
	}
	if n := h.countAlerts(model.AlertRemoteStalled); n != 0 || h.w.stallPasses != 0 {
		t.Fatalf("a non-remote site counted stalled passes: %d alerts, %d passes", n, h.w.stallPasses)
	}
}

// TestStalledProgressIgnoresOrdinaryWaits: a delete waiting for an ordinary
// reason (the settle window, a supersede in flight, an unclassified frame, a
// source deferral) reaches no failed device operation, so it never feeds the
// stalled-progress count.
func TestStalledProgressIgnoresOrdinaryWaits(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *harness, f model.Frame)
	}{
		{"settle window", func(t *testing.T, h *harness, f model.Frame) {
			if err := os.Remove(f.DestPath); err != nil {
				t.Fatal(err)
			}
			h.setMissing(t, f.ID, 60) // culled a minute ago: still settling
		}},
		{"supersede in flight", func(t *testing.T, h *harness, f model.Frame) {
			if err := h.store.PutSupersedeIntent(ctx, store.SupersedeIntent{
				RigID: "rig-01", DestPath: f.DestPath, AsidePath: f.DestPath + ".aside", FrameID: f.ID, CreatedAt: h.nowVal.Unix(),
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"unclassified", func(t *testing.T, h *harness, f model.Frame) {
			f.State = model.StateUnclassified
			if err := h.store.UpsertFrame(ctx, &f); err != nil {
				t.Fatal(err)
			}
		}},
		{"source_changed_deferred", func(t *testing.T, h *harness, f model.Frame) { h.changeSource(f) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := remoteHarness(t)
			cy := h.w.newCycle(h.profile, netsite.Endpoint{Site: "r1", IP: remoteIP})
			f := h.seedEligible(t, 1)[0]
			tc.setup(t, h, f)
			for range stallPasses + 1 {
				var fails linkFailures
				pass := h.w.deletePhase(ctx, h.cfg, cy, watchDevice(h.dev, &fails), h.nowVal.Add(time.Hour))
				if fails.count() != 0 || pass.hashFailed != 0 || pass.unconfirmed {
					t.Fatalf("an ordinary wait counted %d failures, pass %+v", fails.count(), pass)
				}
				h.w.trackStall(ctx, cy, false, false, &fails)
			}
			if !h.deviceHas(f.SourcePath) {
				t.Fatal("the waiting frame was deleted")
			}
			if n := h.countAlerts(model.AlertRemoteStalled); n != 0 || h.w.stallPasses != 0 {
				t.Fatalf("ordinary waits raised %d alerts, counted %d passes", n, h.w.stallPasses)
			}
		})
	}
}

// TestRemoteHashFailureCountsAsStall: a pre-delete digest the remote fails to
// measure, which touches no session operation, feeds the stalled-progress
// count once per frame and pass; the delete is deferred as before.
func TestRemoteHashFailureCountsAsStall(t *testing.T) {
	h, site, _ := remoteHarness(t)
	frames := h.seedEligible(t, 2)
	site.OnMeasure(func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
		return netsite.DigestResult{}, errLink
	})
	for range stallPasses {
		h.cycle()
	}
	var got []model.Alert
	for _, a := range h.notify.Raised() {
		if a.Category == model.AlertRemoteStalled {
			got = append(got, a)
		}
	}
	if len(got) != 1 || !strings.Contains(h.logBuf.String(), "deletes_blocked=6") {
		t.Fatalf("stalled alerts %+v, want one, with the 6 blocked deletes of two frames over three passes logged", got)
	}
	for _, f := range frames {
		if !h.deviceHas(f.SourcePath) {
			t.Fatalf("%s deleted without a digest", f.SourcePath)
		}
	}
}

// TestUnconfirmedDeleteSessionCountsAsStall: a delete session whose identity
// probe gets no answer deletes nothing and reports itself to the
// stalled-progress count.
func TestUnconfirmedDeleteSessionCountsAsStall(t *testing.T) {
	h, _, _ := remoteHarness(t)
	h.seedEligible(t, 1)
	h.resolver.probeOK = false
	var fails linkFailures
	pass := h.w.deletePhase(context.Background(), h.cfg, h.w.newCycle(h.profile, netsite.Endpoint{Site: "r1", IP: remoteIP}), watchDevice(h.dev, &fails), h.nowVal.Add(time.Hour))
	if !pass.unconfirmed {
		t.Fatal("an unconfirmed delete session was not reported to the stall count")
	}
}

// TestUnconfirmedRemoteHashIsNotProgress: a remote whose digest measurement
// keeps reporting no identity ends each slice the way an unconfirmed delete
// session does. Its cursor moves, but that is not progress, so a run until idle
// ends on its no-progress count rather than at idle, and every pass feeds the
// stalled-progress count.
func TestUnconfirmedRemoteHashIsNotProgress(t *testing.T) {
	h, site, _ := remoteHarness(t)
	h.roomy()
	frames := h.seedEligible(t, 1)
	h.keepCaptureDir()
	measures := 0
	site.OnMeasure(func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
		measures++
		r := measured(nil)
		r.GUID, r.CPUID = "", ""
		return r, nil
	})
	h.pauseRig()

	h.runNow()
	for n := 1; n < maxNoProgressCycles && h.w.runUntilIdle; n++ {
		h.runCycleAndWait()
	}
	if h.w.runUntilIdle {
		t.Fatalf("the run was still going after %d cycles", maxNoProgressCycles)
	}
	if h.runEndedBecause("idle") || !h.runEndedBecause(`"no progress"`) {
		t.Fatal("a run whose every measurement confirmed no identity should end on no progress, not at idle")
	}
	if measures != maxNoProgressCycles {
		t.Fatalf("%d measurements, want one per cycle (%d)", measures, maxNoProgressCycles)
	}
	if h.w.stallPasses != stallPasses {
		t.Fatalf("stalled-progress count %d, want one per pass (%d)", h.w.stallPasses, stallPasses)
	}
	if n := h.countAlerts(model.AlertRemoteStalled); n != 1 {
		t.Fatalf("%d stalled alerts, want 1", n)
	}
	if !h.deviceHas(frames[0].SourcePath) {
		t.Fatal("a frame whose measurement confirmed no identity was deleted")
	}
}

// TestStalledProgressIgnoresDeviceRefusals: operations that are not a pull or
// delete attempt, or that the device refuses rather than the link dropping
// (the legacy identity-marker sweep on a read-only root, a log the device
// will not serve), never count toward stalled progress on an idle night.
func TestStalledProgressIgnoresDeviceRefusals(t *testing.T) {
	h, _, _ := remoteHarness(t)
	h.profile.PullLogs = true
	h.dev.SetMarker(emmc, model.IdentityMarkerName, []byte("legacy"))
	refused := errors.New("STATUS_ACCESS_DENIED")
	h.dev.FailDelete(emmc, refused)
	h.dev.FailReadMarker(emmc, model.IdentityMarkerName, refused)
	h.dev.AddFile(emmc, "Log/Autorun_Log_2026-01-01.txt", []byte("log"), h.nowVal.Add(-time.Hour))
	h.dev.FailOpen(emmc, refused)
	for range stallPasses + 2 {
		h.cycle()
	}
	if n := h.countAlerts(model.AlertRemoteStalled); n != 0 || h.w.stallPasses != 0 {
		t.Fatalf("device refusals raised %d stalled alerts, counted %d passes", n, h.w.stallPasses)
	}
}

// TestRemoteWakeWritesRemoteDownAtOnce: the wake the supervisor sends when a
// remote changes state runs a cycle at once, so the rig's published state
// follows its remote down and back up without waiting out the poll or retry
// interval.
func TestRemoteWakeWritesRemoteDownAtOnce(t *testing.T) {
	h, site, _ := remoteHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.w.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, cond func(model.RigStatus) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond(h.status()) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, h.status())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("the first cycle", func(st model.RigStatus) bool { return st.Online && st.Site == "r1" })

	site.SetState(netsite.StateDown)
	h.setRemote(model.RemoteReconnecting)
	h.w.WakeForRemote()
	waitFor("the remote-down card", func(st model.RigStatus) bool {
		return st.PowerOffReason == model.PowerOffRemote && !st.Online && st.Remote != nil && st.Remote.Down
	})

	site.SetState(netsite.StateUp)
	h.setRemote(model.RemoteConnected)
	h.w.WakeForRemote()
	waitFor("the rig's own card again", func(st model.RigStatus) bool {
		return st.Online && st.PowerOffReason != model.PowerOffRemote && st.Remote == nil
	})
}

// unacked reports whether the alert with this id is still unacknowledged.
func (h *harness) unacked(t *testing.T, id int64) bool {
	t.Helper()
	alerts, err := h.store.ListAlerts(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.ID == id {
			return true
		}
	}
	return false
}

// The stalled alert is standing, so its message must not carry a count that
// differs between episodes: a later episode of the same stall (after a restart
// resets the in-memory count, say) would otherwise be a new alert past the
// operator's acknowledgement.
func TestRemoteStalledAlertMessageIsStableAcrossEpisodes(t *testing.T) {
	h, _, _ := remoteHarness(t)
	h.w.notify = notify.New(h.store, h.hub, nil, nil)
	h.seedEligible(t, 2)
	h.dev.FailOpen(emmc, errLink)
	for range stallPasses {
		h.cycle()
	}
	all, _ := h.store.ListAlerts(context.Background(), false, 0)
	if !slices.ContainsFunc(all, func(a model.Alert) bool { return a.Category == model.AlertRemoteStalled }) {
		t.Fatalf("unacked alerts = %+v, want the stalled alert among them", all)
	}
	for _, a := range all {
		if err := h.store.AckAlert(context.Background(), a.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	// A new process: the episode's counts start over from different values.
	h.w.stallPasses, h.w.stallBlocked = 0, 10
	for range stallPasses {
		h.cycle()
	}
	rows, _ := h.store.ListAlerts(context.Background(), true, 0)
	stalled := 0
	for _, a := range rows {
		if a.Category == model.AlertRemoteStalled {
			stalled++
		}
	}
	if stalled != 1 {
		t.Fatalf("stalled rows after a second episode of the same stall = %d, want 1 (acknowledged)", stalled)
	}
}
