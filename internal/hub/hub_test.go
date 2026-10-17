package hub

import (
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
)

// quietLogger discards output so the overflow warning doesn't spam test logs.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func rig(id string, frames int) model.RigStatus {
	return model.RigStatus{ID: id, Name: "rig-" + id, State: model.RigStateIdle, FramesTonight: frames}
}

// TestNew_EmptySnapshot verifies a fresh hub has a non-nil, empty snapshot.
func TestNew_EmptySnapshot(t *testing.T) {
	h := New(quietLogger())
	got := h.Snapshot()
	if len(got.Rigs) != 0 {
		t.Fatalf("fresh hub: want 0 rigs, got %d", len(got.Rigs))
	}
	if got.GeneratedAt == 0 {
		t.Fatal("fresh hub: GeneratedAt not set")
	}
}

// TestPublishRig_SortedDeterministic verifies rigs are sorted by ID.
func TestPublishRig_SortedDeterministic(t *testing.T) {
	h := New(quietLogger())
	h.PublishRig(rig("rig-03", 3))
	h.PublishRig(rig("rig-01", 1))
	h.PublishRig(rig("rig-02", 2))

	got := h.Snapshot()
	want := []string{"rig-01", "rig-02", "rig-03"}
	if len(got.Rigs) != len(want) {
		t.Fatalf("want %d rigs, got %d", len(want), len(got.Rigs))
	}
	for i, id := range want {
		if got.Rigs[i].ID != id {
			t.Errorf("rig[%d]: want %s, got %s", i, id, got.Rigs[i].ID)
		}
	}
}

// TestPublishRig_UpdatesInPlace verifies re-publishing a rig replaces its entry
// rather than appending a duplicate.
func TestPublishRig_UpdatesInPlace(t *testing.T) {
	h := New(quietLogger())
	h.PublishRig(rig("rig-01", 1))
	h.PublishRig(rig("rig-01", 99))

	got := h.Snapshot()
	if len(got.Rigs) != 1 {
		t.Fatalf("want 1 rig after update, got %d", len(got.Rigs))
	}
	if got.Rigs[0].FramesTonight != 99 {
		t.Errorf("want FramesTonight 99, got %d", got.Rigs[0].FramesTonight)
	}
}

// TestRemoveRig drops a rig from the snapshot so a deleted rig stops appearing
// (in the dashboard and in scrape-time metrics). An unknown id is a no-op.
func TestRemoveRig(t *testing.T) {
	h := New(quietLogger())
	h.PublishRig(rig("rig-01", 1))
	h.PublishRig(rig("rig-02", 2))

	h.RemoveRig("rig-01")
	got := h.Snapshot()
	if len(got.Rigs) != 1 || got.Rigs[0].ID != "rig-02" {
		t.Fatalf("after RemoveRig want only rig-02, got %+v", got.Rigs)
	}

	h.RemoveRig("nope") // unknown id: no-op, no panic
	if len(h.Snapshot().Rigs) != 1 {
		t.Errorf("removing an unknown rig changed the snapshot")
	}
}

// TestSetGlobals verifies the snapshot-global fields re-publish.
func TestSetGlobals(t *testing.T) {
	h := New(quietLogger())
	h.SetNASFree(512)
	h.SetBytesLast12h(4096)
	h.SetUnackedAlerts(7)

	got := h.Snapshot()
	if got.NASFree != 512 {
		t.Errorf("want NASFree 512, got %d", got.NASFree)
	}
	if got.BytesLast12h != 4096 {
		t.Errorf("want BytesLast12h 4096, got %d", got.BytesLast12h)
	}
	if got.UnackedAlerts != 7 {
		t.Errorf("want UnackedAlerts 7, got %d", got.UnackedAlerts)
	}
}

// TestSubscribe_ImmediateSnapshot verifies a new subscriber receives the
// current snapshot at once.
func TestSubscribe_ImmediateSnapshot(t *testing.T) {
	h := New(quietLogger())
	h.PublishRig(rig("rig-01", 5))
	h.SetNASFree(1000)

	_, snaps, _, cancel := h.Subscribe()
	defer cancel()

	select {
	case snap := <-snaps:
		if len(snap.Rigs) != 1 || snap.Rigs[0].ID != "rig-01" {
			t.Fatalf("initial snapshot missing rig-01: %+v", snap.Rigs)
		}
		if snap.NASFree != 1000 {
			t.Errorf("initial snapshot NASFree: want 1000, got %d", snap.NASFree)
		}
	case <-time.After(time.Second):
		t.Fatal("no initial snapshot delivered on Subscribe")
	}
}

