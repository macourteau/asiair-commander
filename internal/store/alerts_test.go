package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

func backlogAlert(at int64) model.Alert {
	return model.Alert{
		RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertBacklog,
		Message: "frames can't be removed", CreatedAt: at, Standing: true,
	}
}

func mustRaise(t *testing.T, s Store, a model.Alert) (int64, bool) {
	t.Helper()
	id, created, err := s.RaiseAlert(context.Background(), a)
	if err != nil {
		t.Fatalf("RaiseAlert: %v", err)
	}
	return id, created
}

func alertRows(t *testing.T, s Store) int {
	t.Helper()
	all, err := s.ListAlerts(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	return len(all)
}

// A repeat of an unacknowledged alert lands on its row and reports that nothing
// was created, for both kinds of raise: the notifier keys every announcement on
// that report (design architecture §10).
func TestRaiseAlertReportsWhetherItCreatedARow(t *testing.T) {
	for _, standing := range []bool{false, true} {
		s := newStore(t)
		a := backlogAlert(100)
		a.Standing = standing
		id1, created := mustRaise(t, s, a)
		if !created {
			t.Fatalf("standing=%v: first raise must create a row", standing)
		}
		a.CreatedAt = 200
		id2, created := mustRaise(t, s, a)
		if created || id2 != id1 {
			t.Fatalf("standing=%v: repeat raise = (id %d, created %v), want (id %d, created false)", standing, id2, created, id1)
		}
		if n := alertRows(t, s); n != 1 {
			t.Fatalf("standing=%v: rows = %d, want 1", standing, n)
		}
	}
}

// The acknowledge-doesn't-stick regression: a standing condition that persists
// after the operator acknowledges it must not mint a new unacked row.
func TestStandingAlertStaysAcknowledgedWhileItsConditionPersists(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, _ := mustRaise(t, s, backlogAlert(100))
	if err := s.AckAlert(ctx, id, 150); err != nil {
		t.Fatal(err)
	}
	for at := int64(200); at < 260; at += 4 {
		got, created := mustRaise(t, s, backlogAlert(at))
		if created || got != id {
			t.Fatalf("raise at %d after ack = (id %d, created %v), want (id %d, created false)", at, got, created, id)
		}
	}
	if n, _ := s.UnackedCount(ctx); n != 0 {
		t.Fatalf("unacked = %d, want 0: the acknowledged condition must stay quiet", n)
	}
	if n := alertRows(t, s); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// Once the condition clears (resolve) and comes back, it is a new occurrence and
// raises a fresh row.
func TestStandingAlertRaisesAgainAfterResolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, _ := mustRaise(t, s, backlogAlert(100))
	if err := s.AckAlert(ctx, id, 150); err != nil {
		t.Fatal(err)
	}
	n, err := s.ResolveAlerts(ctx, "rig-01", model.AlertBacklog, "", 300)
	if err != nil || n != 1 {
		t.Fatalf("ResolveAlerts = %d, %v; want 1, nil", n, err)
	}
	id2, created := mustRaise(t, s, backlogAlert(400))
	if !created || id2 == id {
		t.Fatalf("raise after resolve = (id %d, created %v), want a new row", id2, created)
	}
	if n, _ := s.UnackedCount(ctx); n != 1 {
		t.Fatalf("unacked = %d, want 1", n)
	}
}

// Resolving an unacknowledged alert acknowledges it, keeping the badge honest; an
// operator's earlier acknowledgement time is kept.
func TestResolveAlertsAcknowledgesAndKeepsTheOperatorsAckTime(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	open, _ := mustRaise(t, s, backlogAlert(100))
	acked, _ := mustRaise(t, s, model.Alert{RigID: "rig-01", Severity: model.SeverityWarn,
		Category: model.AlertBacklog, Message: "other", CreatedAt: 100, Standing: true})
	if err := s.AckAlert(ctx, acked, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAlerts(ctx, "rig-01", model.AlertBacklog, "", 300); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListAlerts(ctx, true, 0)
	for _, a := range all {
		want := int64(300)
		if a.ID == acked {
			want = 150
		}
		if a.AckedAt != want {
			t.Errorf("alert %d (open=%v) acked_at = %d, want %d", a.ID, a.ID == open, a.AckedAt, want)
		}
	}
}

// ResolveAlerts touches only its scope: the rig (NULL for fleet-wide), the
// category, and the message prefix when one is given.
func TestResolveAlertsScope(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	raise := func(rig string, c model.Category, msg string) int64 {
		id, _ := mustRaise(t, s, model.Alert{RigID: rig, Severity: model.SeverityWarn, Category: c, Message: msg, CreatedAt: 1, Standing: true})
		return id
	}
	fleetA := raise("", model.AlertRemoteStalled, "remote a: down")
	fleetB := raise("", model.AlertRemoteStalled, "remote b: down")
	rigA := raise("rig-01", model.AlertRemoteStalled, "remote a: down")
	other := raise("", model.AlertNASDiskLow, "remote a: down")

	n, err := s.ResolveAlerts(ctx, "", model.AlertRemoteStalled, "remote a: ", 10)
	if err != nil || n != 1 {
		t.Fatalf("ResolveAlerts = %d, %v; want 1, nil", n, err)
	}
	all, _ := s.ListAlerts(ctx, true, 0)
	for _, a := range all {
		if wantAcked := a.ID == fleetA; (a.AckedAt != 0) != wantAcked {
			t.Errorf("alert %d acked=%v, want %v (fleetA=%d fleetB=%d rigA=%d other=%d)",
				a.ID, a.AckedAt != 0, wantAcked, fleetA, fleetB, rigA, other)
		}
	}
}

// An event alert (not standing) keeps its semantics: once acknowledged, the same
// problem happening again is news and raises a new row.
func TestEventAlertRaisesAgainAfterAck(t *testing.T) {
	s := newStore(t)
	a := model.Alert{RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertTransferFailed, Message: "transfer failed for x", CreatedAt: 1}
	id, _ := mustRaise(t, s, a)
	if err := s.AckAlert(context.Background(), id, 2); err != nil {
		t.Fatal(err)
	}
	id2, created := mustRaise(t, s, a)
	if !created || id2 == id {
		t.Fatalf("event raise after ack = (id %d, created %v), want a new row", id2, created)
	}
}

// The acknowledgement survives a restart: the suppression is read from the row
// itself, so the first cycle of a new process raising the same standing condition
// finds it.
func TestStandingAlertStaysAcknowledgedAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := mustRaise(t, s, backlogAlert(100))
	if err := s.AckAlert(ctx, id, 150); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, created := mustRaise(t, s, backlogAlert(200)); created || got != id {
		t.Fatalf("raise after reopen = (id %d, created %v), want (id %d, created false)", got, created, id)
	}
}

// Upgrading to v11 reads every row acknowledged before it as resolved: the old
// schema cannot tell an operator's ack from a resolve, and treating an old
// auto-acked row as standing would silence a condition that later returns.
func TestUpgradeV10ToV11BackfillsResolvedAt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version > 10 {
			break
		}
		if err := m.up(ctx, tx); err != nil {
			t.Fatalf("migrate%d: %v", m.version, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO meta(key,value) VALUES('schema_version','10')`,
		`INSERT INTO alerts(rig_id,severity,category,message,created_at,acked_at) VALUES('rig-01','warn','backlog','old acked',1,2)`,
		`INSERT INTO alerts(rig_id,severity,category,message,created_at,acked_at) VALUES('rig-01','warn','backlog','still open',1,NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer s.Close()
	standing := func(msg string) model.Alert {
		return model.Alert{RigID: "rig-01", Severity: model.SeverityWarn, Category: model.AlertBacklog, Message: msg, CreatedAt: 5, Standing: true}
	}
	if _, created := mustRaise(t, s, standing("old acked")); !created {
		t.Fatal("a row acknowledged before the upgrade must read as resolved, so its condition raises anew")
	}
	if _, created := mustRaise(t, s, standing("still open")); created {
		t.Fatal("an unacknowledged row from before the upgrade must still dedup its condition")
	}
}

// ListNeverDeletable is the frames the backlog alert counts and links to: held
// and failed frames, and unclassified ones not culled, whether or not they were
// ever pulled.
func TestListNeverDeletable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	seed := []*model.Frame{
		{RigID: "rig-01", SourceShare: "s", SourcePath: "held", Size: 1, Mtime: 1, State: model.StateHeld, PulledAt: 1000, DestPath: "a"},
		{RigID: "rig-01", SourceShare: "s", SourcePath: "failed", Size: 1, Mtime: 1, State: model.StateFailed},
		{RigID: "rig-01", SourceShare: "s", SourcePath: "unclassified", Size: 1, Mtime: 1, State: model.StateUnclassified, PulledAt: 1000, DestPath: "u"},
		{RigID: "rig-01", SourceShare: "s", SourcePath: "culled", Size: 1, Mtime: 1, State: model.StateUnclassified, PulledAt: 1000, DestPath: "c"},
		{RigID: "rig-01", SourceShare: "s", SourcePath: "verified", Size: 1, Mtime: 1, State: model.StateVerified, PulledAt: 1000, DestPath: "v"},
		{RigID: "rig-02", SourceShare: "s", SourcePath: "held2", Size: 1, Mtime: 1, State: model.StateHeld, PulledAt: 1000, DestPath: "h2"},
	}
	for _, f := range seed {
		if err := s.UpsertFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
		if f.SourcePath == "culled" {
			if err := s.SetFrameMissing(ctx, f.ID, 1500); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, total, err := s.ListNeverDeletable(ctx, "rig-01", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range got {
		paths = append(paths, f.SourcePath)
	}
	// Newest first, like the rest of the transfer history.
	want := []string{"unclassified", "failed", "held"}
	if total != 3 || len(paths) != 3 || paths[0] != want[0] || paths[1] != want[1] || paths[2] != want[2] {
		t.Fatalf("ListNeverDeletable = %v (total %d), want %v", paths, total, want)
	}
	if _, total, _ := s.ListNeverDeletable(ctx, "", 0, 50); total != 4 {
		t.Fatalf("fleet-wide total = %d, want 4", total)
	}
}
