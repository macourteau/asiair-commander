package devfiles

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/macourteau/asiair-commander/internal/device"
	"github.com/macourteau/asiair-commander/internal/model"
)

func TestListFields(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.AddFile(emmc, "Autorun/Light/M42/a.fit", []byte("frame"), t0)
	f.AddFile(emmc, "Autorun/Light/M42/a_thn.jpg", []byte("p"), t0)
	f.AddFile(emmc, "Preview/ref.fit", []byte("ref"), t0)
	f.AddFile(emmc, "notes.txt", []byte("hello"), t0)
	f.AddDir(emmc, "Empty", t0)

	t.Run("share list", func(t *testing.T) {
		l, err := fx.svc.List(ctx, "r1", "", "")
		if err != nil {
			t.Fatal(err)
		}
		want := []ListEntry{{Name: emmc, Dir: true}, {Name: udisk, Dir: true}}
		if !reflect.DeepEqual(l.Entries, want) || !l.Writable {
			t.Errorf("listing = %+v, want both shares as folders, writable", l)
		}
	})

	t.Run("share root", func(t *testing.T) {
		l, err := fx.svc.List(ctx, "r1", emmc, "")
		if err != nil {
			t.Fatal(err)
		}
		if l.Share != emmc || l.Dir != "" || l.Watched || !l.Writable {
			t.Errorf("listing header = %+v", l)
		}
		want := []ListEntry{
			{Name: "Autorun", Dir: true, Watched: true, RenameRefused: &RenameVerdict{Rule: "reserved",
				Message: "Autorun is, or holds, " + emmc + "/Autorun, a folder Commander depends on (a configured " +
					"capture source, even one turned off, or the ASIAIR's own data folder). Renaming it would " +
					"disrupt archiving on " + emmc + ", so it is refused."}},
			{Name: "Empty", Dir: true, Mtime: t0.Unix()},
			{Name: "Preview", Dir: true},
			{Name: "notes.txt", Size: 5, Mtime: t0.Unix()},
		}
		if !reflect.DeepEqual(l.Entries, want) {
			t.Errorf("entries = %+v\nwant      %+v", l.Entries, want)
		}
	})

	t.Run("inside a watched root", func(t *testing.T) {
		l, err := fx.svc.List(ctx, "r1", emmc, "Autorun/Light/M42")
		if err != nil {
			t.Fatal(err)
		}
		if !l.Watched {
			t.Error("a folder inside a watched root should be watched")
		}
		for _, e := range l.Entries {
			if e.Watched {
				t.Errorf("file %q carries watched; only folders do", e.Name)
			}
		}
	})

	t.Run("a disabled source is not watched", func(t *testing.T) {
		fx.editRig("r1", func(p *model.RigProfile) {
			p.Sources = []model.Source{{Share: emmc, Roots: []string{"Autorun"}, Disabled: true}}
		})
		l, err := fx.svc.List(ctx, "r1", emmc, "Autorun")
		if err != nil {
			t.Fatal(err)
		}
		if l.Watched {
			t.Error("a disabled source's root should not be watched")
		}
	})

	t.Run("an empty folder serializes entries as []", func(t *testing.T) {
		l, err := fx.svc.List(ctx, "r1", emmc, "Empty")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(l)
		if !strings.Contains(string(b), `"entries":[]`) {
			t.Errorf("json = %s, want entries []", b)
		}
	})

	t.Run("wire shape", func(t *testing.T) {
		b, _ := json.Marshal(ListEntry{Name: "a", Dir: true, Size: 0, Mtime: 7, Watched: true})
		if got, want := string(b), `{"name":"a","dir":true,"size":0,"mtime":7,"watched":true}`; got != want {
			t.Errorf("json = %s, want %s", got, want)
		}
		b, _ = json.Marshal(ListEntry{Name: "b", RenameRefused: &RenameVerdict{Rule: "watched", Message: "m"}})
		want := `{"name":"b","dir":false,"size":0,"mtime":0,"watched":false,"rename_refused":{"rule":"watched","message":"m"}}`
		if got := string(b); got != want {
			t.Errorf("json = %s, want %s", got, want)
		}
	})

	t.Run("a missing folder is not_found", func(t *testing.T) {
		_, err := fx.svc.List(ctx, "r1", emmc, "Gone")
		e := wantKind(t, err, ErrNotFound)
		if want := emmc + "/Gone no longer exists on Rig One"; e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
	})

	t.Run("an unknown share says it isn't one", func(t *testing.T) {
		_, err := fx.svc.List(ctx, "r1", "No Such Share", "")
		e := wantKind(t, err, ErrNotFound)
		if want := "No Such Share isn't a share on Rig One"; e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
	})

	t.Run("listing a file says it is a file", func(t *testing.T) {
		fx.dev(ip1).AddFile(emmc, "Preview/plain.fit", []byte("x"), t0)
		_, err := fx.svc.List(ctx, "r1", emmc, "Preview/plain.fit")
		e := wantKind(t, err, ErrNotFound)
		if want := emmc + "/Preview/plain.fit is a file, not a folder"; e.Msg != want {
			t.Errorf("message = %q, want %q", e.Msg, want)
		}
	})

	t.Run("bad paths are refused", func(t *testing.T) {
		for _, c := range [][2]string{{"", "Preview"}, {emmc, "/Preview"}, {emmc, "a/../b"}, {"a/b", ""}} {
			_, err := fx.svc.List(ctx, "r1", c[0], c[1])
			wantKind(t, err, ErrBadPath)
		}
	})
}

