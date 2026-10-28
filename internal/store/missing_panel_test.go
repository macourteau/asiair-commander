package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/macourteau/asiair-commander/internal/model"
)

// mkMissing upserts a frame in the given state, marks it missing at seen, and
// returns its id, through the real production path (UpsertFrame + SetFrameMissing).
func mkMissing(t *testing.T, s Store, rig, path, dest string, state model.FrameState, seen int64) int64 {
	t.Helper()
	ctx := context.Background()
	f := &model.Frame{RigID: rig, SourceShare: "Udisk", SourcePath: path,
		Size: 1, Mtime: 1, State: state, DestPath: dest, SHA256: "h"}
	if err := s.UpsertFrame(ctx, f); err != nil {
		t.Fatalf("upsert %s: %v", path, err)
	}
	if err := s.SetFrameMissing(ctx, f.ID, seen); err != nil {
		t.Fatalf("set missing %s: %v", path, err)
	}
	return f.ID
}

func frameIDs(frames []model.Frame) []int64 {
	out := make([]int64, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]int64(nil), a...)
	b = append([]int64(nil), b...)
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// listAll pages through one filtered section and returns every row, checking that
// each page's total agrees.
func listAll(t *testing.T, s Store, rig string, f MissingFilter) ([]model.Frame, int) {
	t.Helper()
	var all []model.Frame
	total := -1
	for page := 0; ; page++ {
		rows, n, err := s.ListMissingFrames(context.Background(), rig, f, page, 2)
		if err != nil {
			t.Fatalf("ListMissingFrames(%+v): %v", f, err)
		}
		if total >= 0 && n != total {
			t.Fatalf("total changed across pages: %d then %d", total, n)
		}
		total = n
		if len(rows) == 0 {
			break
		}
		all = append(all, rows...)
	}
	return all, total
}

// AC-T1 (design library-reconciliation §13.5): the two sections partition the
// marked set, totals and folder counts match the paged rows, folder counts ignore
// dir and q, dir is byte-exact, and q is case-insensitive (non-ASCII included) and
// literal about % and _.
func TestMissingSectionsPartitionAndFilter(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	ver := mkMissing(t, s, "rig", "v.fit", "/lib/M31/v.fit", model.StateVerified, 100)
	unc := mkMissing(t, s, "rig", "u.fit", "/lib/M31/u.fit", model.StateUnclassified, 100)
	held := mkMissing(t, s, "rig", "h.fit", "/lib/M31/h.fit", model.StateHeld, 100)
	caseVariant := mkMissing(t, s, "rig", "c.fit", "/lib/m31/c.fit", model.StateVerified, 100)
	nonASCII := mkMissing(t, s, "rig", "e.fit", "/lib/Éta Car/e.fit", model.StateVerified, 100)
	pct := mkMissing(t, s, "rig", "p.fit", "/lib/50%_done/p.fit", model.StateVerified, 100)
	del1 := mkMissing(t, s, "rig", "d1.fit", "/lib/M31/d1.fit", model.StateDeleted, 100)
	del2 := mkMissing(t, s, "rig", "d2.fit", "/lib/NGC7000/d2.fit", model.StateDeleted, 100)
	// Noise the panel must never show: another rig's row and an unmarked row.
	mkMissing(t, s, "other", "x.fit", "/lib/M31/x.fit", model.StateDeleted, 100)
	unmarked := &model.Frame{RigID: "rig", SourceShare: "Udisk", SourcePath: "n.fit",
		Size: 1, Mtime: 1, State: model.StateVerified, DestPath: "/lib/M31/n.fit", SHA256: "h"}
	if err := s.UpsertFrame(ctx, unmarked); err != nil {
		t.Fatal(err)
	}

	actionable := []int64{ver, unc, held, caseVariant, nonASCII, pct}
	leftover := []int64{del1, del2}

	act, actTotal := listAll(t, s, "rig", MissingFilter{Section: SectionActionable})
	left, leftTotal := listAll(t, s, "rig", MissingFilter{Section: SectionLeftover})
	if !sameIDs(frameIDs(act), actionable) || actTotal != len(actionable) {
		t.Fatalf("actionable = %v (total %d), want %v", frameIDs(act), actTotal, actionable)
	}
	if !sameIDs(frameIDs(left), leftover) || leftTotal != len(leftover) {
		t.Fatalf("leftover = %v (total %d), want %v", frameIDs(left), leftTotal, leftover)
	}
	// The default (zero) section is the actionable one.
	if def, _ := listAll(t, s, "rig", MissingFilter{}); !sameIDs(frameIDs(def), actionable) {
		t.Fatalf("zero-value section = %v, want the actionable set", frameIDs(def))
	}

	for _, sec := range []MissingSection{SectionActionable, SectionLeftover} {
		groups, totals, err := s.MissingGroupCounts(ctx, "rig", sec)
		if err != nil {
			t.Fatalf("group counts %s: %v", sec, err)
		}
		if totals.Actionable != len(actionable) || totals.Leftover != len(leftover) {
			t.Fatalf("totals = %+v, want actionable %d leftover %d", totals, len(actionable), len(leftover))
		}
		rows, _ := listAll(t, s, "rig", MissingFilter{Section: sec})
		perDir := map[string]int{}
		for _, f := range rows {
			perDir[filepath.Dir(f.DestPath)]++
		}
		sum := 0
		for _, g := range groups {
			if perDir[g.Dir] != g.Count {
				t.Errorf("%s: group %q count %d, paged rows say %d", sec, g.Dir, g.Count, perDir[g.Dir])
			}
			sum += g.Count
		}
		if sum != len(rows) || len(groups) != len(perDir) {
			t.Errorf("%s: groups %+v do not cover the paged rows %v", sec, groups, perDir)
		}
	}

	// dir is byte-exact: /lib/M31 does not pull in the case-variant /lib/m31.
	got, n := listAll(t, s, "rig", MissingFilter{Section: SectionActionable, Dir: "/lib/M31"})
	if !sameIDs(frameIDs(got), []int64{ver, unc, held}) || n != 3 {
		t.Fatalf("dir /lib/M31 = %v (total %d), want [ver unc held]", frameIDs(got), n)
	}
	got, _ = listAll(t, s, "rig", MissingFilter{Section: SectionActionable, Dir: "/lib/m31"})
	if !sameIDs(frameIDs(got), []int64{caseVariant}) {
		t.Fatalf("dir /lib/m31 = %v, want only the case-variant row", frameIDs(got))
	}

	// q is case-insensitive, both ways round, and folds non-ASCII letters.
	got, _ = listAll(t, s, "rig", MissingFilter{Section: SectionActionable, Q: "m31"})
	if !sameIDs(frameIDs(got), []int64{ver, unc, held, caseVariant}) {
		t.Fatalf("q m31 = %v, want both case variants", frameIDs(got))
	}
	got, _ = listAll(t, s, "rig", MissingFilter{Section: SectionActionable, Q: "éTA"})
	if !sameIDs(frameIDs(got), []int64{nonASCII}) {
		t.Fatalf("q éTA = %v, want the non-ASCII row", frameIDs(got))
	}
	// % and _ are literal, not LIKE wildcards.
	got, _ = listAll(t, s, "rig", MissingFilter{Section: SectionActionable, Q: "%_"})
	if !sameIDs(frameIDs(got), []int64{pct}) {
		t.Fatalf("q %%_ = %v, want only the row containing those characters", frameIDs(got))
	}
	got, n = listAll(t, s, "rig", MissingFilter{Section: SectionActionable, Q: "_"})
	if !sameIDs(frameIDs(got), []int64{pct}) || n != 1 {
		t.Fatalf("q _ = %v (total %d), want one row", frameIDs(got), n)
	}
	// dir and q combine.
	got, _ = listAll(t, s, "rig", MissingFilter{Section: SectionLeftover, Dir: "/lib/M31", Q: "d1"})
	if !sameIDs(frameIDs(got), []int64{del1}) {
		t.Fatalf("leftover dir+q = %v, want [d1]", frameIDs(got))
	}
}

