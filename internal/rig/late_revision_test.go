package rig

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/netsite"
)

// The race this file is about: deletePhase runs every cycle regardless of
// quiescence, while the pull that supersedes waits out min_file_age. A revision
// arriving after the grace clock has elapsed is therefore SEEN by the delete pass
// several passes before the pull path can resolve it (design frame-lifecycle
// §11.13).
//
// latePoll is the shipped poll interval and lateGrace a grace below it, so the
// delete pass wins that race deliberately rather than by luck of the clock.
const (
	latePoll  = 45 * time.Second
	lateGrace = 30 * time.Second
	lateAge   = 300 * time.Second
)

// lateRevisionHarness stages the race: a frame filed and re-labelled — so its two
// copies hash to two different values and the three-way check below cannot pass by
// comparing one — then, once its grace has elapsed, revised on the device.
//
// It returns the row as filed and the revised device bytes. The revision's mtime is
// the instant it is written, so the pull path owes it a full min_file_age before it
// may be pulled: every pass until then is the delete pass alone.
func lateRevisionHarness(t *testing.T) (*harness, *model.Frame, []byte) {
	t.Helper()
	h := newHarness(t, emmc, header.New())
	confirmDeleteIdentity(h)
	roomyDevice(h)
	h.cfg.Defaults.MinFileAge = model.Duration(lateAge)
	h.cfg.Defaults.Grace = model.Duration(lateGrace)

	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)
	f := pullFile(t, h, src, original, 10*time.Minute)
	retargetTo(t, h, f.ID, "M31")

	h.nowVal = h.nowVal.Add(time.Minute) // past the grace, so the frame is eligible
	h.dev.AddFile(emmc, f.SourcePath, revised, h.nowVal)
	return h, f, revised
}

// blockedTotal and deferredReason name the series the deferral records, the one
// signal it leaves an operator: the deferral raises nothing and holds nothing, so
// without the counter a rig deferring every pass looks exactly like one with nothing
// to do.
const blockedTotal = "asiair_commander_delete_blocked_total"

// deferredReason is the deferral's own label selector.
func deferredReason() map[string]string {
	return map[string]string{"reason": "source_changed_deferred"}
}

// roomyDevice gives the fake device a capacity that raises nothing on its own, so a
// test asserting "this raised no alert at all" is not reading the harness's default
// zero-byte share as a finding about the frame.
func roomyDevice(h *harness) {
	h.dev.SetStatfs(emmc, 900<<30, 1<<40)
}

// raisedSince returns the categories raised after the first mark alerts, so a test
// whose setup alerts on purpose can still say "and nothing after that".
func raisedSince(h *harness, mark int) []string {
	var out []string
	for _, a := range h.notify.Raised()[mark:] {
		out = append(out, string(a.Category))
	}
	return out
}

// deleteOnly runs one delete pass and reports the device content reads it cost, so a
// caller can say a gate decided without streaming a ~116 MB copy.
func deleteOnly(t *testing.T, h *harness) int {
	t.Helper()
	before := h.dev.ContentReads()
	h.w.deleteAllForTest(context.Background(), h.cfg, h.profile, h.dev, "10.0.0.1")
	return h.dev.ContentReads() - before
}

