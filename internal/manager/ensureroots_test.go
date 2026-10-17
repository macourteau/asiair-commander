package manager

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/localfs"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/notify"
)

func TestEnsureRootsCreatesConfiguredFolders(t *testing.T) {
	m := &Manager{lfs: localfs.New(), notify: &notify.Fake{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// A configured system (password set): all four roots (and parents) are created.
	dir := t.TempDir()
	cfg := &model.Config{PasswordHash: "hash", Roots: model.Roots{
		Light:   filepath.Join(dir, "a", "Lights"),
		Cal:     filepath.Join(dir, "a", "Calibration"),
		Staging: filepath.Join(dir, "a", ".staging"),
		Logs:    filepath.Join(dir, "b", "Logs"),
	}}
	m.ensureRoots(context.Background(), cfg)
	for _, p := range []string{cfg.Roots.Light, cfg.Roots.Cal, cfg.Roots.Staging, cfg.Roots.Logs} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			t.Fatalf("root %q not created: err=%v", p, err)
		}
	}

	// A pristine first run (no password) creates nothing — those roots are just
	// placeholders the operator replaces during setup.
	dir2 := t.TempDir()
	m.ensureRoots(context.Background(), &model.Config{Roots: model.Roots{Light: filepath.Join(dir2, "Lights")}})
	if _, err := os.Stat(filepath.Join(dir2, "Lights")); !os.IsNotExist(err) {
		t.Fatalf("first-run must not create roots; got err=%v", err)
	}
}
