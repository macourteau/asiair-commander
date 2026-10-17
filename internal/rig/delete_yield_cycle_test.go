package rig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
	"github.com/macourteau/asiair-commander/internal/store"
)

// The tests in this file drive delete slices through runCycle: the deadline and
// debt it computes, the delay it returns, and what the card shows between slices
// (design frame-lifecycle §12.1, §12.2, §12.4, §12.6).

// setFree sets the device's free space. The fake reports 0 free by default, which
// is tight, so every test that means a roomy device says so.
func (h *harness) setFree(free int64) { h.dev.SetStatfs(emmc, free, 4*free) }

// roomy leaves the device above the delete-pressure threshold; tight below it but
// above the reserve, so no device-low alert muddies the case.
func (h *harness) roomy() { h.setFree(3 * h.cfg.DiskReserveBytes) }
func (h *harness) tight() { h.setFree(3 * h.cfg.DiskReserveBytes / 2) }

// runCycleAndWait runs one cycle and then lets the delay it returned pass, as
// Run's ticker would, returning that delay.
func (h *harness) runCycleAndWait() time.Duration {
	next := h.w.runCycle(context.Background())
	h.nowVal = h.nowVal.Add(next)
	return next
}

// onDevice counts the frames still on the device.
func (h *harness) onDevice(frames []model.Frame) int {
	n := 0
	for _, f := range frames {
		if h.deviceHas(f.SourcePath) {
			n++
		}
	}
	return n
}

// newCapture is a real light frame the pull path can file.
const newCapture = "Autorun/Light/C-2025 A6/Light_NGC_0001.fit"

func (h *harness) addCapture(t *testing.T) {
	t.Helper()
	h.dev.AddFile(emmc, newCapture, fixture(t, "asiair_light_2600mm_ha.fit"), h.nowVal.Add(-10*time.Minute))
}

// TestDeletePassYieldsAtDeadline: on a slow Wi-Fi link a cycle deletes one
// frame, then comes back after 1 s instead of a full poll, with the countdown and
// the pill telling the truth in between; the readout counts across the drain,
// grows with a frame that becomes eligible mid-drain, and clears once a whole
// sweep deletes nothing.
func TestDeletePassYieldsAtDeadline(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 5)
	h.slowLink(slowLinkRead)
	rec := h.recordStatuses()

	next := h.w.runCycle(context.Background())

	if next != time.Second {
		t.Fatalf("first cycle returned %v, want the 1 s continuation", next)
	}
	if got := 5 - h.onDevice(frames); got != 1 {
		t.Fatalf("first cycle deleted %d frames, want exactly 1", got)
	}
	st := h.status()
	if st.State == model.RigStateDeleting {
		t.Fatal("the pill still reads Deleting after the slice")
	}
	if want := h.nowVal.Add(next).Unix(); st.NextPollAt != want {
		t.Fatalf("NextPollAt = %d, want now + the returned delay = %d", st.NextPollAt, want)
	}
	if poll := int(h.cfg.EffectivePollInterval(h.profile) / time.Second); st.PollIntervalSec != poll {
		t.Fatalf("PollIntervalSec = %d, want the configured cadence %d", st.PollIntervalSec, poll)
	}
	if h.yields() != 1 {
		t.Fatalf("yield counter %v, want 1", h.yields())
	}
	if got := cleanupsOf(rec.take()); !slices.Equal(got, []model.CleanupProgress{{File: filepath.Base(frames[0].SourcePath), Index: 1, Total: 5}}) {
		t.Fatalf("first readout %+v, want 1 of 5", got)
	}
	h.nowVal = h.nowVal.Add(next)

	// The drain goes on one frame per cycle; after the second a sixth frame becomes
	// eligible and joins the total.
	all := append([]model.Frame(nil), frames...)
	wantTotal := 5
	for n := 2; n <= 6; n++ {
		if n == 3 {
			all = append(all, h.seedEligibleFrame(t, 5))
			wantTotal = 6
		}
		next = h.runCycleAndWait()
		if got := len(all) - h.onDevice(all); got != n {
			t.Fatalf("after cycle %d: %d frames deleted, want %d", n, got, n)
		}
		want := []model.CleanupProgress{{File: filepath.Base(all[n-1].SourcePath), Index: n, Total: wantTotal}}
		if got := cleanupsOf(rec.take()); !slices.Equal(got, want) {
			t.Fatalf("cycle %d readout %+v, want %+v", n, got, want)
		}
		if st := h.status(); st.Cleanup != nil {
			t.Fatalf("cycle %d: readout %+v still shown between slices", n, st.Cleanup)
		}
		wantNext := time.Second
		if n == 6 {
			wantNext = h.cfg.EffectivePollInterval(h.profile) // the last slice exhausted the list
		}
		if next != wantNext {
			t.Fatalf("cycle %d returned %v, want %v", n, next, wantNext)
		}
	}
	// Each listing also issues one Delete of the vestigial identity marker, so the
	// six cycles account for six mutations beside the six proven originals.
	if got := h.dev.Mutations(); got != 6+6 {
		t.Fatalf("device mutations = %d, want the 6 proven originals plus 6 listings' marker cleanup", got)
	}
	for _, f := range all {
		if got, _ := h.getFrame(f.SourcePath); got.State != model.StateDeleted {
			t.Fatalf("%s state %s, want deleted", f.SourcePath, got.State)
		}
	}

	// The last slice closed the sweep that held the last delete; the next, empty,
	// sweep deletes nothing and ends the drain.
	if h.w.drainDone != 6 {
		t.Fatalf("drain count %d before a clean sweep, want 6", h.w.drainDone)
	}
	h.runCycleAndWait()
	if h.w.drainDone != 0 {
		t.Fatalf("drain count %d after a whole sweep deleted nothing, want 0", h.w.drainDone)
	}
}

