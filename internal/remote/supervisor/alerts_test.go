package supervisor

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

// alerts returns the alerts of category c standing for remote id, and all of
// them acknowledged or not.
func (h *harness) alerts(id string, c model.Category) (unacked, all []model.Alert) {
	h.t.Helper()
	list, err := h.st.ListAlerts(context.Background(), true, 0)
	if err != nil {
		h.t.Fatalf("ListAlerts: %v", err)
	}
	for _, a := range list {
		if a.Category != c || !strings.HasPrefix(a.Message, model.RemoteAlertPrefix(id)) {
			continue
		}
		if a.RigID != "" {
			h.t.Fatalf("remote alert %+v carries a rig id", a)
		}
		all = append(all, a)
		if a.AckedAt == 0 {
			unacked = append(unacked, a)
		}
	}
	return unacked, all
}

// wantAlert fails unless remote id has exactly `standing` unacknowledged and
// `total` alerts of category c.
func (h *harness) wantAlert(id string, c model.Category, standing, total int) model.Alert {
	h.t.Helper()
	unacked, all := h.alerts(id, c)
	if len(unacked) != standing || len(all) != total {
		h.t.Fatalf("%s alerts for %s: %d standing of %d, want %d of %d: %+v", c, id, len(unacked), len(all), standing, total, all)
	}
	if len(all) == 0 {
		return model.Alert{}
	}
	if all[0].Severity != model.SeverityWarn {
		h.t.Fatalf("%s alert severity %s, want warn", c, all[0].Severity)
	}
	return all[0]
}

// session publishes a rig reached through remote id, with or without a live
// session, as its worker does.
func (h *harness) session(id string, live bool) {
	h.fleet.PublishRig(model.RigStatus{ID: "rig-01", Site: id, InSession: live})
}

