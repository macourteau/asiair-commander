package rig

import (
	"context"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The tests in this file pin the pausable device re-hash (design
// frame-lifecycle §12.8): a Pause abandons the pre-delete hash between
// buffers and the frame defers, and the flag that announces the Pause never
// outlives it, so no later hash is interrupted for a Pause already served.

func (h *harness) hashInterrupts() float64 {
	return h.counterValue("asiair_commander_delete_hash_interrupted_total", map[string]string{"rig": "rig-01"})
}

// assertDeferredIntact checks a frame an interrupted hash deferred: still on the
// device, its row still verified, and nothing on the device changed.
func (h *harness) assertDeferredIntact(t *testing.T, f model.Frame, mutationsBefore int) {
	t.Helper()
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("an interrupted re-hash must never delete the device original")
	}
	if got, ok := h.getFrame(f.SourcePath); !ok || got.State != model.StateVerified {
		t.Fatalf("frame row = %+v, want it still verified", got)
	}
	if got := h.dev.Mutations(); got != mutationsBefore {
		t.Fatalf("device mutations = %d, want %d: an interrupted re-hash changes nothing", got, mutationsBefore)
	}
}

// TestPauseInterruptsDeviceRehash: a Pause sent while the device copy is being
// re-hashed stops the stream at the next buffer; the frame defers intact, the rig
// pauses, and after Resume the frame is proven and deleted as usual.
func TestPauseInterruptsDeviceRehash(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 1)
	f := frames[0]
	h.dev.OnOpenReader = func(_, p string) {
		if p == f.SourcePath {
			h.dev.OnOpenReader = nil
			h.w.Send(Pause)
		}
	}
	before := h.dev.Mutations()

	h.slice(h.nowVal.Add(time.Hour))

	h.assertDeferredIntact(t, f, before)
	if got := h.hashInterrupts(); got != 1 {
		t.Fatalf("hash interrupts = %v, want 1", got)
	}
	if !h.logContains("rig: device re-hash interrupted by pause; deferring") || h.logContains("device re-hash transient error") {
		t.Fatal("an interrupted re-hash should log as such, not as a transient read error")
	}
	if !h.w.paused {
		h.serve(t) // the slice had no later frame boundary; Run's select serves it
	}
	if st := h.status(); !h.w.paused || st.State != model.RigStatePaused {
		t.Fatalf("paused=%v state=%s, want the rig Paused", h.w.paused, st.State)
	}
	if h.w.pauseRequested.Load() {
		t.Fatal("the applied Pause must clear the flag")
	}

	h.w.Send(Resume)
	h.serve(t)

	if h.deviceHas(f.SourcePath) {
		t.Fatal("after Resume the frame should be re-hashed in full and deleted")
	}
	if got := h.hashInterrupts(); got != 1 {
		t.Fatalf("hash interrupts = %v, want still 1", got)
	}
}

// TestContextCancelInterruptsDeviceRehash: the worker's context ending mid-stream
// abandons the hash the same way, and the frame defers intact.
func TestContextCancelInterruptsDeviceRehash(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	f := h.seedEligible(t, 1)[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.dev.OnOpenReader = func(_, p string) {
		if p == f.SourcePath {
			cancel()
		}
	}
	before := h.dev.Mutations()

	h.w.deletePhase(ctx, h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint(sliceAddr)), h.dev, h.nowVal.Add(time.Hour))

	h.assertDeferredIntact(t, f, before)
	if got := h.hashInterrupts(); got != 1 {
		t.Fatalf("hash interrupts = %v, want 1", got)
	}
	// A shutdown is not a pause, so it must not be logged as one.
	if !h.logContains("interrupted by shutdown") || h.logContains("interrupted by pause") {
		t.Fatal("a ctx-cancel interrupt should log as a shutdown, not a pause")
	}
}

// TestDroppedPauseClearsRehashFlag: a Pause dropped on a full command channel is
// never applied, so nothing else would clear its flag; Send clears it itself and
// records no request, and the next hash runs to completion.
func TestDroppedPauseClearsRehashFlag(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	f := h.seedEligible(t, 1)[0]
	h.w.publish()
	wantRequested := h.status().Requested
	for len(h.w.cmds) < cap(h.w.cmds) {
		h.w.cmds <- ReloadConfig // a wedged worker's backlog
	}

	h.w.Send(Pause)

	if h.w.pauseRequested.Load() {
		t.Fatal("a dropped Pause must leave the flag clear")
	}
	if got := h.status().Requested; got != wantRequested {
		t.Fatalf("requested = %q, want %q untouched by a dropped Pause", got, wantRequested)
	}
	for len(h.w.cmds) > 0 {
		<-h.w.cmds
	}

	h.slice(h.nowVal.Add(time.Hour))

	if h.deviceHas(f.SourcePath) || h.hashInterrupts() != 0 {
		t.Fatalf("the next hash should complete and delete (interrupts = %v)", h.hashInterrupts())
	}
}

// TestAppliedPauseClearsRehashFlag: however a Pause is delivered, applying it
// clears the flag, so a Run now on the paused rig afterwards hashes and deletes.
func TestAppliedPauseClearsRehashFlag(t *testing.T) {
	cases := []struct {
		name    string
		deliver func(h *harness)
	}{
		{"Run's select (apply)", func(h *harness) { h.serve(h.t) }},
		{"drainCommands", func(h *harness) { h.w.drainCommands(context.Background()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			h.roomy()
			f := h.seedEligible(t, 1)[0]

			h.w.Send(Pause)
			if !h.w.pauseRequested.Load() {
				t.Fatal("Send(Pause) should set the flag")
			}
			tc.deliver(h)
			if !h.w.paused || h.w.pauseRequested.Load() {
				t.Fatalf("paused=%v flag=%v, want the Pause applied and its flag cleared", h.w.paused, h.w.pauseRequested.Load())
			}

			h.runNow() // starts the run with its first cycle

			if h.deviceHas(f.SourcePath) {
				t.Fatal("a Run now after the Pause was applied should hash and delete")
			}
			if got := h.hashInterrupts(); got != 0 {
				t.Fatalf("hash interrupts = %v, want 0", got)
			}
		})
	}
}

// TestResumeClearsRehashFlag: a Pause and a Resume both still queued leave no
// flag behind, so a hash before either is served is not interrupted.
func TestResumeClearsRehashFlag(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	f := h.seedEligible(t, 1)[0]

	h.w.Send(Pause)
	h.w.Send(Resume)
	if h.w.pauseRequested.Load() {
		t.Fatal("Send(Resume) should clear the flag")
	}
	for len(h.w.cmds) > 0 {
		<-h.w.cmds // neither served: the hash below runs before both
	}

	h.slice(h.nowVal.Add(time.Hour))

	if h.deviceHas(f.SourcePath) || h.hashInterrupts() != 0 {
		t.Fatalf("the hash should complete and delete (interrupts = %v)", h.hashInterrupts())
	}
}