// TestDrainCountResets: frames that never conclude, or that leave eligibility,
// do not hold the drain open: once a whole sweep deletes nothing the readout
// clears, and the next drain counts from 1 again.
func TestDrainCountResets(t *testing.T) {
	t.Run("last frames defer on every visit", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		failVerifyRead(t, frames[1])
		failVerifyRead(t, frames[2])
		h.slowLink(slowLinkRead)

		h.runCycleAndWait() // deletes frame 0
		if h.w.drainDone != 1 {
			t.Fatalf("drain count %d after the first delete, want 1", h.w.drainDone)
		}
		cycles := 0
		for h.w.drainDone != 0 {
			if cycles++; cycles > 6 {
				t.Fatalf("drain still open after %d cycles of deferrals", cycles)
			}
			h.runCycleAndWait()
		}
		if !h.deviceHas(frames[1].SourcePath) || !h.deviceHas(frames[2].SourcePath) {
			t.Fatal("a frame that never verifies must never be deleted")
		}

		// A new drain: two more frames become eligible beside the two stuck ones.
		rec := h.recordStatuses()
		h.seedEligibleFrame(t, 3)
		h.seedEligibleFrame(t, 4)
		for range 4 {
			h.runCycleAndWait()
			if got := cleanupsOf(rec.take()); len(got) > 0 {
				if got[0].Index != 1 || got[0].Total != 4 {
					t.Fatalf("next drain's first readout %+v, want 1 of 4", got[0])
				}
				return
			}
		}
		t.Fatal("the next drain never published a readout")
	})

	t.Run("last frames leave eligibility", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)

		h.runCycleAndWait() // deletes frame 0
		// Both remaining frames are held: neither is eligible now.
		for _, f := range frames[1:] {
			held, _ := h.getFrame(f.SourcePath)
			held.State = model.StateHeld
			if err := h.store.UpsertFrame(context.Background(), held); err != nil {
				t.Fatal(err)
			}
		}
		for range 3 {
			h.runCycleAndWait()
		}
		if h.w.drainDone != 0 {
			t.Fatalf("drain count %d with nothing left eligible, want 0", h.w.drainDone)
		}
	})
}

// TestNewFramePulledMidDrain: a capture that lands on the device while a backlog
// drains is listed, settles and is filed before the backlog is gone, and every
// cycle lists afresh.
func TestNewFramePulledMidDrain(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 5)
	h.slowLink(slowLinkRead)

	h.runCycleAndWait()
	lastTS := h.status().ListingTS
	h.addCapture(t)

	filed := false
	for n := 2; h.onDevice(frames) > 0; n++ {
		if n > 10 {
			t.Fatal("the backlog did not drain")
		}
		h.runCycleAndWait()
		if ts := h.status().ListingTS; ts <= lastTS {
			t.Fatalf("cycle %d: ListingTS %d did not advance past %d", n, ts, lastTS)
		} else {
			lastTS = ts
		}
		if f, ok := h.getFrame(newCapture); ok && f.State == model.StateVerified && h.onDevice(frames) > 0 {
			filed = true
		}
	}
	if !filed {
		t.Fatal("the new capture was not filed while the backlog was still draining")
	}
}

// TestSettleSpacingPreserved: the 1 s continuation never brings listings closer
// than one poll, so settle_cycles keeps its wall-time meaning: a frame first seen
// in one listing is pulled from a listing at least a poll later.
func TestSettleSpacingPreserved(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.cfg.Defaults.SettleCycles = 1 // one more stable sighting before a pull
	h.roomy()
	poll := h.cfg.EffectivePollInterval(h.profile)
	// Short reads, so a slice fills its deadline with deletes and yields right at it:
	// the tightest spacing the continuation can produce.
	h.seedEligible(t, 20)
	h.slowLink(9 * time.Second)

	var starts []int64
	firstSeen, pulledAt := int64(0), int64(0)
	for n := 1; n <= 6; n++ {
		if n == 2 {
			h.addCapture(t)
		}
		h.runCycleAndWait()
		ts := h.status().ListingTS
		starts = append(starts, ts)
		if firstSeen == 0 && h.status().WaitingToSettle > 0 {
			firstSeen = ts
		}
		if f, ok := h.getFrame(newCapture); ok && f.State == model.StateVerified && pulledAt == 0 {
			pulledAt = ts
		}
	}
	for i := 1; i < len(starts); i++ {
		if gap := time.Duration(starts[i]-starts[i-1]) * time.Second; gap < poll {
			t.Fatalf("listings %d and %d started %v apart, want at least a poll (%v)", i, i+1, gap, poll)
		}
	}
	if firstSeen == 0 || pulledAt == 0 {
		t.Fatalf("precondition: new capture first seen at %d, pulled at %d", firstSeen, pulledAt)
	}
	if gap := time.Duration(pulledAt-firstSeen) * time.Second; gap < poll {
		t.Fatalf("pulled %v after it was first seen, want at least a poll (%v)", gap, poll)
	}
}

