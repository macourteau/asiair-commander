package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/remote/proto"
)

// relayGrace is how long every tunnel client may go without a relay
// connection before Commander reads its own path to the relays as down: a
// client's first dial connects it within seconds, and a heartbeat dials every
// ten.
const relayGrace = 30 * time.Second

// AlertStore is where the remotes' alerts are found again to acknowledge them
// once their condition clears (store.Store).
type AlertStore interface {
	ListAlerts(ctx context.Context, includeAcked bool, limit int) ([]model.Alert, error)
	AckAlert(ctx context.Context, id int64, at int64) error
	UnackedCount(ctx context.Context) (int, error)
}

// RigSource is the live fleet (*hub.StatusHub), read for the rigs holding a
// live session through a remote.
type RigSource interface {
	Snapshot() model.Snapshot
}

// RigWaker wakes a rig's worker when the remote it is reached through changes
// state (*manager.Manager), so the worker, the rig's one writer, publishes the
// card that goes with it at once.
type RigWaker interface {
	WakeForRemote(rigID string)
}

// SetWaker installs the RigWaker. The manager is built after the supervisor,
// so the composition root sets it once both exist; until then no rig is woken.
func (s *Supervisor) SetWaker(w RigWaker) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	s.waker = w
}

// onTransitions logs each remote whose state changed since the last publish
// and wakes the workers of the rigs reached through it. add records a remote's
// initial state, so a remote not yet recorded is only recorded: it logs and
// wakes nothing, as every worker runs a cycle when it starts. Caller holds
// s.pubMu.
func (s *Supervisor) onTransitions(list []model.RemoteStatus) {
	var rigs []model.RigStatus
	for _, st := range list {
		prev, seen := s.lastState[st.ID]
		s.lastState[st.ID] = st.State
		if !seen || prev == st.State {
			continue
		}
		s.logTransition(prev, st)
		if s.waker == nil || s.rigs == nil {
			continue
		}
		if rigs == nil {
			rigs = s.rigs.Snapshot().Rigs
		}
		woken := 0
		for _, r := range rigs {
			if r.Site == st.ID {
				s.waker.WakeForRemote(r.ID)
				woken++
			}
		}
		s.log.Debug("remote: state changed; woke the rigs reached through it",
			"remote", st.ID, "from", string(prev), "to", string(st.State), "rigs", woken)
	}
}

// logTransition records a remote's state change, at Warn when it moved away
// from connected or waiting.
func (s *Supervisor) logTransition(prev model.RemoteState, st model.RemoteStatus) {
	args := []any{"remote", st.ID, "from", string(prev), "to", string(st.State)}
	switch st.State {
	case model.RemoteConnected:
		s.log.Info("remote: connected", append(args, "direct", st.Direct, "latency_ms", st.LatencyMS)...)
	case model.RemoteWaiting:
		s.log.Info("remote: waiting for first connection", args...)
	default:
		// A remote never heard from has no last hello to report, not 1970.
		lastHello := any("never")
		if st.LastHelloAt != 0 {
			lastHello = time.Unix(st.LastHelloAt, 0)
		}
		s.log.Warn("remote: "+string(st.State), append(args, "last_hello", lastHello)...)
	}
}

// stickyAlerts are not acknowledged when their condition clears: a rejected
// foreign key is evidence the pairing leaked, which only a Re-pair (or the
// user) settles.
var stickyAlerts = map[model.Category]bool{model.AlertRemoteForeignKey: true}

// remoteAlertCategories are the alerts one remote can have standing, each at
// most once.
var remoteAlertCategories = []model.Category{
	model.AlertRemoteUnreachable, model.AlertRemotePairing,
	model.AlertRemoteForeignKey, model.AlertRemoteBuildStale,
}

// latch is what Commander knows of one of a remote's alerts.
type latch int

const (
	// latchUnknown: not evaluated since this process started; an alert a
	// previous process raised may be standing.
	latchUnknown latch = iota
	latchClear
	latchRaised
)

