package rig

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// TestDeviceFrames pins the pure derivation of the card's device line (design
// frame-lifecycle §14.5): a keep-originals rig folds what it would delete into
// kept, and only a deleting rig with nothing left and nothing pending is drained.
func TestDeviceFrames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		counts  store.DeviceCounts
		pending int
		keep    bool
		want    model.DeviceFrames
	}{
		{"empty device drains", store.DeviceCounts{}, 0, false, model.DeviceFrames{Drained: true}},
		{"uncopied frames are not drained", store.DeviceCounts{}, 3, false, model.DeviceFrames{}},
		{"each reason carried", store.DeviceCounts{InGrace: 3, Due: 1, Culled: 2, Stuck: 4}, 0, false,
			model.DeviceFrames{InGrace: 3, Due: 1, Culled: 2, Stuck: 4}},
		{"stuck alone is not drained", store.DeviceCounts{Stuck: 1}, 0, false, model.DeviceFrames{Stuck: 1}},
		{"keep originals folds the deletable reasons into kept", store.DeviceCounts{InGrace: 3, Due: 1, Culled: 2, Stuck: 4}, 0, true,
			model.DeviceFrames{Kept: 6, Stuck: 4}},
		{"keep originals never drains", store.DeviceCounts{}, 0, true, model.DeviceFrames{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceFrames(tc.counts, tc.pending, tc.keep); got == nil || *got != tc.want {
				t.Fatalf("deviceFrames(%+v, %d, %v) = %+v, want %+v", tc.counts, tc.pending, tc.keep, got, tc.want)
			}
		})
	}
}

// TestDeviceLineSeparatesStuckFromGrace (#29): a held frame beside frames in
// their grace period is counted as stuck, not as one more frame in grace.
func TestDeviceLineSeparatesStuckFromGrace(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	fileLight(t, h, "Autorun/Light/o/Light_0001.fit")
	fileLight(t, h, "Autorun/Light/o/Light_0002.fit")
	held := fileLight(t, h, "Autorun/Light/o/Light_0003.fit")
	held.State = model.StateHeld
	if err := h.store.UpsertFrame(ctx, held); err != nil {
		t.Fatal(err)
	}

	h.cycle()

	want := model.DeviceFrames{InGrace: 2, Stuck: 1}
	if got := h.status().DeviceFrames; got == nil || *got != want {
		t.Fatalf("device frames = %+v, want %+v", got, want)
	}
}

// TestDeviceLineCountsFailedFrames: a frame whose copy keeps failing records no
// dest_path, yet it is on the device and nothing will remove it, so the line
// counts it as stuck rather than hiding as it does for an uncopied-only device.
func TestDeviceLineCountsFailedFrames(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	src := "Autorun/Light/o/Light_0001.fit"
	h.dev.AddFile(emmc, src, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
	h.dev.FailOpen(emmc, errors.New("access denied (test)"))

	for range h.cfg.Defaults.FailedRetry.MaxAttempts {
		h.nowVal = h.nowVal.Add(2 * time.Hour)
		h.cycle()
	}
	if f, ok := h.getFrame(src); !ok || f.State != model.StateFailed || f.DestPath != "" {
		t.Fatalf("precondition: frame = %+v, want failed with no dest_path", f)
	}

	want := model.DeviceFrames{Stuck: 1}
	if got := h.status().DeviceFrames; got == nil || *got != want {
		t.Fatalf("device frames = %+v, want %+v", got, want)
	}
}

// TestDismissBeforeListingKeepsUnchecked: a dismiss on a rig that has not
// completed a listing since startup must not publish counts the device was never
// checked against; the card keeps "Device not checked yet".
func TestDismissBeforeListingKeepsUnchecked(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	f := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Bias/Bias_0001.fit",
		SHA256: "abc", Size: 10, Mtime: h.nowVal.Unix(), State: model.StateUnclassified,
		PulledAt: h.nowVal.Unix(), DestPath: filepath.Join(h.dir, "quarantine", "Bias_0001.fit"),
	}
	if err := h.store.UpsertFrame(ctx, f); err != nil {
		t.Fatal(err)
	}

	if err := h.w.discardOne(ctx, f.ID); err != nil {
		t.Fatalf("discardOne: %v", err)
	}

	if got := h.status().DeviceFrames; got != nil {
		t.Fatalf("device frames = %+v after a dismiss before any listing, want nil (not checked yet)", got)
	}
}

// TestDeviceLineGraceMatchesDeletePass pins the device line's grace cutoff to the
// delete pass's: one second before the rig's grace elapses the frame is in grace
// and stays on the device; at the second it elapses the cycle's status (published
// before its delete pass) counts it due, and that same cycle's delete pass takes it.
func TestDeviceLineGraceMatchesDeletePass(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	f := fileLight(t, h, "Autorun/Light/o/Light_0001.fit")
	grace := h.cfg.EffectiveGrace(h.cfg.Rig("rig-01"))
	pulled := time.Unix(f.PulledAt, 0)

	h.nowVal = pulled.Add(grace - time.Second)
	h.cycle()
	if got, want := h.status().DeviceFrames, (&model.DeviceFrames{InGrace: 1}); got == nil || *got != *want {
		t.Fatalf("a second before grace elapses: device frames = %+v, want %+v", got, want)
	}
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("the delete pass took a frame still inside its grace period")
	}

	h.nowVal = pulled.Add(grace)
	h.cycle()
	if got, want := h.status().DeviceFrames, (&model.DeviceFrames{Due: 1}); got == nil || *got != *want {
		t.Fatalf("as grace elapses: device frames = %+v, want %+v", got, want)
	}
	if h.deviceHas(f.SourcePath) {
		t.Fatal("the line counted the frame due, but the same cycle's delete pass did not take it")
	}
}