// TestDeviceTightFairShare calibrates design frame-lifecycle §12.1's fair share:
// poll 45 s, a pull phase of 67.5 s and 20 s per device read. A delete costs one
// device read (the re-hash; VerifyRead reads the NAS), so a tight device's 67.5 s
// slice starts deletes at 0, 20, 40 and 60 s and lands four, where a roomy device,
// whose deadline the pull already passed, gets only the one-delete floor.
func TestDeviceTightFairShare(t *testing.T) {
	const pullRead, deleteRead = 67500 * time.Millisecond, 20 * time.Second
	run := func(t *testing.T, setup func(h *harness)) (h *harness, deleted int) {
		h = newHarness(t, emmc, header.New())
		if poll := h.cfg.EffectivePollInterval(h.profile); poll != 45*time.Second {
			t.Fatalf("test assumes a 45s poll, got %v", poll)
		}
		frames := h.seedEligible(t, 8)
		h.addCapture(t)
		h.dev.OnOpenReader = func(_, p string) {
			if p == newCapture {
				h.nowVal = h.nowVal.Add(pullRead)
			} else {
				h.nowVal = h.nowVal.Add(deleteRead)
			}
		}
		setup(h)
		h.runCycleAndWait()
		if f, ok := h.getFrame(newCapture); !ok || f.State != model.StateVerified {
			t.Fatal("precondition: the pull phase should have filed the new capture")
		}
		return h, 8 - h.onDevice(frames)
	}
	pressureLogs := func(h *harness) int { return strings.Count(h.logBuf.String(), "device delete pressure changed") }

	t.Run("tight", func(t *testing.T) {
		h, deleted := run(t, func(h *harness) { h.tight() })
		if deleted != 4 {
			t.Fatalf("tight device deleted %d in the slice, want 4", deleted)
		}
		h.runCycleAndWait()
		if got := pressureLogs(h); got != 1 || !h.logContains("reason=tight") {
			t.Fatalf("pressure change logged %d times (want once, reason=tight); log:\n%s", got, h.logBuf.String())
		}
	})
	t.Run("roomy", func(t *testing.T) {
		h, deleted := run(t, func(h *harness) { h.roomy() })
		if deleted != 1 {
			t.Fatalf("roomy device deleted %d past an overrun deadline, want the floor of 1", deleted)
		}
		if got := pressureLogs(h); got != 0 {
			t.Fatalf("pressure change logged %d times on a device that was never tight", got)
		}
	})
	t.Run("unmeasurable", func(t *testing.T) {
		h, deleted := run(t, func(h *harness) { h.dev.FailStatfs(emmc, errors.New("statfs failed")) })
		if deleted != 4 {
			t.Fatalf("unmeasurable device deleted %d, want the tight share of 4", deleted)
		}
		if got := pressureLogs(h); got != 1 || !h.logContains("reason=unknown") {
			t.Fatalf("pressure change logged %d times (want once, reason=unknown)", got)
		}
	})
}

// TestDeviceTightDebtCapped: after one pull phase of 20 polls on a tight device,
// the debt is repaid over capped slices, each followed by a listing, until the
// deletes have had as much link time as the pull.
func TestDeviceTightDebtCapped(t *testing.T) {
	const deleteRead = 20 * time.Second
	setup := func(t *testing.T, n int) (*harness, time.Duration, *time.Duration) {
		h := newHarness(t, emmc, header.New())
		h.tight()
		poll := h.cfg.EffectivePollInterval(h.profile)
		h.seedEligible(t, n)
		h.addCapture(t)
		pulled := new(time.Duration) // pull-phase link time of the cycle in progress
		h.dev.OnOpenReader = func(_, p string) {
			if p == newCapture {
				h.nowVal = h.nowVal.Add(20 * poll)
				*pulled += 20 * poll
				return
			}
			h.nowVal = h.nowVal.Add(deleteRead)
		}
		return h, poll, pulled
	}

	t.Run("slices are capped and catch up", func(t *testing.T) {
		h, poll, pulled := setup(t, 60)
		var sliceTotal, pullTotal time.Duration
		lastTS := int64(0)
		caughtUp := 0
		for n := 1; n <= 12; n++ {
			*pulled = 0
			start := h.nowVal
			h.w.runCycle(context.Background())
			slice := h.nowVal.Sub(start) - *pulled // listing and restore take no time here
			sliceTotal += slice
			pullTotal += *pulled
			if slice > 2*poll+deleteRead {
				t.Fatalf("cycle %d: slice ran %v, more than 2 × poll plus one frame", n, slice)
			}
			if ts := h.status().ListingTS; ts <= lastTS {
				t.Fatalf("cycle %d: ListingTS %d did not advance past %d", n, ts, lastTS)
			} else {
				lastTS = ts
			}
			if caughtUp == 0 && sliceTotal >= pullTotal {
				caughtUp = n
			}
			h.nowVal = h.nowVal.Add(time.Second)
		}
		if caughtUp == 0 || caughtUp > 11 {
			t.Fatalf("deletes caught up with the %v pull at cycle %d, want within 11 cycles", pullTotal, caughtUp)
		}
	})

	t.Run("going roomy clears the debt", func(t *testing.T) {
		h, _, _ := setup(t, 60)
		h.runCycleAndWait()
		if h.w.deleteDebt == 0 {
			t.Fatal("precondition: the long pull should have left a debt")
		}
		h.roomy()
		h.runCycleAndWait()
		if h.w.deleteDebt != 0 {
			t.Fatalf("debt %v on a roomy device, want 0", h.w.deleteDebt)
		}
	})

	t.Run("an exhausted list clears the debt", func(t *testing.T) {
		h, _, _ := setup(t, 3)
		h.runCycleAndWait()
		if h.w.deleteDebt != 0 {
			t.Fatalf("debt %v after the slice exhausted its list, want 0", h.w.deleteDebt)
		}
	})

	t.Run("an unconfirmed identity keeps the debt", func(t *testing.T) {
		h, _, _ := setup(t, 60)
		h.runCycleAndWait()
		owed := h.w.deleteDebt
		h.profile.GUID = "dev-guid"
		h.resolver.probeOK = false
		const listed = 10 * time.Second

		pass := h.w.deleteSlice(context.Background(), h.cfg, h.w.newCycle(h.profile, netsite.LocalEndpoint(sliceAddr)), h.dev, h.nowVal.Add(-listed), h.cfg.EffectivePollInterval(h.profile))

		// A prologue return; the unconfirmed probe is reported for the stall count.
		if pass != (slicePass{unconfirmed: true}) {
			t.Fatalf("result %+v, want a prologue return", pass)
		}
		if h.w.deleteDebt != owed+listed {
			t.Fatalf("debt %v after an unconfirmed slice, want %v owed plus this cycle's %v", h.w.deleteDebt, owed, listed)
		}
	})

	t.Run("a pause between slices keeps the debt", func(t *testing.T) {
		h, _, _ := setup(t, 60)
		h.runCycleAndWait()
		owed := h.w.deleteDebt
		h.w.setPaused(true)
		h.runCycleAndWait()
		if h.w.deleteDebt != owed {
			t.Fatalf("debt %v after a paused cycle, want %v", h.w.deleteDebt, owed)
		}
	})
}

