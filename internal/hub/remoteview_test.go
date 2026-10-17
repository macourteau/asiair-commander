package hub

import (
	"io"
	"log/slog"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestRemoteView: a snapshot rebuild attaches the remote's display fields to
// a rig's card and never alters what the worker wrote. While the remote's
// tunnel is down the card carries the remote's state, marked Down once the
// worker has written its remote-down state, with the last known verdict; a
// live session or a paused rig is not Down. A local rig, or one whose site no
// paired remote has, is never touched.
func TestRemoteView(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	last := &model.LastKnownState{Safe: model.SafeYes, At: 100, Pending: 3}
	written := model.RigStatus{
		ID: "a", Site: "r1", RemoteName: "Backyard", Address: "192.168.7.20",
		State: model.RigStateIdle, Online: true, SafeToPowerOff: model.SafeYes, SafeAsOf: 100, LastKnown: last,
	}
	local := written
	local.ID, local.Site, local.RemoteName = "b", "", ""

	rig := func(id string) model.RigStatus {
		t.Helper()
		for _, r := range h.Snapshot().Rigs {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("no rig %s", id)
		return model.RigStatus{}
	}
	// unaltered fails unless the worker-written fields read as published.
	unaltered := func(when string, got, want model.RigStatus) {
		t.Helper()
		if got.State != want.State || got.Online != want.Online || got.SafeToPowerOff != want.SafeToPowerOff ||
			got.PowerOffReason != want.PowerOffReason || got.SafeAsOf != want.SafeAsOf || got.Draining != want.Draining {
			t.Fatalf("%s: the rebuild altered the worker's card: got %+v, want %+v", when, got, want)
		}
	}
	h.PublishRig(written)
	h.PublishRig(local)

	for _, state := range []model.RemoteState{model.RemoteConnected, model.RemoteNeedsUpdate} {
		h.SetRemotes([]model.RemoteStatus{{ID: "r1", State: state}})
		r := rig("a")
		unaltered(string(state), r, written)
		if r.Remote != nil || r.LastKnown != nil {
			t.Fatalf("%s: remote %+v last known %+v, want none with the tunnel up", state, r.Remote, r.LastKnown)
		}
	}

	// The remote goes down before the worker's woken cycle: its card stands.
	h.SetRemotes([]model.RemoteStatus{{ID: "r1", State: model.RemoteReconnecting, LastHelloAt: 50}})
	r := rig("a")
	unaltered("before the worker ran", r, written)
	if r.Remote == nil || r.Remote.Down || r.LastKnown != nil {
		t.Fatalf("before the worker ran: remote %+v last known %+v, want the remote shown, not down", r.Remote, r.LastKnown)
	}

	// The worker writes its remote-down state: the card is marked Down.
	down := written
	down.State, down.Online, down.SafeToPowerOff, down.PowerOffReason, down.SafeAsOf =
		model.RigStateOffline, false, model.SafeUnknown, model.PowerOffRemote, 0
	h.PublishRig(down)
	for _, state := range []model.RemoteState{model.RemoteReconnecting, model.RemoteUnreachable, model.RemoteWaiting} {
		h.SetRemotes([]model.RemoteStatus{{ID: "r1", State: state, LastHelloAt: 50}})
		r := rig("a")
		unaltered(string(state), r, down)
		if r.Remote == nil || *r.Remote != (model.RigRemote{State: state, LastHelloAt: 50, Down: true}) {
			t.Fatalf("%s: remote view %+v", state, r.Remote)
		}
		if r.LastKnown == nil || *r.LastKnown != *last {
			t.Fatalf("%s: last known %+v, want %+v", state, r.LastKnown, last)
		}
		unaltered(string(state)+", local rig", rig("b"), local)
		if rig("b").Remote != nil {
			t.Fatalf("%s: a local rig shows the remote", state)
		}
	}

	// A live session, or a paused rig, keeps its own card: not Down.
	live := written
	live.InSession = true
	paused := written
	paused.State, paused.SafeToPowerOff, paused.PowerOffReason, paused.SafeAsOf = model.RigStatePaused, model.SafeUnknown, "paused", 0
	for name, w := range map[string]model.RigStatus{"live session": live, "paused": paused} {
		h.PublishRig(w)
		r := rig("a")
		unaltered(name, r, w)
		if r.Remote == nil || r.Remote.Down || r.LastKnown != nil {
			t.Fatalf("%s: remote %+v last known %+v, want the remote beside its own card", name, r.Remote, r.LastKnown)
		}
	}

	// A site no paired remote has (removed): nothing attached.
	h.PublishRig(down)
	h.SetRemotes(nil)
	r = rig("a")
	unaltered("removed remote", r, down)
	if r.Remote != nil || r.LastKnown != nil {
		t.Fatalf("removed remote: remote %+v last known %+v", r.Remote, r.LastKnown)
	}
}

// TestSetRelayPublishes: Commander's relay reachability reaches the snapshot
// as a copy, and nil removes it.
func TestSetRelayPublishes(t *testing.T) {
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	in := &model.RelayStatus{State: model.RelayDown, Since: 7}
	h.SetRelay(in)
	in.State = model.RelayUp
	if got := h.Snapshot().Relay; got == nil || *got != (model.RelayStatus{State: model.RelayDown, Since: 7}) {
		t.Fatalf("relay %+v, want the value as set", got)
	}
	h.SetRelay(nil)
	if got := h.Snapshot().Relay; got != nil {
		t.Fatalf("relay %+v after nil", got)
	}
}
