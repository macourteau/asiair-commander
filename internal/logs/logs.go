// Package logs is the device session-log capture helper for the rig worker
// (design architecture §11). It runs within the worker's single per-rig SMB
// session — not a separate goroutine — so it shares the session's Share rather
// than opening its own.
//
// Log capture follows deliberately relaxed rules relative to science frames:
// no quiescence gate (a log is read mid-write), no device-side delete, and no
// parsing — a partial tail is an acceptable capture. A log is only re-pulled
// when its size has grown since the last recorded pull, and the NAS copy is
// always the latest whole file, overwriting any prior copy.
package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/store"
)

// Log types recorded in log_pulls.log_type (design architecture §7), chosen by
// which glob set matched.
const (
	TypePHD2    = "phd2"
	TypeAutorun = "autorun"
)

// Default selection globs (design architecture §11), matched case-insensitively
// against the basename. The confirmed ASIAIR filename shapes are
// PHD2_GuideLog_<date>_<time>.txt and Autorun_Log_<date>_<time>.txt; the latter
// has a Chinese-language _CHN twin which is excluded separately (see classify)
// since the glob alone would also match it.
var (
	defaultPHD2Globs    = []string{"*phd2*.txt", "*guide*log*.txt"}
	defaultAutorunGlobs = []string{"autorun_log_*.txt"}
)

// Puller pulls a rig's session logs into <logsRoot>/<rigID>/.
type Puller struct {
	fs       localfs.LocalFS
	store    store.Store
	logsRoot string
	logger   *slog.Logger
}

// New returns a Puller writing under logsRoot (cfg.Roots.Logs).
func New(fs localfs.LocalFS, st store.Store, logsRoot string, logger *slog.Logger) *Puller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Puller{fs: fs, store: st, logsRoot: logsRoot, logger: logger}
}

// Pull lists root/log on sh (tolerating an absent folder), selects the wanted
// PHD2 and Autorun logs, and re-pulls each whose size changed since the last
// recorded pull. One log's failure is collected and logged rather than
// aborting the rest; the worker only logs whatever Pull returns, so a log
// capture problem must never fail the frame cycle.
func (p *Puller) Pull(ctx context.Context, sh device.Share, root, rigID string) error {
	logDir := path.Join(root, "log")
	entries, err := sh.ListDir(logDir)
	if err != nil {
		if device.IsNotFound(err) {
			p.logger.Debug("logs: log folder absent, nothing to pull", "rig", rigID, "dir", logDir)
			return nil
		}
		return fmt.Errorf("logs: list %s: %w", logDir, err)
	}

	var errs []error
	for _, ent := range entries {
		if ent.IsDir {
			continue
		}
		logType, ok := classify(ent.Name)
		if !ok {
			continue
		}
		srcPath := path.Join(logDir, ent.Name)
		if err := p.pullOne(ctx, sh, rigID, srcPath, ent.Name, ent.Size, logType); err != nil {
			p.logger.Error("logs: pull failed", "rig", rigID, "source", srcPath, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", ent.Name, err))
		}
	}
	return errors.Join(errs...)
}

// pullOne pulls a single selected log if it has grown since the last recorded
// pull, overwriting any prior NAS copy, then records the pull.
func (p *Puller) pullOne(ctx context.Context, sh device.Share, rigID, srcPath, name string, size int64, logType string) error {
	prior, ok, err := p.store.GetLogPull(ctx, rigID, srcPath)
	if err != nil {
		return fmt.Errorf("get prior pull: %w", err)
	}
	if ok && prior.Size == size {
		p.logger.Debug("logs: unchanged, skipping", "rig", rigID, "source", srcPath, "size", size)
		return nil
	}

	// The logs root is OS-native (operator-typed, "\" on Windows) while rigID and
	// name are plain components; filepath.Join keeps the recorded dest_path
	// host-native so it never mixes separators the way path.Join (always "/")
	// would on Windows.
	destDir := filepath.Join(p.logsRoot, rigID)
	if err := p.fs.MkdirAll(destDir); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}
	destPath := filepath.Join(destDir, name)

	rc, err := sh.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer rc.Close()

	f, err := os.Create(p.fs.ExtendedPath(destPath))
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	n, copyErr := io.Copy(f, rc)
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s: %w", srcPath, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", destPath, closeErr)
	}

	if err := p.store.RecordLogPull(ctx, store.LogPull{
		RigID:      rigID,
		LogType:    logType,
		SourcePath: srcPath,
		DestPath:   destPath,
		Size:       n,
		PulledAt:   time.Now().Unix(),
	}); err != nil {
		return fmt.Errorf("record pull: %w", err)
	}

	p.logger.Info("logs: pulled", "rig", rigID, "type", logType, "source", srcPath, "dest", destPath, "size", n)
	return nil
}

// classify reports the log type of a basename if it is a wanted session log:
// a .txt file matching one of the PHD2 or Autorun globs, with the Autorun
// _CHN Chinese-language twin excluded (case-insensitively) so the English log
// wins (design architecture §11) — the glob itself would also match the twin.
func classify(name string) (logType string, ok bool) {
	if !strings.EqualFold(path.Ext(name), ".txt") {
		return "", false
	}
	if strings.Contains(strings.ToUpper(name), "_CHN") {
		return "", false
	}
	if matchAny(name, defaultPHD2Globs) {
		return TypePHD2, true
	}
	if matchAny(name, defaultAutorunGlobs) {
		return TypeAutorun, true
	}
	return "", false
}

// matchAny reports whether name matches any of globs, case-insensitively.
func matchAny(name string, globs []string) bool {
	lower := strings.ToLower(name)
	for _, g := range globs {
		if ok, err := path.Match(g, lower); err == nil && ok {
			return true
		}
	}
	return false
}
