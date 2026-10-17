package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/demoscenario"
)

// A mistyped path must never cost a real data dir its store: only an empty dir
// or a previous seedremote dir is seeded over.
func TestPrepareDirRefusesAForeignDataDir(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	if err := os.WriteFile(db, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDir(dir); err == nil {
		t.Fatal("prepareDir seeded over a data dir holding no scenario")
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("the foreign store is gone: %v", err)
	}
}

// A re-run starts afresh: the previous run's store and markers go, the
// scenario's directory stays usable.
func TestPrepareDirResetsAPreviousRun(t *testing.T) {
	dir := t.TempDir()
	if err := demoscenario.Save(dir, &demoscenario.Scenario{}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, demoscenario.ControlDir, demoscenario.LinkDown)
	for _, p := range []string{filepath.Join(dir, "state.db"), marker} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareDir(dir); err != nil {
		t.Fatalf("prepareDir: %v", err)
	}
	for _, p := range []string{filepath.Join(dir, "state.db"), marker} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", p)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, demoscenario.ControlDir)); err != nil || !fi.IsDir() {
		t.Errorf("control dir not recreated: %v", err)
	}
}
