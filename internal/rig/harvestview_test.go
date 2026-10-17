package rig

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/header"
	"github.com/macourteau/asiair-commander/internal/model"
)

// harvestViewProfile watches Autorun and Plan on EMMC, Autorun on the Udisk, and
// Live on TF through a disabled source.
func harvestViewProfile() *model.RigProfile {
	return &model.RigProfile{
		ID: "rig-01", Managed: true,
		Sources: []model.Source{
			{Share: emmc, Roots: []string{"Autorun", "Plan"}},
			{Share: udisk, Roots: []string{"Autorun"}},
			{Share: "TF Images", Roots: []string{"Live"}, Disabled: true},
		},
	}
}

var harvestViewExclude = []string{"jpg", "jpeg"}

const hvFrame = "Light_M42_300.0s_Bin1_2600MM_gain100_20261007-231502_-10.0C_0001.fit"
const hvPreview = "Light_M42_300.0s_Bin1_2600MM_gain100_20261007-231502_-10.0C_0001_thn.jpg"

func TestHarvestViewTable(t *testing.T) {
	p := harvestViewProfile()
	files := []struct {
		share, path        string
		candidate, preview bool
	}{
		// Flat layout.
		{emmc, "Autorun/Light/M42/" + hvFrame, true, false},
		{emmc, "Autorun/Light/M42/" + hvPreview, false, true},
		{emmc, "Autorun/" + hvFrame, true, false},
		{emmc, "Plan/Light/x.fit", true, false},
		{emmc, "Autorun/notes.txt", true, false}, // pairListing calls anything non-excluded a frame
		{emmc, "Autorun/Light/preview.JPEG", false, true},
		// Under ASIAIR/ (either layout is accepted on any share).
		{udisk, "ASIAIR/Autorun/Light/M42/" + hvFrame, true, false},
		{udisk, "ASIAIR/Autorun/Light/M42/" + hvPreview, false, true},
		{udisk, "Autorun/Light/" + hvFrame, true, false},
		{emmc, "ASIAIR/Plan/x.fit", true, false},
		// Mixed case on the prefix and the root; segments below keep their case.
		{emmc, "AUTORUN/Light/x.fit", true, false},
		{emmc, "autorun/Light/x_thn.JPG", false, true},
		{udisk, "asiair/AutoRun/Light/x.fit", true, false},
		{udisk, "Asiair/AUTORUN/Light/x_thn.jpg", false, true},
		// Outside every root.
		{emmc, hvFrame, false, false},
		{emmc, "Live/x.fit", false, false},
		{emmc, "ASIAIR/x.fit", false, false},
		{emmc, "ASIAIR/Live/x.fit", false, false},
		{emmc, "AutorunX/x.fit", false, false},
		{emmc, "log/x.fit", false, false},
		{emmc, "Autorun", false, false},        // the root itself is not a file
		{emmc, "ASIAIR/Autorun", false, false}, // nor in the nested layout
		{"Other Share", "Autorun/x.fit", false, false},
		// Share names match case-insensitively, as Samba resolves them.
		{"EMMC IMAGES", "Autorun/Light/x.fit", true, false},
		{"udisk images", "ASIAIR/Autorun/Light/x_thn.jpg", false, true},
		{"tf images", "Live/x.fit", false, false}, // still disabled
		// Disabled source.
		{"TF Images", "Live/x.fit", false, false},
		{"TF Images", "Live/x_thn.jpg", false, false},
		// Junk folders below the root are pruned; junk files dropped.
		{emmc, "Autorun/batch_stack_tmp/x.fit", false, false},
		{emmc, "Autorun/Light/@eaDir/x_thn.jpg", false, false},
		{udisk, "ASIAIR/Autorun/.Trashes/x.fit", false, false},
		{emmc, "Autorun/Light/._x.fit", false, false},
		{emmc, "Autorun/Light/.DS_Store", false, false},
		{emmc, "Autorun/Light/x.fit.tmp", false, false},
		{emmc, "Autorun/.asiair-commander-media-id", false, false},
		{udisk, "ASIAIR/Autorun/Thumbs.db", false, false},
	}
	for _, f := range files {
		if got := HarvestCandidate(p, harvestViewExclude, f.share, f.path); got != f.candidate {
			t.Errorf("HarvestCandidate(%q, %q) = %v, want %v", f.share, f.path, got, f.candidate)
		}
		if got := HarvestPreview(p, harvestViewExclude, f.share, f.path); got != f.preview {
			t.Errorf("HarvestPreview(%q, %q) = %v, want %v", f.share, f.path, got, f.preview)
		}
	}
	// With no excluded extensions a _thn.jpg is a frame, as pairListing would have it.
	if !HarvestCandidate(p, nil, emmc, "Autorun/"+hvPreview) || HarvestPreview(p, nil, emmc, "Autorun/"+hvPreview) {
		t.Errorf("with no exclude list a preview must read as a candidate")
	}

	dirs := []struct {
		share, dir    string
		watched, root bool
	}{
		{emmc, "Autorun", true, true},
		{emmc, "Autorun/", true, true},
		{emmc, "AUTORUN", true, true},
		{emmc, "Plan", true, true},
		{emmc, "Autorun/Light/M42", true, false},
		{emmc, "ASIAIR/Autorun", true, true},
		{udisk, "ASIAIR/Autorun", true, true},
		{udisk, "asiair/autorun", true, true},
		{udisk, "Autorun", true, true}, // both layouts badge on a Udisk holding both
		{udisk, "ASIAIR/AUTORUN/Light", true, false},
		{emmc, "", false, false},
		{emmc, "ASIAIR", false, false},
		{udisk, "ASIAIR", false, false},
		{emmc, "Live", false, false},
		{emmc, "ASIAIR/Live", false, false},
		{emmc, "AutorunX", false, false},
		{"TF Images", "Live", false, false},
		{"Other Share", "Autorun", false, false},
		{"EMMC IMAGES", "Autorun", true, true},
		{"udisk images", "ASIAIR/Autorun/Light", true, false},
		{emmc, "Autorun/batch_stack_tmp", false, false},
		{emmc, "Autorun/Light/@eaDir/sub", false, false},
		{udisk, "ASIAIR/Autorun/System Volume Information", false, false},
	}
	for _, d := range dirs {
		if got := WatchedDir(p, d.share, d.dir); got != d.watched {
			t.Errorf("WatchedDir(%q, %q) = %v, want %v", d.share, d.dir, got, d.watched)
		}
		if got := WatchedRoot(p, d.share, d.dir); got != d.root {
			t.Errorf("WatchedRoot(%q, %q) = %v, want %v", d.share, d.dir, got, d.root)
		}
	}

	// The watched match over a given source list: every configured source,
	// disabled ones included, where WatchedDir passes the enabled ones only.
	all := p.Sources
	in := []struct {
		share, p      string
		under         bool
		watchedDirKey string // watchedDirIn's key; "" when it answers false
	}{
		{"TF Images", "Live", true, "Live"},
		{"TF Images", "LIVE/sub/x.fit", true, "Live"},
		{"tf images", "ASIAIR/Live/sub", true, "ASIAIR/Live"},
		{emmc, "Autorun/Light", true, "Autorun"},
		{udisk, "asiair/autorun", true, "ASIAIR/Autorun"},
		{emmc, "Autorun/batch_stack_tmp", true, ""}, // under the root, but pruned by the walk
		{emmc, "Autorun/batch_stack_tmp/sub", true, ""},
		{emmc, "Live", false, ""}, // Live is configured on TF only
		{emmc, "ASIAIR", false, ""},
		{emmc, "", false, ""},
		{"Other Share", "Autorun", false, ""},
	}
	for _, c := range in {
		if got := UnderWatchedRoot(all, c.share, c.p); got != c.under {
			t.Errorf("UnderWatchedRoot(all, %q, %q) = %v, want %v", c.share, c.p, got, c.under)
		}
		key, ok := watchedDirIn(all, c.share, c.p)
		if key != c.watchedDirKey || ok != (c.watchedDirKey != "") {
			t.Errorf("watchedDirIn(all, %q, %q) = %q, %v; want %q", c.share, c.p, key, ok, c.watchedDirKey)
		}
	}
	if WatchedDir(p, "TF Images", "Live") || UnderWatchedRoot(p.EnabledSources(), "TF Images", "Live/x.fit") {
		t.Errorf("a disabled source's root must stay unwatched for the enabled sources")
	}

	multi := []model.Source{{Share: emmc, Roots: []string{"Foo/Bar"}}}
	holds := []struct {
		sources  []model.Source
		share, p string
		want     bool
	}{
		{all, emmc, "Autorun", true},
		{all, emmc, "AUTORUN", true},
		{all, emmc, "Autorun/", true},
		{all, emmc, "Plan", true},
		{all, emmc, "ASIAIR", true}, // holds the nested layout's ASIAIR/Autorun on any share
		{all, emmc, "ASIAIR/Autorun", true},
		{all, emmc, "asiair/plan", true},
		{all, emmc, "ASIAIR/Autorun/Light", false}, // inside a root, not holding one
		{all, emmc, "Autorun/Light", false},
		{all, emmc, "AutorunX", false},
		{all, emmc, "Preview", false},
		{all, emmc, "", false}, // the share root is never an entry
		{all, udisk, "ASIAIR", true},
		{all, udisk, "asiair", true},
		{all, udisk, "Autorun", true},
		{all, udisk, "Plan", false},
		{all, "TF Images", "Live", true}, // a disabled source, when passed
		{all, "TF Images", "ASIAIR", true},
		{p.EnabledSources(), "TF Images", "Live", false},
		{p.EnabledSources(), "TF Images", "ASIAIR", false},
		{all, "Other Share", "Autorun", false},
		{all, "EMMC IMAGES", "autorun", true},
		{multi, emmc, "Foo", true},
		{multi, emmc, "FOO", true},
		{multi, emmc, "Foo/Bar", true},
		{multi, emmc, "foo/bar", true},
		{multi, emmc, "ASIAIR/Foo", true},
		{multi, emmc, "ASIAIR", true},
		{multi, emmc, "Foo/Bar/Baz", false},
		{multi, emmc, "Foo/Baz", false},
		{multi, emmc, "Bar", false},
	}
	for _, h := range holds {
		if got := HoldsWatchedRoot(h.sources, h.share, h.p); got != h.want {
			t.Errorf("HoldsWatchedRoot(%v, %q, %q) = %v, want %v", h.sources, h.share, h.p, got, h.want)
		}
	}
}

