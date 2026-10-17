package rig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
	"github.com/macourteau/asiair-commander/internal/panel"
)

// objectFixture returns the light fixture's bytes with its OBJECT card rewritten
// to target, so a test can file a frame under any producer target it likes.
func objectFixture(t *testing.T, target string) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.fit")
	writeLocal(t, src, fixture(t, "asiair_light_2600mm_ha.fit"))
	dst := filepath.Join(dir, "dst.fit")
	if _, err := header.RewriteObject(src, dst, target); err != nil {
		t.Fatalf("RewriteObject(%q): %v", target, err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read rewritten fixture: %v", err)
	}
	return b
}

// dateObsFixture returns objectFixture's bytes with the DATE-OBS card rewritten
// too, so a test can file frames belonging to several different nights. The repo
// ships one light fixture carrying one DATE-OBS, and the night a frame is filed
// under is derived from that card, so anything asserting over more than one
// night needs this.
//
// The value is patched in place at its own length: FITS header cards are fixed
// 80-byte records, so a shorter or longer replacement would shift every
// following card and invalidate the header. obs must therefore be spelled
// exactly as the fixture spells it, to the microsecond.
func dateObsFixture(t *testing.T, target, obs string) []byte {
	t.Helper()
	b := objectFixture(t, target)
	const card = "DATE-OBS= '"
	i := bytes.Index(b, []byte(card))
	if i < 0 {
		t.Fatal("fixture has no DATE-OBS card")
	}
	start := i + len(card)
	width := bytes.IndexByte(b[start:], '\'')
	if width < 0 {
		t.Fatal("fixture DATE-OBS card is unterminated")
	}
	if len(obs) != width {
		t.Fatalf("DATE-OBS %q is %d characters; the fixture's card holds exactly %d", obs, len(obs), width)
	}
	copy(b[start:start+width], obs)
	return b
}

// TestFiledFramePanesComeFromRawTarget is the rig half of design
// routing-and-filing AC20: the pane columns are populated for a frame filed while
// mosaic detection is OFF, and they are parsed from the RAW target — the string
// frames.target holds — not the space-stripped one routing uses to pick a
// directory.
//
// The "NGC 7000_1-1" case is the one that matters. With the shipped
// strip_target_spaces the frame files under "NGC7000_1-1", so a live path parsing
// the transformed value would record base "NGC7000" while the store migration —
// which has no config to read the setting from — backfills "NGC 7000" for an
// identical pre-existing row, splitting one mosaic across two groups. The expected
// values here are the ones internal/store's TestUpgradeV7ToV8BackfillsPanes
// asserts for that same target.
func TestFiledFramePanesComeFromRawTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		target   string
		base     string
		row, col int
		destPart string
	}{
		{"spaced base", "NGC 7000_1-1", "NGC 7000", 1, 1, "NGC7000_1-1"},
		{"plain pane", "Sadr_2-5", "Sadr", 2, 5, "Sadr_2-5"},
		{"not a pane", "NGC6888", "", 0, 0, "NGC6888"},
		{"date suffix", "M42_2026-07", "", 0, 0, "M42_2026-07"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, emmc, header.New())
			// The two settings this case turns on: detection off (the pane columns are
			// derived data, not a routing decision, so they are written anyway) and space
			// stripping on (what makes raw and transformed disagree).
			if h.cfg.MosaicDetection {
				t.Fatal("mosaic detection must be off for this case")
			}
			if !h.cfg.StripTargetSpaces {
				t.Fatal("the harness is expected to strip target spaces")
			}

			const src = "Autorun/Light_001.fit"
			h.dev.AddFile(emmc, src, objectFixture(t, tc.target), h.nowVal.Add(-10*time.Minute))
			h.cycle()

			f, ok := h.getFrame(src)
			if !ok {
				t.Fatalf("frame not filed")
			}
			if f.State != model.StateVerified {
				t.Fatalf("state = %s, want verified", f.State)
			}
			if f.Target != tc.target {
				t.Fatalf("target = %q, want the raw producer value %q", f.Target, tc.target)
			}
			if f.PanelBase != tc.base || f.PanelRow != tc.row || f.PanelCol != tc.col {
				t.Errorf("panes = %q %d %d, want %q %d %d",
					f.PanelBase, f.PanelRow, f.PanelCol, tc.base, tc.row, tc.col)
			}
			// The same function the store migration's backfill calls, on the same string.
			base, _, row, col, _ := panel.ParsePanel(f.Target)
			if f.PanelBase != base || f.PanelRow != row || f.PanelCol != col {
				t.Errorf("stored panes %q %d %d disagree with ParsePanel of the stored target: %q %d %d",
					f.PanelBase, f.PanelRow, f.PanelCol, base, row, col)
			}
			// Detection is off, so the frame routes through the light template — and the
			// directory carries the TRANSFORMED target, which is the divergence the raw
			// parse exists to avoid.
			if !strings.Contains(f.DestPath, tc.destPart) {
				t.Errorf("dest_path %q does not carry the routed target %q", f.DestPath, tc.destPart)
			}
		})
	}
}