// TestLatestWins verifies that a subscriber which does not read promptly still
// ends on the newest snapshot: intermediate snapshots coalesce away.
func TestLatestWins(t *testing.T) {
	h := New(quietLogger())
	_, snaps, _, cancel := h.Subscribe()
	defer cancel()

	// Drain the initial snapshot so the slot starts empty.
	<-snaps

	// Rapid publishes with no reader in between: the size-1 slot coalesces.
	for i := 1; i <= 100; i++ {
		h.PublishRig(rig("rig-01", i))
	}

	// Drain whatever is buffered; the last value read must be the newest.
	var last model.Snapshot
	draining := true
	for draining {
		select {
		case s := <-snaps:
			last = s
		default:
			draining = false
		}
	}
	if len(last.Rigs) != 1 || last.Rigs[0].FramesTonight != 100 {
		t.Fatalf("latest-wins failed: want FramesTonight 100, got %+v", last.Rigs)
	}
}

// TestEmit_DeliversToSubscribers verifies discrete events reach subscribers.
func TestEmit_DeliversToSubscribers(t *testing.T) {
	h := New(quietLogger())
	_, _, events, cancel := h.Subscribe()
	defer cancel()

	h.Emit(Event{Name: "frame.done", Data: 42})

	select {
	case e := <-events:
		if e.Name != "frame.done" || e.Data != 42 {
			t.Fatalf("unexpected event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
}

// TestEmit_OverflowUnsubscribes verifies a subscriber that never drains its
// event queue is dropped (slot released, channels closed) once the queue
// overflows, so a dead client can't leak.
func TestEmit_OverflowUnsubscribes(t *testing.T) {
	h := New(quietLogger())
	_, _, events, cancel := h.Subscribe()
	defer cancel() // must be safe after auto-drop

	if h.SubscriberCount() != 1 {
		t.Fatalf("want 1 subscriber, got %d", h.SubscriberCount())
	}

	// Never read events; fill the queue past capacity.
	for i := 0; i < eventQueueCap+5; i++ {
		h.Emit(Event{Name: fmt.Sprintf("e%d", i)})
	}

	if h.SubscriberCount() != 0 {
		t.Fatalf("overflowed subscriber not dropped: subCount=%d", h.SubscriberCount())
	}

	// The event channel must be closed (drain buffered, then observe closed).
	closed := false
	for i := 0; i < eventQueueCap+10; i++ {
		if _, ok := <-events; !ok {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("event channel not closed after overflow drop")
	}
}

// TestCancel_Idempotent verifies cancel releases the slot and is safe to call
// repeatedly, and that Emit after cancel does not panic.
func TestCancel_Idempotent(t *testing.T) {
	h := New(quietLogger())
	_, _, _, cancel := h.Subscribe()

	if h.SubscriberCount() != 1 {
		t.Fatalf("want 1 subscriber, got %d", h.SubscriberCount())
	}
	cancel()
	if h.SubscriberCount() != 0 {
		t.Fatalf("cancel did not release slot: subCount=%d", h.SubscriberCount())
	}
	cancel() // idempotent: must not panic
	cancel()

	// Emit after every subscriber is gone must not panic.
	h.Emit(Event{Name: "after.cancel"})
	h.PublishRig(rig("rig-01", 1))
}

// TestPublishRig_NonBlockingWithFullSubscriber proves the emitter never blocks
// on a subscriber whose snapshot slot is full and unread.
func TestPublishRig_NonBlockingWithFullSubscriber(t *testing.T) {
	h := New(quietLogger())
	_, _, _, cancel := h.Subscribe() // never read snaps
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			h.PublishRig(rig("rig-01", i))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PublishRig blocked on a full/unread subscriber")
	}
}

// TestConcurrentHammer runs many publishers, subscribers, and emitters together
// under -race and asserts no panic plus the final snapshot reflects the last
// published state per rig.
func TestConcurrentHammer(t *testing.T) {
	h := New(quietLogger())

	const (
		nRigs      = 8
		nEmitters  = 4
		nReaders   = 6
		iterations = 2000
	)
	// finalFrames is the value each rig's last publish will carry.
	finalFrames := iterations - 1

	var wg sync.WaitGroup

	// One publisher per rig so the final value per rig is deterministic.
	for r := 0; r < nRigs; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			id := fmt.Sprintf("rig-%02d", r)
			for i := 0; i < iterations; i++ {
				h.PublishRig(rig(id, i))
			}
		}(r)
	}

	// Global-field writers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			h.SetNASFree(int64(i))
			h.SetUnackedAlerts(i % 5)
		}
	}()

	// Emitters.
	for e := 0; e < nEmitters; e++ {
		wg.Add(1)
		go func(e int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				h.Emit(Event{Name: "frame.done", Data: i})
			}
		}(e)
	}

	// Subscribers that churn: subscribe, read a bit, cancel.
	for s := 0; s < nReaders; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(s)))
			for i := 0; i < 200; i++ {
				_, snaps, events, cancel := h.Subscribe()
				// Read a random handful, then cancel.
				reads := rng.Intn(4)
				for j := 0; j < reads; j++ {
					select {
					case <-snaps:
					case <-events:
					default:
					}
				}
				_ = h.Snapshot()
				cancel()
			}
		}(s)
	}

	wg.Wait()

	// After all writers finish, one more publish per rig to guarantee the final
	// coalesced snapshot reflects the known terminal state (a concurrent churn
	// reader could otherwise have left the last observed snapshot mid-flight).
	for r := 0; r < nRigs; r++ {
		h.PublishRig(rig(fmt.Sprintf("rig-%02d", r), finalFrames))
	}

	got := h.Snapshot()
	if len(got.Rigs) != nRigs {
		t.Fatalf("want %d rigs, got %d", nRigs, len(got.Rigs))
	}
	for i, r := range got.Rigs {
		wantID := fmt.Sprintf("rig-%02d", i)
		if r.ID != wantID {
			t.Errorf("rig[%d]: want id %s, got %s", i, wantID, r.ID)
		}
		if r.FramesTonight != finalFrames {
			t.Errorf("rig %s: want FramesTonight %d, got %d", r.ID, finalFrames, r.FramesTonight)
		}
	}

	// No subscribers should remain (all churn readers cancelled).
	if got := h.SubscriberCount(); got != 0 {
		t.Errorf("want 0 residual subscribers, got %d", got)
	}
}