// renameRuleSources: Autorun and Foo/Bar enabled and Plan disabled on EMMC,
// Autorun enabled on the Udisk, Live disabled on TF.
func renameRuleSources() []model.Source {
	return []model.Source{
		{Share: emmc, Roots: []string{"Autorun", "Foo/Bar"}},
		{Share: emmc, Roots: []string{"Plan"}, Disabled: true},
		{Share: udisk, Roots: []string{"Autorun"}},
		{Share: "TF Images", Roots: []string{"Live"}, Disabled: true},
	}
}

func TestRenameRefusalTable(t *testing.T) {
	srcs := renameRuleSources()
	none := RenameRefusal{}
	r1 := func(cause RenameCause, root string) RenameRefusal {
		return RenameRefusal{Rule: RenameInWatched, Cause: cause, Root: root}
	}
	r2 := func(cause RenameCause, root string) RenameRefusal {
		return RenameRefusal{Rule: RenameReserved, Cause: cause, Root: root}
	}
	from := []struct {
		sources          []model.Source
		store            []string
		share, dir, name string
		isDir            bool
		want             RenameRefusal
	}{
		// R1: the item's folder lies in a watched root, in either layout and any case.
		{srcs, nil, emmc, "Autorun/Light/M42", hvFrame, false, r1(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "Autorun", "Light", true, r1(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "AUTORUN/light", "x_thn.jpg", false, r1(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "ASIAIR/Autorun", "x.fit", false, r1(RenameCauseEnabled, "ASIAIR/Autorun")},
		{srcs, nil, udisk, "asiair/autorun/Light", "x.fit", false, r1(RenameCauseEnabled, "ASIAIR/Autorun")},
		{srcs, nil, emmc, "Foo/Bar", "x.fit", false, r1(RenameCauseEnabled, "Foo/Bar")},
		// A junk folder is refused itself; what it holds is not watched.
		{srcs, nil, emmc, "Autorun", "batch_stack_tmp", true, r1(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "Autorun/batch_stack_tmp", "stack.fit", false, none},
		{srcs, nil, udisk, "ASIAIR/Autorun/@eaDir", "sub", true, none},
		// R2, old name: a folder that is, or holds, a reserved path.
		{srcs, nil, emmc, "", "Autorun", true, r2(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "", "autorun", true, r2(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "", "Foo", true, r2(RenameCauseEnabled, "Foo/Bar")},
		{srcs, nil, emmc, "", "ASIAIR", true, r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{srcs, nil, emmc, "ASIAIR", "Autorun", true, r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{srcs, nil, udisk, "", "ASIAIR", true, r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{nil, nil, udisk, "", "ASIAIR", true, r2(RenameCauseDataFolder, "ASIAIR")},
		{nil, nil, udisk, "", "Asiair", true, r2(RenameCauseDataFolder, "ASIAIR")},
		{nil, nil, emmc, "", "ASIAIR", true, none}, // a fixed share's ASIAIR holds no marker
		// A file named like a reserved path stays renamable.
		{srcs, nil, emmc, "", "Autorun", false, none},
		{srcs, nil, udisk, "", "ASIAIR", false, none},
		{nil, nil, udisk, "", "ASIAIR", false, none},
		// A disabled source counts, with its own cause.
		{srcs, nil, emmc, "Plan/Light", "x.fit", false, r1(RenameCauseDisabled, "Plan")},
		{srcs, nil, emmc, "", "Plan", true, r2(RenameCauseDisabled, "Plan")},
		{srcs, nil, "TF Images", "Live", "x.fit", false, r1(RenameCauseDisabled, "Live")},
		// Store-kept roots, flat and nested, cause store only when nothing configured matches.
		{srcs, []string{"Old"}, emmc, "Old/Light", "a.fit", false, r1(RenameCauseStore, "Old")},
		{srcs, []string{"Old"}, emmc, "OLD", "a.fit", false, r1(RenameCauseStore, "Old")},
		{srcs, []string{"Old"}, emmc, "", "Old", true, r2(RenameCauseStore, "Old")},
		{srcs, []string{"Old"}, emmc, "", "ASIAIR", true, r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{nil, []string{"ASIAIR/Autorun"}, emmc, "ASIAIR/Autorun", "a.fit", false, r1(RenameCauseStore, "ASIAIR/Autorun")},
		{nil, []string{"ASIAIR/Autorun"}, emmc, "", "ASIAIR", true, r2(RenameCauseStore, "ASIAIR/Autorun")},
		{nil, []string{"ASIAIR/Autorun"}, emmc, "", "Autorun", true, none}, // the nested root as it stands
		{nil, []string{"ASIAIR/Old"}, udisk, "", "ASIAIR", true, r2(RenameCauseDataFolder, "ASIAIR")},
		{srcs, []string{"Autorun"}, emmc, "Autorun", "x.fit", false, r1(RenameCauseEnabled, "Autorun")},
		{srcs, []string{"Plan"}, emmc, "Plan", "x.fit", false, r1(RenameCauseDisabled, "Plan")},
		{srcs, []string{"Plan"}, emmc, "", "Plan", true, r2(RenameCauseDisabled, "Plan")},
		{srcs, nil, emmc, "Old/Light", "a.fit", false, none}, // drained: no longer kept
		// Outside every root.
		{srcs, nil, emmc, "Preview", "a.fit", false, none},
		{srcs, nil, emmc, "", "Preview", true, none},
		{srcs, nil, emmc, "Live", "x.fit", false, none},
		{srcs, nil, "Other Share", "Autorun", "x.fit", false, none},
	}
	for _, c := range from {
		got, ok := RenameFromRefused(c.sources, c.store, c.share, c.dir, c.name, c.isDir)
		if got != c.want || ok != (c.want != none) {
			t.Errorf("RenameFromRefused(%v, store %q, %q, %q, %q, dir=%v) = %+v, %v; want %+v",
				c.sources, c.store, c.share, c.dir, c.name, c.isDir, got, ok, c.want)
		}
	}

	to := []struct {
		sources             []model.Source
		store               []string
		share, dir, newName string
		want                RenameRefusal
	}{
		{srcs, nil, emmc, "", "Autorun", r2(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "", "AUTORUN", r2(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "", "ASIAIR", r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{srcs, nil, emmc, "", "Foo", r2(RenameCauseEnabled, "Foo/Bar")},
		{srcs, nil, emmc, "Foo", "Bar", r2(RenameCauseEnabled, "Foo/Bar")},
		{srcs, nil, emmc, "ASIAIR", "Autorun", r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{srcs, nil, emmc, "", "Plan", r2(RenameCauseDisabled, "Plan")},
		{srcs, nil, udisk, "", "Autorun", r2(RenameCauseEnabled, "Autorun")},
		{srcs, nil, udisk, "", "asiair", r2(RenameCauseEnabled, "ASIAIR/Autorun")},
		{nil, nil, udisk, "", "asiair", r2(RenameCauseDataFolder, "ASIAIR")},
		{nil, nil, emmc, "", "asiair", none},
		{srcs, []string{"Old"}, emmc, "", "old", r2(RenameCauseStore, "Old")},
		{nil, []string{"ASIAIR/Autorun"}, emmc, "ASIAIR", "Autorun", r2(RenameCauseStore, "ASIAIR/Autorun")},
		{nil, []string{"ASIAIR/Autorun"}, emmc, "", "ASIAIR", r2(RenameCauseStore, "ASIAIR/Autorun")},
		{srcs, nil, emmc, "Autorun", "Renamed", r1(RenameCauseEnabled, "Autorun")},
		{srcs, nil, emmc, "Preview", "b.fit", none},
		{srcs, nil, emmc, "", "Data", none},
		{srcs, nil, emmc, "Foo", "Baz", none},
		{srcs, nil, emmc, "", "Old", none},
	}
	for _, c := range to {
		got, ok := RenameToRefused(c.sources, c.store, c.share, c.dir, c.newName)
		if got != c.want || ok != (c.want != none) {
			t.Errorf("RenameToRefused(%v, store %q, %q, %q, %q) = %+v, %v; want %+v",
				c.sources, c.store, c.share, c.dir, c.newName, got, ok, c.want)
		}
	}
}

// TestHarvestViewAgreesWithWalk lists a device through the harvester's own walk
// and pairListing in each layout and checks the predicates name exactly the
// frames and previews it produced (in the fake's exact case; the table above
// covers mixed case).
func TestHarvestViewAgreesWithWalk(t *testing.T) {
	p := harvestViewProfile()
	mt := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
	dev := device.NewFake()
	all := []string{
		"Autorun/Light/M42/" + hvFrame,
		"Autorun/Light/M42/" + hvPreview,
		"Autorun/Light/M42/orphan_thn.jpg",
		"Autorun/Light/M42/._" + hvFrame,
		"Autorun/Light/batch_stack_tmp/stack.fit",
		"Autorun/@eaDir/x_thn.jpg",
		"Autorun/notes.txt",
		"Autorun/x.tmp",
		"ASIAIR/Autorun/Dark/d.fit",
		"ASIAIR/Autorun/Dark/d_thn.jpg",
		"ASIAIR/Autorun/.Trashes/gone.fit",
		"ASIAIR/Autorun/.DS_Store",
		"ASIAIR/Live/live.fit",
		"ASIAIR/log/a.log",
		"Live/l.fit",
		"top.fit",
	}
	for _, f := range all {
		dev.AddFile(udisk, f, []byte("data"), mt)
	}
	sess, err := dev.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	share, err := sess.Mount(udisk)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{}
	var wantFrames, wantPreviews []string
	for _, prefix := range []string{"", nestedDataPrefix} {
		entries, err := w.listShareRoots(share, prefix, []string{"Autorun"})
		if err != nil {
			t.Fatalf("listShareRoots(%q): %v", prefix, err)
		}
		frames, previews := pairListing(udisk, entries, harvestViewExclude)
		for _, f := range frames {
			wantFrames = append(wantFrames, f.path)
		}
		for _, pv := range previews {
			wantPreviews = append(wantPreviews, pv.path)
		}
	}
	var gotFrames, gotPreviews []string
	for _, f := range all {
		if HarvestCandidate(p, harvestViewExclude, udisk, f) {
			gotFrames = append(gotFrames, f)
		}
		if HarvestPreview(p, harvestViewExclude, udisk, f) {
			gotPreviews = append(gotPreviews, f)
		}
	}
	sort.Strings(wantFrames)
	sort.Strings(gotFrames)
	sort.Strings(wantPreviews)
	sort.Strings(gotPreviews)
	if len(wantFrames) != 3 || len(wantPreviews) != 3 {
		t.Fatalf("fixture drifted: walk listed frames %v previews %v", wantFrames, wantPreviews)
	}
	if !equalStrings(gotFrames, wantFrames) {
		t.Errorf("HarvestCandidate = %v, walk frames = %v", gotFrames, wantFrames)
	}
	if !equalStrings(gotPreviews, wantPreviews) {
		t.Errorf("HarvestPreview = %v, walk previews = %v", gotPreviews, wantPreviews)
	}
}

// TestRenameRuleKeepsRootResolution renames, on a fresh device each time, every
// folder the rename rule does not refuse to every candidate name it does not
// refuse, and checks the harvester's own root resolution and walk answer
// exactly as before: the prefix resolveDataPrefix picks for the share's roots
// together and for each alone, and each root's listing (or its failure). The
// roots are every configured one and the store-kept ones, since a disabled or
// removed root is walked again once (re-)enabled.
func TestRenameRuleKeepsRootResolution(t *testing.T) {
	mt := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
	layouts := []struct {
		name    string
		share   string
		sources []model.Source
		store   []string
		files   []string
	}{
		{
			name:  "flat",
			share: emmc,
			sources: []model.Source{
				{Share: emmc, Roots: []string{"Autorun", "Foo/Bar"}},
				{Share: emmc, Roots: []string{"Plan"}, Disabled: true},
			},
			store: []string{"Old"},
			files: []string{
				"Autorun/Light/M42/a.fit", "Autorun/batch_stack_tmp/sub/s.fit",
				"Foo/Bar/b.fit", "Foo/Other/o.txt",
				"Plan/p.fit", "Old/Light/o.fit",
				"Preview/x.fit", "Docs/sub/y.txt", "ASIAIR/log/l.log",
			},
		},
		{
			name:  "nested",
			share: udisk,
			sources: []model.Source{
				{Share: udisk, Roots: []string{"Autorun"}},
				{Share: udisk, Roots: []string{"Plan"}, Disabled: true},
			},
			store: []string{"ASIAIR/Old"},
			files: []string{
				"ASIAIR/Autorun/Light/a.fit", "ASIAIR/Autorun/@eaDir/sub/t.jpg",
				"ASIAIR/Plan/p.fit", "ASIAIR/Old/o.fit", "ASIAIR/log/l.log",
				"Preview/x.fit", "Docs/sub/y.txt",
			},
		},
	}
	newNames := []string{"Renamed", "Autorun", "AUTORUN", "ASIAIR", "Plan", "Old", "Foo", "Bar", "Light", "log", "sub"}

	for _, l := range layouts {
		var roots []string
		for _, src := range l.sources {
			roots = append(roots, src.Roots...)
		}
		roots = append(roots, l.store...)

		open := func() (*device.Fake, device.Share) {
			dev := device.NewFake()
			for _, f := range l.files {
				dev.AddFile(l.share, f, []byte("data"), mt)
			}
			sess, err := dev.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sh, err := sess.Mount(l.share)
			if err != nil {
				t.Fatal(err)
			}
			return dev, sh
		}
		resolution := func(sh device.Share) string {
			w := &Worker{}
			var out []string
			prefix, err := w.resolveDataPrefix(sh, roots)
			out = append(out, fmt.Sprintf("all: %q %v", prefix, err != nil))
			for _, root := range roots {
				prefix, err := w.resolveDataPrefix(sh, []string{root})
				entries, werr := w.listShareRoots(sh, prefix, []string{root})
				var paths []string
				for _, e := range entries {
					paths = append(paths, e.path)
				}
				sort.Strings(paths)
				out = append(out, fmt.Sprintf("%s: %q %v %v %q", root, prefix, err != nil, werr != nil, paths))
			}
			return strings.Join(out, "\n")
		}

		_, base := open()
		want := resolution(base)
		folders := map[string]bool{}
		for _, f := range l.files {
			for d := path.Dir(f); d != "."; d = path.Dir(d) {
				folders[d] = true
			}
		}
		renamed, refused := 0, 0
		for folder := range folders {
			dir, name := path.Dir(folder), path.Base(folder)
			if dir == "." {
				dir = ""
			}
			if _, ok := RenameFromRefused(l.sources, l.store, l.share, dir, name, true); ok {
				refused++
				continue
			}
			for _, nn := range newNames {
				if nn == name {
					continue
				}
				if _, ok := RenameToRefused(l.sources, l.store, l.share, dir, nn); ok {
					refused++
					continue
				}
				_, sh := open()
				if err := sh.Rename(folder, path.Join(dir, nn)); err != nil {
					continue // taken on the device: nothing moved
				}
				renamed++
				if got := resolution(sh); got != want {
					t.Errorf("%s: renaming %s to %s (allowed by the rule) changed root resolution:\n got %s\nwant %s",
						l.name, folder, nn, got, want)
				}
			}
		}
		if renamed < 10 || refused < 10 {
			t.Fatalf("%s: fixture drifted: %d renames tried, %d refused", l.name, renamed, refused)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHarvestStoreKey(t *testing.T) {
	p := harvestViewProfile()
	p.Sources = append(p.Sources, model.Source{Share: "TF Images", Roots: []string{"Plan"}})
	tests := []struct {
		share, in, want string
		ok              bool
	}{
		{emmc, "Autorun/Light/M42/" + hvFrame, "Autorun/Light/M42/" + hvFrame, true},
		{emmc, "AUTORUN/Light/M42/" + hvFrame, "Autorun/Light/M42/" + hvFrame, true},
		{emmc, "autorun/LIGHT/m42/X.FIT", "Autorun/LIGHT/m42/X.FIT", true}, // below the root, case kept
		{udisk, "asiair/AUTORUN/Light/x.fit", "ASIAIR/Autorun/Light/x.fit", true},
		{udisk, "Asiair/autorun", "ASIAIR/Autorun", true},
		{udisk, "/ASIAIR/Autorun/x.fit", "ASIAIR/Autorun/x.fit", true},
		{"TF Images", "PLAN/x.fit", "Plan/x.fit", true},
		{"TF Images", "Live/x.fit", "Live/x.fit", false}, // disabled source
		{emmc, "Live/x.fit", "Live/x.fit", false},
		{emmc, "asiair/x.fit", "asiair/x.fit", false},
	}
	for _, tc := range tests {
		got, ok := HarvestStoreKey(p, tc.share, tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("HarvestStoreKey(%q, %q) = %q, %v; want %q, %v", tc.share, tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestHarvestStoreKeyFindsHarvesterRow proves the rewritten key reaches the row
// the harvester filed: a frame pulled through a full cycle on a Udisk is found
// from a device path spelled in another case.
func TestHarvestStoreKeyFindsHarvesterRow(t *testing.T) {
	h := newHarness(t, udisk, header.New())
	data := []byte("SIMPLE  =                    T")
	h.dev.AddFile(udisk, "ASIAIR/Autorun/Light/M42/"+hvFrame, data, h.nowVal.Add(-time.Hour))
	h.cycle()
	key, ok := HarvestStoreKey(h.profile, udisk, "asiair/AUTORUN/Light/M42/"+hvFrame)
	if !ok {
		t.Fatal("device path not recognised as watched")
	}
	row, found, err := h.store.GetFrame(context.Background(), "rig-01", udisk, key)
	if err != nil || !found {
		t.Fatalf("GetFrame(%q) found=%v err=%v; the store key form must match the harvester's", key, found, err)
	}
	if row.SourcePath != "ASIAIR/Autorun/Light/M42/"+hvFrame {
		t.Fatalf("row source path %q", row.SourcePath)
	}
}

// archivedCopyCase is one row-vs-listing situation, shared by the ArchivedCopy
// table and the decide cross-check.
type archivedCopyCase struct {
	name    string
	row     *model.Frame // nil: no row
	missing bool         // culled on the NAS (missing_seen_at set)
	size    int64
	mtime   time.Duration // listed mtime relative to the row's
	want    bool
}

func archivedCopyCases() []archivedCopyCase {
	row := func(st model.FrameState) *model.Frame {
		return &model.Frame{SHA256: "abc", Size: 10, State: st}
	}
	return []archivedCopyCase{
		{name: "no row", size: 10, want: false},
		{name: "verified", row: row(model.StateVerified), size: 10, want: true},
		{name: "unclassified", row: row(model.StateUnclassified), size: 10, want: true},
		{name: "verified mtime within tolerance", row: row(model.StateVerified), size: 10, mtime: mtimeTol, want: true},
		{name: "verified mtime within tolerance before", row: row(model.StateVerified), size: 10, mtime: -mtimeTol, want: true},
		{name: "failed", row: row(model.StateFailed), size: 10, want: false},
		{name: "held", row: row(model.StateHeld), size: 10, want: false},
		{name: "dismissed", row: row(model.StateDismissed), size: 10, want: false},
		{name: "deleted", row: row(model.StateDeleted), size: 10, want: false},
		{name: "verified culled", row: row(model.StateVerified), missing: true, size: 10, want: false},
		{name: "unclassified culled", row: row(model.StateUnclassified), missing: true, size: 10, want: false},
		{name: "verified size mismatch", row: row(model.StateVerified), size: 11, want: false},
		{name: "verified mtime mismatch", row: row(model.StateVerified), size: 10, mtime: mtimeTol + time.Second, want: false},
		{name: "unclassified mtime mismatch", row: row(model.StateUnclassified), size: 10, mtime: -time.Hour, want: false},
	}
}

func TestArchivedCopyTable(t *testing.T) {
	base := time.Date(2026, 10, 7, 23, 15, 2, 0, time.UTC)
	for _, tc := range archivedCopyCases() {
		t.Run(tc.name, func(t *testing.T) {
			var row *model.Frame
			if tc.row != nil {
				r := *tc.row
				r.Mtime = base.Unix()
				if tc.missing {
					r.MissingSeenAt = base.Unix()
				}
				row = &r
			}
			if got := ArchivedCopy(row, tc.size, base.Add(tc.mtime)); got != tc.want {
				t.Fatalf("ArchivedCopy = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArchivedCopyImpliesNoPull: on the same fixture, every listing ArchivedCopy
// accepts is one decide would not pull, with rows written through UpsertFrame and
// the cull mark through SetFrameMissing as production writes them.
func TestArchivedCopyImpliesNoPull(t *testing.T) {
	h := newHarness(t, emmc, header.New())
	ctx := context.Background()
	accepted := 0
	for i, tc := range archivedCopyCases() {
		srcPath := "Autorun/f" + string(rune('a'+i)) + ".fit"
		lf := listedFrame{share: emmc, path: srcPath, name: filepath.Base(srcPath), size: tc.size, mtime: h.nowVal.Add(tc.mtime)}
		var stored *model.Frame
		if tc.row != nil {
			f := *tc.row
			f.RigID, f.SourceShare, f.SourcePath = "rig-01", emmc, srcPath
			f.Mtime = h.nowVal.Unix()
			f.PulledAt = h.nowVal.Unix()
			f.DestPath = filepath.Join(h.cfg.Roots.Light, filepath.Base(srcPath))
			if err := h.store.UpsertFrame(ctx, &f); err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				if err := h.store.SetFrameMissing(ctx, f.ID, h.nowVal.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			row, ok, err := h.store.GetFrame(ctx, "rig-01", emmc, srcPath)
			if err != nil || !ok {
				t.Fatalf("%s: GetFrame ok=%v err=%v", tc.name, ok, err)
			}
			stored = row
		}
		if !ArchivedCopy(stored, lf.size, lf.mtime) {
			continue
		}
		accepted++
		if pull, _ := h.w.decide(ctx, h.cfg, h.profile, lf); pull {
			t.Errorf("%s: ArchivedCopy is true but decide would pull", tc.name)
		}
	}
	if accepted == 0 {
		t.Fatal("no case reached the cross-check")
	}
}

// TestReservedPathTable: the reserved-path rule copy and upload apply to a
// file they would write is R2 over the configured sources, enabled or
// disabled, with no store-kept roots; a path inside a root is not reserved.
func TestReservedPathTable(t *testing.T) {
	srcs := renameRuleSources()
	for _, c := range []struct {
		sources []model.Source
		share   string
		p       string
		root    string // "" for not reserved
	}{
		{srcs, emmc, "Autorun", "Autorun"},
		{srcs, emmc, "AUTORUN", "Autorun"},
		{srcs, emmc, "ASIAIR", "ASIAIR/Autorun"},
		{srcs, emmc, "asiair/autorun", "ASIAIR/Autorun"},
		{srcs, emmc, "Foo", "Foo/Bar"},
		{srcs, emmc, "Foo/Bar", "Foo/Bar"},
		{srcs, emmc, "plan", "Plan"}, // a disabled source counts
		{srcs, udisk, "ASIAIR", "ASIAIR/Autorun"},
		{nil, udisk, "asiair", "ASIAIR"},
		{nil, emmc, "ASIAIR", ""},
		{srcs, emmc, "Autorun/a.fit", ""},
		{srcs, emmc, "Foo/Baz", ""},
		{srcs, emmc, "Preview/Autorun", ""},
		{srcs, "Other", "Autorun", ""},
	} {
		root, ok := ReservedPath(c.sources, c.share, c.p)
		if ok != (c.root != "") || root != c.root {
			t.Errorf("ReservedPath(%v, %q, %q) = %q, %v; want %q", c.sources, c.share, c.p, root, ok, c.root)
		}
	}
}
