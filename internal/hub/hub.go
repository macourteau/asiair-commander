// Package hub implements the StatusHub (design architecture §8): the single
// owner of the live fleet state that feeds the dashboard's SSE stream. It
// exposes two delivery mechanisms with opposite requirements:
//
//   - a coalesced, latest-wins whole-fleet Snapshot, stored in an
//     atomic.Pointer and pushed to each subscriber over a size-1 channel so a
//     slow browser can never block a rig worker; and
//   - discrete, non-coalescing events on a bounded per-subscriber queue; a
//     subscriber whose queue overflows is dropped (marked lossy) rather than
//     allowed to block the emitter, and the reconnecting client refetches.
//
// The hub owns the fleet map privately, guarded by a single mutex; no shared
// mutable map ever escapes. Workers publish only their own per-rig delta.
package hub

import (
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
)

// eventQueueCap bounds each subscriber's discrete-event queue. Overflow drops
// the subscriber (see Emit) rather than blocking the emitting rig worker. A
// night of normal frame/alert traffic never approaches this; only a stalled or
// dead consumer does.
const eventQueueCap = 64

// Event is one discrete SSE message (frame.*, rig.*, alert.raised). Data
// marshals to JSON in the web layer.
type Event struct {
	Name string
	Data any
}

// Hub is the StatusHub seam consumed by the rig worker, notifier, and web
// layer. *StatusHub is the only implementation.
type Hub interface {
	// PublishRig updates one rig's status; the hub rebuilds the immutable
	// fleet snapshot and notifies snapshot subscribers latest-wins.
	PublishRig(model.RigStatus)
	// RemoveRig drops a rig from the snapshot when it leaves the managed set, so a
	// deleted rig stops appearing (in the dashboard and in scrape-time metrics).
	RemoveRig(id string)
	// SetRequested records the state an operator command asks a rig for, published
	// on the rig's snapshot as Requested. With onlyFrom given, it changes only a rig
	// whose recorded request is one of them.
	SetRequested(id string, want model.RequestedState, onlyFrom ...model.RequestedState)
	// SetNASFree updates the snapshot-global NAS free-space figure.
	SetNASFree(int64)
	// SetBytesLast12h updates the snapshot-global rolling 12-hour byte total.
	SetBytesLast12h(int64)
	// SetUnackedAlerts updates the snapshot-global unacked-alert count.
	SetUnackedAlerts(int)
	// SetSolverStatus updates the snapshot-global Tier-1 solver provisioning state.
	SetSolverStatus(model.ProvisionStatus)
	// SetRemotes replaces the snapshot-global list of paired remotes' states.
	SetRemotes([]model.RemoteStatus)
	// SetRelay sets Commander's own reachability of the relay regions; nil
	// removes it.
	SetRelay(*model.RelayStatus)
	// Snapshot returns the current immutable snapshot (atomic Load).
	Snapshot() model.Snapshot
	// Emit pushes a discrete event onto every subscriber's bounded queue.
	Emit(Event)
	// Subscribe registers a subscriber, immediately delivering the current
	// snapshot. cancel unsubscribes and releases the slot (idempotent).
	Subscribe() (id int64, snapshots <-chan model.Snapshot, events <-chan Event, cancel func())
}

// StatusHub is the concrete Hub. Construct with New.
type StatusHub struct {
	log *slog.Logger
	// metrics is set once at startup via SetMetrics before any subscriber connects,
	// so it needs no lock. nil-safe.
	metrics *metrics.Metrics

	// snap holds the current immutable whole-fleet snapshot. Handlers and SSE
	// only ever Load this pointer; the value it points to is never mutated
	// after Store, so reading it needs no lock.
	snap atomic.Pointer[model.Snapshot]

	mu            sync.Mutex                      // guards the fields below
	rigs          map[string]model.RigStatus      // the PRIVATE fleet map
	requested     map[string]model.RequestedState // overlaid onto rigs at rebuild
	nasFree       int64
	bytesLast12h  int64
	unackedAlerts int
	solverStatus  *model.ProvisionStatus
	remotes       []model.RemoteStatus
	relay         *model.RelayStatus
	subs          map[int64]*subscriber
	nextID        int64
}

// subscriber is one connected SSE client. Its channels are created and closed
// only under Hub.mu, so producers (rebuildLocked, Emit) never send on a closed
// channel: a closed subscriber is always already removed from the registry.
type subscriber struct {
	snapCh  chan model.Snapshot // size 1; coalesced latest-wins
	eventCh chan Event          // size eventQueueCap; non-coalescing
}