// TestLateRevisionDefersTheDeletePass (design frame-lifecycle AC14) is the race
// itself, cycle by cycle: the delete pass reaches a revised frame before the pull
// path supersedes it. Every pass within the bound defers silently and hashes nothing
// at that gate, and once the revision is old enough to pull the frame supersedes and
// drains on the three-way check unchanged. Without the deferral this sequence is this
// design's own failure class, louder: a hold, a critical delete_mismatch, and then a
// critical frame_lost for a frame whose archive copy is perfectly intact.
func TestLateRevisionDefersTheDeletePass(t *testing.T) {
	h, f, revised := lateRevisionHarness(t)

	// Six passes at the shipped poll interval — 270s — is still short of the 300s the
	// pull path owes the revision, which is precisely why the gate cannot conclude
	// anything here.
	for pass := 1; pass <= 6; pass++ {
		h.nowVal = h.nowVal.Add(latePoll)
		before := h.dev.ContentReads()
		h.cycle()
		if got := h.dev.ContentReads() - before; got != 0 {
			t.Fatalf("pass %d: the deferral cost %d device content reads, want none", pass, got)
		}
		if cats := h.raisedCategories(); len(cats) != 0 {
			t.Fatalf("pass %d: a frame mid-revision must raise nothing; got %v", pass, cats)
		}
		got := mustGetFrame(t, h, f.ID)
		if got.State != model.StateVerified {
			t.Fatalf("pass %d: state=%s, want verified", pass, got.State)
		}
		if got.DeleteDeferCount != pass {
			t.Fatalf("pass %d: delete_defer_count=%d, want %d", pass, got.DeleteDeferCount, pass)
		}
		if !h.deviceHas(f.SourcePath) {
			t.Fatalf("pass %d: nothing is proven about the revised original, so it must stand", pass)
		}
		// Silent to an operator watching alerts, visible to one watching metrics: the
		// deferral raises nothing and holds nothing, so the counter is the only trace
		// a rig deferring every pass leaves. Its own reason, not the post-verify
		// race's.
		if got := h.counterValue(blockedTotal, deferredReason()); got != float64(pass) {
			t.Fatalf("pass %d: delete_blocked_total{reason=source_changed_deferred}=%v, want %d", pass, got, pass)
		}
		if got := h.counterValue(blockedTotal, map[string]string{"reason": "source_changed_pre_delete"}); got != 0 {
			t.Fatalf("pass %d: a deferred revision is not the post-verify race; got %v", pass, got)
		}
		if got := h.counterValue(blockedTotal, map[string]string{"reason": "source_changed"}); got != 0 {
			t.Fatalf("pass %d: source_changed is a held frame plus a critical alert; got %v", pass, got)
		}
	}

	// The revision ages past min_file_age and the pull path resolves it.
	h.nowVal = h.nowVal.Add(10 * time.Minute)
	h.cycle()

	superseded := mustGetFrame(t, h, f.ID)
	if superseded.SHA256 != sha256hex(revised) {
		t.Fatalf("sha256 = %q, want the revised device bytes %q", superseded.SHA256, sha256hex(revised))
	}
	if superseded.DeleteDeferCount != 0 {
		t.Fatalf("the source agrees with its row again, so the budget resets; got %d", superseded.DeleteDeferCount)
	}
	// Two values, not one: the re-label leaves the archive copy hashing to something
	// the device copy never will, so a check that compared a single hash would fail
	// here rather than pass by coincidence.
	onDisk := hashOnDisk(t, superseded.DestPath)
	if superseded.ArchiveSHA256 != onDisk {
		t.Fatalf("archive_sha256 = %q, want the bytes on the NAS %q", superseded.ArchiveSHA256, onDisk)
	}
	if superseded.ArchiveSHA256 == superseded.SHA256 {
		t.Fatal("setup: the re-labelled copies must hash differently, or the three-way check is one value")
	}

	h.nowVal = h.nowVal.Add(time.Minute)
	if reads := deleteOnly(t, h); reads == 0 {
		t.Fatal("the three-way check streams the device copy; a pass that reads nothing proved nothing")
	}
	if cats := h.raisedCategories(); len(cats) != 0 {
		t.Fatalf("a superseded frame passes the three-way check unchanged; got %v", cats)
	}
	if after := mustGetFrame(t, h, f.ID); after.State != model.StateDeleted {
		t.Fatalf("state=%s, want deleted", after.State)
	}
	if h.deviceHas(f.SourcePath) {
		t.Fatal("the proven revised original should have drained")
	}
}