// TestDrainCompletionRestoresPillAndPoll: the slice that empties the backlog
// hands the pill back to the listing's state, goes back to the poll cadence, and
// resets the cursor.
func TestDrainCompletionRestoresPillAndPoll(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool // a frame is ready to copy at the final cycle's listing
		want  model.RigState
	}{
		{"nothing ready to copy", false, model.RigStateIdle},
		{"a frame ready to copy", true, model.RigStatePulling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			h.roomy()
			frames := h.seedEligible(t, 2)
			h.slowLink(slowLinkRead)
			h.runCycleAndWait()
			if tc.ready {
				// The capture's copy starts and then finds its source changed: it is
				// settled and admitted but not filed, so it is still ready to copy.
				h.addCapture(t)
				h.dev.OnOpenReader = func(_, p string) {
					h.nowVal = h.nowVal.Add(slowLinkRead)
					if p == newCapture {
						h.dev.SetMtime(emmc, newCapture, h.nowVal)
					}
				}
			}

			next := h.w.runCycle(context.Background())

			if h.onDevice(frames) != 0 {
				t.Fatal("precondition: the final slice should have drained the backlog")
			}
			if poll := h.cfg.EffectivePollInterval(h.profile); next != poll {
				t.Fatalf("returned %v, want the poll %v", next, poll)
			}
			st := h.status()
			if st.State != tc.want {
				t.Fatalf("state %q after the final slice, want the listing's %q", st.State, tc.want)
			}
			if want := h.nowVal.Add(next).Unix(); st.NextPollAt != want {
				t.Fatalf("NextPollAt = %d, want now + poll = %d", st.NextPollAt, want)
			}
			if st.Draining {
				t.Fatal("still draining after the backlog emptied")
			}
			if h.w.deleteCursor != (deleteCursor{}) {
				t.Fatalf("cursor %+v, want reset", h.w.deleteCursor)
			}
		})
	}
}

// TestSafeVerdictHoldsThroughDrain: a rig reading Safe to power off keeps reading
// it through a sliced drain whose only device changes are its own deletes, and
// any other change still reads Working.
func TestSafeVerdictHoldsThroughDrain(t *testing.T) {
	// idleRig brings a rig with a grace-held backlog to Safe = Yes, then lets grace
	// expire so the drain can start.
	idleRig := func(t *testing.T) (*harness, []model.Frame) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 5)
		// The capture folder outlives its last frame, as on a real device; the fake
		// would otherwise drop the emptied root and fail the listing.
		h.dev.AddDir(emmc, "Autorun/Light", h.nowVal.Add(-3*time.Hour))
		longGrace := model.Duration(3 * time.Hour)
		h.profile.Grace = &longGrace
		h.runCycleAndWait()
		h.runCycleAndWait()
		if st := h.status(); st.SafeToPowerOff != model.SafeYes {
			t.Fatalf("precondition: an idle rig reads %q, want Yes", st.SafeToPowerOff)
		}
		h.profile.Grace = nil // back to the default hour: the drain can start
		h.slowLink(slowLinkRead)
		return h, frames
	}

	t.Run("own deletes only", func(t *testing.T) {
		h, frames := idleRig(t)
		for n := 1; h.onDevice(frames) > 0; n++ {
			if n > 8 {
				t.Fatal("the backlog did not drain")
			}
			h.runCycleAndWait()
			if st := h.status(); st.SafeToPowerOff != model.SafeYes {
				t.Fatalf("cycle %d of the drain reads %q (%s), want Yes", n, st.SafeToPowerOff, st.PowerOffReason)
			}
		}
		h.runCycleAndWait()
		if st := h.status(); st.SafeToPowerOff != model.SafeYes {
			t.Fatalf("after the drain reads %q (%s), want Yes", st.SafeToPowerOff, st.PowerOffReason)
		}
	})

	t.Run("a new capture mid-drain", func(t *testing.T) {
		h, _ := idleRig(t)
		h.runCycleAndWait()
		h.addCapture(t)
		h.runCycleAndWait()
		if st := h.status(); st.SafeToPowerOff != model.SafeWorking {
			t.Fatalf("listing with a new capture reads %q, want Working", st.SafeToPowerOff)
		}
	})

	t.Run("a deleted path rewritten between slices", func(t *testing.T) {
		h, frames := idleRig(t)
		h.runCycleAndWait()
		if h.deviceHas(frames[0].SourcePath) {
			t.Fatal("precondition: the first slice should have deleted frame 0")
		}
		h.dev.AddFile(emmc, frames[0].SourcePath, []byte("a new capture at a reused path"), h.nowVal)
		h.runCycleAndWait()
		if st := h.status(); st.SafeToPowerOff != model.SafeWorking {
			t.Fatalf("listing with a rewritten path reads %q, want Working", st.SafeToPowerOff)
		}
	})
}