// New returns a Hub ready for use. If logger is nil, slog.Default() is used.
func New(logger *slog.Logger) *StatusHub {
	if logger == nil {
		logger = slog.Default()
	}
	h := &StatusHub{
		log:       logger,
		rigs:      make(map[string]model.RigStatus),
		requested: make(map[string]model.RequestedState),
		subs:      make(map[int64]*subscriber),
	}
	// Establish an initial (empty-fleet) snapshot so Snapshot and Subscribe
	// never observe a nil pointer.
	h.mu.Lock()
	h.rebuildLocked()
	h.mu.Unlock()
	return h
}

// PublishRig updates one rig's entry in the private fleet map and rebuilds the
// immutable snapshot. Safe to call concurrently with any other method.
func (h *StatusHub) PublishRig(rs model.RigStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// The request is never inferred from a publish: a run flag clearing does not say
	// whether the run ended by itself or for a Pause with a Run now queued behind
	// it. The worker settles a self-ended run's request through SetRequested.
	h.rigs[rs.ID] = rs
	h.rebuildLocked()
}

// RemoveRig drops a rig from the private fleet map and rebuilds the snapshot, so a
// rig removed from the managed set stops appearing. A no-op for an unknown id.
func (h *StatusHub) RemoveRig(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// A command can be queued before the rig's first publish, so clear its request
	// even for a rig the snapshot has not seen; a re-added rig must not inherit it.
	delete(h.requested, id)
	if _, ok := h.rigs[id]; !ok {
		return
	}
	delete(h.rigs, id)
	h.rebuildLocked()
}

// SetRequested records the state an operator command asks rig id for and
// re-publishes. It lives here rather than on the worker's status because the
// worker is exactly what is busy: it applies a command only between files, so a
// request it published itself would appear no sooner than the change does.
//
// onlyFrom, when given, limits the change to a rig whose current request is one of
// them. The condition is read under the same lock as the write, so two commands
// queued back to back are recorded in the order they were queued, each against the
// request the other left.
func (h *StatusHub) SetRequested(id string, want model.RequestedState, onlyFrom ...model.RequestedState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := h.requestedLocked(id)
	if len(onlyFrom) > 0 && !slices.Contains(onlyFrom, cur) {
		return
	}
	h.requested[id] = want
	if cur != want {
		h.rebuildLocked()
	}
}

// requestedLocked is rig id's recorded request. A rig no command has been queued
// for asks for whatever it is doing: paused if its worker published a pause, else
// running. Caller holds h.mu.
func (h *StatusHub) requestedLocked(id string) model.RequestedState {
	if r, ok := h.requested[id]; ok {
		return r
	}
	if h.rigs[id].Paused {
		return model.RequestPaused
	}
	return model.RequestRunning
}

// SetNASFree updates the snapshot-global NAS free-space figure and re-publishes.
func (h *StatusHub) SetNASFree(free int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nasFree = free
	h.rebuildLocked()
}

// SetBytesLast12h updates the snapshot-global rolling 12-hour byte total and
// re-publishes.
func (h *StatusHub) SetBytesLast12h(bytes int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bytesLast12h = bytes
	h.rebuildLocked()
}

// SetUnackedAlerts updates the snapshot-global unacked-alert count and
// re-publishes. The snapshot carries the authoritative count so a dropped
// alert.raised event self-heals on the next snapshot.
func (h *StatusHub) SetUnackedAlerts(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unackedAlerts = n
	h.rebuildLocked()
}

// SetSolverStatus updates the snapshot-global Tier-1 solver provisioning state
// and re-publishes, so download progress and readiness reach the UI over the
// existing status stream. A copy is stored so the caller's value can't mutate
// under a reader.
func (h *StatusHub) SetSolverStatus(s model.ProvisionStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := s
	h.solverStatus = &cp
	h.rebuildLocked()
}

// SetRemotes replaces the paired remotes' states and re-publishes, so a remote's
// card follows its connection over the existing status stream. The slice and
// each status's warnings are copied, so the caller can reuse its values without
// racing a reader.
func (h *StatusHub) SetRemotes(rs []model.RemoteStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var cp []model.RemoteStatus
	if len(rs) > 0 {
		cp = make([]model.RemoteStatus, len(rs))
		for i, r := range rs {
			r.Warnings = slices.Clone(r.Warnings)
			cp[i] = r
		}
		sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	}
	h.remotes = cp
	h.rebuildLocked()
}

// SetRelay replaces Commander's own relay reachability and re-publishes. The
// value is copied.
func (h *StatusHub) SetRelay(r *model.RelayStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r != nil {
		cp := *r
		r = &cp
	}
	h.relay = r
	h.rebuildLocked()
}

