package rig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The parity scenario's frames: one proven and deleted after its grace, one
// whose device original changes under it, one the operator culls, and one that
// arrives once the device answers with another identity.
const (
	parityProven  = "Autorun/Light/A/Light_0001.fit"
	parityChanged = "Autorun/Light/B/Light_0001.fit"
	parityCulled  = "Autorun/Light/C/Light_0001.fit"
	parityLate    = "Autorun/Light/D/Light_0001.fit"
)

var paritySources = []string{parityProven, parityChanged, parityCulled, parityLate}

// siteParityHarness is a guid-bound rig with the shipped settle, minimum-age,
// grace and reconcile-settle gates, whose last-known endpoint names either the
// local site or remote r1. The remote's site reports a remote's costs (latency,
// its 60 s stall window and handshake allowance, its own admission slot, replies
// that must match their source) but measures pre-delete digests with the local
// read, as a remote that streams through Commander's session does, so the two
// runs read the same bytes and only the site differs.
func siteParityHarness(t *testing.T, remote bool) (*harness, *int) {
	t.Helper()
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.MinFileAge = model.Duration(5 * time.Minute)
	h.cfg.Defaults.SettleCycles = 1
	h.cfg.Defaults.Grace = model.Duration(time.Hour)
	h.cfg.Defaults.ReconcileSettle = model.Duration(24 * time.Hour)
	h.profile.GUID = "dev-guid"
	h.profile.Host = ""
	h.resolver.resolveOnline = false // the registry misses, so lastip decides
	h.resolver.probeGUID = "dev-guid"

	ep := netsite.LocalEndpoint(remoteIP)
	measured := new(int)
	if remote {
		ep = netsite.Endpoint{Site: "r1", IP: remoteIP}
		h.cfg.Remotes = []model.RemoteProfile{{ID: "r1", Name: "Backyard"}}
		sites := netsite.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
		site := netsite.NewFake("r1")
		site.SetRemote("Backyard")
		site.SetLatency(180 * time.Millisecond)
		site.SetMinLatency(150 * time.Millisecond)
		if c := site.Costs(); c.StallWindow != netsite.RemoteStallWindow || c.HandshakeAllowance != netsite.RemoteHandshakeAllowance || !c.RepliesMatchSource {
			t.Fatal("setup: the fake site must report a remote's costs")
		}
		// The local read, counted so the test sees the delete pass measure
		// through the remote's site.
		site.OnMeasure(func(_ context.Context, req netsite.DigestRequest) (netsite.DigestResult, error) {
			*measured++
			return netsite.MeasureLocally(req)
		})
		if err := sites.Add(site); err != nil {
			t.Fatalf("add site: %v", err)
		}
		h.w.sites = sites
		h.setRemote(model.RemoteConnected)
	}
	if err := h.store.SetMeta(context.Background(), LastIPKey("rig-01"), ep.String()); err != nil {
		t.Fatalf("seed lastip: %v", err)
	}
	h.linkRoot(t, h.cfg.Roots.Light)
	return h, measured
}

// paritySnap is everything a verdict decides, after one step of the scenario:
// the rig's card less its display of the site, every frame row, which originals
// the device still holds, the alerts raised and the delete pass's counters.
type paritySnap struct {
	Step     string
	Status   model.RigStatus
	Frames   map[string]*model.Frame
	OnDevice []string
	Alerts   []string
	Counters map[string]float64
}

// paritySnapshot records the harness after step, with its temp dir abstracted so two
// harnesses compare.
func (h *harness) paritySnapshot(t *testing.T, step string) string {
	t.Helper()
	st := h.status()
	// Display may read the site: the address, the site and remote it names and
	// the remote's own state are the card's, never a verdict.
	st.Address, st.Site, st.RemoteName = "", "", ""
	st.Remote, st.LastKnown, st.RefusedForwards = nil, nil, nil
	s := paritySnap{Step: step, Status: st, Frames: map[string]*model.Frame{}, Counters: map[string]float64{}}
	for _, src := range paritySources {
		if f, ok := h.getFrame(src); ok {
			s.Frames[src] = f
		}
		if h.deviceHas(src) {
			s.OnDevice = append(s.OnDevice, src)
		}
	}
	for _, a := range h.notify.Raised() {
		s.Alerts = append(s.Alerts, fmt.Sprintf("%s/%s: %s", a.Category, a.Severity, a.Message))
	}
	fams, err := h.metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range fams {
		if mf.GetType() != dto.MetricType_COUNTER || !strings.Contains(mf.GetName(), "delete") {
			continue
		}
		for _, m := range mf.GetMetric() {
			var labels []string
			for _, lp := range m.GetLabel() {
				labels = append(labels, lp.GetName()+"="+lp.GetValue())
			}
			s.Counters[mf.GetName()+"{"+strings.Join(labels, ",")+"}"] = m.GetCounter().GetValue()
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), h.dir, "<dir>")
}

