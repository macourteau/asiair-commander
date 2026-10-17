package devfiles

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
)

func TestStoreKeptRoots(t *testing.T) {
	const emmc = "EMMC Images"
	configured := []model.Source{
		{Share: emmc, Roots: []string{"Plan", "Foo/Bar"}},
		{Share: emmc, Roots: []string{"Live"}, Disabled: true},
		{Share: "Udisk Images", Roots: []string{"Autorun"}},
	}
	tests := []struct {
		name    string
		sources []model.Source
		leads   []string
		want    []string
	}{
		{"two-segment lead keeps its first segment", nil, []string{"Autorun/Light"}, []string{"Autorun"}},
		{"nested lead is a root as it stands", nil, []string{"ASIAIR/Autorun"}, []string{"ASIAIR/Autorun"}},
		{"one-segment lead", nil, []string{"Autorun"}, []string{"Autorun"}},
		{"multi-segment removed root kept as its first segment", nil, []string{"Foo/Bar"}, []string{"Foo"}},
		{"only the literal ASIAIR prefix nests", nil, []string{"asiair/Autorun"}, []string{"asiair"}},
		{"a lone ASIAIR lead keeps the folder", nil, []string{"ASIAIR"}, []string{"ASIAIR"}},
		{"each root once", nil, []string{"Autorun/Light", "Autorun/Dark", "Autorun"}, []string{"Autorun"}},
		{"no leads, no roots", configured, nil, nil},
		// Leads at or under a configured root are dropped before any widening.
		{"configured multi-segment root yields no Foo", configured, []string{"Foo/Bar"}, nil},
		{"configured root in another case", configured, []string{"PLAN/Light", "plan"}, nil},
		{"configured root's nested layout", configured, []string{"ASIAIR/Plan"}, nil},
		{"disabled source still configured", configured, []string{"Live/Light"}, nil},
		{"another share's source does not drop", configured, []string{"Autorun/Light"}, []string{"Autorun"}},
		{"sibling of a configured multi-segment root", configured, []string{"Foo/Baz"}, []string{"Foo"}},
		{"mixed", configured, []string{"ASIAIR/Autorun", "Foo/Bar", "Old/Light", "Plan/x"}, []string{"ASIAIR/Autorun", "Old"}},
	}
	for _, tc := range tests {
		got := storeKeptRoots(tc.sources, emmc, tc.leads)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: storeKeptRoots(%q) = %q, want %q", tc.name, tc.leads, got, tc.want)
		}
	}

	// A configured root deeper than the cut lead: the lead holds the root, so
	// it is configured, not store-kept.
	deep := []struct {
		name, share string
		sources     []model.Source
		lead        string
	}{
		{"Udisk nested lead above a two-segment root", "Udisk Images",
			[]model.Source{{Share: "Udisk Images", Roots: []string{"Autorun/Light"}}}, "ASIAIR/Autorun"},
		{"eMMC lead above a three-segment root", emmc,
			[]model.Source{{Share: emmc, Roots: []string{"A/B/C"}}}, "a/b"},
	}
	for _, tc := range deep {
		if got := storeKeptRoots(tc.sources, tc.share, []string{tc.lead}); len(got) != 0 {
			t.Errorf("%s: storeKeptRoots(%q) = %q, want none", tc.name, tc.lead, got)
		}
	}
}

// Expected refusal sentences, as the design words them.
const r1Text = "Commander archives this folder by name; renaming here would make it archive these frames again."

func r1DisabledText(root string) string {
	return r1Text + " (" + root + " belongs to a capture source that is turned off; it still counts while it is configured.)"
}

func storeText(root string) string {
	return root + " still has frames Commander is archiving or tracking; renaming here would create false records. " +
		"It is refused until they have drained."
}

const dependsText = "a folder Commander depends on (a configured capture source, even one turned off, or the ASIAIR's own data folder)"

func r2OldText(name, share, root string) string {
	return name + " is, or holds, " + share + "/" + root + ", " + dependsText +
		". Renaming it would disrupt archiving on " + share + ", so it is refused."
}

func r2NewText(name, share, root string) string {
	return name + " would be, or would hold, " + share + "/" + root + ", " + dependsText +
		". Renaming to it would disrupt archiving on " + share + ", so it is refused."
}

// renameCase is one row of the rename refusal table, shared by the route's
// test and the listing's agreement test.
type renameCase struct {
	name                   string
	setup                  func(fx *fixture) // sources and store leads; the fixture watches Autorun on both shares
	share, dir, item, to   string
	itemDir                bool   // the item is a folder (else a file)
	kind                   error  // nil: allowed
	msg                    string // the refusal's exact message
	offline                bool   // decided from the configuration and the store: no device call
	byNewName, notListable bool   // the refusal depends on the new name; the item is never listed
}