// alertRemotes raises each remote's alerts whose condition holds and
// acknowledges those whose condition has cleared, each once per transition.
// Until an alert is first evaluated, either action is taken: the store merges
// a raise into an identical standing alert, and an ack retires one a previous
// process left behind. Caller holds s.pubMu, which guards s.latches.
func (s *Supervisor) alertRemotes(list []model.RemoteStatus) {
	if s.notify == nil {
		return
	}
	ctx := context.Background()
	now := s.now()
	var rigs []model.RigStatus
	if s.rigs != nil {
		rigs = s.rigs.Snapshot().Rigs
	}
	for _, st := range list {
		latches := s.latches[st.ID]
		if latches == nil {
			latches = map[model.Category]latch{}
			s.latches[st.ID] = latches
		}
		for _, c := range remoteAlertCategories {
			raise, clear, msg := s.remoteCondition(c, st, now, rigs)
			switch {
			case raise && latches[c] != latchRaised:
				s.notify.Raise(ctx, model.Alert{Severity: model.SeverityWarn, Category: c, Message: model.RemoteAlertPrefix(st.ID) + msg})
				latches[c] = latchRaised
			case clear && latches[c] != latchClear:
				if !stickyAlerts[c] {
					s.ackRemoteAlerts(ctx, st.ID, c)
				}
				latches[c] = latchClear
			}
		}
	}
}

// remoteCondition says whether one of a remote's alerts should be raised and
// whether its condition has cleared, with the alert's message. They differ
// only for the unreachable alert, which waits to be raised while a rig still
// has a live session through the remote, and clears only on a hello.
func (s *Supervisor) remoteCondition(c model.Category, st model.RemoteStatus, now time.Time, rigs []model.RigStatus) (raise, clear bool, msg string) {
	switch c {
	case model.AlertRemoteUnreachable:
		after := s.unreachableAfter()
		down := st.State == model.RemoteUnreachable
		msg = fmt.Sprintf("%q unreachable for over %s", st.Name, after)
		// A remote that connected before, under any pairing, and is waiting for
		// its new one escalates the same way: its .env was likely not replaced.
		if st.State == model.RemoteWaiting && st.EverConnected && now.Sub(time.Unix(st.LastHelloAt, 0)) >= after {
			down = true
			msg = fmt.Sprintf("%q has not connected under its new pairing for over %s; check the .env on the remote PC", st.Name, after)
		}
		return down && !liveSession(rigs, st.ID), !down, msg
	case model.AlertRemotePairing:
		return st.PairingShared, !st.PairingShared,
			fmt.Sprintf("another remote is answering on %q's pairing; make new setup files for it on its card", st.Name)
	case model.AlertRemoteForeignKey:
		rejected := slices.ContainsFunc(st.Warnings, func(w model.RemoteWarning) bool {
			return w.Kind == string(proto.WarnRejectedClient)
		})
		return rejected, !rejected,
			fmt.Sprintf("%q refused a client key other than Commander's: its pairing has leaked; make new setup files for it on its card", st.Name)
	case model.AlertRemoteBuildStale:
		return st.BuildStale, !st.BuildStale,
			fmt.Sprintf("%q runs a build more than two weeks older than Commander's (%s, Commander %s); check that its updater is running and can reach the image registry",
				st.Name, st.Version, s.build.Version)
	}
	return false, false, ""
}

// liveSession reports whether a rig reached through the remote holds a live
// session.
func liveSession(rigs []model.RigStatus, id string) bool {
	return slices.ContainsFunc(rigs, func(r model.RigStatus) bool { return r.Site == id && r.InSession })
}

// ackRemoteAlertsWindow is how many of the newest unacknowledged alerts are
// searched for a remote's alert to acknowledge.
const ackRemoteAlertsWindow = 500

