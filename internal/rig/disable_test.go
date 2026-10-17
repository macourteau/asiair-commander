package rig

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// TestDisabledShareFullyIgnored (D1) pins that a disabled share is never mounted,
// listed, pulled, or counted: an enabled share alongside it still pulls, while the
// disabled share's frame never reaches the store and the share never mounts (a mount
// would surface as a WARN and a listing failure — a clean log proves it was skipped).
func TestDisabledShareFullyIgnored(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	mt := h.nowVal.Add(-10 * time.Minute)

	enabledSrc := "Autorun/Light/Light_enabled.fit"
	h.dev.AddFile(emmc, enabledSrc, data, mt)

	// A second share the operator disabled, carrying a frame that must be ignored.
	h.dev.AddShare(udisk)
	disabledSrc := "ASIAIR/Autorun/Light/Light_disabled.fit"
	h.dev.AddFile(udisk, disabledSrc, data, mt)
	// If the disabled share were ever mounted, this fault would surface as a WARN and
	// a listing failure; a clean log proves the share was never touched.
	h.dev.FailMount(udisk, device.NetworkError("disabled share must not be mounted"))
	h.profile.Sources = append(h.profile.Sources,
		model.Source{Share: udisk, Roots: []string{"Autorun"}, Disabled: true})

	h.cycle()

	if _, ok := h.getFrame(enabledSrc); !ok {
		t.Fatalf("enabled share frame was not pulled; log:\n%s", h.logBuf.String())
	}
	if h.logContains("mount failed") {
		t.Fatalf("disabled share was mounted (mount-failed WARN present); log:\n%s", h.logBuf.String())
	}
	if _, ok, err := h.store.GetFrame(context.Background(), "rig-01", udisk, disabledSrc); err != nil {
		t.Fatalf("GetFrame(udisk): %v", err)
	} else if ok {
		t.Fatalf("disabled share's frame was pulled into the store; want it ignored")
	}
	st := h.status()
	for _, s := range st.Shares {
		if s.Share == udisk {
			t.Fatalf("disabled share appeared in status shares: %+v", st.Shares)
		}
	}
	for _, s := range st.EmptySlots {
		if s == udisk {
			t.Fatalf("disabled (unlisted) share must not appear in EmptySlots: %v", st.EmptySlots)
		}
	}
}

// TestDeleteAndRestoreSkipDisabledShare (D1) pins that the delete and restore passes
// skip frames on a disabled share: its device original is not cleaned and its frame
// is not restored, even though both would act on it were the share enabled.
func TestDeleteAndRestoreSkipDisabledShare(t *testing.T) {
	t.Run("delete pass skips a disabled share", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true

		src := "Autorun/Light/Light_0001.fit"
		h.deleteEligibleFrame(t, src) // three-way provable, grace elapsed → would delete

		// Disable the share the frame lives on. Identity confirms (host-bound), so the
		// ONLY reason nothing is deleted is that the share is disabled.
		h.profile.Sources[0].Disabled = true

		h.w.deleteAllForTest(ctx, h.cfg, h.profile, h.dev, "10.0.0.1")

		if !h.deviceHas(src) {
			t.Fatalf("delete pass cleaned a disabled share's device original")
		}
		if got, _ := h.getFrame(src); got.State != model.StateVerified {
			t.Fatalf("frame state=%s, want verified (untouched)", got.State)
		}
	})

	t.Run("restore pass skips a disabled share", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		ctx := context.Background()
		h.resolver.resolveAddr, h.resolver.resolveOnline = "10.0.0.1", true

		f, _ := fileAndAge(t, h, false)
		if err := os.Remove(f.DestPath); err != nil { // operator culled the NAS copy
			t.Fatal(err)
		}
		h.setMissing(t, f.ID, 0)
		if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
			t.Fatalf("request restore: ok=%v err=%v", ok, err)
		}

		// Disable the share; the restore pass must leave the frame alone.
		h.profile.Sources[0].Disabled = true
		h.w.restorePhase(ctx, h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint("10.0.0.1")), h.dev)

		if _, err := os.Stat(f.DestPath); err == nil {
			t.Fatalf("restore re-pulled a disabled share's frame; want it skipped")
		}
		got, _ := h.getFrame(f.SourcePath)
		if got.MissingSeenAt == 0 || got.RestoreRequestedAt == 0 {
			t.Fatalf("a skipped restore must keep both marks; got missing=%d restore=%d",
				got.MissingSeenAt, got.RestoreRequestedAt)
		}
	})
}

// TestStructurallyEmptySlotSetExcludesTransient (D3) pins that a frame-bearing share
// hit by a transient mediumUsable error lands in absentShares (verdict suppressed)
// but is NOT published in EmptySlots — the structural set the disable guard trusts.
// mediumUsable returns present=false for both a missing stick and a transient read
// error, so keying EmptySlots on present=false would strand a real un-pulled frame.
func TestStructurallyEmptySlotSetExcludesTransient(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	data := fixture(t, "asiair_light_2600mm_ha.fit")
	mt := h.nowVal.Add(-10 * time.Minute)
	src := "ASIAIR/Autorun/Light/Light_0001.fit"
	h.dev.AddFile(udisk, src, data, mt)
	// A transient marker read fault makes mediumUsable report present=false — the same
	// 2-tuple an absent stick yields — though the share plainly listed a frame.
	h.dev.FailReadMarker(udisk, asiairMediaMarker, device.NetworkError("link reset"))

	h.cycle()

	st := h.status()
	// The transient error routes udisk into absentShares, so the verdict is suppressed.
	if st.SafeToPowerOff == model.SafeYes {
		t.Fatalf("a transient medium error must suppress safe; got %q", st.SafeToPowerOff)
	}
	// But the share holds a frame, so it must NOT be in the structurally-empty set.
	for _, s := range st.EmptySlots {
		if s == udisk {
			t.Fatalf("frame-bearing share hit by a transient error must not be in EmptySlots: %v", st.EmptySlots)
		}
	}
}