func TestMarkersInvisible(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	f := fx.dev(ip1)
	f.ListMarkers(true)
	f.AddFile(udisk, "ASIAIR/Autorun/x.fit", []byte("x"), t0)
	f.SetMarker(udisk, "ASIAIR/"+model.MediaMarkerName, []byte("id"))
	f.SetMarker(udisk, strings.ToUpper(model.IdentityMarkerName), []byte("legacy"))
	f.SetMarker(udisk, "Only/"+model.MediaMarkerName, []byte("id"))

	t.Run("listing", func(t *testing.T) {
		root, err := fx.svc.List(ctx, "r1", udisk, "")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range root.Entries {
			names = append(names, e.Name)
		}
		if want := []string{"ASIAIR", "Only"}; !reflect.DeepEqual(names, want) {
			t.Errorf("root lists %v, want %v", names, want)
		}
		nested, err := fx.svc.List(ctx, "r1", udisk, "ASIAIR")
		if err != nil {
			t.Fatal(err)
		}
		if len(nested.Entries) != 1 || nested.Entries[0].Name != "Autorun" {
			t.Errorf("ASIAIR lists %+v, want only Autorun", nested.Entries)
		}
		only, err := fx.svc.List(ctx, "r1", udisk, "Only")
		if err != nil {
			t.Fatal(err)
		}
		if len(only.Entries) != 0 {
			t.Errorf("a folder holding only a marker lists %+v, want nothing", only.Entries)
		}
	})

	t.Run("walk", func(t *testing.T) {
		es, err := fx.svc.walk(ctx, "Delete check", "r1", udisk, "", []string{"ASIAIR", "Only"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"ASIAIR/", "ASIAIR/Autorun/", "ASIAIR/Autorun/x.fit", "Only/"}
		if got := rels(es); !reflect.DeepEqual(got, want) {
			t.Errorf("walk = %v, want %v", got, want)
		}
	})

	t.Run("a selection naming a marker is refused", func(t *testing.T) {
		for _, n := range []string{model.MediaMarkerName, strings.ToUpper(model.IdentityMarkerName)} {
			_, err := fx.svc.walk(ctx, "Delete check", "r1", udisk, "", []string{n})
			wantKind(t, err, ErrBadPath)
		}
	})

	t.Run("an echoed path naming a marker is refused", func(t *testing.T) {
		for _, p := range []string{"ASIAIR/" + model.MediaMarkerName, "ASIAIR/.Asiair-Commander-Media-Id"} {
			wantKind(t, checkRel(p), ErrBadPath)
		}
	})
}

// TestListRenameRefused checks every listed entry's rename verdict against the
// route's old-path verdict on the rename table's rows: a refusal decided by the
// entry alone reads the same, and one decided by the new name, or none, leaves
// the entry unrefused.
func TestListRenameRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range renameCases() {
		if tc.notListable {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			if tc.setup != nil {
				tc.setup(fx)
			}
			tc.stage(fx)
			l, err := fx.svc.List(ctx, "r1", tc.share, tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			var got *RenameVerdict
			found := false
			for _, e := range l.Entries {
				if e.Name == tc.item {
					got, found = e.RenameRefused, true
				}
			}
			if !found {
				t.Fatalf("%s not listed in %+v", tc.item, l.Entries)
			}
			if tc.kind == nil || tc.byNewName {
				if got != nil {
					t.Errorf("rename_refused = %+v, want none", got)
				}
				return
			}
			if got == nil || got.Message != tc.msg || (got.Rule != "watched" && got.Rule != "reserved") {
				t.Errorf("rename_refused = %+v, want the route's %q", got, tc.msg)
			}
		})
	}

	t.Run("one store read per listing, retries included", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
		f.FailNext(device.FakeOpListDir, 1, errNetworkForTest())
		if _, err := fx.svc.List(ctx, "r1", emmc, "Preview"); err != nil {
			t.Fatal(err)
		}
		if got := fx.store.calls(); got != 1 {
			t.Errorf("ResidentLeads calls = %d, want 1", got)
		}
		if _, err := fx.svc.List(ctx, "r1", "", ""); err != nil {
			t.Fatal(err)
		}
		if got := fx.store.calls(); got != 1 {
			t.Errorf("ResidentLeads calls after a share list = %d, want still 1", got)
		}
	})

	t.Run("a store error refuses every entry and still lists", func(t *testing.T) {
		fx := newFixture(t)
		f := fx.dev(ip1)
		f.AddFile(emmc, "Preview/a.fit", []byte("x"), t0)
		f.AddDir(emmc, "Preview/Set", t0)
		fx.store.leadsErr = errors.New("database is locked")
		l, err := fx.svc.List(ctx, "r1", emmc, "Preview")
		if err != nil {
			t.Fatalf("List = %v, want the listing despite the store error", err)
		}
		want := &RenameVerdict{Rule: "unchecked",
			Message: "Commander couldn't read its records to check renames here. Try again."}
		if len(l.Entries) != 2 {
			t.Fatalf("entries = %+v, want 2", l.Entries)
		}
		for _, e := range l.Entries {
			if !reflect.DeepEqual(e.RenameRefused, want) {
				t.Errorf("%s: rename_refused = %+v, want %+v", e.Name, e.RenameRefused, want)
			}
		}
	})
}
