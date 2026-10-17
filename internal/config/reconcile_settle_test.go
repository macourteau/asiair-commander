package config

import (
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestUpgradeV3ToV4SeedsReconcileSettle pins the upgrade path: a stored v3 config
// predates reconcile_settle, so it loads as zero. Before the v3->v4 migration that
// left Defaults.ReconcileSettle=0, which the settle validator rejects — failing
// every config save and the GUID-binding applier on every upgraded install. The
// migration must backfill the default so the migrated config validates.
func TestUpgradeV3ToV4SeedsReconcileSettle(t *testing.T) {
	cfg := Defaults()
	cfg.SchemaVersion = 3
	cfg.Defaults.ReconcileSettle = 0 // the v3 zero value
	cfg.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h"}}

	if err := migrate(cfg); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if cfg.SchemaVersion != model.CurrentSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", cfg.SchemaVersion, model.CurrentSchemaVersion)
	}
	if cfg.Defaults.ReconcileSettle.D() != defaultReconcileSettle {
		t.Errorf("reconcile_settle=%v, want %v", cfg.Defaults.ReconcileSettle.D(), defaultReconcileSettle)
	}
	if hasFieldError(Validate(cfg, nil), "rigs[rig-01].reconcile_settle") {
		t.Errorf("migrated config must pass the settle validator, got %+v", Validate(cfg, nil))
	}
}

// TestValidateReconcileSettle pins that a non-positive effective settle is rejected
// — and, unlike grace, is rejected even for a keep-originals rig, because the settle
// also drives the missing panel and validation report those rigs still surface.
func TestValidateReconcileSettle(t *testing.T) {
	zero := model.Duration(0)

	bad := Defaults()
	bad.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h", ReconcileSettle: &zero}}
	if !hasFieldError(Validate(bad, nil), "rigs[rig-01].reconcile_settle") {
		t.Errorf("reconcile_settle=0 must be rejected")
	}

	keep := Defaults()
	keep.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h", ReconcileSettle: &zero, KeepOriginals: true}}
	if !hasFieldError(Validate(keep, nil), "rigs[rig-01].reconcile_settle") {
		t.Errorf("reconcile_settle=0 must be rejected even with keep_originals")
	}

	ok := Defaults()
	ok.Rigs = []model.RigProfile{{ID: "rig-01", Host: "h"}}
	if hasFieldError(Validate(ok, nil), "rigs[rig-01].reconcile_settle") {
		t.Errorf("settle inherited from defaults must be accepted, got %+v", Validate(ok, nil))
	}
}