// TestRemoteUnreachableAlert: one remote-level alert once a remote is
// Unreachable, held back while a rig still has a live session through it, and
// acknowledged by the next successful hello. A remote waiting for a new
// pairing after it had connected escalates the same way past the threshold;
// one that never connected does not.
func TestRemoteUnreachableAlert(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	h.session(id, true)

	f.stop()
	h.clock.advance(time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteReconnecting)
	h.wantAlert(id, model.AlertRemoteUnreachable, 0, 0)

	h.clock.advance(15 * time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteUnreachable)
	h.wantAlert(id, model.AlertRemoteUnreachable, 0, 0) // a rig still has a live session

	h.session(id, false)
	h.beat(id)
	h.beat(id)
	a := h.wantAlert(id, model.AlertRemoteUnreachable, 1, 1)
	if !strings.Contains(a.Message, `"Observatory"`) {
		t.Fatalf("alert %q does not name the remote", a.Message)
	}

	// The next successful hello acknowledges it.
	f.start()
	h.clock.advance(rebuildBackoffMax + time.Second)
	h.connect(id, f)
	h.wantAlert(id, model.AlertRemoteUnreachable, 0, 1)

	// Re-paired, and the remote PC never got the new .env: Waiting, with a
	// past, escalates past the same threshold and clears on the first hello
	// under the new pairing.
	p, err := h.sup.Repair(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	h.beat(id)
	h.wantState(id, model.RemoteWaiting)
	h.wantAlert(id, model.AlertRemoteUnreachable, 0, 1)
	h.clock.advance(16 * time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteWaiting)
	a = h.wantAlert(id, model.AlertRemoteUnreachable, 1, 2)
	if !strings.Contains(a.Message, ".env") {
		t.Fatalf("waiting alert %q does not point at the .env", a.Message)
	}
	nf := h.install(p)
	nf.start()
	h.connect(id, nf)
	h.wantAlert(id, model.AlertRemoteUnreachable, 0, 2)

	// A remote that never connected is waiting for its setup, not down.
	id2, _ := h.add("Backyard")
	h.beat(id2)
	h.clock.advance(time.Hour)
	h.beat(id2)
	h.wantState(id2, model.RemoteWaiting)
	h.wantAlert(id2, model.AlertRemoteUnreachable, 0, 0)

	// A Commander restart neither re-raises an alert that cleared nor loses
	// one that stands: the store merges the raise into the standing alert.
	nf.stop()
	h.clock.advance(time.Minute)
	h.beat(id)
	h.clock.advance(16 * time.Minute)
	h.beat(id)
	h.wantAlert(id, model.AlertRemoteUnreachable, 1, 3)
	h.sup.Stop()
	h.sup = h.newSupervisor()
	h.sup.Start(context.Background())
	h.sup.Stop()
	h.wantAlert(id, model.AlertRemoteUnreachable, 1, 3)
}

// TestPairingReuseAlert (design remote-sites AC27): an instance id coming back
// without a restart raises one Warn alert that another remote is using the
// pairing; Re-pair clears the condition and acknowledges it.
func TestPairingReuseAlert(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	hd := f.handler()
	setInstance := func(inst string) {
		hd.mu.Lock()
		hd.instance = inst
		hd.mu.Unlock()
	}
	setInstance("inst-other") // a restart: no alert
	h.beat(id)
	h.wantAlert(id, model.AlertRemotePairing, 0, 0)
	setInstance("inst-a") // the first process again, without a restart
	h.beat(id)
	setInstance("inst-other")
	h.beat(id)
	h.wantAlert(id, model.AlertRemotePairing, 1, 1)

	if _, err := h.sup.Repair(context.Background(), id, 0); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	h.wantAlert(id, model.AlertRemotePairing, 0, 1)
}

// TestRejectedForeignKeyAlert (design remote-sites AC27): a client key other
// than Commander's in the remote's warnings raises one Warn alert, the evidence
// that the address leaked; it is acknowledged once the remote no longer reports
// one.
func TestRejectedForeignKeyAlert(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	hd := f.handler()
	set := func(ws ...proto.Warning) {
		hd.mu.Lock()
		hd.warnings = ws
		hd.mu.Unlock()
	}
	set(proto.Warning{Seq: 1, Kind: proto.WarnRefusedForward, Target: "192.168.1.9:445"})
	h.beat(id)
	h.wantAlert(id, model.AlertRemoteForeignKey, 0, 0)

	set(proto.Warning{Seq: 2, Kind: proto.WarnRejectedClient, ClientKey: "nodekey:abc"},
		proto.Warning{Seq: 3, Kind: proto.WarnRejectedClient, ClientKey: "nodekey:def"})
	h.beat(id)
	h.beat(id)
	a := h.wantAlert(id, model.AlertRemoteForeignKey, 1, 1)
	if !strings.Contains(a.Message, `"Observatory"`) {
		t.Fatalf("alert %q does not name the remote", a.Message)
	}

	// It does not clear on its own: the evidence stands until a Re-pair, even
	// once the remote no longer lists the rejection, and across a restart.
	set()
	h.beat(id)
	h.beat(id)
	h.wantAlert(id, model.AlertRemoteForeignKey, 1, 1)
	h.sup.Stop()
	h.sup = h.newSupervisor()
	h.sup.Start(context.Background())
	h.sup.Stop()
	h.wantAlert(id, model.AlertRemoteForeignKey, 1, 1)

	if _, err := h.sup.Repair(context.Background(), id, 0); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	h.wantAlert(id, model.AlertRemoteForeignKey, 0, 1)
}

// recordingWaker records the rigs a supervisor wakes.
type recordingWaker struct {
	mu    sync.Mutex
	woken []string
}

func (w *recordingWaker) WakeForRemote(rigID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woken = append(w.woken, rigID)
}

func (w *recordingWaker) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.woken
	w.woken = nil
	return out
}

