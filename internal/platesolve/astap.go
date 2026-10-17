package platesolve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/macourteau/asiair-commander/internal/astrometry"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// solveTimeout bounds a single astap_cli invocation so a hung solve can't wedge the
// worker. A hinted solve is sub-second; 120 s is a wide safety margin.
const solveTimeout = 120 * time.Second

// astapSolver shells out to astap_cli. It resolves the binary through the
// provisioner and writes astap's output files under a per-solve temp dir it owns,
// so nothing is ever written next to the source frame on the NAS and a crashed
// prior solve's leftovers can never be read as this frame's result.
type astapSolver struct {
	prov *Provisioner
	hdr  header.Reader
	log  *slog.Logger
}

// NewASTAP builds the real astap_cli-backed Solver.
func NewASTAP(prov *Provisioner, hdr header.Reader, logger *slog.Logger) Solver {
	if logger == nil {
		logger = slog.Default()
	}
	return &astapSolver{prov: prov, hdr: hdr, log: logger}
}

// Solve runs one hinted solve. The command and success-detection rules were
// validated empirically (design features §3.3): -ra is in HOURS, -spd is Dec+90,
// -fov is the field HEIGHT in degrees, success is PLTSOLVD=T in the .ini (never the
// exit code), and the .wcs lacks NAXIS1/2 so the source axis lengths are injected
// before FromHeader.
func (s *astapSolver) Solve(ctx context.Context, req Request) (Result, error) {
	bin := s.prov.BinaryPath()
	if bin == "" {
		return Result{}, fmt.Errorf("astap: no solver binary resolved")
	}
	if req.CatalogDir == "" {
		return Result{}, fmt.Errorf("astap: no catalog dir for %s", req.Path)
	}
	if _, err := os.Stat(req.Path); err != nil {
		return Result{}, fmt.Errorf("astap: source frame unreadable: %w", err)
	}

	// A per-solve temp dir under our own work area. RemoveAll on defer takes every
	// byproduct astap writes (.ini/.wcs/.axy/.log) even when the deadline kills it.
	dir, err := os.MkdirTemp(s.prov.WorkDir(), "solve-")
	if err != nil {
		return Result{}, fmt.Errorf("astap: make work dir: %w", err)
	}
	defer os.RemoveAll(dir)
	base := filepath.Join(dir, "solve")

	runCtx, cancel := context.WithTimeout(ctx, solveTimeout)
	defer cancel()

	// astap_cli exits 0 even on failure, so its exit status is not consulted for the
	// solve verdict; it matters only to distinguish "ran but no .ini" (operational)
	// from a clean F. exec.Command passes argv without a shell, so spaces in the
	// frame path need no quoting.
	args := []string{
		"-f", req.Path,
		"-ra", fmt.Sprintf("%.6f", req.HintRA/15.0), // degrees → hours
		"-spd", fmt.Sprintf("%.6f", req.HintDec+90.0), // south-polar distance
		"-fov", fmt.Sprintf("%.6f", req.FOVHeight), // field height, degrees
		"-r", fmt.Sprintf("%.3f", req.RadiusDeg),
		"-z", "0",
		"-d", req.CatalogDir,
		"-wcs",
		"-o", base,
	}
	cmd := exec.CommandContext(runCtx, bin, args...)
	runErr := cmd.Run()

	iniBytes, iniErr := os.ReadFile(base + ".ini")
	if iniErr != nil {
		// No .ini at all: astap didn't run to completion (binary/catalog/path fault or
		// a killed process). Operational → transient, distinct from a clean F.
		if runCtx.Err() != nil {
			return Result{}, fmt.Errorf("astap: %s: %w", req.Path, ErrSolveTimeout)
		}
		return Result{}, fmt.Errorf("astap: no .ini after solve of %s (run err: %v): %w", req.Path, runErr, iniErr)
	}

	solved, ok := parsePLTSOLVD(iniBytes)
	if !ok {
		return Result{}, fmt.Errorf("astap: .ini for %s carried no PLTSOLVD line", req.Path)
	}
	if !solved {
		return Result{Reason: "no star match"}, nil
	}

	sol, err := s.solutionFromWCS(base+".wcs", req.NAXIS1, req.NAXIS2)
	if err != nil {
		// PLTSOLVD=T but the .wcs is missing/unparseable: a CLEAN failed, not an
		// operational error — otherwise this frame would retry forever. The worker
		// budgets this per-frame parse fault (design features §3.6).
		s.log.Warn("astap: solved but .wcs unreadable", "frame", req.Path, "error", err)
		return Result{Reason: "solved but .wcs unreadable"}, nil
	}
	return Result{Solution: sol}, nil
}

// solutionFromWCS reads astap's header-only .wcs output, injects the source
// NAXIS1/2 (the .wcs carries NAXIS=0 and no NAXIS1/2, so FromHeader would otherwise
// report FOV 0), and runs the identical Tier-0 extractor.
func (s *astapSolver) solutionFromWCS(wcsPath string, n1, n2 int) (*model.WCSSolution, error) {
	h, err := s.hdr.ReadHeader(wcsPath)
	if err != nil {
		return nil, err
	}
	sol := astrometry.FromHeader(withNAXIS(h, n1, n2)).Solution
	if sol == nil {
		return nil, fmt.Errorf("astap: .wcs %s carried no usable WCS", wcsPath)
	}
	return sol, nil
}

// withNAXIS returns a copy of h with NAXIS1/NAXIS2 appended last. model.Header has
// no mutator, so the cards are rebuilt (Keys→Card) and the two axis cards appended;
// NewHeader's later-duplicate-wins makes the appended values override a .wcs that
// carries NAXIS1=0.
func withNAXIS(h model.Header, n1, n2 int) model.Header {
	keys := h.Keys()
	cards := make([]model.Card, 0, len(keys)+2)
	for _, k := range keys {
		if c, ok := h.Card(k); ok {
			cards = append(cards, c)
		}
	}
	cards = append(cards,
		model.Card{Key: "NAXIS1", Value: int64(n1)},
		model.Card{Key: "NAXIS2", Value: int64(n2)})
	return model.NewHeader(cards)
}

// parsePLTSOLVD scans astap's .ini for the PLTSOLVD line, returning whether the
// solve succeeded and whether the line was present at all. The .ini is a flat
// key=value text file; PLTSOLVD=T means solved, PLTSOLVD=F means no match.
func parsePLTSOLVD(ini []byte) (solved, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(ini))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		v, found := strings.CutPrefix(line, "PLTSOLVD=")
		if !found {
			continue
		}
		return strings.EqualFold(strings.TrimSpace(v), "T"), true
	}
	return false, false
}
