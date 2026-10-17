package rig

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/classify"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/route"
	"github.com/macourteau/asiair-commander/internal/store"
)

// frameExtensions are the local FITS extensions a representative-frame scan
// accepts when reading one header per NIGHT_n folder.
var frameExtensions = map[string]bool{".fit": true, ".fits": true, ".fts": true}

// RebuildNightCounters restores the night_counters table by walking the local
// light root for NIGHT_n folders (design architecture §7 startup reconciliation).
// It runs to completion BEFORE any rig worker starts — the manager calls it
// pre-Run — so a live pull can never race the rebuild and mis-number a night. It
// reads exactly one representative frame's header per NIGHT_n folder for the
// OBJECT and DATE-OBS, recovers the OTA by matching folder segments against the
// configured OTA set, and never renumbers (RebuildNightCounters preserves existing
// ordinals). An empty NIGHT_n folder anchors no date and is skipped.
func RebuildNightCounters(ctx context.Context, cfg *model.Config, st store.Store, hdr header.Reader, lfs localfs.LocalFS, siteLoc *time.Location, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if siteLoc == nil {
		siteLoc = time.Local
	}

	// No light root configured yet (first run before setup): there are no NIGHT
	// folders to rebuild. Bail before the walk — an empty root resolves to the
	// drive root and would enumerate protected system directories (e.g.
	// C:\Recovery), which a low-privilege service account cannot read.
	if strings.TrimSpace(cfg.Roots.Light) == "" {
		return nil
	}

	otaSet := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if r.OTA != "" {
			otaSet[r.OTA] = true
		}
	}

	var entries []store.NightEntry
	var walk func(dir string, segs []string) error
	walk = func(dir string, segs []string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		des, err := os.ReadDir(lfs.ExtendedPath(dir))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // the light root does not exist yet — nothing to rebuild
			}
			return err
		}
		for _, de := range des {
			if !de.IsDir() {
				continue
			}
			name := de.Name()
			child := dir + "/" + name
			childSegs := append(append([]string{}, segs...), name)
			if n, ok := parseNightFolder(name); ok {
				if e, ok := representativeNightEntry(child, childSegs, otaSet, cfg, hdr, lfs, siteLoc, n); ok {
					entries = append(entries, e)
				} else {
					logger.Debug("rig: NIGHT folder anchors no date; skipping", "dir", child)
				}
				continue // frames live directly inside; do not descend further
			}
			if err := walk(child, childSegs); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(cfg.Roots.Light, nil); err != nil {
		return err
	}
	if len(entries) == 0 {
		logger.Info("rig: night-counter rebuild found no NIGHT folders")
		return nil
	}
	if err := st.RebuildNightCounters(ctx, entries); err != nil {
		return err
	}
	logger.Info("rig: night-counter rebuild complete", "entries", len(entries))
	return nil
}

// representativeNightEntry reads one frame's header from a NIGHT_n folder and
// builds its NightEntry. It returns ok=false for an empty folder or one whose
// frames lack OBJECT/DATE-OBS (anchoring no date).
func representativeNightEntry(nightDir string, segs []string, otaSet map[string]bool, cfg *model.Config, hdr header.Reader, lfs localfs.LocalFS, siteLoc *time.Location, night int) (store.NightEntry, bool) {
	des, err := os.ReadDir(lfs.ExtendedPath(nightDir))
	if err != nil {
		return store.NightEntry{}, false
	}
	for _, de := range des {
		if de.IsDir() {
			continue
		}
		if !frameExtensions[strings.ToLower(filepath.Ext(de.Name()))] {
			continue
		}
		h, err := hdr.ReadHeader(nightDir + "/" + de.Name())
		if err != nil {
			continue
		}
		cls := classify.Classify(h, cfg.AliasTable, cfg.SensorMap)
		if !cls.TargetPresent || !cls.DateObsPresent {
			continue
		}
		obs := route.ObservingDate(cls.DateObs, siteLoc)
		return store.NightEntry{
			Target:        cls.Target,
			OTA:           firstInSet(segs, otaSet),
			ObservingDate: route.ObservingDateKey(obs),
			Night:         night,
		}, true
	}
	return store.NightEntry{}, false
}

// parseNightFolder parses a "NIGHT_<n>" folder name into its ordinal.
func parseNightFolder(name string) (int, bool) {
	const prefix = "NIGHT_"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(prefix):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// firstInSet returns the first segment present in set, or "" — the OTA recovered
// from a NIGHT_n folder's path, matched against the configured OTA values rather
// than reverse-parsing the editable template (design architecture §7).
func firstInSet(segs []string, set map[string]bool) string {
	for _, s := range segs {
		if set[s] {
			return s
		}
	}
	return ""
}