// TestCullDrainYieldsAndStaysSafe: a drain made only of culled frames, every one
// deleted through handleAbsentNAS, is bounded like any other: its hash counts as
// the slice's streamed visit, so the slice yields after it. The verdict follows
// the only-copy rule, as an unsliced pass does: after the first listing it never
// reads Working, it reads Unknown/missing_on_device while a culled original is
// still listed, and Yes once they are all gone.
func TestCullDrainYieldsAndStaysSafe(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.linkRoot(t, h.cfg.Roots.Light)
	frames := h.seedEligible(t, 3)
	for _, f := range frames {
		if err := os.Remove(f.DestPath); err != nil {
			t.Fatal(err)
		}
		h.setMissing(t, f.ID, int64((48 * time.Hour).Seconds())) // culled, well past settle
	}
	// The capture folder outlives its last frame, as on a real device; the fake
	// would otherwise drop the emptied root and fail the listing.
	h.dev.AddDir(emmc, "Autorun/Light", h.nowVal.Add(-3*time.Hour))
	h.slowLink(slowLinkRead)

	for n := 1; n <= 3; n++ {
		// What this cycle's listing will see: the originals still on the device.
		listed := h.onDevice(frames)
		reads := h.dev.ContentReads()
		next := h.runCycleAndWait()
		if got := h.dev.ContentReads() - reads; got != 1 {
			t.Fatalf("cycle %d streamed %d device originals, want 1", n, got)
		}
		if got := 3 - h.onDevice(frames); got != n {
			t.Fatalf("after cycle %d: %d culled frames deleted, want %d", n, got, n)
		}
		wantNext := time.Second
		if n == 3 {
			wantNext = h.cfg.EffectivePollInterval(h.profile)
		}
		if next != wantNext {
			t.Fatalf("cycle %d returned %v, want %v", n, next, wantNext)
		}
		st := h.status()
		if n > 1 && st.SafeToPowerOff == model.SafeWorking {
			t.Fatalf("cycle %d reads Working after the first listing", n)
		}
		if n > 1 && listed > 0 && (st.SafeToPowerOff != model.SafeUnknown || st.PowerOffReason != "missing_on_device") {
			t.Fatalf("cycle %d listed %d culled originals and reads %q (%s), want Unknown/missing_on_device",
				n, listed, st.SafeToPowerOff, st.PowerOffReason)
		}
	}
	if h.yields() != 2 {
		t.Fatalf("yield counter %v, want 2", h.yields())
	}
	if got := h.counterValue("asiair_commander_frames_deleted_total", map[string]string{"manner": "culled"}); got != 3 {
		t.Fatalf("culled deletes = %v, want 3", got)
	}

	// The next listing finds no culled original left: the only-copy rule no longer
	// applies and the quiet device reads Yes.
	h.runCycleAndWait()
	if st := h.status(); st.SafeToPowerOff != model.SafeYes {
		t.Fatalf("with every culled original gone the verdict reads %q (%s), want Yes", st.SafeToPowerOff, st.PowerOffReason)
	}
}

// TestSweepCountedOncePerRotation: with one frame visited per slice, a sweep of
// three stuck frames takes three cycles, and the count advances once per sweep —
// the slice that stops at the tail and the one that resumes at the head do not
// both count the same boundary.
func TestSweepCountedOncePerRotation(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 3)
	for _, f := range frames {
		failVerifyRead(t, f) // streams and defers on every visit
	}
	h.slowLink(slowLinkRead)

	want := []int{0, 0, 0, 1, 1, 1, 2}
	for n, w := range want {
		h.runCycleAndWait()
		if h.w.sweeps != w {
			t.Fatalf("after cycle %d: %d sweeps, want %d", n+1, h.w.sweeps, w)
		}
	}
}

// TestWithoutKeys: the own-delete discount removes exact (share, path) matches
// only.
func TestWithoutKeys(t *testing.T) {
	lf := func(share, p string) listedFrame { return listedFrame{share: share, path: p} }
	frames := []listedFrame{lf(emmc, "a.fit"), lf(emmc, "a.fits"), lf(udisk, "a.fit"), lf(emmc, "b.fit")}
	cases := []struct {
		name string
		keys map[string]bool
		want []listedFrame
	}{
		{"a.fit never removes a.fits", map[string]bool{listingKey(emmc, "a.fit"): true},
			[]listedFrame{lf(emmc, "a.fits"), lf(udisk, "a.fit"), lf(emmc, "b.fit")}},
		{"the same path on another share is kept", map[string]bool{listingKey(udisk, "a.fit"): true},
			[]listedFrame{lf(emmc, "a.fit"), lf(emmc, "a.fits"), lf(emmc, "b.fit")}},
		{"a prefix removes nothing", map[string]bool{listingKey(emmc, "a"): true}, frames},
		{"no keys", map[string]bool{}, frames},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withoutKeys(frames, tc.keys)
			if !slices.EqualFunc(got, tc.want, func(a, b listedFrame) bool { return a.share == b.share && a.path == b.path }) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDrainingIndication: the published status says a backlog is draining for
// exactly as long as slices keep yielding, and a Pause clears it at once.
func TestDrainingIndication(t *testing.T) {
	t.Run("set while slices yield, cleared when the drain completes", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		for n := 1; n <= 2; n++ {
			h.runCycleAndWait()
			if !h.status().Draining {
				t.Fatalf("cycle %d yielded with frames left but does not read draining", n)
			}
		}
		h.runCycleAndWait()
		if h.onDevice(frames) != 0 {
			t.Fatal("precondition: the third cycle should have drained the backlog")
		}
		if h.status().Draining {
			t.Fatal("still draining after the drain completed")
		}
	})

	t.Run("a pause clears it at once", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		h.runCycleAndWait()
		if !h.status().Draining {
			t.Fatal("precondition: the first cycle should have yielded")
		}
		h.w.apply(context.Background(), Pause)
		if st := h.status(); st.Draining || !st.Paused {
			t.Fatalf("after a Pause draining=%v paused=%v, want not draining and paused", st.Draining, st.Paused)
		}
	})
}

