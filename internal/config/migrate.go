package config

import (
	"fmt"

	"github.com/macourteau/asiair-commander/internal/model"
)

// migration transforms a config from one schema version to the next. Each entry
// in migrations is keyed by the FROM version it upgrades; after it runs, the
// caller bumps SchemaVersion by one. Migrations are ordered by construction (the
// loop steps one version at a time), so a multi-version jump applies each in turn.
type migration func(*model.Config) error

// migrations holds the ordered forward-migration registry, keyed by the FROM
// version each step upgrades.
var migrations = map[int]migration{
	// v1 -> v2: the preview_* fields did not exist, so a loaded v1 config left them
	// at Go zero values (previews disabled, invalid concurrency/cap). Backfill the
	// defaults so an upgraded install comes up with previews enabled and valid.
	1: func(cfg *model.Config) error {
		cfg.PreviewsEnabled = defaultPreviewsEnabled
		cfg.PreviewConcurrency = defaultPreviewConcurrency
		cfg.PreviewCacheMaxBytes = defaultPreviewCacheMaxBytes
		return nil
	},
	// v2 -> v3: the plate_solve_* fields did not exist, so a loaded v2 config left
	// them at Go zero values (solving disabled, radius 0). Backfill the defaults so
	// an upgraded install comes up with plate solving enabled and a valid radius.
	2: func(cfg *model.Config) error {
		cfg.PlateSolveEnabled = defaultPlateSolveEnabled
		cfg.PlateSolveRadiusDeg = defaultPlateSolveRadiusDeg
		return nil
	},
	// v3 -> v4: reconcile_settle did not exist, so a loaded v3 config left
	// Defaults.ReconcileSettle at zero — which the settle validator rejects, failing
	// every config save and the GUID-binding applier. Backfill the shipped default.
	// (missing_display_period also arrived in v4 but zero means "show all", a valid
	// state needing no backfill.)
	3: func(cfg *model.Config) error {
		cfg.Defaults.ReconcileSettle = dur(defaultReconcileSettle)
		return nil
	},
}

// migrate forward-migrates cfg in place from its current SchemaVersion up to
// model.CurrentSchemaVersion, applying each registered step and bumping the
// version after each. A missing step for an intermediate version is a hard error
// rather than a silent gap.
//
// The registry covers the CurrentSchemaVersion track only. A file stamped with
// model.RemotesSchemaVersion means "CurrentSchemaVersion plus at least one
// remote", sits above CurrentSchemaVersion, and so never enters
// the loop. A future bump of CurrentSchemaVersion breaks that: the old
// RemotesSchemaVersion then reads as an intermediate version, and stepping from it
// would either run the wrong migration or skip one. Such a bump must move
// RemotesSchemaVersion above the new CurrentSchemaVersion and teach migrate that a
// file at the old RemotesSchemaVersion is at the old CurrentSchemaVersion before
// the loop runs.
func migrate(cfg *model.Config) error {
	for cfg.SchemaVersion < model.CurrentSchemaVersion {
		step, ok := migrations[cfg.SchemaVersion]
		if !ok {
			return fmt.Errorf("no migration registered from schema_version %d", cfg.SchemaVersion)
		}
		if err := step(cfg); err != nil {
			return fmt.Errorf("migrating from schema_version %d: %w", cfg.SchemaVersion, err)
		}
		cfg.SchemaVersion++
	}
	return nil
}