// ackRemoteAlerts acknowledges the remote's standing alerts of category c.
//
// A remote-level alert row has no rig id and no remote id (the alert store's
// schema is unchanged by the remote bridge), so it is identified by category
// and by its message starting with model.RemoteAlertPrefix, and only within the
// newest ackRemoteAlertsWindow unacknowledged alerts. One buried deeper under
// other unacknowledged alerts is never auto-acknowledged and stays until the
// user acknowledges it; a remote id on the alert row would lift both limits.
func (s *Supervisor) ackRemoteAlerts(ctx context.Context, id string, c model.Category) {
	if s.alerts == nil {
		return
	}
	alerts, err := s.alerts.ListAlerts(ctx, false, ackRemoteAlertsWindow)
	if err != nil {
		s.log.Warn("remote: could not list alerts to auto-ack", "remote", id, "error", err)
		return
	}
	prefix := model.RemoteAlertPrefix(id)
	at := s.now().Unix()
	acked := 0
	for _, a := range alerts {
		if a.RigID != "" || a.Category != c || !strings.HasPrefix(a.Message, prefix) {
			continue
		}
		if err := s.alerts.AckAlert(ctx, a.ID, at); err != nil {
			s.log.Warn("remote: auto-ack failed", "remote", id, "alert", a.ID, "error", err)
			continue
		}
		acked++
	}
	if acked == 0 {
		return
	}
	s.log.Info("remote: auto-acked resolved alerts", "remote", id, "category", string(c), "count", acked)
	if n, err := s.alerts.UnackedCount(ctx); err == nil && s.hub != nil {
		s.hub.SetUnackedAlerts(n)
	}
}

// repairedAlerts acknowledges the alerts a Re-pair settles: a leaked pairing
// is replaced, so its evidence no longer stands.
func (s *Supervisor) repairedAlerts(id string) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if l := s.latches[id]; l != nil {
		l[model.AlertRemoteForeignKey] = latchClear
	}
	s.ackRemoteAlerts(context.Background(), id, model.AlertRemoteForeignKey)
}

// forgetAlerts acknowledges every alert of a removed remote: whatever they
// reported no longer applies.
func (s *Supervisor) forgetAlerts(id string) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	delete(s.latches, id)
	delete(s.lastState, id)
	for _, c := range remoteAlertCategories {
		s.ackRemoteAlerts(context.Background(), id, c)
	}
}

// relayStatus is Commander's own reachability of the relays: up while any
// tunnel client holds its relay connection, down once none has for
// relayGrace, unknown before that or when no client exists. nil when no
// remote is paired. Caller holds s.pubMu, which guards the fields it keeps.
func (s *Supervisor) relayStatus() *model.RelayStatus {
	s.mu.Lock()
	rs := make([]*remote, 0, len(s.remotes))
	for _, r := range s.remotes {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	if len(rs) == 0 {
		s.relayQuietSince = time.Time{}
		return nil
	}
	now := s.now()
	clients, up := 0, false
	for _, r := range rs {
		c, _ := r.site.current()
		if c == nil {
			continue
		}
		clients++
		if c.RelayConnected() {
			up = true
			break
		}
	}
	state := model.RelayUp
	switch {
	case up:
		s.relayQuietSince = time.Time{}
	case clients == 0:
		s.relayQuietSince = time.Time{}
		state = model.RelayUnknown
	default:
		if s.relayQuietSince.IsZero() {
			s.relayQuietSince = now
		}
		state = model.RelayUnknown
		if now.Sub(s.relayQuietSince) >= relayGrace {
			state = model.RelayDown
		}
	}
	if state != s.relay.State {
		switch state {
		case model.RelayDown:
			s.log.Warn("remote: Commander cannot reach the relay; every remote reads down because of this side", "since", s.relayQuietSince)
		case model.RelayUp:
			s.log.Info("remote: Commander reaches the relay", "was", string(s.relay.State))
		}
		s.relay = model.RelayStatus{State: state, Since: now.Unix()}
	}
	out := s.relay
	return &out
}