// TestContinuationSurvivesReloadConfig drives Run: a config reload served in the
// 1 s gap after a yielding slice runs no cycle, so it must not push the next
// slice out to a full poll.
func TestContinuationSurvivesReloadConfig(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 3)
	// The worker goroutine owns the clock from here on; the test only watches the
	// reads it reports.
	reads := make(chan string, 16)
	h.dev.OnOpenReader = func(_, p string) {
		h.nowVal = h.nowVal.Add(slowLinkRead)
		reads <- p
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	if p := <-reads; p != frames[0].SourcePath {
		t.Fatalf("first slice read %s, want frame 0", p)
	}
	// Wait for the first cycle to end: its closing publish is the one that says
	// the backlog is draining.
	deadline := time.Now().Add(5 * time.Second)
	for !h.status().Draining {
		if time.Now().After(deadline) {
			t.Fatal("the first cycle never published the draining indication")
		}
		time.Sleep(time.Millisecond)
	}
	h.w.Send(ReloadConfig)

	select {
	case p := <-reads:
		if p != frames[1].SourcePath {
			t.Fatalf("second slice read %s, want frame 1", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no second slice within 10 s: the reload pushed the continuation out to a full poll")
	}
	if !h.logContains("config reload signalled") {
		t.Fatal("precondition: the reload should have been served by Run's select, not a cycle's drain")
	}
}

// TestReloadConfigReschedulesWithoutContinuation: with no continuation pending,
// a config reload still resets Run's ticker to the poll interval it reads, so a
// shortened poll_interval applies from the reload rather than after the old one.
func TestReloadConfigReschedulesWithoutContinuation(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.cfg.Defaults.PollInterval = model.Duration(time.Hour)
	cycles := func() int { return strings.Count(h.logBuf.String(), "rig: delete slice starting") }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("the initial cycle", func() bool { return cycles() == 1 })
	if h.status().Draining {
		t.Fatal("precondition: nothing to delete, so no continuation is pending")
	}
	// A saved config is a fresh snapshot: the worker goroutine reads it, so it is
	// swapped in whole rather than mutated in place.
	next := *h.cfg
	next.Defaults.PollInterval = model.Duration(time.Second)
	h.cfgPtr.Store(&next)
	h.w.Send(ReloadConfig)

	waitFor("a cycle on the reloaded 1 s poll, not the old hour", func() bool { return cycles() >= 2 })
}

// TestListingPillReadsReadyWork: the listing reads Transferring only when a frame
// is ready to copy; frames that are only settling or held leave it at Monitoring
// while the Safe verdict still reads Working.
func TestListingPillReadsReadyWork(t *testing.T) {
	t.Run("settling only", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.cfg.Defaults.SettleCycles = 1
		h.roomy()
		h.runCycleAndWait()
		h.addCapture(t)
		rec := h.recordStatuses()
		h.runCycleAndWait()
		st := h.status()
		if st.WaitingToSettle != 1 {
			t.Fatalf("precondition: waiting to settle %d, want 1", st.WaitingToSettle)
		}
		if st.State != model.RigStateIdle || st.SafeToPowerOff != model.SafeWorking {
			t.Fatalf("state %q safe %q, want Idle and Working", st.State, st.SafeToPowerOff)
		}
		if slices.ContainsFunc(rec.take(), func(p model.RigStatus) bool { return p.State == model.RigStatePulling }) {
			t.Fatal("a walk with nothing to copy published Pulling")
		}
	})

	t.Run("held only", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		mt := h.nowVal.Add(-time.Hour)
		data := []byte("a held frame's source")
		h.dev.AddFile(emmc, "Autorun/Light/held.fit", data, mt)
		if err := h.store.UpsertFrame(context.Background(), &model.Frame{
			RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Light/held.fit",
			SHA256: sha256hex(data), Size: int64(len(data)), Mtime: mt.Unix(),
			State: model.StateHeld, PulledAt: mt.Unix(),
		}); err != nil {
			t.Fatal(err)
		}
		rec := h.recordStatuses()
		h.runCycleAndWait()
		h.runCycleAndWait()
		if st := h.status(); st.State != model.RigStateIdle || st.SafeToPowerOff != model.SafeWorking {
			t.Fatalf("state %q safe %q, want Idle and Working", st.State, st.SafeToPowerOff)
		}
		if slices.ContainsFunc(rec.take(), func(p model.RigStatus) bool { return p.State == model.RigStatePulling }) {
			t.Fatal("a walk with nothing to copy published Pulling")
		}
	})

	t.Run("ready to copy", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		// The copy starts and then finds its source changed, so the frame is still
		// ready to copy when the listing publishes.
		h.dev.OnOpenReader = func(_, p string) { h.dev.SetMtime(emmc, p, h.nowVal) }
		h.runCycleAndWait()
		if st := h.status(); st.State != model.RigStatePulling {
			t.Fatalf("state %q with a frame ready to copy, want Pulling", st.State)
		}
	})

	t.Run("filed this cycle", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		rec := h.recordStatuses()
		h.runCycleAndWait()
		if f, ok := h.getFrame(newCapture); !ok || f.State != model.StateVerified {
			t.Fatal("precondition: the capture should have been filed")
		}
		if !slices.ContainsFunc(rec.take(), func(p model.RigStatus) bool { return p.State == model.RigStatePulling }) {
			t.Fatal("the transfer should have shown Pulling while it ran")
		}
		if st := h.status(); st.State != model.RigStateIdle {
			t.Fatalf("state %q once everything was filed, want Idle", st.State)
		}
	})

	t.Run("admission turned it away", func(t *testing.T) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.addCapture(t)
		h.adm.acqErr = ErrDiskReserve
		h.runCycleAndWait()
		if st := h.status(); st.State != model.RigStateIdle || st.SafeToPowerOff != model.SafeWorking {
			t.Fatalf("state %q safe %q with the only frame deferred by admission, want Idle and Working", st.State, st.SafeToPowerOff)
		}
	})
}

// scriptedProbe answers the identity probe from the fake resolver until call
// number from, then with foreign or no reply — a device that changes between a
// cycle's pull probe and its delete probe.
type scriptedProbe struct {
	*fakeResolver
	mu      sync.Mutex
	calls   int
	from    int
	foreign bool
}

func (p *scriptedProbe) ProbeGUID(ctx context.Context, ep netsite.Endpoint) (string, string, bool) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if n < p.from {
		return p.fakeResolver.ProbeGUID(ctx, ep)
	}
	if p.foreign {
		return "someone-else", "", true
	}
	return "", "", false
}