// verify *StatusHub satisfies the Hub interface at compile time.
var _ Hub = (*StatusHub)(nil)

// A requested pause shows from the moment it is recorded, survives the worker's
// publishes (which never carry it), agrees with the worker once it publishes the
// pause, and gives way to a resume.
func TestRequestedStateOverlay(t *testing.T) {
	h := New(quietLogger())
	h.PublishRig(rig("a", 0))
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("an unpaused rig with no command requests %q, want running", got)
	}

	h.SetRequested("a", model.RequestPaused)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestPaused {
		t.Fatalf("requested pause shows as %q", got)
	}
	if h.Snapshot().Rigs[0].Paused {
		t.Fatal("precondition: the worker has not applied the pause yet")
	}

	// The worker's own status never carries the request; a publish must not erase it.
	h.PublishRig(rig("a", 1))
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestPaused {
		t.Fatalf("a worker publish changed the request to %q", got)
	}

	paused := rig("a", 1)
	paused.Paused = true
	h.PublishRig(paused)
	if st := h.Snapshot().Rigs[0]; st.Requested != model.RequestPaused || !st.Paused {
		t.Fatalf("after the pause applied: requested %q paused=%v, want the two to agree", st.Requested, st.Paused)
	}

	h.SetRequested("a", model.RequestRunning)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("a resume left the request at %q", got)
	}

	h.SetRequested("a", model.RequestPaused)
	h.RemoveRig("a")
	h.PublishRig(rig("a", 0))
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("a removed rig's request %q survived into its replacement", got)
	}
}

// A request recorded before the rig's first publish must not outlive its removal:
// a rig re-added under the same id would otherwise read "pausing" for good.
func TestRemoveRigClearsUnpublishedRequest(t *testing.T) {
	h := New(quietLogger())
	h.SetRequested("a", model.RequestPaused)
	h.RemoveRig("a")
	h.PublishRig(rig("a", 0))
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("a request on a never-published rig survived its removal: %q", got)
	}
}

// A rig no command was queued for requests what it is doing: a pause restored at
// startup is published by the worker as paused, and must not read "resuming".
func TestRequestedStateDefaultsToPublished(t *testing.T) {
	h := New(quietLogger())
	paused := rig("a", 0)
	paused.Paused = true
	h.PublishRig(paused)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestPaused {
		t.Fatalf("a paused rig with no command requests %q, want paused", got)
	}
}

