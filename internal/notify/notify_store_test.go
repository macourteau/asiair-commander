package notify

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/metrics"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

// realNotifier wires the notifier to a real sqlite store, the path every raise
// takes in production, with a recording hub.
func realNotifier(t *testing.T) (*notifier, store.Store, *fakeHub, *metrics.Metrics) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fh := &fakeHub{}
	mx := metrics.New("test")
	return &notifier{store: st, hub: fh, metrics: mx, log: discardLogger()}, st, fh, mx
}

func raisedEvents(fh *fakeHub) int {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	n := 0
	for _, e := range fh.emitted {
		if e.Name == "alert.raised" {
			n++
		}
	}
	return n
}

func alertsRaisedTotal(t *testing.T, mx *metrics.Metrics) int {
	t.Helper()
	mfs, err := mx.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, mf := range mfs {
		if strings.HasSuffix(mf.GetName(), "alerts_raised_total") {
			for _, m := range mf.GetMetric() {
				total += int(m.GetCounter().GetValue())
			}
		}
	}
	return total
}

var standingBacklog = model.Alert{
	RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertBacklog,
	Message: "frames can't be removed", Standing: true,
}

// The toast-spam regression: a condition re-raised every poll is announced (event,
// metric) once, not once per poll.
func TestRaiseOfAStandingUnackedAlertDoesNotReannounce(t *testing.T) {
	n, _, fh, mx := realNotifier(t)
	ctx := context.Background()
	for range 15 {
		n.Raise(ctx, standingBacklog)
	}
	if got := raisedEvents(fh); got != 1 {
		t.Fatalf("alert.raised events = %d, want 1", got)
	}
	if got := alertsRaisedTotal(t, mx); got != 1 {
		t.Fatalf("alerts_raised_total = %d, want 1", got)
	}
}

// The acknowledge-doesn't-stick regression: after the operator acknowledges it, a
// condition that persists neither creates a row nor announces.
func TestRaiseAfterAckOfAPersistingConditionStaysQuiet(t *testing.T) {
	n, st, fh, _ := realNotifier(t)
	ctx := context.Background()
	n.Raise(ctx, standingBacklog)
	alerts, _ := st.ListAlerts(ctx, false, 0)
	if len(alerts) != 1 {
		t.Fatalf("unacked alerts = %d, want 1", len(alerts))
	}
	if err := st.AckAlert(ctx, alerts[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	for range 15 {
		n.Raise(ctx, standingBacklog)
	}
	if got := raisedEvents(fh); got != 1 {
		t.Fatalf("alert.raised events = %d, want 1 (only the first raise)", got)
	}
	all, _ := st.ListAlerts(ctx, true, 0)
	if len(all) != 1 {
		t.Fatalf("alert rows = %d, want 1: no new row while the acknowledged condition persists", len(all))
	}
	if c, _ := st.UnackedCount(ctx); c != 0 {
		t.Fatalf("unacked = %d, want 0", c)
	}
}

// Once the condition clears and returns, it is raised and announced again.
func TestRaiseAfterTheConditionClearsAndReturnsAnnouncesAgain(t *testing.T) {
	n, st, fh, _ := realNotifier(t)
	ctx := context.Background()
	n.Raise(ctx, standingBacklog)
	alerts, _ := st.ListAlerts(ctx, false, 0)
	if err := st.AckAlert(ctx, alerts[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveAlerts(ctx, "rig-01", model.AlertBacklog, "", 2); err != nil {
		t.Fatal(err)
	}
	n.Raise(ctx, standingBacklog)
	if got := raisedEvents(fh); got != 2 {
		t.Fatalf("alert.raised events = %d, want 2", got)
	}
	if c, _ := st.UnackedCount(ctx); c != 1 {
		t.Fatalf("unacked = %d, want 1", c)
	}
}
