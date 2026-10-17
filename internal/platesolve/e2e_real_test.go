package platesolve

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/macourteau/asiair-commander/internal/header"
)

// TestRealSolveE2E drives the REAL astap_cli against a REAL unsolved frame, proving
// the whole adapter path end to end: the command build, PLTSOLVD detection, the
// header-only .wcs read, the NAXIS overlay, and astrometry.FromHeader. It is skipped
// unless the assets are provided via env, so CI (which has no astap_cli and must
// never touch real FITS) never runs it:
//
//	ASTAP_BIN=/path/to/astap_cli \
//	ASTAP_CATALOG_DIR=/path/to/d05-tiles \
//	ASTAP_FRAME=/path/to/unsolved.fit \
//	go test ./internal/platesolve -run TestRealSolveE2E -v
//
// The frame path is read from the environment and never committed — real FITS carry
// PII (design architecture §14.2).
func TestRealSolveE2E(t *testing.T) {
	bin := os.Getenv("ASTAP_BIN")
	catDir := os.Getenv("ASTAP_CATALOG_DIR")
	frame := os.Getenv("ASTAP_FRAME")
	if bin == "" || catDir == "" || frame == "" {
		t.Skip("set ASTAP_BIN, ASTAP_CATALOG_DIR and ASTAP_FRAME to run the real end-to-end solve")
	}

	// A provisioner shell with just the binary path + a work dir (same-package access
	// to the unexported fields; no download/store needed for a direct solve).
	prov := &Provisioner{astapDir: t.TempDir(), binaryPath: bin}
	if err := os.MkdirAll(prov.WorkDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	// Read the frame's real hints/optics for the request.
	h, err := header.New().ReadHeader(frame)
	if err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	ra, okRA := h.Float("RA")
	dec, okDec := h.Float("DEC")
	focal, _ := h.Float("FOCALLEN")
	pix, _ := h.Float("XPIXSZ")
	n1, _ := h.Float("NAXIS1")
	n2, _ := h.Float("NAXIS2")
	if !okRA || !okDec {
		t.Fatalf("frame %s has no RA/DEC hint", filepath.Base(frame))
	}
	fovHeight := 206.265 * pix / focal * n2 / 3600.0

	solver := NewASTAP(prov, header.New(), slog.Default())
	res, err := solver.Solve(context.Background(), Request{
		Path:       frame,
		HintRA:     ra,
		HintDec:    dec,
		FOVHeight:  fovHeight,
		RadiusDeg:  10,
		CatalogDir: catDir,
		NAXIS1:     int(n1),
		NAXIS2:     int(n2),
	})
	if err != nil {
		t.Fatalf("Solve returned an operational error: %v", err)
	}
	if res.Solution == nil {
		t.Fatalf("no solution (reason: %q) — expected a solve for %s", res.Reason, filepath.Base(frame))
	}

	sol := res.Solution
	t.Logf("solved %s: center RA=%.4f Dec=%.4f  rot=%.2f°  scale=%.3f\"/px  FOV=%.2f×%.2f°  flipped=%v",
		filepath.Base(frame), sol.CenterRA, sol.CenterDec, sol.PositionAngle, sol.PixelScale,
		sol.FOVWidth, sol.FOVHeight, sol.Flipped)

	// The solved center must land near the pointing hint (a hinted solve refines, not
	// relocates). A few degrees of slack covers the object-vs-frame-center offset.
	if math.Abs(angularSepDeg(sol.CenterRA, sol.CenterDec, ra, dec)) > 3 {
		t.Errorf("solved center (%.3f,%.3f) is >3° from the hint (%.3f,%.3f)", sol.CenterRA, sol.CenterDec, ra, dec)
	}
	// The .wcs had no NAXIS1/2; a non-zero FOV proves the overlay worked.
	if sol.FOVHeight <= 0 || sol.FOVWidth <= 0 {
		t.Errorf("FOV is zero (%v×%v) — the NAXIS overlay did not take effect", sol.FOVWidth, sol.FOVHeight)
	}
	if sol.PixelScale <= 0 {
		t.Errorf("non-physical pixel scale %v", sol.PixelScale)
	}
}

// angularSepDeg is the great-circle separation between two sky points (degrees).
func angularSepDeg(ra1, dec1, ra2, dec2 float64) float64 {
	const d2r = math.Pi / 180
	a1, d1, a2, d2 := ra1*d2r, dec1*d2r, ra2*d2r, dec2*d2r
	cos := math.Sin(d1)*math.Sin(d2) + math.Cos(d1)*math.Cos(d2)*math.Cos(a1-a2)
	if cos > 1 {
		cos = 1
	}
	if cos < -1 {
		cos = -1
	}
	return math.Acos(cos) / d2r
}