func sources(ss ...model.Source) func(fx *fixture) {
	return func(fx *fixture) {
		fx.editRig("r1", func(p *model.RigProfile) { p.Sources = ss })
	}
}

// removedAutorun drops eMMC's source, leaving rows the store keeps in play
// under Autorun there, in both layouts.
func removedAutorun(fx *fixture) {
	sources(model.Source{Share: udisk, Roots: []string{"Autorun"}})(fx)
	fx.store.setLeads(emmc, "ASIAIR/Autorun", "Autorun/Light")
}

func renameCases() []renameCase {
	noSources := sources()
	fooBar := sources(model.Source{Share: emmc, Roots: []string{"Foo/Bar"}})
	disabled := sources(model.Source{Share: emmc, Roots: []string{"Autorun"}},
		model.Source{Share: emmc, Roots: []string{"Live"}, Disabled: true})
	return []renameCase{
		{name: "frame in Autorun", share: emmc, dir: "Autorun/Light", item: "a.fit", to: "b.fit",
			kind: ErrWatched, msg: r1Text, offline: true},
		{name: "frame in nested autorun, mixed case", share: udisk, dir: "asiair/AUTORUN/Light", item: "a.fit", to: "b.fit",
			kind: ErrWatched, msg: r1Text, offline: true},
		{name: "junk folder's content allowed", share: emmc, dir: "Autorun/#recycle", item: "a.fit", to: "b.fit"},
		{name: "junk folder itself", share: emmc, dir: "Autorun", item: "#recycle", to: "x", itemDir: true,
			kind: ErrWatched, msg: r1Text, offline: true},
		{name: "Autorun itself, mixed case", share: emmc, item: "autorun", to: "Old", itemDir: true,
			kind: ErrWatched, msg: r2OldText("autorun", emmc, "Autorun")},
		{name: "Udisk ASIAIR with sources", share: udisk, item: "ASIAIR", to: "Data", itemDir: true,
			kind: ErrWatched, msg: r2OldText("ASIAIR", udisk, "ASIAIR/Autorun")},
		{name: "Udisk ASIAIR without sources", setup: noSources, share: udisk, item: "ASIAIR", to: "Data", itemDir: true,
			kind: ErrWatched, msg: r2OldText("ASIAIR", udisk, "ASIAIR")},
		{name: "Foo holding the root Foo/Bar", setup: fooBar, share: emmc, item: "Foo", to: "Baz", itemDir: true,
			kind: ErrWatched, msg: r2OldText("Foo", emmc, "Foo/Bar")},
		{name: "file named Autorun at an eMMC root", share: emmc, item: "Autorun", to: "Autorun.txt"},
		{name: "file named ASIAIR at a Udisk root", share: udisk, item: "ASIAIR", to: "ASIAIR.txt"},
		{name: "Old to Autorun", share: emmc, item: "Old", to: "Autorun", itemDir: true,
			kind: ErrWatched, msg: r2NewText("Autorun", emmc, "Autorun"), offline: true, byNewName: true},
		{name: "X to ASIAIR on eMMC", share: emmc, item: "X", to: "ASIAIR", itemDir: true,
			kind: ErrWatched, msg: r2NewText("ASIAIR", emmc, "ASIAIR/Autorun"), offline: true, byNewName: true},
		{name: "X to Foo for the root Foo/Bar", setup: fooBar, share: emmc, item: "X", to: "Foo",
			kind: ErrWatched, msg: r2NewText("Foo", emmc, "Foo/Bar"), offline: true, byNewName: true},
		{name: "Data to asiair on a Udisk with sources", share: udisk, item: "Data", to: "asiair", itemDir: true,
			kind: ErrWatched, msg: r2NewText("asiair", udisk, "ASIAIR/Autorun"), offline: true, byNewName: true},
		{name: "Data to asiair on a Udisk without sources", setup: noSources, share: udisk, item: "Data", to: "asiair",
			itemDir: true, kind: ErrWatched, msg: r2NewText("asiair", udisk, "ASIAIR"), offline: true, byNewName: true},
		{name: "a root's name at a Udisk root while ASIAIR/Autorun exists", share: udisk, item: "Data", to: "autorun",
			itemDir: true, kind: ErrWatched, msg: r2NewText("autorun", udisk, "Autorun"), offline: true, byNewName: true},
		{name: "marker as the item", share: udisk, dir: "ASIAIR", item: model.MediaMarkerName, to: "x",
			kind: ErrBadPath, msg: `".asiair-commander-media-id" is Commander's device-identity file; the tool never touches it`,
			offline: true, notListable: true},
		{name: "marker as the new name", share: emmc, dir: "Preview", item: "a.fit", to: strings.ToUpper(model.IdentityMarkerName),
			kind: ErrBadPath, msg: `Invalid name "` + strings.ToUpper(model.IdentityMarkerName) + `": it is Commander's device-identity file name`,
			offline: true, byNewName: true},
		{name: "a disabled source's contents", setup: disabled, share: emmc, dir: "Live/Light", item: "a.fit", to: "b.fit",
			kind: ErrWatched, msg: r1DisabledText("Live"), offline: true},
		{name: "a disabled source's root", setup: disabled, share: emmc, item: "Live", to: "Old", itemDir: true,
			kind: ErrWatched, msg: r2OldText("Live", emmc, "Live")},
		{name: "removed source: inside its root", setup: removedAutorun, share: emmc, dir: "Autorun/Light", item: "a.fit",
			to: "b.fit", kind: ErrWatched, msg: storeText("Autorun"), offline: true},
		{name: "removed source: inside its nested root", setup: removedAutorun, share: emmc, dir: "ASIAIR/Autorun",
			item: "a.fit", to: "b.fit", kind: ErrWatched, msg: storeText("ASIAIR/Autorun"), offline: true},
		{name: "removed source: on its root", setup: removedAutorun, share: emmc, item: "Autorun", to: "Old", itemDir: true,
			kind: ErrWatched, msg: storeText("Autorun")},
		{name: "removed source: around its nested root", setup: removedAutorun, share: emmc, item: "ASIAIR", to: "Old",
			itemDir: true, kind: ErrWatched, msg: storeText("ASIAIR/Autorun")},
		{name: "removed source: to its root", setup: removedAutorun, share: emmc, item: "X", to: "AUTORUN", itemDir: true,
			kind: ErrWatched, msg: storeText("Autorun"), offline: true, byNewName: true},
		{name: "removed source: to hold its nested root", setup: removedAutorun, share: emmc, item: "X", to: "ASIAIR",
			itemDir: true, kind: ErrWatched, msg: storeText("ASIAIR/Autorun"), offline: true, byNewName: true},
		{name: "removed source drained", setup: sources(model.Source{Share: udisk, Roots: []string{"Autorun"}}),
			share: emmc, dir: "Autorun/Light", item: "a.fit", to: "b.fit"},
		{name: "a Preview file", share: emmc, dir: "Preview", item: "a.fit", to: "b.fit"},
	}
}

