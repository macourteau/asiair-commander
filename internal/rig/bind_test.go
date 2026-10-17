package rig

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// ourMarker is a valid legacy rig-identity marker naming profileID, used to seed
// a leftover marker file the worker must ignore (identity is confirmed by the
// live probe, not this file).
func ourMarker(profileID string) []byte {
	return []byte(profileID + " 11111111-1111-1111-1111-111111111111\n")
}

func (h *harness) readMarker(share, name string) ([]byte, error) {
	sess, err := h.dev.Open(context.Background())
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	sh, err := sess.Mount(share)
	if err != nil {
		return nil, err
	}
	return sh.ReadMarker(name)
}

func (h *harness) meta(key string) string {
	v, _, _ := h.store.GetMeta(context.Background(), key)
	return v
}

// --- resolveAddress ordering -------------------------------------------------

func TestResolveAddressOrdering(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()

	// 1. A guid that resolves online wins.
	h.profile.GUID = "g1"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.7", true
	if addr, ok := h.w.resolveAddress(ctx, h.profile); !ok || addr != netsite.LocalEndpoint("10.0.0.7") {
		t.Fatalf("guid resolve = %q ok=%v, want 10.0.0.7", addr, ok)
	}

	// 2. Guid offline but a last-known IP is cached → that IP.
	h.resolver.resolveOnline = false
	if err := h.store.SetMeta(ctx, LastIPKey("rig-01"), "10.0.0.9"); err != nil {
		t.Fatalf("seed lastip: %v", err)
	}
	if addr, ok := h.w.resolveAddress(ctx, h.profile); !ok || addr != netsite.LocalEndpoint("10.0.0.9") {
		t.Fatalf("lastip resolve = %q ok=%v, want 10.0.0.9", addr, ok)
	}

	// 3. No guid, no lastip, legacy Host resolves to its first IPv4.
	h.profile.GUID = ""
	if err := h.store.SetMeta(ctx, LastIPKey("rig-01"), ""); err != nil {
		t.Fatalf("clear lastip: %v", err)
	}
	h.profile.Host = "asiair.local"
	h.w.lookupHost = func(string) ([]string, error) { return []string{"::1", "192.168.1.42"}, nil }
	if addr, ok := h.w.resolveAddress(ctx, h.profile); !ok || addr != netsite.LocalEndpoint("192.168.1.42") {
		t.Fatalf("host resolve = %q ok=%v, want 192.168.1.42", addr, ok)
	}

	// 4. Nothing to bind → offline.
	h.profile.Host = ""
	if addr, ok := h.w.resolveAddress(ctx, h.profile); ok || addr != (netsite.Endpoint{}) {
		t.Fatalf("empty resolve = %q ok=%v, want offline", addr, ok)
	}
}

// --- confirmDeviceIdentity truth table ---------------------------------------

func TestConfirmDeviceIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("guid match proceeds", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
		if ok, foreign := h.w.confirmDeviceIdentity(ctx, h.profile, netsite.LocalEndpoint("10.0.0.1")); !ok || foreign {
			t.Fatalf("guid match: ok=%v foreign=%v, want ok", ok, foreign)
		}
	})

	// A guid-bound rig whose stored guid changed but whose cpuId still matches
	// keeps running on the cpuId fallback, with no refuse and no re-bind attempt.
	t.Run("cpuId match on a changed guid proceeds, no re-bind", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "old-guid"
		h.profile.CPUID = "cpu-1"
		h.resolver.probeGUID, h.resolver.probeCPU, h.resolver.probeOK = "new-guid", "cpu-1", true
		if ok, foreign := h.w.confirmDeviceIdentity(ctx, h.profile, netsite.LocalEndpoint("10.0.0.1")); !ok || foreign {
			t.Fatalf("cpuId match: ok=%v foreign=%v, want ok", ok, foreign)
		}
		if v := h.meta(PendingGUIDKey("rig-01")); v != "" {
			t.Fatalf("a bound rig must not record a pending guid (no re-bind), got %q", v)
		}
	})

	// A probed, guid-bound rig that reports neither the stored guid nor cpuId is
	// foreign — including the CM4 both-empty-cpuId case (profile has no cpuId).
	t.Run("guid+cpuId mismatch is foreign (both cpu empty)", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "dev-guid" // no CPUID
		h.resolver.probeGUID, h.resolver.probeCPU, h.resolver.probeOK = "someone-else", "", true
		if ok, foreign := h.w.confirmDeviceIdentity(ctx, h.profile, netsite.LocalEndpoint("10.0.0.1")); ok || !foreign {
			t.Fatalf("mismatch: ok=%v foreign=%v, want foreign", ok, foreign)
		}
	})

	t.Run("no reply is neither ok nor foreign", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "dev-guid"
		h.resolver.probeOK = false
		if ok, foreign := h.w.confirmDeviceIdentity(ctx, h.profile, netsite.LocalEndpoint("10.0.0.1")); ok || foreign {
			t.Fatalf("no reply: ok=%v foreign=%v, want both false", ok, foreign)
		}
	})

	// A host-bound rig records a pending guid bind from THIS probe (single probe)
	// and proceeds.
	t.Run("host-bound records pending guid from the same probe", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = "" // host-bound
		h.resolver.probeGUID, h.resolver.probeCPU, h.resolver.probeOK = "live-guid", "live-cpu", true
		if ok, foreign := h.w.confirmDeviceIdentity(ctx, h.profile, netsite.LocalEndpoint("10.0.0.1")); !ok || foreign {
			t.Fatalf("host-bound: ok=%v foreign=%v, want ok", ok, foreign)
		}
		if v := h.meta(PendingGUIDKey("rig-01")); v != "live-guid,live-cpu" {
			t.Fatalf("host-bound pendingguid = %q, want live-guid,live-cpu", v)
		}
		if h.resolver.calls() != 1 {
			t.Fatalf("host-bound made %d probes, want exactly 1", h.resolver.calls())
		}
	})

	// A host-bound rig whose probe reports an empty guid never persists a
	// ",<cpuId>" key that would bind nothing; it still proceeds.
	t.Run("host-bound with an empty probed guid records nothing", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.profile.GUID = ""
		h.resolver.probeGUID, h.resolver.probeCPU, h.resolver.probeOK = "", "live-cpu", true
		if ok, _ := h.w.confirmDeviceIdentity(ctx, h.profile, netsite.LocalEndpoint("10.0.0.1")); !ok {
			t.Fatal("host-bound with an empty guid should still proceed (ok)")
		}
		if v := h.meta(PendingGUIDKey("rig-01")); v != "" {
			t.Fatalf("an empty probed guid persisted pendingguid = %q, want none", v)
		}
	})
}

