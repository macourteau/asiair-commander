package panel

import (
	"strings"
	"testing"
)

func TestParsePanel(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		wantOK    bool
		wantBase  string
		wantPanel string
		wantRow   int
		wantCol   int
	}{
		// ASIAIR's own mosaic pane naming.
		{"first pane", "Sadr_1-1", true, "Sadr", "1-1", 1, 1},
		{"last pane of a 2x5", "Sadr_2-5", true, "Sadr", "2-5", 2, 5},
		{"double-digit column", "Sadr_1-10", true, "Sadr", "1-10", 1, 10},
		{"base with a space", "NGC 7000_1-1", true, "NGC 7000", "1-1", 1, 1},
		{"base that is itself hyphenated", "Sh2-101_1-1", true, "Sh2-101", "1-1", 1, 1},

		// Catalogue designations as normally written. The underscore
		// requirement rejects every one of them.
		{"sharpless", "Sh2-101", false, "", "", 0, 0},
		{"messier with space", "M 1-92", false, "", "", 0, 0},
		{"kohoutek with space", "K 1-16", false, "", "", 0, 0},
		{"abell with space", "Abell 2-1", false, "", "", 0, 0},
		{"perek-kohoutek", "PK 164+31.1", false, "", "", 0, 0},
		{"plain target", "NGC6888", false, "", "", 0, 0},

		// Date suffixes have the pane shape and are rejected by the bound.
		// These are the false positives that would otherwise let re-file
		// offer to move archived data.
		{"iso year-month", "M42_2026-07", false, "", "", 0, 0},
		{"iso year-month other", "NGC7000_2024-11", false, "", "", 0, 0},
		{"month-day", "M42_12-25", false, "", "", 0, 0},
		{"zero row and column", "Sadr_0-0", false, "", "", 0, 0},
		{"zero row", "Sadr_0-1", false, "", "", 0, 0},
		{"zero column", "Sadr_1-0", false, "", "", 0, 0},
		{"row above the bound", "Sadr_21-1", false, "", "", 0, 0},
		{"column above the bound", "Sadr_1-21", false, "", "", 0, 0},
		{"row exactly at the bound", "Sadr_20-20", true, "Sadr", "20-20", 20, 20},

		// The accepted residual false-positive class, pinned so that
		// changing the bound is a deliberate decision rather than a
		// side effect. Both of these are real catalogue designations
		// spelled with an underscore for their space.
		{"abell with underscore", "Abell_2-1", true, "Abell", "2-1", 2, 1},
		{"kohoutek with underscore", "K_1-16", true, "K", "1-16", 1, 16},

		// Shape failures.
		{"empty", "", false, "", "", 0, 0},
		{"suffix not at the end", "Sadr_1-1 v2", false, "", "", 0, 0},
		{"no underscore before the pane", "Sadr1-1", false, "", "", 0, 0},
		{"space between underscore and pane", "Sadr_ 1-1", false, "", "", 0, 0},
		{"space inside the pane", "Sadr_1 -1", false, "", "", 0, 0},
		{"non-numeric row", "Sadr_a-1", false, "", "", 0, 0},
		{"three parts", "Sadr_1-1-1", false, "", "", 0, 0},
		{"negative-looking row", "Sadr_-1-1", false, "", "", 0, 0},

		// Degenerate but parseable: the caller quarantines an empty base
		// rather than filing the frame under nothing.
		{"empty base", "_1-1", true, "", "1-1", 1, 1},

		// Zero-padding survives in panel while row and col report the
		// parsed integers — the reason the default template renders
		// <PANEL> rather than <PANEL_ROW>/<PANEL_COL>.
		{"zero-padded pane", "Sadr_01-02", true, "Sadr", "01-02", 1, 2},

		// Only the final suffix is considered.
		{"two pane suffixes", "Sadr_1-2_3-4", true, "Sadr_1-2", "3-4", 3, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, pnl, row, col, ok := ParsePanel(tt.target)
			if ok != tt.wantOK {
				t.Fatalf("ParsePanel(%q) ok = %v, want %v", tt.target, ok, tt.wantOK)
			}
			if base != tt.wantBase || pnl != tt.wantPanel || row != tt.wantRow || col != tt.wantCol {
				t.Errorf("ParsePanel(%q) = (%q, %q, %d, %d), want (%q, %q, %d, %d)",
					tt.target, base, pnl, row, col, tt.wantBase, tt.wantPanel, tt.wantRow, tt.wantCol)
			}
		})
	}
}

// TestParsePanelReassembles is the property routing depends on: splitting the
// transformed target and rejoining the halves must reproduce it exactly, or
// "<TARGET_BASE>_<PANEL>" and "<TARGET>" would render different directories.
func TestParsePanelReassembles(t *testing.T) {
	targets := []string{
		"Sadr_1-1",
		"NGC 7000_1-1",
		"Sh2-101_1-20",
		"_1-1",
		"Sadr_01-02",
		"Sadr_1-2_3-4",
		"  Sadr  _1-1",
	}
	for _, target := range targets {
		base, pnl, _, _, ok := ParsePanel(target)
		if !ok {
			t.Fatalf("ParsePanel(%q) did not parse", target)
		}
		if got := base + "_" + pnl; got != target {
			t.Errorf("reassembly of %q = %q, want the input back", target, got)
		}
	}
}

// TestParsePanelRejectsOverlongIndex covers the strconv failure path, which the
// bound alone cannot reach.
func TestParsePanelRejectsOverlongIndex(t *testing.T) {
	target := "Sadr_" + strings.Repeat("9", 40) + "-1"
	if _, _, _, _, ok := ParsePanel(target); ok {
		t.Errorf("ParsePanel(%q) parsed an index too large for an int", target)
	}
}