// stage puts the row's item on r1's device, with Udisk's ASIAIR/Autorun
// nested root present.
func (tc renameCase) stage(fx *fixture) {
	f := fx.dev(ip1)
	f.AddDir(udisk, "ASIAIR/Autorun", t0)
	p := joinRel(tc.dir, tc.item)
	switch {
	case tc.notListable:
		f.SetMarker(tc.share, p, []byte("id"))
	case tc.itemDir:
		f.AddDir(tc.share, p, t0)
	default:
		f.AddFile(tc.share, p, []byte("data"), t0)
	}
}

func TestRenameRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range renameCases() {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			if tc.setup != nil {
				tc.setup(fx)
			}
			tc.stage(fx)
			f := fx.dev(ip1)
			before := f.Mutations()
			err := fx.svc.Rename(ctx, "r1", tc.share, tc.dir, tc.item, tc.to)
			if tc.kind == nil {
				if err != nil {
					t.Fatalf("Rename = %v, want allowed", err)
				}
				sh := mountFake(t, f, tc.share)
				if _, err := sh.Stat(joinRel(tc.dir, tc.to)); err != nil {
					t.Errorf("renamed entry: %v", err)
				}
				return
			}
			if e := wantKind(t, err, tc.kind); e.Msg != tc.msg {
				t.Errorf("message = %q\nwant      %q", e.Msg, tc.msg)
			}
			if got := f.Mutations(); got != before {
				t.Errorf("mutations = %d, want %d", got, before)
			}
			if tc.offline && fx.opens.Load() != 0 {
				t.Errorf("opens = %d, want 0: decided from the configuration and the store", fx.opens.Load())
			}
		})
	}
}

