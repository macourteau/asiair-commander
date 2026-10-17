package config

import (
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// TestUpgradeV2ToV3SeedsPlateSolve pins that a v2 config (no plate_solve_* fields)
// forward-migrates to v3 with plate solving enabled and a valid radius, so an
// upgraded install comes up ready to solve rather than with solving disabled.
func TestUpgradeV2ToV3SeedsPlateSolve(t *testing.T) {
	cfg := Defaults()
	cfg.SchemaVersion = 2
	cfg.PlateSolveEnabled = false // v2 zero values
	cfg.PlateSolveRadiusDeg = 0

	if err := migrate(cfg); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if cfg.SchemaVersion != model.CurrentSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", cfg.SchemaVersion, model.CurrentSchemaVersion)
	}
	if !cfg.PlateSolveEnabled {
		t.Error("plate_solve_enabled should be seeded true on upgrade")
	}
	if cfg.PlateSolveRadiusDeg != defaultPlateSolveRadiusDeg {
		t.Errorf("plate_solve_radius_deg=%d, want %d", cfg.PlateSolveRadiusDeg, defaultPlateSolveRadiusDeg)
	}
}

func TestValidatePlateSolveRadius(t *testing.T) {
	for _, r := range []int{0, -1, 61, 100} {
		cfg := Defaults()
		cfg.PlateSolveRadiusDeg = r
		if !hasFieldError(Validate(cfg, nil), "plate_solve_radius_deg") {
			t.Errorf("radius %d should be rejected", r)
		}
	}
	for _, r := range []int{1, 10, 60} {
		cfg := Defaults()
		cfg.PlateSolveRadiusDeg = r
		if hasFieldError(Validate(cfg, nil), "plate_solve_radius_deg") {
			t.Errorf("radius %d should be accepted", r)
		}
	}
}