// TestRemoteTransitionWakesRigs: every change of a remote's state, down and
// up, wakes the workers of the rigs reached through it and no others, so each
// writes its card at once rather than at its next cycle; a heartbeat that
// changes nothing wakes nobody.
func TestRemoteTransitionWakesRigs(t *testing.T) {
	h := newHarness(t)
	w := &recordingWaker{}
	h.sup.SetWaker(w)
	id, f := h.add("Observatory")
	h.fleet.PublishRig(model.RigStatus{ID: "rig-01", Site: id})
	h.fleet.PublishRig(model.RigStatus{ID: "rig-02", Site: "r99"})
	h.fleet.PublishRig(model.RigStatus{ID: "rig-03"})
	h.beat(id)
	w.take()

	h.connect(id, f)
	if got := w.take(); !slices.Equal(got, []string{"rig-01"}) {
		t.Fatalf("waiting to connected woke %v, want rig-01", got)
	}
	h.beat(id)
	if got := w.take(); len(got) != 0 {
		t.Fatalf("a heartbeat with no change woke %v", got)
	}
	f.stop()
	h.clock.advance(time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteReconnecting)
	if got := w.take(); !slices.Equal(got, []string{"rig-01"}) {
		t.Fatalf("connected to reconnecting woke %v, want rig-01", got)
	}
	h.clock.advance(15 * time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteUnreachable)
	if got := w.take(); !slices.Equal(got, []string{"rig-01"}) {
		t.Fatalf("reconnecting to unreachable woke %v, want rig-01", got)
	}
}

// TestStaleBuildAlert (design remote-sites AC27): a remote build trailing
// Commander's by more than about two weeks raises one Warn alert per remote,
// whatever its state, and it is acknowledged once the remote updates.
func TestStaleBuildAlert(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	f.start()
	hd := f.handler()
	setDate := func(d string) {
		hd.mu.Lock()
		hd.build.Date = d
		hd.mu.Unlock()
	}
	setDate("2026-09-01T00:00:00Z") // Commander's is 2026-10-01
	h.connect(id, f)
	h.beat(id)
	a := h.wantAlert(id, model.AlertRemoteBuildStale, 1, 1)
	if !strings.Contains(a.Message, `"Observatory"`) {
		t.Fatalf("alert %q does not name the remote", a.Message)
	}

	// Still one while the remote is down: the card keeps its last hello's build.
	f.stop()
	h.clock.advance(time.Minute)
	h.beat(id)
	h.wantAlert(id, model.AlertRemoteBuildStale, 1, 1)

	// A second remote that is stale has its own.
	id2, f2 := h.add("Backyard")
	f2.start()
	hd2 := f2.handler()
	hd2.mu.Lock()
	hd2.build.Date = "2026-09-01T00:00:00Z"
	hd2.mu.Unlock()
	h.connect(id2, f2)
	h.wantAlert(id2, model.AlertRemoteBuildStale, 1, 1)
	h.wantAlert(id, model.AlertRemoteBuildStale, 1, 1)

	// The remote is updated: its new process reports Commander's build date.
	f.start()
	h.clock.advance(rebuildBackoffMax + time.Second)
	h.connect(id, f)
	h.wantAlert(id, model.AlertRemoteBuildStale, 0, 1)
	h.wantAlert(id2, model.AlertRemoteBuildStale, 1, 1)

	// Removing a remote retires what it reported.
	if err := h.sup.Remove(id2); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	h.wantAlert(id2, model.AlertRemoteBuildStale, 0, 1)
}