func TestRenameStoreErrorRefuses(t *testing.T) {
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", []byte("data"), t0)
	storeErr := errors.New("database is locked")
	fx.store.leadsErr = storeErr
	err := fx.svc.Rename(context.Background(), "r1", emmc, "Preview", "a.fit", "b.fit")
	if !errors.Is(err, storeErr) {
		t.Fatalf("Rename = %v, want the store error", err)
	}
	var e *Error
	if errors.As(err, &e) {
		t.Errorf("store error classified as %v, want it unclassified", e.Kind)
	}
	if fx.opens.Load() != 0 || f.Mutations() != 0 {
		t.Errorf("opens = %d, mutations = %d, want none", fx.opens.Load(), f.Mutations())
	}
}

// mountFake mounts share on f directly, bypassing the service.
func mountFake(t *testing.T, f *device.Fake, share string) device.Share {
	t.Helper()
	sess, err := f.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sess.Mount(share)
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestRenameFile(t *testing.T) {
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/a.fit", []byte("frame"), t0)
	if err := fx.svc.Rename(context.Background(), "r1", emmc, "Preview", "a.fit", "b.fit"); err != nil {
		t.Fatal(err)
	}
	sh := mountFake(t, f, emmc)
	if _, err := sh.Stat("Preview/a.fit"); !device.IsNotFound(err) {
		t.Errorf("old name: %v, want not found", err)
	}
	if fi, err := sh.Stat("Preview/b.fit"); err != nil || fi.Size != 5 {
		t.Errorf("new name = %+v, %v", fi, err)
	}
	if got := f.Mutations(); got != 1 {
		t.Errorf("mutations = %d, want 1 (the rename alone)", got)
	}
}

func TestRenameFolder(t *testing.T) {
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/Set/a.fit", []byte("a"), t0)
	f.AddFile(emmc, "Preview/Set/Sub/b.fit", []byte("b"), t0)
	f.SetMarker(emmc, "Preview/Set/"+model.MediaMarkerName, []byte("id"))
	if err := fx.svc.Rename(context.Background(), "r1", emmc, "Preview", "Set", "Set 2"); err != nil {
		t.Fatal(err)
	}
	sh := mountFake(t, f, emmc)
	for _, p := range []string{"Preview/Set 2/a.fit", "Preview/Set 2/Sub/b.fit"} {
		if _, err := sh.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := sh.Stat("Preview/Set"); !device.IsNotFound(err) {
		t.Errorf("old folder: %v, want not found", err)
	}
	// A marker moves with its folder, which is why R2 reserves Udisk ASIAIR.
	if _, err := sh.ReadMarker("Preview/Set 2/" + model.MediaMarkerName); err != nil {
		t.Errorf("marker did not move with its folder: %v", err)
	}
}

// collisionErr is the device's answer to a rename onto a taken name, taken
// from the fake's own Rename.
func collisionErr(t *testing.T) error {
	t.Helper()
	f := device.NewFake()
	f.AddFile(emmc, "a", nil, t0)
	f.AddFile(emmc, "b", nil, t0)
	err := mountFake(t, f, emmc).Rename("a", "b")
	if !device.IsExist(err) {
		t.Fatalf("fake rename onto a taken name = %v, want a collision", err)
	}
	return err
}

func TestRenameExists(t *testing.T) {
	ctx := context.Background()
	want := "Rig One already has an item named b.fit here (names that differ only in letter case count as the same). " +
		"Nothing was replaced."

	t.Run("taken name", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("aaa"), t0)
		f.AddFile(emmc, "Preview/b.fit", []byte("bbbbb"), t0)
		err := fx.svc.Rename(ctx, "r1", emmc, "Preview", "a.fit", "b.fit")
		if e := wantKind(t, err, ErrExists); e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
		sh := mountFake(t, f, emmc)
		for p, size := range map[string]int64{"Preview/a.fit": 3, "Preview/b.fit": 5} {
			if fi, err := sh.Stat(p); err != nil || fi.Size != size {
				t.Errorf("%s = %+v, %v, want unchanged at %d bytes", p, fi, err, size)
			}
		}
	})

	t.Run("case variant", func(t *testing.T) {
		// The fake compares names case-sensitively; the device answers a
		// collision for B.FIT beside b.fit.
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("aaa"), t0)
		f.FailNext(device.FakeOpRename, 1, collisionErr(t))
		err := fx.svc.Rename(ctx, "r1", emmc, "Preview", "a.fit", "b.fit")
		if e := wantKind(t, err, ErrExists); e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
		if fx.opens.Load() != 1 {
			t.Errorf("opens = %d, want 1 (exists is not retried)", fx.opens.Load())
		}
	})
}

