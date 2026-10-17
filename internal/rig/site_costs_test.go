package rig

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// remoteCostHarness is sessionSiteHarness with the worker's own registry also
// holding the fake site, so the cycle reads that site's costs, and with every
// device timeout the worker hands its factory recorded.
func remoteCostHarness(t *testing.T) (*harness, *netsite.Fake, *[]time.Duration) {
	t.Helper()
	h, site, log, _ := sessionSiteHarness(t)
	reg := netsite.NewRegistry(nil)
	if err := reg.Add(site); err != nil {
		t.Fatalf("add site: %v", err)
	}
	h.w.sites = reg
	var timeouts []time.Duration
	h.w.newDevice = func(_ netsite.Endpoint, _, _ string, timeout time.Duration) device.Device {
		timeouts = append(timeouts, timeout)
		return orderedDevice{Device: h.dev, log: log}
	}
	return h, site, &timeouts
}

// TestAdmissionPolicyPerRemoteSlotCallers is the rig side of
// TestAdmissionPolicyPerRemoteSlot (internal/manager): a rig reached through a
// remote site names that site to Admission for its pulls, deletes and
// restores, so each takes the site's slot, while its retarget, NAS-only work,
// names the local site and takes the local path.
func TestAdmissionPolicyPerRemoteSlotCallers(t *testing.T) {
	ctx := context.Background()
	h, _, _ := remoteCostHarness(t)
	remote := Requester{Rig: "rig-01", Site: "r1"}

	// requestsSince returns the requesters recorded after the first n, failing
	// unless there is at least one and each is want.
	requestsSince := func(phase string, n int, want Requester) int {
		t.Helper()
		reqs := h.adm.requesters()
		if len(reqs) <= n {
			t.Fatalf("%s: no admission request", phase)
		}
		for _, r := range reqs[n:] {
			if r != want {
				t.Fatalf("%s: admission requester %+v, want %+v", phase, r, want)
			}
		}
		return len(reqs)
	}

	// Pull.
	a := fileLightAs(t, h, "Autorun/Light/A/Light_0001.fit", "M31", dateObsNight1)
	b := fileLightAs(t, h, "Autorun/Light/B/Light_0001.fit", "M33", dateObsNight2)
	n := requestsSince("pull", 0, remote)

	// Retarget is NAS-only: the local path, whatever the rig's site.
	if res, err := h.w.serviceRetarget(ctx, a.ID, "M42"); err != nil || res.Status != "retargeted" {
		t.Fatalf("retarget: status=%q reason=%q err=%v", res.Status, res.Reason, err)
	}
	n = requestsSince("retarget", n, Requester{Rig: "rig-01", Site: netsite.Local})

	// Restore: the operator culled b's NAS copy and asked for it back.
	if err := os.Remove(b.DestPath); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, b.ID, 0)
	if ok, err := h.store.RequestRestore(ctx, "rig-01", b.ID, h.nowVal.Unix()); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}
	h.cycle()
	if _, err := os.Stat(b.DestPath); err != nil {
		t.Fatalf("restore did not re-pull b: %v", err)
	}
	n = requestsSince("restore", n, remote)

	// Delete: past the grace, the delete pass removes the proven originals.
	h.nowVal = h.nowVal.Add(2 * time.Hour)
	h.cycle()
	if h.deviceHas(b.SourcePath) {
		t.Fatal("delete pass left b's proven original on the device")
	}
	requestsSince("delete", n, remote)
}

