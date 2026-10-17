package rig

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wcsHeader is a minimal but valid tangent-plane WCS the extractor will solve.
func wcsHeader() model.Header {
	return model.NewHeader([]model.Card{
		{Key: "CTYPE1", Value: "RA---TAN"},
		{Key: "CTYPE2", Value: "DEC--TAN"},
		{Key: "CRVAL1", Value: 180.0},
		{Key: "CRVAL2", Value: 0.0},
		{Key: "CRPIX1", Value: 50.5},
		{Key: "CRPIX2", Value: 50.5},
		{Key: "CD1_1", Value: -1.0 / 3600.0},
		{Key: "CD2_2", Value: 1.0 / 3600.0},
		{Key: "NAXIS1", Value: int64(100)},
		{Key: "NAXIS2", Value: int64(100)},
	})
}

func seedFrame(t *testing.T, st store.Store, path string) *model.Frame {
	t.Helper()
	f := &model.Frame{
		RigID: "rig", SourceShare: "Udisk", SourcePath: path,
		Size: 1, Mtime: 1, FrameType: "LIGHT", State: model.StateVerified,
		DestPath: path, // dest_path is the file the backfill stats/reads
	}
	if err := st.UpsertFrame(context.Background(), f); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	return f
}

func TestBackfillAstrometry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Three outcomes:
	//  - solvable: file on disk + a mapped header → status "header" with a solution.
	//  - parseFail: file on disk but no mapped header → stat ok, read fails → "none".
	//  - missing: no file on disk → stat fails → left NULL (transient).
	solvablePath := filepath.Join(dir, "solvable.fit")
	parseFailPath := filepath.Join(dir, "parsefail.fit")
	missingPath := filepath.Join(dir, "missing.fit")
	if err := os.WriteFile(solvablePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parseFailPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	solvable := seedFrame(t, st, solvablePath)
	parseFail := seedFrame(t, st, parseFailPath)
	missing := seedFrame(t, st, missingPath)

	fake := header.Fake{Headers: map[string]model.Header{solvablePath: wcsHeader()}}
	BackfillAstrometry(ctx, st, fake, nil, discardLogger())

	get := func(f *model.Frame) *model.Astrometry {
		got, ok, err := st.GetFrameByID(ctx, f.ID)
		if err != nil || !ok {
			t.Fatalf("GetFrameByID(%d): ok=%v err=%v", f.ID, ok, err)
		}
		return got.Astrometry
	}

	if a := get(solvable); a == nil || a.Status != model.AstroStatusHeader || a.Solution == nil {
		t.Errorf("solvable frame: got %+v, want header + solution", a)
	}
	if a := get(parseFail); a == nil || a.Status != model.AstroStatusNone || a.Solution != nil {
		t.Errorf("parse-fail frame: got %+v, want status none, no solution", a)
	}
	if a := get(missing); a != nil {
		t.Errorf("missing-file frame: got %+v, want nil (left NULL for retry)", a)
	}

	// The still-NULL transient row remains in the queue for a next-startup retry;
	// the two processed rows do not.
	need, err := st.ListFramesNeedingAstrometry(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(need) != 1 || need[0].ID != missing.ID {
		t.Fatalf("queue after backfill = %d rows, want only the missing frame", len(need))
	}
}