// onlyFrom limits a change to a rig whose request is one of the given states, read
// against the request itself, so a condition sees what earlier commands recorded.
func TestSetRequestedOnlyFrom(t *testing.T) {
	h := New(quietLogger())
	h.PublishRig(rig("a", 0))

	// Run now on a running rig is a check now: no run is requested.
	h.SetRequested("a", model.RequestRunUntilIdle, model.RequestPaused)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("run until idle recorded on a running rig: %q", got)
	}
	// Stop run with no run requested changes nothing.
	h.SetRequested("a", model.RequestPaused, model.RequestRunUntilIdle)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("stop run recorded paused on a rig with no run: %q", got)
	}
	// A pause queued, then Run now behind it: the run is requested although the
	// worker has applied neither.
	h.SetRequested("a", model.RequestPaused)
	h.SetRequested("a", model.RequestRunUntilIdle, model.RequestPaused)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunUntilIdle {
		t.Fatalf("run now behind a pause recorded %q, want run until idle", got)
	}
	h.SetRequested("a", model.RequestPaused, model.RequestRunUntilIdle)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestPaused {
		t.Fatalf("stop run during a requested run recorded %q, want paused", got)
	}
}

// The hub never infers a request from a publish: a run flag clearing alone leaves
// the request as the last command recorded it. The worker settles a run that ends
// by itself through SetRequested (tested in internal/rig), because a flag cleared
// for a Pause with a Run now queued behind it must keep the run until idle that
// Run now recorded.
func TestRunFlagClearingLeavesRequest(t *testing.T) {
	h := New(quietLogger())
	st := rig("a", 0)
	st.Paused = true
	h.PublishRig(st)
	h.SetRequested("a", model.RequestRunUntilIdle, model.RequestPaused)

	st.RunUntilIdle = true
	h.PublishRig(st)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunUntilIdle {
		t.Fatalf("during the run the request reads %q", got)
	}
	st.RunUntilIdle = false
	h.PublishRig(st)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunUntilIdle {
		t.Fatalf("the run flag clearing changed the request to %q; only a command or the worker may", got)
	}
	// The worker's settle at a self-end: conditional on the run still being requested.
	h.SetRequested("a", model.RequestPaused, model.RequestRunUntilIdle)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestPaused {
		t.Fatalf("the settle left the request at %q, want paused", got)
	}

	// A resume during a run: the flag clears with running requested, which stands.
	st.RunUntilIdle = true
	h.SetRequested("a", model.RequestRunUntilIdle, model.RequestPaused)
	h.PublishRig(st)
	h.SetRequested("a", model.RequestRunning)
	st.RunUntilIdle, st.Paused = false, false
	h.PublishRig(st)
	if got := h.Snapshot().Rigs[0].Requested; got != model.RequestRunning {
		t.Fatalf("a run ended by a resume changed the request to %q", got)
	}
}

// TestSetRemotesPublishes: the remotes reach the snapshot in id order and to a
// subscriber, a later mutation of the caller's values does not, and an empty
// list clears them.
func TestSetRemotesPublishes(t *testing.T) {
	h := New(quietLogger())
	_, snaps, _, cancel := h.Subscribe()
	defer cancel()
	<-snaps // the initial snapshot

	in := []model.RemoteStatus{
		{ID: "r2", State: model.RemoteWaiting},
		{ID: "r1", State: model.RemoteConnected, Warnings: []model.RemoteWarning{{Kind: "broadcast_failed"}}},
	}
	h.SetRemotes(in)
	got := <-snaps
	if len(got.Remotes) != 2 || got.Remotes[0].ID != "r1" || got.Remotes[1].ID != "r2" {
		t.Fatalf("remotes not published in id order: %+v", got.Remotes)
	}
	in[1].Warnings[0].Kind = "mutated"
	in[0].State = model.RemoteUnreachable
	if s := h.Snapshot(); s.Remotes[0].Warnings[0].Kind != "broadcast_failed" || s.Remotes[1].State != model.RemoteWaiting {
		t.Fatalf("the caller's later mutation reached the snapshot: %+v", s.Remotes)
	}

	h.SetRemotes(nil)
	if s := <-snaps; s.Remotes != nil {
		t.Fatalf("remotes not cleared: %+v", s.Remotes)
	}
}