// TestChangedSourcePastTheBoundHolds (design frame-lifecycle AC15) is what keeps
// design frame-lifecycle AC14 from being satisfiable by a gate that defers forever: a
// source that stays changed is eventually the row's problem rather than a frame
// mid-revision, and deferring it silently forever would leave its device original
// undrainable — the never-drains end state "Suppress with no other change" was
// rejected for, reached by a quieter route. The pull path never runs here, so nothing
// ever resolves the disagreement.
func TestChangedSourcePastTheBoundHolds(t *testing.T) {
	h, f, _ := lateRevisionHarness(t)

	for pass := 1; pass <= maxDeleteDefers; pass++ {
		h.nowVal = h.nowVal.Add(latePoll)
		if reads := deleteOnly(t, h); reads != 0 {
			t.Fatalf("pass %d: the deferral cost %d device content reads, want none", pass, reads)
		}
		if cats := h.raisedCategories(); len(cats) != 0 {
			t.Fatalf("pass %d is inside the budget and must raise nothing; got %v", pass, cats)
		}
		got := mustGetFrame(t, h, f.ID)
		if got.State != model.StateVerified {
			t.Fatalf("pass %d: state=%s, want verified", pass, got.State)
		}
		if got.DeleteDeferCount != pass {
			t.Fatalf("pass %d: delete_defer_count=%d, want %d", pass, got.DeleteDeferCount, pass)
		}
	}

	h.nowVal = h.nowVal.Add(latePoll)
	deleteOnly(t, h)

	if !h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("the pass after the budget reaches the ordinary outcome; got %v", h.raisedCategories())
	}
	if got := h.alertSeverity(model.AlertDeleteMismatch); got != model.SeverityCritical {
		t.Fatalf("the delete_mismatch severity is %q, want critical", got)
	}
	if got := mustGetFrame(t, h, f.ID); got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
	if !h.deviceHas(f.SourcePath) {
		t.Fatal("a held frame's device original is kept, never deleted")
	}
	// The escalation crosses from the quiet reason to the held one exactly once.
	if got := h.counterValue(blockedTotal, deferredReason()); got != float64(maxDeleteDefers) {
		t.Fatalf("delete_blocked_total{reason=source_changed_deferred}=%v, want %d", got, maxDeleteDefers)
	}
	if got := h.counterValue(blockedTotal, map[string]string{"reason": "source_changed"}); got != 1 {
		t.Fatalf("delete_blocked_total{reason=source_changed}=%v, want the one held pass", got)
	}
}

// restartWorker replaces the harness's worker with a fresh one over the same store,
// device and filesystem — a process restart, which is the whole reason the deferral
// counter lives on the row rather than in memory.
func restartWorker(t *testing.T, h *harness) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(h.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := NewWorker("rig-01", h.cfgPtr, func(netsite.Endpoint, string, string, time.Duration) device.Device { return h.dev },
		h.resolver, netsite.NewRegistry(logger), h.lfs, header.New(), h.store, h.hub, h.notify, h.adm, nil, nil,
		time.UTC, func() time.Time { return h.nowVal }, h.metrics, logger)
	w.lookupHost = noLookup
	h.w = w
}

// TestDeferralsSurviveARestart (design frame-lifecycle AC15, the persistence half)
// is the alternative "Hold the delete pass's deferral count in memory rather than on
// the row" rejected: an in-memory counter resets on restart, so a rig restarting
// more often than the bound never escalates and the frame defers forever.
func TestDeferralsSurviveARestart(t *testing.T) {
	h, f, _ := lateRevisionHarness(t)

	for pass := 1; pass <= maxDeleteDefers; pass++ {
		h.nowVal = h.nowVal.Add(latePoll)
		deleteOnly(t, h)
		if pass == maxDeleteDefers/2 {
			restartWorker(t, h) // mid-run, with half the budget already spent
		}
	}
	if cats := h.raisedCategories(); len(cats) != 0 {
		t.Fatalf("the budget is spent but not exceeded; got %v", cats)
	}

	h.nowVal = h.nowVal.Add(latePoll)
	deleteOnly(t, h)

	if !h.hasAlert(model.AlertDeleteMismatch) {
		t.Fatalf("the deferrals spent before the restart still count; got %v", h.raisedCategories())
	}
	if got := mustGetFrame(t, h, f.ID); got.State != model.StateHeld {
		t.Fatalf("state=%s, want held", got.State)
	}
}