// runParityScenario drives one rig through identity confirmation, listings
// gated by minimum age and settle, pulls, the grace gate, a three-way match, a
// three-way mismatch, a cull past its reconcile settle and a foreign identity,
// all on the worker's injected clock, and returns a snapshot after each step.
// The outcome assertions hold on whichever site the rig is reached through.
func runParityScenario(t *testing.T, h *harness) []string {
	t.Helper()
	t0 := h.nowVal
	var snaps []string
	step := func(name string, at time.Duration) {
		t.Helper()
		h.nowVal = t0.Add(at)
		h.cycle()
		snaps = append(snaps, h.paritySnapshot(t, name))
	}
	frame := func(src string) model.Frame {
		t.Helper()
		f, ok := h.getFrame(src)
		if !ok {
			t.Fatalf("%s has no row", src)
		}
		return *f
	}

	proven := dateObsFixture(t, "M31", dateObsNight1)
	changed := dateObsFixture(t, "M33", dateObsNight1)
	culled := dateObsFixture(t, "M42", dateObsNight1)
	h.dev.AddFile(emmc, parityProven, proven, t0.Add(-time.Minute))
	h.dev.AddFile(emmc, parityChanged, changed, t0.Add(-10*time.Minute))
	h.dev.AddFile(emmc, parityCulled, culled, t0.Add(-10*time.Minute))

	// The first listing confirms the device and observes each file; none has
	// yet been seen stable across the settle cycle.
	step("first listing", 0)
	if h.status().State == model.RigStateError || h.resolver.calls() == 0 {
		t.Fatalf("the first session confirmed no identity: %+v", h.status())
	}
	for _, src := range []string{parityProven, parityChanged, parityCulled} {
		if f, ok := h.getFrame(src); ok && f.State == model.StateVerified {
			t.Fatalf("%s pulled on its first listing, before the settle cycle", src)
		}
	}

	// A minute on, the two older files have settled and are pulled; the young
	// one is still inside the five-minute minimum age.
	step("settled", time.Minute)
	if frame(parityChanged).State != model.StateVerified || frame(parityCulled).State != model.StateVerified {
		t.Fatal("the settled files were not pulled")
	}
	if f, ok := h.getFrame(parityProven); ok && f.State == model.StateVerified {
		t.Fatal("a file younger than the minimum age was pulled")
	}

	step("minimum age passed", 6*time.Minute)
	if frame(parityProven).State != model.StateVerified {
		t.Fatal("the file past its minimum age was not pulled")
	}

	// The device original of one frame changes under its row, at the same size
	// and mtime, so only the three-way digest check can tell; the operator culls
	// another's NAS copy.
	mutated := append([]byte(nil), changed...)
	mutated[len(mutated)-1] ^= 0xff
	h.dev.AddFile(emmc, parityChanged, mutated, time.Unix(frame(parityChanged).Mtime, 0))
	if err := os.Remove(frame(parityCulled).DestPath); err != nil {
		t.Fatal(err)
	}

	// The grace runs from each pull on the worker's clock: a minute short of it
	// nothing is deleted.
	step("inside grace", time.Minute+59*time.Minute)
	for _, src := range []string{parityProven, parityChanged, parityCulled} {
		if !h.deviceHas(src) {
			t.Fatalf("%s deleted inside its grace", src)
		}
	}

	// Past the grace of the two pulled first: the changed original fails the
	// three-way check and is held; the culled frame's absent copy proves
	// nothing yet.
	step("grace passed", time.Minute+61*time.Minute)
	if !h.deviceHas(parityChanged) || frame(parityChanged).State != model.StateHeld {
		t.Fatalf("the changed original: on device %v state %s, want held", h.deviceHas(parityChanged), frame(parityChanged).State)
	}
	if !h.deviceHas(parityCulled) {
		t.Fatal("the culled frame's original was deleted before its reconcile settle")
	}

	step("proven frame's grace passed", 6*time.Minute+61*time.Minute)
	if h.deviceHas(parityProven) || frame(parityProven).State != model.StateDeleted {
		t.Fatalf("the proven original: on device %v state %s, want deleted", h.deviceHas(parityProven), frame(parityProven).State)
	}

	// The library scan finds the cull; its reconcile settle runs from then.
	step("cull confirmed", 7*time.Hour)
	missingAt := frame(parityCulled).MissingSeenAt
	if missingAt == 0 {
		t.Fatal("the scan did not mark the culled frame missing")
	}
	settled := time.Unix(missingAt, 0).Add(24 * time.Hour).Sub(t0)
	step("inside reconcile settle", settled-time.Minute)
	if !h.deviceHas(parityCulled) {
		t.Fatal("the culled frame's original was deleted inside its reconcile settle")
	}
	step("reconcile settle passed", settled+time.Minute)
	if h.deviceHas(parityCulled) {
		t.Fatal("the culled frame's original survived its reconcile settle")
	}

	// The device at the rig's address answers with another identity: nothing
	// is pulled from it, and the rig stops on the mismatch.
	h.resolver.probeGUID = "someone-else"
	h.dev.AddFile(emmc, parityLate, dateObsFixture(t, "M45", dateObsNight1), h.nowVal.Add(-time.Hour))
	step("foreign identity", settled+2*time.Hour)
	if _, ok := h.getFrame(parityLate); ok {
		t.Fatal("a frame was filed from a device with another identity")
	}
	if st := h.status(); st.State != model.RigStateError {
		t.Fatalf("state %s, want Error on a foreign identity", st.State)
	}
	return snaps
}

