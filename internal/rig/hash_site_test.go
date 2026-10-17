package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The rig's SMB login in TestRemoteHashContract; neither may appear in a log.
const (
	siteHashUser = "smb-user-7f3a"
	siteHashPass = "smb-pass-c41d"
)

// siteHashEndpoint is the delete session's endpoint, on the test's site.
var siteHashEndpoint = netsite.Endpoint{Site: "r1", IP: "192.168.7.20"}

// measuringSite is a site that measures digests in a session of its own, as a
// remote does, scripted per test; it records each request it was given.
type measuringSite struct {
	*netsite.Fake
	mu   sync.Mutex
	reqs []netsite.DigestRequest
}

func (m *measuringSite) requests() []netsite.DigestRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]netsite.DigestRequest(nil), m.reqs...)
}

// siteHashHarness is a guid-bound rig whose cycle site is a measuring site, and
// whose delete session's own probe confirms the device: only the measurement
// stands between a frame and its delete.
func siteHashHarness(t *testing.T, measure func(ctx context.Context, req netsite.DigestRequest) (netsite.DigestResult, error)) (*harness, *measuringSite) {
	t.Helper()
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.profile.User, h.profile.Password = siteHashUser, siteHashPass
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
	return h, installMeasuringSite(t, h, measure)
}

// installMeasuringSite registers a measuring site as siteHashEndpoint's site in
// the worker's registry, so a cycle on that endpoint measures through it.
func installMeasuringSite(t *testing.T, h *harness, measure func(ctx context.Context, req netsite.DigestRequest) (netsite.DigestResult, error)) *measuringSite {
	t.Helper()
	m := &measuringSite{Fake: netsite.NewFake(siteHashEndpoint.Site)}
	if measure != nil {
		m.OnMeasure(func(ctx context.Context, req netsite.DigestRequest) (netsite.DigestResult, error) {
			m.mu.Lock()
			m.reqs = append(m.reqs, req)
			m.mu.Unlock()
			return measure(ctx, req)
		})
	}
	if err := h.w.sites.(*netsite.Registry).Add(m.Fake); err != nil {
		t.Fatalf("add site: %v", err)
	}
	return m
}

// seedEligible seeds a deletion-eligible, three-way-provable frame, as
// deleteEligibleFrame does, under its own NAS name so several can coexist.
func seedEligible(t *testing.T, h *harness, src string, content []byte) model.Frame {
	t.Helper()
	mt := h.nowVal.Add(-2 * time.Hour)
	h.dev.AddFile(emmc, src, content, mt)
	nasPath := filepath.Join(h.dir, "light", filepath.Base(src))
	writeLocal(t, nasPath, content)
	f := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: src,
		SHA256: sha256hex(content), Size: int64(len(content)), Mtime: mt.Unix(),
		ArchiveSHA256: sha256hex(content), ArchiveSize: int64(len(content)),
		DestPath: nasPath, State: model.StateVerified, PulledAt: mt.Unix(),
	}
	if err := h.store.UpsertFrame(context.Background(), f); err != nil {
		t.Fatalf("upsert frame: %v", err)
	}
	got, ok := h.getFrame(src)
	if !ok {
		t.Fatalf("frame %s not stored", src)
	}
	return *got
}

// deleteThroughSite runs one unbounded delete slice on the site's endpoint.
func deleteThroughSite(h *harness) {
	h.w.deletePhase(context.Background(), h.cfg, h.w.newCycle(h.profile, siteHashEndpoint), h.dev, time.Now().Add(100*365*24*time.Hour))
}

// measured answers as a remote that reached this rig's device and measured
// content, with this build's algorithms.
func measured(content []byte) netsite.DigestResult {
	return netsite.DigestResult{
		Digests: netsite.Digests{Whole: sha256hex(content), Data: dataDigestOf(content)},
		Stated:  true, Size: int64(len(content)),
		Probed: true, GUID: "dev-guid", CPUID: "cpu-1", DataRecordable: true,
	}
}