// TestIdentityForeignOnLaterSlice: a device that turns foreign between slices is
// caught by the later slice's own probe, deletes nothing, and falls back to the
// poll cadence with its Error intact.
func TestIdentityForeignOnLaterSlice(t *testing.T) {
	setup := func(t *testing.T, foreign bool) (*harness, []model.Frame) {
		h := newHarness(t, emmc, header.New())
		h.roomy()
		h.profile.GUID = "dev-guid"
		h.resolver.probeGUID = "dev-guid"
		// Probes 1 and 2 are cycle 1's pull and delete; 3 is cycle 2's pull; 4 its slice.
		h.w.resolver = &scriptedProbe{fakeResolver: h.resolver, from: 4, foreign: foreign}
		frames := h.seedEligible(t, 3)
		h.slowLink(slowLinkRead)
		if next := h.runCycleAndWait(); next != time.Second {
			t.Fatalf("precondition: first cycle returned %v, want a yield", next)
		}
		return h, frames
	}
	poll := 45 * time.Second

	t.Run("foreign", func(t *testing.T) {
		h, frames := setup(t, true)
		next := h.w.runCycle(context.Background())
		if next != poll {
			t.Fatalf("returned %v, want the poll", next)
		}
		if h.onDevice(frames) != 2 {
			t.Fatal("the foreign slice must delete nothing")
		}
		if !h.hasAlert(model.AlertIdentityMismatch) {
			t.Fatal("expected the identity alert")
		}
		st := h.status()
		if st.State != model.RigStateError {
			t.Fatalf("state %q, want Error to survive the slice and the re-stamp", st.State)
		}
		if want := h.nowVal.Add(poll).Unix(); st.NextPollAt != want {
			t.Fatalf("NextPollAt %d, want now + poll %d", st.NextPollAt, want)
		}
		if st.Draining {
			t.Fatal("an identity error must clear the draining indication")
		}
	})

	t.Run("unconfirmed", func(t *testing.T) {
		h, frames := setup(t, false)
		next := h.w.runCycle(context.Background())
		if next != poll {
			t.Fatalf("returned %v, want the poll", next)
		}
		if h.onDevice(frames) != 2 {
			t.Fatal("an unconfirmed slice must delete nothing")
		}
		if got := h.counterValue("asiair_commander_delete_blocked_total", map[string]string{"reason": "identity_unconfirmed"}); got != 1 {
			t.Fatalf("identity_unconfirmed = %v, want 1", got)
		}
	})
}

// TestIntentOpenedBetweenSlices: a re-file intent opened between two slices, for
// a frame the first had not reached, is honoured by the second. The intent guards
// the culled-frame path (a re-file moves the NAS copy, which then reads as
// absent), so the frame is a settled cull the second slice would otherwise delete.
func TestIntentOpenedBetweenSlices(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.linkRoot(t, h.cfg.Roots.Light)
	frames := h.seedEligible(t, 3)
	if err := os.Remove(frames[1].DestPath); err != nil {
		t.Fatal(err)
	}
	h.setMissing(t, frames[1].ID, int64((48 * time.Hour).Seconds())) // culled, well past settle
	h.slowLink(slowLinkRead)
	h.runCycleAndWait()
	if !h.deviceHas(frames[1].SourcePath) || h.deviceHas(frames[0].SourcePath) {
		t.Fatal("precondition: the first slice should have deleted frame 0 and stopped before frame 1")
	}
	if err := h.store.PutRefileIntent(context.Background(), store.RefileIntent{
		RigID: "rig-01", FromPath: frames[1].DestPath,
		ToPath: filepath.Join(h.cfg.Roots.Light, "elsewhere", filepath.Base(frames[1].DestPath)), CreatedAt: h.nowVal.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	h.runCycleAndWait()

	if !h.deviceHas(frames[1].SourcePath) {
		t.Fatal("a frame with an open re-file intent must not be deleted")
	}
	if !h.logContains("refile_in_flight") {
		t.Fatalf("expected the deferral to name the open intent; log:\n%s", h.logBuf.String())
	}
	if h.deviceHas(frames[2].SourcePath) {
		t.Fatal("the slice should have passed the deferred frame and deleted the next")
	}
}

// TestKeepOriginalsSetMidDrain: turning KeepOriginals on between slices stops the
// drain without stranding the pill or the fast cadence.
func TestKeepOriginalsSetMidDrain(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 3)
	h.slowLink(slowLinkRead)
	h.runCycleAndWait()
	h.profile.KeepOriginals = true

	next := h.w.runCycle(context.Background())

	if poll := h.cfg.EffectivePollInterval(h.profile); next != poll {
		t.Fatalf("returned %v, want the poll", next)
	}
	if h.onDevice(frames) != 2 {
		t.Fatal("a KeepOriginals rig must delete nothing")
	}
	if st := h.status(); st.State == model.RigStateDeleting || st.Draining {
		t.Fatalf("state %q draining=%v, want neither Deleting nor draining", st.State, st.Draining)
	}
}

// TestOnlyDisabledSharesRemainCycle is the runCycle half of
// TestOnlyDisabledSharesRemain: rows left only on a disabled share never drive a
// continuation.
func TestOnlyDisabledSharesRemainCycle(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	frames := h.seedEligible(t, 2)
	h.profile.Sources[0].Disabled = true

	next := h.w.runCycle(context.Background())

	if poll := h.cfg.EffectivePollInterval(h.profile); next != poll {
		t.Fatalf("returned %v, want the poll", next)
	}
	if h.onDevice(frames) != 2 {
		t.Fatal("frames on a disabled share must not be deleted")
	}
}

// TestRevisingFrameNotHeldSooner pins maxDeleteDefers to wall time: across
// yielded cycles a source that keeps differing from its row is held no sooner
// than 20 polls after its first deferral, because listings stay a poll apart and
// a slice visits a frame at most once.
func TestRevisingFrameNotHeldSooner(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	poll := h.cfg.EffectivePollInterval(h.profile)
	// A revision is young on every listing, so the pull path owes it min_file_age
	// and never supersedes it: only the delete gate's budget can conclude.
	h.cfg.Defaults.MinFileAge = model.Duration(5 * time.Minute)
	frames := h.seedEligible(t, 3)
	// Frames 0 and 2 stream and defer on every visit, so the drain never ends and
	// every slice yields; frame 1 keeps differing from its row.
	failVerifyRead(t, frames[0])
	failVerifyRead(t, frames[2])
	h.slowLink(slowLinkRead)

	var firstDefer time.Time
	for n := 1; ; n++ {
		if n > 100 {
			t.Fatal("the revising frame was never held")
		}
		start := h.nowVal
		h.dev.SetMtime(emmc, frames[1].SourcePath, h.nowVal) // still being revised
		h.runCycleAndWait()
		got, _ := h.getFrame(frames[1].SourcePath)
		if firstDefer.IsZero() && got.DeleteDeferCount > 0 {
			firstDefer = start
		}
		if got.State == model.StateHeld {
			if held := start.Sub(firstDefer); held < maxDeleteDefers*poll {
				t.Fatalf("held %v after its first deferral, want at least %d polls (%v)", held, maxDeleteDefers, maxDeleteDefers*poll)
			}
			break
		}
	}
	if !h.deviceHas(frames[1].SourcePath) {
		t.Fatal("a revising frame must never be deleted")
	}
}

// TestResumeOutsideSliceRestoresListedPill: a Pause and a Resume served by the
// restore pass's drain, with no slice to follow, put the listing's state back on
// the pill the moment the Resume applies, not Paused until the next listing.
func TestResumeOutsideSliceRestoresListedPill(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"nothing eligible", func(h *harness) {
			longGrace := model.Duration(3 * time.Hour)
			h.profile.Grace = &longGrace
		}},
		{"keep originals", func(h *harness) { h.profile.KeepOriginals = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			ctx := context.Background()
			h.roomy()
			frames := h.seedEligible(t, 2)
			// Both frames were culled and asked back: the restore pass walks them,
			// draining commands before each.
			for _, f := range frames {
				if err := os.Remove(f.DestPath); err != nil {
					t.Fatal(err)
				}
				h.setMissing(t, f.ID, 60)
				if ok, err := h.store.RequestRestore(ctx, "rig-01", f.ID, h.nowVal.Unix()); err != nil || !ok {
					t.Fatalf("request restore: ok=%v err=%v", ok, err)
				}
			}
			tc.setup(h)
			queued := false
			h.dev.OnOpenReader = func(_, p string) {
				if !queued && p == frames[0].SourcePath {
					queued = true
					h.w.Send(Pause)
					h.w.Send(Resume)
				}
			}
			rec := h.recordStatuses()

			h.w.runCycle(ctx)

			if !queued {
				t.Fatal("precondition: the restore pass never read the first restored frame")
			}
			pubs := rec.take()
			if slices.ContainsFunc(pubs, func(p model.RigStatus) bool { return p.State == model.RigStateDeleting }) {
				t.Fatal("precondition: no slice should have run")
			}
			paused := slices.IndexFunc(pubs, func(p model.RigStatus) bool { return p.State == model.RigStatePaused })
			if paused < 0 || paused+1 >= len(pubs) {
				t.Fatalf("no Paused publish followed by another: %+v", pubs)
			}
			if next := pubs[paused+1]; next.State != h.w.listedState || next.Paused {
				t.Fatalf("publish on Resume = %+v, want the listing's %q on an unpaused rig", next, h.w.listedState)
			}
			if st := h.status(); st.State != h.w.listedState || st.Paused {
				t.Fatalf("state %q paused=%v at the end of the cycle, want the listing's %q", st.State, st.Paused, h.w.listedState)
			}
		})
	}
}