// AC-T1/AC-T2 (store half): each folder's ackable is the group-acknowledge
// predicate — verified, no restore pending, not already acknowledged.
func TestMissingGroupCountsAckable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	mkMissing(t, s, "rig", "v1.fit", "/lib/A/v1.fit", model.StateVerified, 100)
	mkMissing(t, s, "rig", "v2.fit", "/lib/A/v2.fit", model.StateVerified, 100)
	mkMissing(t, s, "rig", "u.fit", "/lib/A/u.fit", model.StateUnclassified, 100)
	mkMissing(t, s, "rig", "h.fit", "/lib/A/h.fit", model.StateHeld, 100)
	rp := mkMissing(t, s, "rig", "r.fit", "/lib/A/r.fit", model.StateVerified, 100)
	if ok, err := s.RequestRestore(ctx, "rig", rp, 200); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	ack := mkMissing(t, s, "rig", "k.fit", "/lib/A/k.fit", model.StateVerified, 100)
	if ok, err := s.AcknowledgeMissing(ctx, "rig", ack, 200); err != nil || !ok {
		t.Fatalf("ack: ok=%v err=%v", ok, err)
	}
	mkMissing(t, s, "rig", "o.fit", "/lib/B/o.fit", model.StateHeld, 100)
	mkMissing(t, s, "rig", "d.fit", "/lib/A/d.fit", model.StateDeleted, 100)

	groups, _, err := s.MissingGroupCounts(ctx, "rig", SectionActionable)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]model.MissingGroup{
		"/lib/A": {Dir: "/lib/A", Count: 6, Ackable: 2},
		"/lib/B": {Dir: "/lib/B", Count: 1, Ackable: 0},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v, want %+v", groups, want)
	}
	for _, g := range groups {
		if want[g.Dir] != g {
			t.Errorf("group %q = %+v, want %+v", g.Dir, g, want[g.Dir])
		}
	}

	left, _, err := s.MissingGroupCounts(ctx, "rig", SectionLeftover)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != (model.MissingGroup{Dir: "/lib/A", Count: 1, Ackable: 0}) {
		t.Fatalf("leftover groups = %+v, want one /lib/A group, nothing ackable", left)
	}
}