func blockedCount(h *harness, reason string) float64 {
	return h.counterValue("asiair_commander_delete_blocked_total", map[string]string{"reason": reason})
}

// noSiteHashSecrets fails if the rig's log carries its SMB login.
func noSiteHashSecrets(t *testing.T, h *harness) {
	t.Helper()
	if logs := h.logBuf.String(); strings.Contains(logs, siteHashUser) || strings.Contains(logs, siteHashPass) {
		t.Fatalf("the SMB login reached the log:\n%s", logs)
	}
}

// TestRemoteHashContract (Commander half, the delete path): a digest measured
// in a session of the site's own feeds the three-way compare exactly where the
// streamed one does; the identity it probed must be this rig's, or the slice
// ends with nothing deleted (a different device also raising Error and the
// critical identity_mismatch); a failed measurement defers its frame; a data
// unit another algorithm computed is never recorded; and a site that measures
// through Commander's session runs the ordinary read.
func TestRemoteHashContract(t *testing.T) {
	first, second := []byte("first-proven-original"), []byte("second-proven-original")
	const src1, src2 = "Autorun/Light/Light_0001.fit", "Autorun/Light/Light_0002.fit"

	t.Run("matching identity and digests reach the three-way compare", func(t *testing.T) {
		h, site := siteHashHarness(t, func(_ context.Context, req netsite.DigestRequest) (netsite.DigestResult, error) {
			if strings.HasSuffix(req.Path, "0001.fit") {
				return measured(first), nil
			}
			return measured(second), nil
		})
		seedEligible(t, h, src1, first)
		seedEligible(t, h, src2, second)
		deleteThroughSite(h)

		if h.deviceHas(src1) || h.deviceHas(src2) {
			t.Fatal("frames whose measured digests match every anchor must be deleted")
		}
		reqs := site.requests()
		if len(reqs) != 2 {
			t.Fatalf("%d measurements, want one per frame", len(reqs))
		}
		r := reqs[0]
		if r.IP != siteHashEndpoint.IP || r.Share != emmc || r.Path != src1 ||
			r.User.Reveal() != siteHashUser || r.Password.Reveal() != siteHashPass || r.Stop == nil || r.Local == nil {
			t.Fatalf("request ip=%q share=%q path=%q, want the session's device, share, path and the profile's login",
				r.IP, r.Share, r.Path)
		}
		noSiteHashSecrets(t, h)
	})

	t.Run("a measured digest that disagrees holds the frame", func(t *testing.T) {
		h, _ := siteHashHarness(t, func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
			return measured([]byte("other content at the same path")), nil
		})
		f := seedEligible(t, h, src1, first)
		deleteThroughSite(h)

		if !h.deviceHas(src1) {
			t.Fatal("a frame whose measured digest disagrees with its anchor was deleted")
		}
		if got := mustGetFrame(t, h, f.ID); got.State != model.StateHeld {
			t.Fatalf("state %s, want held on the three-way mismatch", got.State)
		}
		if blockedCount(h, "three_way_mismatch") != 1 {
			t.Fatal("the three-way mismatch was not counted")
		}
	})

	t.Run("a different identity is Error and a critical identity_mismatch, and nothing is deleted", func(t *testing.T) {
		h, site := siteHashHarness(t, func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
			r := measured(first)
			r.GUID, r.CPUID = "someone-else", "cpu-9"
			return r, nil
		})
		seedEligible(t, h, src1, first)
		seedEligible(t, h, src2, second)
		deleteThroughSite(h)

		if !h.deviceHas(src1) || !h.deviceHas(src2) {
			t.Fatal("a slice whose measurement reached another device deleted a frame")
		}
		if n := len(site.requests()); n != 1 {
			t.Fatalf("%d measurements, want the slice to end at the first", n)
		}
		var critical bool
		for _, a := range h.notify.Raised() {
			critical = critical || (a.Category == model.AlertIdentityMismatch && a.Severity == model.SeverityCritical)
		}
		if !critical {
			t.Fatalf("raised %v, want a critical identity_mismatch", h.raisedCategories())
		}
		// Only the measurement saw the other device; Commander's own probe decides
		// pulls next cycle, so the error claims no more than deletes.
		const want = "identity mismatch: the device measured for a delete reported a different identity; refusing its deletes"
		if st := h.status(); st.State != model.RigStateError || st.LastError != want {
			t.Fatalf("state %s LastError %q, want Error with %q", st.State, st.LastError, want)
		}
		if blockedCount(h, "hash_identity_mismatch") != 1 || blockedCount(h, "hash_identity_unconfirmed") != 0 {
			t.Fatal("the mismatch was not counted under hash_identity_mismatch")
		}
	})

	t.Run("no identity ends the slice and defers its deletes", func(t *testing.T) {
		h, site := siteHashHarness(t, func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
			r := measured(first)
			r.GUID, r.CPUID = "", ""
			return r, nil
		})
		f := seedEligible(t, h, src1, first)
		seedEligible(t, h, src2, second)
		deleteThroughSite(h)

		if !h.deviceHas(src1) || !h.deviceHas(src2) {
			t.Fatal("a slice whose measurement confirmed no identity deleted a frame")
		}
		if n := len(site.requests()); n != 1 {
			t.Fatalf("%d measurements, want the slice to end at the first", n)
		}
		if got := mustGetFrame(t, h, f.ID); got.State != model.StateVerified {
			t.Fatalf("state %s, want the frame deferred, still verified", got.State)
		}
		if len(h.notify.Raised()) != 0 || h.status().State == model.RigStateError {
			t.Fatalf("an unanswered probe raised %v / state %s, want a quiet deferral", h.raisedCategories(), h.status().State)
		}
		if blockedCount(h, "hash_identity_unconfirmed") != 1 || blockedCount(h, "hash_identity_mismatch") != 0 {
			t.Fatal("the unconfirmed measurement was not counted under hash_identity_unconfirmed")
		}
	})

	t.Run("a transient error defers the frame and the slice goes on", func(t *testing.T) {
		h, site := siteHashHarness(t, func(_ context.Context, req netsite.DigestRequest) (netsite.DigestResult, error) {
			if strings.HasSuffix(req.Path, "0001.fit") {
				return netsite.DigestResult{}, fmt.Errorf("remote protocol: stalled (last error: %w)", errors.New("connection reset"))
			}
			return measured(second), nil
		})
		f := seedEligible(t, h, src1, first)
		seedEligible(t, h, src2, second)
		deleteThroughSite(h)

		if !h.deviceHas(src1) {
			t.Fatal("a frame whose measurement failed was deleted")
		}
		if got := mustGetFrame(t, h, f.ID); got.State != model.StateVerified {
			t.Fatalf("state %s, want the frame deferred, still verified", got.State)
		}
		if h.deviceHas(src2) || len(site.requests()) != 2 {
			t.Fatal("a failed measurement must defer only its own frame")
		}
		noSiteHashSecrets(t, h)
	})

	stops := []struct {
		name string
		// arm returns the slice's context and what to do inside the measurement
		// before its stop fires.
		arm func(h *harness) (context.Context, func())
	}{
		{"a Pause", func(h *harness) (context.Context, func()) {
			return context.Background(), func() { h.w.pauseRequested.Store(true) }
		}},
		{"ctx ending", func(*harness) (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		}},
	}
	for _, c := range stops {
		t.Run(c.name+" stops the measurement and defers", func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			h.profile.GUID = "dev-guid"
			h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
			ctx, fire := c.arm(h)
			installMeasuringSite(t, h, func(_ context.Context, req netsite.DigestRequest) (netsite.DigestResult, error) {
				fire()
				deadline := time.Now().Add(5 * time.Second)
				for !req.Stop() {
					if time.Now().After(deadline) {
						return netsite.DigestResult{}, errors.New("the request's stop never fired")
					}
					time.Sleep(time.Millisecond)
				}
				return netsite.DigestResult{}, fmt.Errorf("%w: context canceled", netsite.ErrStopped)
			})
			seedEligible(t, h, src1, first)
			h.w.deletePhase(ctx, h.cfg, h.w.newCycle(h.profile, siteHashEndpoint), h.dev, time.Now().Add(time.Hour))

			if !h.deviceHas(src1) {
				t.Fatal("a stopped measurement led to a delete")
			}
			if got := h.counterValue("asiair_commander_delete_hash_interrupted_total", nil); got != 1 {
				t.Fatalf("delete_hash_interrupted_total = %v, want the stop counted as an interrupted hash", got)
			}
		})
	}

	t.Run("a data unit from another algorithm is not recorded; the whole-file digest decides", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		culled, original := cullAndForget(t, h)
		h.nowVal = h.nowVal.Add(25 * time.Hour) // past the 24h reconcile settle
		h.profile.User, h.profile.Password = siteHashUser, siteHashPass
		installMeasuringSite(t, h, func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
			r := measured(original)
			r.GUID = "" // a host-bound rig: its delete session's probe already trusted the device
			r.CPUID = "cpu-1"
			r.Data, r.DataRecordable = "data-unit-of-another-algorithm", false
			return r, nil
		})
		deleteThroughSite(h)

		if h.deviceHas(culled.SourcePath) {
			t.Fatal("the culled original proven by the measured whole-file digest was not deleted")
		}
		if got := mustGetFrame(t, h, culled.ID); got.DataSHA256 != "" {
			t.Fatalf("data_sha256 = %q, want none recorded from another algorithm", got.DataSHA256)
		}
		if !strings.Contains(h.logBuf.String(), "not recording the frame's exposure identity") {
			t.Fatal("the skipped record was not logged")
		}
		noSiteHashSecrets(t, h)
	})

	t.Run("a refusal on the NAS-vanished race path ends the slice too", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		culled, original := cullAndForget(t, h)
		h.nowVal = h.nowVal.Add(25 * time.Hour) // past the 24h reconcile settle
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
		// The copy is back for the pre-hash Stat and gone again by VerifyRead, so
		// deleteOne hands the frame to handleAbsentNAS. The first measurement
		// carries no whole-file digest, so handleAbsentNAS measures again, and
		// that measurement reaches another device.
		writeLocal(t, culled.DestPath, original)
		calls := 0
		site := installMeasuringSite(t, h, func(context.Context, netsite.DigestRequest) (netsite.DigestResult, error) {
			calls++
			if calls == 1 {
				if err := os.Remove(culled.DestPath); err != nil {
					return netsite.DigestResult{}, err
				}
				r := measured(original)
				r.Whole = ""
				return r, nil
			}
			r := measured(original)
			r.GUID, r.CPUID = "someone-else", "cpu-9"
			return r, nil
		})
		later := seedEligible(t, h, "Autorun/Light/Light_0009.fit", second)
		if later.PulledAt <= culled.PulledAt {
			t.Fatal("setup: the culled frame must come first in the slice")
		}
		deleteThroughSite(h)

		if n := len(site.requests()); n != 2 {
			t.Fatalf("%d measurements, want the slice to end at the race path's refusal", n)
		}
		if !h.deviceHas(culled.SourcePath) || !h.deviceHas(later.SourcePath) {
			t.Fatal("a slice refused on identity deleted a frame")
		}
		if blockedCount(h, "hash_identity_mismatch") != 1 || h.status().State != model.RigStateError {
			t.Fatal("the race path's refusal did not take the foreign-identity handling")
		}
	})

	t.Run("a site measuring through Commander's session runs the ordinary read", func(t *testing.T) {
		h, _ := siteHashHarness(t, nil) // the Fake's default: the request's local read
		seedEligible(t, h, src1, first)
		before := h.dev.ContentReads()
		deleteThroughSite(h)

		if got := h.dev.ContentReads() - before; got != 1 {
			t.Fatalf("%d device content reads, want the one streaming read", got)
		}
		if h.deviceHas(src1) {
			t.Fatal("the frame proven by Commander's own read was not deleted")
		}
	})
}
