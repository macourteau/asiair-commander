package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWritePEMReplacesUnwritableTarget pins the atomic-replace behavior: a
// regenerated cert/key must overwrite a prior file even when that file's ACL would
// deny an in-place write. A read-only existing target models that cross-platform —
// an O_TRUNC open fails, the temp+rename must succeed.
func TestWritePEMReplacesUnwritableTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(path, []byte("stale cert"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := writePEM(path, "CERTIFICATE", []byte("new-der-bytes")); err != nil {
		t.Fatalf("writePEM over a read-only target: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("target not replaced with a PEM block:\n%s", got)
	}
	// No temp file should be left behind in the directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pem-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}