func TestRenameNotListedExactly(t *testing.T) {
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Preview/Ref.fit", []byte("x"), t0)
	err := fx.svc.Rename(context.Background(), "r1", emmc, "Preview", "ref.fit", "b.fit")
	want := "ref.fit isn't in this folder any more. If the connection dropped, the rename may already have gone " +
		"through; the list now shows the folder as it is."
	if e := wantKind(t, err, ErrNotFound); e.Msg != want {
		t.Errorf("message = %q, want %q", e.Msg, want)
	}
	if f.Mutations() != 0 {
		t.Errorf("mutations = %d, want 0", f.Mutations())
	}
}

func TestRenameDeviceAnswers(t *testing.T) {
	ctx := context.Background()

	t.Run("in use is reported, not retried", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
		f.FailNext(device.FakeOpRename, 1, device.SharingViolationLinkError("rename", "Preview/a.fit", "Preview/b.fit"))
		err := fx.svc.Rename(ctx, "r1", emmc, "Preview", "a.fit", "b.fit")
		want := "a.fit is in use on Rig One (the ASIAIR may be writing it). Try again in a moment."
		if e := wantKind(t, err, ErrInUse); e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
		if f.Mutations() != 1 || fx.opens.Load() != 1 {
			t.Errorf("mutations = %d, opens = %d; want one attempt", f.Mutations(), fx.opens.Load())
		}
	})

	t.Run("a network failure is retried", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
		f.FailNext(device.FakeOpRename, 1, errNetworkForTest())
		if err := fx.svc.Rename(ctx, "r1", emmc, "Preview", "a.fit", "b.fit"); err != nil {
			t.Fatal(err)
		}
		if _, err := mountFake(t, f, emmc).Stat("Preview/b.fit"); err != nil {
			t.Errorf("renamed file: %v", err)
		}
	})

	t.Run("a host-bound rig is refused", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
		fx.editRig("r1", func(p *model.RigProfile) { p.GUID = "" })
		wantKind(t, fx.svc.Rename(ctx, "r1", emmc, "Preview", "a.fit", "b.fit"), ErrHostBound)
		if f.Mutations() != 0 {
			t.Errorf("mutations = %d, want 0", f.Mutations())
		}
	})

	t.Run("busy while a copy runs", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
		unlock, err := fx.svc.lockOp(ctx, "Copy", "r1", "r2")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		wantKind(t, fx.svc.Rename(ctx, "r1", emmc, "Preview", "a.fit", "b.fit"), ErrBusy)
		if f.Mutations() != 0 || fx.opens.Load() != 0 {
			t.Errorf("mutations = %d, opens = %d, want none", f.Mutations(), fx.opens.Load())
		}
	})
}

func TestRenameRefusesBadRequests(t *testing.T) {
	fx := newFixture(t)
	fx.dev(ip1).AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
	for _, c := range []struct{ dir, name, to, msg string }{
		{"Preview", "a.fit", "a.fit", "That is already its name."},
		{"Autorun.", "a.fit", "b.fit", `invalid path "Autorun."`},
		{"Preview ", "a.fit", "b.fit", `invalid path "Preview "`},
		{"Pre*view", "a.fit", "b.fit", `invalid path "Pre*view"`},
		{"Preview", "a.fit:stream", "b.fit", `invalid path "a.fit:stream"`},
		{"Preview", "a?.fit", "b.fit", `invalid path "a?.fit"`},
		{"Preview", "a/b", "b.fit", `invalid name "a/b"`},
		{"Preview", "a.fit", "b.", `Invalid name "b.": it ends with a space or a dot`},
		{"Preview", "a.fit", "", `Invalid name "": it is empty`},
		{"../x", "a.fit", "b.fit", `invalid path "../x"`},
	} {
		err := fx.svc.Rename(context.Background(), "r1", emmc, c.dir, c.name, c.to)
		if e := wantKind(t, err, ErrBadPath); e.Msg != c.msg {
			t.Errorf("Rename(%q, %q, %q) message = %q, want %q", c.dir, c.name, c.to, e.Msg, c.msg)
		}
	}
	wantKind(t, fx.svc.Rename(context.Background(), "nope", emmc, "Preview", "a.fit", "b.fit"), ErrUnknownRig)
	if fx.opens.Load() != 0 || fx.store.calls() != 0 {
		t.Errorf("opens = %d, store reads = %d, want 0 (refused before any call)", fx.opens.Load(), fx.store.calls())
	}
}