// TestTimeoutsScaleWithSiteLatency pins the rig's timeouts: each adds its
// stated round trips of margin from the cycle's site (the SMB budget 2, the
// control dial 1); at latency 0 each equals its base constant; and under a rate
// cap the SMB budget also allows one copy buffer's read at the capped rate.
func TestTimeoutsScaleWithSiteLatency(t *testing.T) {
	const lat = 120 * time.Millisecond
	f := netsite.NewFake("r1")
	f.SetLatency(lat)
	margin := netsite.RoundTrips(f, 1)
	if margin != 2*lat {
		t.Fatalf("one round trip's margin at %v = %v, want %v", lat, margin, 2*lat)
	}

	t.Run("the base constants on the local site", func(t *testing.T) {
		local := netsite.NewRegistry(nil).Site(netsite.Local)
		if got := smbTimeout(local); got != 30*time.Second {
			t.Fatalf("local smbTimeout = %v, want 30s", got)
		}
		if got := controlDialTimeoutOn(local); got != 3*time.Second {
			t.Fatalf("local control dial = %v, want 3s", got)
		}
	})

	t.Run("latency 0 leaves each at its base", func(t *testing.T) {
		z := netsite.NewFake("r0")
		z.SetStallWindow(netsite.LocalStallWindow)
		if got := smbTimeout(z); got != 30*time.Second {
			t.Fatalf("smbTimeout at latency 0 = %v, want 30s", got)
		}
		if got := controlDialTimeoutOn(z); got != 3*time.Second {
			t.Fatalf("control dial at latency 0 = %v, want 3s", got)
		}
	})

	t.Run("margins per stated round trips", func(t *testing.T) {
		f.SetStallWindow(netsite.LocalStallWindow)
		if got, want := smbTimeout(f), 30*time.Second+2*margin; got != want {
			t.Fatalf("smbTimeout = %v, want %v (30s + 2 round trips)", got, want)
		}
		if got, want := controlDialTimeoutOn(f), 3*time.Second+margin; got != want {
			t.Fatalf("control dial = %v, want %v (3s + 1 round trip)", got, want)
		}
		// A remote's own stall window replaces the 30 s base.
		f.SetStallWindow(netsite.RemoteStallWindow)
		if got, want := smbTimeout(f), 60*time.Second+2*margin; got != want {
			t.Fatalf("remote smbTimeout = %v, want %v (60s + 2 round trips)", got, want)
		}
	})

	t.Run("the capped-rate allowance", func(t *testing.T) {
		f.SetRateLimit(0.5)
		defer f.SetRateLimit(0)
		// One 512 KiB copy buffer at 0.5 MB/s.
		allowance := time.Duration(float64(header.CopyBufSize) / 500_000 * float64(time.Second))
		if got, want := smbTimeout(f), 60*time.Second+2*margin+allowance; got != want {
			t.Fatalf("capped smbTimeout = %v, want %v", got, want)
		}
	})

	t.Run("the cycle hands the device its site's budget", func(t *testing.T) {
		h, site, timeouts := remoteCostHarness(t)
		site.SetLatency(lat)
		h.cycle()
		if len(*timeouts) == 0 {
			t.Fatal("the cycle built no device")
		}
		want := netsite.RemoteStallWindow + 2*netsite.RoundTrips(site, 1)
		for _, got := range *timeouts {
			if got != want {
				t.Fatalf("device timeout = %v, want the remote site's budget %v", got, want)
			}
		}
		if st := h.status(); st.LastError != "" {
			t.Fatalf("cycle LastError = %q", st.LastError)
		}
	})
}

// TestIdentityProbeRetriesWithinBudget (design remote-sites AC4): the unicast
// identity probe is re-sent within its budget, so a probe whose first datagram
// is lost still confirms; one that is never answered leaves the session
// unconfirmed once the budget is spent, and not before.
func TestIdentityProbeRetriesWithinBudget(t *testing.T) {
	ep := netsite.Endpoint{Site: "r1", IP: "192.168.7.20"}
	probes := func(site *netsite.Fake) int {
		n := 0
		for _, c := range site.Calls() {
			if strings.HasPrefix(c, "probe ") {
				n++
			}
		}
		return n
	}

	t.Run("a lost first datagram is re-sent and confirms", func(t *testing.T) {
		h, site, _, _ := sessionSiteHarness(t)
		site.DropProbes(1)
		start := time.Now()
		ok, foreign := h.w.confirmDeviceIdentity(context.Background(), h.profile, ep)
		if !ok || foreign {
			t.Fatalf("confirm = ok %v foreign %v, want confirmed", ok, foreign)
		}
		if n := probes(site); n != 2 {
			t.Fatalf("probes sent = %d, want 2 (the lost one and its re-send)", n)
		}
		// The budget here is 3 s plus the fake's 5 s handshake allowance.
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Fatalf("confirmed after %v, past the probe budget", elapsed)
		}
	})

	t.Run("an unanswered probe leaves the session unconfirmed", func(t *testing.T) {
		h, site, _, _ := sessionSiteHarness(t)
		site.SetProbeReply(nil, "")
		// Latency 0 and no handshake allowance: the local site's 3 s budget.
		site.SetHandshakeAllowance(0)
		start := time.Now()
		ok, foreign := h.w.confirmDeviceIdentity(context.Background(), h.profile, ep)
		elapsed := time.Since(start)
		if ok || foreign {
			t.Fatalf("confirm = ok %v foreign %v, want unconfirmed (not foreign)", ok, foreign)
		}
		if n := probes(site); n < 2 {
			t.Fatalf("probes sent = %d, want the probe re-sent within its budget", n)
		}
		if elapsed < 3*time.Second-100*time.Millisecond || elapsed > 4*time.Second {
			t.Fatalf("gave up after %v, want the 3s budget", elapsed)
		}
	})
}
