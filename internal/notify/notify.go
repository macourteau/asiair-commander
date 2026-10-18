// Package notify implements the single choke point for raising alerts (design
// architecture §10). Every problem the system surfaces — transfer failures,
// verify mismatches, low disk, unreachable rigs, and so on — funnels through
// Raise, which persists the alert, pushes it to the dashboard's SSE stream, and
// refreshes the unacked-alert badge. Alerts are problem-only; the UI is the
// only notification surface.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/macourteau/asiair-commander/internal/hub"
	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// Notifier is the alert choke point consumed by the rig worker, manager, and
// discovery. Raise never returns an error: a failure to raise an alert must
// not fail the caller's cycle, so failures are logged internally instead.
type Notifier interface {
	Raise(ctx context.Context, a model.Alert)
}

// alertStore is the slice of store.Store that Raise needs. Narrowing the
// field to just these two methods (any store.Store value satisfies it
// automatically) lets tests use a tiny hand-rolled fake instead of a full
// store.Store implementation.
type alertStore interface {
	RaiseAlert(ctx context.Context, a model.Alert) (int64, bool, error)
	UnackedCount(ctx context.Context) (int, error)
}

// notifier is the real Notifier, backed by the store and hub.
type notifier struct {
	store   alertStore
	hub     hub.Hub
	metrics *metrics.Metrics
	log     *slog.Logger
}

var _ Notifier = (*notifier)(nil)

// New returns a Notifier that persists alerts via s and publishes them
// through h. mx is nil-safe. If logger is nil, slog.Default() is used.
func New(s store.Store, h hub.Hub, mx *metrics.Metrics, logger *slog.Logger) Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &notifier{store: s, hub: h, metrics: mx, log: logger}
}

// Raise persists a and, only when that created a new alert row, emits an
// "alert.raised" event carrying it, records the metric, logs it and refreshes the
// hub's unacked-alert count. A raise absorbed by a row that still stands
// (Store.RaiseAlert) is the same alert seen again, not news: conditions are
// re-raised every cycle, and announcing each would toast and read the same alert
// to screen readers every few seconds (design architecture §10).
func (n *notifier) Raise(ctx context.Context, a model.Alert) {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}

	id, created, err := n.store.RaiseAlert(ctx, a)
	if err != nil {
		n.log.Error("notify: raise alert failed", "rig_id", a.RigID, "category", a.Category, "error", err)
		return
	}
	a.ID = id
	if !created {
		n.log.Debug("notify: alert already standing", "id", id, "rig_id", a.RigID, "category", a.Category, "standing", a.Standing)
		return
	}
	// Counts alert rows created, so it agrees with the alerts table except for the
	// dedup not being severity-aware — see docs/metrics.md.
	n.metrics.RecordAlertRaised(a.Category, a.Severity)

	n.hub.Emit(hub.Event{Name: "alert.raised", Data: a})
	n.logAlert(a)

	count, err := n.store.UnackedCount(ctx)
	if err != nil {
		n.log.Error("notify: refresh unacked count failed", "error", err)
		return
	}
	n.hub.SetUnackedAlerts(count)
}

// logAlert logs the raised alert at a level driven by severity: critical
// alerts are handled the same as warn (both demand operator attention; slog
// has no level between warn and error suited to "critical") and info-level
// alerts log at info.
func (n *notifier) logAlert(a model.Alert) {
	attrs := []any{"id", a.ID, "rig_id", a.RigID, "category", a.Category, "message", a.Message}
	if a.Severity == model.SeverityInfo {
		n.log.Info("notify: alert raised", attrs...)
		return
	}
	n.log.Warn("notify: alert raised", attrs...)
}

// Fake is an in-memory Notifier for tests. Raise is safe for concurrent use
// since rig workers may call it from multiple goroutines.
type Fake struct {
	mu     sync.Mutex
	raised []model.Alert
}

var _ Notifier = (*Fake)(nil)

// Raise records a in call order. It never fails.
func (f *Fake) Raise(_ context.Context, a model.Alert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raised = append(f.raised, a)
}

// Raised returns the alerts recorded so far, in call order.
func (f *Fake) Raised() []model.Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Alert, len(f.raised))
	copy(out, f.raised)
	return out
}
