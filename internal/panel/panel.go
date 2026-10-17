// Package panel parses the mosaic panel suffix the ASIAIR's plan feature
// appends to a target name.
//
// Shooting a mosaic emits one plan entry — and therefore one distinct OBJECT
// value — per pane, named "<base>_<row>-<col>": Sadr_1-1 … Sadr_2-5 for a 2×5
// grid. Recovering the base and the pane indices from that string is what lets
// a mosaic's panes be filed under one parent folder instead of as ten sibling
// top-level directories.
//
// This is a producer-format parse backed by a plausibility bound, not a
// contract the data carries: no FITS header identifies a frame as part of a
// mosaic, so the name is the only signal. The underscore requirement and the
// 1..MaxIndex bound on the pane indices between them reject catalogue
// designations as normally written (Sh2-101, K 1-16, Abell 2-1) and ordinary
// date suffixes (M42_2026-07, M42_12-25), but a designation spelled with an
// underscore for its space and carrying small numbers — Abell_2-1 — does parse
// as a pane. That residual class is why mosaic detection is opt-in rather than
// always on; see design routing-and-filing §7.
//
// It is a leaf package with no dependencies on the rest of the tree because
// two layers apart need it: routing, which parses the transformed target to
// pick a template, and the store migration that backfills the panel columns
// from the raw stored target. A second, approximate parser written in SQL for
// the migration could diverge from the one routing uses, which is the failure
// this package exists to make impossible.
package panel

import (
	"regexp"
	"strconv"
)

// MaxIndex bounds a pane's row and column. ASIAIR panes are 1-indexed and
// grids are small — the planner's own inputs are pane counts, not coordinates
// — so this ceiling costs nothing real and is what rejects a date suffix such
// as M42_2026-07 or M42_12-25, which otherwise has exactly the shape of a pane
// name. Without it such a frame would silently start routing through the
// mosaic template, and re-file would then offer to move archived data on the
// strength of a false positive.
const MaxIndex = 20

// panelSuffix matches the trailing "_<row>-<col>" of a pane name. It is
// end-anchored so that only the final suffix is considered: a target that
// somehow carries two — "Sadr_1-2_3-4" — yields base "Sadr_1-2", the same
// answer a reader gets from looking at it.
var panelSuffix = regexp.MustCompile(`_([0-9]+)-([0-9]+)$`)

// ParsePanel splits a mosaic pane's target into its base name and pane
// coordinates. ok reports whether target has the pane shape at all; when it is
// false every other return is zero and the caller treats the target as an
// ordinary one.
//
// base is a pure prefix slice of target — neither trimmed nor otherwise
// altered — and panel reproduces the suffix verbatim, so base + "_" + panel
// reconstructs the input exactly. Routing depends on that: it splits the
// already-transformed target, and the guarantee is what makes
// "<TARGET_BASE>_<PANEL>" and "<TARGET>" render the same directory by
// construction rather than by coincidence. It is also why the zero-padding of
// a name like Sadr_01-02 survives in panel while row and col report 1 and 2.
//
// An empty base — target "_1-1" — parses successfully. A frame whose base name
// is unknowable has no correct home, and refusing it here would file it flat
// instead; the caller quarantines it, which is loud and recoverable.
func ParsePanel(target string) (base, panel string, row, col int, ok bool) {
	m := panelSuffix.FindStringSubmatchIndex(target)
	if m == nil {
		return "", "", 0, 0, false
	}

	// Atoi fails on an index too long for an int; the bound below rejects
	// anything merely implausible.
	row, err := strconv.Atoi(target[m[2]:m[3]])
	if err != nil {
		return "", "", 0, 0, false
	}
	col, err = strconv.Atoi(target[m[4]:m[5]])
	if err != nil {
		return "", "", 0, 0, false
	}
	if row < 1 || row > MaxIndex || col < 1 || col > MaxIndex {
		return "", "", 0, 0, false
	}

	// m[0] is the underscore; base excludes it and panel starts after it.
	return target[:m[0]], target[m[0]+1:], row, col, true
}