// --- last-known IP -----------------------------------------------------------

func TestLastKnownIPRecordedOnConfirm(t *testing.T) {
	// A confirmed cycle caches the connect address as the last-known-good IP.
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.9", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true
	h.cycle()
	if v := h.meta(LastIPKey("rig-01")); v != "10.0.0.9" {
		t.Fatalf("a confirmed cycle should record lastip=10.0.0.9, got %q", v)
	}

	// A no-reply cycle refuses before confirming, so it records no IP.
	h2 := newHarness(t, emmc, header.New())
	h2.profile.GUID = "dev-guid"
	h2.resolver.resolveAddr, h2.resolver.resolveOnline = "10.0.0.9", true
	h2.resolver.probeOK = false
	h2.cycle()
	if v := h2.meta(LastIPKey("rig-01")); v != "" {
		t.Fatalf("a no-reply cycle must record no lastip, got %q", v)
	}
}

// --- migration: pending guid recorded from the confirm probe -----------------

func TestMigrationRecordsPendingGUID(t *testing.T) {
	// A host-bound rig with a live probe reply records a bind for the manager.
	h := newHarness(t, emmc, header.New())
	h.resolver.probeGUID, h.resolver.probeCPU, h.resolver.probeOK = "live-guid", "live-cpu", true
	h.cycle()
	if v := h.meta(PendingGUIDKey("rig-01")); v != "live-guid,live-cpu" {
		t.Fatalf("pendingguid = %q, want live-guid,live-cpu", v)
	}
}

func TestMigrationRecordsNothingWithoutProbeReply(t *testing.T) {
	// No probe reply → identity unconfirmed → nothing bound.
	h := newHarness(t, emmc, header.New())
	h.resolver.probeOK = false
	h.cycle()
	if v := h.meta(PendingGUIDKey("rig-01")); v != "" {
		t.Fatalf("no probe reply recorded pendingguid = %q, want none", v)
	}
}

// --- delete pass re-confirms identity in its own session ---------------------

// deleteEligibleFrame seeds one deletion-eligible, three-way-provable frame so a
// delete-pass test isolates the identity gate as the only thing between it and a
// verified delete.
func (h *harness) deleteEligibleFrame(t *testing.T, src string) {
	t.Helper()
	ctx := context.Background()
	content := []byte("a-proven-original-frame")
	mt := h.nowVal.Add(-2 * time.Hour) // pulled long ago → grace elapsed
	h.dev.AddFile(emmc, src, content, mt)
	nasPath := filepath.Join(h.dir, "light", "Light_0001.fit")
	writeLocal(t, nasPath, content)
	f := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: src,
		SHA256: sha256hex(content), Size: int64(len(content)), Mtime: mt.Unix(),
		ArchiveSHA256: sha256hex(content), ArchiveSize: int64(len(content)),
		DestPath: nasPath, State: model.StateVerified, PulledAt: mt.Unix(),
	}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert frame: %v", err)
	}
	elig, err := h.store.DeletionEligible(ctx, "rig-01", h.nowVal.Add(-time.Hour).Unix())
	if err != nil || len(elig) != 1 {
		t.Fatalf("expected 1 eligible frame, got %d (err=%v)", len(elig), err)
	}
}