// TestRelayStatus: Commander's own reachability of the relay is published
// apart from any remote's state: up while a tunnel client holds its relay
// connection, whether or not its remote answers; down once none has for the
// grace a fresh client needs; absent with no remote paired.
func TestRelayStatus(t *testing.T) {
	h := newHarness(t)
	relay := func() *model.RelayStatus {
		h.hub.mu.Lock()
		defer h.hub.mu.Unlock()
		return h.hub.relay
	}
	h.sup.publish()
	if r := relay(); r != nil {
		t.Fatalf("relay %+v with no remote", r)
	}

	id, f := h.add("Observatory")
	h.connect(id, f)
	if r := relay(); r == nil || r.State != model.RelayUp {
		t.Fatalf("relay %+v with a connected remote, want up", r)
	}

	// Commander loses the relay: unknown within the grace, down after it.
	f.mu.Lock()
	srv := f.srv
	f.mu.Unlock()
	srv.SetRelayReachable(false)
	h.beat(id)
	if r := relay(); r == nil || r.State != model.RelayUnknown {
		t.Fatalf("relay %+v just after losing it, want unknown", r)
	}
	h.clock.advance(relayGrace)
	h.beat(id)
	r := relay()
	if r == nil || r.State != model.RelayDown || r.Since != h.clock.now().Unix() {
		t.Fatalf("relay %+v past the grace, want down since now", r)
	}
	srv.SetRelayReachable(true)
	h.beat(id)
	if r := relay(); r == nil || r.State != model.RelayUp {
		t.Fatalf("relay %+v once reachable again, want up", r)
	}

	// The remote stops; Commander still reaches the relay, so the remote's
	// state says the remote is down rather than this side.
	f.stop()
	for range 3 {
		h.clock.advance(time.Minute)
		h.beat(id)
	}
	h.wantState(id, model.RemoteReconnecting)
	if r := relay(); r == nil || r.State != model.RelayUp {
		t.Fatalf("relay %+v with the remote down and the relay reachable, want up", r)
	}

	if err := h.sup.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if r := relay(); r != nil {
		t.Fatalf("relay %+v after the last remote was removed", r)
	}
}

// TestTransitionsLoggedOnlyByPublish: reading the remotes (GET /api/remotes)
// has no side effect; a state change is logged once, by the publish that
// first sees it.
func TestTransitionsLoggedOnlyByPublish(t *testing.T) {
	h := newHarness(t)
	id, f := h.add("Observatory")
	h.connect(id, f)
	if n := countLines(h.logs.String(), "remote: connected"); n != 1 {
		t.Fatalf("%d connected lines, want 1:\n%s", n, h.logs.String())
	}
	f.stop()
	h.clock.advance(time.Minute)
	h.beat(id)
	h.wantState(id, model.RemoteReconnecting)

	// Time alone makes the remote unreachable; no heartbeat has published it.
	h.clock.advance(15 * time.Minute)
	for range 3 {
		h.wantState(id, model.RemoteUnreachable)
	}
	if n := countLines(h.logs.String(), "remote: unreachable"); n != 0 {
		t.Fatalf("a read logged the transition %d times:\n%s", n, h.logs.String())
	}
	h.beat(id)
	h.beat(id)
	if n := countLines(h.logs.String(), "remote: unreachable"); n != 1 {
		t.Fatalf("%d unreachable lines after publishing, want 1:\n%s", n, h.logs.String())
	}
}

// TestTransitionLogNeverHeard: a remote that goes Unreachable without ever
// answering logs its last hello as never, not as the Unix epoch.
func TestTransitionLogNeverHeard(t *testing.T) {
	h := newHarness(t)
	h.sup.logTransition(model.RemoteWaiting, model.RemoteStatus{ID: "r9", State: model.RemoteUnreachable})
	logs := h.logs.String()
	if !strings.Contains(logs, "last_hello=never") || strings.Contains(logs, "1970") {
		t.Fatalf("unreachable transition of a never-heard remote logged:\n%s", logs)
	}
	h.sup.logTransition(model.RemoteConnected, model.RemoteStatus{ID: "r9", State: model.RemoteUnreachable, LastHelloAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()})
	// The time is logged in the local zone, so only its year is pinned.
	if logs := h.logs.String(); !strings.Contains(logs, "last_hello=2026-") {
		t.Fatalf("unreachable transition did not log the last hello:\n%s", logs)
	}
}