// Snapshot returns the current immutable snapshot value (atomic Load). The
// returned value shares no mutable state with the hub.
func (h *StatusHub) Snapshot() model.Snapshot {
	return *h.snap.Load()
}

// Emit pushes a discrete event onto every subscriber's bounded queue. A
// subscriber whose queue is full is a slow or dead consumer: it is unsubscribed
// (its slot released, its channels closed to signal "lossy → refetch") so it
// can never make the emitting worker block or accumulate goroutines/memory.
func (h *StatusHub) Emit(evt Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var overflowed []int64
	for id, s := range h.subs {
		select {
		case s.eventCh <- evt:
		default:
			overflowed = append(overflowed, id)
		}
	}
	for _, id := range overflowed {
		h.dropLocked(id)
		h.metrics.RecordHubSubscriberDropped()
		h.log.Warn("hub: dropped lossy subscriber (event queue overflow)", "id", id, "event", evt.Name)
	}
}

// Subscribe registers a subscriber and immediately delivers the current
// snapshot so a fresh SSE client renders state at once. cancel unsubscribes and
// releases the slot; it is idempotent.
func (h *StatusHub) Subscribe() (id int64, snapshots <-chan model.Snapshot, events <-chan Event, cancel func()) {
	h.mu.Lock()
	h.nextID++
	id = h.nextID
	s := &subscriber{
		snapCh:  make(chan model.Snapshot, 1),
		eventCh: make(chan Event, eventQueueCap),
	}
	h.subs[id] = s
	// The channel is empty and buffered, so this never blocks.
	s.snapCh <- *h.snap.Load()
	h.mu.Unlock()

	return id, s.snapCh, s.eventCh, func() { h.remove(id) }
}

// SetMetrics wires the metrics surface. Call once at startup before any subscriber
// connects (the composition root does), so it needs no lock. nil-safe.
func (h *StatusHub) SetMetrics(m *metrics.Metrics) { h.metrics = m }

// SubscriberCount returns the number of live subscribers. Read lock-free of any I/O
// (h.mu guards only pure-memory maps), so the metrics state collector can call it on
// the scrape path without risk (hazard P2).
func (h *StatusHub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// remove unsubscribes id under the lock. Idempotent: absent ids are a no-op.
func (h *StatusHub) remove(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(id)
}

// dropLocked removes a subscriber and closes its channels. Caller holds h.mu.
// Closing signals the consumer that it is unsubscribed (lossy → refetch).
func (h *StatusHub) dropLocked(id int64) {
	s, ok := h.subs[id]
	if !ok {
		return
	}
	delete(h.subs, id)
	close(s.snapCh)
	close(s.eventCh)
}

// rebuildLocked builds a fresh immutable snapshot from the private fleet map
// (rigs sorted by ID for deterministic output), stores it atomically, and
// notifies every snapshot subscriber latest-wins. Caller holds h.mu.
func (h *StatusHub) rebuildLocked() {
	remotes := make(map[string]*model.RemoteStatus, len(h.remotes))
	for i := range h.remotes {
		remotes[h.remotes[i].ID] = &h.remotes[i]
	}
	rigs := make([]model.RigStatus, 0, len(h.rigs))
	for _, r := range h.rigs {
		r.Requested = h.requestedLocked(r.ID)
		rigs = append(rigs, remoteView(r, remotes[r.Site]))
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].ID < rigs[j].ID })

	snap := model.Snapshot{
		Rigs:          rigs,
		NASFree:       h.nasFree,
		BytesLast12h:  h.bytesLast12h,
		UnackedAlerts: h.unackedAlerts,
		SolverStatus:  h.solverStatus,
		Remotes:       h.remotes,
		Relay:         h.relay,
		GeneratedAt:   time.Now().Unix(),
	}
	h.snap.Store(&snap)

	for _, s := range h.subs {
		notifySnap(s.snapCh, snap)
	}
}

// notifySnap delivers snap latest-wins to a size-1 channel without blocking:
// if the slot already holds a stale snapshot, it is coalesced away and replaced
// with the newest. Only the consumer competes for the slot (all producers hold
// h.mu), so after this call the slot holds snap — the newest wins.
func notifySnap(ch chan model.Snapshot, snap model.Snapshot) {
	select {
	case ch <- snap:
	default:
		// Slot full with a stale snapshot: drop it, then store the newest.
		// The consumer is the only other party; it can only drain, never
		// fill, so the second send cannot find the slot full again.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- snap:
		default:
		}
	}
}
