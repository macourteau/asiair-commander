package rig

import (
	"context"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/notify"
)

func backlogRows(t *testing.T, h *harness) []model.Alert {
	t.Helper()
	all, err := h.store.ListAlerts(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	var out []model.Alert
	for _, a := range all {
		if a.Category == model.AlertBacklog {
			out = append(out, a)
		}
	}
	return out
}

// The backlog alert is re-checked every poll. Through the real notifier and store,
// an acknowledged backlog that persists must stay acknowledged (no new row, nothing
// on the badge), and only a backlog that clears and returns raises again.
func TestBacklogAlertStaysAcknowledgedUntilTheBacklogClears(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.w.notify = notify.New(h.store, h.hub, nil, nil)
	const roomy = int64(1 << 40)

	held := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Light/held.fit",
		Size: 100, Mtime: 1, State: model.StateHeld, DestPath: "/nas/held.fit",
	}
	if err := h.store.UpsertFrame(ctx, held); err != nil {
		t.Fatal(err)
	}
	poll := func(n int) {
		for range n {
			h.w.checkBacklogAndCapacity(ctx, h.cfg, h.profile, roomy)
		}
	}

	poll(10)
	rows := backlogRows(t, h)
	if len(rows) != 1 {
		t.Fatalf("backlog rows after 10 polls = %d, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Message, `"Test"`) {
		t.Errorf("backlog message %q must name the rig by its display name", rows[0].Message)
	}
	// The message says what clears each kind, matching what the History view the
	// alert links to offers: no promise of an action that does not exist.
	for _, want := range []string{"reclassify or dismiss", "until a retry succeeds", "confirm their NAS copy"} {
		if !strings.Contains(rows[0].Message, want) {
			t.Errorf("backlog message %q lacks %q", rows[0].Message, want)
		}
	}
	if strings.Contains(rows[0].Message, "fixed or dismissed") {
		t.Errorf("backlog message %q promises a fix or dismissal for every frame", rows[0].Message)
	}

	if err := h.store.AckAlert(ctx, rows[0].ID, h.nowVal.Unix()); err != nil {
		t.Fatal(err)
	}
	poll(10)
	if rows := backlogRows(t, h); len(rows) != 1 {
		t.Fatalf("backlog rows after ack and 10 more polls = %d, want 1: an acknowledged backlog that persists must not re-raise", len(rows))
	}
	if n, _ := h.store.UnackedCount(ctx); n != 0 {
		t.Fatalf("unacked = %d, want 0", n)
	}

	// The backlog clears, then a frame is held again.
	held.State = model.StateVerified
	if err := h.store.UpsertFrame(ctx, held); err != nil {
		t.Fatal(err)
	}
	poll(1)
	held.State = model.StateHeld
	if err := h.store.UpsertFrame(ctx, held); err != nil {
		t.Fatal(err)
	}
	poll(3)
	if rows := backlogRows(t, h); len(rows) != 2 {
		t.Fatalf("backlog rows after clear and return = %d, want 2", len(rows))
	}
	if n, _ := h.store.UnackedCount(ctx); n != 1 {
		t.Fatalf("unacked = %d, want 1", n)
	}
}

// The library-mismatch alert is re-raised by every scan retry while the wrong
// library is mounted. An acknowledgement holds until the root confirms again, and
// a confirmed root resolves only its own alert.
func TestLibraryMismatchAlertStaysAcknowledgedUntilTheRootConfirms(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.w.notify = notify.New(h.store, h.hub, nil, nil)
	rows := func() []model.Alert {
		all, err := h.store.ListAlerts(ctx, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		return all
	}
	const root, other = "/nas/light", "/nas/cal"

	h.w.handleRootUnavailable(ctx, other, "different library")
	for range 3 {
		h.w.handleRootUnavailable(ctx, root, "different library")
	}
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows = %d, want 2 (one per root)", len(got))
	}
	for _, a := range rows() {
		if err := h.store.AckAlert(ctx, a.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		h.w.handleRootUnavailable(ctx, root, "different library")
		h.w.handleRootUnavailable(ctx, other, "different library")
	}
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows after ack and more scans = %d, want 2", len(got))
	}

	h.w.clearRootUnavailable(ctx, root)
	h.w.handleRootUnavailable(ctx, root, "different library")
	h.w.handleRootUnavailable(ctx, other, "different library")
	if got := rows(); len(got) != 3 {
		t.Fatalf("rows after one root confirmed and mismatched again = %d, want 3", len(got))
	}
	if n, _ := h.store.UnackedCount(ctx); n != 1 {
		t.Fatalf("unacked = %d, want 1 (only the root that confirmed and failed again)", n)
	}
}

// The backlog escalates to "capture may stop" while the device is low on a
// capacity-constrained rig. When the device frees up the escalation is over even
// though the backlog remains, so each later onset is a new critical alert that is
// announced, not one absorbed by an acknowledged earlier row.
func TestBacklogCriticalAnnouncesEachOnset(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.w.notify = notify.New(h.store, h.hub, nil, nil)
	held := &model.Frame{
		RigID: "rig-01", SourceShare: emmc, SourcePath: "Autorun/Light/held.fit",
		Size: 100, Mtime: 1, State: model.StateHeld, DestPath: "/nas/held.fit",
	}
	if err := h.store.UpsertFrame(ctx, held); err != nil {
		t.Fatal(err)
	}
	const roomy, low = int64(1 << 40), int64(1)
	poll := func(free int64) { h.w.checkBacklogAndCapacity(ctx, h.cfg, h.profile, free) }
	criticals := func() []model.Alert {
		var out []model.Alert
		for _, a := range backlogRows(t, h) {
			if a.Severity == model.SeverityCritical {
				out = append(out, a)
			}
		}
		return out
	}

	poll(roomy) // warn
	poll(low)   // critical onset
	poll(low)
	c := criticals()
	if len(c) != 1 {
		t.Fatalf("critical rows = %d, want 1", len(c))
	}
	if err := h.store.AckAlert(ctx, c[0].ID, 1); err != nil {
		t.Fatal(err)
	}
	poll(low) // still low: the acknowledgement holds
	if got := len(criticals()); got != 1 {
		t.Fatalf("critical rows while still low after ack = %d, want 1", got)
	}
	poll(roomy) // device frees up; the backlog remains
	poll(low)   // a second onset
	c = criticals()
	if len(c) != 2 || c[0].AckedAt != 0 && c[1].AckedAt != 0 {
		t.Fatalf("critical rows after a second onset = %+v, want 2 with the new one unacknowledged", c)
	}
}

// A root alert stays standing until its root confirms. A root removed from the
// config never confirms again, so its alerts are resolved by the next scan; else
// re-adding the root later with the same fault would be silent behind an old ack.
func TestRootAlertsOfARemovedRootAreResolved(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	h.w.notify = notify.New(h.store, h.hub, nil, nil)
	kept, removed := h.cfg.Roots.Light, "/nas/old-library"

	h.w.handleRootUnavailable(ctx, kept, "different library")
	h.w.handleRootUnavailable(ctx, removed, "different library")
	all, _ := h.store.ListAlerts(ctx, false, 0)
	for _, a := range all {
		if err := h.store.AckAlert(ctx, a.ID, 1); err != nil {
			t.Fatal(err)
		}
	}

	h.w.resolveRemovedRootAlerts(ctx, scanRoots(h.cfg))

	// The removed root, re-added with the same fault, raises anew; the configured
	// root's acknowledged alert still holds.
	h.w.handleRootUnavailable(ctx, removed, "different library")
	h.w.handleRootUnavailable(ctx, kept, "different library")
	rows, _ := h.store.ListAlerts(ctx, true, 0)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if n, _ := h.store.UnackedCount(ctx); n != 1 {
		t.Fatalf("unacked = %d, want 1 (the re-added root)", n)
	}
}
