package store

import (
	"context"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// MissingGroupCounts must return a non-nil slice when nothing is missing, so the
// API marshals it to a JSON [] rather than null — the missing panel reads
// groups.length directly, and a null there crashes the page (drained rig).
func TestMissingGroupCountsEmptyIsNonNil(t *testing.T) {
	s := newStore(t)
	groups, _, err := s.MissingGroupCounts(context.Background(), "rig-none", SectionActionable)
	if err != nil {
		t.Fatalf("MissingGroupCounts: %v", err)
	}
	if groups == nil {
		t.Fatal("empty result must be a non-nil slice (marshals to [], not null)")
	}
	if len(groups) != 0 {
		t.Fatalf("expected no groups, got %d", len(groups))
	}
}

// TestMissingListingIgnoresAge is the regression for the banner-vs-panel divergence:
// the safe-to-power-off banner counts every marked-missing row with no time cutoff,
// so the panel must too. A frame marked missing far in the past (a short
// missing_display_period once excluded it server-side, opening an empty panel under
// the red banner) must still be listed and counted.
func TestMissingListingIgnoresAge(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// seen=1 → the unix epoch's second second, arbitrarily far below any realistic
	// missing_display_period cutoff.
	id := mkMissingVerified(t, s, "rig", "old.fit", "/lib/M31/old.fit", 1)

	frames, total, err := s.ListMissingFrames(ctx, "rig", MissingFilter{}, 0, 50)
	if err != nil {
		t.Fatalf("ListMissingFrames: %v", err)
	}
	if total != 1 || len(frames) != 1 || frames[0].ID != id {
		t.Fatalf("an ancient missing frame must still be listed: total=%d frames=%v", total, frames)
	}

	groups, _, err := s.MissingGroupCounts(ctx, "rig", SectionActionable)
	if err != nil {
		t.Fatalf("MissingGroupCounts: %v", err)
	}
	if len(groups) != 1 || groups[0].Dir != "/lib/M31" || groups[0].Count != 1 {
		t.Fatalf("an ancient missing frame must still be counted, got %+v", groups)
	}
}

// ClearRestoreRequest must be rig-scoped (like RequestRestore): a cancel routed
// through one rig's endpoint must never clear another rig's frame.
func TestClearRestoreRequestIsRigScoped(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	fb := &model.Frame{RigID: "rig-b", SourceShare: "sh", SourcePath: "b.fit",
		Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "/lib/b.fit", SHA256: "h"}
	if err := s.UpsertFrame(ctx, fb); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetFrameMissing(ctx, fb.ID, 100); err != nil {
		t.Fatalf("set missing: %v", err)
	}
	if ok, err := s.RequestRestore(ctx, "rig-b", fb.ID, 200); err != nil || !ok {
		t.Fatalf("request restore: ok=%v err=%v", ok, err)
	}

	// Cancel through the wrong rig: the request must survive.
	if err := s.ClearRestoreRequest(ctx, "rig-a", fb.ID); err != nil {
		t.Fatalf("clear (wrong rig): %v", err)
	}
	got, _, err := s.GetFrameByID(ctx, fb.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RestoreRequestedAt == 0 {
		t.Fatal("a cross-rig cancel cleared rig-b's restore request")
	}

	// The owning rig clears it.
	if err := s.ClearRestoreRequest(ctx, "rig-b", fb.ID); err != nil {
		t.Fatalf("clear (right rig): %v", err)
	}
	got, _, err = s.GetFrameByID(ctx, fb.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RestoreRequestedAt != 0 {
		t.Fatal("the owning rig's cancel did not clear the request")
	}
}

// UnderRoot must be separator-anchored and case-folded, because it gates a
// deletion authorizer: a prefix that is not a path boundary must not match.
func TestUnderRootContainment(t *testing.T) {
	cases := []struct {
		dest, root string
		want       bool
	}{
		{`/astro/light/M31/x.fit`, `/astro/light`, true},
		{`/astro/light`, `/astro/light`, true},                // the root itself
		{`/astro/light-archive/x.fit`, `/astro/light`, false}, // not a separator boundary
		{`/ASTRO/Light/x.fit`, `/astro/light`, true},          // case-folded
		{`/astro/cal/x.fit`, `/astro/light`, false},
		{`/astro/light/x.fit`, ``, false}, // empty root contains nothing
	}
	for _, c := range cases {
		if got := UnderRoot(c.dest, c.root); got != c.want {
			t.Errorf("UnderRoot(%q, %q) = %v, want %v", c.dest, c.root, got, c.want)
		}
	}
}

// longestRoot assigns a nested path to the most specific configured root, so a
// row under cal (nested in light) is never swept by light's walk.
func TestLongestRootAssignment(t *testing.T) {
	roots := []string{`/astro`, `/astro/cal`}
	if got := longestRoot(`/astro/cal/dark.fit`, roots); got != `/astro/cal` {
		t.Errorf("nested path assigned to %q, want /astro/cal", got)
	}
	if got := longestRoot(`/astro/light/x.fit`, roots); got != `/astro` {
		t.Errorf("light path assigned to %q, want /astro", got)
	}
	if got := longestRoot(`/elsewhere/x.fit`, roots); got != `` {
		t.Errorf("outside path assigned to %q, want empty", got)
	}
	// Specificity is measured on the cleaned path: a root with redundant
	// separators must not out-rank a genuinely more specific nested root.
	sloppy := []string{`/astro////`, `/astro/cal`}
	if got := longestRoot(`/astro/cal/dark.fit`, sloppy); got != `/astro/cal` {
		t.Errorf("nested path assigned to %q, want /astro/cal", got)
	}
}

// FiledSamplesUnderRoot must carry the archive-side anchors, since the auto-link
// verify compares the on-disk copy against archive_sha256 at archive_size rather
// than re-deriving an expectation from the device anchors (library.go, design
// frame-lifecycle §11.1). The overlay is carried beside them as the audit record it
// is; a nil one must come back "".
func TestFiledSamplesUnderRootCarriesTheArchiveAnchors(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// A retargeted filed frame under /lib.
	rt := &model.Frame{RigID: "rig-s", SourceShare: "Udisk", SourcePath: "rt.fit",
		SHA256: "orig", RetargetSHA256: "arch", RetargetFrom: "Moon",
		ArchiveSHA256: "arch", ArchiveSize: 12,
		Size: 10, Mtime: 1, State: model.StateVerified, DestPath: "/lib/M31/rt.fit"}
	if err := s.UpsertFrame(ctx, rt); err != nil {
		t.Fatalf("upsert retargeted: %v", err)
	}
	// A plain (never-retargeted) filed frame under the same root.
	plain := &model.Frame{RigID: "rig-s", SourceShare: "Udisk", SourcePath: "plain.fit",
		SHA256: "phash", ArchiveSHA256: "phash", ArchiveSize: 10,
		Size: 10, Mtime: 1, State: model.StateVerified, DestPath: "/lib/M31/plain.fit"}
	if err := s.UpsertFrame(ctx, plain); err != nil {
		t.Fatalf("upsert plain: %v", err)
	}

	samples, err := s.FiledSamplesUnderRoot(ctx, "rig-s", "/lib", []string{"/lib"}, 8)
	if err != nil {
		t.Fatalf("FiledSamplesUnderRoot: %v", err)
	}
	byDest := map[string]FiledSample{}
	for _, sm := range samples {
		byDest[sm.DestPath] = sm
	}
	if got := byDest["/lib/M31/rt.fit"]; got.ArchiveSHA256 != "arch" || got.ArchiveSize != 12 {
		t.Fatalf("retargeted sample archive anchors: sha=%q size=%d, want arch/12", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got := byDest["/lib/M31/rt.fit"]; got.RetargetSHA256 != "arch" || got.SHA256 != "orig" {
		t.Fatalf("retargeted sample: sha=%q retarget=%q, want orig/arch", got.SHA256, got.RetargetSHA256)
	}
	if got := byDest["/lib/M31/plain.fit"]; got.ArchiveSHA256 != "phash" || got.ArchiveSize != 10 {
		t.Fatalf("plain sample archive anchors: sha=%q size=%d, want phash/10", got.ArchiveSHA256, got.ArchiveSize)
	}
	if got := byDest["/lib/M31/plain.fit"]; got.RetargetSHA256 != "" {
		t.Fatalf("plain sample must have empty overlay, got %q", got.RetargetSHA256)
	}
}