// TestBackOnlineClearsOfflinePill: a rig coming back from Offline shows the last
// listing's state while its listing walk runs, not the Offline the walk would
// otherwise keep until a copy starts.
func TestBackOnlineClearsOfflinePill(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.keepCaptureDir()
	h.runCycleAndWait()
	h.dev.SetReachable(false)
	h.runCycleAndWait()
	if st := h.status(); st.State != model.RigStateOffline || st.Online {
		t.Fatalf("precondition: state %q online=%v, want an offline rig", st.State, st.Online)
	}
	h.dev.SetReachable(true)
	var during []model.RigState
	h.dev.OnListDir = func(string, string) { during = append(during, h.status().State) }

	h.runCycleAndWait()

	if len(during) == 0 {
		t.Fatal("precondition: the online cycle never listed the device")
	}
	for i, s := range during {
		if s != model.RigStateIdle {
			t.Fatalf("listing call %d saw the pill at %q, want the last listing's Idle", i, s)
		}
	}
}

// TestBackOnlinePausedRigReadsPaused: a paused rig that comes back from Offline
// (a ResetIdentity cycle runs while it is suspended) reads Paused, not the last
// listing's activity, while its suspended listing runs.
func TestBackOnlinePausedRigReadsPaused(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.keepCaptureDir()
	h.runCycleAndWait()
	h.pauseRig()
	h.w.setState(model.RigStateOffline)
	h.w.st.Online = false

	h.w.markOnline(context.Background())

	if st := h.status(); st.State != model.RigStatePaused || !st.Online {
		t.Fatalf("state %q online=%v, want Paused on a suspended rig back online", st.State, st.Online)
	}
}

// TestSuspendedListingKeepsListedState: a Pause served mid-pull makes the listing
// publish Paused, which is not a listed state, so the Resume that follows puts
// back the previous listing's state, never Paused on an unpaused rig.
func TestSuspendedListingKeepsListedState(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	h.roomy()
	h.keepCaptureDir()
	h.runCycleAndWait()
	if h.w.listedState != model.RigStateIdle {
		t.Fatalf("precondition: listed state %q, want Idle", h.w.listedState)
	}
	h.addCapture(t)
	h.addCaptureAt(t, "Autorun/Light/C-2025 A6/Light_NGC_0002.fit")
	sent := false
	h.dev.OnOpenReader = func(string, string) {
		if !sent {
			sent = true
			h.w.Send(Pause)
		}
	}
	h.runCycleAndWait()
	if st := h.status(); st.State != model.RigStatePaused || !st.Paused {
		t.Fatalf("precondition: state %q paused=%v, want the suspended listing's Paused", st.State, st.Paused)
	}

	h.w.Send(Resume)
	h.w.drainCommands(context.Background())

	if st := h.status(); st.Paused || st.State != model.RigStateIdle {
		t.Fatalf("after Resume state %q paused=%v, want the previous listing's Idle on an unpaused rig", st.State, st.Paused)
	}
}