// TestTheBudgetIsPerEpisodeOfDisagreement (design frame-lifecycle AC15, the
// per-episode half): a row that spent its budget on one revision must not hold on
// the first sighting of the next one, which is the outcome the deferral exists to
// prevent. The reset rides the supersede, the frame's source coming to agree with
// its row again.
func TestTheBudgetIsPerEpisodeOfDisagreement(t *testing.T) {
	h, f, _ := lateRevisionHarness(t)

	// Episode one: the whole budget, then the supersede that ends it.
	for pass := 1; pass <= maxDeleteDefers; pass++ {
		h.nowVal = h.nowVal.Add(latePoll)
		deleteOnly(t, h)
	}
	h.nowVal = h.nowVal.Add(10 * time.Minute)
	h.cycle()
	if got := mustGetFrame(t, h, f.ID); got.DeleteDeferCount != 0 {
		t.Fatalf("the supersede resets the budget; got %d", got.DeleteDeferCount)
	}

	// Episode two: the device revises the same frame again, and the second episode
	// gets the whole bound rather than holding on its first sighting.
	_, twice := revisionPair(revTarget)
	h.nowVal = h.nowVal.Add(time.Minute)
	h.dev.AddFile(emmc, f.SourcePath, deviceRevision(twice), h.nowVal)

	for pass := 1; pass <= maxDeleteDefers; pass++ {
		h.nowVal = h.nowVal.Add(latePoll)
		deleteOnly(t, h)
		if h.hasAlert(model.AlertDeleteMismatch) {
			t.Fatalf("pass %d of the second episode held a frame the first episode's budget had spent", pass)
		}
		got := mustGetFrame(t, h, f.ID)
		if got.DeleteDeferCount != pass || got.State != model.StateVerified {
			t.Fatalf("pass %d of the second episode: delete_defer_count=%d state=%s",
				pass, got.DeleteDeferCount, got.State)
		}
	}
}

// TestHeldRowWithAnIntactArchiveCopyIsNotALostFrame (design frame-lifecycle AC11,
// the held-row half) corrects the one arm that concluded without looking:
// resolveDiffer's held arm raised a critical frame_lost from the row's state flag
// alone, and a held row with missing_seen_at == 0 is exactly what holdFrame leaves
// behind — so a revised frame would reach that arm on the next
// listing and alert critically with nothing in hand. A held row whose copy still
// verifies against its archive expectation is not a lost frame.
func TestHeldRowWithAnIntactArchiveCopyIsNotALostFrame(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	roomyDevice(h)
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	// Held through the real path, so the row carries what a hold actually leaves:
	// state held, the archive copy untouched and the reconciliation marks clear.
	h.w.holdFrame(ctx, *f, model.HoldSourceChanged, "source present but size/mtime changed since filing")
	held := mustGetFrame(t, h, f.ID)
	if held.State != model.StateHeld || held.MissingSeenAt != 0 {
		t.Fatalf("setup: want a held row with no cull mark, got state=%s missing=%d", held.State, held.MissingSeenAt)
	}
	mark := len(h.notify.Raised()) // the hold's own delete_mismatch stops here

	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	if cats := raisedSince(h, mark); len(cats) != 0 {
		t.Fatalf("a held row whose archive copy verifies is not a lost frame; got %v", cats)
	}
	got := mustGetFrame(t, h, f.ID)
	if got.State != model.StateVerified {
		t.Fatalf("state=%s, want verified: a row left held drains never", got.State)
	}
	if got.SHA256 != sha256hex(revised) {
		t.Fatalf("sha256 = %q, want the revised device bytes", got.SHA256)
	}
}

// TestFailedRowStillConcludesFromItsState is the other half of that arm, untouched:
// a failed row never had a good copy, which is a different claim from a held one and
// one no read can settle — there is no archive copy to verify.
func TestFailedRowStillConcludesFromItsState(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	src := "Autorun/Light/C-2025 A6/Light_0001.fit"
	original, revised := revisionPair(revTarget)

	f := pullFile(t, h, src, original, 10*time.Minute)
	if err := h.store.SetState(ctx, f.ID, model.StateFailed); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	h.nowVal = h.nowVal.Add(time.Hour)
	pullFile(t, h, src, revised, 5*time.Minute)

	if !h.hasAlert(model.AlertFrameLost) {
		t.Fatalf("an overwritten source of a frame that never had a proven copy still alerts; got %v", h.raisedCategories())
	}
	if got := h.alertSeverity(model.AlertFrameLost); got != model.SeverityCritical {
		t.Fatalf("frame_lost severity = %q, want critical", got)
	}
}