// TestDeletePassMismatchingProbeDeletesNothing is the cardinal delete-safety
// assertion: a guid-bound rig whose delete-session probe reports a DIFFERENT
// device deletes nothing and alerts, even though the frame is three-way provable.
func TestDeletePassMismatchingProbeDeletesNothing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "someone-else", true // a different device answers

	src := "Autorun/Light/Light_0001.fit"
	h.deleteEligibleFrame(t, src)

	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

	if !h.deviceHas(src) {
		t.Fatal("delete pass deleted from a foreign device (mismatching probe)")
	}
	if !h.hasAlert(model.AlertIdentityMismatch) {
		t.Fatalf("a foreign delete-session probe must alert; got %v", h.raisedCategories())
	}
	if got, _ := h.getFrame(src); got.State != model.StateVerified {
		t.Fatalf("frame state=%s, want verified (untouched)", got.State)
	}
	// Banner parity with the pull phase: the published status must show the error
	// state with an "identity"-bearing message THIS cycle, so the /identity banner
	// does not lag a pull behind the alert.
	st := h.status()
	if st.State != model.RigStateError {
		t.Fatalf("delete-phase foreign state=%s, want error", st.State)
	}
	if !strings.Contains(st.LastError, "identity") {
		t.Fatalf("delete-phase foreign LastError=%q, want it to mention identity", st.LastError)
	}
}

// TestDeletePassNoReplyDeletesNothing: a delete session with no scan_air reply
// cannot confirm identity, so it deletes nothing — but must NOT raise a
// wrong-device mismatch alert (a silent device is not a foreign one).
func TestDeletePassNoReplyDeletesNothing(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeOK = false // no reply

	src := "Autorun/Light/Light_0001.fit"
	h.deleteEligibleFrame(t, src)

	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

	if !h.deviceHas(src) {
		t.Fatal("delete pass deleted with an unconfirmed device")
	}
	if h.hasAlert(model.AlertIdentityMismatch) {
		t.Fatal("a no-reply delete probe must NOT raise a mismatch alert")
	}
	if got, _ := h.getFrame(src); got.State != model.StateVerified {
		t.Fatalf("frame state=%s, want verified (untouched)", got.State)
	}
}

// TestDeletePassConfirmedDeletes: a delete session that positively confirms the
// device proceeds to delete the three-way-provable original.
func TestDeletePassConfirmedDeletes(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.profile.GUID = "dev-guid"
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true

	src := "Autorun/Light/Light_0001.fit"
	h.deleteEligibleFrame(t, src)

	h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

	if h.deviceHas(src) {
		t.Fatal("a confirmed delete pass should remove the proven original")
	}
	if got, _ := h.getFrame(src); got.State != model.StateDeleted {
		t.Fatalf("frame state=%s, want deleted", got.State)
	}
}

// --- resetIdentity gating ----------------------------------------------------

func TestResetIdentityRefusesHostBound(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	// Host-bound (GUID==""): there is no device identity to confirm against.
	h.dev.SetMarker(emmc, model.IdentityMarkerName, ourMarker("rig-01"))
	h.w.resetIdentity(context.Background())
	if _, err := h.readMarker(emmc, model.IdentityMarkerName); err != nil {
		t.Fatalf("a refused reset must NOT touch the share, ReadMarker err=%v", err)
	}
	if !h.hasAlert(model.AlertConfig) {
		t.Fatal("a refused reset should raise an alert")
	}
}

func TestResetIdentityRefusesOnProbeMismatch(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.profile.GUID = "dev-guid"
	h.dev.SetMarker(emmc, model.IdentityMarkerName, ourMarker("rig-01"))
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "wrong-guid", true
	h.w.resetIdentity(context.Background())
	if _, err := h.readMarker(emmc, model.IdentityMarkerName); err != nil {
		t.Fatalf("a probe mismatch must NOT touch the share, ReadMarker err=%v", err)
	}
}

func TestResetIdentityClearsOnMatchingProbe(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	h.profile.GUID = "dev-guid"
	// Seed a media marker with a persisted id, plus a leftover legacy rig-identity
	// marker; a matching probe lets reset clear the media marker and its id and
	// best-effort sweep the leftover.
	h.dev.SetMarker(udisk, model.MediaMarkerName, []byte(`{"id":"persisted-x"}`+"\n"))
	h.dev.SetMarker(udisk, model.IdentityMarkerName, ourMarker("rig-01"))
	if err := h.store.SetMeta(context.Background(), MediaIDKey("rig-01", udisk), "persisted-x"); err != nil {
		t.Fatalf("seed persisted media id: %v", err)
	}
	h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true
	h.resolver.probeGUID, h.resolver.probeOK = "dev-guid", true

	h.w.resetIdentity(context.Background())

	if _, err := h.readMarker(udisk, model.MediaMarkerName); !device.IsNotFound(err) {
		t.Fatalf("a matching probe should clear the media marker, ReadMarker err=%v", err)
	}
	if _, err := h.readMarker(udisk, model.IdentityMarkerName); !device.IsNotFound(err) {
		t.Fatalf("reset should also sweep a leftover rig-identity marker, ReadMarker err=%v", err)
	}
	if v := h.meta(MediaIDKey("rig-01", udisk)); v != "" {
		t.Fatalf("reset should clear the persisted media id, got %q", v)
	}
	// Sanity: the probe was actually consulted.
	if h.resolver.calls() == 0 {
		t.Fatal("reset should have probed the device")
	}
}