// TestVerdictsIgnoreSiteKind is design remote-sites §12 invariant 12 and
// invariant 11's rig half: the same rig scenario run on the local site and on a
// remote's reaches the same verdict at every step (frame rows, deletes, holds,
// alerts, delete-blocked reasons, the rig's card less its display of the site),
// and every time gate opens on the worker's injected clock alone, with the rig
// reached through the remote as through the LAN.
func TestVerdictsIgnoreSiteKind(t *testing.T) {
	local, _ := siteParityHarness(t, false)
	remote, measured := siteParityHarness(t, true)

	localSnaps := runParityScenario(t, local)
	remoteSnaps := runParityScenario(t, remote)

	// The remote run really went through the remote: its card names the site,
	// its pulls and deletes took the site's admission slot, its delete pass
	// measured through the site, and its devices got the remote's SMB budget;
	// the local run took the local path.
	if st := remote.status(); st.Site != "r1" || st.RemoteName != "Backyard" {
		t.Fatalf("remote run card: site %q remote %q", st.Site, st.RemoteName)
	}
	for _, r := range remote.adm.requesters() {
		if r.Site != "r1" && r.Site != netsite.Local {
			t.Fatalf("remote run admission requester %+v", r)
		}
	}
	if !hasRequesterOn(remote.adm.requesters(), "r1") || hasRequesterOn(local.adm.requesters(), "r1") {
		t.Fatal("the remote run took no remote admission slot, or the local run did")
	}
	if *measured == 0 {
		t.Fatal("the remote run's delete pass measured nothing through its site")
	}
	site := remote.w.sites.Site("r1")
	if got, want := smbTimeout(site), netsite.RemoteStallWindow+2*netsite.RoundTrips(site, 1); got != want || site.Costs().Latency == 0 {
		t.Fatalf("remote SMB budget %v at latency %v, want %v", got, site.Costs().Latency, want)
	}

	if len(localSnaps) != len(remoteSnaps) {
		t.Fatalf("%d local steps, %d remote", len(localSnaps), len(remoteSnaps))
	}
	for i := range localSnaps {
		if err := sameSnapshot(localSnaps[i], remoteSnaps[i]); err != nil {
			t.Fatalf("step %d: the remote's verdict differs from the local site's: %v\nlocal:\n%s\nremote:\n%s",
				i, err, localSnaps[i], remoteSnaps[i])
		}
	}
}

func hasRequesterOn(reqs []Requester, site netsite.ID) bool {
	for _, r := range reqs {
		if r.Site == site {
			return true
		}
	}
	return false
}

// sameSnapshot names the first line two snapshots differ on.
func sameSnapshot(a, b string) error {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return fmt.Errorf("line %d: local %q, remote %q", i+1, strings.TrimSpace(la[i]), strings.TrimSpace(lb[i]))
		}
	}
	if len(la) != len(lb) {
		return errors.New("one snapshot is longer")
	}
	return nil
}
